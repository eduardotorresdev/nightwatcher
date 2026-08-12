package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
)

func TestConfigSetPersistsTheNumberForAnotherProcessOpeningTheSameFile(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "", "--db", db, "config", "set", "concurrency", "1")
	if err != nil {
		t.Fatalf("config set devia ter funcionado: %v\n%s", err, out)
	}
	mustContain(t, out, "concurrency = 1")

	// A second, independently opened store: this is `dokku run` writing the file the
	// daemon is reading. The number has to be there without anyone restarting.
	st := openStore(t, db)
	tun, err := st.Tunables(context.Background())
	if err != nil {
		t.Fatalf("lendo os números da operação: %v", err)
	}
	if tun.Concurrency != 1 {
		t.Errorf("concurrency = %d, esperava 1", tun.Concurrency)
	}
	// Everything else stays on the default profile: setting one key must not reset
	// the others.
	if tun.DeepTimeout != config.Defaults().DeepTimeout {
		t.Errorf("deep_timeout = %s, esperava o padrão %s", tun.DeepTimeout, config.Defaults().DeepTimeout)
	}
}

func TestConfigSetRefusesAnUnknownKeyAndNamesTheKnownOnes(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "", "--db", db, "config", "set", "concurrencia", "3")
	if err == nil {
		t.Fatalf("uma chave desconhecida tem que falhar; saída:\n%s", out)
	}
	message := err.Error() + out
	mustContain(t, message, `"concurrencia"`)
	for _, key := range knownKeys() {
		mustContain(t, message, key)
	}

	st := openStore(t, db)
	stored, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("lendo as configurações: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("uma chave recusada não pode ter gravado nada, mas o banco tem %v", stored)
	}
}

func TestConfigSetRefusesAnythingThatLooksLikeACredentialAndSaysWhereSecretsLive(t *testing.T) {
	cases := []struct {
		key string
		env string
	}{
		{"sentry_token", config.EnvSentryToken},
		{"linear_api_key", config.EnvLinearAPIKey},
		{"github_token", config.EnvGitHubToken},
		{"claude_code_oauth_token", config.EnvClaudeOAuthToken},
		{"sentry_client_secret", config.EnvSentryClientSecret},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			db := tempDB(t)
			const value = "sk-nao-devia-existir-aqui"

			out, err := runCLI(t, "", "--db", db, "config", "set", tc.key, value)
			if err == nil {
				t.Fatalf("%s tinha que ser recusada; saída:\n%s", tc.key, out)
			}

			message := err.Error() + out
			mustContain(t, message, "variável de ambiente")
			mustContain(t, message, tc.env)
			// The refused value must not be echoed back either — a message that
			// repeats the token puts it in the terminal scrollback and in the logs.
			mustNotContain(t, message, value)

			st := openStore(t, db)
			stored, err := st.Settings(context.Background())
			if err != nil {
				t.Fatalf("lendo as configurações: %v", err)
			}
			if _, ok := stored[tc.key]; ok {
				t.Errorf("a credencial %s foi parar no banco", tc.key)
			}
		})
	}
}

func TestConfigSetRefusesAValueTheOperatingNumberCannotTake(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "", "--db", db, "config", "set", "deep_timeout", "20 minutos")
	if err == nil {
		t.Fatalf("uma duração ilegível tem que falhar; saída:\n%s", out)
	}
	mustContain(t, err.Error(), "deep_timeout")

	st := openStore(t, db)
	tun, err := st.Tunables(context.Background())
	if err != nil {
		t.Fatalf("lendo os números da operação: %v", err)
	}
	if tun.DeepTimeout != config.Defaults().DeepTimeout {
		t.Errorf("deep_timeout mudou para %s depois de um valor recusado", tun.DeepTimeout)
	}
}

func TestConfigValidateReportsEachServiceItReached(t *testing.T) {
	db := tempDB(t)
	const teamID = "0f1e2d3c-4b5a-4968-8776-655443332211"

	github := fakeGitHub(t, map[string]string{"acme/acme": "main"})
	sentry := fakeSentry(t, "acme", "acme-web")
	linear := fakeLinear(t, teamID, nil)

	t.Setenv(config.EnvSentryToken, "sentry-token")
	t.Setenv(config.EnvSentryClientSecret, "sentry-client-secret")
	t.Setenv(config.EnvLinearAPIKey, "lin_api_chave")
	t.Setenv(config.EnvGitHubToken, "ghp_token")
	t.Setenv(config.EnvClaudeOAuthToken, "claude-token")

	if _, err := runCLI(t, "", "--db", db, "config", "set", "sentry_org", "acme"); err != nil {
		t.Fatalf("config set sentry_org: %v", err)
	}
	if _, err := runCLI(t, "", "--db", db, "config", "set", "linear_team", teamID); err != nil {
		t.Fatalf("config set linear_team: %v", err)
	}

	args := append([]string{"--db", db}, endpointFlags(github, sentry, linear)...)
	args = append(args, "config", "validate")

	out, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatalf("a validação devia ter passado: %v\n%s", err, out)
	}

	mustContain(t, out, "Sentry")
	mustContain(t, out, "Linear")
	mustContain(t, out, "GitHub")
	if strings.Contains(out, "falhou") {
		t.Errorf("nenhum serviço devia ter falhado:\n%s", out)
	}
	// Presence only, never the value.
	mustContain(t, lineWith(t, out, config.EnvGitHubToken), "presente")
	mustNotContain(t, out, "ghp_token")
}

func TestConfigValidateFailsNonZeroWhenACredentialIsRefused(t *testing.T) {
	db := tempDB(t)
	const teamID = "0f1e2d3c-4b5a-4968-8776-655443332211"

	// The Fonte registered here does not exist in the fake Sentry: exactly the typo
	// this command exists to catch before the first webhook does.
	seedProject(t, db, projectFixture("acme", "acme/acme", "acme-web", "web"))

	github := fakeGitHub(t, map[string]string{"acme/acme": "main"})
	sentry := fakeSentry(t, "acme", "outra-fonte")
	linear := fakeLinear(t, teamID, nil)

	t.Setenv(config.EnvSentryToken, "sentry-token")
	t.Setenv(config.EnvLinearAPIKey, "lin_api_chave")
	t.Setenv(config.EnvGitHubToken, "ghp_token")

	if _, err := runCLI(t, "", "--db", db, "config", "set", "sentry_org", "acme"); err != nil {
		t.Fatalf("config set sentry_org: %v", err)
	}
	if _, err := runCLI(t, "", "--db", db, "config", "set", "linear_team", teamID); err != nil {
		t.Fatalf("config set linear_team: %v", err)
	}

	args := append([]string{"--db", db}, endpointFlags(github, sentry, linear)...)
	args = append(args, "config", "validate")

	out, err := runCLI(t, "", args...)
	if err == nil {
		t.Fatalf("a validação tinha que falhar; saída:\n%s", out)
	}
	mustContain(t, err.Error(), "Sentry")
	mustContain(t, out, "acme-web")
}

// TestAFailedCommandPrintsItsFailureOnce is about the operator's screen and not about
// cobra. cmd/nightwatcher prints what Execute returns, prefixed with the program's
// name, because `dokku logs` interleaves this process with everything else on the host
// and a bare "Error:" line belongs to nobody. If the tree printed it too, every failure
// would appear twice in that log — and a log that repeats itself is a log somebody
// stops reading.
func TestAFailedCommandPrintsItsFailureOnce(t *testing.T) {
	db := tempDB(t)

	out, err := runCLI(t, "", "--db", db, "config", "set", "chave-inventada", "1")
	if err == nil {
		t.Fatalf("uma chave desconhecida tem que falhar; saída:\n%s", out)
	}
	// The failure comes back to the caller, and only to the caller.
	mustContain(t, err.Error(), "chave-inventada")
	mustNotContain(t, out, err.Error())
	mustNotContain(t, out, "Error:")
}
