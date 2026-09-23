# Bead br-GI-13-10: the dashboard was inert — a Go keyword at the top level of app.js

**Plan Reference**: none — an unplanned defect found while verifying br-GI-13-07/08 against the
running build. It is not a perf change and does not belong to §3's C-list.

- **Bead ID**: br-GI-13-10
- **Priority**: P0 (critical)
- **Original Estimate**: 15m
- **Dependencies**: None
- **Blocks**: br-GI-13-06 (its re-profile reports a dashboard figure; this is what makes the
  dashboard render at all)

## Description

`internal/web/app.js` did not parse. At line 338, `br-GI-11-08` (the time-window picker, committed
`dcee0ec`) wrote:

```js
func mountWindowPicker(granSel, valueInput, freeLabels, onChange) {
```

`func` is Go. In JavaScript it is a syntax error, and a syntax error in a classic script is
**total**: the browser discards the entire file rather than the offending statement. So none of
`loadTotals()`, `loadProxyMode()`, `show('overview')` or `subscribe()` ever ran. The browser painted
the static HTML shell — zeros in the header, dead tabs, empty panels — and stopped there.

### Why this presented as a backend hang, and cost three rounds of backend work

Every API route answered while the page was inert: `/` in 1.9 ms, `/api/sessions` in 7 ms,
`/api/stats` in 2.2 s, and the consumer's newest row was 30 seconds old. The process was at 17% of
one core. A dead page was indistinguishable from a hung one, and the symptom — "dashboard not
loading" — pointed at the thing that had just been worked on.

The backend work it obscured was not wasted: br-GI-13-07 removed a real 1.65 s-per-flush cost and
br-GI-13-08 a real ~2 s per stats load. Neither was the reason the page was blank.

### Why no test caught it

Every guard in `assets_test.go` reads `app.js` as **text** — a `$('id')` lookup, a
`funcBody(js, name)` slice that greps for the literal `function <name>(`, or a slice between two
anchor comments. All of them pass on a file no browser will execute. `mountWindowPicker` in
particular was referenced by no test at all, so nothing looked at its declaration.

### The fix

1. `internal/web/app.js:338` — `func` → `function`. One word.
2. `internal/web/assets_test.go` — `TestAssetsAppJSIsJavaScript`, the one guard here that reads the
   file as code: it fails on any line beginning with a Go declaration keyword (`func`, `package`,
   `chan`, `defer`). `var`/`const`/`let` are deliberately excluded, since those are lines app.js
   legitimately contains. A one-word typo needs a check that would have caught *this* class, not a
   JS parser in a Go test.

## Rationale

The defect is one word; the reason it survived is the interesting half. `assets_test.go` has 31 KB
of assertions about `app.js` and not one of them would notice the file being unparseable, because
they all assert on its *content* and a parse failure destroys its *behaviour*. That asymmetry is
what the new guard closes.

## Outcome Definition

- `node --check internal/web/app.js` parses (it did not before).
- `TestAssetsAppJSIsJavaScript` fails when the `func` typo is reinstated and passes with it fixed —
  verified by reinstating it, running the test, and reverting.
- `go build ./... && go vet ./... && go test ./... -count=1` is green, including the TTFB gate in
  `internal/proxy/`. No Go source changes, so the hot path is untouched by construction.
- The served page renders: with the fixed build, the header totals populate and the tabs switch.

## Test Specifications

- Unit Tests (`internal/web/assets_test.go`): `TestAssetsAppJSIsJavaScript` — see above.
- Integration / manual: after redeploy, load the dashboard and confirm the totals and tabs work. Not
  automatable here: the repo has no browser harness, and a Go test cannot execute the embedded JS.

## Files to Touch

- `internal/web/app.js` (modify — one word)
- `internal/web/assets_test.go` (modify — the guard)

No other open bead touches either file.
