# Bead br-GI-7-07: README's two claims, and the recorded manual run

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §4 (the `README.md` and plan-doc rows, and the sentence at `:684-686` that puts them outside the generated tree), §10, change-history v2 (F1.11)

- **Bead ID**: br-GI-7-07
- **Priority**: P2 (medium)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-7-01, br-GI-7-02, br-GI-7-03, br-GI-7-04, br-GI-7-05 (and br-GI-7-06 only if it is kept)
- **Blocks**: None

> **Dependency note.** The manual run exercises the shipped UI against a live `clens serve`, so it can only be recorded once br-GI-7-01 … br-GI-7-05 have landed. This bead is therefore the last one on the branch, and it is a docs bead: it ships no code, and no other bead waits on it.

## Description

The plan's §4 names two documentation sites **outside** the generated `docs/context/` tree, and says
why they are named: *"`README.md` and this plan doc are outside the generated tree, which is exactly
why the refresh cannot reach them and why they are named here (F1.11)."* Nothing else in the story
repairs them, and §9's sketch has no bead for them — so without this bead both are left stating the
pre-story behaviour.

**What is explicitly *not* this bead.** The four `docs/context/` rows (`dashboard.md`,
`api-surface.md`, `storage-schema.md`, `INDEX.md`) are **Phase 5.6's REFRESH pass**, because that tree
is generated and the generator owns it. Do not hand-edit them here; a hand edit is overwritten.

### `README.md` — two claims go stale the moment the UI lands

- **The Calls-tab row (`:151`)** currently says the id link replaces the list with *"that call's full
  request and response"*. That is true of the row's metadata and **not** of headers and bodies, which
  this story is what makes reachable. State that the detail shows the request and response **headers
  and bodies**, and that a transcript-sourced row says so rather than rendering empty boxes.
- **The bodies claim (`:241-244`)** says full bodies are stored — still true, and now also **viewable
  from the dashboard**, which is the change a reader of that bullet cares about. Extend it to name the
  one distinction this story introduces: **transcript content is a reconstruction in its own columns,
  not a wire capture** (br-GI-7-06), and a transcript row carries no headers at all (br-GI-7-04). Leave
  the credential/redaction bullets above it alone.

Optional, one sentence: the dashboard paragraph (`:144-146`) may note that the proxy-mode badge sits in
the header on every tab, so a reader learns the indicator exists without opening the Sources tab.

### `docs/planning/GI-7-header-and-body-visibility.md` — §10 gets its run

§10 is a stub today (*"To be completed in Phase 5 against a live `clens serve` on a populated store"*).
Replace it with the run actually performed, against the minimum list §10 already names:

- the Calls list fetch dropping from ~12 MB to a small payload;
- the session drill-down fetch bounded the same way;
- a real captured call rendering readable request **and** response bodies;
- a truncated capture showing its marker;
- a transcript row saying *"not captured"*;
- the boot log's `checkRedaction` self-test still firing against a seeded un-redacted header;
- the badge across its states, **including** the mispointed one (reproducible by pointing
  `ANTHROPIC_BASE_URL` back at `8787`) and the **unknown** one (no `ANTHROPIC_BASE_URL` in
  `settings.json`, the printed-banner onboarding path), which must read *"not set in
  settings.json"* and never *"client elsewhere"*.

The plan file is read-only to the bead author and to the agents implementing br-GI-7-01 … br-GI-7-06;
**this bead is the one place it is written**, because §4 assigns §10 to be filled in. Record the run as
it happened, including any state that did not reproduce, rather than as §10's list hopes it will.

## Rationale

Two documents describe this tool to its next reader: the README, and the plan that will be read
alongside the beads. Both are wrong the moment the UI changes, and neither is reachable by the
generated-tree refresh — the plan says so itself, and names them for exactly this reason. A docs bead
in the GI-5 register (br-GI-5-03 was that story's docs bead) is where that work belongs.

## Outcome Definition

- `README.md`'s Calls-tab row names the headers and bodies the detail now shows, and the transcript
  row's "not captured" state.
- `README.md`'s bodies claim names that bodies are viewable from the dashboard and that transcript
  content is a reconstruction in its own columns, not a capture.
- §10 of the plan doc carries the recorded manual run, covering every item in §10's own minimum list,
  including the badge's mispointed and unknown states.
- No file under `docs/context/` is edited (Phase 5.6's REFRESH pass owns that tree).
- `go build ./...`, `go vet ./...`, `go test ./...` still pass — this bead changes no Go code, and a
  docs commit that breaks the build means something else slipped in.

## Test Specifications

- There is no unit test for prose. The verification is the **recorded manual run** in §10: each item
  in its list is either reproduced and recorded, or recorded as not reproduced with the reason.
- The README's two edited claims are checked against the shipped behaviour during that same run — the
  Calls detail showing headers and bodies, and a transcript row saying *"not captured"*.
- No `go test` is added. A `grep`-style guard over prose is not worth a test; the plan doc and the
  README are read by humans.
- Integration Tests: none.
- E2E: none.

## Files to Touch

- `README.md` (modify — the Calls-tab row at `:151`, the bodies claim at `:241-244`, optionally the
  dashboard paragraph at `:144-146`)
- `docs/planning/GI-7-header-and-body-visibility.md` (modify — §10's recorded manual run; the only
  place in this story where the plan file is written)
