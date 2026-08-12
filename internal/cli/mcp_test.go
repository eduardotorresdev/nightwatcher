package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// fakeMCP is a server that speaks the initialize handshake, and only to a caller
// carrying the expected credential.
func fakeMCP(t *testing.T, wantAuth string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"credencial inválida"}`))
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "initialize" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18",` +
			`"capabilities":{},"serverInfo":{"name":"exemplo-mcp","version":"1.4.0"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// noAgentNetwork stands in for the container probe. A test cannot raise one, and what
// this selects is the difference between a validated configuration and a
// `docker: command not found`.
func noAgentNetwork(t *testing.T, err error) {
	t.Helper()
	original := probeFromAgentNetwork
	probeFromAgentNetwork = func(context.Context, domain.MCPServer) error { return err }
	t.Cleanup(func() { probeFromAgentNetwork = original })
}

func TestMCPAddProvesTheServerBeforeWritingIt(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "Bearer sk-certo")
	t.Setenv("NW_MCP_EXEMPLO", "sk-certo")
	noAgentNetwork(t, nil)

	out, err := runCLI(t, "", "--db", db, "mcp", "add",
		"--nome", "exemplo", "--url", srv.URL,
		"--header", "Authorization:Bearer :NW_MCP_EXEMPLO",
		"--descricao", "documentação de biblioteca")
	if err != nil {
		t.Fatalf("mcp add: %v\n%s", err, out)
	}
	mustContain(t, out, "exemplo-mcp 1.4.0")
	mustContain(t, out, "mcp__exemplo__*")
	mustContain(t, out, "onde a Análise roda")
	// Presence is printed; the value never is.
	mustNotContain(t, out, "sk-certo")

	st := openStore(t, db)
	servidores, err := st.GlobalMCPServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servidores) != 1 {
		t.Fatalf("servidores gravados = %d", len(servidores))
	}
	m := servidores[0]
	if m.Name != "exemplo" || m.URL != srv.URL || m.Description != "documentação de biblioteca" {
		t.Errorf("servidor gravado = %+v", m)
	}
	if len(m.Headers) != 1 || m.Headers[0].Prefix != "Bearer " || m.Headers[0].ValueEnv != "NW_MCP_EXEMPLO" {
		t.Errorf("headers = %+v; o prefixo tem que manter o espaço", m.Headers)
	}
}

// A refused credential writes nothing. `mcp add` is the moment somebody is watching,
// and a server that does not answer breaks nothing visibly later: the Análise runs,
// the Diagnóstico comes out, and nobody learns it was written without the source.
func TestMCPAddWritesNothingWhenTheCredentialIsRefused(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "Bearer sk-certo")
	t.Setenv("NW_MCP_EXEMPLO", "sk-errado")
	noAgentNetwork(t, nil)

	out, err := runCLI(t, "", "--db", db, "mcp", "add",
		"--nome", "exemplo", "--url", srv.URL,
		"--header", "Authorization:Bearer :NW_MCP_EXEMPLO")
	if err == nil {
		t.Fatalf("uma credencial recusada devia ter parado o comando:\n%s", out)
	}
	mustContain(t, err.Error(), "NW_MCP_EXEMPLO")

	servidores, serr := openStore(t, db).GlobalMCPServers(context.Background())
	if serr != nil {
		t.Fatal(serr)
	}
	if len(servidores) != 0 {
		t.Errorf("gravou %d servidor(es) apesar da falha", len(servidores))
	}
}

// An unset variable becomes an empty header and comes back as a 401 that reads exactly
// like a revoked credential, so it is caught before the request and named.
func TestMCPAddNamesTheMissingEnvironmentVariable(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "")
	noAgentNetwork(t, nil)

	_, err := runCLI(t, "", "--db", db, "mcp", "add",
		"--nome", "exemplo", "--url", srv.URL,
		"--header", "Authorization:Bearer :NW_MCP_NAO_DEFINIDA")
	if err == nil {
		t.Fatal("uma variável ausente devia ter parado o comando")
	}
	mustContain(t, err.Error(), "NW_MCP_NAO_DEFINIDA")
	mustContain(t, err.Error(), "dokku config:set")
}

// The failure that matters most, because it is the one that passes every check a
// person would think to run: the URL is right, the token is right, and the network the
// Análise runs on cannot reach the host.
func TestMCPAddRefusesAServerTheAnalysisCannotReach(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "")
	noAgentNetwork(t, errors.New("dial tcp: i/o timeout"))

	out, err := runCLI(t, "", "--db", db, "mcp", "add", "--nome", "exemplo", "--url", srv.URL)
	if err == nil {
		t.Fatalf("um servidor inalcançável da rede da Análise devia ter parado o comando:\n%s", out)
	}
	msg := err.Error()
	for _, want := range []string{"nightwatcher-agent", "egress", "o teste direto passou"} {
		if !strings.Contains(msg, want) {
			t.Errorf("o erro não explica o que fazer (falta %q):\n%s", want, msg)
		}
	}

	servidores, serr := openStore(t, db).GlobalMCPServers(context.Background())
	if serr != nil {
		t.Fatal(serr)
	}
	if len(servidores) != 0 {
		t.Errorf("gravou %d servidor(es) apesar da rede fechada", len(servidores))
	}
}

// Not being able to ASK is not the same as the answer being no. On a laptop with no
// docker the configuration is still written — but never in silence.
func TestMCPAddWarnsInsteadOfFailingWhenDockerIsNotThere(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "")
	noAgentNetwork(t, &dockerUnavailableError{err: errors.New("Cannot connect to the Docker daemon")})

	out, err := runCLI(t, "", "--db", db, "mcp", "add", "--nome", "exemplo", "--url", srv.URL)
	if err != nil {
		t.Fatalf("sem docker o comando devia gravar mesmo assim: %v\n%s", err, out)
	}
	mustContain(t, out, "NÃO foi testada")

	servidores, serr := openStore(t, db).GlobalMCPServers(context.Background())
	if serr != nil {
		t.Fatal(serr)
	}
	if len(servidores) != 1 {
		t.Errorf("servidores gravados = %d", len(servidores))
	}
}

// The one failure where retrying, fixing the token and opening the firewall are all
// wrong answers.
func TestMCPAddSaysWhenTheServerOnlySpeaksOAuth(t *testing.T) {
	db := tempDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer resource_metadata="https://exemplo/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	noAgentNetwork(t, nil)

	_, err := runCLI(t, "", "--db", db, "mcp", "add", "--nome", "exemplo", "--url", srv.URL)
	if err == nil {
		t.Fatal("um servidor só-OAuth devia ter parado o comando")
	}
	mustContain(t, err.Error(), "OAuth")
	mustContain(t, err.Error(), "não dá para usar aqui")
}

// stdio is not a transport this system can have, and the refusal says why rather than
// "desconhecido".
func TestMCPAddRefusesStdioWithItsReason(t *testing.T) {
	_, err := runCLI(t, "", "--db", tempDB(t), "mcp", "add",
		"--nome", "local", "--url", "https://exemplo/mcp", "--transport", "stdio")
	if err == nil {
		t.Fatal("stdio devia ter sido recusado")
	}
	mustContain(t, err.Error(), "só lê")
}

// The prefix is stored and the secret is not, so a credential pasted into the wrong
// half of the flag has to be refused — that one would land in SQLite forever.
func TestMCPAddRefusesACredentialPastedIntoThePrefix(t *testing.T) {
	for _, prefixo := range []string{
		// Long: nobody writes a 29-character authentication scheme.
		"Bearer sk-ant-vazou-tudo-aqui",
		// Short but self-incriminating, caught by the same word list `config set` uses.
		"Token abc12",
	} {
		_, err := runCLI(t, "", "--db", tempDB(t), "mcp", "add",
			"--nome", "exemplo", "--url", "https://exemplo/mcp",
			"--header", "Authorization:"+prefixo+":NW_MCP_X")
		if err == nil {
			t.Errorf("um segredo no prefixo devia ter sido recusado: %q", prefixo)
			continue
		}
		mustContain(t, err.Error(), "NW_MCP_X")
	}
}

func TestMCPListShowsScopeAndWhetherTheCredentialIsSet(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "")
	noAgentNetwork(t, nil)
	t.Setenv("NW_MCP_DEFINIDA", "sk-presente")

	if _, err := runCLI(t, "", "--db", db, "mcp", "add", "--nome", "comcred", "--url", srv.URL,
		"--header", "Authorization:Bearer :NW_MCP_DEFINIDA"); err != nil {
		t.Fatal(err)
	}
	// Written straight to the store, because `mcp add` would refuse it — which is the
	// point. This is the state that really happens: the variable was there when the
	// server was registered and later left the daemon's environment, and nothing
	// re-checks it until the first Diagnóstico. `mcp list` is where it becomes visible.
	if _, err := openStore(t, db).UpsertMCPServer(context.Background(), domain.MCPServer{
		Name:      "semcred",
		Transport: domain.MCPTransportHTTP,
		URL:       srv.URL,
		Headers:   []domain.MCPHeader{{Name: "X-Api-Key", ValueEnv: "NW_MCP_AUSENTE"}},
	}, fixed); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", "--db", db, "mcp", "list")
	if err != nil {
		t.Fatalf("mcp list: %v\n%s", err, out)
	}
	mustContain(t, out, "NW_MCP_DEFINIDA ✓")
	mustContain(t, out, "NW_MCP_AUSENTE ✗")
	mustContain(t, out, "global")
	// The name of the variable, never a piece of its value — a prefix of a token is
	// still a piece of a token.
	mustNotContain(t, out, "sk-presente")
}

func TestMCPRemoveDropsTheServer(t *testing.T) {
	db := tempDB(t)
	srv := fakeMCP(t, "")
	noAgentNetwork(t, nil)

	if _, err := runCLI(t, "", "--db", db, "mcp", "add", "--nome", "exemplo", "--url", srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "", "--db", db, "mcp", "rm", "--nome", "exemplo"); err != nil {
		t.Fatal(err)
	}

	servidores, err := openStore(t, db).GlobalMCPServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servidores) != 0 {
		t.Errorf("sobraram %d servidor(es)", len(servidores))
	}
}

// Three fields always, with the middle allowed to be empty. Two would have to guess
// which of prefix and variable was omitted, and guessing wrong stores a credential.
func TestHeaderFlagFormat(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		want    domain.MCPHeader
	}{
		{in: "Authorization:Bearer :NW_X", want: domain.MCPHeader{Name: "Authorization", Prefix: "Bearer ", ValueEnv: "NW_X"}},
		// The space the shell or the prompter ate comes back.
		{in: "Authorization:Bearer:NW_X", want: domain.MCPHeader{Name: "Authorization", Prefix: "Bearer ", ValueEnv: "NW_X"}},
		{in: "X-Api-Key::NW_X", want: domain.MCPHeader{Name: "X-Api-Key", ValueEnv: "NW_X"}},
		{in: "X-Api-Key:NW_X", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseMCPHeader(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMCPHeader(%q) devia ter falhado", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMCPHeader(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseMCPHeader(%q) = %+v, queria %+v", tc.in, got, tc.want)
		}
	}
}
