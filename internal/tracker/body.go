package tracker

import (
	"fmt"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// NotAnalysedTag é a marca que vai no título da issue de falha. Fica no título, e não
// só no corpo, porque é no board que ela precisa ser vista — uma issue de falha
// indistinguível de uma issue analisada é o defeito do antecessor de volta.
const NotAnalysedTag = "[não analisada]"

// Limites de montagem. O do título é INFERÊNCIA: a pesquisa não achou limite
// documentado, e truncar é mais barato que ter a mutation recusada. O do payload é
// legibilidade: ninguém lê 300 KB de JSON dentro de uma issue.
const (
	maxTitleRunes  = 240
	maxRawPayload  = 4000
	sentryTimeMask = "2006-01-02 15:04 UTC"
)

// BuildTitle monta o título da issue a partir da Análise.
func BuildTitle(a domain.Analysis) string {
	title := strings.TrimSpace(a.Occurrence.Title)
	if title == "" {
		title = "Ocorrência sem título"
		if id := strings.TrimSpace(a.Occurrence.IssueID); id != "" {
			title += " (" + id + ")"
		}
	}
	title = strings.Join(strings.Fields(title), " ")
	if Failed(a) {
		title = NotAnalysedTag + " " + title
	}
	return truncate(title, maxTitleRunes)
}

// BuildBody monta o corpo em markdown da issue.
//
// É função pura, separada do HTTP, porque é o conteúdo — e não a chamada — que
// precisa estar certo em cada desfecho: defeito traz causa provável e arquivos
// suspeitos, sugestão traz viabilidade, alternativas, riscos e dificuldades, e a
// falha traz os dados crus, o link e o motivo de não ter sido analisada.
func BuildBody(a domain.Analysis) string {
	var b strings.Builder
	if Failed(a) {
		writeFailure(&b, a)
	} else {
		writeOutcome(&b, a)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// BuildComment monta o comentário para a Ocorrência que voltou com a issue ainda
// aberta: os dados novos, sem repetir a investigação que já está na descrição.
func BuildComment(a domain.Analysis) string {
	var b strings.Builder
	o := a.Occurrence

	switch {
	case Failed(a):
		b.WriteString("**A Ocorrência voltou e a Análise falhou.** Motivo: " + failureReason(a) + ".\n\n")
	case o.IsRegression():
		b.WriteString("**A Ocorrência voltou** (regressão registrada pelo Sentry).\n\n")
	case o.IsEscalation():
		b.WriteString("**A Ocorrência escalou** — o volume cresceu desde a última vez.\n\n")
	default:
		b.WriteString("**A Ocorrência voltou a acontecer.**\n\n")
	}

	writeOccurrenceTable(&b, o)
	writeSentryLink(&b, o)

	if text := deepText(a); text != "" {
		b.WriteString("\n### " + outcomeHeading(a.Verdict) + " desta vez\n\n")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Failed diz se a Análise chegou ao Destino sem investigação. É exportada porque
// quem monta o IssueRequest precisa da mesma resposta para decidir a Prioridade.
//
// O desfecho gravado manda sobre os campos de falha. Uma Análise que morreu numa
// tentativa e passou na seguinte continua carregando FailureKind e FailureReason da
// tentativa morta — a linha do SQLite guarda os dois campos e nada os apaga —, e
// lê-los aqui carimbaria "[não analisada]" numa issue que traz o Diagnóstico
// completo. A marca de falha só vale quando o desfecho é falha.
func Failed(a domain.Analysis) bool {
	switch a.State {
	case domain.StateFailed, domain.StateFailing:
		return true
	case domain.StatePublished, domain.StateClosed:
		return false
	}
	return a.FailureKind != "" || strings.TrimSpace(a.FailureReason) != ""
}

func writeOutcome(b *strings.Builder, a domain.Analysis) {
	b.WriteString("## " + outcomeHeading(a.Verdict) + "\n\n")
	b.WriteString("_Investigação feita apenas por leitura do código e dos dados do Sentry: o projeto não foi executado e nada foi reproduzido._\n\n")

	if text := deepText(a); text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	} else {
		b.WriteString("_A sessão não devolveu conteúdo. Os dados do Sentry estão abaixo._\n\n")
	}

	writePriority(b, a)
	b.WriteString("## Ocorrência\n\n")
	writeOccurrenceTable(b, a.Occurrence)
	writeSentryLink(b, a.Occurrence)
}

func writeFailure(b *strings.Builder, a domain.Analysis) {
	// A primeira linha é a mensagem inteira em uma frase, porque é o que aparece na
	// prévia da issue no board.
	b.WriteString("> **Análise NÃO realizada.** O nightwatcher não conseguiu investigar esta Ocorrência. Motivo: **" +
		failureReason(a) + "**.\n\n")

	b.WriteString("A Ocorrência virou issue mesmo assim: falha do sistema não pode virar Ocorrência perdida. ")
	b.WriteString("Os dados crus do Sentry estão abaixo e a investigação precisa ser feita à mão.\n\n")

	b.WriteString("## Por que não foi analisada\n\n")
	rows := []tableRow{
		{"Motivo", failureReason(a)},
		{"Detalhe", strings.TrimSpace(a.FailureReason)},
	}
	if a.Attempts > 0 {
		rows = append(rows, tableRow{"Tentativas", fmt.Sprintf("%d", a.Attempts)})
	}
	if a.Verdict != "" {
		// Quando a Triagem passou e só a sessão longa morreu, dizer o que a Triagem
		// concluiu poupa metade do trabalho de quem for investigar.
		rows = append(rows, tableRow{"Triagem", string(a.Verdict)})
	}
	writeTable(b, rows)

	writePriority(b, a)

	b.WriteString("## Dados crus do Sentry\n\n")
	writeOccurrenceTable(b, a.Occurrence)
	writeSentryLink(b, a.Occurrence)
	writeRawPayload(b, a.Occurrence)
}

func writePriority(b *strings.Builder, a domain.Analysis) {
	b.WriteString("## Prioridade\n\n")
	b.WriteString("**" + a.Priority.String() + "**\n\n")
	// A justificativa do ajuste só existe quando o agente moveu a prioridade, e é o
	// que permite calibrar a regra com o tempo.
	if r := strings.TrimSpace(a.PriorityRationale); r != "" {
		for _, line := range strings.Split(r, "\n") {
			b.WriteString("> " + strings.TrimSpace(line) + "\n")
		}
		b.WriteString("\n")
	}
}

func writeOccurrenceTable(b *strings.Builder, o domain.Occurrence) {
	rows := []tableRow{
		{"Fonte", o.SentryProjectSlug},
		{"Ocorrência", firstNonEmpty(o.ShortID, o.IssueID)},
		{"Categoria", string(o.Category)},
		{"Nível", string(o.Level)},
		{"Situação", string(o.Substatus)},
		{"Ambiente", o.Environment},
		{"Plataforma", o.Platform},
		{"Culpado", o.Culprit},
	}
	// Contagem e usuários valem ~1 por construção quando o grupo acaba de nascer:
	// mostrar "1 evento" ali é inventar informação. Só aparecem quando dizem algo.
	if o.Count > 1 {
		rows = append(rows, tableRow{"Eventos", fmt.Sprintf("%d", o.Count)})
	}
	if o.UserCount > 1 {
		rows = append(rows, tableRow{"Pessoas atingidas", fmt.Sprintf("%d", o.UserCount)})
	}
	rows = append(rows,
		tableRow{"Primeira vez", formatTime(o.FirstSeen)},
		tableRow{"Última vez", formatTime(o.LastSeen)},
	)
	writeTable(b, rows)
}

func writeSentryLink(b *strings.Builder, o domain.Occurrence) {
	if url := firstNonEmpty(o.Permalink, o.WebURL); url != "" {
		b.WriteString("[Abrir a Ocorrência no Sentry](" + url + ")\n\n")
	}
}

func writeRawPayload(b *strings.Builder, o domain.Occurrence) {
	raw := strings.TrimSpace(string(o.RawBody))
	if raw == "" {
		return
	}
	b.WriteString("### Payload entregue pelo Sentry\n\n```json\n")
	b.WriteString(truncate(raw, maxRawPayload))
	b.WriteString("\n```\n\n")
}

type tableRow struct{ key, value string }

func writeTable(b *strings.Builder, rows []tableRow) {
	written := false
	for _, r := range rows {
		if strings.TrimSpace(r.value) == "" {
			continue
		}
		if !written {
			b.WriteString("| Campo | Valor |\n| --- | --- |\n")
			written = true
		}
		b.WriteString("| " + cell(r.key) + " | " + cell(r.value) + " |\n")
	}
	if written {
		b.WriteString("\n")
	}
}

// cell neutraliza o que quebraria a tabela. O título da Ocorrência é texto escrito
// por usuário final, então nada nele pode ser tratado como markdown estrutural.
func cell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(sentryTimeMask)
}

func outcomeHeading(v domain.Verdict) string {
	switch v {
	case domain.VerdictDefect:
		return "Diagnóstico"
	case domain.VerdictSuggestion:
		return "Estudo"
	}
	return "Análise"
}

// failureReason nomeia o motivo em uma expressão legível: timeout, limite de uso ou
// serviço indisponível são coisas diferentes para quem lê a issue de manhã.
func failureReason(a domain.Analysis) string {
	if a.FailureKind != "" {
		return string(a.FailureKind)
	}
	if r := strings.TrimSpace(a.FailureReason); r != "" {
		return truncate(r, 120)
	}
	return "motivo não registrado"
}

// deepText é o que a sessão longa produziu — o Diagnóstico ou o Estudo. A última
// sessão vence, porque uma retentativa bem-sucedida vem depois da que falhou.
func deepText(a domain.Analysis) string {
	for i := len(a.Sessions) - 1; i >= 0; i-- {
		s := a.Sessions[i]
		if s.Kind != domain.SessionDiagnosis && s.Kind != domain.SessionStudy {
			continue
		}
		if text := strings.TrimSpace(s.Response); text != "" {
			return text
		}
	}
	return ""
}
