# Bead br-GI-1-18: API routes, Sources/Quota/Reconcile/Models tabs, hand-rolled charts

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §API surface and dashboard, §Invariant 5, §Security posture (re-auth), §Quota engine, §Reconciliation, §Risk areas, tests 12b, 17, 18

- **Bead ID**: br-GI-1-18
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-14, br-GI-1-15, br-GI-1-16
- **Blocks**: br-GI-1-19

## Description

The eight new routes and the four new dashboard tabs, laid over the packages the earlier beads built.
br-GI-1-16 shipped the carried-over routes, the broker, the embedded assets, and **all four seam
setters**; this bead adds only the routes that call them.

**Routes (five GET, three POST).**

| Method | Path | Guard | Reads / calls |
|---|---|---|---|
| GET | `/api/sources` | none | Per-source health from br-GI-1-14: last success, last error, rows written, cursor position, status, per source |
| GET | `/api/quota` | none | br-GI-1-12's quota engine: rolling windows, computed burn, configured limits, last snapshot, calibration |
| GET | `/api/accounts` | none | Configured accounts and their `billing_mode` / plan state |
| GET | `/api/models` | none | br-GI-1-07's catalogue plus per-model pricing coverage (`priced` / `unpriced` / `provisional`) |
| GET | `/api/reconcile` | none | br-GI-1-13's computed-vs-billed comparison by day and model, with drift |
| POST | `/api/ingest` | Origin/Host | `SetIngestTrigger` (br-GI-1-14's `RunOnce`) |
| POST | `/api/accounts` | Origin/Host | `SetAccountWriter` — plan and limits, **no credentials** |
| POST | `/api/secrets` | Origin/Host | `SetCredentialWriter` (`secret.Save`) — the plan's re-auth flow |

The write-route guard count goes from three to six, all sharing br-GI-1-16's parameterized
`replayOriginReject`-shaped Origin/Host allowlist. **An unwired seam returns `503`**, exactly as
`POST /api/prices` does without `SetPricing` — the three POST handlers here never import
`secret`/`config`/`ingest`; they call through the function values br-GI-1-16 stores and br-GI-1-17
wires.

**Credential handling.** `POST /api/secrets` sets or replaces the claude.ai `sessionKey` or the Admin
key. It writes through the seam, returns no credential value, and reflects the Admin key's org-wide
read scope and the `--yes`-style confirmation requirement (the same rule br-GI-1-15's `clens accounts`
enforces). `GET /api/accounts` may report *whether* a credential is present and when it last worked
(br-GI-1-01's `Exists`/`LastUsed`) and **never** its value. The import-direction guarantee is unchanged:
`internal/api` and `internal/web` do not import `internal/secret` (br-GI-1-16's guard test must stay
green).

**Billing discipline (invariant 5).** Every aggregate route splits on `billing_mode`. `/api/stats`
(br-GI-1-16) already does; `/api/reconcile` returns **computed** and **billed** in separate labelled
fields and never a sum, and its `cost_drift` scope is stated as API-accounts-only. `/api/models` lists
`unpriced` models distinctly, never folded into a total, and marks `provisional` rows as such.

**The four dashboard tabs** (br-GI-1-16 shipped Overview, Calls, Sessions, Warnings, Stats, Settings):

- **Sources** — the tab that makes the tech plan's core promise trustworthy rather than aspirational:
  a dead collector is a visible **red row**, not a quietly short chart (test 17's UI half).
- **Quota** — renders a snapshot percentage **only when a limit is configured**; otherwise it states
  **`unconfigured`**. It never renders a percentage of a limit nobody supplied and never guesses that
  "Max 20x" means a particular number of messages.
- **Reconcile** — computed and billed in separate labelled columns, never summed, with the
  `cost_drift` API-accounts-only caveat shown.
- **Models** — the catalogue with pricing coverage; `unpriced` and `provisional` rendered as labelled
  states, not blank cells.

**Charts.** Hand-rolled inline SVG in `app.js`/`style.css` — **no framework, no `package.json`, no
build step, no CDN**. A CDN would add a third-party network load and an offline failure mode to a
loopback tool that holds every prompt.

## Rationale

These routes are the only way the four new tabs can show source health, quota, billing coverage and
reconciliation — the surfaces that turn "the tool says it is collecting" into "I can see which
collector is broken". Keeping them behind the four injected seams is what preserves the "credentials
never reach the API layer" guarantee as a mechanical import property rather than a review habit.

## Outcome Definition

- `go test ./internal/api/... ./internal/web/... -race` passes.
- All eight new routes respond `200` with the expected shape against a fixture store.
- Each of the three POST routes returns `503` when its seam is unwired, and refuses a request with no
  valid Origin/Host.
- `/api/sources` reports a failing collector as a non-OK row (test 17's UI half).
- `/api/quota` reports `unconfigured` — not a percentage — when no limit is set.
- `/api/reconcile` returns computed and billed in separate fields and never a summed figure.
- `/api/models` marks `unpriced` (and `provisional`) distinctly.
- `/api/secrets` and `/api/accounts` never return a credential value.
- The four new tabs exist, and the embedded assets reference no external URL (no CDN).
- `internal/api` and `internal/web` still do not import `internal/secret`.

## Test Specifications

- Unit Tests (`internal/api/sources_test.go`):
  - **Test 17 (route half)**: a fixture with the snapshot collector failed → `/api/sources` returns
    that source with a non-OK status and a reason; the others are OK.
- Unit Tests (`internal/api/quota_test.go`):
  - A configured limit → the snapshot percentage is present; unconfigured → `unconfigured` and no `%`.
  - One row per window is reflected (5-hour, 7-day, per-model).
- Unit Tests (`internal/api/accounts_test.go`):
  - GET returns configured accounts and billing modes; a present credential shows as present without a
    value.
  - POST with a valid Origin writes through a **stub** `SetAccountWriter`; the response carries no
    credential; unwired → `503`; no/foreign Origin → rejected.
- Unit Tests (`internal/api/models_test.go`):
  - `unpriced` and `provisional` are labelled distinctly; neither is rendered as `0`.
- Unit Tests (`internal/api/reconcile_test.go`):
  - Computed and billed are separate fields; no summed field exists; `cost_drift` fires at the
    threshold and not below it (test 15's route half).
- Unit Tests (`internal/api/secrets_test.go`):
  - POST with a valid Origin calls the stub `SetCredentialWriter` with the name and value; the
    response contains neither; unwired → `503`; no/foreign Origin → rejected.
  - Requesting an Admin key without the explicit confirmation is refused (br-GI-1-15's rule).
- Unit Tests (`internal/api/ingest_test.go`):
  - POST with a valid Origin calls the stub `SetIngestTrigger`; unwired → `503`.
- Unit Tests (`internal/web/assets_test.go`):
  - The four tab mount points exist in `index.html`; no asset contains an `http://`/`https://`
    external reference; the charts are inline SVG.
- Integration Tests: the Sources tab renders a failed collector as a red row against a fixture store
  (test 17); the tabs fetch their routes without error.
- E2E (also br-GI-1-19's acceptance step): a running `serve` serves all eight routes and the four tabs
  return `200`.

## Files to Touch

- `internal/api/api.go` (modify — register the eight routes on the existing mux)
- `internal/api/sources.go`, `internal/api/quota.go`, `internal/api/accounts.go`,
  `internal/api/models.go`, `internal/api/reconcile.go` (create — GET handlers)
- `internal/api/secrets.go`, `internal/api/ingest.go` (create — the POST handlers that call the seams)
- `internal/api/sources_test.go`, `internal/api/quota_test.go`, `internal/api/accounts_test.go`,
  `internal/api/models_test.go`, `internal/api/reconcile_test.go`, `internal/api/secrets_test.go`,
  `internal/api/ingest_test.go` (create)
- `internal/web/index.html`, `internal/web/app.js`, `internal/web/style.css` (modify — the four new
  tabs and their hand-rolled inline-SVG charts)
- `internal/web/assets_test.go` (create)
