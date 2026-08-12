package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/agentcli"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

const (
	flagMCPNome        = "nome"
	flagMCPURL         = "url"
	flagMCPTransport   = "transport"
	flagMCPProjeto     = "projeto"
	flagMCPHeader      = "header"
	flagMCPDescricao   = "descricao"
	flagMCPSemRede     = "sem-teste-de-rede"
	flagMCPProbeHeader = "header-valor"
)

// headerHelp is one string because it is printed in three places — the flag, the
// parse error and the guided question — and three copies drift.
const headerHelp = `header no formato Nome:Prefixo:VARIAVEL (repetível). ` +
	`A VARIAVEL é o NOME de uma variável de ambiente, nunca o valor: o segredo nunca entra no banco. ` +
	`Ex.: 'Authorization:Bearer :NW_MCP_CONTEXT7'. Sem prefixo, deixe o meio vazio: 'X-Api-Key::NW_MCP_X'`

func newMCPCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Servidores MCP remotos que o Diagnóstico e o Estudo podem consultar",
		Long: "Servidores MCP que a sessão longa consulta além do repositório.\n\n" +
			"Só transporte remoto (http e sse). stdio não é suportado e não vai ser: ele seria\n" +
			"um processo executando dentro do container da Análise, e o nightwatcher só lê.\n\n" +
			"Sem --projeto, o servidor é global e vale para todos os Projetos. Com --projeto,\n" +
			"vale só para ele — e um servidor do Projeto com o mesmo nome de um global\n" +
			"substitui o global naquele Projeto.\n\n" +
			"A credencial nunca é gravada. O banco guarda o NOME da variável de ambiente; o\n" +
			"valor vem do ambiente do daemon e entra no container da Análise por nome.",
	}

	add := &cobra.Command{
		Use:   "add",
		Short: "Registra um servidor MCP, testando a conectividade antes de gravar",
		Long: "Registra um servidor MCP.\n\n" +
			"Nada é gravado antes de o servidor responder. O teste roda duas vezes: uma daqui,\n" +
			"que prova a URL e a credencial, e outra de DENTRO da rede em que a Análise roda,\n" +
			"que é a única que responde a pergunta que importa. As duas redes são diferentes\n" +
			"de propósito — a da Análise tem o egress restrito — então um teste que passa aqui\n" +
			"não prova nada sobre lá.",
		Args: cobra.NoArgs,
		RunE: runMCPAdd,
	}
	add.Flags().String(flagMCPNome, "", "nome do servidor (vira o prefixo mcp__<nome>__ das ferramentas)")
	add.Flags().String(flagMCPURL, "", "URL do servidor MCP (https)")
	add.Flags().String(flagMCPTransport, "http", "transporte remoto: http ou sse")
	add.Flags().String(flagMCPProjeto, "", "Projeto ao qual o servidor pertence (vazio = global)")
	add.Flags().String(flagMCPDescricao, "", "para que o servidor serve, em uma linha — vai para o prompt da sessão")
	add.Flags().StringArray(flagMCPHeader, nil, headerHelp)
	// Escape hatch and not a default: the network probe is the one that answers the
	// question that matters, so skipping it has to be a decision somebody typed.
	add.Flags().Bool(flagMCPSemRede, false,
		"grava mesmo sem conseguir testar de dentro da rede da Análise")

	list := &cobra.Command{
		Use:   "list",
		Short: "Lista os servidores MCP, globais e por Projeto",
		Args:  cobra.NoArgs,
		RunE:  runMCPList,
	}
	list.Flags().String(flagMCPProjeto, "", "mostra só os deste Projeto (mais os globais)")
	list.Flags().Bool("json", false, "saída em JSON")

	rm := &cobra.Command{
		Use:   "rm",
		Short: "Remove um servidor MCP",
		Args:  cobra.NoArgs,
		RunE:  runMCPRemove,
	}
	rm.Flags().String(flagMCPNome, "", "nome do servidor")
	rm.Flags().String(flagMCPProjeto, "", "Projeto ao qual o servidor pertence (vazio = global)")

	// The half of `add` that runs inside the Análise's container. Hidden because
	// nobody types it: `add` invokes it through docker, and it is a subcommand rather
	// than a flag because the image's entrypoint is this binary.
	probe := &cobra.Command{
		Use:    "probe",
		Short:  "Testa um servidor MCP a partir de onde este processo está",
		Args:   cobra.NoArgs,
		RunE:   runMCPProbe,
		Hidden: true,
	}
	probe.Flags().String(flagMCPNome, "probe", "nome do servidor")
	probe.Flags().String(flagMCPURL, "", "URL do servidor MCP")
	probe.Flags().String(flagMCPTransport, "http", "transporte remoto: http ou sse")
	probe.Flags().StringArray(flagMCPHeader, nil, headerHelp)

	c.AddCommand(add, list, rm, probe)
	return c
}

// parseMCPHeader reads `Nome:Prefixo:VARIAVEL`.
//
// Three fields always, with the middle one allowed to be empty, and no defaulting of
// any of them — the same choice `parseFonte` makes about the rótulo. Two fields would
// have to guess which of prefix and variable was omitted, and guessing wrong here
// means storing a credential.
func parseMCPHeader(s string) (domain.MCPHeader, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return domain.MCPHeader{}, fmt.Errorf("header %q fora do formato Nome:Prefixo:VARIAVEL; %s", s, headerHelp)
	}
	h := domain.MCPHeader{
		Name: strings.TrimSpace(parts[0]),
		// NOT trimmed, and then normalised: "Bearer " ends in a space that belongs to
		// the value, and losing it produces `Bearersk-…` on the wire.
		Prefix:   domain.NormalizeMCPPrefix(parts[1]),
		ValueEnv: strings.TrimSpace(parts[2]),
	}
	return h, nil
}

func parseMCPHeaders(raw []string) ([]domain.MCPHeader, error) {
	out := make([]domain.MCPHeader, 0, len(raw))
	for _, s := range raw {
		h, err := parseMCPHeader(s)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// mcpFromFlags assembles the server from the command line, validating it before the
// database is opened so a refused invocation writes nothing at all.
func mcpFromFlags(cmd *cobra.Command) (domain.MCPServer, error) {
	f := cmd.Flags()
	name, _ := f.GetString(flagMCPNome)
	rawURL, _ := f.GetString(flagMCPURL)
	rawTransport, _ := f.GetString(flagMCPTransport)
	desc, _ := f.GetString(flagMCPDescricao)
	rawHeaders, _ := f.GetStringArray(flagMCPHeader)

	transport, err := domain.ParseMCPTransport(rawTransport)
	if err != nil {
		return domain.MCPServer{}, err
	}
	headers, err := parseMCPHeaders(rawHeaders)
	if err != nil {
		return domain.MCPServer{}, err
	}
	m := domain.MCPServer{
		Name:        strings.ToLower(strings.TrimSpace(name)),
		Transport:   transport,
		URL:         strings.TrimSpace(rawURL),
		Description: strings.TrimSpace(desc),
		Headers:     headers,
	}
	if m.Name == "" || m.URL == "" {
		return domain.MCPServer{}, errors.New("faltam --nome e --url")
	}
	return m, m.Validate()
}

func runMCPAdd(cmd *cobra.Command, _ []string) error {
	m, err := mcpFromFlags(cmd)
	if err != nil {
		return err
	}
	projeto, _ := cmd.Flags().GetString(flagMCPProjeto)
	projeto = strings.TrimSpace(projeto)
	semRede, _ := cmd.Flags().GetBool(flagMCPSemRede)

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		out := cmd.OutOrStdout()

		if projeto != "" {
			p, err := st.ProjectByName(ctx, projeto)
			if err != nil {
				return fmt.Errorf("projeto %s: %w", projeto, err)
			}
			m.ProjectID = p.ID
		}

		if err := checkMCP(ctx, cmd, m, semRede); err != nil {
			return err
		}

		if _, err := st.UpsertMCPServer(ctx, m, clock.Now()); err != nil {
			return err
		}
		escopo := "global"
		if projeto != "" {
			escopo = "do Projeto " + projeto
		}
		fmt.Fprintf(out, "servidor MCP %s registrado (%s); as ferramentas aparecem como %s__*\n",
			m.Name, escopo, m.ToolPrefix())
		return nil
	})
}

// checkMCP is the live validation. It runs before anything is written, and prints what
// it proved as it goes — the same shape as the guided `project add`.
func checkMCP(ctx context.Context, cmd *cobra.Command, m domain.MCPServer, semRede bool) error {
	out := cmd.OutOrStdout()

	client := &http.Client{Timeout: mcpProbeTimeout}
	res, err := probeMCP(ctx, client, m, os.Getenv)
	if err != nil {
		return explainProbe(m, err, false)
	}
	quem := res.ServerName
	if quem == "" {
		quem = m.URL
	}
	if res.ServerVersion != "" {
		quem += " " + res.ServerVersion
	}
	fmt.Fprintf(out, "✓ %s responde e aceita a credencial (daqui)\n", quem)

	// The probe that matters. Everything above proves the URL and the token; only this
	// proves the network the Análise actually runs on.
	switch err := probeFromAgentNetwork(ctx, m); {
	case err == nil:
		fmt.Fprintf(out, "✓ alcançável de dentro da rede %s, onde a Análise roda\n",
			agentcli.DefaultDockerOptions().Network)
	case errors.As(err, new(*dockerUnavailableError)):
		// Running locally, or a daemon that is not there. Never a silent success: the
		// operator has to know which half was not tested.
		if !semRede {
			fmt.Fprintf(out, "⚠ conectividade confirmada daqui, mas a rede da Análise NÃO foi testada: %v\n", err)
			fmt.Fprintf(out, "  rode este mesmo comando na instância (dokku run) para conferir, ou passe --%s\n", flagMCPSemRede)
		}
	default:
		if semRede {
			fmt.Fprintf(out, "⚠ %v\n", explainProbe(m, err, true))
			fmt.Fprintf(out, "  gravando mesmo assim porque --%s foi passado\n", flagMCPSemRede)
			return nil
		}
		return explainProbe(m, err, true)
	}
	return nil
}

// explainProbe turns a failure into the thing to go do about it. The classification
// matters more than the message: a wrong token, a closed network and a server that
// only speaks OAuth are three different afternoons.
func explainProbe(m domain.MCPServer, err error, fromAgentNetwork bool) error {
	rede := agentcli.DefaultDockerOptions().Network

	var envErr *mcpEnvError
	if errors.As(err, &envErr) {
		return fmt.Errorf("servidor MCP %s: %w\n  defina a(s) variável(is) no ambiente do daemon: dokku config:set %s=…",
			m.Name, envErr, strings.Join(envErr.Vars, "=… "))
	}

	var oauthErr *mcpOAuthError
	if errors.As(err, &oauthErr) {
		return fmt.Errorf("servidor MCP %s: %w\n"+
			"  o nightwatcher só suporta autenticação por header, porque o fluxo OAuth pede um\n"+
			"  navegador e um callback que uma sessão headless dentro de um container efêmero\n"+
			"  não tem. Se o serviço emitir uma API key ou um token de longa duração, use-o em\n"+
			"  --%s; se não emitir, este servidor não dá para usar aqui",
			m.Name, oauthErr, flagMCPHeader)
	}

	switch statusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("servidor MCP %s: a credencial não foi aceita (%w)\n  confira o valor de %s no ambiente",
			m.Name, err, strings.Join(envNames(m), ", "))
	case http.StatusNotFound:
		return fmt.Errorf("servidor MCP %s: %w\n  a URL existe mas não é o endpoint MCP; confira o caminho", m.Name, err)
	}

	if fromAgentNetwork {
		return fmt.Errorf("servidor MCP %s: %s não é alcançável de dentro da rede %s, que é a rede em que a Análise roda (%w)\n"+
			"  o teste direto passou, então a URL e a credencial estão certas — o que falta é egress.\n"+
			"  Libere %s para essa rede e rode de novo, ou passe --%s para gravar assim mesmo\n"+
			"  (o servidor vai aparecer no prompt e nenhuma chamada vai funcionar).",
			m.Name, m.Host(), rede, err, m.Host(), flagMCPSemRede)
	}
	return fmt.Errorf("servidor MCP %s: %s não respondeu (%w)", m.Name, m.Host(), err)
}

func envNames(m domain.MCPServer) []string {
	out := make([]string, 0, len(m.Headers))
	for _, h := range m.Headers {
		out = append(out, h.ValueEnv)
	}
	return out
}

// dockerUnavailableError separates "the Análise cannot reach this host" from "I could
// not ask". They lead to opposite actions, and conflating them would either block a
// perfectly good configuration on a developer's laptop or approve one on a VPS whose
// network never allowed it.
type dockerUnavailableError struct{ err error }

func (e *dockerUnavailableError) Error() string { return e.err.Error() }
func (e *dockerUnavailableError) Unwrap() error { return e.err }

// probeAgentNetwork is a package variable for the same reason newWorld is: a test
// cannot raise a container, and what this selects is the difference between a
// validated configuration and a `docker: command not found`.
var probeFromAgentNetwork = func(ctx context.Context, m domain.MCPServer) error {
	opts := agentcli.DefaultDockerOptions()

	args := []string{"mcp", "probe",
		"--" + flagMCPNome, m.Name,
		"--" + flagMCPURL, m.URL,
		"--" + flagMCPTransport, string(m.Transport),
	}
	var envNames []string
	for _, h := range m.Headers {
		// Rebuilt from the same three fields the daemon will use, so what is proved is
		// what will be sent. The VALUE goes by name — see SidecarCommand.
		args = append(args, "--"+flagMCPHeader, h.Name+":"+h.Prefix+":"+h.ValueEnv)
		envNames = append(envNames, h.ValueEnv)
	}
	sort.Strings(envNames)

	cmdArgs, clientEnv := opts.SidecarCommand(agentcli.NightwatcherBinary(), args, envNames)

	// A ceiling well above the probe's own: what is being waited on is a container
	// starting, and a docker daemon under load is slow, not broken.
	ctx, cancel := context.WithTimeout(ctx, mcpProbeTimeout+40*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, opts.Docker, cmdArgs...)
	c.Env = sidecarEnv(clientEnv, envNames)
	outBytes, err := c.CombinedOutput()
	if err == nil {
		return nil
	}
	saida := strings.TrimSpace(string(outBytes))
	// Nothing ran: no docker binary, no daemon, no image. That is not an answer about
	// the network.
	var notFound *exec.Error
	if errors.As(err, &notFound) || looksLikeNoDocker(saida) {
		return &dockerUnavailableError{err: fmt.Errorf("não consegui subir um container para testar: %s", firstLine(saida, err))}
	}
	return fmt.Errorf("%s", firstLine(saida, err))
}

// sidecarEnv is the environment of the docker CLIENT: what it needs to reach its own
// daemon, plus the values of the variables passed by name.
func sidecarEnv(clientEnv map[string]string, envNames []string) []string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	for k, v := range clientEnv {
		env[k] = v
	}
	for _, name := range envNames {
		env[name] = os.Getenv(name)
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func looksLikeNoDocker(s string) bool {
	l := strings.ToLower(s)
	for _, hint := range []string{
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"permission denied while trying to connect",
		"unable to find image",
		"no such image",
		"executable file not found",
	} {
		if strings.Contains(l, hint) {
			return true
		}
	}
	return false
}

func firstLine(out string, err error) string {
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return err.Error()
}

// runMCPProbe is what runs inside the container. It prints nothing on success and
// exits non-zero with the reason on failure, because its caller reads exactly that.
func runMCPProbe(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	name, _ := f.GetString(flagMCPNome)
	rawURL, _ := f.GetString(flagMCPURL)
	rawTransport, _ := f.GetString(flagMCPTransport)
	rawHeaders, _ := f.GetStringArray(flagMCPHeader)

	transport, err := domain.ParseMCPTransport(rawTransport)
	if err != nil {
		return err
	}
	headers, err := parseMCPHeaders(rawHeaders)
	if err != nil {
		return err
	}
	m := domain.MCPServer{
		Name:      strings.ToLower(strings.TrimSpace(name)),
		Transport: transport,
		URL:       strings.TrimSpace(rawURL),
		Headers:   headers,
	}
	if err := m.Validate(); err != nil {
		return err
	}
	_, err = probeMCP(cmd.Context(), &http.Client{Timeout: mcpProbeTimeout}, m, os.Getenv)
	return err
}

// ---------------------------------------------------------------- list e rm

type mcpJSON struct {
	Nome      string       `json:"nome"`
	Escopo    string       `json:"escopo"`
	Projeto   string       `json:"projeto,omitempty"`
	Transport string       `json:"transport"`
	URL       string       `json:"url"`
	Descricao string       `json:"descricao,omitempty"`
	Headers   []headerJSON `json:"headers,omitempty"`
}

type headerJSON struct {
	Nome     string `json:"nome"`
	Prefixo  string `json:"prefixo,omitempty"`
	Variavel string `json:"variavel"`
	Definida bool   `json:"definida"`
}

func runMCPList(cmd *cobra.Command, _ []string) error {
	projeto, _ := cmd.Flags().GetString(flagMCPProjeto)
	projeto = strings.TrimSpace(projeto)
	asJSON, _ := cmd.Flags().GetBool("json")

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		out := cmd.OutOrStdout()

		global, err := st.GlobalMCPServers(ctx)
		if err != nil {
			return err
		}
		rows := make([]mcpJSON, 0, len(global))
		for _, m := range global {
			rows = append(rows, mcpRow(m, "global", ""))
		}

		projetos, err := st.ListProjects(ctx)
		if err != nil {
			return err
		}
		for _, p := range projetos {
			if projeto != "" && p.Name != projeto {
				continue
			}
			for _, m := range p.MCPServers {
				rows = append(rows, mcpRow(m, "projeto", p.Name))
			}
		}

		if asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "nenhum servidor MCP registrado")
			return nil
		}

		body := make([][]string, 0, len(rows))
		for _, r := range rows {
			escopo := r.Escopo
			if r.Projeto != "" {
				escopo = r.Projeto
			}
			body = append(body, []string{r.Nome, escopo, r.Transport, r.URL, credentialCell(r.Headers)})
		}
		table(out, []string{"SERVIDOR", "ESCOPO", "TRANSPORTE", "URL", "CREDENCIAL"}, body)
		return nil
	})
}

// credentialCell says which variable carries the credential and whether it is set
// HERE. A missing one is the failure the Análise would hit, reported where somebody
// is looking.
func credentialCell(headers []headerJSON) string {
	if len(headers) == 0 {
		return "(nenhuma)"
	}
	parts := make([]string, 0, len(headers))
	for _, h := range headers {
		mark := "✗ não definida"
		if h.Definida {
			mark = "✓"
		}
		parts = append(parts, h.Variavel+" "+mark)
	}
	return strings.Join(parts, ", ")
}

func mcpRow(m domain.MCPServer, escopo, projeto string) mcpJSON {
	row := mcpJSON{
		Nome:      m.Name,
		Escopo:    escopo,
		Projeto:   projeto,
		Transport: string(m.Transport),
		URL:       m.URL,
		Descricao: m.Description,
	}
	for _, h := range m.Headers {
		row.Headers = append(row.Headers, headerJSON{
			Nome:    h.Name,
			Prefixo: h.Prefix,
			// The NAME, and never a prefix of the value: a prefix of a token is still a
			// piece of a token, which is the same rule `config validate` follows.
			Variavel: h.ValueEnv,
			Definida: strings.TrimSpace(os.Getenv(h.ValueEnv)) != "",
		})
	}
	return row
}

func runMCPRemove(cmd *cobra.Command, _ []string) error {
	name, _ := cmd.Flags().GetString(flagMCPNome)
	name = strings.ToLower(strings.TrimSpace(name))
	projeto, _ := cmd.Flags().GetString(flagMCPProjeto)
	projeto = strings.TrimSpace(projeto)
	if name == "" {
		return errors.New("falta --" + flagMCPNome)
	}

	return withStore(cmd, func(ctx context.Context, st *store.Store) error {
		var projectID int64
		if projeto != "" {
			p, err := st.ProjectByName(ctx, projeto)
			if err != nil {
				return fmt.Errorf("projeto %s: %w", projeto, err)
			}
			projectID = p.ID
		}
		if err := st.RemoveMCPServer(ctx, projectID, name); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "servidor MCP %s removido\n", name)
		return nil
	})
}
