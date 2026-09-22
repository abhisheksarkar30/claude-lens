# Bead br-GI-11-05: `Store.ReflagIncompleteCaptures` — repair laundered flags from the prefix witness

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4 Code (the `internal/store`
new-method row and the `internal/store/reflag_test.go` row) and §4's "The historical `capture_complete`
backfill — `clens reflag`" block (the witness predicate and the three-bucket report), §5 Unit (the
`Reflag` bullet and acceptance #3/#4), §6 (the `Historical rows` risk row), §8 (the reflag query),
Change History v6's NEW-SCOPE entry and v7 F5.1/v8 F6.3

- **Bead ID**: br-GI-11-05
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: None (the store method and its fixtures are self-contained; the forward fix is
  br-GI-11-04's and this bead repairs the history that fix cannot reach)
- **Blocks**: br-GI-11-06 (the CLI shell calls this method), br-GI-11-11 (the test-file count moves by
  this bead's new file)

> **The residual is defined exactly once, and the naive form is wrong.** `residual = capture_complete=1
> AND EXISTS(a stream_incomplete warning) AND no provable witness`. Without the warning join the
> predicate measures **healthy** complete rows — §8 records the conductor measuring it printing 2,298,
> not the 138 its comment claimed. Do not reinstate `cc = 1 AND NOT witness`.

> **The `length(...) IS NOT NULL` guard is load-bearing.** A body-less `capture_complete=1` row is a
> *real mode* here (`BodyPolicy: "full" | "off"`, `config.go:35`). Without the guard `length(req_body)`
> is NULL, the comparison is NULL, `NOT witness` is NULL, and `SUM` silently drops the row (measured: 62
> rows). Swapping in `COALESCE(length(req_body),0)` instead **falsely witnesses** it (measured: +33
> rows). The guard makes the witness require a stored body, which is what keeps every bucket summing to
> the scope total.

## Description

### Why a backfill is required, in the plan's own logic

RC-B's fix lives in `merge.go`, which runs **only when a merge happens**. Against an existing store it
therefore rewrites **no existing row**: the laundering already happened, and the merge that would apply
the new rule has long since run. Both acceptance #3 (the `cc=0` count rising) and acceptance #4 ("must
be **0**") are consequently **unrunnable as written** — the same defect class F1.10 caught in round 1,
where the "falls again" half was moved to a newly captured day; that fix covered only half the problem.
The repair for history must be its own step.

**Re-merging cannot recover it — the information was destroyed.** The `||` overwrote the proxy row's
original `capture_complete=0` with `true`; no stored bit records which side was truncated, so no
re-merge can reconstruct it. The **only** surviving witness is independent of the flag: a stored
`req_body` that is a strict prefix of its client `Content-Length`. That is what justifies the mechanism.

### The witness predicate — the one predicate

A stored body that is a strict prefix of its client `Content-Length`, on **either** side, guarded so a
body-less row can neither be falsely witnessed nor silently dropped. Acceptance #4 and §8's query use
**this same predicate**, so they cannot drift:

```sql
( (length(req_body)  IS NOT NULL AND COALESCE(CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(req_body))
  OR
  (length(resp_body) IS NOT NULL AND COALESCE(CAST(json_extract(resp_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(resp_body)) )
```

Apply it to the **response** side too where a `Content-Length` exists (`resp_headers`): a non-streamed
response over the cap is a real case, and br-GI-7-08 makes the flag cover **both** bodies. Measured:
the response side adds **0 rows** on this store (`flipped` request-only = 2,865 = request-or-response),
so a request-only form is equivalent *here* — a principled rule, not a speculative one.

### The method

`Store.ReflagIncompleteCaptures(ctx, ...)` performs **one** statement and returns **three** counts:

- **One write:** `UPDATE events SET capture_complete = 0 WHERE capture_complete = 1 AND <witness>`.
- **Three buckets the report needs:**
  - **flipped** = `capture_complete = 1` **and** a provable prefix witness → set to `0`.
  - **already honest** = `capture_complete = 0` already.
  - **residual** = rows with `capture_complete = 1` **and** an existing `stream_incomplete` warning
    **and** no provable prefix witness — the unrecoverable rows, **stated, not hidden**. Via a
    `warnings.kind='stream_incomplete'` join.
- **No `reconcileSessionTx` rollup.** Unlike the cost columns, `capture_complete` is **not** folded into
  `sessions`, so `reflag` does not carry reprice's rollup. Stated explicitly so nobody assumes that
  transaction shape carries over.

*The 59 is a different bucket from the 138, never summed into it.* Rows that *flip* to `cc=0` but carry
**no** warning are all-time **59** (the IST day **0**). The **59 is a sub-count of the `flipped` 2,865**;
the **138 is the `residual`** — the two are **disjoint and must never be added**. The 59 counts flips
that leave a `cc=0` row bearing no warning (a non-stream truncation; `ruleStreamIncomplete` requires
`IsStream`); the 138 counts rows the repair **cannot** reach. The only sum that closes is the four
buckets: `316 + 2,865 + 138 + 2,221 = 5,540`.

**Warnings are NOT synthesised.** `capture_complete` is the stored *fact*; `stream_incomplete` is
analyzer output whose inputs are not fully stored, and `ingest --rebuild` is already the repo's
analysis-refresh path. Synthesising the missing warnings would require re-deriving analyzer output from
inputs the store does not fully hold, so `reflag` deliberately does not — the flag-without-warning rows
are accepted as the visible, honest over-report they are.

### What this bead does not do

- **No CLI, no flags, no dispatch entry.** The shell is br-GI-11-06; the registration is br-GI-11-09.
- **No warning synthesis or deletion, no body/header/cost change.** It rewrites exactly one stored
  fact.
- **No session re-derivation** — see above.
- **No schema change.** The predicate reads stored columns only.
- **No touch of `internal/cli/cli_test.go`.** Its GI-11 changes are br-GI-11-06's single edit to that
  shared file; this bead's cases live in `internal/store/reflag_test.go`.

## Rationale

Without this, RC-B's fix is forward-only and acceptance #3/#4 cannot be met against the existing store:
the flag on 2,865 rows stays laundered. The repair is idempotent — it only ever narrows `1 → 0`, so a
second `--yes` run is a no-op — and its one real limit is the **residual**: rows laundered by the `||`
but carrying no `Content-Length` evidence cannot be repaired, and the command reports them rather than
guessing. That is the honest ceiling of a repair from surviving columns.

## Outcome Definition

- `Store.ReflagIncompleteCaptures` exists, performs the one `UPDATE` over the witness predicate, and
  returns the three counts (flipped / already honest / residual).
- It calls no session re-derivation.
- **Idempotent:** a second run flips nothing.
- A body-less `cc=1` row with no `Content-Length` is **not** witnessed and (absent a warning) is counted
  healthy, not residual — the `IS NOT NULL` guard holds.
- **§5 acceptance #3 (against a frozen copy of the live store, after `reflag --yes`):** under the union
  predicate `(source = 'proxy' OR instr(source_refs,'proxy') > 0)`, the number of `capture_complete=0`
  rows in the IST window `[1789929000000000000, 1790015400000000000)` rises from its **baseline of 128**
  to **1,304** (128 + 1,176). The 128 is two populations: **20** unmerged proxy rows carrying a
  `stream_incomplete` warning plus **108** non-stream truncations carrying none. Run the query *before*
  `reflag` to observe 128 and *after* to observe 1,304.
- **§5 acceptance #4 (same frozen copy, after `reflag --yes`):** the RC-B invariant query over the union
  predicate — `capture_complete = 1` while a stored body is a strict prefix of its `Content-Length` —
  returns **0**, using the **same witness predicate** this bead defines. It is 0 **because the backfill
  ran**, not because the code fix alone achieves it. The predicate is the prefix relation, **not**
  `length(req_body) = cap` (599 legitimate at-cap rows exist, and a `= cap` test is cap-bound and
  vacuous against the new 2 MB default).
  These figures are **IST-window figures, not the all-time figures**; the all-time snapshot is 2,865 /
  316 / 138 (2,221 healthy). Every figure is a **dated snapshot of a live store**, and the acceptance
  figures are the ones measured against the **frozen copy** (§1, §5).
- `go build ./...`, `go vet ./...`, `go test ./internal/store/` pass.

## Test Specifications

All in `internal/store/reflag_test.go`. **Name tests by their Go function name, never by a plan line
range.**

- **Unit Tests:**
  - `TestReflagFlipsAProvablePrefix` — a merged row whose flag was laundered and whose `req_body` is a
    strict prefix of a `Content-Length` header is flipped to `0`.
  - `TestReflagLeavesHonestRowsAlone` — an honest `cc=0` row is untouched and counted already-honest.
  - `TestReflagCountsTheWitnesslessLaunderedRowAsResidual` — a laundered `cc=1` row **carrying a
    `stream_incomplete` warning** with no witness is counted **residual** and left alone. (This is the
    case that fails if the warning join is dropped: without it, the row is healthy, not residual.)
  - `TestReflagDoesNotWitnessABodylessRow` — a body-less `cc=1` row (`BodyPolicy: off`) with no
    `Content-Length` is **not** witnessed and, absent a warning, counted **healthy**, not residual. This
    pins the `length(...) IS NOT NULL` guard so nobody drops it (and the `COALESCE(…,0)` mis-fix).
  - `TestReflagIsIdempotent` — a second run flips nothing.
  - `TestReflagDoesNotRewriteSessionTotals` — the owning sessions' stored columns are identical before
    and after, recording that `capture_complete` is not folded into `sessions`.
- **Integration Tests:** none in-repo. Acceptance #3/#4 above are run manually against a frozen store
  copy (see br-GI-11-11 and §5's `VACUUM INTO` note).

## Files to Touch

- `internal/store/store.go` (modify — add `Store.ReflagIncompleteCaptures` and its three-count query.
  **Also touched by br-GI-11-02** — keep the two edits disjoint: this bead adds only the reflag surface;
  do not reorder or reformat the reprice surface)
- `internal/store/reflag_test.go` (new — every case above)
