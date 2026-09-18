# Bead br-GI-1-09: Analyzer engine, kind catalogue, and T1 cache rules

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Analyzer rule catalogue (T1), §Cold-path pipeline (Analyzer seam), §Cross-source identity (warning upsert), tests 13, 14, 11a

- **Bead ID**: br-GI-1-09
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-07, br-GI-1-08
- **Blocks**: br-GI-1-10, br-GI-1-17

## Description

The rule engine and the cache findings — the flagship class of this tool, because cache misbehaviour
costs money, raises no error, and is invisible.

**The engine.** `internal/analyze` is **pure** — no I/O, no store writes, no config mutation — so the
whole rule matrix is testable from synthetic values. Entry point, unchanged from deepseek-lens:

```go
func Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning
```

It satisfies `consumer.Analyzer` structurally; `analyze` imports `parse`, `store`, `config`,
`pricing` — **never** `consumer` (the interface lives in `consumer`, so the direction holds). Rules
are a table in `rules.go`: a new rule is one table entry and one test case, never a pipeline change.
`Analyze` returns `nil` — not an empty slice — when nothing fires. Severities: `error` (the request
may have failed or lost content), `warn` (cost/quality/safety divergence), `info` (dropped with no
practical consequence).

**`kinds.go` — the single source of truth for kind spellings.** Declare every T1 kind as a typed
`Kind` constant, with an `allKinds []KindInfo` list pairing each with its one-sentence description
(the description is the product and lives beside the kind, not in the markdown). This is what makes
`cache_prefix_below_minimum` the one spelling everywhere — the T1 table, `kinds.go`, the README kind
table and the tests — since a variant spelling is a build failure under `readme_test.go` (br-GI-1-19).

Because several T1 kinds are emitted by packages other than `analyze` (the merge path, the quota
engine, the reconcile path, the consumer), `kinds.go` also carries a **`nonAnalyzeKinds`** list —
the claude-lens generalisation of deepseek-lens's `consumerKinds` — naming those kinds so the
README-consistency test counts them as expected-but-not-here rather than as extras. At minimum:
`analyzer_panic` (consumer), `source_mismatch` (store merge), `cost_drift` (reconcile),
`quota_window_approaching` (quota), plus the collector/transport kinds.

**T1 cache rules owned here.** Two are per-event and live in the pure seam directly:

| Kind | Sev | Fires when |
|---|---|---|
| `cache_breakpoints_exceeded` | error | More than 4 `cache_control` breakpoints in one request (a 400). |
| `cache_prefix_below_minimum` | warn | A `cache_control` breakpoint is present but the prefix is under the model's minimum cacheable length, so `cache_creation_input_tokens` is 0 and the marker did nothing. The minimum is **not monotonic across generations** and is therefore a **table, not a rule of thumb**: 512 on Opus 5 / Fable 5 / 5.1 / Mythos; 1024 on Opus 4.8 / Sonnet 5 / 4.6 / 4.5; 2048 on Opus 4.7; 4096 on Opus 4.6 / 4.5 / Haiku 4.5. This is the one rule whose *firing* genuinely needs the captured body. |

The other five cache rules are **session-scoped**: they compare consecutive calls, not one event.

| Kind | Sev | Fires when |
|---|---|---|
| `cache_prefix_invalidation` | warn | `cache_creation_input_tokens` ≈ full conversation size on every turn of a session while `input_tokens` stays small — a silent invalidator upstream of the breakpoint, so every turn pays full price. Detection is a token-ratio signal source B can compute alone; the captured body upgrades it from "your cache keeps breaking" to "this byte broke it" (localisation). |
| `cache_invalidated_by_tools` | warn | The `tools` array or top-level `system` changed between consecutive calls in a session, invalidating the entire cache from position 0. The *effect* shows in source B's usage; the *cause* — which array changed — needs the captured bodies. |
| `cache_write_never_read` | warn | Write tokens recorded with no subsequent read of that prefix within the TTL — the write premium was paid for nothing. |
| `cache_ttl_mismatch` | info | An `ephemeral_1h` write where the start-to-start gap between reads stayed under 5 minutes (the 5-minute TTL was strictly cheaper), or a 1-hour write with no read in the 5–60 minute band — the only window where the doubled write pays off. |
| `cache_expired_between_turns` | info | A prefix that was cached is re-written instead of read, with a start-to-start gap longer than the TTL it was written with. |
| `cache_concurrent_write_race` | info | N overlapping calls with identical prefixes: an entry is only readable after the first response *begins* streaming, so all N paid full price. |

**The context a session-scoped rule needs — and where it comes from.** The carried-over `Analyzer`
seam is per-event and pure, which those five rules cannot be satisfied from directly. The plan does
not say where they get their history. **This bead's resolution** (flagged in the summary as a
decomposition decision, not the plan's): a **session-scoped analysis pass** runs where the session
fold already runs (br-GI-1-08's post-insert aggregator, extended here), receiving the session's recent
rows (bounded by the session gap) and attaching each finding to the id of the **event that completed
the pattern** — so `warnings.event_id` stays NOT NULL and `UNIQUE(event_id, kind)` still holds, and
`warning_count` (re-derived from `warnings`, br-GI-1-06) counts it once. `analyze` exposes this as a
second pure entry point over a slice of rows, so the rules stay testable without a store.

**The warning-ledger guarantee.** Each rule owns its kind and emits **at most one finding of that kind
per event row** — the one deepseek-lens rule that emitted several findings of one kind
(`unsupported_content_block`) is DeepSeek-specific and is not carried over. The seam runs after
**every** insert, including a cross-source merge, so a call seen by both sources runs the rules twice
on one `event_id`; the store's `(event_id, kind)` upsert (br-GI-1-06) makes the second run an update,
so the two sources contribute the **union** of their findings with no duplicate kinds and no inflated
`warning_count` (test 11a's ledger assertion).

## Rationale

Cache behaviour is the canonical silent-cost failure, and the minimum cacheable length being
non-monotonic across generations is exactly why a rule of thumb would be wrong. Declaring the kind
catalogue in one file is what keeps the README honest mechanically rather than by diligence.

## Outcome Definition

- `go test ./internal/analyze/... -race` passes.
- A healthy synthetic loop (reads growing, writes small) raises nothing (test 13's negative).
- A synthetic invalidator loop (writes ≈ full conversation every turn) raises
  `cache_prefix_invalidation` (test 13).
- A marker below the model minimum raises `cache_prefix_below_minimum`; the non-monotonic cases — a
  3K prefix caches on Opus 5 but not on Opus 4.6 — are asserted explicitly (test 14).
- More than 4 breakpoints → `cache_breakpoints_exceeded`.
- Every rule has a positive and a negative test; a clean session raises nothing.
- `Analyze` returns `nil` for a clean event.
- Each kind appears at most once per `event_id`; `cache_prefix_below_minimum` is spelled identically
  in `kinds.go` and every test.

## Test Specifications

- Unit Tests (`internal/analyze/analyze_test.go`, table-driven over synthetic values):
  - **Clean event** → `nil` (the most important negative).
  - **Cache minimum (test 14)**: below-minimum marker → `cache_prefix_below_minimum`; at-minimum → none;
    a 3K prefix on Opus 5 → none but on Opus 4.6 → fires.
  - 5 breakpoints → `cache_breakpoints_exceeded`; 4 → none.
  - `cache_prefix_invalidation` (test 13): an invalidator loop fires; a healthy loop does not.
  - `cache_invalidated_by_tools`: a changed `tools` array between two calls fires; identical arrays do not.
  - `cache_write_never_read`, `cache_ttl_mismatch`, `cache_expired_between_turns`,
    `cache_concurrent_write_race`: each one positive and one negative fixture.
  - A body whose TTL split is absent does not raise a false cache rule.
  - Multi-rule: an event violating several rules → exactly one warning per kind, no duplicates.
- Unit Tests (`kinds.go` contract):
  - `AllKinds()` returns a copy; every kind has a non-empty description with no `|` or newline.
  - `nonAnalyzeKinds` names at least `analyzer_panic`, `source_mismatch`, `cost_drift`,
    `quota_window_approaching`.
- Integration Tests (br-GI-1-11 owns the merge-ledger assertion):
  - Re-running the seam on one `event_id` leaves `COUNT(*) == COUNT(DISTINCT kind)` and the session's
    `warning_count` equal to the distinct-kind count (test 11a).
- E2E: none.

## Files to Touch

- `internal/analyze/analyze.go`, `internal/analyze/analyze_test.go` (create)
- `internal/analyze/kinds.go` (create — all T1 kinds + `nonAnalyzeKinds`)
- `internal/analyze/rules.go` (create — the cache rules)
- `internal/analyze/rules_test.go` (create)
- `internal/store/store.go` (modify — a session-scoped recent-rows read for the cache pass)
- `internal/consumer/consumer.go` (modify — invoke the session-scoped pass inside the insert/merge
  transaction, after the per-event seam)
