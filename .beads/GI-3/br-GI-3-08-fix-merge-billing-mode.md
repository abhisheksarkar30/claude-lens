# Bead br-GI-3-08: `mergeEvents` moves `billing_mode` with the winning cost columns

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D6/D7 (fix as amended in **v9**), §4 (`store/merge.go`, `store/store_test.go`), §5 T8/T9/T9b/**T9c**, §6 R7 (plan sketch §9 bead 6)

- **Bead ID**: br-GI-3-08
- **Priority**: P0 (critical)
- **Original Estimate**: 1.5h
- **Dependencies**: None (structurally independent; it must land **before** br-GI-3-09's backfill)
- **Blocks**: br-GI-3-09

## Description

**The sharpest defect in the story, and it is currently silent.**

`mergeEvents` (`internal/store/merge.go:144-212`) takes `winner = incoming` when both captures are
complete, and the cost columns move with it (`merge.go:171-173`). But `account`/`billing_mode` use
`preferNonEmpty(existing, incoming)` (`merge.go:185-187`). `existing.BillingMode` is non-empty for
every row reachable today, so **a merge could never correct `billing_mode` — while it does adopt the
incoming cost.**

> **Amended in plan v9 (Phase 5.5, round 1).** This bead originally said "**always** non-empty".
> That universal is false: the column is `billing_mode TEXT NOT NULL DEFAULT ''`
> (`schema.sql:15`), and `billingModeForAuthKind` returns `""` for a credential the classifier does
> not recognize (`consumer.go:470-479`), which is what `ClassifyAuthKind` returns for anything that
> is neither `x-api-key` nor a `Bearer sk-ant-oat…` token (`authkind.go:35-59`). A capture-complete
> proxy row can therefore carry an empty mode, and an unconditional
> `merged.BillingMode = winner.BillingMode` would **blank** a non-empty one — dropping the row out
> of all three cost aggregates, which key on `billing_mode = 'api'`/`'subscription'`
> (`store.go:500-501`, `store.go:1113-1114`, `store.go:1178`). The fix below handles it.

Consequence once br-GI-3-07 lands: `clens ingest --rebuild` is the only re-pricing path (the store
has **no** reprice function; `--rebuild` zeroes every `jsonl:` cursor so the next poll re-reads
from byte 0). The incoming DeepSeek row now has `billing_mode='api'` and a priced `CostUSD`. The
merge would write that `cost_usd` onto a row still marked `billing_mode='subscription'` — **exactly
the pair invariant 5 forbids** — and `TestBillingModeInvariants` would not catch it, because it
exercises the **insert** path only and never a merge (`internal/store/store_test.go:180`).

**Fix — move `billing_mode` only onto `winner`, deriving it from the winner's cost column when the
winner has no mode:**

```go
// The ordinary case: the winner's mode labels its own cost columns.
merged.BillingMode = winner.BillingMode
// The winner's capture could not classify its credential, so it hands over no
// mode. Invariant 5 carries a figure's billing model in the column it lives in,
// so derive the label from the column the winner actually priced. Falling back
// to the loser's mode instead would pair the winner's CostUSD with a
// "subscription" label -- the exact illegal pair this change exists to remove,
// and one that contributes 0 to both aggregates because the subscription sum
// reads the very column now left NULL.
if merged.BillingMode == "" {
    switch {
    case winner.CostUSD != nil:
        merged.BillingMode = "api"
    case winner.ApiEquivalentCostUSD != nil:
        merged.BillingMode = "subscription"
    default:
        // The winner priced nothing, so there is no cost for a label to
        // describe and nothing for the loser's mode to contradict. Keep the
        // stored mode rather than blanking it -- an empty mode drops the row
        // out of every billing_mode-keyed grouping for no gain.
        merged.BillingMode = existing.BillingMode
    }
}
```

`billing_mode` is the one column a cross-source merge is now expected to contradict (the JSONL
tailer resolves it by model prefix, D5). The derivation matches what the writer already did: both
cold-path pricers route cost by `switch ev.BillingMode { case "subscription": ApiEquivalentCostUSD
…; default: CostUSD … }` (`consumer.go:317-320`, `jsonlogs.go:409-414`), so an empty mode already
means "the money went to `cost_usd`". Read the rule as "the label follows the money".

**The `default:` branch is not a retreat to the loser's mode.** It applies only when the winner
priced nothing, so no cost column exists for the adopted label to contradict — the invariant-5 risk
that rules the loser's mode out everywhere else is absent. It also gets this story's own case right:
a DeepSeek call through the proxy with an unrecognized credential leaves the winner unpriced and
modeless, and the stored JSONL row's prefix-derived `api` is the better answer to keep.

**The rejected repair, recorded so it is not re-proposed.** Taking the *loser's* mode when the
winner's is empty looks like the obvious minimal fix and is worse than the defect: it pairs the
winner's `cost_usd` with the loser's `subscription` label, producing the exact invariant-5 pair this
bead exists to remove, while contributing 0 to both aggregates anyway because the subscription sum
reads `api_equivalent_cost_usd`, which the winner left NULL.

**`Account` stays `preferNonEmpty(existing, incoming)`.** `Account` is a separate column the JSONL
tailer structurally cannot supply: the JSONL line "carries no auth signal" (`jsonlogs.go:76-84`).
On the ordinary ordering the proxy row is written live (`existing`, the side that carries a real
account name) and the JSONL row arrives minutes later (`incoming`), so moving `account` onto
`winner` would **blank the proxy row's account name** for no upside — a regression
`preferNonEmpty` was protecting. `AuthKind` likewise stays `preferNonEmpty`.

**Trade-off, recorded rather than buried.** A cross-source merge where the two sides disagree now
takes the JSONL side's `billing_mode`. The "in practice they agree" argument holds **only** for a
DeepSeek call that *also* passed through the proxy: it carries an `api_key` credential, so
`auth_kind` resolves it to `api` (`consumer.go:442-451`) and both sides already say `api`. The case
where the two sides genuinely disagree — a credential that resolves to `subscription` on a
prefix-`api` model — is **not** currently surfaced. `auth_kind_anomaly` fires on
`api_key + subscription` (`internal/analyze/rules.go:239-249`), a *different* case, and this change
removes the only production path that produced its trigger. Record it as a known gap in the code
comment; do not invent a mitigation here (it is §8 out-of-scope).

**Invariant 4 (do not break).** `mergeEvents` re-derives `TotalPromptTokens` as
`InputTokens + CacheWrite5mTokens + CacheWrite1hTokens + CacheReadTokens` (`merge.go:170`) — the
uncached-remainder invariant. This change must not disturb that line.

**Ordering (D7/R7).** This bead must land **before** the `clens ingest --rebuild` backfill
(br-GI-3-09), or the backfill itself manufactures the invariant violation across ~57k rows.

## Rationale

It is the minimal change that keeps the merged `billing_mode` and `cost_usd` consistent (invariant
5): move **only** the column a merge is expected to contradict, leaving `account` on
`preferNonEmpty` so a live proxy row is not blanked. The alternative — leaving `preferNonEmpty` and
suppressing the incoming cost when modes disagree — is worse: it would leave 57k rows unpriced
forever to protect a column from a value that is simply wrong.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- Insert a `subscription` DeepSeek row, merge a priced `api` DeepSeek row over it: the result has
  `cost_usd` set **and** `billing_mode='api'` **and** `api_equivalent_cost_usd` nil (T8).
- A merge where the incoming side is **not** complete leaves `billing_mode` unchanged (T9).
- A merge where the incoming JSONL side has an **empty** `account` preserves the non-empty proxy
  `account` (T9b).
- A merge whose winner is capture-complete with `billing_mode == ''` and its cost in `cost_usd`
  lands on `billing_mode='api'` with `cost_usd` intact — **not** blanked, and **not**
  `subscription`+`cost_usd` (T9c).
- `AuthKind` is still resolved by `preferNonEmpty`.
- `TotalPromptTokens` after a merge still equals the sum of the four prompt classes (invariant 4).

## Test Specifications

- Unit Tests (`internal/store/merge_test.go`) — T8/T9/T9b live here, not in `store_test.go`:
  `merge_test.go` is where every existing merge-behaviour test already sits
  (`TestMergeCollidingRequestID`, `TestMergePrecedenceTruncatedVsComplete`,
  `TestMergeRederivesSessionTotals`, …), and `T8` is a merge-precedence claim like theirs.
  - **T8**: insert a `subscription` DeepSeek row (`CostUSD` nil, mode `subscription`), merge a
    priced `api` DeepSeek row over it (same `request_id`), assert `CostUSD` set,
    `BillingMode == "api"`, `ApiEquivalentCostUSD == nil`. This is the regression that would have
    caught this story's own defect.
  - **T9**: a merge where the incoming capture is incomplete leaves `billing_mode` unchanged.
  - **T9b**: a merge where the incoming (JSONL) side has an empty `account` preserves the non-empty
    proxy `account` (`Account` stays `preferNonEmpty`). The case also pins `AuthKind` on
    `preferNonEmpty` by making the incoming side's value *different* and asserting the existing
    side's survives, with the incoming side's cost winning in the same merge — so a winner-based
    assignment cannot pass it by accident.
  - **T9c** (plan v9): merge a capture-complete proxy row with `BillingMode == ""` and a non-nil
    `CostUSD` over a stored `subscription` row. Assert the result is `BillingMode == "api"` with
    `CostUSD` intact and `ApiEquivalentCostUSD` nil. Add the mirror case — winner `BillingMode == ""`
    with `ApiEquivalentCostUSD` set → `"subscription"` — and the residual one — winner priced
    nothing → the stored mode survives instead of being blanked, and `CostUSD` /
    `ApiEquivalentCostUSD` both stay nil (the "subscription unpriced" / "api unpriced" shapes
    `TestBillingModeInvariants` already declares legal).
- Unit Tests (`internal/store/store_test.go`):
  - Extend `TestBillingModeInvariants` to cover the **merge** path, not just the insert path.
- Integration Tests: none.
- E2E: none (the plan accepts no E2E `--rebuild` against a real 57k-row store; T8 is where the
  logic lives).

## Files to Touch

- `internal/store/merge.go` (modify — `merged.BillingMode = winner.BillingMode`; comment recording
  the trade-off and the known gap)
- `internal/store/merge_test.go` (modify — T8/T9/T9b)
- `internal/store/store_test.go` (modify — extend `TestBillingModeInvariants` with the merge path)
