[← INDEX](INDEX.md)

# Cost and quota model

*(A domain module, not one from the skill's standard catalogue: this repo has no user-facing
feature that is not downstream of the cost model, so it earns its own file.)*

Two rules generate most of the code in [internal/pricing](../../internal/pricing/),
[internal/quota](../../internal/quota/), and [internal/reconcile](../../internal/reconcile/), and
most of the tests:

1. **Two billing models are never summed.** Not in SQL, not in Go, not in the dashboard's totals.
2. **An invented number is worse than no number.** `$0.00` reads as "this was free"; `unpriced`
   tells you to go configure something.

The design is in `docs/planning/GI-1-claude-lens-v1.md` §Billing model. This file is the map to the
code that implements it.

## The seven shapes, and the column that carries them

| Plan | `billing_mode` | Cost shown | Quota shown |
|---|---|---|---|
| Free | `subscription` | API-equivalent value, labelled hypothetical | snapshot % + calibrated burn |
| Pro | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Max 5x | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Max 20x | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Team | `subscription` | API-equivalent value | snapshot % + shared-seat context |
| Enterprise | `subscription` | API-equivalent value | snapshot % + configured spend limit |
| Pay-as-you-go | `api` | real billed dollars (source D) and computed dollars in `cost_usd` (sources A/B) | Admin rate-limit reports |

All seven reduce to **two** billing modes. The separation is carried in the schema, not in a
convention: a `subscription` figure is written to `api_equivalent_cost_usd` and `cost_usd` is left
NULL; an `api` figure is written to `cost_usd`. Both live in [internal/store](../../internal/store/),
enforced by an invariant test — see [storage-schema.md](storage-schema.md) and
[decisions/001](decisions/001-billing-split-by-column.md).

`billing_mode` comes from the **configured account**, not from the credential shape alone. A mixed
account (a Pro subscription *and* a separate API key) is two accounts, rendered side by side and
never summed.

## Auth-kind classification

[internal/proxy](../../internal/proxy/) classifies a call from the *shape* of its credential and
stores only the classification — never the credential:

| Observed | `auth_kind` | Meaning |
|---|---|---|
| `Authorization: Bearer` with an OAuth token | `oauth` | subscription |
| `x-api-key` with a standard key | `api_key` | pay-as-you-go |
| `x-api-key` with an admin key | `admin` | admin credential on the data plane — a misconfiguration |
| cloud-provider signature headers | `cloud` | Bedrock/Vertex/Foundry — classified and stored, **not priced** in v1 |
| none / unrecognised | `unknown` | recorded as unknown, never guessed |

`analyze` fires `auth_kind_anomaly` when the pair disagrees with the account's billing mode in
either direction ([internal/analyze/rules.go](../../internal/analyze/rules.go)).

## Pricing

[internal/pricing/table.go](../../internal/pricing/table.go) is the bundled rate table; each row
carries a `Source` of `shipped` (cited to a doc line in the comment) or `provisional` (bundled but
unverified). A user override is written to a separate file and carries `user`.

`Compute(model, usage, speed, serviceTier, at)` returns `(nil, "unpriced")` for a model with no
rate — never a numeric zero a caller could store as `$0.00`. The full vocabulary of `cost_source`
is `shipped | provisional | user | approximate:<reason> | unpriced`; `approximate:` marks a rate
applied where only a flat cache count was available (e.g. `approximate:cache_ttl_unknown`), so the
caveat survives into the stored row instead of being rounded away.

`prices` is append-only — a rate change is a new `(model, effective_from)` row, and the row in
force for an event is the one with the greatest `effective_from ≤ started_at`.

### Exact money, rounded at display

Every rate is an exact `*big.Rat` **dollars per token**, never a float. A class costs
`tokens × rate`; `batch` halves it and the peak multiplier scales it; `Compute` **sums the classes
exactly and rounds nowhere** ([pricing.go:116-138](../../internal/pricing/pricing.go#L116-L138)).
The rounding happens at the edges and only there: the stored `REAL` column is a `float64` and is
therefore approximate by construction, and each display path formats the value it prints
(`$%.4f` in [internal/cli/format.go](../../internal/cli/format.go), `toFixed(2)` in the dashboard).

**Per-class cent rounding inside `Compute` was the defect GI-11 removed**, and it is worth knowing
why, because the shape looks reasonable until you count the calls. Four sub-cent classes that each
round to `$0.00` sum to `$0.00`, so a call whose true cost is `$0.006` was stored as exactly zero —
and on a DeepSeek workload of small cached calls that is not an edge case, it is the whole tail of
the distribution. Rounding the *total* instead of each class is the same hazard in a subtler form:
`TestComputeBatchHalvesExactly` and `TestComputePeakMultipliesExactly` guard the batch and peak
halves of it, and `TestComputePricesSubCentClassesExactly` is the direct inversion — every class
priced below a cent, asserting the result is non-zero and exact.

The exactness is also why the peak multiplier is applied per class at
([pricing.go:130-136](../../internal/pricing/pricing.go#L130-L136)) rather than to the running
total. With no rounding anywhere, drift is the only remaining risk, and a float accumulation is
exactly how it would arrive.

Rates enter the shipped table as decimal USD strings per million tokens (`perMTok("0.15")`), which
**panics at init** on an unparseable literal rather than silently pricing at zero.

### Peak and off-peak

`Rate.Peak` is a `*PeakWindow`; **`nil` means flat-priced**. The window is per-`Rate` and never
global, because `ShippedTable()` legitimately mixes flat Anthropic rows with time-varying DeepSeek
ones — an unconditional multiply would double every Claude cost
([pricing.go:38-47](../../internal/pricing/pricing.go#L38-L47)).

| | |
|---|---|
| Shipped window | DeepSeek only: peak = **2× off-peak** |
| Hours | `[01:00, 04:00)` and `[06:00, 10:00)` — **UTC**, half-open |
| Days | Monday–Friday, excluding the off-peak dates |
| Off-peak dates | the 33 bundled **2026** Chinese public holidays |

`IsPeak(at)` ([:59](../../internal/pricing/pricing.go#L59)) checks in that order: weekend in UTC →
false; date in `OffPeakDates` → false; hour inside a span → true. `Compute` resolves `peak`
**once per call** and applies it as one exact multiplication on each class's own `big.Rat` cost
([:114-136](../../internal/pricing/pricing.go#L114-L136)) — never to the running total, and with no
rounding on either side of it.

Two things to know before touching the date list:

- **It expires.** It is 2026-only, and past it every weekday hour prices at peak — an
  **over**-charge. That direction is deliberate and named in the `ponytail:` comment in
  [table.go](../../internal/pricing/table.go).
- **It is configurable.** The bundled list and the peak hours can be replaced or cleared from
  config — see the config knobs in [build-and-run.md](build-and-run.md).

`PeakComputer` ([:157](../../internal/pricing/pricing.go#L157)) is an **optional** interface —
`PeakAt(model, at) bool` — implemented by `Table` and `*Loader` and pinned by a compile-time
assertion. It is separate from `Compute` so widening the pricing seam does not force every fake
behind `PriceComputer` to change; a pricer not implementing it simply yields no `peak_pricing`
warning, which is one of the two thirds of that kind's trigger described in the README's
warning-kind table.

### Third-party models and the zero-write rule

Three DeepSeek rows ship in the table — `deepseek-flash`, `deepseek-v4-pro`, `deepseek-v4-flash` —
all `shipped`, with **zero** cache-write rates because that endpoint bills no cache write.
`ZeroWriteShippedRates(model)` is the single home for that rule: `LoadOverrides` inherits the zeros
for such a model *before* deriving the usual write rates from the input rate, so an override that
sets only input/output does not silently acquire a 1.25×/2× write rate. Unknown models and
non-zero-write rows return false and the derivation proceeds as before.

Which models bill pay-as-you-go is a **prefix list, not a code rule** —
`shippedAPIModelPrefixes = ["deepseek-"]`, overridable from config. The routing it drives lives in
the collector rather than here: see [workflows.md](workflows.md) and
[internal/jsonlogs](../../internal/jsonlogs/).

## Quota

[internal/quota](../../internal/quota/) computes the token burn inside a rolling window from
events, and cross-checks it against the polled snapshots from source C.

- **Burn** sums *every* token column over the half-open interval `[now-duration, now)` — input +
  output + both cache-write tiers + cache-read. Using `input_tokens` alone would under-count every
  cached call.
- **`CrossCheck`** pairs each snapshot with the burn computed at *that snapshot's own instant*, not
  at `now`, so an old snapshot is checked against the window as it stood then.
- **`Limits` is empty by default.** `Projection.Configured` is false with no limit, and the caller
  must render `unconfigured` — never read `UtilizationPct` as a real 0%.
- A limit becomes known one of two ways: the user configures it, or
  [internal/quota/calibration.go](../../internal/quota/calibration.go) *learns* it — a snapshot
  window that reached 100% makes the burn recorded at that moment an empirical observation of the
  limit, offered as a **candidate** for confirmation, never applied silently.

`quota_window_approaching` fires when the current rate would exhaust the window before it resets.
The projection is a linear extrapolation of tokens-per-second across the window (marked `ponytail:`
in the source) — good enough to answer "should the user worry", not a simulation of which requests
age out when.

## Reconciliation

[internal/reconcile](../../internal/reconcile/) compares this tool's computed cost against the
Admin cost report, per `(day, model)`. Divergence past an absolute-dollar threshold raises
`cost_drift` — the signal that the price table is stale.

`source_mismatch` is the other half: the same `request_id` arriving from two sources with
disagreeing token counts. **Two measurements are required for a disagreement** — a side with no
observed usage at all is absent, not contradicting, which is the contract `mergeEvents` already
documented and the merge now enforces. A row captured under `--body-policy off` carries zeros in every
token column because usage is parsed from a body it never kept; ungated, that made every off-policy
merge raise this kind at `SeverityError`. It is raised by the store's merge, not by an analyze rule,
and is one of
the five kinds in `nonAnalyzeKinds` ([internal/analyze/kinds.go](../../internal/analyze/kinds.go)) —
the list a README check uses to expect a kind with no rule in that package. See the README's
warning-kind table, which `internal/analyze/readme_test.go` pins against `AllKinds()`.

## Caching rules

The cache rules are where the cost model meets the wire format, and one of them carries a
non-obvious table:

| Kind | Fires when |
|---|---|
| `cache_prefix_below_minimum` | a `cache_control` breakpoint marked a prefix shorter than the model's minimum cacheable length, and nothing was cached |
| `cache_prefix_invalidation` | a previously-cached prefix was rewritten, so the next call re-writes it |
| `cache_write_never_read` | a write was paid for and no later call read it |
| `cache_breakpoints_exceeded` | more `cache_control` markers than the API accepts |
| `cache_ttl_mismatch`, `cache_expired_between_turns`, `cache_invalidated_by_tools`, `cache_concurrent_write_race` | the remaining cache diagnoses |

**The minimum cacheable prefix is a per-model table, not a rule of thumb**, because it is
deliberately **non-monotonic across generations**:

| Tokens | Models |
|---|---|
| 512 | Opus 5, Fable 5, Mythos 5 |
| 1024 | Opus 4.8, Sonnet 5, Sonnet 4.6, Sonnet 4.5 |
| 2048 | Opus 4.7 |
| 4096 | Opus 4.6, Opus 4.5, Haiku 4.5 |

Source: `minimumCacheablePrefix` in [internal/analyze/rules.go](../../internal/analyze/rules.go).
A newer generation having a *lower* minimum than an older one is why this cannot be a version
comparison. The marked prefix is measured from the captured request body, and the estimator is
`len(body)/4` — marked `ponytail:` in the source, with both error modes argued safe because the
table's steps are far coarser than the estimate's error. `TestMinimumCacheablePrefixCoversShippedModels`
fails if a shipped model has no entry here — **except** the three third-party DeepSeek rows, which
are carried as a named exception list with the reason recorded (“no cited cache minimum; the
endpoint exposes no cache-write billing”). That is a literal in the test, not a derived predicate,
so adding another model to it stays a visible, reviewable edit rather than something a predicate
decides silently.

Note the rule only fires when the model is **known** and the body was **captured**: an
unrecognised model, or `--body-policy off`, means no warning rather than a guess.
