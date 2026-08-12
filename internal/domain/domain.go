// Package domain holds the ubiquitous language of the nightwatcher, as fixed by
// CONTEXT.md and the PRD. Identifiers are English by Go convention, but
// the *values* of the domain enums stay Portuguese because they are contractual:
// they surface on the command line, in the SQLite rows and in the agent's reply.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// Verdict is the outcome of the Triagem — the first judgement about an Ocorrência.
// Only Defect and Suggestion carry on; Question and Noise close the Análise.
type Verdict string

const (
	VerdictDefect     Verdict = "defeito"
	VerdictSuggestion Verdict = "sugestão"
	VerdictQuestion   Verdict = "dúvida"
	VerdictNoise      Verdict = "ruído"
)

// CarriesOn reports whether this verdict proceeds to Diagnóstico or Estudo.
func (v Verdict) CarriesOn() bool { return v == VerdictDefect || v == VerdictSuggestion }

func (v Verdict) Valid() bool {
	switch v {
	case VerdictDefect, VerdictSuggestion, VerdictQuestion, VerdictNoise:
		return true
	}
	return false
}

// ParseVerdict accepts the canonical accented form and an unaccented alias, so the
// command line stays typeable without loosening what gets stored.
func ParseVerdict(s string) (Verdict, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "defeito":
		return VerdictDefect, nil
	case "sugestão", "sugestao":
		return VerdictSuggestion, nil
	case "dúvida", "duvida":
		return VerdictQuestion, nil
	case "ruído", "ruido":
		return VerdictNoise, nil
	}
	return "", fmt.Errorf("desfecho de triagem desconhecido: %q", s)
}

// State is the Análise state machine.
//
//	recebida ──► enfileirada ──► triando ──► analisando ──► publicada
//	    │             │             │            │
//	    │             │             ▼            ▼
//	    │             │        encerrada     falhando ──► falhou
//	    └──► recusada
//
// Published, Closed, Refused and Failed are first-class outcomes: all four are
// recorded, which is what makes the Triagem auditable and what stops the defect
// that killed the predecessor — an Ocorrência vanishing in silence.
type State string

const (
	StateReceived  State = "recebida"
	StateQueued    State = "enfileirada"
	StateTriaging  State = "triando"
	StateAnalyzing State = "analisando"
	StateFailing   State = "falhando"

	StatePublished State = "publicada"
	StateClosed    State = "encerrada"
	StateRefused   State = "recusada"
	StateFailed    State = "falhou"
)

// Terminal reports whether the Análise has reached an outcome.
func (s State) Terminal() bool {
	switch s {
	case StatePublished, StateClosed, StateRefused, StateFailed:
		return true
	}
	return false
}

// RefusalReason says why an Ocorrência never entered the queue. Refusal is recorded,
// never silent — every one of these stays listable.
type RefusalReason string

const (
	RefusedBadSignature RefusalReason = "assinatura inválida"
	RefusedUnknownFonte RefusalReason = "fonte desconhecida"
	RefusedOutage       RefusalReason = "categoria outage"
	RefusedDuplicate    RefusalReason = "entrega duplicada"
	RefusedIgnoredEvent RefusalReason = "evento ignorado"

	// RefusedUnknownCategory covers a category the system has never been taught to handle.
	// It exists because `http_client` — Sentry's N+1 detector — turned up in real acme data
	// and nobody had anticipated it — the refusal rules only ruled on `outage`. The policy is therefore
	// an allowlist and not a denylist: `error` and `feedback` carry on, everything else is
	// refused and recorded, so the next unforeseen category is a listable row rather than a
	// crash or, worse, a session spent diagnosing something that is not an application defect.
	RefusedUnknownCategory RefusalReason = "categoria desconhecida"

	// RefusedQueueFull covers an arrival turned away because the queue is already at
	// its ceiling. It needs a name of its own rather than reusing RefusedIgnoredEvent
	// because a full queue is a capacity signal — a bad release, a loop in production,
	// a misconfigured Alert Rule — and filing it beside an `ongoing` delivery would
	// make the moment the flood started invisible in `analysis list`.
	RefusedQueueFull RefusalReason = "fila acima do teto"

	// RefusedMalformed covers authentic bytes the system could not read as an
	// Ocorrência. It is separate from RefusedIgnoredEvent because the two mean opposite
	// things to whoever reads `analysis list`: an ignored event is the system working
	// as decided, while a run of malformed deliveries means Sentry changed the payload
	// shape underneath us.
	RefusedMalformed RefusalReason = "entrega malformada"
)

// Category is the Sentry issueCategory — the discriminator between an error and a
// user-published feedback. Note this is `issueCategory`, NOT `kind`: `kind` does not
// exist in the Sentry payload, which corrected the PRD on exactly this point.
type Category string

const (
	CategoryError    Category = "error"
	CategoryFeedback Category = "feedback"
	CategoryOutage   Category = "outage"
)

// Level is the Sentry severity, and the primary input of the Prioridade rule.
type Level string

const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
	LevelDebug   Level = "debug"
)

// Substatus discriminates what an `issue.unresolved` delivery means. Regressão never
// arrives as `issue.created` — it arrives here.
type Substatus string

const (
	SubstatusNew        Substatus = "new"
	SubstatusRegressed  Substatus = "regressed"
	SubstatusEscalating Substatus = "escalating"
	SubstatusOngoing    Substatus = "ongoing"
)

// Sentry webhook actions the nightwatcher subscribes to.
const (
	ActionCreated    = "created"
	ActionUnresolved = "unresolved"

	// ActionTriggered is what an `event_alert` delivery carries: an Alert Rule fired.
	// It is not a lifecycle action of the group — the payload says nothing about the
	// group being new or regressed — and it is the only action that arrives with an
	// empty Substatus.
	ActionTriggered = "triggered"
)

// IsFirstSighting reports the two actions that can be the FIRST time the nightwatcher
// hears about a group: `issue.created` and an Alert Rule firing.
//
// The distinction is what keeps one Ocorrência from becoming two Análises. Both doors
// deliver the same group, so two first sightings of it are the same Ocorrência arriving
// twice, whatever their substatus. A later `issue.unresolved` is NOT a first sighting:
// a regressão or an escalada is a new Ocorrência that happens to share a group, and
// folding it in here would be the dedup bug turned inside out.
func IsFirstSighting(action string) bool {
	return action == ActionCreated || action == ActionTriggered
}

// Occurrence is what arrives from Sentry and deserves consideration: a *group* of
// errors or a user-published feedback — never the individual event.
type Occurrence struct {
	// Identity. The dedup key is (InstallationUUID, SentryProjectID, IssueID, Substatus).
	// Substatus belongs in the key so a regressão of an already-seen group is not
	// swallowed as a repeat delivery — the dedup bug turned inside out.
	InstallationUUID string
	SentryProjectID  string
	IssueID          string
	Substatus        Substatus
	Action           string

	Category Category
	Level    Level

	Title     string
	Culprit   string
	Permalink string
	WebURL    string
	Platform  string
	ShortID   string

	// Count and UserCount are ~1 by construction on `issue.created`, because the
	// trigger is gated by is_new. They only mean something on `substatus: escalating`
	// — do not prioritise by volume at creation.
	Count     int64
	UserCount int64

	FirstSeen time.Time
	LastSeen  time.Time

	// SentryProjectSlug resolves which Fonte, and through it which Projeto.
	SentryProjectSlug string

	// Environment is NOT in the webhook payload; it costs a separate tags call.
	Environment string

	// RawBody is the exact byte sequence delivered, kept because the HMAC is over
	// raw bytes and because it is the audit record of what arrived.
	RawBody    []byte
	ReceivedAt time.Time
}

// DedupKey is the identity that makes a repeated delivery recognisable. Sentry
// delivers duplicates, always — recognising them is an obligation, not a courtesy.
func (o Occurrence) DedupKey() string {
	return strings.Join([]string{o.InstallationUUID, o.SentryProjectID, o.IssueID, string(o.Substatus)}, "|")
}

// IsRegression reports a group that broke again after being closed.
func (o Occurrence) IsRegression() bool { return o.Substatus == SubstatusRegressed }

// IsEscalation reports a group that grew — the moment volume becomes real.
func (o Occurrence) IsEscalation() bool { return o.Substatus == SubstatusEscalating }

// Project is the unit of nightwatcher configuration: one repository, one Destino,
// one or more Fontes Sentry.
type Project struct {
	ID              int64
	Name            string
	Repo            string
	Branch          string
	LinearProjectID string
	Sources         []Source
}

// Source is a Sentry project feeding a Projeto. Its Label is a free-form tag that
// survives all the way to the issue, so "show me everything from the API" is a board
// filter and not a text search.
type Source struct {
	ID         int64
	ProjectID  int64
	SentrySlug string
	Label      string
}

// SessionKind distinguishes the two Claude sessions and their very different budgets.
type SessionKind string

const (
	SessionTriage    SessionKind = "triagem"
	SessionDiagnosis SessionKind = "diagnóstico"
	SessionStudy     SessionKind = "estudo"
)

// Session records what was handed to the agent and what came back. This is exactly
// what `nightwatcher analysis show` prints, and the raw material for diagnosing a
// bad Triagem.
type Session struct {
	ID        int64
	Kind      SessionKind
	Prompt    string
	Response  string
	StartedAt time.Time
	EndedAt   time.Time
	Err       string
}

// Analysis is the full journey of an Ocorrência: from Sentry delivery to outcome.
// It has a beginning, an end and a recorded result — including when the result is
// "refused" or "failed".
type Analysis struct {
	ID         string
	ProjectID  int64
	SourceID   int64
	Occurrence Occurrence

	State   State
	Verdict Verdict

	Priority          Priority
	PriorityRationale string

	Refusal RefusalReason

	Attempts      int
	FailureReason string
	FailureKind   AgentErrorKind

	IssueID  string
	IssueURL string

	Sessions []Session

	CreatedAt time.Time
	UpdatedAt time.Time
}
