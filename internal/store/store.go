package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	sqlite "modernc.org/sqlite"
)

// Store is the whole state layer behind one type: the configuration of Projetos, the
// queue of accepted Ocorrências, the record of what was already seen, the history of
// Análises with prompt and response, and the link between an Ocorrência and the issue
// it produced in the Destino.
//
// Two rules hold everywhere in here:
//
//   - Every method takes a context, and every method whose behaviour depends on time
//     receives the instant from the caller's Clock. The store never calls time.Now():
//     retry backoff, lease expiry and the hourly window per Projeto have to be
//     testable without real waiting.
//   - Nothing written here is a credential. The database must be readable, copyable
//     and inspectable with `sqlite3` without leaking one.
type Store struct{ db *sql.DB }

// New wraps an already-open database. The schema is not touched — call Migrate, or
// use Setup, which does both.
func New(db *sql.DB) *Store { return &Store{db: db} }

// Setup is the single door for the daemon and for every `dokku run` subcommand: it
// opens the database in WAL and brings the schema up to date before handing it over,
// so no caller can forget the migration and discover it as a missing-table error at
// the first webhook.
func Setup(ctx context.Context, path string, now time.Time) (*Store, error) {
	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, db, now); err != nil {
		db.Close()
		return nil, err
	}
	return New(db), nil
}

// Close releases the underlying handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the integration seam, which builds its own fixtures
// against a real file. Production code goes through the methods.
func (s *Store) DB() *sql.DB { return s.db }

var (
	// ErrNotFound is what every lookup returns when the row is not there, so callers
	// distinguish "does not exist" from "the query broke".
	ErrNotFound = errors.New("registro não encontrado")

	// ErrDuplicate reports that this exact (installation, projeto Sentry, issue,
	// substatus) was already recorded. It is a normal outcome of ingestion, not a
	// defect: Sentry delivers the same Ocorrência more than once.
	ErrDuplicate = errors.New("ocorrência já registrada")

	// ErrLeaseLost reports that the item is no longer the caller's: the deadline
	// lapsed and another worker took it, or it was already finished.
	ErrLeaseLost = errors.New("item da fila não pertence mais a este worker")
)

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE. Spelled out instead of imported
// from modernc.org/sqlite/lib, which is the whole translated C library.
const sqliteConstraintUnique = 2067

// isUniqueViolation recognises the dedup index rejecting a repeated delivery. The
// extended result code is checked rather than the message text, because the message
// names the columns and would change with the index.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqliteConstraintUnique
}

// newID mints the short identifier an Análise is known by on the command line. Random
// rather than sequential so nothing about volume or ordering leaks into a URL or a
// log line, and short enough to be typed back into `analysis show`.
func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gerando id de análise: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Timestamps are stored as Unix milliseconds in UTC; the zero time is stored as NULL.
// Writing the zero time as a number would put the year 1 into a column that the lease
// and window comparisons treat as ordered, and `lease_until < now` would be true for
// an item nobody ever claimed.

func asMillis(t time.Time) int64 { return t.UTC().UnixMilli() }

func asNullMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().UnixMilli()
}

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMillis(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMillis(v.Int64)
}

func asNullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// scanner is what both *sql.Row and *sql.Rows satisfy, so one row-reading function
// serves the single lookup and the listing.
type scanner interface{ Scan(dest ...any) error }
