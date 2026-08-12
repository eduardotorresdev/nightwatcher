// Package cli is the command surface: noun-verb, in English, the
// same shape as the `dokku` you type before it. Everything lives in the one binary
// that also serves the daemon, so operating the nightwatcher is `dokku run` against
// the image that is already in the air.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/sentryapi"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
)

// Version is stamped at build time.
var Version = "dev"

// NewRoot builds the whole command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "nightwatcher",
		Short: "Suporte autônomo: escuta o Sentry, triga, diagnostica lendo o código e registra o trabalho",
		Long: "O nightwatcher escuta o que os usuários relatam e o que quebra em produção,\n" +
			"decide o que é, investiga a causa lendo o código, e registra o resultado como\n" +
			"trabalho rastreável — sem intervenção humana no caminho.",
		Version:      Version,
		SilenceUsage: true,
		// Cobra must not print the failure, because cmd/nightwatcher already does and
		// two printers means the operator reads every failure twice — once as
		// "Error: ..." and once as "nightwatcher: ...". The one that survives is the
		// one carrying the program's name: `dokku logs` interleaves this process with
		// everything else on the host, and a bare "Error:" line belongs to nobody.
		SilenceErrors: true,
	}

	// Every subcommand opens the same SQLite file the daemon is using. The flag is
	// persistent because the choice belongs to the invocation, not to the verb, and it
	// wins over NIGHTWATCHER_DB: a `dokku run` container inherits the app's whole
	// environment, so pointing one command at a copy has to be possible without
	// rewriting that environment. Unset, config.LoadPaths decides — the env var first,
	// then /data/nightwatcher.db on the persistent volume.
	root.PersistentFlags().String(flagDB, "",
		"caminho do SQLite (padrão: $"+config.EnvDBPath+", senão "+config.LoadPaths().DB+")")

	// The endpoints the live validations reach. Hidden because production never sets
	// them; they exist so `config validate` and the guided `project add` can be aimed
	// at an httptest server, and a test that actually calls github.com is not a test.
	root.PersistentFlags().String(flagGitHubAPI, DefaultGitHubAPI, "raiz da API do GitHub")
	root.PersistentFlags().String(flagSentryAPI, sentryapi.DefaultBaseURL, "raiz da API do Sentry")
	root.PersistentFlags().String(flagLinearAPI, tracker.DefaultEndpoint, "endpoint GraphQL do Linear")
	root.PersistentFlags().String(flagLinearTokenURL, tracker.DefaultTokenEndpoint, "endpoint de token do Linear")
	for _, hidden := range []string{flagGitHubAPI, flagSentryAPI, flagLinearAPI, flagLinearTokenURL} {
		_ = root.PersistentFlags().MarkHidden(hidden)
	}

	root.AddCommand(
		newServeCmd(),
		newConfigCmd(),
		newProjectCmd(),
		newMCPCmd(),
		newAnalysisCmd(),
		newOccurrenceCmd(),
	)
	return root
}
