package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/agentcli"
	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/priority"
	"github.com/eduardotorresdev/nightwatcher/internal/prompts"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// Journal is the persistence the analysis pipeline touches. *store.Store satisfies it.
// It is an interface so that `nightwatcher occurrence simulate` can run the very same
// pipeline against a double that writes nothing.
type Journal interface {
	ProjectByID(ctx context.Context, id int64) (domain.Project, error)
	UpdateAnalysis(ctx context.Context, a domain.Analysis, now time.Time) error
	AppendSession(ctx context.Context, analysisID string, s domain.Session) (domain.Session, error)
	AnalysesStartedSince(ctx context.Context, projectID int64, since time.Time) (int, error)
}

// history is the half of the Journal that only the reincidência path needs: which
// issue in the Destino this Ocorrência produced before, and with what Prioridade.
// SQLite is the truth and Linear is a mirror, so this is a local SELECT and
// never a discovery round-trip.
//
// It is a separate, optional interface instead of four more methods on Journal
// because the two callers want opposite things: *store.Store satisfies it for free
// and the daemon gets its history, while NopJournal deliberately does not — a
// simulation has no past and must not pretend to have one.
type history interface {
	ListAnalyses(ctx context.Context, f store.AnalysisFilter) ([]domain.Analysis, error)
}

// mcpCatalog is the other optional half of the Journal: the MCP servers that apply to
// every Projeto. It is separate for the same reason `history` is — *store.Store
// satisfies it for free, and NopJournal deliberately does not. A simulation runs with
// exactly the servers it was handed on the Projeto, and inventing global ones for it
// would mean `occurrence simulate` reaching services the operator did not name.
type mcpCatalog interface {
	GlobalMCPServers(ctx context.Context) ([]domain.MCPServer, error)
}

// historyLookback bounds the scan for the previous issue of an Ocorrência. The store
// has no "which issue did this Sentry group produce" query, so the reincidência path
// reads the Projeto's recent Análises and matches on the Sentry group id. Past this
// many Análises the group is treated as never seen, which produces a new issue rather
// than a comment — noisier, never silent.
const historyLookback = 500

// NopJournal records nothing. It is what `occurrence simulate` passes so a simulation
// leaves no trace, and it needs the Projeto handed to it up front.
type NopJournal struct{ Project domain.Project }

func (j NopJournal) ProjectByID(context.Context, int64) (domain.Project, error) {
	return j.Project, nil
}

func (NopJournal) UpdateAnalysis(context.Context, domain.Analysis, time.Time) error { return nil }

func (NopJournal) AppendSession(_ context.Context, _ string, s domain.Session) (domain.Session, error) {
	return s, nil
}

// AnalysesStartedSince always answers zero: a simulation is not production load and
// must not be held back by the hourly cap of the Projeto it is simulating.
func (NopJournal) AnalysesStartedSince(context.Context, int64, time.Time) (int, error) {
	return 0, nil
}

var _ Journal = NopJournal{}

type AnalystDeps struct {
	Journal   Journal
	Agent     domain.Agent
	Tracker   domain.Tracker
	Workspace domain.Workspace
	SentryAPI domain.SentryAPI // may be nil: not knowing the environment is normal
	Clock     domain.Clock
	Tunables  config.Tunables
	Logger    *slog.Logger

	// SessionWorkDir is where the clone is visible from INSIDE the session, which is
	// not the host path when the session runs in a container: agentcli's DockerRunner
	// mounts it at its MountPoint ("/workspace" by default) and ExecRunner sees the
	// host path itself. Empty means the host path, which is also what CheckPaths
	// always uses. Only the prompt reads this.
	SessionWorkDir string
}

// Analyst conducts one Análise through the state machine. It knows nothing about the
// queue: how many attempts are left arrives inside domain.Analysis.Attempts, and the
// decision to come back later leaves as a *retryLater.
type Analyst struct {
	journal   Journal
	history   history
	catalog   mcpCatalog
	agent     domain.Agent
	tracker   domain.Tracker
	workspace domain.Workspace
	sentry    domain.SentryAPI
	clock     domain.Clock
	tun       config.Tunables
	log       *slog.Logger
	sessionIn string
}

func NewAnalyst(d AnalystDeps) (*Analyst, error) {
	switch {
	case d.Journal == nil:
		return nil, errors.New("worker: o Analyst precisa de um Journal")
	case d.Agent == nil:
		return nil, errors.New("worker: o Analyst precisa de um Agent")
	case d.Tracker == nil:
		return nil, errors.New("worker: o Analyst precisa de um Tracker")
	case d.Workspace == nil:
		return nil, errors.New("worker: o Analyst precisa de um Workspace")
	}

	a := &Analyst{
		journal:   d.Journal,
		agent:     d.Agent,
		tracker:   d.Tracker,
		workspace: d.Workspace,
		sentry:    d.SentryAPI,
		clock:     d.Clock,
		tun:       d.Tunables,
		log:       d.Logger,
		sessionIn: strings.TrimSpace(d.SessionWorkDir),
	}
	if a.clock == nil {
		a.clock = domain.SystemClock{}
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	// The three numbers that turn into a defect when they arrive as zero: an unbounded
	// session, and a MaxAttempts of 0 which would make the very first hiccup terminal.
	def := config.Defaults()
	if a.tun.TriageTimeout <= 0 {
		a.tun.TriageTimeout = def.TriageTimeout
	}
	if a.tun.DeepTimeout <= 0 {
		a.tun.DeepTimeout = def.DeepTimeout
	}
	if a.tun.MaxAttempts < 1 {
		a.tun.MaxAttempts = def.MaxAttempts
	}
	a.history, _ = d.Journal.(history)
	a.catalog, _ = d.Journal.(mcpCatalog)
	return a, nil
}

// mcpServers is the server universe of one Projeto's long session: the global ones
// folded with the Projeto's own, the Projeto winning a name collision.
//
// A failure to read the globals does NOT fail the Análise. Losing a server costs
// capability, and a Diagnóstico written from the repository alone is the behaviour of
// every Análise before this existed — while refusing to analyse because a SELECT
// failed would turn a degraded database into an unanalysed issue.
func (a *Analyst) mcpServers(ctx context.Context, p domain.Project) []domain.MCPServer {
	var global []domain.MCPServer
	if a.catalog != nil {
		var err error
		if global, err = a.catalog.GlobalMCPServers(ctx); err != nil {
			a.log.Error("could not read the global MCP servers", "projeto", p.Name, "error", err)
			global = nil
		}
	}
	return domain.MergeMCPServers(global, p.MCPServers)
}

// timeoutGrace is how long past its own ceiling a session is allowed to be before
// this package stops waiting for it. domain.Agent is supposed to enforce
// AgentRequest.Timeout itself (agentcli does, with SIGTERM), and this is the guard
// for the implementation that does not: a worker slot held forever is the
// predecessor's silent death.
const timeoutGrace = 30 * time.Second

// sessionGuard cuts a session off once it has run past its ceiling plus the grace.
//
// The deadline is kept with the INJECTED Clock and not with context.WithTimeout, which
// reads the wall clock. With context.WithTimeout the two ceilings — two
// minutes for the Triagem, twenty for the Diagnóstico — could only be exercised by a
// test willing to wait twenty real minutes, so they were never exercised at all. The
// predecessor died of a single ceiling nobody had ever tried.
type sessionGuard struct {
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	// fired separates "our own guard cut the session off" from "the process is shutting
	// down", which are the same cancelled context and two completely different endings:
	// one is a timeout that counts as an attempt, the other must not be treated as a
	// failed Análise at all.
	fired atomic.Bool
}

func (a *Analyst) guardSession(ctx context.Context, d time.Duration) *sessionGuard {
	g := &sessionGuard{done: make(chan struct{})}
	g.ctx, g.stop = context.WithCancel(ctx)
	if d <= 0 {
		// No ceiling to enforce. Arming the wait anyway would fire it immediately, since
		// Clock.Sleep of a non-positive duration returns at once.
		close(g.done)
		return g
	}
	go func() {
		defer close(g.done)
		if err := a.clock.Sleep(g.ctx, d); err != nil {
			return
		}
		g.fired.Store(true)
		g.stop()
	}()
	return g
}

// release stops the wait and waits for it to end, so no session leaves a goroutine
// behind holding the Clock — with a fake Clock that goroutine would outlive the test.
func (g *sessionGuard) release() {
	g.stop()
	<-g.done
}

// Analyse conducts one Ocorrência from the queue to an outcome.
//
// It returns either an Análise that reached an ending — publicada, encerrada or
// falhou, all of them recorded — or an error saying the queue item must come back.
// There is deliberately no third possibility: no code path here ends with the
// Ocorrência gone, which is the defect that killed the predecessor.
func (a *Analyst) Analyse(ctx context.Context, an domain.Analysis) (domain.Analysis, error) {
	// A crash between the issue being born in the Destino and the queue item being
	// closed leaves a finished Análise still in the queue. Running it again would open
	// a second issue for the same Ocorrência, so an Análise that already reached its
	// ending is reported as done and nothing else.
	if finished(an) {
		return an, nil
	}

	proj, err := a.journal.ProjectByID(ctx, an.ProjectID)
	if err != nil {
		// Without the Projeto there is no Destino to write to, so there is nowhere to
		// record a failure either. The item waits instead of disappearing.
		return an, &retryLater{
			after: a.tun.BackoffFor(an.Attempts),
			err:   fmt.Errorf("lendo o Projeto %d da análise %s: %w", an.ProjectID, an.ID, err),
		}
	}
	src := sourceOf(proj, an)

	// The previous issue is read before any session runs: it feeds the Prioridade
	// facts and the recurrence line of the prompt, and if the Destino is unreachable
	// it is far better to find out before spending twenty minutes of Claude.
	//
	// It is read before the StateFailed branch below as well, and that is the point. An
	// Análise resumed only to publish its unanalysed issue has to carry the same
	// RelatedTo — and the same Prioridade a regressão would have raised — that it would
	// have carried had it been published in the moment it failed. Handing that path an
	// empty priorIssue made the failure issue of a regressão land on the board unlinked
	// from the issue that was supposed to have fixed it, which is precisely the fact
	// somebody reading it at 3 a.m. needs first.
	prior, err := a.previousIssue(ctx, an)
	if err != nil {
		return an, err
	}

	// The previous attempt gave up and only failed to reach the Destino. Publishing
	// the unanalysed issue is all that is left to do — running a 20-minute session for
	// an Análise already given up on would spend the subscription to reach the same
	// place.
	if an.State == domain.StateFailed {
		return a.publishFailure(ctx, an, proj, src, prior)
	}

	// The environment is not in the webhook payload and costs a separate tags call.
	// Not knowing it is a normal state and never fails an Análise.
	a.resolveEnvironment(ctx, &an)

	// ---- Triagem: short session, no tools, no repository.
	an.State = domain.StateTriaging
	if err := a.save(ctx, &an); err != nil {
		return an, err
	}

	prompt, err := a.render(domain.SessionTriage, an, proj, src, prior, "", domain.PriorityNone, nil, nil)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, err)
	}
	// No tools and no MCP server: a Triagem is one turn over text, with no repository
	// and nothing to ask a server about. Both empties are the contract, not an
	// omission.
	reply, err := a.session(ctx, an, domain.SessionTriage, prompt, "", a.tun.TriageTimeout, []string{}, nil)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, err)
	}
	triage, err := prompts.ParseTriage(reply)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, err)
	}
	an.Verdict = triage.Verdict

	// Dúvida and ruído close the Análise here. Nothing is created in the Destino and
	// the Ocorrência stays listable, which is what lets the Triagem be audited later.
	if !an.Verdict.CarriesOn() {
		// The rationale is not copied onto the Análise: PriorityRationale means the
		// agent's justification for moving the Prioridade, and an encerrada never had
		// one. What the Triagem argued is in its session, where `analysis show` reads it.
		an.State = domain.StateClosed
		if err := a.save(ctx, &an); err != nil {
			return an, err
		}
		a.log.Info("Análise encerrada na Triagem",
			"analise", an.ID, "desfecho", string(an.Verdict), "justificativa", triage.Rationale)
		return an, nil
	}

	// ---- Diagnóstico or Estudo: long session, over the read-only clone.
	kind := domain.SessionDiagnosis
	if an.Verdict == domain.VerdictSuggestion {
		kind = domain.SessionStudy
	}

	// The Prioridade the agent argues against is the deterministic one, computed
	// before it is asked: the predecessor let the model produce a free-form score and
	// every Ocorrência landed on the same two numbers.
	facts := a.facts(an, prior, priority.Adjustment{})
	base := a.compute(an, facts, prior)

	an.State = domain.StateAnalyzing
	if err := a.save(ctx, &an); err != nil {
		return an, err
	}

	dir, release, err := a.workspace.Prepare(ctx, proj)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, &domain.AgentError{
			Kind:   domain.AgentErrUnavailable,
			Detail: "preparando o clone do repositório: " + err.Error(),
			Err:    err,
		})
	}
	defer release()

	sessionDir := dir
	if a.sessionIn != "" {
		sessionDir = a.sessionIn
	}
	servers := a.mcpServers(ctx, proj)
	prompt, err = a.render(kind, an, proj, src, prior, sessionDir, base.Priority, factLines(facts), servers)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, err)
	}
	reply, err = a.session(ctx, an, kind, prompt, dir, a.tun.DeepTimeout, agentcli.DeepTools, servers)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, err)
	}

	deep, adjustment, err := a.readDeepReply(kind, reply, dir, proj.Branch)
	if err != nil {
		return a.failure(ctx, an, proj, src, prior, err)
	}

	facts = a.facts(an, prior, adjustment)
	final := a.compute(an, facts, prior)
	if final.Refused {
		a.log.Warn("ajuste de Prioridade recusado por falta de justificativa escrita", "analise", an.ID)
	}
	an.Priority = final.Priority
	an.PriorityRationale = rationale(final, adjustment)

	return a.record(ctx, an, proj, src, prior, kind, deep)
}

// finished reports an Análise that already reached its ending and needs nothing but
// having its queue item closed.
func finished(an domain.Analysis) bool {
	switch an.State {
	case domain.StateClosed, domain.StateRefused:
		return true
	case domain.StatePublished, domain.StateFailed:
		return an.IssueID != ""
	}
	return false
}

// session runs one Claude session and records it — prompt and response both, because
// that is what `analysis show` prints and the raw material for diagnosing a bad
// Triagem. A session that failed is recorded too: the reason is half the evidence.
func (a *Analyst) session(ctx context.Context, an domain.Analysis, kind domain.SessionKind, prompt, workDir string, timeout time.Duration, tools []string, servers []domain.MCPServer) (string, error) {
	started := a.clock.Now()

	guard := a.guardSession(ctx, timeout+timeoutGrace)
	defer guard.release()

	res, err := a.agent.Run(guard.ctx, domain.AgentRequest{
		Kind:       kind,
		Prompt:     prompt,
		WorkDir:    workDir,
		Timeout:    timeout,
		Tools:      tools,
		MCPServers: servers,
	})
	// Declared and did not come up. Not a failure — the session answered — but the
	// answer was written without a source the prompt told it about, and that is
	// invisible in the text.
	if len(res.MCPUnavailable) > 0 {
		a.log.Warn("servidor MCP declarado não subiu na sessão",
			"analise", an.ID, "sessao", string(kind), "servidores", strings.Join(res.MCPUnavailable, ", "))
	}

	sess := domain.Session{
		Kind:      kind,
		Prompt:    prompt,
		Response:  res.Text,
		StartedAt: started,
		EndedAt:   a.clock.Now(),
	}
	if err != nil {
		sess.Err = err.Error()
	}
	// The record is written even while the process is shutting down: the session was
	// really spent, and a Dokku deploy is no reason to lose the evidence of it.
	if _, serr := a.journal.AppendSession(context.WithoutCancel(ctx), an.ID, sess); serr != nil {
		a.log.Error("could not record the session", "analise", an.ID, "sessao", string(kind), "error", serr)
	}

	if err != nil {
		// Our own guard fired: the Agent did not honour its ceiling. That is a timeout
		// and nothing else — classifying it as a malformed reply would send it down
		// the no-retry path for a fault that has nothing to do with the reply.
		if ctx.Err() == nil && guard.fired.Load() {
			return "", &domain.AgentError{
				Kind:   domain.AgentErrTimeout,
				Detail: fmt.Sprintf("a sessão de %s passou de %s sem terminar", string(kind), timeout),
				Err:    err,
			}
		}
		return "", err
	}
	return res.Text, nil
}

// readDeepReply parses the long session and turns it into the markdown that goes on
// the issue, plus the Prioridade move the agent asked for.
func (a *Analyst) readDeepReply(kind domain.SessionKind, reply, root, branch string) (string, priority.Adjustment, error) {
	if kind == domain.SessionStudy {
		study, err := prompts.ParseStudy(reply)
		if err != nil {
			return "", priority.Adjustment{}, err
		}
		return renderStudy(study), moveOf(study.Adjustment), nil
	}

	diag, err := prompts.ParseDiagnosis(reply)
	if err != nil {
		return "", priority.Adjustment{}, err
	}
	// A path that does not exist in the clone is the classic failure of this kind of
	// session, and it must not reach the board looking like a finding.
	missing := prompts.CheckPaths(root, diag.SuspectFiles)
	return renderDiagnosis(diag, missing, branch), moveOf(diag.Adjustment), nil
}

// record writes the outcome in the Destino: a new issue, or a comment on the issue
// this Ocorrência already produced.
func (a *Analyst) record(ctx context.Context, an domain.Analysis, proj domain.Project, src domain.Source, prior priorIssue, kind domain.SessionKind, deep string) (domain.Analysis, error) {
	// BuildBody and BuildComment read the long session's text off the Análise. What
	// goes on the board is the rendered markdown, not the JSON envelope the agent
	// answered with — the envelope stays in the sessions table, where `analysis show`
	// reads it.
	an.Sessions = []domain.Session{{Kind: kind, Response: deep}}

	// The outcome is stamped BEFORE the issue is built, not after it is created.
	// tracker.Failed reads the state first and only falls back to FailureKind and
	// FailureReason when the state does not decide — and those two fields still carry
	// the attempt that died earlier, because an Análise that recovers keeps its
	// history. Building the issue while the state still said `analisando` therefore
	// sent every recovered Análise to the board titled "[não analisada]", with the
	// failure body in place of the Diagnóstico that had just been produced.
	an.State = domain.StatePublished

	switch {
	case prior.exists() && prior.reopenable(an.Occurrence):
		// Reincidência over something already given as done. A NEW issue, pointing at
		// the previous one, with the Prioridade the rule already raised. Reopening the
		// closed issue is not ours to do.
		ref, err := a.tracker.CreateIssue(ctx, a.issueRequest(an, proj, src, prior.issueID))
		if err != nil {
			return an, destinoFailed("criando a issue da reincidência", err)
		}
		an.IssueID, an.IssueURL = ref.ID, ref.URL

	case prior.exists():
		// The Ocorrência came back while the issue is still standing: the history
		// belongs together, so the new data is a comment and nothing is created.
		if err := a.tracker.Comment(ctx, prior.issueID, tracker.BuildComment(an)); err != nil {
			return an, destinoFailed("comentando na issue anterior", err)
		}
		an.IssueID, an.IssueURL = prior.issueID, prior.issueURL

	default:
		ref, err := a.tracker.CreateIssue(ctx, a.issueRequest(an, proj, src, ""))
		if err != nil {
			return an, destinoFailed("criando a issue no Destino", err)
		}
		an.IssueID, an.IssueURL = ref.ID, ref.URL
	}

	if err := a.commit(ctx, &an); err != nil {
		return an, err
	}
	a.log.Info("Análise publicada no Destino",
		"analise", an.ID, "issue", an.IssueID, "prioridade", an.Priority.String(), "desfecho", string(an.Verdict))
	return an, nil
}

func (a *Analyst) issueRequest(an domain.Analysis, proj domain.Project, src domain.Source, relatedTo string) domain.IssueRequest {
	occ := an.Occurrence
	req := domain.IssueRequest{
		LinearProjectID: proj.LinearProjectID,
		Title:           tracker.BuildTitle(an),
		Body:            tracker.BuildBody(an),
		Priority:        an.Priority,
		RelatedTo:       relatedTo,
		AttachmentURL:   firstNonEmpty(occ.Permalink, occ.WebURL),
		AttachmentTitle: attachmentTitle(occ),
	}
	// The Fonte's rótulo survives all the way here, which is what makes "show me
	// everything from the API" a board filter instead of a text search.
	if label := strings.TrimSpace(src.Label); label != "" {
		req.Labels = []string{label}
	}
	return req
}

// failure is the only ending a failed session may have. Either the Ocorrência comes
// back for another attempt, or it becomes an issue in the Destino carrying the raw
// Sentry data, the não-analisada mark and the reason. Never a silence.
func (a *Analyst) failure(ctx context.Context, an domain.Analysis, proj domain.Project, src domain.Source, prior priorIssue, cause error) (domain.Analysis, error) {
	// A shutdown mid-session is not a failed Análise: the item goes back to the queue
	// whole. Treating a Dokku deploy as a failure would turn every deploy into a batch
	// of unanalysed issues.
	if ctx.Err() != nil {
		return an, fmt.Errorf("Análise interrompida: %w", ctx.Err())
	}

	kind := domain.ClassifyAgentError(cause)
	an.FailureKind = kind
	an.FailureReason = detailOf(cause)

	// Retryable failures are worth waiting out; a malformed reply and an authentication
	// failure are not, because repeating produces the same result and only burns
	// budget. Both readings live in Kind.Retryable(); neither is re-derived.
	if kind.Retryable() && an.Attempts < a.tun.MaxAttempts {
		an.State = domain.StateFailing
		if err := a.save(ctx, &an); err != nil {
			return an, err
		}
		return an, &retryLater{after: a.tun.BackoffFor(an.Attempts), err: cause}
	}

	an.State = domain.StateFailed
	if err := a.save(ctx, &an); err != nil {
		return an, err
	}
	a.log.Error("Análise falhou e vira issue não analisada",
		"analise", an.ID, "motivo", string(kind), "tentativas", an.Attempts, "error", cause)
	return a.publishFailure(ctx, an, proj, src, prior)
}

// publishFailure puts the unanalysed issue in the Destino. This is the single most
// important path in the package: it is what stops a failure of the system from
// becoming a lost Ocorrência.
func (a *Analyst) publishFailure(ctx context.Context, an domain.Analysis, proj domain.Project, src domain.Source, prior priorIssue) (domain.Analysis, error) {
	an.State = domain.StateFailed

	// Whatever the Triagem managed to decide still counts: a failed Diagnóstico of a
	// fatal defeito is not born with the same urgency as a failed anything.
	res := a.compute(an, a.facts(an, prior, priority.Adjustment{}), prior)
	an.Priority = res.Priority

	ref, err := a.tracker.CreateIssue(ctx, a.issueRequest(an, proj, src, prior.issueID))
	if err != nil {
		return an, destinoFailed("criando a issue de falha", err)
	}
	an.IssueID, an.IssueURL = ref.ID, ref.URL
	if err := a.commit(ctx, &an); err != nil {
		return an, err
	}
	return an, nil
}

func (a *Analyst) save(ctx context.Context, an *domain.Analysis) error {
	now := a.clock.Now()
	if err := a.journal.UpdateAnalysis(ctx, *an, now); err != nil {
		return fmt.Errorf("gravando o estado da análise %s: %w", an.ID, err)
	}
	an.UpdatedAt = now
	return nil
}

// commitTries is how many times the one write that must not be lost is attempted.
// The pool is capped at a single connection with a five-second busy timeout, so each
// try is another full window against whatever else holds the file — a `dokku run`
// container mid-transaction, the other daemon of an overlapping deploy.
const commitTries = 3

// commit records an Análise whose result already exists outside this process: the
// issue is in the Destino, or the comment is on the board. It writes through a context
// that cancellation cannot reach, because a deploy landing between the Destino and
// SQLite would leave the next attempt believing nothing was ever created — and it
// would create it a second time, on the board, where it is seen.
//
// It is the only write in the package that is retried in place. Everywhere else a
// failed write means the Ocorrência comes back and is done again, which costs a
// session; here it means the issue already exists and doing it again puts a SECOND one
// on the board. The retry does not close that window — nothing short of an idempotency
// key on the Destino would, and Linear's issueCreate has none — so when it finally
// gives up it says so with the issue in hand, because that line is the only warning
// anybody will get that the duplicate about to appear is the same Ocorrência.
func (a *Analyst) commit(ctx context.Context, an *domain.Analysis) error {
	bg := context.WithoutCancel(ctx)
	var err error
	for try := 1; try <= commitTries; try++ {
		if err = a.save(bg, an); err == nil {
			return nil
		}
		a.log.Warn("could not record the issue that is already in the Destino; trying again",
			"analise", an.ID, "issue", an.IssueID, "tentativa", try, "error", err)
	}
	a.log.Error("the issue exists in the Destino but the Análise could not be recorded: "+
		"the resumption will not know it is there and may open a second one",
		"analise", an.ID, "issue", an.IssueID, "url", an.IssueURL, "error", err)
	return err
}

func (a *Analyst) resolveEnvironment(ctx context.Context, an *domain.Analysis) {
	if a.sentry == nil || an.Occurrence.Environment != "" || an.Occurrence.IssueID == "" {
		return
	}
	envs, err := a.sentry.Environments(ctx, an.Occurrence.IssueID)
	if err != nil {
		a.log.Warn("could not read the Ocorrência's environment from Sentry",
			"analise", an.ID, "error", err)
		return
	}
	if len(envs) > 0 {
		an.Occurrence.Environment = envs[0]
	}
}
