// Package worker is what makes the Análise happen after the webhook has already
// answered — and the place where the predecessor died.
//
// It is two things with one seam between them. The Analyst conducts a single
// Ocorrência through the state machine (Triagem, then Diagnóstico or
// Estudo, then the Destino) and knows nothing about queues. The Pool drains the
// durable queue into Analysts, honouring the concurrency limit and the per-Projeto
// hourly cap, and owns every decision about attempts, leases and backoff.
//
// Three rules run through the whole package, and each one is a defect if forgotten:
//
//   - No code path ends with the Ocorrência gone. A failed session becomes an issue
//     in the Destino carrying the raw Sentry data, the não-analisada mark and the
//     reason; a Destino that cannot be reached keeps the item in the queue instead of
//     dropping it; a shutdown hands the item back whole. The predecessor logged "run
//     falhou" and the Ocorrência simply disappeared.
//   - The queue is a lease with a deadline, not a flag. A worker that dies — SIGKILL,
//     OOM, a Dokku deploy mid-analysis — releases nothing and needs to release
//     nothing: the deadline lapses and the next cycle takes the item over.
//   - Time only ever comes from the injected Clock, and waiting is written down as a
//     future available_at rather than lived through in a sleeping goroutine. Backoff
//     and the hourly window are testable because of it.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// pollInterval is how long Run waits after a pass that took nothing. It is the worst
// case latency between a webhook being accepted and its Análise starting.
const pollInterval = 5 * time.Second

// hourlyDeferral is how long an item waits when its Projeto has already spent the
// hour's budget. It is a poll and not a deadline: the store counts how many Análises
// started in the window but not when the oldest of them started, so the exact instant
// the window frees is not knowable from here.
const hourlyDeferral = 5 * time.Minute

// leaseRenewals is how many times the lease is renewed inside one TTL. Renewing at a
// third of the deadline survives a missed renewal without the item being taken over
// mid-analysis.
const leaseRenewals = 3

type Deps struct {
	Store     *store.Store
	Agent     domain.Agent
	Tracker   domain.Tracker
	Workspace domain.Workspace
	SentryAPI domain.SentryAPI
	Clock     domain.Clock
	Logger    *slog.Logger
	Owner     string // identifies this process in the lease; empty means hostname+pid

	// SessionWorkDir is passed straight to the Analyst: where the clone is visible
	// from inside the session, when that is not the host path.
	SessionWorkDir string
}

// Pool drains the queue. One per process.
type Pool struct {
	store     *store.Store
	agent     domain.Agent
	tracker   domain.Tracker
	workspace domain.Workspace
	sentry    domain.SentryAPI
	clock     domain.Clock
	log       *slog.Logger
	owner     string
	sessionIn string
}

func New(d Deps) (*Pool, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("worker: o Pool precisa de um Store")
	case d.Agent == nil:
		return nil, errors.New("worker: o Pool precisa de um Agent")
	case d.Tracker == nil:
		return nil, errors.New("worker: o Pool precisa de um Tracker")
	case d.Workspace == nil:
		return nil, errors.New("worker: o Pool precisa de um Workspace")
	}

	p := &Pool{
		store:     d.Store,
		agent:     d.Agent,
		tracker:   d.Tracker,
		workspace: d.Workspace,
		sentry:    d.SentryAPI,
		clock:     d.Clock,
		log:       d.Logger,
		owner:     strings.TrimSpace(d.Owner),
		sessionIn: d.SessionWorkDir,
	}
	if p.clock == nil {
		p.clock = domain.SystemClock{}
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.owner == "" {
		p.owner = defaultOwner()
	}
	return p, nil
}

// sane replaces the two operating numbers whose zero value is a defect the Pool would
// otherwise act on silently: a concurrency below one claims nothing at all, and a
// lease that expires the millisecond it is taken means every worker steals every item
// from every other worker, which is double analysis and two issues per Ocorrência.
//
// config.Apply now refuses both on the way in, so nothing an operator types reaches
// here broken. This stays as the guard for a Tunables built in code — a zero value
// handed straight to worker.New — which no validation on the command line can cover.
// The Analyst guards its own three numbers the same way.
func sane(t config.Tunables) config.Tunables {
	def := config.Defaults()
	if t.Concurrency < 1 {
		t.Concurrency = def.Concurrency
	}
	if t.LeaseTTL <= 0 {
		t.LeaseTTL = def.LeaseTTL
	}
	return t
}

// defaultOwner names this process in the lease. It only has to be unique among the
// processes touching the same database; the deadline is what makes a dead owner
// harmless.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "desconhecido"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// Run drains until ctx is cancelled, then lets the in-flight Análises finish or
// releases them.
//
// A cancelled context is a clean stop and not an error: `serve` cancels this on
// SIGTERM, which is how every Dokku deploy ends.
func (p *Pool) Run(ctx context.Context) error {
	p.log.Info("worker draining the queue", "owner", p.owner)
	for {
		if ctx.Err() != nil {
			p.log.Info("worker stopped", "owner", p.owner)
			return nil
		}

		_, claimed, err := p.drain(ctx)
		if err != nil && ctx.Err() == nil {
			p.log.Error("queue cycle failed", "error", err)
		}
		if ctx.Err() != nil {
			p.log.Info("worker stopped", "owner", p.owner)
			return nil
		}
		if claimed > 0 && err == nil {
			// There was work; look again at once instead of idling for five seconds
			// with a queue that may still be full.
			continue
		}
		if err := p.clock.Sleep(ctx, pollInterval); err != nil {
			p.log.Info("worker stopped", "owner", p.owner)
			return nil
		}
	}
}

// DrainOnce takes one pass over the queue and returns how many items it finished.
// The integration seam and `occurrence replay` use it to drain deterministically.
//
// "Finished" means reached an ending — publicada, encerrada or falhou — and had its
// queue item closed. An item that went back for another attempt, or that is waiting
// out its Projeto's hourly cap, is not finished and is not lost either: it is in the
// queue with a later available_at, and the pass that follows the wait picks it up.
func (p *Pool) DrainOnce(ctx context.Context) (int, error) {
	finished, _, err := p.drain(ctx)
	return finished, err
}

func (p *Pool) drain(ctx context.Context) (finished, claimed int, err error) {
	// The daemon re-reads configuration on every queue cycle, not at startup
	//: `dokku run ... config set` writes and the next cycle already obeys
	// it, with no restart and no blind window.
	tun, err := p.store.Tunables(ctx)
	if err != nil {
		// A number the store cannot read must not stop the queue. Returning here would
		// abandon every Ocorrência already accepted for as long as the bad row stands —
		// the whole system frozen by one unparseable string — so the cycle runs on the
		// defaults and says so, loudly, every five seconds.
		p.log.Error("números da operação ilegíveis no banco: ciclo seguindo com os padrões", "error", err)
		tun = config.Defaults()
	}
	tun = sane(tun)

	analyst, err := NewAnalyst(AnalystDeps{
		Journal:        p.store,
		Agent:          p.agent,
		Tracker:        p.tracker,
		Workspace:      p.workspace,
		SentryAPI:      p.sentry,
		Clock:          p.clock,
		Tunables:       tun,
		Logger:         p.log,
		SessionWorkDir: p.sessionIn,
	})
	if err != nil {
		return 0, 0, err
	}

	var (
		wg    sync.WaitGroup
		done  atomic.Int64
		slots = make(chan struct{}, tun.Concurrency)
		// seen guards against a pass that claims the same item twice. An item released
		// with no wait would otherwise be taken again immediately, and the pass would
		// never end.
		seen = map[int64]bool{}
	)

	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			// Shutdown while every slot is busy. Stop claiming; the in-flight
			// Análises release what they hold below.
		}
		if ctx.Err() != nil {
			break
		}

		item, ok, cerr := p.store.Claim(ctx, p.owner, p.clock.Now(), tun.LeaseTTL)
		if cerr != nil {
			<-slots
			err = cerr
			break
		}
		if !ok {
			<-slots
			break
		}
		if seen[item.ID] {
			// The claim predicate handed us something one of our own goroutines is
			// running right now. Stop claiming — but do NOT release it. The goroutine
			// holding it is mid-Análise and will release it with its backoff, or close
			// it; releasing here strips the lease from under it, so its own Release
			// fails with ErrLeaseLost, the backoff is never written down, and the item
			// becomes takeable at once by anybody, including a second worker. That is a
			// double Análise and two issues for one Ocorrência — the loss of a retry
			// wait dressed up as a safety net.
			<-slots
			p.log.Error("the queue handed back an item this pass is already running",
				"item", item.ID, "analise", item.AnalysisID, "tentativa", item.Attempts)
			break
		}
		seen[item.ID] = true
		claimed++

		wg.Add(1)
		go func(item store.QueueItem) {
			defer wg.Done()
			defer func() { <-slots }()
			if p.process(ctx, analyst, tun, item) {
				done.Add(1)
			}
		}(item)
	}

	wg.Wait()
	return int(done.Load()), claimed, err
}

// process runs one claimed item to its ending, or puts it back. It returns true only
// when the queue item was closed.
func (p *Pool) process(ctx context.Context, analyst *Analyst, tun config.Tunables, item store.QueueItem) (done bool) {
	log := p.log.With("analise", item.AnalysisID, "item", item.ID, "tentativa", item.Attempts)

	// One Análise blowing up must not take the daemon — and the other Análise in
	// flight — down with it. The item goes back to the queue and the attempt counts,
	// so a payload that panics every time ends as an unanalysed issue instead of a
	// crash loop that never records anything.
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic while conducting the Análise", "panic", r)
			p.giveBack(context.WithoutCancel(ctx), item, p.clock.Now().Add(tun.BackoffFor(item.Attempts)))
			done = false
		}
	}()

	an, err := p.store.Analysis(ctx, item.AnalysisID)
	if err != nil {
		// The row cannot be read right now. Closing the item over that would lose the
		// Ocorrência, so it waits and comes back.
		log.Error("could not read the Análise behind the queue item", "error", err)
		p.giveBack(ctx, item, p.clock.Now().Add(tun.BackoffFor(item.Attempts)))
		return false
	}

	if p.overHourlyCap(ctx, item, an, tun, log) {
		return false
	}

	// How many attempts have been spent arrives inside the Análise: the Analyst
	// decides between another attempt and the unanalysed issue, and never learns that
	// a queue exists.
	an.Attempts = item.Attempts

	sessionCtx, stop := context.WithCancel(ctx)
	var extending sync.WaitGroup
	extending.Add(1)
	go func() {
		defer extending.Done()
		p.keepLease(sessionCtx, item, tun, log)
	}()
	// Also deferred, so that a panic inside the Análise cannot leave a goroutine
	// renewing the lease of an item nobody is working on any more.
	defer func() {
		stop()
		extending.Wait()
	}()

	an, err = analyst.Analyse(sessionCtx, an)
	stop()
	extending.Wait()

	switch {
	case err == nil:
		// Closed through a context that cancellation does not reach, and before the
		// shutdown case is even considered: an Análise that reached its ending during
		// a deploy is finished work, and leaving its item open would hand it to the
		// next process to run all over again.
		if derr := p.store.MarkDone(context.WithoutCancel(ctx), item.ID, p.owner, p.clock.Now()); derr != nil {
			// The lease lapsed and somebody else owns the item now. The Análise is
			// recorded either way, and the guard at the top of Analyse stops the new
			// owner from opening a second issue.
			log.Warn("could not close the queue item", "error", derr)
		}
		return true

	case ctx.Err() != nil:
		// Dokku is restarting. Hand the item back cleanly rather than waiting out the
		// lease: it becomes takeable the moment the next process looks.
		p.releaseOnShutdown(ctx, item, an, log)
		return false

	default:
		wait := tun.BackoffFor(item.Attempts)
		var later *retryLater
		if errors.As(err, &later) && later.after > 0 {
			wait = later.after
		}
		log.Warn("the Análise goes back to the queue", "espera", wait, "error", err)
		p.giveBack(ctx, item, p.clock.Now().Add(wait))
		return false
	}
}

// overHourlyCap makes the excess WAIT. It is never discarded — high load becomes
// delay, not loss (User Story 22).
func (p *Pool) overHourlyCap(ctx context.Context, item store.QueueItem, an domain.Analysis, tun config.Tunables, log *slog.Logger) bool {
	if an.ProjectID == 0 || tun.PerProjectHourly <= 0 {
		return false
	}
	now := p.clock.Now()
	started, err := p.store.AnalysesStartedSince(ctx, an.ProjectID, now.Add(-time.Hour))
	if err != nil {
		log.Error("could not measure the Projeto's hourly window", "error", err)
		return false
	}
	if started < tun.PerProjectHourly {
		return false
	}
	log.Info("Projeto at its hourly cap: the Ocorrência waits",
		"projeto", an.ProjectID, "iniciadas", started, "teto", tun.PerProjectHourly)
	p.deferItem(ctx, item, now.Add(hourlyDeferral), log)
	return true
}

// giveBack releases an item so somebody can take it again. A zero availableAt leaves
// the column where it was, which means "takeable as soon as the hold is dropped".
//
// Like every other write that hands an item back, it goes through a context that
// cancellation cannot reach. SIGTERM can land between the caller deciding to release
// and this statement running, and a Release that fails with "context canceled" leaves
// the item leased for the whole TTL with nobody working on it — twenty-five minutes of
// an Ocorrência stuck showing a state no process is in.
func (p *Pool) giveBack(ctx context.Context, item store.QueueItem, availableAt time.Time) {
	if err := p.store.Release(context.WithoutCancel(ctx), item.ID, p.owner, availableAt); err != nil {
		p.log.Warn("could not hand the queue item back",
			"item", item.ID, "analise", item.AnalysisID, "error", err)
	}
}

// releaseOnShutdown hands an interrupted item back whole.
//
// It writes through a context that cancellation does not reach, because the ordinary
// one is already cancelled — that is why we are here — and every write through it
// would fail, leaving the item leased for the whole TTL and the Análise stuck showing
// a state nobody is running.
func (p *Pool) releaseOnShutdown(ctx context.Context, item store.QueueItem, an domain.Analysis, log *slog.Logger) {
	bg := context.WithoutCancel(ctx)

	switch an.State {
	case domain.StateTriaging, domain.StateAnalyzing, domain.StateFailing:
		an.State = domain.StateQueued
		if err := p.store.UpdateAnalysis(bg, an, p.clock.Now()); err != nil {
			log.Warn("could not record the interrupted Análise as queued again", "error", err)
		}
	}
	if err := p.store.Release(bg, item.ID, p.owner, time.Time{}); err != nil {
		log.Warn("could not hand the interrupted item back", "error", err)
		return
	}
	log.Info("Análise interrupted by the shutdown and back in the queue")
}

// keepLease holds the claim while a long session runs. A Diagnóstico is allowed
// twenty minutes and the lease lasts twenty-five: without this, a session that uses
// its whole budget would be taken over by another worker mid-flight and analysed
// twice.
func (p *Pool) keepLease(ctx context.Context, item store.QueueItem, tun config.Tunables, log *slog.Logger) {
	interval := tun.LeaseTTL / leaseRenewals
	if interval <= 0 {
		return
	}
	for {
		if err := p.clock.Sleep(ctx, interval); err != nil || ctx.Err() != nil {
			return
		}
		if err := p.store.ExtendLease(ctx, item.ID, p.owner, p.clock.Now(), tun.LeaseTTL); err != nil {
			if errors.Is(err, store.ErrLeaseLost) {
				// Somebody else owns the item now, or it is already closed. Renewing it
				// back would mean two workers believing they hold the same Análise.
				log.Warn("the lease is gone: another worker owns this Análise now", "error", err)
				return
			}
			// Anything else says nothing about ownership — a busy database, a blip. Giving
			// up on the FIRST such error would stop renewing for the rest of a twenty-minute
			// Diagnóstico and hand the item to the next worker mid-session, which is the
			// double analysis this goroutine exists to prevent.
			log.Warn("could not extend the lease; will try again at the next renewal", "error", err)
		}
	}
}

// deferItem puts an item back with a later availableAt AND gives the attempt back.
// Store.Defer is what does both: Claim raises `attempts` unconditionally, so an
// Ocorrência held back by the hourly cap would otherwise spend one of its attempts
// without a single session having run, and four deferrals would send a perfectly
// healthy Ocorrência down the failure path. Waiting is not attempting.
//
// Written through a context cancellation cannot reach, like every other hand-back: a
// SIGTERM landing here would otherwise leave the item leased for the whole TTL with
// nobody working on it.
func (p *Pool) deferItem(ctx context.Context, item store.QueueItem, availableAt time.Time, log *slog.Logger) {
	if err := p.store.Defer(context.WithoutCancel(ctx), item.ID, p.owner, availableAt); err != nil {
		log.Error("could not hold the Ocorrência back for the hourly cap", "error", err)
	}
}
