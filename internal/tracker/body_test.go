package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

func occurrence() domain.Occurrence {
	return domain.Occurrence{
		IssueID:           "4242",
		ShortID:           "ACME-WEB-3F",
		Substatus:         domain.SubstatusNew,
		Action:            domain.ActionCreated,
		Category:          domain.CategoryError,
		Level:             domain.LevelError,
		Title:             "TypeError: cannot read property 'id' of undefined",
		Culprit:           "app/routes/checkout.tsx",
		Permalink:         "https://sentry.io/organizations/acme/issues/4242/",
		Platform:          "javascript",
		SentryProjectSlug: "acme-web",
		Environment:       "production",
		FirstSeen:         time.Date(2026, 8, 10, 22, 15, 0, 0, time.UTC),
		LastSeen:          time.Date(2026, 8, 11, 2, 40, 0, 0, time.UTC),
		RawBody:           []byte(`{"action":"created","data":{"issue":{"id":"4242"}}}`),
	}
}

// A issue de falha é o defeito que matou o antecessor: Ocorrência que some sem
// registro. Ela precisa dizer, em português e sem ambiguidade, que NÃO foi analisada,
// por quê, e onde estão os dados crus.
func TestTheFailureIssueSaysItWasNotAnalysedAndNamesTheReason(t *testing.T) {
	for _, kind := range []domain.AgentErrorKind{
		domain.AgentErrTimeout,
		domain.AgentErrRateLimit,
		domain.AgentErrUnavailable,
		domain.AgentErrMalformed,
		domain.AgentErrAuth,
		domain.AgentErrPerimeter,
	} {
		t.Run(string(kind), func(t *testing.T) {
			a := domain.Analysis{
				Occurrence:    occurrence(),
				State:         domain.StateFailed,
				Attempts:      4,
				FailureKind:   kind,
				FailureReason: "a sessão foi encerrada com SIGTERM",
				Priority:      domain.PriorityMedium,
			}

			body := tracker.BuildBody(a)
			if !strings.Contains(body, "NÃO realizada") {
				t.Errorf("o corpo precisa dizer que a análise não foi feita:\n%s", body)
			}
			if !strings.Contains(body, string(kind)) {
				t.Errorf("o corpo precisa nomear o motivo %q:\n%s", kind, body)
			}
			if !strings.Contains(body, a.Occurrence.Permalink) {
				t.Error("o corpo precisa linkar a Ocorrência no Sentry")
			}
			if !strings.Contains(body, string(a.Occurrence.RawBody)) {
				t.Error("o corpo precisa carregar os dados crus do Sentry")
			}
			if !strings.Contains(body, "a sessão foi encerrada com SIGTERM") {
				t.Error("o corpo precisa carregar o detalhe da falha")
			}

			title := tracker.BuildTitle(a)
			if !strings.HasPrefix(title, tracker.NotAnalysedTag) {
				t.Errorf("o título precisa avisar no board: %q", title)
			}
			if !strings.Contains(title, "TypeError") {
				t.Errorf("o título precisa manter o título da Ocorrência: %q", title)
			}
		})
	}
}

// Uma Análise que falhou depois da Triagem ainda sabe o que a Ocorrência é; dizer
// isso poupa metade do trabalho de quem for investigar à mão.
func TestTheFailureIssueKeepsWhatTheTriageAlreadyDecided(t *testing.T) {
	body := tracker.BuildBody(domain.Analysis{
		Occurrence:  occurrence(),
		State:       domain.StateFailed,
		Verdict:     domain.VerdictDefect,
		FailureKind: domain.AgentErrTimeout,
	})
	if !strings.Contains(body, string(domain.VerdictDefect)) {
		t.Errorf("o corpo precisa dizer o que a Triagem concluiu:\n%s", body)
	}
}

func TestABodyWithNoFailureIsNotMarkedAsUnanalysed(t *testing.T) {
	a := domain.Analysis{
		Occurrence: occurrence(),
		State:      domain.StatePublished,
		Verdict:    domain.VerdictDefect,
		Priority:   domain.PriorityHigh,
		Sessions: []domain.Session{
			{Kind: domain.SessionDiagnosis, Response: "## Causa provável\n\nfalta guarda em `cart.id`"},
		},
	}
	body := tracker.BuildBody(a)
	if strings.Contains(body, "NÃO realizada") {
		t.Errorf("análise publicada não pode se dizer não realizada:\n%s", body)
	}
	if strings.HasPrefix(tracker.BuildTitle(a), tracker.NotAnalysedTag) {
		t.Error("o título não pode carregar a marca de falha")
	}
}

func TestTheDefectBodyCarriesTheDiagnosisAndThePriorityAdjustment(t *testing.T) {
	body := tracker.BuildBody(domain.Analysis{
		Occurrence:        occurrence(),
		State:             domain.StatePublished,
		Verdict:           domain.VerdictDefect,
		Priority:          domain.PriorityHigh,
		PriorityRationale: "subi um nível: atinge o checkout, que é o caminho do dinheiro",
		Sessions: []domain.Session{
			{Kind: domain.SessionTriage, Response: "defeito"},
			{Kind: domain.SessionDiagnosis, Response: "## Causa provável\n\n`cart` chega nulo em `checkout.tsx:88`\n\n## Arquivos suspeitos\n\n- app/routes/checkout.tsx"},
		},
	})

	for _, want := range []string{
		"Diagnóstico",
		"Causa provável",
		"app/routes/checkout.tsx",
		"2 — High",
		"subi um nível",
		"acme-web",
		"production",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("o corpo precisa conter %q:\n%s", want, body)
		}
	}
}

func TestTheSuggestionBodyCarriesTheStudy(t *testing.T) {
	body := tracker.BuildBody(domain.Analysis{
		Occurrence: occurrence(),
		State:      domain.StatePublished,
		Verdict:    domain.VerdictSuggestion,
		Priority:   domain.PriorityLow,
		Sessions: []domain.Session{
			{Kind: domain.SessionStudy, Response: "## Viabilidade\n\nviável\n\n## Alternativas\n\n1. recomendada\n2. b\n3. c\n\n## Riscos\n\nnenhum"},
		},
	})

	for _, want := range []string{"Estudo", "Viabilidade", "Alternativas", "Riscos", "4 — Low"} {
		if !strings.Contains(body, want) {
			t.Errorf("o corpo precisa conter %q:\n%s", want, body)
		}
	}
}

// O texto da Ocorrência é escrito por usuário final: nada nele pode virar estrutura
// de markdown e quebrar a tabela.
func TestTheOccurrenceTextNeverBreaksTheTable(t *testing.T) {
	o := occurrence()
	o.Culprit = "a | b\nc"
	body := tracker.BuildBody(domain.Analysis{
		Occurrence: o, State: domain.StatePublished, Verdict: domain.VerdictDefect,
		Sessions: []domain.Session{{Kind: domain.SessionDiagnosis, Response: "texto"}},
	})
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| Culpado") {
			continue
		}
		// O pipe escapado (`\|`) é conteúdo; só os três livres separam colunas.
		if bars := strings.Count(strings.ReplaceAll(line, "\\|", ""), "|"); bars != 3 {
			t.Errorf("a linha da tabela ganhou colunas: %q", line)
		}
	}
}

// Contagem e usuários valem ~1 por construção quando o grupo acaba de nascer;
// mostrá-los ali seria inventar informação que ainda não existe.
func TestVolumeOnlyShowsUpWhenItMeansSomething(t *testing.T) {
	o := occurrence()
	o.Count, o.UserCount = 1, 1
	body := tracker.BuildBody(domain.Analysis{Occurrence: o, State: domain.StatePublished})
	if strings.Contains(body, "Eventos") || strings.Contains(body, "Pessoas atingidas") {
		t.Errorf("volume de ~1 não deveria aparecer:\n%s", body)
	}

	o.Count, o.UserCount = 5000, 300
	body = tracker.BuildBody(domain.Analysis{Occurrence: o, State: domain.StatePublished})
	if !strings.Contains(body, "5000") || !strings.Contains(body, "300") {
		t.Errorf("volume real precisa aparecer:\n%s", body)
	}
}

func TestTheCommentCarriesTheNewDataOfARecurrence(t *testing.T) {
	o := occurrence()
	o.Substatus = domain.SubstatusRegressed
	body := tracker.BuildComment(domain.Analysis{
		Occurrence: o,
		Verdict:    domain.VerdictDefect,
		Sessions:   []domain.Session{{Kind: domain.SessionDiagnosis, Response: "mesma causa de antes"}},
	})

	for _, want := range []string{"voltou", o.Permalink, "mesma causa de antes"} {
		if !strings.Contains(body, want) {
			t.Errorf("o comentário precisa conter %q:\n%s", want, body)
		}
	}
}

func TestAnOccurrenceWithoutATitleStillGetsOne(t *testing.T) {
	o := occurrence()
	o.Title = "   "
	got := tracker.BuildTitle(domain.Analysis{Occurrence: o, State: domain.StatePublished})
	if got == "" || !strings.Contains(got, o.IssueID) {
		t.Errorf("título = %q, quero algo identificável", got)
	}
}

// Uma Análise que morreu numa tentativa e passou na seguinte continua carregando o
// motivo da tentativa morta na linha do SQLite. Ler esses campos em vez do desfecho
// carimbaria "[não analisada]" numa issue que traz o Diagnóstico completo — e uma
// issue de falha indistinguível de uma analisada vale tanto quanto o contrário.
func TestAnAnalysisThatRecoveredIsNotMarkedAsUnanalysed(t *testing.T) {
	a := domain.Analysis{
		Occurrence:    occurrence(),
		State:         domain.StatePublished,
		Verdict:       domain.VerdictDefect,
		Attempts:      2,
		FailureKind:   domain.AgentErrTimeout,
		FailureReason: "a primeira sessão estourou o teto",
		Priority:      domain.PriorityHigh,
		Sessions: []domain.Session{
			{Kind: domain.SessionDiagnosis, Response: "## Causa provável\n\nfalta guarda em `cart.id`"},
		},
	}
	if tracker.Failed(a) {
		t.Error("o desfecho gravado é `publicada`; a falha da tentativa anterior não é o desfecho")
	}
	if title := tracker.BuildTitle(a); strings.HasPrefix(title, tracker.NotAnalysedTag) {
		t.Errorf("título = %q", title)
	}
	body := tracker.BuildBody(a)
	if strings.Contains(body, "NÃO realizada") {
		t.Errorf("o corpo diz que não foi analisada, mas traz o Diagnóstico:\n%s", body)
	}
	if !strings.Contains(body, "Causa provável") {
		t.Errorf("o corpo precisa trazer o Diagnóstico:\n%s", body)
	}
}

// A retentativa em curso ainda é falha: o desfecho `falhando` mantém a marca, para
// a issue que nascer nesse estado não se passar por analisada.
func TestAnAnalysisStillFailingKeepsTheMark(t *testing.T) {
	a := domain.Analysis{
		Occurrence:  occurrence(),
		State:       domain.StateFailing,
		FailureKind: domain.AgentErrRateLimit,
	}
	if !tracker.Failed(a) {
		t.Error("`falhando` é caminho de falha")
	}
	if !strings.HasPrefix(tracker.BuildTitle(a), tracker.NotAnalysedTag) {
		t.Error("o título precisa avisar no board")
	}
}
