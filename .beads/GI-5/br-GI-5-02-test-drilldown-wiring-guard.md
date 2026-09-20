# Bead br-GI-5-02: Pin the two-mode drill-down wiring (T1–T3)

**Plan Reference**: `docs/planning/GI-5-call-detail-drilldown.md` — §2.4, §5 T1/T2/T3, §6 R1 (plan sketch §9 bead 06)

- **Bead ID**: br-GI-5-02
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-5-01
- **Blocks**: None

## Description

`assets_test.go` is entirely static regex over the asset *text*. It asserts that a mount point
exists, never that a click reveals anything — which is why a regression this loud stayed invisible.
This bead adds the guard the fix lacks: three tests over the wiring br-GI-5-01 added, in the style
the file already uses (`readAsset`, `funcBody`, the "guard the negative" idiom). All three live in
`internal/web/assets_test.go`.

The whole point of every assertion below is that it must **fail on the pre-fix text and on the
plausible regression**, not merely pass on the fixed text — so each negative is preceded by a
found-and-non-trivial vacuity guard, asserted **first**.

### T1 — `TestAssetsTheCallDetailReplacesTheList`

- `index.html` defines `id="calls-list"` and `id="sessions-list"`.
- Both detail containers are declared `hidden` (`<div id="call-detail" … hidden>`): a detail that
  starts visible would flash an empty panel on every tab load.
- `app.js` defines `setCallDetail` and `setSessionDetail`, **each toggling both** of its two
  containers — a one-sided toggle hides the list and shows nothing, a blank panel.
- `showCall`'s body contains `call-back` and `setCallDetail(false)`; likewise `showSession` /
  `session-back` / `setSessionDetail(false)`.

**The `[data-call]` slice, and why a scoped slice and not a file-wide check.** None of the file's
existing extraction helpers can produce the `[data-call]` branch: `funcBody`
(`assets_test.go:141-152`) reads a whole *top-level* `function name(` only, and the branch is nested
inside the anonymous `document.addEventListener('click', …)` listener (`app.js:641-662`). So T1
slices `app.js` between two literal source anchors —

- **start**: `const call = ev.target.closest('[data-call]')`
- **end**: `} catch` (the next occurrence of that literal after the start)

— and asserts, **in this order**:

1. **vacuity guard first** — the slice was found (both anchors present, end after start) and is
   non-trivial: it contains `showCall(` and `dataset.call`. Without this the negatives below pass
   for free on an empty string, or if a later refactor moves the anchors.
2. the slice contains `reveal('calls')`.
3. the slice does **not** contain `loadCalls(`.
4. the slice does **not** contain `show('calls')`.

Slicing to the handler's own `} catch` is exactly what exempts the deliberate failure-path fallback:
`show('calls')` lives *after* that anchor (it is in the catch), so it is out of the slice — while the
happy-path `await show('calls')` a regression would reintroduce (the D3 bug, which sits *before* the
`try`) is in it, and trips assertion 4. This is the same "scope it, or it passes for free" reasoning
as `TestAssetsEveryLookupHasAMount`'s `len(seen) < 30` floor (`assets_test.go:57-62`).

5. **the `[data-call]` catch guard, scoped to the catch body** — extract the region **after** the
   `} catch` anchor, up to that branch's terminating `return;`, and apply the same
   found-and-non-trivial vacuity guard (the region was found and contains `setStatus(`); then assert
   it contains `if (seq === detailSeq)`.
6. **the `[data-session]` catch guard, likewise** — slice from
   `const sess = ev.target.closest('[data-session]')` to its own `} catch`, take the region up to its
   `return;`, apply the same vacuity guard, and assert `if (seq === detailSeq)`.

Assertions 5 and 6 are scoped to their branch's **own** catch body, never run file-wide or
branch-wide. `if (seq === detailSeq)` is not unique in the handler: the sibling catch carries the
identical guard, and the branch entry `const seq = detailSeq;` and the success guard
`seq !== detailSeq` are the handler's other `detailSeq` comparisons. A check over the whole file (or
the whole handler) could be satisfied by the sibling catch alone, passing even with the `[data-call]`
catch left unguarded — the exact **F5.1** state assertion 5 exists to catch.

`app.js:1-741` is the file under test; the plan's verified line cites for the anchors are
`app.js:642` (`[data-call]`) and `:649` (`[data-session]`).

### T2 — the mode is not flipped before the fetch resolves, and a stale response is dropped

`setCallDetail(true)` must appear in `showCall` **after** the `await api(...)` line. Asserted by
index order within the function body, since this is the fail-open property from D4 and a reordering
in a later refactor would otherwise be silent.

The same index-order idiom pins D10's guard: `showCall`'s and `showSession`'s bodies must each
contain `seq !== detailSeq` **after** their `await api(...)` line. A guard moved above the fetch
would compare a generation before the response exists and drop a live detail every time; placed
after, it drops only a response a newer view change or drill-down has superseded. Each body is
extracted with `funcBody` (`assets_test.go:141-152`), and each assertion is preceded by the vacuity
guard the file already uses in T1 and T3 — the body was found and contains `await api(` and
`setCallDetail(true)` (resp. `setSessionDetail(true)`) — so a rename that defeats the extraction
fails loudly rather than passing on an empty string.

### T3 — `show` resets both modes unconditionally, and `reveal` takes the next generation

A bare presence check (`show`'s body references `setCallDetail(false)` and `setSessionDetail(false)`)
would stay green through the exact **F2.1** regression it exists to catch: the pre-F2.1 body was
`if (view === 'calls') setCallDetail(false);` / `if (view === 'sessions') setSessionDetail(false);`,
which contains both literals but skips the reset on Overview and Warnings — the bug. So T3 asserts
the *property*, not the substrings. Extract `show`'s body with `funcBody(js, "show")` (it matches
`async function show(view)`) and assert **in this order**:

1. **vacuity guard first** — the body was found and contains **both** `setCallDetail(false)` and
   `setSessionDetail(false)`. Load-bearing here for the same reason as in T1: assertion 2 passes for
   free on an empty or un-extracted body.
2. the body does **not** contain `view ===` — the unconditional-reset property, and the negative
   that catches a re-added guard around either reset.

This is a **raw-text** assertion, so the literal `view ===` is banned from `show`'s body entirely,
prose included. br-GI-5-01's prescribed comment is worded to avoid it ("a reset guarded by the
tab's own name", not the comparison itself) for exactly this reason. A later comment that
re-introduces the string therefore fails this test — a false positive in principle, and a loud one
in practice. The alternative (a comment-stripping regex) is machinery no other test in this file
has, and it would be the only one; the wording constraint is cheaper. `reveal`'s body does contain
`b.dataset.view === view`, which is why the assertion is scoped by `funcBody(js, "show")` rather
than run over the file.

T3 also pins D10's single bump site: `reveal`'s body (extracted with `funcBody(js, "reveal")`, which
matches `function reveal(view)`) contains `detailSeq++`. Every tab click routes through `show` →
`reveal` and every `[data-call]` drill-down calls `reveal` directly, so a `detailSeq++` that drifts
out of `reveal` — or a token renamed away from `detailSeq` — would silently stop superseding stale
detail responses. The same vacuity guard applies (the body was found and contains `current =`).

## Rationale

The fix is a text-level wiring contract that no other test in the repo can observe — the assets are
static strings and there is no JS harness (§5). Without this guard, a later "tidy" that swaps
`reveal('calls')` back to `show('calls')` reintroduces the exact reported bug silently. Each
assertion is scoped and vacuity-guarded precisely because a static check that stops matching must
fail loudly, never go green for the wrong reason (R1).

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- All three new tests pass against br-GI-5-01's committed assets, and the three existing tests
  (`TestAssetsEveryLookupHasAMount`, `TestAssetsTheFourNewTabsHaveTheirMountPoints`,
  `TestAssetsChartsAreInlineSVG`) pass unchanged.
- Each test **fails** when its property is broken: `reveal('calls')` reverted to `show('calls')`
  trips T1 assertion 4; a one-sided `setCallDetail` trips T1's toggle assertion; moving
  `setCallDetail(true)` above the fetch trips T2's index order; a re-added `view ===` guard in
  `show` trips T3 assertion 2; `detailSeq++` moved out of `reveal` trips T3's bump-site check.
- `go test ./internal/api/... ./internal/web/... -race` passes (T4).

## Test Specifications

- Unit Tests (`internal/web/assets_test.go`):
  - **T1** — `id="calls-list"`, `id="sessions-list"`, both detail containers `hidden`, both setters
    present and two-sided, the four back-control assertions, the `[data-call]` slice (vacuity guard
    → `reveal('calls')` → no `loadCalls(` → no `show('calls')`), and the two scoped catch-guard
    assertions (each with its own vacuity guard).
  - **T2** — `showCall` and `showSession`: `setCallDetail(true)` / `setSessionDetail(true)` after the
    `await api(...)` line, and `seq !== detailSeq` after it, each behind a vacuity guard.
  - **T3** — `show` body: vacuity guard (both resets present), then no `view ===`; `reveal` body:
    vacuity guard (`current =`), then `detailSeq++`.
- Integration Tests: none.
- E2E: none.

## Files to Touch

- `internal/web/assets_test.go` (modify — T1, T2, T3)
