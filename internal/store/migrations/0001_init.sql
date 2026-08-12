-- Schema v1. Two rules run through the whole file:
--
--   * Every timestamp is an INTEGER of Unix milliseconds in UTC. Text dates would
--     only compare correctly while every writer agreed on the exact same layout and
--     zone, and the lease deadline is compared inside SQL — one writer using a local
--     offset would hand out an item that is still leased.
--   * No column here ever holds a credential. The database has to be readable and
--     copyable without leaking one; secrets live only in environment variables.

-- Projeto: one repository, one Destino, one or more Fontes Sentry.
CREATE TABLE projects (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL UNIQUE,
    repo              TEXT    NOT NULL,
    branch            TEXT    NOT NULL,
    linear_project_id TEXT    NOT NULL,
    created_at        INTEGER NOT NULL
);

-- Fonte Sentry. The label is a free-form rótulo that becomes a Linear label, so
-- "show me everything from the API layer" is a board filter and not a text search.
-- The slug is unique across every Projeto because it is what the webhook resolves by.
CREATE TABLE sources (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    sentry_slug TEXT    NOT NULL UNIQUE,
    label       TEXT    NOT NULL
);

CREATE INDEX sources_by_project ON sources (project_id);

-- The operating numbers, here so they can be recalibrated without a
-- redeploy. Never a secret.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Análise: the full journey of an Ocorrência, from delivery to outcome, with the
-- Ocorrência flattened into it.
CREATE TABLE analyses (
    id TEXT PRIMARY KEY,

    -- Nullable on purpose. A recusa by invalid signature or unknown Fonte has no
    -- Projeto to point at and still has to be recorded: no code path ends with an
    -- Ocorrência discarded in silence.
    project_id INTEGER REFERENCES projects (id),
    source_id  INTEGER REFERENCES sources (id),

    state              TEXT    NOT NULL,
    verdict            TEXT    NOT NULL DEFAULT '',
    priority           INTEGER NOT NULL DEFAULT 0,
    priority_rationale TEXT    NOT NULL DEFAULT '',
    refusal            TEXT    NOT NULL DEFAULT '',

    attempts       INTEGER NOT NULL DEFAULT 0,
    failure_reason TEXT    NOT NULL DEFAULT '',
    failure_kind   TEXT    NOT NULL DEFAULT '',

    -- The issue in the Destino. Spelled `linear_` because `sentry_issue_id` below
    -- identifies a different thing entirely, and a column named plain `issue_id`
    -- invites precisely that confusion at every join and every scan.
    linear_issue_id  TEXT NOT NULL DEFAULT '',
    linear_issue_url TEXT NOT NULL DEFAULT '',

    -- The Ocorrência as delivered.
    installation_uuid   TEXT    NOT NULL DEFAULT '',
    sentry_project_id   TEXT    NOT NULL DEFAULT '',
    sentry_issue_id     TEXT    NOT NULL DEFAULT '',
    substatus           TEXT    NOT NULL DEFAULT '',
    action              TEXT    NOT NULL DEFAULT '',
    category            TEXT    NOT NULL DEFAULT '',
    level               TEXT    NOT NULL DEFAULT '',
    title               TEXT    NOT NULL DEFAULT '',
    culprit             TEXT    NOT NULL DEFAULT '',
    permalink           TEXT    NOT NULL DEFAULT '',
    web_url             TEXT    NOT NULL DEFAULT '',
    platform            TEXT    NOT NULL DEFAULT '',
    short_id            TEXT    NOT NULL DEFAULT '',
    -- Sentry's `count`, spelled out: a bare column named `count` reads as the
    -- aggregate function at every call site that mentions it.
    event_count         INTEGER NOT NULL DEFAULT 0,
    user_count          INTEGER NOT NULL DEFAULT 0,
    first_seen          INTEGER,
    last_seen           INTEGER,
    sentry_project_slug TEXT    NOT NULL DEFAULT '',
    environment         TEXT    NOT NULL DEFAULT '',
    -- The exact bytes delivered: the HMAC is over raw bytes and this is the audit
    -- record of what arrived.
    raw_body            BLOB,
    received_at         INTEGER,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Dedup. Sentry delivers the same Ocorrência more than once, always; recognising it
-- is an obligation, not a courtesy. The substatus belongs in the key because without
-- it a regressão of an already-seen group would be swallowed as a repeat delivery —
-- the dedup bug turned inside out.
--
-- Partial because a recusa by invalid signature carries no Ocorrência identity at
-- all, only four empty strings: without the predicate the second such recusa would
-- collide with the first and go unrecorded.
CREATE UNIQUE INDEX analyses_dedup
    ON analyses (installation_uuid, sentry_project_id, sentry_issue_id, substatus)
    WHERE sentry_issue_id <> '';

CREATE INDEX analyses_by_project ON analyses (project_id, created_at);
CREATE INDEX analyses_by_created ON analyses (created_at);

-- The queue, leased with a deadline. A worker that dies lets the deadline lapse and
-- the item becomes takeable again on the next cycle, which is how this survives
-- SIGKILL, OOM and a Dokku deploy with no coordination beyond SQLite itself.
CREATE TABLE queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    analysis_id TEXT    NOT NULL REFERENCES analyses (id) ON DELETE CASCADE,
    -- NULL means unclaimed. The claim predicate reads `owner IS NULL`, so an empty
    -- string here would look like a live claim held by nobody.
    owner       TEXT,
    lease_until INTEGER,
    attempts    INTEGER NOT NULL DEFAULT 0,
    -- How retry backoff is expressed without a sleeping goroutine.
    available_at INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    done_at      INTEGER
);

CREATE INDEX queue_takeable ON queue (done_at, available_at, created_at);
CREATE INDEX queue_by_analysis ON queue (analysis_id);

-- One row per Claude session. This is what `analysis show` prints, and the raw
-- material for diagnosing a bad Triagem.
CREATE TABLE sessions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    analysis_id TEXT    NOT NULL REFERENCES analyses (id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL,
    prompt      TEXT    NOT NULL,
    response    TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    err         TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX sessions_by_analysis ON sessions (analysis_id, started_at);
