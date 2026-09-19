# Bead br-GI-3-01: `perMTok` accepts a decimal string, `rateExact` for zero-write rows

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D3, §4 (`internal/pricing/table.go`), §5 T5/T6, §6 R2 (plan sketch §9 bead 1)

- **Bead ID**: br-GI-3-01
- **Priority**: P1 (high)
- **Original Estimate**: 1.5h
- **Dependencies**: None
- **Blocks**: br-GI-3-03

## Description

The shipped-table constructor currently takes **integer cents** per MTok
(`perMTok(usdPerMillionInCents int64)`, `internal/pricing/table.go:17-19`). DeepSeek's
off-peak rates are **not** integers in cents: `deepseek-flash`'s cache-hit rate is
`$0.003/MTok` (0.3 cents) and `deepseek-v4-pro`'s is `$0.022/MTok` (2.2 cents). Neither is
representable, and truncating to `$0.00` would silently price every DeepSeek cache hit at
zero — the "a wrong number shown as a right one" failure invariant 5 exists to prevent.

Change `perMTok` to accept a **decimal USD-per-MTok string** and return an exact `*big.Rat`,
reusing the parsing `mtokToPerToken` already implements (`internal/pricing/pricing.go:152-158`).
`perMTok` **panics** on an unparseable literal — every caller passes a shipped-table constant,
so a bad one is a build/test failure to catch at first call, never a runtime condition to
degrade on (fail open: the panic happens at construction, not mid-request).

Update all 11 Claude rows and the 2 fast-rate rows **mechanically** — same numeric value,
new spelling (`1000` → `"10.00"`, `25` → `"0.25"`, `5000` → `"50.00"`, `200` → `"2.00"`,
`20` → `"0.20"`, `500` → `"5.00"`, `2500` → `"25.00"`, `50` → `"0.50"`, `300` → `"3.00"`,
`1500` → `"15.00"`, `30` → `"0.30"`, `100` → `"1.00"`; the fast block's `perMTok(1000)` /
`perMTok(5000)` → `perMTok("10.00")` / `perMTok("50.00")`).

Add a second constructor for models that charge **no separate cache-write fee**:

```go
// rateExact builds a shipped Rate from decimal USD-per-MTok strings, with
// cache-write rates given explicitly rather than derived as 1.25x/2x input --
// for a model like DeepSeek that bills no cache-write premium.
func rateExact(model, input, output, cacheRead, cacheWrite5m, cacheWrite1h, source string) Rate
```

`rateExact` is **added** here but first **used** in br-GI-3-03 (the DeepSeek rows); it lands
in this bead so its constructor half shares one review with the `perMTok` refactor. `rate()`
keeps its 1.25x/2x derivation for Anthropic rows.

This bead has **no behaviour change** for existing rows: every Claude rate must be bit-identical
to before.

## Rationale

The DeepSeek off-peak cache-hit rates are unrepresentable by the current constructor. This is a
load-bearing refactor, not a cosmetic one, and it is the highest-risk edit in the story (R2) —
which is why the panic is paired with two existing backstops: the shipped rates must stay non-nil
and `TestComputeSixClassesIndependently` pins `claude-sonnet-5`'s absolute total, so a mistyped
literal fails loudly. It is its own bead so the mechanical row update reviews as a mechanical move.

## Outcome Definition

- `go test ./internal/pricing/...` passes, including the existing absolute-total test for
  `claude-sonnet-5` (`TestComputeSixClassesIndependently`) unchanged.
- Every one of the 11 Claude rows and the 2 fast-rate rows produces a rate **identical** to the
  pre-refactor value (`perMTok("10.00")` equals `perMTok(1000)`).
- `perMTok("garbage")` panics; `perMTok("0.003")` returns exactly `3/1000_000_000` USD per token.
- `rateExact` builds a `Rate` whose write rates are the strings given, with no 1.25x/2x derivation.
- No production call site of `rate()` changes shape; only the integer literals become strings.
- No new non-stdlib dependency (`internal/pricing` already imports only `math/big`, `time`, `os`,
  `path/filepath`, `sort`, `strings`, `sync`, `internal/parse`).

## Test Specifications

- Unit Tests (`internal/pricing/pricing_test.go`):
  - `perMTok("10.00")` equals the old `perMTok(1000)`; `perMTok("0.25")` equals the old
    `perMTok(25)`; `perMTok("0.003")` is a non-zero exact `big.Rat` equal to `$0.003/MTok`
    per token — the value integer cents could not express (T6's constructor half; T6 proper lands
    in br-GI-3-03).
  - `perMTok("garbage")` panics.
  - Every rate in `ShippedTable()` is non-nil (R2's backstop, asserted here so it covers the
    mechanical update before the DeepSeek rows exist).
- Integration Tests: none (br-GI-3-03 consumes `rateExact`).
- E2E: none.

## Files to Touch

- `internal/pricing/table.go` (modify — `perMTok` signature, `rateExact`, mechanical row update)
- `internal/pricing/pricing_test.go` (modify — new-constructor and panic cases)
