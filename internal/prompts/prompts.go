// Package prompts holds the three sessions' prompts as versioned files in the
// repository, embedded in the binary, plus the strict envelope the agent has to
// answer with.
//
// They are files and not string literals for two reasons: a prompt change shows up
// in a diff and gets reviewed like any other change, and `analysis show` can name
// which version produced a given Session — which is how a bad Triagem gets traced
// back to the wording that caused it.
package prompts

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

//go:embed templates/*.md
var files embed.FS

// Version identifiers, stored alongside the Session so a prompt change is visible in
// the record instead of being inferred from a date.
const (
	VersionSystem    = "sistema.v1"
	VersionTriage    = "triagem.v1"
	VersionDiagnosis = "diagnostico.v1"
	VersionStudy     = "estudo.v1"
)

var templates = template.Must(template.ParseFS(files, "templates/*.md"))

// System is the session-wide instruction handed to the CLI through
// --append-system-prompt. It repeats the untrusted-content rule outside the user's
// reach: the Ocorrência text lands in the user turn, so an instruction embedded in it
// argues against a system prompt it never gets to touch.
func System() string {
	b, err := files.ReadFile("templates/" + VersionSystem + ".md")
	if err != nil {
		// Unreachable: the file is embedded at build time.
		panic("prompts: prompt de sistema ausente do binário: " + err.Error())
	}
	return string(b)
}

// Version names the prompt file that Render would use for this session.
func Version(kind domain.SessionKind) (string, error) {
	switch kind {
	case domain.SessionTriage:
		return VersionTriage, nil
	case domain.SessionDiagnosis:
		return VersionDiagnosis, nil
	case domain.SessionStudy:
		return VersionStudy, nil
	}
	return "", fmt.Errorf("sessão desconhecida: %q", string(kind))
}

// Context is everything the prompt needs that the Ocorrência itself does not carry.
//
// Three of these fields exist because domain.Occurrence has no place for them: the
// feedback body and the stack trace live inside the webhook's RawBody, and the
// Prioridade facts belong to the rule that ran before the session. Whoever renders
// the prompt extracts them.
type Context struct {
	ProjectName string
	Repo        string
	Branch      string

	// SourceLabel is the Fonte's free-form rótulo — "web", "api" — which is what
	// tells the agent which layer it is looking at.
	SourceLabel string

	// WorkDir is where the read-only clone is visible from INSIDE the session, which
	// is not the host path when the session runs in a container.
	WorkDir string

	// Message is the free text of the Ocorrência: the feedback a user published, or
	// the error message Sentry captured. Untrusted.
	Message string

	// StackTrace is the stack Sentry delivered, when there is one. Untrusted, because
	// user-supplied values show up inside frames.
	StackTrace string

	// BasePriority is what the deterministic rule already decided and PriorityFacts
	// are the facts it decided from. The agent adjusts against these, never against a
	// blank slate — the predecessor's free-form score piled every Ocorrência onto the
	// same two numbers.
	BasePriority  domain.Priority
	PriorityFacts []string

	// Recurrence describes the previous issue when this Ocorrência already produced
	// one. Empty the first time round.
	Recurrence string

	// MCPServers are the remote MCP servers this session can consult. They reach the
	// prompt because a model does not use a tool it does not know exists. Nothing
	// here is secret — name, URL and what the server is for — and the credential is
	// not part of the type at all.
	MCPServers []domain.MCPServer
}

// view is what the templates actually see. Building it here instead of handing the
// domain types to the template keeps every bit of formatting — dates, empty fields,
// the delimiters — in Go, where it is testable.
type view struct {
	Version string

	ProjectName string
	Repo        string
	Branch      string
	SourceLabel string
	WorkDir     string

	Category    string
	Level       string
	Substatus   string
	Action      string
	Platform    string
	ShortID     string
	Environment string
	Count       int64
	UserCount   int64
	FirstSeen   string
	LastSeen    string
	Permalink   string
	Recurrence  string

	BasePriority  string
	PriorityFacts []string

	MCPServers []mcpView

	BeginMarker string
	EndMarker   string
	Untrusted   string
}

// mcpView is one server as the template sees it. The headers are absent by
// construction: what a template cannot reach, a template cannot print.
type mcpView struct {
	ToolPrefix  string
	Name        string
	URL         string
	Description string
}

func mcpViews(servers []domain.MCPServer) []mcpView {
	out := make([]mcpView, 0, len(servers))
	for _, s := range servers {
		out = append(out, mcpView{
			ToolPrefix:  s.ToolPrefix(),
			Name:        s.Name,
			URL:         s.URL,
			Description: strings.TrimSpace(s.Description),
		})
	}
	return out
}

// Render produces the full prompt for one session. It is deterministic: the same
// Ocorrência and the same Context render byte-for-byte the same text, which is what
// makes `occurrence replay` a comparison of prompts and not of coincidences.
func Render(kind domain.SessionKind, occ domain.Occurrence, c Context) (string, error) {
	version, err := Version(kind)
	if err != nil {
		return "", err
	}

	untrusted := untrustedBlob(occ, c)
	begin, end := markers(untrusted)

	v := view{
		Version:       version,
		ProjectName:   fallback(c.ProjectName),
		Repo:          fallback(c.Repo),
		Branch:        fallback(c.Branch),
		SourceLabel:   fallback(c.SourceLabel),
		WorkDir:       fallback(c.WorkDir),
		Category:      fallback(string(occ.Category)),
		Level:         fallback(string(occ.Level)),
		Substatus:     fallback(string(occ.Substatus)),
		Action:        fallback(occ.Action),
		Platform:      fallback(occ.Platform),
		ShortID:       fallback(occ.ShortID),
		Environment:   fallback(occ.Environment),
		Count:         occ.Count,
		UserCount:     occ.UserCount,
		FirstSeen:     stamp(occ.FirstSeen),
		LastSeen:      stamp(occ.LastSeen),
		Permalink:     fallback(firstNonEmpty(occ.Permalink, occ.WebURL)),
		Recurrence:    strings.TrimSpace(c.Recurrence),
		BasePriority:  c.BasePriority.String(),
		PriorityFacts: c.PriorityFacts,
		MCPServers:    mcpViews(c.MCPServers),
		BeginMarker:   begin,
		EndMarker:     end,
		Untrusted:     untrusted,
	}

	var b strings.Builder
	if err := templates.ExecuteTemplate(&b, version+".md", v); err != nil {
		return "", fmt.Errorf("não consegui montar o prompt de %s: %w", string(kind), err)
	}
	return b.String(), nil
}

// untrustedBlob gathers every piece of free text a human or a runtime wrote. It all
// goes inside the delimiters together: splitting it across the prompt would give an
// injected instruction a second place to land.
func untrustedBlob(occ domain.Occurrence, c Context) string {
	var b strings.Builder
	field(&b, "Título", occ.Title)
	field(&b, "Culprit", occ.Culprit)
	field(&b, "Mensagem", c.Message)
	if s := strings.TrimSpace(c.StackTrace); s != "" {
		b.WriteString("Stack trace:\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		// An Ocorrência with no text at all is itself the answer to the Triagem, so
		// say so rather than rendering an empty block the agent has to guess about.
		return "(a Ocorrência chegou sem texto algum)"
	}
	return out
}

func field(b *strings.Builder, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}

// markers derives the delimiters from the SHA-256 of the untrusted bytes themselves.
//
// The Ocorrência text is written by an end user, so the boundary between data and
// instruction has to be one the author cannot forge. A fixed delimiter only has to be
// copied: the attacker writes the closing marker in the middle of their feedback and
// everything after it reads as instruction. A content-derived mark cannot be copied,
// because closing the block early would mean writing text that contains a prefix of
// its own hash — a preimage they would have to solve, not a string they can paste.
//
// It stays deterministic on purpose: the same Ocorrência renders the same prompt, so
// replaying a known case against a changed prompt compares the prompts and nothing
// else.
func markers(untrusted string) (begin, end string) {
	sum := sha256.Sum256([]byte(untrusted))
	mark := hex.EncodeToString(sum[:6])
	return "[[[OCORRENCIA:" + mark + "]]]", "[[[FIM-OCORRENCIA:" + mark + "]]]"
}

func fallback(s string) string {
	if strings.TrimSpace(s) == "" {
		return "não informado"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "não informado"
	}
	return t.UTC().Format(time.RFC3339)
}
