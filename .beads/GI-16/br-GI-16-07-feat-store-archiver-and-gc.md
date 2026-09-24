# Bead br-GI-16-07: `Archiver` (batched, verified, ordered) and `GCArchive`

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream C, §C.4 (all), §C.6 (GC predicate), §C.10 (risk rows).

- **Bead ID**: br-GI-16-07
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-16-06 (schema, day-file write/read helpers, archive-dir field)
- **Blocks**: br-GI-16-08, br-GI-16-09

## Description

No schema change here (bead 06 owns it), so the migration backup rule does not trigger; tests run on synthetic
aged rows in temp stores and **never on the live DB** (with today's data nothing is archivable until 2026-09-27).

Add `Archiver` and `Store.GCArchive` to `internal/store/archive.go`.

### The invariant (every mechanism below derives from it)

The day-file row is the authority for what the archive holds; its coverage is **monotone** (columns only added).
The marker mirrors it and is itself monotone. Step 4 NULLs exactly the columns the file's mask covers. `hydrate`
reads the file row. Therefore no body exists in neither place, and any stale artifact can only *under*-claim.

### Candidate selection

Rows with `started_at < now − HotDays·24h`, at least one body column non-NULL (`typeof(col) != 'null'`, header
only, never reads the blob), and `NOT EXISTS (SELECT 1 FROM body_archive …)`. Query must use
`idx_events_started_at`. **Skip and count** rows awaiting `backfill-tool-names` (`req_body` present,
`req_tool_names IS NULL`) — archiving them would strand them.

### One batch (≤ 20 rows **or** ≤ 8 MiB raw body, whichever first; all in one UTC day)

1. **Read** the batch's bodies from the hot DB (short hold of the single connection; GI-13's hang was a long
   hold — never hold it across compress/write).
2. **Compress and write** into the day file in one archive transaction (monotone upsert from bead 06); derive
   `body_mask` inside it from the columns actually written non-NULL.
3. **Verify inside the same archive transaction, before commit**: `count(*)` and the three length sums read back
   equal what was written. Commit.
4. **Only then** one hot transaction per batch. Per event, **re-read the day file row's `body_mask` inside that
   transaction** (the file row is the authority), then:
   - `INSERT INTO body_archive (event_id, day, archived_at, body_mask) VALUES (?,?,?,:mask) ON CONFLICT(event_id)
     DO UPDATE SET body_mask = body_archive.body_mask | excluded.body_mask` — the conflict arm **ORs**, so the
     marker is monotone and can only lag the file. **Not** `INSERT OR IGNORE`, **not** a plain `SET body_mask =
     excluded.body_mask`.
   - `UPDATE events SET req_body = CASE WHEN :mask & 1 THEN NULL ELSE req_body END, resp_body = CASE WHEN :mask &
     2 THEN NULL ELSE resp_body END, transcript_content = CASE WHEN :mask & 4 THEN NULL ELSE transcript_content
     END WHERE id=?` — the **same** `:mask`. Never a blanket three-column NULL (a `mergeEvents` backfill landing
     between step 1 and here writes a body the mask does not list).
   - Do **not** use `_txlock=immediate`/`BEGIN IMMEDIATE` (plan rejects it: changes the mode of all eleven
     `BeginTx` sites and cannot order the day-file read anyway). Add a code comment saying why.
5. Yield a short sleep (default 50 ms, injectable for tests) so the consumer flush and dashboard reads interleave.

`ctx` is checked between batches; a half-run leaves a consistent store. **No `VACUUM`** anywhere. A column that
arrives after archival stays hot and is never re-archived (documented corollary; add no machinery).

Expose an injectable fault hook (e.g. `afterStep func(step int) error`) so tests can stop after each step.
`Archiver` reads its window from an `atomic`-friendly value/func so bead 09 can hand it the live `HotDays`.

### `GCArchive`

Deletes day-file `bodies` rows that are **true orphans** and removes day files that become empty. **A true orphan
is: no `body_archive` marker for that `event_id` AND the hot row's `req_body`, `resp_body` and
`transcript_content` are all NULL** (vacuously true for a purged, now-absent event). The second clause is
load-bearing: without it GCArchive would delete the only copy of a body between an archiver's step 3 and 4.

## Rationale

Bodies are ~99.5% of the store and grow ~0.7 GB/day. The ordering (copy → verify → commit → only then NULL)
plus the monotone marker is what makes "purge means purge" and "never lose a body" both hold across crashes and
a second `clens archive run` process.

## Outcome Definition

- Synthetic aged rows are archived: bodies gone from `events`, present in the day file, marker present; hydrating
  returns byte-identical bodies. Recent rows and un-backfilled rows are untouched (the latter counted).
- Fault injection after each step leaves every body recoverable from one side; re-running is idempotent.
- Two archivers with different masks leave both the file's mask **and the marker's mask** as the union; the marker never regresses.
- `GCArchive` between an injected step 3 and step 4 deletes nothing; after purge it deletes the orphan copies.
- The archiver never makes a concurrent `ListEvents` wait longer than a stated bound.
- `go build ./... && go vet ./... && go test -race ./internal/store/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests (`internal/store/archiver_test.go`):
  - Round trip (bytes in == bytes out, NULL vs empty); single-body row; a day with one row; batch caps (20 rows, 8 MiB).
  - `HotDays` boundary at exactly the cutoff (`started_at == now − HotDays·24h` is not archived; one ns older is).
  - Fault injection after steps 1, 2, 3, 4: every body recoverable from one side; idempotent re-run.
  - Late row into an already-archived day upserts (monotone), does not replace.
  - Two-archiver different-mask case (a `mergeEvents` backfill between the two step-1 reads): file mask = union; **marker mask never regresses below what the wider archiver committed**.
  - Step 4 NULLs only masked columns: a body backfilled between step 1 and step 4 stays hot.
  - Skipped-for-backfill rows are skipped and counted.
  - `EXPLAIN QUERY PLAN` guard: candidate query uses `idx_events_started_at` and does not scan blobs (same shape as GI-13's guards).
  - `ctx` cancelled between batches leaves a consistent store.
  - `GCArchive`: true orphan collected; marker-bearing row spared; hot-bodies-still-present (crash-after-step-3 state) spared **including with `GCArchive` run concurrently**; empty day file removed.
- Integration / perf Tests:
  - Archiver-vs-reader latency: run the archiver while a second goroutine times `ListEvents`; fail if any read waits beyond a stated bound (GI-13 regression).
  - Benchmark over a synthetic 500-row day so batch size is chosen from a number (record it in the commit body).
  - `go test -race` clean for the archiver/reader test.

## Files to Touch

- `internal/store/archive.go` (modify — `Archiver`, `GCArchive`; file created by bead 06)
- `internal/store/archiver_test.go` (create)
