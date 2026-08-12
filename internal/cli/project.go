package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// Flags of `project add`. They are the same set the guided flow asks for, one to one,
// so a guided run can be replayed as a scripted one by reading the questions back.
const (
	flagNome          = "nome"
	flagRepo          = "repo"
	flagBranch        = "branch"
	flagLinearProject = "linear-project"
	flagFonte         = "fonte"
	flagInteractive   = "interactive"
)

func newProjectCmd() *cobra.Command {
	c := &cobra.Command{Use: "project", Short: "Projetos: repositório, Destino e Fontes Sentry"}

	add := &cobra.Command{
		Use:   "add",
		Short: "Adiciona um Projeto (guiado por padrão, validando cada resposta ao vivo)",
		Long: "Adiciona um Projeto: um repositório, um Destino e uma ou mais Fontes Sentry.\n\n" +
			"Sem argumentos e com terminal, pergunta em sequência e confere cada resposta ao\n" +
			"vivo — o repositório no GitHub (de onde sai a branch default), os Projects do\n" +
			"Linear para escolha por número, e cada Fonte no Sentry. Assim um erro de\n" +
			"digitação aparece na pergunta seguinte, e não no primeiro webhook.\n\n" +
			"Com todas as flags preenchidas, roda direto e não pergunta nada. Sem terminal e\n" +
			"sem flags, falha dizendo o que faltou: `dokku run -d` não tem terminal, e falhar\n" +
			"claro é melhor do que travar esperando uma resposta que nunca vem.",
		Args: cobra.NoArgs,
		RunE: runProjectAdd,
	}
	add.Flags().String(flagNome, "", "nome do Projeto")
	add.Flags().String(flagRepo, "", "repositório no GitHub (owner/repo)")
	add.Flags().String(flagBranch, "", "branch analisada (padrão: a branch default do repo)")
	add.Flags().String(flagLinearProject, "", "Project do Linear que é o Destino")
	add.Flags().StringArray(flagFonte, nil, "Fonte Sentry no formato slug:característica (repetível)")
	// Hidden: forces the guided flow where there is no terminal. It exists so the
	// guided half can be exercised by a test with a scripted stdin, and it is honest
	// about being usable to pipe answers in.
	add.Flags().Bool(flagInteractive, false, "força o modo guiado mesmo sem terminal")
	_ = add.Flags().MarkHidden(flagInteractive)

	list := &cobra.Command{
		Use:   "list",
		Short: "Lista os Projetos e suas Fontes",
		Args:  cobra.NoArgs,
		RunE:  runProjectList,
	}
	list.Flags().Bool("json", false, "saída em JSON")

	c.AddCommand(add, list)
	return c
}

// draft is the Projeto being assembled, whether by flag or by question. It exists so
// the two paths converge on one value instead of on five parallel variables.
type draft struct {
	name          string
	repo          string
	branch        string
	linearProject string
	sources       []domain.Source

	// mcpServers are the servers the guided flow collected. There is no --mcp flag to
	// fill this from a script, and deliberately not: a server needs a name, a URL, a
	// header name, a prefix and a variable name, and cramming five fields into one
	// repeatable string produces a flag nobody can type correctly and an error nobody
	// can read. `nightwatcher mcp add --projeto <nome>` is the scripted path, and it
	// is a complete one.
	mcpServers []domain.MCPServer
}

func runProjectAdd(cmd *cobra.Command, args []string) error {
	flags := cmd.Flags()
	name, _ := flags.GetString(flagNome)
	repo, _ := flags.GetString(flagRepo)
	branch, _ := flags.GetString(flagBranch)
	linearProject, _ := flags.GetString(flagLinearProject)
	fonteFlags, _ := flags.GetStringArray(flagFonte)
	forced, _ := flags.GetBool(flagInteractive)

	sources, err := parseFontes(fonteFlags)
	if err != nil {
		return err
	}
	d := &draft{
		name:          strings.TrimSpace(name),
		repo:          strings.TrimSpace(repo),
		branch:        strings.TrimSpace(branch),
		linearProject: strings.TrimSpace(linearProject),
		sources:       sources,
	}

	// There is nothing to ask when the command line already says everything, and a
	// terminal being attached does not change that: `dokku run` allocates one by
	// default, so gating the guided flow on the terminal alone would make the scripted
	// invocation this command documents — every flag given, nothing asked — unreachable
	// in the very environment it is meant for. It would still stop on "Outra Fonte
	// Sentry (vazio para terminar)" and wait for an Enter nobody scripted.
	missing := d.missingFlags()
	guided := forced || (missing != nil && interactive(cmd))

	// Without a terminal, everything has to be on the command line. The check runs
	// before the database is opened, so a refused invocation writes nothing at all.
	if !guided && missing != nil {
		return missing
	}

	r := newRemote(cmd)

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		out := cmd.OutOrStdout()

		switch {
		case guided:
			if err := d.guide(ctx, cmd, r, st); err != nil {
				return err
			}
		case d.branch == "":
			// The one network call the scripted path makes, and only because there is
			// no answer to give without it.
			found, err := r.repo(ctx, d.repo)
			if err != nil {
				return fmt.Errorf("descobrindo a branch default de %s: %w", d.repo, err)
			}
			d.branch = found.DefaultBranch
			fmt.Fprintf(out, "branch default de %s: %s\n", d.repo, d.branch)
		}

		saved, err := st.UpsertProject(ctx, domain.Project{
			Name:            d.name,
			Repo:            d.repo,
			Branch:          d.branch,
			LinearProjectID: d.linearProject,
			Sources:         d.sources,
		}, clock.Now())
		if err != nil {
			return err
		}

		// After the Projeto and not with it: a server needs the Projeto's id, and the
		// connectivity check already ran before any of this was written.
		for _, m := range d.mcpServers {
			m.ProjectID = saved.ID
			stored, err := st.UpsertMCPServer(ctx, m, clock.Now())
			if err != nil {
				return err
			}
			saved.MCPServers = append(saved.MCPServers, stored)
		}

		fmt.Fprintf(out, "\nProjeto %s registrado.\n", saved.Name)
		rows := [][]string{
			{"  repositório", saved.Repo + " @ " + saved.Branch},
			{"  Destino (Linear)", saved.LinearProjectID},
			{"  Fontes", fontesLine(saved.Sources)},
		}
		if len(saved.MCPServers) > 0 {
			rows = append(rows, []string{"  Servidores MCP", mcpLine(saved.MCPServers)})
		}
		table(out, []string{"  CAMPO", "VALOR"}, rows)
		return nil
	})
}

// missingFlags is the no-terminal failure: it says exactly what was missing, because
// `dokku run -d` has no terminal and failing clearly beats hanging on input that will
// never come.
func (d *draft) missingFlags() error {
	var missing []string
	if d.name == "" {
		missing = append(missing, "--"+flagNome)
	}
	if d.repo == "" {
		missing = append(missing, "--"+flagRepo)
	}
	if d.linearProject == "" {
		missing = append(missing, "--"+flagLinearProject)
	}
	if len(d.sources) == 0 {
		missing = append(missing, "--"+flagFonte)
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"sem terminal para perguntar, então tudo precisa vir por flag; faltou: %s\n"+
			"(--%s é opcional: sem ela a branch default do repositório é lida no GitHub)",
		strings.Join(missing, ", "), flagBranch)
}

// guide asks for whatever is still missing and confirms every answer against the
// service that owns it. Values given by flag are confirmed too: being typed on the
// command line makes a slug no less mistyped.
func (d *draft) guide(ctx context.Context, cmd *cobra.Command, r *remote, st *store.Store) error {
	p := newPrompter(cmd)

	if d.name == "" {
		answer, err := p.askValid("Nome do Projeto", "", func(v string) error {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("o nome não pode ser vazio")
			}
			return nil
		})
		if err != nil {
			return err
		}
		d.name = strings.TrimSpace(answer)
	}

	// The repository question proves it exists and yields the default branch in the
	// same call.
	var found githubRepo
	confirmRepo := func(v string) error {
		got, err := r.repo(ctx, v)
		if err != nil {
			return err
		}
		found = got
		return nil
	}
	if d.repo != "" {
		if err := confirmRepo(d.repo); err != nil {
			return err
		}
	} else {
		answer, err := p.askValid("Repositório no GitHub (owner/repo)", "", confirmRepo)
		if err != nil {
			return err
		}
		d.repo = strings.TrimSpace(answer)
	}
	p.printf("  ✓ %s (branch default: %s)\n", found.FullName, found.DefaultBranch)

	if d.branch == "" {
		answer, err := p.ask("Branch analisada", found.DefaultBranch)
		if err != nil {
			return err
		}
		d.branch = strings.TrimSpace(answer)
	}

	if d.linearProject == "" {
		chosen, err := chooseLinearProject(ctx, p, r)
		if err != nil {
			return err
		}
		d.linearProject = chosen
	}

	sources, err := askFontes(ctx, p, r, st, d.sources)
	if err != nil {
		return err
	}
	d.sources = sources

	servers, err := askMCPServers(ctx, cmd, p)
	if err != nil {
		return err
	}
	d.mcpServers = servers
	return nil
}

// chooseLinearProject lists the Destinos by number. When the listing cannot be had —
// no credential, Linear unreachable — the flow degrades to asking for the id rather
// than dying: a Projeto registered with a wrong Destino is fixable, a `project add`
// that refuses to finish is not.
func chooseLinearProject(ctx context.Context, p *prompter, r *remote) (string, error) {
	projects, err := r.linearProjects(ctx)
	if err != nil {
		p.printf("  ! não consegui listar os Projects do Linear: %v\n", err)
		return p.askValid("Id do Project do Linear (Destino)", "", func(v string) error {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("o Destino não pode ser vazio")
			}
			return nil
		})
	}
	if len(projects) == 0 {
		return "", fmt.Errorf("o Linear não devolveu nenhum Project para esta credencial")
	}

	p.printf("Projects do Linear:\n")
	for i, lp := range projects {
		p.printf("  %2d) %s\n", i+1, lp.Name)
	}

	var chosen string
	_, err = p.askValid("Destino (número ou nome)", "", func(v string) error {
		v = strings.TrimSpace(v)
		if n, convErr := strconv.Atoi(v); convErr == nil {
			if n < 1 || n > len(projects) {
				return fmt.Errorf("escolha entre 1 e %d", len(projects))
			}
			chosen = projects[n-1].ID
			return nil
		}
		for _, lp := range projects {
			if strings.EqualFold(strings.TrimSpace(lp.Name), v) || lp.ID == v {
				chosen = lp.ID
				return nil
			}
		}
		return fmt.Errorf("não existe Project %q na lista", v)
	})
	if err != nil {
		return "", err
	}
	return chosen, nil
}

// askFontes collects the Fontes, confirming each against Sentry before accepting it.
// At least one is required: a Projeto with no Fonte receives no webhook and is a
// Projeto that does nothing.
func askFontes(ctx context.Context, p *prompter, r *remote, st *store.Store, given []domain.Source) ([]domain.Source, error) {
	org, err := identity(ctx, st, settingSentryOrg, envSentryOrg)
	if err != nil {
		return nil, err
	}

	confirms := strings.TrimSpace(r.secrets.SentryToken) != ""
	if !confirms {
		p.printf("  ! %s não está no ambiente; as Fontes entram sem confirmação no Sentry\n", config.EnvSentryToken)
	}

	if confirms && org == "" {
		answer, err := p.askValid("Organização no Sentry", "", func(v string) error {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("a organização não pode ser vazia")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		org = strings.TrimSpace(answer)
		// Stored so the next `project add` and every `config validate` stop asking.
		if err := st.SetSetting(ctx, settingSentryOrg, org); err != nil {
			return nil, err
		}
	}

	confirm := func(src domain.Source) error {
		if !confirms {
			return nil
		}
		if _, err := r.fonte(ctx, org, src.SentrySlug); err != nil {
			return err
		}
		return nil
	}

	sources := make([]domain.Source, 0, len(given)+1)
	for _, src := range given {
		if err := confirm(src); err != nil {
			return nil, err
		}
		p.printf("  ✓ Fonte %s (rótulo: %s)\n", src.SentrySlug, src.Label)
		sources = append(sources, src)
	}

	for {
		question := "Fonte Sentry (slug:rótulo)"
		if len(sources) > 0 {
			question = "Outra Fonte Sentry (slug:rótulo, vazio para terminar)"
		}

		var accepted domain.Source
		var done bool
		_, err := p.askValid(question, "", func(v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				if len(sources) == 0 {
					return fmt.Errorf("um Projeto precisa de pelo menos uma Fonte")
				}
				done = true
				return nil
			}
			src, err := parseFonte(v)
			if err != nil {
				return err
			}
			if err := confirm(src); err != nil {
				return err
			}
			accepted = src
			return nil
		})
		if err != nil {
			return nil, err
		}
		if done {
			return sources, nil
		}
		p.printf("  ✓ Fonte %s (rótulo: %s)\n", accepted.SentrySlug, accepted.Label)
		sources = append(sources, accepted)
	}
}

// parseFontes turns the repeated --fonte flag into Fontes.
func parseFontes(values []string) ([]domain.Source, error) {
	out := make([]domain.Source, 0, len(values))
	for _, v := range values {
		src, err := parseFonte(v)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// parseFonte splits `slug:rótulo`. The rótulo is required and not defaulted to the
// slug: it is free text that becomes a Linear label, and "me mostra tudo que veio da
// API" is only a board filter if somebody chose the word (User Story 28).
func parseFonte(v string) (domain.Source, error) {
	slug, label, ok := strings.Cut(strings.TrimSpace(v), ":")
	slug = strings.TrimSpace(slug)
	label = strings.TrimSpace(label)
	if !ok || slug == "" || label == "" {
		return domain.Source{}, fmt.Errorf("Fonte %q não está no formato slug:característica (ex.: acme-web:web)", v)
	}
	return domain.Source{SentrySlug: slug, Label: label}, nil
}

func fontesLine(sources []domain.Source) string {
	if len(sources) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(sources))
	for _, src := range sources {
		parts = append(parts, fmt.Sprintf("%s (%s)", src.SentrySlug, src.Label))
	}
	return strings.Join(parts, ", ")
}

// projectJSON is the machine-readable shape of `project list`. Named in Portuguese
// because these are the domain's nouns and they are contractual everywhere else the
// operator meets them.
type projectJSON struct {
	Nome          string      `json:"nome"`
	Repositorio   string      `json:"repositorio"`
	Branch        string      `json:"branch"`
	LinearProject string      `json:"linear_project"`
	Fontes        []fonteJSON `json:"fontes"`
}

type fonteJSON struct {
	Slug   string `json:"slug"`
	Rotulo string `json:"rotulo"`
}

func runProjectList(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		projects, err := st.ListProjects(ctx)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()

		if asJSON {
			rows := make([]projectJSON, 0, len(projects))
			for _, p := range projects {
				fontes := make([]fonteJSON, 0, len(p.Sources))
				for _, src := range p.Sources {
					fontes = append(fontes, fonteJSON{Slug: src.SentrySlug, Rotulo: src.Label})
				}
				rows = append(rows, projectJSON{
					Nome:          p.Name,
					Repositorio:   p.Repo,
					Branch:        p.Branch,
					LinearProject: p.LinearProjectID,
					Fontes:        fontes,
				})
			}
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}

		if len(projects) == 0 {
			fmt.Fprintln(out, "nenhum Projeto registrado — `nightwatcher project add`")
			return nil
		}

		rows := make([][]string, 0, len(projects))
		for _, p := range projects {
			rows = append(rows, []string{
				p.Name, p.Repo, p.Branch, dash(p.LinearProjectID), fontesLine(p.Sources),
			})
		}
		table(out, []string{"PROJETO", "REPOSITÓRIO", "BRANCH", "LINEAR PROJECT", "FONTES"}, rows)
		return nil
	})
}

// mcpLine renders the servers for the confirmation table. The URL is enough: what
// would identify the credential is a variable name, and `mcp list` is where that
// belongs.
func mcpLine(servers []domain.MCPServer) string {
	parts := make([]string, 0, len(servers))
	for _, m := range servers {
		parts = append(parts, fmt.Sprintf("%s (%s)", m.Name, m.URL))
	}
	return strings.Join(parts, ", ")
}

// askMCPServers is the optional tail of the guided flow. Answering nothing is the
// normal path and the default: most Projetos are diagnosed from the repository alone,
// which is what every Análise did before this existed.
//
// Each server is proved before it is accepted, the same as a repositório and a Fonte —
// and with one more reason. A Fonte that does not exist stops the first webhook, loudly.
// A servidor MCP that does not answer stops nothing: the Análise runs, the Diagnóstico
// comes out, and it was written without the source the operator believed was there.
func askMCPServers(ctx context.Context, cmd *cobra.Command, p *prompter) ([]domain.MCPServer, error) {
	var out []domain.MCPServer
	for {
		label := "Servidor MCP: nome (vazio para nenhum)"
		if len(out) > 0 {
			label = "Outro servidor MCP: nome (vazio para terminar)"
		}
		name, err := p.ask(label, "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(name) == "" {
			return out, nil
		}

		m := domain.MCPServer{
			Name:      strings.ToLower(strings.TrimSpace(name)),
			Transport: domain.MCPTransportHTTP,
		}

		answer, err := p.ask("  transporte (http ou sse)", "http")
		if err != nil {
			return nil, err
		}
		if m.Transport, err = domain.ParseMCPTransport(answer); err != nil {
			return nil, err
		}

		url, err := p.askValid("  URL", "", func(v string) error {
			candidate := m
			candidate.URL = strings.TrimSpace(v)
			return candidate.Validate()
		})
		if err != nil {
			return nil, err
		}
		m.URL = strings.TrimSpace(url)

		desc, err := p.ask("  para que serve (vai no prompt da sessão)", "")
		if err != nil {
			return nil, err
		}
		m.Description = strings.TrimSpace(desc)

		headers, err := askMCPHeaders(p, m)
		if err != nil {
			return nil, err
		}
		m.Headers = headers

		if err := checkMCP(ctx, cmd, m, false); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
}

// askMCPHeaders collects the credential headers, asking for the VARIABLE and never for
// the value. A prompt that asked for the token would put it in the operator's shell
// history and one paste away from the database, which is the exact thing the schema
// is built to prevent.
func askMCPHeaders(p *prompter, m domain.MCPServer) ([]domain.MCPHeader, error) {
	var out []domain.MCPHeader
	for {
		label := "  header de credencial: nome (vazio para nenhum)"
		if len(out) > 0 {
			label = "  outro header: nome (vazio para terminar)"
		}
		name, err := p.ask(label, "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(name) == "" {
			return out, nil
		}
		h := domain.MCPHeader{Name: strings.TrimSpace(name)}

		prefix, err := p.ask("    prefixo fixo (ex.: 'Bearer ' — o segredo NÃO vai aqui)", "Bearer ")
		if err != nil {
			return nil, err
		}
		h.Prefix = domain.NormalizeMCPPrefix(prefix)

		env, err := p.askValid("    variável de ambiente que carrega o segredo", "", func(v string) error {
			candidate := h
			candidate.ValueEnv = strings.TrimSpace(v)
			return candidate.Validate(m.Name)
		})
		if err != nil {
			return nil, err
		}
		h.ValueEnv = strings.TrimSpace(env)
		out = append(out, h)
	}
}
