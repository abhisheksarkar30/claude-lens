-- Schema for the one claude-lens SQLite file. CREATE TABLE IF NOT EXISTS
-- only -- no migration framework in v1; the schema is created whole. Times
-- are Unix nanoseconds.

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
    req_headers             TEXT,
    resp_headers            TEXT,
    req_body                BLOB,
    resp_body               BLOB
);

CREATE INDEX IF NOT EXISTS idx_events_session_id ON events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_started_at ON events(started_at);
CREATE INDEX IF NOT EXISTS idx_events_cost_source ON events(cost_source);

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
