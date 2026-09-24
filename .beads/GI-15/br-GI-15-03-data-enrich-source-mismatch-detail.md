# Bead br-GI-15-03: enrich source_mismatch's Warning.Detail with the actual differing fields and values

**Plan Reference**: `docs/planning/GI-15-deepseek-capture-gap.md` — Addendum "Findings" §2 first
bullet, "What changes" table row 1 (br-GI-15-03), "Test strategy" bead 3, "Risk areas / edge
cases" first bullet, self-review (QA), "Rationale for keeping Kind: source_mismatch".

- **Bead ID**: br-GI-15-03
- **Priority**: P2 (medium — diagnostics only, no behavior change to what gets flagged or how
  severely; unblocks future investigation of the addendum's open SSE-timing hypothesis)
- **Original Estimate**: 1.5h
- **Dependencies**: None
- **Blocks**: None (independently implementable and independently revertible from br-GI-15-04,
  per the plan's own QA self-review)

## Description

`internal/store/merge.go`'s `applyMergeTx` (currently lines 134-151) builds a `source_mismatch`
warning whose `Detail` (line 143) reads only:

```go
Detail: fmt.Sprintf("sources %s and %s disagree on token counts for request_id %s", existing.Source, incoming.Source, existing.RequestID),
```

This names neither *which* of the six compared token fields differ nor their two values. The
mismatch condition itself is computed a few lines earlier in `mergeEvents` (`merge.go:168-173`)
as an inline `||` chain over exactly six fields:

```go
tokensDiffer := existing.InputTokens != incoming.InputTokens ||
	existing.OutputTokens != incoming.OutputTokens ||
	existing.CacheWrite5mTokens != incoming.CacheWrite5mTokens ||
	existing.CacheWrite1hTokens != incoming.CacheWrite1hTokens ||
	existing.CacheReadTokens != incoming.CacheReadTokens ||
	existing.ThinkingTokens != incoming.ThinkingTokens
```

### The fix

1. **Extract a shared field table** naming the same six fields `tokensDiffer` compares, e.g.:

   ```go
   // tokenFields lists every token column a merge compares for disagreement.
   // tokensDiffer and diffTokenFields both iterate this one table so the two
   // can never drift into checking different fields.
   var tokenFields = []struct {
   	name string
   	get  func(*Event) int
   }{
   	{"input_tokens", func(e *Event) int { return e.InputTokens }},
   	{"output_tokens", func(e *Event) int { return e.OutputTokens }},
   	{"cache_write_5m_tokens", func(e *Event) int { return e.CacheWrite5mTokens }},
   	{"cache_write_1h_tokens", func(e *Event) int { return e.CacheWrite1hTokens }},
   	{"cache_read_tokens", func(e *Event) int { return e.CacheReadTokens }},
   	{"thinking_tokens", func(e *Event) int { return e.ThinkingTokens }},
   }
   ```

   The `get` function's return type is `int` — matching the actual declared Go type of all six
   fields on `Event`/`EventSummary` (`internal/store/types.go`), not `int64`. A signature mismatch
   here will not compile.

2. **Rewrite `tokensDiffer`** in `mergeEvents` to iterate `tokenFields` instead of the inline `||`
   chain, so the comparison and the new detail-builder read the same list by construction (this is
   the load-bearing part — a bespoke helper with its own independent six-field enumeration would
   satisfy the acceptance test today while silently drifting from `tokensDiffer` the next time
   either list is edited alone).

3. **Add an unexported helper**, e.g.:

   ```go
   // diffTokenFields returns a human-readable list of exactly the fields in
   // tokenFields where existing and incoming disagree, each as
   // "field existing_value vs incoming_value", comma-separated. It reports
   // nothing for fields that are equal.
   func diffTokenFields(existing, incoming *Event) string {
   	var parts []string
   	for _, f := range tokenFields {
   		ev, iv := f.get(existing), f.get(incoming)
   		if ev != iv {
   			parts = append(parts, fmt.Sprintf("%s %d vs %d", f.name, ev, iv))
   		}
   	}
   	return strings.Join(parts, ", ")
   }
   ```

4. **Use it in `applyMergeTx`'s mismatch branch** to build `Detail`, e.g.:

   ```go
   Detail: fmt.Sprintf("sources %s and %s disagree on token counts for request_id %s: %s",
   	existing.Source, incoming.Source, existing.RequestID, diffTokenFields(existing, incoming)),
   ```

   Adapt wording to house style; the acceptance criterion is that the resulting string names every
   differing field with both its values and omits every field that agreed — not this exact
   sentence shape. This bead does not change `Severity` or the cross-source phrasing's overall
   framing ("sources %s and %s disagree...") — that stays as the default; br-GI-15-04 is the one
   that branches it by same-source vs cross-source. Implement this bead so it composes cleanly
   with that branch (e.g. keep `diffTokenFields`'s output as a self-contained suffix/clause that
   either phrasing can append), but do not implement br-GI-15-04's branch here.

## Rationale

This session's own investigation had to manually cross-reference a DB row against its raw JSONL
transcript line by hand to confirm an actual token disagreement, because the existing warning gave
no lead — it said only that two sources disagreed, not on what. The shared table is what keeps
this fix honest: without it, a second, independently-maintained six-field list in the same file is
one edit away from silently checking different fields than the mismatch condition it is meant to
explain.

## Outcome Definition

- `tokensDiffer` (in `mergeEvents`) and the new `diffTokenFields` helper both iterate one shared
  `tokenFields`-style table — there is exactly one place in `merge.go` that enumerates the six
  compared token fields.
- The persisted `Warning.Detail` for a `source_mismatch` names every field that actually differed,
  with both its existing and incoming values, and does not name any field that agreed.
- A mismatch on exactly one field (the common case per the sampled data) and a mismatch on more
  than one field are both handled correctly — the message is not a fixed-width template assuming
  all six are always reported.
- `Kind` stays `"source_mismatch"`; `Severity` stays `"error"` for the cross-source case (this bead
  does not touch severity or add the same-source branch — that is br-GI-15-04).
- `go build ./... && go vet ./... && go test ./internal/store/` passes, followed by
  `go test ./...` before pushing.

## Test Specifications

- Unit Tests (`internal/store/merge_test.go`):
  - Direct test of `diffTokenFields` against a hand-built pair of `Event`s differing in exactly
    one field (e.g. `OutputTokens`) — asserts the returned string names that field and both values,
    and does not name any field that did not differ.
  - Direct test of `diffTokenFields` against a pair differing in more than one field at once —
    asserts all and only the differing fields are named (do not test only the multi-field case; a
    naive single-field-only implementation would otherwise pass).
  - An end-to-end test at the `mergeEvents`/`applyMergeTx`/insert level (extending or sitting
    beside `TestMergeStillWarnsOnATrueDisagreement`, `merge_test.go:994`, following that test's use
    of `newTestStore`, `fullEvent`, and `st.InsertEvent`/`st.ListWarnings`) asserts the persisted
    `Warning.Detail` contains the same differing-field content, not just that a `source_mismatch`
    warning exists.
  - `TestMergeStillWarnsOnATrueDisagreement` itself is checked to still pass unmodified — it only
    asserts `Kind == "source_mismatch"` via a `found` bool, not `Detail` text or `Severity`, so it
    is not expected to need a change, but must be run to confirm.
- Integration Tests: none beyond the above — same-package, same-bead per the plan's Test Strategy.

## Files to Touch

- `internal/store/merge.go` (modify — extract `tokenFields` shared table, rewrite `tokensDiffer`
  to iterate it, add `diffTokenFields` helper, use it in `applyMergeTx`'s mismatch branch's
  `Detail`)
- `internal/store/merge_test.go` (modify — new `diffTokenFields` unit tests, extended/new
  end-to-end `Detail`-content test)

br-GI-15-04 also touches `internal/store/merge.go` (the same mismatch branch, for `Severity` and
the same-source phrasing) and `internal/store/merge_test.go` (a same-source fixture test). The two
beads are independently implementable in either order: this bead only changes what the `Detail`
string names for a disagreement that already existed; br-GI-15-04 only changes which phrasing/
severity is chosen based on `existing.Source == incoming.Source`. Implement br-GI-15-03 first if
sequencing is needed, so br-GI-15-04's two phrasings both build on the enriched `diffTokenFields`
output rather than the old field-free sentence.
