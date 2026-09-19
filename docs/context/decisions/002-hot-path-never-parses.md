[← INDEX](../INDEX.md)

# ADR 002: The hot path only tees bytes

**Status:** Accepted

**Context:** A proxy is in the path of every request the user's editor makes. Any work on the
client's goroutine is work the user waits for. The obvious implementation — read the stream,
decompress it, count tokens, then forward — makes the user's TTFB depend on this tool's speed and
on its correctness.

The rejected design is the natural one: parse on the way through, because that is where the bytes
already are.

**Decision:** [internal/proxy](../../../internal/proxy/) does no parsing, no decompression, and
touches no database. It tees the request and response bytes into a bounded
[internal/sink](../../../internal/sink/) and returns. Everything expensive — decompress, parse,
analyze, price, write — happens on the consumer's own goroutine in
[internal/consumer](../../../internal/consumer/).

Two things enforce it:

- **An import rule.** `internal/proxy` may import only `internal/sink` and `internal/config`. If it
  ever imports `analyze`, `store`, or `pricing`, the hot path has grown a dependency on the cold
  one. Asserted by `internal/proxy/importguard_test.go`.
- **A TTFB test.** `TestNoBufferingSSE` streams SSE slowly from a fake upstream and asserts the
  client sees its first event *before* upstream sends its last.

**Consequences:**

- The two compression libraries (`klauspost/compress`, `andybalholm/brotli`) are dependencies *because*
  decoding moved to the cold path. A claim of "one non-stdlib dependency" could not be true alongside
  decoding `br`/`zstd`.
- The sink is bounded and **drops rather than waits**. A slow consumer degrades capture; it never
  applies backpressure to the client.
- **`TestNoBufferingSSE` is a design gate, not a correctness test.** A proxy that buffered the whole
  stream would still return the correct bytes, just late — so no other test in the suite would catch
  the regression. A change that requires touching that test is a design change.
- Capture is best-effort by construction: a full sink means a lost row, not a delayed response. This
  is the same trade as fail-open ([../architecture.md](../architecture.md)).
