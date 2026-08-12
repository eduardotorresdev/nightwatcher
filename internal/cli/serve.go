package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/eduardotorresdev/nightwatcher/internal/agentcli"
	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
	"github.com/eduardotorresdev/nightwatcher/internal/prompts"
	"github.com/eduardotorresdev/nightwatcher/internal/sentryapi"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
	"github.com/eduardotorresdev/nightwatcher/internal/tracker"
	"github.com/eduardotorresdev/nightwatcher/internal/worker"
	"github.com/eduardotorresdev/nightwatcher/internal/workspace"
)

// healthzPath is what Dokku's CHECKS polls. It and the webhook are the entire public
// surface: the mux has no catch-all, so anything else is a 404 and there is no
// administrative API to forget to protect.
const healthzPath = "/healthz"

// shutdownTimeout is how long the HTTP server is given to finish the deliveries already
// in flight before the process stops waiting for them. Dokku sends SIGTERM on every
// deploy, so this path runs constantly and has to be boring: a delivery cut short is
// one Sentry redelivers five minutes later, while a deploy that hangs is an outage.
const shutdownTimeout = 20 * time.Second

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Sobe o daemon: webhook do Sentry e worker de Análises",
		Long: "Sobe o daemon.\n\n" +
			"Serve o webhook do Sentry e " + healthzPath + ", e nada mais — a superfície pública é\n" +
			"só a entrega. O worker drena a fila em paralelo, relendo a configuração a cada\n" +
			"ciclo, de modo que `dokku run ... config set` vale no ciclo seguinte sem restart.\n\n" +
			"SIGTERM e SIGINT desligam limpo: para de aceitar, drena o que já está em voo e\n" +
			"devolve à fila as Análises em andamento. É o que o Dokku faz a cada deploy.",
		Args: cobra.NoArgs,
		RunE: runServe,
	}
}

func runServe(cmd *cobra.Command, _ []string) error {
	// JSON on stdout because Dokku collects stdout, and `dokku logs` is the operator's
	// only window at three in the morning.
	log := slog.New(slog.NewJSONHandler(cmd.OutOrStdout(), &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Loudly, at startup, naming exactly what is missing. Discovering an absent
	// credential at the first Ocorrência at 3 a.m. is the worst possible moment, and a
	// daemon that came up without one would answer 401 to every delivery Sentry sends.
	secrets := config.LoadSecrets()
	if missing := secrets.Missing(); len(missing) > 0 {
		return fmt.Errorf(
			"o daemon não sobe sem estas variáveis de ambiente: %s.\n"+
				"Segredo só vem do ambiente, nunca do banco: defina-as com `dokku config:set` e suba de novo",
			strings.Join(missing, ", "))
	}

	ctx, cancel := context.WithCancel(baseContext(cmd))
	defer cancel()
	watchSignals(ctx, cancel, log)

	paths := config.LoadPaths()
	path := dbPath(cmd)
	// Setup and not Open: the migration is inside, so no caller can forget it and meet
	// it as a missing table at the first delivery.
	st, err := store.Setup(ctx, path, clock.Now())
	if err != nil {
		return fmt.Errorf("abrindo o banco %s: %w", path, err)
	}
	defer st.Close()

	// The Destino's team is configuration, not a credential, so it lives in the settings
	// table beside the operating numbers — with LINEAR_TEAM overriding it for a one-off
	// run. Without it there is nowhere to write the result of an Análise, which is a
	// startup failure and not something to discover on the first publicação.
	team, err := identity(ctx, st, settingLinearTeam, envLinearTeam)
	if err != nil {
		return fmt.Errorf("lendo o time do Destino: %w", err)
	}
	if team == "" {
		return fmt.Errorf("o time do Destino não está configurado: `nightwatcher config set %s <uuid|chave>` (ou %s no ambiente)",
			settingLinearTeam, envLinearTeam)
	}

	r := newRemote(cmd)
	world, err := newWorld(worldSpec{
		secrets:     secrets,
		paths:       paths,
		logger:      log,
		team:        team,
		linear:      r.linear,
		linearToken: r.linearToken,
		sentry:      r.sentry,
	})
	if err != nil {
		return err
	}

	// Only now the global default, so that the packages which fall back to slog.Default()
	// land in the same stream instead of printing text beside it. Not earlier: everything
	// above returns its failure as an error, which the command line prints, and a
	// subcommand has no business rewiring the process's logger before it has decided it
	// is going to run.
	slog.SetDefault(log)

	return runDaemon(ctx, daemonDeps{
		Store:          st,
		Secret:         secrets.SentryClientSecret,
		Agent:          world.agent,
		Tracker:        world.tracker,
		Workspace:      world.workspace,
		SentryAPI:      world.sentry,
		SessionWorkDir: world.sessionDir,
		Clock:          clock,
		Logger:         log,
		Addr:           net.JoinHostPort("", config.Port()),
	})
}

func baseContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// watchSignals turns SIGTERM and SIGINT into a cancelled context, and says in the log
// which one arrived. Naming the signal matters: a deploy (SIGTERM from Dokku) and a
// container the kernel is killing look identical in a log that only says "stopping".
func watchSignals(ctx context.Context, cancel context.CancelFunc, log *slog.Logger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sig)
		select {
		case s := <-sig:
			log.Info("sinal recebido: desligando", "sinal", s.String())
			cancel()
		case <-ctx.Done():
		}
	}()
}

// daemonDeps is the whole daemon with every adapter handed in. `serve` builds the real
// ones out of the environment; the integration test builds doubles and asks for a
// random port. Nothing in runDaemon reads the environment, which is what makes the
// daemon exercisable end to end instead of only in production.
type daemonDeps struct {
	Store *store.Store

	// Secret is the Sentry integration's Client Secret, and the only thing standing
	// between the public endpoint and forged Ocorrências.
	Secret string

	Agent     domain.Agent
	Tracker   domain.Tracker
	Workspace domain.Workspace
	SentryAPI domain.SentryAPI

	// SessionWorkDir is where the clone is visible from inside the session, which is not
	// the host path when the session runs in a container.
	SessionWorkDir string

	Clock  domain.Clock
	Logger *slog.Logger

	// Addr is host:port. A port of 0 asks the kernel for a free one, which is how the
	// test avoids fighting over 5000 with whatever else is on the machine.
	Addr string

	// Listening is called once the socket is bound, with the address it landed on. It is
	// the only way to learn the port when Addr asked for 0; production leaves it nil.
	Listening func(net.Addr)

	// ShutdownTimeout overrides how long the drain is given. Zero means shutdownTimeout.
	ShutdownTimeout time.Duration
}

// runDaemon raises the two halves of the nightwatcher and keeps them up until the
// context is cancelled.
//
// The shutdown has an order and the order is the point: stop accepting, drain the
// deliveries already inside the HTTP server, and only THEN cancel the worker so it
// hands back what it is holding. Cancelling the worker first would have it release
// Análises while the endpoint was still writing new ones, and a delivery accepted
// during the drain would sit in the queue with nobody left to claim it until the next
// process starts. Either way nothing is lost — the queue is durable and the lease
// expires — but this order is the one that costs no latency.
func runDaemon(ctx context.Context, d daemonDeps) error {
	if d.Clock == nil {
		d.Clock = domain.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.ShutdownTimeout <= 0 {
		d.ShutdownTimeout = shutdownTimeout
	}

	hook, err := ingest.New(ingest.Deps{
		Store:  d.Store,
		Clock:  d.Clock,
		Secret: d.Secret,
		Logger: d.Logger,
	})
	if err != nil {
		return err
	}
	pool, err := worker.New(worker.Deps{
		Store:          d.Store,
		Agent:          d.Agent,
		Tracker:        d.Tracker,
		Workspace:      d.Workspace,
		SentryAPI:      d.SentryAPI,
		Clock:          d.Clock,
		Logger:         d.Logger,
		SessionWorkDir: d.SessionWorkDir,
	})
	if err != nil {
		return err
	}

	// No catch-all pattern: the webhook and the health check are registered by exact
	// path and everything else is a 404. There is no administrative API — inspection is
	// `dokku run`, against the same file, over the same SQLite.
	mux := http.NewServeMux()
	mux.Handle(ingest.WebhookPath, hook)
	mux.HandleFunc("GET "+healthzPath, healthz)

	// Before the socket is open and before anything is claimed: no delivery can be
	// half-written while this runs, and nothing of this process holds a lease yet.
	if n, err := sweepStranded(ctx, d.Store, d.Clock.Now(), d.Logger); err != nil {
		// A sweep that could not run is not a reason to refuse to serve: the queue is
		// still durable and everything already in it is still analysed.
		d.Logger.Error("a varredura de Ocorrências órfãs falhou", "recolocadas", n, "error", err)
	} else if n > 0 {
		d.Logger.Warn("Ocorrências órfãs recolocadas na fila", "quantidade", n)
	}

	ln, err := net.Listen("tcp", d.Addr)
	if err != nil {
		return fmt.Errorf("abrindo a porta %s: %w", d.Addr, err)
	}
	if d.Listening != nil {
		d.Listening(ln.Addr())
	}

	srv := &http.Server{
		Handler: mux,
		// Sentry considers a webhook slower than a second a failure and redelivers, so
		// these ceilings are about an abusive caller and not about a slow Sentry: the
		// handler itself never waits on anything but SQLite.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.Logger.Handler(), slog.LevelWarn),
	}

	// The worker's context is deliberately NOT a child of the caller's: it is cancelled
	// by hand, after the HTTP drain, so that the order above is real.
	workerCtx, stopWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorker()

	workerDone := make(chan error, 1)
	go func() { workerDone <- pool.Run(workerCtx) }()

	serveDone := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			// The ordinary end of a graceful shutdown, not a failure.
			err = nil
		}
		serveDone <- err
	}()

	d.Logger.Info("nightwatcher no ar",
		"endereco", ln.Addr().String(), "webhook", ingest.WebhookPath, "health", healthzPath)

	var (
		listenerErr  error
		listenerDown bool
	)
	select {
	case <-ctx.Done():
	case listenerErr = <-serveDone:
		listenerDown = true
		// The listener died on its own — the port was taken from under us, or Accept
		// failed for good. That is a real error and the exit code has to say so.
		if listenerErr != nil {
			d.Logger.Error("o servidor HTTP caiu", "error", listenerErr)
		}
	}

	// Shutdown closes the listener first, so Serve has already returned by the time this
	// does, whether or not the drain finished inside the deadline.
	drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), d.ShutdownTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		// A drain that ran out of time is not a failed deploy: Sentry redelivers what it
		// did not hear an answer to, and every accepted Ocorrência is already in SQLite.
		d.Logger.Warn("entregas em voo não terminaram dentro do prazo do desligamento",
			"prazo", d.ShutdownTimeout, "error", err)
	}
	// Serve has already returned — Shutdown closes the listener before it waits — but
	// its value is taken only if the select above did not take it. Deciding that on
	// `listenerErr == nil` instead is a shutdown that never ends: the channel carries
	// exactly one value, and a listener that came back nil (Serve answering
	// ErrServerClosed for a Close this function did not ask for) would leave the daemon
	// blocked here for good, holding every lease it is holding, with SIGTERM already
	// spent and only SIGKILL left.
	if !listenerDown {
		listenerErr = <-serveDone
	}

	// Now the worker: cancelling it makes every Análise in flight hand its queue item
	// back whole, so the next process — the new Dokku container — finds them takeable
	// instead of waiting out the lease.
	stopWorker()
	workerErr := <-workerDone

	d.Logger.Info("nightwatcher parado")
	return errors.Join(listenerErr, workerErr)
}

// sweepStranded puts back in line every Ocorrência that is recorded, unfinished, and
// not in the queue.
//
// Boot is the right moment and the only one needed: it is when a crash has just
// happened, and it is the one instant at which no worker of this process holds
// anything, so an item seen as unqueued really is unqueued. The arrival path records and
// queues in a single transaction, so nothing it accepts can land here; this is the
// backstop for everything else — a row left behind by a killed process, a hand-edited
// database, a bug nobody has found yet. Sentry's redelivery would not rescue those:
// dedup recognises the identity and answers "duplicada", which is dedup doing its job
// and losing the Ocorrência anyway.
func sweepStranded(ctx context.Context, st *store.Store, now time.Time, log *slog.Logger) (int, error) {
	back, err := st.EnqueueStranded(ctx, now)
	for _, id := range back {
		log.Warn("Ocorrência estava gravada e fora da fila: recolocada", "analise", id)
	}
	if err != nil {
		// Some went back and some did not. The ones that did are still worth reporting,
		// and the caller decides that a partial sweep is no reason to refuse to serve.
		return len(back), fmt.Errorf("recolocando Ocorrências fora da fila: %w", err)
	}
	return len(back), nil
}

// healthz is what Dokku's CHECKS decides a deploy on.
//
// It deliberately does not touch SQLite. The question Dokku is asking is "is this
// container accepting requests", and answering it with a query would make a transient
// write lock — the daemon and a `dokku run` container share the file — fail a deploy
// that has nothing wrong with it. A database that is really unreachable shows up in the
// webhook's 5xx and in the log, which is where it belongs.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// outsideWorld is everything the analysis pipeline reaches that is not SQLite: the
// Claude session in its container, the read-only clone, the Sentry tags call, and the
// Destino.
type outsideWorld struct {
	agent     domain.Agent
	workspace domain.Workspace
	sentry    domain.SentryAPI

	// tracker is nil when no Destino was asked for, which is the case of the two
	// `occurrence` commands: they write to a Spy and never reach Linear at all.
	tracker domain.Tracker

	// sessionDir is where the clone is visible from INSIDE the session. DockerRunner
	// mounts the host path at its MountPoint and nothing exported says which of the two
	// the prompt should name, so the wiring is the only place that knows both halves —
	// and getting it wrong points every Diagnóstico at a directory that does not exist
	// inside its own container.
	sessionDir string
}

// worldSpec is what building that world takes.
type worldSpec struct {
	secrets config.Secrets
	paths   config.Paths
	logger  *slog.Logger

	// team is the Linear team of the Destino. Empty means no Destino is wanted, and no
	// Linear credential is then required: `occurrence simulate` must run on a machine
	// that has none.
	team string

	// The endpoints, so a test can aim the Destino at an httptest server. Production
	// leaves them at the package defaults through the hidden flags.
	linear      string
	linearToken string
	sentry      string
}

// newWorld exists as a package variable for the same reason the four hidden endpoint
// flags do: `serve`, `occurrence replay` and `occurrence simulate` are the three
// commands that raise a container with a Claude session inside it and clone a
// repository over the network, and a test that really did either would not be a test.
// Production never replaces it.
var newWorld = productionWorld

func productionWorld(spec worldSpec) (outsideWorld, error) {
	// One ephemeral container per Análise: unprivileged user, read-only root filesystem
	// with a writable tmpfs at HOME, the clone mounted read-only, egress restricted, and
	// an environment built from scratch holding only HOME and the OAuth token — no
	// Linear, Sentry or GitHub credential anywhere inside. The Ocorrência's
	// text is written by an end user, so the perimeter is what makes the worst case a
	// bad issue.
	docker := agentcli.DefaultDockerOptions()
	w := outsideWorld{
		agent: agentcli.New(agentcli.Options{
			Runner:       agentcli.DockerRunner{Opts: docker},
			Token:        spec.secrets.ClaudeOAuthToken,
			SystemPrompt: prompts.System(),
			Clock:        clock,
		}),
		sessionDir: docker.MountPoint,
	}

	clones, err := workspace.New(spec.paths.Clones, spec.secrets.GitHubToken)
	if err != nil {
		return outsideWorld{}, fmt.Errorf("preparando a raiz dos clones em %s: %w", spec.paths.Clones, err)
	}
	w.workspace = clones

	// Not knowing the Ocorrência's environment is a normal state that never fails an
	// Análise, so an absent token leaves this nil instead of refusing to start.
	if strings.TrimSpace(spec.secrets.SentryToken) != "" {
		c := sentryapi.New(spec.secrets.SentryToken)
		if strings.TrimSpace(spec.sentry) != "" {
			c.BaseURL = spec.sentry
		}
		w.sentry = c
	}

	if strings.TrimSpace(spec.team) != "" {
		opts := tracker.Options{
			Secrets:       spec.secrets,
			Endpoint:      spec.linear,
			TokenEndpoint: spec.linearToken,
			Logger:        spec.logger,
			Clock:         clock,
		}
		// A team is either the UUID or the human key ("ACME"); the Client resolves the
		// key with one extra query and refuses to be built without one or the other.
		if looksLikeUUID(spec.team) {
			opts.TeamID = spec.team
		} else {
			opts.TeamKey = spec.team
		}
		destino, err := tracker.New(opts)
		if err != nil {
			return outsideWorld{}, fmt.Errorf("montando o Destino no Linear: %w", err)
		}
		w.tracker = destino
	}

	return w, nil
}
