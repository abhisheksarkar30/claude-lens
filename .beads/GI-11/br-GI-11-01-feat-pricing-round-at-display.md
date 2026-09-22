# Bead br-GI-11-01: `pricing` — stop rounding every token class to a cent inside `Compute`

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §2 RC-A, §4 Code (the
`internal/pricing/pricing.go` and `internal/pricing/pricing_test.go` rows), §5 Unit
(`TestComputePricesSubCentClassesExactly`, the batch/peak rewrites, `TestComputeSixClassesIndependently`),
§6 (the `roundHalfUp` deletion row), §7 QA (the "invert, never delete" rule)

- **Bead ID**: br-GI-11-01
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: None
- **Blocks**: br-GI-11-02 (its `Store.RepriceCosts` tests assert a corrected sub-cent value), br-GI-11-10
  (the behavior docs document this rule)

> **The defect is pinned by tests that must be inverted, not deleted.** `TestComputeBatchRoundsPerClass`
> and `TestComputePeakRoundsPerClass` assert the per-class rounding as a requirement — the peak one
> asserts that an **off-peak call costs $0.00**. Deleting them leaves the behaviour unpinned and it
> comes back, which is why §7's QA paragraph makes inversion mandatory. `TestComputeSixClassesIndependently`
> is unaffected and `TestComputePeakAndBatchCompose` becomes literally true rather than true-by-rounding.

## Description

### The change

`Compute` sums **already-rounded** classes. In the per-class loop at `pricing.go:133` the total is
built with `total.Add(total, roundHalfUp(cost, centsPerUnit))`, where `centsPerUnit = 1/100`
(`:72-74`). A DeepSeek call costs roughly $0.001 split across classes — input $0.0003, output
$0.0005, cache-read $0.0002 — so **each class rounds to $0.00 on its own** and the row stores $0.00
however many tokens it carried. Measured on the live store for 2026-09-21: 2,352 of 2,426 rows stored
exactly `$0.000000` while carrying 196.68 M of the day's 201.09 M cache-read tokens. (The count was
stated as 2,352 through the plan's first ten versions; it was re-measured at the 2026-09-22 snapshot
and is 2,327. The other two figures in that sentence — 2,426 rows and 196.68 M — were correct.)

`Compute` must leave the rounding to the display path entirely:

- Replace `total.Add(total, roundHalfUp(cost, centsPerUnit))` with `total.Add(total, cost)` — the
  exact `big.Rat` per-class cost, summed. `total` stays a `big.Rat`; the single `total.Float64()`
  at the end is the only float conversion, as today.
- Delete `centsPerUnit` (`:72-74`) and `roundHalfUp` (`:173-183`), now dead. §6 records the grep:
  `roundHalfUp`'s only production reference is `:133`, the rest are comments — **the compiler is the
  check**, so a leftover caller fails the build rather than silently keeping a rounding.
- Rewrite the package doc (`:1-4`), which currently states the per-class rounding as an invariant
  ("Six token classes are priced independently and rounded per class, then summed — never rounded
  once on the total — because the per-class figure is the one an invoice line reproduces"). The
  premise is false: DeepSeek invoices per *month*, and per-call cent granularity is what destroys
  money. The new doc states the round-at-display rule: the table sums exact `big.Rat` and rounds
  nowhere; only the stored `REAL` is approximate.
- Rewrite the stale comment at `:106-109` that argues for the rounding (the "Peak is resolved once
  per call and applied per class, in the same place batch halving sits: two exact multiplications,
  one rounding" block). The two exact multiplications stay; the "one rounding" does not.

### Why the rounding leaves `Compute` entirely, not just moves

The plan's own table rules out the halfway fix: rounding **once per call** on the total still loses
70% of the money, because a single sub-cent call rounds to zero either way ($1.05 against the exact
$3.46). Only removing the rounding entirely reaches the $3.46 that DeepSeek's $3.25 corroborates.

### What this bead does not do

- **No change to `clens reconcile` or the replay gate.** `reconcile.go:57`, `:61` and `replay.go:141`
  format at `%.2f`, and the replay gate's magnitude test (`replay.go:130-143`, threshold
  `replayCostThresholdUSD = 0.25`) reads the *value*. Correcting the value changes what those paths
  display and which replays trip `--yes` — that is **intended**, not a regression: those paths
  consume the value and none depends on it being rounded to a cent (§2 RC-A). `TestReplayGateBounds`
  (`internal/cli/replay_test.go`) constructs its events with explicit costs and is unaffected.
- **No display-path change.** The CLI already formats `$%.4f` (`format.go`) and the dashboard
  already has a sub-cent branch (`app.js`), so nothing new is needed to show the corrected figures.
- **No `reprice`.** This bead fixes the value for calls priced **after** it lands; the historical
  rows are br-GI-11-02/03's job.

## Rationale

`SUM(cost_usd)` on the 2026-09-21 proxy rows reads $0.82 while clens's own captured tokens priced at
clens's own shipped rates give $3.46 — a 6% agreement with DeepSeek's invoice. The stored figure is
the outlier, and §2's arithmetic localises the whole gap to this one expression: re-running the same
rates, the same peak window and the same rounding in SQL reproduces the stored figure to the cent.
No statistical drift, no second contributor — one rounding call in a loop.

## Outcome Definition

- `Compute` (both `Table.Compute` and the `Compute` convenience entry point that delegates to it)
  returns the **exact** sum of the per-class costs, with no per-class rounding.
- `roundHalfUp` and `centsPerUnit` no longer exist in the package; `go build ./...` and
  `go vet ./...` pass (the deleted symbols have no remaining caller).
- A call whose **every** class is sub-cent prices **above zero** and equals the exact arithmetic sum.
- The batch and peak modifier tests assert **exact** values and still distinguish their modifier from
  the no-modifier case (they are modifier tests now, not rounding tests).
- `go test ./internal/pricing/` passes.
- `go test ./...` passes — in particular nothing in `internal/cli` depended on a cent-rounded value.

## Test Specifications

All in `internal/pricing/pricing_test.go`.

- **Unit Tests:**
  - `TestComputePricesSubCentClassesExactly` (**new**) — a call whose every class is sub-cent
    (e.g. 300 tokens at $10/MTok per class) returns a non-zero total equal to the exact sum. This
    is the case `TestComputePeakRoundsPerClass` currently asserts is $0.00, so it is the direct
    inversion; it must **fail** against today's code.
  - `TestComputeBatchHalvesExactly` (**rewrite of `TestComputeBatchRoundsPerClass`**) — keeps the
    halving semantics and asserts the exact halved value (1000 + 1000 tokens at $10/MTok: exact
    $0.01, not the per-class-rounded $0.02). The fixture must still fail if the halving is dropped.
  - **Peak equivalent of the above** (**rewrite of `TestComputePeakRoundsPerClass`**) — at peak the
    exact doubled value; **off peak a non-zero exact value**, replacing the current
    `if *off != 0.00` assertion at `:463-465` with the exact figure. The peak-vs-off-peak
    distinction assertion stays.
  - `TestComputePeakAndBatchCompose` (**rewrite**) — "halving and doubling cancel exactly" becomes
    literally true (exact equality of the composed figure with the plain off-peak figure), not
    true-by-rounding.
  - `TestComputeSixClassesIndependently` (**unchanged**) — whole-dollar classes, so it pins the
    thinking/subset guard and is the control that must stay green through the rewrite.
- **Integration Tests:** none. The pricing package is pure; the end-to-end effect is asserted by
  br-GI-11-03's reprice tests and §5's acceptance #1.

## Files to Touch

- `internal/pricing/pricing.go` (modify — sum the exact per-class cost at `:133`; delete
  `centsPerUnit` `:72-74` and `roundHalfUp` `:173-183`; rewrite the package doc `:1-4` and the
  now-wrong rounding comment `:106-109`)
- `internal/pricing/pricing_test.go` (modify — rewrite `TestComputeBatchRoundsPerClass`,
  `TestComputePeakRoundsPerClass` and `TestComputePeakAndBatchCompose` to assert exact values and
  invert the off-peak-$0.00 assertion; add `TestComputePricesSubCentClassesExactly`; leave
  `TestComputeSixClassesIndependently` alone)
