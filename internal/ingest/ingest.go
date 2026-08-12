// Package ingest is the public endpoint of the nightwatcher and the only exposed
// surface the whole system has. Four rules shape everything in here.
//
// # The signature is over the raw bytes
//
// The body is read once, bounded, into a []byte, and the HMAC covers exactly those
// bytes. Decoding before verifying is the classic way to get this wrong: Sentry signs
// the string it serialised, and re-encoding it in Go produces a different one. The
// same bytes are then kept on the Ocorrência, because the failure path
// publishes them and `occurrence replay` re-reads them.
//
// # Nothing is analysed inside the request cycle
//
// Sentry treats a webhook slower than one second as a timeout and redelivers it, three
// times, five minutes apart. So the handler validates, writes and answers; every
// judgement about the Ocorrência happens later, in the worker, off the queue.
//
// # Every refusal is recorded, with exactly one exception
//
// A refusal is an Análise in state `recusada` with its Refusal set, which is what keeps
// it visible in `analysis list` and what stops the defect that killed the predecessor:
// an Ocorrência vanishing in silence. The exception is a bad signature — recording it
// would let an unauthenticated caller write rows into the database at will, turning the
// public endpoint into an unbounded INSERT. That one is recorded in the log only.
//
// # The status code is a contract with Sentry's retry machinery
//
// It is not decoration. 401 for a bad signature and 4xx for a body we could not read:
// retrying produces the same answer. 2xx for every handled outcome, refusals included,
// because asking Sentry to redeliver an Ocorrência we have already decided about would
// go on forever. 5xx only for a genuine internal failure — the database being
// unreachable — where the retry five minutes later is the cheapest recovery there is.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/config"
	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// WebhookPath is where the Sentry integration points.
const WebhookPath = "/webhooks/sentry"

// maxBodyBytes bounds one delivery. A Sentry issue payload is a few kilobytes; a
// mebibyte is generous. The bound exists because the body is read before the signature
// can be checked, and an unauthenticated caller must not be able to decide how much
// memory the daemon spends on them.
const maxBodyBytes = 1 << 20

// Deps is everything the endpoint needs. There is no HTTP client and no Tracker here on
// purpose: nothing in the request cycle talks to anything but SQLite.
type Deps struct {
	Store *store.Store
	Clock domain.Clock

	// Secret is the Sentry internal integration's Client Secret. Empty is a
	// configuration error, not a permissive mode.
	Secret string

	// Logger may be nil, which means slog.Default().
	Logger *slog.Logger
}

// Handler is the webhook endpoint.
type Handler struct {
	store  *store.Store
	clock  domain.Clock
	secret string
	log    *slog.Logger
}

// New builds the endpoint, refusing a configuration that could only fail later, at the
// first delivery, where nobody is looking.
func New(d Deps) (*Handler, error) {
	if d.Store == nil {
		return nil, errors.New("ingestão exige um store: sem ele nada do que chega pode ser gravado")
	}
	if d.Clock == nil {
		return nil, errors.New("ingestão exige um Clock: o handler nunca chama time.Now()")
	}
	if d.Secret == "" {
		return nil, fmt.Errorf("ingestão exige %s: sem o Client Secret nenhuma entrega pode ser autenticada", config.EnvSentryClientSecret)
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: d.Store, clock: d.Clock, secret: d.Secret, log: log}, nil
}

// delivery is what one request has established about itself so far. It grows as the
// handler learns more, and it is what a refusal is recorded from.
type delivery struct {
	now       time.Time
	occ       domain.Occurrence
	projectID int64
	sourceID  int64
}

// storeTimeout bounds the work that outlives the request, so that a wedged database
// cannot pile up goroutines nobody is waiting for.
//
// It has to be comfortably longer than the whole chain, not than one statement: one
// arrival runs up to eight of them, each already bounded by store.BusyTimeout, and a
// deadline that expires halfway through would strand the Ocorrência in exactly the way
// this context exists to prevent.
const storeTimeout = 2 * time.Minute

// storeContext is the context every database call in the arrival path uses, and it is
// deliberately NOT the request's.
//
// Sentry hangs up on a delivery slower than one second, and Go cancels r.Context() the
// instant the client disconnects. Reusing it here means that the moment Sentry gives up
// — precisely when the daemon is slow, which is precisely when deliveries pile up —
// every store call fails at once: an authenticated Ocorrência answered 500 with nothing
// written down, or worse, recorded and never enqueued. Sentry redelivers three times and
// then stops, so a handler that loses the race against its own caller loses the
// Ocorrência for good. That is the defect that killed the predecessor, arriving through
// the one door that is supposed to prevent it.
//
// Values survive (anything a middleware put on the request), cancellation does not.
func storeContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), storeTimeout)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "método não suportado", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := storeContext(r)
	defer cancel()

	body, ok := h.readBody(w, r)
	if !ok {
		return
	}

	// The one refusal that leaves no trace in the database, and the reason is not
	// tidiness: an unauthenticated caller who could make the endpoint write a row could
	// fill the disk with forged Ocorrências. The log is where a forged delivery belongs,
	// and the signature itself is never logged.
	if !Verify(h.secret, body, []byte(r.Header.Get(HeaderSignature))) {
		h.log.Warn("entrega recusada: assinatura inválida",
			"remote", r.RemoteAddr,
			"resource", r.Header.Get(HeaderResource),
			"bytes", len(body))
		http.Error(w, "assinatura inválida", http.StatusUnauthorized)
		return
	}

	d := delivery{now: h.clock.Now()}

	// The action comes from the envelope; Sentry has no action header. The resource
	// does, and Parse is where it is checked, so no path can read a payload of one
	// shape as if it were another.
	occ, err := Parse(body, r.Header.Get(HeaderResource), "")
	if err != nil {
		// Authentic, but not readable as an Ocorrência. It is recorded with no Sentry
		// identity at all — the dedup index is partial precisely so rows like this can
		// coexist — and answered 2xx, because a redelivery of the same bytes parses the
		// same way.
		reason := domain.RefusedMalformed
		if errors.Is(err, ErrUnsupportedResource) {
			reason = domain.RefusedIgnoredEvent
		}
		d.occ = domain.Occurrence{RawBody: body, ReceivedAt: d.now}
		h.refuse(ctx, w, d, reason, err.Error())
		return
	}
	occ.ReceivedAt = d.now
	d.occ = occ

	// Dedup before anything else that writes, so that a repeated delivery of an
	// Ocorrência already refused is recognised as the repeat it is instead of colliding
	// with its own record on the way in.
	if id, seen, err := h.store.OccurrenceRecorded(ctx, occ); err != nil {
		h.fail(w, "verificando ocorrência já vista", err)
		return
	} else if seen {
		h.reportDuplicate(w, id, occ)
		return
	}

	project, source, err := h.store.ResolveSource(ctx, occ.SentryProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.refuse(ctx, w, d, domain.RefusedUnknownFonte,
				fmt.Sprintf("projeto Sentry %q não está registrado como Fonte", occ.SentryProjectSlug))
			return
		}
		h.fail(w, "resolvendo a Fonte", err)
		return
	}
	// From here on every refusal carries the Projeto, which is what makes
	// `analysis list --projeto acme` show what was discarded and not only what ran.
	d.projectID, d.sourceID = project.ID, source.ID

	if reason, detail, refused := Refusal(occ); refused {
		h.refuse(ctx, w, d, reason, detail)
		return
	}

	// Read on every delivery, never cached at startup: `dokku run ... config set` has to
	// take effect on the next webhook, with no restart and no blind window.
	tunables, err := h.store.Tunables(ctx)
	if err != nil {
		// One unreadable row in `settings` must not cost an Ocorrência. Answering 5xx
		// here asks Sentry to redeliver, it redelivers three times, gets the same answer
		// three times, and gives up — an authenticated Ocorrência lost to a typo in a
		// number. The only tunable this path reads is the queue ceiling, which is a
		// safety valve and not a correctness requirement, so the arrival goes on under
		// the defaults and the broken row is shouted about on every single delivery
		// until somebody fixes it.
		h.log.Error("números da operação ilegíveis no banco: seguindo com os padrões",
			"erro", err, "issue", occ.IssueID)
		tunables = config.Defaults()
	}
	depth, err := h.store.QueueDepth(ctx)
	if err != nil {
		h.fail(w, "medindo a fila", err)
		return
	}
	if depth >= tunables.MaxQueue {
		// This is a signal, not a normal path. Past the ceiling the arrival rate is a
		// symptom — a bad release, a loop in production, a misconfigured Alert Rule —
		// and no amount of queueing fixes it. Loud in the log, and recorded rather than
		// dropped, so the Ocorrências that were refused are the evidence of when it
		// started.
		h.log.Error("fila acima do teto: entrada recusada",
			"depth", depth, "max_queue", tunables.MaxQueue,
			"projeto", project.Name, "issue", occ.IssueID)
		h.refuse(ctx, w, d, domain.RefusedQueueFull,
			fmt.Sprintf("fila com %d itens, teto %d", depth, tunables.MaxQueue))
		return
	}

	h.accept(ctx, w, d, project.Name)
}

// readBody reads the delivery once, bounded, and answers the request itself when it
// cannot. Both failures are 4xx: a body that overflowed the bound will overflow it
// again, and a body we could not read is a body we cannot authenticate, so nothing
// about it may be written down.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err == nil {
		return body, true
	}

	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		h.log.Warn("entrega recusada: corpo acima do limite",
			"remote", r.RemoteAddr, "limit", maxBodyBytes)
		http.Error(w, "entrega acima do limite", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	h.log.Warn("entrega recusada: corpo ilegível", "remote", r.RemoteAddr, "erro", err)
	http.Error(w, "não foi possível ler a entrega", http.StatusBadRequest)
	return nil, false
}

// Refusal applies the nightwatcher's policy to a parsed Ocorrência: is this one the
// system knows how to handle, or is it refused and recorded on the way in?
//
// It is exported because `nightwatcher occurrence simulate` has to reach the same
// verdict this endpoint would. A simulator that spent a twenty-minute session on an
// `outage` — which the daemon refuses at ingest and never analyses — would be
// calibrating a path that does not exist.
//
// It covers only the refusals that are knowable from the Ocorrência itself. A forged
// signature and a queue over the ceiling are decided by the request and by the state of
// the queue, and neither is visible from here.
func Refusal(occ domain.Occurrence) (domain.RefusalReason, string, bool) {
	if reason, detail, refused := refusedByEvent(occ); refused {
		return reason, detail, true
	}
	return refusedByCategory(occ)
}

// refusedByEvent applies the refusal rules to the action and the substatus.
func refusedByEvent(occ domain.Occurrence) (domain.RefusalReason, string, bool) {
	switch occ.Action {
	case domain.ActionTriggered:
		// An Alert Rule fired. There is no substatus to gate on — the payload does not
		// carry one — so the only questions left about this delivery are its category
		// and whether the group is already on record, and both are asked elsewhere.
		//
		// Gating this on a substatus would refuse every single feedback, because
		// `event_alert` is the ONLY door feedback arrives through.
		return "", "", false

	case domain.ActionCreated:
		// No substatus gate here, and that is load-bearing: a `created` delivery carries
		// whatever substatus the group happens to have, and the recorded
		// defeito-error-volume.json arrives as created + ongoing. The ongoing rule below
		// is gated on `unresolved` for exactly that reason — applying it to `created`
		// would silently discard real Ocorrências.
		return "", "", false

	case domain.ActionUnresolved:
		switch occ.Substatus {
		case domain.SubstatusRegressed, domain.SubstatusEscalating:
			return "", "", false
		default:
			// `ongoing` is the noise the refusal rules decided to ignore. Any other substatus is
			// one nobody has taught the system to read, and the http_client lesson
			// applies: refuse and record, so the next unforeseen value is a listable row
			// and a reversible decision instead of a silent acceptance.
			return domain.RefusedIgnoredEvent,
				fmt.Sprintf("issue.unresolved com substatus %q", occ.Substatus), true
		}

	default:
		return domain.RefusedIgnoredEvent, fmt.Sprintf("action %q", occ.Action), true
	}
}

// refusedByCategory applies the allowlist. It is an allowlist and not an `outage`
// denylist because `http_client` — Sentry's N+1 detector — turned up in live acme data
// and no decision had foreseen it. Anything the system does not know how to handle is
// refused and recorded, so the decision shows up in `analysis list` and can be revisited
// instead of costing a twenty-minute session on something that is not a defect.
func refusedByCategory(occ domain.Occurrence) (domain.RefusalReason, string, bool) {
	switch occ.Category {
	case domain.CategoryError, domain.CategoryFeedback:
		return "", "", false
	case domain.CategoryOutage:
		return domain.RefusedOutage,
			"falha de cron ou uptime não é defeito de aplicação", true
	default:
		// The category that arrived goes in the reason, so the row says which unknown
		// category it was and the next one is a five-second read instead of an
		// investigation. It is spelled `categoria` and not `issueCategory` because the
		// two doors derive it from different fields: `issueCategory` on the issue door,
		// `occurrence.type` on the event_alert one, which names itself in the value.
		return domain.RefusedUnknownCategory,
			fmt.Sprintf("categoria %q", occ.Category), true
	}
}

// accept records the Ocorrência and puts it in line, in one transaction.
//
// One transaction and not three writes, because the gap between recording and queueing
// is the one place an ACCEPTED Ocorrência can be lost. A process that died in that gap
// left a row nobody would ever claim, and Sentry's redelivery could not rescue it: dedup
// recognises the identity and answers "duplicada", so dedup doing exactly its job is
// what loses the Ocorrência. Either both writes land or neither does, and a delivery
// whose transaction did not commit is one Sentry sends again.
func (h *Handler) accept(ctx context.Context, w http.ResponseWriter, d delivery, projectName string) {
	created, err := h.store.CreateQueued(ctx, domain.Analysis{
		ProjectID:  d.projectID,
		SourceID:   d.sourceID,
		Occurrence: d.occ,
		State:      domain.StateQueued,
	}, d.now)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			// Two deliveries of the same Ocorrência arriving together both saw "not
			// recorded" above; the unique index is what actually decides, and losing that
			// race is the ordinary outcome of a system Sentry redelivers into, not an
			// error.
			h.duplicate(ctx, w, d.occ)
			return
		}
		h.fail(w, "gravando e enfileirando a Ocorrência", err)
		return
	}

	h.log.Info("ocorrência enfileirada",
		"analysis", created.ID,
		"projeto", projectName,
		"fonte", d.occ.SentryProjectSlug,
		"issue", d.occ.IssueID,
		"action", d.occ.Action,
		"substatus", string(d.occ.Substatus),
		"category", string(d.occ.Category))

	h.respond(w, http.StatusAccepted, response{Status: statusQueued, Analysis: created.ID})
}

// refuse records the refusal as a first-class outcome and answers 2xx. It is 2xx and not
// 4xx because the delivery was handled: the nightwatcher decided about it, wrote the
// decision down, and has nothing to gain from Sentry sending it again.
func (h *Handler) refuse(ctx context.Context, w http.ResponseWriter, d delivery, reason domain.RefusalReason, detail string) {
	a := domain.Analysis{
		ProjectID:     d.projectID,
		SourceID:      d.sourceID,
		Occurrence:    d.occ,
		State:         domain.StateRefused,
		Refusal:       reason,
		FailureReason: detail,
	}

	created, err := h.store.CreateAnalysis(ctx, a, d.now)
	if err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			h.duplicate(ctx, w, d.occ)
			return
		}
		h.fail(w, "registrando a recusa", err)
		return
	}

	h.log.Info("ocorrência recusada",
		"analysis", created.ID,
		"motivo", string(reason),
		"detalhe", detail,
		"fonte", d.occ.SentryProjectSlug,
		"issue", d.occ.IssueID)

	h.respond(w, http.StatusOK, response{
		Status:   statusRefused,
		Analysis: created.ID,
		Refusal:  string(reason),
		Detail:   detail,
	})
}

// duplicate answers a delivery that lost the insert race, looking up which Análise won
// it so the answer names the record just like the ordinary duplicate path does.
func (h *Handler) duplicate(ctx context.Context, w http.ResponseWriter, occ domain.Occurrence) {
	id, _, err := h.store.OccurrenceRecorded(ctx, occ)
	if err != nil {
		// The Ocorrência is on record either way; only its id is missing from the answer.
		h.log.Warn("duplicata sem id da análise vencedora", "dedup", occ.DedupKey(), "erro", err)
	}
	h.reportDuplicate(w, id, occ)
}

// reportDuplicate answers a delivery of an Ocorrência already on record.
//
// Nothing new is written, and that is not an omission: a second row with the same
// identity is precisely what the dedup index forbids, and the first row IS the record of
// this Ocorrência. Recording the repeat separately would mean recording the same
// Ocorrência twice, which is the thing dedup exists to prevent. The repeat shows up in
// the log, where deliveries belong; the Ocorrência shows up in `analysis list`, where
// Ocorrências belong.
func (h *Handler) reportDuplicate(w http.ResponseWriter, analysisID string, occ domain.Occurrence) {
	h.log.Info("entrega duplicada descartada",
		"analysis", analysisID, "dedup", occ.DedupKey())
	h.respond(w, http.StatusOK, response{Status: statusDuplicate, Analysis: analysisID})
}

// fail answers the one status that asks Sentry to try again. Reserved for an internal
// failure — the database being unreachable — where the redelivery five minutes later is
// the cheapest recovery available and the Ocorrência is otherwise lost.
func (h *Handler) fail(w http.ResponseWriter, attempting string, err error) {
	h.log.Error("falha interna na ingestão", "etapa", attempting, "erro", err)
	http.Error(w, "falha interna", http.StatusInternalServerError)
}

// The outcome names are what a `curl` against the endpoint and the daemon log agree on.
const (
	statusQueued    = "enfileirada"
	statusRefused   = "recusada"
	statusDuplicate = "duplicada"
)

// response is the small body the endpoint answers with. Sentry ignores it; it exists so
// that a delivery replayed by hand says what happened to it.
type response struct {
	Status   string `json:"status"`
	Analysis string `json:"analysis,omitempty"`
	Refusal  string `json:"refusal,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (h *Handler) respond(w http.ResponseWriter, code int, body response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The decision is already written down; the caller merely did not hear it.
		h.log.Warn("resposta do webhook não pôde ser escrita", "erro", err)
	}
}
