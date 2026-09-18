# Bead br-GI-1-07: Pricing engine (6 classes, speed/tier modifiers, shipped table) + catalog

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Cost engine, §Shipped price table, §Storage schema (`prices`, `model_catalog`), tests 7, 12b

- **Bead ID**: br-GI-1-07
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-05, br-GI-1-06
- **Blocks**: br-GI-1-08, br-GI-1-09, br-GI-1-12, br-GI-1-13, br-GI-1-16, br-GI-1-17

## Description

The per-call cost arithmetic and the model catalogue. This bead must land **before** the consumer
(br-GI-1-08), because the consumer's cost step is what this bead builds.

**`pricing.Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd, costSource)`.**

Six priced token classes, because Claude bills them differently:

| Class | Rate | From |
|---|---|---|
| `input` | base input rate | `usage.InputTokens` (uncached remainder) |
| `output` | base output rate | `usage.OutputTokens` |
| `cache_write_5m` | **1.25×** input rate | `CacheWrite5mTokens` |
| `cache_write_1h` | **2×** input rate | `CacheWrite1hTokens` |
| `cache_read` | **0.1×** input rate (**0.025×** on Fable 5.1) | `CacheReadTokens` |
| `thinking` | **priced as output** | `ThinkingTokens` — a subset of output, **never added again** |

**Modifiers**:

| Modifier | Effect |
|---|---|
| `speed: "fast"` | Opus 5 / Opus 4.8 use their own rates ($10/$50 per MTok), not the standard Opus rates |
| `service_tier: "batch"` | **×0.5 on every class, applied per class *before* rounding** |
| `service_tier: "priority"` | **no rate change, and none asserted.** The skill bundle documents no per-token Priority rate — neither a premium nor "standard rates" — so the plan claims neither. The only documented facts carried: Priority Tier is unsupported on Fable 5.1 / Mythos 5.1, and Priority costs are absent from the cost report. Leave what Priority costs to the live usage endpoint's `service_tier` dimension. |

**Rounding is applied per class, then summed** — never on the total after modifiers — because the
per-class version is the one that matches an invoice line. Use `math/big.Rat` internally
(deepseek-lens's `ratOf`/`roundHalfUp`) so a rate like 1.25× cannot introduce binary-float drift
before rounding. The carried-over test's subject changes from deepseek-lens's dropped 2× peak
multiplier to the modifier that **does** exist here: port `TestComputePeakRoundsSumNotTotal` as
**`TestComputeBatchRoundsPerClass`** (test 7) — the ×0.5 is applied per class before rounding, and
rounding the discounted total instead would not match the invoice.

**`unpriced` is a first-class labelled state, never `$0.00`.** A model with no rate row returns
`costSource = "unpriced"`; br-GI-1-06 then leaves **both** cost columns NULL. The `unpriced` count is
reported alongside totals, never folded in.

**`approximate` + `cache_ttl_unknown`.** When `usage.TTLUnknown` is set (br-GI-1-05's flat-only
cache-creation shape), record the flat count as a 5-minute write and return
`costSource = "approximate"` with a `cache_ttl_unknown` note — the true rate could be 1.25× or 2×, and
the tool says which it does not know rather than picking one silently.

**Shipped price table.** Unlike deepseek-lens's deliberately-empty table, this one **ships
populated** — with a documented rate, or, for a model the first-party list carries but the bundle does
not price, a `provisional` figure with a named verification step. Every row carries `effective_from`
and `source`.

| Model | Input $/MTok | Output $/MTok | Cache read | Source |
|---|---|---|---|---|
| `claude-fable-5-1` | 10.00 | 50.00 | $0.25 (0.025×) | `shared/models.md:73` |
| `claude-fable-5`, `claude-mythos-5` | 10.00 | 50.00 | $1.00 (0.1×) | `shared/models.md:74` |
| `claude-mythos-5-1` | 10.00 | 50.00 | $0.25 (0.025×) | `shared/models.md:75` (same per-token pricing as Fable 5.1 — **cited**, not provisional) |
| `claude-opus-5`, `claude-opus-4-8` | 5.00 | 25.00 | 0.1× | `shared/models.md:76` |
| `claude-opus-4-7`, `claude-opus-4-6` | 5.00 | 25.00 | 0.1× | **`provisional`** — no rate in the bundle |
| `claude-sonnet-5` | 2.00 | 10.00 | 0.1× | `shared/model-migration.md:1291` |
| `claude-sonnet-4-6` | 3.00 | 15.00 | 0.1× | `shared/model-migration.md:1291` |
| `claude-haiku-4-5` | 1.00 | 5.00 | 0.1× | **`provisional`** — no rate in the bundle |
| any other model | `unpriced` | — | — | — |

- Cache-write rates are 1.25× (5m) / 2× (1h) on every row.
- **Cache-read rates are per-model, not per-tier.** `claude-fable-5` and `claude-mythos-5` are the
  *predecessors* of the 5.1 generation: same 10/50 input/output, but their cache reads are **$1/MTok
  (0.1×)** — four times `claude-fable-5-1`'s $0.25/MTok (0.025×). Conflating the two underprices every
  predecessor cache read by 4×, so they are separate rows. `claude-mythos-5` is an active model and
  would otherwise be `unpriced`.
- **Three models across two rows are `provisional`, not asserted**: `claude-opus-4-7`,
  `claude-opus-4-6`, `claude-haiku-4-5` carry no per-token rate anywhere in the skill bundle. Their
  $5/$25 and $1/$5 figures are historical first-party rates, so the row is marked `provisional` with a
  `source` note naming the verification step: check the Pricing URL in `shared/live-sources.md` before
  treating the row as authoritative. A provisional rate is a claim with a known gap, not a number the
  plan asserts.
- Fast-mode rates are shipped for Opus 5 / Opus 4.8 only.
- Anything not in this table is `unpriced`, never guessed — **including** Bedrock/Vertex/Foundry
  partner pricing, documented as different and explicitly out of scope for v1.

**Persistence + edits.** Mirror the effective table into the `prices` table (br-GI-1-06) for
auditability. A shipped row has `source='shipped'` (a provisional row carries that note in `source`,
so the gap is queryable, not just prose); a user edit sets `source='user'` and **wins**. Support
`--set`, `--unset`, `--edit` (the CLI wiring is br-GI-1-17, the save/validate functions live here).
A loader re-reads the file when it changes so a `clens prices --set` takes effect without a restart —
deepseek-lens's `pricing.NewLoader(pricing.DefaultPath())` shape, injected into the consumer by
br-GI-1-08/17.

**`internal/catalog`.** Refresh `model_catalog` from live `GET /v1/models`, with a shipped fallback so
an offline start still has a catalogue. Supplies context window and output cap per model, so ceiling
checks come from live data rather than a hand-maintained table. Optional: a staleness signal for a
shipped rate unverified for more than N days.

## Rationale

Using a documented rate is strictly better than `unpriced`; a provisional rate is a claim with a
known gap and is labelled as one. Per-class rounding is the choice an invoice can reproduce, and the
per-model cache-read split is the difference between pricing a predecessor cache read correctly and
underpricing it by 4×.

## Outcome Definition

- `go test ./internal/pricing/... ./internal/catalog/... -race` passes.
- `TestComputeBatchRoundsPerClass` passes: batch ×0.5 applied per class before rounding, and the
  result differs from rounding the discounted total on at least one fixture.
- A model absent from the table returns `unpriced`, and no code path turns that into `0`.
- A flat-cache-creation usage returns `approximate` with `cache_ttl_unknown`.
- `claude-fable-5` prices its cache read 4× `claude-fable-5-1`'s.
- Every shipped row has `effective_from` and a `source` naming either a skill-file citation or the
  `provisional` label + verification step.
- `--set`/`--unset` round-trip through the `prices` table and a reload picks up the edit.

## Test Specifications

- Unit Tests (`internal/pricing/pricing_test.go`):
  - **TestComputeBatchRoundsPerClass (test 7)**: per-class ×0.5 + per-class rounding; assert the sum
    differs from rounding the discounted total on a fixture that separates the two.
  - Each of the six classes priced independently; `thinking` is not double-counted into `output`.
  - `speed: "fast"` on Opus 5/4.8 uses the fast rates; on another model it does not.
  - `service_tier: "priority"` applies **no** rate change.
  - **Cache-read per-model**: `claude-fable-5-1` → 0.025×; `claude-fable-5` and `claude-mythos-5` →
    0.1×; assert the 4× relationship.
  - `claude-mythos-5-1` prices identically to `claude-fable-5-1`.
  - Unknown model → `unpriced`; `unpriced` never returns a numeric `0` that a caller could store as
    `$0.00`.
  - `usage.TTLUnknown` → `approximate` + `cache_ttl_unknown`.
  - A provisional row carries its verification note in `source`.
  - Round-trip a `--set` edit → user row wins over the shipped row.
- Unit Tests (`internal/catalog/catalog_test.go`):
  - A live `GET /v1/models` response populates `model_catalog`.
  - Offline/unreachable falls back to the shipped catalogue without error.
- Integration Tests: br-GI-1-08 asserts a priced row lands with the right `cost_source`.
- E2E: none.

## Files to Touch

- `internal/pricing/pricing.go`, `internal/pricing/pricing_test.go` (create)
- `internal/pricing/table.go` (create — the shipped table above)
- `internal/catalog/catalog.go`, `internal/catalog/catalog_test.go` (create)
