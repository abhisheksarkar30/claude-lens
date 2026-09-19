# Bead br-GI-5-01: The drill-down replaces its list — the two-mode Calls and Sessions detail

**Plan Reference**: `docs/planning/GI-5-call-detail-drilldown.md` — §3 D1–D10, §4 (`internal/web/index.html`, `internal/web/app.js`, `internal/web/style.css`), §5 T4/T5, §6 R2/R5 (plan sketch §9 beads 01–05, collapsed)

- **Bead ID**: br-GI-5-01
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: br-GI-5-02, br-GI-5-03

## Description

Clicking a call id never shows that call. The cause is not a broken route or a thrown exception:
`show('calls')` runs the Calls tab's **loader** (`loadCalls`, `app.js:124-144`), which fetches and
renders the whole 50-row list, and only then does `showCall` write the detail into `#call-detail` —
a block that sits **after** the list and pager in `index.html` (`:60`) and that nothing reveals or
scrolls to. From another tab the user sees the tab switch and the list flash; from the Calls tab
itself the click re-renders a byte-identical table and appears to do nothing. `#session-detail`
(`index.html:65`) has the identical defect on `showSession` (`app.js:218-236`).

This bead makes each drill-down a **mode switch**: the detail replaces the list it was clicked in,
with a back control, and a drill-down stops fetching a list it is about to hide. Everything in §9's
beads 01–05 lands here as one change — see "Why this is one bead" below.

### `index.html`

- Wrap the Calls view's filter row, `#calls-table` (`:54`) and pager (`:55-59`) in
  `<div id="calls-list"> … </div>`. The wrapper is what makes "the list" structural: a future
  container that should hide in detail mode must be added inside it (D1's residual-coupling note).
- Wrap `#sessions-table` (`:64`) in `<div id="sessions-list"> … </div>`.
- Add the `hidden` attribute to `#call-detail` (`:60`) and `#session-detail` (`:65`). A detail that
  starts visible would flash an empty panel on every tab load.

### `app.js` — the mode primitives (D2, D3, D5, D10)

```js
// Selecting a call shows the detail *instead of* the list it was clicked in:
// the list is hidden exactly when its detail is shown. Deriving the mode from
// the two `hidden` flags rather than a parallel boolean leaves nothing that can
// drift from what is actually on screen.
function setCallDetail(on) {
  $('calls-list').hidden = on;
  $('call-detail').hidden = !on;
}

function setSessionDetail(on) {
  $('sessions-list').hidden = on;
  $('session-detail').hidden = !on;
}
```

Both assignments are settled by the `hidden` attribute's UA default (`display: none`), so
`style.css` needs no rule for these containers — the existing `.view[hidden]` rule
(`style.css:87`) is unrelated. Two named functions rather than one `setMode(listId, detailId, on)`
helper: the call sites read as what they do and no id string is passed around to get wrong.

Split `show` (`app.js:617-631`) so the drill-down can switch to the Calls view **without** fetching
its list (D3). `setStatus('')` moves into `reveal`, so every view change still clears the status
line, including the drill-down path that no longer goes through `show`:

```js
// reveal switches the tab and un-hides the section -- the half of show() that
// does no fetching. The drill-down needs exactly this and nothing more: it is
// about to render one call, and a list of fifty is a fetch nobody sees.
function reveal(view) {
  detailSeq++; // take a fresh generation: any detail fetch still in flight is now stale (D10)
  current = view;
  document.querySelectorAll('#tabs .tab').forEach((b) => {
    b.classList.toggle('active', b.dataset.view === view);
  });
  document.querySelectorAll('.view').forEach((s) => {
    s.hidden = s.id !== 'view-' + view;
  });
  setStatus('');
}

async function show(view) {
  reveal(view);
  // Both details close on ANY tab click, unconditionally. A reset guarded by the
  // tab's own name would leave a detail open when the user leaves via Overview
  // or Warnings -- and both of those render their own data-call links,
  // so the next drill-down would reveal the previous call's request and response
  // bodies under a click for a different call until the fetch resolved.
  setCallDetail(false);
  setSessionDetail(false);
  try {
    await loaders[view]();
  } catch (err) {
    setStatus(err.message, true);
  }
}
```

The reset is **unconditional** — both resets run on every call, with no test on the view name (D5).
The comment's wording is load-bearing for br-GI-5-02's T3, which asserts `show`'s body does not
contain the literal `view ===`: it is a raw-text assertion, so it cannot tell a re-added guard from
a comment that quotes one. Do not reword that comment to spell the comparison out.
`show` has three callers: the startup
call `show('overview')` (`app.js:740`), the tab handler `show(tab.dataset.view)` (`app.js:637`), and
the D4 failure fallback below; the unconditional reset makes **all three** clear both modes, so no
tab leaves a detail open. Over **all four** `data-*` renderers this is the one route back to a list
that never needs a mode variable. Note the happy-path drill-down calls `reveal('calls')`, **never**
`show` (a future happy-path caller of `show('calls')` silently reintroduces the bug — R2); only the
failure fallback calls `show('calls')`, deliberately, because it *wants* the list.

Add the generation token (D10). This is a deliberate exception to D2: `detailSeq` does not say
which container is on screen (the `hidden` flags still do), it says **whether a response still
describes something the user asked for** — which is not derivable from the DOM, because by the time
the response resolves the screen may have moved on twice.

```js
// Every view change and every drill-down takes the next generation. A detail
// response whose generation is no longer current is dropped instead of
// rendered: the fetch is not cancelled, only its result is.
let detailSeq = 0;
```

`reveal` carries the bump as its **first** statement, before `current = view`, so every tab click
(`show` calls `reveal`) and every `[data-call]` drill-down take a fresh generation from the one
site.

### `app.js` — the two detail renderers (D4, D6, D7, D8, D10)

`showCall` (`app.js:146-191`) gains a `seq` parameter and, **immediately after the fetch, before any
`setCallDetail` and before any rendering**, drops a stale response:

```js
async function showCall(id, seq) {
  const { body } = await api('/api/requests/' + id);   // may throw
  // Stale by the time it landed: a later view change or drill-down has taken a
  // newer generation, so these bodies are not what was asked for. Rendering
  // them would put one call's request and response on screen under a click for
  // a different one.
  if (seq !== detailSeq) return;
  const e = body;
  ...
  setCallDetail(true);                                 // only now -- T2
  $('call-detail').innerHTML =
    '<p><button type="button" id="call-back">‹ all calls</button></p>' +
    '<h2>Call ' + esc(e.ID) + '</h2><table class="kv">' + details + '</table>' + ...;
  $('call-back').addEventListener('click', () => {
    setCallDetail(false);
    loadCalls();
  });
  $('call-detail').scrollIntoView();
}
```

- The mode flips only **after** `await api(...)` resolves (fail-open, D4) — the property T2 pins.
- **`esc(e.ID)`** — the existing `<h2>` line (`app.js:171`) interpolates `e.ID` with no escaping,
  the one interpolation here that breaks the file's rule ("`esc()` wraps every interpolated value",
  `dashboard.md:55`). Fixed while the line is rewritten; an `int64` cannot carry markup, so this is
  a rule-consistency fix, not a live injection.
- The back control is rendered into the injected markup (not parked in `index.html`) so it exists
  only when a detail does, and is bound immediately after the `innerHTML` assignment — exactly as
  the replay button already is (`app.js:179`). It calls `loadCalls()` so the list returned to is
  fresh (on a drill-down that started from another tab, the back click is the list's first fetch).
  `TestAssetsEveryLookupHasAMount` scans `app.js` for `id="…"` as well as `index.html`, so the
  runtime-created `call-back` / `session-back` mounts satisfy it.
- `scrollIntoView()` (D7): hiding a 50-row list collapses the page under a scroll position pointing
  into it, and the browser clamps to the new height, landing at the *bottom* of an ~800px detail.

`showSession` (`app.js:218-236`) gets the **identical** treatment (D8): a `seq` parameter, the same
`if (seq !== detailSeq) return;` guard after its fetch, `setSessionDetail(true)`, a
`session-back` control that calls `setSessionDetail(false); loadSessions();`, and `scrollIntoView()`.
Its `[data-session]` caller can race the same way — the sessions list stays visible until the fetch
resolves, so two session clicks are possible. Its drill-down does **not** call `reveal('sessions')`:
a session link only ever exists on the sessions view, so there is no view switch to make. That
asymmetry is real (D8), not an oversight.

### `app.js` — the delegated handler (D3, D4, D10)

One listener (`app.js:641-662`) feeds all four call-id renderers (`loadCalls` `:128`,
`showSession` `:221`, `loadWarnings` `:260`, `loadOverview` `:593`), so one fix covers all four. The
`[data-call]` branch (`:642-648`) reveals the Calls view without fetching and reads its generation
back after `reveal` (D10):

```js
document.addEventListener('click', async (ev) => {
  const call = ev.target.closest('[data-call]');
  if (call) {
    ev.preventDefault();
    // Reveal the Calls view without running its loader: the detail is what was
    // asked for, and fetching fifty rows only to hide them is a fetch nobody
    // sees. reveal() has just taken this drill-down's generation (D10), so a
    // late response from an earlier drill-down cannot land in #call-detail.
    reveal('calls');
    const seq = detailSeq;
    try {
      await showCall(call.dataset.call, seq);
    } catch (err) {
      // Only report a failure that is still current. A stale rejection must not
      // pull the user back to a view they have already left, and show('calls')
      // would bump the generation and take a newer detail down with it.
      if (seq === detailSeq) {
        await show('calls');
        setStatus(err.message, true);
      }
    }
    return;
  }
  const sess = ev.target.closest('[data-session]');
  if (sess) {
    ev.preventDefault();
    // No view change here, so this drill-down takes its generation directly.
    const seq = ++detailSeq;
    try {
      await showSession(sess.dataset.session, seq);
    } catch (err) {
      // Same symmetry as the [data-call] branch: only a failure that is still
      // current writes the status line; a stale one must not overwrite a newer
      // detail's.
      if (seq === detailSeq) setStatus(err.message, true);
    }
    return;
  }
  ...
});
```

Two load-bearing details:

- **The `[data-call]` happy path calls `reveal('calls')`, never `show`.** This is the defect: the
  drill-down must not fetch the list. The error line comes **after** `await show('calls')` because
  `show` starts with `reveal`, which runs `setStatus('')` — moving it before, or swapping the two
  lines, lets `show` wipe the detail error. This fallback is the repo's fail-open rule applied to
  the drill-down: a broken detail request degrades to the working list plus a visible error, never a
  blank detail panel.
- **Both catches carry `if (seq === detailSeq)`** (D10, F5.1). A stale *failure* is the sharper
  case: an unguarded `[data-call]` catch would run `show('calls')`, which bumps `detailSeq` past a
  newer in-flight fetch (dropping it by its own guard), resets both modes (wiping a newer rendered
  detail), and switches the view the user has already left. The rule is general — **nothing a stale
  fetch produces, value or error, is acted on.**

### `style.css` (D7)

```css
/* 72px is the single-row .app-header height: the sticky header would otherwise
   cover the ‹ all calls control, which is the first element of the detail. A
   header that wraps to two rows is taller than 72px and can still overlap the
   control; that ceiling is recorded in R4. */
#call-detail, #session-detail { scroll-margin-top: 72px; }
```

`.app-header` is `position: sticky` (`style.css:61`, the block `:53-63`), so without this offset it
would cover the back control the user needs next. 72px is the header's own shape (its `12px 20px`
padding, the title, and one row of tab buttons), not a value derived at runtime.

### Deliberately not changed (D9)

The `#call-detail` / `#session-detail` container ids; the SSE refresh (each detail is only ever
**visible** while `current` is its own view, so `subscribe()`'s `loaders[current]()` already writes
only the hidden list — the outcome D5/D9 agree on for every case); row-click targets (only the id
cell is a link — `dashboard.md`'s stale "click a row" is corrected in br-GI-5-03, not widened here).

### Why this is one bead (§9's 01–05 collapsed)

§9's 01–05 are one logical change. They are also **atomic**, which is why they cannot be split even
for review checkpoints: `index.html` adds `hidden` to `#call-detail`/`#session-detail`, and
**nothing un-hides them** until `showCall`/`showSession` call `setCallDetail(true)`/
`setSessionDetail(true)`. Land the html half first and the drill-down renders into a permanently
hidden container — a *worse* bug than the one being fixed. Land the app.js half first and
`$('calls-list')`/`$('sessions-list')` are `null`, so `setCallDetail` throws. The reveal/show split,
the setters, the `detailSeq` token and `scroll-margin-top` are all inert without the detail
renderers that use them (a `detailSeq++` with no reader, a `scroll-margin-top` with no
`scrollIntoView`). So the html, the app.js and the CSS land together. The guard (br-GI-5-02) and
the docs (br-GI-5-03) are separately verifiable and stay separate.

## Rationale

Without the mode switch, the detail always renders under a fully re-fetched 50-row table where
nobody can see it, and the drill-down keeps fetching a list the click was about to hide — both
halves of the report. Scrolling the existing detail into view (the three-line alternative) is
genuinely smaller and was rejected on the report's own wording: it leaves the user looking at the
same table shifted, and it keeps the wasted list fetch (D1). The `detailSeq` token is kept because
it closes a demonstrated race (F4.1, D10) in which one call's stored request and response bodies
render under a click for a different call; it is the structural fix three rounds of hand-tracing had
each wrongly declared closed. The change composes with what is already there — `callState`
(`app.js:109`) is module-level, so page and filters survive the round trip — and it adds no router,
store, component, build step, dependency, route, or schema change.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass, including the existing `assets_test.go`
  tests unchanged (`TestAssetsEveryLookupHasAMount` covers R5's new `calls-list` / `sessions-list`
  mounts).
- Clicking a call id from **any** of the four renderers (`app.js:128`, `:221`, `:260`, `:593`) shows
  only that call's detail — the list it was clicked in is hidden — and the happy path issues **no**
  `/api/requests?…` fetch (T5 step 1, DevTools Network).
- Clicking a session id (`app.js:202`) shows only the session detail, with a `‹ all sessions` back
  control; there is no view switch.
- Clicking **any** tab while a detail is open returns to that tab's list and runs its loader
  (unconditional reset, D5); the back control is the second route back.
- A detail fetch that is superseded by a later tab click or drill-down **never** renders — its
  success is dropped by `seq !== detailSeq` and its failure by the catch's `seq === detailSeq`.
- Functionally: T5's ten runbook steps pass (executed against `go run ./cmd/clens serve` at
  `http://127.0.0.1:8798` with a populated store; recorded in br-GI-5-03).

## Test Specifications

- Unit Tests: **none in this bead.** §9 makes the wiring guard its own bead; the T1–T3 assertions
  over these exact changes land in br-GI-5-02, and the existing `assets_test.go` suite must stay
  green here (R5).
- Integration Tests: none — no API, store or schema change (the endpoint is already covered by
  `TestGetRequestIncludesWarnings` / `TestGetRequestNotFound`, `api_test.go:135`/`:152`).
- E2E: none (no JS harness exists; `testing-and-quality.md:10` records "End-to-end: none").
- Manual (`§5 T5`, recorded by br-GI-5-03): the ten-step click-through — Overview/Warnings/Calls/
  Sessions drill-downs, the back control, the tab-as-second-route (step 8), the F4.1 racing window
  (step 9), and the stale-rejection catch guard (step 10).

## Files to Touch

- `internal/web/index.html` (modify — `#calls-list` / `#sessions-list` wrappers, `hidden` on both
  detail containers)
- `internal/web/app.js` (modify — `setCallDetail`/`setSessionDetail`, `reveal`/`show` split,
  `detailSeq`, `showCall`/`showSession` mode entry + stale-drop + back controls + `scrollIntoView` +
  `esc(e.ID)`, both handler branches)
- `internal/web/style.css` (modify — `scroll-margin-top` on the two detail containers)
