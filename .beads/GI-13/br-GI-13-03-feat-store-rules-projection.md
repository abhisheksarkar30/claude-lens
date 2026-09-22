# Bead br-GI-13-03: `SessionEventsForRules` — the rules-shaped projection, `SessionEvents` deleted

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §2, §3.3 (C3), §5 D2/D3, §6 tests
1/2/3, §9

- **Bead ID**: br-GI-13-03
- **Priority**: P1 (high)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-13-01 (it restructures the two call sites this bead renames), br-GI-13-02
  (this bead's `EXPLAIN QUERY PLAN` test asserts against that bead's composite index)
- **Blocks**: br-GI-13-06

> **The type does not change.** `SessionEventsForRules` keeps returning `[]*store.Event`, so `analyze`
> sees the same shape and `ruleCacheInvalidatedByTools` keeps reading `ReqBody`. Only *which columns
> come off disk* changes. This is not a re-run of GI-7's D1/F2.1 — that rejected changing the *type*;
> no rule loses an input here (§9).

## Description

Follow the machinery that already exists (`store.go:1289-1309`): `eventColumnNames` (47) →
`summaryOmittedColumns` (6 blobs) → `columnsMinus` → `summarySelectColumns` (41), scanned through
`eventScanVals.dest(&es, extra...)`, guarded by `TestSummaryColumnsAreTheFullSetMinusBodies` and
`TestSummaryScanMatchesSummaryColumns`. Add the same shape a **third** time:

- `rulesOmittedColumns = columnsMinus(summaryOmittedColumns, []string{"req_body"})` — the five blobs
  the rules never read. **Derive it** rather than listing five names, so `summaryOmittedColumns` stays
  the one home for "which columns are bodies".
- `rulesColumnNames` (42) = `columnsMinus(eventColumnNames, rulesOmittedColumns)`, and
  `rulesSelectColumns = selectFrom(rulesColumnNames)`.
- `scanEventForRules(rowScanner) (*Event, error)` — `dest(&es, &reqBody)`, one extra destination in
  `eventColumnNames`' tail order.

### Rename and delete

- Rename `Store.SessionEvents` (`store.go:336`) to `Store.SessionEventsForRules`, with its query
  becoming `rulesSelectColumns + " FROM events WHERE session_id = ? ORDER BY started_at ASC"`.
- **Delete `SessionEvents`.** Both its remaining callers (`consumer.go:249`, `jsonlogs.go:551`) want
  the rules shape, so nothing needs the full-width read — and a method named `SessionEvents` that
  silently returns `nil` for `RespBody` is the footgun this repo's own naming convention
  (`ListEvents`/`ListEventsFull`, `SessionEvents`/`SessionEventsSummary`) exists to avoid.
- Call sites and interface declarations to move: `consumer.go:44`, `jsonlogs.go:35`,
  `consumer_test.go:241-242`, and the doc comment at `publishing_store.go:9-25` (both the interface
  list and the "needs no override" note name `SessionEvents`).

`SessionEventsSummary` (`store.go:358`) is **untouched** — it is a different projection for a different
caller (the session route's call list).

## Rationale

`SessionEvents` reads all 47 columns, so the rules pay to pull `resp_body`, the two header blobs and
the two transcript columns off disk for every row — the exact `_vdbeColumnFromOverflow` /
`_accessPayload` cost in §1. The rules read exactly **nine** columns (§2), and `req_body` among them
is the dominant one, so the win is the other five blobs. D2 keeps `req_body` in the projection
because dropping it would break `ruleCacheInvalidatedByTools`, and reading it *sometimes* would put a
rule's column needs in two places. The projection is derived from the existing column set so it
cannot drift.

## Outcome Definition

- `SessionEventsForRules` returns `[]*store.Event` for the same rows, in the same order
  (`started_at ASC`), as the previous full-width read — with `ReqBody` populated and the five omitted
  blobs zero.
- `SessionEvents` no longer exists; `go build ./...` proves no caller wanted the full width.
- `rulesOmittedColumns` is derived from `summaryOmittedColumns`, not hand-listed.
- The session-scoped SELECT carries `ORDER BY started_at ASC` and the planner reports **no
  `USE TEMP B-TREE FOR ORDER BY`** for it (§6 test 1). Assert the *property*, not the index name.
- The projection lists every one of the nine columns the six rules read (§6 test 3).
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/store/ ./internal/consumer/ ./internal/jsonlogs/`.

## Test Specifications

- Unit Tests (`internal/store/store_test.go`):
  - `TestSessionEventsForRulesUsesTheIndexWithoutASort` (§6 test 1) — the runnable check for the whole
    story. (a) `EXPLAIN QUERY PLAN` for the session-scoped SELECT contains **no** `USE TEMP B-TREE FOR
    ORDER BY`; (b) a **textual** assertion that the query string still contains its `ORDER BY
    started_at`, read the way `TestSummaryColumnsAreTheFullSetMinusBodies` (`:699-720`) reads query
    strings — because once the composite index exists, `WHERE session_id = ?` returns `started_at`
    order off the index even with no `ORDER BY`, so the index masks a lost sort and (a) alone is a
    negative-only assertion; (c) a fixture whose rowid order and `started_at` order disagree comes back
    in `started_at` order, catching a direction flip or a hand-built reordering.
  - `TestSessionEventsForRulesReturnsTheSameRowsInOrder` (§6 test 2): compared against a
    `ListEventsFull`-shaped expectation on a fixture, so the projection cannot silently drop a column
    the rules use.
  - `TestRulesProjectionNamesEveryColumnTheRulesRead` (§6 test 3): **two** assertions. (i) the mirrored
    check `rulesSelectColumns == eventColumnNames − rulesOmittedColumns`; and (ii) — because
    `rulesOmittedColumns` *derives* from `summaryOmittedColumns`, whose membership nothing pins, so
    (i) passes even if a body column the rules read is omitted — name the nine columns (`id`,
    `started_at`, `ended_at`, `total_prompt_tokens`, `cache_write_5m_tokens`, `cache_write_1h_tokens`,
    `cache_read_tokens`, `prefix_hash`, `req_body`) and assert `rulesSelectColumns` lists every one.
    Deliberately **not** a per-rule fixture test: a rule reading an omitted column receives a zero
    value, not an error, so such a guard fails only when the fixture is also updated — exactly when it
    has stopped doing its job.
  - `TestRulesScanMatchesRulesColumns` (§6 test 3): the 42-destination count,
    mirroring `TestSummaryScanMatchesSummaryColumns`.
- Unit Tests (`internal/consumer/consumer_test.go`):
  - `failingStore.SessionEvents` (`:241-242`) follows the rename to `SessionEventsForRules`; no other
    change.
- Integration Tests: none.

## Files to Touch

- `internal/store/store.go` (modify — `rulesOmittedColumns`, `rulesColumnNames`, `rulesSelectColumns`,
  `scanEventForRules`; rename `SessionEvents` (`:336`) to `SessionEventsForRules` and delete the old
  name. **Do not touch** the migration region br-GI-13-02 owns)
- `internal/consumer/consumer.go` (modify — `Store` interface (`:44`) and the `runSessionRule` call
  (`:249`) renamed)
- `internal/jsonlogs/jsonlogs.go` (modify — the `Store` interface (`:35`) and the `runSessionRule`
  call (`:551`) renamed)
- `internal/consumer/consumer_test.go` (modify — `failingStore.SessionEvents` (`:241-242`))
- `internal/api/publishing_store.go` (modify — the doc comment (`:9-25`), both mentions)
- `internal/store/store_test.go` (modify — the store cases above)

**Shared-file note.** `store.go`, `store_test.go` and `consumer.go` are also touched by br-GI-13-01/02
(which this bead depends on) and `consumer_test.go` also by br-GI-13-05. Keep each edit to its own
region; the dependency edges order the landings.
