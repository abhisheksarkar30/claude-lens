# Bead br-GI-15-04: split source_mismatch severity/phrasing for same-source vs cross-source merges

**Plan Reference**: `docs/planning/GI-15-deepseek-capture-gap.md` — Addendum "Findings" §2 second
bullet, "What changes" table row 2 (br-GI-15-04) and its "Rationale for keeping Kind:
source_mismatch" section, "Test strategy" bead 4, "Risk areas / edge cases" second and third
bullets, self-review (QA/security), "Context docs to refresh".

- **Bead ID**: br-GI-15-04
- **Priority**: P2 (medium — corrects a genuinely misleading severity on a real, observed case;
  diagnostics/presentation only, no schema or migration)
- **Original Estimate**: 1h
- **Dependencies**: None (independently implementable from br-GI-15-03; if both land in the same
  PR, implement br-GI-15-03 first so this bead's two phrasings both build on the enriched
  `diffTokenFields` output rather than the old field-free sentence — see br-GI-15-03's "Files to
  Touch" note)
- **Blocks**: None

## Description

`internal/store/merge.go`'s `applyMergeTx` (currently lines 134-151) builds every `source_mismatch`
warning with the same phrasing and severity regardless of whether the two merging sides come from
different collectors or the same one:

```go
w := Warning{
	Kind:      "source_mismatch",
	Severity:  "error",
	Detail:    fmt.Sprintf("sources %s and %s disagree on token counts for request_id %s", existing.Source, incoming.Source, existing.RequestID),
	CreatedAt: time.Now(),
}
```

One traced live sample had `existing.Source == incoming.Source == "jsonl"` — the same collector
re-observing its own transcript with amended numbers on a later tailer pass (Claude Code can amend
a previously-written transcript line, e.g. finalizing usage once a stream that looked interrupted
actually completes; `internal/jsonlogs/jsonlogs.go:300-303` documents that a file-rotation re-read
is absorbed as a merge unconditionally, regardless of whether the re-read content matches what was
already ingested). This is a structurally different, generally benign event (a self-correction)
from a true cross-source (`proxy` vs `jsonl`) parsing disagreement, but today's code reuses the
identical "sources %s and %s disagree..." phrasing at `SeverityError` for both — reading a benign,
expected case as an urgent cross-source conflict.

### The fix

In `applyMergeTx`'s mismatch branch, branch on `existing.Source == incoming.Source`:

- **Same source** (`existing.Source == incoming.Source`): emit a distinct `Detail` phrasing that
  names the shared source and reads as a same-source re-read/self-correction, not a cross-source
  claim — it must **not** read as "sources X and X disagree" verbatim (that specific phrasing is
  the misleading part this bead removes for this case). Set `Severity: "info"`.
- **Cross source** (`existing.Source != incoming.Source`): unchanged — the existing "sources %s and
  %s disagree on token counts for request_id %s" phrasing (now enriched by br-GI-15-03's
  `diffTokenFields`, if that bead has landed) and `Severity: "error"` stay exactly as they are.

`Kind` stays `"source_mismatch"` in both cases — do **not** introduce a second `Kind`. Per the
plan's own rationale: `internal/analyze/kinds.go`'s `allKinds` table declares one static `Severity`
per `Kind`, checked by `TestReadmeKindTableMatchesAllKinds`
(`internal/analyze/readme_test.go:60-61`) against that static table, not against any individual
`Warning` row written at runtime — `warnings.severity` is a free column and nothing ties a written
row's `Severity` to the kind's declared value. `internal/store` cannot import `internal/analyze`
(the existing hardcoded `"source_mismatch"`/`"error"` string literals at `merge.go:141-142` already
demonstrate the two packages don't share Kind/Severity constants), so this branch is two plain
string literals chosen by the `Source` comparison, not a lookup into any shared table. A second
`Kind` would additionally require a new `README.md` row, a `nonAnalyzeKinds` entry, and dashboard
kind-count surface changes for what is otherwise the same conceptual finding at a different
severity — rejected as unjustified surface area for a diagnostics-only fix.

### Accepted residual risk (do not attempt to close in this bead)

A same-source pair can also arise from two independent captures that collide on a fallback-derived
key — e.g. two DeepSeek completions that happen to reuse the same `message.id` (a provider-side
anomaly `internal/jsonlogs/dedup.go:96-106` explains as the reason DeepSeek falls back to
`message.id` when no `Request-Id` header exists). In that scenario this branch would classify a
true duplicate/collision as `"info"` rather than `"error"`. This is a named, accepted low-probability
risk (it depends on the upstream provider issuing a duplicate `message.id` across two distinct
calls, outside `clens`'s control) — it was never observed in the 48 sampled rows, and this bead
changes only the same-source case's presentation, not its likelihood. Do not add detection logic
for this scenario; it is out of scope.

## Rationale

A same-source pair is a structurally different, generally benign event (a corrected re-read) from a
true two-source parsing disagreement, and today's single wording/severity conflates them,
overstating the benign case as an `"error"`. This was directly observed in one of the two live
samples this addendum's investigation traced end-to-end.

## Outcome Definition

- When `existing.Source == incoming.Source` at merge time, the persisted `source_mismatch`
  warning's `Severity` is `"info"` and its `Detail` names the shared source and reads as a
  same-source re-read/self-correction — it does not contain the literal cross-source phrasing
  "sources X and X disagree".
- When `existing.Source != incoming.Source`, the persisted warning's `Severity` is `"error"` and
  `Detail` uses the existing cross-source phrasing (enriched by br-GI-15-03's `diffTokenFields` if
  that bead has landed) — byte-for-byte unchanged behavior from before this bead for this case.
- `Kind` is `"source_mismatch"` in both cases. No new `Kind` is added anywhere (`kinds.go`'s
  `allKinds` table is unchanged by this bead).
- `TestMergeStillWarnsOnATrueDisagreement` (`merge_test.go:994`) still passes unmodified — it only
  asserts `Kind`, which is unaffected.
- `go build ./... && go vet ./... && go test ./internal/store/` passes, followed by
  `go test ./...` before pushing.
- Context docs updated in the same PR (see "Files to Touch"): `docs/context/glossary.md:21`,
  `docs/context/cost-and-quota.md:175`, and `internal/analyze/kinds.go`'s
  `nonAnalyzeKinds[KindSourceMismatch]` entry plus its two `README.md` mirrors, all broadened from
  "cross-source only" to "cross-source or same-source" phrasing. `KindSourceMismatch`'s
  `Description` string (optional polish per the plan) may also be touched but is not required.

## Test Specifications

- Unit Tests (`internal/store/merge_test.go`):
  - A same-source fixture (`existing.Source = incoming.Source = "jsonl"`, differing tokens, both
    `CaptureComplete`) asserts the persisted warning's `Severity == "info"` and that `Detail` does
    not contain the literal string `"sources jsonl and jsonl disagree"` (the whole point is that
    phrasing is misleading here).
  - A cross-source fixture (`existing.Source = "proxy"`, `incoming.Source = "jsonl"`) in the same
    test table asserts `Severity == "error"` is unchanged from before this bead — proving the
    branch both ways, not just in the new direction.
  - `TestMergeStillWarnsOnATrueDisagreement` is run and confirmed to still pass unmodified.
- Integration Tests: none beyond the above — same-package, same-bead per the plan's Test Strategy.

## Files to Touch

- `internal/store/merge.go` (modify — `applyMergeTx`'s mismatch branch: branch `Detail`/`Severity`
  on `existing.Source == incoming.Source`; `Kind` unchanged)
- `internal/store/merge_test.go` (modify — new same-source/cross-source severity test table)
- `internal/analyze/kinds.go` (modify — `nonAnalyzeKinds[KindSourceMismatch]` entry, currently
  `"store (cross-source merge)"` at `kinds.go:104`, broadened to note same-source merges are now
  also possible, e.g. `"store (cross-source or same-source merge)"`; `KindSourceMismatch`'s
  `Description` string may optionally be touched too — not required)
- `README.md` (modify — the `source_mismatch` kind table row's third column mirroring
  `nonAnalyzeKinds[KindSourceMismatch]` verbatim, currently referenced at `README.md:256` and
  `README.md:262`; `readme_test.go`'s `row[2] != by` check enforces these two stay in lockstep with
  `kinds.go`, so both must change together)
- `docs/context/glossary.md` (modify — line 21's "a second **source** arriving with disagreeing
  counts for the same id is a `source_mismatch` warning" broadened to cover a same-source re-read
  with amended counts too, e.g. "a second observation of the same id — from another source or from
  the same source re-reading with amended counts — is a `source_mismatch` warning")
- `docs/context/cost-and-quota.md` (modify — line 175's "the same `request_id` arriving from **two
  sources** with disagreeing token counts" similarly broadened to acknowledge the same-source/
  self-correction case)

br-GI-15-03 also touches `internal/store/merge.go` (the same mismatch branch, for `Detail`'s
field-level content) and `internal/store/merge_test.go`. No file conflict: br-GI-15-03 changes what
the cross-source `Detail` string names; this bead changes which phrasing/severity is chosen and
adds the doc/README/kinds.go updates that only this bead's behavior change (a new, previously
undocumented same-source case) makes necessary.
