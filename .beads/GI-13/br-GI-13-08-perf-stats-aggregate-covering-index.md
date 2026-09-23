# Bead br-GI-13-08: make the stats aggregates read an index, not the 2.3 GB table

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §3.2 (C2, the same "give the planner
something to read" move, applied to the read path). **Post-convergence addition.**

- **Bead ID**: br-GI-13-08
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-13-02 (it owns the migration runner and `schemaVersion`), br-GI-13-07 (it
  lands first and its `ALTER TABLE` occupies `migrations[2]`, so this bead's entry is `migrations[3]`)
- **Blocks**: br-GI-13-06

> **Ordering note for br-GI-13-06.** `br-GI-13-06` predates this post-convergence set and will not be
> updated: its own `Dependencies` line names only `br-GI-13-01…05` and `Blocks: None`. Read that list
> as needing to gain `br-GI-13-07` and `br-GI-13-08`, and run the re-profile **after both** (07 first,
> then this one) — otherwise it measures only C1/C2/C3 and this bead's index goes unmeasured.

> This is the **second** half of the dashboard's problem, and the smaller one. It is not a
> substitute for br-GI-13-07: that bead removes ~1.6 s of read per flush, this one removes ~0.5 s
> per dashboard load. Do 07 first; if 07 alone makes the dashboard acceptable, this bead is still
> worth landing but it is no longer urgent.

## Description

`/api/stats` runs **four** aggregate queries, one after another, on the store's single connection
(`internal/api/api.go:505-548` → `StatsSummary`, `StatsByModel`, `StatsByPeriod`, `StatsByCostSource`).
The unbounded call at `internal/web/app.js:1077` (`api('/api/stats')`) adds no `WHERE`, so *that* load
aggregates **all** history; the other two callers add a `started_at` bound
(`app.js:909` `api('/api/stats?since=24h')`, `app.js:604` `api('/api/stats?' + q.toString())`, both via
`EventFilter.whereClause`), which the covering index also serves. Which caller is bounded does not
change the fix: the walk is the same full-table scan either way.

Measured through the same driver as the process, against the live store (51,833 rows):

| | cold | warm |
|---|---|---|
| one aggregate | 1.30 s | 0.14 s |
| `/api/stats` (four, plus serialization), live | 1.93–5.47 s | ~0.55 s (derived) |

None of these queries selects a blob column — `statsSelectColumnsInner` (`store.go:1492`) is integers
and two `SUM(CASE …)` cost expressions — so the cost is not payload. It is that a 2.3 GB table's B-tree
has to be walked to reach ~52 MB of leaf pages, because each row's local portion is large enough that
the numeric columns live on no smaller structure.

**Give the planner a smaller structure to walk.** Add one covering index over exactly the columns the
four aggregates touch, so the scan reads the index instead of the table.

**Verified before writing this bead** on a synthetic table of the same shape (20,000 rows with a 2 KB
blob, `ANALYZE` run):

```
before:  SCAN events
after:   SCAN events USING COVERING INDEX idx_events_stats
grouped: SCAN events USING COVERING INDEX idx_events_stats
         USE TEMP B-TREE FOR GROUP BY
```

SQLite takes the covering index for a bare aggregate — this is not an assumption. The temp B-tree for
the grouped variants is over the handful of distinct groups, not over rows.

- **`internal/store/schema.sql`** — add, alongside the existing index block,
  `CREATE INDEX IF NOT EXISTS idx_events_stats ON events(<the column list below>);`.
- **`internal/store/store.go`** — append `migrations[3]` creating the same index, and bump
  `const schemaVersion` (`:128`) from 3 to **4**. (`br-GI-13-07` lands first and its `ALTER TABLE` is
  `migrations[2]`, which moved `schemaVersion` to 3; this bead takes the next slot.) The runner's
  invariants bind this exactly as br-GI-13-02's note describes: `schemaVersion == len(migrations)`,
  and `migrations[n]` upgrades `n` to `n+1` — so with indices 0–3, `len(migrations) == 4`.
- **Back up before the migration (CLAUDE.md "Migrations").** `migrations[3]` runs on the same live
  store br-GI-13-07 migrates, on the next boot, and it is a DDL write against the only copy of the
  captured traffic. Before `Open` runs it, copy `lens.db`, `lens.db-wal` and `lens.db-shm` to a
  separate path and state that backup path in the PR body — regardless of file size.

**The column list** — every column the four queries reference, in any order (the scans are full):

```
input_tokens, output_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
cache_read_tokens, thinking_tokens, total_prompt_tokens,
model_resolved, billing_mode, cost_source,
cost_usd, api_equivalent_cost_usd, started_at
```

### The write-side trade, stated rather than hidden

This index costs one more B-tree write per inserted row, on a story whose subject is insert cost — the
same tension br-GI-13-02 resolved in the *opposite* direction when it dropped
`idx_events_session_id`. It is justified here because it is **not** redundant: no existing index
covers these columns, the dashboard is the product's primary surface, and the added cost is one small
B-tree write per *inserted row* — paid on the same per-row transaction that already commits that row.
The consumer groups events into flush batches (that is what `flush` reduces — goroutine wake-ups and
analyzer runs), but it does **not** write them in one transaction: each event's write stays isolated,
so there is no 50-row amortization to lean on. **The bead is not done until that is measured, not
asserted**: record the per-insert delta from the store's own benchmark or a fixture timing in the PR
body, and if the delta is material, say so rather than shipping quietly.

### Alternatives considered and rejected

- **Bounding the window in the UI.** Cheapest, but it silently changes what the "total" figures mean
  for every user; the store should be able to answer its own documented query.
- **Folding the four aggregates into one scan in Go.** Fewer scans, no migration — but costs are
  floats, and re-grouping them changes the last bits of a summed cost. This repo enforces cost-figure
  invariants; not worth it for 0.4 s.

## Rationale

The dashboard's first paint depends on this route, and the route's cost is entirely "walk a 2.3 GB
B-tree to add up 13 small columns". A covering index is the native answer to exactly that, it is one
DDL statement, and — unlike every other candidate — it needs no change to query shape, no change to
what the figures mean, and no floating-point regrouping.

## Outcome Definition

- `schema.sql` creates `idx_events_stats`; `migrations[3]` creates it;
  `schemaVersion == len(migrations) == 4`.
- `EXPLAIN QUERY PLAN` for each of the four aggregate queries reports the index rather than
  `SCAN events` — assert the **property** ("no **full table scan** of `events` (the plan may still read
  `SCAN events USING COVERING INDEX idx_events_stats`)"), not the index name.
- A store left one migration behind (at `user_version = 3`, the version br-GI-13-07 leaves) reaches 4
  and holds the index; a **fresh** DB reaches the same final shape — the two homes cannot drift
  (br-GI-13-02's own test shape, repeated).
- `/api/stats` is measurably cheaper against the live store, with both the before and after recorded
  in the PR body — and the numbers reported as the **pair** (cold and warm), because the first
  measurement of this story was misleading for quoting only one.
- The insert-side delta is measured and recorded.
- No acceptance criterion is pinned to a live-store row count; every assertion is structural or built
  inside the test's own fixture.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/store/`.

## Test Specifications

- Unit Tests (`internal/store/store_test.go`):
  - `TestStatsQueriesUseTheCoveringIndex`: `EXPLAIN QUERY PLAN` for each of the four aggregate queries
    contains no `SCAN events` without a `USING COVERING INDEX` — read the plan the way br-GI-13-03's
    `TestSessionEventsForRulesUsesTheIndexWithoutASort` reads it.
  - `TestMigrateAddsTheStatsIndexAtVersionFour`: add a `preStatsIndexSchema` fixture **beside**
    `preIndexChangeSchema` (`store_test.go:1182`) that returns `schemaSQL` with the
    `CREATE INDEX IF NOT EXISTS idx_events_stats …` line text-stripped — the on-disk shape of a real v3
    store (all prior columns present) that is one migration behind *this* bead. Build the DB from that
    text, seed a row by hand, set `PRAGMA user_version = 3` — the version br-GI-13-07 leaves — then
    `Open` (which runs `migrations[3]`). Assert `userVersion == schemaVersion` (`4`) and
    `hasIndex(t, st.db, "idx_events_stats")`. Model it on
    `TestMigrateAddsTheCompositeIndexAtVersionTwo` (`store_test.go:1195-1210`), which builds via
    `preIndexChangeSchema` and sets `user_version` by hand.
    **Do not** use `buildPreChangeDB` for this: its fixture omits the columns `migrations[0]` and
    `migrations[2]` add (it text-strips the transcript block and, per 07, `req_tool_names`), and `Open`
    cannot add a column to an existing table (`CREATE TABLE IF NOT EXISTS` is a no-op), so stamping
    `user_version = 3` on it yields a database that is not a v3 store. Build from `schemaSQL` with only
    the `idx_events_stats` line removed. Neither fixture lets the *index* assertion distinguish
    `migrations[3]` (`Open` re-execs `schemaSQL` first), so pin the runner invariant
    (`schemaVersion == len(migrations) == 4`) or the migration's text as well.
  - `TestFreshSchemaMatchesTheMigratedShape` (extend br-GI-13-02's case): the fresh and migrated
    shapes agree on `idx_events_stats` too.
  - `TestStatsAggregatesAreUnchangedByTheIndex`: a fixture with known token and cost values returns
    identical `StatsSummary`, `StatsByModel`, `StatsByPeriod` and `StatsByCostSource` before and after
    — the guard that an added index changed no number. Include a **mixed billing-mode** fixture, so
    the "two billing models are never summed" invariant is exercised in the same test.
- Integration Tests: none.
- Manual (recorded in the PR body): `/api/stats` timing against the live store, cold and warm pairs,
  before and after.

## Files to Touch

- `internal/store/schema.sql` (modify — the index)
- `internal/store/store.go` (modify — `migrations[3]`, `schemaVersion` to 4)
- `internal/store/store_test.go` (modify — the cases above)

`store.go` and `store_test.go` are shared with br-GI-13-07, which **this bead depends on**: 07 lands
first and its `ALTER TABLE` occupies `migrations[2]`. The earlier claim that the two touch "disjoint
regions" was wrong — 07 also edits the migration runner (it adds a migration and bumps
`schemaVersion`), so the two beads edit that runner **in sequence, 07 first**. With 07 landed, this
bead's entry is `migrations[3]` and `schemaVersion` goes 3 → 4. **Land 07 first** so the re-profile in
br-GI-13-06 measures the larger fix.
