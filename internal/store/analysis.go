package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/eduardotorresdev/nightwatcher/internal/domain"
)

// analysisColumns fixes the read order in one place, so the single lookup and the
// listing cannot drift apart in how they scan a row.
const analysisColumns = `
	id, project_id, source_id,
	state, verdict, priority, priority_rationale, refusal,
	attempts, failure_reason, failure_kind,
	linear_issue_id, linear_issue_url,
	installation_uuid, sentry_project_id, sentry_issue_id, substatus, action,
	category, level, title, culprit, permalink, web_url, platform, short_id,
	event_count, user_count, first_seen, last_seen, sentry_project_slug,
	environment, raw_body, received_at,
	created_at, updated_at`

// CreateAnalysis records an Ocorrência and the Análise that starts from it. The ID is
// minted here when the caller leaves it empty.
//
// A repeated delivery is rejected by the dedup index and comes back as ErrDuplicate,
// which the ingest path treats as the ordinary outcome it is. The check-then-insert
// race is closed by the index rather than by a lock: two webhook deliveries arriving
// together would both see "not recorded" and only one would survive the insert.
func (s *Store) CreateAnalysis(ctx context.Context, a domain.Analysis, now time.Time) (domain.Analysis, error) {
	return insertAnalysis(ctx, s.db, a, now)
}

// CreateQueued records an Ocorrência and puts it in line in ONE transaction.
//
// Doing it as two writes is a hole the arrival path cannot patch afterwards: a process
// that dies between them — a Dokku deploy, an OOM, a disk that blinked — leaves an
// accepted Ocorrência recorded and with nothing to claim it, and Sentry's redelivery is
// then recognised by dedup and answered "duplicada" instead of rescuing it. Dedup doing
// exactly its job would be what loses the Ocorrência, which is the predecessor's defect
// arriving through the one door built to prevent it.
//
// The two writes commit together or neither does, so the only two states the world can
// be left in are "never arrived" and "recorded and in line".
func (s *Store) CreateQueued(ctx context.Context, a domain.Analysis, now time.Time) (domain.Analysis, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Analysis{}, fmt.Errorf("abrindo transação da Ocorrência: %w", err)
	}
	defer tx.Rollback()

	if a.State == "" {
		a.State = domain.StateQueued
	}
	created, err := insertAnalysis(ctx, tx, a, now)
	if err != nil {
		return domain.Analysis{}, err
	}
	if _, err := enqueue(ctx, tx, created.ID, now, time.Time{}); err != nil {
		return domain.Analysis{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Analysis{}, fmt.Errorf("confirmando a Ocorrência %s: %w", created.ID, err)
	}
	return created, nil
}

func insertAnalysis(ctx context.Context, q execQuerier, a domain.Analysis, now time.Time) (domain.Analysis, error) {
	if a.ID == "" {
		id, err := newID()
		if err != nil {
			return domain.Analysis{}, err
		}
		a.ID = id
	}
	if a.State == "" {
		a.State = domain.StateReceived
	}
	a.CreatedAt = now.UTC()
	a.UpdatedAt = now.UTC()

	o := a.Occurrence
	args := []any{
		a.ID, asNullID(a.ProjectID), asNullID(a.SourceID),
		string(a.State), string(a.Verdict), int(a.Priority), a.PriorityRationale, string(a.Refusal),
		a.Attempts, a.FailureReason, string(a.FailureKind),
		a.IssueID, a.IssueURL,
		o.InstallationUUID, o.SentryProjectID, o.IssueID, string(o.Substatus), o.Action,
		string(o.Category), string(o.Level), o.Title, o.Culprit, o.Permalink, o.WebURL, o.Platform, o.ShortID,
		o.Count, o.UserCount, asNullMillis(o.FirstSeen), asNullMillis(o.LastSeen), o.SentryProjectSlug,
		o.Environment, o.RawBody, asNullMillis(o.ReceivedAt),
		asMillis(a.CreatedAt), asMillis(a.UpdatedAt),
	}

	query := `INSERT INTO analyses (` + analysisColumns + `) VALUES (` +
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`

	if _, err := q.ExecContext(ctx, query, args...); err != nil {
		if isUniqueViolation(err) {
			return domain.Analysis{}, fmt.Errorf("%s: %w", o.DedupKey(), ErrDuplicate)
		}
		return domain.Analysis{}, fmt.Errorf("gravando análise: %w", err)
	}
	return a, nil
}

// UpdateAnalysis writes back everything about an Análise that can change as it runs.
//
// The Ocorrência is immutable except for the environment, which is not in the webhook
// payload and only arrives later, from a separate tags call.
func (s *Store) UpdateAnalysis(ctx context.Context, a domain.Analysis, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE analyses SET
			project_id = ?, source_id = ?,
			state = ?, verdict = ?, priority = ?, priority_rationale = ?, refusal = ?,
			attempts = ?, failure_reason = ?, failure_kind = ?,
			linear_issue_id = ?, linear_issue_url = ?,
			environment = ?,
			updated_at = ?
		WHERE id = ?`,
		asNullID(a.ProjectID), asNullID(a.SourceID),
		string(a.State), string(a.Verdict), int(a.Priority), a.PriorityRationale, string(a.Refusal),
		a.Attempts, a.FailureReason, string(a.FailureKind),
		a.IssueID, a.IssueURL,
		a.Occurrence.Environment,
		asMillis(now), a.ID,
	)
	if err != nil {
		return fmt.Errorf("atualizando análise %s: %w", a.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("análise %s: %w", a.ID, ErrNotFound)
	}
	return nil
}

// Analysis returns one Análise with its sessions loaded — this is `analysis show`,
// which prints what was handed to the agent and what came back.
func (s *Store) Analysis(ctx context.Context, id string) (domain.Analysis, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+analysisColumns+` FROM analyses WHERE id = ?`, id)
	a, err := scanAnalysis(row)
	if err == sql.ErrNoRows {
		return domain.Analysis{}, fmt.Errorf("análise %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return domain.Analysis{}, fmt.Errorf("lendo análise %s: %w", id, err)
	}

	sessions, err := s.sessionsOf(ctx, id)
	if err != nil {
		return domain.Analysis{}, err
	}
	a.Sessions = sessions
	return a, nil
}

// AnalysisFilter is what `analysis list` narrows by.
type AnalysisFilter struct {
	// Outcome matches either a Triagem verdict (defeito, sugestão, dúvida, ruído) or
	// a state (recusada, falhou, publicada, encerrada). Refused, closed and failed
	// Análises stay listable: seeing what was discarded, and judging whether the
	// Triagem is getting it wrong, is the whole reason nothing is deleted.
	Outcome string

	// ProjectID and ProjectName both narrow to one Projeto; the name is what the
	// command line has in hand.
	ProjectID   int64
	ProjectName string

	// Limit caps the result. Zero means everything.
	Limit int
}

// ListAnalyses returns the most recent Análises first. Sessions are not loaded — the
// listing shows outcomes, and loading every prompt and response to print one line
// each would read the whole history into memory.
func (s *Store) ListAnalyses(ctx context.Context, f AnalysisFilter) ([]domain.Analysis, error) {
	var (
		where []string
		args  []any
	)

	if f.Outcome != "" {
		outcome, err := resolveOutcome(f.Outcome)
		if err != nil {
			return nil, err
		}
		where = append(where, `(verdict = ? OR state = ?)`)
		args = append(args, outcome, outcome)
	}
	if f.ProjectID != 0 {
		where = append(where, `project_id = ?`)
		args = append(args, f.ProjectID)
	}
	if f.ProjectName != "" {
		where = append(where, `project_id = (SELECT id FROM projects WHERE name = ?)`)
		args = append(args, f.ProjectName)
	}

	query := `SELECT ` + analysisColumns + ` FROM analyses`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	// The id breaks ties: two Ocorrências of the same burst land on the same
	// millisecond often enough that an unstable order shows up as a flaky listing.
	query += ` ORDER BY created_at DESC, id DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listando análises: %w", err)
	}
	defer rows.Close()

	var out []domain.Analysis
	for rows.Next() {
		a, err := scanAnalysis(rows)
		if err != nil {
			return nil, fmt.Errorf("lendo análise: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// states is every value the state column can hold, so `analysis list --outcome` can
// tell a filter it does not recognise from one that simply matched nothing.
var states = []domain.State{
	domain.StateReceived, domain.StateQueued, domain.StateTriaging, domain.StateAnalyzing,
	domain.StateFailing, domain.StatePublished, domain.StateClosed, domain.StateRefused,
	domain.StateFailed,
}

// resolveOutcome turns what the operator typed into the stored value, and refuses what
// is neither a Triagem verdict nor a state.
//
// Refusing matters more here than it looks: `--outcome recusadas` returning an empty
// list reads as "nothing was ever refused", which is the one conclusion this table
// exists to make impossible. A typo has to fail where the operator is looking.
func resolveOutcome(s string) (string, error) {
	if v, err := domain.ParseVerdict(s); err == nil {
		// The command line is typed without accents as often as with them, and
		// ParseVerdict already owns that aliasing.
		return string(v), nil
	}
	for _, st := range states {
		if s == string(st) {
			return s, nil
		}
	}

	known := []string{
		string(domain.VerdictDefect), string(domain.VerdictSuggestion),
		string(domain.VerdictQuestion), string(domain.VerdictNoise),
	}
	for _, st := range states {
		known = append(known, string(st))
	}
	return "", fmt.Errorf("desfecho desconhecido %q; conhecidos: %s", s, strings.Join(known, ", "))
}

// OccurrenceRecorded reports whether this Ocorrência is already on record, and under
// which Análise.
//
// There are two ways to be a repeat, because there are two doors.
//
// The exact identity — (installation, projeto Sentry, issue, substatus) — is the
// original rule, and the substatus is part of it so that a regressão of a group already
// seen is NOT swallowed: it is a new Ocorrência that happens to share a group.
//
// The first sighting — (installation, projeto Sentry, issue) among deliveries whose
// action is `created` or `triggered` — covers what the two doors made possible. The
// same group is delivered as `issue.created` AND as an `event_alert` fired by an Alert
// Rule; those are one Ocorrência described twice, not two Ocorrências. Without this,
// every error would be analysed twice, in two Claude sessions, and open two issues in
// the Destino. The substatus is deliberately ignored in this half, because an
// `event_alert` carries none at all and matching on it would never match anything.
//
// The second half is scoped to first sightings, and that scope is load-bearing: a later
// `issue.unresolved` + `regressed` must still get through, which it does, because
// `unresolved` is not a first sighting and only the exact identity applies to it.
//
// An Ocorrência with no Sentry issue id has no identity to compare, and is never
// treated as a repeat.
func (s *Store) OccurrenceRecorded(ctx context.Context, o domain.Occurrence) (string, bool, error) {
	if o.IssueID == "" {
		return "", false, nil
	}

	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM analyses
		WHERE installation_uuid = ? AND sentry_project_id = ? AND sentry_issue_id = ?
		  AND (substatus = ? OR (? = 1 AND action IN (?, ?)))
		LIMIT 1`,
		o.InstallationUUID, o.SentryProjectID, o.IssueID,
		string(o.Substatus), boolToInt(domain.IsFirstSighting(o.Action)),
		domain.ActionCreated, domain.ActionTriggered,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("verificando ocorrência já vista: %w", err)
	}
	return id, true, nil
}

// boolToInt exists because SQLite has no boolean parameter and `?` bound to a Go bool
// through database/sql is not something the driver promises to compare with `= 1`.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AppendSession records one Claude session against an Análise.
func (s *Store) AppendSession(ctx context.Context, analysisID string, sess domain.Session) (domain.Session, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (analysis_id, kind, prompt, response, started_at, ended_at, err)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		analysisID, string(sess.Kind), sess.Prompt, sess.Response,
		asMillis(sess.StartedAt), asNullMillis(sess.EndedAt), sess.Err,
	)
	if err != nil {
		return domain.Session{}, fmt.Errorf("gravando sessão da análise %s: %w", analysisID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return domain.Session{}, fmt.Errorf("gravando sessão da análise %s: %w", analysisID, err)
	}
	sess.ID = id
	return sess, nil
}

// AnalysesStartedSince counts how many Análises of one Projeto started inside the
// window, which is what the per-Projeto hourly cap is measured against.
//
// "Started" means a session was opened, not that the Ocorrência arrived. Counting
// arrivals would be wrong in exactly the case the cap exists for: a burst of thirty
// deliveries in an hour would count thirty against a cap of twenty and stall the
// Projeto for an hour, even though the queue is holding them and nothing has run.
func (s *Store) AnalysesStartedSince(ctx context.Context, projectID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT s.analysis_id)
		FROM sessions s JOIN analyses a ON a.id = s.analysis_id
		WHERE a.project_id = ? AND s.started_at >= ?`,
		projectID, asMillis(since),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("contando análises iniciadas do projeto %d: %w", projectID, err)
	}
	return n, nil
}

func (s *Store) sessionsOf(ctx context.Context, analysisID string) ([]domain.Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, prompt, response, started_at, ended_at, err
		FROM sessions WHERE analysis_id = ? ORDER BY started_at, id`, analysisID)
	if err != nil {
		return nil, fmt.Errorf("listando sessões da análise %s: %w", analysisID, err)
	}
	defer rows.Close()

	var out []domain.Session
	for rows.Next() {
		var (
			sess    domain.Session
			kind    string
			started int64
			ended   sql.NullInt64
		)
		if err := rows.Scan(&sess.ID, &kind, &sess.Prompt, &sess.Response, &started, &ended, &sess.Err); err != nil {
			return nil, fmt.Errorf("lendo sessão: %w", err)
		}
		sess.Kind = domain.SessionKind(kind)
		sess.StartedAt = fromMillis(started)
		sess.EndedAt = fromNullMillis(ended)
		out = append(out, sess)
	}
	return out, rows.Err()
}

func scanAnalysis(row scanner) (domain.Analysis, error) {
	var (
		a         domain.Analysis
		projectID sql.NullInt64
		sourceID  sql.NullInt64

		state, verdict, refusal, failureKind string
		substatus, category, level           string
		firstSeen, lastSeen, receivedAt      sql.NullInt64
		priority                             int
		createdAt, updatedAt                 int64
		rawBody                              []byte
	)

	o := &a.Occurrence
	err := row.Scan(
		&a.ID, &projectID, &sourceID,
		&state, &verdict, &priority, &a.PriorityRationale, &refusal,
		&a.Attempts, &a.FailureReason, &failureKind,
		&a.IssueID, &a.IssueURL,
		&o.InstallationUUID, &o.SentryProjectID, &o.IssueID, &substatus, &o.Action,
		&category, &level, &o.Title, &o.Culprit, &o.Permalink, &o.WebURL, &o.Platform, &o.ShortID,
		&o.Count, &o.UserCount, &firstSeen, &lastSeen, &o.SentryProjectSlug,
		&o.Environment, &rawBody, &receivedAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return domain.Analysis{}, err
	}

	a.ProjectID = projectID.Int64
	a.SourceID = sourceID.Int64
	a.State = domain.State(state)
	a.Verdict = domain.Verdict(verdict)
	a.Priority = domain.Priority(priority)
	a.Refusal = domain.RefusalReason(refusal)
	a.FailureKind = domain.AgentErrorKind(failureKind)
	a.CreatedAt = fromMillis(createdAt)
	a.UpdatedAt = fromMillis(updatedAt)

	o.Substatus = domain.Substatus(substatus)
	o.Category = domain.Category(category)
	o.Level = domain.Level(level)
	o.FirstSeen = fromNullMillis(firstSeen)
	o.LastSeen = fromNullMillis(lastSeen)
	o.ReceivedAt = fromNullMillis(receivedAt)
	o.RawBody = rawBody

	return a, nil
}
