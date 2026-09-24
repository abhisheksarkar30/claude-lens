# Bead br-GI-16-11: README operator runbooks — restart, reload, archive, one-time vacuum, auto-mode notice

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Files table "Docs" row; §B.3, §B.4, §C.4 (no-VACUUM), §C.8, §D.3.

- **Bead ID**: br-GI-16-11
- **Priority**: P3 (low — docs only)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-16-03, br-GI-16-04, br-GI-16-09 (so runbooks describe shipped behaviour)
- **Blocks**: br-GI-16-12 (optional; docs do not gate integration)

## Description

Edit `README.md` only. **Do not edit `docs/context/**`** — it is generated and only the context-refresh phase
writes there; do not edit the plan. Add runbooks:

1. **Restart**: `clens restart [--exe PATH] [--timeout 30s]`; the Windows locked-binary answer
   (`clens restart --exe D:\build\clens.exe`, build elsewhere while the old one runs); what the state file
   (`serve.state.json`, `serve.log` next to the DB) is; the measured gap; rollback behaviour and non-zero exit; that
   replacing the installed binary stays the operator's call; **warning not to test against the live session.**
2. **Reload**: what applies live (exactly `Accounts`, `RetentionDays`, `HotDays`), what needs a restart (addresses,
   upstream URL, DB path, body policy/cap, pprof); all-or-nothing on a bad file; the loopback-only guard.
3. **Archive**: what is archived (bodies only; rows and aggregates stay in `lens.db`), `--hot-days`/
   `CLENS_HOT_DAYS`/`HotDays` (default 7, `0` disables, `HotDays > RetentionDays` rejected), per-UTC-day files
   under `archive/`, `clens archive status|run|restore` (`--yes` gates), that `--retention-days` deletes archived
   bodies too, that archive files are as sensitive as `lens.db` (same directory, **no better protection on
   Windows**), first real archival expected 2026-09-27, and the C.4 corollary (a body arriving post-archival stays hot).
4. **One-time vacuum**: the hot file does not shrink on its own; `clens shutdown`, `clens purge --vacuum --yes`,
   `clens restart` — serve never `VACUUM`s (would stall capture).
5. **Migration backup**: before first running the new binary against the live store, copy `lens.db`, `lens.db-wal`,
   `lens.db-shm` to a separate path (schema 4 → 5).
6. **Auto-mode notice note** (§D.3): the notice naming `127.0.0.1:8797` misattributes — clens's upstream is
   DeepSeek's Anthropic-compatible surface, so Anthropic's server-side checks can never run there; opt out with
   `CLAUDE_CODE_AUTO_MODE_SERVER=0` or route to `api.anthropic.com` (mutually exclusive with DeepSeek models);
   clens is byte-transparent and that is pinned by a test.

Verify each command's flags against the shipped code (`go run ./cmd/clens <cmd> --help` where available) rather
than the plan.

## Rationale

`docs/context/**` is generated, so operator runbooks live in `README.md`. Restart/archive have failure modes
(locked exe, no-shrink, sensitive archive dir) an operator must know before running them.

## Outcome Definition

- README contains all six sections, and every flag/name it mentions exists in the code.
- No file under `docs/` or `internal/` changed.
- `go build ./...` still passes (sanity).

## Test Specifications

- Unit Tests: none (docs). Verification by grep: each documented flag (`--exe`, `--timeout`, `--hot-days`, `--yes`, `--dry-run`, `--since`, `--until`, `--vacuum`) is present in the corresponding source file.
- Integration Tests: none.

## Files to Touch

- `README.md` (modify)
