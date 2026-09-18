# Bead br-GI-1-12: Snapshot poller, quota engine, calibration

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §The four sources (C), §Quota engine, §Billing model (no invented numbers), §Security posture, tests 16, 17

- **Bead ID**: br-GI-1-12
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-06, br-GI-1-07
- **Blocks**: br-GI-1-14, br-GI-1-15, br-GI-1-18

## Description

Source C — the only quota truth for a subscription — and the engine that compares the computed burn
against it. Quota is *the* question a subscription user has, and nothing else answers it.

**`internal/snapshot`** polls claude.ai's internal usage endpoint with the cookie `sessionKey` (read
via `internal/secret`, the only package that may read it), and writes to `quota_snapshots`: **one row
per window per poll** — `observed_at`, `account`, `window`, `utilization_pct`, `resets_at`, `status`,
`raw`. One row per window rather than fixed `session_pct`/`weekly_pct` columns, because the endpoint
returns **different window sets on different plans** (5-hour, 7-day, and per-model 7-day windows); a
fixed pair of columns would silently drop whichever windows the user's plan actually has.

**Tolerant extraction (test 16).** The endpoint is undocumented and can change or break:

- A renamed, missing or extra window field records `parse_error` or an extra `window` row and **does
  not crash**.
- A 401 records `unauthorized`.
- An unreachable or unparseable snapshot marks that window `unavailable` in `quota_snapshots.status`
  and in `GET /api/sources` — **fail-soft**: tokens keep being recorded and the quota view says what it
  does not know.
- The cookie expires → an explicit re-auth path (br-GI-1-15/17, `POST /api/secrets`) **and** a visible
  degraded state, never a silent failure.
- Collector errors report a status and a reason, **never the credential**.

`ingest_state` holds `snapshot:<account>` → last poll.

**`internal/quota`** computes, from `events`, the burn inside each rolling window and compares it to
the snapshot:

- **Windows are rolling, not calendar.** Evaluated at any instant as `[now-5h, now)` and
  `[now-7d, now)`, per account and per model family — a windowed query over `events`, and it works
  identically for both billing modes.
- **Snapshot cross-check.** Each `quota_snapshots` row is compared with the computed burn at that
  instant.
- **`quota.calibration`** reports tokens-per-percent, **learned from your own history** — the only
  honest way to translate between tokens and "percent of plan" when Anthropic publishes no conversion.
- **Projection.** With limits configured, `quota_window_approaching` fires when the current burn rate
  projects to exhaust the window before it resets. With limits unconfigured, the **same computation
  runs** and reports **`unconfigured`** instead of a percentage.
- **Limits are empty by default and never invented.** They become known one of two ways: the user
  configures them, or the engine *learns* them — when a snapshot window reaches 100%, the burn
  recorded at that moment is an empirical observation of the limit, **offered for confirmation rather
  than applied silently**. It never guesses that "Max 20x" means a particular number of messages.
- `unconfigured` is a first-class labelled state: stored, queried and rendered, never collapsed to
  `0%`.

**`quota_window_approaching` is a quota-surface finding, not a `warnings` row.** It is window-scoped
and has no single `event_id`, and br-GI-1-06 keeps `warnings.event_id` NOT NULL. It is declared in
br-GI-1-09's `kinds.go` (so the README kind table covers it) and returned by the `quota` surface that
br-GI-1-15/18 render. The same reasoning applies to `cost_drift` in br-GI-1-13. See the summary flag.

## Rationale

Source C is undocumented and fragile, so it is built tolerant and fail-soft, and its failures are
visible rather than silent. The calibration-from-own-history rule is what keeps the tool honest about
the tokens↔percent translation it cannot look up.

## Outcome Definition

- `go test ./internal/snapshot/... ./internal/quota/... -race` passes.
- A plan with 5-hour, 7-day and per-model windows stores one row per window per poll; no window is
  dropped.
- A renamed/missing/extra field and a 401 are recorded as `parse_error`/`unauthorized` without a crash
  (test 16).
- With limits unset, the projection reports `unconfigured`, not a percentage.
- A snapshot reaching 100% offers a learned limit; it is not applied silently.
- Rolling windows are half-open `[now-W, now)`; a call exactly at the boundary is handled consistently.
- No collector error message contains the `sessionKey` value.

## Test Specifications

- Unit Tests (`internal/snapshot/snapshot_test.go`):
  - **Test 16 — tolerance**: a renamed window field → `parse_error`; a missing field → the row is still
    written with the windows present; an extra window → an extra `window` row; a 401 →
    `unauthorized`; an unparseable body → `unavailable`.
  - One row per window per poll; three windows → three rows.
  - The credential is never echoed in any error string.
- Unit Tests (`internal/quota/quota_test.go`):
  - Rolling 5h and 7d burns over a synthetic `events` fixture; half-open boundary correctness.
  - Snapshot cross-check pairs each snapshot with the burn at its instant.
  - `calibration` learns tokens-per-percent from a fixture with a 100% snapshot.
  - Configured limits → `quota_window_approaching` fires when the projection exhausts the window.
  - Unconfigured limits → `unconfigured`, never `0%`.
  - A learned limit is offered, not applied.
- Integration Tests (`internal/snapshot` + temp store):
  - A poll writes `quota_snapshots` rows and advances the `snapshot:<account>` cursor.
- E2E: none.

## Files to Touch

- `internal/snapshot/snapshot.go`, `internal/snapshot/snapshot_test.go` (create)
- `internal/quota/quota.go`, `internal/quota/quota_test.go` (create)
- `internal/quota/calibration.go` (create)

The `sessionKey` is read through `secret.Get("sessionKey")` (br-GI-1-01's accessor, returning
`ErrUnset` when unset) — this bead does **not** edit `internal/secret/secret.go`; that package's API
is complete at br-GI-1-01 so br-GI-1-12 and br-GI-1-13 are not two writers of one file.
