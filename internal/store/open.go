// Package store is the state layer. SQLite in a file on the persistent volume, in
// WAL mode — which is mandatory, not a preference: the daemon and the ephemeral
// `dokku run` containers touch the same file at the same time.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sqlite "modernc.org/sqlite"
)

// BusyTimeout is how long a writer waits for a lock before giving up. It exists
// because `dokku run ... project add` writes to the same file the daemon is draining
// its queue against, and a bare SQLITE_BUSY there would surface as a confusing
// command failure.
const BusyTimeout = 5 * time.Second

// Open opens (creating if needed) the database at path, in WAL mode, with the
// pragmas concurrent access between the daemon and the CLI requires.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("criando diretório do banco %s: %w", dir, err)
		}
	}

	// journal_mode is deliberately NOT in the DSN. The pragmas here are per-connection
	// and are applied in the order written, so busy_timeout has to come first: a
	// pragma that runs before it has no busy handler installed and fails on the spot
	// instead of waiting.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		path, BusyTimeout.Milliseconds(),
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrindo banco %s: %w", path, err)
	}

	// SQLite takes one writer at a time. Letting database/sql open an unbounded pool
	// of writers turns contention into SQLITE_BUSY errors instead of into waiting.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("alcançando banco %s: %w", path, err)
	}

	mode, err := enableWAL(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("pondo %s em WAL: %w", path, err)
	}
	if mode != "wal" {
		db.Close()
		return nil, fmt.Errorf("journal_mode ficou %q, esperado wal — acesso concorrente entre daemon e dokku run exige WAL", mode)
	}

	return db, nil
}

// enableWAL switches the file to WAL and returns the mode it settled in.
//
// The switch needs an exclusive lock for the moment it rewrites the header, and it is
// the one statement SQLite does NOT route through the busy handler — it returns
// SQLITE_BUSY at once. That is exactly the situation this package exists for: the
// daemon booting while a `dokku run` container opens the same file. So the wait is
// done here instead, bounded by the same BusyTimeout. On an existing WAL database the
// statement is a no-op and the loop runs once.
func enableWAL(db *sql.DB) (string, error) {
	deadline := time.Now().Add(BusyTimeout)
	for {
		var mode string
		err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil {
			return mode, nil
		}
		if !isBusy(err) || !time.Now().Before(deadline) {
			return "", err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy recognises the whole SQLITE_BUSY family. The extended codes carry the reason
// in the high byte (SQLITE_BUSY_SNAPSHOT, _RECOVERY, _TIMEOUT), and all of them mean
// the same thing to us: somebody else is holding the lock right now.
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqliteBusy
}

const sqliteBusy = 5
