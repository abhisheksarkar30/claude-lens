# Bead br-GI-1-13: adminrep collector, reconcile, cost_drift

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §The four sources (D), §Storage schema (admin tables), §Reconciliation, tests 15, 20

- **Bead ID**: br-GI-1-13
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-06, br-GI-1-07
- **Blocks**: br-GI-1-14, br-GI-1-15, br-GI-1-18

## Description

Source D — the only authoritative **actually-billed dollars** — and the labelled comparison against
the locally computed figure.

**`internal/adminrep`.** Raw stdlib `net/http` with the Admin API key (read via `internal/secret`),
daily. **No Anthropic SDK**: the Admin usage/cost report endpoints are documented as curl-only and are
explicitly not covered by any SDK. Record this so a future reader does not "fix" it into an SDK
dependency.

Three collectors:

- **usage report → `admin_usage_days`**, keyed **UNIQUE(`day_start`, `model`, `workspace_id`)**.
- **cost report → `admin_cost_days`**, a **separate table with a different key**:
  **UNIQUE(`day_start`, `model`, `description`, `currency`)**. They come from two endpoints with
  different groupings and bucket widths; merging them would force one to be denormalised into the
  other's key.
- **rate-limit reports → `admin_rate_limits`** (`scope`, `workspace_id`, `model`, `group_type`,
  `limit`, `fetched_at`).

Each row also stores the fetched page's `window_start`/`window_end`. **Inserts are UPSERTs on the
natural key, never appends** (test 20): the endpoints return a multi-day window per call (7 daily
buckets by default), so a `clens refresh`, a `clens ingest --rebuild`, or a scheduler retry after a
partial failure re-fetches days already stored — the constraint makes that a no-op instead of a silent
inflation of the one ground-truth figure. `ingest_state` holds `admin:usage` / `admin:cost` → the
last-fetched `day_start` plus the fetched `[window_start, window_end)`, so a re-run re-requests only
the gap it does not hold (an optimisation; the constraint is the control).

**`internal/reconcile`.** A vs D and B vs D are compared with an explicit, **labelled** JOIN on
`(day, model)`. This is a side-by-side comparison of two labelled figures, **not a sum**, and it
cannot silently merge a subscription figure with a billed one because the `events` side is restricted
to API rows by construction (`events.cost_usd` is NULL for subscription rows):

- **computed** = `SUM(events.cost_usd)` — empty for subscription rows by the schema rule — against
  **billed** = `admin_cost_days.amount_usd`, in separate columns, never added.
- Divergence beyond a configurable threshold raises **`cost_drift`** (test 15). This closes
  deepseek-lens's stated open risk "pricing table needs manual upkeep" **for API accounts only**.
- **`cost_drift` can never fire for a subscription account** — there is no billed counterpart. The
  guard there is `model_catalog` refresh plus the `unpriced`/`approximate` labels, with an optional
  staleness signal for a shipped rate unverified for more than N days. The bead must not claim
  `cost_drift` answers the upkeep risk globally.
- A vs B is compared by `request_id` overlap and raises `source_mismatch` on token disagreement
  (declared in br-GI-1-09's `kinds.go`; the merge gate itself is br-GI-1-06/11).

**`cost_drift` is a reconcile-surface finding, not a `warnings` row.** It is per (day, model) and has
no single `event_id`, and br-GI-1-06 keeps `warnings.event_id` NOT NULL. It is declared in
br-GI-1-09's `kinds.go` (so the README kind table covers it) and returned by the reconcile surface
that `clens reconcile`, `GET /api/reconcile` and the Reconcile tab render. Same resolution as
`quota_window_approaching` in br-GI-1-12 — see the summary flag.

**Admin-key scope.** The Admin key is **organization-wide read**. `clens doctor` states this in plain
words when one is configured, and the CLI **refuses to store one without `--yes`**.

## Rationale

Dollars are the one number a user can check against a bill, so the billed figure must be stored
exactly once per (day, model) and never inflated by a re-fetch. Scoping `cost_drift` to API accounts
keeps the plan honest: six of the seven billing shapes have no billed counterpart to reconcile
against.

## Outcome Definition

- `go test ./internal/adminrep/... ./internal/reconcile/... -race` passes.
- Running the usage and cost collectors twice over the same 7-day window yields the same row counts
  and totals, not double (test 20).
- An overlapping re-fetch updates in place, leaving no duplicate `(day, model)` rows.
- `cost_drift` fires at the threshold and not below it (test 15).
- `cost_drift` never fires for a subscription-only account.
- A `clens reconcile` output shows computed and billed in separate labelled columns, never summed.
- No admin collector error message contains the Admin key.

## Test Specifications

- Unit Tests (`internal/adminrep/adminrep_test.go`):
  - **Test 20 — idempotence**: two runs over the same window → identical row counts and totals.
  - An overlapping re-fetch (days 1–7, then days 4–10) → no duplicate `(day, model)` rows; days 4–7
    updated in place, days 8–10 inserted.
  - Usage and cost land in their **separate** tables with their own keys.
  - `window_start`/`window_end` recorded per row; the `admin:*` cursor advances to the last-fetched
    `day_start` so a re-run requests only the gap.
  - The API key never appears in an error string.
  - A 401/403 records a status and reason, not the key.
- Unit Tests (`internal/reconcile/reconcile_test.go`):
  - **Test 15 — drift**: divergence above the threshold → `cost_drift`; at or below → none.
  - Computed and billed are returned in separate fields and never summed.
  - A subscription-only fixture → no `cost_drift` possible.
  - A vs B `request_id` overlap with disagreeing tokens → `source_mismatch`.
- Integration Tests: `clens reconcile` renders both columns and the drift (via br-GI-1-15).
- E2E: none.

## Files to Touch

- `internal/adminrep/adminrep.go`, `internal/adminrep/adminrep_test.go` (create)
- `internal/adminrep/reports.go` (create — usage/cost/rate-limit endpoint shapes)
- `internal/reconcile/reconcile.go`, `internal/reconcile/reconcile_test.go` (create)

The Admin key is read through `secret.Get("admin")` (br-GI-1-01's accessor) and the admin tables are
written through `store.UpsertAdminUsageDays` / `UpsertAdminCostDays` / `UpsertAdminRateLimits`
(br-GI-1-06) — this bead edits neither `internal/secret/secret.go` nor `internal/store/store.go`.
