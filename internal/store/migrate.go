package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one versioned step. The version comes from the file name so the
// ordering is visible in the directory listing and cannot drift from the code.
type migration struct {
	version int64
	name    string
	body    string
}

// Migrate brings the schema up to date, in version order, exactly once each. It is
// idempotent: every process that opens the database calls it at startup, and the
// daemon and a `dokku run` container may well call it at the same moment — the
// applied set is read and written inside the same transaction as the step itself, so
// the loser of that race finds the version already recorded.
func Migrate(ctx context.Context, db *sql.DB, now time.Time) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("criando schema_migrations: %w", err)
	}

	steps, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range steps {
		// A hint, not the decision: it only saves opening a write transaction on the
		// overwhelmingly common boot where everything is already applied. Whether the
		// step actually runs is settled inside applyMigration, under the write lock.
		applied, err := isApplied(ctx, db, m.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := applyMigration(ctx, db, m, now); err != nil {
			return err
		}
	}
	return nil
}

func isApplied(ctx context.Context, db *sql.DB, version int64) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lendo schema_migrations: %w", err)
	}
	return true, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("abrindo transação da migration %s: %w", m.name, err)
	}
	defer tx.Rollback()

	// The version is claimed BEFORE the DDL runs, and this ordering is the whole
	// concurrency story. The INSERT is what takes SQLite's write lock, so from here
	// on no other process can be applying anything; and if one got here first, the
	// row already exists and RowsAffected reports zero instead of the insert failing.
	//
	// Deciding this from the read outside the transaction would be a race the daemon
	// hits on every deploy: it and a `dokku run` container open the same file at the
	// same moment, both read "not applied", and the loser dies with `table projects
	// already exists` — a migration that is idempotent only when nobody else is
	// booting is not idempotent.
	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, asMillis(now),
	)
	if err != nil {
		return fmt.Errorf("registrando migration %s: %w", m.name, err)
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registrando migration %s: %w", m.name, err)
	}
	if claimed == 0 {
		// Somebody else applied it while we were getting here. Nothing to do, and the
		// deferred Rollback undoes the empty transaction.
		return nil
	}

	// Applied inside the same transaction as the version row: a crash between the two
	// would otherwise leave a schema that no version describes, or a version that no
	// schema backs.
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("aplicando migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("confirmando migration %s: %w", m.name, err)
	}
	return nil
}

// loadMigrations reads the embedded steps and sorts them by version. Sorting by
// version rather than by file name means a step numbered 10 does not land before 2.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("lendo migrations embutidas: %w", err)
	}

	var out []migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := versionOf(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %s e %s têm a mesma versão %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("lendo migration %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), body: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func versionOf(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("migration %s não segue o padrão <versão>_<nome>.sql", name)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration %s não começa com um número de versão: %w", name, err)
	}
	return version, nil
}
