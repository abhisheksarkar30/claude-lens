# Bead br-GI-3-03: DeepSeek shipped rows, the 2026 holiday list, `ShippedAPIModelPrefixes()`, and the `analyze` exception list

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D3/D4, §4 (`table.go`), §5 T5 (shipped half)/T6/T17(a), §6 R3 (plan sketch §9 bead 3, first half)

- **Bead ID**: br-GI-3-03
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-3-01, br-GI-3-02
- **Blocks**: br-GI-3-04

## Description

Add the three DeepSeek rows to the shipped table, ship the 2026 Chinese public holiday calendar as
the default off-peak date list, and keep `TestMinimumCacheablePrefixCoversShippedModels` green.

**The rows.** Off-peak / peak, USD per 1M tokens (off-peak is the value stored; peak is derived at
2x by br-GI-3-02's multiplier):

| Model | Cache hit (`CacheReadRate`) | Cache miss (`InputRate`) | Output | Peak |
|---|---|---|---|---|
| `deepseek-flash` | 0.003 / 0.006 | 0.15 / 0.30 | 0.60 / 1.20 | 2x |
| `deepseek-v4-pro` | 0.022 / 0.044 | 0.66 / 1.32 | 1.98 / 3.96 | 2x |
| `deepseek-v4-flash` | 0.003 / 0.006 | 0.15 / 0.30 | 0.60 / 1.20 | 2x |

`deepseek-v4-flash` is a **retired alias** that still routes to V4.1-Flash and bills at the Flash
price — it appears in the captured data, so it needs its own row.

Build each row with br-GI-3-01's `rateExact`, because **DeepSeek charges no separate cache-write
fee**: the cache-miss price is what populates the cache, and `cache_write_5m_tokens` /
`cache_write_1h_tokens` are 0 on every one of the 56,984 captured rows. Both write rates are
therefore `"0"` — the documented value, not an invention.

**Peak windows.** Every DeepSeek row carries the shipped `PeakWindow`: `Multiplier` = 2,
`Hours` = `{{1,4},{6,10}}` (01:00–04:00 and 06:00–10:00 UTC, Monday–Friday), and `OffPeakDates` =
the 2026 holiday list below. `EffectiveFrom` stays `shippedEffectiveFrom` (2025-01-01) — it is the
notional date the binary's table was compiled, not a per-row fetch time (`table.go:8-11`), so no
DeepSeek row gets a bespoke 2026 value that would falsely claim the *rate* dates from 2026.

**The 2026 holiday list** (the shipped `OffPeakDates`, exclusions from peak):

```
2026-01-01..03  2026-02-15..23  2026-04-04..06  2026-05-01..05
2026-06-19..21  2026-09-25..27  2026-10-01..07
```

Source: State Council General Office notice of 2025-11-04. Weekends inside those spans are already
off-peak, so listing full spans is redundant but harmless — and it makes the list exactly the
official holiday table, the citable artifact. Add a `ponytail:` comment naming the ceiling (this
list goes stale on 2027-01-01) and the failure direction (a missing holiday leaves a day in peak,
so it **over**-charges, never under-charges) — R5.

**Freshness (F3.1).** `ShippedTable()` must build a **fresh `*PeakWindow` per call** — never a
package-level `var` in the `shippedEffectiveFrom` style (`table.go:8-11`). `serve` runs two pricers
in one process and both call `Table()`→`reload()` when the override file's mtime moves, so a shared
window would be a data race, and `ShippedTable()` itself would report the last configured list
instead of the shipped 33. A helper that returns a fresh window (and a fresh `OffPeakDates` map)
per call is the mechanism; the reload-side write is br-GI-3-04.

**Prefixes (F2.1).** `internal/pricing` owns both shipped facts. Add
`ShippedAPIModelPrefixes() []string` returning a **copy** of `{"deepseek-"}` (a copy, matching
`AllKinds()` at `internal/analyze/kinds.go:87-91`, so no caller can mutate the package's slice).

**Per-row citation.** Each DeepSeek row carries a trailing comment naming the source, matching the
existing per-row anchor style (`table.go:43-53`): the pricing page
`https://api-docs.deepseek.com/quick_start/pricing/`, retrieved **2026-09-19**.

**Holiday exclusion is config (D4's other half).** This bead ships the *default* list; the config
key that overrides it, and the `none` sentinel, land in br-GI-3-05. Nothing here reads config.

**R3 — the `analyze` exception list lands here, not later.** Adding DeepSeek rows makes
`TestMinimumCacheablePrefixCoversShippedModels` (`internal/analyze/analyze_test.go:54-63`) fail,
because it requires *every* shipped model to have a cache minimum. Resolve it with an **explicit,
reviewable literal entry per third-party model** — not a derived predicate:

```go
// no cited cache minimum; the endpoint exposes no cache-write billing
var thirdPartyWithoutCacheMinimum = []string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash"}
```

and let the coverage test skip exactly those names.

The literal lives in **`internal/analyze/analyze_test.go`** (`package analyze`), not in
`rules.go`: it exists for the coverage test's benefit only, and production already declines for
these models — `minimumCacheablePrefixFor` reports not-ok, so `ruleCachePrefixBelowMinimum` never
fires. The literal makes that decline an explicit, reviewable claim instead of an implicit side
effect. Adding another third-party model stays a visible edit to this one literal. This does **not** widen a gap:
`minimumCacheablePrefixFor` already reports not-ok for an unknown model, so
`ruleCachePrefixBelowMinimum` declines for DeepSeek today (`internal/analyze/rules.go:108-111`).
The exception list makes that decline explicit rather than implicit. Adding another third-party
model must stay a visible edit to this literal.

**Why the exception list is in this bead, not br-GI-3-09**: the plan's §9 bead 7 owns "analyze test
scoping", but that bead lands after this one, so the suite would be red in between. Landing the
literal with the rows it names keeps every bead's outcome ("`go test ./...` passes") true.

## Rationale

These rows are the point of the story: ~56,984 captured DeepSeek calls currently contribute $0 to
every total because they are `unpriced`. The holiday list and the prefix default are the shipped
facts the rest of the story reads; keeping them in `internal/pricing` beside the rate table is what
lets D4's "nil means the shipped default" rule be uniform across both config keys. The freshness
rule is what keeps the shipped facts from being mutated by a configured loader.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass — including
  `TestMinimumCacheablePrefixCoversShippedModels` via the exception list.
- `ShippedTable()` parses end-to-end: every rate non-nil, no panic.
- The 3 DeepSeek rows carry a non-nil `Peak` window and write rates of exactly `0`; all 11 Claude
  rows have `Peak == nil`.
- DeepSeek cache-hit rates are exactly `$0.003` / `$0.022` per MTok (T6).
- Two successive `ShippedTable()` calls return **distinct** `*PeakWindow` instances with equal
  dates (F3.1's fresh-window half).
- `ShippedAPIModelPrefixes()` returns `{"deepseek-"}` and a mutation of the returned slice does not
  affect a subsequent call.
- `go vet ./...` clean.

## Test Specifications

- Unit Tests (`internal/pricing/pricing_test.go`):
  - **T5 (shipped half)**: `ShippedTable()` has 14 rows; every rate is non-nil; the 3 DeepSeek rows
    carry a non-nil `Peak`; all 11 Claude rows have `Peak == nil`; the DeepSeek
    `CacheWrite5mRate`/`CacheWrite1hRate` are `0`.
  - **T6**: `deepseek-flash` cache-hit rate is exactly `$0.003`/MTok and `deepseek-v4-pro`'s is
    exactly `$0.022`/MTok — the values the old integer-cent constructor could not express.
  - **T17(a)**: `ShippedTable()`'s DeepSeek `Peak.OffPeakDates` is the shipped 33 dates, and the
    window is a fresh instance per call (the custom-list half, T17(b), lands in br-GI-3-04).
  - `ShippedAPIModelPrefixes()` returns `{"deepseek-"}`; mutating the result leaves the next call
    unchanged.
- Unit Tests (`internal/analyze/analyze_test.go`):
  - `TestMinimumCacheablePrefixCoversShippedModels` passes with the three DeepSeek names in the
    exception literal and still fails for a hypothetical unlisted third-party row (keep the
    existing `claude-nonesuch-9` assertion).
- Integration Tests: none.
- E2E: none.

## Files to Touch

- `internal/pricing/table.go` (modify — 3 DeepSeek rows, holiday list, fresh `*PeakWindow` per
  call, `ShippedAPIModelPrefixes()`, per-row citation)
- `internal/pricing/pricing_test.go` (modify — T5 shipped half, T6, T17(a), prefixes)
- `internal/analyze/analyze_test.go` (modify — R3 exception literal)
