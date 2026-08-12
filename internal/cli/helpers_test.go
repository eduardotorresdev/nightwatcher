package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// The tests drive the real cobra tree — NewRoot, SetArgs, SetOut — against a real
// SQLite file, and assert on what came out and on the rows that landed. Nothing in
// here inspects the commands' internals, and nothing here reaches the real network:
// the three services live behind httptest servers wired in through the hidden
// endpoint flags.

// fixed is the instant the fixtures are dated from. A fake Clock, never time.Now():
// the listing prints timestamps and a test that prints the wall clock is a test that
// asserts on the day it ran.
var fixed = time.Date(2026, 8, 11, 3, 4, 5, 0, time.UTC)

// runCLI executes the command line and returns everything it printed, stdout and
// stderr together, the way an operator sees it.
func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()

	root := NewRoot()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	// A strings.Reader is not a terminal, which is exactly the `dokku run -d`
	// situation: the guided flow must not be entered by accident.
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)

	err := root.Execute()
	return buf.String(), err
}

func tempDB(t *testing.T) string {
	t.Helper()
	// A real file and never :memory: — WAL and the concurrency between the daemon and
	// `dokku run` are the point of this store.
	return filepath.Join(t.TempDir(), "nw.db")
}

// openStore opens the database the way a second, independent process would.
func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Setup(context.Background(), path, fixed)
	if err != nil {
		t.Fatalf("abrindo o banco: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("esperava encontrar %q na saída:\n%s", want, got)
	}
}

func mustNotContain(t *testing.T, got, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Errorf("não esperava encontrar %q na saída:\n%s", unwanted, got)
	}
}

// ------------------------------------------------------------------ fakes

// fakeGitHub answers `GET /repos/{owner}/{repo}` for the repositories it knows and
// 404 for everything else, which is what GitHub does for a repository that does not
// exist and for one the token cannot see.
func fakeGitHub(t *testing.T, defaultBranch map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			writeJSON(w, http.StatusOK, map[string]any{"login": "nightwatcher"})
			return
		}
		spec := strings.TrimPrefix(r.URL.Path, "/repos/")
		branch, ok := defaultBranch[spec]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"full_name":      spec,
			"default_branch": branch,
			"private":        true,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeSentry answers `GET /projects/{org}/{slug}/` for the Fontes it knows.
func fakeSentry(t *testing.T, org string, slugs ...string) *httptest.Server {
	t.Helper()
	known := map[string]bool{}
	for _, s := range slugs {
		known[s] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 1 && parts[0] == "projects" {
			writeJSON(w, http.StatusOK, []map[string]any{{"id": "1", "slug": "qualquer"}})
			return
		}
		if len(parts) != 3 || parts[0] != "projects" || parts[1] != org || !known[parts[2]] {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "The requested resource does not exist"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "42", "slug": parts[2], "name": parts[2]})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeLinearAccessToken is what the fake token endpoint hands out. It is a secret in
// its own right: a `config validate` that printed the token it just obtained would
// leak just as badly as one that printed the client secret.
const fakeLinearAccessToken = "SEGREDO-oauth-access-777"

// fakeLinear answers the two GraphQL operations the command line asks for — the team
// behind `config validate` and the Project listing behind the guided `project add` —
// plus the client_credentials token endpoint of the service account.
func fakeLinear(t *testing.T, teamID string, projects []linearProject) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": fakeLinearAccessToken,
				"token_type":   "Bearer",
				"expires_in":   2592000,
			})
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)

		switch {
		case strings.Contains(req.Query, "projects("):
			nodes := make([]map[string]any, 0, len(projects))
			for _, p := range projects {
				nodes = append(nodes, map[string]any{"id": p.ID, "name": p.Name, "slugId": p.Slug})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{"projects": map[string]any{"nodes": nodes}},
			})
		case strings.Contains(req.Query, "team(id:"):
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{"team": map[string]any{"id": teamID, "key": "ACME", "name": "Engenharia"}},
			})
		case strings.Contains(req.Query, "teams("):
			writeJSON(w, http.StatusOK, map[string]any{
				"data": map[string]any{"teams": map[string]any{
					"nodes": []map[string]any{{"id": teamID, "key": "ACME"}},
				}},
			})
		default:
			writeJSON(w, http.StatusOK, map[string]any{
				"errors": []map[string]any{{"message": "operação inesperada: " + req.Query}},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// endpointFlags points the three live validations at the fakes.
func endpointFlags(github, sentry, linear *httptest.Server) []string {
	var out []string
	if github != nil {
		out = append(out, "--"+flagGitHubAPI, github.URL)
	}
	if sentry != nil {
		out = append(out, "--"+flagSentryAPI, sentry.URL)
	}
	if linear != nil {
		out = append(out, "--"+flagLinearAPI, linear.URL, "--"+flagLinearTokenURL, linear.URL+"/oauth/token")
	}
	return out
}

// ------------------------------------------------------------------ fixtures

// seedProject registers a Projeto directly, for the commands that read one.
func seedProject(t *testing.T, path string, p domain.Project) domain.Project {
	t.Helper()
	st := openStore(t, path)
	saved, err := st.UpsertProject(context.Background(), p, fixed)
	if err != nil {
		t.Fatalf("gravando projeto de fixture: %v", err)
	}
	return saved
}

// seedAnalysis records one Análise as the ingest path would have, with a distinct
// Sentry identity per call so the dedup index never rejects a fixture.
func seedAnalysis(t *testing.T, path string, a domain.Analysis, at time.Time) domain.Analysis {
	t.Helper()
	st := openStore(t, path)

	if a.Occurrence.InstallationUUID == "" {
		a.Occurrence.InstallationUUID = "inst-1"
	}
	if a.Occurrence.SentryProjectID == "" {
		a.Occurrence.SentryProjectID = "4507"
	}
	if a.Occurrence.IssueID == "" {
		a.Occurrence.IssueID = fmt.Sprintf("issue-%d", at.UnixNano())
	}
	if a.Occurrence.Substatus == "" {
		a.Occurrence.Substatus = domain.SubstatusNew
	}

	saved, err := st.CreateAnalysis(context.Background(), a, at)
	if err != nil {
		t.Fatalf("gravando análise de fixture: %v", err)
	}
	return saved
}

func seedSession(t *testing.T, path, analysisID string, sess domain.Session) {
	t.Helper()
	st := openStore(t, path)
	if _, err := st.AppendSession(context.Background(), analysisID, sess); err != nil {
		t.Fatalf("gravando sessão de fixture: %v", err)
	}
}

// projectFixture is a Projeto with one Fonte, which is the shape every command that
// reads configuration needs.
func projectFixture(name, repo, slug, label string) domain.Project {
	return domain.Project{
		Name:            name,
		Repo:            repo,
		Branch:          "main",
		LinearProjectID: "11112222-3333-4444-5555-666677778888",
		Sources:         []domain.Source{{SentrySlug: slug, Label: label}},
	}
}

// lineWith returns the printed line carrying needle, so an assertion can be about one
// row of a table instead of about the whole screen.
func lineWith(t *testing.T, out, needle string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("nenhuma linha da saída contém %q:\n%s", needle, out)
	return ""
}
