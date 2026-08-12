// Package config separates the two kinds of configuration the nightwatcher has, and
// keeps them apart on purpose: secrets live only in environment variables so the
// database can be read, copied and versioned without leaking a credential, while the
// operating numbers live in the database so they can be recalibrated without a
// redeploy.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Secrets are read from the environment and never written anywhere.
type Secrets struct {
	// SentryClientSecret is the internal integration's Client Secret, used to verify
	// the webhook HMAC.
	SentryClientSecret string
	// SentryToken authenticates the Sentry REST API.
	SentryToken string

	// Linear service-account credentials — OAuth client_credentials with actor=app.
	LinearClientID     string
	LinearClientSecret string
	// LinearAPIKey is the personal-key fallback. It goes in `Authorization: <key>`
	// with NO `Bearer` prefix.
	LinearAPIKey string

	// GitHubToken is the global, read-only credential. The nightwatcher never
	// creates a branch, a commit or a pull request.
	GitHubToken string

	// ClaudeOAuthToken authenticates the CLI against the subscription. Note that
	// ANTHROPIC_API_KEY takes precedence over it and would silently bill per token,
	// which is why the agent container builds its Env from scratch.
	ClaudeOAuthToken string
}

// Paths locate the persistent volume shared by the daemon and the ephemeral
// `dokku run` containers.
type Paths struct {
	DB     string
	Clones string
}

// Env names, kept in one place so `config validate` can name what is missing.
const (
	EnvSentryClientSecret = "SENTRY_CLIENT_SECRET"
	EnvSentryToken        = "SENTRY_TOKEN"
	EnvLinearClientID     = "LINEAR_CLIENT_ID"
	EnvLinearClientSecret = "LINEAR_CLIENT_SECRET"
	EnvLinearAPIKey       = "LINEAR_API_KEY"
	EnvGitHubToken        = "GITHUB_TOKEN"
	EnvClaudeOAuthToken   = "CLAUDE_CODE_OAUTH_TOKEN"
	EnvDBPath             = "NIGHTWATCHER_DB"
	EnvClonesPath         = "NIGHTWATCHER_CLONES"
	EnvPort               = "PORT"
)

// LoadSecrets reads the environment. It does not validate: `config validate` is the
// command that proves each credential actually works, because validating the shape
// of a token protects against nothing.
func LoadSecrets() Secrets {
	return Secrets{
		SentryClientSecret: os.Getenv(EnvSentryClientSecret),
		SentryToken:        os.Getenv(EnvSentryToken),
		LinearClientID:     os.Getenv(EnvLinearClientID),
		LinearClientSecret: os.Getenv(EnvLinearClientSecret),
		LinearAPIKey:       os.Getenv(EnvLinearAPIKey),
		GitHubToken:        os.Getenv(EnvGitHubToken),
		ClaudeOAuthToken:   os.Getenv(EnvClaudeOAuthToken),
	}
}

// Missing names the secrets that are unset, for a clear failure at startup instead
// of a confusing one at the first webhook.
func (s Secrets) Missing() []string {
	var out []string
	req := []struct {
		name string
		val  string
	}{
		{EnvSentryClientSecret, s.SentryClientSecret},
		{EnvSentryToken, s.SentryToken},
		{EnvGitHubToken, s.GitHubToken},
		{EnvClaudeOAuthToken, s.ClaudeOAuthToken},
	}
	for _, r := range req {
		if strings.TrimSpace(r.val) == "" {
			out = append(out, r.name)
		}
	}
	if strings.TrimSpace(s.LinearAPIKey) == "" &&
		(strings.TrimSpace(s.LinearClientID) == "" || strings.TrimSpace(s.LinearClientSecret) == "") {
		out = append(out, EnvLinearAPIKey+" (ou "+EnvLinearClientID+"+"+EnvLinearClientSecret+")")
	}
	return out
}

// LoadPaths resolves the persistent volume layout, defaulting to the mount point
// used by the Dokku app.
func LoadPaths() Paths {
	db := os.Getenv(EnvDBPath)
	if db == "" {
		db = "/data/nightwatcher.db"
	}
	clones := os.Getenv(EnvClonesPath)
	if clones == "" {
		clones = "/data/clones"
	}
	return Paths{DB: db, Clones: clones}
}

// Port is the HTTP port the webhook listens on.
func Port() string {
	if p := os.Getenv(EnvPort); p != "" {
		return p
	}
	return "5000"
}

// Tunables are the operating numbers of the daemon. The profile is deliberately
// conservative: every number errs towards processing slowly rather than blowing up
// the VPS or the Claude subscription. They live in the database precisely so they can
// be corrected without a deploy.
type Tunables struct {
	// Concurrency is how many Análises run at once. Each one raises its own
	// container with a Claude session inside.
	Concurrency int

	// TriageTimeout caps the short session. Small prompt, no repository — past this
	// it has hung, not slowed down.
	TriageTimeout time.Duration

	// DeepTimeout caps Diagnóstico and Estudo. The predecessor died at 5 minutes
	// WITHOUT opening the repository; v1 opens it, and a single ceiling was what
	// killed the deepest analyses.
	DeepTimeout time.Duration

	// MaxAttempts and Backoff cover a short outage and a usage limit that resets in
	// about an hour. Past that it becomes an unanalysed issue.
	MaxAttempts int
	Backoff     []time.Duration

	// PerProjectHourly caps analyses per Projeto per hour. The excess WAITS in the
	// queue; it is never discarded.
	PerProjectHourly int

	// MaxQueue is the point past which the arrival rate is a symptom, not load.
	MaxQueue int

	// LeaseTTL is how long a claimed queue item stays claimed. A worker that dies
	// lets the lease expire and the item becomes takeable again — which is how the
	// queue survives SIGKILL, OOM and a Dokku deploy with no coordination at all.
	LeaseTTL time.Duration
}

// Defaults is the conservative profile.
//
// MaxAttempts 4 and a Backoff of length 4 are deliberately off by one: with four
// attempts the fourth wait never happens, because the fourth failure ends the
// Análise instead of scheduling a fifth try. The ceiling is the session budget —
// four attempts times DeepTimeout is the "~80 minutos de sessão por Ocorrência" the
// ticket set — and the last entry exists so that raising the ceiling with
// `config set max_attempts 5` does not silently reuse the 15-minute wait.
func Defaults() Tunables {
	return Tunables{
		Concurrency:      2,
		TriageTimeout:    2 * time.Minute,
		DeepTimeout:      20 * time.Minute,
		MaxAttempts:      4,
		Backoff:          []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute},
		PerProjectHourly: 20,
		MaxQueue:         200,
		LeaseTTL:         25 * time.Minute,
	}
}

// BackoffFor returns the wait before the given attempt number (1-based).
func (t Tunables) BackoffFor(attempt int) time.Duration {
	if len(t.Backoff) == 0 {
		return time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(t.Backoff) {
		return t.Backoff[len(t.Backoff)-1]
	}
	return t.Backoff[attempt-1]
}

// Settable is the set of keys `nightwatcher config set` accepts.
var Settable = []string{
	"concurrency",
	"triage_timeout",
	"deep_timeout",
	"max_attempts",
	"per_project_hourly",
	"max_queue",
	"lease_ttl",
}

// Apply overlays one stored key/value onto the tunables.
func (t *Tunables) Apply(key, value string) error {
	// Every duration key is rejected at zero and below, and that is not tidiness.
	// `config set lease_ttl 0s` is reachable from the command line, and a lease that
	// has already expired the instant it is taken means every worker steals every item
	// from every other worker: the same Ocorrência analysed twice and two issues born
	// from it. A `triage_timeout` of 0s is the same defect with a different face — the
	// ceiling fires before the session can answer, so every Triagem times out.
	dur := func() (time.Duration, error) {
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return 0, err
		}
		if d <= 0 {
			return 0, fmt.Errorf("duração precisa ser positiva")
		}
		return d, nil
	}
	// strconv and not fmt.Sscanf: Sscanf stops at the first character it cannot read
	// and reports success, so `config set concurrency 5x` would silently store 5 and
	// `2m` would silently store 2. A typo has to fail where the operator is looking.
	num := func() (int, error) { return strconv.Atoi(strings.TrimSpace(value)) }
	switch key {
	case "concurrency":
		n, err := num()
		if err != nil || n < 1 {
			return fmt.Errorf("concurrency precisa ser inteiro >= 1, veio %q", value)
		}
		t.Concurrency = n
	case "triage_timeout":
		d, err := dur()
		if err != nil {
			return fmt.Errorf("triage_timeout precisa ser duração positiva (ex.: 2m), veio %q: %w", value, err)
		}
		t.TriageTimeout = d
	case "deep_timeout":
		d, err := dur()
		if err != nil {
			return fmt.Errorf("deep_timeout precisa ser duração positiva (ex.: 20m), veio %q: %w", value, err)
		}
		t.DeepTimeout = d
	case "max_attempts":
		n, err := num()
		if err != nil || n < 1 {
			return fmt.Errorf("max_attempts precisa ser inteiro >= 1, veio %q", value)
		}
		t.MaxAttempts = n
	case "per_project_hourly":
		n, err := num()
		if err != nil || n < 1 {
			return fmt.Errorf("per_project_hourly precisa ser inteiro >= 1, veio %q", value)
		}
		t.PerProjectHourly = n
	case "max_queue":
		n, err := num()
		if err != nil || n < 1 {
			return fmt.Errorf("max_queue precisa ser inteiro >= 1, veio %q", value)
		}
		t.MaxQueue = n
	case "lease_ttl":
		d, err := dur()
		if err != nil {
			return fmt.Errorf("lease_ttl precisa ser duração positiva (ex.: 25m), veio %q: %w", value, err)
		}
		t.LeaseTTL = d
	default:
		return fmt.Errorf("chave desconhecida %q; conhecidas: %s", key, strings.Join(Settable, ", "))
	}
	return nil
}
