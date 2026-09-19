# Bead br-GI-3-08: `mergeEvents` moves `billing_mode` with the winning cost columns

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D6/D7, §4 (`store/merge.go`, `store/store_test.go`), §5 T8/T9/T9b, §6 R7 (plan sketch §9 bead 6)

- **Bead ID**: br-GI-3-08
- **Priority**: P0 (critical)
- **Original Estimate**: 1.5h
- **Dependencies**: None (structurally independent; it must land **before** br-GI-3-09's backfill)
- **Blocks**: br-GI-3-09

## Description

**The sharpest defect in the story, and it is currently silent.**

`mergeEvents` (`internal/store/merge.go:144-212`) takes `winner = incoming` when both captures are
complete, and the cost columns move with it (`merge.go:171-173`). But `account`/`billing_mode` use
`preferNonEmpty(existing, incoming)` (`merge.go:185-187`). Since `existing.BillingMode` is
**always** non-empty, **a merge can never correct `billing_mode` — while it does adopt the incoming
cost.**

Consequence once br-GI-3-07 lands: `clens ingest --rebuild` is the only re-pricing path (the store
has **no** reprice function; `--rebuild` zeroes every `jsonl:` cursor so the next poll re-reads
from byte 0). The incoming DeepSeek row now has `billing_mode='api'` and a priced `CostUSD`. The
merge would write that `cost_usd` onto a row still marked `billing_mode='subscription'` — **exactly
the pair invariant 5 forbids** — and `TestBillingModeInvariants` would not catch it, because it
exercises the **insert** path only and never a merge (`internal/store/store_test.go:180`).

**Fix — move `billing_mode` only onto `winner`:**

```go
merged.BillingMode = winner.BillingMode   // was preferNonEmpty(existing, incoming)
```

`billing_mode` is the one column a cross-source merge is now expected to contradict (the JSONL
tailer resolves it by model prefix, D5).

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
