# Bead br-GI-16-02: `serve` writes and removes `serve.state.json`, with a default `log_path`

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream B, §B.2.

- **Bead ID**: br-GI-16-02
- **Priority**: P1 (high — `restart` cannot relaunch "the same thing" without it)
- **Original Estimate**: 1.5h
- **Dependencies**: None
- **Blocks**: br-GI-16-03 (reads the file), br-GI-16-04 (sequencing only: both edit `serve.go`'s startup)

## Description

`serve` writes `<dir of cfg.DBPath>/serve.state.json` once **both** listeners are up:

```json
{"pid": 4242, "exe": "D:/clens/clens.exe", "args": ["serve", "--replay"],
 "started_at": "2026-09-24T06:22:00Z", "proxy_addr": "127.0.0.1:8797",
 "dashboard_addr": "0.0.0.0:8798", "log_path": "D:/clens/serve.log"}
```

- `exe` = `os.Executable()`; `args` = `os.Args[1:]` (so it **already begins with the subcommand** `serve`).
- `log_path` default = `<dir of DBPath>/serve.log` (the file already next to the live DB). The outline calls
  this the "`--log` default": implement it as the default value recorded in the state file; do **not** add a new
  flag or config key unless the plan is amended (see summary — the plan text only specifies the default path).
- Removed on graceful exit (the same `stop` path `POST /api/shutdown` drives). **A leftover file is a hint, never
  the truth**: nothing in this bead decides liveness from it.
- Contains no secret (paths and addresses only). Written `0600` (a no-op on Windows; same directory as the DB).
- Put the read/write helpers in a small unexported type in `internal/cli` (e.g. `serveState` with
  `writeServeState`, `readServeState`, `removeServeState`) so bead 03 reuses `readServeState`. Write atomically
  (temp file + rename) so a concurrent `restart` never reads a torn file.
- Provide a `statePath(dbPath string) string` helper; bead 03 and 04 use it.
- Record the actual bound addresses (use the listener's `Addr()`, not the configured string, so `:0` ephemeral
  ports in tests are correct).

## Rationale

`restart` is a *separate process invocation*; flags are not in config, so the only way it can relaunch what is
running is a record written by the running `serve`. Without it `restart` would restart with the wrong flags.

## Outcome Definition

- After `serve` is healthy, the state file exists next to the DB with all seven keys, `args[0] == "serve"`.
- After graceful shutdown the file is gone. After a failed listener bind the file is never written.
- A stale file from a killed process is overwritten by the next `serve`.
- `go build ./... && go vet ./... && go test ./internal/cli/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests (`internal/cli/serve_state_test.go`):
  - Round trip write/read preserves every field, including `args` beginning with `serve`.
  - `readServeState` on a missing file returns a "not present" result, not a fatal error; on garbage JSON, an error.
  - `removeServeState` on a missing file is a no-op.
  - Written file is atomic (no partial file visible: write goes through rename).
- Integration Tests:
  - Start `serve` in-process on ephemeral ports with a temp DB (never `127.0.0.1:8797`); assert the file appears
    with the real bound addresses, then trigger graceful stop and assert removal.

## Files to Touch

- `internal/cli/serve.go` (modify — write after both listeners up; remove on graceful exit)
- `internal/cli/serve_state.go` (create)
- `internal/cli/serve_state_test.go` (create)
