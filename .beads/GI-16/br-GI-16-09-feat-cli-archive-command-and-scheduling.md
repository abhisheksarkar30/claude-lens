# Bead br-GI-16-09: `clens archive status/run/restore`, `serve` scheduling, and `reload`'s `HotDays` arm

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream C, §C.8 (all), §C.6 (restore ordering), §C.10; Workstream B §B.4 step 3 (the `HotDays` arm).

- **Bead ID**: br-GI-16-09
- **Priority**: P1 (high)
- **Original Estimate**: 2h (at the ceiling; see summary — carries the story's widest surface)
- **Dependencies**: br-GI-16-04 (reload mechanism and settings struct), **br-GI-16-05 (`HotDays` exists — explicit; this bead owns the `HotDays` reload arm)**, br-GI-16-07 (`Archiver`, `GCArchive`), br-GI-16-08 (archive-aware writers)
- **Blocks**: br-GI-16-11, br-GI-16-12

## Description

No schema change, so the migration backup rule does not trigger here. All tests use temp stores and ephemeral
ports; never the live DB or `127.0.0.1:8797`.

### `clens archive` (`internal/cli/archive.go`, one entry point with sub-verbs)

- **`status`**: hot-window boundary, archived/unarchived row counts, archive dir size and file count, rows
  skipped for the backfill reason, any `missing` markers, and **restore duplicates** (a day row present with no
  `body_archive` marker and non-NULL hot bodies). Prints UTC days.
- **`run [--dry-run] [--yes]`**: same gate as every writer — `--yes` to write, `--dry-run` reports. Runs the
  `Archiver` (bead 07) from a second process (WAL allows it; small batches keep `busy_timeout(5000)` from being
  exhausted). Uses `cfg.HotDays`; `HotDays == 0` prints that archival is disabled.
- **`restore --since X --until Y [--dry-run] [--yes]`**: the inverse of the archiver's step 4. Intended with
  `serve` stopped (warn otherwise). Mechanism-drops-a-marker rules, each mandatory:
  - **One hot transaction per row**: the body `UPDATE`s and the marker `DELETE` commit together or not at all.
  - **Never drop a marker for a body you did not restore.** Missing day file or any undecodable blob ⇒ report
    the row, leave the marker **intact**, no partial application; abort before the marker `DELETE`.
  - **Delete the day-file row only *after* the hot transaction commits — never before** (the reverse order leaves
    marker present, hot NULL and no archive copy: a body in neither place). A crash between them leaves a harmless
    duplicate that `status` reports; **no collector** is added for it, and do **not** widen `GCArchive`'s predicate.
  - Restore respects the archive's mask: write back exactly the columns the day row's mask holds.
  - Fault-injection hook stops after each step.
- Use `takeFlag`/existing writer-command patterns (`purge.go`) for `--yes`/`--dry-run`.
- Register `archive` in `cmd/clens/main.go` and add `archive` to `carriedOver` in `cmd/clens/main_test.go`
  (final count 26 = 23 + `restart` + `reload` + `archive`; the hand-pinned list fails `go test ./...` otherwise).

### `serve` scheduling (`internal/cli/serve.go`)

One goroutine, started **after the listeners are up** (never on the boot path — the redaction self-test and
retention purge run there). It runs the archiver at boot and on the existing 24-hour ticker, **after
`purgeOnStartup`**, then `GCArchive`. **Fail-open**: an error is logged, never fatal. It reads `HotDays` from the
atomic settings struct bead 04 introduced. `HotDays == 0` ⇒ goroutine idles.

### `reload`'s `HotDays` arm

Extend bead 04's `reloadFunc`: `HotDays` joins `Accounts` and `RetentionDays` as live-applied (add it to the
atomic struct, the `applied` list, and its diff). A reduced `HotDays` makes rows archivable on the **next tick**,
not immediately. The live-apply set stays exactly three fields.

## Rationale

Bodies exist in a second at-rest location, so the operator needs to see the state (`status`), run it on demand
(`run`), and undo it (`restore`) — and `restore` is the one path that can create the body-in-neither-place state
if its ordering is wrong. `serve` must schedule the archiver without ever blocking capture.

## Outcome Definition

- `archive status` output covers every field above, including `missing` markers and restore duplicates.
- `archive run --yes` archives synthetic aged rows; without `--yes` it changes nothing; `--dry-run` reports counts.
- `archive restore --yes` returns bodies to hot rows byte-identically and drops markers; a crash/decode failure leaves marker intact and the body recoverable from one side.
- `serve` runs the archiver at boot and on the ticker after purge, fail-open, off the boot path.
- `clens reload` after editing `HotDays` reports it under `applied`; the archiver uses it on the next tick.
- `go build ./... && go vet ./... && go test -race ./internal/cli/ ./cmd/clens/` passes, then `go test ./...`; `GOOS=linux go vet ./...` passes.

## Test Specifications

- Unit Tests (`internal/cli/archive_test.go`):
  - `status` on empty, partially archived, missing-file and restore-duplicate stores.
  - `run` gating: no flag ⇒ refuses/dry; `--dry-run` no writes; `--yes` writes; `HotDays==0` disabled message.
  - `restore` happy path (byte-identical, marker gone, day row gone); partial mask restore.
  - **Restore fault injection**: stop (a) after hot commit before day-row delete ⇒ duplicate reported by `status`, body hot, marker gone, `GCArchive` collects nothing; (b) decode failure ⇒ marker intact, body still in the archive, no hot change; (c) missing day file ⇒ same as (b). Assert no state of "no marker and all three hot bodies NULL".
  - `restore` deletes the day row strictly after the hot commit (order asserted via the hook).
  - `carriedOver` includes `archive`; `TestEveryCarriedOverCommandIsDispatched` passes at 26.
- Unit Tests (`internal/cli/serve_test.go`, `reload_test.go`):
  - Scheduler runs archiver after `purgeOnStartup`, not on the boot path, logs-not-fatal on error.
  - `reload` with changed `HotDays` ⇒ `applied` contains it, the atomic value changes; unchanged ⇒ `unchanged:true`; invalid (`HotDays > RetentionDays`) ⇒ 400, nothing applied.
- Integration Tests: in-process serve with a synthetic aged store, ephemeral ports: archiver runs and a call-detail fetch still returns bodies.

## Files to Touch

- `internal/cli/archive.go` (create)
- `internal/cli/archive_test.go` (create)
- `internal/cli/serve.go` (modify — scheduler goroutine; `HotDays` in the atomic settings)
- `internal/cli/reload.go` (modify — `HotDays` arm in the diff/apply)
- `internal/cli/serve_test.go`, `internal/cli/reload_test.go` (modify)
- `internal/store/archive.go` (modify — restore primitive `RestoreBodies` and the status queries, if not already exposed)
- `cmd/clens/main.go` (modify — register `archive`)
- `cmd/clens/main_test.go` (modify — add `archive` to `carriedOver`)
