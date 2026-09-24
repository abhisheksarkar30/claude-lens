# Bead br-GI-16-04: `consumer.SetAccounts`, `POST /api/reload`, `clens reload`, live accounts route

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream B, §B.4, §B.5 (and §B.2 for why `bootArgs` matters).

- **Bead ID**: br-GI-16-04
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-16-02 (sequencing only — both edit `serve.go`'s startup, so 02 lands first; **reload reads nothing from 02's state file**)
- **Blocks**: br-GI-16-09 (adds the `HotDays` arm), br-GI-16-11, br-GI-16-12

## Description

A small live `clens reload` for the cheap subset that can change without rebinding a listener. Anything bound at
boot (addresses, upstream URL, DB path, body policy/cap, pprof) needs a restart; reload says so.

1. **`consumer.SetAccounts`** (`internal/consumer/consumer.go`): `consumer.New` copies the account list today
   (`:55,81`) and exposes no setter. Add a mutex-guarded `SetAccounts([]config.Account)` and make `resolveAccount`
   (`:518`) read under the same lock.
2. **`POST /api/reload`** in `internal/api/reload.go`, same guard as `shutdown`: `originReject` **and** a loopback
   caller. It is a *second route under the existing loopback-caller guard* — the guard mechanisms stay three. The
   handler calls an injected `reloadFunc` (wired in `serve.go`, registered in `api.go`). Response:
   `{"applied":[…],"restart_required":[…],"unchanged":true|false}`; an invalid file changes nothing and returns
   **400** with the validation error — **reload is all-or-nothing**.
3. **`reloadFunc`** in `serve.go`:
   1. `config.Load(bootArgs)` again (flag > env > `config.toml` > default), then `Validate()`. `bootArgs` is the
      **post-subcommand** slice `serve` received (`os.Args[2:]`), **not** the state file's `args`: `config.Load` →
      `applyFlags` is a plain `flag.Parse` that **stops at the first non-flag argument**, so handed
      `["serve","--replay"]` it would drop every flag and reload could conclude `RetentionDays` changed `30 → 0`.
   2. Diff field-by-field against the boot `cfg`.
   3. **Live-apply set is exactly `Accounts`, `RetentionDays`, `HotDays` — no others.** This bead ships the
      mechanism plus the `Accounts` and `RetentionDays` arms. **`HotDays` does not exist until bead 05; its arm is
      bead 09's.** Do not reference `HotDays` here.
   4. `RetentionDays` moves into one small `atomic`-guarded struct the purge ticker reads (bead 09 adds
      `HotDays` to the same struct). Apply the fields under one lock.
   5. Everything else that differs is reported under `restart_required`. Prices already hot-reload
      (`newPriceLoader`) and are unaffected.
4. **`clens reload`** (`internal/cli/reload.go`): dials `POST /api/reload` on the dialable dashboard address
   (reuse `runShutdown`'s round-trip pattern), prints the report; exits non-zero on 400 with the message.
5. **Accounts route**: `POST /api/accounts` currently *validates only* and tells the user to restart
   (`internal/api/accounts.go:87`, `serve.go:391`, both marked `ponytail:`). Make it call the same apply path and
   change the message; this removes those two `ponytail:` ceilings. This is the only pre-existing *route* B edits.
6. Register `reload` in `cmd/clens/main.go` and add `reload` to `carriedOver` in `cmd/clens/main_test.go`
   (the hand-written list; 23 today, this takes it to 25 with bead 03's `restart`; bead 09 adds `archive` → 26).

**Safety**: tests use ephemeral ports and a temp DB; never call `/api/reload` or `/api/shutdown` on `127.0.0.1:8797`.

## Rationale

Accounts and retention currently require a restart, which drops the live session's proxy. Reload removes that
cost for the changes that can safely apply live and honestly reports the rest.

## Outcome Definition

- Editing accounts/retention in `config.toml` then `clens reload` applies them with no restart; the report lists
  `applied` / `restart_required` accurately.
- An invalid file changes nothing (all-or-nothing) and returns 400.
- Non-loopback caller and bad Origin get 403 exactly as `shutdown`.
- `SetAccounts` is race-clean under `go test -race`.
- `go build ./... && go vet ./... && go test ./internal/consumer/ ./internal/api/ ./internal/cli/ ./cmd/clens/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests:
  - `internal/consumer`: `SetAccounts` swap concurrent with `resolveAccount` under `-race`; new accounts take effect for the next request.
  - `internal/api/reload_test.go`: guard tests (bad Origin, non-loopback caller, malformed body) copied from `shutdown`'s; 400 on invalid config; response shape.
  - `internal/cli/reload_test.go`: diff classification — a changed `Accounts`/`RetentionDays` is `applied`; a changed `ProxyAddr` is `restart_required`; identical is `unchanged:true`.
  - All-or-nothing: a file with one valid change and one invalid value applies neither.
  - `bootArgs` regression: with boot args `--retention-days 30`, an unchanged file does not diff `RetentionDays` to 0.
  - Accounts route no longer says "restart" and applies via `SetAccounts`.
- Integration Tests:
  - In-process serve on ephemeral ports: rewrite temp `config.toml`, run `runReload`, assert the consumer sees the new accounts.
  - `cmd/clens/main_test.go`: `reload` in `carriedOver`; dispatch test passes.

## Files to Touch

- `internal/consumer/consumer.go` (modify — `SetAccounts`, lock in `resolveAccount`)
- `internal/consumer/consumer_test.go` (modify)
- `internal/api/reload.go` (create)
- `internal/api/reload_test.go` (create)
- `internal/api/api.go` (modify — register the route / `reloadFunc` hook)
- `internal/api/accounts.go` (modify — apply live, new message)
- `internal/cli/reload.go` (create)
- `internal/cli/reload_test.go` (create)
- `internal/cli/serve.go` (modify — wire `reloadFunc`, keep `bootArgs`, atomic settings struct)
- `cmd/clens/main.go` (modify — register `reload`)
- `cmd/clens/main_test.go` (modify — add `reload` to `carriedOver`)
