# Bead br-GI-16-10: Dashboard and `show` archived-state display (optional `GET /api/archive`)

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream C, §C.5 (wire keys), §C.9.

- **Bead ID**: br-GI-16-10
- **Priority**: P2 (medium)
- **Original Estimate**: 1.5h
- **Dependencies**: br-GI-16-06 (`Event.BodiesArchived` and hydration). Also edits `internal/web/app.js`/`assets_test.go`, which br-GI-16-01 edits: land after 01 (sequencing only).
- **Blocks**: None

## Description

Show the archived state instead of "not captured".

1. **Dashboard call detail** (`internal/web/app.js`): when `BodiesArchived != ""`, show a line —
   `restored` ⇒ *"bodies loaded from the archive (2026-09-20)"* (the day is derived from the row's
   `started_at` in UTC, or omitted if unavailable); `missing` ⇒ *"archived — archive file for 2026-09-20 not
   found"*. `missing` must never render as "not captured". `/api/requests/{id}` already returns it because
   `eventDetail` embeds `*store.Event` (`api.go:394-395`): **no `internal/api/api.go` edit**. Existing body
   markers (`captureMarker`, `readPathMarker`) must keep working on restored bodies (they compare lengths against
   the cap; the restored blob is byte-identical) — assert nothing there regresses.
2. **`internal/cli/show.go`**: same two messages in `clens show` output.
3. **Wire contract note**: `ls --json` (`ls.go:57`) and `export` (`export.go:90`) encode whole `*store.Event`
   values, so the one new key **`BodiesArchived`** appears in their JSON (`"BodiesArchived":"restored"`).
   `ArchivedBodyMask` is `json:"-"` and must not appear. This is an additive machine-readable contract change:
   add a test that pins both facts, and mention it in the code comment.
4. **`GET /api/archive`** (`internal/api/archive.go`): add it **only if cheap** (a thin wrapper over the same
   status numbers `clens archive status` computes; **read-only**, no new write guard). If it is not cheap
   (needs the archiver's status queries from bead 09, which this bead does not depend on), **do not create the
   file** and record "dropped: `clens archive status` is the surface" in the commit body (plan: YAGNI). A
   Settings-tab summary is built only if the route exists.

## Rationale

Without visible state an operator who moved `archive/` is told a call had no body, and a scripted consumer of
`export` needs to know the additive key exists.

## Outcome Definition

- Detail page and `show` display both messages; neither says "not captured" for an archived/missing row.
- `ls --json`/`export` output contains `BodiesArchived` and never `ArchivedBodyMask`.
- `internal/api/api.go` is unchanged; `internal/api/archive.go` exists only if the route was judged cheap.
- `go build ./... && go vet ./... && go test ./internal/web/ ./internal/cli/ ./internal/api/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests:
  - `assets_test.go` source-shape guards: `app.js` references `BodiesArchived`, contains the `restored` and `missing` messages, and the `missing` message text differs from the "not captured" text.
  - `show_test.go`: an event with `BodiesArchived="restored"` and `"missing"` prints the two messages; `""` prints neither.
  - `ls_test.go`/`export_test.go`: encoded JSON contains `"BodiesArchived":"restored"` and does not contain `ArchivedBodyMask`.
  - If the API route is added: `internal/api/archive_test.go` — read-only, GET only, 405 on POST.
- Integration Tests: call-detail via the API on a synthetic archived store returns `BodiesArchived == "restored"` with bodies present.

## Files to Touch

- `internal/web/app.js` (modify)
- `internal/web/assets_test.go` (modify)
- `internal/cli/show.go` (modify)
- `internal/cli/show_test.go`, `internal/cli/ls_test.go`, `internal/cli/export_test.go` (modify)
- `internal/api/archive.go` (create **only if** cheap; else not created) and `internal/api/archive_test.go` (same condition)
