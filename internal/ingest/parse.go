package ingest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// The two values of `Sentry-Hook-Resource` this system knows how to read. The other
// values Sentry can send (`installation`, `metric_alert`, `error`, `comment`, `seer`,
// `preprod_artifact`) put a different object under `data` and are not an Ocorrência.
//
// # Why there are two doors and not one
//
// The system was designed around `issue.created`, on the assumption that it was
// how everything arrived. A production session on 2026-08-12 measured otherwise:
//
//   - Feedback ALWAYS becomes an issue in Sentry (five created during the session,
//     every one of them `issueCategory: feedback`).
//   - Feedback NEVER fires `issue.created`. Zero deliveries with resource `issue` and
//     action `created` in a ~50 minute window, with the integration's `Issue` checkbox
//     provably on — `issue` deliveries with action `resolved` arrived normally in the
//     same window.
//   - Feedback DOES arrive as `event_alert`, fired by an Alert Rule, with a complete
//     body.
//
// So `event_alert` is not the contingency it was taken for. For feedback it is the
// only path there is, and the predecessor's captured payload says why it ever worked:
// `"triggered_rule": "Notify via autoissues"` — the `autoissues`/`sentrybot` never
// received `issue.created` either, it lived off an Alert Rule.
//
// The two doors carry the same Ocorrência in different shapes: `issue` delivers the
// *group* (`data.issue`), `event_alert` delivers one *event of* the group
// (`data.event`) plus the group's id. Both resolve to the same identity, which is what
// keeps the same Ocorrência from becoming two Análises — see OccurrenceRecorded.
const (
	ResourceIssue      = "issue"
	ResourceEventAlert = "event_alert"
)

// feedbackGroupType is Sentry's `occurrence.type` for a FeedbackGroup. An `event_alert`
// carries no `issueCategory` at all — an Alert Rule fires for whatever matches it — so
// this number is the only discriminator between "a user wrote to us" and "something
// threw".
const feedbackGroupType = 6001

var (
	// ErrUnsupportedResource reports a delivery whose `Sentry-Hook-Resource` is not
	// `issue`. It is not a malformed request — it is a resource nobody taught the
	// system to read, and the caller records it rather than dropping it.
	ErrUnsupportedResource = errors.New("recurso de webhook não suportado")

	// ErrMalformedPayload reports a delivery that is not readable as an Ocorrência:
	// broken JSON, no `data.issue`, or no Sentry issue id to identify it by.
	ErrMalformedPayload = errors.New("entrega do webhook malformada")
)

// Parse turns the raw delivery into an Ocorrência.
//
// It is exported because `nightwatcher occurrence simulate <arquivo>` replays a
// recorded payload through this very function: a simulation that parsed differently
// from the endpoint would be worthless as calibration. Callers with no HTTP headers
// in hand get the resource from DetectResource and pass an empty action.
//
// resource is the `Sentry-Hook-Resource` header. It is checked here and not left to
// the caller so that no path can read a payload of one shape as if it were another —
// which matters more now that there are two shapes: `issue` puts the group under
// `data.issue`, `event_alert` puts one event under `data.event`.
//
// action overrides the envelope's `action` when non-empty; empty means "take what the
// delivery says", which is what the endpoint does, since Sentry carries the action in
// the body and not in a header. The override exists for the replay path, which holds
// the action as a column and must not let a hand-edited body make a replay mean
// something different from what was recorded.
//
// Parse decides nothing about policy: whether the action, the substatus or the
// category are ones the nightwatcher handles is the endpoint's judgement, because
// only the endpoint can record the refusal.
//
// ReceivedAt is always left empty, because Parse has no Clock and the nightwatcher
// never calls time.Now() outside one.
func Parse(body []byte, resource, action string) (domain.Occurrence, error) {
	switch resource {
	case ResourceIssue:
		return parseIssue(body, action)
	case ResourceEventAlert:
		return parseEventAlert(body, action)
	default:
		return domain.Occurrence{}, fmt.Errorf("%w: %q (esperados %q e %q)",
			ErrUnsupportedResource, resource, ResourceIssue, ResourceEventAlert)
	}
}

// DetectResource reads which of the two shapes a body has, for the callers that hold a
// payload with no headers around it: `occurrence simulate` against a file on disk, and
// `occurrence replay` against the bytes recorded on an Análise.
//
// It answers by shape and not by guesswork: an `issue` delivery has `data.issue` and an
// `event_alert` has `data.event`, and no delivery has both. An empty answer means
// neither, which Parse turns into ErrUnsupportedResource with the body named — the same
// refusal the endpoint would record.
//
// The endpoint does NOT use this. There the header is authoritative, because a body
// that disagrees with the header it was signed alongside is exactly the confusion this
// package refuses to resolve on the payload's word.
func DetectResource(body []byte) string {
	var probe struct {
		Data struct {
			Issue json.RawMessage `json:"issue"`
			Event json.RawMessage `json:"event"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	switch {
	case len(probe.Data.Issue) > 0 && string(probe.Data.Issue) != "null":
		return ResourceIssue
	case len(probe.Data.Event) > 0 && string(probe.Data.Event) != "null":
		return ResourceEventAlert
	}
	return ""
}

// parseIssue reads a `data.issue` delivery: the group itself.
//
// Two fields it deliberately leaves empty:
//   - Environment, which is not in this payload at all and costs a separate tags
//     call. The `event_alert` door does not have this problem.
//   - Substatus is read here and is the discriminator of what an `issue.unresolved`
//     means; an `event_alert` has none, which is why dedup cannot rely on it alone.
func parseIssue(body []byte, action string) (domain.Occurrence, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return domain.Occurrence{}, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if env.Data.Issue == nil {
		return domain.Occurrence{}, fmt.Errorf("%w: entrega sem data.issue", ErrMalformedPayload)
	}
	iss := env.Data.Issue
	if iss.ID == "" {
		// Without an issue id there is no identity, so dedup cannot recognise a repeat
		// of it and the queue would grow one item per redelivery.
		return domain.Occurrence{}, fmt.Errorf("%w: data.issue.id vazio", ErrMalformedPayload)
	}

	if action == "" {
		action = env.Action
	}

	return domain.Occurrence{
		InstallationUUID: env.Installation.UUID,
		SentryProjectID:  iss.Project.ID,
		IssueID:          iss.ID,
		Substatus:        domain.Substatus(iss.Substatus),
		Action:           action,

		Category: domain.Category(iss.Category),
		Level:    domain.Level(iss.Level),

		Title:     iss.Title,
		Culprit:   iss.Culprit,
		Permalink: iss.Permalink,
		WebURL:    iss.WebURL,
		Platform:  iss.Platform,
		ShortID:   iss.ShortID,

		Count:     int64(iss.Count),
		UserCount: int64(iss.UserCount),

		FirstSeen: time.Time(iss.FirstSeen),
		LastSeen:  time.Time(iss.LastSeen),

		SentryProjectSlug: iss.Project.Slug,

		// The exact bytes the HMAC was computed over. The failure path publishes them
		// and `occurrence replay` re-reads them, so they are the audit record of what
		// actually arrived — not a re-encoding of what we understood.
		RawBody: body,
	}, nil
}

// parseEventAlert reads a `data.event` delivery: one event, fired by an Alert Rule,
// carrying the id of the group it belongs to.
//
// What it reads out of a shape the original design never looked at, and why each one
// is not simply the `issue` field under a different name:
//
//   - Identity is `data.event.issue_id`. It is the same group id `data.issue.id`
//     carries, which is what lets dedup recognise one Ocorrência arriving through both
//     doors.
//   - The Fonte is resolved from `data.event.url`, NOT from `data.event.project`. That
//     field is the project's numeric id (`4509380076634112`) and `sources.sentry_slug`
//     holds a slug, so resolving by it would refuse every delivery as "fonte
//     desconhecida". The URL is the only place the slug appears.
//   - Substatus is left EMPTY, and that is a statement, not an omission: this payload
//     says nothing about whether the group is new, regressed or ongoing, and inventing
//     `new` would let a real regressão be swallowed as a repeat.
//   - Environment comes for free, which contradicts an early premise: it recorded
//     that environment is absent from the delivery and needs a separate tags call.
//     Through this door it does not. That is one fewer Sentry API call on
//     the path this feedback actually takes.
//   - Count and UserCount are absent, and stay at zero rather than being faked. They
//     are ~1 by construction at creation anyway, and the volume band is by people
//     affected — a fabricated 1 and a truthful 0 land in the same band.
//   - FirstSeen stays zero: this is an event, and it knows when IT happened, not when
//     the group was first seen. LastSeen takes the event's own instant, which is true.
func parseEventAlert(body []byte, action string) (domain.Occurrence, error) {
	var env eventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return domain.Occurrence{}, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if env.Data.Event == nil {
		return domain.Occurrence{}, fmt.Errorf("%w: entrega sem data.event", ErrMalformedPayload)
	}
	ev := env.Data.Event
	if ev.IssueID == "" {
		// Same rule as the issue door: no identity, no dedup, one queue item per
		// redelivery. An event alert on an issueless event is not an Ocorrência.
		return domain.Occurrence{}, fmt.Errorf("%w: data.event.issue_id vazio", ErrMalformedPayload)
	}

	if action == "" {
		action = env.Action
	}

	title, level, culprit := ev.Title, ev.Level, ev.Culprit
	if ev.Occurrence != nil {
		// The occurrence block is the authoritative rendering of a non-error group:
		// for feedback, `issueTitle` is the summarised title and `level` is the one
		// Sentry assigned to the group, both of which the bare event fields may lack.
		title = firstNonEmpty(ev.Occurrence.IssueTitle, title)
		level = firstNonEmpty(ev.Occurrence.Level, level)
		culprit = firstNonEmpty(ev.Occurrence.Culprit, culprit)
	}

	return domain.Occurrence{
		InstallationUUID: env.Installation.UUID,
		SentryProjectID:  string(ev.Project),
		IssueID:          ev.IssueID,
		Action:           action,

		Category: categoryOfEvent(ev),
		Level:    domain.Level(level),

		Title:     title,
		Culprit:   culprit,
		Permalink: ev.WebURL,
		WebURL:    ev.WebURL,
		Platform:  ev.Platform,

		LastSeen: time.Time(ev.Datetime),

		Environment:       ev.Environment,
		SentryProjectSlug: slugFromEventURL(ev.URL),

		RawBody: body,
	}, nil
}

// categoryOfEvent decides what kind of Ocorrência an `event_alert` is carrying.
//
// The `issue` door reads `issueCategory` straight off the payload. This door has no
// such field: an Alert Rule fires for whatever matches its conditions, and the delivery
// describes the event, not the group's category. The discriminator is
// `occurrence.type` — 6001 is a FeedbackGroup — and its absence, because a plain error
// event has no `occurrence` block at all.
//
// Anything else keeps the http_client lesson: it is named in the value, so the refusal
// row says which type it was and the next unforeseen one is a five-second read instead
// of an investigation.
func categoryOfEvent(ev *eventPayload) domain.Category {
	if ev.Occurrence == nil {
		return domain.CategoryError
	}
	if ev.Occurrence.Type == feedbackGroupType {
		return domain.CategoryFeedback
	}
	return domain.Category(fmt.Sprintf("occurrence.type %d", ev.Occurrence.Type))
}

// slugFromEventURL pulls the Fonte's slug out of the event's API URL, which reads
// `https://sentry.io/api/0/projects/<org>/<slug>/events/<id>/`.
//
// This exists because `data.event.project` is the numeric project id and
// `sources.sentry_slug` is a slug: resolving the Fonte by the numeric id would refuse
// every single event_alert as "fonte desconhecida". The URL is the only place in the
// whole payload the slug appears.
//
// An unreadable URL yields an empty slug, which the endpoint turns into exactly that
// recusa — visible in `analysis list` — instead of a silent misroute to some other
// Projeto.
func slugFromEventURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "projects" && i+2 < len(parts) {
			return parts[i+2]
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// envelope is the Integration Platform wrapper. Only the fields the nightwatcher acts
// on are named; the payload carries several dozen more and listing them would create
// dozens of ways to be wrong about Sentry's shape for no gain.
type envelope struct {
	Action       string `json:"action"`
	Installation struct {
		UUID string `json:"uuid"`
	} `json:"installation"`
	Data struct {
		Issue *issuePayload `json:"issue"`
	} `json:"data"`
}

// issuePayload is `data.issue`: the *group*, never the individual event. An error that
// happens five thousand times is one Ocorrência.
type issuePayload struct {
	ID      string `json:"id"`
	ShortID string `json:"shortId"`

	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Permalink string `json:"permalink"`
	WebURL    string `json:"web_url"`
	Level     string `json:"level"`
	Platform  string `json:"platform"`

	// Substatus is what discriminates an `issue.unresolved`: `regressed` is a
	// reincidência, `escalating` is the Ocorrência growing, `ongoing` is noise.
	Substatus string `json:"substatus"`

	// Category is `issueCategory`, NOT `kind`. There is no `kind` field in the Sentry
	// payload — the one seen in the predecessor's log was the predecessor's own.
	Category string `json:"issueCategory"`

	Count     flexInt64 `json:"count"`
	UserCount flexInt64 `json:"userCount"`

	FirstSeen sentryTime `json:"firstSeen"`
	LastSeen  sentryTime `json:"lastSeen"`

	Project struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"project"`
}

// eventEnvelope is the Integration Platform wrapper around an `event_alert`. Same
// outer shape as envelope — action, installation, data — with a different object under
// `data`, which is exactly why the resource header decides which one is unmarshalled
// and no path may guess.
type eventEnvelope struct {
	// Action on this door is always `triggered`: the Alert Rule fired. It is not the
	// group's lifecycle action and must not be read as one.
	Action       string `json:"action"`
	Installation struct {
		UUID string `json:"uuid"`
	} `json:"installation"`
	Data struct {
		Event *eventPayload `json:"event"`

		// TriggeredRule is the name of the Alert Rule that fired. Not used in any
		// decision, and named here because it is the field that explained the whole
		// discovery: the captured payload reads `Notify via autoissues`, which is how
		// the predecessor received feedback it was never subscribed to.
		TriggeredRule string `json:"triggered_rule"`
	} `json:"data"`
}

// eventPayload is `data.event`: ONE event, not the group. The group is present only as
// `issue_id` and `issue_url`, and that id is the Ocorrência's identity — an error that
// happens five thousand times is still one Ocorrência, however many events an Alert
// Rule fires for.
type eventPayload struct {
	IssueID string `json:"issue_id"`

	// Project is the project's numeric id, delivered as a JSON number here and as a
	// quoted string on the issue door. flexString makes the two doors agree, which is
	// what lets one dedup key cover both — a "4509380076634112" that did not equal
	// 4509380076634112 would make every Ocorrência arrive twice.
	Project flexString `json:"project"`

	// URL is the API URL of the event and the only place the Fonte's slug appears.
	URL    string `json:"url"`
	WebURL string `json:"web_url"`

	Title    string `json:"title"`
	Culprit  string `json:"culprit"`
	Level    string `json:"level"`
	Platform string `json:"platform"`

	// Environment arrives here, unlike on the issue door.
	Environment string `json:"environment"`

	// Datetime is when the event happened — not when the group was first seen.
	Datetime sentryTime `json:"datetime"`

	// Occurrence is present for the "issue platform" groups — feedback among them —
	// and absent for a plain error. Its absence is as meaningful as its contents.
	Occurrence *occurrencePayload `json:"occurrence"`
}

// occurrencePayload is Sentry's own `occurrence` block. Only what discriminates and
// what titles the Ocorrência is named; the free text a user wrote is deliberately NOT
// read here, because domain.Occurrence has no field for it and whoever renders the
// prompt digs it out of RawBody as untrusted input.
type occurrencePayload struct {
	// Type is the group type. 6001 is FeedbackGroup; see feedbackGroupType.
	Type int `json:"type"`

	IssueTitle string `json:"issueTitle"`
	Level      string `json:"level"`
	Culprit    string `json:"culprit"`
}

// flexString reads a value Sentry sends as a number on one door and as a quoted string
// on the other. The project id is the case that forced it: `"4509380076634112"` inside
// `data.issue.project.id` and `4509380076634112` inside `data.event.project`, the same
// project both times.
type flexString string

func (v *flexString) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*v = ""
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return fmt.Errorf("identificador do Sentry ilegível (%s): %w", s, err)
		}
		*v = flexString(unquoted)
		return nil
	}
	*v = flexString(s)
	return nil
}

// flexInt64 reads a number that Sentry may send either quoted or bare. In the recorded
// payloads `count` is the string "585" and `userCount` is the number 133, in the same
// object — a plain int64 field fails on the first and a plain string field fails on the
// second, and either way the value silently becomes zero for the whole delivery.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*n = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return fmt.Errorf("número do Sentry ilegível (%s): %w", s, err)
		}
		if unquoted == "" {
			*n = 0
			return nil
		}
		s = unquoted
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Sentry has been known to widen a counter into a float. Truncating beats
		// refusing an Ocorrência over a decimal point.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return fmt.Errorf("número do Sentry ilegível (%s): %w", s, err)
		}
		v = int64(f)
	}
	*n = flexInt64(v)
	return nil
}

// sentryTime reads an RFC 3339 instant, tolerating both spellings the recorded payloads
// use (`...Z` and `...+00:00`) and the fractional seconds both carry.
//
// It is deliberately forgiving: an unparseable timestamp leaves the zero time rather
// than failing the delivery. firstSeen and lastSeen decide nothing at ingest, and
// dropping a real defect because Sentry changed a date format would be a far worse
// failure than an empty column.
type sentryTime time.Time

func (t *sentryTime) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || len(s) < 2 || s[0] != '"' {
		return nil
	}
	raw, err := strconv.Unquote(s)
	if err != nil {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	*t = sentryTime(parsed.UTC())
	return nil
}
