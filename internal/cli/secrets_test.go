package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// TestNoCommandEverPrintsACredential is a security property, not a nicety: these
// commands run under `dokku run`, their output lands in terminal scrollback, in CI
// logs and in pasted bug reports. A token that shows up in any of them is leaked.
func TestNoCommandEverPrintsACredential(t *testing.T) {
	const (
		sentrySecret = "SEGREDO-sentry-client-000"
		sentryToken  = "SEGREDO-sentry-token-111"
		linearID     = "SEGREDO-linear-id-222"
		linearSecret = "SEGREDO-linear-secret-333"
		linearKey    = "SEGREDO-linear-key-444"
		githubToken  = "SEGREDO-github-token-555"
		claudeToken  = "SEGREDO-claude-token-666"
		teamID       = "0f1e2d3c-4b5a-4968-8776-655443332211"
	)

	t.Setenv(config.EnvSentryClientSecret, sentrySecret)
	t.Setenv(config.EnvSentryToken, sentryToken)
	t.Setenv(config.EnvLinearAPIKey, linearKey)
	t.Setenv(config.EnvGitHubToken, githubToken)
	t.Setenv(config.EnvClaudeOAuthToken, claudeToken)
	// The OAuth pair is added further down, so both Linear credential paths — the
	// personal key here, the service account there — get the same scrutiny.

	secrets := []string{
		sentrySecret, sentryToken, linearID, linearSecret, linearKey, githubToken, claudeToken,
		fakeLinearAccessToken,
	}

	db := tempDB(t)
	github := fakeGitHub(t, map[string]string{"acme/acme": "main"})
	sentry := fakeSentry(t, "acme", "acme-web", "acme-api")
	linear := fakeLinear(t, teamID, []linearProject{{ID: "aaaa1111-2222-3333-4444-555566667777", Name: "ACME"}})
	endpoints := endpointFlags(github, sentry, linear)

	seeded := seedHistory(t, db)
	published := seeded["publicada"].ID
	seedSession(t, db, published, domain.Session{
		Kind:      domain.SessionDiagnosis,
		Prompt:    "leia o repositório e diga onde quebrou",
		Response:  "src/lib/pedidos.ts:42 não trata undefined",
		StartedAt: fixed,
		EndedAt:   fixed.Add(time.Minute),
	})

	runs := [][]string{
		{"--db", db, "config", "set", "concurrency", "3"},
		{"--db", db, "config", "set", "sentry_org", "acme"},
		{"--db", db, "config", "set", "linear_team", teamID},
		{"--db", db, "config", "set", "github_token", githubToken},
		{"--db", db, "config", "set", "chave-inventada", "valor"},
		append(append([]string{"--db", db}, endpoints...), "config", "validate"),
		{"--db", db, "project", "add", "--nome", "outro", "--repo", "acme/acme",
			"--branch", "main", "--linear-project", "aaaa1111", "--fonte", "acme-api:api"},
		{"--db", db, "project", "list"},
		{"--db", db, "project", "list", "--json"},
		{"--db", db, "analysis", "list"},
		{"--db", db, "analysis", "list", "--json"},
		{"--db", db, "analysis", "list", "--outcome", "recusada"},
		// `analysis show` prints the most text of any command: everything handed to the
		// agent and everything it answered.
		{"--db", db, "analysis", "show", published},
		{"--db", db, "analysis", "show", seeded["recusada"].ID},
		{"--db", db, "project", "add", "--help"},
		{"--db", db, "config", "validate", "--help"},
		// The daemon and the two dry runs read every credential there is, so they get the
		// same scrutiny. `serve` itself is only asked for its help: raising it would bind
		// a port and never come back.
		{"--db", db, "serve", "--help"},
		{"--db", db, "occurrence", "replay", "--help"},
		{"--db", db, "occurrence", "simulate", "--help"},
		{"--db", db, "occurrence", "replay", "análise-que-não-existe"},
		{"--db", db, "occurrence", "simulate", "/caminho/que/não/existe.json"},
		{"--help"},
	}

	for _, args := range runs {
		// The error is not asserted on: some of these runs are supposed to fail, and
		// what is under test is what they printed, not whether they succeeded.
		out, err := runCLI(t, "", args...)
		printed := out
		if err != nil {
			printed += "\n" + err.Error()
		}
		for _, secret := range secrets {
			if strings.Contains(printed, secret) {
				t.Errorf("`nightwatcher %s` imprimiu uma credencial:\n%s", strings.Join(args, " "), printed)
			}
		}
	}

	// The guided flow prints the most, and it is the one that asks questions about
	// services it authenticated against.
	answers := strings.Join([]string{"guiado", "acme/acme", "", "1", "acme-web:web", "", ""}, "\n") + "\n"
	guided := append(append([]string{"--db", db}, endpoints...), "project", "add", "--"+flagInteractive)
	out, err := runCLI(t, answers, guided...)
	if err != nil {
		t.Fatalf("o modo guiado devia ter terminado: %v\n%s", err, out)
	}
	for _, secret := range secrets {
		mustNotContain(t, out, secret)
	}

	// The service account is the other Linear credential path: it trades the pair for
	// an access token, and that token must not surface either.
	t.Setenv(config.EnvLinearClientID, linearID)
	t.Setenv(config.EnvLinearClientSecret, linearSecret)
	out, err = runCLI(t, "", append(append([]string{"--db", db}, endpoints...), "config", "validate")...)
	if err != nil {
		t.Fatalf("a validação com a conta de serviço devia ter passado: %v\n%s", err, out)
	}
	for _, secret := range secrets {
		mustNotContain(t, out, secret)
	}

	// The dry runs are the commands that build the whole outside world out of every
	// credential there is — the session, the clone, the Destino — and they print the
	// most: the prompt, the reply and the issue that would have been born.
	freezeClock(t)
	prevWorld := newWorld
	newWorld = func(worldSpec) (outsideWorld, error) {
		return outsideWorld{agent: stubAgentAnswering(domain.VerdictDefect), workspace: fixtureWorkspace{}}, nil
	}
	t.Cleanup(func() { newWorld = prevWorld })

	for _, args := range [][]string{
		{"--db", db, "occurrence", "simulate", fixturePath("defeito-error-new.json"),
			"--projeto", "acme", "--sessions"},
		{"--db", db, "occurrence", "replay", published, "--sessions"},
	} {
		out, err := runCLI(t, "", args...)
		printed := out
		if err != nil {
			printed += "\n" + err.Error()
		}
		for _, secret := range secrets {
			if strings.Contains(printed, secret) {
				t.Errorf("`nightwatcher %s` imprimiu uma credencial:\n%s", strings.Join(args, " "), printed)
			}
		}
	}

	// productionWorld is the one place the wiring hands every credential to somebody
	// else — the session, the clone, the Destino — and therefore the one place a wrapped
	// error from one of them could carry a token back out to the terminal. The success
	// path cannot be raised in a test (it builds a Docker runner and clones over the
	// network), but the failure path is the path that prints, and it is reachable.
	_, err = productionWorld(worldSpec{
		secrets: config.LoadSecrets(),
		// A file, so the clone root underneath it cannot be created.
		paths: config.Paths{Clones: filepath.Join(db, "raiz-impossível")},
		team:  teamID,
	})
	if err == nil {
		t.Fatal("uma raiz de clones impossível tinha que falhar")
	}
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a falha ao montar o mundo externo imprimiu uma credencial: %v", err)
		}
	}

	// And nothing landed in the database either. The whole file is scanned and not only
	// the settings table: the point is that the volume can be copied off the VPS, opened
	// with `sqlite3` and read by anyone, so a credential anywhere in it — a settings row,
	// a recorded prompt, a raw Sentry body — is the same leak.
	st := openStore(t, db)
	if _, err := st.DB().ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("consolidando o WAL antes de ler o arquivo: %v", err)
	}
	raw, err := os.ReadFile(db)
	if err != nil {
		t.Fatalf("lendo o arquivo do banco: %v", err)
	}
	for _, secret := range secrets {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("o arquivo do banco guarda uma credencial (%d bytes lidos)", len(raw))
		}
	}
}
