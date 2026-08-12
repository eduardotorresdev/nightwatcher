package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// These tests raise the real daemon — the real webhook handler, the real worker Pool,
// the real Analyst — on a random port against real SQLite in a temp file, and assert on
// what the outside world observed: the Spy that stands in for the Destino, and the rows
// that ended up in the database.

// daemonSecret stands in for the integration's Client Secret. The signatures are
// computed with it at delivery time rather than recorded as a dead digest, so the whole
// signing path is exercised instead of a stored string.
const daemonSecret = "s3gr3d0-de-teste-do-nightwatcher"

// ------------------------------------------------------------------ fake Clock

// daemonClock is time under the test's control. Sleep really blocks, and it announces
// itself on sleeping so a test can advance the clock exactly when the daemon is waiting
// for it. Nothing here waits a real duration: the worker's five-second poll and its
// twenty-five-minute lease cost the suite nothing.
type daemonClock struct {
	mu       sync.Mutex
	now      time.Time
	waiters  []*daemonSleeper
	sleeping chan struct{}
}

type daemonSleeper struct {
	until time.Time
	woken chan struct{}
}

func newDaemonClock() *daemonClock {
	return &daemonClock{now: fixed, sleeping: make(chan struct{}, 128)}
}

func (c *daemonClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *daemonClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	s := &daemonSleeper{until: c.now.Add(d), woken: make(chan struct{})}
	c.waiters = append(c.waiters, s)
	c.mu.Unlock()

	select {
	case c.sleeping <- struct{}{}:
	default: // nobody is pumping right now; the next Advance still wakes this waiter
	}

	select {
	case <-s.woken:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *daemonClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	var wake []*daemonSleeper
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

// pump advances the clock every time the daemon blocks on it, until done fires.
//
// This is what replaces waiting. The daemon polls its queue every five seconds and the
// clock is fake, so each advance costs nothing and only happens when there is something
// asleep to wake — no busy loop, and no real duration is ever lived through. The
// deadline is the failure guard, not the wait: reaching it means the daemon never got
// where it was going.
//
// The deadline is armed ONCE, before the loop, and never rearmed. A per-iteration
// `case <-time.After(…)` would be reset by every tick of the fake clock, and the daemon
// idling on its five-second poll ticks forever — so a scenario the daemon never reaches
// would spin at full CPU until the go test timeout killed the whole package, instead of
// failing here in thirty seconds saying what it never reached. That is how this file
// behaved before, and it is what a broken implementation looks like from the outside:
// a hang, not a red test.
func (c *daemonClock) pump(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.sleeping:
			c.Advance(6 * time.Second)
		case <-deadline.C:
			t.Fatalf("o daemon não chegou a %s", what)
		}
	}
}

// ------------------------------------------------------------------ fake Agent

// daemonAgent answers with canned text in the exact envelope internal/prompts parses.
type daemonAgent struct {
	mu     sync.Mutex
	calls  []domain.AgentRequest
	triage string
	deep   string

	// started closes on the first session, which is how a test knows an Análise is in
	// flight without watching the clock.
	started chan struct{}
	once    sync.Once

	// hang makes the session wait for cancellation, the way a real one behaves when
	// Dokku restarts the container mid-analysis.
	hang bool
}

func (a *daemonAgent) Run(ctx context.Context, req domain.AgentRequest) (domain.AgentResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, req)
	hang := a.hang
	a.mu.Unlock()
	a.once.Do(func() { close(a.started) })

	if hang {
		<-ctx.Done()
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrTimeout,
			Detail: "a sessão foi cancelada antes de terminar",
			Err:    ctx.Err(),
		}
	}
	if req.Kind == domain.SessionTriage {
		return domain.AgentResult{Text: a.triage}, nil
	}
	return domain.AgentResult{Text: a.deep}, nil
}

func (a *daemonAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// fixtureRepoPath exists in testdata/repo and matches the stack of the recorded
// payloads, so prompts.CheckPaths does not mark the finding as hallucinated.
const fixtureRepoPath = "src/routes/(sistema)/pedidos/_components/CronogramaGrade.svelte"

func triageAnswer(v domain.Verdict) string {
	return fmt.Sprintf(
		`{"desfecho": %q, "justificativa": "o Sentry capturou uma exceção não tratada em produção"}`,
		string(v))
}

func diagnosisAnswer() string {
	return fmt.Sprintf(`{
	  "causa_provavel": "o componente usa Tooltip sem importar o símbolo",
	  "arquivos_suspeitos": [{"caminho": %q, "linha": 12, "porque": "usa Tooltip sem import"}],
	  "evidencias": ["ReferenceError: Tooltip is not defined"],
	  "confianca": "alta",
	  "ajuste_prioridade": {"direcao": "manter", "justificativa": ""}
	}`, fixtureRepoPath)
}

// ------------------------------------------------------------------ fake Workspace

// fixtureWorkspace hands out the repository slice whose files match the stack traces in
// the recorded Sentry payloads.
type fixtureWorkspace struct{}

func (fixtureWorkspace) Prepare(context.Context, domain.Project) (string, func(), error) {
	return filepath.Join("..", "..", "testdata", "repo"), func() {}, nil
}

// ------------------------------------------------------------------ Destino

// watchedSpy is the Spy plus a way to know the exact moment an issue was born, so a
// scenario driving the whole daemon can act on that instead of on the wall clock.
type watchedSpy struct {
	*tracker.Spy
	created chan struct{}
	once    sync.Once
}

func newWatchedSpy() *watchedSpy {
	return &watchedSpy{Spy: tracker.NewSpy(), created: make(chan struct{})}
}

func (s *watchedSpy) CreateIssue(ctx context.Context, req domain.IssueRequest) (domain.IssueRef, error) {
	ref, err := s.Spy.CreateIssue(ctx, req)
	s.once.Do(func() { close(s.created) })
	return ref, err
}

// ------------------------------------------------------------------ harness

type daemonHarness struct {
	dbPath string
	store  *store.Store
	spy    *watchedSpy
	agent  *daemonAgent
	clock  *daemonClock
	proj   domain.Project
	base   string

	cancel context.CancelFunc
	done   chan error
}

func newDaemonHarness(t *testing.T) *daemonHarness {
	t.Helper()

	h := &daemonHarness{
		dbPath: tempDB(t),
		spy:    newWatchedSpy(),
		clock:  newDaemonClock(),
		agent: &daemonAgent{
			triage:  triageAnswer(domain.VerdictDefect),
			deep:    diagnosisAnswer(),
			started: make(chan struct{}),
		},
		done: make(chan error, 1),
	}
	h.proj = seedProject(t, h.dbPath, projectFixture("acme", "acme/acme", "acme", "web"))
	h.store = openStore(t, h.dbPath)
	return h
}

// start raises the daemon on a random port and returns once the socket is bound.
func (h *daemonHarness) start(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	bound := make(chan string, 1)
	go func() {
		h.done <- runDaemon(ctx, daemonDeps{
			Store:     h.store,
			Secret:    daemonSecret,
			Agent:     h.agent,
			Tracker:   h.spy,
			Workspace: fixtureWorkspace{},
			Clock:     h.clock,
			Logger:    slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Addr:      "127.0.0.1:0",
			Listening: func(a net.Addr) { bound <- a.String() },
			// The drain is bounded so a test can never hang on it; nothing in these
			// scenarios holds a request open.
			ShutdownTimeout: 5 * time.Second,
		})
	}()

	select {
	case addr := <-bound:
		h.base = "http://" + addr
	case err := <-h.done:
		t.Fatalf("o daemon morreu antes de abrir a porta: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("o daemon não abriu a porta")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(20 * time.Second):
			t.Error("o daemon não desligou")
		}
	})
}

// stop cancels the daemon's context — the SIGTERM path — and returns what it exited
// with. The exit is put back on the channel so the harness's own cleanup, which waits
// for the same thing, is not left waiting for a value already taken.
func (h *daemonHarness) stop(t *testing.T) error {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("o daemon não desligou depois do cancelamento")
		return nil
	}
}

// deliver signs a recorded payload with the Client Secret and POSTs it at the webhook,
// exactly as the Sentry integration does.
func (h *daemonHarness) deliver(t *testing.T, fixture string) *http.Response {
	t.Helper()
	body := readFixture(t, fixture)

	req, err := http.NewRequest(http.MethodPost, h.base+ingest.WebhookPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("montando a entrega: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ingest.HeaderResource, ingest.ResourceIssue)
	// HMAC-SHA256 in hex over the RAW BODY BYTES. Re-marshalling before signing is the
	// classic way to get this wrong.
	mac := hmac.New(sha256.New, []byte(daemonSecret))
	mac.Write(body)
	req.Header.Set(ingest.HeaderSignature, hex.EncodeToString(mac.Sum(nil)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("entregando o webhook: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sentry", name))
	if err != nil {
		t.Fatalf("lendo a fixture %s: %v", name, err)
	}
	return body
}

// queueRow is the other half of "what the outside world observed", next to the Tracker.
type queueRow struct {
	attempts    int
	availableAt time.Time
	owner       string
	done        bool
}

func (h *daemonHarness) queue(t *testing.T) queueRow {
	t.Helper()
	var (
		row    queueRow
		owner  *string
		avail  int64
		doneAt *int64
	)
	err := h.store.DB().QueryRowContext(context.Background(),
		`SELECT attempts, available_at, owner, done_at FROM queue ORDER BY id DESC LIMIT 1`).
		Scan(&row.attempts, &avail, &owner, &doneAt)
	if err != nil {
		t.Fatalf("lendo o item da fila: %v", err)
	}
	row.availableAt = time.UnixMilli(avail).UTC()
	if owner != nil {
		row.owner = *owner
	}
	row.done = doneAt != nil
	return row
}

func (h *daemonHarness) onlyAnalysis(t *testing.T) domain.Analysis {
	t.Helper()
	list, err := h.store.ListAnalyses(context.Background(), store.AnalysisFilter{})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("análises gravadas = %d, queria 1", len(list))
	}
	return list[0]
}

// ------------------------------------------------------------------ scenarios

func TestTheDaemonAnalysesASignedDeliveryAndTheDestinoSeesTheIssue(t *testing.T) {
	h := newDaemonHarness(t)
	h.start(t)

	resp := h.deliver(t, "defeito-error-new.json")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d", resp.StatusCode, http.StatusAccepted)
	}

	h.clock.pump(t, "criar a issue no Destino", h.spy.created)

	created := h.spy.Created()
	if len(created) != 1 {
		t.Fatalf("issues criadas = %d, queria 1", len(created))
	}
	issue := created[0]
	if issue.ProjectID != h.proj.LinearProjectID {
		t.Errorf("Project do Destino = %q, queria %q", issue.ProjectID, h.proj.LinearProjectID)
	}
	if issue.Priority == domain.PriorityNone {
		t.Errorf("a issue nasceu sem Prioridade; a regra nunca produz `0 — Sem prioridade`")
	}
	// The Fonte's rótulo survives all the way to the board, which is what makes "show me
	// everything from the API" a filter instead of a text search.
	if len(issue.Labels) != 1 || issue.Labels[0] != "web" {
		t.Errorf("rótulos = %v, queria [web]", issue.Labels)
	}
	if !bytes.Contains([]byte(issue.Body), []byte(fixtureRepoPath)) {
		t.Errorf("o corpo da issue não aponta para o arquivo suspeito:\n%s", issue.Body)
	}
	if tracker.NotAnalysedTag == "" || bytes.Contains([]byte(issue.Title), []byte(tracker.NotAnalysedTag)) {
		t.Errorf("a issue nasceu marcada como não analisada: %q", issue.Title)
	}

	// The row is read only after the daemon has stopped. The Spy sees the issue the
	// instant it is created, which is BEFORE the Análise is committed to SQLite — and a
	// shutdown waits for every Análise in flight, so this is the ordering the assertion
	// needs instead of a poll.
	if err := h.stop(t); err != nil {
		t.Fatalf("o desligamento devolveu erro: %v", err)
	}

	an := h.onlyAnalysis(t)
	if an.State != domain.StatePublished {
		t.Errorf("estado = %q, queria %q", an.State, domain.StatePublished)
	}
	if an.Verdict != domain.VerdictDefect {
		t.Errorf("triagem = %q, queria %q", an.Verdict, domain.VerdictDefect)
	}
	if an.IssueID == "" {
		t.Error("a Análise não guardou o vínculo com a issue; a reincidência depende dele")
	}
	if row := h.queue(t); !row.done {
		t.Errorf("o item da fila continuou aberto depois de publicada: %+v", row)
	}
}

func TestHealthzAnswersOKWithTheWorkerRunning(t *testing.T) {
	h := newDaemonHarness(t)
	h.start(t)

	resp, err := http.Get(h.base + healthzPath)
	if err != nil {
		t.Fatalf("GET %s: %v", healthzPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, queria %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(bytes.TrimSpace(body)) == 0 {
		t.Error("o health check respondeu vazio")
	}
}

func TestTheWebhookAndHealthzAreTheWholePublicSurface(t *testing.T) {
	h := newDaemonHarness(t)
	h.start(t)

	for _, path := range []string{"/", "/admin", "/analyses", "/metrics", "/webhooks"} {
		resp, err := http.Get(h.base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s respondeu %d; nada além do webhook e do health check é exposto",
				path, resp.StatusCode)
		}
	}
}

func TestAForgedDeliveryIsRefusedAndNothingIsWrittenDown(t *testing.T) {
	h := newDaemonHarness(t)
	h.start(t)

	body := readFixture(t, "defeito-error-new.json")
	req, _ := http.NewRequest(http.MethodPost, h.base+ingest.WebhookPath, bytes.NewReader(body))
	req.Header.Set(ingest.HeaderResource, ingest.ResourceIssue)
	req.Header.Set(ingest.HeaderSignature, hex.EncodeToString([]byte("assinatura forjada")))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("entregando o webhook forjado: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, queria %d", resp.StatusCode, http.StatusUnauthorized)
	}
	list, err := h.store.ListAnalyses(context.Background(), store.AnalysisFilter{})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("uma entrega sem assinatura válida gravou %d linha(s); ela só pode ir para o log", len(list))
	}
}

func TestTheShutdownHandsTheAnaliseBackToTheQueueTakeable(t *testing.T) {
	h := newDaemonHarness(t)
	// The session hangs until the context is cancelled, which is what a Dokku deploy
	// landing mid-analysis looks like from inside the Análise.
	h.agent.hang = true
	h.start(t)

	if resp := h.deliver(t, "defeito-error-new.json"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d", resp.StatusCode, http.StatusAccepted)
	}
	h.clock.pump(t, "abrir a sessão de Triagem", h.agent.started)

	if err := h.stop(t); err != nil {
		t.Fatalf("o desligamento devolveu erro: %v", err)
	}

	// Not an unanalysed issue: a deploy is not a failed Análise, and treating it as one
	// would turn every deploy into a batch of issues nobody asked for.
	if n := h.spy.CreatedCount(); n != 0 {
		t.Errorf("issues criadas durante o desligamento = %d, queria 0", n)
	}

	row := h.queue(t)
	if row.done {
		t.Fatal("o item foi fechado durante o desligamento; a Ocorrência teria sumido")
	}
	if row.owner != "" {
		t.Errorf("o item continuou preso ao worker morto (owner=%q); o próximo processo esperaria o lease vencer", row.owner)
	}
	if row.availableAt.After(h.clock.Now()) {
		t.Errorf("o item só ficaria tomável em %s, depois de agora (%s)", row.availableAt, h.clock.Now())
	}

	an := h.onlyAnalysis(t)
	if an.State != domain.StateQueued {
		t.Errorf("estado = %q, queria %q: a Análise interrompida volta para a fila inteira",
			an.State, domain.StateQueued)
	}
	if h.agent.callCount() == 0 {
		t.Error("nenhuma sessão chegou a abrir; o cenário não exercitou o desligamento no meio da Análise")
	}
}

func TestTheDaemonRefusesToStartNamingEveryMissingCredential(t *testing.T) {
	for _, env := range []string{
		"SENTRY_CLIENT_SECRET", "SENTRY_TOKEN", "GITHUB_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN", "LINEAR_API_KEY", "LINEAR_CLIENT_ID", "LINEAR_CLIENT_SECRET",
	} {
		t.Setenv(env, "")
	}

	out, err := runCLI(t, "", "serve", "--db", tempDB(t))
	if err == nil {
		t.Fatal("o daemon subiu sem nenhuma credencial")
	}

	// Naming every one of them at once is the point: finding them one restart at a time
	// is what makes 3 a.m. expensive.
	for _, want := range []string{
		"SENTRY_CLIENT_SECRET", "SENTRY_TOKEN", "GITHUB_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "LINEAR_API_KEY",
	} {
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("a falha de partida não nomeia %s: %v", want, err)
		}
	}
	mustNotContain(t, out, "no ar")
}

func TestAnOcorrenciaRecordedButNeverQueuedIsPickedUpAtBoot(t *testing.T) {
	h := newDaemonHarness(t)

	// The state the endpoint leaves behind when it dies between CreateAnalysis and
	// Enqueue: recorded, listable, non-terminal, and with nothing in line for it. A
	// Sentry redelivery does not rescue it — dedup answers "duplicada" — so if boot does
	// not pick it up, the Ocorrência is gone.
	raw := readFixture(t, "defeito-error-new.json")
	occ, err := ingest.Parse(raw, ingest.ResourceIssue, "")
	if err != nil {
		t.Fatalf("lendo a fixture: %v", err)
	}
	occ.ReceivedAt = fixed
	stranded, err := h.store.CreateAnalysis(context.Background(), domain.Analysis{
		ProjectID:  h.proj.ID,
		SourceID:   h.proj.Sources[0].ID,
		Occurrence: occ,
		State:      domain.StateReceived,
	}, fixed)
	if err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}

	h.start(t)
	h.clock.pump(t, "analisar a Ocorrência órfã", h.spy.created)

	if n := h.spy.CreatedCount(); n != 1 {
		t.Fatalf("issues criadas = %d, queria 1", n)
	}

	// Stopped first: the Spy sees the issue before the Análise reaches SQLite.
	if err := h.stop(t); err != nil {
		t.Fatalf("o desligamento devolveu erro: %v", err)
	}
	again, err := h.store.Analysis(context.Background(), stranded.ID)
	if err != nil {
		t.Fatalf("relendo a Análise: %v", err)
	}
	if again.State != domain.StatePublished {
		t.Errorf("estado = %q, queria %q", again.State, domain.StatePublished)
	}
}

// TestTheCommandLineWritesTheSameFileTheDaemonIsReadingWithoutEitherBlocking is the
// `dokku run` story: the subcommands are a second process on the same persistent volume
// as the daemon, writing the very file the daemon is draining its queue against. WAL and
// the busy timeout are what make that safe, and configuration read on every cycle rather
// than at startup is what makes a write land without a restart.
func TestTheCommandLineWritesTheSameFileTheDaemonIsReadingWithoutEitherBlocking(t *testing.T) {
	h := newDaemonHarness(t)
	h.start(t)

	// A Projeto that did not exist when the daemon came up. The daemon is never told;
	// it has to see it because it re-reads on the next delivery.
	out, err := runCLI(t, "", "--db", h.dbPath, "project", "add",
		"--nome", "acme-api",
		"--repo", "acme/acme",
		"--branch", "main",
		"--linear-project", "99998888-7777-6666-5555-444433332222",
		"--fonte", "acme-api:api")
	if err != nil {
		t.Fatalf("`project add` contra o banco do daemon no ar: %v\n%s", err, out)
	}
	if out, err := runCLI(t, "", "--db", h.dbPath, "config", "set", "max_attempts", "2"); err != nil {
		t.Fatalf("`config set` contra o banco do daemon no ar: %v\n%s", err, out)
	}

	// outage-created.json arrives from acme-api. Registered a moment ago by another
	// process, it must resolve to that Projeto instead of being refused as an unknown
	// Fonte — the difference between the daemon having re-read and not.
	if resp := h.deliver(t, "outage-created.json"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status da entrega recusada = %d, queria %d", resp.StatusCode, http.StatusOK)
	}

	// And a real Ocorrência still goes all the way through, with the command line
	// hammering the same file while the worker runs.
	if resp := h.deliver(t, "defeito-error-new.json"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d", resp.StatusCode, http.StatusAccepted)
	}

	cli := make(chan error, 1)
	go func() {
		for i := 0; i < 15; i++ {
			if _, err := runCLI(t, "", "--db", h.dbPath, "config", "set", "concurrency", "2"); err != nil {
				cli <- fmt.Errorf("`config set` na volta %d: %w", i, err)
				return
			}
			if _, err := runCLI(t, "", "--db", h.dbPath, "analysis", "list"); err != nil {
				cli <- fmt.Errorf("`analysis list` na volta %d: %w", i, err)
				return
			}
		}
		cli <- nil
	}()

	h.clock.pump(t, "criar a issue no Destino com o CLI escrevendo junto", h.spy.created)
	if err := <-cli; err != nil {
		t.Fatalf("o CLI foi bloqueado pelo daemon: %v", err)
	}

	if err := h.stop(t); err != nil {
		t.Fatalf("o desligamento devolveu erro: %v", err)
	}

	// The refusal carries the Projeto that only exists because a separate process wrote
	// it after the daemon was already up.
	list, err := h.store.ListAnalyses(context.Background(), store.AnalysisFilter{ProjectName: "acme-api"})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("análises do Projeto registrado com o daemon no ar = %d, queria 1", len(list))
	}
	if list[0].State != domain.StateRefused || list[0].Refusal != domain.RefusedOutage {
		t.Errorf("desfecho = %q (%q), queria recusada por outage", list[0].State, list[0].Refusal)
	}

	// The number the command line wrote is the one the daemon's own handle reads: one
	// file, one truth, no restart.
	tun, err := h.store.Tunables(context.Background())
	if err != nil {
		t.Fatalf("Tunables: %v", err)
	}
	if tun.MaxAttempts != 2 {
		t.Errorf("max_attempts = %d, esperava o 2 que o CLI gravou", tun.MaxAttempts)
	}

	// And the command line reads back what the daemon wrote.
	listed, err := runCLI(t, "", "--db", h.dbPath, "analysis", "list")
	if err != nil {
		t.Fatalf("`analysis list`: %v\n%s", err, listed)
	}
	mustContain(t, listed, "publicada (defeito)")
	mustContain(t, listed, "recusada (categoria outage)")
}

// TestSIGTERMCancelsTheDaemonsContext covers the wiring every Dokku deploy depends on:
// runDaemon hands the queue item back when its context is cancelled, and this is the
// only thing that cancels it. Without it a deploy would be a SIGKILL, and every Análise
// in flight would sit leased for the whole TTL with nobody running it.
func TestSIGTERMCancelsTheDaemonsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var log bytes.Buffer
	watchSignals(ctx, cancel, slog.New(slog.NewTextHandler(&log, nil)))

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("enviando SIGTERM: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("o SIGTERM não cancelou o contexto do daemon")
	}

	// Naming the signal matters: a deploy and a container the kernel is killing look
	// identical in a log that only says "stopping".
	if !bytes.Contains(log.Bytes(), []byte("terminated")) {
		t.Errorf("o log não nomeia o sinal recebido: %s", log.String())
	}
}

// burst is the set of recorded deliveries that are all analysable and all distinct: one
// Sentry issue id each, `error` and `feedback` only, and none of them the `ongoing`
// unresolved that the refusal rules ignore. Delivered together they are the arrival pattern a bad
// release produces.
var burst = []string{
	"defeito-error-new.json",
	"defeito-error-new-2.json",
	"defeito-error-volume.json",
	"defeito-feedback.json",
	"defeito-feedback-com-dados-de-cliente.json",
	"ruido-error-instrumentacao.json",
	"sugestao-feedback.json",
}

// pumpUntil advances the fake clock until cond holds, which is how a scenario waits for
// something the daemon does off its own poll without any real duration passing.
func (h *daemonHarness) pumpUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	reached := make(chan struct{})
	go func() {
		for {
			if cond() {
				close(reached)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	h.clock.pump(t, what, reached)
}

// TestABurstOfDeliveriesProducesExactlyOneIssueEach is the concurrency ceiling seen from
// the outside: seven Ocorrências arriving at once, two Análises allowed at a time, and
// the two answers that matter are that every one of them ended and that none of them
// ended twice. Counting an in-memory peak instead would be satisfied by an unbounded
// implementation as well; a duplicated issue on the board would not.
func TestABurstOfDeliveriesProducesExactlyOneIssueEach(t *testing.T) {
	h := newDaemonHarness(t)
	h.start(t)

	for _, fixture := range burst {
		if resp := h.deliver(t, fixture); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status = %d, queria %d", fixture, resp.StatusCode, http.StatusAccepted)
		}
	}

	h.pumpUntil(t, "publicar a rajada inteira", func() bool { return h.spy.CreatedCount() >= len(burst) })
	if err := h.stop(t); err != nil {
		t.Fatalf("o desligamento devolveu erro: %v", err)
	}

	if n := h.spy.CreatedCount(); n != len(burst) {
		t.Errorf("issues criadas = %d, queria %d: duplicata no board é o ruído que derruba a confiança", n, len(burst))
	}

	list, err := h.store.ListAnalyses(context.Background(), store.AnalysisFilter{})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	if len(list) != len(burst) {
		t.Fatalf("análises gravadas = %d, queria %d", len(list), len(burst))
	}
	for _, a := range list {
		if a.State != domain.StatePublished {
			t.Errorf("análise %s parou em %q; nenhuma Ocorrência pode ficar sem desfecho", a.ID, a.State)
		}
	}

	// One queue row per Análise, all of them closed. Two open rows for one Análise is
	// two workers on the same Ocorrência.
	rows, err := h.store.DB().QueryContext(context.Background(),
		`SELECT analysis_id, count(*), sum(done_at IS NULL) FROM queue GROUP BY analysis_id`)
	if err != nil {
		t.Fatalf("lendo a fila: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var total, open int
		if err := rows.Scan(&id, &total, &open); err != nil {
			t.Fatalf("lendo a fila: %v", err)
		}
		if total != 1 || open != 0 {
			t.Errorf("análise %s: %d linha(s) na fila, %d ainda aberta(s)", id, total, open)
		}
	}
}

// runSecondProcess raises another daemon on the same file, which is what a Dokku deploy
// is. It does not go through the harness's start: that registers a cleanup waiting on
// the same channel, and two lifetimes fighting over one channel is a test that hangs.
func (h *daemonHarness) runSecondProcess(t *testing.T, what string, until func() bool) {
	t.Helper()

	h.agent = &daemonAgent{
		triage:  triageAnswer(domain.VerdictDefect),
		deep:    diagnosisAnswer(),
		started: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	bound := make(chan string, 1)
	go func() {
		done <- runDaemon(ctx, daemonDeps{
			Store:           h.store,
			Secret:          daemonSecret,
			Agent:           h.agent,
			Tracker:         h.spy,
			Workspace:       fixtureWorkspace{},
			Clock:           h.clock,
			Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Addr:            "127.0.0.1:0",
			Listening:       func(a net.Addr) { bound <- a.String() },
			ShutdownTimeout: 5 * time.Second,
		})
	}()
	select {
	case addr := <-bound:
		h.base = "http://" + addr
	case err := <-done:
		t.Fatalf("o segundo processo morreu antes de abrir a porta: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("o segundo processo não abriu a porta")
	}

	h.pumpUntil(t, what, until)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("o segundo processo saiu com erro: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("o segundo processo não desligou")
	}
}

// TestADeployLandingMidAnaliseLosesNothingAndDuplicatesNothing is User Story 19 end to
// end: four Ocorrências accepted, the sessions still running, SIGTERM, and then the
// container that replaces this one. Everything has to end, exactly once, without anybody
// waiting out a lease — the window of a Dokku restart must not be a blind window.
func TestADeployLandingMidAnaliseLosesNothingAndDuplicatesNothing(t *testing.T) {
	h := newDaemonHarness(t)
	h.agent.hang = true
	h.start(t)

	const accepted = 4
	for _, fixture := range burst[:accepted] {
		if resp := h.deliver(t, fixture); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: status = %d, queria %d", fixture, resp.StatusCode, http.StatusAccepted)
		}
	}
	h.clock.pump(t, "abrir a primeira sessão", h.agent.started)

	if err := h.stop(t); err != nil {
		t.Fatalf("o desligamento devolveu erro: %v", err)
	}
	if n := h.spy.CreatedCount(); n != 0 {
		t.Fatalf("o deploy produziu %d issue(s); um deploy não é uma Análise que falhou", n)
	}

	h.runSecondProcess(t, "o processo novo terminar o que o antigo largou",
		func() bool { return h.spy.CreatedCount() >= accepted })

	if n := h.spy.CreatedCount(); n != accepted {
		t.Errorf("issues criadas = %d, queria %d", n, accepted)
	}
	list, err := h.store.ListAnalyses(context.Background(), store.AnalysisFilter{})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	if len(list) != accepted {
		t.Fatalf("análises gravadas = %d, queria %d", len(list), accepted)
	}
	for _, a := range list {
		if a.State != domain.StatePublished {
			t.Errorf("análise %s ficou em %q depois do redeploy", a.ID, a.State)
		}
	}
}
