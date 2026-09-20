# Bead br-GI-7-05: The proxy-mode badge — configured *and* observed, because the disagreement is the signal

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D5, §2.6 (the mode signal that exists but is informational only), §4 (`store.go`, `cli/serve.go`, `api/mode.go` (new), `index.html`, `app.js`, `style.css`), §5 T7, §6 (the mispointed-badge row), §9 bead 05

- **Bead ID**: br-GI-7-05
- **Priority**: P0 (critical)
- **Original Estimate**: 2.5h
- **Dependencies**: br-GI-7-01
- **Blocks**: None

## Description

This is the failure that produced the story. On this machine `clens serve` was healthy, listening and
capturing; Claude Code was pointed at a different product on a different port. The dashboard looked
fine and showed only transcript rows, and the user had to run `clens doctor` and read a PASS line to
discover the proxy was being bypassed. Nothing in the dashboard said so.

### What "mode" means — two independent facts

| | Question | Source |
|---|---|---|
| **Configured** | Does Claude Code's `ANTHROPIC_BASE_URL` point at this process's `ProxyAddr`? **Three-valued** — match, mismatch, or **unknown** when `settings.json` has no `ANTHROPIC_BASE_URL` at all. | `readSettingsBaseURL` (`doctor.go:252-266`) |
| **Observed** | Has a proxy-sourced row arrived recently? | a store read of the newest `events.started_at` where `source='proxy'` |

Neither half alone catches the incident: a configured-only indicator says "pointed correctly" while
the proxy is dead; an observed-only indicator says "capturing" while the client is pointed elsewhere.
The interesting state is the **disagreement**, and the real incident is exactly
`configured ✗ / observed ✗`.

**The observed source does not exist today.** `/api/sources` reports three collectors and never
`proxy`: `internal/ingest/ingest.go:42-46` defines only `jsonl`/`snapshot`/`admin`, `:184` iterates
exactly those, and `:9-12` states the proxy is *deliberately not one of `RunOnce`'s collectors*. The
consumer's `LastWriteAt` on `/api/health` (`api.go:622`) is **not** acceptable: it counts any source,
so a transcript-only install would read as "receiving".

### `internal/store/store.go` — the newest proxy row (D5)

One read, exposed as a store method, e.g.:

```go
// LatestProxyStartedAt returns the newest source='proxy' row's started_at, or
// the zero time when no proxy row has ever been written.
func (s *Store) LatestProxyStartedAt(ctx context.Context) (time.Time, error)
```

`SELECT MAX(started_at) FROM events WHERE source = 'proxy'`, converted through the existing
`timeFromNano`. This is not a `Store`-interface addition for `internal/api` — the seam below is what
calls it, from the composition root.

### `internal/api/mode.go` (new) — the seam, the route, and the label mapping (F4.2)

```go
// ProxyConfigured is the three-valued configured half. Unknown is a real
// state, not "mismatch": settings.json can legitimately carry no
// ANTHROPIC_BASE_URL (the printed-banner onboarding path).
type ProxyConfigured int

const (
    ProxyConfiguredMatch ProxyConfigured = iota
    ProxyConfiguredMismatch
    ProxyConfiguredUnknown
)

type ProxyMode struct {
    Configured ProxyConfigured `json:"Configured"`
    Observed   bool            `json:"Observed"`
    // Badge is the rendered label. It is computed here, server-side, so the
    // six-row grid below is a pure Go function a test can assert; app.js has
    // no runtime in this toolchain.
    Badge string `json:"Badge"`
}

// SetProxyMode wires GET /api/mode. Unset is supported: the route answers 503.
func (a *api) SetProxyMode(fn func(ctx context.Context) (ProxyMode, error)) { a.proxyMode = fn }
```

Declare a `proxyMode` field on the `api` struct beside `sourceHealth`/`accounts`, and register the
route with `methodGet` in `New` (e.g. `mux.HandleFunc("/api/mode", methodGet(a.mode))`). The handler
calls the seam, fills `Badge` from the `(Configured, Observed)` pair, and encodes the `ProxyMode`. An
unwired seam answers **503**, exactly as `SetSourceHealth`/`SetAccounts` do — an empty badge and a
broken one look identical in a UI.

The state → label grid, verbatim:

| Configured | Observed | Badge |
|---|---|---|
| match | ✓ | `proxy: active` |
| match | ✗ | `proxy: configured, not receiving` |
| mismatch | ✓ | `proxy: receiving, client elsewhere` |
| mismatch | ✗ | `proxy: off` |
| unknown | ✓ | `proxy: receiving — base URL not set in settings.json` |
| unknown | ✗ | `proxy: not receiving — base URL not set in settings.json` |

**`client elsewhere` is reserved for a definite mismatch.** A user who followed the `clens serve`
banner runs `export ANTHROPIC_BASE_URL=http://127.0.0.1:8797` (`serve.go:335`), which `settings.json`
never sees; that case is `unknown` and must never read *"client elsewhere"*.

### `internal/cli/serve.go` — the composition root builds the seam (F1.13)

`internal/api` cannot reach `readSettingsBaseURL`: not because a guard forbids `internal/cli`
(`importguard_test.go:31-38` bans `secret`, `config`, `ingest` — not `cli`), but because the edge is a
**build cycle** — `serve.go:14` already imports `internal/api`. So the settings read is injected, the
same **shape** as `SetSourceHealth` (`api.go:140`). Beside the other read seams:

```go
dashAPI.SetProxyMode(func(ctx context.Context) (api.ProxyMode, error) {
    at, err := st.LatestProxyStartedAt(ctx)
    if err != nil {
        return api.ProxyMode{}, err
    }
    base, ok := readSettingsBaseURL(filepath.Join(claudeConfigDir(), "settings.json"))
    return api.ProxyMode{
        Configured: configuredAgainst(base, ok, cfg.ProxyAddr),
        Observed:   !at.IsZero() && time.Since(at) <= proxyRecentWindow,
    }, nil
})
```

- `proxyRecentWindow = 5 * time.Minute` — an unexported `internal/cli` constant. Without a stated
  window the "receiving" state has no boundary and is untestable.
- `claudeConfigDir()` and `readSettingsBaseURL` already live in `internal/cli/doctor.go`; no change
  to either.

**The comparison is normalized, or it fails on the tool's own output (F1.7).** `serve.go:335` prints
`http://127.0.0.1:8797` while `cfg.ProxyAddr` is the bare `127.0.0.1:8797` (`config.go:54`) and
`readSettingsBaseURL` returns the raw settings string with no parsing. A string equality check would
report the bad state for a correctly-pointed client — the one thing D5 exists to get right. Add an
unexported `internal/cli` helper:

```go
// configuredAgainst normalizes both sides before comparing: strip a leading
// scheme, strip any path and trailing slash, normalize the host "localhost" to
// "127.0.0.1", then compare host:port. A bare host with no port is a mismatch --
// the port is the signal.
func configuredAgainst(settings string, ok bool, proxyAddr string) api.ProxyConfigured
```

`readSettingsBaseURL` returns `("", false)` for an absent/unparseable `settings.json` **or** an absent
`ANTHROPIC_BASE_URL`; that is `ProxyConfiguredUnknown`, not a mismatch.

### `internal/web/index.html` and `app.js` — the badge lives in the header, on every tab

- `index.html`: one element in `<header class="app-header">` with an id, e.g.
  `<span class="proxy-badge" id="proxy-mode"></span>`, so the answer does not depend on the user
  knowing to look at the Sources tab.
- `app.js`: a `loadProxyMode()` that fetches `/api/mode` and writes the **server-supplied**
  `Badge` label into that element, with no state → label logic of its own (the mapping is in
  `mode.go`). On a fetch failure it clears the badge and stays silent — fail open, like `loadTotals`.
  Call it once at startup and on each SSE event, alongside `loadTotals()`.
- `style.css`: badge styling.

## Rationale

`doctor`'s `client_config` check already reads the same settings file and reports the value — but its
own doc comment says *"It can only PASS"*, and it never compares the value against `cfg.ProxyAddr`, so
the one condition a user needs (client pointed somewhere else) reads as `[PASS]`. `doctor` is a CLI
invocation; the dashboard is where a user actually sits, so the check is surfaced where it is looked
at. Whether `doctor` should also stop reporting `[PASS]` unconditionally is a separate, smaller
question, explicitly out of scope (§8).

Putting the state → label mapping in Go rather than `app.js` is what makes it testable at all:
`app.js` has no runtime in this toolchain, so a mapping there could be asserted only by a source-shape
guard, and the browser's only job becomes printing the string it is handed.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `GET /api/mode` returns the `ProxyMode` shape with the `Badge` label for every one of the six
  `(Configured, Observed)` pairs.
- A `settings.json` with no `ANTHROPIC_BASE_URL` reads `unknown`; its labels say *"not set in
  settings.json"* and **never** *"client elsewhere"*.
- The `match` fixture is the exact string `clens serve` prints (`http://` + `ProxyAddr`), so the
  normalization is exercised on the tool's own output.
- A proxy row older than `proxyRecentWindow` reads as not receiving.
- The badge is present in the dashboard header on every tab and renders the server-supplied label.
- An unwired `SetProxyMode` answers 503, never a nil dereference.

## Test Specifications

- Unit Tests (`internal/api/mode_test.go` — T7, six behavioural cases over the seam):
  - For each of the six `(Configured, Observed)` pairs, inject a fixture seam returning that pair,
    fetch the mode route, and assert the exact `ProxyMode.Badge` string — the state → label mapping is
    server-side, so this is a behavioural Go test, not an assertion against a string nothing produces.
  - The two `unknown` rows assert the label reads `not set in settings.json` and **never** *"client
    elsewhere"*.
  - `TestModeRouteUnwiredReturns503`.
- Unit Tests (`internal/cli` — the normalization and the window):
  - `TestProxyModeNormalizesServeBannerString`: the fixture base URL is the exact string
    `clens serve` prints, `http://` + `ProxyAddr`, and it reads `ProxyConfiguredMatch` — not mismatch.
  - `TestProxyModeBaseURLSpellings`: `https://127.0.0.1:8797`, a trailing slash, and a trailing path
    all match; `localhost:8797` matches `127.0.0.1:8797`; a bare host with no port is a mismatch; an
    absent value is `Unknown`.
  - `TestProxyModeObservedWindow`: over a temp store, a `source='proxy'` row newer than
    `proxyRecentWindow` reads observed, one older reads not-receiving, and a store with no proxy row
    reads not-receiving.
- Unit Tests (`internal/web/assets_test.go` — the badge's mount and the fail-open):
  - `TestAssetsTheBadgeHasAMount`: `index.html` defines the badge id and `app.js` looks it up — the
    existing `TestAssetsEveryLookupHasAMount` covers the lookup mechanically, so this asserts the
    element is in the **header**, on every tab.
- Integration Tests: the mode route through `New(...)` with a fixture seam, in the `internal/api`
  harness.
- E2E: none.

## Files to Touch

- `internal/store/store.go` (modify — `LatestProxyStartedAt`)
- `internal/store/store_test.go` (modify — the store read's cases)
- `internal/api/mode.go` (create — `ProxyMode`, `ProxyConfigured`, `SetProxyMode`, the mode route, the
  six-row label grid)
- `internal/api/api.go` (modify — the `proxyMode` field; register `/api/mode` with `methodGet`)
- `internal/api/mode_test.go` (create — T7's six cases and the unwired-seam case)
- `internal/cli/serve.go` (modify — `proxyRecentWindow`, `configuredAgainst`, the `SetProxyMode`
  closure)
- `internal/cli/serve_test.go` (modify — the normalization and the window cases)
- `internal/web/index.html` (modify — the badge element in the header)
- `internal/web/app.js` (modify — `loadProxyMode`, wired into startup and the SSE tick)
- `internal/web/style.css` (modify — badge styles)
- `internal/web/assets_test.go` (modify — the badge's mount)
