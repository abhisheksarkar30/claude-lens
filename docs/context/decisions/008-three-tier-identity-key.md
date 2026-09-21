[← INDEX](../INDEX.md)

# ADR 008: cross-source identity is a three-tier key

**Status:** Accepted

**Context:** The proxy and `internal/jsonlogs` can each capture the same call, and the merge that
should fold them into one row (`request_id` is `UNIQUE`) depends on both writers landing on the
same key. GI-1 assumed a single tier — the `request-id` response header, on the unverified
assumption that it is byte-equal to Claude Code's transcript `requestId` — with a header-less call
falling straight to a namespaced synthetic key (`proxy:<sha256(body)>:<started_at_ns>:<attempt>` /
`jsonl:<sessionId>:<uuid>`). On this install the header is never sent at all (0 of 728 stored
responses), so every call fell to the synthetic tier — and two synthetic keys, minted independently
on each side, never match. The merge that made the schema's `UNIQUE` constraint meaningful almost
never fired.

**Decision:** a middle tier, ranked between the header and the synthetic fallback: **the response
body's own `message.id`**, read out of the same body the consumer already decodes for usage. Both
writers apply the identical three-tier precedence —

```go
func requestID(call *sink.CapturedCall, usage parse.Usage) string {
    if call.RequestIDHeader != "" { return call.RequestIDHeader } // tier 1: upstream header
    if usage.MessageID != "" { return usage.MessageID }           // tier 2: upstream message id
    return syntheticRequestID(call)                               // tier 3: per-attempt fallback
}
```

mirrored on the JSONL side by `requestKey` (`internal/jsonlogs/dedup.go`). The header leads because
it is the only tier that can key a row whose body carries no id at all (an error body, a
cap-truncated body, a response with no content type — 65 of 728 rows here); tier 2 is what lets a
proxy row and its JSONL counterpart converge whenever the upstream sends a body id but no header
(DeepSeek-shaped traffic, and — as measured on this install — every Anthropic call too, since the
header is never sent here); tier 3 is unchanged from GI-1 and stays last-resort, still
per-attempt-disambiguated so two retried, byte-identical bodies produce two rows, not one.

The tier a call lands on is a property of the **upstream response**, not of the reader — both
sources read the same response, so both land on the same tier by construction. They can only ever
meet within a tier, never across one: if a proxy capture fails to record the header (an error
response with no headers stored) while the JSONL transcript saw it, the two rows land on different
tiers and stay split. That is an accepted, visible exception, not a defect — see
[docs/planning/GI-9-merge-jsonl-and-proxy-rows.md](../../planning/GI-9-merge-jsonl-and-proxy-rows.md)
§D1.

*Rejected alternative:* build the composite fallback key GI-1 originally proposed —
`(model, session_id, started_at ±1s, token quadruple)` — instead of a message-id tier. It was never
implemented; the message-id tier does the same job (converging two independently-captured rows on
one key) with a value both sides already parse out of the same response, rather than an approximate
match over several columns that could itself disagree between sources.

**Consequences:**

- The `request-id` ↔ `requestId` equivalence GI-1 named as a prerequisite (test 11(b)) is now **moot
  on this install** — the merge does not depend on it, because the message-id tier already converges
  the two sides without the header. It remains unverified and load-bearing on any install whose
  upstream *does* send the header: if that header were ever byte-unequal to the JSONL `requestId`,
  tier 1 would key the two sides apart and they would silently fail to merge (see
  `docs/planning/GI-1-claude-lens-v1.md` §Cross-source identity and `docs/acceptance.md`
  §"test 11(b)").
  - The identity resolution lives in **one function in the cold-path consumer**
  (`internal/consumer.requestID`), never in `internal/proxy` — the hot path must not parse a body to
  resolve a key ([decision 002](002-hot-path-never-parses.md)).
- `clens rekey` (br-GI-9-04) exists because this decision changes the key **historical** rows were
  captured under: rows minted before this story shipped hold a pre-D1 synthetic key even though their
  stored body now yields a real message id, so a one-off backfill re-keys them onto the tier the live
  path would have chosen.
