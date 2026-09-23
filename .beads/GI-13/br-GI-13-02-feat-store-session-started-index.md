# Bead br-GI-13-02: `(session_id, started_at)` composite index, drop the redundant single-column index, `schemaVersion = 2`

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §3.2 (C2), §5 D4, §6 test 9, §7 R3/R4

- **Bead ID**: br-GI-13-02
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: br-GI-13-03 (its `EXPLAIN QUERY PLAN` test asserts **against this index**), br-GI-13-06

> **One DDL change, two homes.** The store's migration runner keys on `PRAGMA user_version`. An index
> the schema exec re-creates must also appear in the ordered `migrations` slice, and a new migration
> means `len(migrations)` grew, which means the `const schemaVersion` beside it must grow with it.
> Miss the constant and every `Open` refuses the database it just wrote; miss the migration and an
> existing database never gets the index.

## Description

Add, in **both** homes `schema.sql` requires (`schema.sql:1-9`: it is the current shape, and
`store.go`'s runner owns every `ALTER`/`DROP`):

- **`internal/store/schema.sql`** — replace line 68,
  `CREATE INDEX IF NOT EXISTS idx_events_session_id ON events(session_id);`, with
  `CREATE INDEX IF NOT EXISTS idx_events_session_started ON events(session_id, started_at);`.
  `CREATE … IF NOT EXISTS` throughout; never `ALTER` — the schema exec is the *current* shape and runs
  on every `Open`.
- **`internal/store/store.go`** — append `migrations[1]`:
  `CREATE INDEX IF NOT EXISTS idx_events_session_started ON events(session_id, started_at);` followed by
  `DROP INDEX IF EXISTS idx_events_session_id;`, and change `const schemaVersion = 1` (`:128`) to `2`.

**The runner's invariants bind this change** (`store.go:126-210`): `schemaVersion` must equal
`len(migrations)`; `migrations[n]` upgrades version `n` to `n+1`; each migration runs in its own
transaction with the version bump **inside** it, so a failure part-way leaves the version where it was
and the next `Open` retries. Adding one entry therefore means `migrations` has length 2 and
`schemaVersion` is 2 — no other value compiles into a correct store.

### Why dropping the single-column index is justified, not incidental

`idx_events_session_id` is a strict **prefix** of the new composite, and every `session_id`-filtered
query in the store is served by the composite's leading column. Verified — the four sites are
`store.go:337` and `:359` (`ORDER BY started_at ASC`, served *better* by the composite),
`store.go:494` (an `EventFilter.whereClause` condition with no `ORDER BY` of its own), and
`store.go:733` / `:748` (aggregates, no `ORDER BY`). Leaving a redundant B-tree means every insert
pays for two indexes on one column — on a story whose subject is insert cost, that is the wrong trade.

`doctor_test.go:67` asserts against `store.SchemaVersion()`, not a literal, so the version bump is
safe there. `SchemaVersion()` remains exported and `doctor` still prints it (`doctor.go:221-226`); a
newer binary than the store reports the gap rather than failing — existing behaviour, kept true.

## Rationale

`store.SessionEvents` (`store.go:337`) is
`SELECT <47 columns> FROM events WHERE session_id = ? ORDER BY started_at ASC` with **no index that
satisfies the `ORDER BY`**, so SQLite materializes the session's rows, carries the multi-MB BLOBs
through an external merge sort, and spills them to a temp file on disk — the `_vdbePmaWriteBlob` /
`_vdbeIncrSwap` / `_vdbePmaReadBlob` signature in §1. The composite index lets the sort be satisfied
by the index, removing the spill. It is also what makes any future history bound cheap, which §8 names
as the reason to add it now.

## Outcome Definition

- `schema.sql` creates `idx_events_session_started ON events(session_id, started_at)` and no longer
  creates `idx_events_session_id`.
- `migrations[1]` creates the composite index and drops `idx_events_session_id`; `schemaVersion ==
  len(migrations) == 2`.
- A store opened at `user_version = 1` **with rows present** reaches version 2, holds the composite
  index, has no `idx_events_session_id`, and still returns its rows.
- A **fresh** DB reaches the same final shape as the migrated one — the two homes cannot drift.
- `TestMigrateHealsAPartialDatabase` (`store_test.go:956`) is updated: its fixture drops an index the
  *new* `schema.sql` still creates (e.g. `idx_events_started_at`), not `idx_events_session_id`.
- `clens doctor` still prints `db_schema` with the store's version, now 2.
- No acceptance criterion is pinned to a live-store row count; every assertion is structural or built
  inside the test's own fixture.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/store/` and `go test ./internal/cli/` (doctor).

## Test Specifications

- Unit Tests (`internal/store/store_test.go`):
  - `TestMigrateAddsTheCompositeIndexAtVersionTwo` (§6 test 9): build a database at `user_version = 1`
    with rows (via the existing `buildPreChangeDB` helper), `Open` it, assert `userVersion == 2`, that
    `hasIndex(t, st.db, "idx_events_session_started")` is true, that
    `hasIndex(t, st.db, "idx_events_session_id")` is false, and that the seeded rows are still
    returned by `SessionEvents`.
  - `TestFreshSchemaMatchesTheMigratedShape`: a fresh DB reaches the same index set as the migrated one
    — assert the two share `idx_events_session_started` and neither carries `idx_events_session_id`, so
    a future edit to one home alone fails the other.
  - `TestMigrateHealsAPartialDatabase` (modify, `:956-990`): change the dropped index at `:963`/`:965`
    to one `schema.sql` still creates (`idx_events_started_at`), keeping the "the always-run exec heals
    a missing index" assertion at `:979` meaningful. As written it fails at its own `t.Fatalf` once
    `schema.sql` stops creating `idx_events_session_id`.
- Integration Tests: none — the migration is exercised entirely by `Open` in the store tests.

## Files to Touch

- `internal/store/schema.sql` (modify — line 68)
- `internal/store/store.go` (modify — `schemaVersion` (`:128`); append `migrations[1]`)
- `internal/store/store_test.go` (modify — the three cases above; `TestMigrateHealsAPartialDatabase`
  at `:956`)

`store.go` and `store_test.go` are shared with br-GI-13-03 (the projection vars / `SessionEvents`
rename and its store tests). Keep this bead's edits to the migration region and the `Open` path, and
br-GI-13-03's to the projection region; br-GI-13-03 depends on this bead, so land this one first.
