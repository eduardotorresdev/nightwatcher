package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// Servidores MCP: por Projeto e globais, na mesma tabela discriminados por
// project_id NULL. Nada aqui grava credencial — value_env é o NOME de uma variável de
// ambiente, e é o schema que garante isso ao não ter coluna para o valor.

// UpsertMCPServer records a server and replaces its headers wholesale, keyed by
// (project_id, name).
//
// The headers are replaced and not merged, which is the opposite of what
// UpsertProject does with Fontes — and deliberately so. A Fonte left out of a
// repeated `project add` is still delivering Ocorrências, so forgetting it must not
// unhook it. A header left out is a header the operator has decided the server does
// not need, and merging would leave an obsolete Authorization on the wire with no way
// to remove it short of deleting the server.
func (s *Store) UpsertMCPServer(ctx context.Context, m domain.MCPServer, now time.Time) (domain.MCPServer, error) {
	if err := m.Validate(); err != nil {
		return domain.MCPServer{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.MCPServer{}, fmt.Errorf("abrindo transação: %w", err)
	}
	defer tx.Rollback()

	// Two statements and not one with an ON CONFLICT clause: the uniqueness is carried
	// by two PARTIAL indexes (global and scoped), and SQLite's upsert wants a conflict
	// target it can match against one of them. Reading first and then writing is
	// honest about that, and the whole thing is inside a transaction on a database
	// opened with SetMaxOpenConns(1), so there is no window between the two.
	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM mcp_servers WHERE name = ? AND project_id IS ?`,
		m.Name, asNullID(m.ProjectID),
	).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		err = tx.QueryRowContext(ctx, `
			INSERT INTO mcp_servers (project_id, name, transport, url, description, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			RETURNING id`,
			asNullID(m.ProjectID), m.Name, string(m.Transport), m.URL, m.Description, asMillis(now),
		).Scan(&id)
		if err != nil {
			return domain.MCPServer{}, fmt.Errorf("gravando servidor MCP %s: %w", m.Name, err)
		}
	case err != nil:
		return domain.MCPServer{}, fmt.Errorf("lendo servidor MCP %s: %w", m.Name, err)
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE mcp_servers SET transport = ?, url = ?, description = ? WHERE id = ?`,
			string(m.Transport), m.URL, m.Description, id,
		); err != nil {
			return domain.MCPServer{}, fmt.Errorf("atualizando servidor MCP %s: %w", m.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_headers WHERE server_id = ?`, id); err != nil {
			return domain.MCPServer{}, fmt.Errorf("limpando headers do servidor MCP %s: %w", m.Name, err)
		}
	}
	m.ID = id

	for i := range m.Headers {
		h := &m.Headers[i]
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO mcp_headers (server_id, name, prefix, value_env)
			VALUES (?, ?, ?, ?)
			RETURNING id`,
			id, h.Name, h.Prefix, h.ValueEnv,
		).Scan(new(int64)); err != nil {
			return domain.MCPServer{}, fmt.Errorf("gravando header %s do servidor MCP %s: %w", h.Name, m.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.MCPServer{}, fmt.Errorf("confirmando servidor MCP %s: %w", m.Name, err)
	}
	return m, nil
}

// RemoveMCPServer drops a server and, by cascade, its headers. projectID 0 removes a
// global one.
func (s *Store) RemoveMCPServer(ctx context.Context, projectID int64, name string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM mcp_servers WHERE name = ? AND project_id IS ?`, name, asNullID(projectID))
	if err != nil {
		return fmt.Errorf("removendo servidor MCP %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("servidor MCP %s: %w", name, ErrNotFound)
	}
	return nil
}

// GlobalMCPServers returns the servers that apply to every Projeto.
func (s *Store) GlobalMCPServers(ctx context.Context) ([]domain.MCPServer, error) {
	byProject, err := s.mcpServers(ctx, `WHERE project_id IS NULL`)
	if err != nil {
		return nil, err
	}
	return byProject[0], nil
}

// mcpServersOf returns one Projeto's own servers, without the global ones. Folding
// the two is domain.MergeMCPServers' job, so a listing can still tell them apart.
func (s *Store) mcpServersOf(ctx context.Context, projectID int64) ([]domain.MCPServer, error) {
	byProject, err := s.mcpServers(ctx, `WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	return byProject[projectID], nil
}

// mcpServers reads servers and their headers in two queries, grouped by project_id
// with 0 meaning global — the same shape ListProjects needs and the one-Projeto reads
// need, so there is one loader and not three.
func (s *Store) mcpServers(ctx context.Context, where string, args ...any) (map[int64][]domain.MCPServer, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(project_id, 0), name, transport, url, description
		FROM mcp_servers `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, fmt.Errorf("listando servidores MCP: %w", err)
	}
	defer rows.Close()

	out := map[int64][]domain.MCPServer{}
	// Indexed by server id so the headers can be attached without a second scan, and
	// pointing INTO the slices so an append later would invalidate it — hence the
	// headers are attached after every server is read, never during.
	index := map[int64]*domain.MCPServer{}
	var all []*domain.MCPServer
	for rows.Next() {
		var (
			m         domain.MCPServer
			transport string
		)
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Name, &transport, &m.URL, &m.Description); err != nil {
			return nil, fmt.Errorf("lendo servidor MCP: %w", err)
		}
		m.Transport = domain.MCPTransport(transport)
		held := m
		all = append(all, &held)
		index[m.ID] = &held
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listando servidores MCP: %w", err)
	}
	if len(all) == 0 {
		return out, nil
	}

	hdrRows, err := s.db.QueryContext(ctx, `
		SELECT h.server_id, h.name, h.prefix, h.value_env
		FROM mcp_headers h JOIN mcp_servers m ON m.id = h.server_id
		`+where+`
		ORDER BY h.server_id, h.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("listando headers MCP: %w", err)
	}
	defer hdrRows.Close()

	for hdrRows.Next() {
		var (
			serverID int64
			h        domain.MCPHeader
		)
		if err := hdrRows.Scan(&serverID, &h.Name, &h.Prefix, &h.ValueEnv); err != nil {
			return nil, fmt.Errorf("lendo header MCP: %w", err)
		}
		if m, ok := index[serverID]; ok {
			m.Headers = append(m.Headers, h)
		}
	}
	if err := hdrRows.Err(); err != nil {
		return nil, fmt.Errorf("listando headers MCP: %w", err)
	}

	for _, m := range all {
		out[m.ProjectID] = append(out[m.ProjectID], *m)
	}
	return out, nil
}
