# Bead br-GI-22-03: Prefer a priced loser when the completeness winner is unpriced

**Plan Reference**: `docs/planning/GI-22-prices-set-clobber-and-rebuild-backfill-gaps.md` v8 (`<!-- version=8, status=converged -->`), §3.2, §5.4, §6, §7, §8 bead 03. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-22-03
- **Priority**: P1 (high — ships only when br-GI-22-02 records a valid `'unpriced'` result; otherwise this bead is not applicable)
- **Status**: pending
- **Original Estimate**: 90m
- **Dependencies**: br-GI-22-02
- **Blocks**: None
- **Commit**: `GI#22 fix: prefer a priced merge loser over an unpriced winner (br-GI-22-03)`

## Description

Do not start the edit below until `.beads/GI-22/evidence-02.txt` exists and its gate line has been read.

- If the gate line is `bead-03: not-applicable`, or the file is missing, or the recorded failure had `incoming.CaptureComplete` at the zero value `false`: do not edit `internal/store/merge.go`. Append a Review Notes block on this bead stating not applicable and quoting the gate line (or `evidence-02.txt missing`). Leave the commit unmade. A zero-value `false` flag is a broken fixture in br-GI-22-02, not this bead.
- If the gate line is `bead-03: start` and the evidence file says the test returned `CostSource` `'unpriced'` with `incoming.CaptureComplete` already `true`, apply the edit below. That is the only confirmation the plan accepts.

**The edit, only on `bead-03: start`.** `mergeEvents` (`internal/store/merge.go:223`) currently copies cost columns from the completeness winner (`merge.go:299-301`) after the `usageObserved` swap (`merge.go:280-290`). Stop keying the cost-column pick to `CaptureComplete` / `usageObserved` alone: after that existing swap, and before the column copies at `merge.go:292`, if `winner.CostSource == "unpriced"` and the other argument priced successfully (`CostSource` not `"unpriced"` — the fixture's priced source is `'user'`), assign `winner` to that other argument. Mirror the loser selection already used at `merge.go:281-284`. Do not redesign the `usageObserved` swap at `merge.go:280-290`; leave that block as it is.

A wholesale reassignment of `winner` in that style also copies tokens and `ModelResolved` from the new winner (`merge.go:292-306`), not the cost columns alone. That side effect is accepted. Do not add a second copy that moves only `CostUSD` / `ApiEquivalentCostUSD` / `CostSource`. The plan records the side effect and leaves the swap's shape unchanged.

`TestMergeEventsPrefersPricedOverUnpriced` (added by br-GI-22-02 in `internal/store/merge_test.go`) already asserts `CostSource` is the incoming priced source. After this edit that test passes. Do not weaken it, and do not edit the test file unless a compile break in that one test forces a fixture correction — the fixture's `CaptureComplete` and non-zero tokens are specified by br-GI-22-02 and stay.

Do not edit `docs/context/`. §11's doc corrections are not assigned to this bead.

## Rationale

If a valid fixture still returns `'unpriced'`, the completeness winner is keeping an unpriced cost on a row whose other capture priced successfully. The reproduction is the only justification for changing that pick. Writing the change when the test already returns `'user'` would override a rule the comment at `merge.go:235-249` exists to protect.

## Outcome Definition

Either:

- `evidence-02.txt` says `bead-03: not-applicable` (or the start condition is unmet), `git diff` shows no change to `internal/store/merge.go` from this bead, and Review Notes on this bead say not applicable; or
- `go test ./internal/store/ -count=1 -run TestMergeEventsPrefersPricedOverUnpriced` passes after the winner correction, and the manual negative control below was observed once.

Manual negative control, once, only when the edit landed: revert the new winner correction and re-run `TestMergeEventsPrefersPricedOverUnpriced`. It must fail by returning `CostSource` `'unpriced'` rather than `'user'`, and the fixture's `incoming.CaptureComplete` must still be `true`. A failure with a zero-value `false` flag is not this control. Restore the correction afterward. Do not commit the revert.

## Test Specifications

- `internal/store/merge_test.go`: `TestMergeEventsPrefersPricedOverUnpriced` — already specified by br-GI-22-02. This bead does not add a test. The existing assertion is the proof the correction works.
- **Manual / recorded**: the revert check in Outcome Definition, only if the edit landed. No new evidence file.

## Files to Touch

- `internal/store/merge.go` (modify — only when the gate line is `bead-03: start`; the additional winner correction described above, placed after `merge.go:280-290` and before the copies at `merge.go:292`)

## Review Notes
