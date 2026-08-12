package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// `occurrence replay` and `occurrence simulate` are the two commands that must leave no
// trace, so these tests assert on exactly that: the rows before and after, and a Destino
// that was handed to the command and never touched.

// frozen pins the package clock, so the timestamps these commands print are the
// fixture's and not the day the suite ran.
type frozen struct{}

func (frozen) Now() time.Time { return fixed }

func (frozen) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func freezeClock(t *testing.T) {
	t.Helper()
	prev := clock
	clock = frozen{}
	t.Cleanup(func() { clock = prev })
}

// stubWorld replaces the three adapters that reach outside SQLite, and hands the
// command a real Destino it is not supposed to use. Asserting that the Spy returned here
// stayed empty is what proves a dry run never reached Linear — it is the very object the
// command would have written to if it wrote anywhere.
func stubWorld(t *testing.T, agent domain.Agent) *tracker.Spy {
	t.Helper()
	// The two credentials a dry run genuinely needs: it opens a real session over a real
	// clone. Neither reaches the fakes, which is the point of setting them here rather
	// than of the command not asking.
	t.Setenv(config.EnvClaudeOAuthToken, "token-de-teste-nunca-usado")
	t.Setenv(config.EnvGitHubToken, "token-de-teste-nunca-usado")

	destino := tracker.NewSpy()
	prev := newWorld
	newWorld = func(worldSpec) (outsideWorld, error) {
		return outsideWorld{
			agent:     agent,
			workspace: fixtureWorkspace{},
			tracker:   destino,
		}, nil
	}
	t.Cleanup(func() { newWorld = prev })
	return destino
}

func stubAgentAnswering(verdict domain.Verdict) *daemonAgent {
	return &daemonAgent{
		triage:  triageAnswer(verdict),
		deep:    diagnosisAnswer(),
		started: make(chan struct{}),
	}
}

// counts is every row the two commands could possibly write.
func counts(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"analyses", "sessions", "queue", "projects", "sources", "settings"} {
		var n int
		if err := st.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("contando %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func mustBeUnchanged(t *testing.T, before, after map[string]int) {
	t.Helper()
	for table, want := range before {
		if got := after[table]; got != want {
			t.Errorf("a tabela %s foi de %d para %d linhas; o comando não pode gravar nada", table, want, got)
		}
	}
}

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "sentry", name)
}

func TestSimulateOfARecordedPayloadPrintsAVerdictAndLeavesTheDatabaseUntouched(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)
	seedProject(t, db, projectFixture("acme", "acme/acme", "acme", "web"))
	st := openStore(t, db)

	destino := stubWorld(t, stubAgentAnswering(domain.VerdictDefect))
	before := counts(t, st)

	out, err := runCLI(t, "", "occurrence", "simulate", fixturePath("defeito-error-new.json"), "--db", db)
	if err != nil {
		t.Fatalf("occurrence simulate: %v\n%s", err, out)
	}

	// The verdict, the Prioridade and the issue that would have been born: what the
	// operator is calibrating against.
	mustContain(t, out, string(domain.VerdictDefect))
	mustContain(t, out, "issue nova")
	mustContain(t, out, fixtureRepoPath)
	// The Fonte's rótulo has to reach the previewed issue, because it is what makes
	// "show me everything from the web layer" a board filter — and a preview that
	// dropped it would be calibrating against an issue nobody would actually get.
	mustContain(t, lineWith(t, out, "rótulos"), "web")
	priority := lineWith(t, out, "prioridade")
	if strings.Contains(priority, domain.PriorityNone.String()) {
		t.Errorf("a simulação não calculou Prioridade: %q", priority)
	}
	mustContain(t, out, "Nada foi criado no Destino e nada foi gravado no banco.")

	if n := destino.CreatedCount(); n != 0 {
		t.Errorf("a simulação criou %d issue(s) no Destino de verdade; ela não pode tocar no Linear", n)
	}
	mustBeUnchanged(t, before, counts(t, st))
}

func TestSimulateOfADuvidaClosesTheAnaliseWithNothingInTheDestino(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)
	seedProject(t, db, projectFixture("acme", "acme/acme", "acme", "web"))
	st := openStore(t, db)

	destino := stubWorld(t, stubAgentAnswering(domain.VerdictQuestion))
	before := counts(t, st)

	out, err := runCLI(t, "", "occurrence", "simulate", fixturePath("defeito-feedback.json"), "--db", db)
	if err != nil {
		t.Fatalf("occurrence simulate: %v\n%s", err, out)
	}

	mustContain(t, out, string(domain.StateClosed))
	mustContain(t, out, "Destino: nada seria criado nem comentado.")
	if n := destino.CreatedCount(); n != 0 {
		t.Errorf("uma dúvida produziu %d issue(s); dúvida encerra a Análise", n)
	}
	mustBeUnchanged(t, before, counts(t, st))
}

func TestSimulateStopsAtTheRefusalTheEndpointWouldRecord(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)
	seedProject(t, db, domain.Project{
		Name:            "acme",
		Repo:            "acme/acme",
		Branch:          "main",
		LinearProjectID: "11112222-3333-4444-5555-666677778888",
		// outage-created.json comes from the acme-api Fonte, which has to be registered or
		// the refusal would be "fonte desconhecida" instead of the category.
		Sources: []domain.Source{{SentrySlug: "acme", Label: "web"}, {SentrySlug: "acme-api", Label: "api"}},
	})
	st := openStore(t, db)

	agent := stubAgentAnswering(domain.VerdictDefect)
	stubWorld(t, agent)
	before := counts(t, st)

	out, err := runCLI(t, "", "occurrence", "simulate", fixturePath("outage-created.json"), "--db", db)
	if err != nil {
		t.Fatalf("occurrence simulate: %v\n%s", err, out)
	}

	mustContain(t, out, "RECUSADA")
	mustContain(t, out, string(domain.RefusedOutage))
	// A session spent on a category the daemon refuses at ingest would be calibrating a
	// path that does not exist.
	if agent.callCount() != 0 {
		t.Errorf("a simulação abriu %d sessão(ões) para uma Ocorrência que seria recusada na entrada", agent.callCount())
	}
	mustBeUnchanged(t, before, counts(t, st))
}

func TestSimulateSaysSoWhenTheFonteIsNotRegistered(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)
	seedProject(t, db, projectFixture("outro", "acme/outro", "outro", "web"))
	stubWorld(t, stubAgentAnswering(domain.VerdictDefect))

	out, err := runCLI(t, "", "occurrence", "simulate", fixturePath("defeito-error-new.json"), "--db", db)
	if err == nil {
		t.Fatalf("a simulação aceitou uma Fonte desconhecida:\n%s", out)
	}
	// The daemon would refuse this delivery as "fonte desconhecida"; saying so instead of
	// guessing a Projeto is what keeps the simulation honest about the real path.
	mustContain(t, err.Error(), "não está registrado como Fonte")
	mustContain(t, err.Error(), "--projeto")
}

// ------------------------------------------------------------------ replay

// seedPublished records an Ocorrência that already reached the board, which is the state
// a replay has to be able to run against without producing a second issue.
func seedPublished(t *testing.T, db, fixture string, proj domain.Project) domain.Analysis {
	t.Helper()
	raw := readFixture(t, fixture)
	occ, err := ingest.Parse(raw, ingest.ResourceIssue, "")
	if err != nil {
		t.Fatalf("lendo a fixture %s: %v", fixture, err)
	}
	occ.ReceivedAt = fixed

	return seedAnalysis(t, db, domain.Analysis{
		ProjectID:  proj.ID,
		SourceID:   proj.Sources[0].ID,
		Occurrence: occ,
		State:      domain.StatePublished,
		Verdict:    domain.VerdictDefect,
		Priority:   domain.PriorityHigh,
		IssueID:    "issue-ja-existente",
		IssueURL:   "https://linear.app/acme/issue/ACME-1",
	}, fixed)
}

func TestReplayRerunsAStoredOcorrenciaWithoutDuplicatingTheIssue(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)
	proj := seedProject(t, db, projectFixture("acme", "acme/acme", "acme", "web"))
	recorded := seedPublished(t, db, "defeito-error-new.json", proj)
	st := openStore(t, db)

	agent := stubAgentAnswering(domain.VerdictDefect)
	destino := stubWorld(t, agent)
	before := counts(t, st)

	out, err := runCLI(t, "", "occurrence", "replay", recorded.ID, "--db", db)
	if err != nil {
		t.Fatalf("occurrence replay: %v\n%s", err, out)
	}

	// The Ocorrência really was conducted again — that is the point of a replay.
	if agent.callCount() < 2 {
		t.Errorf("sessões abertas = %d, queria a Triagem e o Diagnóstico", agent.callCount())
	}
	mustContain(t, out, "Replay da Análise "+recorded.ID)
	mustContain(t, out, "issue-ja-existente")
	mustContain(t, out, fixtureRepoPath)

	// And nothing about the board changed: no second issue, and no comment either.
	if n := destino.CreatedCount(); n != 0 {
		t.Errorf("o replay criou %d issue(s) no Destino; a Ocorrência já está no board", n)
	}
	if n := destino.CommentCount(); n != 0 {
		t.Errorf("o replay comentou %d vez(es) no Destino", n)
	}

	mustBeUnchanged(t, before, counts(t, st))
	again, err := st.Analysis(context.Background(), recorded.ID)
	if err != nil {
		t.Fatalf("relendo a Análise: %v", err)
	}
	if again.IssueID != "issue-ja-existente" || again.State != domain.StatePublished {
		t.Errorf("o replay reescreveu a Análise gravada: estado=%q issue=%q", again.State, again.IssueID)
	}
}

func TestReplayPrintsThePromptAndTheReplyWhenAsked(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)
	proj := seedProject(t, db, projectFixture("acme", "acme/acme", "acme", "web"))
	recorded := seedPublished(t, db, "defeito-error-new.json", proj)
	stubWorld(t, stubAgentAnswering(domain.VerdictDefect))

	out, err := runCLI(t, "", "occurrence", "replay", recorded.ID, "--sessions", "--db", db)
	if err != nil {
		t.Fatalf("occurrence replay --sessions: %v\n%s", err, out)
	}

	// Judging a prompt change against a real case means reading the prompt and the
	// reply, and a replay that writes nothing has nowhere else to leave them.
	mustContain(t, out, "Sessões (2)")
	mustContain(t, out, string(domain.SessionTriage))
	mustContain(t, out, string(domain.SessionDiagnosis))
	mustContain(t, out, "prompt:")
	mustContain(t, out, "causa_provavel")
}

func TestReplayOfAnUnknownAnaliseIsAnErrorAndNotSilence(t *testing.T) {
	db := tempDB(t)
	out, err := runCLI(t, "", "occurrence", "replay", "não-existe", "--db", db)
	if err == nil {
		t.Fatalf("o replay aceitou uma Análise inexistente:\n%s", out)
	}
	mustContain(t, err.Error(), "não existe Análise")
}

func TestReplayOfARecusaThatNeverHadAProjetoSaysSoInsteadOfBlamingProjeto0(t *testing.T) {
	freezeClock(t)
	db := tempDB(t)

	// The "fonte desconhecida" recusa: recorded, listable, and with no Projeto — which
	// is precisely the row an operator reaches for after `analysis list`.
	refused := seedAnalysis(t, db, domain.Analysis{
		State:   domain.StateRefused,
		Refusal: domain.RefusedUnknownFonte,
		Occurrence: domain.Occurrence{
			IssueID: "7658235590", Category: domain.CategoryError,
			Title: "fetch failed", SentryProjectSlug: "acme-desconhecido",
		},
	}, fixed)

	out, err := runCLI(t, "", "occurrence", "replay", refused.ID, "--db", db)
	if err == nil {
		t.Fatalf("um replay sem Projeto não tem repositório para ler:\n%s", out)
	}
	mustContain(t, err.Error(), "acme-desconhecido")
	mustContain(t, err.Error(), string(domain.RefusedUnknownFonte))
	// The Projeto 0 never existed; saying it "is no longer registered" is a wild goose
	// chase at three in the morning.
	mustNotContain(t, err.Error(), "não está mais registrado")
}
