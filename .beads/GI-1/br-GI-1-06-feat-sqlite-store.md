# Bead br-GI-1-06: SQLite store, schema, single ingest writer, merge path, purge

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Storage schema, §Invariants 3/4/5, §Cross-source identity, §Reconciliation, tests 5, 12a, 12b, 20

- **Bead ID**: br-GI-1-06
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-01
- **Blocks**: br-GI-1-07, br-GI-1-08, br-GI-1-11, br-GI-1-12, br-GI-1-13, br-GI-1-16

## Description

The one SQLite file, its whole schema, the single ingest writer, and the two invariants the rest of
the tool relies on.

**Connection.** `modernc.org/sqlite` (pure Go, no cgo) — this is the plan's one uncontested non-stdlib
dependency. `Open(dbPath)` sets WAL, `foreign_keys(ON)`, `busy_timeout(5000)`, and a **single write
connection** serializing all writers. `CREATE TABLE IF NOT EXISTS` only — no migration framework in
v1; the schema is created whole. Times are Unix nanoseconds (`UnixNano()`/`time.Unix(0, ns)` round-trip).

**Schema** — every table in the plan's §Storage schema table, created here even where a later bead
populates it:

- `events` — one row per observed turn, from source A or B. `request_id` **NOT NULL UNIQUE**;
  `source`, `source_refs`, `first_source`, `started_at`, `ended_at`, `auth_kind`, `account`,
  `billing_mode`, `model_requested`/`model_resolved`, the six token columns
  (`input_tokens`, `output_tokens`, `cache_write_5m_tokens`, `cache_write_1h_tokens`,
  `cache_read_tokens`, `thinking_tokens`), `total_prompt_tokens`, `service_tier`, `speed`, `effort`,
  `inference_geo`, `stop_reason`, `stop_category`, `is_sidechain`, `session_id`, `project`,
  `git_branch`, `client_version`, `cli_entrypoint`, the two cost columns (below), `cost_source`,
  `prefix_hash`, `replay_of`, `replay_edits`, `capture_complete`.
- `events` proxy-only nullable columns: `method`, `path`, `status`, `req_headers`, `resp_headers`,
  `req_body`, `resp_body`.
- `sessions` — `id` (`s_<unix-ms>_<8hex>`), `prefix_hash`, `first_seen`/`last_seen`, `request_count`,
  token totals, `priced_count`/`unpriced_count`, `model_set`, `warning_count`, and the **two-column
  cost split** (below).
- `warnings` — `event_id` FK → `events(id)` ON DELETE CASCADE, `kind`, `severity`, `detail`, `path`,
  `created_at`, **UNIQUE(`event_id`, `kind`)**.
- `admin_usage_days` — UNIQUE(`day_start`, `model`, `workspace_id`), plus `day_start`,
  `window_start`/`window_end`, token columns, `raw`, `fetched_at`.
- `admin_cost_days` — UNIQUE(`day_start`, `model`, `description`, `currency`), plus
  `window_start`/`window_end`, `amount_usd`, `currency`, `raw`, `fetched_at`.
- `admin_rate_limits` — `scope`, `workspace_id`, `model`, `group_type`, `limit`, `fetched_at`.
- `quota_snapshots` — one row per window per poll: `observed_at`, `account`, `window`,
  `utilization_pct`, `resets_at`, `status`, `raw`.
- `prices` — `model`, `input_rate`, `output_rate`, `cache_write_5m_rate`, `cache_write_1h_rate`,
  `cache_read_rate`, `fast_input_rate`, `fast_output_rate`, `batch_multiplier`, `effective_from`,
  `source`.
- `model_catalog` — `model_id` PK, `display_name`, `max_input_tokens`, `max_output_tokens`,
  `capabilities`, `fetched_at`, `source`.
- `ingest_state` — `key` PK, `value`, `status`, `error`, `updated_at`.

**Invariant 4 — derived prompt total.** `total_prompt_tokens` is computed **by the store** on every
write as `input + cache_write_5m + cache_write_1h + cache_read`, so no caller can get it wrong. This
is a tested invariant, not a convention (test 12a).

**Invariant 5 — a cost figure's billing model is carried in the column it lives in.** There is no
single `cost_usd`:

- a `subscription` row writes `api_equivalent_cost_usd` and leaves `cost_usd` **NULL**;
- an `api` row writes `cost_usd`;
- **an unpriced row leaves both NULL** — `cost_usd` is NULL when `billing_mode='subscription'` **or**
  when `cost_source='unpriced'`; `api_equivalent_cost_usd` is populated only when
  `billing_mode='subscription'` and is NULL when the row is unpriced. An unpriced API row is **not**
  `$0.00`.
- `sessions` carries the same split: `total_cost_usd` (API-billed rows only) and
  `total_api_equivalent_cost_usd` (subscription rows only), each **NULL** — never `$0.00` — when the
  session has no row of that model. A session whose API rows are all `unpriced` reads NULL on
  `total_cost_usd`; `unpriced_count` (shown alongside) disambiguates that from a session with no API
  row at all.

**Invariant 3 — one writer package per source, a single serialized connection, and one deliberate
`UPDATE`.** The proxy consumer (br-GI-1-08) is the only writer for `source='proxy'`; `jsonlogs`
(br-GI-1-11) the only writer for `source='jsonl'`; purge is a third writer (in-process sharing the
store's connection; a separate `clens purge` process serialized by WAL + `busy_timeout`). The
cross-source merge is the first `UPDATE` on `events`, and it is this store's job to make it one
serialized statement. An insert that collides on `request_id` **merges rather than duplicates**:

- `source_refs` gains the new source; `first_source` is preserved.
- Token counts are **not** re-added — the columns are a union, not a sum. The **complete capture
  wins**: A when `capture_complete`, otherwise B. A merge therefore **rewrites** a row's token (and
  cost) columns.
- `merge` does **not** rewrite `session_id` (the row's grouping identity, not a token column), so
  exactly one session is the re-derivation target.
- `source_mismatch` is raised **only** when two *complete* sources disagree on the token counts — a
  real parser bug — and never when one source simply has nothing to contribute (an absent or partial
  A against a full B is a `0 vs N` non-disagreement). The store's merge path is where the
  disagreement is detected; it reports a `source_mismatch` finding for the merged `event_id`.
- The columns A structurally cannot supply — `client_version`, `project`, `git_branch`,
  `is_sidechain`, `cli_entrypoint` — are preserved from whichever writer supplied them.
- **The analyzer seam re-runs on a merge**, so the warning attach is an **upsert keyed by
  `(event_id, kind)`**, never a second `INSERT`. `UNIQUE(event_id, kind)` is the mechanism (not a
  replace-on-merge DELETE): the pipeline runs the analyzer after every insert and has no "this was a
  merge" branch, and a delete-and-reattach would drop a kind the first source raised if the merged row
  does not re-raise it, losing the union the merge exists to produce.
- **In the same transaction as the merge `UPDATE`**, the owning session's totals are **re-derived,
  not incremented**: token/cost totals reconstructed from `events`, and `warning_count` from
  `warnings` (never from `events`, never by increment). The re-derivation wins over the incremental
  fold for that session.
- **`warnings.event_id` stays NOT NULL** (`UNIQUE(event_id, kind)`). Findings that are not per-event
  — `cost_drift` (per day×model) and `quota_window_approaching` (per rolling window) — are **not**
  rows in this table; they are surface findings returned by `reconcile` (br-GI-1-13) and `quota`
  (br-GI-1-12). This is the plan's ambiguity resolved — see the summary flag. Every per-event kind,
  including `source_mismatch`, does carry an `event_id`.

**`Open`, plus the reader/writer surface** the later beads call: `InsertEvent`,
`InsertEvents` (batched), `UpsertWarnings(ctx, eventID, warnings)`, session `UpsertSession` /
`reconcileSession`, `ListEvents`/`CountEvents` (filter + pagination), `GetEvent`, `StatsSummary`,
`StatsByModel`, `StatsByPeriod(granularity)`, `StatsByCostSource`, `ListSessions`/`GetSession` /
`LatestSessionByPrefix`, `ListWarnings`/`CountWarnings`/`WarningSummary`, `PurgeOlderThan`,
`PurgeUnpriced`, `CountPurgeable`, `PurgeableBytes`, `Vacuum`, `RedactCheck`. Also the source-C/D writer surface the collectors call,
so all SQL lives here rather than in the collector packages: `InsertQuotaSnapshot` /
`ListQuotaSnapshots` (br-GI-1-12) and the natural-key UPSERTs `UpsertAdminUsageDays` /
`UpsertAdminCostDays` / `UpsertAdminRateLimits` (br-GI-1-13) — `INSERT … ON CONFLICT(<natural key>)
DO UPDATE`, never a bare insert. Every aggregate query that could blend models **groups or filters
by `billing_mode`**.

## Rationale

The schema is where the plan's two hardest guarantees stop being conventions: the two-column cost
split makes `SUM(cost_usd)` unable to merge billing models with or without a JOIN, and
`UNIQUE(event_id, kind)` is what makes the warning union testable rather than asserted. Both are
schema constraints precisely so a future query cannot forget them.

## Outcome Definition

- `go test ./internal/store/... -race` passes.
- Write, read back, WAL confirmed, redaction verified, FK cascade verified (test 5).
- A duplicate `request_id` insert merges: one row, `first_source` preserved, `source_refs` listing
  both, tokens not summed.
- `total_prompt_tokens` equals the four-class sum on every row (test 12a).
- Schema invariant test (12b): a subscription row has `cost_usd IS NULL` unconditionally and
  `api_equivalent_cost_usd` non-NULL unless unpriced; an API row is the reverse; an unpriced row of
  either mode has NULL on both.
- `UpsertWarnings` twice with the same `(event_id, kind)` leaves one row.
- A merge that rewrites a row's tokens re-derives the session's totals to the recomputed values.

## Test Specifications

- Unit Tests (`internal/store/store_test.go`):
  - **Round trip (test 5)**: insert → read back all fields; WAL is on; FK cascade deletes a row's
    warnings; `RedactCheck` flags a planted `x-api-key`.
  - **Derived total (test 12a)**: `total_prompt_tokens == input + cache_write_5m + cache_write_1h +
    cache_read` for every fixture row.
  - **Billing-mode invariants (test 12b, schema level)**: subscription → `cost_usd IS NULL` and
    `api_equivalent_cost_usd IS NOT NULL` unless unpriced; api → reverse; unpriced (either mode) →
    both NULL.
  - **Session split**: a mixed session yields non-NULL on both `total_cost_usd` and
    `total_api_equivalent_cost_usd`; an all-unpriced API session yields NULL on `total_cost_usd`.
  - **Merge**: two inserts, same `request_id` → one row, `source_refs` = both, `first_source` = first,
    tokens = the complete capture's.
  - **Merge precedence**: truncated A (`capture_complete=false`) + complete B → B's tokens; no
    `source_mismatch`. Two complete sources disagreeing → `source_mismatch`.
  - **Session re-derivation**: merge that rewrites a row's tokens leaves the session totals equal to
    the recomputed `SUM` over `events`, not the old increment.
  - **Warning upsert**: same `(event_id, kind)` twice → one row; `warning_count` re-derived equals
    `COUNT(DISTINCT kind)`.
  - **Admin idempotence (test 20, store half)**: a second UPSERT of the same `(day, model, …)` key
    updates in place, leaving no duplicate rows.
  - **Purge**: `PurgeOlderThan` and `PurgeUnpriced` delete the right rows and leave counts consistent.
  - `-race` clean with concurrent readers during a write batch.
- Integration Tests: br-GI-1-08 exercises insert-through-pipeline; br-GI-1-11 the JSONL merge.
- E2E: none.

## Files to Touch

- `internal/store/schema.sql` (create)
- `internal/store/store.go`, `internal/store/store_test.go` (create)
- `internal/store/types.go` (create — `Event`, `Warning`, `Session`, filter/summary types)
- `internal/store/merge.go`, `internal/store/merge_test.go` (create)
