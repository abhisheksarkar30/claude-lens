# Bead br-GI-7-02: `decode.Body` reports how complete its answer is

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D2, §4 (`decode.go`, `decode_test.go`, `consumer.go`), §5 T4 (the unit half), §6 (the decompression-bomb and false-marker rows), §9 bead 02

- **Bead ID**: br-GI-7-02
- **Priority**: P0 (critical)
- **Original Estimate**: 1.5h
- **Dependencies**: None
- **Blocks**: br-GI-7-03

## Description

`decode.Body` (`decode.go:61`) already reads one byte past its cap (`:91-93`) and already swallows
the "a non-empty prefix decoded, then the stream errored" case (`:98-100` returns an error **only**
when nothing decoded). It computes the distinction the read path needs and then throws it away, so
br-GI-7-03 cannot tell a cap-truncated body from a complete one from a corrupt tail. This bead hands
the caller the signal the function already has.

### `internal/decode/decode.go` — the `Completeness` result (D2, F2.2)

```go
type Completeness int

const (
    Complete       Completeness = iota // fully available: no Content-Encoding, or EOF reached within the cap
    TruncatedAtCap                     // more than limit bytes decoded; the prefix is returned
    PartialCorrupt                     // a non-empty prefix, then a read error (the tail is corrupt)
    NotDecoded                         // a compressed body that produced no decoded bytes, or an encoding this package cannot read
)

func Body(h http.Header, body []byte, limit int) ([]byte, http.Header, Completeness, error)
```

The mapping, pinned here so it is not left to the implementer:

- **No `Content-Encoding`** → the early return at `decode.go:63-65` (`if len(encs) == 0`) yields
  `Complete`. This is the tool's central case (a plain or SSE-streamed response) and it **decodes
  nothing**, so it is easily misread as `NotDecoded`. It is not: the bytes are already plaintext and
  the read path draws no marker.
- **`limit <= 0`** → keep the existing error return (`decode.go:66-68`) exactly as it is; the
  `Completeness` result on this path is immaterial because br-GI-7-03 never calls `Body` with a
  non-positive cap. Do not change the guard or its message.
- **Decoded, `len(out) > limit` before truncation** → `TruncatedAtCap`. Compute this from the
  pre-truncation length, so a body that decodes to **exactly** `limit` bytes is `Complete`. That
  exact-length case is why `decode.go:91-92` reads `limit+1`: `len(decoded) == limit` as a cap test is
  a false positive for it, and this result replaces that test outright.
- **A non-empty prefix, then a read error** → `PartialCorrupt`, with the prefix returned. This is the
  case `Body` currently swallows; it becomes reachable and assertable.
- **Empty output from a compressed body, or an encoding `wrapperFor` does not implement** →
  `NotDecoded`, with the existing `err` and the raw body unchanged (the `ErrUnsupported` path at
  `:76-79` and the all-empty path at `:98-100`).

`err` keeps its current meaning: non-nil only when nothing decoded or the encoding is unsupported.
The new value carries the rest. Only br-GI-7-03's two read-path callers consult `Completeness`.

### `internal/consumer/consumer.go:299` — the one production call site

The signature change breaks this call (`if decoded, decodedHeaders, err := decode.Body(...)` is a
three-value receive). Take the new result as `_`:

```go
if decoded, decodedHeaders, _, err := decode.Body(respHeaders, respBody, c.bodyCapBytes); err == nil {
```

Behaviour is **unchanged**: the consumer keys off `err` exactly as today, because a degraded parse
still beats none (`decode.go:53-56`). Do not make the consumer branch on `Completeness`.

### `internal/decode/decode_test.go` — ten call sites, plus the corrupt-tail assertion

The signature change breaks every `Body(...)` call in the file. All ten take the new fourth result
(most as `_`): `:71`, `:95`, `:112`, `:132`, `:153`, `:168`, `:173`, `:180`, `:190`, `:202`.

`:202`'s corrupt-tail case is the natural home for the new `PartialCorrupt` unit assertion. Add
cases alongside the existing ones for `TruncatedAtCap` (a body that expands past the cap) and for the
enum's `Complete` on the exact-length-at-cap input — the case the withdrawn `len(decoded) == limit`
test would have mislabelled.

## Rationale

The round-1 draft inferred completeness with a helper *wrapping* `Body`. That is not derivable: the
only signal `Body` returned was an error that means "nothing decoded", never "cap hit" or "corrupt
tail after a clean prefix". Two of br-GI-7-03's four read-path cases were unassertable under it, and
its cap test (`len(decoded) == limit`) was a false positive for a body whose true decoded length is
exactly the cap. Moving the signal into the function that computes it is the smaller change and the
only correct one.

The decompression-bomb guard is unchanged and is the reason to reuse this function rather than write
a new one: a compressed blob that expands past the cap is truncated, never unbounded.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `decode.Body` returns the four-valued result above; the unencoded early return yields `Complete`;
  a body decoding to exactly `limit` is `Complete`, not `TruncatedAtCap`; a body expanding past the
  cap is `TruncatedAtCap`; a clean prefix followed by a corrupt tail is `PartialCorrupt`; a compressed
  body that produces nothing is `NotDecoded` with a non-nil error and the raw bytes.
- `internal/consumer`'s behaviour is unchanged — it still keys off `err` alone.
- `TestNoBufferingSSE` stays green (nothing on the hot path changes).

## Test Specifications

- Unit Tests (`internal/decode/decode_test.go`):
  - Every existing `Body` case still asserts the same bytes and headers after the signature change.
  - `Complete` on a body with no `Content-Encoding` (the existing `:190` case).
  - `Complete` on a compressed body that decodes to **exactly** `limit` bytes — the exact-length
    false positive the withdrawn test would have produced.
  - `TruncatedAtCap` on a compressed body that expands past `limit`, with the returned bytes being the
    prefix and no error.
  - `PartialCorrupt` on the `:202` corrupt-tail fixture: a non-empty prefix returned, and the result
    distinguishing it from both `Complete` and `NotDecoded`.
  - `NotDecoded` plus a non-nil error on an unsupported `Content-Encoding` (the existing `:168` case)
    and on a compressed body that yields no bytes.
- Integration Tests: none — `internal/decode` is a leaf package.
- E2E: none.

## Files to Touch

- `internal/decode/decode.go` (modify — `Completeness` type and the new `Body` signature)
- `internal/decode/decode_test.go` (modify — ten call sites take the fourth result; new
  `TruncatedAtCap`/`PartialCorrupt`/exact-length cases)
- `internal/consumer/consumer.go` (modify — the `decode.Body` call at `:299` takes the result as `_`)
