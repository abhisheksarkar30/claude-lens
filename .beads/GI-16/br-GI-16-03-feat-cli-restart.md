# Bead br-GI-16-03: `clens restart` with detached spawn, health wait, measured gap and rollback

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream B, §B.1, §B.3, §B.5.

- **Bead ID**: br-GI-16-03
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-16-02 (reads `serve.state.json` via `readServeState`/`statePath`)
- **Blocks**: br-GI-16-11 (docs), br-GI-16-12 (integration)

## Description

New subcommand `clens restart [--exe PATH] [--timeout 30s]` in `internal/cli/restart.go`
(`runRestart`). Exists because on Windows a running `clens.exe` cannot be replaced, and `shutdown` alone leaves
the live Claude session with no proxy.

Steps:

1. **Strip `restart`'s own flags first** — `--exe` and `--timeout` via `takeFlag`
   (`internal/cli/accounts.go:101`), exactly as `purge` does (`purge.go:31-35`) — **then** `config.Load(rest)`.
   The order is load-bearing: `config.Load` → `applyFlags` uses a closed `flag.FlagSet` (`config.go:238-252`) with
   no `--exe`/`--timeout`; an unknown flag is a returned error. Read the state file (missing ⇒ no exe/args to
   reuse; then `--exe` is required, or `os.Executable()` is used with `serve` and forwarded flags). **Retain the
   exe and args read** for step 6 — the state file is deleted on the graceful exit step 3 drives.
2. **Running?** `GET /api/health` on the dialable dashboard address (reuse `dialableDashboardAddr`). Not running
   ⇒ skip to step 4 and print "was not running". Liveness is decided by health, never by the file or pid.
3. `POST /api/shutdown` (**reuse `runShutdown`'s round trip; do not duplicate it**), then wait until **both** the
   dashboard and proxy ports refuse a dial, bounded by `--timeout`. This proves the drain finished and the store
   is closed.
4. Spawn `exe <args…>` **detached**. The recorded `args` already begin with `serve`; **never prepend a second
   `serve`** (`clens.exe serve serve …` is what `main.go:63` would read as its subcommand). stdout+stderr appended
   to `log_path`.
   - Windows: `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS` in `detach_windows.go`; Unix: `Setsid` in
     `detach_unix.go`. Two small build-tagged files.
5. Poll `/api/health` until 200 or `--timeout`. Report pid, exe, log path and **the measured gap** (shutdown
   request → healthy).
6. **Rollback.** If `--exe` was given and the new process does not become healthy: kill the child, respawn the
   *previous* exe **from the exe/args step 1 retained**, wait for health, print the log tail, and exit
   **non-zero** naming which binary is now serving. If no previous exe is known, say so.

Register the command in `cmd/clens/main.go`'s `commands` map and add `restart` to `carriedOver` in
`cmd/clens/main_test.go` (`TestEveryCarriedOverCommandIsDispatched` fails on `len(commands) != len(carriedOver)`;
the list is hand-written, 23 today; this bead takes it to 24, bead 04 to 25, bead 09 to 26).

**Safety: every test uses ephemeral ports and a temp DB. No test or verification step may run `restart` or
`shutdown` against `127.0.0.1:8797`** — that kills the operator's live Claude session.

## Rationale

Makes the proxy gap short, measured and self-healing; without rollback a bad `--exe` build leaves the session
with no proxy at all.

## Outcome Definition

- `clens restart` against a running in-process fake serve: shutdown → ports free → respawn → healthy; prints pid,
  exe, log path, gap.
- Not running ⇒ starts it and says "was not running".
- `--exe` bad build ⇒ rollback to the retained previous exe, non-zero exit naming the serving binary.
- The spawned child survives its parent's exit.
- `GOOS=linux go vet ./...` and Windows `go vet ./...` both pass (build-tagged files).
- `go build ./... && go test ./internal/cli/ ./cmd/clens/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests (`internal/cli/restart_test.go`, table-driven `runRestart` against an in-process fake server on
  ephemeral ports):
  - running / not running / new exe healthy / new exe unhealthy → rollback / rollback impossible (no previous exe).
  - `--exe` and `--timeout` do not reach `config.Load` (would error on the unknown flag).
  - Spawn argv equals `exe` + recorded `args` with exactly one `serve`.
  - Rollback uses the retained args, not a re-read of the (deleted) state file.
  - Timeout waiting for ports to close is a clear error.
- Integration Tests:
  - Detached child survives parent exit, using a helper-process pattern via `TestMain`.
  - `cmd/clens/main_test.go` `carriedOver` includes `restart` and the dispatch test passes.

## Files to Touch

- `internal/cli/restart.go` (create)
- `internal/cli/restart_test.go` (create)
- `internal/cli/detach_windows.go` (create, build-tagged)
- `internal/cli/detach_unix.go` (create, build-tagged)
- `cmd/clens/main.go` (modify — register `restart`)
- `cmd/clens/main_test.go` (modify — add `restart` to `carriedOver`; shared with beads 04 and 09)
