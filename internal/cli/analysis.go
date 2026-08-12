package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

func newAnalysisCmd() *cobra.Command {
	c := &cobra.Command{Use: "analysis", Short: "O que o sistema andou fazendo"}

	list := &cobra.Command{
		Use:   "list",
		Short: "Lista as Análises recentes com o desfecho de cada uma, inclusive as descartadas",
		Long: "Lista as Análises da mais recente para a mais antiga.\n\n" +
			"As descartadas aparecem junto com as publicadas: encerrada por dúvida ou ruído,\n" +
			"recusada na entrada, falhada depois das retentativas. Poder ver o que foi\n" +
			"descartado, e julgar se a Triagem está errando, é a razão de nada ser apagado.",
		Args: cobra.NoArgs,
		RunE: runAnalysisList,
	}
	list.Flags().String("outcome", "", "filtra por desfecho: defeito|sugestão|dúvida|ruído|falhou|recusada")
	list.Flags().String("projeto", "", "filtra por Projeto")
	list.Flags().Bool("json", false, "saída em JSON")

	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Mostra o que foi entregue ao agente e o que ele respondeu",
		Args:  cobra.ExactArgs(1),
		RunE:  runAnalysisShow,
	}

	c.AddCommand(list, show)
	return c
}

// analysisJSON is the machine-readable line. It carries the same facts as the table
// plus the ones a table has no room for, so `--json` is never the poorer view.
type analysisJSON struct {
	ID         string `json:"id"`
	CriadaEm   string `json:"criada_em"`
	Projeto    string `json:"projeto"`
	Fonte      string `json:"fonte"`
	Rotulo     string `json:"rotulo,omitempty"`
	Estado     string `json:"estado"`
	Triagem    string `json:"triagem,omitempty"`
	Recusa     string `json:"recusa,omitempty"`
	Falha      string `json:"falha,omitempty"`
	Desfecho   string `json:"desfecho"`
	Prioridade int    `json:"prioridade"`
	IssueID    string `json:"issue_id,omitempty"`
	IssueURL   string `json:"issue_url,omitempty"`
	Titulo     string `json:"titulo"`
	Categoria  string `json:"categoria,omitempty"`
	Substatus  string `json:"substatus,omitempty"`
}

func runAnalysisList(cmd *cobra.Command, args []string) error {
	outcome, _ := cmd.Flags().GetString("outcome")
	projeto, _ := cmd.Flags().GetString("projeto")
	asJSON, _ := cmd.Flags().GetBool("json")

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		out := cmd.OutOrStdout()

		// An unknown Projeto is refused instead of listed as empty, for the same
		// reason store refuses an unknown --outcome: an empty list reads as "nothing
		// ever happened here", which is the one conclusion this table exists to make
		// impossible.
		if name := strings.TrimSpace(projeto); name != "" {
			if _, err := st.ProjectByName(ctx, name); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("projeto %q não está registrado; veja `nightwatcher project list`", name)
				}
				return err
			}
		}

		analyses, err := st.ListAnalyses(ctx, store.AnalysisFilter{
			Outcome:     strings.TrimSpace(outcome),
			ProjectName: strings.TrimSpace(projeto),
		})
		if err != nil {
			return err
		}

		names, fontes, labels, err := nameIndex(ctx, st)
		if err != nil {
			return err
		}

		if asJSON {
			rows := make([]analysisJSON, 0, len(analyses))
			for _, a := range analyses {
				rows = append(rows, analysisJSON{
					ID:         a.ID,
					CriadaEm:   a.CreatedAt.UTC().Format(time.RFC3339),
					Projeto:    names[a.ProjectID],
					Fonte:      fonteOf(a, fontes),
					Rotulo:     labels[a.SourceID],
					Estado:     string(a.State),
					Triagem:    string(a.Verdict),
					Recusa:     string(a.Refusal),
					Falha:      failureOf(a),
					Desfecho:   outcomeOf(a),
					Prioridade: int(a.Priority),
					IssueID:    a.IssueID,
					IssueURL:   a.IssueURL,
					Titulo:     a.Occurrence.Title,
					Categoria:  string(a.Occurrence.Category),
					Substatus:  string(a.Occurrence.Substatus),
				})
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}

		if len(analyses) == 0 {
			fmt.Fprintln(out, "nenhuma Análise registrada com esse filtro")
			return nil
		}

		rows := make([][]string, 0, len(analyses))
		for _, a := range analyses {
			rows = append(rows, []string{
				a.ID,
				stamp(a.CreatedAt),
				dash(names[a.ProjectID]),
				dash(fonteOf(a, fontes)),
				outcomeOf(a),
				dash(a.IssueURL),
			})
		}
		table(out, []string{"ID", "QUANDO", "PROJETO", "FONTE", "DESFECHO", "ISSUE"}, rows)
		return nil
	})
}

// nameIndex resolves the ids on an Análise into the names the operator reads. Two
// queries for the whole listing, not one per line.
func nameIndex(ctx context.Context, st *store.Store) (names map[int64]string, fontes map[int64]string, labels map[int64]string, err error) {
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	names = map[int64]string{}
	fontes = map[int64]string{}
	labels = map[int64]string{}
	for _, p := range projects {
		names[p.ID] = p.Name
		for _, src := range p.Sources {
			fontes[src.ID] = src.SentrySlug
			labels[src.ID] = src.Label
		}
	}
	return names, fontes, labels, nil
}

// fonteOf falls back to the slug carried by the Ocorrência itself, which is the only
// thing a refusal by unknown Fonte has: there is no source row to point at, and that
// recusa is precisely the one an operator goes looking for.
func fonteOf(a domain.Analysis, fontes map[int64]string) string {
	if slug, ok := fontes[a.SourceID]; ok && slug != "" {
		return slug
	}
	return a.Occurrence.SentryProjectSlug
}

// outcomeOf renders the state together with what qualifies it, because "encerrada" on
// its own does not say whether the Triagem called it a dúvida or ruído, and "recusada"
// does not say whether the signature was forged or the category was outage.
func outcomeOf(a domain.Analysis) string {
	switch a.State {
	case domain.StatePublished, domain.StateClosed:
		if a.Verdict != "" {
			return fmt.Sprintf("%s (%s)", a.State, a.Verdict)
		}
	case domain.StateRefused:
		if a.Refusal != "" {
			return fmt.Sprintf("%s (%s)", a.State, a.Refusal)
		}
	case domain.StateFailed, domain.StateFailing:
		if f := failureOf(a); f != "" {
			return fmt.Sprintf("%s (%s)", a.State, f)
		}
	}
	if a.State == "" {
		return "—"
	}
	return string(a.State)
}

func failureOf(a domain.Analysis) string {
	switch {
	case a.FailureKind != "" && a.FailureReason != "":
		return fmt.Sprintf("%s: %s", a.FailureKind, a.FailureReason)
	case a.FailureKind != "":
		return string(a.FailureKind)
	default:
		return a.FailureReason
	}
}

func runAnalysisShow(cmd *cobra.Command, args []string) error {
	id := strings.TrimSpace(args[0])

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		a, err := st.Analysis(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("não existe Análise %s; veja `nightwatcher analysis list`", id)
			}
			return err
		}

		names, fontes, labels, err := nameIndex(ctx, st)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fonte := fonteOf(a, fontes)
		if label := labels[a.SourceID]; label != "" {
			fonte = fmt.Sprintf("%s (rótulo: %s)", fonte, label)
		}

		fmt.Fprintf(out, "Análise %s\n", a.ID)
		rows := [][]string{
			{"  criada em", stampSeconds(a.CreatedAt)},
			{"  atualizada em", stampSeconds(a.UpdatedAt)},
			{"  projeto", dash(names[a.ProjectID])},
			{"  fonte", dash(fonte)},
			{"  desfecho", outcomeOf(a)},
			{"  prioridade", a.Priority.String()},
		}
		if a.PriorityRationale != "" {
			rows = append(rows, []string{"  ajuste do agente", a.PriorityRationale})
		}
		if a.Attempts > 0 {
			rows = append(rows, []string{"  tentativas", fmt.Sprint(a.Attempts)})
		}
		if a.IssueID != "" || a.IssueURL != "" {
			rows = append(rows, []string{"  issue", strings.TrimSpace(a.IssueID + " " + a.IssueURL)})
		}
		if f := failureOf(a); f != "" {
			rows = append(rows, []string{"  falha", f})
		}
		table(out, []string{"  CAMPO", "VALOR"}, rows)

		o := a.Occurrence
		fmt.Fprintln(out, "\nOcorrência")
		table(out, []string{"  CAMPO", "VALOR"}, [][]string{
			{"  título", dash(o.Title)},
			{"  categoria", dash(string(o.Category))},
			{"  nível", dash(string(o.Level))},
			{"  ação", dash(strings.TrimSpace(o.Action + " " + string(o.Substatus)))},
			{"  ambiente", dash(o.Environment)},
			{"  pessoas atingidas", fmt.Sprint(o.UserCount)},
			{"  eventos", fmt.Sprint(o.Count)},
			{"  culprit", dash(o.Culprit)},
			{"  no Sentry", dash(firstFilled(o.WebURL, o.Permalink))},
		})

		// The sessions are the point of this command: what was handed to the agent and
		// what came back is the raw material for diagnosing a bad Triagem.
		fmt.Fprintf(out, "\nSessões (%d)\n", len(a.Sessions))
		if len(a.Sessions) == 0 {
			fmt.Fprintln(out, "  nenhuma sessão foi aberta para esta Análise")
			return nil
		}
		for _, sess := range a.Sessions {
			printSession(out, sess)
		}
		return nil
	})
}

func printSession(out io.Writer, sess domain.Session) {
	fmt.Fprintf(out, "\n── %s  %s → %s", sess.Kind, stampSeconds(sess.StartedAt), stampSeconds(sess.EndedAt))
	if !sess.StartedAt.IsZero() && !sess.EndedAt.IsZero() {
		fmt.Fprintf(out, "  (%s)", sess.EndedAt.Sub(sess.StartedAt).Round(time.Second))
	}
	fmt.Fprintln(out)
	if sess.Err != "" {
		fmt.Fprintf(out, "erro: %s\n", cell(sess.Err))
	}

	fmt.Fprintln(out, "\nprompt:")
	fmt.Fprintln(out, indent(sess.Prompt))
	fmt.Fprintln(out, "resposta:")
	fmt.Fprintln(out, indent(sess.Response))
}

// indent offsets the agent's text so it cannot be mistaken for the command's own
// output — the prompt carries the Ocorrência, which is written by an end user. For the
// same reason it goes through block() first: the offset says whose text it is, and
// block() is what stops that text from driving the terminal it is printed on.
func indent(s string) string {
	if strings.TrimSpace(s) == "" {
		return "  (vazio)"
	}
	lines := strings.Split(strings.TrimRight(block(s), "\n"), "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}

func firstFilled(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
