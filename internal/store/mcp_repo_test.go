package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

func mcpFixture(name, url string) domain.MCPServer {
	return domain.MCPServer{
		Name:        name,
		Transport:   domain.MCPTransportHTTP,
		URL:         url,
		Description: "documentação",
		Headers: []domain.MCPHeader{
			{Name: "Authorization", Prefix: "Bearer ", ValueEnv: "NW_MCP_" + strings.ToUpper(name)},
		},
	}
}

// The two partial indexes are not tidiness: SQLite does not treat NULL as equal to
// NULL, so a single UNIQUE (project_id, name) would let two globals of the same name
// in — and then two configurations would fight over the same mcp__<name>__ prefix in
// the same session.
func TestGlobalAndProjectServersOfTheSameNameCoexist(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-web")

	global := mcpFixture("context7", "https://global.exemplo/mcp")
	if _, err := s.UpsertMCPServer(ctx, global, t0); err != nil {
		t.Fatalf("global: %v", err)
	}
	own := mcpFixture("context7", "https://do-projeto.exemplo/mcp")
	own.ProjectID = p.ID
	if _, err := s.UpsertMCPServer(ctx, own, t0); err != nil {
		t.Fatalf("do projeto: %v", err)
	}

	globais, err := s.GlobalMCPServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(globais) != 1 || globais[0].URL != "https://global.exemplo/mcp" {
		t.Fatalf("globais = %+v", globais)
	}
	// The store answers what was configured; folding the two is the worker's job, so
	// a listing can still tell them apart.
	if len(globais[0].Headers) != 1 || globais[0].Headers[0].ValueEnv != "NW_MCP_CONTEXT7" {
		t.Errorf("headers do global = %+v", globais[0].Headers)
	}

	lido, err := s.ProjectByName(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(lido.MCPServers) != 1 || lido.MCPServers[0].URL != "https://do-projeto.exemplo/mcp" {
		t.Fatalf("servidores do Projeto = %+v", lido.MCPServers)
	}
}

// Two globals of the same name is the collision the partial index exists to stop —
// as an update, not as a second row.
func TestUpsertingAGlobalTwiceCorrectsItInsteadOfDuplicating(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.UpsertMCPServer(ctx, mcpFixture("context7", "https://errada.exemplo/mcp"), t0); err != nil {
		t.Fatal(err)
	}
	corrigido := mcpFixture("context7", "https://certa.exemplo/mcp")
	// Headers are replaced wholesale, unlike Fontes: a header left out is one the
	// operator decided the server does not need, and merging would leave an obsolete
	// Authorization on the wire with no way to remove it.
	corrigido.Headers = []domain.MCPHeader{{Name: "X-Api-Key", ValueEnv: "NW_MCP_OUTRA"}}
	if _, err := s.UpsertMCPServer(ctx, corrigido, t0); err != nil {
		t.Fatal(err)
	}

	globais, err := s.GlobalMCPServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(globais) != 1 {
		t.Fatalf("servidores = %d, queria 1: %+v", len(globais), globais)
	}
	if globais[0].URL != "https://certa.exemplo/mcp" {
		t.Errorf("URL = %s", globais[0].URL)
	}
	if len(globais[0].Headers) != 1 || globais[0].Headers[0].Name != "X-Api-Key" {
		t.Errorf("headers = %+v; a substituição devia ter apagado o Authorization antigo", globais[0].Headers)
	}
}

func TestRemovingAProjectTakesItsMCPServersWithIt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-web")

	own := mcpFixture("context7", "https://exemplo/mcp")
	own.ProjectID = p.ID
	if _, err := s.UpsertMCPServer(ctx, own, t0); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}

	var servidores, headers int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_servers`).Scan(&servidores); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_headers`).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	if servidores != 0 || headers != 0 {
		t.Errorf("sobraram %d servidor(es) e %d header(s) órfãos", servidores, headers)
	}
}

func TestRemovingAnUnknownServerSaysSo(t *testing.T) {
	err := newStore(t).RemoveMCPServer(context.Background(), 0, "nao-existe")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("erro = %v, queria ErrNotFound", err)
	}
}

// The schema is built so the database stays copyable without leaking a credential.
// This asserts that at the level that matters: what is actually on disk.
func TestNoCredentialEverReachesTheDatabase(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	m := mcpFixture("context7", "https://exemplo/mcp")
	if _, err := s.UpsertMCPServer(ctx, m, t0); err != nil {
		t.Fatal(err)
	}

	// There is no column for a value, which is the whole design — so the assertion is
	// that the columns that DO exist carry the variable's name and its fixed prefix,
	// and nothing else.
	var name, prefix, valueEnv string
	err := s.DB().QueryRowContext(ctx, `SELECT name, prefix, value_env FROM mcp_headers`).
		Scan(&name, &prefix, &valueEnv)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Authorization" || prefix != "Bearer " || valueEnv != "NW_MCP_CONTEXT7" {
		t.Errorf("header gravado = %q / %q / %q", name, prefix, valueEnv)
	}
}

// Validation lives in the domain and is enforced here, so a bad server cannot get in
// through a caller that forgot to check.
func TestTheStoreRefusesAnInvalidServer(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, m := range []domain.MCPServer{
		{Name: "Context7", Transport: domain.MCPTransportHTTP, URL: "https://exemplo/mcp"},
		{Name: "context7", Transport: "stdio", URL: "https://exemplo/mcp"},
		{Name: "context7", Transport: domain.MCPTransportHTTP, URL: "http://exemplo.com/mcp"},
		{Name: "context7", Transport: domain.MCPTransportHTTP, URL: "https://exemplo/mcp",
			Headers: []domain.MCPHeader{{Name: "Authorization", Prefix: "Bearer sk-vazou-aqui", ValueEnv: "NW_X"}}},
	} {
		if _, err := s.UpsertMCPServer(ctx, m, t0); err == nil {
			t.Errorf("o store aceitou um servidor inválido: %+v", m)
		}
	}
}
