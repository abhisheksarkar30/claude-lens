# Bead br-GI-11-08: the time-window picker on Calls and Stats — one `timeWindow()`, two mounts

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4's "The time-window picker on
Calls and Stats" block (the change table and every decision paragraph) and the "Why a reprice command is
in scope" context, §5's `Picker` bullet, §6 (the window-drift, DST and "part to cut" risk rows), §7

- **Bead ID**: br-GI-11-08
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: None (front-end only — no server, store, or API change)
- **Blocks**: br-GI-11-11 (the `dashboard.md` / `INDEX.md` line-count and route-table edits describe
  this picker)

> **This fix can rebuild the C-1 defect inside itself, and the likeliest implementation does.**
> `new Date('2026-09-21').toISOString()` emits `2026-09-21T00:00:00.000Z`, and Go **accepts** it,
> reading it as **UTC** (`api.go:301` is `time.Parse(time.RFC3339, s)`; the `Z07:00` layout matches a
> literal `Z` and the parser accepts a fractional-second field it does not itself print). Measured:
> that Z form is unix **1789948800** against the intended **1789929000** — **19,800 s = 5h30m off,
> silently**. Build the `±hh:mm` suffix from the local offset (e.g. `-date.getTimezoneOffset()`) and
> **never** call `toISOString()`.

> **The control's raw value is never a valid query param.** `<input type="date">` yields bare
> `YYYY-MM-DD` and `<input type="month">` yields `YYYY-MM` — both are **rejected** by
> `time.Parse(time.RFC3339, …)` (`cannot parse "" as "T"` / `as "-"`). So `timeWindow()` must expand the
> picked value into a full instant **carrying the local offset** before it reaches the query string.

## Description

**A scope addition, requested mid-story, front-end only.** The report that opened GI-11 was a
comparison against a *day* — DeepSeek's page for 21 Sept — and the plan establishes that no shipped
surface can express that day: `clens stats --by day` buckets by **UTC** (`store.go:1252`) while the
dashboard renders **local** time (`app.js:31-36`), and the Calls tab has no time filter at all. The
picker makes the local day expressible.

### The control — one implementation, two mounts

A granularity `<select>` and the native input it reveals:

| Granularity | Input | Window (half-open) |
|---|---|---|
| `hour` | `<input type="datetime-local">` | `[H:00, H+1:00)` |
| `date` | `<input type="date">` | `[D 00:00, D+1 00:00)` |
| `month` | `<input type="month">` | `[M-01 00:00, M+1-01 00:00)` |
| `custom` | the retained free-text `s-since`/`s-until` pair — **Stats mount only; omitted on Calls** | whatever the operator types |

**The `<option>` set is built per mount, and Calls does not offer `custom`.** "Reveals nothing" is a
description of the gap, not a resolution of it — a selectable entry that filters nothing is a dead
control, so the Calls `<select>` is populated with `hour | date | month` only and the Stats `<select>`
with all four. **No dead option on either mount.** (The alternative — disabling the option on Calls —
was rejected: a greyed entry still advertises a capability the tab does not have.)

**Mount points.** On Calls (`index.html:54-63`) add **two new ids**, `c-window-gran` (the `<select>`)
and `c-window-value` (the input). On Stats (`:89-98`) add `s-window-gran`/`s-window-value` **beside** the
existing row and **keep** the free-text `s-since`/`s-until` mounted — hidden unless the granularity is
`custom`. The Calls row has no free-text pair at all, which is why `custom` is Stats-only.

**The ids must move with the code.** `TestAssetsEveryLookupHasAMount` fails the build on a `$('…')`
lookup with no mount, so removing `s-since`/`s-until` while `loadStats` still reads them is a **build
failure** — the guard is what turns a typo'd id into a failure instead of a silent no-op.

### Two axes, not one — the picker is the window, `s-granularity` is the bucket

Stats already carries `<select id="s-granularity">` of `day | week | month` (`index.html:92-96`), sent
as `?granularity=` (`app.js:499`) and parsed by `parseGranularityParam` (`api.go:329-338`). That is the
**bucket** axis: unchanged, and `s-apply` stays its trigger. The picker supplies only `since`/`until`;
its own `<select>` must **never** be wired to `granularity=` — `parseGranularityParam` accepts only
`day|week|month`, so `hour`/`date`/`custom` sent as a granularity would 400 on the spot. The two compose,
with one output-shape consequence: a bounded window (say one hour) bucketed by `day` collapses the
"By period" table and the chart to a **single** bucket — correct, but surprising.

### Wire format and local time — the C-1 fix

RFC3339 **carrying the local offset**, never a duration. `parseTimeBoundParam` accepts both
(`api.go:293-305`), but a duration is resolved against `time.Now()` *at request time* (`:298-300`), and
`24h` cannot express "the local day of 21 September". The window is computed in the browser's zone and
sent with its offset, so the range the operator picked is the range the server filters on. The `date`
granularity therefore means the **local** day, and the `started_at` range it produces for 21 Sept is the
same half-open IST range §5's acceptance is written against. **One window, computed in one place**
(`timeWindow()`) — otherwise the UI and the acceptance query disagree and the plan has rebuilt the bug
it is fixing.

### Native inputs, no library

`type="date"`, `type="month"` and `type="datetime-local"` are platform features. The dashboard is
hand-written vanilla JS with no build step and no dependencies — the same reasoning that made
`chartByPeriod` an inline SVG rather than a charting library (`app.js:538-542`). A calendar library is
exactly that, so there is not one.

### Two behaviours that must not be lost

- A picker change resets `callState.offset = 0` **before** reloading, exactly as `f-apply` does
  (`app.js:928`) — page 4 of the old window is not page 4 of the new one.
- `custom` keeps free text working on Stats, so nothing that works today stops working; replacing the
  text inputs outright would silently narrow `24h` and arbitrary RFC3339 ranges out of existence.

### What this bead does not do

- **No server, store, or API change.** `/api/requests` parses `since`/`until` today
  (`api.go:351-367`) and plumbs them into `EventFilter.Since/Until` (`types.go:168-169`); the Calls tab
  simply never set them (`app.js:275-286`). This is front-end-only, which is why it belongs here.
- **No new dependency, no build step.**
- **No executed JS tests.** There is no JS runtime in this toolchain and no JS engine in the module
  graph (`assets_test.go:409-416`; `go.mod`), so the Go tests assert over the embedded bytes only and
  the behavioural cases are **manual verification** (below).
- **No `style.css` change unless the new controls need it** — the existing `.row`/`label` classes
  already carry the filter rows.

## Rationale

The one window §5's entire acceptance section is written against is the one window the UI cannot ask
for. The picker makes it expressible and makes the reprice's effect visible on the surface the operator
was reading when the numbers did not match. It is front-end-only, which is what keeps it off the three
fixes' critical path; §6 records it as the part to cut if the story must shrink, since no acceptance
step depends on it.

## Outcome Definition

- Calls has `c-window-gran` + `c-window-value`; Stats has `s-window-gran` + `s-window-value` plus the
  retained `s-since`/`s-until`, hidden unless the granularity is `custom`.
- The Calls `<select>` offers `hour | date | month`; the Stats `<select>` offers all four. No dead
  option on either mount.
  - **Corrected during implementation (ratified in Phase 5.5's review round 1):** the Calls mount
    also carries a leading `<option value="">any time</option>`, so the mounts offer **four and
    four** — the difference between them is the *fourth* value (`any time` on Calls, `custom` on
    Stats), not the number of options. The literal three-option reading is not implementable: a
    native `<select>` has no unset state, so `hour | date | month` alone would default-select `hour`
    while `timeWindow()` returns `null` (the value input is empty, and `if (!gran || !value) return
    null` makes that the same no-window path) — a control advertising a granularity it is not
    applying, which is the dead-option misdescription the clause above exists to prevent. `any time`
    is a neutral default, not a dead option; `custom` remains Calls-excluded.
  - This is recorded here and **not** in the converged plan: plan §4 carries the same three-option
    phrasing, and re-asserting `status=converged` on text no review round read is precisely the
    dishonesty round 9's finding O4 names. The divergence is therefore an open item for the plan's
    next revision, stated here so it is not silently lost.
  - `TestAssetsThePickerMountsBothTabs` now pins both defaults — Calls carries no `selected` and an
    empty-valued first option, Stats' `custom` carries `selected` — so the ratified decision is
    enforced rather than only documented (mutation-checked: adding `selected` to Calls' `any time`
    fails the test).
- `timeWindow()` is a top-level `function timeWindow(...)` (not a `const … => {…}`), taking granularity
  and value and returning `{since, until}` as RFC3339 strings **carrying the local offset**.
- `callFilter()` and `loadStats()` both route their `since`/`until` through `timeWindow()`; a change
  handler mirroring `f-apply` resets `callState.offset = 0` before reloading.
- The picker's `<select>` is never wired to `granularity=`.
- `TestAssetsEveryLookupHasAMount` green (every new `$('…')` id has a mount);
  `go test ./internal/web/` passes.
- **Manual verification (the PR's test plan), not in-repo tests:** the emitted `since`/`until` denote
  the intended **local** instant — they carry the local offset, do not end in `Z`, and denote the picked
  wall-clock in the browser's zone, checked by eye against the emitted query string. "It round-trips
  through `parseTimeBoundParam` without a 400" is **not** the check: the `Z` form parses cleanly and
  *is* the defect, so a parse-only test is green on the bug. Also: Dec→Jan month rollover; the month
  width **not** being a constant (28–31 days, so a `+30d` shortcut is wrong); and **DST**, where a local
  `date` window is built with `new Date(y, m, d)` → `new Date(y, m, d + 1)`, never
  `start + 86_400_000`, because a DST day is 23 or 25 hours long.

## Test Specifications

All in `internal/web/assets_test.go`. **Name tests by their Go function name, never by a plan line
range.** There is **no executed `timeWindow()` case** — a Go test only asserts over the embedded bytes.

- **Unit Tests:**
  - `TestAssetsEveryLookupHasAMount` (**keep, extends automatically**) — covers every new picker
    `$('…')` id: it fails on a lookup with no mount, so a typo'd `c-window-*`/`s-window-*` id is a build
    failure. No edit needed beyond the ids, but verify it still passes.
  - **The `timeWindow`-body guard** (the F3.1/F3.2 source-shape guard; the plan does not name it, so
    this bead proposes `TestAssetsTimeWindowBuildsTheOffsetByHand`) — slices the function with
    `funcBody(js, "timeWindow")` (`assets_test.go:358-377`) and, **scoped to that slice**, asserts
    **both** that it builds the `±hh:mm` offset by hand **and** that it does **not** call
    `toISOString()`.
    - `funcBody` requires a literal top-level `function timeWindow(` and terminates at the first
      column-0 `\n}\n`, so the test must `t.Fatal` on `!ok` (the vacuity trap `funcBody`'s own doc
      comment warns about, `:359-366`) — the same `!ok → t.Fatal` shape
      `TestAssetsTheBodyRendererEscapes` uses (`:426-428`) — so a renamed or restyled anchor fails
      loudly rather than asserting over an empty slice.
    - **Both halves are scoped to that body**: a whole-file `!strings.Contains(js, "toISOString")`
      would be a tripwire for any future legitimate use elsewhere, and a whole-file positive
      `contains` would pass for a `timeWindow` that ignores its own computed offset.
    - The *why* goes in the test comment so nobody simplifies it back: `new Date('2026-09-21').toISOString()`
      emits `2026-09-21T00:00:00.000Z`, Go accepts it as UTC, and that is unix 1789948800 against the
      intended 1789929000, **19,800 s = 5h30m off, silently**.
  - **The wiring guard — `TestAssetsThePickerMountsBothTabs` (added during implementation; the bead
    named only the guard above, and it left four Outcome Definition clauses with no coverage at all).**
    It pins: `callFilter` and `loadStats` **both** call `timeWindow(`; the two `custom` option sets are
    read **per `<select>`** (via a `selectOptions(html, id)` helper that slices to `</select>`, so a
    `custom` in one row cannot satisfy an assertion about the other); all three of
    `hour`/`date`/`month` appear in both; `s-since`/`s-until` stay mounted **and** read; and the two
    defaults. The default half is the escalation above made enforceable. A picker mounted but never
    consulted would leave `TestAssetsEveryLookupHasAMount` green with a dead filter — that, not the
    id existence, is what this guards.
- **Integration Tests:** none in-repo. The semantic cases (local-instant denotation, Dec→Jan rollover,
  month width, DST) are **manual verification in the PR's test plan** — the ceiling is
  `assets_test.go:409-416`, which is regex and text over the embedded bytes.

## Files to Touch

- `internal/web/index.html` (modify — add `c-window-gran`/`c-window-value` to the Calls filter row
  `:54-63`; add `s-window-gran`/`s-window-value` beside the Stats row `:89-98` and keep
  `s-since`/`s-until` mounted)
- `internal/web/app.js` (modify — one `timeWindow()` helper; `callFilter()` `:275-286` sets
  `since`/`until`; `loadStats()` `:495-499` reads the picker; a change handler mirroring `f-apply`
  `:928` that also resets `callState.offset = 0`; populate each `<select>`'s `<option>` set per mount)
- `internal/web/assets_test.go` (modify — the `timeWindow`-body guard above; keep
  `TestAssetsEveryLookupHasAMount` covering the new ids)
- `internal/web/style.css` (modify — **only if** the new controls need it; the existing `.row`/`label`
  classes may already suffice)
