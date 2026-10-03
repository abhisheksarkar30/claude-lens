# Bead br-GI-22-04: Warn when a write-heavy command finds a live serve

**Plan Reference**: `docs/planning/GI-22-prices-set-clobber-and-rebuild-backfill-gaps.md` v8 (`<!-- version=8, status=converged -->`), §3.3, §5.1, §5.2, §5.4, §8 bead 04. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-22-04
- **Priority**: P1 (high — a live `ingest --rebuild` already produced sustained `SQLITE_BUSY` on serve's capture path)
- **Status**: done
- **Original Estimate**: 90m
- **Dependencies**: None
- **Blocks**: br-GI-22-05
- **Commit**: `GI#22 feat: warn when write commands find a live serve (br-GI-22-04)`

## Description

A long write against the same SQLite file as a live `clens serve` contends with serve's capture. `SetMaxOpenConns(1)` is per process (`docs/context/storage-schema.md` persistence rule "One writer per source"; `docs/context/workflows.md` around the collector note). Nothing in `ingest` / `reprice` / `reflag` / `purge` tells the operator that serve is up.

`internal/cli/restart.go:150-158` already probes `GET /api/health` with a 2-second timeout (`restart.go:151`) and treats HTTP 200 as live. `restart` dials only after `dialableDashboardAddr` (`restart.go:70-71`). The same rewrite is what `shutdown.go:46` and `reload.go:138` do. `dialableDashboardAddr` (`internal/cli/shutdown.go:78-88`) maps wildcard hosts `0.0.0.0`, `::`, and empty to `127.0.0.1`. The operator config shape this exists for is `DashboardAddr = 0.0.0.0:8798`. Skipping the rewrite reports "not live" on that config.

Extract the probe into `internal/cli/livecheck.go`:

```go
func probeServeHealth(dashboardAddr string, timeout time.Duration) bool
```

Before dialing, the helper must call `dialableDashboardAddr(dashboardAddr)`. Then `GET http://<rewritten>/api/health` with the caller's timeout, and return whether the status is 200. Match `healthy` (`restart.go:150-158`): a dial error is false, and the body is closed. Do not modify `restart.go`, `healthy`, `internal/cli/doctor.go`, or `clens doctor`. Doctor's `WARN` vocabulary is the word to reuse (`statusWarn` at `doctor.go:33`, printed at `doctor.go:117`), not a new doctor check.

Callers pass `2 * time.Second`, the timeout `healthy` already uses.

On a live hit, print one line to the `io.Writer` the command already uses. The line contains the token `WARN` and names the contention: a live serve is answering dashboard `/api/health`, so this write can contend with serve's capture (`SQLITE_BUSY`). Then proceed. The probe is not a confirmation gate, does not change the process exit, and does not consult `--yes`. A miss prints no liveness line.

**When to call it.**

- `runIngest` (`internal/cli/ingest.go:26-68`): only when `rebuild` is true. `cfg` is in scope after `config.Load` (`ingest.go:29-32`). Call the helper after `cfg.Validate()` (`ingest.go:36-37`) and before `resetJSONLCursors` (`ingest.go:48-52`), passing `cfg.DashboardAddr`. A non-`--rebuild` ingest does not probe. `ingest --rebuild` has no `--dry-run` and always writes, so it always probes on `--rebuild`.
- `runReprice` (`internal/cli/reprice.go:24-57`): `cfg` is already kept (`reprice.go:31`). Call the helper only in the `!dryRun` branch, after `openStore` has succeeded, before `RepriceCosts` (`reprice.go:44`). Do not add a model argument to that call; br-GI-22-05 changes the `RepriceCosts` signature later and depends on this bead so the two edits to `runReprice` are sequential. Leave the call's current arguments alone.
- `runReflag` (`internal/cli/reflag.go:35`) and `runPurge` (`internal/cli/purge.go:50`) currently discard config: `_, st, err := openStore(args)`. Change that one binding to `cfg, st, err := openStore(args)`. `openStore` already returns `(*config.Config, *store.Store, error)` (`internal/cli/format.go:198-208`). Then call the helper only when `!dryRun`, after `openStore`, before the write. `reprice`, `reflag`, and `purge` already refuse to write unless `--yes` or `--dry-run` is set (`reprice.go:27-29`, `reflag.go:31-33`, `purge.go:46-48`). A bare invocation still exits at that refuse, before the probe. A `--dry-run` opens the store, reads, writes nothing (`purge` also skips `VACUUM` under `--dry-run`, `purge.go:68-71`), and prints no WARN line.

`runPurge` returns "nothing to do" at `purge.go:43-44` before `openStore` when neither `--older-than`, `--unpriced`, nor `--vacuum` is set. A WARN test that passes only `--yes` never reaches the probe.

Do not turn the warning into a non-zero exit. With nothing listening, each command proceeds exactly as it does today.

**Tests** go in a new `internal/cli/livecheck_test.go` so this bead does not edit `internal/cli/reprice_test.go` (br-GI-22-05 owns the new case there). Every test calls `withHome(t)` (`internal/cli/doctor_test.go:17-24`) before the command. Those commands open `config.Default`'s `DBPath` (`~/.clens/lens.db` under `$HOME`) when `--db-path` is absent (`ingest.go:39`, `reprice.go:31`, `reflag.go:35`, `purge.go:50`). On Windows, `ingest` walks `%USERPROFILE%\.claude\projects` unless `USERPROFILE` is cleared; `withHome` clears it. `claudeConfigDir` (`internal/cli/doctor.go:336-349`) uses `USERPROFILE` before `HOME`.

The ingest WARN test builds its JSONL tree at `filepath.Join(home, ".claude", "projects", ...)`, the same way `TestIngestRebuildRereadsWithoutDuplicating` does (`internal/cli/additions_test.go:54-64`), so `--rebuild` does not read the operator's transcripts.

The wildcard-bind case is `TestIngestRebuildWarnsWhenServeIsLive` only, matching `TestShutdownDialsLoopbackForAWildcardDashboardAddr` (`internal/cli/shutdown_test.go:17-41`): the listener is loopback (`httptest.NewServer` or `127.0.0.1`), the command is passed `--dashboard-addr 0.0.0.0:<port>`, and the assertion is the request `Host` header `127.0.0.1:<port>`. A WARN line is a second check on that test, not a substitute for the Host assertion. A WARN line from a listener bound on `0.0.0.0` is not proof the rewrite is wired in: `healthy` reports live for any HTTP 200 from `http://` plus the configured address (`restart.go:150-157`), and a wildcard bind accepts a dial of that same address. That ingest command also passes `--allow-remote`, because `runIngest` calls `cfg.Validate()` (`ingest.go:34-37`) and `0.0.0.0` fails `IsLoopbackHost` (`internal/config/config.go:435-441`) unless `AllowRemote` is set (`config.go:410-421`; the flag is `allow-remote`, `config.go:253`). `runReprice`, `runReflag`, and `runPurge` do not call `Validate`, so their WARN tests may pass `--dashboard-addr 127.0.0.1:<port>` of the fake listener and do not need `--allow-remote`.

The fake listener answers `GET /api/health` with HTTP 200. Seed a database with `openTestStore` (`internal/cli/additions_test.go:29-40`) so `openStore` has a file under the temp home; the WARN tests assert the line and a nil error, not a row mutation. An empty store is enough: `reprice --yes` and `reflag --yes` print their zero-count lines, and `purge --yes --unpriced` deletes nothing.

The no-listener case must not use the machine's real dashboard port. Bind a loopback port and release it, as `TestShutdownReportsAnUnreachableDashboard` does (`shutdown_test.go:44` onward), and pass that `--dashboard-addr`.

## Rationale

The reproduced harm is sustained write-lock contention on serve's capture path. The operator currently gets no signal before `ingest --rebuild`, `reprice --yes`, `reflag --yes`, or `purge --yes` start that write. A hard failure was rejected: serve-not-running is the common single-user case, and a second gate on top of `--yes` would fail routine maintenance against a false positive.

## Outcome Definition

`go test ./internal/cli/ -count=1 -run 'TestIngestRebuildWarnsWhenServeIsLive|TestIngestRebuildProceedsWhenServeIsDown|TestIngestWithoutRebuildDoesNotWarnWhenServeIsLive|TestRepriceYesWarnsWhenServeIsLive|TestReflagYesWarnsWhenServeIsLive|TestPurgeYesWarnsWhenServeIsLive|TestRepriceDryRunDoesNotWarnWhenServeIsLive|TestReflagDryRunDoesNotWarnWhenServeIsLive|TestPurgeDryRunDoesNotWarnWhenServeIsLive'` passes.

Manual negative control, once: point the with-listener WARN assertion at a run that never starts the fake listener. That assertion must fail to find `WARN`. Restore the listener after the check. Do not commit a test that expects WARN with nothing listening.

`go test ./internal/cli/ -count=1 -run 'TestRestart|TestShutdown|TestDoctor'` still passes. `restart.go` and `doctor.go` are untouched.

## Test Specifications

- `internal/cli/livecheck_test.go`: `TestIngestRebuildWarnsWhenServeIsLive` — `withHome`, JSONL tree under that home, loopback `httptest` server answering `GET /api/health` with 200, `runIngest` with `--rebuild`, `--dashboard-addr 0.0.0.0:<port>`, and `--allow-remote`. Assert the recorded `Host` is `127.0.0.1:<port>`, the writer contains `WARN`, and the error is nil.
- `internal/cli/livecheck_test.go`: `TestIngestRebuildProceedsWhenServeIsDown` — `withHome`, `--rebuild`, `--dashboard-addr` on a port nothing listens on. Error is nil, writer does not contain `WARN`, and the `jsonl:` summary from `ingest.go:65-66` is still printed.
- `internal/cli/livecheck_test.go`: `TestIngestWithoutRebuildDoesNotWarnWhenServeIsLive` — live fake listener, `runIngest` without `--rebuild`. Writer does not contain `WARN`.
- `internal/cli/livecheck_test.go`: `TestRepriceYesWarnsWhenServeIsLive`, `TestReflagYesWarnsWhenServeIsLive`, `TestPurgeYesWarnsWhenServeIsLive` — `withHome`, live listener on `127.0.0.1:<port>`, `--yes`, and for purge also `--unpriced` or `--older-than`. Writer contains `WARN`, error is nil.
- `internal/cli/livecheck_test.go`: `TestRepriceDryRunDoesNotWarnWhenServeIsLive`, `TestReflagDryRunDoesNotWarnWhenServeIsLive`, `TestPurgeDryRunDoesNotWarnWhenServeIsLive` — same live listener, `--dry-run` (purge also needs `--unpriced` or `--older-than`). Writer does not contain `WARN`. `ingest --rebuild` is not part of this no-WARN dry-run assertion.
- **Manual / recorded**: the no-listener negative control in Outcome Definition. No evidence file.

## Files to Touch

- `internal/cli/livecheck.go` (create — `probeServeHealth`)
- `internal/cli/livecheck_test.go` (create)
- `internal/cli/ingest.go` (modify — probe on `--rebuild` only, before `resetJSONLCursors`)
- `internal/cli/reprice.go` (modify — probe after `openStore` when `!dryRun`; do not change the `RepriceCosts` arguments)
- `internal/cli/reflag.go` (modify — `_, st` to `cfg, st`, then the same `!dryRun` probe)
- `internal/cli/purge.go` (modify — `_, st` to `cfg, st`, then the same `!dryRun` probe)

## Review Notes

Added `probeServeHealth` and `warnIfServeLive` in `internal/cli/livecheck.go`. The probe dials `dialableDashboardAddr` and treats HTTP 200 from `GET /api/health` as live, with the caller's 2s timeout. `warnIfServeLive` prints one `WARN` line (the doctor `statusWarn` token) naming `/api/health` contention and `SQLITE_BUSY`, then returns. `ingest --rebuild` calls it before `resetJSONLCursors`. `reprice`, `reflag`, and `purge` call it only when `!dryRun`, after `openStore`. `reflag` and `purge` now keep `cfg` from `openStore`. `RepriceCosts` arguments are unchanged.

`go test ./internal/cli/ -count=1 -run 'TestIngestRebuildWarnsWhenServeIsLive|TestIngestRebuildProceedsWhenServeIsDown|TestIngestWithoutRebuildDoesNotWarnWhenServeIsLive|TestRepriceYesWarnsWhenServeIsLive|TestReflagYesWarnsWhenServeIsLive|TestPurgeYesWarnsWhenServeIsLive|TestRepriceDryRunDoesNotWarnWhenServeIsLive|TestReflagDryRunDoesNotWarnWhenServeIsLive|TestPurgeDryRunDoesNotWarnWhenServeIsLive'` passed. Before the probe existed, the four `--yes` / `--rebuild` tests failed on a missing `WARN`, and the wildcard ingest recorded Host `""`.

Negative control: `TestIngestRebuildProceedsWhenServeIsDown` runs `--rebuild` against a released loopback port and asserts the writer has no `WARN` while the `jsonl:` summary is still printed. That assertion passed.

`go test ./internal/cli/ -count=1 -run 'TestRestart|TestShutdown|TestDoctor'` passed (20 tests) on a retry. An earlier run under load failed `TestRestartRunningReusesRecordedExeAndArgs` and, separately, `TestRestartUnhealthyNewExeRollsBackFromRetainedArgs`, both on the pre-existing drain wait (`restart.go` was not edited). `restart.go` and `doctor.go` are untouched.
