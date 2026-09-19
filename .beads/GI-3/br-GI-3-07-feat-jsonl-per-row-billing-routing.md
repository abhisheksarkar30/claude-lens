# Bead br-GI-3-07: JSONL per-row billing routing by model prefix + `SetModelBilling` wiring

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D5, §4 (`jsonlogs.go`, `jsonlogs_test.go`, `refresh.go`, `ingest.go`), §5 T7/T14, §6 R7 (plan sketch §9 bead 5)

- **Bead ID**: br-GI-3-07
- **Priority**: P0 (critical)
- **Original Estimate**: 1.5h
- **Dependencies**: br-GI-3-06
- **Blocks**: br-GI-3-10

## Description

A model whose prefix is listed in `ApiModelPrefixes` bills pay-as-you-go, so its JSONL rows must
land in the `api` account with cost in `cost_usd` (real money), not `api_equivalent_cost_usd`
(hypothetical). Today the tailer fixes `account`/`billingMode` for **every** row, defaulting to
`subscription` (`internal/jsonlogs/jsonlogs.go:76-84`, `:91-93`), so DeepSeek rows are
mislabelled.

**1. Per-row resolution in the tailer.** `Tailer` gains three fields — `apiPrefixes []string`,
`apiAccount string`, `apiBillingMode string` — plus:

```go
// SetModelBilling lists the model prefixes that bill pay-as-you-go, and the
// account/billing mode such a model's rows get. A prefix list consumed only
// behind a tailer.
func (t *Tailer) SetModelBilling(prefixes []string, apiAccount, apiBillingMode string)
```

`buildEvent` (`jsonlogs.go:291-350`) resolves account/billing **per row** before the `store.Event`
literal, instead of once:

```go
account, billingMode := t.account, t.billingMode
for _, p := range t.apiPrefixes {
    if strings.HasPrefix(model, p) {
        account, billingMode = t.apiAccount, t.apiBillingMode
        break
    }
}
```

`model` here is the resolved model id that will become `ev.ModelResolved`. Because the pricing
switch reads `ev.BillingMode` (`jsonlogs.go:341`), assigning it before the literal propagates
correctly with **no change to that switch**.

`t.apiPrefixes` is the **resolved** list — `resolvedAPIPrefixes(cfg)` (br-GI-3-05), i.e.
`cfg.ApiModelPrefixes` if non-nil, else `pricing.ShippedAPIModelPrefixes()` (`{"deepseek-"}`) — so
an unconfigured install routes DeepSeek by default and `clens ingest --rebuild` alone repairs the
57k rows with no configuration step.

**2. Wiring lands in one place — `newTailer` (br-GI-3-06).** Add to the helper:

```go
t.SetModelBilling(resolvedAPIPrefixes(cfg), firstAccount(cfg, "api").Name, "api")
```

`apiBillingMode` is the literal `"api"`; `apiAccount` is `firstAccount(cfg, "api").Name`. The same
`firstAccount` helper sets the **subscription** account (`refresh.go:93-95`, `ingest.go:49-51`).
`firstAccount` returns a `config.Account` **value**, or a **zero `Account`** when no account of
that mode is configured (`ingest.go:70-77`) — so `t.apiAccount` is `""` while `t.apiBillingMode`
stays the literal `"api"`; **`billing_mode` is never the empty string**. The empty `account` is
fine: it is the same value the consumer path already permits (`consumer.go:424-432`), and
`billing_mode` is the column invariant 5 keys off.

Both tailer sites (`addCollectors`, `runIngest`) reach this through `newTailer`, so the wiring is
one place rather than mirrored.

**3. Config, not a code rule.** "Which models are third-party pay-as-you-go" is a fact about the
user's setup, not the model — deriving it from "has a peak window" or "is in the shipped table"
would conflate two independent concepts and break the first time a Claude model gains time-of-day
pricing or a DeepSeek model loses it.

**4. Comment amendment.** A new seam replaces the fixed-field `ponytail:` comment's ceiling for the
prefix-matched case only (`jsonlogs.go:76-84`); the comment stays and is amended to say so.

**Invariant 5.** A prefix-matched row is written to `cost_usd` with `api_equivalent_cost_usd` NULL
(the switch already routes the cost to the right column once `BillingMode` is `api`). An unpriced
prefix-matched row stays NULL in both columns, never `$0.00`.

## Rationale

This is the repair §1 promised: without it, adding the DeepSeek rates would still leave 57k rows
labelled `subscription`, so their cost would land in the hypothetical column. Routing by a
configured prefix (not a derived predicate) keeps "which models are third-party" a user fact.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- A JSONL row for `deepseek-flash` → `billing_mode='api'`, cost priced in `cost_usd`,
  `api_equivalent_cost_usd` nil; a `claude-*` row from the **same** tailer still →
  `subscription` with cost in `api_equivalent_cost_usd`.
- An unconfigured install (both config keys unset) routes `deepseek-*` to `api` by default — the
  resolver, not a manual step, produces this.
- The `billing_mode` on a route is never the empty string; an install with no `api` account still
  writes `billing_mode='api'` with an empty `account`.
- The `ponytail:` comment is amended for the prefix-matched case.

## Test Specifications

- Unit Tests (`internal/jsonlogs/jsonlogs_test.go`):
  - **T7**: a `deepseek-flash` JSONL row → `billing_mode='api'`, `CostUSD` set,
    `ApiEquivalentCostUSD` nil; a `claude-*` row through the same tailer → `subscription`,
    `ApiEquivalentCostUSD` set, `CostUSD` nil.
  - No-prefix-list case: `SetModelBilling(nil, "", "api")` routes nothing, so an unset resolver at a
    call site would be visible (the nil-`HasPrefix` failure shape).
- Unit Tests (`internal/jsonlogs/jsonlogs_test.go`) — **T14 folds into T7's case above**: drive a
  tailer directly with `SetModelBilling(pricing.ShippedAPIModelPrefixes(), "", "api")` — the exact
  value `resolvedAPIPrefixes(cfg)` returns on an unconfigured install — and assert a `deepseek-*`
  row lands in `api` while a `claude-*` row through the **same** tailer stays `subscription`. That
  is Outcome Definition's "an unconfigured install routes by default", asserted at the seam that can
  see it.
- Unit Tests (`internal/cli`): **none added.** The resolver's own contract is br-GI-3-05's T10;
  re-asserting it here would exercise the same function a second time without touching the wiring.
  An assertion that `newTailer` *calls* `SetModelBilling` is unwritable from this package —
  `apiPrefixes`/`apiAccount`/`apiBillingMode` are unexported with no accessor, and no test in
  `internal/cli` builds a tailer at all (`cli_test.go`'s fixtures write rows straight to the store
  via `seedEvent`), so a routing assertion here would need a net-new poll harness to observe what
  `jsonlogs` already observes directly. br-GI-3-06 sets the precedent: the helper's two calls are a
  reviewed-move property, visible in a small diff, and the behaviour they wire is asserted one
  package down.
- Integration Tests: none (T8 in br-GI-3-08 covers the merge interaction).
- E2E: none.

## Files to Touch

- `internal/jsonlogs/jsonlogs.go` (modify — `apiPrefixes`/`apiAccount`/`apiBillingMode`,
  `SetModelBilling`, per-row resolution in `buildEvent`, `ponytail:` comment amendment)
- `internal/jsonlogs/jsonlogs_test.go` (modify — T7)
- `internal/cli/ingest.go` (modify — `newTailer` gains the `SetModelBilling` call)
- `internal/cli/refresh.go` (modify — `addCollectors` reaches it through `newTailer`; no separate
  edit expected)
