# Bead br-GI-3-02: `PeakWindow` + `IsPeak` + `Rate.Peak` + peak multiplier in `Compute`

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D1/D2, §4 (`internal/pricing/pricing.go`), §5 T1–T4, §6 R1 (plan sketch §9 bead 2, first half)

- **Bead ID**: br-GI-3-02
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: br-GI-3-03, br-GI-3-04, br-GI-3-10

## Description

Add the per-model time-of-day rate concept and apply it inside the existing per-class cost loop.

**D1 — peak is a property of the `Rate`, never global.** `ShippedTable()` legitimately mixes
flat-priced Anthropic rows with time-varying DeepSeek rows, so an unconditional multiply would
double every Claude cost. The window is therefore optional and per-model:

```go
// PeakWindow describes a model whose rate varies by time of day. A nil
// *PeakWindow on a Rate means the model is flat-priced.
type PeakWindow struct {
    Multiplier   *big.Rat            // peak = off-peak x Multiplier (DeepSeek: 2)
    Hours        [][2]int            // [start,end) UTC hour ranges: {{1,4},{6,10}}
    OffPeakDates map[string]struct{} // "2006-01-02" UTC dates excluded from peak
}

func (w *PeakWindow) IsPeak(at time.Time) bool
// UTC; false on Sat/Sun; false when the date is in OffPeakDates; else true
// when the hour falls in any [start,end) range.
```

`Rate` gains one field: `Peak *PeakWindow`. Nothing in this bead populates it (the shipped
DeepSeek rows land in br-GI-3-03); this bead adds the type, the method, the field, and the
arithmetic.

**D2 — the multiplier is applied to the exact per-class cost, before rounding.** Peak is applied
inside the existing per-class loop of `Table.Compute` (`internal/pricing/pricing.go:74-93`),
exactly where batch halving already sits:

```go
cost := new(big.Rat).Mul(big.NewRat(int64(class.tokens), 1), class.rate)
if batch {
    cost.Mul(cost, big.NewRat(1, 2))
}
if peak {
    cost.Mul(cost, r.Peak.Multiplier)
}
total.Add(total, roundHalfUp(cost, centsPerUnit))
```

`peak` is computed **once per call**: `r.Peak != nil && r.Peak.IsPeak(at)`. Two exact rational
multiplications, **one** rounding. Multiplying the already-rounded total instead would drift —
the identical discipline `TestComputeBatchRoundsPerClass` already guards for batch.

`Compute` already takes `at time.Time` and ignores it (`internal/pricing/pricing.go:59`); this
is the first caller to use it. **No interface signature changes.**

## Rationale

Without a per-model window, expressing DeepSeek's time-of-day billing would require an
unconditional multiply that doubles every Claude row (R1). Applying the multiplier per class
before rounding preserves the repo's exact-money discipline (`math/big.Rat` throughout, one
rounding per class) — which is why the batch-halving code is the template.

## Outcome Definition

- `go test ./internal/pricing/...` passes.
- A Claude row (`Peak == nil`) is priced **identically** at a peak instant and off peak — peak
  does not leak onto flat rows (R1's guard).
- `IsPeak` is correct at every window edge: 00:59 off, 01:00 peak, 03:59 peak, 04:00 off,
  05:59 off, 06:00 peak, 09:59 peak, 10:00 off; a Saturday and a Sunday are off at a peak hour;
  a date in `OffPeakDates` is off at a peak hour.
- On a fixture where per-class rounding and total-then-multiply differ, the per-class result is
  asserted to be the one produced; and batch halving + the peak multiplier compose on one call
  with the same exact result as the reverse order.
- No new non-stdlib dependency; no `float64` in the cost path.

## Test Specifications

- Unit Tests (`internal/pricing/pricing_test.go`):
  - **T1 — `IsPeak` boundary table**: 00:59 / 01:00 / 03:59 / 04:00 / 05:59 / 06:00 / 09:59 /
    10:00, plus a Saturday and a Sunday at a peak hour. The hour edge is the single most likely
    bug and produces plausible-looking costs.
  - **T2 — `OffPeakDates`**: a date in `OffPeakDates` is off-peak at a peak hour; removing it
    from the list makes the same instant peak. Construct a `*PeakWindow` directly (this bead
    needs no shipped row for T1/T2).
  - **T3 — peak analogue of `TestComputeBatchRoundsPerClass`**: a fixture where per-class rounding
    and total-then-multiply give *different* answers, asserted to differ; and the same fixture
    with `serviceTier="batch"` at a peak instant, so batch halving **and** the peak multiplier
    compose on one call, asserted order-independent (two exact multiplies before the single
    `roundHalfUp`). Build the `Table` as an in-test fixture with a `Peak` row so this bead is
    self-contained.
  - **T4 — no leak**: a Claude model at a peak instant prices identically to the same call off
    peak.
- Integration Tests: none.
- E2E: none.

## Files to Touch

- `internal/pricing/pricing.go` (modify — `PeakWindow`, `IsPeak`, `Rate.Peak`, peak in `Compute`)
- `internal/pricing/pricing_test.go` (modify — T1–T4)
