[← INDEX](INDEX.md)

# Dashboard

*(Not a module from the skill's standard catalogue — this repo has no npm frontend, so no
`package.json` trigger fires. It is documented anyway because 838 lines of hand-written JavaScript
with a hard no-build-step rule and a no-CDN rule is exactly what a future change would break.)*

The dashboard is served by the **dashboard listener** (`127.0.0.1:8798`) from
[internal/web](../../internal/web/), embedded into the binary with `go:embed`
([internal/web/embed.go](../../internal/web/embed.go)) and mounted at `/` by the API mux
([internal/api/api.go:194](../../internal/api/api.go#L194)).

## The rule that shapes everything

**There is no build step.** The HTML, CSS, and JS are committed as-is and embedded; nothing is
fetched at runtime — not a font, not a charting library, not a CDN script. See
[decisions/006](decisions/006-dashboard-with-no-build-step.md). A change that requires a bundler,
a transpile step, or a `<script src="https://…">` is a design change, not a refactor.

| File | Lines | Role |
|---|---|---|
| [index.html](../../internal/web/index.html) | 142 | the shell: header totals, the tab nav, one `<section class="view">` per tab |
| [app.js](../../internal/web/app.js) | 838 | every fetch, every table render, the three SVG charts, and the one SSE subscription |
| [style.css](../../internal/web/style.css) | 192 | |

## Route table

Tab switching is **not** URL routing — no hash, no history, no deep links. A tab click toggles
`hidden` on the matching `<section id="view-…">` and calls that view's loader.

| Tab (`data-view`) | Shows | Loader → API route |
|---|---|---|
| `overview` | totals and the most recent calls | `/api/requests` |
| `calls` | the call log with filters. Only the **id** cell is a link — there is no row handler, so clicking elsewhere in the row does nothing (D9). The link replaces the list with that call's full request and response, under a `‹ all calls` control | `/api/requests`, `/api/requests/{id}` |
| `sessions` | one row per run, with **both** cost models labelled side by side. The **session** cell is a link: it replaces the sessions list with that session's detail, its own calls table included, under a `‹ all sessions` control, and switches no tab | `/api/sessions`, `/api/sessions/{id}` |
| `warnings` | findings by kind, and one row per occurrence | `/api/warnings`, `/api/warnings/summary` |
| `stats` | totals over a window, charted by day/week/month | `/api/stats` |
| `sources` | every collector's last success, last error, rows written | `/api/sources` |
| `quota` | per-account burn against each window, and candidate limits | `/api/quota` |
| `reconcile` | computed vs billed cost, side by side per day and model | `/api/reconcile` |
| `models` | the catalogue, plus every model traffic used, priced or not | `/api/models` |
| `settings` | health, the rate table, and its edit path | `/api/health`, `/api/prices` |

## State management

There is no store, no framework, and no virtual DOM. The pattern is uniform:

1. A tab click calls the view's `load*()` function.
2. That function `fetch`es its route and calls `table(container, cols, rows, emptyLabel, rowClass)`.
3. `table` builds an HTML string and assigns `container.innerHTML`.

Two conventions inside that, both deliberate:

- **`esc()` wraps every interpolated value.** The renderers concatenate HTML strings, so escaping is
  manual and load-bearing — an unescaped model name or warning detail is an injection.
- **An absent figure renders as `--`, never as `0`.** `fmtUSD` and `fmtPct` return `'--'` for a
  null/undefined value — a null cost means "no row of that billing mode", which is *not* `$0.00`. A
  genuine zero still renders as `$0.00`; it is the *missing* one that must not be invented. This is
  the cost model's second rule ([cost-and-quota.md](cost-and-quota.md)) expressed in the UI — and
  the header carries **two labelled totals, `api` and `sub`, never one sum**.

### Calls and Sessions are each two modes

Both tabs are a **list** or a **detail** — never both, never neither. The mode *is* the two
elements' `hidden` flags, toggled as a pair by `setCallDetail(on)` / `setSessionDetail(on)`; there
is no parallel boolean that could drift out of sync with what is on screen (D2). The list side of
each is a wrapper (`#calls-list`, `#sessions-list`) rather than the table alone, so the filter row
and the pager hide with it.

- **`reveal(view)` and `show(view)` are not interchangeable.** `reveal` switches the tab and takes
  the next generation, and **fetches nothing**; `show` is `reveal` plus a reset of both details plus
  the tab's loader. A **tab click** goes through `show`; a **drill-down** goes through `reveal`, so a
  call-id click issues `/api/requests/{id}` and *no* list fetch (D3/D4). A happy-path caller that
  reached for `show('calls')` would refetch the very list it is replacing.
- **The reset is unconditional** — `show` clears both details whichever tab was clicked, not only
  the tab that owns the open one (D5). A detail carries its own `[data-call]` links, and Overview
  renders them too, so a reset guarded by the tab's own name would let the previous call's bodies be
  revealed by the next drill-down.
- **A session drill-down switches no view** (D8): Sessions is already on screen, so the detail
  replaces the sessions list in place.
- **`detailSeq` is a generation token**, taken by `reveal` and by each drill-down, and compared after
  each detail fetch resolves (`showCall`, `showSession`) and at the top of both `catch` branches. A
  response whose generation is no longer current is dropped rather than rendered, success and
  failure alike — a stale *rejection* would otherwise run the fail-open fallback and snap the view
  back to a tab the user had already left (D10). One effect is deliberately unguarded, so do not
  read the guard as total: in the `[data-call]` catch, `setStatus` runs *after* `await show('calls')`
  and nothing re-checks the generation there — it cannot, because `show` bumps `detailSeq` itself,
  so a re-check would always fail and the fallback would never report its error at all. The plan's
  D10 records that ceiling, its width, and why it self-corrects. The token is a deliberate exception
  to the derive-don't-track rule above: the mode still derives from `hidden`, but *which* pending
  response is allowed to set it needs a sequence.

## The three charts

Hand-rolled inline SVG — no charting library:

| Function | Renders |
|---|---|
| `chartByPeriod(periods)` | the Stats tab's time series |
| `chartQuota(windows)` | quota burn against each window |
| `chartReconcile(rows)` | computed vs billed, side by side |

## Live updates

One SSE subscription, opened by `subscribe()` against `GET /api/stream`, drives the Overview and
Calls tabs as new rows land. The broker behind it **drops a slow subscriber rather than blocking**
(`TestBrokerDropsSlowSubscriberRatherThanBlocking`) — a stalled browser tab must not be able to
apply backpressure to the capture path.

Write routes are reached through the same-origin guard; the dashboard is the only browser origin
that satisfies it. See [api-surface.md](api-surface.md).

## Assets are tested

[internal/web/assets_test.go](../../internal/web/assets_test.go) is the guard on the no-build-step
rule — with no bundler to resolve references, these are the checks that catch a renamed id or a
dropped mount point:

| Test | Asserts |
|---|---|
| `TestAssetsEveryLookupHasAMount` | every `$('…')` id `app.js` looks up has a matching `id=` somewhere in the assets. It also fails if the pattern matched suspiciously few lookups, so the check cannot silently stop matching. |
| `TestAssetsTheFourNewTabsHaveTheirMountPoints` | for each tab: a `data-view` button, a `<section id="view-…">`, the container its loader writes into, and a registered loader function |
| `TestAssetsChartsAreInlineSVG` | no `<img>`/`<canvas>` in the chart code; each chart function still exists and emits `<svg>`; every drawn shape carries a `<title>` (so a bar has a label to hover or read); and `style.css` still styles `svg.chart` |
| `TestAssetsTheCallDetailReplacesTheList` | the two-mode wiring above: both list wrappers exist, both details are declared `hidden`, each setter is two-sided, each detail renders its own back control, and the `[data-call]` branch of the delegated click handler reveals Calls **without** fetching a list. That last assertion is the reported defect, and it is invisible to every other check in this file — the pre-fix `app.js` passed all of them. Its siblings `TestAssetsTheDetailModeFlipFollowsTheFetch` and `TestAssetsShowResetsBothModesUnconditionally` pin the two orderings a refactor would silently reverse (mode flip after the fetch resolves; both resets unconditional) and that a stale response is dropped |

A tab that renders a permanently blank panel, or a chart that loses its accessible labels, fails
here rather than in a browser.
