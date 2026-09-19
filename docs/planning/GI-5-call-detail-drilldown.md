# GI-5 — Clicking a call id never opens the call

**Issue**: [GI#5](https://github.com/abhisheksarkar30/claude-lens/issues/5)
**Branch**: `GI-5-call-detail-drilldown`
**Status**: converged (plan v6, 6 review rounds)

---

## 1. Summary

Clicking a call id anywhere in the dashboard never shows that call. From another tab it switches to
the Calls tab and fetches the entire call log; from inside the Calls tab it appears to do nothing at
all. The Sessions tab has the same defect on its own drill-down.

The cause is not a broken route, a thrown exception, or a lost click. **The detail renders
correctly — under a fully re-fetched 50-row list, where nobody can see it.** `internal/web/app.js`
has one delegated click handler for every drill-down link, and it calls `show('calls')` before
`showCall(id)`. `show` runs the tab's *loader* (`loadCalls`), which fetches and renders the whole
list; `showCall` then writes the detail into `#call-detail`, a block that sits **after** the list
and pager in `index.html` and that nothing reveals or scrolls to.

This plan makes each of the two drill-downs a mode switch: **the detail replaces the list it was
clicked in, with a back control**, and a drill-down stops fetching a list it is about to hide.

Scope is small, but not as small as the defect suggests: on the order of 80 changed lines across
three source files (`app.js`, `index.html`, `style.css`), much of it the comments this codebase's
style requires. No build step, no new dependency, no API change, no schema change.

The gap between the two is the story of this plan. The reported symptom is a navigation bug, and
the obvious fix for it is three lines. The cross-review established that the obvious fix leaves two
paths on which the *wrong* call's stored request and response bodies reach the screen — §3 D10 and
R7 — and closing those is what the other ~75 lines are for.

---

## 2. Evidence base

### 2.1 The defect, reproduced

Driving the real `internal/web/app.js` against a minimal DOM shim with canned API responses and
firing a click on the markup `table()` actually builds (`<a href="#" data-call="42">42</a>`):

```
preventDefault called      : true
fetch calls issued         : ["/api/requests?limit=50&offset=0","/api/requests/42"]
view-calls hidden          : false          <- the tab does switch
#calls-table html length   : 389           <- the whole list renders
#call-detail html length   : 809           <- the detail renders too, below it
errors                     : none
```

Read off it:

| Observation | Consequence |
|---|---|
| Two fetches, **list first** | "abruptly fetches list of calls" |
| `view-calls` un-hidden | "switches the tab to calls" |
| `#call-detail` populated, **not hidden** | the detail *does* render — there is no exception |
| `#calls-table` populated in the same click | the detail is always below a 50-row table |
| `errors: none` | nothing is silently failing; nothing is logged |

The list fetch is unconditional, so it also runs when the Calls tab is **already** the current view.
There it re-renders a byte-identical table and changes nothing on screen — which is precisely the
"nothing happens, nothing opened" half of the report.

> **Method note.** This harness was a throwaway run outside the repo purely to settle the
> diagnosis; it is not committed and is not part of the test strategy (§5). The code it drove is
> the committed asset.

### 2.2 The single point every drill-down routes through

`app.js:641-662` is one delegated listener on `document`, and **all four** call-id renderers feed it:

| Renderer | Line | Renders into |
|---|---|---|
| `loadCalls` | [app.js:128](../../internal/web/app.js#L128) | Calls tab table |
| `showSession` | [app.js:221](../../internal/web/app.js#L221) | Sessions tab's call table |
| `loadWarnings` | [app.js:260](../../internal/web/app.js#L260) | Warnings tab table |
| `loadOverview` | [app.js:593](../../internal/web/app.js#L593) | Overview "Recent calls" |

So "all the tabs having the hyperlink of call id" is literally one code path, and one fix covers all
four. There is no second handler to forget.

### 2.3 The API side is correct and already covered

- `GET /api/requests/{id}` exists (`api.go:168`) and returns the event with its warnings attached
  (`eventDetail`, `api.go:366-397`).
- It is tested: `TestGetRequestIncludesWarnings` and `TestGetRequestNotFound`
  ([api_test.go:135](../../internal/api/api_test.go#L135), [:152](../../internal/api/api_test.go#L152)).
- `store.Event.ID` is `int64` with no JSON tags ([types.go:9](../../internal/store/types.go#L9)), so
  it serializes as `"ID": 42` and the JS reads `e.ID` — matching `dataset.call === "42"`.

**The defect is entirely client-side.** No API, store or schema change is in scope.

### 2.4 The existing guard structurally cannot catch this

[assets_test.go](../../internal/web/assets_test.go) is entirely static regex over the asset *text*.
It asserts that a mount point exists, never that a click reveals anything
(`TestAssetsEveryLookupHasAMount`, `TestAssetsTheFourNewTabsHaveTheirMountPoints`,
`TestAssetsChartsAreInlineSVG`). A view that renders perfectly into a permanently-invisible
container passes all three. That is why a regression this loud stayed invisible — and it is why
§5 adds a guard rather than assuming the existing suite would have caught it.

### 2.5 A context-doc claim that is stale

[dashboard.md:35](../../docs/context/dashboard.md#L35) describes the Calls tab as *"the call log with
filters; **click a row** for the full request and response"*. Only the **id cell** is a link and no
row handler exists, so "click a row" was never true. The code wins; §4 corrects the doc as part of
this story rather than at some future audit (Phase 5.6).

**The sentence has a second home, found in the impl cross-review (F1.1).** `README.md:151` carries
the identical row, byte-for-byte:

```
| Calls | the call log with filters; click a row for the full request and response |
```

`README.md` is at the repo root — **outside** the generated `docs/context/` tree — so no Phase 5.6
refresh will ever repair it, and correcting one copy while shipping the other leaves the story's own
claim false in the place a new contributor reads first. It is corrected in the same pass, and §4's
inventory gains both files. Recorded here because this section is the story's evidence base: a stale
claim counted once when it exists twice is how the second copy survives.

---

## 3. Design

### D1 — Each drill-down is a mode switch, not a block appended below

The Calls and Sessions views each gain two modes, and the detail is *shown instead of* the list it
was clicked in:

```
BEFORE                                    AFTER
  [ filters              ]                  ‹ all calls
  id | time | model | ...  <- 50 rows       Call 42
  ‹ prev  1-50 of 900  next ›               request id   req_abc
  ...detail, below the fold...              session      sess-1
                                            ...
```

**Why not the three-line alternative** (`scrollIntoView` with the list left in place). It is
genuinely smaller, and it does fix "nothing happens". It was rejected because:

1. The report asks for *"a single one in detail"* **instead of** the list, explicitly. Scrolling
   leaves the user looking at the same 50-row table they were already looking at, just shifted.
2. It keeps the drill-down fetching a list the click was about to hide — the "abruptly fetches
   list of calls" half of the report survives the fix.

It also composes: the back control is what makes the list reachable again, and `callState`
(`app.js:109`) is module-level, so page, filters and scroll-independent state survive the round
trip unchanged.

### D2 — The mode is derived from the DOM, never a parallel boolean

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

Two small named functions rather than one `setMode(listId, detailId, on)` helper: the call sites
read as what they do, and no id string is passed around to get wrong. Both assignments are settled
by the **`hidden` attribute's UA default** (`display: none`), so `style.css` needs no rule for
these two containers — the existing `.view[hidden]` rule is unrelated to them.

This rule governs the *mode*. The one other piece of state the design carries — the `detailSeq`
generation token (**D10**) — is not a mode and is not derivable from the DOM, which is why D2 does
not forbid it; D10 states the distinction.

### D3 — `show` splits: `reveal` does no fetching

The drill-down needs "switch to the Calls view" *without* "fetch the call list". Today those are
the same call. Split them:

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

`setStatus('')` moves into `reveal` so every view change still clears the status line, including
the drill-down path that no longer goes through `show`.

After this change the **happy path** of the drill-down calls `reveal('calls')`, never `show` — that
is the property T1 pins (assertion 4), and a future happy-path caller of `show('calls')` would
silently reintroduce this bug. The drill-down is the only caller of `reveal` that is not `show`, and
it does not reset its own mode: it depends on the mode being whatever the last `show` left it.

`show` has three callers — the startup call (`show('overview')`, `app.js:740`), the tab handler
(`show(tab.dataset.view)`, `app.js:637`), and the D4 failure fallback (`show('calls')`). Because the
reset is unconditional, **all three** reset both modes: no tab leaves a detail open. Keeping the
reset in `show` rather than in each caller is what makes D5 hold for the tab handler and the fallback
alike, with no third copy to drift. Both facts get a comment, because the contract is what stops the
bug returning.

The drill-down does **not** reset its own mode on entry. It relies on the mode being whatever the
last `show` left it, and that reliance is safe because the one response that could flip the mode
outside a fresh drill-down — the late `setCallDetail(true)` from an in-flight `showCall` the user has
tabbed away from — is dropped by **D10** (R7, F4.1). The "mode is always `list` at a drill-down's
start" premise the last three rounds leaned on is **false** on its own: a tab click *between* two
drill-downs lets that late flip land *after* `show`'s reset, so the entry reset F2.2 once added was
**not** behaviourally inert, and F3.3 deleted it on that false premise (retracted in v5). D10 — not
the impossibility of the race — is why there is no entry reset now. Stated in a comment at the
handler's `reveal('calls')` call (D4).

### D4 — The drill-down fetches no list; a failure falls back to one

The `[data-call]` branch reveals the Calls view without fetching — the point of the D3 split — and a
failure falls back to the list it already revealed:

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
  ...
});
```

`showCall` itself flips the mode only after its own `await api(...)` resolves (T2):

```js
async function showCall(id, seq) {
  const { body } = await api('/api/requests/' + id);   // may throw
  if (seq !== detailSeq) return;                        // stale, dropped -- D10
  const e = body;
  ...
  setCallDetail(true);                                 // only now
  $('call-detail').innerHTML = ...;
```

**Success.** `reveal('calls')` switched the tab, cleared the status line, and took a fresh generation
(D10). Any earlier detail response still in flight — which a tab click between two drill-downs could
otherwise let land late and flip a stale body back on screen (F4.1) — now fails its `seq !== detailSeq`
guard and is dropped, so the mode is `list` when this drill-down starts and no previous call's body
can be on screen; `showCall` then fetched and rendered the one call, and flipped to the detail — no
list fetch, no stale body, no blank panel.

**Failure.** `showCall` throws out of `api()` (a 404, a dead API); the catch runs `show('calls')`
**only if the failure is still current** (`seq === detailSeq`) — a stale rejection is dropped rather
than acted on, because falling back would bump the generation and take a newer detail down with it
(D10). A live failure **does switch the tab** — it does not stay on the original tab — and lands on
the Calls view, not on an empty detail panel: `show` resets both modes (D3) — right, because a list
view is what is now on screen — and then runs `loadCalls()`. When the API answers, that paints the
list; when the API is the thing that is down, `loadCalls` throws before `table()` runs
(`app.js:124-144`), so `#calls-table` keeps whatever it last held — empty if it was never loaded —
with the filter row intact above it. In both cases the error goes on the status line.

**Ordering is load-bearing and must not be "tidied".** `show` starts with `reveal`, which runs
`setStatus('')`, so the error line must come *after* `await show('calls')`; moving it before, or
swapping the two lines, lets `show` wipe the detail error. This fallback is the repo's fail-open
rule applied to the drill-down: a broken detail request degrades to the working list plus a visible
error, never a blank detail panel.

The mode-flip order is pinned by T2; the fallback's outcome is a recorded manual step (T5 step 7),
because no JS harness exists to click a link (§5).

### D5 — The tab click is the way back to a list

`show()` resets both modes unconditionally (D3), so clicking **any** tab while a detail is open
returns to that tab's list and runs its loader. The detail's own back control is the first route
back; the tab click is the second. Both end at the same place, and neither needs a mode variable to
stay in sync.

The reset has to be unconditional, not guarded by the tab's own view, and the cross-drill-down is
the case that shows why. A **session** detail renders its own calls table (`showSession`,
`app.js:221`), and clicking a call id *inside* it runs the D4 handler — which calls `reveal('calls')`
and leaves `#view-sessions` still in detail mode, hidden behind the Calls view. Nothing is looking at
that hidden detail, and the D4 handler's happy path never calls `show`, so a reset guarded by
`view === 'sessions'` would only run if the user reselected the Sessions tab. The unconditional
reset is what clears it — and the same argument covers a call detail left behind when the user
switches to Overview or Warnings, both of which render their own `data-call` links (`app.js:593`,
`app.js:260`).

The drill-down's failure fallback (D4) reaches the list the same way — through `show('calls')`, mode
reset included — so the tab handler and the fallback share the one reset rather than two.

### D6 — The back control is rendered inside the detail

```js
  setCallDetail(true);
  $('call-detail').innerHTML =
    '<p><button type="button" id="call-back">‹ all calls</button></p>' +
    '<h2>Call ' + esc(e.ID) + '</h2><table class="kv">' + details + '</table>' + ...;

  $('call-back').addEventListener('click', () => {
    setCallDetail(false);
    loadCalls();
  });
```

Rendered into the injected markup rather than parked statically in `index.html` so it exists only
when a detail does — and bound immediately after the `innerHTML` assignment, exactly as the
existing replay button already is (`app.js:179`). `TestAssetsEveryLookupHasAMount` scans app.js for
`id="…"` as well as index.html, so the runtime-created `call-back` / `session-back` mounts satisfy
it (the file's own comment says this is the intent).

Two details worth stating:

- **`esc(e.ID)`** — the existing line is `'<h2>Call ' + e.ID + '</h2>'` with no escaping. An
  `int64` cannot carry markup, so this is not a live injection, but it is the one interpolation in
  this function that breaks the file's uniform rule ("`esc()` wraps every interpolated value",
  `dashboard.md:55`). Fixed while the line is being rewritten anyway.
- **The back control calls `loadCalls()`**, so the list the user returns to is fresh. On a
  drill-down that started from another tab, the list has never been loaded at all — the back click
  is the first time it is fetched, which is exactly when it is first wanted.

### D7 — Scroll the detail into view, with a `scroll-margin-top` for the sticky header

Hiding a 50-row list collapses the page under a scroll position that was pointing into that list;
the browser clamps to the new height, which lands the viewport at the *bottom* of an ~800px detail.
So after rendering:

```js
  $('call-detail').scrollIntoView();
```

and in `style.css`, `scroll-margin-top` on the two detail containers, because `.app-header` is
`position: sticky` (`style.css:61`) and would otherwise cover the control the user needs next:

```css
/* 72px is the single-row .app-header height: the sticky header would otherwise
   cover the ‹ all calls control, which is the first element of the detail. A
   header that wraps to two rows is taller than 72px and can still overlap the
   control; that ceiling is recorded in R4. */
#call-detail, #session-detail { scroll-margin-top: 72px; }
```

72px is the header's own shape — its `12px 20px` padding, the title, and one row of tab buttons —
not a value derived at runtime. Native CSS rather than a scroll-offset calculation in JS: sizing it
to a *wrapped* header would take JS measurement (or a CSS custom property written on resize) for a
layout this tool only reaches at narrow widths, and R4 names that as an accepted ceiling rather than
machinery this fix should carry.

### D8 — The Sessions tab gets the same treatment, in the same story

`showSession` (`app.js:218-236`) renders `#session-detail` under an un-hidden 50-row sessions table:
the identical defect on the identical pattern, reached by a different `data-*` attribute. The
report did not name it, but the fix is one call to `setSessionDetail` and one back control, and
leaving it means shipping a fix that knowingly misses its own sibling. Included.

Its drill-down does **not** call `reveal('sessions')`: a session link only ever exists on the
sessions view, so there is no view switch to make. That asymmetry is real, not an oversight.

### D9 — What is deliberately *not* changing

- **The existing `#call-detail` / `#session-detail` container ids.** Renaming them would churn
  `TestAssetsEveryLookupHasAMount` and every renderer for no behavioural gain.
- **`loadCalls` on the SSE refresh.** Each detail is only ever open while `current` is that detail's
  own view: the call detail only while `current === 'calls'` (`reveal('calls')` sets it, D10 drops a
  late response that would otherwise show it after a tab click, and the unconditional reset (D3)
  clears it on any tab click), and the session detail is **visible** only while
  `current === 'sessions'` (`showSession` is reached from the `[data-session]` branch,
  `app.js:649-654`, which calls neither `show` nor `reveal`, so `#session-detail` is open with
  `current` left at `'sessions'`). The one case where the session detail stays open-but-**hidden** is
  the cross-drill-down (D5): clicking a call id *inside* a session detail runs `reveal('calls')`, so
  `current` becomes `'calls'` while `#view-sessions` keeps its detail mode, behind the Calls view.
  Read as "visible", D5 and D9 agree. So `subscribe()` still calls `loaders[current]()` → that view's
  loader, which writes the hidden list in every case — and
  the same holds after a D4 failure fallback, because `show('calls')` leaves `current === 'calls'`
  too. It is one request per event whose result is invisible — the same cost as today, so this is
  not a regression — and it leaves the list warm for the back click. Left alone deliberately rather
  than special-cased, which would add a branch to the one place that must not branch wrongly.
- **Row-click targets.** `dashboard.md`'s "click a row" stays false; only the id cell is a link.
  §4 fixes the doc to match the code rather than widening the click target in a bug-fix story.

### D10 — A generation token invalidates a detail fetch whose result is no longer wanted

D3 and D4 above used to justify the absent entry reset with "the mode is always `list` at a
drill-down's start, so a reset could not change behaviour". **That premise is false**, and round 4
supplied the counter-example: click call **A** on Overview, and while `showCall(A)` is in flight click
the **Warnings** tab (`show('warnings')` resets the mode), then click a **different** call **B** on
Warnings. A's late `setCallDetail(true)` lands *after* `show`'s reset, so the mode is `detail` with
A's request and response bodies in `#call-detail`; `reveal('calls')` — which does not touch the mode —
then un-hides that stale body under a click for **B** until B's fetch resolves. That is F2.1's harm
verbatim, and it is the window R7(a) always admitted.

Reinstating the entry reset is not the fix: a second mechanism for a property `show` already owns
would leave the next reader unable to tell which one is load-bearing. The fix is to make a detail
response that no longer describes what the user asked for un-renderable:

```js
// Every view change and every drill-down takes the next generation. A detail
// response whose generation is no longer current is dropped instead of
// rendered: the fetch is not cancelled, only its result is.
let detailSeq = 0;
```

D3's `reveal` carries the bump as its **first** statement, before `current = view`, so every tab
click (`show` calls `reveal`) and every `[data-call]` drill-down take a fresh generation from the one
site. D3 is the function's actual shape; the token's rationale is here.

The `[data-session]` branch takes its generation directly, because it calls neither `show` nor
`reveal` — it stays on the sessions view (D8):

```js
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
```

The `[data-call]` branch reads it back after `reveal`, which has just taken one (this is the same
handler D4 shows, in the same form — one branch, one rendering):

```js
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
```

Both fetchers drop a result that is no longer current, guarding **immediately after the fetch** —
before any `setCallDetail`/`setSessionDetail` and before any rendering:

```js
async function showCall(id, seq) {
  const { body } = await api('/api/requests/' + id);
  // Stale by the time it landed: a later view change or drill-down has taken a
  // newer generation, so these bodies are not what was asked for. Rendering
  // them would put one call's request and response on screen under a click for
  // a different one.
  if (seq !== detailSeq) return;
  const e = body;
  ...
}
```

`showSession` gets the identical guard; its `[data-session]` caller can race the same way, because
the sessions list stays visible until the fetch resolves, so two session clicks are equally possible.

**The guard is symmetric across both halves of a fetch — success *and* failure — because a fetch can
end either way.** The rule is general: **nothing a stale fetch produces, value or error, is acted
on.** The success guard in `showCall`/`showSession` and the guard in each branch's `catch` are its
two instances. A stale *failure* is the sharper case: an unguarded `[data-call]` catch would run
`show('calls')`, which bumps `detailSeq` past a newer in-flight fetch (dropping it by its own guard),
resets both modes (wiping a newer rendered detail), and switches the view the user has already left.
The `seq === detailSeq` check on the `catch` leaves only a live failure to fall back.

**One effect is deliberately left unguarded, found by the impl cross-review (F1.3) — an accepted
ceiling, recorded rather than claimed away.** In the `[data-call]` catch, `setStatus(err.message,
true)` runs *after* `await show('calls')` and is not re-guarded. A drill-down begun during that
`await` can therefore have the status line overwritten by the abandoned failure's message. It cannot
be closed with the same token: `show('calls')` reaches `reveal`, which bumps `detailSeq` itself, so a
generation re-check after the `await` would always fail and the fallback would never report its error
at all — observably wrong, since T5 step 7 asserts exactly that status line. The order is
load-bearing in the other direction too (D4): `setStatus` has to come *after* `show`, because
`reveal` calls `setStatus('')` and would clear it. Closing this needs a second token, or a generation
threaded back out of `show` — real machinery for a cosmetic window that is ~5 ms wide (the measured
`loadCalls` fetch, §10) and self-correcting in its own common case: the newer drill-down is hitting
the same failing server and writes its own error, and a live failure writes the same
`Failed to fetch` text either way. So the rule above is precise as **everything a stale fetch
produces that would be *rendered* is dropped**; the status line is the one channel where an
abandoned failure's message can briefly land, and it is named here rather than left to be
rediscovered.

**This does not contradict D2.** D2 derives the *mode* from the DOM rather than keeping a parallel
boolean, because the mode — which of two containers is on screen — is already in the `hidden` flags,
and a boolean could only drift from them. `detailSeq` is not that: it does not say which container is
on screen (the `hidden` flags still do), it says **whether a response still describes something the
user asked for** — which is not derivable from anything on screen, because by the time the response
resolves the screen may have moved on twice. A token a stale response is measured against is not a
second copy of a fact the DOM already holds.

The mode-flip order is unchanged and still pinned by T2; D10's guard sits *after* the fetch, so a
stale response never writes the mode, and the first `setCallDetail(true)` a live response writes is
the only one that runs. R7 records what D10 does **not** do: the in-flight request is not aborted,
only its result discarded.

---

## 4. Files changed

| File | Change |
|---|---|
| `internal/web/index.html` | Wrap the calls filter/table/pager in `<div id="calls-list">`; wrap `#sessions-table` in `<div id="sessions-list">`; add the `hidden` attribute to `#call-detail` and `#session-detail`. |
| `internal/web/app.js` | Split `show` into `reveal` + loader run, with `show` resetting both modes unconditionally (D3); add `setCallDetail` / `setSessionDetail`; add the `detailSeq` generation token, bumped in `reveal` and taken by each drill-down, with `showCall` / `showSession` dropping a stale response whose `seq` is no longer current (D10); `showCall` / `showSession` enter detail mode and render a back control; the `[data-call]` branch's happy path calls `reveal('calls')` instead of `show('calls')` (no entry reset — D4), and its failure path falls back to `show('calls')` (D4); `esc(e.ID)`. |
| `internal/web/style.css` | `scroll-margin-top` on the two detail containers. |
| `internal/web/assets_test.go` | The wiring guard (§5, T1–T3). |
| `docs/context/dashboard.md` | Correct "click a row" and document the two modes per view (Phase 5.6). |
| `README.md` | The same "click a row" row at `:151`, corrected in the same pass (F1.1 — §2.5). It is outside the generated tree, so nothing else repairs it. |
| `docs/context/INDEX.md` | `:40`'s "741 lines" for `app.js`, stale at 838 the moment this story's fix lands. Phase 5.6 owns this file; the count is corrected here so the tree is not knowingly wrong in the interim (F1.2). |
| `docs/planning/GI-5-call-detail-drilldown.md` | New §10: the recorded manual run (T5), appended during implementation. |

Nothing else. No Go handler, no store, no schema, no config, no dependency, no build step.

The last row is the one file here that is not source: T5's run has to land somewhere, and §5 T5 says
the story follows `docs/acceptance.md`'s precedent without naming a home. The **plan doc** is that
home — recorded in §10 beside the runbook it reports on. `docs/acceptance.md` is the precedent for
the *practice*, but its own header scopes it to br-GI-1-19's acceptance run and its Local-half /
Live-half shape is a whole-product run, so a story-scoped UI click-through would be misfiled there
(and would make [testing-and-quality.md:48](context/testing-and-quality.md#L48)'s "one recorded
manual run" claim wrong). `docs/context/` cannot hold one at all — that tree is generated, and a
hand-written runbook there is clobbered by the next refresh. Named here because §4's list is the
contract Phase 3 derives beads from, and it was incomplete without it.

---

## 5. Test strategy

**State the ceiling first.** The repo has no browser automation and no E2E suite
([testing-and-quality.md:10](../../docs/context/testing-and-quality.md#L10) records "End-to-end:
none"). A Go test cannot click a link, and this repo's whole dashboard story is "no build step, no
bundler, no CDN" — so introducing a JS toolchain to test ~80 lines of DOM would be a larger change
than the fix, and would violate the rule the assets exist under. **The automated half of this story
is a wiring guard plus a recorded manual click-through**, and the plan says so rather than implying
a test proves the pixels.

### T1 — `TestAssetsTheCallDetailReplacesTheList` (`internal/web/assets_test.go`)

The regression, pinned as a text invariant, in the style the file already uses:

- `index.html` defines `id="calls-list"` and `id="sessions-list"`.
- Both detail containers are declared `hidden` (`<div id="call-detail" … hidden>`): a detail that
  starts visible would flash an empty panel on every tab load.
- `app.js` defines `setCallDetail` and `setSessionDetail`, each toggling **both** of its two
  containers — a one-sided toggle hides the list and shows nothing, which is a blank panel.
- The **happy path** of the `[data-call]` branch contains `reveal('calls')` and contains neither
  `loadCalls(` nor `show('calls')`. This is the actual defect: **the drill-down must not fetch the
  list.** (The failure fallback legitimately calls `show('calls')` — D4 — so this is asserted over a
  *scoped slice*, not over the whole file: see below.)
- `showCall`'s body contains `call-back` and `setCallDetail(false)`; likewise `showSession` /
  `session-back` / `setSessionDetail(false)`.

**The extraction is a named slice, and the negatives are guarded and ordered.** None of the file's
existing extraction helpers can produce this branch: `funcBody` (`assets_test.go:141-152`) reads a
whole *top-level* `function name(` only, and the `[data-call]` branch is nested inside the anonymous
`document.addEventListener('click', …)` listener. So T1 slices `app.js` between two literal source
anchors —

- **start**: `const call = ev.target.closest('[data-call]')`
- **end**: `} catch` (the next occurrence of that literal after the start)

— and asserts, **in this order**:

1. **vacuity guard first** — the slice was found (both anchors present, end after start) and is
   non-trivial: it contains `showCall(` and `dataset.call`. Without this the negatives below pass
   for free on an empty string, or if a later refactor moves the anchors out from under the test.
2. the slice contains `reveal('calls')`.
3. the slice does **not** contain `loadCalls(`.
4. the slice does **not** contain `show('calls')`.
5. **the `[data-call]` catch guard, scoped to the catch body** — extract the region **after** the
   `} catch` anchor, up to that branch's terminating `return;`, and apply the same
   found-and-non-trivial vacuity guard the slice uses (the region was found and contains
   `setStatus(`); then assert it contains `if (seq === detailSeq)`.
6. **the `[data-session]` catch guard, likewise** — slice from
   `const sess = ev.target.closest('[data-session]')` to its own `} catch`, take the region up to its
   `return;`, apply the same vacuity guard, and assert `if (seq === detailSeq)`.

**Assertions 5 and 6 are scoped to their branch's own catch body, never run file-wide or
branch-wide.** `if (seq === detailSeq)` is not unique in the handler: the sibling `[data-session]`
catch carries the identical guard (both added in the same round, D10), and the branch entry
`const seq = detailSeq;` and the success guard `seq !== detailSeq` are the handler's other
`detailSeq` comparisons. A check over the whole file — or the whole handler — could therefore be
satisfied by the sibling catch alone, passing even with the `[data-call]` catch left unguarded: the
exact **F5.1** state assertion 5 exists to catch. Bounding each region to `} catch` … `return;` is
what makes these assertions about *their own* catch. This is the same "scope it, or it passes for
free" reasoning F3.2 established for T3: aimed at the property, not a string that already lives
elsewhere in the file.

Slicing to the handler's own `} catch` is exactly what exempts D4's deliberate failure-path
fallback: `show('calls')` lives *after* that anchor, so it is out of the slice — while the
happy-path `await show('calls')` a regression would reintroduce (the bug D3 names, which sits
*before* the `try`) is in it, and trips assertion 4. The order is load-bearing: assertions 2–4 are
only meaningful once assertion 1 has proved the slice is real. This is the same reasoning as
`TestAssetsEveryLookupHasAMount`'s `len(seen) < 30` floor — a pattern that silently stops matching
must fail loudly, not go green vacuously.

D10 adds nothing to this slice: the `[data-call]` happy path still contains no `loadCalls(` and no
`show('calls')` (the `const seq = detailSeq;` it adds is neither), and the failure fallback's
`show('calls')` still sits after the `} catch` anchor. T1's assertion 4 is unaffected.

### T2 — the mode is not flipped before the fetch resolves, and a stale response is dropped

`setCallDetail(true)` must appear in `showCall` **after** the `await api(...)` line. Asserted by
index order within the function body, since this is the fail-open property from D4 and a
reordering during a later refactor would be silent otherwise.

The same index-order idiom pins D10's guard: `showCall`'s and `showSession`'s bodies must each
contain `seq !== detailSeq` **after** their `await api(...)` line. A guard moved above the fetch
would compare a generation before the response exists and drop a live detail every time; placed
after, it drops only a response a newer view change or drill-down has superseded. Each body is
extracted with `funcBody` ([assets_test.go:141-152](../../internal/web/assets_test.go#L141)), and
each assertion is preceded by the vacuity guard the file already uses in T1 and T3 — the body was
found and contains `await api(` and `setCallDetail(true)` (resp. `setSessionDetail(true)`) — so a
rename that defeats the extraction fails loudly rather than passing on an empty string.

### T3 — `show` resets both modes unconditionally, and `reveal` takes the next generation

A bare presence check (`show`'s body references `setCallDetail(false)` and `setSessionDetail(false)`)
would stay green through the exact F2.1 regression it exists to catch: the pre-F2.1 body was
`if (view === 'calls') setCallDetail(false);` / `if (view === 'sessions') setSessionDetail(false);`,
which contains both literals but skips the reset on Overview and Warnings — the bug. So T3 asserts
the *property*, not the substrings. Extract `show`'s body with `funcBody(js, "show")` (it matches
`async function show(view)`, `assets_test.go:141-152`) and assert **in this order**:

1. **vacuity guard first** — the body was found and contains **both** `setCallDetail(false)` and
   `setSessionDetail(false)`. Load-bearing here for the same reason it is in T1: assertion 2 below
   passes for free on an empty or un-extracted body.
2. the body does **not** contain `view ===` — the unconditional-reset property, and the negative that
   catches a re-added guard around either reset.

   This is a **raw-text** assertion, so the literal is banned from `show`'s body entirely, prose
   included: it cannot tell a re-added guard from a comment that quotes one. D3's `show` comment is
   worded around that on purpose ("a reset guarded by the tab's own name", never the comparison
   itself) — see D3, and do not reword it to spell the comparison out. A future comment that
   re-introduces the string fails this test: a false positive in principle, a loud one in practice,
   and cheaper than the comment-stripping regex it would take to avoid. `reveal`'s body *does*
   contain `b.dataset.view === view`, which is exactly why the assertion is scoped by
   `funcBody(js, "show")` and not run over the file.

Without assertion 2 a tab click can leave the user staring at a detail while the loader fetches a
list behind it — precisely the pre-F2.1 bug, with both literals present.

T3 also pins D10's single bump site: `reveal`'s body (extracted with `funcBody(js, "reveal")`, which
matches `function reveal(view)`) contains `detailSeq++`. Every tab click routes through `show` →
`reveal` and every `[data-call]` drill-down calls `reveal` directly, so a `detailSeq++` that drifts
out of `reveal` — or a token renamed away from `detailSeq` — would silently stop superseding stale
detail responses. The same vacuity guard applies (the body was found and contains `current =`),
because the assertion would otherwise pass for free on an un-extracted body.

### T4 — the suite

```
go build ./...
go vet ./...
go test ./...
go test ./internal/api/... ./internal/web/... -race
```

`-race` on `./internal/web/...` because the embedded-asset reads are one of the two concurrent
pairs this repo runs under it ([build-and-run.md:16](../../docs/context/build-and-run.md#L16)).

### T5 — recorded manual click-through

No E2E harness exists; `docs/acceptance.md` is the precedent for a recorded manual run, and this
story follows the practice rather than inventing a suite. **The run itself is recorded in §10 of
this plan** — see §4's last row for why that is the home rather than `docs/acceptance.md` or the
generated `docs/context/` tree. The runbook, to be executed against
`go run ./cmd/clens serve` at `http://127.0.0.1:8798` with a populated store:

| # | Step | Expected |
|---|---|---|
| 1 | Overview → click a call id | Only that call's detail; **no** list. DevTools Network shows `/api/requests/{id}` and **no** `/api/requests?...`. |
| 2 | Click `‹ all calls` | The Calls list appears, filters and pager intact. |
| 3 | Calls tab → click an id in the `id` column | The list is replaced by the detail. |
| 4 | Warnings tab → click a `call` id | Detail replaces the warnings view's switch target (Calls), same as step 1. |
| 5 | Sessions tab → click a session id | Session detail only, with `‹ all sessions`; back restores the sessions list. |
| 6 | With a session detail open (step 5), click a `call` id in its calls table (`app.js:221`) | The view switches to **Calls**, in detail mode for that call — the fourth `data-call` renderer, and `reveal('calls')` exercised from the Sessions view. |
| 7 | From **Overview**, turn `clens serve` off, then click a call id. (Starting state matters: on a **straight sequential run** `#calls-table` still holds the rows step 3 last loaded — nothing clears it, and `show('overview')` does not touch it; on a **fresh page load** it was never loaded.) | The view switches to **Calls**; the Calls view is revealed with its filter row intact and `#calls-table` showing **the rows step 3 last loaded** (sequential run) **or** an **empty** table (fresh load) — `loadCalls` is `#calls-table`'s only writer and writes nothing when `api()` rejects (`app.js:124-144`), so in neither case is the container left blank (`table()` would have cleared it only had `loadCalls` reached it), and the status line carries the error. |
| 8 | Open a call detail (step 1), then click the **Overview** tab, then click a **different** call id on Overview | The Calls view shows the **new** call — never the previous one, not even momentarily during the fetch. This is the tab-as-second-route (D5) and the runbook step that exercises F2.1's fix on the ordinary (non-racing) path — step 9 is the racing variant: leaving the first detail via the Overview tab runs `show('overview')`, whose unconditional reset clears both modes, so nothing from the first drill-down survives into the second. |
| 9 | Overview → click a call id, then **immediately** click the **Warnings** tab (while the detail fetch is still in flight), then click a **different** call id there | The new call's detail appears; the first call's request and response bodies are **never** shown — not even momentarily. This is the F4.1 window D10 closes: the Warnings tab click bumps `detailSeq`, so the first fetch's late resolution fails its `seq !== detailSeq` guard and is dropped rather than flipping a stale body into `#call-detail` for `reveal('calls')` to un-hide under the second click. |
| 10 | **Stale rejection — F5.1's `[data-call]` catch guard.** Against a store large enough that the detail fetch is not instantaneous, from **Overview** click a call id, then click the **Warnings** tab immediately (while the detail fetch is still in flight); let that in-flight request **fail** before it resolves (stop `clens serve` inside the window the slow fetch leaves open). | The view stays on **Warnings** — nothing pulls it back to Calls — and **no** status line appears for the abandoned click. The pre-F5.1 catch ran `await show('calls')` on **any** rejection, so a stale one snapped the view back to Calls and wrote the error; the `if (seq === detailSeq)` guard now drops it, so neither happens. **Amended during the recorded run (§10):** by *this* method the status line is never empty, because stopping `clens serve` also trips the SSE subscription's error handler; the observable the run asserts is the narrower "no status line **from the abandoned click**" — the abandoned call's `api()` failure would read `Failed to fetch`, and it does not appear. |

**Step 10's precondition is timing, stated rather than assumed.** The step exercises the catch guard
only if the request is still in flight when the **Warnings** tab is clicked (the large store widens
that window) *and* it then rejects. A request that resolves before the tab switch, or one that fails
before it, does not: the first is a live success (the mode flips and the click is over), the second
is step 7's live failure (the fallback legitimately runs), and a request that resolves *after* the
switch is D10's stale-**success** case, not this one — the same "nothing lands" observable, but not
the catch guard. So the step is **timing-dependent and therefore best-effort**, in the same voice as
this section's stated verification ceiling: a real manual run attempts it, and a run that cannot land
inside the window records that rather than claiming the guard was exercised.

**The 404 path is not reachable through the UI.** Every id a drill-down offers came from a row that
existed at render time, so a click always resolves to a live row — there is no UI action that
produces a 404, and manufacturing one (deleting a row out from under the store mid-run) is not
something a user does. It is covered where it can be: `TestGetRequestNotFound`
([api_test.go:152](../../internal/api/api_test.go#L152)) proves the endpoint's 404 at the API layer,
and the D4 handler's failure branch is the shared path every `api()` rejection takes. It is
deliberately **not** a runbook step — naming the gap beats inventing a step that tests a state the
UI cannot enter.

---

## 6. Risk areas

| | Risk | Mitigation |
|---|---|---|
| **R1** | The static guard is regex over asset text; a later refactor could satisfy it vacuously or defeat it by renaming. | The T1 found-and-non-trivial pre-assertion, plus the file's existing floor idiom. A renamed helper fails the test, which is the right outcome — it forces the guard to be updated with the code. |
| **R2** | A *future happy-path* caller using `show('calls')` for a drill-down silently reintroduces this exact bug — it fetches the list. (The D4 failure fallback calls `show('calls')` deliberately — it *wants* the list — so the rule the code must keep is "the happy path uses `reveal`, never `show`", not "never call `show`".) | Commented at the function and the handler (D3/D4/D5); T1 assertion 4 pins the happy path. The calling contract is written down, not implied. |
| **R3** | Hiding the list removes the pager and filters from view while a detail is open; a user could think their filters were lost. | `callState` is module-level and the filter inputs live inside the hidden div, so values and page survive (D1). Step 2 of T5 verifies it. |
| **R4** | The scroll behaviour after a 50-row table collapses is browser-dependent and cannot be verified in this repo. And the `scroll-margin-top: 72px` offset (D7) is the **single-row** `.app-header` height: when the header wraps (`flex-wrap: wrap`, `style.css:53-63`) to two rows it is ~85px+, so a detail scrolled to the top can still sit under the sticky header and hide its `‹ all calls` control. Accepted ceiling, not solved here. | `scrollIntoView` + `scroll-margin-top` (D7) make the outcome explicit rather than relying on the browser's scroll clamp. The offset is the measured single-row header height; deriving it from the header at runtime (JS `getBoundingClientRect` into a CSS custom property) is machinery this ~30-line fix does not justify, and the tool is a loopback dev dashboard, not a responsive public page. Isolated to one line if it proves unnecessary. |
| **R5** | A new `$('calls-list')` lookup with no mount is a silent no-op that throws on `undefined.hidden`. | `TestAssetsEveryLookupHasAMount` already covers every new id; T1 additionally asserts the mounts exist. |
| **R6** | Scope creep — "while we're in here, make the whole row clickable". | Explicitly out of scope (D9); `dashboard.md` is corrected to describe the code instead. |
| **R7** | A tab click during an in-flight `showCall` is not cancelled, and two in-flight `showCall`s render in resolution order. Round 4 (**F4.1**) showed the first window **could** surface a stale body: the late `setCallDetail(true)` from a still-pending fetch, landing *after* `show`'s reset, flipped the calls mode back to `detail` with the previous call's bodies, and `reveal('calls')` (no reset) then un-hid them under a click for a different call. Round-4 repro: click call A on Overview, tab to Warnings while the fetch is in flight, click call B on Warnings — B's click showed A's request and response until B's fetch resolved. | **Closed by D10**, both halves. The generation token is bumped in `reveal` (one site covers every tab click and every `[data-call]` drill-down) and taken directly by `[data-session]`, and `showCall` / `showSession` drop a response whose `seq` is no longer current — so window (a) and the out-of-order pair (b) both resolve to "the newest click wins". What D10 does **not** do is abort the in-flight request: the fetch still runs to completion, only its result is discarded, so the ceiling that remains is a wasted loopback response, not a visible artifact. This is the request-sequence token v4 named as its upgrade path, built (the three rounds of hand-tracing that each declared this race closed are the argument for the structural fix over a fourth reading). |

---

## 7. Self-review

### As a senior engineer

The fix is in the one place all four callers route through, so there is no sibling left broken. The
two-mode design uses the mechanism the dashboard already has (toggle `hidden`, re-render a
container) rather than introducing a router, a store, or a component — consistent with the
no-build-step rule the assets live under. `reveal`/`show` is the minimum split that lets the
drill-down skip a fetch; the alternative of a `loaders` entry for a detail-only view would have
needed a no-op loader and a special case in the SSE handler.

The one piece of parallel state the design carries — the `detailSeq` generation token (D10) — is a
deliberate exception to D2, not a breach of it: D2 keeps the *mode* out of a boolean because the DOM
already holds it, but whether a response still describes what the user asked for is not on screen
and cannot be derived from the `hidden` flags (D10 names the distinction). It is forced by F4.1, the
window three rounds of reading each wrongly declared closed.

The main "is this simpler?" challenge — scroll-only, three lines — was considered and rejected on
the report's own wording (§3 D1), with the reasoning recorded rather than left implicit.

**Residual coupling worth naming:** the mode is encoded in two `hidden` flags, so a future
container that should be part of "the list" must be added inside `#calls-list` or it will stay
visible in detail mode. The wrapper div makes that structural rather than a list of ids to
maintain, which is the best available answer without a framework.

### As a QA engineer

Happy paths for all four id renderers — Overview, Calls, Warnings, and the call id *inside* a
session detail (`app.js:221`) — plus the back control and the tab-as-second-route are in T5 (the
tab-as-second-route is step 8, which exits a call detail via the Overview tab and drills down again,
proving F2.1's unconditional reset; step 9 is the sharper F4.1 case — a tab click *while the detail
fetch is in flight* — which only D10's generation token passes; step 6 is the cross-drill-down, the
one path where the session detail is left open-but-hidden, D9/F4.2). The 404
path is **not** in T5: it is not reachable through the UI, so it is covered at the API layer
(`TestGetRequestNotFound`) plus the shared `api()` failure path the D4 fallback already exercises.
The edge cases that would bite a naive implementation are covered as code specs, not prose:
flipping the mode before the fetch resolves (T2, D4), dropping a stale detail fetch (T2, D10), the
negative assertion that could pass vacuously (T1), and the property, not the literals, that the tab
click resets (T3). Failure behaviour
is fail-open and observable: the Calls view is revealed with the
status line carrying the error and never a stale detail (the fallback's `show('calls')` resets both
modes, D3), and the list it lands on is whatever `loadCalls` can produce — the real rows when the
server answers, an empty table when it is the server that is down.

The honest gap: **no automated behaviour test**, because the repo has no JS harness and adding one
is a bigger change than the fix. T5 is manual and must actually be run and recorded; a plan that
claimed otherwise would be overselling its verification.

### As a security engineer

No new network surface, no new route, no change to the Origin/Host guard, no authz path. The
change is client-side string building in a file whose rule is "`esc()` wraps every interpolated
value" — and it *adds* an `esc()` that was missing on `e.ID` (D6), moving that line toward the rule
rather than away from it. The only new interpolation is the back control's markup, which contains
no data. No credential, no PII, no new fetch target: the detail request already exists and is
already loopback-bound.

---

## 8. Out of scope

- Making the whole row a click target (`dashboard.md`'s stale "click a row" is corrected in the doc
  instead).
- Deep links / URL routing for a call (`dashboard.md:29` — tab switching is deliberately not URL
  routing; hash routing is a design change, not a bug fix).
- Patching the SSE refresh to skip the hidden list fetch (D9).
- Any change to `/api/requests/{id}`, the store, the schema, or the replay flow.
- **Fixing the replay button's async flow — a known sibling of the F5.1 class, not fixed here.** The
  replay listener (`internal/web/app.js:179-190`) runs `setStatus('replayed as id ' + r.body.ID,
  false)` (`app.js:185`) and `await loadCalls()` (`app.js:186`) after its `await api(... POST ...)`,
  and nothing invalidates them if the user navigates away mid-replay. It is the SAME class as **F5.1**
  — a stale post-`await` effect acted on after navigation — but a materially SMALLER blast radius:
  neither effect touches `detailSeq` or either mode flag, and every write `loadCalls` makes lands
  inside the wrapped `#calls-list` — `#calls-table` (`table()`, `app.js:127-138`) plus the pager
  (`#calls-range` / `#calls-prev` / `#calls-next`, `app.js:141-143`) — so the residual harm is a
  stale status line and a redundant write to a possibly-hidden **list**, never a mode flip or a
  corrupted detail. **Pre-existing and unchanged by this
  story** — the plan does not touch the replay flow, and the mode work does not make it newly harmful.
- "Back" through browser history, or restoring the scroll position of the list on return.

---

## 9. Bead sketch (Phase 3 formalises this)

| # | Bead | Depends on |
|---|---|---|
| 01 | `index.html`: wrap both lists, add `hidden` to both detail containers | — |
| 02 | `app.js`: `reveal`/`show` split + `setCallDetail`/`setSessionDetail` + the `detailSeq` token bumped in `reveal` (D2, D3, D5, D10) | 01 |
| 03 | `app.js`: `showCall`/`showSession` enter detail mode (each dropping a stale `seq`, D10), back controls, `scrollIntoView`, `esc(e.ID)` (D4, D6, D7, D10) | 02 |
| 04 | `app.js`: the `[data-call]` branch calls `reveal('calls')` and reads its generation; the `[data-session]` branch takes one directly — the defect itself (D3, D4, D10) | 02 |
| 05 | `style.css`: `scroll-margin-top` (D7) | 03 |
| 06 | `assets_test.go`: T1–T3 wiring guard | 03, 04 |
| 07 | `docs/context/dashboard.md`: correct "click a row", document the two modes | 03, 04 |

Beads 01–04 are one logical change split only where a reviewer would want a separate checkpoint;
beads 06 and 07 are the guard and the docs. Whether 01–05 collapse into fewer beads is Phase 3's
call — the split above is a sketch, not a commitment.

**Phase 3 took that call: 01–05 landed as one bead (`br-GI-5-01`); 06 and 07 stayed separate, as
`br-GI-5-02` (the wiring guard) and `br-GI-5-03` (the docs and the recorded run). The table above is
the sketch, not the decomposition — `.beads/GI-5/` is the artifact of record.** The collapse went
further than "one logical change": §9's 02 ships primitives that are inert until 03's renderers use
them, and 01's `hidden` attributes leave the detail permanently invisible until 03 un-hides it — so
the sketch's halves are unsafe in *either* landing order, not merely unverifiable alone.

---

## 10. Recorded manual run (T5)

Run on 2026-09-19 against the fix as merged into `GI-5-call-detail-drilldown` at `039711d`. §5 T5
names this section as the run's home; §4's last row records why.

**What was actually run, and where it departs from §5's stated setup.** §5 T5 says `go run ./cmd/clens
serve` at `127.0.0.1:8798` with a populated store. A `clens serve` built *before* this story was
already listening on 8798 and serving the old embedded assets, so it could not exercise the fix. The
run therefore used a binary built from this branch, on `127.0.0.1:8799` with `-allow-remote=false`,
against a **copy** of that populated store (`lens.db`, 39.5 MB, `X-Total-Count: 81006`) — the copy
because the live instance holds SQLite's single write connection. Same binary, same store, same
`api()` and asset bytes; only the port and the file differ. The runbook's ten steps are otherwise
followed verbatim.

**How it was driven.** No browser automation exists in this repo (§5 T5), but that is a statement
about *this repo*, not about the machine: the run drove the real dashboard in **headless Edge
153.0.4234.32** over the Chrome DevTools Protocol, from a throwaway Node script outside the tree.
Each step is a real DOM `.click()` on a real link rendered from the real store, and the step-1
network observation is CDP's own `Network.requestWillBeSent` stream, not an inference. The
observations below are what the browser did; the harness that drove it is not committed, and
`internal/web/assets_test.go` remains the only automated guard the repo ships.

| # | Result | What was observed |
|---|---|---|
| 1 | **PASS** | Network: exactly `["/api/requests/81006"]`. See below. |
| 2 | **PASS** | `‹ all calls` issued one list fetch; the list reappeared with its filter row (`f-source`, `f-model`, `f-billing`) and pager (`calls-prev`, `calls-next`) intact, range `1-50 of 81006`. R3 holds. |
| 3 | **PASS** | Calls tab → the `id` cell of the first row: list replaced by the detail, only `/api/requests/81006` issued. |
| 4 | **PASS** | Warnings tab → a `call` id (72797): the view switched to Calls and the detail showed 72797, with no list fetch. |
| 5 | **PASS** | Sessions tab → a session id: session detail only, `‹ all sessions` rendered, sessions list hidden, **no** view switch (D8), no `#calls-list` fetch. The detail named `Session s_1789818336907_d8530832` and carried its calls table. |
| 6 | **PASS** | From that open detail, a call id in its calls table (79682): view switched to **Calls** in detail mode, no list fetch — the fourth `data-call` renderer, and `reveal('calls')` exercised from the Sessions view. |
| 7 | **PASS** | See below — the sequential-run starting state reproduced exactly. |
| 8 | **PASS** | Detail open for 81006 → **Overview** tab → the detail closed and the list was unhidden (D5) → clicking a *different* call (81007) rendered 81007, never 81006, not even transiently. |
| 9 | **PASS** | The racing window **was** observed — but only with instrumentation. See below. |
| 10 | **PASS** | Landed inside its timing window, with one amendment to the runbook's stated observable. See below. |

**Step 1, in full** — the reported defect, at the wire:

```
Network observed: ["http://127.0.0.1:8799/api/requests/81006"]
```

One request, the detail endpoint. No `/api/requests?…` follows it. The pre-fix signature recorded in
§2.1 was the reverse order — `["/api/requests?limit=50&offset=0","/api/requests/42"]` — the list
fetch that made the click "abruptly fetch the list of calls". Alongside the wire observation: the
detail opened, `#calls-list` was hidden, the view switched to Calls, the tab strip marked Calls
active, the heading read `Call 81006`, and the status line stayed empty.

**Step 7 — the sequential-run state.** The step's own text says the outcome differs between a
straight sequential run and a fresh load, so the run reproduced the sequential state deliberately:
the Calls tab was visited first, loading `#calls-table` to 10,283 chars of rows, then Overview, then
the server was stopped (verified dead: `still-listening=0`), then a call id was clicked. Observed:

```
{"views":["calls"],"active":["calls"],"detailOpen":false,"callsListHidden":false,
 "callsTableLen":10283,"status":"Failed to fetch","statusErr":"error"}
```

The view switched to Calls as §5 T5 predicts, the filter row was intact, the detail did not open,
and `#calls-table` still held **exactly** the 10,283 chars the Calls visit had loaded — not blanked.
The status line carried the error, in the error style. This is the D4 fallback working, and it is
also the run's **positive control** for step 10 (below): this is what the code does when an
`api()` rejection is *live*.

**Step 9 — the window was landed in, and the honest measurement of how.** §5 T5's step 9 asks for the
tab to be clicked while the detail fetch is still in flight. Measured on this store, the detail fetch
takes **2.7–3.4 ms** (six samples; the list fetch takes ~5.1 ms) — a ~3 ms window, which a human
cannot hit and a script cannot hit reliably. The run therefore *held* the fetch at the CDP request
stage, asserting explicitly that it was still in flight at the moment the Warnings tab was clicked,
then clicked a different call id there, then released the held request as a **success**. Observed:
the second call's detail rendered, the first call's bodies never appeared, the view was Calls (the
newest click), and the status line was empty. So the window was genuinely exercised — D10's
stale-**success** half — but by instrumentation, not by timing. A run that reported "the window was
observed" without saying this would be overstating it.

**Step 10 — landed inside the window, both variants, one amendment.** §5 T5's precondition paragraph
allows this step to record a miss rather than claim the guard. It did not have to: the run landed
inside the window, twice, by two methods.

*Variant (a) — the runbook's literal method.* From Overview, click a call id, hold its fetch in
flight, click the **Warnings** tab, then stop `clens serve` inside the window (verified dead), then
release the held request so it fails against a dead server. Observed: the view stayed on **Warnings**,
the tab strip stayed on Warnings, the abandoned call never rendered — and the **status line was not
empty**. It read `live updates disconnected — retrying…`.

**That is the SSE subscription's own error handler, not the catch guard.** Stopping `clens serve`
trips the `EventSource` independently of the detail fetch, and `subscribe()` writes its own message.
So §5 T5's expected observable — "**no** status line appears" — is **not literally achievable by the
method that step prescribes**; the server being down is precisely what makes the status line
non-empty. The assertion has to be the narrower *"no status line **from the abandoned click**"*, and
that is how it was applied: the abandoned call's `api()` failure would have read `Failed to fetch`
(step 7's exact text, same code path), and it does not appear. Recorded as an amendment to §5 T5
step 10 rather than smoothed over — the step's *intent* is unambiguous and met, but its stated
observable, read literally, is unmeetable.

*Variant (b) — isolated.* Same in-flight-then-stale timing, but the held request was failed via CDP
`Fetch.failRequest` with the server left **up**, so SSE kept its connection and the catch guard was
the only code that could write to the status line. Observed: the view stayed on Warnings, no detail
rendered, and the status line was **empty — nothing wrote at all**. This is the clean test of F5.1.

*Why the two together mean the guard works.* Variant (b) alone shows "a stale rejection writes
nothing", which would also be true if the catch did nothing ever. Step 7 supplies the discriminating
case in the same instrument: the *same* `api()` rejection on the *live* path did switch the view to
Calls and did write `Failed to fetch`. Same app, same failure mode, no generation bump in between —
the guard is the only thing that differs, and it is what changes the outcome.

**Beyond the runbook — R4's ceiling is real and was measured.** §6 R4 accepts that
`scroll-margin-top: 72px` is the *single-row* header height and that a wrapped header can still
cover the `‹ all calls` control. Forced to scroll (400px-tall viewport), measured across widths:

| Viewport | `.app-header` height | `#call-back` top | Covered by the sticky header? |
|---|---|---|---|
| 600 px | 138 px | 72 px | **yes** |
| 800–1200 px | 101 px | 72 px | **yes** |
| ≥ 1400 px | 58 px | 72 px | no |

The control lands at `top: 72` in every case — `scrollIntoView` is doing exactly what it is told;
the constant is simply the wrong one once the header wraps. So R4's accepted ceiling is confirmed,
and its trigger is narrower than "sometimes": **the control is hidden whenever the header wraps to
two or more rows (≲1200 px on this machine's font stack) and the document scrolls.** At ≥1400 px,
D7's offset works as designed. Left as the documented ceiling: the fix for it is the runtime
`getBoundingClientRect` → custom-property machinery R4 declines to add for a loopback dev
dashboard, and it is one line if it ever matters.

**What this run does not cover.** It is a click-through of the shipped assets, not a regression
suite: nothing here re-runs on a future change, which is why T1–T3 exist alongside it. The 404 path
is absent for the reason §5 T5 gives (no UI action produces one). The two racing windows, if ever
exercised again, will need the same instrumentation or a slow enough store to land in naturally.

---

## Change History

### v1 — initial plan (2026-09-19)

Derived from the Phase 1 intake on GI#5. Diagnosis settled empirically by driving the committed
`app.js` against a throwaway DOM harness rather than by reading alone (§2.1) — which is what
established that there is no thrown exception and that the detail does render, moving the fix from
"repair the drill-down" to "the detail has nowhere visible to render".

### v2 — round-1 cross-review applied (2026-09-19)

- **F1.1 (MINOR, tests) — scoped, not global.** T1's `[data-call]` guard now names its extraction: a
  slice of `app.js` between the anchors `const call = ev.target.closest('[data-call]')` and
  `} catch`, with the vacuity guard asserted **first** (slice found and contains `showCall(` and
  `dataset.call`) and the three content assertions ordered after it (contains `reveal('calls')`;
  does not contain `loadCalls(`; does not contain `show('calls')`). The reviewer's suggested
  `!strings.Contains(js, "show('calls')")` **global** form is invalid and not adopted: F1.2
  deliberately reintroduces `show('calls')` on the failure path, so the assertion must be scoped,
  and slicing to `} catch` is exactly what exempts that fallback.
- **F1.2 (MINOR, edge-case) — strengthened, code and spec.** The `[data-call]` handler's failure
  path now falls back to `await show('calls')` — the list it already revealed — so a failed
  drill-down lands on a *loaded list*, not an empty panel. D4 rewritten with the handler and the
  true success/failure behaviour; T5 step 6 rewritten to match: the tab **switches**, the list view
  is shown, the error lands in the status line, and the error line must follow `show` (whose
  `reveal` runs `setStatus('')`). The reviewer's "a previously open detail stays on screen" premise
  is **not** the design: it is unreachable (`#call-detail` is un-hidden only after a successful
  `showCall`, and while it is un-hidden `#calls-list` is hidden, so no `data-call` link inside it is
  clickable; every other route back to a link is another tab, which runs `show()` →
  `setCallDetail(false)`). **Retracted in v3 (F2.1): this reachability argument is false.** It held
  only for the `calls`/`sessions` tabs; a click to Overview or Warnings ran no reset (the guard was
  `view === 'calls'`), and both of those render their own `data-call` links, so a stale detail *was*
  reachable. D3's caller enumeration, D5, D9 and R2 updated for the new `show('calls')` caller.
- **F1.3 (MINOR, correctness) — minimal fix.** `scroll-margin-top: 72px` kept; the CSS comment now
  states it is the single-row `.app-header` height and that a wrapped header overlaps the control,
  and R4 records that as an accepted ceiling. No CSS-variable or JS-measurement machinery.
- **F1.4 (NIT, correctness).** Two anchors corrected: `style.css:60` → `:61` (§3 D7),
  `build-and-run.md:15` → `:16` (§5 T4).

### v3 — round-2 cross-review applied (2026-09-19)

- **F2.1 (MINOR, edge-case) — JUSTIFIED, applied in the unconditional form (conductor override).**
  D3's `show` now drops both `if (view === …)` guards and resets both modes on **every** tab click.
  This retracts the v2/F1.2 reachability claim (above): the stale-detail state *was* reachable
  through Overview and Warnings, which run no reset under the guard and render their own `data-call`
  links (`app.js:593`, `app.js:260`), so a drill-down from either would flash the previous call's
  bodies under a new click. D3, D4, D5, D9 and R2 corrected so nothing relies on the retracted
  claim; D5 now names the cross-drill-down (a call id clicked *inside* the session detail,
  `app.js:221`) as the case the unconditional reset cleans up.
- **F2.2 (MINOR, edge-case) — JUSTIFIED, applied in the narrowed form (conductor override).** The
  D4 handler now resets at its entry point (`reveal('calls'); setCallDetail(false);`) so a drill-down
  starts from the list by construction rather than depending on `show`'s last action — the reviewer's
  request-sequence token is **not** added. The residual races (a tab click during an in-flight
  `showCall`; two out-of-order in-flight `showCall`s) are recorded as accepted ceiling **R7**, with
  the token named as the upgrade path.
- **F2.3 (MINOR, tests) — JUSTIFIED.** §7 corrected to claim only what T5 performs; T5 gains a step
  (now step 6) that clicks a call id *inside* a session detail (`app.js:221`, the fourth renderer);
  §5 now states plainly that the 404 path is not UI-reachable and is covered only by
  `TestGetRequestNotFound` plus the shared failure path — no manufactured-404 step.
- **F2.4 (NIT, tests) — JUSTIFIED.** The server-down step (now step 7) names its starting tab
  (Overview) and states what the code actually produces: the Calls view with its filter row and an
  empty `#calls-table` (`loadCalls` is `#calls-table`'s only writer and writes nothing when `api()`
  rejects, `app.js:124-144`), the error in the status line, no stale detail. D4's failure prose and
  §7's residual "the list stays" phrasing corrected to match.
- **Informational (no plan edit needed).** Round-1's `changelog.md` mis-cites the real file (it
  names `show('calls')` at the tab handler / startup path; those are `show(tab.dataset.view)`
  `app.js:637` and `show('overview')` `app.js:740`, neither containing the literal). Not adopted as
  evidence — the F1.1 conclusion rests on the extraction argument alone — and no plan text leaned on
  that sentence, so nothing was changed for it.

### v4 — round-3 cross-review applied (2026-09-19)

All five findings applied under the author's conductor overrides; no finding rejected. The v3/F2.2
entry-point reset is **deleted** this round (see F3.3), so every live reference to it was re-scanned
and corrected.

- **F3.1 (MINOR, correctness) — JUSTIFIED.** D9's SSE-refresh bullet claimed "a detail is only ever
  open while `current` is `'calls'`". That is **false for the session detail**:
  `showSession` is reached from the `[data-session]` branch (`app.js:649-654`), which calls neither
  `show` nor `reveal`, so `#session-detail` is open with `current === 'sessions'`. D9 now states both
  halves (call detail open only while `current === 'calls'`; session detail only while
  `'sessions'`), and drops the "D4 handler sets it on entry" phrase for the precise `reveal('calls')`
  sets `current`. The conclusion — leave the SSE refresh alone, because `loaders[current]()` writes
  the hidden list either way — is unchanged and still holds.
- **F3.2 (MINOR, tests) — JUSTIFIED.** T3 was a pure presence check and would stay green through the
  exact F2.1 regression it exists to catch (the pre-F2.1 guarded body contains both literals). T3 now
  extracts `show`'s body with `funcBody(js, "show")` and asserts **in order**: (1) the body was found
  and contains both resets (the vacuity guard, load-bearing for the same reason as T1's), then (2)
  the body does **not** contain `view ===` — the unconditional-reset property and the negative that
  catches a re-added guard.
- **F3.3 (MINOR, tests) — conductor override, applied by DELETION.** The v3/F2.2 entry-point
  `setCallDetail(false)` in the `[data-call]` branch is **removed**; the reviewer's suggested T1
  assertion for it is **not** added (the line no longer exists). With F2.1's unconditional reset in
  `show`, the line could not change behaviour — every route to a clickable `data-call` link passes
  through a tab click first, while a call detail is open its own list's id links are hidden, and a
  second click landing mid-`showCall` already finds the mode `'list'`. The happy path is now exactly
  `reveal('calls'); try { await showCall(call.dataset.call); } catch (err) { await show('calls');
  setStatus(err.message, true); }`, with a comment at `reveal` stating the mode is whatever the last
  `show` reset it to and why that is safe. Follow-on corrections: D3's "resets its own mode
  explicitly" sentence corrected; D4 success prose now attributes "no stale body" to `show`'s reset;
  T5 step 7 reworded (it starts from Overview with nothing open, so it never exercised the deleted
  line — its real content, the revealed filter row, empty `#calls-table` and error in the status
  line, is kept); **R7** rewritten so its "no stale content" half is attributed to F2.1's
  unconditional reset in `show`, leaving only the genuine residual (a tab click during an in-flight
  `showCall` is not cancelled; two in-flight `showCall`s render in resolution order) with the
  request-sequence token kept as the named upgrade path; R2's rule restated as "the happy path uses
  `reveal`, never `show`".
- **F3.4 (MINOR, tests) — JUSTIFIED, applied as a runbook STEP (conductor override).** The
  tab-as-second-route is the only thing that verifies F2.1's fix, since the F2.1 defect is exactly "a
  previous call's bodies reappear after leaving via a tab that does not reset". T5 gains **step 8**:
  open a call detail, click the **Overview** tab, click a different call id there, and confirm the
  Calls view shows the **new** call — never the previous one, not even momentarily. §7 corrected to
  tie the tab-as-second-route claim to that step, so it now claims only coverage T5 performs.
- **F3.5 (NIT, tests) — JUSTIFIED.** T5 step 4's Step cell was self-contradictory ("Calls tab → click
  a `call` id on the Warnings tab"); corrected to "Warnings tab → click a `call` id", matching the
  source → action format of the other steps and the Expected column.

### v5 — round-4 cross-review applied (2026-09-19)

Two findings, both confirmed and applied. F4.1 is the round that finally settled a race that **three
rounds of hand-tracing had each declared closed** — round 1's F1.2 "unreachable", round 3's F3.3
"the entry reset could never change behaviour", and now F4.1's counter-example — which is itself the
argument for a structural fix rather than a fourth round of prose. Applied via the generation token
below (conductor override, Option B), **not** the reviewer's Option A.

- **F4.1 (MAJOR, correctness) — OVERRIDE, applied as the new decision D10 (generation token).** The
  round-3 F3.3 override is **retracted**: its premise ("the entry reset could never change behaviour")
  was false, and the reviewer's counter-example is real — a tab click *between* two drill-downs lets
  an in-flight `showCall`'s late `setCallDetail(true)` land *after* `show`'s reset, so `reveal('calls')`
  un-hides a stale body under a click for a different call until the new fetch resolves (F2.1's harm).
  Rather than reinstate the entry reset (two mechanisms for one property), the race is closed
  structurally: `let detailSeq = 0`; `reveal` bumps it as its first statement (one site covers every
  tab click and every `[data-call]` drill-down); the `[data-session]` branch bumps it directly (it
  calls neither `show` nor `reveal`); `[data-call]` reads it back after `reveal`; and
  `showCall`/`showSession` drop a result whose `seq !== detailSeq` immediately after the fetch. D10
  states why this does not contradict D2: the token says *whether a response still describes what was
  asked for* — not derivable from anything on screen — whereas the mode is *which container is on
  screen*, which the `hidden` flags already hold. **D3's and D4's now-false justifications are
  corrected**: "the mode is always `list` at a drill-down's start" / "could never change behaviour"
  are marked false (the reason there is no entry reset is now D10, not impossibility), and D4's
  handler comment, success prose, and `showCall` snippet carry the guard. **R7 rewritten, not
  deleted**, to record that D10 closes both halves — the F4.1 tab-click window and the out-of-order
  pair — naming the round-4 repro, leaving only that the in-flight request is not aborted, only
  discarded. Test specs folded into existing numbers: `reveal`'s body contains `detailSeq++` (T3);
  `showCall`/`showSession` bodies contain `seq !== detailSeq` **after** the `await api(...)` line,
  the same index-order idiom as T2; T5 gains a step 9 reproducing F4.1; T1's slice assertion
  confirmed unaffected (the happy path still has no `loadCalls(` and no `show('calls')`).
- **F4.2 (MINOR, correctness) — OVERRIDE, applied.** D9 said "the session detail only while
  `current === 'sessions'`", which the cross-drill-down contradicts: clicking a call id *inside* a
  session detail runs `reveal('calls')`, so `current === 'calls'` while `#session-detail` stays
  un-hidden (D5). Softened to "the session detail is **visible** only while `current === 'sessions'`",
  naming the cross-drill-down as the one case where it stays open-but-hidden. D9's conclusion — leave
  the SSE refresh alone — is unchanged: on that path `current === 'calls'`, so SSE calls `loadCalls`,
  which writes only the already-hidden `#calls-table`.

### v6 — round-5 cross-review applied (2026-09-19)

Applied under the author's conductor overrides: **F5.1 (both catches)**, **N1**, **N2**, **N3**, plus
the **§8 addition**. No finding rejected.

- **F5.1 (MINOR, correctness) — OVERRIDE, applied, both catches.** The generation guard sat in the
  success path only, so a *stale, failed* drill-down still acted: `[data-call]`'s catch ran
  `await show('calls')`, which bumps `detailSeq` (dropping a newer in-flight fetch by its own guard),
  resets both modes (wiping a newer rendered detail), and switches the view the user has deliberately
  left. Both catches now carry `if (seq === detailSeq)` — around `await show('calls'); setStatus(...)`
  in `[data-call]`, around `setStatus(...)` in `[data-session]`. D4's failure prose corrected ("the
  catch runs `show('calls')` **only if the failure is still current**"). D10 gains the general rule:
  **nothing a stale fetch produces, value or error, is acted on** — the guard is symmetric across both
  halves (success **and** failure), because a fetch can end either way; the two success guards and the
  two `catch` guards are its instances. R7's "only its result is discarded / not a visible artifact"
  wording — false for a rejection before this round — is now true and left unchanged.
- **N1 (non-blocking note) — applied.** `detailSeq++` folded into D3's `reveal` snippet as its first
  statement, with a one-line comment, so D3 is the function's actual shape. D10's duplicate `reveal`
  body snippet is removed and its prose now points at D3's shape, so D10 amends the rationale rather
  than re-declaring the body.
- **N2 (non-blocking note) — applied.** The `[data-call]` branch was shown two ways (D4 multi-line,
  D10 single-line); harmonized to D4's multi-line form in both places. The literal `} catch` anchor
  T1's slice ends on is preserved — it now closes the multi-line `try` — and the slice still contains
  `reveal('calls')` + `showCall(` + `dataset.call` and neither `loadCalls(` nor `show('calls')`, with
  the fallback's `show('calls')` after the anchor. T1's assertions are unaffected.
- **N3 (non-blocking note) — applied.** T5 step 7's Expected assumed a fresh load. The step now states
  the starting assumption explicitly: on a straight sequential run `#calls-table` still holds the rows
  step 3 last loaded (nothing clears it, and `show('overview')` does not touch it); on a fresh load it
  was never loaded. Either way the container is never left blank — it shows the previously loaded rows
  or an empty table — with the filter row intact and the error in the status line.
- **§8 addition (conductor-traced) — added.** §8 now names the replay button's async flow
  (`app.js:179-190`) as a known sibling of the F5.1 class, out of scope here: it runs
  `setStatus('replayed as id ' + r.body.ID, false)` (`app.js:185`) and `await loadCalls()`
  (`app.js:186`) after its `await api(... POST ...)`, with nothing invalidating them on navigation. It
  is the same class — a stale post-`await` effect acted on after navigation — but a materially smaller
  blast radius: neither effect touches `detailSeq` or a mode flag, and `loadCalls` writes only
  `#calls-table` (`app.js:124-144`), so the residual is a stale status line and a redundant write to a
  possibly-hidden table, never a mode flip or a corrupted detail. Pre-existing and unchanged by this
  story — named, not fixed.

### v6.1 — Phase 3/4 correction: the plan's code failed the plan's own test (2026-09-19)

Not a review round — a defect found while polishing the beads Phase 3 derived from this plan, and
found in **this document**, not in a bead. Three changes, no design change of any kind.

- **D3's `show` comment contradicted §5 T3.** Both existed since v3 (the comment) and v4 (T3's
  negative, F3.2), and no round caught that they cannot both hold: T3 asserts `show`'s body does not
  contain the literal `view ===`, and D3's comment spelled the comparison out inside that very body.
  An implementer copying D3 would have written code that fails T3 — a guard failing on *correct*
  code, which is the one failure mode the review spent six rounds trying to eliminate in the other
  direction. The comment now says "a reset guarded by the tab's own name", which carries the same
  reason without the banned literal, and both D3 and T3 now state the constraint and why it exists
  (a raw-text assertion cannot distinguish a re-added guard from a comment quoting one; the
  alternative is comment-stripping regex no other test in the file has). `reveal`'s
  `b.dataset.view === view` is unaffected — T3 is scoped by `funcBody(js, "show")`, which is what
  the scoping was for.
- **§4's "Files changed" contract omitted T5's home.** §5 T5 cites `docs/acceptance.md` as the
  precedent for a recorded manual run and says this story follows it, but §4 listed only
  `docs/context/dashboard.md` for documentation and never named where the run is written — the one
  place Phase 3's bead author had to make a call this plan did not cover. Resolved in favour of a
  new **§10 of this plan**, with §4 gaining the row and §5 T5 naming it: `docs/acceptance.md` is the
  precedent for the *practice*, but its header scopes it to br-GI-1-19's acceptance run and its
  Local-half / Live-half shape is a whole-product run, and `docs/context/` is generated so it cannot
  hold a hand-written runbook at all.
- **The §9 bead sketch's split was replaced, not edited.** Phase 3 collapsed §9's beads 01–05 into
  one bead (its 02's primitives are inert without 03's renderers, and 01's `hidden` attributes make
  the detail permanently invisible until 03 un-hides it — unsafe in *either* landing order, which is
  a stronger argument than §9's own "one logical change"). §9 said this was Phase 3's call
  explicitly, so nothing here was contradicted. Recorded because §9's table now describes a split
  that does not exist; the beads are the artifact of record.
