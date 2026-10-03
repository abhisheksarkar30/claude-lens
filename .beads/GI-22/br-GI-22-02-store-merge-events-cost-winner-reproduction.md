# Bead br-GI-22-02: Reproduce whether `mergeEvents` prefers a priced incoming row

**Plan Reference**: `docs/planning/GI-22-prices-set-clobber-and-rebuild-backfill-gaps.md` v8 (`<!-- version=8, status=converged -->`), §3.2, §5.2, §5.4, §7, §8 bead 02. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-22-02
- **Priority**: P0 (critical)
- **Status**: done
- **Original Estimate**: 1h
- **Dependencies**: None
- **Blocks**: br-GI-22-03
- **Commit**: `GI#22 chore: pin mergeEvents priced-over-unpriced cost winner (br-GI-22-02)`

## Description

This bead is a regression guard plus a recorded diagnostic. It does not change `mergeEvents`. Do not edit `internal/store/merge.go`. Do not start br-GI-22-03 from inside this bead.

`mergeEvents` (`internal/store/merge.go:223`) picks a completeness winner (`merge.go:250-264`) and then copies `CostUSD`, `ApiEquivalentCostUSD`, and `CostSource` from that winner (`merge.go:299-301`). When both `CaptureComplete` flags are true, `winner = incoming` (`merge.go:252-253`). A later correction (`merge.go:280-290`) swaps that winner back only when the winner has no observed usage and the loser does. `usageObserved` (`merge.go:512-516`) is true when any token column is non-zero. JSONL rows set `CaptureComplete: true` unconditionally (`internal/jsonlogs/jsonlogs.go:462`). On that path the incoming row wins the cost columns. Code inspection therefore expects `CostSource` `'user'` when the fixture below is valid. An `'unpriced'` result counts as the completeness-pick cause only when `incoming.CaptureComplete` is already true. With both flags at Go's zero value `false`, `winner` stays `existing` (`merge.go:250-264`) and `CostSource` is copied from that winner, so the result is `'unpriced'` even when incoming carries a real price. That is a broken fixture, not a confirmed root cause, and it must not be recorded as a reason to start br-GI-22-03.

Add `TestMergeEventsPrefersPricedOverUnpriced` to `internal/store/merge_test.go` (`package store`, so it can call unexported `mergeEvents` directly — do not go through `InsertEvent`). Construct the two `*Event` values and call `mergeEvents(existing, incoming)`.

- `existing`: `CostSource` `'unpriced'`, both cost pointers nil, `ModelResolved` `'claude-haiku-4-5-20251001'`, `CaptureComplete` set explicitly to `true` (the ordinary complete proxy row), and non-zero `InputTokens` and `OutputTokens`.
- `incoming`: the same non-zero token columns (so the `usageObserved` swap at `merge.go:280-290` does not apply), `CaptureComplete` set explicitly to `true`, `ModelResolved` the same string, `CostSource` `'user'`, and a non-nil cost computed against a `pricing.Table` that contains a real rate for that model. `internal/store/reprice_test.go` already imports `internal/pricing`; this test may too. Assign `CostUSD` from that `Compute` result for an `api` billing row (`BillingMode` on `fullEvent` defaults to `"api"`, `internal/store/store_test.go:52`). Do not leave `CaptureComplete` to a helper default: set both flags in the test body even if `fullEvent` (`store_test.go:68`) currently sets `true`.

Assert the returned event's `CostSource` is `'user'`.

**Expected outcome of a valid fixture.** The test passes. That pass means `mergeEvents` is not the cause of the six stuck `claude-haiku-4-5-20251001` rows. The test stays in the tree as the regression guard. It does not, by itself, explain those rows.

**Manual check, same bead.** The six stuck rows are not reproducible in CI. Query the operator database at `config.Default().DBPath`, which is `~/.clens/lens.db` (`internal/config/config.go:80` via `defaultPath`, `config.go:104-109`):

```sql
SELECT source, source_refs, model_resolved
FROM events
WHERE cost_source = 'unpriced'
  AND model_resolved = 'claude-haiku-4-5-20251001';
```

`source` and `source_refs` are columns (`internal/store/schema.sql:15` has `source_refs`; `model_resolved` is `schema.sql:23`). Also run `SELECT DISTINCT model_resolved FROM events WHERE cost_source = 'unpriced'` and compare each value byte-for-byte with the override-file key in `pricing.DefaultPath()` (`pricing.go:182-187`). `Table.Compute` looks the model up with `t[model]` and no normalization (`pricing.go:94-97`).

Write the result to `.beads/GI-22/evidence-02.txt`. Include the `go test` pass/fail line, the `CostSource` the test observed, the database path opened, the SQL rows (or `db: absent` if `lens.db` is not there), and the override-key comparison. End with one gate line, exactly one of:

- `bead-03: not-applicable` — the test passed (CostSource `'user'`), which is the expected result. Also use this line when the test passed and the SQL rows are all `source='proxy'` with no `jsonl` counterpart in `source_refs`, or when a stuck row's `model_resolved` does not byte-for-byte equal the override key. Those live-row findings explain the symptom without a `merge.go` change. Candidate (a) is already documented: `runIngest` (`internal/cli/ingest.go:26-68`) reaches rows only through `tailer.Poll` (`ingest.go:61`); lines 48-52 are `resetJSONLCursors`, not the poll. A row with no JSONL counterpart never enters `mergeEvents`.
- `bead-03: start` — only when the test failed by returning `CostSource` `'unpriced'` and the fixture's `incoming.CaptureComplete` was already `true`. A failure caused by a zero-value `false` flag is a broken fixture: fix the fixture in this bead until the flag is true, then re-run. Do not write `bead-03: start` for that broken fixture.
- `bead-03: not-applicable` as well when the database is absent. Absence is not confirmation of the completeness-pick hypothesis. Say `db: absent` in the file. The unit-test result alone still decides the gate line: a passing test is `not-applicable`; only the valid `'unpriced'` failure is `start`.

Do not edit `docs/context/`. The plan's §11 doc corrections are not this bead's.

## Rationale

An earlier live-database investigation already produced a false lead. A `merge.go` change written before this controlled reproduction would repeat that. The test is still worth keeping when it passes: it pins the cost-column copy to the completeness winner for a priced incoming row.

## Outcome Definition

`go test ./internal/store/ -count=1 -run TestMergeEventsPrefersPricedOverUnpriced` has been run, and `.beads/GI-22/evidence-02.txt` exists with the test result, the SQL result or `db: absent`, and exactly one `bead-03:` gate line as specified above. `internal/store/merge.go` is unchanged.

## Test Specifications

- `internal/store/merge_test.go`: `TestMergeEventsPrefersPricedOverUnpriced` — the fixture in Description. Asserts `CostSource` is `'user'`. Both sides have non-zero tokens. Both `CaptureComplete` flags are explicitly `true`.
- **Manual / recorded**: `.beads/GI-22/evidence-02.txt`, contents specified in Description. That file is the checkpoint br-GI-22-03 reads. Do not start br-GI-22-03 without it.

## Files to Touch

- `internal/store/merge_test.go` (modify — add `TestMergeEventsPrefersPricedOverUnpriced` only)
- `.beads/GI-22/evidence-02.txt` (create — the recorded test and SQL result)

## Review Notes

Added `TestMergeEventsPrefersPricedOverUnpriced` only. `internal/store/merge.go` is unchanged.

`go test ./internal/store/ -count=1 -run TestMergeEventsPrefersPricedOverUnpriced` passed. The fixture set both `CaptureComplete` flags to true and both sides had non-zero input and output tokens. The returned `CostSource` was `user`.

`config.Default().DBPath` resolved to `C:\Users\abhis\.clens\lens.db` (`HOME` unset, so `os.UserHomeDir`). That file is not present, so the SQL check is `db: absent` and no override-key comparison was possible. Recorded in `.beads/GI-22/evidence-02.txt`. Gate line: `bead-03: not-applicable`.
