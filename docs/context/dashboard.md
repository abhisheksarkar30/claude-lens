[← INDEX](INDEX.md)

# Dashboard

*(Not a module from the skill's standard catalogue — this repo has no npm frontend, so no
`package.json` trigger fires. It is documented anyway because 741 lines of hand-written JavaScript
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
| [index.html](../../internal/web/index.html) | 133 | the shell: header totals, the tab nav, one `<section class="view">` per tab |
| [app.js](../../internal/web/app.js) | 741 | every fetch, every table render, the three SVG charts, and the one SSE subscription |
| [style.css](../../internal/web/style.css) | 186 | |

## Route table

Tab switching is **not** URL routing — no hash, no history, no deep links. A tab click toggles
`hidden` on the matching `<section id="view-…">` and calls that view's loader.

| Tab (`data-view`) | Shows | Loader → API route |
|---|---|---|
| `overview` | totals and the most recent calls | `/api/requests` |
| `calls` | the call log with filters; click a row for the full request and response | `/api/requests`, `/api/requests/{id}` |
| `sessions` | one row per run, with **both** cost models labelled side by side | `/api/sessions`, `/api/sessions/{id}` |
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

A tab that renders a permanently blank panel, or a chart that loses its accessible labels, fails
here rather than in a browser.
