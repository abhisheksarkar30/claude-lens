[← INDEX](INDEX.md)

# API Surface

Two HTTP surfaces, on two listeners. The proxy surface is not a REST API at all — it is a
transparent pass-through — and is documented first because it is the one that carries traffic.

## Proxy listener — `127.0.0.1:8797`

| Method | Path | Auth | Summary | Evidence |
|---|---|---|---|---|
| any | any path | passed through untouched | **Transparent reverse proxy.** The method, path, headers, and body reach `--upstream-url` unchanged, and the response returns unchanged. The listener does not parse, does not require a credential, and does not fail closed. | [internal/proxy/proxy.go](../../internal/proxy/proxy.go) |

The only paths with behaviour of their own are the ones the capture logic inspects to decide
whether it is looking at a `/v1/messages` call. Everything else is a byte-for-byte pass-through —
`TestByteIdentityNonStreaming` asserts the bytes, and `TestNoBufferingSSE` asserts that streaming
responses are not buffered.

*Clients are pointed here by setting `ANTHROPIC_BASE_URL=http://127.0.0.1:8797`.*

## Dashboard listener — `127.0.0.1:8798`

Loopback-bound. **There is no authentication on any route** — the security model is the bind
address, deliberately (single-user local tool). Every write route additionally passes the
same-origin guard described below.

### Reads

All read routes are wrapped in `methodGet`, which answers a non-GET with a JSON `405` before the
handler runs.

| Method | Path | Summary | Parameters | Evidence |
|---|---|---|---|---|
| `GET` | `/api/requests` | the call log, newest first | `limit`, `offset`, filters (see below) | [internal/api/api.go:167](../../internal/api/api.go#L167) |
| `GET` | `/api/requests/{id}` | one call in full | — | [internal/api/api.go:168](../../internal/api/api.go#L168) |
| `GET` | `/api/stats` | totals by period, model, or cost source | window + `granularity` | [internal/api/api.go:170](../../internal/api/api.go#L170) |
| `GET` | `/api/warnings` | findings, one row per occurrence | `limit`, `offset` | [internal/api/api.go:175](../../internal/api/api.go#L175) |
| `GET` | `/api/warnings/summary` | findings grouped by kind | — | [internal/api/api.go:174](../../internal/api/api.go#L174) |
| `GET` | `/api/sessions` | one row per agentic run | `limit`, `offset` | [internal/api/api.go:176](../../internal/api/api.go#L176) |
| `GET` | `/api/sessions/{id}` | one session and its events | — | [internal/api/api.go:177](../../internal/api/api.go#L177) |
| `GET` | `/api/sources` | collector health: last success, last error, rows written | — | [internal/api/api.go:185](../../internal/api/api.go#L185) |
| `GET` | `/api/quota` | burn per account/window, snapshots, calibration candidates | — | [internal/api/api.go:186](../../internal/api/api.go#L186) |
| `GET` | `/api/accounts` | configured accounts and their plan/billing state | — | [internal/api/api.go:187](../../internal/api/api.go#L187) |
| `GET` | `/api/models` | the catalogue, plus every model traffic used | — | [internal/api/api.go:188](../../internal/api/api.go#L188) |
| `GET` | `/api/reconcile` | computed (A/B) vs billed (D), per day and model | — | [internal/api/api.go:189](../../internal/api/api.go#L189) |
| `GET` | `/api/prices` | the effective rate table | — | [internal/api/api.go:180](../../internal/api/api.go#L180) |
| `GET` | `/api/health` | health, plus the counters that would otherwise be invisible | — | [internal/api/api.go:179](../../internal/api/api.go#L179) |
| `GET` | `/api/stream` | **SSE.** Live push of new events and warnings. | — | [internal/api/api.go:178](../../internal/api/api.go#L178) |
| `GET` | `/` and below | the embedded dashboard assets | — | [internal/api/api.go:194](../../internal/api/api.go#L194) |

**Pagination** rides on response headers, not in the body, so a list response stays the bare JSON
array. `limit`/`offset` default to `0`, which every store list method treats as *its own capped
default* — never unbounded. A malformed or negative value is rejected with an error, not clamped.

### Writes

| Method | Path | Auth | Summary | Unwired seam |
|---|---|---|---|---|
| `POST` | `/api/requests/{id}/replay` | same-origin + `--replay` | re-issue a captured call | **disabled by default** — 403 unless `clens serve --replay` |
| `POST` | `/api/prices` | same-origin | write a user rate override | `503` without `SetPricing` |
| `POST` | `/api/accounts` | same-origin | save the accounts file | `503` without `SetAccountWriter` |
| `POST` | `/api/secrets` | same-origin | store a credential | `503` without `SetCredentialWriter` |
| `POST` | `/api/ingest` | same-origin | run every collector once | `503` without `SetIngestTrigger` |

### The two write guards

1. **Same-origin (`originReject`)** — every write route shares it
   ([internal/api/origin.go](../../internal/api/origin.go)). A request with **no** `Origin` header
   **passes**: browsers always send one, so its absence means a non-browser caller, which is the
   CLI's own path, deliberately. A request **with** an `Origin` must match the request's own
   `Host`, compared as host:port. A cross-origin POST gets `403`.
2. **Opt-in for the one billable route** — replay sends a real, billable call, so it is off unless
   the server was started with `--replay`.

Every rejection is counted in `replayRejected`, surfaced on `GET /api/health` — without it, a
rejected probe against the one billable route would leave no server-side trace at all.

### 503, not empty

A write route whose backing seam is unwired answers `503` **with a reason**. "Nothing is wired" and
"everything is fine and nothing happened" are different claims, and a dashboard that renders them
identically is one you cannot trust about the difference. Read seams (`SetSourceHealth`,
`SetAccounts`) behave the same way rather than returning an empty list.

## Scheduled / CLI triggers

There is no cron, no queue, and no serverless trigger. The repeatable work is driven by subcommands
— `clens refresh` is the one designed as a scheduler target (cron / Task Scheduler). See
[cli-and-tooling.md](cli-and-tooling.md).

## Representative payloads

Real observed responses from the acceptance run are recorded in
[docs/acceptance.md](../acceptance.md) §Local half — including `GET /api/quota` answering `200`
with a `"(no accounts configured)"`-shaped body rather than `503`, because its read seam *is*
wired. No sample JSON is reproduced here; the acceptance doc is the record.
