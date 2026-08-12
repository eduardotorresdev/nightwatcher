package worker_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
	"github.com/eduardotorresdev/nightwatcher/internal/worker"
)

// A defeito is the whole point of the system: the Ocorrência arrives, the code is
// read, and the work shows up in the Destino already triaged and prioritised.
func TestAQueuedDefectBecomesAnIssueInTheDestinoWithTheFonteLabel(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	occ := occurrenceFrom(t, "defeito-error-new.json")
	an := enqueue(t, h.store, h.proj, occ)

	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas = %d, quero 1", n)
	}

	created := h.spy.Created()
	if len(created) != 1 {
		t.Fatalf("issues criadas = %d, quero 1", len(created))
	}
	issue := created[0]
	if issue.ProjectID != "linear-project-acme" {
		t.Errorf("issue nasceu no Project %q, quero linear-project-acme", issue.ProjectID)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "web" {
		t.Errorf("labels = %v, quero o rótulo da Fonte [web]", issue.Labels)
	}
	// defeito + level error, sem volume e sem reincidência: a regra determinística
	// pousa em 3 — Medium.
	if issue.Priority != domain.PriorityMedium {
		t.Errorf("prioridade = %s, quero %s", issue.Priority, domain.PriorityMedium)
	}
	if issue.AttachmentURL != occ.Permalink {
		t.Errorf("anexo = %q, quero o permalink da Ocorrência %q", issue.AttachmentURL, occ.Permalink)
	}
	for _, want := range []string{"Diagnóstico", "Causa provável", realPath, "Tooltip"} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("o corpo da issue precisa conter %q:\n%s", want, issue.Body)
		}
	}
	if strings.Contains(issue.Title, tracker.NotAnalysedTag) {
		t.Errorf("uma Análise concluída não pode carregar a marca de falha: %q", issue.Title)
	}

	got := h.analysis(t, an.ID)
	if got.State != domain.StatePublished {
		t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
	}
	if got.Verdict != domain.VerdictDefect {
		t.Errorf("desfecho = %q, quero %q", got.Verdict, domain.VerdictDefect)
	}
	if got.IssueID != issue.Ref.ID || got.IssueURL != issue.Ref.URL {
		t.Errorf("a Análise não guardou a issue: id=%q url=%q", got.IssueID, got.IssueURL)
	}
	// O que foi entregue ao agente e o que ele respondeu ficam gravados: é o que
	// `analysis show` imprime e a matéria-prima para diagnosticar triagem ruim.
	if len(got.Sessions) != 2 {
		t.Fatalf("sessões gravadas = %d, quero 2 (triagem e diagnóstico)", len(got.Sessions))
	}
	for _, s := range got.Sessions {
		if strings.TrimSpace(s.Prompt) == "" || strings.TrimSpace(s.Response) == "" {
			t.Errorf("sessão %q ficou sem prompt ou sem resposta", s.Kind)
		}
	}
	if h.work.prepared != 1 || h.work.released != 1 {
		t.Errorf("clone preparado %dx e liberado %dx, quero 1 e 1", h.work.prepared, h.work.released)
	}
}

// A sugestão segue pelo Estudo e nasce Low: pedido de recurso não compete em urgência
// com coisa quebrada.
func TestASuggestionBecomesAStudyBornAtTheLowestPriority(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictSuggestion)}
	h.agent.deep = []reply{studyReply()}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "sugestao-feedback.json"))

	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas = %d, quero 1", n)
	}
	issue, ok := h.spy.LastCreated()
	if !ok {
		t.Fatal("nenhuma issue criada para a sugestão")
	}
	if issue.Priority != domain.PriorityLow {
		t.Errorf("prioridade = %s, quero %s", issue.Priority, domain.PriorityLow)
	}
	for _, want := range []string{"Estudo", "Viabilidade", "Alternativas", "recomendada", "Riscos"} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("o corpo precisa conter %q:\n%s", want, issue.Body)
		}
	}
	if got := h.analysis(t, an.ID); got.Verdict != domain.VerdictSuggestion {
		t.Errorf("desfecho = %q, quero %q", got.Verdict, domain.VerdictSuggestion)
	}
}

// Encerrar não é apagar: dúvida e ruído somem do board e continuam consultáveis, que
// é o que permite auditar depois se a Triagem está acertando.
func TestQuestionAndNoiseCloseTheAnalysisWithNothingInTheDestino(t *testing.T) {
	for _, verdict := range []domain.Verdict{domain.VerdictQuestion, domain.VerdictNoise} {
		t.Run(string(verdict), func(t *testing.T) {
			h := newHarness(t)
			h.agent.triage = []reply{triageReply(verdict)}

			an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "ruido-error-instrumentacao.json"))

			if n := h.drain(t); n != 1 {
				t.Fatalf("análises concluídas = %d, quero 1", n)
			}
			if h.spy.CreatedCount() != 0 {
				t.Errorf("issues criadas = %d, quero nenhuma", h.spy.CreatedCount())
			}
			if h.spy.CommentCount() != 0 {
				t.Errorf("comentários = %d, quero nenhum", h.spy.CommentCount())
			}
			if h.work.prepared != 0 {
				t.Error("a Triagem não pode abrir o repositório: a sessão curta existe para ser barata")
			}

			got := h.analysis(t, an.ID)
			if got.State != domain.StateClosed {
				t.Errorf("estado = %q, quero %q", got.State, domain.StateClosed)
			}
			if got.Verdict != verdict {
				t.Errorf("desfecho = %q, quero %q", got.Verdict, verdict)
			}
			if len(got.Sessions) != 1 {
				t.Errorf("sessões = %d, quero só a da Triagem", len(got.Sessions))
			}
		})
	}
}

// A espera é escrita como um available_at no futuro, nunca como goroutine dormindo:
// é isso que faz o backoff sobreviver a um SIGKILL e ser medível num Clock falso.
func TestARetryableFailureComesBackOnlyAfterTheDecidedWait(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{
		agentFailing(domain.AgentErrRateLimit, "o limite de uso da assinatura estourou"),
		triageReply(domain.VerdictDefect),
	}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	if n := h.drain(t); n != 0 {
		t.Fatalf("análises concluídas = %d, quero 0: a primeira tentativa falhou", n)
	}
	row := h.queue(t, an.ID)
	if row.done {
		t.Fatal("o item saiu da fila numa falha retentável — é assim que uma Ocorrência some")
	}
	if row.attempts != 1 {
		t.Errorf("tentativas = %d, quero 1", row.attempts)
	}
	if want := t0.Add(time.Minute); !row.availableAt.Equal(want) {
		t.Errorf("volta em %s, quero %s (primeira espera do backoff)", row.availableAt, want)
	}
	if got := h.analysis(t, an.ID); got.State != domain.StateFailing {
		t.Errorf("estado = %q, quero %q", got.State, domain.StateFailing)
	}

	// Antes da hora ninguém encosta nela.
	h.clock.Advance(30 * time.Second)
	if n := h.drain(t); n != 0 {
		t.Fatalf("análises concluídas = %d antes da espera terminar", n)
	}
	if calls := h.agent.callCount(); calls != 1 {
		t.Fatalf("sessões abertas = %d, quero 1: a espera ainda não venceu", calls)
	}

	h.clock.Advance(30 * time.Second)
	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas = %d, quero 1 depois da espera", n)
	}
	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1", h.spy.CreatedCount())
	}
	if got := h.analysis(t, an.ID); got.State != domain.StatePublished {
		t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
	}
}

// O defeito que matou o antecessor: `run falhou`, e a Ocorrência sumia. Esgotadas as
// tentativas, ela ainda vira issue — marcada, com o motivo e com os dados crus.
func TestWhenTheAttemptsRunOutTheOccurrenceStillBecomesAnUnanalysedIssue(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{agentFailing(domain.AgentErrRateLimit, "o limite de uso da assinatura estourou")}

	occ := occurrenceFrom(t, "defeito-error-new.json")
	an := enqueue(t, h.store, h.proj, occ)

	// Quatro tentativas, esperando 1, 5 e 15 minutos entre elas.
	for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute} {
		if n := h.drain(t); n != 0 {
			t.Fatalf("tentativa %d: análises concluídas = %d, quero 0", i+1, n)
		}
		row := h.queue(t, an.ID)
		if row.done {
			t.Fatalf("tentativa %d: o item saiu da fila antes de esgotar as tentativas", i+1)
		}
		if row.attempts != i+1 {
			t.Errorf("tentativa %d: tentativas gravadas = %d", i+1, row.attempts)
		}
		if want := h.clock.Now().Add(wait); !row.availableAt.Equal(want) {
			t.Errorf("tentativa %d: volta em %s, quero %s", i+1, row.availableAt, want)
		}
		h.clock.Advance(wait)
	}

	if n := h.drain(t); n != 1 {
		t.Fatalf("a quarta tentativa precisa encerrar a Análise, concluídas = %d", n)
	}

	created := h.spy.Created()
	if len(created) != 1 {
		t.Fatalf("issues criadas = %d, quero exatamente 1 — a Ocorrência não pode sumir nem duplicar", len(created))
	}
	issue := created[0]
	if !strings.HasPrefix(issue.Title, tracker.NotAnalysedTag) {
		t.Errorf("o título precisa avisar no board que não foi analisada: %q", issue.Title)
	}
	if !strings.Contains(issue.Title, "ReferenceError") {
		t.Errorf("o título precisa manter o título da Ocorrência: %q", issue.Title)
	}
	for _, want := range []string{
		"NÃO realizada",
		string(domain.AgentErrRateLimit),
		"o limite de uso da assinatura estourou",
		occ.Permalink,
		`"issueCategory": "error"`, // os dados crus do Sentry, inteiros
	} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("o corpo da issue de falha precisa conter %q:\n%s", want, issue.Body)
		}
	}

	got := h.analysis(t, an.ID)
	if got.State != domain.StateFailed {
		t.Errorf("estado = %q, quero %q", got.State, domain.StateFailed)
	}
	if got.Attempts != 4 {
		t.Errorf("tentativas gravadas = %d, quero 4", got.Attempts)
	}
	if got.FailureKind != domain.AgentErrRateLimit {
		t.Errorf("motivo gravado = %q, quero %q", got.FailureKind, domain.AgentErrRateLimit)
	}
	if got.IssueID == "" {
		t.Error("a Análise falhada precisa apontar para a issue que a registrou")
	}
	if !h.queue(t, an.ID).done {
		t.Error("o item precisa sair da fila depois de virar issue de falha")
	}

	// E não nasce uma segunda issue quando o worker olhar de novo.
	h.clock.Advance(time.Hour)
	if n := h.drain(t); n != 0 {
		t.Errorf("nova passagem concluiu %d itens, quero 0", n)
	}
	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d depois de nova passagem, quero 1", h.spy.CreatedCount())
	}
}

// Resposta malformada e falha de autenticação produzem o mesmo resultado se repetidas,
// então repetir só queima orçamento: vão direto para o caminho de falha.
func TestANonRetryableFailureGoesStraightToTheFailurePath(t *testing.T) {
	cases := []struct {
		name   string
		reply  reply
		reason string
	}{
		{
			name:   "resposta malformada",
			reply:  reply{text: "desculpe, não consegui classificar essa ocorrência"},
			reason: string(domain.AgentErrMalformed),
		},
		{
			name:   "falha de autenticação",
			reply:  agentFailing(domain.AgentErrAuth, "o token da assinatura foi recusado"),
			reason: string(domain.AgentErrAuth),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.agent.triage = []reply{tc.reply}

			an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

			if n := h.drain(t); n != 1 {
				t.Fatalf("análises concluídas = %d, quero 1 já na primeira passagem", n)
			}
			if calls := h.agent.callCount(); calls != 1 {
				t.Errorf("sessões abertas = %d, quero 1: repetir daria o mesmo resultado", calls)
			}
			issue, ok := h.spy.LastCreated()
			if !ok {
				t.Fatal("a Ocorrência precisa virar issue mesmo sem análise")
			}
			if !strings.HasPrefix(issue.Title, tracker.NotAnalysedTag) {
				t.Errorf("título = %q, quero a marca de não analisada", issue.Title)
			}
			if !strings.Contains(issue.Body, tc.reason) {
				t.Errorf("o corpo precisa nomear o motivo %q:\n%s", tc.reason, issue.Body)
			}
			got := h.analysis(t, an.ID)
			if got.State != domain.StateFailed {
				t.Errorf("estado = %q, quero %q", got.State, domain.StateFailed)
			}
			if got.Attempts != 1 {
				t.Errorf("tentativas = %d, quero 1: não se gastam quatro num erro que não muda", got.Attempts)
			}
		})
	}
}

// A Ocorrência voltou com a issue ainda aberta: o histórico fica junto, num comentário,
// e nada é criado.
func TestAnEscalationCommentsOnTheOpenIssueAndCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	first := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-volume.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a primeira Ocorrência precisa virar issue, concluídas = %d", n)
	}
	issue, _ := h.spy.LastCreated()

	h.clock.Advance(2 * time.Hour)
	escalation := enqueue(t, h.store, h.proj, occurrenceFrom(t, "escalada-do-defeito-error-volume.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a escalada precisa ser concluída, concluídas = %d", n)
	}

	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1: escalada comenta, não cria", h.spy.CreatedCount())
	}
	comments := h.spy.Comments(issue.Ref.ID)
	if len(comments) != 1 {
		t.Fatalf("comentários na issue = %d, quero 1", len(comments))
	}
	if !strings.Contains(comments[0], "escalou") {
		t.Errorf("o comentário precisa dizer que a Ocorrência escalou:\n%s", comments[0])
	}

	got := h.analysis(t, escalation.ID)
	if got.IssueID != issue.Ref.ID {
		t.Errorf("a Análise da escalada aponta para %q, quero a issue existente %q", got.IssueID, issue.Ref.ID)
	}
	if got.State != domain.StatePublished {
		t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
	}
	// 133 pessoas atingidas: na escalada o volume finalmente é real e a banda manda.
	recorded := h.analysis(t, first.ID).Priority
	if !got.Priority.MoreUrgentThan(recorded) {
		t.Errorf("prioridade da escalada = %s, precisa superar a registrada %s", got.Priority, recorded)
	}
	if got.Priority != domain.PriorityHigh {
		t.Errorf("prioridade = %s, quero %s", got.Priority, domain.PriorityHigh)
	}
}

// Algo dado como resolvido quebrou de novo: issue nova, referenciando a anterior, mais
// urgente. Reabrir issue fechada não é nosso papel.
func TestARegressionOverAClosedIssueBecomesANewRelatedAndMoreUrgentIssue(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	first := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a primeira Ocorrência precisa virar issue, concluídas = %d", n)
	}
	original, _ := h.spy.LastCreated()
	h.spy.Close(original.Ref.ID)

	h.clock.Advance(24 * time.Hour)
	regression := enqueue(t, h.store, h.proj, occurrenceFrom(t, "regressao-do-defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a regressão precisa ser concluída, concluídas = %d", n)
	}

	created := h.spy.Created()
	if len(created) != 2 {
		t.Fatalf("issues criadas = %d, quero 2", len(created))
	}
	novel := created[1]
	if novel.RelatedTo != original.Ref.ID {
		t.Errorf("a issue nova referencia %q, quero a anterior %q", novel.RelatedTo, original.Ref.ID)
	}
	if !novel.Priority.MoreUrgentThan(original.Priority) {
		t.Errorf("prioridade da regressão = %s, precisa superar %s", novel.Priority, original.Priority)
	}
	if h.spy.CommentCount() != 0 {
		t.Errorf("comentários = %d, quero nenhum: a issue anterior estava fechada", h.spy.CommentCount())
	}

	first = h.analysis(t, first.ID)
	got := h.analysis(t, regression.ID)
	if got.IssueID != novel.Ref.ID {
		t.Errorf("a Análise da regressão aponta para %q, quero %q", got.IssueID, novel.Ref.ID)
	}
	if first.IssueID == got.IssueID {
		t.Error("a regressão não pode reaproveitar a issue fechada")
	}
}

// Carga alta vira atraso, nunca perda — e esperar não é tentar: o excedente não pode
// gastar uma das quatro tentativas da Análise.
func TestThePerProjectHourlyCapMakesTheExcessWaitWithoutSpendingAnAttempt(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	if err := h.store.SetTunable(ctx, "per_project_hourly", "1"); err != nil {
		t.Fatalf("SetTunable: %v", err)
	}

	// Uma Análise deste Projeto já começou dentro da janela — e já terminou, então não
	// está na fila: o que segura o excedente é a janela, não a concorrência.
	spent, err := h.store.CreateAnalysis(ctx, domain.Analysis{
		ProjectID:  h.proj.ID,
		SourceID:   h.proj.Sources[0].ID,
		Occurrence: occurrenceFrom(t, "defeito-error-new-2.json"),
		State:      domain.StatePublished,
	}, t0.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}
	if _, err := h.store.AppendSession(ctx, spent.ID, domain.Session{
		Kind:      domain.SessionTriage,
		Prompt:    "prompt",
		Response:  "resposta",
		StartedAt: t0.Add(-10 * time.Minute),
		EndedAt:   t0.Add(-9 * time.Minute),
	}); err != nil {
		t.Fatalf("AppendSession: %v", err)
	}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	if n := h.drain(t); n != 0 {
		t.Fatalf("análises concluídas = %d, quero 0 com o teto estourado", n)
	}
	if h.spy.CreatedCount() != 0 || h.agent.callCount() != 0 {
		t.Fatalf("nada podia ter rodado: issues=%d sessões=%d", h.spy.CreatedCount(), h.agent.callCount())
	}

	row := h.queue(t, an.ID)
	if row.done {
		t.Fatal("o excedente foi descartado; ele tem que esperar na fila")
	}
	if row.owner != "" {
		t.Errorf("o item continua preso a %q em vez de voltar para a fila", row.owner)
	}
	if row.attempts != 0 {
		t.Errorf("tentativas = %d, quero 0: esperar pelo teto não é tentar", row.attempts)
	}
	if !row.availableAt.After(t0) {
		t.Errorf("volta em %s, precisa ser depois de %s", row.availableAt, t0)
	}

	// Passada a janela, ela roda inteira — o atraso não custou nada além do tempo.
	h.clock.Advance(time.Hour)
	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas depois da janela = %d, quero 1", n)
	}
	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1", h.spy.CreatedCount())
	}
	if got := h.analysis(t, an.ID); got.Attempts != 1 {
		t.Errorf("tentativas gastas = %d, quero 1", got.Attempts)
	}
}

// O teste do deploy do Dokku: o processo morre no meio e o que estava em voo volta a
// ser tomável na hora, sem esperar o lease vencer.
func TestAShutdownMidAnalysisHandsTheItemBackAtOnce(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.agent.beforeRun = func(domain.AgentRequest) { cancel() }
	h.agent.waitForCancel = true

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	n, err := h.pool.DrainOnce(ctx)
	if err != nil && ctx.Err() == nil {
		t.Fatalf("DrainOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("análises concluídas = %d, quero 0: o processo caiu no meio", n)
	}

	row := h.queue(t, an.ID)
	if row.done {
		t.Fatal("o item sumiu da fila num restart — é a janela cega que a v1 existe para fechar")
	}
	if row.owner != "" {
		t.Errorf("o item continua preso a %q; deveria ter voltado limpo", row.owner)
	}
	if row.availableAt.After(h.clock.Now()) {
		t.Errorf("volta só em %s; deveria ser tomável agora", row.availableAt)
	}
	if got := h.analysis(t, an.ID); got.State != domain.StateQueued {
		t.Errorf("estado = %q, quero %q: ninguém está analisando", got.State, domain.StateQueued)
	}
	if h.spy.CreatedCount() != 0 {
		t.Error("um restart não pode virar issue de falha")
	}

	// Um processo novo assume e termina o trabalho.
	h.agent.beforeRun = nil
	h.agent.waitForCancel = false
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	fresh := h.newPool(t, "worker-b")
	got, err := fresh.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("DrainOnce no processo novo: %v", err)
	}
	if got != 1 {
		t.Fatalf("análises concluídas pelo processo novo = %d, quero 1", got)
	}
	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1", h.spy.CreatedCount())
	}
}

// O ajuste do agente entra por último, vale um nível e exige justificativa escrita —
// que vai para a issue, porque é lendo essas frases que a regra é calibrada.
func TestTheAgentsPriorityAdjustmentMovesOneLevelAndIsWrittenOnTheIssue(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "subir", "perde pedido salvo sem avisar o usuário")}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas = %d, quero 1", n)
	}

	issue, _ := h.spy.LastCreated()
	if issue.Priority != domain.PriorityHigh {
		t.Errorf("prioridade = %s, quero %s (3 — Medium subindo um nível)", issue.Priority, domain.PriorityHigh)
	}
	if !strings.Contains(issue.Body, "perde pedido salvo sem avisar o usuário") {
		t.Errorf("a justificativa do ajuste precisa aparecer na issue:\n%s", issue.Body)
	}
	if got := h.analysis(t, an.ID); got.Priority != domain.PriorityHigh {
		t.Errorf("prioridade gravada = %s, quero %s", got.Priority, domain.PriorityHigh)
	}
}

// Caminho inventado é a falha clássica da sessão que lê código, e chegar ao board sem
// marcação manda alguém procurar um arquivo que nunca existiu.
func TestAHallucinatedPathReachesTheBoardMarkedAsSuch(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply("src/nao/existe/EsseArquivo.svelte", "manter", "")}

	enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas = %d, quero 1", n)
	}

	issue, _ := h.spy.LastCreated()
	if !strings.Contains(issue.Body, "não existe no repositório") {
		t.Errorf("o corpo precisa desconfiar do caminho inventado:\n%s", issue.Body)
	}
}

// Falhar em alcançar o Linear é o único caso em que o item TEM que continuar na fila:
// não há outro lugar onde registrar, e soltá-lo aqui é exatamente como uma Ocorrência
// some.
func TestADestinoThatCannotBeReachedKeepsTheOccurrenceInTheQueue(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}
	h.spy.FailCreate(&tracker.APIError{Op: "issueCreate", StatusCode: 400, Code: "RATELIMITED", Message: "cota estourada"})

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	if n := h.drain(t); n != 0 {
		t.Fatalf("análises concluídas = %d, quero 0 com o Destino fora do ar", n)
	}
	row := h.queue(t, an.ID)
	if row.done {
		t.Fatal("o item saiu da fila com o Destino fora do ar: a Ocorrência sumiu")
	}
	if !row.availableAt.After(t0) {
		t.Errorf("volta em %s, precisa esperar antes de tentar de novo", row.availableAt)
	}

	h.spy.FailCreate(nil)
	h.clock.Advance(time.Hour)
	if n := h.drain(t); n != 1 {
		t.Fatalf("com o Destino de volta, concluídas = %d, quero 1", n)
	}
	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1", h.spy.CreatedCount())
	}
	if got := h.analysis(t, an.ID); got.State != domain.StatePublished {
		t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
	}
}

// Dois processos drenando o mesmo banco é o estado normal durante um deploy do Dokku.
// Nenhuma Ocorrência pode ser analisada duas vezes, e nenhuma pode ficar para trás.
func TestTwoPoolsDrainingTheSameDatabaseNeverProcessTheSameItemTwice(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	const items = 6
	ids := make([]string, 0, items)
	base := occurrenceFrom(t, "defeito-error-new.json")
	for i := 0; i < items; i++ {
		occ := base
		occ.IssueID = fmt.Sprint(7700000000 + i)
		occ.Title = fmt.Sprintf("ReferenceError: Tooltip is not defined (%d)", i)
		ids = append(ids, enqueue(t, h.store, h.proj, occ).ID)
	}

	other := h.newPool(t, "worker-b")

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for _, pool := range []interface {
		DrainOnce(context.Context) (int, error)
	}{h.pool, other} {
		wg.Add(1)
		go func(p interface {
			DrainOnce(context.Context) (int, error)
		}) {
			defer wg.Done()
			n, err := p.DrainOnce(ctx)
			if err != nil {
				t.Errorf("DrainOnce: %v", err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}(pool)
	}
	wg.Wait()

	if total != items {
		t.Errorf("itens concluídos = %d, quero %d", total, items)
	}
	if h.spy.CreatedCount() != items {
		t.Errorf("issues criadas = %d, quero %d — uma por Ocorrência", h.spy.CreatedCount(), items)
	}
	for _, id := range ids {
		got := h.analysis(t, id)
		if got.State != domain.StatePublished {
			t.Errorf("análise %s ficou em %q", id, got.State)
		}
		if len(got.Sessions) != 2 {
			t.Errorf("análise %s tem %d sessões, quero 2: mais que isso é análise repetida", id, len(got.Sessions))
		}
	}
}

// Run é o daemon: drena enquanto houver fila, dorme quando não há, e para limpo
// quando o contexto é cancelado — que é como todo deploy do Dokku termina.
func TestRunDrainsTheQueueAndStopsCleanlyWhenCancelled(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	signal := &signallingTracker{Spy: h.spy, created: make(chan struct{})}
	pool, err := worker.New(worker.Deps{
		Store:     h.store,
		Agent:     h.agent,
		Tracker:   signal,
		Workspace: h.work,
		Clock:     h.clock,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Owner:     "worker-run",
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- pool.Run(ctx) }()

	select {
	case <-signal.created:
	case <-time.After(10 * time.Second):
		t.Fatal("Run não drenou a fila")
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Run devolveu %v; parar por cancelamento é parada limpa, não erro", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run não voltou depois do cancelamento")
	}

	if got := h.analysis(t, an.ID); got.State != domain.StatePublished {
		t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
	}
	if !h.queue(t, an.ID).done {
		t.Error("o item precisa sair da fila: trabalho concluído no meio de um deploy continua concluído")
	}
}

// O teto de simultâneas existe para uma sequência de Ocorrências não saturar a VPS nem
// estourar o limite de uso do Claude.
//
// A prova é a fila no SQLite, e não um contador dentro do teste: enquanto o teto está
// cheio e as sessões estão presas, o Pool está bloqueado ANTES do Claim, então as
// Ocorrências excedentes continuam com owner nulo na tabela. Um contador de pico não
// serve — ele passa com um Pool sem teto nenhum, porque as goroutines a mais podem
// simplesmente não se cruzar.
func TestNoMoreAnalysesRunAtOnceThanTheConcurrencyAllows(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictNoise)}

	const (
		limit = 2 // config.Defaults().Concurrency
		items = 6
	)
	var (
		entered = make(chan struct{}, items) // uma ficha por sessão que começou
		release = make(chan struct{})        // fechado quando o teste já observou a fila
	)
	h.agent.beforeRun = func(domain.AgentRequest) {
		entered <- struct{}{}
		<-release
	}

	base := occurrenceFrom(t, "ruido-error-instrumentacao.json")
	for i := 0; i < items; i++ {
		occ := base
		occ.IssueID = fmt.Sprint(7800000000 + i)
		enqueue(t, h.store, h.proj, occ)
	}

	drained := make(chan int, 1)
	go func() {
		n, err := h.pool.DrainOnce(ctx)
		if err != nil {
			t.Errorf("DrainOnce: %v", err)
		}
		drained <- n
	}()

	// Espera o teto encher. Cada uma dessas sessões está parada dentro do agente, de
	// modo que o Pool só pode ter avançado até tentar ocupar a vaga seguinte.
	for i := 0; i < limit; i++ {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("só %d sessões começaram, quero %d", i, limit)
		}
	}

	var held int
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM queue WHERE owner IS NOT NULL AND done_at IS NULL`).Scan(&held); err != nil {
		t.Fatalf("contando itens presos: %v", err)
	}
	if held != limit {
		t.Errorf("itens presos por um worker = %d, quero %d: o excedente tinha que continuar esperando na fila", held, limit)
	}

	close(release)
	select {
	case n := <-drained:
		if n != items {
			t.Errorf("análises concluídas = %d, quero %d", n, items)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a passada não terminou depois de soltar as sessões")
	}
}

// Uma Análise que morreu numa tentativa e passou na seguinte continua carregando o
// motivo da tentativa morta nos campos da linha do SQLite. Se o desfecho não estiver
// gravado quando a issue é montada, `tracker.Failed` cai nesses campos velhos e a
// issue nasce marcada "[não analisada]" com o corpo de falha no lugar do Diagnóstico
// que acabou de ser produzido — barulho falso no board e a análise inteira perdida.
func TestAnAnalysisThatRecoveredFromARetryReachesTheBoardAsAnalysed(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{
		agentFailing(domain.AgentErrRateLimit, "o limite de uso da assinatura estourou"),
		triageReply(domain.VerdictDefect),
	}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 0 {
		t.Fatalf("a primeira tentativa precisa falhar, concluídas = %d", n)
	}
	h.clock.Advance(time.Minute)
	if n := h.drain(t); n != 1 {
		t.Fatalf("a segunda tentativa precisa concluir, concluídas = %d", n)
	}

	issue, ok := h.spy.LastCreated()
	if !ok {
		t.Fatal("nenhuma issue nasceu")
	}
	if strings.HasPrefix(issue.Title, tracker.NotAnalysedTag) {
		t.Errorf("título = %q: a tentativa que falhou não é o desfecho", issue.Title)
	}
	if strings.Contains(issue.Body, "NÃO realizada") {
		t.Errorf("o corpo se diz não analisado, mas o Diagnóstico foi feito:\n%s", issue.Body)
	}
	for _, want := range []string{"Causa provável", realPath} {
		if !strings.Contains(issue.Body, want) {
			t.Errorf("o corpo precisa trazer o Diagnóstico (%q):\n%s", want, issue.Body)
		}
	}
	if got := h.analysis(t, an.ID); got.State != domain.StatePublished {
		t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
	}
}

// O processo caiu entre a issue nascer no Destino e o item sair da fila. Rodar de novo
// abriria uma segunda issue para a mesma Ocorrência — o barulho que destrói a
// confiança no board.
func TestAnAnalysisThatAlreadyEndedOnlyHasItsQueueItemClosed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("concluídas = %d, quero 1", n)
	}
	published := h.analysis(t, an.ID)

	// O item que o SIGKILL deixou aberto, com a Análise já publicada.
	if _, err := h.store.Enqueue(ctx, an.ID, h.clock.Now(), time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	sessionsBefore := h.agent.callCount()

	if n := h.drain(t); n != 1 {
		t.Fatalf("o item órfão precisa ser fechado, concluídas = %d", n)
	}
	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1: a Análise já tinha desfecho", h.spy.CreatedCount())
	}
	if got := h.agent.callCount(); got != sessionsBefore {
		t.Errorf("sessões abertas = %d, quero %d: não se paga uma sessão por um desfecho já gravado", got, sessionsBefore)
	}
	if !h.queue(t, an.ID).done {
		t.Error("o item órfão precisa sair da fila")
	}
	if got := h.analysis(t, an.ID); got.IssueID != published.IssueID {
		t.Errorf("a Análise passou a apontar para %q, quero continuar em %q", got.IssueID, published.IssueID)
	}
}

// As tentativas acabaram e a falha ficou gravada, mas o Destino estava fora do ar na
// hora de publicá-la. Só falta publicar: gastar mais uma sessão de vinte minutos numa
// Análise já desistida queima a assinatura para chegar ao mesmo lugar.
func TestAnAnalysisAlreadyGivenUpOnIsOnlyPublishedAndNeverAnalysedAgain(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}

	an, err := h.store.CreateAnalysis(ctx, domain.Analysis{
		ProjectID:     h.proj.ID,
		SourceID:      h.proj.Sources[0].ID,
		Occurrence:    occurrenceFrom(t, "defeito-error-new.json"),
		State:         domain.StateFailed,
		Verdict:       domain.VerdictDefect,
		Attempts:      4,
		FailureKind:   domain.AgentErrTimeout,
		FailureReason: "a sessão passou de 20m sem terminar",
	}, t0)
	if err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}
	if _, err := h.store.Enqueue(ctx, an.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if n := h.drain(t); n != 1 {
		t.Fatalf("concluídas = %d, quero 1", n)
	}
	if calls := h.agent.callCount(); calls != 0 {
		t.Errorf("sessões abertas = %d, quero 0", calls)
	}
	issue, ok := h.spy.LastCreated()
	if !ok {
		t.Fatal("a Ocorrência precisa virar issue mesmo sem análise")
	}
	if !strings.HasPrefix(issue.Title, tracker.NotAnalysedTag) {
		t.Errorf("título = %q, quero a marca de não analisada", issue.Title)
	}
	if !strings.Contains(issue.Body, string(domain.AgentErrTimeout)) {
		t.Errorf("o corpo precisa nomear o motivo:\n%s", issue.Body)
	}
	if got := h.analysis(t, an.ID); got.IssueID == "" {
		t.Error("a Análise precisa apontar para a issue que a registrou")
	}
}

// Um Diagnóstico tem vinte minutos e o lease vale vinte e cinco. Sem a renovação, uma
// sessão que usa todo o orçamento é tomada por outro worker no meio do caminho — duas
// Análises da mesma Ocorrência e duas issues.
func TestTheLeaseIsRenewedSoALongSessionIsNotTakenOverMidFlight(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.store.SetTunable(ctx, "lease_ttl", "3m"); err != nil {
		t.Fatalf("SetTunable: %v", err)
	}
	h.agent.triage = []reply{triageReply(domain.VerdictNoise)}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.agent.beforeRun = func(domain.AgentRequest) {
		once.Do(func() { close(entered) })
		<-release
	}

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	drained := make(chan int, 1)
	go func() {
		n, err := h.pool.DrainOnce(ctx)
		if err != nil {
			t.Errorf("DrainOnce: %v", err)
		}
		drained <- n
	}()

	<-entered
	h.clock.awaitSleeper(t) // a renovação já está esperando o primeiro terço do lease

	// Quatro minutos com a sessão presa: sem renovação o lease de três minutos teria
	// vencido há um minuto. Cada volta só volta a esperar depois de ter renovado, então
	// receber o próximo sleeper é prova de que a renovação foi gravada.
	for i := 0; i < 4; i++ {
		h.clock.Advance(time.Minute)
		h.clock.awaitSleeper(t)
	}

	var leaseUntil int64
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT lease_until FROM queue WHERE analysis_id = ?`, an.ID).Scan(&leaseUntil); err != nil {
		t.Fatalf("lendo o lease: %v", err)
	}
	if got := time.UnixMilli(leaseUntil).UTC(); !got.After(h.clock.Now()) {
		t.Errorf("o lease venceu em %s com a sessão ainda rodando em %s", got, h.clock.Now())
	}

	other := h.newPool(t, "worker-b")
	if n, err := other.DrainOnce(ctx); err != nil || n != 0 {
		t.Errorf("outro worker levou %d itens (err=%v); a Análise está em curso", n, err)
	}
	if h.spy.CreatedCount() != 0 {
		t.Errorf("issues criadas = %d por um segundo worker", h.spy.CreatedCount())
	}

	close(release)
	select {
	case n := <-drained:
		if n != 1 {
			t.Errorf("concluídas = %d, quero 1", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a passada não terminou")
	}
}

// Reincidência com a issue anterior AINDA ABERTA não é o caso da User Story 15: o
// histórico pertence junto, então é comentário. Criar uma segunda issue aberta para o
// mesmo grupo é duplicata no board.
func TestARegressionWhileThePreviousIssueIsStillOpenCommentsInsteadOfCreatingASecond(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a primeira Ocorrência precisa virar issue, concluídas = %d", n)
	}
	original, _ := h.spy.LastCreated()

	h.clock.Advance(24 * time.Hour)
	regression := enqueue(t, h.store, h.proj, occurrenceFrom(t, "regressao-do-defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a regressão precisa ser concluída, concluídas = %d", n)
	}

	if h.spy.CreatedCount() != 1 {
		t.Errorf("issues criadas = %d, quero 1: a issue anterior continua aberta", h.spy.CreatedCount())
	}
	if got := len(h.spy.Comments(original.Ref.ID)); got != 1 {
		t.Errorf("comentários na issue aberta = %d, quero 1", got)
	}
	if got := h.analysis(t, regression.ID); got.IssueID != original.Ref.ID {
		t.Errorf("a Análise da regressão aponta para %q, quero a issue existente %q", got.IssueID, original.Ref.ID)
	}
}

// A espera diante de um Destino fora do ar sai do próprio erro: o estouro de cota do
// Linear chega como HTTP 400 e carrega o instante em que a cota volta. Um cliente que
// leia só o status trata cota como erro permanente e desiste — e, do outro lado, uma
// credencial recusada não se conserta em um minuto e não pode ficar batendo à porta.
func TestTheWaitForAnUnreachableDestinoComesFromWhatTheLinearErrorSays(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{
			name: "cota estourada, com a janela conhecida",
			err:  &tracker.APIError{Op: "issueCreate", StatusCode: 400, Code: "RATELIMITED", RetryAfter: 90 * time.Second},
			want: 90 * time.Second,
		},
		{
			name: "cota estourada, sem a janela",
			err:  &tracker.APIError{Op: "issueCreate", StatusCode: 400, Code: "RATELIMITED"},
			want: time.Minute,
		},
		{
			name: "credencial recusada",
			err:  &tracker.APIError{Op: "issueCreate", StatusCode: 401, Code: "AUTHENTICATION_ERROR"},
			want: 15 * time.Minute,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
			h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}
			h.spy.FailCreate(tc.err)

			an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
			if n := h.drain(t); n != 0 {
				t.Fatalf("concluídas = %d, quero 0 com o Destino fora do ar", n)
			}

			row := h.queue(t, an.ID)
			if row.done {
				t.Fatal("o item saiu da fila com o Destino fora do ar: a Ocorrência sumiu")
			}
			if want := t0.Add(tc.want); !row.availableAt.Equal(want) {
				t.Errorf("volta em %s, quero %s", row.availableAt, want)
			}
		})
	}
}

// A escalada é a segunda passada da regra: o modificador de `escalating` já
// foi cobrado na criação e agora quem fala é a banda de volume. Contá-lo duas vezes
// tornaria a banda inalcançável — e a Prioridade nunca desce abaixo da que já está
// registrada na issue.
func TestAnEscalationIsPricedByTheEscalationPassAndNeverBelowWhatIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	first := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-volume.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("a primeira Ocorrência precisa virar issue, concluídas = %d", n)
	}
	if got := h.analysis(t, first.ID).Priority; got != domain.PriorityMedium {
		t.Fatalf("prioridade registrada = %s, quero %s", got, domain.PriorityMedium)
	}

	// A mesma escalada, mas com pouca gente atingida: abaixo da banda o volume não diz
	// nada, e a segunda passada não torna a cobrar o `escalating`.
	h.clock.Advance(2 * time.Hour)
	occ := occurrenceFrom(t, "escalada-do-defeito-error-volume.json")
	occ.UserCount = 2
	escalation := enqueue(t, h.store, h.proj, occ)
	if n := h.drain(t); n != 1 {
		t.Fatalf("a escalada precisa ser concluída, concluídas = %d", n)
	}

	got := h.analysis(t, escalation.ID)
	if got.Priority != domain.PriorityMedium {
		t.Errorf("prioridade = %s, quero %s: sem volume a escalada não sobe de novo", got.Priority, domain.PriorityMedium)
	}
	if got.Priority.MoreUrgentThan(domain.PriorityMedium) {
		t.Errorf("prioridade = %s: o modificador de escalada foi cobrado duas vezes", got.Priority)
	}
}

// A Prioridade não cai sozinha: quem já está registrado na issue foi decidido com
// informação que o daemon não tem — inclusive uma subida do próprio agente — e a
// segunda passada da regra nunca escreve um valor menos urgente que esse.
func TestAnEscalationNeverLowersThePriorityAlreadyRecorded(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{
		diagnosisReply(realPath, "subir", "perde pedido salvo sem avisar o usuário"),
		diagnosisReply(realPath, "manter", ""),
	}

	first := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-volume.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("concluídas = %d, quero 1", n)
	}
	recorded := h.analysis(t, first.ID).Priority
	if recorded != domain.PriorityHigh {
		t.Fatalf("prioridade registrada = %s, quero %s", recorded, domain.PriorityHigh)
	}

	// Escalada magra: a regra sozinha pousaria em Medium, abaixo do que já está na issue.
	h.clock.Advance(2 * time.Hour)
	occ := occurrenceFrom(t, "escalada-do-defeito-error-volume.json")
	occ.UserCount = 2
	escalation := enqueue(t, h.store, h.proj, occ)
	if n := h.drain(t); n != 1 {
		t.Fatalf("a escalada precisa ser concluída, concluídas = %d", n)
	}

	if got := h.analysis(t, escalation.ID).Priority; got != recorded {
		t.Errorf("prioridade da escalada = %s, quero %s: a urgência já registrada é o piso", got, recorded)
	}
}

// O ambiente não vem no payload e custa uma chamada de tags à parte. Ele
// vale a chamada porque é o que separa "quebrou em produção" de "quebrou no staging"
// para quem lê a issue — e não saber é estado normal, nunca motivo para falhar uma
// Análise.
func TestTheEnvironmentIsFetchedFromSentryAndSurvivesToTheIssue(t *testing.T) {
	occ := occurrenceFrom(t, "defeito-error-new.json")

	t.Run("quando o Sentry responde", func(t *testing.T) {
		h := newHarness(t)
		h.sentry = stubSentry{envs: map[string][]string{occ.IssueID: {"production"}}}
		h.pool = h.newPool(t, "worker-a")
		h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
		h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

		an := enqueue(t, h.store, h.proj, occ)
		if n := h.drain(t); n != 1 {
			t.Fatalf("concluídas = %d, quero 1", n)
		}
		if got := h.analysis(t, an.ID).Occurrence.Environment; got != "production" {
			t.Errorf("ambiente gravado = %q, quero production", got)
		}
		issue, _ := h.spy.LastCreated()
		if !strings.Contains(issue.Body, "production") {
			t.Errorf("o ambiente precisa chegar na issue:\n%s", issue.Body)
		}
	})

	t.Run("quando o Sentry não responde", func(t *testing.T) {
		h := newHarness(t)
		h.sentry = stubSentry{err: fmt.Errorf("sentry fora do ar")}
		h.pool = h.newPool(t, "worker-a")
		h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
		h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

		an := enqueue(t, h.store, h.proj, occ)
		if n := h.drain(t); n != 1 {
			t.Fatalf("concluídas = %d, quero 1: não saber o ambiente não falha uma Análise", n)
		}
		if got := h.analysis(t, an.ID); got.State != domain.StatePublished {
			t.Errorf("estado = %q, quero %q", got.State, domain.StatePublished)
		}
		if h.spy.CreatedCount() != 1 {
			t.Errorf("issues criadas = %d, quero 1", h.spy.CreatedCount())
		}
	})
}

// Um lease que vence no instante em que é tomado faz todo worker roubar o item de todo
// outro worker: duas Análises e duas issues para uma Ocorrência. `config set` recusa o
// valor na porta; este teste força a linha direto na tabela, como faria uma edição à mão
// do banco, e cobra que o Pool ainda assim não deixe a Análise em curso ser roubada.
func TestAnImpossibleLeaseTTLDoesNotLetASecondWorkerStealAnAnalysisInFlight(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.store.SetTunable(ctx, "lease_ttl", "0s"); err == nil {
		t.Error("`config set lease_ttl 0s` foi aceito; um lease não positivo é roubo garantido")
	}
	if err := h.store.SetSetting(ctx, "lease_ttl", "0s"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	h.agent.triage = []reply{triageReply(domain.VerdictNoise)}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.agent.beforeRun = func(domain.AgentRequest) {
		once.Do(func() { close(entered) })
		<-release
	}

	enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	drained := make(chan int, 1)
	go func() {
		n, err := h.pool.DrainOnce(ctx)
		if err != nil {
			t.Errorf("DrainOnce: %v", err)
		}
		drained <- n
	}()

	<-entered
	h.clock.Advance(time.Minute)

	other := h.newPool(t, "worker-b")
	if n, err := other.DrainOnce(ctx); err != nil || n != 0 {
		t.Errorf("outro worker levou %d itens (err=%v) de uma Análise em curso", n, err)
	}
	if h.spy.CreatedCount() != 0 {
		t.Errorf("issues criadas = %d por um segundo worker", h.spy.CreatedCount())
	}

	close(release)
	select {
	case n := <-drained:
		if n != 1 {
			t.Errorf("concluídas = %d, quero 1", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a passada não terminou")
	}
}

// O teto de tempo da sessão é do Analyst, não do Agent: `domain.Agent` deve honrar
// AgentRequest.Timeout, e este é o guarda para a implementação que não honra. Um slot de
// worker preso para sempre é a morte silenciosa do antecessor.
//
// O relógio é o injetado, e é isso que torna o teto de dois minutos da Triagem
// exercitável: com context.WithTimeout só um teste disposto a esperar dois minutos de
// verdade chegaria aqui, então nenhum chegava. O Agent aqui devolve um erro que não diz
// nada sobre tempo, de modo que o "timeout" no corpo da issue só pode ter vindo do teto.
func TestASessionThatIgnoresItsOwnCeilingIsCutOffAndRecordedAsATimeout(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.store.SetTunable(ctx, "max_attempts", "1"); err != nil {
		t.Fatalf("SetTunable: %v", err)
	}
	h.agent.hangs = true

	entered := make(chan struct{})
	var once sync.Once
	h.agent.beforeRun = func(domain.AgentRequest) { once.Do(func() { close(entered) }) }

	an := enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))

	drained := make(chan int, 1)
	go func() {
		n, err := h.pool.DrainOnce(ctx)
		if err != nil {
			t.Errorf("DrainOnce: %v", err)
		}
		drained <- n
	}()

	<-entered
	// Duas esperas registradas: a renovação do lease e o teto da sessão. Esperar as duas
	// antes de mexer no relógio é o que torna o avanço determinístico.
	h.clock.awaitSleeper(t)
	h.clock.awaitSleeper(t)

	// Teto da Triagem (2m) mais a folga (30s). Um segundo além e a sessão tem de acabar.
	h.clock.Advance(2*time.Minute + 31*time.Second)

	select {
	case n := <-drained:
		if n != 1 {
			t.Errorf("concluídas = %d, quero 1", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a sessão travada nunca foi cortada: o teto não é do relógio injetado")
	}

	if h.spy.CreatedCount() != 1 {
		t.Fatalf("issues criadas = %d, quero 1: falha do sistema nunca vira perda de Ocorrência", h.spy.CreatedCount())
	}
	issue := h.spy.Created()[0]
	if !strings.HasPrefix(issue.Title, tracker.NotAnalysedTag) {
		t.Errorf("título = %q, quero a marca de não analisada", issue.Title)
	}
	if !strings.Contains(issue.Body, string(domain.AgentErrTimeout)) {
		t.Errorf("o corpo precisa nomear o timeout — só o teto sabe que foi isso:\n%s", issue.Body)
	}
	got := h.analysis(t, an.ID)
	if got.State != domain.StateFailed {
		t.Errorf("estado = %q, quero %q", got.State, domain.StateFailed)
	}
	if got.FailureKind != domain.AgentErrTimeout {
		t.Errorf("motivo = %q, quero %q", got.FailureKind, domain.AgentErrTimeout)
	}
}

// A retomada de uma Análise já desistida precisa levar consigo o que a Ocorrência já
// produziu no Destino.
//
// O caminho do StateFailed publica direto, sem sessão — e por isso é fácil publicar
// sem antes procurar a issue anterior. Se ele publicar com um priorIssue vazio, a
// issue de falha nasce solta: quem abrir o board vê duas issues do mesmo grupo Sentry
// sem nada dizendo que são a mesma coisa, que é exatamente a confusão que o RelatedTo
// existe para evitar. O prejuízo é silencioso, porque a Ocorrência não se perde.
// A feedback arriving through the Alert Rule door has to reach the session with the
// words the person actually typed. The title Sentry writes is a summary in English
// ("User Feedback: Search field not appearing"); the complaint itself lives in
// `contexts.feedback.message`, in a place the `issue` door does not have, and a Triagem
// judging the summary instead of the complaint is judging a paraphrase.
func TestAFeedbackFromTheEventAlertDoorReachesTheSessionWithItsText(t *testing.T) {
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictSuggestion)}
	h.agent.deep = []reply{studyReply()}

	occ := occurrenceFrom(t, "feedback-por-event-alert.json")
	enqueue(t, h.store, h.proj, occ)
	if n := h.drain(t); n != 1 {
		t.Fatalf("concluídas = %d, quero 1", n)
	}

	reqs := h.agent.requests()
	if len(reqs) == 0 {
		t.Fatal("nenhuma sessão rodou")
	}
	triage := reqs[0]
	if triage.Kind != domain.SessionTriage {
		t.Fatalf("a primeira sessão foi %q, quero %q", triage.Kind, domain.SessionTriage)
	}
	for _, want := range []string{
		// O texto do usuário, verbatim.
		"campo de busca",
		// E o ambiente, que nesta porta vem de graça — sem a chamada de tags à parte.
		"prod",
	} {
		if !strings.Contains(triage.Prompt, want) {
			t.Errorf("o prompt da Triagem precisa conter %q:\n%s", want, triage.Prompt)
		}
	}
}

func TestAnAnalysisGivenUpOnStillPublishesLinkedToTheIssueTheGroupAlreadyHas(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	occ := occurrenceFrom(t, "defeito-error-new.json")
	enqueue(t, h.store, h.proj, occ)
	if n := h.drain(t); n != 1 {
		t.Fatalf("a primeira Ocorrência precisa virar issue, concluídas = %d", n)
	}
	original, ok := h.spy.LastCreated()
	if !ok {
		t.Fatal("a primeira Ocorrência não virou issue")
	}

	h.clock.Advance(2 * time.Hour)
	// O mesmo grupo Sentry — outra situação, porque a chave de dedup carrega o
	// substatus — numa Análise cujas tentativas já acabaram e que só não chegou ao
	// Destino.
	again := occ
	again.Action = domain.ActionUnresolved
	again.Substatus = domain.SubstatusRegressed
	failed, err := h.store.CreateAnalysis(ctx, domain.Analysis{
		ProjectID:     h.proj.ID,
		SourceID:      h.proj.Sources[0].ID,
		Occurrence:    again,
		State:         domain.StateFailed,
		Verdict:       domain.VerdictDefect,
		Attempts:      4,
		FailureKind:   domain.AgentErrTimeout,
		FailureReason: "a sessão passou de 20m sem terminar",
	}, h.clock.Now())
	if err != nil {
		t.Fatalf("CreateAnalysis: %v", err)
	}
	if _, err := h.store.Enqueue(ctx, failed.ID, h.clock.Now(), time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if n := h.drain(t); n != 1 {
		t.Fatalf("concluídas = %d, quero 1", n)
	}

	created := h.spy.Created()
	if len(created) != 2 {
		t.Fatalf("issues criadas = %d, quero 2: a falha precisa aparecer no board", len(created))
	}
	failure := created[1]
	if !strings.HasPrefix(failure.Title, tracker.NotAnalysedTag) {
		t.Errorf("título = %q, quero a marca de não analisada", failure.Title)
	}
	if failure.RelatedTo != original.Ref.ID {
		t.Errorf("a issue de falha referencia %q, quero a issue que o grupo já tem, %q",
			failure.RelatedTo, original.Ref.ID)
	}
	if got := h.analysis(t, failed.ID); got.IssueID == "" {
		t.Error("a Análise precisa apontar para a issue que a registrou")
	}
}

// A Triagem é uma sessão de um turno, sem clone e sem ferramenta: nenhum servidor MCP
// chega até ela. O Diagnóstico recebe a fusão — os globais mais os do Projeto, com o
// Projeto ganhando quando os dois nomeiam o mesmo servidor.
func TestOnlyTheLongSessionReceivesTheMCPServersMergedProjectFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, m := range []domain.MCPServer{
		{Name: "docs", Transport: domain.MCPTransportHTTP, URL: "https://global.exemplo.com/mcp"},
		{Name: "board", Transport: domain.MCPTransportHTTP, URL: "https://board.exemplo.com/mcp"},
		{ProjectID: h.proj.ID, Name: "docs", Transport: domain.MCPTransportHTTP, URL: "https://acme.exemplo.com/mcp"},
	} {
		if _, err := h.store.UpsertMCPServer(ctx, m, t0); err != nil {
			t.Fatalf("UpsertMCPServer(%s): %v", m.Name, err)
		}
	}

	h.agent.triage = []reply{triageReply(domain.VerdictDefect)}
	h.agent.deep = []reply{diagnosisReply(realPath, "manter", "")}

	enqueue(t, h.store, h.proj, occurrenceFrom(t, "defeito-error-new.json"))
	if n := h.drain(t); n != 1 {
		t.Fatalf("análises concluídas = %d, quero 1", n)
	}

	reqs := h.agent.requests()
	if len(reqs) != 2 {
		t.Fatalf("sessões = %d, quero 2 (triagem e diagnóstico)", len(reqs))
	}

	triage, deep := reqs[0], reqs[1]
	if triage.Kind != domain.SessionTriage {
		t.Fatalf("a primeira sessão foi %q, quero a Triagem", triage.Kind)
	}
	if len(triage.MCPServers) != 0 {
		t.Errorf("a Triagem recebeu %d servidor(es) MCP, quero nenhum: %v", len(triage.MCPServers), triage.MCPServers)
	}

	got := map[string]string{}
	for _, m := range deep.MCPServers {
		got[m.Name] = m.URL
	}
	want := map[string]string{
		"board": "https://board.exemplo.com/mcp",
		// o global de mesmo nome perdeu para o do Projeto
		"docs": "https://acme.exemplo.com/mcp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("servidores do Diagnóstico = %v, quero %v", got, want)
	}
}
