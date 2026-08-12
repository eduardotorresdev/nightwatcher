package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/sentryapi"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// DefaultGitHubAPI is the GitHub REST root. Reached read-only and nothing else: the
// nightwatcher never creates a branch, a commit or a pull request, so proving a
// repository exists and reading its default branch is the entire surface used.
const DefaultGitHubAPI = "https://api.github.com"

// remote is every service the live validations touch, with its base URL as a field.
// Fields and not constants because `config validate` and the guided `project add` are
// the two commands that reach the network, and both have to be testable against
// httptest servers.
type remote struct {
	github      string
	sentry      string
	linear      string
	linearToken string

	secrets config.Secrets
	http    *http.Client
}

// newRemote reads the hidden endpoint flags. A missing or empty flag falls back to
// production, so the commands behave the same whether or not the flags exist.
func newRemote(cmd *cobra.Command) *remote {
	pick := func(name, def string) string {
		if v, err := cmd.Flags().GetString(name); err == nil && strings.TrimSpace(v) != "" {
			return strings.TrimRight(strings.TrimSpace(v), "/")
		}
		return def
	}
	return &remote{
		github:      pick(flagGitHubAPI, DefaultGitHubAPI),
		sentry:      pick(flagSentryAPI, sentryapi.DefaultBaseURL),
		linear:      pick(flagLinearAPI, tracker.DefaultEndpoint),
		linearToken: pick(flagLinearTokenURL, tracker.DefaultTokenEndpoint),
		secrets:     config.LoadSecrets(),
		// A validation that hangs is a validation that gets skipped next time.
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// get performs an authenticated GET and decodes the body into out. The response body
// is read whole and truncated into the error message, so a failure says what the
// service answered — but the request never has its headers echoed anywhere, because
// that is where the credential is.
func (r *remote) get(ctx context.Context, endpoint, authorization string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("montando requisição para %s: %w", endpoint, err)
	}
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("alcançando %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("lendo a resposta de %s: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{Status: resp.StatusCode, Endpoint: endpoint, Body: string(body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("resposta ilegível de %s: %w", endpoint, err)
	}
	return nil
}

// httpError keeps the status code apart from the prose, so a 404 ("does not exist")
// can be told from a 401 ("the credential is wrong") by the caller and not by reading
// the message.
type httpError struct {
	Status   int
	Endpoint string
	Body     string
}

func (e *httpError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("HTTP %d em %s", e.Status, e.Endpoint)
	}
	return fmt.Sprintf("HTTP %d em %s: %s", e.Status, e.Endpoint, body)
}

// statusOf recovers the status code from anywhere in the error chain, so callers
// distinguish "does not exist" from "the credential is wrong" without matching prose.
func statusOf(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// ---------------------------------------------------------------- GitHub

// githubRepo is the slice of the repository payload the nightwatcher cares about.
type githubRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
}

// repo proves the repository exists and discovers its default branch, which is all a
// plain `GET /repos/{owner}/{repo}` is needed for.
func (r *remote) repo(ctx context.Context, spec string) (githubRepo, error) {
	owner, name, ok := splitRepo(spec)
	if !ok {
		return githubRepo{}, fmt.Errorf("repositório %q não está no formato owner/repo", spec)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s", r.github, url.PathEscape(owner), url.PathEscape(name))
	var out githubRepo
	err := r.get(ctx, endpoint, githubAuth(r.secrets.GitHubToken), map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}, &out)
	if err != nil {
		switch statusOf(err) {
		case http.StatusNotFound:
			// GitHub answers 404 for both "does not exist" and "your token cannot see
			// it", on purpose. Saying only "does not exist" would send the operator
			// hunting for a typo in a name that is right.
			return githubRepo{}, fmt.Errorf("o repositório %s não existe ou %s não o alcança", spec, config.EnvGitHubToken)
		case http.StatusUnauthorized, http.StatusForbidden:
			return githubRepo{}, fmt.Errorf("o GitHub recusou %s: %w", config.EnvGitHubToken, err)
		}
		return githubRepo{}, fmt.Errorf("consultando o repositório %s: %w", spec, err)
	}
	if out.DefaultBranch == "" {
		return githubRepo{}, fmt.Errorf("o GitHub respondeu sobre %s sem informar a branch default", spec)
	}
	return out, nil
}

// githubReach proves the credential itself works when there is no repository yet to
// prove it against.
func (r *remote) githubReach(ctx context.Context) error {
	var out struct {
		Login string `json:"login"`
	}
	err := r.get(ctx, r.github+"/user", githubAuth(r.secrets.GitHubToken), map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}, &out)
	if err != nil {
		return fmt.Errorf("o GitHub recusou %s: %w", config.EnvGitHubToken, err)
	}
	return nil
}

func githubAuth(token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	return "Bearer " + strings.TrimSpace(token)
}

func splitRepo(spec string) (owner, name string, ok bool) {
	spec = strings.TrimSpace(spec)
	spec = strings.TrimSuffix(spec, ".git")
	spec = strings.TrimPrefix(spec, "https://github.com/")
	spec = strings.Trim(spec, "/")

	parts := strings.Split(spec, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ---------------------------------------------------------------- Sentry

type sentryProject struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

// fonte confirms a Fonte Sentry exists inside the organisation. Sentry authenticates
// with `Bearer` — the Linear personal key next door must NOT carry that prefix, and
// mixing the two up is a silent 401.
func (r *remote) fonte(ctx context.Context, org, slug string) (sentryProject, error) {
	if strings.TrimSpace(org) == "" {
		return sentryProject{}, fmt.Errorf("a organização do Sentry não está configurada: `nightwatcher config set %s <org>`", settingSentryOrg)
	}

	endpoint := fmt.Sprintf("%s/projects/%s/%s/", r.sentry, url.PathEscape(org), url.PathEscape(slug))
	var out sentryProject
	if err := r.get(ctx, endpoint, "Bearer "+strings.TrimSpace(r.secrets.SentryToken), nil, &out); err != nil {
		switch statusOf(err) {
		case http.StatusNotFound:
			return sentryProject{}, fmt.Errorf("a Fonte %s não existe na organização %s do Sentry", slug, org)
		case http.StatusUnauthorized, http.StatusForbidden:
			return sentryProject{}, fmt.Errorf("o Sentry recusou %s: %w", config.EnvSentryToken, err)
		}
		return sentryProject{}, fmt.Errorf("consultando a Fonte %s: %w", slug, err)
	}
	return out, nil
}

// sentryReach proves the token works without needing to know the organisation, which
// is what `config validate` falls back to before any Projeto is registered.
func (r *remote) sentryReach(ctx context.Context) error {
	var out []sentryProject
	if err := r.get(ctx, r.sentry+"/projects/", "Bearer "+strings.TrimSpace(r.secrets.SentryToken), nil, &out); err != nil {
		return fmt.Errorf("o Sentry recusou %s: %w", config.EnvSentryToken, err)
	}
	return nil
}

// ---------------------------------------------------------------- Linear

// linearProject is a Project inside the Destino's team — the thing `project add` lists
// for selection by number.
type linearProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slugId"`
}

// listProjectsQuery is asked from here and not from internal/tracker because that
// package exposes no project listing: it owns issue
// creation, comments and issue state, and none of them need one.
const listProjectsQuery = `
query Projects {
  projects(first: 100, includeArchived: false) {
    nodes { id name slugId }
  }
}`

// linearProjects lists the Projects the credential can see, so the guided `project
// add` can offer them by number instead of asking for a UUID nobody has memorised.
func (r *remote) linearProjects(ctx context.Context) ([]linearProject, error) {
	authorization, err := r.linearAuthorization(ctx)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{"query": listProjectsQuery})
	if err != nil {
		return nil, fmt.Errorf("montando a consulta de Projects: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.linear, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("montando a consulta de Projects: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", authorization)

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alcançando o Linear: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("lendo a resposta do Linear: %w", err)
	}

	var parsed struct {
		Data struct {
			Projects struct {
				Nodes []linearProject `json:"nodes"`
			} `json:"projects"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("resposta ilegível do Linear (HTTP %d): %w", resp.StatusCode, err)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("o Linear recusou a consulta de Projects: %s", parsed.Errors[0].Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Status: resp.StatusCode, Endpoint: r.linear, Body: string(body)}
	}
	return parsed.Data.Projects.Nodes, nil
}

// linearAuthorization builds the header exactly the way internal/tracker does, because
// the two forms are different and confusing them is a 401 with no clue as to why: the
// OAuth token carries `Bearer`, the personal key goes raw.
func (r *remote) linearAuthorization(ctx context.Context) (string, error) {
	id := strings.TrimSpace(r.secrets.LinearClientID)
	secret := strings.TrimSpace(r.secrets.LinearClientSecret)
	if id != "" && secret != "" {
		token, err := r.linearAppToken(ctx, id, secret)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	}
	if key := strings.TrimSpace(r.secrets.LinearAPIKey); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("faltam credenciais do Linear (%s+%s ou %s)",
		config.EnvLinearClientID, config.EnvLinearClientSecret, config.EnvLinearAPIKey)
}

func (r *remote) linearAppToken(ctx context.Context, id, secret string) (string, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {tracker.DefaultScopes},
		"actor":      {"app"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.linearToken, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("pedindo token ao Linear: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(id, secret)

	resp, err := r.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("pedindo token ao Linear: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("lendo o token do Linear: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("o Linear recusou %s+%s: HTTP %d",
			config.EnvLinearClientID, config.EnvLinearClientSecret, resp.StatusCode)
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("o Linear devolveu um token ilegível para %s", config.EnvLinearClientID)
	}
	return out.AccessToken, nil
}

// linearClient builds the tracker Client `config validate` proves the Linear half
// with. Retries are cut short here: the daemon is right to wait out a rate limit, but
// an operator staring at `config validate` needs an answer, not patience.
func (r *remote) linearClient(team string) (*tracker.Client, error) {
	opts := tracker.Options{
		Secrets:       r.secrets,
		Endpoint:      r.linear,
		TokenEndpoint: r.linearToken,
		HTTPClient:    r.http,
		Retries:       1,
		BaseBackoff:   200 * time.Millisecond,
		MaxBackoff:    2 * time.Second,
	}
	// A team is either the UUID or the human key ("ACME"); the Client resolves the key
	// with one extra query and refuses to be built without one or the other.
	if looksLikeUUID(team) {
		opts.TeamID = team
	} else {
		opts.TeamKey = team
	}
	return tracker.New(opts)
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
