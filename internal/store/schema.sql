-- Schema for the one claude-lens SQLite file. This file is the *current*
-- shape, created whole on a fresh database; a database that already exists is
-- brought forward by store.go's PRAGMA user_version runner, which owns every
-- ALTER. Each piece of SQL has one home -- no PRAGMA and no ALTER here.
--
-- Every statement is IF NOT EXISTS, so Open can run this unconditionally: the
-- exec is a no-op against an existing database and repairs a partial one. It
-- is deliberately not atomic, and the runner's stamp ordering accounts for
-- that. Times are Unix nanoseconds.

CREATE TABLE IF NOT EXISTS events (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id              TEXT NOT NULL UNIQUE,
    source                  TEXT NOT NULL,
    source_refs             TEXT NOT NULL DEFAULT '',
    first_source            TEXT NOT NULL,
    started_at              INTEGER NOT NULL,
    ended_at                INTEGER,
    auth_kind               TEXT NOT NULL DEFAULT '',
    account                 TEXT NOT NULL DEFAULT '',
    billing_mode            TEXT NOT NULL DEFAULT '',
    model_requested         TEXT NOT NULL DEFAULT '',
    model_resolved          TEXT NOT NULL DEFAULT '',
    input_tokens            INTEGER NOT NULL DEFAULT 0,
    output_tokens           INTEGER NOT NULL DEFAULT 0,
    cache_write_5m_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_write_1h_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
    thinking_tokens         INTEGER NOT NULL DEFAULT 0,
    total_prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    service_tier            TEXT NOT NULL DEFAULT '',
    speed                   TEXT NOT NULL DEFAULT '',
    effort                  TEXT NOT NULL DEFAULT '',
    inference_geo           TEXT NOT NULL DEFAULT '',
    stop_reason             TEXT NOT NULL DEFAULT '',
    stop_category           TEXT NOT NULL DEFAULT '',
    is_sidechain            INTEGER NOT NULL DEFAULT 0,
    session_id              TEXT NOT NULL DEFAULT '',
    project                 TEXT NOT NULL DEFAULT '',
    git_branch              TEXT NOT NULL DEFAULT '',
    client_version          TEXT NOT NULL DEFAULT '',
    cli_entrypoint          TEXT NOT NULL DEFAULT '',
    cost_usd                REAL,
    api_equivalent_cost_usd REAL,
    cost_source             TEXT NOT NULL DEFAULT '',
    prefix_hash             TEXT,
    replay_of               TEXT NOT NULL DEFAULT '',
    replay_edits            TEXT NOT NULL DEFAULT '',
    capture_complete        INTEGER NOT NULL DEFAULT 1,

    -- proxy-only, nullable for a source='jsonl' row.
    method                  TEXT,
    path                    TEXT,
    status                  INTEGER,
    -- NULL iff the row has no request body (structurally for a JSONL row, or
    -- because --body-policy dropped it); otherwise the request's tool names
    -- in body order, JSON-encoded ('[]' when the body declares none). This is
    -- what lets the session-scoped rules compare tool names between turns
    -- without reading req_body -- see internal/store/store.go's
    -- encodeToolNames, the one place this contract is enforced at write time.
    req_tool_names          TEXT,
    req_headers             TEXT,
    resp_headers            TEXT,
    req_body                BLOB,
    resp_body               BLOB,
    -- Transcript-only, and deliberately not req_body: a transcript excerpt is
    -- a reconstruction of intent, not the request that produced it, and
    -- writing one into req_body would make it indistinguishable from a
    -- capture inside a column the cross-source merge already has precedence
    -- rules for.
    transcript_content      BLOB,
    transcript_role         TEXT
);

CREATE INDEX IF NOT EXISTS idx_events_session_started ON events(session_id, started_at);
CREATE INDEX IF NOT EXISTS idx_events_started_at ON events(started_at);
-- Covers the four /api/stats aggregate queries (StatsSummary, StatsByModel,
-- StatsByPeriod, StatsByCostSource): every column they SUM, GROUP BY, or
-- filter on, so the aggregate is served entirely out of the index without
-- walking the table's 2GB+ of blob-bearing rows. This folds in
-- idx_events_cost_source (dropped below): that index only ever served
-- StatsByCostSource's GROUP BY and PurgeUnpriced's equality DELETE, and this
-- one now covers the former outright. PurgeUnpriced trades an index seek for
-- a full scan of this (much smaller, blob-free) index -- acceptable because
-- it is an infrequent manual `clens purge`, not a per-request path.
CREATE INDEX IF NOT EXISTS idx_events_stats ON events(
    input_tokens, output_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
    cache_read_tokens, thinking_tokens, total_prompt_tokens, model_resolved,
    billing_mode, cost_source, cost_usd, api_equivalent_cost_usd, started_at
);

CREATE TABLE IF NOT EXISTS sessions (
    id                             TEXT PRIMARY KEY,
    prefix_hash                    TEXT NOT NULL DEFAULT '',
    first_seen                     INTEGER NOT NULL,
    last_seen                      INTEGER NOT NULL,
    request_count                  INTEGER NOT NULL DEFAULT 0,
    input_tokens                   INTEGER NOT NULL DEFAULT 0,
    output_tokens                  INTEGER NOT NULL DEFAULT 0,
    cache_write_5m_tokens          INTEGER NOT NULL DEFAULT 0,
    cache_write_1h_tokens          INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens              INTEGER NOT NULL DEFAULT 0,
    thinking_tokens                INTEGER NOT NULL DEFAULT 0,
    total_prompt_tokens            INTEGER NOT NULL DEFAULT 0,
    priced_count                   INTEGER NOT NULL DEFAULT 0,
    unpriced_count                 INTEGER NOT NULL DEFAULT 0,
    model_set                      TEXT NOT NULL DEFAULT '',
    warning_count                  INTEGER NOT NULL DEFAULT 0,
    total_cost_usd                 REAL,
    total_api_equivalent_cost_usd  REAL
);

CREATE TABLE IF NOT EXISTS warnings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id   INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    severity   TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    path       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE(event_id, kind)
);

CREATE TABLE IF NOT EXISTS admin_usage_days (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    day_start          INTEGER NOT NULL,
    window_start       INTEGER NOT NULL,
    window_end         INTEGER NOT NULL,
    model              TEXT NOT NULL,
    workspace_id       TEXT NOT NULL DEFAULT '',
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    raw                TEXT NOT NULL DEFAULT '',
    fetched_at         INTEGER NOT NULL,
    UNIQUE(day_start, model, workspace_id)
);

CREATE TABLE IF NOT EXISTS admin_cost_days (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    day_start    INTEGER NOT NULL,
    window_start INTEGER NOT NULL,
    window_end   INTEGER NOT NULL,
    model        TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    amount_usd   REAL NOT NULL,
    currency     TEXT NOT NULL,
    raw          TEXT NOT NULL DEFAULT '',
    fetched_at   INTEGER NOT NULL,
    UNIQUE(day_start, model, description, currency)
);

CREATE TABLE IF NOT EXISTS admin_rate_limits (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scope        TEXT NOT NULL,
    workspace_id TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    group_type   TEXT NOT NULL DEFAULT '',
    limit_value  INTEGER NOT NULL,
    fetched_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS quota_snapshots (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    observed_at      INTEGER NOT NULL,
    account          TEXT NOT NULL,
    window           TEXT NOT NULL,
    utilization_pct  REAL,
    resets_at        INTEGER,
    status           TEXT NOT NULL,
    raw              TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS prices (
    model               TEXT NOT NULL,
    input_rate          REAL,
    output_rate         REAL,
    cache_write_5m_rate REAL,
    cache_write_1h_rate REAL,
    cache_read_rate     REAL,
    fast_input_rate     REAL,
    fast_output_rate    REAL,
    batch_multiplier    REAL,
    effective_from      INTEGER NOT NULL,
    source              TEXT NOT NULL,
    PRIMARY KEY (model, effective_from)
);

CREATE TABLE IF NOT EXISTS model_catalog (
    model_id         TEXT PRIMARY KEY,
    display_name     TEXT NOT NULL DEFAULT '',
    max_input_tokens INTEGER NOT NULL DEFAULT 0,
    max_output_tokens INTEGER NOT NULL DEFAULT 0,
    capabilities     TEXT NOT NULL DEFAULT '',
    fetched_at       INTEGER NOT NULL,
    source           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ingest_state (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);
