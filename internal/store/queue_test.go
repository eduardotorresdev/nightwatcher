package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

const leaseTTL = 25 * time.Minute

func TestWorkersRacingForTheQueueNeverGetTheSameItem(t *testing.T) {
	// The sequential test below proves a claimed item looks claimed afterwards, which
	// a read-then-write Claim would satisfy just as well. This one is the one that
	// separates them: eight workers going at the queue at once, and a Claim that
	// selected a candidate and then wrote the owner in a second round trip would hand
	// the same row to two of them in the gap between the two statements.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")

	const items = 24
	for i := 0; i < items; i++ {
		a := fixtureAnalysis(t, s, p, "race-"+string(rune('a'+i)))
		if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	var (
		mu    sync.Mutex
		taken = map[int64]string{}
		wg    sync.WaitGroup
	)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			for {
				item, ok, err := s.Claim(ctx, worker, t0, leaseTTL)
				if err != nil {
					t.Errorf("Claim(%s): %v", worker, err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				if other, dup := taken[item.ID]; dup {
					t.Errorf("item %d entregue a %s e a %s", item.ID, other, worker)
				}
				taken[item.ID] = worker
				mu.Unlock()
			}
		}("worker-" + string(rune('a'+w)))
	}
	wg.Wait()

	// And every item was handed out exactly once, so the race lost nothing either.
	if len(taken) != items {
		t.Errorf("itens reivindicados = %d, quero %d", len(taken), items)
	}
}

func TestAClaimedItemIsInvisibleToTheSecondWorker(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9200")

	if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	first, ok, err := s.Claim(ctx, "worker-a", t0, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("primeiro Claim: ok=%v err=%v", ok, err)
	}
	if first.AnalysisID != a.ID {
		t.Errorf("reivindicou a análise %s, quero %s", first.AnalysisID, a.ID)
	}

	_, ok, err = s.Claim(ctx, "worker-b", t0.Add(time.Minute), leaseTTL)
	if err != nil {
		t.Fatalf("segundo Claim: %v", err)
	}
	if ok {
		t.Error("o segundo worker levou um item que já estava reivindicado")
	}
}

func TestALeaseThatLapsesMakesTheItemTakeableAgain(t *testing.T) {
	// This is the whole recovery story: a worker killed by SIGKILL, by the OOM killer
	// or by a Dokku deploy releases nothing, and nothing has to notice it died.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9300")

	if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	dead, ok, err := s.Claim(ctx, "worker-morto", t0, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	taken, ok, err := s.Claim(ctx, "worker-vivo", t0.Add(leaseTTL+time.Minute), leaseTTL)
	if err != nil {
		t.Fatalf("Claim depois do prazo: %v", err)
	}
	if !ok {
		t.Fatal("o item com lease vencido continuou intocável")
	}
	if taken.ID != dead.ID {
		t.Errorf("pegou o item %d, quero o %d", taken.ID, dead.ID)
	}
	if taken.Owner != "worker-vivo" {
		t.Errorf("dono = %q", taken.Owner)
	}

	// And the worker that died cannot come back and close the run someone else is now
	// doing.
	if err := s.MarkDone(ctx, dead.ID, "worker-morto", t0.Add(leaseTTL+2*time.Minute)); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("MarkDone do worker morto devolveu %v, quero ErrLeaseLost", err)
	}
}

func TestAttemptsCountsEveryClaimSoAnItemThatHangsRunsOut(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9400")

	if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Three workers in a row take the item and hang until the lease lapses. Nobody
	// ever reports a failure, and the count still climbs — that is what stops it from
	// being retried forever.
	for attempt := 1; attempt <= 3; attempt++ {
		at := t0.Add(time.Duration(attempt-1) * (leaseTTL + time.Minute))
		item, ok, err := s.Claim(ctx, "worker", at, leaseTTL)
		if err != nil || !ok {
			t.Fatalf("Claim %d: ok=%v err=%v", attempt, ok, err)
		}
		if item.Attempts != attempt {
			t.Fatalf("tentativa %d: attempts = %d", attempt, item.Attempts)
		}
	}
}

func TestReleaseWithBackoffKeepsTheItemOutOfReachUntilItsTime(t *testing.T) {
	// Backoff is a date in a column, never a sleeping goroutine.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9500")

	if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, ok, err := s.Claim(ctx, "worker-a", t0, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	retryAt := t0.Add(5 * time.Minute)
	if err := s.Release(ctx, item.ID, "worker-a", retryAt); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, ok, err := s.Claim(ctx, "worker-b", retryAt.Add(-time.Second), leaseTTL); err != nil || ok {
		t.Errorf("item foi reivindicado antes da hora: ok=%v err=%v", ok, err)
	}

	again, ok, err := s.Claim(ctx, "worker-b", retryAt, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("Claim na hora do retry: ok=%v err=%v", ok, err)
	}
	if again.Attempts != 2 {
		t.Errorf("attempts = %d depois do retry, quero 2", again.Attempts)
	}
}

func TestExtendingTheLeaseHoldsTheItemThroughASlowAnalysis(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9600")

	if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, ok, err := s.Claim(ctx, "worker-a", t0, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	halfway := t0.Add(20 * time.Minute)
	if err := s.ExtendLease(ctx, item.ID, "worker-a", halfway, leaseTTL); err != nil {
		t.Fatalf("ExtendLease: %v", err)
	}

	// Past the original deadline, the item is still worker-a's.
	if _, ok, err := s.Claim(ctx, "worker-b", t0.Add(leaseTTL+time.Minute), leaseTTL); err != nil || ok {
		t.Errorf("item foi tomado apesar do lease estendido: ok=%v err=%v", ok, err)
	}
}

func TestAWorkerCannotTouchAnItemAnotherWorkerNowHolds(t *testing.T) {
	// MarkDone is covered by the lapsed-lease test above; Release and ExtendLease are
	// the same hazard by another name. A worker that comes back from a hung analysis
	// must not push the deadline of, or hand back, a run somebody else is doing.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9900")

	if _, err := s.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	stale, ok, err := s.Claim(ctx, "worker-lento", t0, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	// Its lease lapses and somebody else picks the item up.
	after := t0.Add(leaseTTL + time.Minute)
	if _, ok, err := s.Claim(ctx, "worker-novo", after, leaseTTL); err != nil || !ok {
		t.Fatalf("Claim do sucessor: ok=%v err=%v", ok, err)
	}

	if err := s.ExtendLease(ctx, stale.ID, "worker-lento", after, leaseTTL); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("ExtendLease do worker vencido devolveu %v, quero ErrLeaseLost", err)
	}
	if err := s.Release(ctx, stale.ID, "worker-lento", after.Add(time.Minute)); !errors.Is(err, store.ErrLeaseLost) {
		t.Errorf("Release do worker vencido devolveu %v, quero ErrLeaseLost", err)
	}

	// And the successor still holds it: nothing above knocked it loose.
	if _, ok, err := s.Claim(ctx, "worker-terceiro", after.Add(time.Minute), leaseTTL); err != nil || ok {
		t.Errorf("um terceiro worker levou o item: ok=%v err=%v", ok, err)
	}
}

func TestReleasingWithoutABackoffLeavesTheItemTakeableAtOnce(t *testing.T) {
	// A failure that is retryable right away hands the item back with no date. That
	// must mean "now", and must not write a placeholder into a column the claim
	// predicate orders by and Claim hands back to the caller as a time.
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")
	a := fixtureAnalysis(t, s, p, "9910")

	enqueued, err := s.Enqueue(ctx, a.ID, t0, time.Time{})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, ok, err := s.Claim(ctx, "worker-a", t0, leaseTTL); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Release(ctx, enqueued.ID, "worker-a", time.Time{}); err != nil {
		t.Fatalf("Release: %v", err)
	}

	again, ok, err := s.Claim(ctx, "worker-b", t0, leaseTTL)
	if err != nil || !ok {
		t.Fatalf("Claim depois do Release sem backoff: ok=%v err=%v", ok, err)
	}
	if !again.AvailableAt.Equal(t0) {
		t.Errorf("available_at = %v, quero %v — devolver sem backoff não pode inventar data", again.AvailableAt, t0)
	}
}

func TestTheQueueIsDrainedOldestFirstAndEmptiesAsItemsFinish(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	p := fixtureProject(t, s, "acme", "acme-api")

	first := fixtureAnalysis(t, s, p, "9700")
	second := fixtureAnalysis(t, s, p, "9701")
	if _, err := s.Enqueue(ctx, first.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Enqueue(ctx, second.ID, t0.Add(time.Minute), time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	depth, err := s.QueueDepth(ctx)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 2 {
		t.Fatalf("profundidade = %d, quero 2", depth)
	}

	now := t0.Add(2 * time.Minute)
	var drained []string
	for {
		item, ok, err := s.Claim(ctx, "worker", now, leaseTTL)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if !ok {
			break
		}
		drained = append(drained, item.AnalysisID)
		if err := s.MarkDone(ctx, item.ID, "worker", now); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
	}

	if len(drained) != 2 || drained[0] != first.ID || drained[1] != second.ID {
		t.Errorf("drenou %v, quero [%s %s]", drained, first.ID, second.ID)
	}
	if depth, err = s.QueueDepth(ctx); err != nil || depth != 0 {
		t.Errorf("profundidade final = %d (err=%v)", depth, err)
	}
}

func TestAnItemEnqueuedSurvivesTheProcessThatEnqueuedIt(t *testing.T) {
	// The queue is on disk, so a restart in the middle of a burst loses nothing.
	ctx := context.Background()
	path := t.TempDir() + "/nightwatcher.db"

	before, err := store.Setup(ctx, path, t0)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	p := fixtureProject(t, before, "acme", "acme-api")
	a := fixtureAnalysis(t, before, p, "9800")
	if _, err := before.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, ok, err := before.Claim(ctx, "worker-antigo", t0, leaseTTL); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	before.Close()

	after, err := store.Setup(ctx, path, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("Setup depois do reinício: %v", err)
	}
	defer after.Close()

	item, ok, err := after.Claim(ctx, "worker-novo", t0.Add(time.Hour), leaseTTL)
	if err != nil {
		t.Fatalf("Claim depois do reinício: %v", err)
	}
	if !ok {
		t.Fatal("a ocorrência enfileirada sumiu no reinício")
	}
	if item.AnalysisID != a.ID {
		t.Errorf("voltou a análise %s, quero %s", item.AnalysisID, a.ID)
	}
}

// The race test above runs eight goroutines against ONE *store.Store, which is one
// SQLite connection: the pool is capped at a single writer, so a two-statement Claim is
// caught there only because each statement gives the connection back in between.
//
// Production has the other topology, and it is the one that matters. A Dokku deploy
// overlaps two daemon containers on the same file for a few seconds, and every
// `dokku run` subcommand is a third process. This test is that: eight SEPARATE
// connections to the same database, each with its own SQLite write lock to fight for,
// hammering Claim. If the subquery that picks a candidate could ever run against a
// snapshot older than the write, two processes would take the same Ocorrência — two
// analyses and two issues, and nothing anywhere would say so.
func TestSeparateProcessesRacingOnTheSameFileNeverGetTheSameItem(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nightwatcher.db")

	seed, err := store.Setup(ctx, path, t0)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer seed.Close()

	p := fixtureProject(t, seed, "acme", "acme-api")
	const items = 40
	for i := 0; i < items; i++ {
		a := fixtureAnalysis(t, seed, p, fmt.Sprintf("cross-%d", i))
		if _, err := seed.Enqueue(ctx, a.ID, t0, time.Time{}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	const workers = 8
	handles := make([]*store.Store, workers)
	for i := range handles {
		h, err := store.Setup(ctx, path, t0)
		if err != nil {
			t.Fatalf("Setup(%d): %v", i, err)
		}
		defer h.Close()
		handles[i] = h
	}

	var (
		mu    sync.Mutex
		taken = map[int64]string{}
		start = make(chan struct{})
		wg    sync.WaitGroup
	)
	for i, h := range handles {
		wg.Add(1)
		go func(h *store.Store, worker string) {
			defer wg.Done()
			<-start
			for {
				// The lease is long and the instant is fixed, so nothing here can become
				// takeable again by expiry: every second hand-out is a real double claim.
				item, ok, err := h.Claim(ctx, worker, t0, leaseTTL)
				if err != nil {
					t.Errorf("Claim(%s): %v", worker, err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				if other, dup := taken[item.ID]; dup {
					t.Errorf("item %d entregue a %s e a %s: duas Análises da mesma Ocorrência", item.ID, other, worker)
				}
				taken[item.ID] = worker
				mu.Unlock()
			}
		}(h, fmt.Sprintf("processo-%d", i))
	}
	close(start)
	wg.Wait()

	if len(taken) != items {
		t.Errorf("itens reivindicados = %d, quero %d: a disputa perdeu Ocorrências", len(taken), items)
	}
	// And every claim counted exactly once. A row handed out twice would show attempts
	// of 2 even if the map above happened to miss the overlap.
	var maxAttempts int
	if err := seed.DB().QueryRowContext(ctx, `SELECT max(attempts) FROM queue`).Scan(&maxAttempts); err != nil {
		t.Fatalf("lendo attempts: %v", err)
	}
	if maxAttempts != 1 {
		t.Errorf("max(attempts) = %d, quero 1: algum item foi reivindicado mais de uma vez", maxAttempts)
	}
}

// A deploy racing a restart puts two `serve` processes on one volume for a few seconds,
// and both sweep at boot. If the "is it already queued?" guard is a separate statement
// from the insert, every sweeper passes the guard and every sweeper inserts: two queue
// rows for one Análise, two workers claiming them, two full Análises, and two issues in
// the Destino for one Ocorrência — the noise that killed the predecessor, arriving from
// the recovery path meant to protect against losing work.
func TestConcurrentSweepsPutAStrandedAnaliseBackExactlyOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nightwatcher.db")

	seed, err := store.Setup(ctx, path, t0)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	p := fixtureProject(t, seed, "acme", "acme-api")
	// Recorded and never enqueued: the stranded shape.
	a := fixtureAnalysis(t, seed, p, "orfa-1")
	seed.Close()

	const sweepers = 8
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		rescued  int
		sweepErr error
	)
	for i := 0; i < sweepers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := store.Setup(ctx, path, t0)
			if err != nil {
				mu.Lock()
				sweepErr = errors.Join(sweepErr, err)
				mu.Unlock()
				return
			}
			defer h.Close()
			<-start
			back, err := h.EnqueueStranded(ctx, t0)
			mu.Lock()
			rescued += len(back)
			sweepErr = errors.Join(sweepErr, err)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if sweepErr != nil {
		t.Fatalf("EnqueueStranded: %v", sweepErr)
	}

	after, err := store.Setup(ctx, path, t0)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer after.Close()

	var open int
	if err := after.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM queue WHERE analysis_id = ? AND done_at IS NULL`, a.ID).Scan(&open); err != nil {
		t.Fatalf("contando a fila: %v", err)
	}
	if open != 1 {
		t.Errorf("linhas abertas na fila = %d, quero 1: a Ocorrência seria analisada %d vezes", open, open)
	}
	// Only the sweeper that actually wrote reports the rescue, so the boot log of the
	// losing process does not claim work it did not do.
	if rescued != 1 {
		t.Errorf("resgates reportados = %d, quero 1", rescued)
	}
}
