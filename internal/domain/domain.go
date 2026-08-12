// Package domain holds the ubiquitous language of the nightwatcher, as fixed by
// CONTEXT.md and the PRD. Identifiers are English by Go convention, but
// the *values* of the domain enums stay Portuguese because they are contractual:
// they surface on the command line, in the SQLite rows and in the agent's reply.
package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
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

	// MCPServers are this Projeto's own remote MCP servers. The global ones are NOT
	// folded in here: the store answers what was configured, and MergeMCPServers is
	// where the two become one universe, so a listing can still tell them apart.
	MCPServers []MCPServer
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

// MCPServer is a remote MCP server the long session may consult. Only remote: a
// stdio server would be a process running inside the container, which is precisely
// what the Diagnóstico does not do — "só lê, nunca executa" is structural here, not
// merely intended.
//
// It carries NO credential. Headers name an environment variable, and the value is
// read from the daemon's environment at invocation time, so the SQLite file stays
// copyable without leaking one — the rule 0001_init.sql states for the whole schema.
type MCPServer struct {
	ID int64

	// ProjectID is 0 for a global server, which applies to every Projeto. A Projeto's
	// own server of the same Name wins over the global one.
	ProjectID int64

	// Name forms the mcp__<Name>__ prefix of every tool the server exposes, which is
	// why it is narrower than a free-form label.
	Name string

	Transport MCPTransport
	URL       string

	// Description is what the session is told the server is for. A model does not use
	// a tool it does not know exists, so this reaches the prompt.
	Description string

	Headers []MCPHeader
}

// MCPHeader is one HTTP header sent to the server. ValueEnv is the NAME of an
// environment variable, never the value: what goes to the server is
// Prefix + os.Getenv(ValueEnv).
type MCPHeader struct {
	Name string

	// Prefix is fixed text in front of the secret — "Bearer " for the overwhelmingly
	// common case. It is stored, so it must never be the credential itself.
	Prefix string

	ValueEnv string
}

// MCPTransport is the remote transport. stdio is deliberately absent and has no
// constant here: a value that cannot be spelled cannot be configured by accident.
type MCPTransport string

const (
	MCPTransportHTTP MCPTransport = "http"
	MCPTransportSSE  MCPTransport = "sse"
)

// ParseMCPTransport accepts what an operator types, and nothing else. `stdio` gets an
// error that says why rather than "unknown", because it is the one wrong answer
// somebody will reasonably try.
func ParseMCPTransport(s string) (MCPTransport, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "http":
		return MCPTransportHTTP, nil
	case "sse":
		return MCPTransportSSE, nil
	case "stdio":
		return "", errors.New("transporte stdio não é suportado: ele seria um processo executando dentro do container da Análise, e o nightwatcher só lê")
	}
	return "", fmt.Errorf("transporte MCP desconhecido: %q; use http ou sse", s)
}

// mcpNamePattern is what a server name may be. It is narrow because the name becomes
// the prefix of a tool name (mcp__<name>__…), and a character the CLI does not accept
// there would fail at the init event of every Análise instead of here.
var mcpNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// mcpEnvPattern is the shape of an environment variable name, so a typo fails at
// `mcp add` rather than as an empty header at the first Diagnóstico.
var mcpEnvPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Validate refuses a server that could not work, and it lives here because three
// callers need the same answer: the CLI before writing, the store before persisting,
// and agentcli before invoking.
func (m MCPServer) Validate() error {
	if !mcpNamePattern.MatchString(m.Name) {
		return fmt.Errorf("nome de servidor MCP %q inválido: use minúsculas, dígitos, - e _, começando por letra ou dígito — ele vira o prefixo mcp__%s__ das ferramentas", m.Name, m.Name)
	}
	switch m.Transport {
	case MCPTransportHTTP, MCPTransportSSE:
	default:
		return fmt.Errorf("servidor MCP %s: transporte %q inválido; use http ou sse", m.Name, m.Transport)
	}
	u, err := url.Parse(strings.TrimSpace(m.URL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("servidor MCP %s: URL %q inválida", m.Name, m.URL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Plain http is accepted only for a loopback host, and even then the Análise
		// will not reach it: its container has its own network namespace, so localhost
		// in there is the container itself. Saying so here beats a timeout later.
		if !isLoopback(u.Hostname()) {
			return fmt.Errorf("servidor MCP %s: URL precisa ser https (http só é aceito para localhost, que o container da Análise não alcança)", m.Name)
		}
	default:
		return fmt.Errorf("servidor MCP %s: esquema %q não é suportado; use https", m.Name, u.Scheme)
	}
	seen := make(map[string]bool, len(m.Headers))
	for _, h := range m.Headers {
		if err := h.Validate(m.Name); err != nil {
			return err
		}
		key := strings.ToLower(h.Name)
		if seen[key] {
			return fmt.Errorf("servidor MCP %s: header %s declarado duas vezes", m.Name, h.Name)
		}
		seen[key] = true
	}
	return nil
}

// ToolPrefix is how a tool of this server is named in the session.
func (m MCPServer) ToolPrefix() string { return "mcp__" + m.Name }

// Host is what has to be reachable from the Análise's network, port included, so the
// failure message can name it.
func (m MCPServer) Host() string {
	u, err := url.Parse(strings.TrimSpace(m.URL))
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "http" {
		return u.Host + ":80"
	}
	return u.Host + ":443"
}

// mcpSecretHints are the words that betray a credential pasted where only fixed text
// belongs. The same list guards `config set`, and for the same reason: the database
// has to stay copyable without leaking one.
var mcpSecretHints = []string{"token", "secret", "key", "password", "senha", "credential", "credencial", "bearer "}

func (h MCPHeader) Validate(server string) error {
	if strings.TrimSpace(h.Name) == "" || strings.ContainsAny(h.Name, " \t\r\n:") {
		return fmt.Errorf("servidor MCP %s: nome de header %q inválido", server, h.Name)
	}
	if !mcpEnvPattern.MatchString(h.ValueEnv) {
		return fmt.Errorf("servidor MCP %s: header %s precisa apontar para uma variável de ambiente (ex.: NW_MCP_%s), veio %q", server, h.Name, strings.ToUpper(server), h.ValueEnv)
	}
	// The prefix is stored, so it may be "Bearer " and it may not be the token that
	// follows it. Long or secret-looking text here is somebody pasting the credential
	// into the wrong half of the flag, and that lands in SQLite forever.
	trimmed := strings.TrimSpace(h.Prefix)
	if len(trimmed) > 16 {
		return fmt.Errorf("servidor MCP %s: o prefixo do header %s tem %d caracteres; ele é texto fixo (ex.: \"Bearer \"), o segredo vai na variável %s", server, h.Name, len(trimmed), h.ValueEnv)
	}
	lower := strings.ToLower(trimmed)
	for _, hint := range mcpSecretHints {
		// "Bearer " alone is the legitimate prefix; "Bearer sk-…" is the mistake. The
		// hint list is consulted against what remains after the scheme word.
		if hint == "bearer " {
			continue
		}
		if strings.Contains(lower, hint) {
			return fmt.Errorf("servidor MCP %s: o prefixo do header %s parece uma credencial; ela vai na variável de ambiente %s, nunca no banco", server, h.Name, h.ValueEnv)
		}
	}
	return nil
}

// Value is what the header carries on the wire, given the process environment.
func (h MCPHeader) Value(lookup func(string) string) string {
	return h.Prefix + lookup(h.ValueEnv)
}

// NormalizeMCPPrefix puts back the space an authentication scheme needs and that
// everything between the operator and here wants to eat.
//
// `Bearer ` ends in a space that belongs to the value, and losing it produces
// `Bearersk-…` on the wire — a 401 that reads exactly like a revoked credential and
// is invisible in every listing, because the prefix prints identically either way.
// The guided flow trims every answer it reads, and a flag typed as
// `Authorization:Bearer:NW_X` looks right to whoever typed it.
//
// A prefix that is all letters is a scheme word and gets its space. Anything ending in
// punctuation — `token=`, `v1:` — was written deliberately and is left alone.
func NormalizeMCPPrefix(prefix string) string {
	if strings.TrimSpace(prefix) == "" {
		return ""
	}
	if strings.HasSuffix(prefix, " ") {
		return prefix
	}
	for _, r := range prefix {
		if !unicode.IsLetter(r) {
			return prefix
		}
	}
	return prefix + " "
}

// MergeMCPServers folds the global servers with a Projeto's own. The Projeto wins on a
// name collision, because a global default a Projeto cannot override is not a default.
// The order is stable so the invocation, the prompt and the perimeter all agree.
func MergeMCPServers(global, own []MCPServer) []MCPServer {
	byName := make(map[string]MCPServer, len(global)+len(own))
	var order []string
	for _, s := range append(append([]MCPServer{}, global...), own...) {
		if _, seen := byName[s.Name]; !seen {
			order = append(order, s.Name)
		}
		byName[s.Name] = s
	}
	sort.Strings(order)
	out := make([]MCPServer, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
