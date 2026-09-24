# Bead br-GI-16-01: Calls tab `custom` from/to range, with the flipped source-shape guards

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream A (A.1-A.4), issue #16.

- **Bead ID**: br-GI-16-01
- **Priority**: P1 (high — the anchor issue of the story)
- **Original Estimate**: 1.5h
- **Dependencies**: None
- **Blocks**: None (bead 10 also edits `app.js`/`assets_test.go`; land 01 first to avoid a merge conflict — sequencing only, see summary)

## Description

The Calls tab's window select (`c-window-gran`) has no `custom` option. Add one. The backend already accepts
`since`/`until` (`listRequests`, `internal/api/api.go:357-373`, `parseTimeBoundParam`), so **no backend or API
change**. If implementation finds one is needed, stop and re-plan.

Behaviour (both inputs are *hour* selections, inclusive of the hour picked):
`since = timeWindow('hour', from).since`, `until = timeWindow('hour', to).until`.

| from | to | Filter |
|---|---|---|
| set | set | `[from-hour start, to-hour end)`; if `to` < `from`, show an inline message and apply **no** window |
| set | empty | `since` only |
| empty | set | `until` only |
| empty | empty | no window (same as "any time") |

Edits:

1. `internal/web/index.html`: add `<option value="custom">custom</option>` to `c-window-gran`; add
   `<label>from <input id="c-from" type="datetime-local"></label>` and the `to` twin (`c-to`). **Do not
   hard-code `hidden` on the inputs** — a `hidden` on the input itself is permanent (un-hiding the parent label
   never clears a child's own `hidden`); `mountWindowPicker` toggles the **labels**. The Stats pair
   (`index.html:115-116`) is the model: labels carry no `hidden` in HTML and are hidden at mount. Rewrite the
   comment above the select that says there is deliberately no `custom`.
2. `internal/web/app.js`: `mountWindowPicker($('c-window-gran'), …, [c-from label, c-to label], …)` — the
   `freeLabels` parameter already exists and already hides/reveals on `custom` (see `app.js:352`).
   `callFilter()` gains a `custom` branch calling a new `customWindow(fromVal, toVal)` helper that calls
   `timeWindow` twice. `change` listeners on the two inputs reload. Offset resets to 0 on any window change
   (already true for the picker — verify for the new inputs). **No `toISOString()`, no second offset
   implementation**: `timeWindow()` is the one place the `±hh:mm` offset is built by hand.
3. `internal/web/assets_test.go`, `TestAssetsThePickerMountsBothTabs`: invert the Calls-has-no-`custom`
   assertion at `:654-656`; **rewrite the two prose blocks that assert the opposite** — the header comment "a
   `custom` option on Calls would be a control offering a capability that tab does not have" (`:625-630`) and
   the defaults comment calling Calls' omission of `custom` "the dead-option misdescription the Calls mount
   already avoids" (`:661-671`); leave the default checks at `:672-677` unchanged (no `selected`; an
   empty-valued option present — the default remains `any time`). Add: `c-from`/`c-to` mounted as
   `datetime-local`; `callFilter` reaches `customWindow`; `customWindow` calls `timeWindow(` and contains no
   `toISOString`.

## Rationale

Issue #16: Calls can only filter by preset windows. Without this the operator cannot inspect an arbitrary
interval, although the API already supports it. Leaving the old test assertions would fail `go test ./...`.

## Outcome Definition

- `custom` on Calls reveals from/to `datetime-local` inputs with hour precision; default stays `any time`.
- Cross-check (issue AC): the `since`/`until` strings `customWindow` produces equal what typing the same
  instants into Stats' `s-since`/`s-until` sends (same `EventFilter` store-side, so equal query string = equal rows).
- No file under `internal/api`, `internal/store` changed.
- `go build ./... && go vet ./... && go test ./internal/web/` passes, then `go test ./...`.
- Manual (stated, not automated; no JS runtime in the toolchain): browser pass incl. a DST-adjacent range and `to < from`.

## Test Specifications

- Unit Tests (`internal/web/assets_test.go`, source-shape guards):
  - `c-window-gran` has a `custom` option; `c-from`/`c-to` are `datetime-local` and carry no `hidden` attribute.
  - `mountWindowPicker` for Calls receives the two labels as `freeLabels`.
  - `callFilter` source contains a `custom` branch that reaches `customWindow`.
  - `customWindow` body contains `timeWindow(` twice and no `toISOString`.
  - Default checks (no `selected`, empty-valued option) still pass unchanged.
- Integration Tests: none (manual browser pass above).

## Files to Touch

- `internal/web/index.html` (modify)
- `internal/web/app.js` (modify)
- `internal/web/assets_test.go` (modify)
