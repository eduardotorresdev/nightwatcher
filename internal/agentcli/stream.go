package agentcli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// The session is read as `stream-json` rather than `json` for one reason: the
// system/api_retry event is the only place where a usage limit shows up as a closed
// enum instead of as a sentence in English that changes between versions.
// That event is what puts "limite de uso" on the failure issue instead of "erro".

type envelopeType struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
}

// initEvent is the session's declaration of what it actually loaded. It is cheap to
// read and it catches host configuration, a repository's own .mcp.json, or a plugin
// leaking into a session that is about to read untrusted text.
type initEvent struct {
	Tools          []string          `json:"tools"`
	MCPServers     []json.RawMessage `json:"mcp_servers"`
	Plugins        []json.RawMessage `json:"plugins"`
	SlashCommands  []json.RawMessage `json:"slash_commands"`
	PermissionMode string            `json:"permissionMode"`
	APIKeySource   string            `json:"apiKeySource"`
	Model          string            `json:"model"`
	CWD            string            `json:"cwd"`
}

// retryEvent precedes every API retry. Its `error` field is a documented closed enum
// — rate_limit, authentication_failed, billing_error, overloaded and friends.
type retryEvent struct {
	Attempt     int    `json:"attempt"`
	MaxRetries  int    `json:"max_retries"`
	ErrorStatus int    `json:"error_status"`
	Error       string `json:"error"`
}

// resultEvent is the last line of the stream. It exists even when the session was
// interrupted by SIGTERM.
type resultEvent struct {
	Subtype        string          `json:"subtype"`
	IsError        bool            `json:"is_error"`
	TerminalReason string          `json:"terminal_reason"`
	APIErrorStatus int             `json:"api_error_status"`
	Result         json.RawMessage `json:"result"`
	NumTurns       int             `json:"num_turns"`
	TotalCostUSD   float64         `json:"total_cost_usd"`
	SessionID      string          `json:"session_id"`
}

// text is the session's answer. `result` is null on an interrupted run, so a nil here
// is normal and not a parse failure.
func (r *resultEvent) text() string {
	if len(r.Result) == 0 || bytes.Equal(r.Result, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(r.Result, &s); err == nil {
		return s
	}
	return string(r.Result)
}

// stream accumulates the events of one session and inspects the perimeter as they
// arrive.
type stream struct {
	expected map[string]bool

	init    *initEvent
	retries []retryEvent
	result  *resultEvent

	lines   int
	garbage int

	// perimeter holds the first violation seen. It is checked before anything else,
	// because a session that did not have the tool universe we asked for produced
	// nothing worth reading.
	perimeter error
}

func newStream(expectedTools []string) *stream {
	s := &stream{expected: make(map[string]bool, len(expectedTools))}
	for _, t := range expectedTools {
		s.expected[strings.TrimSpace(t)] = true
	}
	return s
}

// line consumes one stream-json line. A non-nil return aborts the session.
func (s *stream) line(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	s.lines++

	var env envelopeType
	if err := json.Unmarshal(b, &env); err != nil {
		s.garbage++
		return nil
	}

	switch {
	case env.Type == "system" && env.Subtype == "init":
		var ev initEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			s.garbage++
			return nil
		}
		s.init = &ev
		if err := s.checkPerimeter(&ev); err != nil && s.perimeter == nil {
			s.perimeter = err
			return err
		}
	case env.Type == "system" && env.Subtype == "api_retry":
		var ev retryEvent
		if err := json.Unmarshal(b, &ev); err == nil {
			s.retries = append(s.retries, ev)
		}
	case env.Type == "result":
		var ev resultEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			s.garbage++
			return nil
		}
		s.result = &ev
	}
	return nil
}

// feed reads a whole buffered stream, for a Runner that did not stream line by line.
func (s *stream) feed(stdout []byte) {
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		_ = s.line(sc.Bytes())
	}
}

// checkPerimeter compares what the session says it loaded against what was asked for.
//
// The asymmetry is deliberate: a tool we asked for and did NOT get is less capability
// and never a violation, while a tool we did not ask for is config leaking in from
// somewhere — the host's settings, a plugin, or the cloned repository's own
// .mcp.json, which is content nobody here controls.
func (s *stream) checkPerimeter(ev *initEvent) error {
	var bad []string
	for _, t := range ev.Tools {
		t = strings.TrimSpace(t)
		if t == "" || s.expected[t] {
			continue
		}
		// EndConversation cannot be removed while any other tool exists, so its
		// presence says nothing about leakage.
		if t == "EndConversation" {
			continue
		}
		bad = append(bad, "ferramenta fora do combinado: "+t)
	}
	if n := len(ev.MCPServers); n > 0 {
		bad = append(bad, fmt.Sprintf("%d servidor(es) MCP carregado(s)", n))
	}
	if n := len(ev.Plugins); n > 0 {
		bad = append(bad, fmt.Sprintf("%d plugin(s) carregado(s)", n))
	}
	if n := len(ev.SlashCommands); n > 0 {
		bad = append(bad, fmt.Sprintf("%d slash command(s) carregado(s)", n))
	}
	if ev.PermissionMode != "" && ev.PermissionMode != permissionMode {
		bad = append(bad, "modo de permissão "+ev.PermissionMode+" em vez de "+permissionMode)
	}
	// The credential the session actually used. ANTHROPIC_API_KEY outranks the
	// subscription token, so this is the money bug reporting itself at runtime. Only
	// a positive match is a violation: the field's values are not documented, and an
	// unknown one must not fail every analysis.
	if strings.Contains(strings.ToUpper(ev.APIKeySource), "ANTHROPIC_API_KEY") {
		bad = append(bad, "a sessão autenticou por ANTHROPIC_API_KEY, que cobra por token")
	}
	if len(bad) == 0 {
		return nil
	}
	return errors.New(strings.Join(bad, "; "))
}

// interpret turns a finished session into a result or into a classified failure. It
// is pure over what the outside world produced, which is what makes it testable
// against recorded streams.
func interpret(out Outcome, s *stream) (domain.AgentResult, error) {
	if s.perimeter != nil {
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrPerimeter,
			Detail: s.perimeter.Error(),
		}
	}

	// Our own ceiling. It arrives as SIGTERM, so the CLI usually still prints a
	// well-formed result with terminal_reason "aborted_streaming" — the interruption
	// is recorded rather than silent, though total_cost_usd comes back zeroed and the
	// cost accounting for the run is lost.
	if out.TimedOut || (s.result != nil && isAbort(s.result.TerminalReason)) {
		reason := "a sessão foi interrompida antes de responder"
		if out.TimedOut {
			reason = "a sessão passou do teto de tempo e foi interrompida"
		}
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrTimeout,
			Detail: fmt.Sprintf("%s depois de %s", reason, out.Duration.Round(time.Second)),
		}
	}

	if s.result == nil {
		// Nothing at all came out of the pipe: the CLI never got far enough to print
		// a single line. That is not a malformed reply, it is the tool being out —
		// a docker daemon that is down, an image that is missing, a network that does
		// not exist. Malformed is never retried, so calling it that would turn the
		// first infrastructure hiccup into an Ocorrência marked unanalysable.
		if s.lines == 0 {
			return domain.AgentResult{}, &domain.AgentError{
				Kind:   domain.AgentErrUnavailable,
				Detail: "a sessão não chegou a produzir stream" + streamHint(out, s),
			}
		}
		// Lines came out and none of them closed the session: the reply itself is
		// broken, and repeating it produces the same broken reply.
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrMalformed,
			Detail: "a sessão terminou sem evento result" + streamHint(out, s),
		}
	}

	if s.result.IsError {
		// Ahead of the init gate on purpose: a session that failed has no answer to
		// protect. Reporting "violação de perímetro" on top of a 401 or a usage limit
		// would hide the real reason from the failure issue, keep the breaker shut and
		// make a retryable failure non-retryable.
		kind, detail := classifyFailure(s.result, s.retries)
		return domain.AgentResult{}, &domain.AgentError{Kind: kind, Detail: detail}
	}

	if s.init == nil {
		// The session ran and answered, but never declared what it had loaded — so
		// there is no way to know whether it was the session we asked for, and this
		// one had text written by a stranger in front of it.
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrPerimeter,
			Detail: "a sessão não declarou o que carregou: nenhum evento system/init no stream" + streamHint(out, s),
		}
	}

	// `subtype` is deliberately not consulted here. An authentication failure comes
	// back with subtype "success" and is_error true; trusting the subtype
	// would publish "diagnóstico pronto" on top of a 401.
	text := strings.TrimSpace(s.result.text())
	if text == "" {
		return domain.AgentResult{}, &domain.AgentError{
			Kind:   domain.AgentErrMalformed,
			Detail: "a sessão terminou sem erro e sem resposta",
		}
	}

	return domain.AgentResult{
		Text:     text,
		Duration: out.Duration,
		CostUSD:  s.result.TotalCostUSD,
		NumTurns: s.result.NumTurns,
	}, nil
}

// classifyFailure ranks the evidence: the HTTP status the CLI reported, then the
// retry events with their closed enum, and only last the human-facing sentence — that
// one is UI text and changes between versions, so it decides nothing on its own.
func classifyFailure(res *resultEvent, retries []retryEvent) (domain.AgentErrorKind, string) {
	detail := strings.TrimSpace(res.text())
	if detail == "" {
		detail = res.TerminalReason
	}
	if detail == "" {
		detail = res.Subtype
	}

	switch {
	case res.APIErrorStatus == 401 || res.APIErrorStatus == 403:
		return domain.AgentErrAuth, detail
	case res.APIErrorStatus == 429:
		return domain.AgentErrRateLimit, detail
	case res.APIErrorStatus >= 500:
		return domain.AgentErrUnavailable, detail
	}

	// A ceiling of ours, reached from the inside. It is not wall-clock time, but it is
	// the same shape of event: the session was cut short before answering.
	switch res.Subtype {
	case "error_max_turns":
		return domain.AgentErrTimeout, "limite de turnos atingido: " + detail
	case "error_max_budget_usd":
		return domain.AgentErrTimeout, "teto de custo atingido: " + detail
	}

	if kind, ok := kindFromRetries(retries); ok {
		return kind, detail
	}
	if kind, ok := kindFromText(detail); ok {
		return kind, detail
	}
	return domain.AgentErrUnavailable, detail
}

// kindFromRetries reads the retry events backwards: the last thing the API said about
// the session is the reason it ended.
func kindFromRetries(retries []retryEvent) (domain.AgentErrorKind, bool) {
	for i := len(retries) - 1; i >= 0; i-- {
		switch retries[i].Error {
		case "rate_limit":
			return domain.AgentErrRateLimit, true
		case "authentication_failed", "oauth_org_not_allowed", "billing_error":
			return domain.AgentErrAuth, true
		case "overloaded", "server_error":
			return domain.AgentErrUnavailable, true
		}
	}
	return "", false
}

var (
	rateLimitMarks = []string{
		"hit your session limit", "hit your weekly limit", "hit your opus limit",
		"usage limit reached", "rate limit", "(429)",
	}
	authMarks = []string{
		"failed to authenticate", "authentication failed", "oauth", "login expired",
		"please run /login", "invalid api key", "credit balance", "(401)", "(403)",
	}
)

func kindFromText(s string) (domain.AgentErrorKind, bool) {
	s = strings.ToLower(s)
	for _, m := range rateLimitMarks {
		if strings.Contains(s, m) {
			return domain.AgentErrRateLimit, true
		}
	}
	for _, m := range authMarks {
		if strings.Contains(s, m) {
			return domain.AgentErrAuth, true
		}
	}
	return "", false
}

func isAbort(terminalReason string) bool {
	return terminalReason == "aborted_streaming" || terminalReason == "aborted_tools"
}

// streamHint says what actually came out, because "malformed" with no evidence is the
// silent failure this whole system exists to avoid. Errors are printed on stdout, not
// stderr, so both are worth showing.
func streamHint(out Outcome, s *stream) (hint string) {
	parts := []string{fmt.Sprintf("%d linha(s), %d ilegível(is), saída %d", s.lines, s.garbage, out.ExitCode)}
	if out.ReadErr != nil {
		parts = append(parts, "leitura interrompida: "+out.ReadErr.Error())
	}
	if tail := excerpt(out.Stderr); tail != "" {
		parts = append(parts, "stderr: "+tail)
	}
	if s.lines == 0 {
		if head := excerpt(out.Stdout); head != "" {
			parts = append(parts, "stdout: "+head)
		}
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func excerpt(b []byte) string {
	const max = 200
	r := []rune(strings.Join(strings.Fields(string(b)), " "))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
