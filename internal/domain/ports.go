package domain

import (
	"context"
	"errors"
	"time"
)

// The four falsified boundaries of the PRD's Testing Decisions, plus an injectable
// Clock. Everything the nightwatcher cannot control lives behind one of these, and
// each has a test substitute. The integration seam asserts on Tracker.

// SentryAPI fetches detail about an Ocorrência that the webhook payload does not
// carry. In practice that is only the environment: the detail endpoint is the same
// serializer as the webhook and adds nothing but URLs, so calling it would spend
// rate limit for no information.
type SentryAPI interface {
	// Environments returns the environment tag values seen for an issue.
	Environments(ctx context.Context, issueID string) ([]string, error)
}

// Agent receives a prompt and a working directory, and returns text. That is the
// whole contract — the CLI mechanics, the tool restriction and the container live
// behind it.
type Agent interface {
	Run(ctx context.Context, req AgentRequest) (AgentResult, error)
}

// AgentRequest describes one Claude session.
type AgentRequest struct {
	Kind SessionKind

	// Prompt is the full text handed to the session.
	Prompt string

	// WorkDir is the read-only clone mounted for the long session. Empty means the
	// short Triagem session, which mounts no repository at all.
	WorkDir string

	// Timeout is the wall-clock ceiling. There is NO timeout flag on the CLI
	// — the ceiling is enforced by the caller with SIGTERM.
	Timeout time.Duration

	// Tools is the universe of tools the session may use. This is hygiene, not a
	// security boundary: the containment is the container.
	Tools []string
}

// AgentResult is what a session produced.
type AgentResult struct {
	Text     string
	Duration time.Duration
	CostUSD  float64
	NumTurns int
}

// AgentErrorKind classifies a failed session, because the failure issue has to show
// the reason: was it a timeout, a usage limit, or a service that was down?
type AgentErrorKind string

const (
	AgentErrTimeout     AgentErrorKind = "timeout"
	AgentErrRateLimit   AgentErrorKind = "limite de uso"
	AgentErrAuth        AgentErrorKind = "falha de autenticação"
	AgentErrMalformed   AgentErrorKind = "resposta malformada"
	AgentErrUnavailable AgentErrorKind = "serviço indisponível"
	AgentErrPerimeter   AgentErrorKind = "violação de perímetro"
)

// Retryable says which failures are worth waiting out. Malformed replies and auth
// failures are not: repeating produces the same result and only burns budget. Both
// go straight to the failure path, with the reason on the issue.
func (k AgentErrorKind) Retryable() bool {
	switch k {
	case AgentErrRateLimit, AgentErrUnavailable, AgentErrTimeout:
		return true
	}
	return false
}

// AgentError carries the classification alongside the underlying cause.
type AgentError struct {
	Kind   AgentErrorKind
	Detail string
	Err    error
}

func (e *AgentError) Error() string {
	if e.Detail == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Detail
}

func (e *AgentError) Unwrap() error { return e.Err }

// ClassifyAgentError extracts the kind from an error, defaulting to unavailable.
func ClassifyAgentError(err error) AgentErrorKind {
	var ae *AgentError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return AgentErrUnavailable
}

// Tracker is where the result is recorded as work. It is the external world for
// testing purposes: what issue was created, in which Project, with what Priority and
// what content — or which comment landed on which existing issue.
type Tracker interface {
	CreateIssue(ctx context.Context, req IssueRequest) (IssueRef, error)
	Comment(ctx context.Context, issueID, body string) error
	// IssueState answers whether a previously created issue is still open. Openness
	// is decided by state.type, never by the state's name, because the team's states
	// are customised and in Portuguese.
	IssueState(ctx context.Context, issueID string) (IssueState, error)
}

// IssueRequest is an issue about to be born in the Destino.
type IssueRequest struct {
	LinearProjectID string
	Title           string
	Body            string
	Priority        Priority

	// Labels carries the Fonte's free-form rótulo, created on demand.
	Labels []string

	// RelatedTo references the previous issue when this is a regressão after close.
	RelatedTo string

	// AttachmentURL is the Ocorrência's Sentry URL. attachmentCreate is idempotent
	// by url.
	AttachmentURL   string
	AttachmentTitle string
}

// IssueRef identifies an issue that now exists.
type IssueRef struct {
	ID         string
	Identifier string
	URL        string
}

// IssueState is the coarse open/closed judgement the reincidência path needs.
type IssueState struct {
	Exists bool
	// Open is false when state.type is completed, canceled or duplicate.
	Open       bool
	StateType  string
	Identifier string
	URL        string
}

// Workspace delivers the Projeto's directory ready for reading, and nothing more.
// The clone is updated to the configured branch before every analysis, so the
// diagnosis never points at code that has already changed.
type Workspace interface {
	// Prepare returns a directory holding the project at the configured branch.
	// The release function must be called when the analysis is done.
	Prepare(ctx context.Context, p Project) (dir string, release func(), err error)
}

// Clock is injectable because retry backoff and time windows cannot be tested with
// real waiting.
type Clock interface {
	Now() time.Time
	// Sleep waits for d, or returns early if the context is cancelled.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
