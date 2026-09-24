# Bead br-GI-16-12: Integration — restart plus archival end-to-end, and a live-store *copy* migration check

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Test strategy (B, C, Invariants, Race), §B.5, §C.10 "Migration on the live store".

- **Bead ID**: br-GI-16-12
- **Priority**: P1 (high — the story's landing gate)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-16-01 through br-GI-16-11 (bead 13 is independent and not required)
- **Blocks**: None

## MIGRATION BACKUP RULE (repo CLAUDE.md §Migrations)

Step 3 exercises the 4 → 5 migration. **Never run it against the live `D:/clens/lens.db`.** Before touching
anything, copy `lens.db`, `lens.db-wal` and `lens.db-shm` from the live directory to a separate scratch path
(the session scratchpad), **state that backup path**, and run the migration only on a second copy of that backup.
If the live proxy is running, copying the three files together while it runs may be inconsistent: if so, say so
and use the newest consistent copy, or ask the operator to `clens shutdown` first (the operator's call — never
shut down `127.0.0.1:8797` from a test).

## Description

Add cross-feature checks not owned by any single bead, all on temp dirs and ephemeral ports (never `8797/8798`):

1. **Restart e2e**: in-process `serve` (temp DB) writes `serve.state.json`; `runRestart` shuts it down, waits for
   both ports, respawns the test binary via the helper-process pattern, waits healthy, reports the gap. Then a
   bad `--exe` helper that never becomes healthy ⇒ rollback to the previous exe, non-zero exit.
2. **Archival e2e**: synthetic store with rows aged past `HotDays`, mixed body shapes (all three, req-only,
   empty `[]byte{}`, NULL); run `archive run --yes`; then verify: call-detail API returns identical bodies,
   `BodiesArchived == "restored"`; aggregates (`/api/stats`, sessions) are **identical before and after**
   archival; `purge` afterward leaves no sentinel bytes in `archive/`; `archive restore` returns bodies
   byte-identically; `reload` changing `HotDays` takes effect on the next tick.
3. **Live-store copy migration check**: on the backup **copy** (path stated), open with the new binary, assert
   `user_version == 5`, row counts equal pre-migration, `events` columns unchanged, `body_archive` empty, and
   `serve`'s boot completes without archiving (nothing is past the 7-day window until 2026-09-27). Record
   timings in the commit body.
4. **Invariants gate**: `internal/proxy` still imports only `sink`/`config`; `internal/store` still does not
   import `pricing`; `go vet ./...` on Windows **and** `GOOS=linux go vet ./...`; `go test -race` for the
   consumer `SetAccounts` swap and the archiver/reader test; `go test ./...` fully green, including
   `TestEveryCarriedOverCommandIsDispatched` at 26 entries.

Tests go in a new `internal/cli/integration_gi16_test.go` (build tag not needed; keep the copy-migration check
skipped unless `CLENS_LIVE_COPY_DB` points at a copy, so CI never depends on the operator's machine). Manual and
stated as such, not automated: a real Claude session across a real `restart`, and the first real archival on 2026-09-27.

## Rationale

Each bead tests itself in isolation; the risky behaviours (restart+rollback, archival not changing aggregates,
migration on real data) only show up composed.

## Outcome Definition

- All four checks pass; `go test ./...`, `go vet ./...`, `GOOS=linux go vet ./...` and `go test -race` on the named packages are green.
- Aggregates before and after archival are byte-identical.
- The backup path used for the migration check is stated in the bead's commit body; the live DB was never opened by a test.

## Test Specifications

- Unit Tests: none new beyond the integration tests.
- Integration Tests:
  - Restart e2e (healthy new exe; unhealthy `--exe` ⇒ rollback ⇒ non-zero).
  - Archive → detail → purge → no-bytes-left → restore round trip; aggregates equal before/after.
  - `reload` `HotDays` live change (with `Archiver` window observed on the next tick).
  - Migration check on a copy, gated on `CLENS_LIVE_COPY_DB`.
  - Import-guard test for `internal/proxy` and `internal/store` (extend existing guards if they exist).

## Files to Touch

- `internal/cli/integration_gi16_test.go` (create)
- `internal/proxy/imports_test.go` or the existing import-guard test file (modify only if the existing guard needs the assertion extended; otherwise re-confirm)
