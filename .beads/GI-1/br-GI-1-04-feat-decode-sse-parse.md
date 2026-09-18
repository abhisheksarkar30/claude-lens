# Bead br-GI-1-04: Content-Encoding decode + SSE/JSON response parsing

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Carried over (decode, incremental SSE parse), §Invariants 1/2, §Infrastructure (dependencies), test 3

- **Bead ID**: br-GI-1-04
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-01
- **Blocks**: br-GI-1-05

## Description

Turn the captured bytes into a stream of parseable events, in the cold path — never inline in the
proxy's tee, because decompression on the client's goroutine is exactly what the TTFB gate forbids.

**`internal/decode`** — `Body(h http.Header, body []byte, limit int) ([]byte, http.Header, error)`.
Removes the transport `Content-Encoding`, handling, in the order RFC 9110 requires (last listed
coding comes off first): `gzip`/`x-gzip`, `deflate` (zlib framing), `br`, `zstd`. `identity` and
empty codings are dropped, so a body with neither takes the unchanged early return.

- Returns a copy of the header with `Content-Encoding` and `Content-Length` removed, so the returned
  pair describes the returned bytes consistently — that is what keeps a stored row replayable
  (br-GI-1-17 re-sends a row's stored body with that row's stored headers).
- Output is capped at `limit` bytes: the proxy's cap is on the **compressed** capture, and 256 KB of
  brotli can expand to many megabytes, so a second cap on the decoded form is required.
- A partially decoded body (the proxy's cap cut the encoded stream) yields the decoded prefix and no
  error — some parsed usage beats none.
- An encoding with no decoder returns the body unchanged plus an error naming it, exported as
  `ErrUnsupported`, so the caller can store what was captured and say why.
- The zstd decoder is built with `zstd.WithDecoderConcurrency(1)`: one goroutine per body rather than
  the default `GOMAXPROCS`-many, since a decoder is built fresh per captured call.

**Dependency decision (this bead is where the plan's dependency claim breaks — see the summary).**
Claude Code sends `Accept-Encoding: gzip, deflate, br, zstd` and the endpoint answers with a
non-identity `Content-Encoding`. Go 1.24's stdlib has gzip and deflate only — there is **no public
brotli or zstd decoder** in the standard library. Supporting `br` and `zstd` therefore requires two
non-stdlib modules, the same two deepseek-lens ships: `github.com/andybalholm/brotli` and
`github.com/klauspost/compress` (its `zstd` subpackage). That makes the plan's Infrastructure claim
("`modernc.org/sqlite` remains the **only** non-stdlib dependency") **false as written**.

**Decided (user, after cross-review): carry both modules.** This bead adds them, and the plan was
corrected to three direct dependencies at v6 (`F6.1`) — each with a stated purpose: `sqlite` for
storage, `brotli` for `br`, `klauspost/compress` for `zstd`. br-GI-1-19 verifies the README and docs
state three, not one. Dropping `br`/`zstd` was the alternative considered and rejected: it would make
Claude Code's compressed responses — the common case, and the exact bug deepseek-lens's decode
package was written to fix — parse to zero tokens.

**`internal/parse` (framing half)** — incremental SSE + non-stream JSON:

- `parse/sse.go`: an incremental SSE reader that survives an event split across a chunk boundary
  (test 3) and emits `event:`/`data:` frames. It must track the Claude event sequence
  (`message_start`, `content_block_*`, `message_delta`, `message_stop`) so `stream_incomplete`
  (br-GI-1-10) can be decided: a stream that ends without `message_stop` is incomplete.
- `parse/types.go`: non-stream JSON bodies (`application/json`) decode to the same event stream.
- The parse package degrades rather than fails on a shape it cannot finish reading — a
  partially-decoded body must still yield whatever usage it can.

The **extraction** half (Meta and Usage fields) is br-GI-1-05; this bead delivers the framing and the
decoder only, so br-GI-1-05 has a stable seam to build on.

## Rationale

Without decode, every compressed call reports zero tokens and an unresolved model — deepseek-lens
measured exactly this. Keeping decode in the cold path, not in the tee, is invariant 1.

## Outcome Definition

- `go test ./internal/decode/... ./internal/parse/... -race` passes.
- A gzip-, deflate-, brotli- and zstd-encoded body each decode to the same bytes.
- An event split across two chunk boundaries parses to one event (test 3).
- A `Content-Encoding` this package cannot decode returns the input unchanged plus `ErrUnsupported`.
- The decoded output never exceeds `limit`.
- `go.mod` records the decoder modules the package actually imports.

## Test Specifications

- Unit Tests (`internal/decode/decode_test.go`):
  - Each of gzip, deflate, br, zstd round-trips.
  - `identity` and empty take the unchanged path.
  - Stacked codings (`gzip, br`) decode in the correct order.
  - Output is capped at `limit`; a body exactly at the limit is not mistaken for truncated.
  - A truncated encoded stream yields the decoded prefix and no error.
  - An unsupported coding returns the input plus `ErrUnsupported`.
  - `Content-Encoding` and `Content-Length` are removed from the returned header.
- Unit Tests (`internal/parse/sse_test.go`):
  - **Split-chunk SSE (test 3)**: one event delivered across two `Write`s parses correctly.
  - A full `message_start → message_delta → message_stop` sequence yields the expected frames.
  - A stream ending without `message_stop` is flagged incomplete.
  - A non-stream JSON body yields the equivalent event set.
  - A malformed frame is skipped, not fatal.
- E2E: none.

## Files to Touch

- `go.mod`, `go.sum` (modify — add `github.com/andybalholm/brotli`, `github.com/klauspost/compress`)
- `internal/decode/decode.go`, `internal/decode/decode_test.go` (create)
- `internal/parse/sse.go`, `internal/parse/sse_test.go` (create)
- `internal/parse/types.go` (create)
