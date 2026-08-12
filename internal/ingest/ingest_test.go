package ingest_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
	"github.com/eduardotorresdev/nightwatcher/internal/store"
)

// The tests drive the real handler over real HTTP against real SQLite in a temp file,
// with the payloads recorded from the acme org. What they assert on is what the outside
// world can see: the status Sentry got back, and the rows that ended up in the database.

// t0 is the fixed instant everything is measured from. Nothing here waits.
var t0 = time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC)

// testSecret stands in for the integration's Client Secret. The signatures are computed
// with it at delivery time rather than recorded as a dead digest, which is what makes
// these tests exercise the whole signing path instead of a stored string.
const testSecret = "s3gr3d0-de-teste-do-nightwatcher"

func TestAValidErrorDeliveryFromAKnownFonteQueuesExactlyOneOcorrencia(t *testing.T) {
	h := newHarness(t)

	resp := h.deliver(t, "defeito-error-new.json")
	if resp.code != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusAccepted, resp.raw)
	}
	if resp.body.Status != "enfileirada" {
		t.Errorf("status da resposta = %q, queria \"enfileirada\"", resp.body.Status)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateQueued {
		t.Errorf("estado = %q, queria %q", a.State, domain.StateQueued)
	}
	if a.Refusal != "" {
		t.Errorf("recusa = %q, queria vazia", a.Refusal)
	}
	if a.ProjectID == 0 || a.SourceID == 0 {
		t.Errorf("análise sem Projeto/Fonte: project=%d source=%d", a.ProjectID, a.SourceID)
	}
	if a.Occurrence.IssueID != "7658235585" {
		t.Errorf("issue = %q, queria \"7658235585\"", a.Occurrence.IssueID)
	}
	if a.Occurrence.Category != domain.CategoryError {
		t.Errorf("categoria = %q, queria %q", a.Occurrence.Category, domain.CategoryError)
	}
	if a.Occurrence.Substatus != domain.SubstatusNew || a.Occurrence.Action != domain.ActionCreated {
		t.Errorf("identidade = (%q,%q), queria (created,new)", a.Occurrence.Action, a.Occurrence.Substatus)
	}
	if a.Occurrence.SentryProjectSlug != "acme" {
		t.Errorf("fonte = %q, queria \"acme\"", a.Occurrence.SentryProjectSlug)
	}
	// The raw bytes are kept because the failure path publishes them and `occurrence
	// replay` re-reads them.
	if !bytes.Equal(a.Occurrence.RawBody, fixture(t, "defeito-error-new.json")) {
		t.Errorf("o corpo cru gravado não é o que foi entregue")
	}

	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

func TestTheSameDeliveryTwiceQueuesOneOcorrenciaAndTheSecondIsRecognisedAsDuplicate(t *testing.T) {
	h := newHarness(t)

	first := h.deliver(t, "defeito-error-new.json")
	if first.code != http.StatusAccepted {
		t.Fatalf("primeira entrega: status = %d, queria %d", first.code, http.StatusAccepted)
	}

	second := h.deliver(t, "defeito-error-new.json")
	if second.code != http.StatusOK {
		t.Fatalf("segunda entrega: status = %d, queria %d (corpo: %s)", second.code, http.StatusOK, second.raw)
	}
	if second.body.Status != "duplicada" {
		t.Errorf("segunda entrega: status = %q, queria \"duplicada\"", second.body.Status)
	}
	if second.body.Analysis != first.body.Analysis {
		t.Errorf("a duplicata apontou para %q, queria a análise já gravada %q",
			second.body.Analysis, first.body.Analysis)
	}

	if got := len(h.analyses(t)); got != 1 {
		t.Errorf("análises = %d, queria 1", got)
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

func TestAnInvalidSignatureLeavesNoTraceInTheQueueAndAnswers401(t *testing.T) {
	h := newHarness(t)

	resp := h.post(t, fixture(t, "defeito-error-new.json"), ingest.ResourceIssue,
		sign("outro segredo", fixture(t, "defeito-error-new.json")))

	if resp.code != http.StatusUnauthorized {
		t.Fatalf("status = %d, queria %d", resp.code, http.StatusUnauthorized)
	}
	// The one refusal that writes nothing: an unauthenticated caller must not be able to
	// put rows in the database at will.
	if got := len(h.analyses(t)); got != 0 {
		t.Errorf("análises = %d, queria 0 — assinatura inválida não pode deixar rastro", got)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

// This is the regression test for the "unmarshal then HMAC" bug. The signature is
// computed over a re-serialised copy of the payload and the original bytes are
// delivered: a handler that decoded before hashing would accept it.
func TestASignatureComputedOverReSerialisedJSONIsRefused(t *testing.T) {
	h := newHarness(t)

	original := fixture(t, "defeito-error-new.json")

	var round any
	if err := json.Unmarshal(original, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	reserialised, err := json.Marshal(round)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Equal(original, reserialised) {
		t.Fatal("o teste seria vazio: reserializar não mudou nenhum byte")
	}

	resp := h.post(t, original, ingest.ResourceIssue, sign(testSecret, reserialised))

	if resp.code != http.StatusUnauthorized {
		t.Fatalf("status = %d, queria %d — o HMAC tem que cobrir os bytes crus", resp.code, http.StatusUnauthorized)
	}
	if got := len(h.analyses(t)); got != 0 {
		t.Errorf("análises = %d, queria 0", got)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

func TestAnUnknownFonteIsRefusedAndRecordedNeverQueued(t *testing.T) {
	h := newHarness(t)
	// `acme` stops being a registered Fonte, so the recorded payload arrives from a
	// Sentry project nobody hooked up.
	if err := h.store.RemoveSource(context.Background(), "acme"); err != nil {
		t.Fatalf("RemoveSource: %v", err)
	}

	resp := h.deliver(t, "defeito-error-new.json")
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateRefused || a.Refusal != domain.RefusedUnknownFonte {
		t.Errorf("desfecho = (%q,%q), queria (%q,%q)", a.State, a.Refusal,
			domain.StateRefused, domain.RefusedUnknownFonte)
	}
	if !strings.Contains(a.FailureReason, "acme") {
		t.Errorf("motivo = %q, queria nomear a Fonte que chegou", a.FailureReason)
	}
	if a.Occurrence.IssueID != "7658235585" {
		t.Errorf("a recusa não guardou a identidade da Ocorrência: issue = %q", a.Occurrence.IssueID)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

func TestAnOutageIsRefusedAtIngestAndRecorded(t *testing.T) {
	h := newHarness(t)

	resp := h.deliver(t, "outage-created.json")
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateRefused || a.Refusal != domain.RefusedOutage {
		t.Errorf("desfecho = (%q,%q), queria (%q,%q)", a.State, a.Refusal,
			domain.StateRefused, domain.RefusedOutage)
	}
	// Refused, but attributed: it still shows up under `analysis list --projeto acme`.
	if a.ProjectID == 0 {
		t.Errorf("a recusa por outage ficou sem Projeto")
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0 — abrir o repositório para diagnosticar um uptime check não produz nada", got)
	}
}

func TestACategoryNobodyAnticipatedIsRefusedAndRecordedRatherThanCrashing(t *testing.T) {
	h := newHarness(t)

	resp := h.deliver(t, "http-client-nao-previsto.json")
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateRefused || a.Refusal != domain.RefusedUnknownCategory {
		t.Errorf("desfecho = (%q,%q), queria (%q,%q)", a.State, a.Refusal,
			domain.StateRefused, domain.RefusedUnknownCategory)
	}
	// The row has to say *which* unknown category arrived, or the next one costs an
	// investigation instead of a glance.
	if !strings.Contains(a.FailureReason, "http_client") {
		t.Errorf("motivo = %q, queria nomear a categoria que chegou", a.FailureReason)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

func TestAnOngoingUnresolvedDeliveryIsIgnoredAndRecorded(t *testing.T) {
	h := newHarness(t)

	resp := h.deliver(t, "ongoing-deve-ser-ignorado.json")
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateRefused || a.Refusal != domain.RefusedIgnoredEvent {
		t.Errorf("desfecho = (%q,%q), queria (%q,%q)", a.State, a.Refusal,
			domain.StateRefused, domain.RefusedIgnoredEvent)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

// The substatus is part of the dedup key, and this is the pair that proves it: the same
// group, delivered twice with different substatus, is two Ocorrências. Without the
// substatus in the key the regressão would be swallowed as a repeat delivery — the very
// bug dedup exists to prevent, inside out.
func TestARegressionOfAnAlreadySeenGroupQueuesASecondOcorrencia(t *testing.T) {
	h := newHarness(t)

	if resp := h.deliver(t, "defeito-error-new.json"); resp.code != http.StatusAccepted {
		t.Fatalf("criação: status = %d, queria %d", resp.code, http.StatusAccepted)
	}
	if resp := h.deliver(t, "regressao-do-defeito-error-new.json"); resp.code != http.StatusAccepted {
		t.Fatalf("regressão: status = %d, queria %d", resp.code, http.StatusAccepted)
	}

	all := h.analyses(t)
	if len(all) != 2 {
		t.Fatalf("análises = %d, queria 2 — a regressão não pode ser engolida como duplicata", len(all))
	}

	seen := map[domain.Substatus]bool{}
	for _, a := range all {
		if a.Occurrence.IssueID != "7658235585" {
			t.Errorf("issue = %q, queria as duas no mesmo grupo \"7658235585\"", a.Occurrence.IssueID)
		}
		if a.State != domain.StateQueued {
			t.Errorf("estado = %q, queria %q", a.State, domain.StateQueued)
		}
		seen[a.Occurrence.Substatus] = true
	}
	if !seen[domain.SubstatusNew] || !seen[domain.SubstatusRegressed] {
		t.Errorf("substatus vistos = %v, queria new e regressed", seen)
	}

	if got := h.queueDepth(t); got != 2 {
		t.Errorf("fila = %d, queria 2", got)
	}
}

func TestFeedbackTravelsTheSamePathAsErrorAndKeepsItsCategory(t *testing.T) {
	h := newHarness(t)

	if resp := h.deliver(t, "defeito-error-new.json"); resp.code != http.StatusAccepted {
		t.Fatalf("erro: status = %d, queria %d", resp.code, http.StatusAccepted)
	}
	if resp := h.deliver(t, "defeito-feedback.json"); resp.code != http.StatusAccepted {
		t.Fatalf("feedback: status = %d, queria %d", resp.code, http.StatusAccepted)
	}

	byCategory := map[domain.Category]domain.Analysis{}
	for _, a := range h.analyses(t) {
		byCategory[a.Occurrence.Category] = a
	}
	if len(byCategory) != 2 {
		t.Fatalf("categorias enfileiradas = %v, queria error e feedback distintas", byCategory)
	}

	fb, ok := byCategory[domain.CategoryFeedback]
	if !ok {
		t.Fatal("o feedback não chegou à fila")
	}
	if fb.State != domain.StateQueued {
		t.Errorf("estado do feedback = %q, queria %q", fb.State, domain.StateQueued)
	}
	if fb.Occurrence.IssueID != "7642992276" {
		t.Errorf("issue do feedback = %q, queria \"7642992276\"", fb.Occurrence.IssueID)
	}
	// The recorded feedback arrives as created + ongoing. The ongoing rule is gated on
	// `unresolved`; applying it here would discard every feedback there is.
	if fb.Occurrence.Substatus != domain.SubstatusOngoing {
		t.Errorf("substatus do feedback = %q, queria %q", fb.Occurrence.Substatus, domain.SubstatusOngoing)
	}

	if got := h.queueDepth(t); got != 2 {
		t.Errorf("fila = %d, queria 2", got)
	}
}

// An escalation goes to the worker, which comments on the issue that already
// exists. Refusing it at ingest would make the growth of an Ocorrência invisible, so
// what this asserts is that it reaches the queue like any other accepted delivery.
func TestAnEscalationOfAnAlreadySeenGroupIsQueuedForTheWorkerToCommentOn(t *testing.T) {
	h := newHarness(t)

	if resp := h.deliver(t, "defeito-error-volume.json"); resp.code != http.StatusAccepted {
		t.Fatalf("criação: status = %d, queria %d", resp.code, http.StatusAccepted)
	}
	if resp := h.deliver(t, "escalada-do-defeito-error-volume.json"); resp.code != http.StatusAccepted {
		t.Fatalf("escalada: status = %d, queria %d", resp.code, http.StatusAccepted)
	}

	var escalation domain.Analysis
	for _, a := range h.analyses(t) {
		if a.Occurrence.Substatus == domain.SubstatusEscalating {
			escalation = a
		}
	}
	if escalation.ID == "" {
		t.Fatal("a escalada não foi registrada")
	}
	if escalation.State != domain.StateQueued || escalation.Refusal != "" {
		t.Errorf("desfecho da escalada = (%q,%q), queria (%q, sem recusa)",
			escalation.State, escalation.Refusal, domain.StateQueued)
	}
	if escalation.Occurrence.IssueID != "7318899062" {
		t.Errorf("issue da escalada = %q, queria o mesmo grupo \"7318899062\"", escalation.Occurrence.IssueID)
	}
	if got := h.queueDepth(t); got != 2 {
		t.Errorf("fila = %d, queria 2 — a escalada é uma Ocorrência própria, não uma duplicata", got)
	}
}

// Sentry counts a delivery slower than one second as failed and hangs up, and Go cancels
// the request context the moment it does. Every store call after that point would fail,
// which on this path means an authenticated Ocorrência answered 500 with nothing written
// down — and Sentry gives up after three redeliveries. The writes therefore have to
// outlive the request.
func TestACallerThatHangsUpDoesNotMakeTheOcorrenciaDisappear(t *testing.T) {
	h := newHarness(t)

	handler, err := ingest.New(ingest.Deps{
		Store:  h.store,
		Clock:  h.clock,
		Secret: testSecret,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := fixture(t, "defeito-error-new.json")
	ctx, cancel := context.WithCancel(context.Background())
	// The client is already gone by the time the handler runs — the deterministic form
	// of hanging up halfway through it.
	cancel()

	req := httptest.NewRequest(http.MethodPost, ingest.WebhookPath, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Sentry-Hook-Resource", ingest.ResourceIssue)
	req.Header.Set("Sentry-Hook-Signature", sign(testSecret, body))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	a := h.onlyAnalysis(t)
	if a.State != domain.StateQueued {
		t.Errorf("estado = %q, queria %q — a Ocorrência sumiu junto com a conexão", a.State, domain.StateQueued)
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

// User Story 18: the webhook answers before any analysis begins, because Sentry counts
// anything slower than a second as a failed delivery.
func TestTheHandlerAnswersWithTheOcorrenciaWaitingAndNothingConsumed(t *testing.T) {
	h := newHarness(t)

	resp := h.deliver(t, "defeito-error-new.json")
	if resp.code != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d", resp.code, http.StatusAccepted)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateQueued {
		t.Errorf("estado = %q, queria %q — nada pode ter avançado dentro da requisição", a.State, domain.StateQueued)
	}

	full, err := h.store.Analysis(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("Analysis: %v", err)
	}
	if len(full.Sessions) != 0 {
		t.Errorf("sessões = %d, queria 0 — nenhuma sessão do Claude pode rodar no ciclo da requisição", len(full.Sessions))
	}
	if full.Verdict != "" || full.IssueID != "" {
		t.Errorf("a Ocorrência já foi julgada dentro da requisição: verdict=%q issue=%q", full.Verdict, full.IssueID)
	}

	if got := h.queueDepth(t); got != 1 {
		t.Fatalf("fila = %d, queria 1", got)
	}
	// The item is still takeable and has never been claimed: attempts comes back as 1,
	// which is this claim and no other.
	item, ok, err := h.store.Claim(context.Background(), "worker-de-teste", t0, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatal("a Ocorrência não estava na fila esperando por um worker")
	}
	if item.AnalysisID != a.ID {
		t.Errorf("item da fila aponta para %q, queria %q", item.AnalysisID, a.ID)
	}
	if item.Attempts != 1 {
		t.Errorf("tentativas = %d, queria 1 — alguém consumiu o item antes do teste", item.Attempts)
	}
}

// The door feedback actually arrives through. Before this the endpoint answered
// `recurso de webhook não suportado: "event_alert"` and every user feedback in
// production was refused and never analysed — while Sentry created the issue for it all
// the same.
func TestAFeedbackArrivingByEventAlertIsQueued(t *testing.T) {
	h := newHarness(t)

	resp := h.deliver(t, "feedback-por-event-alert.json")
	if resp.code != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusAccepted, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateQueued {
		t.Fatalf("estado = %q, queria %q (recusa: %q %q)", a.State, domain.StateQueued, a.Refusal, a.FailureReason)
	}
	// Resolved through the slug dug out of the event URL — the numeric `project` would
	// have landed on "fonte desconhecida".
	if a.ProjectID == 0 || a.SourceID == 0 {
		t.Errorf("Ocorrência enfileirada sem Projeto/Fonte: projeto=%d fonte=%d", a.ProjectID, a.SourceID)
	}
	if a.Occurrence.Category != domain.CategoryFeedback {
		t.Errorf("categoria = %q, queria %q", a.Occurrence.Category, domain.CategoryFeedback)
	}
	if a.Occurrence.Environment != "prod" {
		t.Errorf("environment = %q, queria %q sem custar uma chamada de tags", a.Occurrence.Environment, "prod")
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

// The same Ocorrência through both doors is one Ocorrência. An error fires the Alert
// Rule AND creates the group, so without this the ordinary case would be two Análises,
// two sessões do Claude and two issues in the Destino for one defect.
func TestTheSameOcorrenciaThroughBothDoorsIsOneAnalise(t *testing.T) {
	h := newHarness(t)

	first := h.deliver(t, "feedback-por-event-alert.json")
	if first.code != http.StatusAccepted {
		t.Fatalf("primeira entrega: status = %d (corpo: %s)", first.code, first.raw)
	}
	won := h.onlyAnalysis(t)

	// The same group, now delivered as a created group instead of a fired rule. Built
	// from the recorded event_alert so the identity is provably the same one — the whole
	// point is that these two payloads describe one Ocorrência.
	body := fixture(t, "feedback-por-event-alert.json")
	viaAlert, err := ingest.Parse(body, ingest.ResourceEventAlert, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	asIssue := issueDeliveryFor(t, viaAlert)

	second := h.post(t, asIssue, ingest.ResourceIssue, sign(testSecret, asIssue))
	if second.code != http.StatusOK {
		t.Fatalf("segunda entrega: status = %d, queria %d (corpo: %s)", second.code, http.StatusOK, second.raw)
	}
	if second.body.Status != "duplicada" {
		t.Errorf("desfecho = %q, queria %q", second.body.Status, "duplicada")
	}
	if second.body.Analysis != won.ID {
		t.Errorf("a duplicata aponta para %q, queria a Análise que já existe, %q", second.body.Analysis, won.ID)
	}
	if got := len(h.analyses(t)); got != 1 {
		t.Errorf("análises = %d, queria 1: a mesma Ocorrência por duas portas é uma só", got)
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

// A regressão still gets through. This is the guard on the guard: the first-sighting
// rule ignores the substatus, and if it were applied to `issue.unresolved` as well it
// would swallow every regressão of a group that ever fired an Alert Rule — the dedup bug
// turned inside out, arriving through the new door.
func TestARegressaoAfterAnEventAlertIsStillANewOcorrencia(t *testing.T) {
	h := newHarness(t)

	if resp := h.deliver(t, "feedback-por-event-alert.json"); resp.code != http.StatusAccepted {
		t.Fatalf("status = %d (corpo: %s)", resp.code, resp.raw)
	}

	body := fixture(t, "feedback-por-event-alert.json")
	viaAlert, err := ingest.Parse(body, ingest.ResourceEventAlert, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	regressed := viaAlert
	regressed.Action = domain.ActionUnresolved
	regressed.Substatus = domain.SubstatusRegressed
	regressed.Category = domain.CategoryError
	asIssue := issueDeliveryFor(t, regressed)

	resp := h.post(t, asIssue, ingest.ResourceIssue, sign(testSecret, asIssue))
	if resp.code != http.StatusAccepted {
		t.Fatalf("a regressão precisa entrar: status = %d (corpo: %s)", resp.code, resp.raw)
	}
	if got := len(h.analyses(t)); got != 2 {
		t.Errorf("análises = %d, queria 2: a regressão é uma Ocorrência nova do mesmo grupo", got)
	}
}

// issueDeliveryFor builds an `issue` delivery carrying the identity of an Ocorrência
// that was read off the other door. It exists because no recorded `issue.created` and
// recorded `event_alert` describe the same group — feedback never fires `issue.created`
// at all, which is the discovery this whole door exists for — so the only way to test
// that both doors reach the same identity is to construct one from the other.
func issueDeliveryFor(t *testing.T, occ domain.Occurrence) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action":       occ.Action,
		"installation": map[string]any{"uuid": occ.InstallationUUID},
		"data": map[string]any{
			"issue": map[string]any{
				"id":            occ.IssueID,
				"substatus":     string(occ.Substatus),
				"issueCategory": string(occ.Category),
				"level":         string(occ.Level),
				"title":         occ.Title,
				"project":       map[string]any{"id": occ.SentryProjectID, "slug": occ.SentryProjectSlug},
			},
		},
	})
	if err != nil {
		t.Fatalf("montando a entrega issue: %v", err)
	}
	return body
}

func TestADeliveryOfAnotherHookResourceIsIgnoredAndRecorded(t *testing.T) {
	h := newHarness(t)

	body := fixture(t, "defeito-error-new.json")
	resp := h.post(t, body, "metric_alert", sign(testSecret, body))
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateRefused || a.Refusal != domain.RefusedIgnoredEvent {
		t.Errorf("desfecho = (%q,%q), queria (%q,%q)", a.State, a.Refusal,
			domain.StateRefused, domain.RefusedIgnoredEvent)
	}
	if !strings.Contains(a.FailureReason, "metric_alert") {
		t.Errorf("motivo = %q, queria nomear o recurso que chegou", a.FailureReason)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

// Past the ceiling the arrival rate is a symptom, not load. The excess is refused and
// recorded rather than dropped, so the operator can see when it started.
func TestADeliveryPastTheQueueCeilingIsRefusedAndRecorded(t *testing.T) {
	h := newHarness(t)
	if err := h.store.SetTunable(context.Background(), "max_queue", "1"); err != nil {
		t.Fatalf("SetTunable: %v", err)
	}

	if resp := h.deliver(t, "defeito-error-new.json"); resp.code != http.StatusAccepted {
		t.Fatalf("primeira: status = %d, queria %d", resp.code, http.StatusAccepted)
	}

	resp := h.deliver(t, "defeito-error-new-2.json")
	if resp.code != http.StatusOK {
		t.Fatalf("segunda: status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}
	if resp.body.Status != "recusada" {
		t.Errorf("status = %q, queria \"recusada\"", resp.body.Status)
	}

	var refused domain.Analysis
	for _, a := range h.analyses(t) {
		if a.State == domain.StateRefused {
			refused = a
		}
	}
	if refused.ID == "" {
		t.Fatal("a Ocorrência excedente não foi registrada")
	}
	if !strings.Contains(refused.FailureReason, "teto 1") {
		t.Errorf("motivo = %q, queria dizer qual teto foi atingido", refused.FailureReason)
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

func TestABodyThatIsNotAnOcorrenciaIsRecordedInsteadOfSwallowed(t *testing.T) {
	h := newHarness(t)

	body := []byte(`{"action":"created","installation":{"uuid":"u"},"data":{}}`)
	resp := h.post(t, body, ingest.ResourceIssue, sign(testSecret, body))
	if resp.code != http.StatusOK {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusOK, resp.raw)
	}

	a := h.onlyAnalysis(t)
	if a.State != domain.StateRefused {
		t.Errorf("estado = %q, queria %q", a.State, domain.StateRefused)
	}
	// Not "evento ignorado": a run of these means Sentry changed the payload shape, and
	// filed under the same reason as an `ongoing` delivery that signal is invisible.
	if a.Refusal != domain.RefusedMalformed {
		t.Errorf("motivo = %q, queria %q", a.Refusal, domain.RefusedMalformed)
	}
	if a.Occurrence.IssueID != "" {
		t.Errorf("identidade = %q, queria vazia", a.Occurrence.IssueID)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}
}

// The body is read before the signature can be checked, so its size is the one thing an
// unauthenticated caller gets to choose about the daemon. Without the bound, deciding
// how much memory the process spends is a decision anyone on the internet can make.
func TestABodyPastTheBoundIsRefusedWithoutBeingBuffered(t *testing.T) {
	h := newHarness(t)

	// Two mebibytes of valid JSON: past the bound, and correctly signed, so the only
	// thing that can refuse it is the bound itself.
	huge := append([]byte(`{"action":"created","data":{"issue":{"id":"1","title":"`),
		bytes.Repeat([]byte("a"), 2<<20)...)
	huge = append(huge, []byte(`"}}}`)...)

	resp := h.post(t, huge, ingest.ResourceIssue, sign(testSecret, huge))
	if resp.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusRequestEntityTooLarge, resp.raw)
	}
	// Nothing is written: the body was never authenticated, so nothing about it may be
	// recorded — the same rule as a forged signature.
	if got := len(h.analyses(t)); got != 0 {
		t.Errorf("análises = %d, queria 0", got)
	}
	if got := h.queueDepth(t); got != 0 {
		t.Errorf("fila = %d, queria 0", got)
	}

	// And the endpoint is still standing for the next delivery.
	if next := h.deliver(t, "defeito-error-new.json"); next.code != http.StatusAccepted {
		t.Errorf("entrega seguinte: status = %d, queria %d", next.code, http.StatusAccepted)
	}
}

func TestOnlyPostIsAccepted(t *testing.T) {
	h := newHarness(t)

	resp, err := http.Get(h.server.URL + ingest.WebhookPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, queria %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	if got := len(h.analyses(t)); got != 0 {
		t.Errorf("análises = %d, queria 0", got)
	}
}

func TestNewRefusesAConfigurationThatCouldOnlyFailAtTheFirstDelivery(t *testing.T) {
	s := newStore(t)
	clock := &fakeClock{now: t0}

	cases := []struct {
		name string
		deps ingest.Deps
	}{
		{"sem store", ingest.Deps{Clock: clock, Secret: testSecret}},
		{"sem clock", ingest.Deps{Store: s, Secret: testSecret}},
		{"sem client secret", ingest.Deps{Store: s, Clock: clock}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ingest.New(tc.deps); err == nil {
				t.Fatal("New aceitou uma configuração incompleta")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	store  *store.Store
	clock  *fakeClock
	server *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	s := newStore(t)
	// The two Fontes the recorded payloads actually come from: `acme` is the SvelteKit
	// app and `acme-api` is where the uptime monitor lives. One Projeto, two Fontes —
	// which is the topology the predecessor's configuration disproved the alternative of.
	if _, err := s.UpsertProject(context.Background(), domain.Project{
		Name:            "acme",
		Repo:            "acme/acme",
		Branch:          "main",
		LinearProjectID: "lp-acme",
		Sources: []domain.Source{
			{SentrySlug: "acme", Label: "web"},
			{SentrySlug: "acme-api", Label: "api"},
		},
	}, t0); err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}

	clock := &fakeClock{now: t0}
	h, err := ingest.New(ingest.Deps{
		Store:  s,
		Clock:  clock,
		Secret: testSecret,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle(ingest.WebhookPath, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &harness{store: s, clock: clock, server: srv}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Setup(context.Background(), filepath.Join(t.TempDir(), "nightwatcher.db"), t0)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// delivered is what Sentry would have observed coming back.
type delivered struct {
	code int
	raw  string
	body struct {
		Status   string `json:"status"`
		Analysis string `json:"analysis"`
		Refusal  string `json:"refusal"`
		Detail   string `json:"detail"`
	}
}

// deliver posts a recorded payload signed exactly as Sentry would sign it.
// deliver sends a recorded fixture the way Sentry would, with the header that matches
// the shape the fixture actually has — `issue` for a group payload, `event_alert` for an
// Alert Rule firing. Hardcoding `issue` here would make every test in this file blind to
// the door feedback really arrives through.
func (h *harness) deliver(t *testing.T, name string) delivered {
	t.Helper()
	body := fixture(t, name)
	return h.post(t, body, ingest.DetectResource(body), sign(testSecret, body))
}

func (h *harness) post(t *testing.T, body []byte, resource, signature string) delivered {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.server.URL+ingest.WebhookPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sentry-Hook-Resource", resource)
	req.Header.Set("Sentry-Hook-Timestamp", "1786000000")
	// New on every delivery, which is exactly why it is useless as a dedup key.
	req.Header.Set("Request-ID", "b3f1c0de0000000000000000000000ff")
	req.Header.Set("Sentry-Hook-Signature", signature)

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("lendo a resposta: %v", err)
	}

	out := delivered{code: resp.StatusCode, raw: strings.TrimSpace(string(raw))}
	// Only the handled outcomes answer JSON; the plain-text refusals do not, and that is
	// fine to ignore here.
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (h *harness) analyses(t *testing.T) []domain.Analysis {
	t.Helper()
	all, err := h.store.ListAnalyses(context.Background(), store.AnalysisFilter{})
	if err != nil {
		t.Fatalf("ListAnalyses: %v", err)
	}
	return all
}

func (h *harness) onlyAnalysis(t *testing.T) domain.Analysis {
	t.Helper()
	all := h.analyses(t)
	if len(all) != 1 {
		t.Fatalf("análises = %d, queria exatamente 1", len(all))
	}
	return all[0]
}

func (h *harness) queueDepth(t *testing.T) int {
	t.Helper()
	n, err := h.store.QueueDepth(context.Background())
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	return n
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	// Read as bytes and never decoded: the whole point of these payloads is that the
	// exact bytes are what gets signed.
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sentry", name))
	if err != nil {
		t.Fatalf("lendo a fixture %s: %v", name, err)
	}
	return b
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// fakeClock is mandatory: the handler stamps every row with it and must never reach for
// the wall clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

// testWriter routes the daemon log into the test log, so a failing scenario prints the
// decisions the handler took to get there.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}

// Sentry redelivers, and two deliveries of the same Ocorrência can be in flight at
// once. The check-then-insert is closed by the unique index and not by a lock, so both
// requests can pass the dedup lookup and only one may survive.
func TestConcurrentDeliveriesOfTheSameOcorrenciaQueueItOnce(t *testing.T) {
	h := newHarness(t)
	body := fixture(t, "defeito-error-new.json")
	signature := sign(testSecret, body)

	const deliveries = 6
	codes := make([]int, deliveries)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = h.post(t, body, ingest.ResourceIssue, signature).code
		}(i)
	}
	wg.Wait()

	accepted := 0
	for i, code := range codes {
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusOK:
		default:
			t.Errorf("entrega %d: status = %d, queria 202 ou 200", i, code)
		}
	}
	if accepted != 1 {
		t.Errorf("entregas aceitas = %d, queria 1", accepted)
	}
	if got := len(h.analyses(t)); got != 1 {
		t.Errorf("análises = %d, queria 1", got)
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1", got)
	}
}

// The same storm the daemon meets after a bad release: every recorded payload arriving
// five times over, accepted ones racing refused ones, with the unique index as the only
// thing between them. What has to hold is that each Ocorrência is recorded exactly once
// — including the refusals, which carry the same identity and race on the same index.
func TestAStormOfOverlappingDeliveriesRecordsEachOcorrenciaExactlyOnce(t *testing.T) {
	h := newHarness(t)

	// Six of these are accepted (two creations, a high-volume creation, a regressão, an
	// escalada and a feedback) and three are refused on arrival (outage, http_client,
	// unresolved+ongoing) — refusals write a row too, and it is the same index.
	names := []string{
		"defeito-error-new.json",
		"defeito-error-new-2.json",
		"defeito-error-volume.json",
		"regressao-do-defeito-error-new.json",
		"escalada-do-defeito-error-volume.json",
		"defeito-feedback.json",
		"outage-created.json",
		"http-client-nao-previsto.json",
		"ongoing-deve-ser-ignorado.json",
	}
	const queued = 6
	const copies = 5

	codes := make([]int, len(names)*copies)
	var wg sync.WaitGroup
	for i, name := range names {
		for c := range copies {
			wg.Add(1)
			go func(slot int, name string) {
				defer wg.Done()
				body := fixture(t, name)
				codes[slot] = h.post(t, body, ingest.ResourceIssue, sign(testSecret, body)).code
			}(i*copies+c, name)
		}
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusAccepted && code != http.StatusOK {
			t.Errorf("entrega %d: status = %d, queria 202 ou 200", i, code)
		}
	}

	perKey := map[string]int{}
	for _, a := range h.analyses(t) {
		perKey[a.Occurrence.DedupKey()]++
	}
	for key, n := range perKey {
		if n != 1 {
			t.Errorf("a Ocorrência %s foi gravada %d vezes, queria 1", key, n)
		}
	}
	if len(perKey) != len(names) {
		t.Errorf("Ocorrências distintas = %d, queria %d — alguma sumiu na corrida", len(perKey), len(names))
	}
	if got := h.queueDepth(t); got != queued {
		t.Errorf("fila = %d, queria %d", got, queued)
	}
}

// Um número ilegível no banco não pode custar uma Ocorrência. O 5xx pede reentrega, o
// Sentry reentrega três vezes, recebe a mesma resposta três vezes e desiste — uma
// Ocorrência autenticada perdida por causa de um erro de digitação num número. O teto da
// fila é válvula de segurança, não requisito de correção: a entrada segue com os padrões
// e a linha quebrada aparece no log.
func TestAnUnreadableOperatingNumberDoesNotCostTheOcorrencia(t *testing.T) {
	h := newHarness(t)
	if err := h.store.SetSetting(context.Background(), "max_queue", "duzentos"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	resp := h.deliver(t, "defeito-error-new.json")
	if resp.code != http.StatusAccepted {
		t.Fatalf("status = %d, queria %d (corpo: %s)", resp.code, http.StatusAccepted, resp.raw)
	}
	if got := h.queueDepth(t); got != 1 {
		t.Errorf("fila = %d, queria 1: a Ocorrência sumiu por causa de uma linha de settings", got)
	}
	if a := h.onlyAnalysis(t); a.State != domain.StateQueued {
		t.Errorf("estado = %q, queria %q", a.State, domain.StateQueued)
	}
}
