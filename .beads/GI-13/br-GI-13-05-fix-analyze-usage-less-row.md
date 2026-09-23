# Bead br-GI-13-05: `ruleCachePrefixInvalidation` stops aborting on a usage-less row — a new `rowsWithUsage` predicate

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §3.1 (C1 class 3), §5 D9, §6 test 5,
§8

- **Bead ID**: br-GI-13-05
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-13-01 (its pipe-level case asserts the batch-end pass that bead creates)
- **Blocks**: br-GI-13-06

> **This is the story's one rules-semantics change, and it is deliberate.** Without it, C1 (br-GI-13-01)
> would unmask a *pre-existing* fragility in `ruleCachePrefixInvalidation` and ship a **silent,
> permanent loss** of a true finding (§3.1 class 3, D9). The accepted cost is stated, not hidden: the
> new predicate also changes findings for existing sessions that a usage-less row has already disabled.

## Description

`ruleCachePrefixInvalidation` (`rules.go:294-308`) walks consecutive-row pairs and **`return nil` on
the first violating pair** (`:301`), aborting the rule for the whole session instead of skipping that
pair. A usage-less row breaks the walk both ways:

- a usage-less row has `TotalPromptTokens == 0` (the store recomputes the column from the token
  columns at `store.go:265`, so a 429 or a truncated body lands one) and aborts as a zero `prev`;
- a usage-less row arriving *behind* a full-rewrite run aborts it as a `cur` whose write is zero.

So for `[P1,P2,P3,Z]` (three full rewrites then a usage-less `Z`), a batch-end pass reaches the
`(P3,Z)` pair, returns `nil`, and writes nothing — where today's write-early pass after `P3` wrote a
finding. Today's never-retract behaviour masks this; C1 removes the mask.

### The fix: give the rule its sibling's shape

Add a **new** `rowsWithUsage` predicate mirroring the existing `rowsWithRequestBody`
(`rules.go:324-331` — the model for exactly this, doc comment and all), and filter the row set
*before* the walk in `ruleCachePrefixInvalidation`:

```go
rows = rowsWithUsage(rows)
```

- **Filter on usage, not on the body.** The sibling filters `ReqBody != ""`; this rule wants
  usage-carrying rows including JSONL ones, which structurally never carry a request body. So the
  predicate keeps rows with `TotalPromptTokens != 0` (or the equivalent usage test), not a body test.
- **Do not change the walk or its abort semantics.** The filter changes *which rows are walked*; the
  `len(rows) < 3` guard and the pair walk are unchanged. The runnable behaviour of the rule's actual
  condition (`write < 0.5*prevTotal`) is already correct and must not change.

### D9's direction, so it is checkable

`[P1,P2,Z,P3]` in `started_at` order with a usage-less `Z`: today the walk aborts at `(P2,Z)`
(`rules.go:301`) and the session gets **nothing** (the `len(rows) < 3` guard means the earlier two-row
state never fired either). Shipped, `rowsWithUsage` filters `Z`, the walk is `[P1,P2,P3]`, and it fires
on `P3`. For the `[P1,P2,P3,Z]` shape the anchor lands on `P3` — the same row today's pass-after-`P3`
anchors on — so the shipped output **reproduces** today's. That is class 3's neutrality.

### Documentation correction to carry (F8.1 — a carried-open MINOR)

The plan's §3.1 class-3 caveat contains two sentences that **over-generalize** the class-2/class-3
boundary. They were deliberately left unfixed in the plan to stop a prose-edit regress, and the plan's
review artifacts are gitignored and will not travel — so **this bead carries the exact correction**,
because this is where the classification is actually implemented. Frame it as a correction to any
**doc comment the implementer writes** explaining the boundary, or to the rule's own comment. **The
runnable behaviour is already correct; do not change it.** The correction, verbatim:

> Scope "a real row follows the usage-less one" to "a real row **that continues the rewrite run**
> follows it" — false as written when the following row is a **read**, because `rowsWithUsage` keeps a
> read (it carries usage), the filtered pair still fails, the walk returns nil, and the finding is
> class 1, not class 2. And soften "class 3 is **exactly** the case where the suppressed finding has no
> re-anchored successor" to "…is **the case it demonstrates**" — class 1's losses also have no
> successor.

## Rationale

Without `rowsWithUsage`, C1's single final pass would permanently drop a real warning for any session a
usage-less row touches — a user-visible regression (§3.1). The fix is bounded to one rule: no other
rule is modified, none is added or removed, and no threshold moves (§8). Mirroring the sibling
predicate is the repo's own established shape, so the change reads as the rule joining its neighbour
rather than a new mechanism.

## Outcome Definition

- `rowsWithUsage` exists in `internal/analyze/rules.go`, documented and modelled on
  `rowsWithRequestBody`; it keeps usage-carrying rows and is **not** a body predicate.
- `ruleCachePrefixInvalidation` filters with it *before* the walk; the walk, the `len(rows) < 3` guard
  and the `write < 0.5*prevTotal` condition are otherwise unchanged.
- Over `[P1,P2,P3,Z]` the rule returns the `cache_prefix_invalidation` finding, anchored on `P3` (the
  last usage-carrying row) — not on `Z`, not nil.
- A session where a real turn reads from cache instead of rewriting still **declines** the finding.
- A batch of those four rows writes the warning through a **single** batch-end pass (the pipe-level
  half, on top of br-GI-13-01).
- The F8.1 correction is present in the rule's explanation (comment/doc), and no runnable behaviour was
  changed for it.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/analyze/ ./internal/consumer/`.

## Test Specifications

- Unit Tests (`internal/analyze/rules_test.go`):
  - `TestRuleCachePrefixInvalidationSurvivesAUsageLessTail` (§6 test 5, rule level): `[P1,P2,P3,Z]` —
    three full rewrites then a usage-less row with zero `TotalPromptTokens` (as a 429 or a truncated
    body produces) — returns the `cache_prefix_invalidation` finding **anchored on `P3`**, the last
    usage-carrying row. That anchor is the point: it is the same row today's pass-after-`P3` anchors on.
  - `TestRuleCachePrefixInvalidationStillDeclinesARealCacheRead` (§6 test 5): a session where a real
    turn **reads** from cache instead of rewriting returns nil — the `write < 0.5*prevTotal` disjunct
    is the rule's actual condition and must survive the filter. **The fixture must carry at least three
    usage-carrying rows**, or the rule returns `nil` from its `len(rows) < 3` guard (`rules.go:295`)
    before reaching the disjunct and the case passes for a reason it does not claim — the same
    "not a negative-only assertion" bar §6 test 1 sets.
- Unit Tests (`internal/consumer/consumer_test.go`):
  - `TestBatchEndPassWritesTheUsageLessTailFinding` (§6 test 5, pipe level): a batch of `[P1,P2,P3,Z]`
    for one session writes the warning through the **single** batch-end pass. Together with the
    rule-level case it fails if `rowsWithUsage` is dropped, if the walk reverts to aborting on a
    usage-less pair, or if the anchor lands on `Z`.
- Integration Tests: none.

## Files to Touch

- `internal/analyze/rules.go` (modify — add `rowsWithUsage`; filter in `ruleCachePrefixInvalidation`
  (`:294`); the doc/comment correction)
- `internal/analyze/rules_test.go` (modify — the two rule-level cases)
- `internal/consumer/consumer_test.go` (modify — the pipe-level case; also edited by br-GI-13-01/03,
  which this bead depends on — keep to this bead's own region)

No other bead touches `internal/analyze`, so the rule change is isolated. The `internal/consumer`
pipe-level case is the only shared-file overlap, ordered by this bead's dependency on br-GI-13-01.
