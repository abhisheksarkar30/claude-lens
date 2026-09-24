# Bead br-GI-16-05: `HotDays` config, validation and `doctor` line

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream C, §C.7.

- **Bead ID**: br-GI-16-05
- **Priority**: P1 (high — foundation for all archival beads)
- **Original Estimate**: 1h
- **Dependencies**: None
- **Blocks**: br-GI-16-06, br-GI-16-09 (which owns the `reload` `HotDays` arm and therefore depends on this)

## Description

Add `HotDays int` to `internal/config/config.go`, following how `RetentionDays` is wired (struct field, TOML
file key `HotDays`, env `CLENS_HOT_DAYS`, flag `--hot-days`, precedence flag > env > file > default):

- Default **7**. `0` disables archival (bodies stay hot forever — the pre-GI-16 behaviour).
- Negative is rejected by `Validate`.
- **`HotDays > RetentionDays` when both are > 0 is rejected by `Validate`** with a message like
  "HotDays exceeds RetentionDays: nothing would ever be archived before it is deleted".
- The flag must be added to `applyFlags`' closed flag set (`config.go:238-252`).
- `clens doctor` (`internal/cli/doctor.go`) prints the effective `HotDays`, and a WARN when the archive
  directory (`<dir of DBPath>/archive`) exists-or-would-be-created but is unwritable. Keep it to a print plus that
  WARN; no archival logic here. Do not touch `toolNamesBackfillCheck` (bead 08 owns re-confirming it).

## Rationale

Every later archival bead reads `cfg.HotDays`. Without a validated, defaulted value the archiver could be
configured to never run or to run past retention.

## Outcome Definition

- `--hot-days`, `CLENS_HOT_DAYS` and file key `HotDays` each set the value with the documented precedence; default 7.
- `Validate` rejects negative and `HotDays > RetentionDays > 0`; accepts `HotDays = 0` and `RetentionDays = 0` with any `HotDays`.
- `clens doctor` shows `HotDays` and the unwritable-archive-dir WARN.
- `go build ./... && go vet ./... && go test ./internal/config/ ./internal/cli/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests (`internal/config/config_test.go`):
  - Default is 7; env overrides file; flag overrides env.
  - `Validate`: `-1` rejected; `HotDays=30, RetentionDays=7` rejected; `HotDays=7, RetentionDays=7` accepted; `HotDays=30, RetentionDays=0` accepted; `HotDays=0` accepted.
  - Non-integer env/flag value gives a clear error.
- Unit Tests (`internal/cli/doctor_test.go`):
  - `doctor` output contains the `HotDays` line; an unwritable archive dir yields the WARN (use a path under a file, not a chmod, so it works on Windows).
- Integration Tests: none.

## Files to Touch

- `internal/config/config.go` (modify)
- `internal/config/config_test.go` (modify)
- `internal/cli/doctor.go` (modify)
- `internal/cli/doctor_test.go` (modify)
