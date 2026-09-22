# Bead br-GI-11-04: the merge stops laundering `capture_complete` — the flag follows the surviving bodies

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §2 RC-B, §4 Code (the
`internal/store/merge.go` and `internal/store/merge_test.go` rows), §5 Unit (the merge bullet), §6 (the
"RC-B changes the dashboard's flags, not its warning count" row and the merge-precedence row), §7 QA
(the rule-is-an-expression paragraph), Change History v2 F2.2 and v4 F4.3/F4.6

- **Bead ID**: br-GI-11-04
- **Priority**: P0 (critical)
- **Original Estimate**: 5h
- **Dependencies**: None
- **Blocks**: br-GI-11-10 (the behavior docs state the merge rule and the `capture_complete` meaning)

> **Assign the flag *after* the body backfill, not at its current site.** At `merge.go:241` the backfill
> has not run and `merged := *existing` (`:165`) means the test reads `existing`'s *pre-merge* bodies.
> On the jsonl-first / rekey shape (`existing` is the JSONL taker with **no** bodies, `incoming` is the
> proxy row holding them) the "any body" test reads false and falls through to `existing.CaptureComplete`
> — the JSONL row's `true` — the very laundering the `||` did. The §4 rekey test case ("flag follows to
> false") must pass at the new location; **if it cannot, the rule is wrong, not the test**.

> **The rule errs toward `false` deliberately.** A single row-level `bool` cannot say *which* body was
> cut, so an exact answer is impossible and the rule must state which way it errs. A false `cc=0` is a
> visible, honest over-report (an `incomplete` flag on a row that was whole); a false `cc=1` is exactly
> the laundering this whole story exists to remove. When the two retained bodies have **different**
> owners the flag is the **`&&`** of the two owners' flags — **never the `||`**.

## Description

### The defect

`merge.go:241` reads:

```go
merged.CaptureComplete = existing.CaptureComplete || incoming.CaptureComplete
```

`CaptureComplete` is set on the proxy side as `!respBuf.truncated && !st.reqBody.truncated`
(`proxy.go:107`) — it means "some body hit the read cap". A JSONL row has **no** bodies at all, so it
always reports `true`. The `||` therefore turns a truncated proxy capture into a complete one, **while
the merged row keeps the proxy's truncated body** (the backfill at `merge.go:323-328` takes bodies only
from whichever side has them; the transcript has none). This is br-GI-7-08's defect reintroduced one
layer up: that bead fixed the proxy submitting `!respBuf.truncated` alone, and the merge undoes it.

### The change

1. **Remove** the `||` assignment at `:241`.
2. **After** the body backfill at `:323-328`, derive the flag from **body ownership**:
   - Determine which side supplied each retained body — the request body and the response body are
     backfilled **independently**, so their owners can differ.
   - When the **request and response bodies share one owner** → that one owner's flag.
   - When the two retained bodies have **different owners** → the **`&&`** of the two owners' flags
     (complete only if **both** contributing sides were complete).
   - When the merged row holds **no** body → keep `existing.CaptureComplete`, which is what keeps the
     `--body-policy off` row (no bodies, flag `true`) honest.
3. **Rewrite the comment block at `:176-183`**, whose "the record that is whole is the safer one to
   quote" argument the `||` on the line below it contradicts. (That comment describes the *winner pick*
   and its reasoning survives; the flag assignment does not live there any more and must not be
   described as if it did.)

**The `--body-policy off` row and the errored proxy row are two different shapes, not one.** An
**errored** proxy row is **request-only**, not body-less: its `ErrorHandler` calls
`submit(0, nil, nil, false, err)` and `submit` tees the request body it holds
(`proxy.go:49-53`, `:215-224`), so it is the **single-owner** case and takes its one body's owner's
flag. "No bodies at all" is the `--body-policy off` shape. Do not conflate the two.

### The mechanism the plan rejected, and why

The winner-pick comment at `:176-183` argues a truncated proxy row should lose the pick to a whole
JSONL row. That argument is **not** the flag rule: the flag is downstream of the pick and must describe
the bodies the surviving row actually holds. The two can legitimately disagree, and where they do, the
bodies win. This bead does not change the pick — see §6's merge-precedence row: `mergeEvents` never
reads `merged.CaptureComplete`, so RC-B cannot change the pick inside the merge that applies it, and a
later merge's `usageObserved` guard (`:214-224`, `:372-376`) swaps it back.

### What this bead does not do

- **No analyzer change, no warning synthesis, no warning deletion.** The `stream_incomplete` warning is
  written once at insert (`consumer.go:210-214`) and `ruleStreamIncomplete` fires iff
  `IsStream && !CaptureComplete` (`rules.go:182-187`). The merge writes **only** `source_mismatch`
  (`:139-149`) and never deletes or re-derives a warning. RC-B makes the *flag* agree with the warning
  already attached; it does not add or remove a warning. The warning count does **not** jump.
- **No backfill of history.** This fix only prevents **new** laundering. The rows already laundered are
  `clens reflag`'s job (br-GI-11-05/06).
- **No change to the winner pick, `source_refs`, `session_id`, `first_source` or any token/cost
  column.**
- **No cap change.** RC-B and RC-C ship together (br-GI-11-07), but a truncated capture re-captured
  under the 2 MB cap is a *newly captured day* concern, not this bead's.

## Rationale

Measured across the union predicate's 5,540 rows, **2,865 are false-complete** and every one is a
**merged** row (2,844 proxy-first + 21 jsonl-first); the unmerged bucket never lies (0 false-complete,
316 honest truncations). The `||` overwrites the proxy row's original `capture_complete=0` with `true`
and no stored bit records which side was truncated. The forward fix is this bead; the historical repair
is `reflag`, because re-merging cannot recover what the `||` destroyed.

## Outcome Definition

- `mergeEvents` assigns `CaptureComplete` **after** the req/resp body backfill, from the owner(s) of
  the retained bodies, never the `||`.
- The seven body-ownership shapes below are pinned by tests, and the merged row's **bodies and its flag
  agree** in each.
- `TestMergePrecedenceTruncatedVsComplete` is **inverted** (its assertion becomes
  `CaptureComplete == false`), not deleted.
- `TestBillingModeInvariants`, the `source_mismatch` tests, and
  `TestMergeDoesNotLetABodylessRowZeroObservedUsage` stay green.
- `go build ./...`, `go vet ./...`, `go test ./internal/store/` pass.

## Test Specifications

All in `internal/store/merge_test.go`.

- **Unit Tests — the seven body-ownership cases:**
  1. **proxy-first, proxy truncated** → flag stays `false`. (`TestMergePrecedenceTruncatedVsComplete`,
     **inverted** — its fixture inserts a truncated `fullEvent` first, so under the new rule the
     surviving bodies are that side's and the assertion at `:297-298` becomes
     `CaptureComplete == false`; the same treatment §5 applies to `TestComputePeakRoundsPerClass`.)
  2. **proxy-first, proxy whole** → flag stays `true`.
  3. **rekey collision**: a JSONL taker (`existing`, no bodies) takes the proxy's bodies (`incoming`)
     → flag **follows to `false`** when the proxy side was truncated. This is the case the old
     assignment site gets wrong, and the reason the flag must move below the backfill.
  4. **`--body-policy off`**: the merged row holds **no** body → flag stays `true`
     (keeps `existing.CaptureComplete`). Related existing coverage:
     `TestMergeDoesNotLetABodylessRowZeroObservedUsage`.
  5. **both sides carry bodies** — the shape the existing fixtures already produce (`fullEvent` sets
     both `ReqBody` and `RespBody`, `store_test.go:72-73`) → the surviving flag follows the side the
     **kept** bodies came from.
  6. **errored proxy row, request-only** → a single-body row takes **its one body's owner's** flag.
     `proxy.go:49-53` calls `submit(0, nil, nil, false, err)` and `submit` tees the request body it
     holds (`:215-224`), so the row is request-only under the default policy — a *different* shape from
     case 4's no-body row, **not** a restatement of it.
  7. **mixed-owner** — `existing` holds only one body and `incoming` the other (request from one side,
     response from the other) → the surviving flag is `false` if **either** owner was incomplete;
     this pins the **`&&`**. It is **defensive, not reachable end-to-end** (a `request_id` has exactly
     one proxy row and jsonl rows carry no body), so it is pinned by a **direct `mergeEvents` unit test
     on constructed inputs** — the rule is conservative precisely so that *if* the shape is ever
     reachable the row errs to `false`, **never** the `||`. No other case splits the two retained
     bodies across two owners, so none of the others exercises the `&&`.
  - **Bodies-and-flag agreement** — an assertion that the merged row's retained bodies and its
    `CaptureComplete` agree; this is the invariant the `||` broke, and it is the reason the other seven
    cases are not merely a list.
- **Unit Tests — the fixture that is load-bearing for its flag assertion:**
  `internal/store/merge_test.go:873-875` (`TestMergePrefersTheWhollyCapturedRowOverATruncatedRequest`)
  asserts `CaptureComplete == true` and survives only because its `existing` side carries bodies. Its
  JSONL side is a `fullEvent` that sets **both** bodies (`store_test.go:72-73`), but a *real* JSONL row
  has none (`jsonlogs.go:436`) — so under the `&&` rule a production-shaped fixture would take the
  proxy row's bodies and the flag would follow to `false`, the inverse of the assertion. **Give the
  JSONL side no bodies and invert the assertion** (the same treatment case 1 gets), **or** state in the
  test comment that the bodies are artificial and the flag assertion is incidental. Leaving it as-is
  pins the flag via a body set the JSONL source can never supply.
- **Re-grep the tree for other assertions on the merged flag** before calling the bead done — the plan
  names in particular the `:873-875` site; a sibling copy missed here is the failure mode
  br-GI-9-04 records.
- **Integration Tests:** none. The end-to-end statement of RC-B is §5's acceptance #3/#4 (run manually
  after `reflag` against a frozen store copy; br-GI-11-06/11).

## Files to Touch

- `internal/store/merge.go` (modify — remove the `||` at `:241`; add the body-ownership derivation
  **after** the backfill at `:323-328`, with the `&&` for the mixed-owner case and
  `existing.CaptureComplete` for the no-body case; rewrite the winner-pick comment block at `:176-183`)
- `internal/store/merge_test.go` (modify — the seven cases above; invert
  `TestMergePrecedenceTruncatedVsComplete`; fix or re-comment
  `TestMergePrefersTheWhollyCapturedRowOverATruncatedRequest`'s flag assertion; keep
  `TestBillingModeInvariants` and the `source_mismatch` tests green)
