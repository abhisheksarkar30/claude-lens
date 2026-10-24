# Bead br-GI-25-04: Billing mode `local` — routing, merge rule, third aggregate, and every column-keyed consumer

**Plan Reference**: `docs/planning/GI-25-local-llm-toggle-and-store-hardening.md` v8, §2 R1/R1a/R1b/R1c, §3.4, §4.1, §4.2, §5.1, §5.2 (all `TestLocal*`, `TestMerge*`, `TestWriteGroupedLocalBucket`, `TestCostCellLocalLabel`, `TestModelsCoverageShowsLocalPriced`, `TestRepriceRoutesLocalToApiEquivalent`, `TestValidateRejectsLocalAsAccountName`), §5.4, §6 R5, Change History v3–v8 (merge rule's four-round simplification; v8's carried file-list and Models-tab-fold notes). Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-25-04
- **Priority**: P0
- **Status**: pending
- **Original Estimate**: 2h (largest bead in this ticket by surface — the plan calls it "the riskiest, an invariant"; treat the estimate as a floor)
- **Dependencies**: None
- **Blocks**: br-GI-25-05, br-GI-25-06
- **Commit**: `GI#25 feat: add the local billing mode, its routing and merge rule (br-GI-25-04)`

## Description

This bead makes `local` a first-class third billing mode, kept out of real spend and out of quota burn, without touching the schema (no column, no `schemaVersion` bump — `billing_mode` has no `CHECK`, `schema.sql:21`) and without giving the proxy-switch wiring (bead 05) anything to block on.

### 1. Config constants

`internal/config` (new package-level `const`s, beside the existing `Account`/`Config` types — `config.go` has no internal imports, so no import cycle for anything downstream that imports `config`):

```go
const (
	BillingLocal           = "local"
	LocalAccount           = "local"
	LocalPriceModel        = "deepseek-flash"
)
var LocalPriceAt = time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC) // a Saturday: off-peak
```

`LocalPriceAt` is 2026-10-24 12:00 UTC, a Saturday (weekends are off-peak per `pricing.go:61`: `d == time.Saturday || d == time.Sunday`). Confirm in the implementation that this instant is off-peak for `deepseek-flash` (the `TestLocalPriceModelIsTheCheapestShippedDeepseekRow` test below asserts it), and that it is not a peak-override date in `pricing.ShippedOffPeakDates()`; if it is, pick another Saturday. Every local token is priced at this fixed off-peak instant regardless of the call's real time, so peak demand pricing (a cloud-only concept) never applies to a model running on the operator's own machine.

### 2. `sink.CapturedCall` gains a `Provider` field

Add `Provider string` to `sink.CapturedCall` (`internal/sink/sink.go`, beside the existing `AuthKind` field at line 34). This bead only *defines* the field and the consumer-side logic that reads it; nothing stamps it with a real value yet (`Provider` is simply `""` on every existing capture) until br-GI-25-05 wires the proxy's `Rewrite` to stamp `targets.Active()` into it. `consumer.go`'s routing switch (below) treats `call.Provider == "local"` as the local case and everything else (today, unconditionally `""`) as non-local — so this bead changes no observable behavior for existing cloud traffic and is safe to land independently.

### 3. Routing — four explicit `default:` sites gain an explicit `local` case

Today three `default:` branches route every non-`subscription` mode to `cost_usd` (the fourth is `merge.go`'s identity rule, handled separately in §5 below). A bare new constant flowing into these `default:` branches unchanged would silently route local money into `cost_usd` — invariant 5's forbidden shape. Each site gets an **explicit** `case "local":` that writes `api_equivalent_cost_usd` instead:

- **`internal/consumer/consumer.go:352-365`** (`processCall`): after resolving `account, billingMode := c.resolveAccountNow(call.AuthKind)`, add: when `call.Provider == "local"`, override to `ev.Account = config.LocalAccount; ev.BillingMode = config.BillingLocal` (regardless of what the auth-kind resolution produced — a local call carries no meaningful credential-derived account). Then in the pricing block (`consumer.go:356-365`), substitute the model and instant **before** calling `Compute`: when `ev.BillingMode == config.BillingLocal`, call `c.pricer.Compute(config.LocalPriceModel, usage, usage.Speed, ev.ServiceTier, config.LocalPriceAt)` instead of `c.pricer.Compute(ev.ModelResolved, usage, usage.Speed, ev.ServiceTier, call.StartedAt)` — `ev.ModelResolved` itself is **not** rewritten; the row still shows the real local model name (e.g. `lmstudio-community/...`), only the *price lookup* is substituted. Extend the `switch ev.BillingMode` (`consumer.go:359-364`) with an explicit `case config.BillingLocal: ev.ApiEquivalentCostUSD = usd` arm, ahead of (or instead of) `default`, so `local` is never silently caught by `default: ev.CostUSD = usd`.
  Also guard the peak-pricing warning: the existing call `peakWarning(c.pricer, ev, call.StartedAt)` (`consumer.go:377`) is skipped entirely for a local row — wrap it `if ev.BillingMode != config.BillingLocal { if w, ok := peakWarning(...); ok { ... } }` — a local row was never billed at a cloud peak rate, and this is the explicit, defensive form the plan specifies rather than relying on an unrelated lookup failing to collide.
- **`internal/jsonlogs/jsonlogs.go:431-437,487-492`** (`buildEvent`): `Tailer` gains a `localPrefixes []string`, `localAccount = config.LocalAccount` (fixed, not configurable per-prefix), and reuses `billingMode = config.BillingLocal` — add a `SetLocalPrefixes([]string)` setter alongside the existing `SetModelBilling` (`jsonlogs.go:183-186`). In the classification loop (`jsonlogs.go:431-437`), **check `apiPrefixes` first**; only if no `apiPrefixes` entry matches, check `localPrefixes` and set `account, billingMode = config.LocalAccount, config.BillingLocal`. A model prefix matching both lists classifies `api` (§2 R1c) — this preserves real spend when cloud and local both serve DeepSeek-family models. In the pricing block (`jsonlogs.go:484-493`), the same model/instant substitution as the consumer applies: when `billingMode == config.BillingLocal`, call `t.pricer.Compute(config.LocalPriceModel, usage, "", "", config.LocalPriceAt)` instead of `t.pricer.Compute(ev.ModelResolved, usage, "", "", ev.StartedAt)`, and extend the `switch ev.BillingMode` (`jsonlogs.go:487-492`) with an explicit `case config.BillingLocal: ev.ApiEquivalentCostUSD = usd` arm.
- **`internal/store/store.go:2203-2208`** (`repriceCandidate.routed()`): add an explicit case so a `local` row's stored figure is read from (and written back to) `api_equivalent_cost_usd`:
  ```go
  func (c repriceCandidate) routed() (sql.NullFloat64, bool) {
  	switch c.billingMode {
  	case "subscription", config.BillingLocal:
  		return c.apiEquivalentCostUSD, true
  	default:
  		return c.costUSD, false
  	}
  }
  ```
  Note the second return value's name (`subscription` in the existing call sites) now also covers `local` — check `RepriceCosts`' two call sites of `routed()` (`store.go:2309`, and the `UPDATE` branch following it) read this boolean as "route to `api_equivalent_cost_usd`" generically, not as "is subscription" specifically; rename the local variable if the existing name reads misleadingly after this change (e.g. `stored, apiEquivRoute := c.routed()`).
- **`store.go:2300`** (the `table.Compute` call inside `RepriceCosts`): apply the same model/instant substitution as the two live paths — `model, at := c.model, c.startedAt; if c.billingMode == config.BillingLocal { model, at = config.LocalPriceModel, config.LocalPriceAt }; usd, source := table.Compute(model, c.usage(), c.speed, c.serviceTier, at)` — so a reprice pass cannot move a local row onto its own (almost certainly unpriced) real model name.

`internal/store` and `internal/jsonlogs` both gain a new import of `internal/config` for these three constants; `config.go` has no internal imports, so no cycle (confirmed: `store.go:1-27`'s current import block has no `config`; `store.go:24` already imports `proxy`, and `config` is lower-level than `proxy`, so this is also fine per plan §4.2).

### 4. Merge rule — `internal/store/merge.go`

This is the subtlest part of the bead; the exact line numbers below are current (verified against the `main` HEAD this plan branched from, `a4f1a31`, and unchanged since — `mergeEvents`, `merge.go:223-467`).

Define **"proxy-involved side"** for a given `*Event` as `Source == "proxy" || contains(SourceRefs, "proxy")` — the `reflagScope` idiom already used elsewhere (`store.go:2367`). A JSONL-first merged row keeps `Source=="jsonl"` but gains `SourceRefs=[jsonl,proxy]` once a proxy row later merges into it (`mergeEvents`'s `SourceRefs` union at `merge.go:225`), so a bare `Source`-equality test would wrongly call that row "not proxy-involved" on its *next* merge. Compute this for both `existing` and `incoming` right after the `SourceRefs` union at line 225.

**No-op cases** (apply before anything else below): if **neither** side is proxy-involved (both `Source=="jsonl"`, the `--rebuild` case) — do nothing, fall through to the unmodified rest of `mergeEvents`. If **both** sides are proxy-involved — do nothing either (modes necessarily agree when both come from the proxy's own target, the normal single-provider case). Both no-ops mean: this whole block only has an effect when exactly one side is proxy-involved.

When exactly one side is proxy-involved, call it `proxySide` and the other `other`. Everything below runs **after** the existing completeness pick (`merge.go:250-264`) and `usageObserved` swap (`merge.go:280-290`) have already produced `winner`, and **before** the existing `if merged.BillingMode == ""` fallback block (`merge.go:361-377`):

1. **Token-supplying side is unchanged** — still whichever of `existing`/`incoming` the completeness pick and `usageObserved` swap selected as `winner` (`merge.go:250-290`). This block does not touch that selection.
2. **Identity override.** When `proxySide.BillingMode != ""`: `merged.BillingMode = proxySide.BillingMode` and `merged.Account = proxySide.Account` (the real account when the proxy side is `api`/`subscription`, or `config.LocalAccount` when it is `local`) — overriding both the plain `merged.BillingMode = winner.BillingMode` at `merge.go:341` and the plain `merged.Account = preferNonEmpty(existing.Account, incoming.Account)` at `merge.go:319` for these two columns specifically, when it fires. **Skip condition**: when `proxySide.BillingMode == ""` (an unclassified credential), skip the override entirely — treat it like `preferNonEmpty` already does, i.e. fall through to the existing lines 319 and 341 unmodified.
3. **Cost alignment**, applied only when step 2 fired (i.e. `proxySide.BillingMode != ""`):
   - If the token-supplying `winner` is the same side as `proxySide` (the guard "fires" — proxy-involved side also won tokens): keep `winner`'s cost columns unchanged (`merge.go:299-301`, already copied from `winner`).
   - If `winner` is the *other* side (the guard did not fire — e.g. an aborted/incomplete/zero-usage proxy row, with `other` supplying real tokens): set `merged.CostUSD = nil`, `merged.ApiEquivalentCostUSD = nil`, `merged.CostSource = "unpriced"` — the existing NULL convention (`clens reprice --model <m>` can fill it later once the identity side is reconciled). The cost cell must never be `$0.00` and must never carry a label (`"shipped"` etc.) that implies it burns quota.
   - When step 2 was skipped (`proxySide.BillingMode == ""`), cost alignment is also skipped — no identity mode exists to compare `winner`'s mode against, so `winner`'s cost columns are kept as-is.
4. **Account invariant post-condition**, applied to **every** merge output (whether or not steps 2/3 fired, and even on the two no-op cases above):
   ```go
   switch {
   case merged.BillingMode == config.BillingLocal:
   	merged.Account = config.LocalAccount
   case merged.Account == config.LocalAccount:
   	merged.Account = nonLocalSideAccount // the non-local side's Account, or "" if neither has one
   }
   ```
   The first branch closes the both-JSONL (`--rebuild`) gap: line 341 can set `merged.BillingMode = winner.BillingMode = "local"` while line 319's `preferNonEmpty` leaves the *old* subscription account in place (because `existing.Account` is non-empty), which would leak tokens into `quota.ComputeBurn` — that function filters by account name only, with no `BillingMode` parameter (`internal/quota/quota.go:63-71`, called from `api/quota.go:119,125` with `acct.Name, ""`). The second branch is the inverse: a subscription re-ingest that wins the mode must not carry a leftover `local` account, which would silently drop real subscription tokens out of burn. Together these make `BillingMode == "local" ⟺ Account == config.LocalAccount` a property of **every** merge output, independent of which branch above ran — including the ordinary non-local case, where no override fires and both columns simply keep their pre-existing values (which already satisfy the invariant).

Apply the model/instant substitution **nowhere in `merge.go`** — a merged row's `CostUSD`/`ApiEquivalentCostUSD` either comes straight from `winner`'s already-computed figure (step 3's "keep" branch) or is nulled (step 3's "misalign" branch); `merge.go` never calls `Compute` itself.

### 5. Third stats aggregate

**`internal/store/store.go:1667-1676`** (`statsSelectColumnsInner`): add a third `SUM`:
```sql
SUM(CASE WHEN billing_mode = 'local' THEN api_equivalent_cost_usd END)
```
as a fourth/third column after the existing two `SUM(CASE ...)` lines (1674-1675). **`StatsSummary`** (`internal/store/types.go:208-224`) gains `TotalLocalProjectedCostUSD *float64`. Both scan helpers that currently read the two-column pair — `scanStatsSummary` (`store.go:1680-1703`) and `scanStatsRow` (`store.go:1705-1731`) — gain the third `sql.NullFloat64`/pointer-assignment pair, mirroring the existing `totalApiEquivCostUSD` handling exactly. Because `StatsSummary`, `StatsByModel`, `StatsByPeriod` all route through `statsSelectColumnsInner` + one of these two scan helpers, this one change covers all three aggregates; `StatsByCostSource` (`store.go:1735-1749`) is **not** touched — it has its own narrower two-column `SELECT` and the plan does not ask for a local figure there. `reconcileSessionTx`'s session-level sums (`store.go:790-806`) are **intentionally untouched** — see §6 below (Sessions note): a session's real and subscription totals must continue to exclude local rows.

### 6. Models tab — `StatsByModel`/`ModelStats.PriceModel` and `api/models.go`'s fold

**`store.go:1332-1352`** (`StatsByModel`): the query already scans a `billingMode` local variable (`store.go:1345`) that is currently discarded after the scan — never assigned anywhere. Use it: add `PriceModel string` to `ModelStats` (`internal/store/types.go:227-230`), and after each row's scan set `m.PriceModel = config.LocalPriceModel` if `billingMode == config.BillingLocal`, else `m.PriceModel = m.Model`. Do **not** add a `BillingMode` field to `ModelStats` (removed deliberately in an earlier round, v4/F3.3) — `PriceModel` differing from `Model` is itself the local signal, and is the only signal `api/models.go` needs without re-adding the field the import guard would then have to keep out of the JSON response.

**`internal/api/models.go:54-89`** (`models` handler) — **this is the fold that must not silently collapse local and cloud rows of the same model name into one entry.** Today `byModel` is keyed by `s.Model` alone (`models.go:62,66-80`): `StatsByModel` already returns one row per `(model_resolved, billing_mode)` (`store.go:1334-1335`), so a model observed both locally and in the cloud arrives as two `ModelStats` rows with the same `Model` but (after the change above) different `PriceModel` — and the current fold loop (`models.go:71-79`) would add both rows' `Requests`/`PricedCount`/`UnpricedCount` into **one** `modelCoverage` entry and then call `coverage(table, row)` (`models.go:84`) exactly once against whichever `PriceModel` the last-iterated row set — nondeterministic, and wrong for whichever mode lost the race. Change the fold key to a composite of `(s.Model, s.PriceModel)` — e.g. `key := s.Model + "\x00" + s.PriceModel` — so a local-substituted row (`PriceModel == config.LocalPriceModel`, `Model` = the real local model name) and a cloud row of the same `Model` (`PriceModel == Model`) land in **separate** `modelCoverage` entries. Seed the catalogue loop (`models.go:63-65`) with `row.PriceModel = e.ModelID` (equal to `Model`) under the same composite key, so a catalogue entry for a model that is *never* called locally still unions correctly with its own cloud-observed rows and does not spuriously create a second entry. `api/models.go` must not import `internal/config` (enforced by `TestAPIWebImportGuard`, `importguard_test.go:36`) — comparing `PriceModel != Model` (or just using it as a map-key component) never needs the constant's name, only its already-materialized string value from the store.

`coverage()` (`models.go:91-105`) changes its lookup from `table[row.Model]` (`models.go:97`) to `table[row.PriceModel]` (a new `modelCoverage.PriceModel` field carrying the value through from the fold — `modelCoverage` gains this field, unexported-JSON or not per the implementer's choice of whether the dashboard needs to display it; the plan does not require it to be user-visible).

**`internal/cli/models.go:34-59`** (`runModels`): line 55's lookup `table[s.Model]` becomes `table[s.PriceModel]`, reading the new field directly rather than any `row.local`-style boolean (that field name belongs to an earlier, superseded round of this plan and was never implemented).

### 7. Column-keyed CLI display consumers (NOT `web/app.js`/`index.html` — those are br-GI-25-06)

- **`internal/cli/stats.go:194-203`** (`writeGrouped`'s per-group `switch`): add, **before** the existing `case ev.ApiEquivalentCostUSD != nil:` (line 198) — ordering matters, a local row also carries a non-nil `ApiEquivalentCostUSD` and would otherwise be caught by the existing case first — a new case: `case ev.BillingMode == config.BillingLocal && ev.ApiEquivalentCostUSD != nil: g.localCost += *ev.ApiEquivalentCostUSD; g.hasLocal = true`. A local row with a nil cost (the no-fire merge branch above, an unpriced local model, or genuinely zero usage) falls through to the existing `default: g.unpriced++` (line 202) unchanged — it must not panic and must not be miscounted as `api`/`sub`. `group` (the unexported struct `writeGrouped` builds, just above line 175) gains `localCost float64` and `hasLocal bool` fields beside `apiCost`/`hasAPI`/`subCost`/`hasSub`. At the print site (`stats.go:216-230`), build a third `local *float64` the same way `api`/`sub` are built (lines 216-224) and pass it into `costCell` (see next bullet); print a `local (projected)` column beside `api`/`sub`, omitted from the row when `g.hasLocal` is false so existing output is unchanged for every install with no local traffic.
- **`internal/cli/format.go:86-88`** (`costCell`): change the signature to `costCell(api, sub, local *float64) string`, rendering `api %s  sub %s  local (proj) %s` when `local != nil`, and the existing two-field rendering unchanged when it is `nil` (every aggregate call site below passes `nil` for `local` except where a local figure genuinely exists, so existing output is byte-identical wherever no local row is present). Add a `rowCosts(ev *store.EventSummary) (api, sub, local *float64)` helper in the same file: `store.EventSummary` is the type `ls.go`/`tail.go` iterate directly and `store.Event` embeds it (`show.go` passes `&ev.EventSummary`) — `rowCosts` returns `ev.ApiEquivalentCostUSD` as `local` (and `nil` as `sub`) when `ev.BillingMode == config.BillingLocal`; otherwise it returns `ev.ApiEquivalentCostUSD` as `sub` and `nil` as `local`, with `ev.CostUSD` always returned as `api` in both branches.
- **Per-row call sites**, all three updated to call `rowCosts(ev)` instead of passing the two raw fields directly:
  - `internal/cli/ls.go:95`: `costCell(ev.CostUSD, ev.ApiEquivalentCostUSD)` → `costCell(rowCosts(&ev.EventSummary))` (spread the three return values — Go allows `f(g())` only when `g` returns exactly `f`'s parameter count, which holds here).
  - `internal/cli/tail.go:101`: same substitution.
  - `internal/cli/show.go:72`: same substitution (already receives `&ev.EventSummary` per the existing pattern noted above — confirm the call site's exact receiver and adjust accordingly).
- **`internal/cli/sessions.go:72`**: passes `(s.TotalCostUSD, s.TotalApiEquivalentCostUSD, nil)` directly (three literal arguments, not through `rowCosts`) — session money sums **exclude** local rows by design (§6 Sessions note below), so the third argument is always a literal `nil`, never a computed value.
- **Aggregate call sites** (`stats.go:89,112,140`): pass `summary.TotalLocalProjectedCostUSD` / `p.TotalLocalProjectedCostUSD` / `m.TotalLocalProjectedCostUSD` (the new `StatsSummary` field from §5) as the third `costCell` argument — these do **not** call `rowCosts` (that helper is for row-level `EventSummary` values, not pre-aggregated `StatsSummary` values).

### 8. `Config.Validate` — reject an account literally named `local` (§6 R5)

In `internal/config/config.go`'s `Validate` (`config.go:340-408`), add, after the existing `ApiModelPrefixes` loop (`config.go:392-396`) and before the `PprofAddr` check (`config.go:397-406`):
```go
for _, a := range c.Accounts {
	if a.Name == LocalAccount {
		return fmt.Errorf("config: validate: Accounts: account name %q is reserved for local-provider traffic", LocalAccount)
	}
}
```
A user account literally named `local` would collide with the fixed local-provider account the quota exclusion (R1b) depends on.

### 9. `internal/sink/sink.go` and `internal/cli/doctor.go`

Per the plan's v8 change history (a decision not yet applied as of v7, explicitly carried into this bead's file list): `sink.go` is touched for the `Provider` field addition in §2 above. `doctor.go`'s existing `config_valid` check (`doctor.go:189-193`) calls `cfg.Validate()` and already surfaces *any* `Validate` error generically, including the new account-name rejection from §8 — so no functional code change to `doctor.go` is required for that path to work. No other doctor.go functional change is specified by the plan text for this bead; if the implementer finds none is needed beyond confirming the existing `config_valid` check surfaces the new error (worth a quick manual check, not a new automated test), say so in Review Notes rather than inventing an unrequested doctor.go feature.

### Sessions note (no code change, stated for the record)

`reconcileSessionTx`'s sums (`store.go:790-806`) stay exactly as they are — no `local` aggregate is added there — so a session's `total_cost_usd`/`total_api_equivalent_cost_usd` continue to exclude local rows (keeping invariant 5's two-column split meaningful at the session level too). Session *token* totals still include local tokens (`reconcileSessionTx`'s `SUM(input_tokens)` etc. have no `billing_mode` filter and are intentionally not changed) — a session spanning a mid-session provider toggle is the one place token and cost totals diverge in scope, which is R4 in plan §6, accepted and not solved by any bead in this ticket.

## Rationale

Reusing `billing_mode = 'subscription'` for local rows and filtering by account name was considered and rejected (plan §9): every `default:` branch and the quota loop would then rely on a name match holding forever, and a stray account named `local` (now explicitly rejected by §8) would have silently broken it. A `local` mode keeps the exclusion structural — the same `default:`-branch discipline invariant 5 already uses for `api` vs `subscription` — rather than convention-based.

## Outcome Definition

`go test ./internal/config/ ./internal/consumer/ ./internal/jsonlogs/ ./internal/store/ ./internal/cli/ ./internal/api/ ./internal/pricing/ -count=1` passes, including (named explicitly so a partial run can target them):
`TestBillingModeInvariants` (extended for the `local` shape), `TestLocalRowsPriceIntoApiEquivalentColumn`, `TestLocalSumIsNeverMergedIntoApiOrSub`, `TestLocalRowIsPricedAsDeepseekFlashBase`, `TestLocalPriceModelIsTheCheapestShippedDeepseekRow`, `TestLocalRowsAreAbsentFromQuotaBurn` (including its merged-row extension), `TestMergeKeepsLocalSticky` (including its three-event sub-case), `TestMergeLocalZeroUsageTokensSurvive`, `TestMergeLocalIncompleteTokensSurvive`, `TestMergeLocalModeMismatch` (both cases), `TestMergeLocalAccountInvariant` (both directions), `TestRepriceRoutesLocalToApiEquivalent`, `TestWriteGroupedLocalBucket`, `TestCostCellLocalLabel`, `TestModelsCoverageShowsLocalPriced`, `TestModelsEndpointKeepsLocalAndCloudRowsSeparate` (new, see Test Specifications), `TestValidateRejectsLocalAsAccountName`, `TestTailerLocalPrefixClassification`.
`go test ./internal/store/ -count=1 -run TestStoreDoesNotImportPricing` still passes (store never imports `internal/pricing`; it now imports `internal/config`, which is unrelated to that guard).
`go build ./... && go vet ./...` clean.

## Test Specifications

- `internal/store/store_test.go` (or wherever `TestBillingModeInvariants` lives today): extend to the `local` shape — a row with `BillingMode="local"` has `CostUSD == nil` and `ApiEquivalentCostUSD != nil` after `InsertEvent`; per plan §5.4, this store-level test calls `InsertEvent` directly (no cost routing happens there), so it only pins the *stored shape* — the routing sites each need their own named test (next three bullets).
- `internal/consumer/consumer_test.go`: a test (name it `TestLocalProviderCapturePricesIntoApiEquivalentColumn` or similar) that feeds a `sink.CapturedCall{Provider: "local", ...}` through `processCall`, asserting `ev.CostUSD == nil`, `ev.ApiEquivalentCostUSD != nil`, `ev.Account == config.LocalAccount`, `ev.BillingMode == config.BillingLocal`, and that the priced figure equals `deepseek-flash`'s off-peak rate for the given usage (not the real model's rate, and not affected by whether `call.StartedAt` falls in a peak window). **Negative control**: remove the explicit `local` case from the `switch ev.BillingMode` in `consumer.go` — the test fails on a non-nil `CostUSD`.
- `internal/jsonlogs/jsonlogs_test.go`: the tailer analogue of the above, via a `LocalModelPrefixes`-matched model — same assertions, same negative control (removing the tailer's explicit `local` case).
- `internal/jsonlogs/jsonlogs_test.go`: `TestTailerLocalPrefixClassification` — a model matching only `LocalModelPrefixes` classifies `local`; a model matching only `ApiModelPrefixes` classifies `api`; a model matching **both** lists classifies `api` (precedence rule, §2 R1c).
- `internal/store/reprice_test.go`: `TestRepriceRoutesLocalToApiEquivalent` — a `local`-mode, `cost_source='unpriced'` row with a real-looking model name is repriced via `RepriceCosts`; asserts the written figure lands in `api_equivalent_cost_usd`, `cost_usd` stays NULL, and the figure equals `deepseek-flash`'s off-peak rate for the row's usage (not whatever the real model name would price to, even if that name happens to also be in the table). **Negative control**: remove the `routed()` `local` case — the test fails on the figure landing in `cost_usd`, or on `cost_source` staying `'unpriced'` because the pre-substitution lookup against the real model name misses.
- `internal/consumer/consumer_test.go` (not `pricing_test.go`: the substitution and the peak-warning skip live in `processCall`, so the test must drive it): `TestLocalRowIsPricedAsDeepseekFlashBase` — an unknown local model name (one `ShippedTable` does not contain) still prices (not `"unpriced"`) to exactly the `deepseek-flash` figure when priced via the substitution, and a call timestamped inside a peak window still prices at the off-peak base rate with no `peak_pricing` warning surfaced by the consumer path. **Negative control**: price with the real (peak) `at` instead of `config.LocalPriceAt` — the figure doubles and `peakWarning` would fire if called with `ev.ModelResolved`="deepseek-flash" instead of the real local model name.
- `internal/pricing/pricing_test.go`: `TestLocalPriceModelIsTheCheapestShippedDeepseekRow` — `config.LocalPriceModel` exists in `pricing.ShippedTable()`, is off-peak at `config.LocalPriceAt` (`table[config.LocalPriceModel].Peak == nil || !table[config.LocalPriceModel].Peak.IsPeak(config.LocalPriceAt)`), and is not dearer (summed input+output+cache-read rate) than any other shipped `deepseek-*` row — so a future table edit cannot silently re-point it or make the "cheapest" label false.
- `internal/quota/quota_test.go` or `internal/api/quota_test.go`: `TestLocalRowsAreAbsentFromQuotaBurn` — a `local` row inside the 5h window leaves a subscription account's burn unchanged; extended with a **merged**-row sub-case: proxy `local` + JSONL `subscription`, both arrival orders — the merged row must keep `account == config.LocalAccount` and must not appear in `quota.ComputeBurn` for any subscription account.
- `internal/store/merge_test.go`: `TestMergeKeepsLocalSticky` — proxy `local` + JSONL `subscription`, both arrival orders; asserts tokens, cost, `Account == config.LocalAccount`, `BillingMode == config.BillingLocal`. Three-event sub-case: JSONL first, then proxy `local`, then JSONL again — the merged row must stay `local`/`config.LocalAccount` throughout, `cost_usd` nil, cost only in `api_equivalent_cost_usd`. **Negative control**: identify the proxy-involved side by bare `Source` equality instead of the `reflagScope`-style union test — the row flips to `subscription` on the third observation (the second merge's `SourceRefs` already contains `"proxy"` but `Source=="jsonl"`).
- `internal/store/merge_test.go`: `TestMergeLocalZeroUsageTokensSurvive` — proxy `local` with `usageObserved=false` + JSONL `subscription` with real tokens; JSONL tokens survive (the completeness/usageObserved guard does not fire), `BillingMode` stays `local`, `Account == config.LocalAccount`, cost cell NULL, `CostSource == "unpriced"`. **Negative control**: an unconditional reassignment (bypassing the guard) zeroes the tokens.
- `internal/store/merge_test.go`: `TestMergeLocalIncompleteTokensSurvive` — proxy `local` with `CaptureComplete=false` + complete JSONL `subscription` with real tokens; same assertions and negative control as above, for the completeness guard instead of the usageObserved one.
- `internal/store/merge_test.go`: `TestMergeLocalModeMismatch` — proxy `api` + JSONL prefix-classified `local`, both arrival orders; the merged row keeps `cost_usd` (not nil), `BillingMode == "api"`, and a real (non-`LocalAccount`) account when the guard fires. Additional case: an incomplete or zero-usage proxy `api` row against a complete JSONL `local` row — mode `api`, real account, `cost_usd` nil, `CostSource == "unpriced"`, JSONL tokens kept. **Negative control**: remove the identity override — the test fails on `BillingMode == "local"` (real account erased) or `cost_usd == nil` (real spend erased), depending on arrival order; for the incomplete/zero-usage sub-case, removing the cost-alignment step fails the test on a non-`"unpriced"` `CostSource` with a nil cost.
- `internal/store/merge_test.go`: `TestMergeLocalAccountInvariant` — both-JSONL no-op: JSONL `subscription`/`Max` (existing) + JSONL `local`/`config.LocalAccount` (incoming, re-ingest after `LocalModelPrefixes` is set), both orders; `Account == config.LocalAccount` iff `BillingMode == "local"`. Reverse case: JSONL `local`/`config.LocalAccount` (existing) + JSONL `subscription`/`Max` (incoming, re-ingest after the prefix is removed), both orders; `Account != config.LocalAccount` when `BillingMode == "subscription"`. **Negative control**: remove the account-invariant post-condition — account stays `Max` when mode is `local` (both-JSONL order); the quota-burn test would then see the row.
- `internal/cli/stats_test.go`: `TestWriteGroupedLocalBucket` — a `local` row with a non-nil `ApiEquivalentCostUSD` must not change `writeGrouped`'s `sub` figure, appearing only in the new `local (projected)` column; a `local` row with a nil cost (no-fire branch) falls to `default` (counted `unpriced`), not panic. **Negative control**: remove the `local` case from the switch, or remove the `default` fallthrough — the test fails on either a wrong bucket or a panic.
- `internal/cli/format_test.go`: `TestCostCellLocalLabel` — `rowCosts(ev)` on a local `*store.EventSummary` returns `local != nil`, `sub == nil`; `costCell(nil, nil, &x)` renders `local (proj) $x`; `rowCosts(ev)` on a non-local row returns `sub != nil`, `local == nil`.
- `internal/api/models_test.go`: `TestModelsCoverageShowsLocalPriced` — `coverage()` with a `modelCoverage` whose `PriceModel == config.LocalPriceModel` and an unknown real `Model` name returns `pricingPriced` (substitutes `PriceModel` for the lookup), not `pricingUnpriced`.
- `internal/api/models_test.go`: `TestModelsEndpointKeepsLocalAndCloudRowsSeparate` — **new test, not named in the plan's §5.2 list; added here because the plan's v8 Models-tab decision (§3.4, "kept as separate entries keyed by (model, billing mode)") has no dedicated test against the `models` handler's own fold loop, only against the standalone `coverage()` function.** Seed `StatsByModel` (via the store, through two captured calls) so one model name produces both a `local`-substituted `ModelStats` row (`PriceModel == config.LocalPriceModel`, `Model` = a real local model id) and an ordinary cloud `ModelStats` row for a *different* real model name that happens to equal `config.LocalPriceModel` itself (i.e., a genuine cloud call to `deepseek-flash`) — the adversarial case the composite key exists for. Assert `GET /api/models` (or the `models` handler called directly) returns **two** `modelCoverage` entries, not one, and that each one's `pricing`/`rate_source` reflects its own `PriceModel`, not whichever row's write happened to land last in the old single-key fold. **Negative control**: key `byModel` by `s.Model` alone (the pre-fix code) — the test fails on a single collapsed entry.
- `internal/config/config_test.go`: `TestValidateRejectsLocalAsAccountName` — `Config.Validate` rejects an account named `config.LocalAccount`.

## Files to Touch

- `internal/config/config.go` (modify — `BillingLocal`, `LocalAccount`, `LocalPriceModel`, `LocalPriceAt` constants; account-name validation in `Validate`)
- `internal/config/config_test.go` (modify — `TestValidateRejectsLocalAsAccountName`)
- `internal/sink/sink.go` (modify — `CapturedCall.Provider string` field)
- `internal/consumer/consumer.go` (modify — `processCall`'s account/billing-mode override, model/instant substitution, explicit `local` cost-routing case, peak-warning skip)
- `internal/consumer/consumer_test.go` (modify — local-provider routing test)
- `internal/jsonlogs/jsonlogs.go` (modify — `SetLocalPrefixes`, classification precedence, model/instant substitution, explicit `local` cost-routing case)
- `internal/jsonlogs/jsonlogs_test.go` (modify — local-prefix routing and classification-precedence tests)
- `internal/store/store.go` (modify — `routed()`'s `local` case, `RepriceCosts`'s model/instant substitution at the `table.Compute` call, `statsSelectColumnsInner`'s third aggregate, `scanStatsSummary`/`scanStatsRow` third column, `StatsByModel`'s `PriceModel` assignment; new `internal/config` import)
- `internal/store/types.go` (modify — `StatsSummary.TotalLocalProjectedCostUSD`, `ModelStats.PriceModel`)
- `internal/store/merge.go` (modify — proxy-involved-side identity override, cost alignment, account-invariant post-condition, all inside `mergeEvents`; new `internal/config` import)
- `internal/store/merge_test.go` (modify — `TestMergeKeepsLocalSticky`, `TestMergeLocalZeroUsageTokensSurvive`, `TestMergeLocalIncompleteTokensSurvive`, `TestMergeLocalModeMismatch`, `TestMergeLocalAccountInvariant`)
- `internal/store/reprice_test.go` (modify — `TestRepriceRoutesLocalToApiEquivalent`)
- `internal/store/store_test.go` (modify — extend `TestBillingModeInvariants` to the `local` shape)
- `internal/pricing/pricing_test.go` (modify — `TestLocalPriceModelIsTheCheapestShippedDeepseekRow`; `TestLocalRowIsPricedAsDeepseekFlashBase` lives in `consumer_test.go`, above)
- `internal/quota` or `internal/api` quota test file (modify — extend `TestLocalRowsAreAbsentFromQuotaBurn` to the merged-row case)
- `internal/cli/stats.go` (modify — `writeGrouped`'s `group` struct and switch, aggregate `costCell` call sites)
- `internal/cli/stats_test.go` (modify — `TestWriteGroupedLocalBucket`)
- `internal/cli/format.go` (modify — `costCell` signature, `rowCosts` helper)
- `internal/cli/format_test.go` (modify — `TestCostCellLocalLabel`)
- `internal/cli/ls.go`, `internal/cli/tail.go`, `internal/cli/show.go` (modify — call `rowCosts`)
- `internal/cli/sessions.go` (modify — pass literal `nil` third argument)
- `internal/cli/models.go` (modify — `table[s.PriceModel]` lookup)
- `internal/api/models.go` (modify — composite `(Model, PriceModel)` fold key, `coverage()` lookup by `PriceModel`)
- `internal/api/models_test.go` (modify — `TestModelsCoverageShowsLocalPriced`, new `TestModelsEndpointKeepsLocalAndCloudRowsSeparate`)
- `internal/cli/doctor.go` (modify if the implementer finds a concrete gap beyond the automatic `config_valid` surfacing described above; otherwise note "no change needed" in Review Notes)

## Review Notes

Implemented. `go build`, `go vet` and `go test ./...` green (23 packages). Negative controls run: no `local` case in the consumer switch fails the consumer test; no `local` in `routed()` fails the reprice test; disabling the identity alignment fails the sticky/zero-usage/incomplete/mismatch merge tests; disabling the account invariant fails `TestMergeLocalAccountInvariant`. Deviations: (1) `alignBillingIdentity` acts only when one side's mode is `local`. The bead's general rule broke the existing `TestMergeMovesBillingModeWithCost` (a transcript re-ingest flipping subscription to api by design), and nothing in the plan asked to change api/subscription arbitration. (2) The merge nulls the winner's cost only when its mode differs from the final identity; where the modes agree the winner's figure is kept (the bead text read as unconditional). (3) The Models fold keeps one entry per (model, price model); the JSON gains an optional `price_model` field so the dashboard can tell the two entries apart. (4) The merge tests pin the combinations in which the proxy wins the token pick explicitly (setting CaptureComplete on the transcript side), since with both sides complete the incoming row wins. Consequence worth knowing: without `LocalModelPrefixes` set, a transcript row of a local call is classified subscription, wins the pick when it arrives second, and the row's cost goes to the unpriced convention until the prefixes are configured (bead 05) and `clens refresh`/`reprice` run. `doctor.go`: no change needed, `config_valid` already surfaces the new `Validate` error. Not done here: the `LocalModelPrefixes` config key and the tailer wiring (bead 05).
