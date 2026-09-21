# Bead br-GI-9-02: identity precedence is one `requestID` function in the consumer; the proxy stops deciding it

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §3 D2, §4 (the `sink.go`,
`consumer.go`, `proxy.go` and `proxy_test.go` rows), §5 (`internal/consumer`, `internal/proxy`),
§9 beads 02 and 03 (folded here)

- **Bead ID**: br-GI-9-02
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-9-01 (the `Usage.MessageID` value this function keys on)
- **Blocks**: br-GI-9-04

> **Why §9's beads 02 and 03 are one bead here.** The sink field rename is **one compile-breaking
> change**: `CapturedCall.RequestID` becomes `RequestIDHeader`, which breaks the **producer**
> (`proxy.go`'s `submit`, which populates it) and the **reader** (the consumer, which consumes it) in
> the same commit. Landing either half alone leaves a tree that does not build. What sits between the
> two halves — *who decides identity* — is a single design choice (D2), not two. The fold is stated
> because the deleted set folded them for the same reason and §9's sketch lists them apart.

## Description

D2 moves the identity rule out of the hot path and into one function, because the answer now depends
on a **parsed response body** and `internal/proxy` is a tee by design
(`docs/context/decisions/002-hot-path-never-parses.md`) — it must not parse. The consumer already
decodes and extracts usage from that same body two lines earlier.

**One function, readable top-to-bottom:**

```go
func requestID(call *sink.CapturedCall, usage parse.Usage) string {
    if call.RequestIDHeader != "" { return call.RequestIDHeader } // the upstream header, when sent
    if usage.MessageID != "" { return usage.MessageID }           // the upstream message id
    return syntheticRequestID(call)                               // a key unique to this attempt
}
```

### `internal/sink/sink.go` — the rename

`CapturedCall.RequestID` → `RequestIDHeader`. Its doc comment calls the field "the cross-source dedup
key", which D2 makes false, and the name would otherwise invite back the two-tier split D2 deletes.
Correct the comment. The rename has exactly three sites: the **field** (`sink.go:61`), the
**producer** at `internal/proxy/proxy.go:254`, and the single **reader** at
`internal/consumer/consumer.go:403`. `internal/sink`'s own test does not name the field, so the sink
package needs no test change.

### `internal/consumer/consumer.go` — the policy

`buildEvent` calls `requestID(call, usage)`. The synthetic fallback **moves here with its counter**:
a package-level `*uint64` incremented per `requestID` call supplies the `attempt` component, keeping
the deleted code's reason — a server rebuilt mid-process still never reuses a disambiguator, and
`started_at_ns` alone is not sufficient because a fast enough retry could share a nanosecond
timestamp. Keep the exact `proxy:<sha256(body)>:<started_at_ns>:<attempt>` shape, its `attempt`
counter, and its rationale (the deleted code at `internal/proxy/proxy.go:145-152`, `:259-267`).

### `internal/proxy/proxy.go` — three deletions, named so the diff is machine-checkable

- the **method** `captureState.requestID` (`:268-291`, its doc comment `:259-267`);
- the per-capture **field** `captureState.fallbackSeq` (`:210`, assigned at `:126`, unused once the
  method goes);
- the package-level **counter** `fallbackSeqCounter` (`:145-152`).

`submit`'s `RequestID:` argument (`:254`) passes the header value it saw —
`respHeaders.Get("Request-Id")`, or `""` — instead of resolving a key.

**The nil-`*boundedBuffer` dereference guard inside the deleted method is deleted, not moved.** The
consumer's fallback hashes `call.ReqBody`, a `[]byte` for which `sha256.Sum256(nil)` is defined, so
after D2 there is nothing to guard. A port of that guard would compile and pass while covering
nothing.

### Why one place, not two (D2's three reasons)

1. The answer depends on a **parsed body**, and the hot path must not parse (decision 002).
2. Splitting the policy — hot path synthesizes, cold path overrides when it can — means the synthetic
   namespace has to be **sniffed** to know which tier applies. One function has no such question.
3. It **removes work from the hot path**: today every header-less call hashes its whole request body
   merely to build a fallback key. After this, it hashes nothing.

*Rejected alternative:* leave `captureState.requestID` in place and have the consumer prefer
`usage.MessageID` whenever the stored key starts with `proxy:`. Smaller diff, but it leaves the
identity rule expressed in two packages and makes a string prefix load-bearing for correctness.

## Rationale

D1's key change needs the proxy to produce the body id, and the body is only parsed in the cold path.
This is the plan's most important structural choice, and it is a **deletion** from the hot path: the
proxy loses a function and gains nothing, and the identity rule becomes readable in one function
instead of inferable from two namespaces.

## Outcome Definition

- `requestID` returns the header value when present, else the body id, else a well-formed
  `proxy:<sha256>:<started_at_ns>:<attempt>` key.
- `CapturedCall.RequestIDHeader` carries the raw header value, or `""`.
- `internal/proxy` resolves no key: the method, the per-capture field and the package counter are
  gone; `submit` passes `respHeaders.Get("Request-Id")` (or `""`).
- Two byte-identical bodies in one process produce distinct synthetic keys.
- `TestNoBufferingSSE` passes unchanged — the hot path still does not buffer.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

- Unit Tests (`internal/consumer/consumer_test.go`) — the precedence table, all three tiers:
  - a header value wins over a body id;
  - a body id wins over the synthetic key;
  - no header and no body id yields a synthetic key of the documented shape;
  - the property the `attempt` counter exists for: two byte-identical bodies in one process never
    collapse;
  - the `off`-policy shape: a call with no header, no body id **and** a nil body still yields a
    well-formed synthetic key (this is where the consumer's `off` coverage lives — not in a port of
    the deleted proxy assertion).
- Unit Tests (`internal/proxy/proxy_test.go`) — three tests, three fates:
  - `TestHashFallbackTwoAttemptsProduceDistinctIDs` **moves** to `internal/consumer` with the code it
    exercises;
  - `TestResponseDerivedRequestIDWins` **changes meaning**: it now asserts the **raw header value**
    passes through, not that it is the dedup key;
  - `TestPolicyOffSurvivesAMissingRequestID`'s synthetic-key assertion
    (`strings.HasPrefix(call.RequestID, "proxy:")` at `:456-458`) is **deleted** — it reads the
    renamed field, which is `""` on these nil-header paths, so it goes red rather than moving. The
    test's surviving half is its `off`-policy no-panic / no-lost-row coverage.
- Integration Tests: none — no route, no schema change.
- E2E: none.

## Files to Touch

- `internal/sink/sink.go` (modify — rename the field to `RequestIDHeader`; correct its doc comment)
- `internal/consumer/consumer.go` (modify — `requestID(call, usage)`, `syntheticRequestID` and its
  package-level `*uint64` counter, `buildEvent` uses it)
- `internal/consumer/consumer_test.go` (modify — the precedence table, the two-identical-bodies case,
  the nil-body synthetic key)
- `internal/proxy/proxy.go` (modify — the three deletions; `submit` passes the header value)
- `internal/proxy/proxy_test.go` (modify — the three tests' fates)
