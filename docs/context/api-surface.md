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
| `GET` | `/api/requests` | the call log, newest first. **Projected**: `store.EventSummary`, which carries none of the four header/body columns nor the two transcript ones | `limit`, `offset`, filters (see below) | [internal/api/api.go:167](../../internal/api/api.go#L185) |
| `GET` | `/api/requests/{id}` | one call in full, plus three decoded-body fields (below) | — | [internal/api/api.go:168](../../internal/api/api.go#L186) |
| `GET` | `/api/mode` | the proxy-mode signal the header badge renders: `Configured`, `Observed`, `Badge` | — | [internal/api/mode.go](../../internal/api/mode.go) |
| `GET` | `/api/stats` | totals by period, model, or cost source | window + `granularity` | [internal/api/api.go:170](../../internal/api/api.go#L188) |
| `GET` | `/api/warnings` | findings, one row per occurrence | `limit`, `offset` | [internal/api/api.go:175](../../internal/api/api.go#L193) |
| `GET` | `/api/warnings/summary` | findings grouped by kind | — | [internal/api/api.go:174](../../internal/api/api.go#L192) |
| `GET` | `/api/sessions` | one row per agentic run | `limit`, `offset` | [internal/api/api.go:176](../../internal/api/api.go#L194) |
| `GET` | `/api/sessions/{id}` | one session and its events. **Projected** the same way as `/api/requests` | — | [internal/api/api.go:177](../../internal/api/api.go#L195) |
| `GET` | `/api/sources` | collector health: last success, last error, rows written | — | [internal/api/api.go:185](../../internal/api/api.go#L203) |
| `GET` | `/api/quota` | burn per account/window, snapshots, calibration candidates | — | [internal/api/api.go:186](../../internal/api/api.go#L204) |
| `GET` | `/api/accounts` | configured accounts and their plan/billing state | — | [internal/api/api.go:187](../../internal/api/api.go#L205) |
| `GET` | `/api/models` | the catalogue, plus every model traffic used | — | [internal/api/api.go:188](../../internal/api/api.go#L206) |
| `GET` | `/api/reconcile` | computed (A/B) vs billed (D), per day and model | — | [internal/api/api.go:189](../../internal/api/api.go#L207) |
| `GET` | `/api/prices` | the effective rate table | — | [internal/api/api.go:180](../../internal/api/api.go#L198) |
| `GET` | `/api/health` | health, plus the counters that would otherwise be invisible | — | [internal/api/api.go:179](../../internal/api/api.go#L197) |
| `GET` | `/api/stream` | **SSE.** Live push of new events and warnings. | — | [internal/api/api.go:178](../../internal/api/api.go#L196) |
| `GET` | `/` and below | the embedded dashboard assets | — | [internal/api/api.go:194](../../internal/api/api.go#L213) |

**Pagination** rides on response headers, not in the body, so a list response stays the bare JSON
array. `limit`/`offset` default to `0`, which every store list method treats as *its own capped
default* — never unbounded. A malformed or negative value is rejected with an error, not clamped.

### The list projection, and why it is a type

`/api/requests` and `/api/sessions/{id}` return `store.EventSummary` — the scalar block — rather
than `store.Event`. The distinction is a **compile error, not a filter flag**: a caller that needs a
body cannot reach one, because the field is not there to name, whereas a forgotten flag would return
`nil` where bytes were expected, and a body-less `jsonl` row is byte-identical on the wire to an
unselected one. Fifty captured calls carry ~15 MB of stored bodies (measured), so the projection is
what keeps the dashboard's hottest fetch at ~48 KB. `ListEventsFull` is the full-row read;
`SessionEventsForRules` (GI#13) is the session-scoped analyzer pass's own read. It started as a
narrower, nine-column projection that still included `req_body` (needed to compare tool names
between two calls); **br-GI-13-07 moved that comparison onto the new `req_tool_names` column**, so
`req_body` rejoined the omitted set and `SessionEventsForRules` now selects the same 42 columns as
`EventSummary` — the two projections' omitted-column lists are identical, kept as separate named
lists in code only because tests predating the change assert them by name. All three SELECTs still
derive from one base column list so they cannot drift. See [storage-schema.md](storage-schema.md).

`/api/requests/{id}` adds the decoded response body and the two fields that keep it honest:

| Field | Meaning |
|---|---|
| `RespBodyDecoded` | the bytes the view renders — the decoded form when the cap was wired and decoding ran, the raw `RespBody` otherwise. **Equalling `RespBody` does not mean "undecoded"**: that is also true of a body with no `Content-Encoding`. |
| `RespBodyCompleteness` | `decode.Completeness` as its **integer** value (`0` Complete, `1` TruncatedAtCap, `2` PartialCorrupt, `3` NotDecoded). `app.js` compares those integers; `TestDetailPinsCompletenessWireValue` pins all four spellings. |
| `BodyCapBytes` | the read cap in force, or **`0` for unwired** — never a zero-byte cap. The view checks this *before* the completeness, so a missing cap cannot manufacture "would not decompress". It is the **running process's** configured cap, injected on every response and *not* recorded per row — so a view that infers "this body was cut at the cap" by comparing lengths is only as good as the cap not having changed since the row was written. The authoritative signal is `CaptureComplete`; the comparison only names *which* body (see [storage-schema.md](storage-schema.md) §The marker's known limitation). |

Decoding is display-only: nothing here writes back, and replay sends the stored `ReqBody` and
`RespHeaders` straight from the row.

### Writes

| Method | Path | Auth | Summary | Unwired seam |
|---|---|---|---|---|
| `POST` | `/api/requests/{id}/replay` | same-origin + `--replay` | re-issue a captured call | **disabled by default** — 403 unless `clens serve --replay` |
| `POST` | `/api/prices` | same-origin | write a user rate override | `503` without `SetPricing` |
| `POST` | `/api/accounts` | same-origin | save the accounts file | `503` without `SetAccountWriter` |
| `POST` | `/api/secrets` | same-origin | store a credential | `503` without `SetCredentialWriter` |
| `POST` | `/api/ingest` | same-origin | run every collector once | `503` without `SetIngestTrigger` |
| `POST` | `/api/shutdown` | same-origin **+ loopback caller** (GI#13) | `clens shutdown`'s remote-triggered graceful stop — the same cancel func Ctrl+C already drives | `503` without `SetShutdown`; response is written before the func runs, on its own goroutine |

### The write guards

1. **Same-origin (`originReject`)** — every write route shares it
   ([internal/api/origin.go](../../internal/api/origin.go)). A request with **no** `Origin` header
   **passes**: browsers always send one, so its absence means a non-browser caller, which is the
   CLI's own path, deliberately. A request **with** an `Origin` must match the request's own
   `Host`, compared as host:port. A cross-origin POST gets `403`.
2. **Opt-in for the one billable route** — replay sends a real, billable call, so it is off unless
   the server was started with `--replay`.
3. **Loopback caller, for the one process-stopping route** (GI#13) — `originReject` alone checks
   only the `Host` header, which is the DNS-rebinding case (a page whose own hostname resolves to
   `127.0.0.1`). A caller that can already reach a dashboard bound to `0.0.0.0` (`--allow-remote`)
   can send `Host: 127.0.0.1` and pass that guard regardless of where the connection actually came
   from. `/api/shutdown` additionally checks `r.RemoteAddr` itself against a loopback predicate —
   `--allow-remote` widens neither guard for this route; it is loopback-only unconditionally. See
   [security-and-permissions.md](security-and-permissions.md).

Every replay rejection is counted in `replayRejected`, surfaced on `GET /api/health` — without it, a
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
