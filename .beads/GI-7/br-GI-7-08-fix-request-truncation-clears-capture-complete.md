# Bead br-GI-7-08: a request body cut at the read cap clears `CaptureComplete`

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D3, §4 (the `internal/proxy`,
`internal/analyze`, `internal/cli` and `app.js` rows this bead adds), §10's *"What the run found that
no bead covers"*

- **Bead ID**: br-GI-7-08
- **Priority**: P1 (high)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-7-04 (the dashboard marker this restores), br-GI-7-07 (the run that found it)
- **Blocks**: None

> **Provenance.** This bead is not from the plan's §9 sketch. It comes from the manual run §10
> records, and it is the run's whole yield: the state the bead fixes is one no test fixture could
> reach, because every fixture in the story truncates a *response*.

## Description

`internal/proxy/proxy.go:79` submits the capture as `!respBuf.truncated` — the **response** buffer's
flag alone. The request buffer keeps its own `truncated` flag (`proxy.go:249`, set at `:260` and
`:265` on both overflow paths) and nothing ever reads it.

Measured, on the §10 run, `-body-cap-bytes 512`:

| row | request sent | `req_body` stored | `capture_complete` |
|---|---|---|---|
| 84768 | 502 bytes | 502 | 1 |
| **84769** | **1753 bytes** | **512 (the cap)** | **1** |
| 84770 | 98 bytes | 98 (response cut at 512) | 0 |

So a row holding a 512-byte *prefix* of a 1753-byte request reports a complete capture. The D3
capture marker is therefore reachable for a truncated response and unreachable for a truncated
request, and the detail page shows the prefix with its byte count and no marker — which is precisely
the *"a truncated capture and a complete one look identical"* defect this story exists to close,
surviving in the half the response-side fixtures cannot exercise.

### The fix, and the two ripples it was held back for

**1. `proxy.go:79`** becomes `!respBuf.truncated && !st.reqBody.truncated`.

`st.reqBody` is structurally non-nil at that site and needs no guard: the field is set at `:98`
before the request is sent, `onClose` can only run after the response body is closed, and
`BodyPolicy == "off"` returns at `:59` before `ModifyResponse` is installed at all — so the closure
carrying `:79` cannot exist without it. The request body is fully teed by then as well, so its flag
is final.

**2. `CaptureComplete`'s meaning broadens, and its doc must move with it.** It meant *"the response
was captured whole"*; it now means *"the capture is whole on both sides"*. `sink.go:41-44` and
`store/types.go:71-73` both describe it, and a widened flag with a narrow comment is how the next
reader gets it wrong. Neither the column nor its type changes, so there is no migration.

**3. The `analyze` ripple, decided: reword the warning rather than narrow the rule.**
`ruleStreamIncomplete` (`rules.go:172-181`) fires on `usage.IsStream && !ev.CaptureComplete` and
asserts *"the SSE stream ended without a message_stop event"*. Its own doc already claims
`CaptureComplete` is false for *either* cause — the rule has always been asserting one of two, and
the widening is what makes the over-claim reachable, because a request body over the cap is now a
second way in. The rule cannot tell the two apart and never could: the cap is runtime config and is
not on the event. So the detail becomes the disjunction it can actually justify, in the wording the
CLI and the dashboard already use, and `KindStreamIncomplete`/`SeverityError` are unchanged. This is
a wording fix the widening *forces*, not a behaviour change it causes — the rule fires on exactly the
same set of rows plus the request-truncated ones, which it should always have covered.

**4. The `merge` ripple, decided: accept it, and name it in a test.**
`merge.go:159-181` picks the token/cost winner between two rows sharing a `request_id`, preferring a
complete capture, and ORs the flag into the merged row. Under the widened meaning that rule reads
*"prefer the more complete record"*, which stays defensible. The concrete change is that a
request-truncated proxy row no longer wins against a `jsonl` row for the same request. That is
mild — the merge already records a `tokens_differ` mismatch warning when the two disagree, so the
disagreement is not lost — and it is the conservative direction: with a body known to be a prefix,
the record that is wholly captured is the safer one to quote. Asserted rather than left as a side
effect.

**5. `app.js`'s `captureMarker` must check both bodies.** `CaptureComplete` decides *whether* the
marker draws and keeps doing so — a body whose length merely equals the cap is a coincidence, not
evidence, and must not conjure a marker for a capture the proxy recorded as whole. What the length
comparison decides is *which* body the marker names, and today it compares `RespBody` alone: a
re-captured row of row 84769's shape reports a cut request body as *"the row does not record which
cause"*, a weaker statement than the data supports. It compares both and names whichever matched.

**6. The fix is not retroactive, and cannot be.** Row 84769 as it exists in the store keeps
`capture_complete = 1` — it was written by the binary before this bead — and no client-side logic
should guess otherwise from a length match, for the reason above. It stays unmarked. New rows of the
same shape are marked, and the 84,765 pre-existing rows are unaffected in both directions.

**7. `cli/show.go` needs no change.** Its marker at `:82` is already driven by `!CaptureComplete`, so
widening the flag is what makes `clens show` print it too.

### What this bead does not fix

A request body that upstream never reads in full — a `401` returned without draining the body —
leaves `truncated` false and stores a prefix with no marker, since nothing compares the stored length
to `Content-Length`. That is the same class of defect one level down, it is pre-existing, and it is
out of this bead's scope: fixing it means deciding what to do when the two disagree, which is a
design question of its own.

## Rationale

The story's D3 marker exists so that an absence is never indistinguishable from a failure. The run
found one absence that still is — in the half of the capture the story's own fixtures never
truncate, which is why eight review rounds and every test in beads 01–07 missed it. A docs bead that
recorded it and moved on would leave the marker's guarantee untrue for exactly the rows a long
conversation produces, and a long conversation is the ordinary case for a body over 256 KB.

The ripple decisions are recorded here rather than discovered in review because both were the reason
the finding was escalated rather than patched on sight: the flag feeds an analyzer warning and a
merge rule, and a bead that changed it without saying so would be trading a visible defect for a
silent behaviour change.

## Outcome Definition

- A request body over the cap produces a row with `CaptureComplete` false; a request body at or under
  the cap still produces true, and a response body over the cap still produces false.
- `clens show` prints its incomplete marker for a request-truncated row (`show.go:82`, unchanged).
- A newly captured row of row 84769's shape draws the capture marker and names the **request** as the
  body at the cap — confirmed on a live run against the store copy, not only in a fixture. The
  pre-existing row itself stays unmarked, per item 6 above.
- `ruleStreamIncomplete` no longer says the stream ended without `message_stop`; it states the
  disjunction it can justify, and still fires on the same rows.
- The merge case is asserted: a request-truncated proxy row does not win the token pick against a
  complete `jsonl` row, and the merged row keeps `CaptureComplete` true.
- `TestNoBufferingSSE` passes unchanged — this touches the hot path and the TTFB gate is the proof it
  still does not buffer.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

- Unit Tests (`internal/proxy/proxy_test.go`):
  - A request body larger than the cap yields `CaptureComplete` false, and the stored `req_body` is
    exactly the cap. The fixture posts through the real handler with a small `BodyCapBytes`, the way
    the existing proxy tests drive it.
  - A request body exactly at the cap and one under it both yield true — the boundary the fix could
    get wrong in the other direction.
  - An SSE response larger than the cap still yields false, so the response half is not regressed
    while the request half is added.
- Unit Tests (`internal/analyze/rules_test.go`):
  - A streamed row with `CaptureComplete` false produces `KindStreamIncomplete` at
    `SeverityError`, and its detail does **not** contain `message_stop` — the assertion that fails if
    the wording is left claiming a cause the rule cannot know.
  - A complete streamed row still produces nothing.
- Unit Tests (`internal/store/merge_test.go`):
  - A request-truncated proxy row and a complete `jsonl` row for the same `request_id`: the merged
    row takes the `jsonl` row's tokens and `CaptureComplete` is true. The pinning of a decision, so
    a later change to it is deliberate.
- Unit Tests (`internal/web/assets_test.go`, extending T6): `captureMarker` is asserted to compare
  **both** `ReqBody` and `RespBody` against the cap — a source-shape assertion, with T6's stated
  ceiling unchanged (no JS runtime in this toolchain).
- Integration Tests: none — no API or schema change.
- E2E: none.

## Files to Touch

- `internal/proxy/proxy.go` (modify — the submit call at `:79`, and the doc on `boundedBuffer.truncated`
  if it still reads as response-only)
- `internal/proxy/proxy_test.go` (modify — the three capture-completeness cases)
- `internal/analyze/rules.go` (modify — `ruleStreamIncomplete`'s detail and its doc)
- `internal/analyze/rules_test.go` (modify — the wording assertion)
- `internal/sink/sink.go` (modify — `CaptureComplete`'s doc, `:41-44`)
- `internal/store/types.go` (modify — `CaptureComplete`'s doc, `:71-73`)
- `internal/store/merge.go` (modify — the comment on the precedence rule, so the decision is where
  the code is)
- `internal/store/merge_test.go` (modify — the merge case)
- `internal/web/app.js` (modify — `captureMarker` names whichever body is at the cap)
- `internal/web/assets_test.go` (modify — T6's extended assertion)
