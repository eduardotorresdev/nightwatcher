package worker

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/prompts"
)

// The long session answers a strict JSON envelope, and the board needs prose. These
// two functions are the translation, and they live here rather than in the parser
// because the envelope is a contract with the model and this is a contract with the
// person reading the issue in the morning.
//
// Both write under the heading tracker.BuildBody already opened ("## Diagnóstico"),
// so every section here is one level down.

func renderDiagnosis(d prompts.Diagnosis, missingPaths []string, branch string) string {
	missing := map[string]bool{}
	for _, p := range missingPaths {
		missing[p] = true
	}

	var b strings.Builder
	b.WriteString("### Causa provável\n\n")
	b.WriteString(d.ProbableCause)
	b.WriteString("\n\n### Arquivos suspeitos\n\n")
	for _, f := range d.SuspectFiles {
		b.WriteString("- `" + f.Path)
		if f.Line > 0 {
			fmt.Fprintf(&b, ":%d", f.Line)
		}
		b.WriteString("`")
		if f.Why != "" {
			b.WriteString(" — " + f.Why)
		}
		if missing[f.Path] {
			// A path that does not exist in the clone is the classic failure of a
			// session that reads code, and an unmarked one sends whoever opens the
			// issue looking for a file that was never there.
			b.WriteString("  \n  **⚠ este caminho não existe no repositório")
			if branch != "" {
				b.WriteString(" na branch `" + branch + "`")
			}
			b.WriteString(" — trate a indicação com desconfiança**")
		}
		b.WriteString("\n")
	}

	if len(d.Evidence) > 0 {
		b.WriteString("\n### Evidências\n\n")
		for _, e := range d.Evidence {
			b.WriteString("- " + e + "\n")
		}
	}

	fmt.Fprintf(&b, "\n**Confiança do diagnóstico:** %s\n", string(d.Confidence))
	if len(missing) > 0 {
		fmt.Fprintf(&b, "\n> %d dos caminhos apontados não foram encontrados no repositório analisado.\n", len(missing))
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderStudy(s prompts.Study) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Viabilidade: %s\n\n", string(s.Feasibility))
	b.WriteString(s.Summary)
	b.WriteString("\n\n### Alternativas\n")
	for _, alt := range s.Alternatives {
		b.WriteString("\n#### " + alt.Name)
		if alt.Recommended {
			b.WriteString(" — **recomendada**")
		}
		fmt.Fprintf(&b, "\n\nEsforço: %s\n\n", string(alt.Effort))
		if alt.Description != "" {
			b.WriteString(alt.Description + "\n")
		}
	}
	writeList(&b, "Riscos", s.Risks)
	writeList(&b, "Dificuldades", s.Difficulties)
	return strings.TrimRight(b.String(), "\n")
}

func writeList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("\n### " + heading + "\n\n")
	for _, it := range items {
		b.WriteString("- " + it + "\n")
	}
}

// occurrenceText digs the free text of the Ocorrência out of the delivered payload.
//
// domain.Occurrence has no field for it on purpose: the feedback a user wrote and the
// frame Sentry captured live inside the webhook body, and whoever renders the prompt
// extracts them. Everything read here is written by an end user or by a runtime, so
// it is untrusted — which is why it goes to prompts.Context.Message and .StackTrace,
// the two fields that end up inside the content-derived delimiters, and nowhere else.
//
// It reads BOTH delivery shapes, because the text lives in a different place in each
// and a feedback that arrived through the Alert Rule door would otherwise reach the
// Triagem with nothing but its title — which, for a feedback, is the summary Sentry
// wrote and not a word of what the person actually said.
//
// A payload that does not parse yields nothing. The Ocorrência still has its title,
// its culprit and its metadata, and failing an Análise over a body we could not read
// would be losing an Ocorrência to a JSON detail.
func occurrenceText(occ domain.Occurrence) (message, stack string) {
	if len(occ.RawBody) == 0 {
		return "", ""
	}
	var body deliveryText
	if json.Unmarshal(occ.RawBody, &body) != nil {
		return "", ""
	}
	if body.Data.Event != nil {
		return eventText(body.Data.Event)
	}
	return issueText(body.Data.Issue.Metadata)
}

// deliveryText is the two shapes at once. Unmarshalling both in one pass is safe here
// in a way it would not be at ingest: no delivery carries both keys, this reads only
// free text, and nothing downstream branches on which one was populated except to know
// where to look.
type deliveryText struct {
	Data struct {
		Issue struct {
			Metadata issueMetadata `json:"metadata"`
		} `json:"issue"`
		Event *eventBody `json:"event"`
	} `json:"data"`
}

type issueMetadata struct {
	Value    string `json:"value"`
	Message  string `json:"message"`
	Type     string `json:"type"`
	Filename string `json:"filename"`
	Function string `json:"function"`
}

// eventBody is `data.event`, read only for what a human or a runtime wrote.
type eventBody struct {
	Message  string `json:"message"`
	Metadata struct {
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"metadata"`

	Contexts struct {
		Feedback struct {
			Message string `json:"message"`
			URL     string `json:"url"`
		} `json:"feedback"`
	} `json:"contexts"`

	Occurrence *struct {
		Subtitle     string `json:"subtitle"`
		EvidenceData struct {
			Message string `json:"message"`
		} `json:"evidenceData"`
	} `json:"occurrence"`

	Exception struct {
		Values []struct {
			Type       string `json:"type"`
			Value      string `json:"value"`
			Stacktrace struct {
				Frames []struct {
					Filename string `json:"filename"`
					Function string `json:"function"`
					LineNo   int    `json:"lineno"`
					InApp    bool   `json:"in_app"`
				} `json:"frames"`
			} `json:"stacktrace"`
		} `json:"values"`
	} `json:"exception"`
}

// issueText reads the `issue` door.
//
// Sentry does not deliver a stack trace in this shape — the detail endpoint adds only
// URLs — so what comes out is every frame there is: the exception type and
// the single frame the grouping picked.
func issueText(m issueMetadata) (message, stack string) {
	message = firstNonEmpty(m.Message, m.Value)

	var b strings.Builder
	if t := strings.TrimSpace(m.Type); t != "" {
		b.WriteString("Tipo: " + t + "\n")
	}
	if f := strings.TrimSpace(m.Filename); f != "" {
		b.WriteString("Arquivo apontado pelo Sentry: " + f + "\n")
	}
	if fn := strings.TrimSpace(m.Function); fn != "" {
		b.WriteString("Função: " + fn + "\n")
	}
	return message, strings.TrimRight(b.String(), "\n")
}

// eventText reads the `event_alert` door, which is richer than the other one in the two
// places that matter to a session.
//
// The message: for a feedback, `contexts.feedback.message` is the text the person typed,
// verbatim and unsummarised. `occurrence.evidenceData.message` says the same thing and
// is the fallback; `occurrence.subtitle` and the event's own message cover the
// non-feedback groups.
//
// The stack: this shape carries `exception.values[].stacktrace.frames`, real frames,
// which the issue door does not have at all. That is the single biggest thing this door
// gives a Diagnóstico that the original one could not — so the frames are rendered here
// rather than left in the body for nobody to read. Frames are Sentry-ordered, innermost
// last, and are printed in that order with the application's own frames marked, because
// a stack where every frame looks alike is what leaves the investigation with the human.
func eventText(ev *eventBody) (message, stack string) {
	message = firstNonEmpty(
		ev.Contexts.Feedback.Message,
		evidenceMessage(ev),
		ev.Metadata.Value,
		ev.Message,
	)

	var b strings.Builder
	for _, exc := range ev.Exception.Values {
		if t := strings.TrimSpace(exc.Type); t != "" {
			b.WriteString("Tipo: " + t + "\n")
		}
		if v := strings.TrimSpace(exc.Value); v != "" {
			b.WriteString("Valor: " + v + "\n")
		}
		for _, f := range exc.Stacktrace.Frames {
			b.WriteString("  " + frameLine(f.Filename, f.Function, f.LineNo, f.InApp) + "\n")
		}
	}
	if b.Len() == 0 {
		// No exception at all is the normal case for a feedback, and the metadata type
		// is still worth saying when it is there.
		if t := strings.TrimSpace(ev.Metadata.Type); t != "" {
			b.WriteString("Tipo: " + t + "\n")
		}
	}
	if u := strings.TrimSpace(ev.Contexts.Feedback.URL); u != "" {
		// The page the person was on when they wrote. For a feedback this is the
		// closest thing to a reproduction step that exists in the payload.
		b.WriteString("Página: " + u + "\n")
	}
	return message, strings.TrimRight(b.String(), "\n")
}

func evidenceMessage(ev *eventBody) string {
	if ev.Occurrence == nil {
		return ""
	}
	return firstNonEmpty(ev.Occurrence.EvidenceData.Message, ev.Occurrence.Subtitle)
}

// frameLine renders one stack frame. The `[app]` mark is the whole point: Sentry mixes
// framework and node_modules frames in with the ones the repository actually owns, and
// the Diagnóstico is supposed to name a real file in the repository.
func frameLine(filename, function string, line int, inApp bool) string {
	var b strings.Builder
	b.WriteString(fallbackDash(filename))
	if line > 0 {
		b.WriteString(":" + strconv.Itoa(line))
	}
	if fn := strings.TrimSpace(function); fn != "" {
		b.WriteString(" em " + fn)
	}
	if inApp {
		b.WriteString(" [app]")
	}
	return b.String()
}

func fallbackDash(s string) string {
	if t := strings.TrimSpace(s); t != "" {
		return t
	}
	return "(sem arquivo)"
}
