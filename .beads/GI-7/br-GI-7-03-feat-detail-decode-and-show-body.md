# Bead br-GI-7-03: The detail route decodes a response body; `clens show --body` decodes the same way

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D2, §4 (`api.go`, `cli/show.go`, `cli/serve.go`), §5 T4/T5, §6 (the unwired-cap row and the "decoding is not a rewrite" row), §9 bead 03

- **Bead ID**: br-GI-7-03
- **Priority**: P0 (critical)
- **Original Estimate**: 2.5h
- **Dependencies**: br-GI-7-01, br-GI-7-02
- **Blocks**: br-GI-7-04

## Description

Claude Code advertises `Accept-Encoding: gzip, deflate, br, zstd` and the proxy stores the bytes it
teed, unmodified — decompressing on the client's goroutine is what the TTFB gate forbids. So a stored
response body is routinely brotli, and the detail view has nothing to show. The browser cannot decode
it: `DecompressionStream` supports `gzip`, `deflate` and `deflate-raw` but **not** `br`, and the
no-build-step rule (`decisions/006-dashboard-with-no-build-step.md`) forbids bundling a library. So
decoding is necessarily a server-side, **read-path** concern, and this bead adds it to the one place
that can do it correctly: `decode.Body`, called from the two surfaces that render a body.

Decoding is **display-only and never rewrites the stored bytes**. Replay sends `orig.ReqBody` /
`orig.ReqHeaders` straight from the row (`replay.go:86-96`, `:166-178`), so nothing here has a route
back to a replay.

### `internal/api/api.go` — `eventDetail` gains the decoded body and its honesty fields (F3.4)

```go
type eventDetail struct {
    *store.Event
    Warnings []store.Warning `json:"warnings"`
    // RespBodyDecoded is the response bytes the UI renders: the decoded form
    // when the cap was wired and decoding ran, else the raw RespBody.
    RespBodyDecoded []byte `json:"RespBodyDecoded"`
    // RespBodyCompleteness's NotDecoded value means RespBodyDecoded is the raw
    // RespBody unchanged.
    RespBodyCompleteness decode.Completeness `json:"RespBodyCompleteness"`
    // BodyCapBytes == 0 means the read cap is unwired, never a zero-byte cap.
    BodyCapBytes int `json:"BodyCapBytes"`
}
```

`getRequest` fills them: parse `ev.RespHeaders` (a JSON object) into an `http.Header`; on a parse
failure pass an empty header, which has no `Content-Encoding` and is therefore `Complete` — a
malformed header blob must degrade to "shown raw", never fail the request. Then:

- **cap positive** → `decoded, _, completeness, err := decode.Body(hdr, ev.RespBody, a.bodyCapBytes)`.
  On `err != nil` (nothing decoded, or an encoding we cannot read) serve `RespBodyDecoded = ev.RespBody`
  with `completeness = decode.Completeness(decode.NotDecoded)`. On `err == nil` serve the decoded bytes
  and the returned `completeness`.
- **cap not positive** → **do not call `decode.Body` at all**. Serve `RespBodyDecoded = ev.RespBody`
  and `BodyCapBytes = 0`. Everything else about the response is unchanged.

The request half is **not** decoded: no compressed request body has been observed, and inventing
behaviour for an unobserved case is scope this story does not need (F1.14). `clens show --body`
prints the request half raw (`show.go:106`) and keeps doing so.

### `SetBodyCapBytes` — the cap arrives as a seam, and the zero is guarded (F1.4, F4.4)

`internal/api` may not import `internal/config`: `importguard_test.go:31-38` bans `/internal/config`
(and `/internal/ingest`, `/internal/secret`) from `internal/api` and `internal/web`, and
`internal/cli/serve_test.go` asserts the same edges. So the cap cannot be read, only injected:

```go
// SetBodyCapBytes wires the read-path decode cap. A non-positive n is ignored:
// leaving the seam unwired is a supported state, so a zero can never reach
// decode.Body.
func (a *api) SetBodyCapBytes(n int) {
    if n > 0 {
        a.bodyCapBytes = n
    }
}
```

Declare `bodyCapBytes int` on the `api` struct beside the existing seam fields. This mirrors
`SetSourceHealth`/`SetAccounts` (`api.go:137-145`) and the consumer's own `SetBodyDecoding` guard
(`consumer.go:95-98`).

**Both guards matter.** `decode.Body(…, 0)` returns the raw body **and** an error
(`decode.go:66-68`), so a wiring omission would otherwise make br-GI-7-04 render *"response shown
undecoded — it would not decompress"* for a body that decodes fine. With the cap ignored and the
route not decoding, the cap-not-configured state is signalled by `BodyCapBytes == 0` alone, and
br-GI-7-04 keys its cap-not-configured line off that **before** consulting
`RespBodyCompleteness`. So `NotDecoded`'s wording is reachable only when a positive cap was applied
**and** the body was compressed **and** decoding still produced nothing — never manufactured by a
missing cap.

### `internal/cli/show.go` — the terminal decodes the same body the same way

`--body` decodes the **response** half through `decode.Body` with `cfg.BodyCapBytes`, using the same
"only when the cap is positive" rule, and prints the result. It prints the marker the CLI's own
vocabulary calls for when the result is not `Complete`:

- `TruncatedAtCap` → the response was truncated at the read cap of N bytes;
- `PartialCorrupt` → the response decoded only partially — its tail was corrupt;
- `NotDecoded` → the response is shown raw because it would not decompress.

`printBody` (`show.go:128-135`) keeps its "not stored" line for an empty body. The request half stays
raw.

### `internal/cli/serve.go` — wire the seam at the composition root

Beside the existing `dashAPI.SetSourceHealth(...)`/`SetAccounts(...)` calls, add:

```go
dashAPI.SetBodyCapBytes(cfg.BodyCapBytes)
```

`serve.go:105` already hands the same value to the consumer.

## Rationale

Without this, the detail view's response section can only ever show mojibake — the observed failure
(`clens show --body` printing `e 81 ����OW�…` for a 917-byte brotli body). The decode has to be
server-side (§2.4) and has to stay on the read path (the TTFB gate forbids it on the client's
goroutine). Reusing `decode.Body` rather than writing a new decoder also inherits its decompression
cap: a compressed blob that expands past the cap is truncated, not unbounded.

The completeness signal is what keeps the view honest. A partial or undecoded body presented as
complete is a lie about the response, and an absence indistinguishable from a failure is this story's
recurring defect. The signal lives in `decode.Body` because that is where it is computable
(br-GI-7-02); this bead only carries it to the wire.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- A brotli-encoded stored body comes back plaintext from `GET /api/requests/{id}` with
  `RespBodyCompleteness == Complete`, alongside the unchanged metadata and warnings.
- A body that decodes to **exactly** `BodyCapBytes` comes back `Complete`, **not** `TruncatedAtCap`.
- A body that expands past the cap comes back `TruncatedAtCap` with the prefix; a body with a corrupt
  tail after a clean prefix comes back `PartialCorrupt`; a body that is not valid brotli comes back
  raw with `NotDecoded` and a 200 — never a 500.
- A stored plain body with **no** `Content-Encoding` and a positive cap comes back `Complete`, with
  `RespBodyDecoded` equal to `RespBody` and never the *"would not decompress"* state.
- A handler with no `SetBodyCapBytes` (and one called with `0`) returns the raw body with
  `BodyCapBytes == 0`, and never the `NotDecoded` state.
- `clens show --body` prints a decoded response body for the same stored row the API serves, with the
  same marker vocabulary.
- `internal/proxy` gains no import; `TestNoBufferingSSE` stays green.
- The four header/body columns in `internal/api`'s list and session routes are still absent
  (br-GI-7-01's outcome is not regressed).

## Test Specifications

- Unit Tests (`internal/api/api_test.go` — T4):
  - `TestDetailDecodesResponseBody`: a brotli-encoded stored body returns plaintext with
    `Completeness == Complete`.
  - `TestDetailExactCapBodyIsComplete`: a body decoding to exactly `BodyCapBytes` returns `Complete`,
    not `TruncatedAtCap` (the case the withdrawn `len(decoded) == cap` test would have mislabelled).
  - `TestDetailMarksCapTruncatedBody`: expands past the cap → `TruncatedAtCap`, prefix returned.
  - `TestDetailMarksCorruptTailBody`: clean prefix then a read error → `PartialCorrupt` — reachable
    now that `decode.Body` returns the signal it used to discard. **Fixture corrected during
    implementation:** this case must use **gzip** (or zstd), not brotli. brotli's reader emits per
    meta-block, so a stream cut mid-way decodes to *zero* bytes and lands on `NotDecoded`, never
    here — measured at 2/16, 4/16 and 8/16 cut points across 12 KB, 200 KB and 1 MB payloads, all
    zero-length. gzip and zstd are stream-oriented and do yield a clean prefix plus a read error.
    Both shapes reach a stored row, since Claude Code advertises all four codings.
  - `TestDetailFallsBackToRawOnCorruptBody`: not valid brotli → raw bytes, `NotDecoded`, **200 not
    500**.
  - `TestDetailUnencodedBodyIsComplete`: a stored plain body with no `Content-Encoding` and a positive
    cap → `Complete`, `RespBodyDecoded == RespBody`, and never the *"would not decompress"* state.
  - `TestDetailUnwiredCapServesRawBody`: a handler with no `SetBodyCapBytes`, and one whose cap was set
    to `0`, each return the raw body with `BodyCapBytes == 0` and never the `NotDecoded` state — the
    guard against `decode.Body(…, 0)`'s error being surfaced as a decode failure.
  - Each case sets `SetBodyCapBytes` explicitly on the handler it builds, so the test does not depend
    on `New`'s default.
- Unit Tests (`internal/cli/cli_test.go` — T5):
  - `TestShowBodyDecodesResponse`: `clens show --body` against a stored brotli row prints the decoded
    response, and the cap-truncated case prints its marker — the CLI and the API agreeing on the same
    stored row.
- Integration Tests: the detail route exercised end to end through `New(...)` with a seeded temp store
  (the existing `api_test.go` harness).
- E2E: none.

## Files to Touch

- `internal/api/api.go` (modify — `eventDetail`'s three fields, `getRequest`'s decode, the
  `bodyCapBytes` field and `SetBodyCapBytes`)
- `internal/api/api_test.go` (modify — T4's cases)
- `internal/cli/show.go` (modify — response-half decode and the marker vocabulary)
- `internal/cli/cli_test.go` (modify — T5)
- `internal/cli/serve.go` (modify — `dashAPI.SetBodyCapBytes(cfg.BodyCapBytes)`)
