# Bead br-GI-5-03: Correct the Calls-tab doc claim and record the manual click-through

**Plan Reference**: `docs/planning/GI-5-call-detail-drilldown.md` — §2.5, §4 (`docs/context/dashboard.md`, and the new §10 this bead adds), §5 T5, §7 (plan sketch §9 bead 07)

- **Bead ID**: br-GI-5-03
- **Priority**: P2 (medium)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-5-01, br-GI-5-02
- **Blocks**: None

Depending on br-GI-5-02 and not just br-GI-5-01 is deliberate: this bead adds the Assets-are-tested
row that *documents* `TestAssetsTheCallDetailReplacesTheList`, so landing it first would make
`dashboard.md` name a test that does not exist yet.

## Description

The closing documentation for the story, plus the recorded manual run the fix's verification rests
on.

**1. Correct the stale claim and document the two modes (`docs/context/dashboard.md`).** The route
table (`dashboard.md:35`) describes the Calls tab as *"the call log with filters; click a row for
the full request and response"*. Only the **id cell** is a link (`app.js:128`) and no row handler
exists, so "click a row" was never true — the code wins (D9). Correct it to say the **id** cell is
the link and that the detail **replaces** the list it was clicked in, with a `‹ all calls` control
back. The Sessions row (`dashboard.md:36`) gets the same two-mode note. Fix the doc to match the
code rather than widening the click target in a bug-fix story.

The change corrects the "click a row" text, so the doc still describes the shipped code after this
story; a doc that keeps the old phrase would describe behaviour the code has never had and now
definitely does not (D9, R6). Refresh the surrounding prose too:

- The **State management** section (`dashboard.md:45-61`) gains the mode switch: each of Calls and
  Sessions is two modes, the mode is derived from the `hidden` flags (never a parallel boolean,
  D2), a tab click returns to the list (D5), and a drill-down takes its own view — a call drill-down
  switches to Calls and fetches no list (`reveal`, D3/D4), while a session drill-down stays on
  Sessions (D8).
- The `dashboard.md:29` "Tab switching is not URL routing" note stays true and is not touched.
- The **Assets are tested** table (`dashboard.md:84-96`) gains a row for T1's
  `TestAssetsTheCallDetailReplacesTheList` (the wiring guard br-GI-5-02 adds), matching the existing
  one-row-per-test shape.

Do **not** hand-write a runbook into `docs/context/`: that tree is *generated* (Phase 5.6 runs
`document-project-context` in REFRESH mode across it), so a hand-written runbook there is clobbered
by the next refresh (the same reasoning br-GI-3-09 used to keep a runbook in the README). This bead's
generated-tree change is limited to the correction above, which belongs in the doc that already
describes the dashboard.

**2. Record the manual click-through (§5 T5).** The repo has no browser automation and no E2E suite
(`testing-and-quality.md:10`), so the automated half of this story is the T1–T3 wiring guard
(br-GI-5-02) and the behaviour half is a **recorded manual run**. The record goes in the **plan doc
itself**, as a new `## 10. Recorded manual run (T5)` section beside the runbook it reports on
(`docs/planning/GI-5-call-detail-drilldown.md` §5) — for two reasons. `docs/acceptance.md` is the
precedent §5 T5 cites for the *practice* of recording a manual run, but its own header scopes it to
br-GI-1-19's acceptance run and its Local-half/Live-half shape is a whole-product run; a
story-scoped UI click-through is neither, and folding it in would also make
`testing-and-quality.md`'s "one recorded manual run" claim wrong. And `docs/context/` cannot hold it
at all (see the generated-tree constraint below). The runbook is ten steps against
`go run ./cmd/clens serve` at `http://127.0.0.1:8798` with a populated store:

| # | Step | Expected |
|---|---|---|
| 1 | Overview → click a call id | Only that call's detail; **no** list. DevTools Network shows `/api/requests/{id}` and **no** `/api/requests?...`. |
| 2 | Click `‹ all calls` | The Calls list appears, filters and pager intact. |
| 3 | Calls tab → click an id in the `id` column | The list is replaced by the detail. |
| 4 | Warnings tab → click a `call` id | Detail replaces the warnings view's switch target (Calls), same as step 1. |
| 5 | Sessions tab → click a session id | Session detail only, with `‹ all sessions`; back restores the sessions list. |
| 6 | With a session detail open, click a `call` id in its calls table (`app.js:221`) | The view switches to **Calls**, in detail mode for that call. |
| 7 | From **Overview**, stop `clens serve`, then click a call id | The view switches to **Calls**; filter row intact, `#calls-table` showing the rows step 3 last loaded (sequential run) **or** empty (fresh load) — never blank (`loadCalls` is its only writer and writes nothing when `api()` rejects, `app.js:124-144`); the error is in the status line. |
| 8 | Open a call detail, click the **Overview** tab, then click a **different** call id there | The Calls view shows the **new** call, never the previous one, not even momentarily (the unconditional-reset case, D5 / F2.1). |
| 9 | Overview → click a call id, **immediately** click the **Warnings** tab (fetch still in flight), then click a **different** call id there | The new call's detail appears; the first call's bodies are **never** shown, not even momentarily (the F4.1 window D10 closes). |
| 10 | **Timing-dependent, best-effort.** From Overview click a call id, immediately click the **Warnings** tab, let that in-flight request **fail** before it resolves (stop `clens serve` in the window a slow fetch leaves open) | The view stays on **Warnings** and **no** status line appears for the abandoned click (the F5.1 catch guard). A run that cannot land inside the window records that rather than claiming the guard was exercised. |

The 404 path is **not** a step: every id a drill-down offers came from a row that existed at render
time, so no UI action produces a 404; it is covered at the API layer
(`TestGetRequestNotFound`, `api_test.go:152`) plus the shared `api()` failure path.

The plan's new §10 records, per step, what was observed against the run above — including:
the DevTools Network observation that step 1 issued `/api/requests/{id}` and **no**
`/api/requests?...` (this is the whole reported defect, so it is the one result worth stating in
full); whether step 9's racing window was actually observed; and whether step 10 landed inside its
timing window, or — the honest and likely outcome — that it did not, in which case §10 says so
rather than claiming the catch guard was exercised. A run whose store is too small to open step 9's
or 10's window records **that**, which is a real result about the verification, not a failure of
the fix.

## Rationale

`dashboard.md` is the map a future contributor reads before touching the dashboard; a stale "click a
row" there is the same class of error this story fixes in the code, and §2.5 chose to correct it in
this story rather than at a future audit. The manual run is the only thing that exercises the pixels
— the fix's whole failure mode is a detail that renders correctly where nobody can see it, which no
static guard can observe (T1 asserts the wiring; it cannot click).

## Outcome Definition

- `docs/context/dashboard.md` no longer says "click a row"; it says the **id** cell is the link and
  the detail replaces the list, for both Calls and Sessions, and the State-management prose describes
  the two modes, the back control, and the tab-click route back.
- The Assets-are-tested table lists the new `TestAssetsTheCallDetailReplacesTheList` guard.
- The §5 T5 runbook has been executed once against a real `clens serve` with a populated store, and
  its per-step results (including whether step 10 landed inside its timing window, and whether step
  9's racing window was actually observed) are recorded in the plan's new §10.
- `go build ./...`, `go vet ./...`, `go test ./...` still pass (docs-only change).

## Test Specifications

- Unit Tests: none (documentation).
- Integration Tests: none.
- E2E: none (no harness).
- Manual (`§5 T5`, recorded): the ten-step runbook above, with the step-7 starting-state assumption
  and the step-10 timing dependence stated in the record rather than assumed away.

## Files to Touch

- `docs/context/dashboard.md` (modify — correct `:35`/`:36`, refresh the State-management section,
  add the guard row to the Assets-are-tested table)
- `docs/planning/GI-5-call-detail-drilldown.md` (modify — append `## 10. Recorded manual run (T5)`
  with the per-step observed results; the plan's §4 table and §5 T5 name this as the run's home)
