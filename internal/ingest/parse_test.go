package ingest_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
	"github.com/eduardotorresdev/nightwatcher/internal/ingest"
)

// Parse is exported because `occurrence simulate` replays a recorded payload through it,
// so what it reads out of the recorded bytes is a contract and not an internal detail.
// The table is every payload captured from the acme org.
func TestParseReadsTheRecordedPayloadsAsTheOcorrenciasTheyAre(t *testing.T) {
	cases := []struct {
		fixture   string
		action    string
		substatus domain.Substatus
		category  domain.Category
		level     domain.Level
		issueID   string
		slug      string
		count     int64
		userCount int64
	}{
		{"defeito-error-new.json", domain.ActionCreated, domain.SubstatusNew, domain.CategoryError, domain.LevelError, "7658235585", "acme", 2, 1},
		{"defeito-error-new-2.json", domain.ActionCreated, domain.SubstatusNew, domain.CategoryError, domain.LevelError, "7657851992", "acme", 2, 1},
		{"defeito-error-volume.json", domain.ActionCreated, domain.SubstatusOngoing, domain.CategoryError, domain.LevelError, "7318899062", "acme", 585, 133},
		{"ruido-error-instrumentacao.json", domain.ActionCreated, domain.SubstatusNew, domain.CategoryError, domain.LevelError, "7656760031", "acme", 1, 1},
		{"defeito-feedback.json", domain.ActionCreated, domain.SubstatusOngoing, domain.CategoryFeedback, domain.LevelInfo, "7642992276", "acme", 1, 1},
		{"defeito-feedback-com-dados-de-cliente.json", domain.ActionCreated, domain.SubstatusOngoing, domain.CategoryFeedback, domain.LevelInfo, "7608724050", "acme", 1, 1},
		{"sugestao-feedback.json", domain.ActionCreated, domain.SubstatusOngoing, domain.CategoryFeedback, domain.LevelInfo, "7630979184", "acme", 1, 1},
		{"http-client-nao-previsto.json", domain.ActionCreated, domain.SubstatusOngoing, "http_client", domain.LevelInfo, "7370658318", "acme", 105, 2},
		{"outage-created.json", domain.ActionCreated, domain.SubstatusNew, domain.CategoryOutage, domain.LevelError, "6100000007", "acme-api", 1, 0},
		{"regressao-do-defeito-error-new.json", domain.ActionUnresolved, domain.SubstatusRegressed, domain.CategoryError, domain.LevelError, "7658235585", "acme", 2, 1},
		{"escalada-do-defeito-error-volume.json", domain.ActionUnresolved, domain.SubstatusEscalating, domain.CategoryError, domain.LevelError, "7318899062", "acme", 585, 133},
		{"ongoing-deve-ser-ignorado.json", domain.ActionUnresolved, domain.SubstatusOngoing, domain.CategoryError, domain.LevelError, "7657851992", "acme", 2, 1},
	}

	keys := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			body := fixture(t, tc.fixture)
			occ, err := ingest.Parse(body, ingest.ResourceIssue, "")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			if occ.Action != tc.action {
				t.Errorf("action = %q, queria %q", occ.Action, tc.action)
			}
			if occ.Substatus != tc.substatus {
				t.Errorf("substatus = %q, queria %q", occ.Substatus, tc.substatus)
			}
			if occ.Category != tc.category {
				t.Errorf("issueCategory = %q, queria %q", occ.Category, tc.category)
			}
			if occ.Level != tc.level {
				t.Errorf("level = %q, queria %q", occ.Level, tc.level)
			}
			if occ.IssueID != tc.issueID {
				t.Errorf("issue = %q, queria %q", occ.IssueID, tc.issueID)
			}
			if occ.SentryProjectSlug != tc.slug {
				t.Errorf("fonte = %q, queria %q", occ.SentryProjectSlug, tc.slug)
			}
			// `count` arrives quoted and `userCount` bare, in the same object.
			if occ.Count != tc.count || occ.UserCount != tc.userCount {
				t.Errorf("volume = (%d,%d), queria (%d,%d)", occ.Count, occ.UserCount, tc.count, tc.userCount)
			}
			if occ.InstallationUUID == "" || occ.SentryProjectID == "" {
				t.Errorf("identidade incompleta: installation=%q projeto=%q", occ.InstallationUUID, occ.SentryProjectID)
			}
			// The outage payload spells the offset `+00:00` where the others use `Z`.
			if occ.FirstSeen.IsZero() || occ.LastSeen.IsZero() {
				t.Errorf("datas não lidas: firstSeen=%v lastSeen=%v", occ.FirstSeen, occ.LastSeen)
			}
			if occ.FirstSeen.Location() != time.UTC {
				t.Errorf("firstSeen fora de UTC: %v", occ.FirstSeen)
			}
			// Environment costs a separate tags call and is not in the payload.
			if occ.Environment != "" {
				t.Errorf("environment = %q, queria vazio", occ.Environment)
			}
			// Parse has no Clock, so it never stamps the arrival.
			if !occ.ReceivedAt.IsZero() {
				t.Errorf("receivedAt = %v, queria zero", occ.ReceivedAt)
			}

			if prev, clash := keys[occ.DedupKey()]; clash {
				t.Errorf("chave de dedup repetida entre %s e %s: %s", prev, tc.fixture, occ.DedupKey())
			}
			keys[occ.DedupKey()] = tc.fixture
		})
	}
}

// The regressão and the criação share a group and differ only in substatus. If the key
// collapsed them, the regressão would be swallowed as a repeat delivery.
func TestTheDedupKeySeparatesARegressionFromTheCreationOfTheSameGroup(t *testing.T) {
	created, err := ingest.Parse(fixture(t, "defeito-error-new.json"), ingest.ResourceIssue, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	regressed, err := ingest.Parse(fixture(t, "regressao-do-defeito-error-new.json"), ingest.ResourceIssue, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if created.IssueID != regressed.IssueID {
		t.Fatalf("as duas fixtures deviam ser o mesmo grupo: %q e %q", created.IssueID, regressed.IssueID)
	}
	if created.DedupKey() == regressed.DedupKey() {
		t.Errorf("chave de dedup igual (%s): a regressão seria engolida como duplicata", created.DedupKey())
	}
}

func TestParseRefusesWhatIsNotAnOcorrencia(t *testing.T) {
	valid := fixture(t, "defeito-error-new.json")

	cases := []struct {
		name     string
		body     []byte
		resource string
		want     error
	}{
		{"outro recurso do webhook", valid, "metric_alert", ingest.ErrUnsupportedResource},
		{"sem cabeçalho de recurso", valid, "", ingest.ErrUnsupportedResource},
		{"json quebrado", []byte(`{"action":`), ingest.ResourceIssue, ingest.ErrMalformedPayload},
		{"envelope sem data.issue", []byte(`{"action":"created","data":{}}`), ingest.ResourceIssue, ingest.ErrMalformedPayload},
		{"issue sem id", []byte(`{"action":"created","data":{"issue":{"title":"x"}}}`), ingest.ResourceIssue, ingest.ErrMalformedPayload},
		// The two doors read different keys, so each refuses the other's payload rather
		// than reading it as a shape it is not.
		{"envelope sem data.event", []byte(`{"action":"triggered","data":{}}`), ingest.ResourceEventAlert, ingest.ErrMalformedPayload},
		{"event sem issue_id", []byte(`{"action":"triggered","data":{"event":{"title":"x"}}}`), ingest.ResourceEventAlert, ingest.ErrMalformedPayload},
		{"payload de issue lido como event_alert", valid, ingest.ResourceEventAlert, ingest.ErrMalformedPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ingest.Parse(tc.body, tc.resource, "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("erro = %v, queria %v", err, tc.want)
			}
		})
	}
}

// The `event_alert` door. Everything asserted here was measured off the real delivery
// captured from acme on 2026-08-12 — the one in feedback-por-event-alert.json — and each
// assertion is a place the shape differs from the `issue` door in a way that would
// silently lose the Ocorrência if read as if it were the same.
func TestParseReadsTheEventAlertDoor(t *testing.T) {
	body := fixture(t, "feedback-por-event-alert.json")

	occ, err := ingest.Parse(body, ingest.ResourceEventAlert, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The identity is the group's, not the event's: `issue_id`, the same id the other
	// door delivers as `data.issue.id`.
	if occ.IssueID != "7667858493" {
		t.Errorf("issue = %q, queria %q", occ.IssueID, "7667858493")
	}
	// `data.event.project` is a JSON number here and a quoted string on the issue door.
	// If these two do not come out identical, one Ocorrência becomes two Análises.
	if occ.SentryProjectID != "4509380076634112" {
		t.Errorf("projeto Sentry = %q, queria o mesmo id que a porta issue traz", occ.SentryProjectID)
	}
	// And the slug is NOT in `project` — it only exists inside the event's API URL.
	// Reading the numeric id as a slug refuses every delivery as "fonte desconhecida".
	if occ.SentryProjectSlug != "acme" {
		t.Errorf("fonte = %q, queria %q — o slug só existe em data.event.url", occ.SentryProjectSlug, "acme")
	}
	// `occurrence.type` 6001 is the only thing saying this is a feedback: there is no
	// issueCategory on this door at all.
	if occ.Category != domain.CategoryFeedback {
		t.Errorf("categoria = %q, queria %q", occ.Category, domain.CategoryFeedback)
	}
	if occ.Action != domain.ActionTriggered {
		t.Errorf("action = %q, queria %q", occ.Action, domain.ActionTriggered)
	}
	// No substatus exists in this payload, and inventing one would let a real regressão
	// be swallowed as a repeat.
	if occ.Substatus != "" {
		t.Errorf("substatus = %q, queria vazio: o event_alert não carrega nenhum", occ.Substatus)
	}
	// The premise recorded early — that environment is absent from the delivery and
	// needs a separate tags call — does not hold on this door.
	if occ.Environment != "prod" {
		t.Errorf("environment = %q, queria %q direto do corpo", occ.Environment, "prod")
	}
	if occ.Level != domain.LevelInfo {
		t.Errorf("level = %q, queria %q", occ.Level, domain.LevelInfo)
	}
	if occ.Title == "" || occ.WebURL == "" {
		t.Errorf("Ocorrência sem título ou sem link: título=%q web=%q", occ.Title, occ.WebURL)
	}
	// The event knows when IT happened; it does not know when the group was first seen,
	// and a fabricated firstSeen would be read as fact by the Prioridade.
	if occ.LastSeen.IsZero() {
		t.Error("lastSeen não lido do datetime do evento")
	}
	if !occ.FirstSeen.IsZero() {
		t.Errorf("firstSeen = %v, queria vazio: esta entrega não sabe disso", occ.FirstSeen)
	}
	if !bytes.Equal(occ.RawBody, body) {
		t.Error("RawBody precisa ser os bytes exatos que a assinatura cobriu")
	}
}

// The Ocorrência is the group, so the same group arriving through both doors has to
// produce the same identity — that is what lets dedup recognise it. Everything except
// the substatus must line up, and the substatus is exactly the field the event_alert
// does not have, which is why dedup cannot be left to DedupKey alone.
func TestBothDoorsAgreeOnTheProjetoSentry(t *testing.T) {
	viaIssue, err := ingest.Parse(fixture(t, "sugestao-feedback.json"), ingest.ResourceIssue, "")
	if err != nil {
		t.Fatalf("Parse(issue): %v", err)
	}
	viaAlert, err := ingest.Parse(fixture(t, "feedback-por-event-alert.json"), ingest.ResourceEventAlert, "")
	if err != nil {
		t.Fatalf("Parse(event_alert): %v", err)
	}

	if viaIssue.SentryProjectID != viaAlert.SentryProjectID {
		t.Errorf("projeto Sentry: issue=%q event_alert=%q — as duas portas precisam concordar",
			viaIssue.SentryProjectID, viaAlert.SentryProjectID)
	}
	if viaIssue.SentryProjectSlug != viaAlert.SentryProjectSlug {
		t.Errorf("fonte: issue=%q event_alert=%q", viaIssue.SentryProjectSlug, viaAlert.SentryProjectSlug)
	}
	if viaIssue.InstallationUUID != viaAlert.InstallationUUID {
		t.Errorf("installation: issue=%q event_alert=%q", viaIssue.InstallationUUID, viaAlert.InstallationUUID)
	}
}

// The callers with no HTTP headers around the payload — `occurrence simulate` off a
// file, `occurrence replay` off recorded bytes — get the shape from the body. Before
// this existed they assumed `issue`, which made replay fail on every Ocorrência that
// came through the Alert Rule.
func TestDetectResourceReadsTheShapeOffTheBody(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{"defeito-error-new.json", ingest.ResourceIssue},
		{"sugestao-feedback.json", ingest.ResourceIssue},
		{"feedback-por-event-alert.json", ingest.ResourceEventAlert},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			body := fixture(t, tc.fixture)
			if got := ingest.DetectResource(body); got != tc.want {
				t.Fatalf("DetectResource = %q, queria %q", got, tc.want)
			}
			// And what it detects has to be what Parse can read.
			if _, err := ingest.Parse(body, ingest.DetectResource(body), ""); err != nil {
				t.Fatalf("Parse com o recurso detectado: %v", err)
			}
		})
	}

	if got := ingest.DetectResource([]byte(`{"action":"created","data":{}}`)); got != "" {
		t.Errorf("DetectResource(sem nenhuma das duas chaves) = %q, queria vazio", got)
	}
	if got := ingest.DetectResource([]byte(`{`)); got != "" {
		t.Errorf("DetectResource(json quebrado) = %q, queria vazio", got)
	}
}

// The action lives in the envelope, so the endpoint passes none. A replay passes the
// action it recorded, so a hand-edited body cannot make it mean something else.
func TestAnExplicitActionOverridesTheEnvelope(t *testing.T) {
	body := fixture(t, "defeito-error-new.json")

	fromEnvelope, err := ingest.Parse(body, ingest.ResourceIssue, "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if fromEnvelope.Action != domain.ActionCreated {
		t.Errorf("action = %q, queria %q", fromEnvelope.Action, domain.ActionCreated)
	}

	overridden, err := ingest.Parse(body, ingest.ResourceIssue, domain.ActionUnresolved)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if overridden.Action != domain.ActionUnresolved {
		t.Errorf("action = %q, queria %q", overridden.Action, domain.ActionUnresolved)
	}
}

func TestVerifyAcceptsOnlyTheHexHMACOfTheExactBytes(t *testing.T) {
	body := fixture(t, "defeito-error-new.json")
	good := sign(testSecret, body)

	cases := []struct {
		name   string
		secret string
		body   []byte
		sig    string
		want   bool
	}{
		{"a assinatura correta", testSecret, body, good, true},
		{"a mesma em maiúsculas", testSecret, body, strings.ToUpper(good), true},
		{"outro segredo", "outro", body, good, false},
		{"segredo vazio nunca autentica", "", body, sign("", body), false},
		{"um byte a mais no corpo", testSecret, append(append([]byte{}, body...), ' '), good, false},
		{"assinatura vazia", testSecret, body, "", false},
		{"assinatura truncada", testSecret, body, good[:32], false},
		{"assinatura que não é hex", testSecret, body, strings.Repeat("z", 64), false},
		// Longer than a SHA-256 digest, and hex all the way: without the length check
		// before it, hex.Decode writes past the 32-byte buffer and panics — an
		// unauthenticated caller crashing the handler with one header.
		{"assinatura hex longa demais", testSecret, body, good + good, false},
		{"assinatura de comprimento ímpar", testSecret, body, good[:63], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ingest.Verify(tc.secret, tc.body, []byte(tc.sig)); got != tc.want {
				t.Fatalf("Verify = %v, queria %v", got, tc.want)
			}
		})
	}
}
