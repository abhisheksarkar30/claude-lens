# Bead br-GI-7-04: The call detail renders headers and bodies, collapsed, escaped, and honest about absence

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D3, §4 (`app.js`, `style.css`, `assets_test.go`), §5 T6, §6 (the HTML-injection row, the detail-unusable-for-a-large-body row), §9 bead 04

- **Bead ID**: br-GI-7-04
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-7-03 (and br-GI-7-06 only if it is kept — this bead renders the transcript
  content that bead writes)
- **Blocks**: None

## Description

br-GI-7-03 puts a decoded response body and its completeness on the wire; nothing renders it. This
bead extends the GI-5 two-mode detail (`setCallDetail` `app.js:113-116`, the injected markup at
`:199-207`) rather than adding a new view, and it is the one place in this story where a mistake is a
real vulnerability rather than a cosmetic bug.

### `app.js` — three new sections inside the existing `<h2>Call {id}</h2>` block

**Request headers / Response headers.** The stored `ReqHeaders`/`RespHeaders` JSON blobs, parsed and
rendered as rows in the existing `kv` table shape. `[redacted]` renders as `[redacted]` — it is the
redactor's output and should be visible as such. A blob that will not parse renders as a labelled
"not parseable" line, never an empty table.

**Request body / Response body.** Each inside a `<details>` that is **collapsed by default**, with the
byte count in the `<summary>`. Fields, exactly as br-GI-7-03 emits them:

- the **request** half renders `ReqBody` (raw — the request half is not decoded);
- the **response** half renders `RespBodyDecoded`.

`RespBodyDecoded` equals the raw `RespBody` when a compressed body could not be decoded
(`RespBodyCompleteness === NotDecoded`) **and** also for a body with no `Content-Encoding`, which is
`Complete` — so "the response bytes equal `RespBody`" does **not** by itself mean "undecoded";
`RespBodyCompleteness` is what decides the marker.

**Capture-incomplete marker.** When `CaptureComplete` is false, a visible line reading the CLI's own
wording, *"incomplete (truncated, or the stream ended early)"* (`show.go:78-82`), because
`CaptureComplete` is false for **either** cause (`types.go:64-66`, `sink.go:41-44`): a body cut at the
cap **or** a stream that ended without `message_stop`. The marker must not claim the cap
unconditionally — it names the cap only when the stored body's length equals it (the one case where
the cap is knowably the cause) and otherwise says the cause is not recorded on the row. Today nothing
surfaces this at all, so a truncated capture and a complete one look identical.

**Read-path markers**, one line per state, distinct from the capture marker above. Selection order is
load-bearing:

1. `BodyCapBytes === 0` → *"response shown raw — read cap not configured"*. Selected **before**
   `RespBodyCompleteness` is consulted, so a decodable body is never labelled "would not decompress"
   merely because the cap was not wired.
2. `RespBodyCompleteness === TruncatedAtCap` → *"response truncated at the read cap of {BodyCapBytes}
   bytes"*.
3. `RespBodyCompleteness === PartialCorrupt` → *"response decoded only partially — its tail was
   corrupt"*.
4. `RespBodyCompleteness === NotDecoded` → *"response shown undecoded — it would not decompress"*.

A partial or undecoded body is never presented as complete.

**Empty is not absent, and a transcript row has two distinct states.** A `jsonl`-sourced row
(`Source === 'jsonl'`) has no *wire* bodies because transcripts have none, not because capture
failed — and it renders **no** header tables (a transcript carries no headers at all), the same
distinction `sources.go` draws for health. What it renders where the body boxes would be depends on
whether br-GI-7-06 filled the row:

- **`TranscriptContent` non-empty** → the content goes in its own collapsed section under the
  explicit label *"reconstructed from transcript — not a wire capture"*, through the same
  `bodySection`/`esc()` path as the wire bodies. It must **not** be presented as a request or a
  response body: it is one assistant message, not the request that produced it, and the label is the
  only thing keeping that provenance visible (br-GI-7-06).
- **`TranscriptContent` empty** (a row written before br-GI-7-06, or a non-assistant line — only
  assistant lines carry `Message`) → the *"not captured — transcript source"* line, and **no** empty
  boxes.

Neither state renders a request or response body box, and neither renders a header table.

### Escaping — the story's one true security surface

Bodies are arbitrary bytes from a remote endpoint rendered into `innerHTML`. Everything goes through
the existing `esc()` (`app.js:45-49`). A body containing `</script>` or `<img onerror=…>` is a live
injection path into a page that also holds a replay button that spends money.

Factor the body rendering into **one top-level function** in `app.js` (e.g.
`bodySection(label, bytes, marker)`) that emits the `<details>` block. One renderer means one place
`esc(` has to be, and it gives T6 a top-level function to slice with the file's existing `funcBody`
helper (`assets_test.go:357-368` reads a whole top-level `function name(`).

### `style.css`

`details`/`summary` styling and the marker lines, matching the existing table/kv look. No new rule is
needed to hide the detail containers — the `hidden` attribute's UA default already does that.

## Rationale

The tool's single most valuable asset — what was actually sent and what actually came back — is
unreachable from the UI today. Rendering it without the markers would trade one defect for another: a
truncated capture, a body-less transcript row and a compressed body that will not decode would each be
an absence indistinguishable from a failure, which is exactly the class of defect this story exists to
close.

The `<details>` collapse buys **paint, not DOM bytes**: a collapsed `<details>` still parses and
retains its children, and the body arrives in the single-row detail fetch either way. The byte-size
win is br-GI-7-01's projection, not this collapse. That distinction is recorded so a later reader does
not "optimize" by deferring the fetch.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass, including every existing `assets_test.go`
  test unchanged.
- Opening a captured call shows request and response header tables and two collapsed body sections
  with byte counts; expanding the response body shows readable text for a stored brotli body.
- A truncated capture shows the capture-incomplete line; a `jsonl` row with no transcript content
  shows *"not captured — transcript source"* with no header tables and no empty boxes; a `jsonl` row
  **with** transcript content shows that content under the *"reconstructed from transcript — not a
  wire capture"* label, escaped, still with no header tables and no request/response body box.
- With `BodyCapBytes` unwired, the response section reads *"response shown raw — read cap not
  configured"* and never *"would not decompress"*; with a positive cap and a valid brotli body, no
  read-path marker is drawn at all.
- Every rendered field goes through `esc()`; T6 fails if the `esc(` call is deleted from the body
  renderer, and only T6 fails.

## Test Specifications

- Unit Tests (`internal/web/assets_test.go` — T6, a **source-shape** assertion, not behavioural):
  - Slice the body-rendering function out of `app.js` with the existing `funcBody` helper and assert
    the slice wraps the body in `esc(`, with the found-and-non-trivial **vacuity guard first** (the
    slice must be non-empty and must contain the body variable), so a rename that defeats the
    extraction fails loudly instead of passing on an empty string.
  - The **mutation check**: delete the `esc(` from the body renderer and confirm this test — and only
    this test — fails.
  - Assert the read-path marker strings, the *"not captured — transcript source"* string and the
    *"reconstructed from transcript — not a wire capture"* label are all present in `app.js`, and
    that the `BodyCapBytes === 0` branch precedes the `RespBodyCompleteness` branch in the source
    slice (a positional assertion, the same technique `TestAssetsTheDetailModeFlipFollowsTheFetch`
    uses). Both transcript strings are asserted because both states are reachable on the same row
    type and a single label would silently reclassify the other.
  - **State the ceiling in the test's comment**: there is no JS runtime in this toolchain and no
    dependency is a JS engine — `assets_test.go` is regex/text over the embedded bytes — so this
    proves the escaping *call is present in the source*, not that the rendered pixels are safe. That
    is the strongest guarantee the no-build-step, no-browser-automation posture allows; it is named
    rather than implied.
- Integration Tests: none — no API, store or schema change here.
- E2E: none (no JS harness exists; `testing-and-quality.md` records "End-to-end: none").

## Files to Touch

- `internal/web/app.js` (modify — header tables, the `bodySection`-style collapsed-body renderer, the
  capture-incomplete and four read-path markers, the transcript row's two states — the
  `TranscriptContent` section and the "not captured" line — and `RespBodyDecoded`
  /`RespBodyCompleteness`/`BodyCapBytes` consumed inside `showCall`)
- `internal/web/style.css` (modify — `details`/`summary` and marker styles)
- `internal/web/assets_test.go` (modify — T6's source-shape guard)
