package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// Flag names the whole tree shares. The four endpoint flags are hidden: they exist so
// the live validations can be aimed at an httptest server, and a test that actually
// calls github.com is not a test. Nothing in production sets them.
const (
	flagDB             = "db"
	flagGitHubAPI      = "github-api"
	flagSentryAPI      = "sentry-api"
	flagLinearAPI      = "linear-api"
	flagLinearTokenURL = "linear-token-url"
)

// clock is what the commands date their writes with. It is a package variable rather
// than a parameter because no command *reads* time to decide anything — only
// store.Setup and UpsertProject need an instant — and a test that wanted a fixed one
// writes its own rows through the store.
var clock domain.Clock = domain.SystemClock{}

// dbPath resolves which SQLite file the command opens.
//
// Precedence is --db, then NIGHTWATCHER_DB, then /data/nightwatcher.db — the last two
// being exactly what config.LoadPaths already decides. The flag wins because it is
// the only per-invocation override: a `dokku run` container inherits the app's whole
// environment, so pointing one command at another file (a copy, a local run, a test)
// has to be possible without rewriting that environment.
func dbPath(cmd *cobra.Command) string {
	if v, err := cmd.Flags().GetString(flagDB); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return config.LoadPaths().DB
}

// withStore opens the same database the daemon uses — same file, same WAL, same
// migrations — hands it to fn and closes it afterwards. Every subcommand goes through
// here so none of them can forget the migration and meet it as a missing table.
func withStore(cmd *cobra.Command, fn func(ctx context.Context, st *store.Store) error) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	path := dbPath(cmd)
	st, err := store.Setup(ctx, path, clock.Now())
	if err != nil {
		return fmt.Errorf("abrindo o banco %s: %w", path, err)
	}
	defer st.Close()

	return fn(ctx, st)
}

// Identity settings: which Sentry organisation and which Linear team the credentials
// point at. Neither is a credential — an org slug and a team UUID leak nothing — and
// neither is an operating number, so they sit in the settings table beside them and
// are the only keys `config set` accepts outside config.Settable.
//
// They have to live somewhere: internal/config has no field for either — the same
// is true of the team — and both `config validate` and the guided `project add`
// need them to reach anything at all.
const (
	settingSentryOrg  = "sentry_org"
	settingLinearTeam = "linear_team"

	envSentryOrg  = "SENTRY_ORG"
	envLinearTeam = "LINEAR_TEAM"
)

// identity reads one of those two, with the environment overriding the stored value.
// The environment wins so a one-off `dokku run -e` can point at another org or team
// without writing to the file the daemon is reading from.
func identity(ctx context.Context, st *store.Store, setting, env string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	v, _, err := st.Setting(ctx, setting)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

// interactive reports whether there is a human on the other end to answer questions.
// It is a variable for the same reason newWorld is: a test cannot attach a terminal to
// its own process, and which branch this selects is the difference between a scripted
// `dokku run` and one that stops on a question nobody is there to answer.
//
// `dokku run -d` has no terminal, so a command that would ask has to fail saying what
// was missing instead of hanging on input that will never come. Both ends are checked:
// a terminal to print the question on and a terminal to read the answer from. The
// check is by hand — os.File.Stat and ModeCharDevice — because go-isatty is only an
// indirect dependency of this module and importing it would make it a direct one.
var interactive = func(cmd *cobra.Command) bool {
	return isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout())
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		// Anything that is not a file — a bytes.Buffer, a pipe wrapper, a test's
		// strings.Reader — is by definition not somebody typing.
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// prompter asks the guided questions of `project add` and reads the answers.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(cmd *cobra.Command) *prompter {
	return &prompter{in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout()}
}

func (p *prompter) printf(format string, args ...any) {
	fmt.Fprintf(p.out, format, args...)
}

// ask prints the question and returns the trimmed answer. An empty answer returns
// def, so `[main]` in the question means what it looks like.
//
// End of input is an error and never an empty answer: a script whose here-document ran
// out must not be read as the operator pressing Enter on every remaining question.
func (p *prompter) ask(question, def string) (string, error) {
	if def != "" {
		p.printf("%s [%s]: ", question, def)
	} else {
		p.printf("%s: ", question)
	}

	line, err := p.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("a entrada acabou antes da resposta de %q: %w", question, err)
	}

	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// maxRetries bounds how many times a question is repeated after a rejected answer, so
// a wrong repository typed against an unreachable GitHub ends the command instead of
// looping forever.
const maxRetries = 5

// askValid repeats the question until validate accepts the answer or the patience
// runs out. The rejection is printed where the operator is looking — which is the
// whole point of validating live: a typo shows up at the next question, not at the
// first webhook.
func (p *prompter) askValid(question, def string, validate func(string) error) (string, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		answer, err := p.ask(question, def)
		if err != nil {
			return "", err
		}
		if err := validate(answer); err != nil {
			lastErr = err
			p.printf("  ✗ %v\n", err)
			continue
		}
		return answer, nil
	}
	return "", fmt.Errorf("%s: %d respostas recusadas, a última por: %w", question, maxRetries, lastErr)
}

// table is the readable default output. Columns are named in the header so a line can
// be read without counting fields.
//
// Every cell goes through cell() first, and that is not cosmetics. Half of what lands
// in these tables — the título of an Ocorrência, its culprit, the título of the issue a
// dry run would create — is text an end user wrote, and a newline or a tab inside it is
// a row separator and a column separator to a tabwriter. A feedback titled
// "inofensivo\n  desfecho\tpublicada (defeito)" prints an extra line that reads exactly
// like the command's own verdict. These listings are the evidence the Triagem is
// audited from (User Stories 4, 30 and 31), so whoever writes the feedback must not be
// able to write a line of them.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(cells(header), "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(cells(row), "\t"))
	}
	tw.Flush()
}

func cells(row []string) []string {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = cell(v)
	}
	return out
}

// cell makes one value safe to be a table cell: every control character becomes a
// space. That covers the newline and the tab that would forge a row, and it also
// defangs an ESC — an Ocorrência carrying "\x1b[2J" would otherwise clear the terminal
// of the operator reading it.
func cell(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
}

// block is cell for text that is printed as text rather than as a table cell: the
// prompt handed to the agent, the reply it gave, the body of the issue a dry run would
// create. The exception list is the whole difference — a newline and a tab are
// structure in a block and only forge a row inside a tabwriter — and everything else
// still goes, ESC above all.
//
// Sanitising here and not only in cell() is not belt and braces. `analysis show` is the
// command that prints the most, it prints the prompt whole, and the prompt carries the
// Ocorrência verbatim — which is text an end user wrote. Flattening the título in the
// table above and then echoing the same título raw two screens below would defang
// nothing at all.
func block(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
}

// stamp is the one timestamp layout the command line prints. UTC and absolute, never
// relative: "2 hours ago" would make the same row read differently every time it is
// listed, and the rows are evidence.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func stampSeconds(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
