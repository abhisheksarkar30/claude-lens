# Bead br-GI-13-01: Every per-session derivation runs once per distinct session; `RecordCall` loses its dead `warningCount`

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §1.1 (the amplification), §3.1 (C1),
§5 D1 and D8, §6 tests 4/6/7, §7 R1, §8

- **Bead ID**: br-GI-13-01
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: None
- **Blocks**: br-GI-13-03 (the rename moves the two call sites this bead restructures), br-GI-13-05
  (its pipe-level case asserts the batch-end pass this bead creates), br-GI-13-06

> **This bead is the whole of C1 and it is build-atomic across three packages.** `RecordCall`'s
> signature changes, and both its declarations (`consumer/analyzer.go:32`,
> `jsonlogs/jsonlogs.go:55`) and its implementation (`session/session.go:96`) plus both call sites
> (`consumer/consumer.go:234`, `jsonlogs/jsonlogs.go:528`) must land together or the tree does not
> compile. Do not split it by package.

> **Do NOT make `prefix_hash` last-writer-wins to "simplify" the fold.** See D8 below. The column is
> INSERT-only by design and `store.go:1811` re-keys a session by passing `""`; a conflict clause that
> wrote `prefix_hash` would **blank** a re-keyed session's prefix.

## Description

### The rule

Every per-session derivation — the session-rule pass where one is wired, and the `RecordCall` fold
*always* — runs **once per distinct session, after every row of the batch is written**. Restructure
the consumer's `flush` (`consumer.go:205-238`) and the tailer's per-file loop (`jsonlogs.go:344-351`)
into three phases:

1. **Insert every row.** For each batch entry: `InsertEvent`, then `UpsertWarnings` for the row's own
   analyzer warnings. Record each distinct **returned** `sessionID`, in arrival order, with the
   **first inserted row** seen for it (see D8).
2. **Run the pass once per distinct session** (consumer only, where `sessionRule != nil`).
3. **Fold once per distinct session** (`RecordCall`).

The existing ordering comment (`consumer.go:225-227`) is *preserved* by that phase split, not
violated: passes still run before folds, so `warning_count`'s re-derivation still counts findings the
pass attached to earlier rows of the same session.

### The grouping key is `InsertEvent`'s returned session id, not `pe.ev.SessionID`

Unchanged from today, and load-bearing (`consumer.go:228-232`): a cross-source merge keeps the
*existing* row's session, so grouping on the event's own field would fold into the wrong session — or
create a session row owning no events. The tailer's `insert` already returns the written row's
session for the same reason (`jsonlogs.go:520-521`).

### The tailer: it gets the fold, not a pass it never ran

`internal/jsonlogs` declares `SetSessionRule` (`jsonlogs.go:145`) but `newTailer`
(`internal/cli/ingest.go:84-106`) never calls it, so `t.sessionRule == nil` and the guard at
`jsonlogs.go:523` is false for every row the tailer writes. **Leave the tailer's pass half exactly as
it is (unwired).** C1 gives the tailer the *fold*:

- `jsonlogs.insert` (`:498`) loses its last block (the `warningCount` local and the `RecordCall`
  call) and returns `(sessionID string, ok bool)` instead of `bool`.
- The tailer's loop (`:344-351`) takes the fold over: group the inserted rows by returned session id
  and fold **once per distinct session in that file**. A session spanning two files still gets one
  fold per file — a collapse from one per row, which is the point.

### D8 — the deduped fold passes the batch's first *inserted* row per session

Both folds pass the **first inserted row for that session in the batch**, in **arrival order** — the
row `flush` reaches first — **not the smallest `started_at`**, and **not the last row**.

This is forced, not chosen. `prefix_hash` is written only by the `INSERT` in `upsertSessionExec`
(`store.go:680-689`); its `ON CONFLICT(id) DO UPDATE` clause sets **only** `first_seen`/`last_seen`,
and `reconcileSessionTx`'s `UPDATE` never mentions `prefix_hash`. The column is therefore
**first-writer-wins**: today's per-row fold inserts the batch's *first* row's hash and every later
fold conflicts and leaves it alone. Folding once with the *last* row would insert a **different**
hash — reachable, because `prefixHash` (`parse/meta.go:144-158`) hashes the `system` value plus the
first `min(n,2)` messages, so a session's opening turn and its later turns differ.

Everything else is end-state-equivalent: `reconcileSessionTx` re-derives `request_count`, the token
SUMs, `MIN/MAX(started_at)`, `model_set`, the cost columns and the `warnings` COUNT from the tables,
and `first_seen`/`last_seen` self-correct through the `MIN`/`MAX` conflict clause (`store.go:685-686`,
`:771-772`). `ev` is consulted only for `prefixHash` and its own timestamp — which makes the fold row
an identity *except* for the hash, and that exception is exactly what pins it to the first.

> **The forbidden fix, stated once.** Do not reorder or deepen the `ON CONFLICT` clause to make
> `prefix_hash` last-writer-wins. The rekey path calls `upsertSessionTx(…, "")`
> (`store.go:1811`), so a conflict clause that wrote `prefix_hash` would blank a re-keyed session's
> prefix. The column's INSERT-only behaviour is load-bearing; the correct answer is the first fold
> row, not a changed upsert.

### Delete the dead `warningCount` parameter

`warningCount` is dead at every one of its declaration sites: `consumer/analyzer.go:32`
(`SessionAggregator`), `jsonlogs/jsonlogs.go:55` (`SessionRecorder`), `session/session.go:96`
(`Resolver.RecordCall`). `Resolver.RecordCall` calls `UpsertSession` then `ReconcileSession` and
**never reads the parameter**; `sessions.warning_count` is re-derived from the `warnings` table inside
`reconcileSessionTx` (`store.go:746-751`). This is *provable* dead, not suspected: grep `warningCount`
in `internal/session` returns the signature and nothing else. Once the fold is deduped there is no
per-row local to thread through anyway, so the deletion falls out of C1.

### The hot path is not touched

This bead changes no code in `internal/proxy`. The TTFB gate (`go test ./internal/proxy/`) must stay
green and is the proof: the consumer restructure must never make the proxy listener buffer the stream
to count tokens. Run it as part of this bead's verification, asserting it is *unchanged*.

## Rationale

The fold is `O(session rows)` work **per row** (`store.go:716-751`) at both sites, so it is quadratic
over a burst and runs on the store's single write connection (`SetMaxOpenConns(1)`). Section 1.1 of
the plan shows the amplification is real at both call sites; C1 targets the shared shape — per-session
work repeated per row — rather than either symptom alone. Without it, a burst of `defaultBatchSize`
rows re-derives one session's totals once per row, and the tailer does the same once per line.

The `warningCount` deletion is not cosmetic: leaving a dead parameter threaded through a now-deduped
fold would keep a signature that lies about what the fold consumes, and it survives only because the
value was never read.

## Outcome Definition

- `consumer.flush` inserts every row of the batch, then runs the session-rule pass **once per distinct
  returned session id**, then folds **once per distinct session id** — in that order.
- The tailer's per-file loop folds **once per distinct returned session id in that file**; a file of N
  lines for one session folds once, asserted.
- Both folds pass the **first inserted** row for the session in the batch, and the session row's
  `prefix_hash` equals that row's hash.
- `SessionAggregator.RecordCall`, `SessionRecorder.RecordCall` and `Resolver.RecordCall` no longer
  take `warningCount`; `grep -rn warningCount internal/` returns nothing.
- `sessions.warning_count` still equals `COUNT(*)` of the session's `warnings` rows after a multi-event
  batch (the invariant that survives the deletion).
- The tailer's `sessionRule` remains unwired (no new `SetSessionRule` call); no pass is run in the
  tailer.
- §6 test 1's subject — the session-scoped SELECT that the pass issues — is unchanged in shape here;
  the rename and index are br-GI-13-02/03.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/consumer/ ./internal/jsonlogs/ ./internal/session/`, plus
  `go test ./internal/proxy/` to confirm the TTFB gate is **unchanged**.

## Test Specifications

- Unit Tests:
  - `internal/consumer/consumer_test.go` — `TestFlushFoldsAndPassesOncePerSession`: a batch of a
    multi-event, single-session burst folds and passes **once per distinct session, not once per
    event**. Count `RecordCall` and `SessionEvents` invocations (a counting store, as
    `failingStore` already demonstrates at `:241-242`), asserting 1 each, and assert warning output
    is the same subset §3.1 predicts. Uses the existing `SetSessionRule` fixture (`:459`).
  - `internal/consumer/consumer_test.go` — `TestFlushPassRunsBeforeFold`: a two-row same-session batch
    where the pass attaches a warning to the first row; the fold that follows sees a
    `warning_count` counting it (the `consumer.go:225-227` ordering, preserved by the phase split).
  - `internal/consumer/consumer_test.go` — `TestFlushFoldsTheFirstInsertedRow`: a batch holding two
    rows for one session with **different** `PrefixHash` values (reachable — turn 1 vs turn 2 differ);
    the session row ends up with the **first inserted** row's hash. This is the one test that fails if
    "first" is read as `started_at` order or as "last" (D8, §6 test 6).
  - `internal/consumer/consumer_test.go` — `TestFlushDedupesAWriteThenReadOfIt`: a fixture whose batch
    contains a cache write and then a read of it produces **no** warning row for that session after
    the single pass; the converse — a session whose write is genuinely never read — still warns. This
    pins D1's chosen cadence in both directions (§6 test 4, §7 R1).
  - `internal/consumer/consumer_test.go` — `TestSessionWarningCountMatchesWarningRows`: after a
    multi-event batch, `sessions.warning_count` equals the `warnings` rows for the session, asserted
    through `reconcileSessionTx`'s own derivation so it cannot pass by both sides being wrong (§6
    test 7).
  - `internal/jsonlogs/*_test.go` — `TestTailerFoldsOncePerSessionPerFile`: a file of N lines for one
    session folds **once**; a second session in the same file folds separately. Assert by counting
    `RecordCall` calls on an injected recorder.
  - `internal/jsonlogs/*_test.go` — `TestTailerFoldsTheFirstInsertedRow`: the tailer's fold carries the
    first inserted line's hash for a multi-line single-session file (D8's tailer half).
  - `internal/session/session_test.go` — update the two calls at `:130`, `:145` from
    `RecordCall(ctx, …, ev, 0)` to the three-argument form; no behaviour change.
- Integration Tests: none here. The dedupe's end-to-end effect (a burst no longer re-derives per row)
  is measured in br-GI-13-06, not asserted against a live store.

## Files to Touch

- `internal/consumer/consumer.go` (modify — `flush` → three phases; `runSessionRule` (`:248`) returns
  nothing; delete the `warningCount` local; **do not** rename `SessionEvents` in the `Store` interface
  at `:44` — that is br-GI-13-03)
- `internal/consumer/analyzer.go` (modify — `SessionAggregator.RecordCall` (`:32`) loses
  `warningCount`)
- `internal/jsonlogs/jsonlogs.go` (modify — `insert` (`:498`) returns `(sessionID, ok)`, loses its fold
  block; the tailer loop (`:344-351`) gains the per-session fold; `SessionRecorder` (`:55`) loses
  `warningCount`. **Leave `runSessionRule` (`:550`) and the `SessionRule` (`:35`) interface exactly as
  they are** — the pass stays unwired; the `SessionEvents` identifier in that interface is renamed in
  br-GI-13-03)
- `internal/session/session.go` (modify — `Resolver.RecordCall` (`:96`) loses `warningCount`)
- `internal/consumer/consumer_test.go`, `internal/session/session_test.go`,
  `internal/jsonlogs/jsonlogs_test.go` (modify — the tests above; the two `RecordCall(ctx, …, 0)` call
  sites in `session_test.go` are the signature fallout)

**Shared-file note.** `internal/consumer/consumer_test.go` is also touched by br-GI-13-03 (the
`failingStore.SessionEvents` rename, `:241-242`) and br-GI-13-05 (its pipe-level case). Both depend on
this bead, so land this bead first and keep each later edit to its own region.
