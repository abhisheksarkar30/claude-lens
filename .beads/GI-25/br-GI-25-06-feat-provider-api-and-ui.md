# Bead br-GI-25-06: `/api/provider`, header toggle, local cost display and filter

**Plan Reference**: `docs/planning/GI-25-local-llm-toggle-and-store-hardening.md` v8, §2 R1, §3.4 (column-keyed `web/app.js`/`index.html` rows), §3.5, §4.1, §5.2 (`TestProviderEndpointRejectsNonLoopbackAndForeignOrigin`), §6 R1, Change History v7 (bead 06's purpose cell naming `app.js` cost display/summary tiles and `index.html`'s billing-filter option) and v8's carried three small API tests (`/api/provider` happy path, nil-seam 503, bad-value status code). Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-25-06
- **Priority**: P0
- **Status**: pending
- **Original Estimate**: 2h
- **Dependencies**: br-GI-25-05
- **Blocks**: None
- **Commit**: `GI#25 feat: the dashboard provider toggle and local cost display (br-GI-25-06)`

## Description

### `internal/api/provider.go` (new file)

```go
type ProviderSeam struct {
	Get func() (active, cloudHost, localHost string, localConfigured bool)
	Set func(active string) error
}

func (a *api) SetProvider(seam ProviderSeam) { a.provider = seam }
```

Follows the existing injected-seam pattern (`SetReload`, `internal/api/reload.go:18-20`), not a new shape: nil seam answers 503, exactly like `reload`'s `a.reloadFunc == nil` branch (`reload.go:36-39`) and `shutdown`'s `a.shutdownFunc == nil` branch (`internal/api/shutdown.go:45-48`). `internal/api/provider.go` must not import `internal/config` (enforced by `TestAPIWebImportGuard`, `internal/api/importguard_test.go:36`) — `ProviderSeam.Get`'s return shape is deliberately four plain values (two strings for the resolved display names, a bool, and the active label), not a `config.Config` or a `proxy.Targets`, so the seam carries no type the forbidden-import guard would catch.

**`GET /api/provider`** (read-only, registered with `methodGet` beside `/api/mode` and friends, `internal/api/api.go:205-216`):
```go
func (a *api) getProvider(w http.ResponseWriter, r *http.Request) {
	if a.provider.Get == nil {
		writeError(w, http.StatusServiceUnavailable, "provider is unavailable: no provider reader is wired")
		return
	}
	active, cloudHost, localHost, localConfigured := a.provider.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"active": active, "cloud_url_host": cloudHost, "local_url_host": localHost, "local_configured": localConfigured,
	})
}
```
`cloud_url_host`/`local_url_host` are hostnames only (e.g. `api.anthropic.com`, `127.0.0.1:1234`), not full URLs with scheme — the dashboard displays where traffic is headed without echoing a full upstream URL verbatim into a page a browser renders. The composition root's `Get` closure (wired in `serve.go`, below) resolves these from the already-parsed `*url.URL`s `proxy.Targets` holds (`u.Host` on each), so `internal/api` needs no URL parsing of its own.

**`POST /api/provider`** — guarded **exactly like `reload`/`shutdown`**: `originReject` for the DNS-rebinding case, plus an explicit loopback-caller check, because switching providers redirects where the caller's credentials are sent (plan §6 R1):
```go
func (a *api) setProvider(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "provider"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackHost(host) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("provider requires a loopback caller, got %q", r.RemoteAddr))
		return
	}
	if a.provider.Set == nil {
		writeError(w, http.StatusServiceUnavailable, "provider is unavailable: no provider writer is wired")
		return
	}
	var body struct{ Active string `json:"active"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := a.provider.Set(body.Active); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": body.Active})
}
```
A bad `active` value (anything other than `"cloud"`/`"local"`, or `"local"` with no local URL configured) is rejected by `targets.Set` itself (br-GI-25-05) and surfaced here as `400`, not `403`/`503` — the caller and origin are fine, the *value* is wrong. This is the "bad-value status code" case the plan's v8 change history carries into this bead.

**Registration** (`internal/api/api.go:211-221`, beside the other reads/writes in that block):
```go
mux.HandleFunc("/api/provider", methodGet(a.getProvider))
mux.HandleFunc("POST /api/provider", a.setProvider)
```
The loopback-guarded route count in `docs/context/api-surface.md`/`security-and-permissions.md` goes from two (`shutdown`, `reload`) to three (`shutdown`, `reload`, `provider`) — tracked as a context-doc staleness item, not edited by this bead (plan §11; do not edit `docs/context/`).

**`internal/cli/serve.go` wiring** (completing the line br-GI-25-05 deferred): after `dashAPI := api.New(...)` (`serve.go:130`), alongside the other `dashAPI.SetXxx(...)` calls (`serve.go:135-145`):
```go
hostOf := func(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
dashAPI.SetProvider(api.ProviderSeam{
	Get: func() (string, string, string, bool) {
		return targets.Active(), hostOf(cfg.UpstreamURL), hostOf(cfg.LocalUpstreamURL), cfg.LocalUpstreamURL != ""
	},
	Set: targets.Set,
})
```
`cloud_url_host`/`local_url_host` are bare hosts (`url.Parse(...).Host`), never the raw config strings. Add the `net/url` import to `serve.go` if it is not already there. If `cfg` is reloadable (`liveFields`), read the current value inside the closure rather than capturing a stale copy.

### Dashboard UI — `internal/web/index.html` and `internal/web/app.js`

- **Header toggle.** A segmented control `cloud | local` beside `#proxy-mode` (`index.html:25`) — e.g. `<div class="provider-toggle" id="provider-toggle"><button data-provider="cloud">cloud</button><button data-provider="local">local</button></div>`. A muted/disabled state on the `local` button when `local_configured` is `false` (CSS class toggle, `internal/web/style.css`). Vanilla JS, no build step (ADR 006) — follow the existing `loadProxyMode`/`$('proxy-mode')` pattern (`app.js:1134-1156`) exactly: a `loadProvider()` function that `GET /api/provider`s and renders the active state into `#provider-toggle` (e.g. an `active` class on the matching button), and a click handler on each button that `POST /api/provider`s `{"active": "cloud"|"local"}` via the existing `api()` helper (`app.js:53-69`, the same one `src-collect`'s click handler at `app.js:1103-1117` uses), then re-runs `loadProvider()` (and `loadTotals()`, since the active provider can change which rows future calls contribute to). A `403` from a non-loopback dashboard session (the listener can bind `0.0.0.0`, R1) should render as a status-line error via the existing `setStatus(err.message, true)` pattern (`app.js:1112`), not a silent no-op.
- **Billing filter.** `#f-billing` (`index.html:57-61`) gains a third `<option value="local">local</option>` beside the existing `api`/`subscription` options — no JS change needed beyond this, since the filter already round-trips its raw value into `EventFilter.BillingMode` (`internal/store/types.go:186`), which already accepts any string including `"local"` with no code change required there.
- **Local cost display.** One `fmtCost(e)` helper (new, consolidating what today are per-site inline `fmtUSD` calls) used at the **row-level** sites `app.js:429,570,966` and the **summary** sites `app.js:552,581,652,659,663,956,1128` (plus the header total at `app.js:1127-1128`, which gains a third tile). Line numbers are from before this ticket; re-find them with `grep -n 'fmtUSD' internal/web/app.js` and convert every hit, so none is missed: render `local (proj) $x` when `e.BillingMode == "local"` (row-level) or when a summary object carries a non-null/non-zero `TotalLocalProjectedCostUSD` (summary-level), beside the existing `api`/`sub` figures — never replacing them, since a non-local row still has real `api`/`sub` figures to show. The summary sites all read `TotalApiEquivalentCostUSD` today (per plan §3.4's citation of each) and gain a parallel `TotalLocalProjectedCostUSD` read once `internal/store`'s `StatsSummary` carries that field (added in br-GI-25-04 §5) — `index.html:17-18`'s header totals span gains a third `<span>local <strong id="total-cost-local">$0.00</strong></span>`, populated in `loadTotals()` (`app.js:1121-1132`) from `s.TotalLocalProjectedCostUSD` the same way the existing two spans are populated.

### By-hand verification against the real LM Studio server

Once `GET /api/provider`/`POST /api/provider` and the header toggle are wired, **check by hand** (not an automated test — neither LM Studio nor a browser exists in CI, plan §5.3): start `clens serve` with `LocalUpstreamURL` pointed at the running LM Studio instance, open the dashboard, click the toggle to `local`, send one real Claude Code request, and confirm (a) the toggle visibly reflects the switch, (b) the request actually reaches LM Studio (check LM Studio's own logs or the response content), and (c) the resulting row in the Calls/Stats views shows `billing_mode = local` with a non-`$0.00`, non-blank `local (proj)` figure. Record the outcome in Review Notes — pass/fail and what was observed, the same discipline br-GI-25-05's three pre-flight checks use.

## Rationale

The toggle is the one feature in this ticket a user directly interacts with, and it is also the one place credentials get redirected to a different host by a dashboard click — hence the identical guard `reload`/`shutdown` already use, applied a third time rather than invented fresh. The cost-display pieces are mechanical once br-GI-25-04's `TotalLocalProjectedCostUSD`/`rowCosts` exist; this bead's job is wiring them into the one UI surface that was deliberately left untouched by br-GI-25-04 so the two beads' diffs do not collide on the same files.

## Outcome Definition

`go test ./internal/api/ -count=1 -run 'TestProvider|TestSetProvider|TestGetProvider'` passes, covering the happy path, the nil-seam 503, the non-loopback/forged-origin 403s, and the bad-value 400. `go build ./... && go vet ./...` clean. The by-hand LM Studio check above is recorded in Review Notes. `docs/context/` is not edited by this bead (plan §11 tracks the staleness separately).

## Test Specifications

- `internal/api/provider_test.go`: `TestGetProviderHappyPath` — **new, carried from plan v8's change history.** A wired `ProviderSeam.Get` returning `("cloud", "api.anthropic.com", "127.0.0.1:1234", true)` makes `GET /api/provider` return `200` with exactly those four values in the JSON body.
- `internal/api/provider_test.go`: `TestProviderWithoutASeamIsNotFatal` — **new, carried from plan v8's change history (the "nil-seam 503" case).** With no `SetProvider` wired, both `GET /api/provider` and `POST /api/provider` (past the origin/loopback guards) return `503`, not a panic on a nil func.
- `internal/api/provider_test.go`: `TestSetProviderRejectsBadValue` — **new, carried from plan v8's change history (the "bad-value status code" case).** A wired seam whose `Set` returns an error for an unrecognised `active` value (or `"local"` with no local URL) makes `POST /api/provider {"active":"bogus"}` return `400`, with the seam's error message in the body, not `403`/`503`.
- `internal/api/provider_test.go`: `TestProviderEndpointRejectsNonLoopbackAndForeignOrigin` — mirrors `TestShutdownRefusesANonLoopbackCaller`/`TestShutdownRefusesANonLoopbackHost` (`internal/api/api_test.go`, the shutdown route's own tests): a loopback `Host` header with a public `RemoteAddr` is refused (403, the forged-Host case); a public `Host` header with a loopback `RemoteAddr` is also refused (403, the DNS-rebinding case `originReject` covers) — both guards are asserted separately so they cannot be collapsed into one.
- `internal/api/provider_test.go`: `TestSetProviderIsNotReachableByGet` — mirrors `TestIngestIsNotReachableByGet`/`TestShutdownRefusesNonPost`: a `GET /api/provider` is served by `getProvider` (the `methodGet`-wrapped read route, registered separately from the POST write route under Go's exact-method `"POST /api/provider"` pattern) — not a 404, since unlike `shutdown`/`ingest` this path **does** have a GET sibling. Confirm the two registrations do not collide (the GET and the POST are the same path string with different methods, which `net/http`'s `ServeMux` since Go 1.22 dispatches correctly) by asserting both a `GET` and a `POST` reach their respective handlers in the same test.
- **Manual / recorded**: the by-hand LM Studio round-trip and dashboard-toggle check described above, recorded in Review Notes (what was sent, what came back, and the resulting row's `billing_mode`/cost figure) — neither LM Studio nor a browser exists in CI.

## Files to Touch

- `internal/api/provider.go` (create — `ProviderSeam`, `SetProvider`, `getProvider`, `setProvider`)
- `internal/api/provider_test.go` (create — the five tests above)
- `internal/api/api.go` (modify — route registration for `GET /api/provider` and `POST /api/provider`, `provider ProviderSeam` field on `api` struct)
- `internal/cli/serve.go` (modify — complete the `dashAPI.SetProvider(...)` wiring deferred from br-GI-25-05)
- `internal/web/index.html` (modify — header segmented control, `#f-billing`'s third option, header totals' third span)
- `internal/web/app.js` (modify — `loadProvider()`, toggle click handler, `fmtCost(e)` helper used at every cited row/summary site, `loadTotals()`'s third tile)
- `internal/web/style.css` (modify — the toggle's active/muted states)

## Review Notes

Implemented. `go build ./...`, `go vet ./...`, `go test ./...` green. Negative control: dropping the loopback-caller check fails `TestProviderEndpointRejectsNonLoopbackAndForeignOrigin`.

**By-hand check: partly done.** LM Studio at 192.168.0.185:1234 timed out when this bead was reached (it passed the pre-flight earlier), so the real LM Studio round-trip is NOT verified and still needs doing by the operator. What was run instead: the built binary, `clens serve` on spare ports with a throwaway HOME, a fake cloud upstream and a fake local upstream (both on loopback, each answering `/v1/messages` with a `msg_` id and usage of 1200 in / 300 out). Observed: `GET /api/provider` showed cloud active with both hosts and `local_configured: true`; a request through the proxy reached the cloud fake and became an `api` row ($0.0081 in `cost_usd`); `POST /api/provider {"active":"local"}` returned 200, wrote `provider` containing `local`, and the next request reached the local fake and became a `local` row (account `local`, `cost_usd` NULL, `$0.00036` in `api_equivalent_cost_usd`, model name `qwen3-local` kept). `{"active":"bogus"}` answered 400 with the Targets error; a forged `Host: evil.example` answered 403. In the dashboard (built-in browser): the toggle rendered the active side from the server, a click on cloud re-rendered it, the Calls table showed `--` under `sub $` and `$0.000360` under `local $ (proj)` for the local row, the header carried three separate totals (`api $0.008100`, `sub --`, `local (proj) $0.000360`), and the billing filter offered `local`. Not exercised: the 403 status-line message from a non-loopback browser session, and a restart with the saved choice (covered by `TestProviderPersistsAcrossRestart`).

Deviations from the bead text: (1) row-level cost cells use two small helpers, `subCost`/`localCost`, that split the shared api-equivalent column by `BillingMode`, and add a `local $ (proj)` column, instead of one `fmtCost(e)` that prints `local (proj) $x` inline; a table column keeps the figures separate and sortable by eye, and the `sub $` column no longer shows a projection. Summary sites read `TotalLocalProjectedCostUSD` directly. (2) The Stats by-model table notes `priced as deepseek-flash` beside a local model, using the `PriceModel` field bead 04 added. (3) The toggle is hidden until `/api/provider` answers, so a build without the seam shows no dead control. (4) The seam's hosts are computed once at boot: `LocalUpstreamURL` is not a live field, so a reload must not make the toggle name a host the proxy is not routing to. (5) The sessions list has no local column: a session's totals sum `api` and `subscription` rows only, and a per-session local total is a schema change this ticket does not make. Sessions that mix local calls therefore show only their api/sub figures; the session detail's call rows do show the local projection.

Context-doc staleness for the plan's section 11 list: the loopback-guarded route count (shutdown, reload) is now three with `/api/provider`.
