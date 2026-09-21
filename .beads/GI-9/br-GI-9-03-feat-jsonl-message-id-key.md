# Bead br-GI-9-03: `requestKey` gains the transcript's `message.id` as its middle tier

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §3 D1, §4 (the `dedup.go` and
`dedup_test.go` rows), §5 (`internal/jsonlogs`), §2.4, §9 bead 04

- **Bead ID**: br-GI-9-03
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: br-GI-9-04

## Description

`requestKey` (`internal/jsonlogs/dedup.go:100-105`) becomes the three-tier rule D1 states:

```go
func requestKey(l *line) string {
    if l.RequestID != "" { return l.RequestID }
    if l.Message != nil && l.Message.ID != "" { return l.Message.ID }
    return "jsonl:" + l.SessionID + ":" + l.UUID
}
```

The `message` struct (`dedup.go:25-29`) gains `ID string \`json:"id"\`` — the value is already in the
line and is currently discarded.

**The precedence is deliberate and mirrors the proxy's**: the upstream header when there is one, then
the upstream message id, then the synthetic fallback. The header leads because it is the **only**
tier that can key a row whose **body carries no id** — an error body, a cap-truncated body, or a
response with no content type. Where the upstream sends both (Anthropic), the header must still win
on **both** sides or the Anthropic case would regress from working to broken; the proxy side of that
tier is br-GI-9-02's `requestID`. DeepSeek traffic sends only the body id, so both sides fall to tier
two and meet there.

**This one rule fixes both defects this story is about.** The content-block duplicates collapse
because they share a `message.id`; and the JSONL row's key becomes the exact value the proxy can
produce, so `insertOrMerge` finally takes its merge branch.

**Correct the code comment near `requestKey`** (`dedup.go:95-99`), which records the cross-source
identity assumption as "not yet verified". What is actually wrong is that it describes a fallback —
`(model, session_id, started_at ±1s, token quadruple)` — that was **never built**. The implemented
fallback is the namespaced `jsonl:<sessionId>:<uuid>` / `proxy:<sha256>:<started_at_ns>:<attempt>`
pair. (The fuller docs reconciliation — test 11(b), GI-1's §Cross-source identity — is br-GI-9-05;
this is the in-code copy, and it must say the same thing.)

## Rationale

§2.4: `dedupeAssistantLines` groups by `requestKey` and keeps the last line per key. With a real
`requestId` it collapses the per-content-block duplicates correctly. With the fallback key it does
not — the fallback embeds the line's own `uuid`, and each content-block line of one response carries
a **different** uuid, so a pair becomes two rows. Two independent measurements bound the resulting
inflation (≈32% statistically, ≈71% structurally); the middle tier is what removes it, and it is
simultaneously the key the proxy can produce, which is what lets the two writers meet.

## Outcome Definition

- `requestKey` prefers a real `requestId`, then `message.id`, then the
  `jsonl:<sessionId>:<uuid>` fallback.
- Two lines sharing a `message.id` with different uuids collapse to one row.
- Two lines with **different** `message.id`s and identical usage stay two rows.
- The code comment no longer describes a fallback that was never built.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

- Unit Tests (`internal/jsonlogs/dedup_test.go`):
  - the middle tier resolves when `requestId` is absent and `message.id` is present;
  - two lines sharing a `message.id` and differing in `uuid` collapse to one row — **the measured
    defect**;
  - the precedence still prefers `requestId` when **both** are present;
  - the existing distinct-uuid case still yields two rows, and it currently passes only
    **incidentally** — it sets no `message.id`, so its comment ("A line with no requestId falls back
    to `jsonl:<sessionId>:<uuid>`") becomes **false as a general statement**. Update the comment
    **and the fixture** to "no `requestId` **and** no `message.id`".
  - a direct **negative**: two lines with **different** `message.id`s, identical usage, the same
    session → two rows. This is the case the existing test 8 fixture does **not** exercise, since it
    shares a `requestId` rather than two distinct requests with equal tokens.
- Integration Tests: none — the end-to-end one-row case is br-GI-9-04's.
- E2E: none.

## Files to Touch

- `internal/jsonlogs/dedup.go` (modify — `ID` on the `message` struct; the middle tier in
  `requestKey`; the corrected comment)
- `internal/jsonlogs/dedup_test.go` (modify — the cases above, including the fixture that currently
  passes incidentally)
