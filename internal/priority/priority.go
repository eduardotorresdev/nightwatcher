// Package priority computes the Prioridade of an Ocorrência: the urgency with which
// an issue is born in the Destino.
//
// The rule is deterministic first and the agent may only nudge it, and that ordering
// is evidence, not taste: in the predecessor the model's free-form score landed on
// 0.45 or 0.55 in every single triage, hugging the 0.5 cut, so Ocorrências with
// opposite destinies ended up separated by noise.
//
// Everything here is pure — no database, no clock, no network. That is what lets the
// case table in priority_test.go be the readable specification of the rule and the
// place it gets calibrated over time.
//
// Two facts deliberately do NOT enter the calculation, and are absent from Facts
// rather than present and ignored, because a field nobody reads is a field somebody
// will eventually expect to matter:
//
//   - environment — it does not arrive in the creation payload, and a Fonte Sentry
//     pointing at a non-production environment simply is not registered. Filtering in
//     configuration is cheaper and more honest than guessing in the calculation.
//   - count — events with no people behind them is one user in a tight loop: noisy,
//     not grave. The volume band is by PEOPLE AFFECTED, never by events.
package priority

import (
	"strings"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// The volume band is by people affected. The thresholds are named because they are
// the two numbers most likely to move once real arrival distribution is observed.
const (
	usersForHigh   = 25
	usersForMedium = 5
)

// Move is the direction of the agent's adjustment. It is a named direction and not a
// signed number, because the Linear scale is inverted: `+1` would read like "more
// urgent" and do the opposite.
type Move string

const (
	// MoveKeep — the agent accepted the rule's own value. Any unrecognised Move is
	// treated as this one: an adjustment nobody can name is not an adjustment.
	MoveKeep Move = ""
	MoveUp   Move = "subir"
	MoveDown Move = "descer"
)

// Adjustment is the agent's judgement over the rule's value — gravity that a number
// does not capture, such as silent data loss.
type Adjustment struct {
	Move Move

	// Justification is the written reason. A move without one is REFUSED and the rule
	// keeps its own value. Reading these justifications is how the rule gets
	// calibrated over time, so an unjustified move is worth exactly nothing.
	Justification string
}

// Facts is what the rule reads about an Ocorrência.
type Facts struct {
	// Verdict is the Triagem outcome. Only `defeito` looks at Level; `sugestão` is
	// born Low whatever the Sentry severity says.
	Verdict domain.Verdict

	// Level is the Sentry severity and the primary input of the rule.
	Level domain.Level

	// Substatus decides which modifier fires. At `issue.created` it is always `new`,
	// so `regressed` and `escalating` only ever reach here through `issue.unresolved`.
	Substatus domain.Substatus

	// PreviousIssue is the state of the Linear issue this Ocorrência produced before,
	// straight from Tracker.IssueState — passed through rather than flattened into a
	// bool, so that "no previous issue" and "previous issue closed" cannot collapse
	// into the same zero value.
	PreviousIssue domain.IssueState

	// UserCount is how many people the group hit. It is ~1 by construction at
	// creation, because `issue.created` is gated by is_new, and only becomes real on
	// `substatus: escalating` — which is the only pass that reads it.
	UserCount int64

	// Agent is applied last and is not a fact; it is the one place opinion enters.
	Agent Adjustment
}

// Result is what the rule decided, plus enough of its reasoning to be written onto
// the issue.
type Result struct {
	// Priority is the value to record. It is never 0 — No priority, because an absent
	// priority is indistinguishable from a bug in the rule and the board loses its
	// ordering; and never 1 — Urgent, because Urgent fires an urgent e-mail plus a 24h
	// SLA and is a human act.
	Priority domain.Priority

	// Rule is where the deterministic part landed, before the agent touched it. The
	// issue text needs both numbers to be honest about who decided what.
	Rule domain.Priority

	// Justification is the agent's written reason, trimmed, and carried only when the
	// adjustment was accepted — so a caller can never write a rationale onto an issue
	// for a move that did not happen.
	Justification string

	// Refused reports a move thrown away for lack of a written justification.
	Refused bool
}

// Adjusted reports that the agent's move was accepted. Accepted is not the same as
// changed: an accepted move can still saturate at the ceiling or the floor.
func (r Result) Adjusted() bool { return r.Justification != "" }

// pass names which of the two runs this is. The rule runs twice over the same
// Ocorrência with different inputs: the creation payload is poor, the
// escalation payload is finally rich.
type pass int

const (
	creation pass = iota
	escalation
)

// AtCreation computes the Prioridade an issue is born with.
func AtCreation(f Facts) Result {
	return compute(f, creation, domain.PriorityNone)
}

// AtEscalation recomputes the Prioridade once `count` and `userCount` are real, given
// the value already recorded on the issue. Under usersForMedium the band says nothing
// and the creation value stands. The returned Priority is never less urgent than
// recorded: priority does not fall on its own.
func AtEscalation(f Facts, recorded domain.Priority) Result {
	return compute(f, escalation, recorded)
}

func compute(f Facts, p pass, recorded domain.Priority) Result {
	out := fromVerdictAndLevel(f)
	out = withModifiers(out, f, p)
	if p == escalation {
		out = withVolumeBand(out, f.UserCount)
	}

	res := Result{Rule: out, Priority: out}

	switch f.Agent.Move {
	case MoveUp, MoveDown:
		why := strings.TrimSpace(f.Agent.Justification)
		if why == "" {
			res.Refused = true
			break
		}
		res.Justification = why
		// Raise and Lower saturate at AutoCeiling and AutoFloor, so the agent reaches
		// neither 1 — Urgent nor 0 — No priority no matter what it asks for.
		if f.Agent.Move == MoveUp {
			res.Priority = out.Raise(1)
		} else {
			res.Priority = out.Lower(1)
		}
	}

	// Last word, and only on the second pass: whoever lowered the priority by hand
	// lowered it for a reason the daemon does not know, so only a HIGHER value gets
	// written. This also protects a 1 — Urgent set by a person, which the rule never
	// produces and must never undo.
	if p == escalation && recorded.MoreUrgentThan(res.Priority) {
		res.Priority = recorded
	}

	return res
}

// fromVerdictAndLevel is the level table, written out case by case. There is no
// arithmetic anywhere near it: the Linear scale is inverted with 0 outside the order,
// so `p - 1` produces the opposite of what it reads like.
func fromVerdictAndLevel(f Facts) domain.Priority {
	if f.Verdict != domain.VerdictDefect {
		// Sugestão is born Low: a feature request does not compete for urgency with
		// something that is broken. Dúvida and ruído close the Triagem and never get
		// here, but if they ever do they get Low and not 0 — the rule has no output
		// that means "no opinion".
		return domain.PriorityLow
	}
	switch f.Level {
	case domain.LevelFatal:
		return domain.PriorityHigh
	case domain.LevelError:
		return domain.PriorityMedium
	case domain.LevelWarning, domain.LevelInfo, domain.LevelDebug:
		return domain.PriorityLow
	}
	// An unknown or absent level lands on Low. Sentry's level is free-form enough that
	// guessing upwards here would be inventing urgency out of a typo.
	return domain.PriorityLow
}

// withModifiers applies the modifiers one at a time. The ceiling of 2 — High needs no
// check here because Raise saturates at domain.AutoCeiling.
func withModifiers(p domain.Priority, f Facts, ps pass) domain.Priority {
	// `escalating` is worth one level only while the volume is unknown. On the second
	// pass the very same fact arrives with real numbers and is spoken by the volume
	// band, so raising here too would count it twice — and would leave the
	// `userCount >= 5` band unreachable, since nothing could still be Low by the time
	// the band ran.
	if f.Substatus == domain.SubstatusEscalating && ps == creation {
		p = p.Raise(1)
	}
	if f.Substatus == domain.SubstatusRegressed && resolvedBefore(f.PreviousIssue) {
		p = p.Raise(1)
	}
	// `ongoing` and `new` move nothing.
	return p
}

// resolvedBefore reports a regressão of something that had already been given as
// done. Openness comes from state.type (completed, canceled, duplicate) and never
// from the state's name, because the team's states are customised and in Portuguese
// — which is exactly why this reads domain.IssueState instead of a bool.
//
// A regressão whose previous issue is still open raises nothing: nobody has resolved
// it yet, so raising the priority adds no information and just churns the board — that
// case gets a comment instead. And with no previous issue at all there is nothing that
// was ever given as resolved, so there is nothing to elevate.
func resolvedBefore(s domain.IssueState) bool { return s.Exists && !s.Open }

// withVolumeBand raises p to the floor that the number of people affected demands. It
// is a floor and never a cap: under usersForMedium the band contributes nothing and
// whatever the rest of the rule produced stands.
func withVolumeBand(p domain.Priority, userCount int64) domain.Priority {
	floor := domain.PriorityNone
	switch {
	case userCount >= usersForHigh:
		floor = domain.PriorityHigh
	case userCount >= usersForMedium:
		floor = domain.PriorityMedium
	}
	if floor.MoreUrgentThan(p) {
		return floor
	}
	return p
}
