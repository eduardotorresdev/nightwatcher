package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// UpsertProject records a Projeto and the Fontes given with it, keyed by name — the
// name is what the operator types, so running `project add` twice with a corrected
// branch fixes the Projeto instead of creating a second one.
//
// Fontes already stored and absent from p.Sources are left alone: the flag is
// repeatable and a forgotten repetition must not silently unhook a Fonte that is
// still delivering. Use RemoveSource to drop one on purpose.
func (s *Store) UpsertProject(ctx context.Context, p domain.Project, now time.Time) (domain.Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Project{}, fmt.Errorf("abrindo transação: %w", err)
	}
	defer tx.Rollback()

	var createdAt int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO projects (name, repo, branch, linear_project_id, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			repo = excluded.repo,
			branch = excluded.branch,
			linear_project_id = excluded.linear_project_id
		RETURNING id, created_at`,
		p.Name, p.Repo, p.Branch, p.LinearProjectID, asMillis(now),
	).Scan(&p.ID, &createdAt)
	if err != nil {
		return domain.Project{}, fmt.Errorf("gravando projeto %s: %w", p.Name, err)
	}

	for i := range p.Sources {
		p.Sources[i].ProjectID = p.ID
		if err := upsertSourceTx(ctx, tx, &p.Sources[i]); err != nil {
			return domain.Project{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.Project{}, fmt.Errorf("confirmando projeto %s: %w", p.Name, err)
	}
	return p, nil
}

// UpsertSource adds or updates one Fonte. Repointing a slug at another Projeto is
// allowed and is the correction path for a Fonte registered under the wrong Projeto.
func (s *Store) UpsertSource(ctx context.Context, src domain.Source) (domain.Source, error) {
	if err := upsertSourceTx(ctx, s.db, &src); err != nil {
		return domain.Source{}, err
	}
	return src, nil
}

// execQuerier is what *sql.DB and *sql.Tx have in common here.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func upsertSourceTx(ctx context.Context, q execQuerier, src *domain.Source) error {
	err := q.QueryRowContext(ctx, `
		INSERT INTO sources (project_id, sentry_slug, label)
		VALUES (?, ?, ?)
		ON CONFLICT (sentry_slug) DO UPDATE SET
			project_id = excluded.project_id,
			label = excluded.label
		RETURNING id`,
		src.ProjectID, src.SentrySlug, src.Label,
	).Scan(&src.ID)
	if err != nil {
		return fmt.Errorf("gravando fonte %s: %w", src.SentrySlug, err)
	}
	return nil
}

// RemoveSource unhooks a Fonte. The Análises it already produced stay listable.
func (s *Store) RemoveSource(ctx context.Context, sentrySlug string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE sentry_slug = ?`, sentrySlug)
	if err != nil {
		return fmt.Errorf("removendo fonte %s: %w", sentrySlug, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("fonte %s: %w", sentrySlug, ErrNotFound)
	}
	return nil
}

// ListProjects returns every Projeto with its Fontes loaded.
func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, repo, branch, linear_project_id
		FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listando projetos: %w", err)
	}
	defer rows.Close()

	var out []domain.Project
	byID := map[int64]int{}
	for rows.Next() {
		var p domain.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Repo, &p.Branch, &p.LinearProjectID); err != nil {
			return nil, fmt.Errorf("lendo projeto: %w", err)
		}
		byID[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listando projetos: %w", err)
	}

	// One query for every Fonte instead of one per Projeto: the list is short, and
	// this keeps `project list` at two round trips no matter how it grows.
	srcRows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, sentry_slug, label
		FROM sources ORDER BY sentry_slug`)
	if err != nil {
		return nil, fmt.Errorf("listando fontes: %w", err)
	}
	defer srcRows.Close()

	for srcRows.Next() {
		var src domain.Source
		if err := srcRows.Scan(&src.ID, &src.ProjectID, &src.SentrySlug, &src.Label); err != nil {
			return nil, fmt.Errorf("lendo fonte: %w", err)
		}
		if i, ok := byID[src.ProjectID]; ok {
			out[i].Sources = append(out[i].Sources, src)
		}
	}
	if err := srcRows.Err(); err != nil {
		return nil, fmt.Errorf("listando fontes: %w", err)
	}

	// A third round trip, for the same reason as the second: one query for every
	// Projeto's servers rather than one query per Projeto. The global servers are NOT
	// folded in here — `project list` has to be able to show what this Projeto itself
	// declared.
	servers, err := s.mcpServers(ctx, `WHERE project_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for projectID, list := range servers {
		if i, ok := byID[projectID]; ok {
			out[i].MCPServers = list
		}
	}
	return out, nil
}

// ProjectByID returns one Projeto with its Fontes, or ErrNotFound.
func (s *Store) ProjectByID(ctx context.Context, id int64) (domain.Project, error) {
	return s.project(ctx, `WHERE id = ?`, id)
}

// ProjectByName returns one Projeto with its Fontes, or ErrNotFound.
func (s *Store) ProjectByName(ctx context.Context, name string) (domain.Project, error) {
	return s.project(ctx, `WHERE name = ?`, name)
}

func (s *Store) project(ctx context.Context, where string, arg any) (domain.Project, error) {
	var p domain.Project
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, repo, branch, linear_project_id FROM projects `+where, arg,
	).Scan(&p.ID, &p.Name, &p.Repo, &p.Branch, &p.LinearProjectID)
	if err == sql.ErrNoRows {
		return domain.Project{}, fmt.Errorf("projeto %v: %w", arg, ErrNotFound)
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("lendo projeto %v: %w", arg, err)
	}

	sources, err := s.sourcesOf(ctx, p.ID)
	if err != nil {
		return domain.Project{}, err
	}
	p.Sources = sources

	servers, err := s.mcpServersOf(ctx, p.ID)
	if err != nil {
		return domain.Project{}, err
	}
	p.MCPServers = servers
	return p, nil
}

func (s *Store) sourcesOf(ctx context.Context, projectID int64) ([]domain.Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, sentry_slug, label
		FROM sources WHERE project_id = ? ORDER BY sentry_slug`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listando fontes do projeto %d: %w", projectID, err)
	}
	defer rows.Close()

	var out []domain.Source
	for rows.Next() {
		var src domain.Source
		if err := rows.Scan(&src.ID, &src.ProjectID, &src.SentrySlug, &src.Label); err != nil {
			return nil, fmt.Errorf("lendo fonte: %w", err)
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// ResolveSource answers the question the ingest path asks on every webhook: which
// Fonte is this Sentry project slug, and which Projeto does it belong to. ErrNotFound
// here is the "fonte desconhecida" recusa, not a broken query.
//
// It is one query on purpose: this runs inside the request cycle, before the handler
// answers Sentry.
func (s *Store) ResolveSource(ctx context.Context, sentrySlug string) (domain.Project, domain.Source, error) {
	var (
		p   domain.Project
		src domain.Source
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.repo, p.branch, p.linear_project_id,
		       s.id, s.project_id, s.sentry_slug, s.label
		FROM sources s JOIN projects p ON p.id = s.project_id
		WHERE s.sentry_slug = ?`, sentrySlug,
	).Scan(&p.ID, &p.Name, &p.Repo, &p.Branch, &p.LinearProjectID,
		&src.ID, &src.ProjectID, &src.SentrySlug, &src.Label)
	if err == sql.ErrNoRows {
		return domain.Project{}, domain.Source{}, fmt.Errorf("fonte %s: %w", sentrySlug, ErrNotFound)
	}
	if err != nil {
		return domain.Project{}, domain.Source{}, fmt.Errorf("resolvendo fonte %s: %w", sentrySlug, err)
	}
	return p, src, nil
}
