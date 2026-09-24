# Bead br-GI-16-13: Proxy pass-through conformance test (six raw-byte-equality cases)

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream D, §D.1-D.4.

- **Bead ID**: br-GI-16-13
- **Priority**: P3 (low — evidence, not behaviour; droppable without affecting the story)
- **Original Estimate**: 1h
- **Dependencies**: None
- **Blocks**: None (no other bead references it; the story is intact if it is dropped)

## Description

**Test-only. Change no proxy behaviour.** Diagnosis found no pass-through defect: `Rewrite` only calls `SetURL`
(`internal/proxy/proxy.go:42-44`), both bodies are wrapped in `io.TeeReader` (`:128`, `:92`), `ModifyResponse`
edits neither headers nor status, and the hot path never parses JSON. This bead pins that as a test, so a future
change that parses/re-serializes a body or "redacts before forwarding" fails CI.

**First step: read `internal/proxy/proxy_test.go`** and reuse its fake-upstream fixture. Existing tests include
`TestByteIdentityNonStreaming` (`:174`) and `TestNoBufferingSSE` (`:111`); if an existing case already covers a row
below, **extend it instead of adding a duplicate** and say so in the commit body. No existing test asserts case
6 (credential header reaches upstream unchanged) as of this writing; verify.

The upstream fixture records the **raw bytes** it received; every assertion compares bytes, **never
decoded-then-compared** (a decode-and-compare would pass while a real field drop goes uncaught).

| # | Case | Assertion |
|---|---|---|
| 1 | Request body with an unrecognized top-level field (`"safeguards":[…]`) | upstream received the body byte-identical |
| 2 | Response body with an unrecognized top-level field (`"safeguard_results":{…}`) | client received the body byte-identical |
| 3 | Streamed SSE with an unknown field inside a `message_delta` event's `delta` | byte-identical; event framing and ordering unchanged |
| 4 | Tool-use IDs in the streamed events | unchanged, no rewriting |
| 5 | Request header the proxy has no knowledge of (`anthropic-beta`) | reaches upstream unchanged |
| 6 | Credential header (`x-api-key` **and** `authorization`) | reaches upstream **unchanged**, even though the capture copy is redacted |

Case 6 is load-bearing: redaction is capture-only (`redactHeaders` feeds `st.reqHeaders`, never `r.Header`). Also
assert in case 6 that the captured (sink) copy **is** redacted, so the test proves both halves.

Name the two sanctioned exceptions in a test comment: hop-by-hop headers RFC 9110 requires `ReverseProxy` to
drop, and the capture-side copy. Assert equality of the body and of end-to-end headers, **not** the full header set.
Non-goals: no new `doctor` check, no route, no config key, no fuzz harness.

## Rationale

The auto-mode classifier notice blamed `127.0.0.1:8797`. It misattributes (upstream is DeepSeek), but "clens is
byte-transparent" was only a claim from reading the hot path. A green test answers the next notice in one command.

## Outcome Definition

- Six cases pass against the current proxy with no production-code change.
- Temporarily making the proxy rewrite a body byte, or redact a forwarded credential, turns the relevant case red (verify locally once, do not commit the mutation).
- `internal/proxy` still imports only `sink` and `config`.
- `go build ./... && go vet ./... && go test ./internal/proxy/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests (`internal/proxy/proxy_test.go`): the six cases above, each comparing raw `[]byte`; for the SSE cases compare the full concatenated stream and the ordered list of event blocks split on `\n\n`.
- Integration Tests: none.

## Files to Touch

- `internal/proxy/proxy_test.go` (modify — new/extended tests only; no production file)
