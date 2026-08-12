package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
	"github.com/eduardotorresdev/nightwatcher/internal/worker"
)

func newOccurrenceCmd() *cobra.Command {
	c := &cobra.Command{Use: "occurrence", Short: "Reprocessar e simular Ocorrências"}

	replay := &cobra.Command{
		Use:   "replay <id>",
		Short: "Reprocessa uma Ocorrência conhecida contra os prompts atuais, sem tocar no Destino",
		Long: "Reprocessa uma Ocorrência já registrada contra os prompts de hoje.\n\n" +
			"Serve para julgar uma mudança de prompt contra um caso real conhecido: a mesma\n" +
			"entrega, relida pelo mesmo parser do endpoint, conduzida pelo mesmo Analyst do\n" +
			"daemon, com uma sessão de verdade sobre o clone de verdade.\n\n" +
			"NÃO escreve no Destino e NÃO escreve no banco — nem com flag. Ver o comentário\n" +
			"de runReplay: a Análise gravada já produziu a issue dela, e um replay que\n" +
			"escrevesse abriria uma segunda issue para uma Ocorrência que já está no board.",
		Args: cobra.ExactArgs(1),
		RunE: runReplay,
	}
	replay.Flags().Bool(flagSessions, false, "imprime o prompt e a resposta de cada sessão")

	simulate := &cobra.Command{
		Use:   "simulate <arquivo>",
		Short: "Simula uma Ocorrência sem deixar rastro: nada no Destino, nada no banco",
		Long: "Lê uma entrega gravada do Sentry de um arquivo e mostra o que teria acontecido.\n\n" +
			"O arquivo é lido pelo mesmo ingest.Parse do endpoint e passa pela mesma política\n" +
			"de recusa, para que a calibragem seja sobre o caminho que existe de verdade.\n\n" +
			"Nada é criado no Destino e nada é gravado no banco. É ferramenta de calibragem:\n" +
			"dá para rodar contra testdata/sentry/*.json antes de confiar num prompt novo.",
		Args: cobra.ExactArgs(1),
		RunE: runSimulate,
	}
	simulate.Flags().Bool(flagSessions, false, "imprime o prompt e a resposta de cada sessão")
	simulate.Flags().String(flagProjeto, "",
		"simula contra este Projeto quando a Fonte da entrega não está registrada")

	c.AddCommand(replay, simulate)
	return c
}

const (
	flagSessions = "sessions"
	flagProjeto  = "projeto"
)

// runReplay re-runs a recorded Ocorrência and writes nothing, anywhere.
//
// The ticket left the question open and this is the answer: `replay` never touches the
// Destino. Its purpose (User Story 32) is judging a prompt change against a real case,
// and the Análise being replayed has already produced its issue. Publishing again would
// open a SECOND issue for an Ocorrência that is on the board — precisely the noise User
// Stories 13 and 14 exist to prevent — because the reincidência lookup skips the Análise
// it is running (worker.previousIssue: `old.ID == an.ID`), so it would not even find the
// issue to comment on instead. There is no undo for an issue in Linear, and a --dry-run
// that defaulted to on would still leave a flag whose off position is only ever a
// mistake. What a prompt change needs is the reply, and the reply is printed here.
func runReplay(cmd *cobra.Command, args []string) error {
	id := strings.TrimSpace(args[0])
	sessions, _ := cmd.Flags().GetBool(flagSessions)

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		recorded, err := st.Analysis(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("não existe Análise %s; veja `nightwatcher analysis list`", id)
			}
			return err
		}

		// A recusa decided before the Fonte was resolved — an unknown slug, a body that
		// did not parse — carries no Projeto at all, and those are exactly the rows an
		// operator goes looking for in `analysis list`. Saying "the Projeto 0 is no
		// longer registered" would send them hunting for a Projeto that never existed.
		if recorded.ProjectID == 0 {
			return fmt.Errorf(
				"a Análise %s foi recusada na entrada (%s) antes de qualquer Projeto ser resolvido, "+
					"então não há repositório para reprocessá-la.\n"+
					"Registre a Fonte %q com `nightwatcher project add`: a próxima entrega dela é analisada",
				recorded.ID, dash(string(recorded.Refusal)), recorded.Occurrence.SentryProjectSlug)
		}

		proj, err := st.ProjectByID(ctx, recorded.ProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("a Análise %s aponta para o Projeto %d, que não está mais registrado",
					recorded.ID, recorded.ProjectID)
			}
			return err
		}

		occ, err := occurrenceOf(recorded)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Replay da Análise %s (%s)\n", recorded.ID, outcomeOf(recorded))
		if recorded.IssueID != "" {
			// Named up front so the operator reads the comparison, and so it is obvious
			// that the issue below is not this one being written again.
			fmt.Fprintf(out, "  a Análise gravada já produziu %s — nada aqui a altera\n",
				strings.TrimSpace(recorded.IssueID+" "+recorded.IssueURL))
		}
		fmt.Fprintln(out)

		return dryRun(ctx, cmd, st, dryRunSpec{
			project:    proj,
			source:     sourceOf(proj, recorded.SourceID, occ.SentryProjectSlug),
			occurrence: occ,
			analysisID: recorded.ID,
			sessions:   sessions,
		})
	})
}

// occurrenceOf rebuilds the Ocorrência from the bytes Sentry really delivered, through
// the very parser the endpoint uses, so a replay judges today's code against yesterday's
// delivery instead of against columns written by the parser of the day.
//
// The recorded action is passed as the override for the reason ingest.Parse documents:
// the action is a column of the record, and a hand-edited body must not be able to make
// a replay mean something different from what actually arrived.
func occurrenceOf(recorded domain.Analysis) (domain.Occurrence, error) {
	old := recorded.Occurrence
	if len(old.RawBody) == 0 {
		// A row recorded with no body — a refusal that never had one. Replaying it from
		// the columns is less faithful and much better than refusing to replay it.
		return old, nil
	}
	// The resource is read back off the recorded bytes rather than assumed: an Análise
	// recorded from an `event_alert` has `data.event` and no `data.issue`, and replaying
	// it as an issue delivery would fail every time — turning `occurrence replay` into a
	// tool that works on half the Ocorrências.
	occ, err := ingest.Parse(old.RawBody, ingest.DetectResource(old.RawBody), old.Action)
	if err != nil {
		return domain.Occurrence{}, fmt.Errorf(
			"relendo a entrega gravada da Análise %s: %w", recorded.ID, err)
	}
	// The instant of arrival belongs to the record and not to the parser. The
	// environment is kept from the record only when the payload has none: on the issue
	// door it is absent and cost a separate tags call, so the recorded column
	// is the only copy; on the event_alert door it comes in the body, and the parsed
	// value is the more faithful one to replay against.
	if occ.Environment == "" {
		occ.Environment = old.Environment
	}
	occ.ReceivedAt = old.ReceivedAt
	return occ, nil
}

func runSimulate(cmd *cobra.Command, args []string) error {
	path := args[0]
	sessions, _ := cmd.Flags().GetBool(flagSessions)
	override, _ := cmd.Flags().GetString(flagProjeto)

	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lendo a entrega gravada em %s: %w", path, err)
	}

	// Parsed by the exact function the endpoint calls. A simulation that parsed
	// differently would be worthless as calibration — which is why ingest.Parse is
	// exported at all.
	//
	// The signature is deliberately NOT verified: this file was chosen off disk by the
	// operator and did not arrive from the network, so there is no forgery for an HMAC
	// to catch, and requiring the Client Secret would stop the tool working against the
	// recorded fixtures.
	occ, err := ingest.Parse(body, ingest.DetectResource(body), "")
	if err != nil {
		return fmt.Errorf("lendo %s como entrega do Sentry: %w", path, err)
	}
	occ.ReceivedAt = clock.Now()

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		proj, src, err := projectFor(ctx, st, occ, override)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Simulação de %s contra o Projeto %s\n\n", path, proj.Name)

		return dryRun(ctx, cmd, st, dryRunSpec{
			project:    proj,
			source:     src,
			occurrence: occ,
			analysisID: "simulação",
			sessions:   sessions,
		})
	})
}

// projectFor answers the question the endpoint asks on every delivery: which Fonte is
// this Sentry slug, and which Projeto does it belong to. An unknown slug is the
// "fonte desconhecida" recusa, and saying so — instead of silently picking something —
// is what keeps the simulation honest about what the daemon would do.
func projectFor(ctx context.Context, st *store.Store, occ domain.Occurrence, override string) (domain.Project, domain.Source, error) {
	if name := strings.TrimSpace(override); name != "" {
		p, err := st.ProjectByName(ctx, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return domain.Project{}, domain.Source{},
					fmt.Errorf("projeto %q não está registrado; veja `nightwatcher project list`", name)
			}
			return domain.Project{}, domain.Source{}, err
		}
		return p, sourceOf(p, 0, occ.SentryProjectSlug), nil
	}

	p, src, err := st.ResolveSource(ctx, occ.SentryProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.Project{}, domain.Source{}, fmt.Errorf(
				"o projeto Sentry %q não está registrado como Fonte, então o daemon recusaria esta entrega como fonte desconhecida.\n"+
					"Use --%s <nome> para simular contra outro Projeto, ou registre a Fonte com `nightwatcher project add`",
				occ.SentryProjectSlug, flagProjeto)
		}
		return domain.Project{}, domain.Source{}, err
	}

	// ResolveSource answers in one query and leaves the Projeto's Fontes unloaded, which
	// is right inside the request cycle and wrong here: this Projeto is handed to
	// worker.NopJournal, and the Analyst looks the Fonte up inside it to carry the rótulo
	// to the issue. With no Fontes loaded the simulation would quietly drop the label
	// that the real Análise keeps, and the preview would lie about the board filter.
	full, err := st.ProjectByID(ctx, p.ID)
	if err != nil {
		return domain.Project{}, domain.Source{}, err
	}
	return full, src, nil
}

// sourceOf finds the Fonte an Ocorrência came through, so its rótulo reaches the issue
// exactly as it would in production. The slug is the fallback because a Fonte repointed
// at another Projeto keeps its slug and changes its id.
func sourceOf(p domain.Project, sourceID int64, slug string) domain.Source {
	for _, s := range p.Sources {
		if sourceID != 0 && s.ID == sourceID {
			return s
		}
	}
	for _, s := range p.Sources {
		if slug != "" && s.SentrySlug == slug {
			return s
		}
	}
	return domain.Source{}
}

type dryRunSpec struct {
	project    domain.Project
	source     domain.Source
	occurrence domain.Occurrence
	analysisID string
	sessions   bool
}

// dryRun conducts one Ocorrência through the very pipeline the daemon uses, with both
// ends replaced by doubles that cannot write: worker.NopJournal where the store would
// be, and tracker.Spy where Linear would be. Everything between them is production
// code — the same Analyst, the same prompts, the same real Claude session over the same
// real clone — because a calibration run through a different pipeline calibrates
// nothing.
func dryRun(ctx context.Context, cmd *cobra.Command, st *store.Store, spec dryRunSpec) error {
	out := cmd.OutOrStdout()
	printOccurrence(out, spec.occurrence, spec.source)

	// The same policy gate the endpoint applies before queueing. Without it a simulation
	// of an `outage` would spend a twenty-minute session on a path the daemon refuses at
	// ingest and never analyses.
	if reason, detail, refused := ingest.Refusal(spec.occurrence); refused {
		fmt.Fprintf(out, "\nA entrega seria RECUSADA na entrada: %s\n", reason)
		if detail != "" {
			fmt.Fprintf(out, "  %s\n", detail)
		}
		fmt.Fprintln(out, "  a recusa é gravada e fica listável; nenhuma sessão é aberta.")
		return nil
	}

	// The operating numbers come from the database like the daemon's do, so a simulation
	// obeys the same ceilings the real Análise would.
	tun, err := st.Tunables(ctx)
	if err != nil {
		return fmt.Errorf("lendo os números da operação: %w", err)
	}

	secrets := config.LoadSecrets()
	// A dry run still raises a real session over a real clone — that is the whole point
	// of judging a prompt against a real case. It needs exactly these two credentials
	// and no others: no Linear, because the Destino is a Spy, and no Client Secret,
	// because nothing here arrived over the network to authenticate.
	var need []string
	if strings.TrimSpace(secrets.ClaudeOAuthToken) == "" {
		need = append(need, config.EnvClaudeOAuthToken)
	}
	if strings.TrimSpace(secrets.GitHubToken) == "" {
		need = append(need, config.EnvGitHubToken)
	}
	if len(need) > 0 {
		return fmt.Errorf("faltam variáveis de ambiente para abrir a sessão e ler o repositório: %s",
			strings.Join(need, ", "))
	}

	// No team: no Destino is built at all, so there is no path from here to Linear even
	// if a credential happens to be in the environment.
	//
	// The Sentry endpoint does travel, for the same reason it travels into `serve`: the
	// Analyst makes a real tags call to learn the Ocorrência's ambiente, and a dry run
	// pointed at an httptest Sentry that quietly reached sentry.io instead would be
	// proving nothing. That flag has to mean the same thing in every command that reads
	// it or it is worse than not existing.
	world, err := newWorld(worldSpec{
		secrets: secrets,
		paths:   config.LoadPaths(),
		logger:  slog.Default(),
		sentry:  newRemote(cmd).sentry,
	})
	if err != nil {
		return err
	}

	spy := tracker.NewSpy()
	journal := &recordingJournal{NopJournal: worker.NopJournal{Project: spec.project}}
	analyst, err := worker.NewAnalyst(worker.AnalystDeps{
		Journal:   journal,
		Agent:     world.agent,
		Tracker:   spy,
		Workspace: world.workspace,
		SentryAPI: world.sentry,
		Clock:     clock,
		Tunables:  tun,
		// The Analyst's own log goes to stderr at warning level: what it decided is in
		// the report below, and what went wrong deserves to be visible without burying
		// the report in it.
		Logger:         slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelWarn})),
		SessionWorkDir: world.sessionDir,
	})
	if err != nil {
		return err
	}

	an, runErr := analyst.Analyse(ctx, domain.Analysis{
		ID:         spec.analysisID,
		ProjectID:  spec.project.ID,
		SourceID:   spec.source.ID,
		Occurrence: spec.occurrence,
		State:      domain.StateReceived,
		CreatedAt:  clock.Now(),
	})

	printOutcome(out, an, spy, runErr)
	if spec.sessions {
		printSessions(out, journal.recorded())
	}
	fmt.Fprintf(out, "\nNada foi criado no Destino e nada foi gravado no banco.\n")
	return nil
}

// recordingJournal is worker.NopJournal with a memory. It still writes nothing — every
// method that persists is the embedded one — and it keeps the sessions so `--sessions`
// can print the prompt and the reply, which is the material a prompt change is judged
// on.
type recordingJournal struct {
	worker.NopJournal
	mu       sync.Mutex
	sessions []domain.Session
}

func (j *recordingJournal) AppendSession(ctx context.Context, id string, s domain.Session) (domain.Session, error) {
	j.mu.Lock()
	j.sessions = append(j.sessions, s)
	j.mu.Unlock()
	return j.NopJournal.AppendSession(ctx, id, s)
}

func (j *recordingJournal) recorded() []domain.Session {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]domain.Session(nil), j.sessions...)
}

func printOccurrence(out io.Writer, occ domain.Occurrence, src domain.Source) {
	fmt.Fprintln(out, "Ocorrência")
	rows := [][]string{
		{"  título", dash(occ.Title)},
		{"  categoria", dash(string(occ.Category))},
		{"  nível", dash(string(occ.Level))},
		{"  evento", dash(strings.TrimSpace(occ.Action + " " + string(occ.Substatus)))},
		{"  fonte", dash(occ.SentryProjectSlug)},
	}
	if src.Label != "" {
		rows = append(rows, []string{"  rótulo", src.Label})
	}
	rows = append(rows,
		[]string{"  pessoas atingidas", fmt.Sprint(occ.UserCount)},
		[]string{"  eventos", fmt.Sprint(occ.Count)},
		// The dedup key is printed because it is the identity the daemon would refuse a
		// second delivery on, and it is not obvious from the fields above.
		[]string{"  chave de dedupe", occ.DedupKey()},
		[]string{"  no Sentry", dash(firstFilled(occ.WebURL, occ.Permalink))},
	)
	table(out, []string{"  CAMPO", "VALOR"}, rows)
}

func printOutcome(out io.Writer, an domain.Analysis, spy *tracker.Spy, runErr error) {
	fmt.Fprintln(out, "\nDesfecho")
	rows := [][]string{
		{"  desfecho", outcomeOf(an)},
		{"  prioridade", an.Priority.String()},
	}
	if an.PriorityRationale != "" {
		rows = append(rows, []string{"  ajuste do agente", an.PriorityRationale})
	}
	if f := failureOf(an); f != "" {
		rows = append(rows, []string{"  falha", f})
	}
	table(out, []string{"  CAMPO", "VALOR"}, rows)

	if runErr != nil {
		// The Analyst leaves through here when the Ocorrência must come back: a session
		// that failed with attempts left, or a Destino that could not be reached. In
		// production the queue item would return with a wait; here it is the end.
		fmt.Fprintf(out, "\nA Análise não chegou a um desfecho e voltaria para a fila:\n%s\n", indent(runErr.Error()))
	}

	created := spy.Created()
	comments := spy.AllComments()
	if len(created) == 0 && len(comments) == 0 {
		fmt.Fprintln(out, "\nDestino: nada seria criado nem comentado.")
		return
	}

	fmt.Fprintln(out, "\nDestino (o que teria sido escrito no Linear)")
	for _, issue := range created {
		fmt.Fprintln(out, "\n  issue nova")
		irows := [][]string{
			{"    Project", dash(issue.ProjectID)},
			{"    título", issue.Title},
			{"    prioridade", issue.Priority.String()},
		}
		if len(issue.Labels) > 0 {
			irows = append(irows, []string{"    rótulos", strings.Join(issue.Labels, ", ")})
		}
		if issue.RelatedTo != "" {
			irows = append(irows, []string{"    relacionada a", issue.RelatedTo})
		}
		if issue.AttachmentURL != "" {
			irows = append(irows, []string{"    anexo", issue.AttachmentURL})
		}
		table(out, []string{"    CAMPO", "VALOR"}, irows)
		fmt.Fprintln(out, "\n  corpo:")
		fmt.Fprintln(out, indent(issue.Body))
	}
	for _, c := range comments {
		fmt.Fprintf(out, "\n  comentário na issue %s:\n", c.IssueID)
		fmt.Fprintln(out, indent(c.Body))
	}
}

func printSessions(out io.Writer, sessions []domain.Session) {
	fmt.Fprintf(out, "\nSessões (%d)\n", len(sessions))
	for _, s := range sessions {
		printSession(out, s)
	}
}
