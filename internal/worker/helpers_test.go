package worker_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
	"github.com/eduardotorresdev/nightwatcher/internal/worker"
)

// t0 is the instant every scenario starts at. Nothing here ever reads the wall clock:
// backoff and the hourly window are the behaviour under test, and real waiting would
// only make the suite slow and flaky.
var t0 = time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------- fake Clock

// fakeClock is time under the test's control. Sleep really blocks — it returns when
// the test moves the clock past the deadline, or when the context is cancelled — so
// the Pool's poll loop and the lease renewal behave as they do in production without
// a single real millisecond passing.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*sleeper

	// slept receives one token every time somebody starts waiting. It is what lets a
	// test say "the lease renewal has come back round and is waiting again" — which,
	// because the renewal loop only waits after ExtendLease has returned, is a precise
	// happens-after for the renewal instead of a guess dressed up as a sleep.
	slept chan struct{}
}

type sleeper struct {
	until time.Time
	woken chan struct{}
}

func newClock() *fakeClock {
	return &fakeClock{now: t0, slept: make(chan struct{}, 64)}
}

// awaitSleeper blocks until somebody starts waiting on this clock. Nothing here reads
// the wall clock: the token is sent from inside Sleep, so a test that has received it
// knows the loop under test has finished the work before the wait and not merely that
// enough real milliseconds have gone by.
func (c *fakeClock) awaitSleeper(t *testing.T) {
	t.Helper()
	select {
	case <-c.slept:
	case <-time.After(10 * time.Second):
		t.Fatal("ninguém começou a esperar no relógio")
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	s := &sleeper{until: c.now.Add(d), woken: make(chan struct{})}
	c.waiters = append(c.waiters, s)
	c.mu.Unlock()

	select {
	case c.slept <- struct{}{}:
	default:
	}

	select {
	case <-s.woken:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Advance moves the clock and wakes whoever was waiting for an instant that has now
// passed.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	var wake []*sleeper
	for _, s := range c.waiters {
		if !s.until.After(c.now) {
			wake = append(wake, s)
			continue
		}
		kept = append(kept, s)
	}
	c.waiters = kept
	c.mu.Unlock()

	for _, s := range wake {
		close(s.woken)
	}
}

// ---------------------------------------------------------------- fake Agent

type reply struct {
	text string
	err  error
}

// stubAgent answers with canned text in the exact envelope internal/prompts parses.
// Replies are consumed in order and the last one repeats, so "fails four times" and
// "fails once then works" are both one line of setup.
type stubAgent struct {
	mu     sync.Mutex
	triage []reply
	deep   []reply
	calls  []domain.AgentRequest

	// beforeRun runs inside the session, which is what lets a test cancel the context
	// with an Análise in flight.
	beforeRun func(domain.AgentRequest)
	// waitForCancel makes the session hang until the context is cancelled, the way a
	// real one behaves during a Dokku restart.
	waitForCancel bool

	// hangs is the Agent that does NOT honour AgentRequest.Timeout: it blocks until
	// somebody else cancels it and then fails with an error saying nothing about why.
	// Only the caller's own ceiling can know that this was a timeout.
	hangs bool
}

func (s *stubAgent) Run(ctx context.Context, req domain.AgentRequest) (domain.AgentResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	hook, wait := s.beforeRun, s.waitForCancel
	s.mu.Unlock()

	if hook != nil {
		hook(req)
	}
	if s.hangsSession() {
		<-ctx.Done()
		return domain.AgentResult{}, errors.New("a sessão terminou sem dizer por quê")
	}
	if wait {
		<-ctx.Done()
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrTimeout,
			Detail: "a sessão foi cancelada antes de terminar",
			Err:    ctx.Err(),
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	queue := &s.deep
	if req.Kind == domain.SessionTriage {
		queue = &s.triage
	}
	if len(*queue) == 0 {
		return domain.AgentResult{}, fmt.Errorf("o teste não preparou resposta para a sessão de %s", string(req.Kind))
	}
	r := (*queue)[0]
	if len(*queue) > 1 {
		*queue = (*queue)[1:]
	}
	return domain.AgentResult{Text: r.text}, r.err
}

func (s *stubAgent) hangsSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hangs
}

func (s *stubAgent) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubAgent) requests() []domain.AgentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.AgentRequest(nil), s.calls...)
}

func triageReply(v domain.Verdict) reply {
	return reply{text: fmt.Sprintf(
		`{"desfecho": %q, "justificativa": "o Sentry capturou uma exceção não tratada em produção"}`,
		string(v))}
}

// realPath exists in testdata/repo and matches the stack of the recorded fixtures.
const realPath = "src/routes/(sistema)/pedidos/_components/CronogramaGrade.svelte"

func diagnosisReply(path string, adjust, why string) reply {
	return reply{text: fmt.Sprintf(`{
	  "causa_provavel": "o componente usa Tooltip sem importar o símbolo",
	  "arquivos_suspeitos": [{"caminho": %q, "linha": 12, "porque": "usa Tooltip sem import"}],
	  "evidencias": ["ReferenceError: Tooltip is not defined"],
	  "confianca": "alta",
	  "ajuste_prioridade": {"direcao": %q, "justificativa": %q}
	}`, path, adjust, why)}
}

func studyReply() reply {
	return reply{text: `{
	  "viabilidade": "viável",
	  "resumo": "dá para fazer reaproveitando o contador que já existe",
	  "alternativas": [
	    {"nome": "caminho mínimo", "descricao": "usar o contador atual", "esforco": "baixo", "recomendada": true},
	    {"nome": "caminho completo", "descricao": "criar um serviço próprio", "esforco": "alto", "recomendada": false}
	  ],
	  "riscos": ["mexer no cálculo pode regredir o painel"],
	  "dificuldades": ["não há teste nessa área"],
	  "ajuste_prioridade": {"direcao": "manter", "justificativa": ""}
	}`}
}

func agentFailing(kind domain.AgentErrorKind, detail string) reply {
	return reply{err: &domain.AgentError{Kind: kind, Detail: detail}}
}

// ---------------------------------------------------------------- fake Workspace

// fakeWorkspace hands out the fixture repository, whose files match the stack traces
// in the recorded Sentry payloads.
type fakeWorkspace struct {
	dir      string
	err      error
	mu       sync.Mutex
	prepared int
	released int
}

func newWorkspace() *fakeWorkspace {
	return &fakeWorkspace{dir: filepath.Join("..", "..", "testdata", "repo")}
}

func (w *fakeWorkspace) Prepare(context.Context, domain.Project) (string, func(), error) {
	if w.err != nil {
		return "", nil, w.err
	}
	w.mu.Lock()
	w.prepared++
	w.mu.Unlock()
	return w.dir, func() {
		w.mu.Lock()
		w.released++
		w.mu.Unlock()
	}, nil
}

// ---------------------------------------------------------------- fake SentryAPI

// stubSentry answers the one question the webhook payload cannot: which environment
// the Ocorrência was seen in. A zero value is not the same as being absent — the Pool
// is wired without a SentryAPI unless a scenario asks for one.
type stubSentry struct {
	envs map[string][]string
	err  error
}

func (s stubSentry) Environments(_ context.Context, issueID string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.envs[issueID], nil
}

// ---------------------------------------------------------------- fixtures

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Setup(context.Background(), filepath.Join(t.TempDir(), "nw.db"), t0)
	if err != nil {
		t.Fatalf("store.Setup: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func fixtureProject(t *testing.T, s *store.Store) domain.Project {
	t.Helper()
	p, err := s.UpsertProject(context.Background(), domain.Project{
		Name:            "acme",
		Repo:            "acme/acme",
		Branch:          "main",
		LinearProjectID: "linear-project-acme",
		Sources:         []domain.Source{{SentrySlug: "acme", Label: "web"}},
	}, t0)
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	return p
}

// occurrenceFrom reads one of the recorded Sentry payloads. They are real deliveries
// from the acme org, anonymised — the shape is authoritative and the raw bytes are what
// the failure issue has to carry.
func occurrenceFrom(t *testing.T, name string) domain.Occurrence {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sentry", name))
	if err != nil {
		t.Fatalf("lendo a fixture %s: %v", name, err)
	}

	// Read through the very parser the endpoint uses, and with the shape detected off
	// the body, so a worker test exercises the Ocorrência the daemon would really hand
	// it — including the ones that arrive through the event_alert door, which a
	// hand-rolled reader of `data.issue` cannot see at all.
	occ, err := ingest.Parse(raw, ingest.DetectResource(raw), "")
	if err != nil {
		t.Fatalf("lendo a fixture %s como entrega do Sentry: %v", name, err)
	}
	occ.ReceivedAt = t0
	return occ
}

// enqueue records an accepted Ocorrência and puts it in line, which is exactly what
// the webhook handler does before answering Sentry.
func enqueue(t *testing.T, s *store.Store, p domain.Project, occ domain.Occurrence) domain.Analysis {
	t.Helper()
	an, err := s.CreateAnalysis(context.Background(), domain.Analysis{
		ProjectID:  p.ID,
		SourceID:   p.Sources[0].ID,
		Occurrence: occ,
		State:      domain.StateQueued,
	}, t0)
	if err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}
	if _, err := s.Enqueue(context.Background(), an.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return an
}

// harness is one scenario's world: real SQLite in a temp file, a Spy as the Destino,
// a fake Clock, and a Pool wired exactly as `serve` wires it.
type harness struct {
	store *store.Store
	spy   *tracker.Spy
	agent *stubAgent
	work  *fakeWorkspace
	clock *fakeClock
	pool  *worker.Pool
	proj  domain.Project

	// sentry is left nil on purpose: not knowing the environment is the normal state
	//, and only a scenario about the environment wires one in.
	sentry domain.SentryAPI
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		store: newStore(t),
		spy:   tracker.NewSpy(),
		agent: &stubAgent{},
		work:  newWorkspace(),
		clock: newClock(),
	}
	h.proj = fixtureProject(t, h.store)
	h.pool = h.newPool(t, "worker-a")
	return h
}

func (h *harness) newPool(t *testing.T, owner string) *worker.Pool {
	t.Helper()
	p, err := worker.New(worker.Deps{
		Store:     h.store,
		Agent:     h.agent,
		Tracker:   h.spy,
		Workspace: h.work,
		SentryAPI: h.sentry,
		Clock:     h.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:     owner,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	return p
}

func (h *harness) drain(t *testing.T) int {
	t.Helper()
	n, err := h.pool.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	return n
}

func (h *harness) analysis(t *testing.T, id string) domain.Analysis {
	t.Helper()
	an, err := h.store.Analysis(context.Background(), id)
	if err != nil {
		t.Fatalf("Analysis(%s): %v", id, err)
	}
	return an
}

// queueRow is what the queue looks like from the outside — the other half of "what the
// outside world observed", next to the Tracker.
type queueRow struct {
	attempts    int
	availableAt time.Time
	owner       string
	done        bool
}

func (h *harness) queue(t *testing.T, analysisID string) queueRow {
	t.Helper()
	var (
		row      queueRow
		owner    *string
		avail    int64
		doneAt   *int64
		selected = `SELECT attempts, available_at, owner, done_at FROM queue WHERE analysis_id = ? ORDER BY id DESC LIMIT 1`
	)
	err := h.store.DB().QueryRowContext(context.Background(), selected, analysisID).
		Scan(&row.attempts, &avail, &owner, &doneAt)
	if err != nil {
		t.Fatalf("lendo o item da fila de %s: %v", analysisID, err)
	}
	row.availableAt = time.UnixMilli(avail).UTC()
	if owner != nil {
		row.owner = *owner
	}
	row.done = doneAt != nil
	return row
}

// signallingTracker is the Spy plus a way for a test to know the exact moment an
// issue was born, so a scenario that runs the daemon loop can act on that instead of
// waiting on the wall clock.
type signallingTracker struct {
	*tracker.Spy
	created chan struct{}
	once    sync.Once
}

func (s *signallingTracker) CreateIssue(ctx context.Context, req domain.IssueRequest) (domain.IssueRef, error) {
	ref, err := s.Spy.CreateIssue(ctx, req)
	s.once.Do(func() { close(s.created) })
	return ref, err
}
