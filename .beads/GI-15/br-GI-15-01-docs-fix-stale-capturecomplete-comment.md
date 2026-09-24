# Bead br-GI-15-01: correct the five doc comments claiming CaptureComplete can be false because the SSE stream ended without message_stop

**Plan Reference**: `docs/planning/GI-15-deepseek-capture-gap.md` — finding 1, "What changes" table
row 1, Test Strategy "Bead 1", change history v2/v3 (F1.1, F2.1).

- **Bead ID**: br-GI-15-01
- **Priority**: P2 (medium — no user-facing behavior is wrong, but the false claim has already
  cost real investigation time and will cost more if left)
- **Original Estimate**: 45m
- **Dependencies**: None
- **Blocks**: None

## Description

Five locations carry a doc comment (one of them a runtime `Description` string) asserting that
`CaptureComplete` / capture incompleteness can be false because "the stream ended without
`message_stop`". This is not true and cannot be true: `internal/proxy` never parses the SSE
stream — that is the entire point of the hot/cold split
(`docs/context/decisions/002-hot-path-never-parses.md`) — so the flag structurally cannot be
driven by stream framing. The actual mechanism, already stated correctly in
`docs/context/storage-schema.md:116-126`, is that `capture_complete` is computed from **the two
body buffers' `truncated` bits alone**: `!respBuf.truncated && !st.reqBody.truncated`
(`internal/proxy/proxy.go:107`). Nothing else sets it false.

Correct all five locations to state that mechanism (or point at it) instead of the message_stop
claim. Do not change any runtime-visible warning/detail text or any logic — this bead is
comments and one catalogue description string only.

1. **`internal/sink/sink.go:41-43`** — `CapturedCall.CaptureComplete`'s doc comment currently
   reads:
   ```go
   // CaptureComplete is false when either body was truncated by the cap, or
   // the stream ended without message_stop -- the merge-precedence flag
   // downstream storage reads.
   ```
   Drop the `, or the stream ended without message_stop` clause. State plainly that the flag is
   false exactly when either body was truncated by the read cap, and (optionally, one sentence)
   that it says nothing about SSE framing because the hot path never parses the stream. Leave the
   following "Both bodies: ..." paragraph (lines 45-49) as is — it is already correct.

2. **`internal/store/types.go:74-78`** — `EventSummary`/`Event`'s `CaptureComplete` doc comment
   currently reads:
   ```go
   // CaptureComplete is false when either body was truncated or the stream
   // ended without message_stop -- the merge-precedence flag. Which of the
   // two bodies was cut is not recorded on the row; the detail view names
   // it by comparing each stored body's length against the read cap it was
   // configured with.
   ```
   Drop `or the stream ended without message_stop`; state the flag is false exactly when either
   body was truncated at the read cap. Leave the "Which of the two bodies was cut..." sentence
   as is.

3. **`internal/analyze/rules.go:173-182`** — the doc comment directly above
   `func ruleStreamIncomplete` currently opens with:
   ```go
   // ruleStreamIncomplete fires on a streamed response the store recorded as an
   // incomplete capture -- CaptureComplete is false exactly when a body was
   // truncated at the read cap or the SSE stream ended without message_stop.
   ```
   This overclaims the flag's own semantics (there is only one cause: cap truncation). Correct
   this opening sentence to say the flag is false exactly when a body was truncated at the read
   cap — full stop. **Do not touch the rest of the comment (lines 176-182) or the function body,
   including the `warning(...)` call's Detail text at line 188** (`"incomplete (truncated, or the
   stream ended early)"`): per `storage-schema.md:122-126`, that hedge wording is a deliberate,
   already-correct choice about what the rule can tell a user (a body under the cap that still
   never received `message_stop` is a real case the flag cannot see at all, since it wouldn't be
   truncated), not a claim about what drives `CaptureComplete`. Only the doc comment's mechanism
   claim is wrong; the display text and the rest of the reasoning are not part of this bead's
   scope.

4. **`internal/analyze/kinds.go:74`** — the `KindStreamIncomplete` entry's `Description` string
   in the warnings catalogue currently reads:
   ```go
   {KindStreamIncomplete, SeverityError, "The capture is incomplete: a body was truncated at the read cap, or the SSE stream ended before message_stop. The row does not record which."},
   ```
   Reword to state the capture is incomplete because a body was truncated at the read cap —
   drop the "or the SSE stream ended before message_stop" disjunction and the "does not record
   which" sentence, since there is only one recorded cause now that the false disjunction is
   removed. Check `internal/analyze/readme_test.go` (named in `storage-schema.md:111` as the
   test that reads this catalogue) for any assertion on this exact string before changing it.

5. **`internal/web/app.js:191-197`** — the doc comment directly above `function captureMarker`
   currently reads:
   ```js
   // captureMarker reports a capture the proxy could not finish, in the CLI's own
   // wording. CaptureComplete is false for either cause -- a body cut at the cap,
   // or a stream that ended without message_stop -- so the line names the cap only
   // in the one case where it is knowably the cause, and says so plainly when the
   // row does not record which of the two it was. Claiming the cap unconditionally
   // would be a second, quieter defect in the thing that exists to report the
   // first.
   ```
   Correct the "CaptureComplete is false for either cause" sentence to say the flag is false
   exactly when a body was cut at the cap. **Do not touch the function body (lines 211-224)** —
   its returned strings ("incomplete (truncated, or the stream ended early) — ...") are the same
   deliberate display hedge as rules.go's, out of scope here (see item 3's rationale).

### The app.js line-count dependency

`docs/context/dashboard.md:6` and `docs/context/INDEX.md:40` both pin `app.js` at exactly
**1142 lines** (confirmed current: `wc -l internal/web/app.js` → 1142). After editing item 5
above, re-run `wc -l internal/web/app.js`. If the count changed, update both occurrences of
"1142 lines" in those two docs to the new count in the same commit — this is a one-line
grep/wc-l check, not a full context-doc REFRESH pass (the plan is explicit that no other context
doc content is stale from this story).

## Rationale

`docs/context/INDEX.md`'s own refresh history names this exact defect class repeatedly: a
control (here, a doc comment) that looks like it documents a real code path (SSE framing) and
doesn't. `internal/proxy` structurally cannot observe `message_stop` — it never parses the
stream — so five places asserting it can is not a style nit, it actively misdirects the next
person debugging a capture-completeness question, as it already did in the session that produced
this plan.

## Outcome Definition

- None of the five locations claims `CaptureComplete`/incompleteness can result from the SSE
  stream ending without `message_stop`. Each states (or points at) the actual mechanism: the two
  body-cap `truncated` bits, `!respBuf.truncated && !st.reqBody.truncated`
  (`internal/proxy/proxy.go:107`).
- `docs/context/storage-schema.md`'s existing description (already correct, untouched by this
  bead) and all five corrected comments agree.
- No runtime-visible string changes except `kinds.go:74`'s `Description` (a deliberate, in-scope
  exception) — `rules.go`'s `warning(...)` Detail text and `app.js`'s `captureMarker`/
  `transcriptCapMarker` return strings are unchanged.
- No logic changes anywhere; `go build ./... && go vet ./... && go test ./...` stays green with
  no new failures, since nothing behavioral moved.
- `internal/web/app.js`'s line count is verified against `docs/context/dashboard.md:6` and
  `docs/context/INDEX.md:40`, and both are updated in the same commit if it changed.

## Test Specifications

- No new test. Per the plan's Test Strategy: "no behavior changes, so no new test — verified by
  re-reading the corrected comment against `storage-schema.md` and `proxy.go:107` for agreement."
  Run the existing suite (`go build ./... && go vet ./... && go test ./...`) to confirm nothing
  broke, including `internal/analyze/readme_test.go` in case it asserts on the `kinds.go:74`
  string verbatim.

## Files to Touch

- `internal/sink/sink.go` (modify — `CapturedCall.CaptureComplete` doc comment, lines 41-43)
- `internal/store/types.go` (modify — `CaptureComplete` doc comment, lines 74-75)
- `internal/analyze/rules.go` (modify — `ruleStreamIncomplete`'s doc comment opening sentence,
  lines 174-175 only; leave the function body and the `warning(...)` Detail text unchanged)
- `internal/analyze/kinds.go` (modify — `KindStreamIncomplete`'s `Description` string, line 74)
- `internal/web/app.js` (modify — `captureMarker`'s doc comment, lines 192-193 only; leave the
  function body unchanged)
- `docs/context/dashboard.md` (modify, conditional — only if app.js's line count moves off 1142)
- `docs/context/INDEX.md` (modify, conditional — same)

No other open bead touches any of these files; br-GI-15-02 is confined to `internal/proxy/`.
