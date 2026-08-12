package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// QueueItem is one accepted Ocorrência waiting for, or holding, a worker.
type QueueItem struct {
	ID         int64
	AnalysisID string

	// Owner is the worker holding the item, empty when nobody does. LeaseUntil is the
	// deadline that claim expires at.
	Owner      string
	LeaseUntil time.Time

	// Attempts counts claims, not failures. An item that hangs and is retaken after
	// its lease lapses raises it just the same, which is what stops a run that always
	// hangs from spinning forever: the count reaches MaxAttempts and the worker sends
	// it down the failure path.
	Attempts int

	// AvailableAt is when the item may be claimed. Retry backoff is this field moving
	// forward, never a sleeping goroutine.
	AvailableAt time.Time

	CreatedAt time.Time
	DoneAt    time.Time
}

// Enqueue puts an accepted Ocorrência in line. A zero availableAt means it is
// takeable immediately.
func (s *Store) Enqueue(ctx context.Context, analysisID string, now time.Time, availableAt time.Time) (QueueItem, error) {
	return enqueue(ctx, s.db, analysisID, now, availableAt)
}

func enqueue(ctx context.Context, q execQuerier, analysisID string, now time.Time, availableAt time.Time) (QueueItem, error) {
	if availableAt.IsZero() {
		availableAt = now
	}

	item := QueueItem{
		AnalysisID:  analysisID,
		AvailableAt: availableAt.UTC(),
		CreatedAt:   now.UTC(),
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO queue (analysis_id, available_at, created_at) VALUES (?, ?, ?)`,
		analysisID, asMillis(availableAt), asMillis(now))
	if err != nil {
		return QueueItem{}, fmt.Errorf("enfileirando análise %s: %w", analysisID, err)
	}
	if item.ID, err = res.LastInsertId(); err != nil {
		return QueueItem{}, fmt.Errorf("enfileirando análise %s: %w", analysisID, err)
	}
	return item, nil
}

// Claim takes the oldest item that is due and unheld, marking it as this worker's
// until now+ttl. The second return value is false when there is nothing to take.
//
// The whole claim is one statement, so it is one transaction: selecting a candidate
// and then writing the owner in a second round trip would let two workers pick the
// same row. And because the hold is a deadline rather than a flag, a worker that dies
// — SIGKILL, OOM, a Dokku deploy mid-analysis — releases nothing and needs to release
// nothing: the deadline lapses and the next cycle takes the item over. There is no
// coordination here beyond SQLite and the injected Clock.
func (s *Store) Claim(ctx context.Context, owner string, now time.Time, ttl time.Duration) (QueueItem, bool, error) {
	if owner == "" {
		return QueueItem{}, false, errors.New("claim exige um identificador de worker não vazio")
	}

	nowMS := asMillis(now)
	var (
		item        QueueItem
		ownerCol    sql.NullString
		leaseUntil  sql.NullInt64
		availableAt int64
		createdAt   int64
	)
	err := s.db.QueryRowContext(ctx, `
		UPDATE queue SET owner = ?, lease_until = ?, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM queue
			WHERE done_at IS NULL
			  AND available_at <= ?
			  AND (owner IS NULL OR lease_until IS NULL OR lease_until < ?)
			ORDER BY created_at, id
			LIMIT 1
		)
		RETURNING id, analysis_id, owner, lease_until, attempts, available_at, created_at`,
		owner, asMillis(now.Add(ttl)), nowMS, nowMS,
	).Scan(&item.ID, &item.AnalysisID, &ownerCol, &leaseUntil, &item.Attempts, &availableAt, &createdAt)
	if err == sql.ErrNoRows {
		return QueueItem{}, false, nil
	}
	if err != nil {
		return QueueItem{}, false, fmt.Errorf("reivindicando item da fila: %w", err)
	}

	item.Owner = ownerCol.String
	item.LeaseUntil = fromNullMillis(leaseUntil)
	item.AvailableAt = fromMillis(availableAt)
	item.CreatedAt = fromMillis(createdAt)
	return item, true, nil
}

// ExtendLease pushes the deadline of an item this worker holds out to now+ttl, for an
// analysis that is still running and legitimately slow.
//
// An expired but untaken lease can still be extended: the owner has not changed, so
// nobody else is running it and stealing it back from ourselves would serve nothing.
// Once another worker has taken it over, the owner differs and this reports
// ErrLeaseLost.
func (s *Store) ExtendLease(ctx context.Context, id int64, owner string, now time.Time, ttl time.Duration) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE queue SET lease_until = ?
		WHERE id = ? AND owner = ? AND done_at IS NULL`,
		asMillis(now.Add(ttl)), id, owner)
	if err != nil {
		return fmt.Errorf("estendendo lease do item %d: %w", id, err)
	}
	return affectedOne(res, id)
}

// Release hands an item back unfinished and says when it may be taken again — this is
// how retry backoff is written down. The attempts already spent are kept.
//
// A zero availableAt means "no backoff, takeable as soon as the hold is dropped" and
// leaves the column where it was. Writing the zero time through asMillis would put the
// year 1 into a column that the claim predicate compares as ordered and that Claim
// hands back to the caller as a time — the right behaviour by accident, with a
// nonsense date to show for it.
func (s *Store) Release(ctx context.Context, id int64, owner string, availableAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE queue SET owner = NULL, lease_until = NULL, available_at = COALESCE(?, available_at)
		WHERE id = ? AND owner = ? AND done_at IS NULL`,
		asNullMillis(availableAt), id, owner)
	if err != nil {
		return fmt.Errorf("devolvendo item %d à fila: %w", id, err)
	}
	return affectedOne(res, id)
}

// MarkDone closes an item. It is scoped to the owner so that a worker finishing after
// its lease lapsed — and after another worker took the item over — does not close the
// run that is currently happening.
func (s *Store) MarkDone(ctx context.Context, id int64, owner string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE queue SET done_at = ?, owner = NULL, lease_until = NULL
		WHERE id = ? AND owner = ? AND done_at IS NULL`,
		asMillis(now), id, owner)
	if err != nil {
		return fmt.Errorf("concluindo item %d: %w", id, err)
	}
	return affectedOne(res, id)
}

// QueueDepth counts everything still in line, held or not. It is what the arrival
// path compares against MaxQueue — past that point the arrival rate is a symptom, not
// load.
func (s *Store) QueueDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue WHERE done_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("medindo a fila: %w", err)
	}
	return n, nil
}

func affectedOne(res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("lendo resultado do item %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("item %d: %w", id, ErrLeaseLost)
	}
	return nil
}

// Queued reports whether this Análise has an item still in line — held by a worker or
// merely waiting. It is the question that separates "recorded and on its way" from
// "recorded and nobody is ever going to look at it", and without it the only way to ask
// was to reach past the store for the handle.
func (s *Store) Queued(ctx context.Context, analysisID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM queue WHERE analysis_id = ? AND done_at IS NULL LIMIT 1`, analysisID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verificando se a análise %s está na fila: %w", analysisID, err)
	}
	return true, nil
}

// unfinishedStates are the states an Análise can be recorded in without having reached
// an outcome. A row in any of them with no open queue item is an accepted Ocorrência
// that nobody is going to analyse.
var unfinishedStates = []domain.State{
	domain.StateReceived,
	domain.StateQueued,
	domain.StateTriaging,
	domain.StateAnalyzing,
	domain.StateFailing,
}

// EnqueueStranded puts back in line every Análise that is recorded, unfinished, and has
// no open queue item, and returns the ids it rescued.
//
// The window it closes is narrow and real: a worker killed between finishing and closing
// its item leaves nothing behind, and any future write path that records before queueing
// leaves the same shape. Sentry's redelivery does not rescue those — dedup recognises the
// identity and answers "duplicada", which is dedup doing its job and losing the
// Ocorrência anyway.
//
// It is safe to call only when this process holds no lease of its own, which in practice
// means at boot: an item claimed by a LIVE worker of another process still has its queue
// row open, so it is not stranded and is not touched. The whole list is read before the
// first insert, because enqueueing with the cursor still open would have one statement
// reading and another writing the same tables at once.
//
// Two processes may sweep at the same instant — a deploy racing a restart is the ordinary
// case, and two `serve` processes over one volume is a design premise of this system —
// so the guard is re-evaluated INSIDE the insert rather than trusted from the SELECT
// above. Checking in one statement and writing in another lets every sweeper pass the
// check and every sweeper insert: two queue rows for one Análise, two workers claiming
// them, two full Análises, and two issues in the Destino for one Ocorrência. There is no
// unique constraint standing behind this — the queue deliberately allows an Análise to be
// queued again after its previous item is closed — so the statement has to carry the
// guard itself.
func (s *Store) EnqueueStranded(ctx context.Context, now time.Time) ([]string, error) {
	holes := make([]string, 0, len(unfinishedStates))
	args := make([]any, 0, len(unfinishedStates))
	for _, st := range unfinishedStates {
		holes = append(holes, "?")
		args = append(args, string(st))
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM analyses
		 WHERE state IN (`+strings.Join(holes, ",")+`)
		   AND NOT EXISTS (
		         SELECT 1 FROM queue q
		          WHERE q.analysis_id = analyses.id AND q.done_at IS NULL)
		 ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("procurando Ocorrências fora da fila: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lendo as Ocorrências fora da fila: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("lendo as Ocorrências fora da fila: %w", err)
	}
	rows.Close()

	back := make([]string, 0, len(ids))
	var failed error
	for _, id := range ids {
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO queue (analysis_id, available_at, created_at)
			SELECT ?, ?, ?
			 WHERE NOT EXISTS (
			       SELECT 1 FROM queue WHERE analysis_id = ? AND done_at IS NULL)`,
			id, asMillis(now), asMillis(now), id)
		if err != nil {
			// One that cannot go back must not stop the others from going back.
			failed = errors.Join(failed, fmt.Errorf("reenfileirando análise %s: %w", id, err))
			continue
		}
		n, err := res.RowsAffected()
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("reenfileirando análise %s: %w", id, err))
			continue
		}
		if n == 0 {
			// Another sweeper got there first. Not an error, and deliberately not
			// counted: the caller logs what it rescued, and claiming a rescue that
			// another process performed would double-count it in the boot log.
			continue
		}
		back = append(back, id)
	}
	return back, failed
}

// Defer hands an item back with a later availableAt AND gives the attempt back.
//
// It is not Release with a date: Claim raises `attempts` unconditionally, so an
// Ocorrência held back by the per-Projeto hourly cap would spend one of its attempts
// without a single session having run, and four deferrals would send a perfectly
// healthy Ocorrência down the failure path. Waiting is not attempting.
//
// Scoped by owner exactly like Release, so a worker whose lease already lapsed cannot
// rewrite an item that now belongs to somebody else.
func (s *Store) Defer(ctx context.Context, id int64, owner string, availableAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE queue
		   SET owner = NULL, lease_until = NULL,
		       available_at = COALESCE(?, available_at), attempts = max(attempts - 1, 0)
		 WHERE id = ? AND owner = ? AND done_at IS NULL`,
		asNullMillis(availableAt), id, owner)
	if err != nil {
		return fmt.Errorf("adiando o item %d: %w", id, err)
	}
	return affectedOne(res, id)
}
