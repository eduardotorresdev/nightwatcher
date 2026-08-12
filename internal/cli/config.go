package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Credenciais globais e números da operação"}
	c.AddCommand(
		&cobra.Command{
			Use:   "set <chave> <valor>",
			Short: "Ajusta um número da operação sem redeploy",
			Long: "Ajusta um número da operação sem redeploy.\n\n" +
				"Chaves aceitas: " + strings.Join(knownKeys(), ", ") + ".\n\n" +
				"Credencial NÃO entra aqui: segredo vive só em variável de ambiente, para que\n" +
				"o banco possa ser lido, copiado e versionado sem vazar nenhum.",
			Args: cobra.ExactArgs(2),
			RunE: runConfigSet,
		},
		&cobra.Command{
			Use:   "validate",
			Short: "Prova que cada credencial funciona alcançando Sentry, Linear e GitHub",
			Long: "Alcança de verdade o Sentry, o Linear e o GitHub com as credenciais do ambiente.\n\n" +
				"Validar o formato de um token não protege contra nada: um token revogado tem o\n" +
				"mesmo formato de um válido. Nenhum valor de credencial é impresso — só se está\n" +
				"presente e se o serviço do outro lado a aceitou.",
			RunE: runConfigValidate,
		},
	)
	return c
}

// knownKeys is everything `config set` accepts: the operating numbers plus
// the two identity settings. Named in the error of an unknown key, because a rejected
// key with no list of the accepted ones just moves the guessing one step later.
func knownKeys() []string {
	out := slices.Clone(config.Settable)
	return append(out, settingSentryOrg, settingLinearTeam)
}

// secretHints are the substrings that mark a key as an attempt to store a credential.
// None of the accepted keys contains any of them, and that is not an accident: the
// operating numbers are counts and durations, never a value with a secret in it.
var secretHints = []string{"token", "secret", "key", "password", "senha", "credential", "credencial", "auth"}

// credentialEnv names the environment variable a would-be credential key belongs in,
// so the refusal ends with somewhere to actually put it.
var credentialEnv = map[string]string{
	"sentry_client_secret":    config.EnvSentryClientSecret,
	"sentry_token":            config.EnvSentryToken,
	"linear_client_id":        config.EnvLinearClientID,
	"linear_client_secret":    config.EnvLinearClientSecret,
	"linear_api_key":          config.EnvLinearAPIKey,
	"github_token":            config.EnvGitHubToken,
	"claude_code_oauth_token": config.EnvClaudeOAuthToken,
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	key := strings.ToLower(strings.TrimSpace(args[0]))
	value := strings.TrimSpace(args[1])

	// The refusal comes before the database is even opened: a credential must not
	// reach the file at all, not even to be rejected by a constraint afterwards.
	if looksLikeSecret(key) {
		env, ok := credentialEnv[key]
		where := "a variável de ambiente correspondente"
		if ok {
			where = "a variável de ambiente " + env
		}
		return fmt.Errorf(
			"a chave %q parece uma credencial, e credencial nunca entra no banco — o banco precisa poder ser lido e copiado sem vazar nenhuma.\n"+
				"Segredos vêm só de variável de ambiente: defina %s no ambiente do app (dokku config:set).\n"+
				"Aqui só entram: %s",
			key, where, strings.Join(knownKeys(), ", "))
	}

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		switch {
		case slices.Contains(config.Settable, key):
			// SetTunable runs config.Tunables.Apply before storing, so the value is
			// validated where the operator is looking instead of at the next daemon
			// start, where nobody is.
			if err := st.SetTunable(ctx, key, value); err != nil {
				return err
			}
		case key == settingSentryOrg || key == settingLinearTeam:
			if value == "" {
				return fmt.Errorf("%s não aceita valor vazio", key)
			}
			if err := st.SetSetting(ctx, key, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("chave desconhecida %q; conhecidas: %s", key, strings.Join(knownKeys(), ", "))
		}

		// Echoing the value back is safe precisely because credentials never get this
		// far, and it is what makes a `dokku run` invocation self-evidencing.
		fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n", key, value)
		return nil
	})
}

func looksLikeSecret(key string) bool {
	for _, hint := range secretHints {
		if strings.Contains(key, hint) {
			return true
		}
	}
	return false
}

// check is one line of the validation report: a service reached, or not.
type check struct {
	name   string
	detail string
	err    error
}

func runConfigValidate(cmd *cobra.Command, args []string) error {
	r := newRemote(cmd)

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		out := cmd.OutOrStdout()

		reportSecrets(out, r.secrets)

		org, err := identity(ctx, st, settingSentryOrg, envSentryOrg)
		if err != nil {
			return err
		}
		team, err := identity(ctx, st, settingLinearTeam, envLinearTeam)
		if err != nil {
			return err
		}
		projects, err := st.ListProjects(ctx)
		if err != nil {
			return err
		}

		globalMCP, err := st.GlobalMCPServers(ctx)
		if err != nil {
			return err
		}

		checks := []check{
			checkSentry(ctx, r, org, projects),
			checkLinear(ctx, r, team),
			checkGitHub(ctx, r, projects),
		}
		// One check per server rather than one for all of them: they are independent
		// services, and "os MCPs falharam" would send whoever reads this looking at
		// four servers when one is down.
		checks = append(checks, checkMCPServers(ctx, globalMCP, projects)...)

		fmt.Fprintln(out, "\nalcance")
		rows := make([][]string, 0, len(checks))
		var failed []string
		for _, c := range checks {
			if c.err != nil {
				failed = append(failed, c.name)
				rows = append(rows, []string{"  " + c.name, "falhou", c.err.Error()})
				continue
			}
			rows = append(rows, []string{"  " + c.name, "ok", c.detail})
		}
		table(out, []string{"  SERVIÇO", "ESTADO", "DETALHE"}, rows)

		if tun, err := st.Tunables(ctx); err == nil {
			fmt.Fprintln(out, "\nnúmeros da operação")
			table(out, []string{"  CHAVE", "VALOR"}, [][]string{
				{"  concurrency", fmt.Sprint(tun.Concurrency)},
				{"  triage_timeout", tun.TriageTimeout.String()},
				{"  deep_timeout", tun.DeepTimeout.String()},
				{"  max_attempts", fmt.Sprint(tun.MaxAttempts)},
				{"  per_project_hourly", fmt.Sprint(tun.PerProjectHourly)},
				{"  max_queue", fmt.Sprint(tun.MaxQueue)},
				{"  lease_ttl", tun.LeaseTTL.String()},
			})
		} else {
			failed = append(failed, "configuração armazenada")
			fmt.Fprintf(out, "\nnúmeros da operação: %v\n", err)
		}

		if len(failed) > 0 {
			// A failed validation has to exit non-zero: `config validate` in a deploy
			// script that always succeeds validates nothing.
			return fmt.Errorf("a validação falhou em: %s", strings.Join(failed, ", "))
		}
		return nil
	})
}

// reportSecrets says which credentials the environment carries. Presence only — the
// value is never printed, not even redacted: a prefix of a token is still a piece of
// a token, and there is no question a redacted value answers that "presente" does not.
func reportSecrets(out io.Writer, s config.Secrets) {
	present := func(v string) string {
		if strings.TrimSpace(v) == "" {
			return "ausente"
		}
		return "presente"
	}
	fmt.Fprintln(out, "segredos (só do ambiente, nunca do banco)")
	table(out, []string{"  VARIÁVEL", "ESTADO"}, [][]string{
		{"  " + config.EnvSentryClientSecret, present(s.SentryClientSecret)},
		{"  " + config.EnvSentryToken, present(s.SentryToken)},
		{"  " + config.EnvLinearClientID, present(s.LinearClientID)},
		{"  " + config.EnvLinearClientSecret, present(s.LinearClientSecret)},
		{"  " + config.EnvLinearAPIKey, present(s.LinearAPIKey)},
		{"  " + config.EnvGitHubToken, present(s.GitHubToken)},
		{"  " + config.EnvClaudeOAuthToken, present(s.ClaudeOAuthToken)},
	})
}

// checkSentry confirms every registered Fonte, which is a stronger statement than
// "the token works": a Fonte whose slug is wrong never resolves at ingest, and the
// Ocorrência is refused as unknown. With no Fonte yet — or no organisation named —
// it falls back to proving the credential alone.
func checkSentry(ctx context.Context, r *remote, org string, projects []domain.Project) check {
	c := check{name: "Sentry"}
	if strings.TrimSpace(r.secrets.SentryToken) == "" {
		c.err = fmt.Errorf("%s não está definida no ambiente", config.EnvSentryToken)
		return c
	}

	var fontes []string
	for _, p := range projects {
		for _, src := range p.Sources {
			fontes = append(fontes, src.SentrySlug)
		}
	}

	if org == "" || len(fontes) == 0 {
		if err := r.sentryReach(ctx); err != nil {
			c.err = err
			return c
		}
		switch {
		case org == "":
			c.detail = fmt.Sprintf("credencial aceita; organização não configurada (`config set %s <org>`), Fontes não conferidas", settingSentryOrg)
		default:
			c.detail = "credencial aceita; nenhuma Fonte registrada ainda"
		}
		return c
	}

	for _, slug := range fontes {
		if _, err := r.fonte(ctx, org, slug); err != nil {
			c.err = err
			return c
		}
	}
	c.detail = fmt.Sprintf("%d Fonte(s) confirmada(s) em %s", len(fontes), org)
	return c
}

func checkLinear(ctx context.Context, r *remote, team string) check {
	c := check{name: "Linear"}
	if strings.TrimSpace(team) == "" {
		c.err = fmt.Errorf("o time do Destino não está configurado: `nightwatcher config set %s <uuid|chave>`", settingLinearTeam)
		return c
	}

	client, err := r.linearClient(team)
	if err != nil {
		c.err = err
		return c
	}
	// Validate reads the team even when the UUID is already configured, on purpose:
	// stopping at the configured value would approve a revoked key in silence.
	if err := client.Validate(ctx); err != nil {
		c.err = err
		return c
	}
	c.detail = fmt.Sprintf("credencial aceita e time %s alcançado", team)
	return c
}

// checkGitHub reaches every registered repository read-only. Read-only is the whole
// contract: the nightwatcher never creates a branch, a commit or a pull request.
func checkGitHub(ctx context.Context, r *remote, projects []domain.Project) check {
	c := check{name: "GitHub"}
	if strings.TrimSpace(r.secrets.GitHubToken) == "" {
		c.err = fmt.Errorf("%s não está definida no ambiente", config.EnvGitHubToken)
		return c
	}
	if len(projects) == 0 {
		if err := r.githubReach(ctx); err != nil {
			c.err = err
			return c
		}
		c.detail = "credencial aceita; nenhum Projeto registrado ainda"
		return c
	}

	for _, p := range projects {
		repo, err := r.repo(ctx, p.Repo)
		if err != nil {
			c.err = fmt.Errorf("projeto %s: %w", p.Name, err)
			return c
		}
		if repo.Archived {
			// An archived repository still clones, so this is a warning shaped like a
			// success: the analysis would work and point at code nobody maintains.
			c.detail += fmt.Sprintf("%s está arquivado no GitHub; ", p.Repo)
		}
	}
	c.detail += fmt.Sprintf("%d repositório(s) alcançado(s)", len(projects))
	return c
}

// checkMCPServers proves every registered MCP server, global and per Projeto, the same
// way checkSentry proves every registered Fonte: what is validated is the whole
// configuration, not a sample of it.
//
// It runs the direct probe only. The container probe belongs to `mcp add`, where a
// human is waiting and can act on it; here it would raise one container per server on
// every deploy, and `config validate` runs in a deploy script.
func checkMCPServers(ctx context.Context, global []domain.MCPServer, projects []domain.Project) []check {
	type scoped struct {
		server domain.MCPServer
		escopo string
	}
	var all []scoped
	for _, m := range global {
		all = append(all, scoped{server: m, escopo: "global"})
	}
	for _, p := range projects {
		for _, m := range p.MCPServers {
			all = append(all, scoped{server: m, escopo: p.Name})
		}
	}
	if len(all) == 0 {
		return nil
	}

	client := &http.Client{Timeout: mcpProbeTimeout}
	out := make([]check, 0, len(all))
	for _, s := range all {
		c := check{name: "MCP " + s.server.Name + " (" + s.escopo + ")"}
		res, err := probeMCP(ctx, client, s.server, os.Getenv)
		switch {
		case err != nil:
			// Not explainProbe: that one prints the multi-line instructions `mcp add`
			// gives a human at a terminal, and this lands in a one-line table cell.
			c.err = err
		case res.ServerName != "":
			c.detail = res.ServerName + " " + res.ServerVersion
		default:
			c.detail = s.server.URL
		}
		out = append(out, c)
	}
	return out
}
