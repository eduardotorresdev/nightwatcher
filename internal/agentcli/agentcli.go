// Package agentcli implements domain.Agent by driving the Claude Code CLI headlessly.
//
// It is split in two seams on purpose. A Runner raises the process — DockerRunner is
// the ephemeral container that production uses, ExecRunner is the direct call a
// developer runs locally — and everything else in here reads the stream-json the CLI
// prints and turns it into a result or into a classified failure. The word "docker"
// appears in exactly one file, and the parsing has never heard of it.
//
// The CLI flags are hygiene. The containment is the container.
package agentcli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// Fallback ceilings, used only when a caller forgets to set AgentRequest.Timeout.
// The real numbers live in config.Tunables and arrive with the request; these exist
// so a missing one is a bounded session instead of an unbounded one.
const (
	defaultTriageTimeout = 2 * time.Minute
	defaultDeepTimeout   = 20 * time.Minute
)

// defaultCooldown is how long the nightwatcher stops dispatching after a usage limit.
// The CLI never says when the subscription resets — there is no resets_at field, no
// dedicated terminal_reason and no dedicated exit code — so the only honest reaction
// is to wait a fixed while.
const defaultCooldown = time.Hour

// Options configures the agent. Everything with a zero value has a working default,
// except the token: without it the session reaches the API and comes back 401, which
// is classified as an authentication failure and is not retried.
type Options struct {
	// Runner defaults to ExecRunner. Production passes DockerRunner.
	Runner Runner

	// Token is CLAUDE_CODE_OAUTH_TOKEN, the subscription credential.
	Token string

	// Home is the writable HOME for the session. Empty lets the Runner provide one,
	// which is what both runners do.
	Home string

	// Path defaults to the daemon's PATH, and is the only variable inherited from the
	// host.
	Path string

	// SystemPrompt goes to --append-system-prompt. In practice this is
	// prompts.System(); the wiring passes it so this package stays independent of the
	// prompt files.
	SystemPrompt string

	// MaxTurns and MaxBudgetUSD are the ceilings the CLI does understand. They do not
	// replace the wall-clock ceiling, which only exists because the caller enforces it.
	MaxTurns     int
	MaxBudgetUSD float64

	// RateLimitCooldown is how long the circuit breaker stays open.
	RateLimitCooldown time.Duration

	// Clock is injectable so the breaker can be tested without waiting an hour.
	Clock domain.Clock

	// Env resolves the MCP credentials by variable name. It is injectable for the same
	// reason the Clock is: a test must not depend on the machine's environment, and a
	// test that sets one really does change the process it runs in.
	Env func(string) string
}

// LookupEnv is where an MCP credential is read from. Note this is the DAEMON's
// environment, deliberately: the child's is built from scratch and receives only what
// the config references, so a variable that is not named by a server never reaches
// the session.
func (o Options) LookupEnv() func(string) string {
	if o.Env != nil {
		return o.Env
	}
	return os.Getenv
}

// Agent is the nightwatcher's domain.Agent.
type Agent struct {
	opts    Options
	breaker *breaker
}

var _ domain.Agent = (*Agent)(nil)

func New(o Options) *Agent {
	if o.Runner == nil {
		o.Runner = ExecRunner{}
	}
	if o.Path == "" {
		o.Path = os.Getenv("PATH")
	}
	if o.Clock == nil {
		o.Clock = domain.SystemClock{}
	}
	if o.RateLimitCooldown <= 0 {
		o.RateLimitCooldown = defaultCooldown
	}
	return &Agent{
		opts:    o,
		breaker: &breaker{cooldown: o.RateLimitCooldown, clock: o.Clock},
	}
}

// Run drives one session end to end: build the invocation, watch the stream as it
// arrives, and turn the outcome into a result or into a reason.
func (a *Agent) Run(ctx context.Context, req domain.AgentRequest) (domain.AgentResult, error) {
	if left, open := a.breaker.blocked(); open {
		return domain.AgentResult{}, &domain.AgentError{
			Kind: domain.AgentErrRateLimit,
			Detail: fmt.Sprintf(
				"disjuntor aberto: o limite de uso estourou e o CLI não avisa quando reseta; próxima tentativa em %s",
				left.Round(time.Second)),
		}
	}

	tools := effectiveTools(req)
	args, err := buildArgs(req, tools, a.opts)
	if err != nil {
		// A server that cannot be rendered is a misconfiguration, not an outage: it
		// will fail identically on every retry, so it goes down the no-retry path with
		// the reason on the issue.
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrPerimeter,
			Detail: err.Error(),
			Err:    err,
		}
	}
	if missing := MissingMCPEnv(req.MCPServers, a.opts.LookupEnv()); len(missing) > 0 {
		// An unset variable becomes an empty header, and an empty header comes back as
		// a 401 that reads exactly like a revoked credential. Naming the variable here
		// is the difference between a five-minute fix and an afternoon.
		return domain.AgentResult{}, &domain.AgentError{
			Kind: domain.AgentErrPerimeter,
			Detail: "credencial de servidor MCP ausente do ambiente: " +
				strings.Join(missing, ", "),
		}
	}

	s := newStream(tools, req.MCPServers)
	spec := Spec{
		Args:    args,
		Env:     buildEnv(req, a.opts, a.opts.LookupEnv()),
		WorkDir: req.WorkDir,
		Stdin:   req.Prompt,
		Timeout: timeoutFor(req),
		// Watching the stream as it arrives is what makes the perimeter check a gate
		// and not a post-mortem: an MCP server declared in the cloned repository is
		// caught at the init event, before the untrusted text is ever answered.
		OnLine: s.line,
	}

	out, err := a.opts.Runner.Run(ctx, spec)
	if err != nil {
		// The process never started: no binary, no docker daemon. The service is out,
		// and that is worth waiting out.
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrUnavailable,
			Detail: err.Error(),
			Err:    err,
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A shutdown mid-session is not a malformed reply. Classifying it as one would
		// turn every deploy into a batch of unanalysed issues.
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrTimeout,
			Detail: "a sessão foi cancelada antes de terminar",
			Err:    ctxErr,
		}
	}
	if s.lines == 0 && len(out.Stdout) > 0 {
		// A Runner that buffered instead of streaming still gets read.
		s.feed(out.Stdout)
	}

	res, err := interpret(out, s)
	if err != nil && domain.ClassifyAgentError(err) == domain.AgentErrRateLimit {
		a.breaker.trip()
	}
	return res, err
}

func timeoutFor(req domain.AgentRequest) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	if req.Kind == domain.SessionTriage {
		return defaultTriageTimeout
	}
	return defaultDeepTimeout
}

// breaker stops dispatching sessions for a while after a usage limit. Retrying into a
// wall costs the same subscription it is trying to protect, and the CLI gives no reset
// time to wait for — so the wait is a fixed period, decided here.
type breaker struct {
	cooldown time.Duration
	clock    domain.Clock

	mu    sync.Mutex
	until time.Time
}

func (b *breaker) blocked() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until.IsZero() {
		return 0, false
	}
	left := b.until.Sub(b.clock.Now())
	if left <= 0 {
		b.until = time.Time{}
		return 0, false
	}
	return left, true
}

func (b *breaker) trip() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.until = b.clock.Now().Add(b.cooldown)
}
