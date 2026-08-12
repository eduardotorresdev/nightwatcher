package agentcli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

func context7() domain.MCPServer {
	return domain.MCPServer{
		Name:        "context7",
		Transport:   domain.MCPTransportHTTP,
		URL:         "https://mcp.context7.com/mcp",
		Description: "documentação de biblioteca",
		Headers: []domain.MCPHeader{
			{Name: "Authorization", Prefix: "Bearer ", ValueEnv: "NW_MCP_CONTEXT7"},
		},
	}
}

func deepRequestWithMCP(servers ...domain.MCPServer) domain.AgentRequest {
	req := deepRequest()
	req.MCPServers = servers
	return req
}

func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// The whole point of the feature, and the flag combination that took a CLI --help to
// get right: --tools governs only the built-in set, so what authorises an MCP tool
// under `dontAsk` is --allowed-tools — the very flag that restricts nothing for a
// built-in one.
func TestMCPServersAreDeclaredAndAuthorised(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "mcp_declarado.jsonl")}
	a := New(Options{
		Runner: f, Token: "tok",
		Env: fakeEnv(map[string]string{"NW_MCP_CONTEXT7": "sk-segredo-do-context7"}),
	})

	if _, err := a.Run(context.Background(), deepRequestWithMCP(context7())); err != nil {
		t.Fatalf("run: %v", err)
	}
	args := f.spec.Args

	if v, ok := flagValue(args, "--allowed-tools"); !ok || v != "mcp__context7" {
		t.Errorf("--allowed-tools = %q (presente: %v); sem ele toda chamada ao servidor é negada em silêncio", v, ok)
	}
	// The deny rule outranks the allow, so it has to come off — and only it.
	deny, _ := flagValue(args, "--disallowed-tools")
	if strings.Contains(deny, "mcp__*") {
		t.Errorf("--disallowed-tools ainda contém mcp__*, que vence o --allowed-tools: %q", deny)
	}
	if !strings.Contains(deny, "Bash") || !strings.Contains(deny, "WebFetch") {
		t.Errorf("liberar MCP afrouxou o resto do deny: %q", deny)
	}
	// The clone's own .mcp.json still has no way in.
	if !hasArg(args, "--strict-mcp-config") {
		t.Error("--strict-mcp-config sumiu: o .mcp.json do repositório clonado voltaria a ser carregado")
	}
	// The built-in universe is untouched by any of this.
	if v, _ := flagValue(args, "--tools"); v != "Read,Grep,Glob" {
		t.Errorf("--tools = %q", v)
	}
}

// The credential travels as a variable reference and never as itself: argv is
// world-readable through /proc, and the docker command line is built from this.
func TestMCPCredentialNeverReachesTheArgv(t *testing.T) {
	const segredo = "sk-segredo-do-context7"
	f := &fakeRunner{stdout: fixture(t, "mcp_declarado.jsonl")}
	a := New(Options{
		Runner: f, Token: "tok",
		Env: fakeEnv(map[string]string{"NW_MCP_CONTEXT7": segredo}),
	})

	if _, err := a.Run(context.Background(), deepRequestWithMCP(context7())); err != nil {
		t.Fatalf("run: %v", err)
	}

	if strings.Contains(strings.Join(f.spec.Args, " "), segredo) {
		t.Fatal("o segredo do servidor MCP vazou para o argv")
	}

	raw, ok := flagValue(f.spec.Args, "--mcp-config")
	if !ok {
		t.Fatal("--mcp-config ausente")
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("--mcp-config não é JSON válido: %v\n%s", err, raw)
	}
	s, ok := cfg.MCPServers["context7"]
	if !ok {
		t.Fatalf("context7 não está no --mcp-config: %s", raw)
	}
	if s.Type != "http" || s.URL != "https://mcp.context7.com/mcp" {
		t.Errorf("servidor mal montado: %+v", s)
	}
	if got := s.Headers["Authorization"]; got != "Bearer ${NW_MCP_CONTEXT7}" {
		t.Errorf("header = %q; tem que ser a referência à variável, não o valor", got)
	}

	// And the value does travel — in the environment, which the DockerRunner passes
	// by name.
	if f.spec.Env["NW_MCP_CONTEXT7"] != segredo {
		t.Errorf("a variável não chegou ao ambiente da sessão: %q", f.spec.Env["NW_MCP_CONTEXT7"])
	}
}

// An unset variable becomes an empty header, and an empty header comes back as a 401
// that reads exactly like a revoked credential. It has to fail before the session, by
// name.
func TestMissingMCPCredentialFailsBeforeTheSession(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "mcp_declarado.jsonl")}
	a := New(Options{Runner: f, Token: "tok", Env: fakeEnv(nil)})

	_, err := a.Run(context.Background(), deepRequestWithMCP(context7()))
	if err == nil {
		t.Fatal("uma credencial ausente devia ter parado a sessão")
	}
	if got := domain.ClassifyAgentError(err); got != domain.AgentErrPerimeter {
		t.Errorf("classificação = %q", got)
	}
	if !strings.Contains(err.Error(), "NW_MCP_CONTEXT7") {
		t.Errorf("o erro não nomeia a variável: %v", err)
	}
	if f.calls != 0 {
		t.Error("a sessão chegou a rodar")
	}
}

// A session with no server of its own keeps the blanket deny, which is what makes an
// MCP arriving out of the cloned repository useless as well as fatal.
func TestSessionWithoutMCPKeepsTheBlanketDeny(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "sucesso.jsonl")}
	a := New(Options{Runner: f, Token: "tok"})

	if _, err := a.Run(context.Background(), deepRequest()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if v, _ := flagValue(f.spec.Args, "--disallowed-tools"); !strings.Contains(v, "mcp__*") {
		t.Errorf("--disallowed-tools = %q", v)
	}
	if hasArg(f.spec.Args, "--mcp-config") || hasArg(f.spec.Args, "--allowed-tools") {
		t.Error("uma sessão sem servidor não devia carregar --mcp-config nem --allowed-tools")
	}
}

// The perimeter used to be the number zero and is now a list. This is the assertion
// that the defence survived the change: the clone's .mcp.json is a server nobody
// declared, and it kills the session at the init event, before the untrusted text is
// ever answered.
func TestUndeclaredMCPServerIsStillAPerimeterViolation(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "mcp_estranho.jsonl")}
	a := New(Options{
		Runner: f, Token: "tok",
		Env: fakeEnv(map[string]string{"NW_MCP_CONTEXT7": "sk-segredo"}),
	})

	_, err := a.Run(context.Background(), deepRequestWithMCP(context7()))
	if got := domain.ClassifyAgentError(err); got != domain.AgentErrPerimeter {
		t.Fatalf("classificação = %q, erro = %v", got, err)
	}
	if !strings.Contains(err.Error(), "loja") {
		t.Errorf("o erro não nomeia o servidor intruso: %v", err)
	}
	if strings.Contains(err.Error(), `"context7"`) {
		t.Errorf("o servidor declarado foi acusado junto: %v", err)
	}
	// The abort has to stop the reading, not merely notice afterwards.
	if f.fed != 1 {
		t.Errorf("linhas lidas = %d; a sessão devia ter parado no init", f.fed)
	}
}

// The same asymmetry that governs tools governs servers: what we asked for and did not
// get is less capability and never a violation. It leaves the package so the caller
// can say so, because an answer written without it looks identical to one written
// with it.
func TestDeclaredMCPServerThatDidNotComeUpIsReportedAndNotFatal(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "mcp_ausente.jsonl")}
	a := New(Options{
		Runner: f, Token: "tok",
		Env: fakeEnv(map[string]string{"NW_MCP_CONTEXT7": "sk-segredo"}),
	})

	res, err := a.Run(context.Background(), deepRequestWithMCP(context7()))
	if err != nil {
		t.Fatalf("um servidor que não subiu não é violação: %v", err)
	}
	if len(res.MCPUnavailable) != 1 || res.MCPUnavailable[0] != "context7" {
		t.Errorf("MCPUnavailable = %v", res.MCPUnavailable)
	}
}

// A Triagem has no repository and nothing to ask a server about. Both emptinesses are
// the contract.
func TestTriageNeverCarriesAnMCPServer(t *testing.T) {
	f := &fakeRunner{stdout: fixture(t, "triagem.jsonl")}
	a := New(Options{Runner: f, Token: "tok"})

	req := domain.AgentRequest{
		Kind:    domain.SessionTriage,
		Prompt:  "triagem",
		Tools:   []string{},
		Timeout: time.Minute,
	}
	if _, err := a.Run(context.Background(), req); err != nil {
		t.Fatalf("run: %v", err)
	}
	if hasArg(f.spec.Args, "--mcp-config") {
		t.Error("a Triagem carregou --mcp-config")
	}
}

// stdio has no constant in the domain and no way through the parser: a transport that
// cannot be spelled cannot be configured by accident.
func TestStdioIsRefusedWithItsReason(t *testing.T) {
	_, err := domain.ParseMCPTransport("stdio")
	if err == nil {
		t.Fatal("stdio devia ter sido recusado")
	}
	if !strings.Contains(err.Error(), "só lê") {
		t.Errorf("o erro não diz por que: %v", err)
	}
}

// The Projeto's server wins over the global one of the same name, and the order is
// stable so the invocation, the prompt and the perimeter all agree.
func TestProjectServerOverridesTheGlobalOfTheSameName(t *testing.T) {
	global := []domain.MCPServer{
		{Name: "context7", URL: "https://global.exemplo/mcp", Transport: domain.MCPTransportHTTP},
		{Name: "atlas", URL: "https://atlas.exemplo/mcp", Transport: domain.MCPTransportHTTP},
	}
	own := []domain.MCPServer{
		{Name: "context7", URL: "https://do-projeto.exemplo/mcp", Transport: domain.MCPTransportHTTP},
	}

	got := domain.MergeMCPServers(global, own)
	if len(got) != 2 {
		t.Fatalf("servidores = %d, queria 2: %+v", len(got), got)
	}
	if got[0].Name != "atlas" || got[1].Name != "context7" {
		t.Errorf("ordem instável: %s, %s", got[0].Name, got[1].Name)
	}
	if got[1].URL != "https://do-projeto.exemplo/mcp" {
		t.Errorf("o global venceu o do Projeto: %s", got[1].URL)
	}
}

// A prefix that is all letters is an authentication scheme and needs its space back:
// the guided flow trims every answer it reads, and `Bearersk-…` on the wire is a 401
// that reads exactly like a revoked token.
func TestSchemePrefixKeepsItsSpace(t *testing.T) {
	cases := map[string]string{
		"Bearer":  "Bearer ",
		"Bearer ": "Bearer ",
		"Basic":   "Basic ",
		"token=":  "token=",
		"":        "",
		"   ":     "",
	}
	for in, want := range cases {
		if got := domain.NormalizeMCPPrefix(in); got != want {
			t.Errorf("NormalizeMCPPrefix(%q) = %q, queria %q", in, got, want)
		}
	}
}
