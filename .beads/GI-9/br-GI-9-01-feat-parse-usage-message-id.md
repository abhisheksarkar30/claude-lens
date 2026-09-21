# Bead br-GI-9-01: `parse.Usage` gains `MessageID`, from both response shapes, gated on the body's own `type`

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §3 D1 and D2, §4 (the
`internal/parse` rows), §5 (`internal/parse`), §6 (the `message.id` collision bullet), §9 bead 01

- **Bead ID**: br-GI-9-01
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: br-GI-9-02, br-GI-9-07

## Description

`parse.ExtractUsage` already visits the frames this needs. Add the upstream message id to `Usage`:

- `MessageID string` on `Usage` (`internal/parse/types.go`).
- `ID string` on `sseMessageStart.Message` (`internal/parse/usage.go`) — the streaming shape's
  `message_start.message.id`.
- `ID string` and `Type string` on `nonStreamBody` (`internal/parse/usage.go`) — the non-streaming
  shape's top-level `id` and its own `type`.

That is four struct declarations across three structs plus two assignments. **No new pass over the
body and no new decode** — the extractor already decodes and walks these frames.

**Two assignments, both guarded on "still empty".** `usageFromFrames` folds every frame, so without
a guard the **last** `message_start` visited would win silently. Assign `u.MessageID` only when
`u.MessageID == ""`, in the `message_start` case and in the `message` case. The `message` case is the
non-stream body's synthetic frame and is **also reachable through the SSE path** — `parseEvent`
resolves a frame's type from the payload's own `type` when there is no `event:` line — so the guard
belongs on the **assignment**, not on the caller. The first `message_start` is the message the
response opened with; a later one is a re-emission (a gateway or retry artefact, or a concatenated
stream) and keying the row on its id would mis-key it.

**The non-stream `id` is read only when the body's own `type` is `"message"` — and the guard must
read the body's `type`, not the frame's.** `NonStreamFrame` synthesises `Type: "message"` for every
`application/json` body, so a guard phrased against the frame type would admit everything. That is
why `nonStreamBody` needs the extra `Type` field rather than reusing the type the extractor already
routes on.

The gate buys **shape**, not per-request-ness. It stops a body that is not message-shaped from
contributing an id at all. What it does **not** prove is that a message-shaped body's top-level `id`
is per-request — a stable org or gateway id carries `type: "message"` too — so that property rests on
§6's base rate and on §5's integration assertion that one id yields one row, **not on this gate**.
Say so in the field's doc comment rather than overstating the guard.

## Rationale

The upstream mints a message id, the proxy already stores it inside the response body it keeps, and
Claude Code writes the same value into the transcript as `message.id`. It is the only identity that
exists on **both** sides on this install: the documented `request-id` header is never sent by this
upstream — **0 of 728** stored responses carry one. Without this field the cold path has nothing to
key a proxy row on, and D1's merge never fires.

This is the story's first bead because everything forward-facing depends on the value being
extractable. It is also the value the **backfill** must re-read (br-GI-9-04), which is why D2 insists
the precedence live in one place and why the field's doc comment is part of the deliverable.

## Outcome Definition

- `Usage.MessageID` is populated from a streaming body's `message_start.message.id` and from a
  non-streaming body's top-level `id` whose `type` is `"message"`.
- A non-streaming body with a top-level `id` but a `type` other than `"message"` yields no
  `MessageID` — the shape gate.
- A body carrying two `message_start` frames yields the **first** id.
- Neither shape present, or no id in the body, yields `""` — no panic, no error.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

- Unit Tests (`internal/parse/usage_test.go`):
  - a streaming SSE fixture whose `message_start` frame carries `message.id` → `MessageID` set to it.
  - a non-streaming fixture with a top-level `id` and `type: "message"` → `MessageID` set.
  - a non-streaming body with a top-level `id` but a `type` that is **not** `"message"` → **no**
    `MessageID`. Assert this explicitly: the synthesised frame type would otherwise wave it through,
    which is the whole reason the guard reads the body's own type.
  - **two `message_start` frames carrying different ids → the first wins.** This is the rule D2
    states for the case the measured population (0 bodies with two ids) cannot exclude, so only a
    fixture can pin it.
  - both shapes with no id at all (an error body, `{"type":"error",…}`) → empty.
  - the existing usage assertions (`IsStream`, the input/output/cache columns, the config cap) are
    unchanged.
- Integration Tests: none — no route, no schema change.
- E2E: none.

## Files to Touch

- `internal/parse/types.go` (modify — `Usage.MessageID`, with a doc comment stating the first-seen
  precedence and the gate's limit)
- `internal/parse/usage.go` (modify — `sseMessageStart.Message.ID`; `nonStreamBody.ID` and
  `nonStreamBody.Type`; the two guarded assignments; the non-stream tier gated on the body's own
  `type`)
- `internal/parse/usage_test.go` (modify — the cases above)
