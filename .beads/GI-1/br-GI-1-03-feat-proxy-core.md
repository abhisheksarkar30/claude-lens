# Bead br-GI-1-03: Transparent proxy core, upstream api.anthropic.com, redaction

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Invariants 1/2/6/7, §Billing model (auth-kind), §Cross-source identity, §Security posture, tests 1, 2, 19, 21

- **Bead ID**: br-GI-1-03
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-02
- **Blocks**: br-GI-1-08, br-GI-1-16, br-GI-1-17

## Description

The hot path. A streaming reverse proxy that forwards to `https://api.anthropic.com` by default,
tees the bytes it sees into the sink, and never buffers the stream.

- `proxy.New(cfg *config.Config, sk *sink.Sink) (http.Handler, error)` and
  `proxy.NewServer(cfg, sk) (*http.Server, error)`. stdlib `net/http` + `httputil.ReverseProxy`
  only.
- **Invariant 1 — never buffer the stream to count tokens.** The tee uses an `io.TeeReader`-style
  wrapper (`teeCloser` in deepseek-lens) and a `boundedBuffer` that counts *room* and stops storing
  at the cap rather than truncating silently. A buffered stream still returns correct bytes — just
  late — so only the TTFB test catches it. Response bodies are captured by a separate accumulator
  (`CapturedCall.RespBody` is nil while streaming).
- **Invariant 2 — import direction.** This package may depend only on `sink`, `config`, and
  `secret`-free primitives. It must **never** import `analyze`, `store`, `pricing`,
  `consumer`, `secret`, `jsonlogs`, `snapshot`, or `adminrep`. A test enforces this (see below).
- **Invariant 6 — fail open.** A broken observer never breaks the user's session. Any capture-side
  error is recorded on `CapturedCall.Err` and the response is returned unmodified.
- **Invariant 7 — redaction before the tee.** `internal/proxy/redact.go` strips
  `x-api-key`, `Authorization`, `Cookie`, plus the two new claude-lens-specific patterns: a
  `sessionKey` header (or cookie) and any `sk-ant-admin…` value. Redaction runs **before** the call is
  submitted, so the credential never reaches the sink, the consumer, or the DB.
  - Redaction must handle **multi-valued headers**: deepseek-lens's v1 blocker was that
    `Header.Get` returns only the first value, so a second `x-api-key` slipped through (test 19).
  - `RedactCheck` is the startup self-test (invoked from `serve`, br-GI-1-17, and `doctor`) that
    scans stored header JSON for a reachable `x-api-key`, a `sessionKey`, and an `sk-ant-admin…`
    pattern.
- **Upstream**: default `https://api.anthropic.com`; overridable by config. The client points Claude
  Code at clens with `ANTHROPIC_BASE_URL` (banner printed by `serve`).
- **`auth_kind` classification** — derived from the *shape* of the credential, and only the
  classification is stored, never the value:

  | Observed | `auth_kind` |
  |---|---|
  | `Authorization: Bearer` carrying an OAuth token (`sk-ant-oat…`) | `oauth` |
  | `x-api-key` carrying a standard key (`sk-ant-api…`) | `api_key` |
  | `x-api-key` carrying an admin key (`sk-ant-admin…`) | `admin` |
  | cloud-provider signature headers (Bedrock/Vertex/Foundry) | `cloud` (classified and stored, **not** priced) |
  | none / unrecognised | `unknown` (recorded as such, never guessed) |

  `billing_mode` is **not** derived here — it comes from the configured account (br-GI-1-08).
- **`RequestID` resolution** (the dedup key, §Cross-source identity). Always present:
  - **Response-derived id preferred**: the response's `request-id` header, which is distinct per
    attempt (a retried call carries a new one). Never derived from the request body when a response
    exists.
  - **Body hash is a last-resort identifier only**, for a call that never produced a response
    (transport failure). Even then it must not collapse attempts: the synthetic key carries a
    per-attempt disambiguator — `proxy:<sha256(body)>:<started_at_ns>:<attempt>` — so two
    byte-identical bodies on two attempts produce **two** rows (test 21). Collapsing them would
    destroy the `rate_limited` / `overloaded` signal those attempts exist to record.
- **`CaptureComplete`** is set false when the body was truncated by the cap or the stream ended
  without `message_stop` — the flag the merge precedence in br-GI-1-06/11 reads.
- **Body policy**: `full` / `truncated` / `off`, same 256 KB default cap as deepseek-lens.
- **`proxy.WithReplay(r, meta)`** exists so br-GI-1-17's replay path re-issues through the same
  handler (same transport, same tee, same writer).
- **Loopback + Origin/Host guard**: bind loopback unless `--allow-remote` (config.Validate enforces);
  the Origin/Host allowlist (`replayOriginReject`-shaped) is shared by every write route and is
  exercised by br-GI-1-18, but the guard function lives here.

## Rationale

The proxy is the only source that sees the request body — cache breakpoints, the `tools` array, the
top-level `system`, `thinking` config, and the credential shape. Without it the tool cannot localise
a cache break, cannot classify `auth_kind`, and sees only Claude Code traffic. The TTFB and
byte-identity gates are what keep that benefit from costing the user latency or fidelity.

## Outcome Definition

- `go test ./internal/proxy/... -race` passes.
- **TTFB**: with a fake upstream that slow-streams SSE, the client sees the first event before the
  upstream sends its last.
- **Byte identity**: client-request and upstream-response bodies are unchanged end to end, streaming
  and non-streaming.
- An imported-package test fails the build if `internal/proxy` imports `analyze`, `store`, `pricing`,
  `consumer`, `secret`, `jsonlogs`, `snapshot`, or `adminrep`.
- An `x-api-key` (including a second value on the same header) and a `sessionKey` are absent from the
  submitted `CapturedCall`.
- Two byte-identical request bodies with distinct response `request-id`s produce two distinct
  `RequestID`s.

## Test Specifications

- Unit Tests (`internal/proxy/proxy_test.go`):
  - **TestNoBufferingSSE (test 1 — the hard gate)**: fake upstream holds the last SSE event; assert
    the client reads the first event before the last is written.
  - **Byte identity (test 2)**: streaming and non-streaming bodies round-trip unchanged.
  - **Fail-open**: an error in the capture path still returns the upstream body unmodified.
  - **Body cap**: a body over the cap is stored as `truncated` with `CaptureComplete == false`.
  - **Hash fallback**: a transport failure with no `request-id` yields
    `proxy:<sha256>:<ns>:<attempt>`; two identical bodies on two attempts yield two distinct keys
    (test 21's proxy half).
  - **Response-derived id wins**: when a `request-id` header is present, the body hash is not used.
- Unit Tests (`internal/proxy/redact_test.go`):
  - **Multi-valued-header bypass (test 19)**: `x-api-key` set twice — both values redacted.
  - `Authorization`, `Cookie`, a `sessionKey` header and an `sk-ant-admin…` value all redacted.
  - `RedactCheck`'s scan pattern catches both new patterns in stored header JSON.
- Unit Tests (`internal/analyze`-free import guard): `TestProxyImportsAreNarrow` parses
  `internal/proxy/*.go` imports and fails on any forbidden package.
- Unit Tests (`internal/proxy/authkind_test.go`):
  - `sk-ant-oat…` bearer → `oauth`; `sk-ant-api…` → `api_key`; `sk-ant-admin…` → `admin`;
    cloud signature headers → `cloud`; absent → `unknown`.
- E2E: none (opt-in real-request path via br-GI-1-17).

## Files to Touch

- `internal/proxy/proxy.go`, `internal/proxy/proxy_test.go` (create)
- `internal/proxy/redact.go`, `internal/proxy/redact_test.go` (create)
- `internal/proxy/authkind.go`, `internal/proxy/authkind_test.go` (create)
