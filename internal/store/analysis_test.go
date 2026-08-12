package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

func TestTheSameDeliveryTwiceIsRejectedButARegressaoOfTheSameGroupIsNot(t *testing.T) {
	// This is the dedup key earning its shape. Sentry delivers the same Ocorrência
	// more than once and the repeat has to be recognised; but a group that broke
	// again arrives as the *same* group with substatus "regressed", and swallowing
	// that as a repeat is the dedup bug turned inside out (ACME-352).
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")

	first := fixtureOccurrence("9001", domain.SubstatusNew)
	if _, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID, Occurrence: first,
	}, t0); err != nil {
		t.Fatalf("primeira entrega: %v", err)
	}

	_, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID, Occurrence: first,
	}, t0.Add(time.Second))
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("entrega repetida devolveu %v, quero ErrDuplicate", err)
	}

	regression := fixtureOccurrence("9001", domain.SubstatusRegressed)
	regression.Action = domain.ActionUnresolved
	if _, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID, Occurrence: regression,
	}, t0.Add(24*time.Hour)); err != nil {
		t.Fatalf("regressão do mesmo grupo foi recusada: %v", err)
	}

	if _, seen, err := s.OccurrenceRecorded(ctx, first); err != nil || !seen {
		t.Errorf("OccurrenceRecorded(entrega repetida) = %v, %v", seen, err)
	}
	if _, seen, err := s.OccurrenceRecorded(ctx, fixtureOccurrence("9002", domain.SubstatusNew)); err != nil || seen {
		t.Errorf("OccurrenceRecorded(grupo inédito) = %v, %v", seen, err)
	}
}

// The same group arriving through both doors. `issue.created` carries a substatus and
// an `event_alert` carries none, so the substatus in the dedup key — which is there on
// purpose, to let a regressão through — cannot tell these two apart on its own. Without
// the first-sighting rule, every error that fires an Alert Rule AND creates a group is
// analysed twice and opens two issues in the Destino.
func TestTheSameGroupThroughBothDoorsIsOneOcorrencia(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")

	// A porta do Alert Rule: sem substatus, action `triggered`.
	viaAlert := fixtureOccurrence("9001", "")
	viaAlert.Action = domain.ActionTriggered
	viaAlert.Category = domain.CategoryFeedback
	if _, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID, Occurrence: viaAlert,
	}, t0); err != nil {
		t.Fatalf("entrega por event_alert: %v", err)
	}

	// O mesmo grupo pela porta issue, minutos depois.
	viaIssue := fixtureOccurrence("9001", domain.SubstatusNew)
	id, seen, err := s.OccurrenceRecorded(ctx, viaIssue)
	if err != nil {
		t.Fatalf("OccurrenceRecorded: %v", err)
	}
	if !seen {
		t.Fatal("o mesmo grupo pela outra porta precisa ser reconhecido como repetição")
	}
	if id == "" {
		t.Error("a repetição precisa nomear a Análise que já existe")
	}
	if _, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID, Occurrence: viaIssue,
	}, t0.Add(time.Minute)); !errors.Is(err, store.ErrDuplicate) {
		// A consulta acima perde a corrida quando as duas entregas chegam juntas; o
		// índice é quem decide de fato.
		t.Fatalf("gravando o mesmo grupo pela outra porta devolveu %v, quero ErrDuplicate", err)
	}

	// E a regressão continua passando: ela não é primeira aparição.
	regression := fixtureOccurrence("9001", domain.SubstatusRegressed)
	regression.Action = domain.ActionUnresolved
	if _, seen, err := s.OccurrenceRecorded(ctx, regression); err != nil || seen {
		t.Fatalf("OccurrenceRecorded(regressão) = %v, %v — a regressão não pode ser engolida", seen, err)
	}
	if _, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID, Occurrence: regression,
	}, t0.Add(24*time.Hour)); err != nil {
		t.Fatalf("regressão depois de um event_alert foi recusada: %v", err)
	}
}

func TestRecusasWithNoOcorrenciaIdentityAreAllRecorded(t *testing.T) {
	// A delivery with an invalid signature carries nothing to identify it by. If the
	// dedup key swallowed those, the second bad signature would go unrecorded — and
	// no code path may end with an Ocorrência discarded in silence.
	ctx := context.Background()
	s := newStore(t)

	for i := 0; i < 2; i++ {
		if _, err := s.CreateAnalysis(ctx, domain.Analysis{
			State:   domain.StateRefused,
			Refusal: domain.RefusedBadSignature,
		}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("recusa %d: %v", i, err)
		}
	}

	got, err := s.ListAnalyses(ctx, store.AnalysisFilter{Outcome: string(domain.StateRefused)})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recusas listadas = %d, quero 2", len(got))
	}
}

func TestDiscardedAndFailedAnalysesStayListableByOutcome(t *testing.T) {
	// `analysis list --outcome` is how the quality of the Triagem gets audited, so
	// closing an Análise cannot mean losing it.
	ctx := context.Background()
	s := newStore(t)
	acme := fixtureProject(t, s, "acme", "acme-api")
	other := fixtureProject(t, s, "outro", "outro-api")

	cases := []struct {
		issue   string
		state   domain.State
		verdict domain.Verdict
		project domain.Project
	}{
		{"1", domain.StatePublished, domain.VerdictDefect, acme},
		{"2", domain.StateClosed, domain.VerdictNoise, acme},
		{"3", domain.StateClosed, domain.VerdictQuestion, acme},
		{"4", domain.StateFailed, domain.VerdictDefect, acme},
		{"5", domain.StatePublished, domain.VerdictSuggestion, other},
	}
	for _, c := range cases {
		if _, err := s.CreateAnalysis(ctx, domain.Analysis{
			ProjectID: c.project.ID, SourceID: c.project.Sources[0].ID,
			State: c.state, Verdict: c.verdict,
			Occurrence: fixtureOccurrence(c.issue, domain.SubstatusNew),
		}, t0); err != nil {
			t.Fatalf("criando análise %s: %v", c.issue, err)
		}
	}

	for _, tc := range []struct {
		name   string
		filter store.AnalysisFilter
		want   int
	}{
		{"tudo", store.AnalysisFilter{}, 5},
		{"ruído continua consultável", store.AnalysisFilter{Outcome: "ruído"}, 1},
		{"falhou é desfecho listável", store.AnalysisFilter{Outcome: "falhou"}, 1},
		{"sem acento porque é linha de comando", store.AnalysisFilter{Outcome: "duvida"}, 1},
		{"por projeto", store.AnalysisFilter{ProjectName: "acme"}, 4},
		{"projeto e desfecho juntos", store.AnalysisFilter{ProjectName: "acme", Outcome: "defeito"}, 2},
	} {
		got, err := s.ListAnalyses(ctx, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d análises, quero %d", tc.name, len(got), tc.want)
		}
	}
}

func TestAnUnknownOutcomeFilterFailsInsteadOfLookingLikeAnEmptyHistory(t *testing.T) {
	// `analysis list --outcome` is the audit path. A mistyped filter answering "zero
	// análises" reads as "nothing was ever refused", which is the one conclusion this
	// table exists to make impossible — the same reason `config set` validates on the
	// way in rather than at the next daemon start.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	if _, err := s.CreateAnalysis(ctx, domain.Analysis{
		ProjectID: p.ID, SourceID: p.Sources[0].ID,
		State: domain.StateRefused, Refusal: domain.RefusedOutage,
		Occurrence: fixtureOccurrence("7001", domain.SubstatusNew),
	}, t0); err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}

	if _, err := s.ListAnalyses(ctx, store.AnalysisFilter{Outcome: "recusadas"}); err == nil {
		t.Error("desfecho inexistente devolveu lista vazia em vez de erro")
	}

	// The real value still works, accented or not.
	for _, outcome := range []string{"recusada", "ruido", "ruído"} {
		if _, err := s.ListAnalyses(ctx, store.AnalysisFilter{Outcome: outcome}); err != nil {
			t.Errorf("desfecho válido %q recusado: %v", outcome, err)
		}
	}
}

func TestAnalysisComesBackWithTheOcorrenciaAndTheSessionsIntact(t *testing.T) {
	// `analysis show` prints what was handed to the agent and what came back; it is
	// also the raw material for diagnosing a bad Triagem.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9100")

	if _, err := s.AppendSession(ctx, a.ID, domain.Session{
		Kind:      domain.SessionTriage,
		Prompt:    "classifique esta ocorrência",
		Response:  "defeito",
		StartedAt: t0,
		EndedAt:   t0.Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("AppendSession triagem: %v", err)
	}
	if _, err := s.AppendSession(ctx, a.ID, domain.Session{
		Kind:      domain.SessionDiagnosis,
		Prompt:    "investigue a causa",
		StartedAt: t0.Add(time.Minute),
		Err:       "timeout",
	}); err != nil {
		t.Fatalf("AppendSession diagnóstico: %v", err)
	}

	a.State = domain.StatePublished
	a.Verdict = domain.VerdictDefect
	a.Priority = domain.PriorityHigh
	a.PriorityRationale = "atinge o checkout inteiro"
	a.IssueID = "issue-uuid"
	a.IssueURL = "https://linear.app/acme/issue/ACME-1"
	a.Occurrence.Environment = "production"
	if err := s.UpdateAnalysis(ctx, a, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("UpdateAnalysis: %v", err)
	}

	got, err := s.Analysis(ctx, a.ID)
	if err != nil {
		t.Fatalf("Analysis: %v", err)
	}

	if got.State != domain.StatePublished || got.Verdict != domain.VerdictDefect {
		t.Errorf("estado/desfecho = %q/%q", got.State, got.Verdict)
	}
	if got.Priority != domain.PriorityHigh || got.PriorityRationale != "atinge o checkout inteiro" {
		t.Errorf("prioridade = %v (%q)", got.Priority, got.PriorityRationale)
	}
	// The Sentry issue id and the Linear issue id are different things and must not
	// have been written over one another.
	if got.IssueID != "issue-uuid" || got.Occurrence.IssueID != "9100" {
		t.Errorf("issue do Linear = %q, issue do Sentry = %q", got.IssueID, got.Occurrence.IssueID)
	}
	// The environment is not in the webhook payload; it arrives later, from a
	// separate tags call.
	if got.Occurrence.Environment != "production" {
		t.Errorf("environment = %q", got.Occurrence.Environment)
	}
	if string(got.Occurrence.RawBody) != `{"action":"created"}` {
		t.Errorf("raw_body voltou %q", got.Occurrence.RawBody)
	}
	if !got.Occurrence.LastSeen.Equal(t0) {
		t.Errorf("last_seen = %v, quero %v", got.Occurrence.LastSeen, t0)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("sessões = %d, quero 2", len(got.Sessions))
	}
	if got.Sessions[0].Kind != domain.SessionTriage || got.Sessions[0].Prompt != "classifique esta ocorrência" {
		t.Errorf("primeira sessão = %+v", got.Sessions[0])
	}
	if !got.Sessions[1].EndedAt.IsZero() || got.Sessions[1].Err != "timeout" {
		t.Errorf("sessão que travou = %+v", got.Sessions[1])
	}
}

func TestAnalysisNotFoundIsDistinguishable(t *testing.T) {
	s := newStore(t)
	if _, err := s.Analysis(context.Background(), "nao-existe"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Analysis de id inexistente devolveu %v, quero ErrNotFound", err)
	}
}

func TestAnalysesStartedCountsPerProjetoInsideTheWindow(t *testing.T) {
	// The per-Projeto hourly cap is measured against this, and it counts Análises
	// that actually started — a burst waiting in the queue has not consumed anything.
	ctx := context.Background()
	s := newStore(t)
	acme := fixtureProject(t, s, "acme", "acme-api")
	other := fixtureProject(t, s, "outro", "outro-api")

	started := func(a domain.Analysis, at time.Time) {
		t.Helper()
		if _, err := s.AppendSession(ctx, a.ID, domain.Session{
			Kind: domain.SessionTriage, Prompt: "p", StartedAt: at,
		}); err != nil {
			t.Fatalf("AppendSession: %v", err)
		}
	}

	inWindow := fixtureAnalysis(t, s, acme, "1")
	started(inWindow, t0.Add(-10*time.Minute))
	// Two sessions of the same Análise are one Análise started, not two.
	started(inWindow, t0.Add(-9*time.Minute))

	started(fixtureAnalysis(t, s, acme, "2"), t0.Add(-2*time.Hour))
	started(fixtureAnalysis(t, s, other, "3"), t0.Add(-5*time.Minute))
	fixtureAnalysis(t, s, acme, "4") // enfileirada, nunca começou

	n, err := s.AnalysesStartedSince(ctx, acme.ID, t0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("AnalysesStartedSince: %v", err)
	}
	if n != 1 {
		t.Errorf("análises iniciadas na última hora = %d, quero 1", n)
	}
}
