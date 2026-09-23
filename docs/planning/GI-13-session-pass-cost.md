# GI-13 — the session-scoped rule pass, and a profiler to find the next one

**Status**: **v8, CONVERGED** — cross-review rounds 1–8 complete (findings per round: 9, 4, 2, 5, 4, 1,
1, 1 — all applied) plus one user decision (D9, the rule fix). Rounds 4, 6, 7 and 8 were clean, so the
{7,8} window holds and **the plan has converged**. §3.1 is frozen after round 6's recommendation.

**One MINOR is carried open (F8.1), deliberately rather than fixed.** Two sentences in §3.1's class-3
caveat over-generalize the class-2/class-3 boundary: "when a real row follows the usage-less one … it
re-anchors forward" is false when that row is a *read* (the walk then aborts and the finding is class
1), and "class 3 is **exactly** the case with no re-anchored successor" over-reaches because class 1's
losses also have no successor. **The exact correction — scope "a real row" to "a real row that
continues the rewrite run", and soften "exactly" to "the case it demonstrates" — is recorded in this
revision's commit message and in `br-GI-13-05`**, where the classification is actually implemented. No
further edit was made to §3.1 because three successive edits to that boundary (rounds 6, 7, 8) each
produced a new finding, and the claims at issue are explanatory — they change no bead, no test and no
behaviour. Correcting them is a one-commit change at bead time; re-deriving §3.1 for it is the regress
the freeze exists to stop.
**Issue**: #13 (`GI#13`). **Branch**: `GI-13-session-pass-cost`, cut from `main` at `9ee53f3`.
**Provenance**: diagnosed 2026-09-22 by CPU profile of the live process. This plan supersedes
nothing; it settles the question `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md:1654-1668`
explicitly deferred to "the next story".

---

## 1. The bug

`clens serve` degrades to unusable as the store grows: `GET /api/health` answers in ~5 ms while
every route that touches SQLite takes 5–46 s. The 30-second CPU profile recorded while it was in
that state showed **96.5% of a core**, spent in — in `go tool pprof -top` order —
`runtime.cgocall` 89.9%, `_sqlite3VdbeExec` 94.2%, `_winRead` 57.5%,
`_vdbeColumnFromOverflow` 34.6%, `_accessPayload` 34.2%, `_vdbePmaWriteBlob` 31.6%,
`_vdbeIncrSwap` 27.5%. (Those are one sample's printed figures, cited as the recorded signature
rather than as a claim about magnitudes. The shape is the claim.) The call chain:

```
consumer.(*Consumer).Run        consumer.go:188
 └─ consumer.(*Consumer).flush  consumer.go:222
     └─ runSessionRule          consumer.go:249
         └─ store.SessionEvents store.go:337
             └─ sqlite _sqlite3VdbeSorterNext → _vdbePmaReadBlob → _winRead
```

`_vdbePmaWriteBlob` + `_vdbeIncrSwap` + `_vdbePmaReadBlob` are the *sort spilling to a temp file*;
`_vdbeColumnFromOverflow` + `_accessPayload` are *reading BLOBs off overflow pages*. Both are
explained by one query, `store.SessionEvents` (`store.go:336`):

```go
rows, err := s.db.QueryContext(ctx, eventSelectColumns+" FROM events WHERE session_id = ? ORDER BY started_at ASC", sessionID)
```

- `eventSelectColumns` is **all 47 columns**, including `req_body`, `resp_body` and
  `transcript_content`.
- `ORDER BY started_at` has **no index to satisfy it**. `schema.sql:68-70` provides
  `idx_events_session_id`, `idx_events_started_at` and `idx_events_cost_source` — none of them
  `(session_id, started_at)`. So SQLite materialises the session's rows, carries the multi-MB BLOBs
  through an external merge sort, and **spills them to a temp file on disk**.
- The store is `SetMaxOpenConns(1)`, so the query holds the *only* connection for its whole 20–40 s
  and every dashboard read queues behind it. That is what makes one slow query look like a dead
  application, and it is why `/api/health` — which touches no DB — stays fast.

### 1.1 Why it is amplified, not merely slow

Two different per-session derivations run **once per written row**, and they are wired differently.
This split is the part an earlier draft of this plan got wrong, so it is spelled out.

**The session-rule pass** — the one issuing the query above — has exactly one caller:
`consumer.flush` (`consumer.go:205-238`) runs it inside its per-event loop
(`consumer.go:221-223`), so one flush of `defaultBatchSize = 50` (`consumer.go:30`) events from a
single session issues 50 whole-session re-reads, each of which sorts and spills.

**The JSONL tailer does not run that pass at all.** `internal/jsonlogs` declares `SetSessionRule`
(`jsonlogs.go:145`) but `newTailer` (`internal/cli/ingest.go:84-106`) never calls it — the only
production `SetSessionRule` site is `serve.go:101`, on the *consumer* — so `t.sessionRule == nil`
and the guard at `jsonlogs.go:523` is false for every row the tailer writes. It is dead code, and a
1,259-line ingest burst is **not** a multiplier for this query. GI-9's frozen plan records the same
wiring at `:1659` ("`serve` is `SetSessionRule`'s only caller").

**The fold — `RecordCall` → `ReconcileSession` — does run per row, at both sites**:
`consumer.go:233-237` per event and `jsonlogs.go:527-531` per inserted line. That fold re-derives
the session's whole aggregate and its warning count from scratch on every call
(`store.go:716-751`), so it is `O(session rows)` work *per row* — quadratic over a burst — and it is
on the single connection too.

So the amplification is real at both call sites, for two different reasons: one site re-reads and
re-sorts the session's bodies per row, and *both* re-derive the session's totals per row. Both
goroutines share the one connection. The cost is `passes × (rows × body bytes)` plus
`folds × rows`, and every factor grows on its own. C1 below targets the shared shape — **per-session
work repeated per row** — rather than either symptom alone.

### 1.2 The bound that used to hold, and stopped

`SessionEvents`' own doc comment (`store.go:329-335`) already records this:

> Before D7 a session's row count was bounded by the session resolver's own gap window … D7 makes a
> session the whole conversation a header names, so that bound no longer holds and nothing here
> replaces it.

So this is a known, documented, unfixed consequence of GI-9's rekeying — not a regression GI-11
introduced. `internal/consumer`, `internal/analyze` and this query are untouched by GI-11
(`git log origin/main..HEAD -- internal/consumer internal/analyze` is empty at the merge base).
GI-11's 256 KB → 2 MB body-cap raise does make each carried row larger, which lowers the store size
at which this becomes visible.

---

## 2. What the rules actually need

`internal/analyze/analyze.go:34-41` lists six session rules: `ruleCachePrefixInvalidation`,
`ruleCacheInvalidatedByTools`, `ruleCacheWriteNeverRead`, `ruleCacheTTLMismatch`,
`ruleCacheExpiredBetweenTurns`, `ruleCacheConcurrentWriteRace`. Across all six, exactly **nine
columns** are read: `id`, `started_at`, `ended_at`, `total_prompt_tokens`, `cache_write_5m_tokens`,
`cache_write_1h_tokens`, `cache_read_tokens`, `prefix_hash`, `req_body`. Only
`ruleCacheInvalidatedByTools` touches `req_body` — it compares consecutive bodies.

`req_body` staying is what makes this interesting: it is the *dominant* body (`config.go:39-46`:
"a Claude Code request carries the system prompt, the full tool schemas and the conversation
history"), so the one blob the rules need is the expensive one. Dropping the other five blobs is
worth having, but it cannot be the fix on its own — which is why §3.1 is load-bearing and §3.3 is
not.

---

## 3. The change

Four parts, in leverage order.

### 3.1 C1 — every per-session derivation runs once per distinct session, not once per row

The rule: **every per-session derivation — the session-rule pass where one is wired, and the
`RecordCall` fold always — runs once per distinct session, after every row of the batch is
written.** Restructure both call sites into three phases: **insert every row → run the pass once
per distinct session → fold once per distinct session.** The existing comment at
`consumer.go:225-227` ("Run last, after the session-scoped pass, so warning_count's
re-derivation counts findings that pass attached to earlier rows in this same session too") is
preserved by that ordering, not violated by it: passes still run before folds.

**The grouping key is `InsertEvent`'s returned session id, not `pe.ev.SessionID`.** The existing
comment at `consumer.go:228-232` is explicit about why: a cross-source merge keeps the *existing*
row's session, so grouping on the event's own field would fold into the wrong session — or create a
session row owning no events. The same applies to the tailer, whose `insert` already returns the
written row's session for this reason (`jsonlogs.go:520-521`).

`jsonlogs.insert` (`jsonlogs.go:498`) currently does the insert, the per-event warnings and the fold
itself, so it loses its last block and returns `(sessionID string, ok bool)`; the tailer's loop
(`jsonlogs.go:344-351`) takes the fold over and runs it once per distinct session *in that file*. A
session spanning two files still gets one fold per file — a collapse from one per row. **The
tailer's pass half is left exactly as it is** (unwired, per §1.1): C1 gives the tailer the fold, not
a pass it never ran.

**Both folds pass the *first inserted* row for that session in the batch, and this is the one part of
C1 that is not interchangeable.** "First" means **arrival order** — the row `flush` reaches first —
not the smallest `started_at`. `flush` folds `pe.ev` in batch order (`consumer.go:205-234`) and that
is what today stores; §6 test 1 already records that rowid order and `started_at` order can disagree,
so reading "first" the other way would re-create exactly the defect below. `prefix_hash` is written
only by the `INSERT` in
`upsertSessionExec` (`store.go:680-689`); its `ON CONFLICT(id) DO UPDATE` clause sets **only**
`first_seen` and `last_seen`, and `reconcileSessionTx`'s `UPDATE` (`store.go:761-773`) never mentions
`prefix_hash` at all. The column is therefore *first*-writer-wins: today's per-row fold inserts the
batch's **first** row's hash, and every later fold conflicts and leaves it alone. Folding once with
the *last* row would insert a **different** hash.

That difference is reachable, not theoretical. `prefixHash` (`parse/meta.go:144-158`) hashes the
`system` value plus the first `min(n,2)` messages, so a session's opening turn (one message) and its
later turns (two or more) hash differently. A backed-up flush spanning a session's early turns
contains both values, so choosing the wrong end of the batch lands the wrong one on the session row —
visible in `ListSessions` and on the session detail.

> **Do not "fix" this by making the conflict clause last-writer-wins.** The rekey path calls
> `upsertSessionTx(…, "")` (`store.go:1811`), so a conflict clause that wrote `prefix_hash` would
> **blank** a re-keyed session's prefix. The column's INSERT-only behaviour is load-bearing; the
> correct answer is the first fold row, not a changed upsert.

Everything else *is* end-state-equivalent, because `reconcileSessionTx` re-derives from the tables
rather than from the argument: `request_count`, the token SUMs, `MIN(started_at)`/`MAX(started_at)`
(`store.go:729-730`), `model_set`, the cost columns and the `warnings` COUNT (`store.go:746-751`) are
all read back out of `events` and `warnings`, and `first_seen`/`last_seen` self-correct through the
`MIN`/`MAX` conflict clause (`store.go:685-686`) and `reconcileSessionTx`'s
`COALESCE(MIN(first_seen, ?), first_seen)` (`store.go:771-772`). `ev` is consulted only for
`prefixHash` and its own timestamp — which makes the fold row an identity *except* for the hash, and
that exception is exactly what pins it to the first.

**Why this is safe, and the one place it is not free.**

- The pass's only store-visible effect is the warnings it upserts (`consumer.go:262-268`,
  `jsonlogs.go:564-570`). Its return value is folded into a `warningCount` argument that
  `session.Resolver.RecordCall` (`session.go:96-111`) **never reads** — it calls `UpsertSession`
  then `ReconcileSession` and ignores the parameter entirely. `warningCount` is dead at every one
  of its declaration sites: `analyzer.go:31-32` (`SessionAggregator`), `jsonlogs.go:55`
  (`SessionRecorder`), `session.go:96` (`Resolver.RecordCall`). (This is *provable* dead, not
  suspected: grep `warningCount` in `internal/session` returns the signature and nothing else.)
  Deleting it is part of this change — and once the fold is deduped there is no longer a per-row
  local to thread through anyway, so the deletion falls out of C1 rather than being an extra.
- `sessions.warning_count` is re-derived from the `warnings` table inside `reconcileSessionTx`
  (`store.go:746-751`): `SELECT COUNT(*) FROM warnings WHERE event_id IN (SELECT id FROM events
  WHERE session_id = ?)`. It is not fed by the pass's return value.
- The pass is a pure function of the session's rows at the moment it runs, and it *
  upserts* — `UpsertWarnings` → `upsertWarningsTx` (`store.go:537-555`) is
  `INSERT … ON CONFLICT(event_id, kind) DO UPDATE` and **never retracts**. So the warning row set
  is a function of the *states that were passed*, unioned.

That last point is the honest caveat, and it is a **behaviour change**, so it is a decision rather
than a claim of equivalence:

> **The dropped passes are prefix-state passes. They write two kinds of row — both benign — and a
> third that `br-GI-13-05` removes rather than accepts.**
>
> 1. **Stale findings**, where the session's later state contradicts the finding's own claim or
>    precondition. If a rule fires against rows `1..N` but not against rows `1..N+k`, the current code
>    writes that warning and nothing ever removes it, because `UpsertWarnings` only ever inserts or
>    updates. A rule read as a statement about the session — `ruleCacheWriteNeverRead` is literally
>    "a cache write that is never read" — fires against a prefix only because the read has not been
>    ingested yet. A single final pass cannot write these, and should not.
>    The class is defined by **contradiction**, not by any one rule's shape: only
>    `ruleCacheWriteNeverRead` is literally monotone-decreasing, and a rule such as
>    `ruleCacheExpiredBetweenTurns` can fire, stop, and fire again. What puts a finding here is that
>    the final state does not support it.
> 2. **Superseded re-anchorings**, for a rule that anchors on the last row.
>    `ruleCachePrefixInvalidation` (`rules.go:294-308`) anchors on `rows[len-1]`, so today's per-row
>    passes write the *same true finding* once per **successful pass**, each anchored on that pass's
>    own last row — a long session can therefore carry one per row. Each is keyed to a different
>    `event_id`, so each survives the `ON CONFLICT(event_id, kind)` upsert as its own row. Those are
>    not false positives — one fact recorded once per row it was recomputed against — but they are
>    duplicates, and a single final pass anchors once. This class is an **anchor moving, not a
>    suppression**; it sits with class 1 only because both are improvements.
> 3. **True findings a later usage-less row suppresses — removed, not accepted.** Round 3 found this
>    class and `br-GI-13-05` fixes it at the rule. `ruleCachePrefixInvalidation` walks its pairs and
>    **returns `nil` on the first violating pair** (`:301`), aborting the rule for the whole session
>    instead of skipping that pair. Both disjuncts are reachable: a usage-less row has
>    `TotalPromptTokens == 0` (the store recomputes the column from the token columns at
>    `store.go:265`, so a 429 or a truncated body lands one) and aborts the walk as a zero `prev`;
>    and a usage-less row arriving *behind* a full-rewrite run aborts it as a `cur` whose write is
>    zero. So for `[P1,P2,P3,Z]` — three full rewrites, then a usage-less `Z` — the batch-end pass
>    reaches `(P3,Z)`, returns `nil`, and writes nothing, where today's pass after `P3` wrote a
>    finding anchored on `P3`.
>
>    Left alone this would have been a **permanent, user-visible loss** — and it is not a new bug: it
>    is a *pre-existing* rule fragility that today's write-early-and-never-retract behaviour masks,
>    because the finding was written before `Z` arrived and nothing removes it. C1 removes that mask,
>    so without a fix this story would ship a silent, permanent loss of a real warning. **The user
>    chose to fix the rule here rather than accept it** (D9), so class 3 is listed to record what C1
>    would otherwise have cost. `br-GI-13-05` gives the rule its sibling's shape: a **new**
>    `rowsWithUsage` predicate, mirroring the existing `rowsWithRequestBody`, filtering the row set
>    *before* the walk — `ruleCacheInvalidatedByTools` (`:324-331`) is the model for exactly this, doc
>    comment and all. The filter is deliberately not the sibling's *body* filter: this rule wants
>    usage-carrying rows including JSONL ones, which structurally never carry a request body.
>
> **The mechanism underneath classes 1 and 2: within a batch, today's warnings are written early and
> never retracted.** Never-retract is untouched by C1 — `UpsertWarnings` still only inserts or
> updates. What C1 removes is some of the *passes*: the per-row ones inside a batch. A session still
> accumulates across batches and still gets one pass per batch, so this preservation is per-batch, not
> per-session.
>
> **Stated once, with both sides named: with the rule held fixed, C1's cadence writes a subset of
> what today's cadence writes.** Every warning the single final pass produces is also produced by
> some prefix pass *under the same rule*. That is the whole guarantee. Which *classes* it drops
> depends on which rule it is held at, so they are listed below rather than folded into one sentence
> — an earlier revision folded them and was false for one of the two rules.

**What the change does to warning output.** Three comparisons, each against an explicitly named
baseline:

| Comparison | Baseline | Direction | Effect |
|---|---|---|---|
| C1's cadence alone | today's code, **today's** rule | subset | drops classes **1, 2 and 3** — including class 3, which is not benign |
| D9's rule alone | today's code, today's rule | superset | **adds** findings, for every session a usage-less row had disabled |
| **Shipped state (C1 + D9)** | today's code | mixed | drops classes **1 and 2**; class 3 is **net-neutral** — C1 drops it, D9 restores it; plus D9's additions |

"The difference is only the two benign classes" is true of the **shipped** row and of nothing else.
Attached to the first row it is false, because C1 alone drops class 3 too. That is what round 6
caught, and it is why these are three rows rather than one narrated pair.

**Class 3's neutrality holds for the shape it demonstrates — and the class-2/class-3 boundary is
stated here, once, so that class 2's text and D1 read consistently against it.** In `[P1,P2,P3,Z]`
with a usage-less `Z` **last**: C1 would drop the finding, D9 restores it, and the anchor lands on
`P3` — the same row today's pass-after-`P3` anchored on, so the shipped output **reproduces** today's.
That is a preservation, and it is class 3.

When a **real row follows** the usage-less one, the same finding is *not* lost — it **re-anchors
forward**. In `[P1,P2,P3,Z,P4]` today writes a `P3`-anchored finding and aborts at the `(P3,Z)` pair,
while the shipped walk filters `Z` and anchors on `P4`: count-neutral 1→1, **row not reproduced**.
That case is **class 2**, not class 3 — a superseded re-anchoring of a finding the final pass still
reports, which is D1's phrasing. **Class 3 is therefore exactly the case where the suppressed finding
has no re-anchored successor**, and the neutrality claim above is scoped to that case rather than
stated generally. An earlier revision stated it generally, which is what round 7 caught.

D9's own direction, so that it is checkable: `[P1,P2,Z,P3]` in `started_at` order with a usage-less
`Z`. Today the walk aborts at the `(P2,Z)` pair (`rules.go:301`) and the session gets **nothing** —
the `len(rows) < 3` guard means the earlier two-row state never fired either. Shipped,
`rowsWithUsage` filters `Z`, the walk is `[P1,P2,P3]`, and it fires on `P3`.

**For `sessions.warning_count`, which is what a user will actually notice:** the shipped story's two
effects are **opposed**, so the net direction is not predictable from either alone — some sessions
gain warnings and some lose them. **Do not describe this change as "fewer warnings".** §5's D9 and §8
say the same thing.

> **§3.1 is frozen.** This section has been the site of a finding in five of six review rounds: three
> flaws in the original argument, then damage the *editing* did — a duplicate bullet, and one
> conclusion left stale by a later change to this same section. The taxonomy itself has survived a
> dedicated attack, with round 4 walking all six rules against both write paths and folding every
> candidate into class 1, finding no fourth class. **A further finding here should therefore reopen
> C1's or D9's design, not add a fourth bullet or another qualifying clause** — which is why the
> section now ends in a named guarantee and a flat table of effects rather than more prose.
>
> The fallback if D1 is unwanted entirely: keep the per-row pass and rely on C2/C3 alone. That is
> **not** sufficient (§2 — a pass still reads `req_body` for every row), so it would have to be
> paired with the history bound §8 lists as a non-goal — which also changes findings. There is no
> option here that leaves warning output byte-identical at acceptable cost, which is why this is a
> decision rather than a tuning knob.

### 3.2 C2 — `(session_id, started_at)`

Add, in **both** homes schema.sql requires (`schema.sql:1-9`: it is the current shape, and
`store.go`'s runner owns every ALTER):

- `schema.sql`: replace `idx_events_session_id` (line 68) with
  `CREATE INDEX IF NOT EXISTS idx_events_session_started ON events(session_id, started_at);`
- `store.go`: `migrations[1]` = the same `CREATE INDEX IF NOT EXISTS`, plus
  `DROP INDEX IF EXISTS idx_events_session_id`, and `const schemaVersion = 2` (`store.go:128`).

**Dropping the single-column index is justified, not incidental**: it is a strict prefix of the new
one, and every `session_id`-filtered query in the store is served by the composite's leading column.
Verified — the four sites are `store.go:337` and `:359` (`ORDER BY started_at ASC`, served
*better*), `store.go:494` (an `EventFilter.whereClause` condition, no `ORDER BY` of its own), and
`store.go:733` / `:748` (aggregates, no `ORDER BY`). Leaving a redundant B-tree means every insert
pays for two indexes on one column — on a story whose subject is insert cost, that is the wrong
trade. `doctor_test.go:67` asserts against `store.SchemaVersion()`, not a literal, so the bump is
safe.

### 3.3 C3 — a projection shaped like what the rules read

Follow the machinery that already exists (`store.go:1250-1309`): `eventColumnNames` (47) →
`summaryOmittedColumns` (6 blobs) → `columnsMinus` → `summarySelectColumns` (41), scanned through
`eventScanVals.dest(&es, extra...)`, guarded by `TestSummaryColumnsAreTheFullSetMinusBodies` and
`TestSummaryScanMatchesSummaryColumns`. Add the same shape a third time:

- `rulesOmittedColumns = columnsMinus(summaryOmittedColumns, []string{"req_body"})` — the five blobs
  the rules never read. Deriving it rather than listing five names keeps the one home for "which
  columns are bodies".
- `rulesColumnNames` (42) / `rulesSelectColumns`, and `rulesSelectColumns + " FROM events WHERE
  session_id = ? ORDER BY started_at ASC"`.
- `scanEventForRules(rowScanner) (*Event, error)` — `dest(&es, &reqBody)`, one extra, in
  `eventColumnNames`' tail order.

**The type does not change.** `SessionEvents` keeps returning `[]*store.Event`, so
`analyze` sees the same shape and `ruleCacheInvalidatedByTools` keeps reading `ReqBody`. Only which
columns come off disk changes. §9 explains why this is not a re-run of GI-7's D1/F2.1.

**Rename to `SessionEventsForRules` and delete `SessionEvents`.** Both remaining callers
(`consumer.go:249`, `jsonlogs.go:551`) want the rules shape, so nothing needs the full-width read —
and a method named `SessionEvents` that silently returns `nil` for `RespBody` is the exact footgun
this repo's own naming convention (`ListEvents`/`ListEventsFull`,
`SessionEvents`/`SessionEventsSummary`) exists to avoid. Call sites to move: `consumer.go:44`,
`jsonlogs.go:35`, `consumer_test.go:241-242`, and the doc comment at `publishing_store.go:10-25`.

### 3.4 C4 — the profiler, as a first-class diagnostic

The profile that found this bug came from an uncommitted local patch to `internal/cli/serve.go`
that reads `os.Getenv("CLENS_PPROF_ADDR")` directly. That was the only way to profile the live
process at all: `dlv attach` suspends the proxy and a SIGBREAK stack dump exits it, and either
would kill the client whose traffic routes through this process. Making it a real, flag-gated,
config-resolved feature is the second half of this story's request — so that the *next*
investigation can profile without re-deriving the patch.

- **`internal/config/config.go`**: `PprofAddr string` on `Config` (`:30-60`), left as the zero value
  in `Default()` (`:63-77`) — disabled unless asked for, with the same "deliberately absent from
  Default()" note `PeakOffPeakDates` carries (`:56-59`); `"CLENS_PPROF_ADDR": "PprofAddr"` in
  `fieldsByEnv` (`:104-118`) — the same env name the local patch already uses, so an existing
  invocation keeps working; a `PprofAddr` case in `applyKV` (`:180`); `--pprof-addr` in
  `applyFlags` (`:226`).
- **`Validate` (`:319`)**: `if c.PprofAddr != "" { validateLoopback("PprofAddr", c.PprofAddr, false) }`
  — the literal `false` is the point. `validateLoopback` returns `nil` immediately when
  `allowRemote` is true (`:378-380`), so reusing it *with* `c.AllowRemote` would let `--allow-remote`
  open a listener whose heap profile contains whatever is in memory. Hardcoding `false` reuses the
  function and closes that door. A profile carries full request bodies; this listener is
  loopback-only and `--allow-remote` cannot widen it.
- **One home for "what counts as loopback."** Today `validateLoopback` (`config.go:373-388`) accepts
  any loopback IP via `ip.IsLoopback()`, while the existing `startPprof` patch accepts only
  `127.0.0.1`, `::1` and `localhost`. The mismatch is fail-closed — the stricter layer wins, so
  nothing unsafe ever opens — but the cost is that a legitimately-loopback `127.0.0.2` passes
  validation and is then silently refused. Two spellings of one predicate is this repo's own named
  defect class ("a figure or a rule asserted as a constant at more than one site"), so settle it by
  extracting `config.IsLoopbackHost(host string) bool` and calling it from both `validateLoopback`
  and `startPprof`. **The extracted function's contract must keep `"localhost"`**: `net.ParseIP`
  returns `nil` for it, so a naive `ip.IsLoopback()`-only predicate would accept two of the three
  spellings and regress a working `--proxy-addr localhost`. Both sites check `host == "localhost"`
  *before* parsing today (`config.go:381`, `serve.go:430`), and the shared predicate has to preserve
  that order. One definition, two callers, and the strictness question decided in one place instead
  of differing by accident.
- **`internal/cli/serve.go`**: `startPprof(cfg.PprofAddr)`, replacing the `os.Getenv` read, with its
  host check becoming `config.IsLoopbackHost`. Its refusal stays as the second layer, keeps its
  rationale comment, and now agrees with `Validate` by construction rather than by coincidence.
- **`internal/cli/doctor.go`**: one line, so the feature is discoverable instead of folklore —
  when unset, names the flag that enables it.
- **`README.md`**: a short profiling subsection. This is the runbook home, not `docs/context/` —
  the repo settled that in GI-3's D7 and GI-5's §10, and `docs/context/build-and-run.md:128-129`
  states the rule: that tree is generated, so a hand-written runbook there is clobbered by the next
  refresh.

**What makes it generic rather than a one-off.** A `net/http/pprof` listener is not a CPU-only
tool; it serves every profile a future investigation might need, and four of them cost nothing extra
to wire: `/debug/pprof/profile?seconds=30` (CPU), `/debug/pprof/heap`, `/debug/pprof/goroutine?debug=2`
(all goroutines and their stacks — the single highest-value artifact for a hang),
`/debug/pprof/trace?seconds=5`. The README should also carry the *method* that cracked this case,
because the method generalizes further than the tool: a no-DB route (`/api/health`) answering in
5 ms while every DB route takes tens of seconds is the signature of `SetMaxOpenConns(1)`
contention, and it distinguishes contention from a starved runtime or a slow disk in one curl.

`ponytail:` block and mutex profiles need `runtime.SetBlockProfileRate` /
`SetMutexProfileFraction`, which are off by default and cost something on the hot path. Deliberately
not wired. CPU, heap and goroutine — the three a hang investigation wants — need no such knob.

---

## 4. Files

| File | Change |
|---|---|
| `internal/consumer/consumer.go` | `flush` → three phases, folding once per distinct session; `runSessionRule` returns nothing; delete the `warningCount` local; `Store` iface `:44` renamed |
| `internal/consumer/analyzer.go` | `SessionAggregator.RecordCall` (`:31-32`) loses `warningCount` |
| `internal/jsonlogs/jsonlogs.go` | `insert` → returns `(sessionID, ok)`; tailer loop `:344-351` gains the per-session **fold** (not a pass — §1.1); same `warningCount` deletion; ifaces `:35`, `:55` |
| `internal/session/session.go` | `RecordCall` (`:96`) loses the dead `warningCount` parameter |
| `internal/store/store.go` | `schemaVersion = 2` (`:128`); `migrations[1]`; `SessionEvents` → `SessionEventsForRules` on the rules projection; `rulesOmittedColumns` + derived list + `scanEventForRules` |
| `internal/store/schema.sql` | line 68: single-column index → composite |
| `internal/api/publishing_store.go` | doc comment `:10-25` only — the promoted method's name and its "needs no override" rationale |
| `internal/config/config.go` | `PprofAddr` + `Default` note + `fieldsByEnv` + `applyKV` + `applyFlags` + `Validate` |
| `internal/cli/serve.go` | `startPprof(cfg.PprofAddr)` |
| `internal/cli/doctor.go` | one printed line |
| `README.md` | profiling runbook subsection |
| `internal/store/store_test.go` | `TestMigrateHealsAPartialDatabase` (`:963`/`:965`) drops `idx_events_session_id` with a bare `DROP INDEX`, so once `schema.sql` stops creating that index the test fails at its own `t.Fatalf` **before** reaching the `hasIndex` assertion at `:979`; its fixture derives from the embedded `schemaSQL`, so it tracks the edit and breaks on it. Plus the migration-shape test and §6's projection set check |
| `internal/session/session_test.go` | `:130`, `:145` call `RecordCall(ctx, …, ev, 0)` — the dropped parameter |
| `internal/consumer/consumer_test.go` | `failingStore.SessionEvents` (`:241-242`) follows the rename; `SetSessionRule` (`:459`) is the pass-wiring fixture, and C1 needs a test that a multi-event one-session batch folds and passes once, not once per event |
| `internal/jsonlogs/*_test.go` | the fold's new home in the tailer loop, and that a file of N lines for one session folds once |
| `internal/config/config_test.go`, `internal/cli/*_test.go` | C4: `PprofAddr` precedence across flag/env/file; the loopback rejection holding **with `AllowRemote` true**; the shared `IsLoopbackHost` predicate, including that it still accepts `"localhost"`; `doctor`'s line |

No new files. No new dependencies.

---

## 5. Decision log

- **D1 — every per-session derivation runs once per distinct session per batch, accepting a subset
  of warning rows.** Justified in §3.1. Once `br-GI-13-05` lands, the dropped rows are prefix-state
  findings of exactly **two** kinds, both benign: stale findings the session's later state
  contradicts, and superseded re-anchorings of a finding the final pass still reports. Chosen over
  byte-identical output because byte-identical output is not achievable at acceptable cost without
  the history bound §8 lists as a non-goal. **This is the decision to challenge at the checkpoint**,
  and its cost is now bounded to stale and duplicated warnings.
- **D9 — the third class is fixed at the rule, in this story, rather than accepted.** Round 3 showed
  C1 would unmask a *pre-existing* fragility in `ruleCachePrefixInvalidation` — it aborts the whole
  rule on a usage-less pair (`rules.go:301`) — and permanently lose a true finding. Accepting that
  would have shipped a silent regression, so the rule gets its sibling's shape via a **new**
  `rowsWithUsage` predicate mirroring the existing `rowsWithRequestBody`, as `br-GI-13-05`. This is
  the story's **one** rules-semantics change, and the cost is stated rather than hidden: the filter
  also restores the rule for every existing session a usage-less row has already disabled, so **it
  changes findings for existing sessions**. That is the price of not shipping the loss; §8 records
  it, and §9 amends its "analyze untouched" promise accordingly.
- **D2 — the nine columns the rules read are kept whole; `req_body` among them.** A projection that
  dropped `req_body` would break `ruleCacheInvalidatedByTools`, and one that read it *sometimes*
  would put a rule's column needs in two places. §3.3's value is the other five blobs.
- **D3 — `SessionEvents` is deleted, not narrowed in place.** A method whose name promises full rows
  and returns `nil` blobs is a trap for the next caller; the repo already resolves this by naming
  the shape (§3.3).
- **D4 — the redundant single-column index is dropped in the same migration.** It is a strict
  prefix; leaving it charges every insert for an index nothing reads (§3.2).
- **D5 — `PprofAddr`'s loopback check hardcodes `allowRemote = false`.** `--allow-remote` exists to
  let a user bind a dashboard they understand; it must not also open a memory dump (§3.4).
- **D6 — the profiler's runbook home is `README.md`.** Settled precedent: `docs/context/` is
  generated (GI-3 D7, GI-5 §10, `build-and-run.md:128`).
- **D7 — no new abstraction for the profiler.** It is a config field, a flag, and 12 lines that
  already exist in the working tree. No `internal/diagnostics` package, no profile-capture helper.
- **D8 — the deduped fold passes the batch's first *inserted* row for each session, not the last, and
  not the smallest `started_at`.**
  Forced, not chosen: `prefix_hash` is INSERT-only (`store.go:680-689`), so it is first-writer-wins
  and the first fold row is what reproduces today's value. `first_seen`/`last_seen` self-correct and
  every other column is re-derived from the tables, so the fold row is an identity *except* for the
  hash — which is what makes this the only row choice that matters (§3.1). Round 2 caught this as
  MAJOR after v2 asserted the opposite.

---

## 6. Test strategy

The load-bearing tests, each of which fails if the thing it names stops being true:

1. **`EXPLAIN QUERY PLAN` asserts no temp B-tree** for the session-scoped SELECT. This is the
   runnable check for the whole story: SQLite does not spill a sort it does not perform, and no
   other test would notice the index being dropped or the query drifting. Assert the property
   (no `USE TEMP B-TREE FOR ORDER BY`), not the chosen index name. **It must not be a negative-only
   assertion** — but be precise about what a positive half can buy. Once `(session_id, started_at)`
   exists, `WHERE session_id = ?` *without* any `ORDER BY` already returns `started_at` order off the
   index, so both behavioural halves pass if the `ORDER BY` drifts away: the index masks the loss.
   So pair them with a **textual** assertion — the query string still contains its
   `ORDER BY started_at`, read the way `TestSummaryColumnsAreTheFullSetMinusBodies`
   (`store_test.go:699-720`) reads the query strings — and keep a fixture whose rowid order and
   `started_at` order disagree, asserted to come back in `started_at` order, to catch a direction
   flip or a hand-built reordering.
2. **`SessionEventsForRules` returns the same rows, in the same order, as the full-width read** —
   compared against `ListEventsFull`-shaped output or a hand-built expectation on a fixture, so the
   projection cannot silently drop a column the rules use.
3. **`TestRulesProjectionNamesEveryColumnTheRulesRead`** — two assertions, because the mirrored
   derivation on its own is not a guard. It is tempting to check only that
   `rulesSelectColumns == eventColumnNames − rulesOmittedColumns`, mirroring
   `TestSummaryColumnsAreTheFullSetMinusBodies` (`store_test.go:699-720`) — but `rulesOmittedColumns`
   *derives* from `summaryOmittedColumns`, whose own membership nothing pins, so one wrong entry
   there silently drops a column the rules read while the mirrored check still passes. The test must
   therefore also **name the nine columns the six rules need** — `id`, `started_at`, `ended_at`,
   `total_prompt_tokens`, `cache_write_5m_tokens`, `cache_write_1h_tokens`, `cache_read_tokens`,
   `prefix_hash`, `req_body` — and assert `rulesSelectColumns` lists every one of them.
   Deliberately *not* a per-rule fixture test: a rule reading an omitted column receives a **zero
   value, not an error**, so a fixture guard fails only when the fixture is also updated — which is
   exactly when it has stopped doing its job. Pair it with `TestRulesScanMatchesRulesColumns` for the
   42-destination count, mirroring `TestSummaryScanMatchesSummaryColumns`.
4. **The dedupe's semantics, pinned deliberately**: a fixture whose batch/file contains a cache
   write and then a read of it produces **no** warning for that row after one pass — asserting D1's
   chosen behaviour rather than leaving it implicit. Plus its converse: a session whose write is
   genuinely never read still warns.
5. **Class 3 is fixed, pinned at both levels.** At the rule: `ruleCachePrefixInvalidation` over
   `[P1,P2,P3,Z]` — three full rewrites, then a usage-less row with zero `TotalPromptTokens`, as a 429
   or a truncated body produces — returns the `cache_prefix_invalidation` finding, anchored on `P3`,
   the last **usage-carrying** row. That anchor is the point: it is the same row today's
   pass-after-`P3` anchors on, so `br-GI-13-05` preserves today's value rather than substituting a new
   one. At the pipe: a batch of those four rows writes that warning through the single batch-end pass.
   Together they fail if `rowsWithUsage` is dropped, if the walk reverts to aborting on a usage-less
   pair, or if the anchor lands on `Z`. A second rule-level case asserts the rule still **declines** a
   session where a real turn reads from cache instead of rewriting — the `write < 0.5*prevTotal`
   disjunct is the rule's actual condition and must survive the filter. **That fixture must carry at
   least three usage-carrying rows**, or the rule returns `nil` from its `len(rows) < 3` guard
   (`rules.go:295`) before reaching the disjunct, and the case passes for a reason it does not claim —
   the same "not a negative-only assertion" bar test 1 sets.
6. **The fold carries the right row.** A fixture whose batch holds two rows for one session with
   **different** `PrefixHash` values — reachable, since `prefixHash` covers the first `min(n,2)`
   messages, so turn 1 and turn 2 differ — asserts the session row ends up with the **first
   inserted** row's hash. This is F2.1's missing assertion, and it is the one test that fails if
   "first" is read as `started_at` order or as "last".
7. **`sessions.warning_count` still equals the `warnings` rows for the session** after a multi-event
   batch — the invariant that survives deleting the dead parameter, asserted through
   `reconcileSessionTx`'s own derivation so it cannot pass by both sides being wrong together.
8. **Config**: `PprofAddr` resolves from flag, env and file at the documented precedence; `Validate`
   rejects a non-loopback `PprofAddr` **even when `AllowRemote` is true** (D5's test — the one that
   would catch the `validateLoopback` reuse being "tidied" to pass `c.AllowRemote`); empty
   `PprofAddr` validates clean, since it is the default.
9. **Migration**: a store opened at `user_version = 1` with rows present reaches version 2, holds the
   composite index, has no `idx_events_session_id`, and still returns its rows — plus a fresh DB at
   the same final shape, so the two homes for the DDL cannot drift.
10. **Regression on the hot path**: `go test ./internal/proxy/` unchanged — the TTFB gate. This story
    touches nothing in the hot path, and the gate says so.

## 7. Risks

- **R1 — D1's subset behaviour is the whole semantic risk**, and it is user-visible. Mitigation:
  test 4 pins it in both directions; the checkpoint surfaces it. Fallback chain if rejected: C2+C3
  alone are insufficient (§2) → the history bound is the next lever → §8's non-goal becomes this
  story's scope.
- **R2 — the index could be chosen but still not remove the sort.** Mitigated by test 1 asserting
  the *property*, which is checked against the real planner, not against my reading of it.
- **R3 — the migration runs against a live-looking DB.** It is `CREATE INDEX` + `DROP INDEX` on a
  schema the repo's own tests build at version 1; the runner is the established `PRAGMA
  user_version` path (ADR 007) with the version bump inside the same transaction. Test 7 covers it.
  Note the store is a *single-user local* file, and the composite index build is a one-time cost on
  first open after the upgrade.
- **R4 — `SchemaVersion()` is exported and `doctor` prints it** (`doctor.go:221-226`). A binary that
  is newer than the store reports the gap rather than failing; that is existing behaviour and test 7
  keeps it true.
- **R5 — the perf claim itself is unmeasured after the fix.** The profile proves the *current* cost
  and the mechanism; it cannot prove the new one without a re-profile. The honest check is C4:
  after the change, take the same 30-second profile against the same store with traffic, and assert
  `_vdbePmaWriteBlob` / `_vdbeIncrSwap` are gone. That is a manual verification step, run once, and
  worth doing — it is the story's own instrument applied to the story's own fix.

## 8. Non-goals

- **Bounding the history the rules scan** — a `LIMIT`, a window, a rule-specific row set *to bound
  history*, or an incremental fold. This is the decision GI-9 deferred (`GI-9-…:1654-1668`) and it
  stays deferred: a `LIMIT` changes which findings are produced, which is a rules-semantics question,
  not a performance one. **The ceiling this leaves is real and should be stated in the code**: after
  this story a single pass still costs `rows × req_body bytes`, so a long enough session still makes
  one pass expensive. C2's index is what makes any future bound cheap, and naming that is the point.
  The qualifier is load-bearing: D9 **is** a rule-specific row set (`rowsWithUsage`), so the bare
  phrase would read as banning the one rules change this story does make. What is out of scope is
  narrowing the row set *to bound history* — dropping rows that carry data, rather than rows that
  carry none.
- **`runtime.SetBlockProfileRate` / `SetMutexProfileFraction`** (§3.4).
- **Widening the rules-semantics scope past D9.** `br-GI-13-05` changes exactly one rule's handling of
  usage-less rows, because not changing it would ship the permanent loss class 3 describes. That is
  the whole of this story's licence to touch `internal/analyze`: no other rule is modified, none is
  added or removed, and no threshold moves. **The accepted consequence is stated rather than hidden**
  — the new `rowsWithUsage` predicate changes findings for existing sessions that a usage-less row
  has already disabled, so it is a dashboard behaviour change, not only a bug fix.
- **Anything in `internal/proxy`.** The hot path is not touched and must stay not touched.
- **`sessions.warning_count`'s derivation.** It already counts from the `warnings` table; deleting
  the dead parameter does not move it.

## 9. Relationship to GI-7's D1/F2.1

`docs/planning/GI-7-header-and-body-visibility.md:192-203` rejected a draft that put `SessionEvents`
on the summary, calling it "a blocker, not a style choice" — retyping `SessionEvents` to
`[]*EventSummary` would drop `ReqBody`, and `ruleCacheInvalidatedByTools` reads `prev.ReqBody` /
`cur.ReqBody`, so "the build fails at the composition root before any rule runs."

**This plan does not reopen that.** GI-7 rejected changing the *type*; this keeps
`[]*store.Event`, keeps `ReqBody` populated, and changes only *which columns the SELECT lists*. The
distinction is the whole design: GI-7's blocker was a rule losing its input, and **no rule loses an
input here** — the projection keeps all nine columns the six rules read. (The story does modify one
rule, but for an unrelated reason: D9 / `br-GI-13-05` hardens `ruleCachePrefixInvalidation` against
usage-less rows, which is about which *rows* the rule walks, not which *columns* it can see. That is
the only `internal/analyze` change, and §8 bounds it.)

The name does change (D3, `SessionEventsForRules`). That is a rename in service of the same
principle GI-7 applied — the shape is in the name — and it is called out here so the round-1
reviewer can check it rather than infer it.

**One deliberate divergence, recorded rather than glossed.** GI-7 argued its case twice: that the
retype would break a rule (`:192-203`), and that the *compiler* is the guard — retyping "stops
compiling at the composition root", which is what makes the invariant durable (`:205-211`). This plan
keeps the type, so it forgoes that second half. The guard here is a **name**, not a type: a future
caller wanting full rows gets a compile error on the missing `SessionEvents`, which is nearly as
good — but a caller who reaches for `SessionEventsForRules` *expecting* `RespBody` gets a silent zero
value instead of an error. §6's set check is what backstops that, and this is the one place the
design is weaker than GI-7's precedent rather than merely different from it.

## 10. Proposed bead decomposition

Phase 3 owns the final cut. This is the shape the dependencies suggest:

| # | Bead | Depends on | Why this unit |
|---|---|---|---|
| 01 | Every per-session derivation runs once per distinct session; delete the dead `warningCount` threading | — | One build-atomic change: the `RecordCall` signature and both its call sites must land together or the tree does not compile. Covers the fold as well as the pass (§1.1), which is what makes the tailer's half worth touching |
| 02 | `(session_id, started_at)`, drop the redundant index, `schemaVersion = 2` | — | One migration, one DDL concept, both homes |
| 03 | `SessionEventsForRules`: the rules-shaped projection, `SessionEvents` deleted | 01 | 01 is what moves both callers onto the one method, so the rename has exactly two sites to touch |
| 04 | `--pprof-addr` as a config-resolved, loopback-locked profiler + README runbook | — | Independent of 01-03; the local patch in the tree is 80% of it |
| 05 | `ruleCachePrefixInvalidation` stops aborting on a usage-less row: a **new** `rowsWithUsage` predicate mirroring `rowsWithRequestBody`, plus the rule-level and pipe-level cases in §6 test 5 | 01 | D9. Self-contained in `internal/analyze`, but depends on 01 because its pipe-level half asserts the batch-end pass writes the finding. Must land **before** 06 |
| 06 | Re-profile against the same store and record the result (R5) | 01, 02, 03, 04, 05 | The story's own instrument, applied to the story's own fix |

01, 02 and 04 are mutually independent and can land in any order. 03 follows 01; 05 follows 01; 06 is
the only one that must be last.
