# Bead br-GI-1-16: Dashboard JSON API, SSE broker, embedded web assets

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §API surface and dashboard, §Invariant 5, §Security posture, tests 12b, 18

- **Bead ID**: br-GI-1-16
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-02, br-GI-1-03, br-GI-1-06, br-GI-1-07, br-GI-1-08
- **Blocks**: br-GI-1-17, br-GI-1-18

> **Dependency note.** The plan lists br-GI-1-06 only. This bead **also needs 02, 03, 07, 08**, because
> `internal/api` imports what deepseek-lens's does: `consumer` (the live stats on `/api/health`),
> `proxy` (`ReplayMeta` / `WithReplay` on the replay route), `replay` (the body-edit helpers the replay
> route calls), `sink` (the drop counter), `store` (all reads), and `pricing` (`/api/prices`). It also
> **creates `internal/replay` here**, not in br-GI-1-17 — the route and the package land together, as
> they did in deepseek-lens, and that is what keeps 16 from depending on 17 while 17 depends on 16.
> Every added edge points to a lower number. See the summary flag.

## Description

The read surface and the live feed. This bead carries over all 15 deepseek-lens routes; br-GI-1-18
adds the eight new ones on top.

**`internal/api`.**

- `api.New(st Store, sk *sink.Sink, cons *consumer.Consumer, broker *Broker, assets fs.FS, proxyHandler http.Handler, replayEnabled bool) *api`, imported as in deepseek-lens. `proxyHandler` is the live proxy's own Handler: the replay route re-issues through it rather than through a transport of its own, so a replay uses the same transport, tee and consumer writer as every other call.
- **`internal/replay` lands here** — `ParseSets`/`ApplyEdits`/`MarshalEdits`/`Result`/`OutcomeOf`, plus `proxy.ReplayMeta`/`proxy.WithReplay` handling. The replay *route* and its body-edit package are one deliverable, which is why this bead depends on br-GI-1-03; br-GI-1-17's CLI `replay` command consumes this package.

- All 15 carried-over routes, including the three pagination headers (`X-Total-Count`, `X-Limit`,
  `X-Offset`) with `?limit`/`?offset`, the **unpaginated** `GET /api/warnings/summary`, the SSE
  `GET /api/stream`, and `GET /api/health`.
- **SSE live push + broker + `PublishingStore` decorator**, carried over verbatim: the consumer's
  writes also publish SSE events. The decorator wraps the store at the composition root
  (br-GI-1-17) rather than changing `consumer`, because `consumer.New`'s Store parameter is already a
  narrow interface anything satisfies structurally. Read endpoints use the **bare** store, not the
  decorated one, so a read can never trigger a publish.
- **One kept contract is deliberately restated, not silently changed**: `GET /api/sessions` no longer
  returns a single per-session `total_cost_usd`. It returns `total_cost_usd` (API-billed) **plus**
  `total_api_equivalent_cost_usd` (subscription), each **NULL** when that side is empty, with
  `unpriced_count` alongside — so the route can never return a figure that mixes billing models or
  `$0.00` for a subscription or unpriced session (invariant 5). Update the response type and the
  dashboard's Sessions rendering for the two fields.
- **Every aggregate route splits on `billing_mode`.** `/api/stats` groups or filters by it, and a
  mixed fixture returns **two labelled totals**, never one `SUM(cost_usd)` (test 12b). The schema
  already guarantees `events.cost_usd` cannot hold a subscription figure, but the count/token
  aggregates are a second place the two models could be blended, so they carry the split explicitly.
- **Injected function-value seams, not imports.** `internal/api` and `internal/web` **never import
  `internal/secret`** (nor `config`/`ingest` for writes). A write route reaches the write through a
  seam. **All four seam setters are declared here**, because br-GI-1-17 (the composition root) wires
  them and 17 precedes br-GI-1-18, which only *adds the routes that call them*:
  - `SetPricing` → `pricing.Save` (carried over; used by `POST /api/prices`).
  - `SetCredentialWriter(fn func(name, value string) error)` → `secret.Save`.
  - `SetAccountWriter(fn func(...) error)` → config/accounts save.
  - `SetIngestTrigger(fn func(ctx) error)` → `ingest.RunOnce`.

  **An unwired seam returns `503`**, exactly as deepseek-lens's `POST /api/prices` does without
  `SetPricing`. Defining the handlers' seams here rather than in br-GI-1-18 is what keeps the DAG free
  of a 17→18 edge: a seam's *setter* is a package-level capability with no route attached, so it can
  ship a bead before the route that consumes it.
- **Origin/Host allowlist on every write route** — the shared `replayOriginReject`-shaped guard
  (br-GI-1-03). Write routes carry it; read routes do not.
- Replay route wiring exists here, with `internal/replay` created alongside it; the opt-in
  (`--replay`) gate and the cost gate are br-GI-1-17 (the CLI path).

**`internal/web`** — `go:embed`'d `index.html`, `app.js`, `style.css`. **Vanilla HTML/JS, no
framework, no `package.json`, no build step, no CDN.** Charts are hand-rolled inline SVG (the real
charts land in br-GI-1-18). This bead ships the shell plus the tabs that have data from br-GI-1-06:
Overview, Calls, Sessions, Warnings, Stats, Settings.

**Credential containment (test 18).** The web and API layers can report *whether* a credential is
present and when it last worked, and cannot return its value. No route reads `secret`.

## Rationale

The dashboard is the deliverable; the JSON API is its only data source. Keeping the SSE publishing as
a store decorator means the consumer's tests (which assert exact warning counts) stay untouched, and
keeping write routes behind injected seams is what makes the "credentials never reach the API layer"
guarantee a mechanical import-direction property rather than a review habit.

## Outcome Definition

- `go test ./internal/api/... -race` passes.
- All 15 carried-over routes respond; the three pagination headers are present and correct.
- `/api/warnings/summary` is unpaginated; `/api/stream` delivers an SSE event on a new row.
- `GET /api/sessions` returns the two labelled totals, each NULL when empty (test 12b).
- A mixed-fixture `/api/stats` returns two labelled totals, never one merged sum (test 12b).
- An unwired write seam returns `503`.
- A write route without a valid Origin/Host is rejected.
- `internal/api` and `internal/web` do not import `internal/secret` (import-guard test).
- The embedded assets are served with no external network fetch.

## Test Specifications

- Unit Tests (`internal/api/api_test.go`):
  - Each carried-over route returns 200 with expected shape against a fixture store.
  - **Pagination**: `?limit`/`?offset` and the three headers (test from GI-15's suite, ported).
  - `/api/warnings/summary` ignores `?limit` (unpaginated by design).
  - `POST /api/prices` without `SetPricing` → `503`; with it → writes.
  - All four seam setters compile and are assignable with no route attached; leaving one unset is a
    supported state, not a nil dereference (the routes that consume them are br-GI-1-18's).
  - A write route with a foreign Origin → rejected; a valid one → allowed.
  - `GET /api/health` returns the expected fields.
- Unit Tests (`internal/api/api_test.go`, billing):
  - **Test 12b (route half)**: a mixed fixture through `/api/stats` → two labelled totals; through
    `/api/sessions` → the two labelled session totals, NULL-not-`$0.00` for the empty side.
- Unit Tests (`internal/api/broker_test.go`):
  - A publish reaches a connected SSE client; a client disconnect does not leak a goroutine.
  - A read (bare store) does not publish.
- Unit Tests (import guard): `TestAPIWebDoNotImportSecret` parses imports and fails otherwise.
- Integration Tests: `PublishingStore` publishes on a consumer write (wired in br-GI-1-17).
- Unit Tests (`internal/replay/replay_test.go`): `ApplyEdits` rewrites a stored body; a malformed
  `--set` fails before anything is sent; `OutcomeOf` folds a row's warnings into the result.
- E2E: assets served by the running server return 200 and contain no external URL.

## Files to Touch

- `internal/api/api.go`, `internal/api/api_test.go` (create)
- `internal/api/broker.go`, `internal/api/broker_test.go` (create)
- `internal/api/publishing_store.go` (create)
- `internal/api/prices.go`, `internal/api/prices_test.go` (create)
- `internal/api/pagination_test.go` (create)
- `internal/replay/replay.go`, `internal/replay/replay_test.go` (create — the body-edit and re-issue
  helpers the replay route calls; br-GI-1-17's CLI `replay` command consumes this package)
- `internal/web/embed.go`, `internal/web/index.html`, `internal/web/app.js`, `internal/web/style.css` (create)
