package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// seedHistory records one Análise per outcome — published, closed by ruído, closed by
// dúvida, refused at ingest and failed after the retries. All five are first class and
// all five are recorded: that is what makes the Triagem auditable.
func seedHistory(t *testing.T, db string) map[string]domain.Analysis {
	t.Helper()
	p := seedProject(t, db, projectFixture("acme", "acme/acme", "acme-web", "web"))
	src := p.Sources[0]

	out := map[string]domain.Analysis{}

	out["publicada"] = seedAnalysis(t, db, domain.Analysis{
		ProjectID: p.ID, SourceID: src.ID,
		State: domain.StatePublished, Verdict: domain.VerdictDefect,
		Priority: domain.PriorityHigh,
		IssueID:  "issue-uuid-1", IssueURL: "https://linear.app/acme/issue/ACME-401",
		Occurrence: domain.Occurrence{
			IssueID: "7318899062", Category: domain.CategoryError, Level: domain.LevelError,
			Title: "TypeError: Cannot read properties of undefined", SentryProjectSlug: "acme-web",
			UserCount: 12, Count: 40,
		},
	}, fixed)

	out["ruído"] = seedAnalysis(t, db, domain.Analysis{
		ProjectID: p.ID, SourceID: src.ID,
		State: domain.StateClosed, Verdict: domain.VerdictNoise,
		Occurrence: domain.Occurrence{
			IssueID: "7658235585", Category: domain.CategoryFeedback,
			Title: "teste teste", SentryProjectSlug: "acme-web",
		},
	}, fixed.Add(-time.Hour))

	out["dúvida"] = seedAnalysis(t, db, domain.Analysis{
		ProjectID: p.ID, SourceID: src.ID,
		State: domain.StateClosed, Verdict: domain.VerdictQuestion,
		Occurrence: domain.Occurrence{
			IssueID: "7658235586", Category: domain.CategoryFeedback,
			Title: "como emito a segunda via?", SentryProjectSlug: "acme-web",
		},
	}, fixed.Add(-2*time.Hour))

	out["recusada"] = seedAnalysis(t, db, domain.Analysis{
		State: domain.StateRefused, Refusal: domain.RefusedOutage,
		Occurrence: domain.Occurrence{
			IssueID: "7658235587", Category: domain.CategoryOutage,
			Title: "Cron job diario falhou", SentryProjectSlug: "acme-cron",
		},
	}, fixed.Add(-3*time.Hour))

	out["falhou"] = seedAnalysis(t, db, domain.Analysis{
		ProjectID: p.ID, SourceID: src.ID,
		State: domain.StateFailed, Attempts: 4,
		FailureKind: domain.AgentErrTimeout, FailureReason: "a sessão passou de 20m",
		IssueID: "issue-uuid-2", IssueURL: "https://linear.app/acme/issue/ACME-402",
		Occurrence: domain.Occurrence{
			IssueID: "7658235588", Category: domain.CategoryError,
			Title: "fetch failed", SentryProjectSlug: "acme-web",
		},
	}, fixed.Add(-4*time.Hour))

	return out
}

func TestAnalysisListShowsTheDiscardedOnesBesideThePublishedOnes(t *testing.T) {
	db := tempDB(t)
	seeded := seedHistory(t, db)

	out, err := runCLI(t, "", "--db", db, "analysis", "list")
	if err != nil {
		t.Fatalf("analysis list: %v\n%s", err, out)
	}

	// An Análise closed by the Triagem and one refused at ingest both have to be
	// here: seeing what was discarded is the whole point of User Story 4.
	closed := lineWith(t, out, seeded["ruído"].ID)
	mustContain(t, closed, "encerrada (ruído)")
	mustContain(t, closed, "acme")
	mustContain(t, closed, "acme-web")

	refused := lineWith(t, out, seeded["recusada"].ID)
	mustContain(t, refused, "recusada (categoria outage)")
	// A recusa has no Projeto and no Fonte row to point at, and still names the
	// Sentry slug the delivery carried.
	mustContain(t, refused, "acme-cron")

	published := lineWith(t, out, seeded["publicada"].ID)
	mustContain(t, published, "publicada (defeito)")
	mustContain(t, published, "https://linear.app/acme/issue/ACME-401")
	mustContain(t, published, "2026-08-11 03:04")

	failed := lineWith(t, out, seeded["falhou"].ID)
	mustContain(t, failed, "falhou")
	mustContain(t, failed, "timeout")
}

func TestAnalysisListFiltersByOutcomeWithAndWithoutAccents(t *testing.T) {
	db := tempDB(t)
	seeded := seedHistory(t, db)

	for _, spelling := range []string{"ruído", "ruido"} {
		out, err := runCLI(t, "", "--db", db, "analysis", "list", "--outcome", spelling)
		if err != nil {
			t.Fatalf("analysis list --outcome %s: %v\n%s", spelling, err, out)
		}
		mustContain(t, out, seeded["ruído"].ID)
		mustNotContain(t, out, seeded["publicada"].ID)
		mustNotContain(t, out, seeded["dúvida"].ID)
	}

	for _, outcome := range []string{"recusada", "falhou", "defeito"} {
		out, err := runCLI(t, "", "--db", db, "analysis", "list", "--outcome", outcome)
		if err != nil {
			t.Fatalf("analysis list --outcome %s: %v\n%s", outcome, err, out)
		}
		if strings.Count(strings.TrimSpace(out), "\n") != 1 {
			t.Errorf("--outcome %s devia trazer uma linha além do cabeçalho:\n%s", outcome, out)
		}
	}
}

func TestAnalysisListRefusesAnOutcomeNobodyRecognises(t *testing.T) {
	db := tempDB(t)
	seedHistory(t, db)

	// An unknown filter answering "nothing here" would read as "nothing was ever
	// discarded", which is the one conclusion this listing exists to make impossible.
	out, err := runCLI(t, "", "--db", db, "analysis", "list", "--outcome", "recusadas")
	if err == nil {
		t.Fatalf("um desfecho inexistente tinha que falhar; saída:\n%s", out)
	}
	mustContain(t, err.Error(), "recusadas")
}

func TestAnalysisListFiltersByProjetoAndRefusesOneThatIsNotRegistered(t *testing.T) {
	db := tempDB(t)
	seeded := seedHistory(t, db)

	out, err := runCLI(t, "", "--db", db, "analysis", "list", "--projeto", "acme")
	if err != nil {
		t.Fatalf("analysis list --projeto acme: %v\n%s", err, out)
	}
	mustContain(t, out, seeded["publicada"].ID)
	mustNotContain(t, out, seeded["recusada"].ID)

	out, err = runCLI(t, "", "--db", db, "analysis", "list", "--projeto", "acmes")
	if err == nil {
		t.Fatalf("um Projeto inexistente tinha que falhar; saída:\n%s", out)
	}
	mustContain(t, err.Error(), "acmes")
}

func TestAnalysisListInJSONIsValidAndCarriesTheSameRows(t *testing.T) {
	db := tempDB(t)
	seeded := seedHistory(t, db)

	out, err := runCLI(t, "", "--db", db, "analysis", "list", "--json")
	if err != nil {
		t.Fatalf("analysis list --json: %v\n%s", err, out)
	}

	var rows []analysisJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("a saída não é JSON válido: %v\n%s", err, out)
	}
	if len(rows) != len(seeded) {
		t.Fatalf("esperava %d linhas, vieram %d", len(seeded), len(rows))
	}

	byID := map[string]analysisJSON{}
	for _, row := range rows {
		byID[row.ID] = row
	}

	published := byID[seeded["publicada"].ID]
	if published.Estado != string(domain.StatePublished) || published.Triagem != string(domain.VerdictDefect) {
		t.Errorf("linha publicada veio errada: %+v", published)
	}
	if published.IssueURL != "https://linear.app/acme/issue/ACME-401" {
		t.Errorf("o link da issue não sobreviveu: %+v", published)
	}
	if published.Projeto != "acme" || published.Fonte != "acme-web" || published.Rotulo != "web" {
		t.Errorf("Projeto/Fonte/rótulo vieram errados: %+v", published)
	}
	if published.CriadaEm != fixed.Format(time.RFC3339) {
		t.Errorf("criada_em = %q, esperava %q", published.CriadaEm, fixed.Format(time.RFC3339))
	}

	refused := byID[seeded["recusada"].ID]
	if refused.Recusa != string(domain.RefusedOutage) {
		t.Errorf("a recusa não sobreviveu ao JSON: %+v", refused)
	}
}

func TestAnalysisShowPrintsThePromptAndTheResponseOfEverySession(t *testing.T) {
	db := tempDB(t)
	seeded := seedHistory(t, db)
	id := seeded["publicada"].ID

	seedSession(t, db, id, domain.Session{
		Kind:      domain.SessionTriage,
		Prompt:    "PROMPT-DA-TRIAGEM: classifique esta Ocorrência",
		Response:  `{"veredito":"defeito"}`,
		StartedAt: fixed,
		EndedAt:   fixed.Add(30 * time.Second),
	})
	seedSession(t, db, id, domain.Session{
		Kind:      domain.SessionDiagnosis,
		Prompt:    "PROMPT-DO-DIAGNOSTICO: leia o repositório",
		Response:  "RESPOSTA-DO-DIAGNOSTICO: src/lib/pedidos.ts:42 não trata undefined",
		StartedAt: fixed.Add(time.Minute),
		EndedAt:   fixed.Add(4 * time.Minute),
	})

	out, err := runCLI(t, "", "--db", db, "analysis", "show", id)
	if err != nil {
		t.Fatalf("analysis show: %v\n%s", err, out)
	}

	for _, want := range []string{
		"PROMPT-DA-TRIAGEM",
		`{"veredito":"defeito"}`,
		"PROMPT-DO-DIAGNOSTICO",
		"RESPOSTA-DO-DIAGNOSTICO: src/lib/pedidos.ts:42 não trata undefined",
		string(domain.SessionTriage),
		string(domain.SessionDiagnosis),
		"publicada (defeito)",
		"2 — High",
	} {
		mustContain(t, out, want)
	}
}

func TestAnalysisShowRefusesAnIdNobodyRecorded(t *testing.T) {
	db := tempDB(t)
	seedHistory(t, db)

	out, err := runCLI(t, "", "--db", db, "analysis", "show", "naoexiste")
	if err == nil {
		t.Fatalf("um id inexistente tinha que falhar; saída:\n%s", out)
	}
	mustContain(t, err.Error(), "naoexiste")
}

func TestAnalysisShowOfARefusedOccurrenceSaysNoSessionWasEverOpened(t *testing.T) {
	db := tempDB(t)
	seeded := seedHistory(t, db)

	out, err := runCLI(t, "", "--db", db, "analysis", "show", seeded["recusada"].ID)
	if err != nil {
		t.Fatalf("analysis show: %v\n%s", err, out)
	}
	mustContain(t, out, "recusada (categoria outage)")
	mustContain(t, out, "nenhuma sessão")
}

// TestTheOcorrenciasOwnTextCannotForgeALineOfTheListing is a property of the audit
// surface, not of formatting. `analysis show` is where the quality of the Triagem is
// judged (User Stories 4 and 31), and half of what it prints — the título, the culprit —
// is text an end user wrote. To a tabwriter a newline is a new row and a tab is a new
// column, so without flattening, a feedback whose title carries both prints extra lines
// that read exactly like the command's own verdict.
func TestTheOcorrenciasOwnTextCannotForgeALineOfTheListing(t *testing.T) {
	db := tempDB(t)
	p := seedProject(t, db, projectFixture("acme", "acme/acme", "acme-web", "web"))

	forged := seedAnalysis(t, db, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID,
		State: domain.StateClosed, Verdict: domain.VerdictNoise,
		Occurrence: domain.Occurrence{
			IssueID: "7658235599", Category: domain.CategoryFeedback, SentryProjectSlug: "acme-web",
			// What an end user can write into a feedback, verbatim.
			Title: "reclamação inofensiva\n  desfecho\tpublicada (defeito)\n  prioridade\t1 — Urgent",
			// And the escape that would wipe the screen of whoever is reading.
			Culprit: "\x1b[2Jsumiu",
		},
	}, fixed)

	out, err := runCLI(t, "", "--db", db, "analysis", "show", forged.ID)
	if err != nil {
		t.Fatalf("analysis show: %v\n%s", err, out)
	}

	// The forged text survives — nothing is censored, it is evidence — but it stays
	// inside the one cell it was written into.
	titulo := lineWith(t, out, "reclamação inofensiva")
	for _, smuggled := range []string{"publicada (defeito)", "1 — Urgent"} {
		if line := lineWith(t, out, smuggled); line != titulo {
			t.Errorf("o texto da Ocorrência forjou a linha %q; devia estar contido na do título", line)
		}
	}

	// And the real outcome is the only row the table opens with "desfecho".
	var desfecho []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "desfecho ") {
			desfecho = append(desfecho, line)
		}
	}
	if len(desfecho) != 1 {
		t.Fatalf("a saída tem %d linhas de desfecho; a Ocorrência escreveu as outras:\n%s", len(desfecho), out)
	}
	mustContain(t, desfecho[0], "encerrada (ruído)")

	// No escape sequence reaches the terminal of the operator reading the evidence.
	mustNotContain(t, out, "\x1b")
}

// TestTheOcorrenciasOwnTextCannotDriveTheOperatorsTerminal is the other half of the
// property above, and the half the table does not cover. `analysis show` prints the
// prompt whole — that is what it is for — and the prompt carries the Ocorrência
// verbatim, which is text an end user wrote. Flattening the título inside the table and
// then echoing the same título raw two screens below would defang nothing: the ESC that
// wipes the screen and the CR that overwrites the line reach the terminal either way.
func TestTheOcorrenciasOwnTextCannotDriveTheOperatorsTerminal(t *testing.T) {
	db := tempDB(t)
	p := seedProject(t, db, projectFixture("acme", "acme/acme", "acme-web", "web"))

	a := seedAnalysis(t, db, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID,
		State: domain.StateClosed, Verdict: domain.VerdictNoise,
		Occurrence: domain.Occurrence{
			IssueID: "7658235601", Category: domain.CategoryFeedback, SentryProjectSlug: "acme-web",
			Title: "reclamação inofensiva",
		},
	}, fixed)
	seedSession(t, db, a.ID, domain.Session{
		Kind:      domain.SessionTriage,
		Prompt:    "<ocorrencia>\ntítulo: \x1b[2J\x1b[H sumiu tudo\rSOBRESCRITO\n</ocorrencia>",
		Response:  "{\"desfecho\":\"ruído\"}",
		StartedAt: fixed,
		EndedAt:   fixed.Add(time.Second),
		Err:       "a sessão falhou\x1b[2J",
	})

	out, err := runCLI(t, "", "--db", db, "analysis", "show", a.ID)
	if err != nil {
		t.Fatalf("analysis show: %v\n%s", err, out)
	}

	if strings.ContainsRune(out, '\x1b') {
		t.Error("uma sequência de escape escrita por usuário final chegou ao terminal do operador")
	}
	if strings.ContainsRune(out, '\r') {
		t.Error("um carriage return escrito por usuário final chegou ao terminal do operador")
	}
	// Nothing is censored: the evidence is still readable, and the newlines that give a
	// prompt its shape are still there.
	mustContain(t, out, "<ocorrencia>")
	mustContain(t, out, "sumiu tudo")
	mustContain(t, out, "SOBRESCRITO")
	mustContain(t, out, "a sessão falhou")
}
