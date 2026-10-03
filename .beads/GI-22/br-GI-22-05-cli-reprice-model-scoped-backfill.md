# Bead br-GI-22-05: Backfill one model's unpriced rows with `reprice --model`

**Plan Reference**: `docs/planning/GI-22-prices-set-clobber-and-rebuild-backfill-gaps.md` v8 (`<!-- version=8, status=converged -->`), §3.4, §4.1, §5.1, §5.2, §6, §8 bead 05, §9. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-22-05
- **Priority**: P2 (medium — lowest priority in the set; the bead to drop first, §8. The nil-rate guard does not raise that priority)
- **Status**: done
- **Original Estimate**: 2h
- **Dependencies**: br-GI-22-04
- **Blocks**: None
- **Commit**: `GI#22 feat: backfill one model with reprice --model (br-GI-22-05)`

## Description

This is the narrower backfill, not "plain `reprice` plus this model's previously-unpriced rows." It is optional relative to the rest of GI-22 and stays the first bead to cut. The dependency on br-GI-22-04 is sequencing only: both edit `runReprice` in `internal/cli/reprice.go`. Land 04 first so this bead keeps the `!dryRun` live-serve probe 04 inserts after `openStore`. `--model` does not functionally require that probe. Do not remove the probe, and do not change its `!dryRun` gate.

**CLI flag.** `clens reprice --model <name>` is a new flag on `internal/cli/reprice.go`. Before `openStore`, take `--model` off `args` with `takeFlag` (`internal/cli/accounts.go:95-110`), the same way `--dry-run` and `--yes` are removed at `reprice.go:25-26`. `openStore` forwards the remaining args to `config.Load` (`internal/cli/format.go:198-202`), and `applyFlags`' `FlagSet` has no `model` flag (`internal/config/config.go` flag registrations around `config.go:250-253`), so a flag left on `args` is rejected and the command errors before any reprice runs. Strip `--model` even when the later refuse-without-`--yes` path returns, so it is never forwarded.

When the flag is set, pass the name through to `store.RepriceCosts`. When it is unset, pass `""` and today's full-table pass is unchanged, including the `repriceInScope` skip of `cost_source='unpriced'` (`internal/store/store.go:2139-2151`). No default behavior change.

Printed output, whenever the flag is set (dry-run and `--yes`), includes this sentence with the flag's name substituted, so a dry-run does not present any other model's row as moving:

`` `--model <name>` is backfilling previously-unpriced rows for this model only ``

The plan's example name is `claude-haiku-4-5-20251001`. Keep the existing moved/unchanged/skipped line (`reprice.go:55-56`).

**Store.** `RepriceCosts` takes no model argument today (`store.go:2229`):

```go
func (s *Store) RepriceCosts(ctx context.Context, table PriceComputer, dryRun bool) (RepriceCounts, error)
```

Add the model as a `string` parameter immediately before `dryRun`:

```go
func (s *Store) RepriceCosts(ctx context.Context, table PriceComputer, model string, dryRun bool) (RepriceCounts, error)
```

`model == ""` keeps the current unfiltered `SELECT` (`store.go:2242-2249`) and the current `repriceInScope` skip (`store.go:2280-2283`). `model != ""` adds `WHERE model_resolved = ?` with that name bound, so the one `BeginTx` (`store.go:2232-2236`) covers that model only. The comparison is verbatim equality. No prefix normalization, matching `Table.Compute`'s plain map lookup (`internal/pricing/pricing.go:94-97`).

For rows that match, bypass `repriceInScope` (`store.go:2280-2283`) only when `costSource == "unpriced"`. Every other not-in-scope label (any `approximate:<reason>` other than `cache_ttl_unknown`, per `repriceInScope` at `store.go:2139-2151`) stays skipped. Other models are not in the filtered `SELECT`: an in-scope row for a different model is not `Moved`, and an unpriced row for a different model is not updated. Do not require those other-model rows to increment `Skipped`; the pass never reads them.

A nil `usd` from `Compute` still skips and keeps the stored row (`store.go:2286-2292`). There is no store-side rate check. Subscription routing stays as it is: `billingMode == "subscription"` writes `api_equivalent_cost_usd` and leaves `cost_usd` NULL (`store.go:2203-2207` and the `UPDATE`s at `store.go:2308-2317`).

Update every existing `RepriceCosts` call to pass `""` so the package still compiles and today's tests keep the full-table pass. Call sites today: `internal/cli/reprice.go:44` and `internal/store/reprice_test.go` (first call at `reprice_test.go:64`, and every later `st.RepriceCosts` in that file). The CLI call passes the flag value instead of `""` when `--model` was set. The store must not import `internal/pricing` (the production import guard is `internal/store/importguard_test.go`; tests may import it, as `reprice_test.go` already does).

**Nil-rate guard, in `Table.Compute`, not in the store and not only on the reprice path.** `Compute` (`pricing.go:94-146`) returns `(nil, "unpriced")` for an absent model (`pricing.go:96-98`). Once the model is in the table, it multiplies every non-zero token class by that class's `*big.Rat` with no nil check (`pricing.go:130`: `new(big.Rat).Mul(..., class.rate)`). `LoadOverrides` fills a nil cache-write rate from `input_rate` (`pricing.go:295-300`) and leaves `CacheReadRate` nil. A from-scratch override that leaves a used class unset panics inside `RepriceCosts`'s transaction. `Rat.Mul` dereferences the rate; a nil `class.rate` is not a skipped class. Agentic rows of the models this ticket is about carry `cache_read_tokens`.

In the class loop, after the existing `class.tokens == 0` continue (`pricing.go:127-129`) and before the multiply at `pricing.go:130`: if `class.tokens > 0` and `class.rate == nil`, return `(nil, "unpriced")` immediately. That is the same answer as an absent model, not a partial sum and not a panic. A nil rate on a class with zero tokens stays skipped by the existing continue and must still price the other classes. The live capture path already calls this `Compute` (`internal/consumer/consumer.go:356-361`) with no `recover`. Do not edit `consumer.go`. Do not put the guard only in `RepriceCosts`. Do not edit `internal/pricing/table.go`.

**The CLI test** `TestRepriceModelFlagBackfillsOnlyNamedModel` in `internal/cli/reprice_test.go` calls `withHome(t)` before `runReprice`. Follow the seed pattern already in that file: `openTestStore` (`internal/cli/additions_test.go:29-40`), `UpsertSession`, `seedEvent` (`internal/cli/cli_test.go`), and `repriceSeedRow` (`reprice_test.go:16-29`) as the starting row. `repriceSeedRow` sets input and output tokens only and does not set `cache_read_tokens` or `BillingMode`. A case that prices those two classes and never sets `cache_read_tokens` does not satisfy the from-scratch requirement. Set the fields each case needs on the seeded `*store.Event`.

Use `--yes` so the subscription row is actually written. The output contains the limited-pass sentence for the name passed to `--model`.

Cases inside that one test:

1. **Other model, in scope, not moved.** An in-scope row (`cost_source` `shipped`, a stored cost) whose `model_resolved` is not the `--model` name. After the command its cost columns and `cost_source` are unchanged.
2. **Other model, unpriced, stays skipped.** `cost_source` `'unpriced'`, both cost pointers nil, `model_resolved` a different model. After the command it is still `'unpriced'` and both cost columns are still nil.
3. **Named model, complete-rate subscription.** `cost_source` `'unpriced'`, `billing_mode` `"subscription"`, and every used class has a non-nil rate. `cache_read_tokens == 0` when `cache_read_rate` is the unset class. Shipped `claude-sonnet-5` (`internal/pricing/table.go:164`) has non-nil input and output rates; use it as the named model for this case with `cache_read_tokens` left 0. The backfilled figure lands in `api_equivalent_cost_usd` and `cost_usd` stays NULL.
4. **Named model, from-scratch cache-read.** Seed `filepath.Join(home, ".clens", "prices.toml")` with an override for a model `ShippedTable` does not contain. `claude-haiku-4-5-20251001` is not the shipped key (`table.go:166` is `claude-haiku-4-5` only). Set `input_rate` and `output_rate` and omit `cache_read_rate`, so the loaded table contains that model and `CacheReadRate` is nil. The row's `model_resolved` is that name, `cache_read_tokens > 0`, `cost_source` `'unpriced'`, both cost pointers nil. A missing override row does not count: an absent model returns `(nil, "unpriced")` at the lookup (`pricing.go:96-98`) and never reads a rate. The command returns nil (no panic), the row stays `cost_source='unpriced'`, and both cost columns stay NULL. Those stored-row assertions do not prove the nil-rate branch ran.

**Direct `Compute` assertion,** in `internal/pricing/pricing_test.go` as `TestComputeNilUsedClassReturnsUnpriced`. Build a `Table` whose rate has `InputRate` and `OutputRate` set and `CacheReadRate` nil. `Compute` with `CacheReadTokens > 0` returns `(nil, "unpriced")` and does not panic. A second call with `CacheReadTokens == 0` and `InputTokens > 0` returns a non-nil usd, so a nil rate on an unused class is not an unpriced result. A `recover` around `RepriceCosts` does not satisfy this test. Run the named model in case 4 as whatever string the override used; the pricing test can use its own model id as long as that id is the map key and is absent from the shipped table.

Do not edit `docs/context/`. §11's doc corrections are not this bead's. Do not add a `clens backfill` subcommand. The name must not become a general "reprice unpriced rows" switch: with the flag set, rows whose `model_resolved` is not that name are outside the pass.

## Rationale

`repriceInScope` deliberately never newly prices `cost_source='unpriced'` (`store.go:2141-2146`), and `ingest --rebuild` never reaches a proxy-only row. Without `--model`, a newly priced model's already-captured history has no narrow backfill. The nil-rate guard is in `Compute` because this is the first pass that sends previously-unpriced from-scratch rows, including ones with `cache_read_tokens`, through a multiply that panics on a nil `*big.Rat`, and the live capture path calls the same function.

## Outcome Definition

`go test ./internal/pricing/ ./internal/store/ ./internal/cli/ -count=1 -run 'TestComputeNilUsedClassReturnsUnpriced|TestRepriceModelFlagBackfillsOnlyNamedModel|TestReprice'` passes. Existing `RepriceCosts` tests in `internal/store/reprice_test.go` still pass with the empty model argument (full-table behavior unchanged).

`go test ./internal/store/ -count=1 -run TestStoreDoesNotImportPricing` passes. `internal/store/store.go` does not import `internal/pricing`.

## Test Specifications

- `internal/cli/reprice_test.go`: `TestRepriceModelFlagBackfillsOnlyNamedModel` — the four cases in Description, one `runReprice` with `--model` and `--yes`, `withHome` first, override file seeded only for the from-scratch case. Assert the limited-pass sentence, the other-model rows unchanged, the subscription figure in `api_equivalent_cost_usd` with `cost_usd` NULL, and the from-scratch row still unpriced with both cost columns NULL and a nil error.
- `internal/pricing/pricing_test.go`: `TestComputeNilUsedClassReturnsUnpriced` — nil `CacheReadRate` with `CacheReadTokens > 0` returns `(nil, "unpriced")` and does not panic; the same rate with `CacheReadTokens == 0` and `InputTokens > 0` returns a non-nil usd.
- **Manual / recorded**: none. A `recover` around `RepriceCosts` is not an acceptable substitute for the `Compute` test.

## Files to Touch

- `internal/cli/reprice.go` (modify — `takeFlag` for `--model` before `openStore`; pass the name into `RepriceCosts`; print the limited-pass sentence when the flag is set; keep br-GI-22-04's probe)
- `internal/cli/reprice_test.go` (modify — add `TestRepriceModelFlagBackfillsOnlyNamedModel`)
- `internal/store/store.go` (modify — `RepriceCosts` model parameter, filtered `SELECT`, unpriced bypass only for that model)
- `internal/store/reprice_test.go` (modify — pass `""` at every existing `RepriceCosts` call so the full-table tests keep their meaning)
- `internal/pricing/pricing.go` (modify — nil-rate return in `Table.Compute` before the multiply at `pricing.go:130`)
- `internal/pricing/pricing_test.go` (modify — add `TestComputeNilUsedClassReturnsUnpriced`)

## Review Notes

`--model` is taken off `args` with `takeFlag` before the refuse-without-`--yes` return and before `openStore`. An empty model keeps `RepriceCosts`' full-table pass, including the `repriceInScope` skip of `unpriced`. A non-empty model adds `WHERE model_resolved = ?` and bypasses that skip only for `cost_source == "unpriced"`. The live-serve probe from br-GI-22-04 stays behind `!dryRun`. When the flag is set, the writer includes `` `--model <name>` is backfilling previously-unpriced rows for this model only ``.

`Table.Compute` returns `(nil, "unpriced")` when a class has tokens and a nil rate, before the multiply. Zero-token classes still skip a nil rate. `consumer.go` and `table.go` were not edited.

`TestRepriceModelFlagBackfillsOnlyNamedModel` issues two `runReprice` calls. One `--model` value cannot be both shipped `claude-sonnet-5` and from-scratch `claude-haiku-4-5-20251001`. Both rows share one database; the second pass does not move the subscription figure the first pass wrote.

`go test ./internal/pricing/ ./internal/store/ ./internal/cli/ -count=1 -run 'TestComputeNilUsedClassReturnsUnpriced|TestRepriceModelFlagBackfillsOnlyNamedModel|TestReprice|TestStoreDoesNotImportPricing'` — 17 passed. Before the guard, `TestComputeNilUsedClassReturnsUnpriced` panicked on a nil `*big.Rat` multiply. `go test ./internal/pricing/ ./internal/store/ -count=1` — 169 passed.
