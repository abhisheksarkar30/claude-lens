# Bead br-GI-3-10: `peak_pricing` warning via an optional `PeakComputer` interface

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D8, §4 (`pricing.go`, `consumer.go`, `jsonlogs.go`, `kinds.go`, `README.md`), §5 T11/T12 (plan sketch §9 bead 8)

- **Bead ID**: br-GI-3-10
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-3-02, br-GI-3-04, br-GI-3-07
- **Blocks**: None

## Description

A call billed at peak cost double the same call off-peak, and nothing in the row says so. This bead
adds a sibling-parity `peak_pricing` warning so the reason for a cost variance is legible per row.

Severity **warn** — matching the repo's own definition ("cost/quality/safety divergence"): a peak
call is a cost divergence from the off-peak baseline.

**1. The optional interface avoids widening any seam.** Returning peak-ness from `Compute` would
add a third return value to `Table.Compute`, `pricing.Compute`, `Loader.Compute`, and **both**
`PriceComputer` interfaces, forcing an update to every fake in `consumer_test`, `jsonlogs_test`, and
the CLI tests. Instead, a separate **optional** interface is type-asserted at the two call sites:

```go
// PeakComputer is an optional refinement of PriceComputer: a pricer that can
// also report whether a given call fell in a model's peak window. A pricer
// that does not implement it simply yields no peak_pricing warning.
type PeakComputer interface {
    PeakAt(model string, at time.Time) bool
}
```

`PriceComputer` is **unchanged** (`internal/consumer/consumer.go` and
`internal/jsonlogs/jsonlogs.go:57-60` both keep their current signature); every existing fake still
satisfies it, and no test fake needs an edit unless it wants to exercise the warning.

`Table.PeakAt` and `*Loader.PeakAt` are each three lines:

```go
func (t Table) PeakAt(model string, at time.Time) bool {
    r, ok := t[model]
    return ok && r.Peak != nil && r.Peak.IsPeak(at)
}
// (*Loader).PeakAt delegates to its current table: l.Table().PeakAt(model, at)
```

**2. Attach at the two call sites.** Both already hold `model` and `StartedAt`, so the assertion
and the attach are local:

- The consumer's pricing block (`internal/consumer/consumer.go:325-336`, where the `warnings`
  slice is built).
- **`insert`** in the JSONL tailer (`internal/jsonlogs/jsonlogs.go:366-381`) — **not**
  `buildEvent` at `:338`, which returns `(*store.Event, parse.Meta, parse.Usage)` and has no
  warnings slice to attach to (F2.7).

```go
if pc, ok := pricer.(PeakComputer); ok && ev.CostSource != "unpriced" && pc.PeakAt(ev.ModelResolved, ev.StartedAt) {
    // append the peak_pricing warning
}
```

Two conditions matter: the warning fires **only on a priced row** (an unpriced row was never
billed at any rate, so claiming it was billed at peak would be a lie — invariant 5), and only once
per row — `(event_id, kind)` is already unique, so a re-ingest upserts rather than duplicates.

**3. Kind bookkeeping** (all mechanically enforced by `internal/analyze/readme_test.go`):

- `KindPeakPricing Kind = "peak_pricing"` in `internal/analyze/kinds.go` with a `KindInfo`
  description and `SeverityWarn`, added to `allKinds`.
- Added to `nonAnalyzeKinds` as `"consumer, jsonlogs"`, since it is emitted by the capture path
  rather than by a pure per-event rule (`kinds.go:100-105`).
- A README row whose emitted-by cell is **exactly** `consumer, jsonlogs`. The test requires the
  cell to **equal** whatever `nonAnalyzeKinds` holds (`readme_test.go:67-70`) — the existing values
  are `"consumer (panic recovery)"` and `"store (cross-source merge)"`, so the new cell follows the
  *rule*, not those two spellings. A cell containing `|` or a newline fails the test.

**4. README prose — grow the enumeration, not only the numeral (F3.3).** The line at
`README.md:194-199` both counts **and** enumerates. Change "Four of these are not emitted by
`analyze` at all — `quota_window_approaching` … `cost_drift` … `source_mismatch` …
and `analyzer_panic` …" to "**Five** of these … — `quota_window_approaching` from
`internal/quota`, `cost_drift` from `internal/reconcile`, `source_mismatch` from the store's
cross-source merge, `analyzer_panic` from the consumer's panic recovery, and `peak_pricing` from
the capture path's own pricers". Changing only the numeral would leave a five-item claim over a
four-item list. **Nothing catches this**: `readme_test.go` asserts only the kind *table*
(`readme_test.go:24-89`), not this prose, so this half is verified by reading the rendered sentence.

**Noise, stated plainly**: this fires on roughly half of ~57k DeepSeek rows. `warn` is chosen for
sibling parity and because it is genuinely actionable (work can be shifted off-peak). If the
warning list becomes unusable in practice, the fix is a session-level aggregate rule (§8), not a
severity downgrade that would hide it.

**Fail open.** The attach never fails the insert or the consumer's write; a pricer without
`PeakComputer` simply yields no warning.

## Rationale

Without it, a doubled cost is only inferable from a total, never attributable to a row. The
optional interface keeps the change off every existing fake and off both public `PriceComputer`
interfaces, so the design does not erode.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `peak_pricing` fires on a peak-billed **priced** row via the `insert` path and via the consumer
  path; does **not** fire on the same row off-peak; does **not** fire on an **unpriced** row at a
  peak instant (T11).
- `readme_test.go` passes **unchanged** — proving the new kind's spelling, severity, and emitted-by
  cell all agree (T12).
- The README prose sentence reads "Five of these …" and lists five kinds including `peak_pricing`
  (verified by reading, not by the test).
- `PriceComputer` is unchanged in both packages; no existing test fake needed an edit.
- Re-ingesting the same row upserts the warning rather than duplicating it.

## Test Specifications

- Unit Tests (`internal/jsonlogs/jsonlogs_test.go`):
  - **T11 (insert path)**: a peak-instant priced DeepSeek row gains a `peak_pricing` warning; the
    same row off-peak gains none; a peak-instant **unpriced** row gains none.
- Unit Tests (`internal/consumer/consumer_test.go`):
  - **T11 (consumer path)**: the same three cases through the consumer's pricing block.
- Unit Tests (`internal/analyze/readme_test.go`):
  - **T12**: `TestReadmeKindTableMatchesAllKinds` passes unchanged with `peak_pricing` present in
    the table and `nonAnalyzeKinds`; the emitted-by cell equals `consumer, jsonlogs`.
- Unit Tests (`internal/pricing/pricing_test.go`):
  - `Table.PeakAt` / `*Loader.PeakAt` return true at a peak instant for a DeepSeek row and false for
    a Claude row / an unknown model (the interface's own contract).
- Integration Tests: none.
- E2E: none.

## Files to Touch

- `internal/pricing/pricing.go` (modify — `PeakComputer`, `Table.PeakAt`, `*Loader.PeakAt`)
- `internal/pricing/pricing_test.go` (modify — `PeakAt` contract cases)
- `internal/consumer/consumer.go` (modify — assertion + `peak_pricing` attach in the pricing block)
- `internal/consumer/consumer_test.go` (modify — T11 consumer path)
- `internal/jsonlogs/jsonlogs.go` (modify — assertion + attach in `insert`, not `buildEvent`)
- `internal/jsonlogs/jsonlogs_test.go` (modify — T11 insert path)
- `internal/analyze/kinds.go` (modify — `KindPeakPricing` + `allKinds` + `nonAnalyzeKinds`)
- `README.md` (modify — kind-table row; "Four" → "Five" prose with the enumeration grown)
