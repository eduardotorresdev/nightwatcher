package worker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/priority"
	"github.com/eduardotorresdev/nightwatcher/internal/prompts"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// priorIssue is what this Ocorrência produced in the Destino the last time it was
// seen — the local half from SQLite, and the open/closed half from the Tracker.
type priorIssue struct {
	analysisID string
	issueID    string
	issueURL   string

	// priority is the value recorded on that issue, which is what AtEscalation is not
	// allowed to lower: whoever changed it by hand did it for a reason the daemon does
	// not know.
	priority domain.Priority

	state domain.IssueState
	at    time.Time
}

func (p priorIssue) exists() bool { return p.issueID != "" }

// reopenable reports the case of User Story 15: something given as resolved broke
// again. It is the only case that produces a second issue for the same group.
func (p priorIssue) reopenable(occ domain.Occurrence) bool {
	return occ.IsRegression() && p.state.Exists && !p.state.Open
}

// recurrence is the line the prompt shows about the previous issue.
func (p priorIssue) recurrence() string {
	if !p.exists() {
		return ""
	}
	state := "fechada"
	if p.state.Open {
		state = "aberta"
	}
	line := fmt.Sprintf("esta Ocorrência já produziu a issue %s, hoje %s",
		firstNonEmpty(p.state.Identifier, p.issueID), state)
	if !p.at.IsZero() {
		line += ", registrada em " + p.at.UTC().Format("2006-01-02")
	}
	return line
}

// previousIssue answers "did this Sentry group already become work in the Destino".
//
// The dedup key carries the substatus, so a regressão and the creation it
// regressed from are two different Análises of the same group — which is exactly why
// this matches on the group and not on the dedup key.
func (a *Analyst) previousIssue(ctx context.Context, an domain.Analysis) (priorIssue, error) {
	if a.history == nil || an.Occurrence.IssueID == "" {
		return priorIssue{}, nil
	}

	past, err := a.history.ListAnalyses(ctx, store.AnalysisFilter{
		ProjectID: an.ProjectID,
		Limit:     historyLookback,
	})
	if err != nil {
		return priorIssue{}, fmt.Errorf("procurando a issue anterior da Ocorrência %s: %w", an.Occurrence.IssueID, err)
	}

	var prior priorIssue
	for _, old := range past { // most recent first
		if old.ID == an.ID || old.IssueID == "" || !sameGroup(old.Occurrence, an.Occurrence) {
			continue
		}
		prior = priorIssue{
			analysisID: old.ID,
			issueID:    old.IssueID,
			issueURL:   old.IssueURL,
			priority:   old.Priority,
			at:         old.CreatedAt,
		}
		break
	}
	if !prior.exists() {
		return priorIssue{}, nil
	}

	state, err := a.tracker.IssueState(ctx, prior.issueID)
	if err != nil {
		return priorIssue{}, destinoFailed("consultando o estado da issue anterior", err)
	}
	if !state.Exists {
		// The Destino has never heard of it: deleted, or from a workspace that no
		// longer exists. Commenting into the void would be a lost Ocorrência with a
		// receipt, so this counts as no previous issue at all.
		return priorIssue{}, nil
	}
	prior.state = state
	return prior, nil
}

// sameGroup reports two Ocorrências being the same Sentry group. It is the group and
// never the individual event: an error that happens five thousand times is one
// Ocorrência.
func sameGroup(a, b domain.Occurrence) bool {
	if a.IssueID == "" || a.IssueID != b.IssueID {
		return false
	}
	if a.SentryProjectID != "" && b.SentryProjectID != "" && a.SentryProjectID != b.SentryProjectID {
		return false
	}
	if a.InstallationUUID != "" && b.InstallationUUID != "" && a.InstallationUUID != b.InstallationUUID {
		return false
	}
	return true
}

func (a *Analyst) facts(an domain.Analysis, prior priorIssue, adj priority.Adjustment) priority.Facts {
	return priority.Facts{
		Verdict:       an.Verdict,
		Level:         an.Occurrence.Level,
		Substatus:     an.Occurrence.Substatus,
		PreviousIssue: prior.state,
		UserCount:     an.Occurrence.UserCount,
		Agent:         adj,
	}
}

// compute runs the rule over the right pass. The creation payload is poor and the
// escalation payload is finally rich, so an Ocorrência that grew is the only
// one that reads volume — and it is never allowed to come out less urgent than what
// is already recorded on the issue.
func (a *Analyst) compute(an domain.Analysis, f priority.Facts, prior priorIssue) priority.Result {
	if an.Occurrence.IsEscalation() {
		return priority.AtEscalation(f, prior.priority)
	}
	return priority.AtCreation(f)
}

// factLines is what the prompt shows as the facts the rule decided from, so the agent
// argues against a number with reasons attached instead of against a blank slate.
func factLines(f priority.Facts) []string {
	lines := []string{
		"desfecho da Triagem: " + string(f.Verdict),
		"nível reportado pelo Sentry: " + fallback(string(f.Level)),
		"situação da Ocorrência: " + fallback(string(f.Substatus)),
	}
	if f.Substatus == domain.SubstatusEscalating {
		lines = append(lines, fmt.Sprintf("pessoas atingidas: %d", f.UserCount))
	}
	switch {
	case f.PreviousIssue.Exists && f.PreviousIssue.Open:
		lines = append(lines, "já existe issue aberta para esta Ocorrência")
	case f.PreviousIssue.Exists:
		lines = append(lines, "a issue anterior desta Ocorrência estava fechada")
	}
	return lines
}

// moveOf translates the agent's reply into the rule's vocabulary. The direction is a
// name and not a signed number on both sides, because the Linear scale is inverted
// and `+1` would read like "more urgent" and do the opposite.
func moveOf(adj prompts.Adjustment) priority.Adjustment {
	move := priority.MoveKeep
	switch adj.Direction {
	case prompts.AdjustUp:
		move = priority.MoveUp
	case prompts.AdjustDown:
		move = priority.MoveDown
	}
	return priority.Adjustment{Move: move, Justification: adj.Rationale}
}

// rationale is the sentence the issue carries about the Prioridade. It is written only
// when the agent's move was accepted, so a justification can never appear on the board
// for a move that did not happen.
func rationale(res priority.Result, adj priority.Adjustment) string {
	if !res.Adjusted() {
		return ""
	}
	verb := "subiu"
	if adj.Move == priority.MoveDown {
		verb = "desceu"
	}
	return fmt.Sprintf("A regra calculou %s e o agente %s um nível: %s",
		res.Rule.String(), verb, res.Justification)
}

// render builds the prompt of one session.
func (a *Analyst) render(kind domain.SessionKind, an domain.Analysis, proj domain.Project, src domain.Source, prior priorIssue, workDir string, base domain.Priority, facts []string) (string, error) {
	message, stack := occurrenceText(an.Occurrence)
	text, err := prompts.Render(kind, an.Occurrence, prompts.Context{
		ProjectName:   proj.Name,
		Repo:          proj.Repo,
		Branch:        proj.Branch,
		SourceLabel:   src.Label,
		WorkDir:       workDir,
		Message:       message,
		StackTrace:    stack,
		BasePriority:  base,
		PriorityFacts: facts,
		Recurrence:    prior.recurrence(),
	})
	if err != nil {
		// Our own fault, not the model's — and repeating it produces the same result,
		// which is precisely what the non-retryable classification means.
		return "", &domain.AgentError{
			Kind:   domain.AgentErrMalformed,
			Detail: "montando o prompt: " + err.Error(),
			Err:    err,
		}
	}
	return text, nil
}

// sourceOf finds the Fonte the Ocorrência came through, so its rótulo can survive to
// the issue. The slug is the fallback because a Fonte repointed at another Projeto
// keeps its slug and changes its id.
func sourceOf(proj domain.Project, an domain.Analysis) domain.Source {
	for _, s := range proj.Sources {
		if s.ID == an.SourceID && an.SourceID != 0 {
			return s
		}
	}
	for _, s := range proj.Sources {
		if s.SentrySlug != "" && s.SentrySlug == an.Occurrence.SentryProjectSlug {
			return s
		}
	}
	return domain.Source{}
}

func attachmentTitle(occ domain.Occurrence) string {
	if id := strings.TrimSpace(occ.ShortID); id != "" {
		return "Ocorrência no Sentry · " + id
	}
	return "Ocorrência no Sentry"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func fallback(s string) string {
	if strings.TrimSpace(s) == "" {
		return "não informado"
	}
	return s
}
