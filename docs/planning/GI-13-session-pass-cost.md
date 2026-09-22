# GI-13 — the session-scoped rule pass, and a profiler to find the next one

**Status**: v1, draft. Not yet cross-reviewed.
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

The pass is not run once. It is run **once per written row**, at two independent call sites:

| Site | Loop | Runs the pass |
|---|---|---|
| `consumer.flush` (`consumer.go:205-238`) | one iteration per event in the batch | `consumer.go:221-223`, inside the loop. `defaultBatchSize = 50` (`consumer.go:30`) |
| `jsonlogs.insert` (`jsonlogs.go:498-533`) | called once per distinct assistant line of one file | `jsonlogs.go:523-525`. The tailer walks every distinct line of a file in one loop (`jsonlogs.go:344-351`); ~1,259 in the burst that was observed |

Both goroutines share the one connection. So a 50-event flush from a single session issues 50
whole-session re-reads, each of which sorts and spills; the JSONL tailer issues one per transcript
line. The cost is `passes × (rows × body bytes)`, and both factors grow on their own.

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

### 3.1 C1 — one pass per distinct session per batch, not one per row

Restructure both call sites into three phases: **insert every row → run the pass once per distinct
session → fold each row into its session.** The existing comment at `consumer.go:225-227`
("Run last, after the session-scoped pass, so warning_count's re-derivation counts findings that
pass attached to earlier rows in this same session too") is preserved by that ordering, not
violated by it.

`jsonlogs.insert` (`jsonlogs.go:498`) currently does all three itself, so it loses its last two
blocks and returns `(sessionID string, ok bool)`; the tailer's loop (`jsonlogs.go:344-351`) takes
them over. The pass is then run once per distinct session *in that file*. A session spanning two
files still gets one pass per file — still a collapse from one per row.

**Why this is safe, and the one place it is not free.**

- The pass's only store-visible effect is the warnings it upserts (`consumer.go:262-268`,
  `jsonlogs.go:564-570`). Its return value is folded into a `warningCount` argument that
  `session.Resolver.RecordCall` (`session.go:96-111`) **never reads** — it calls `UpsertSession`
  then `ReconcileSession` and ignores the parameter entirely. `warningCount` is dead at every one
  of its declaration sites: `consumer.go:31-32`, `jsonlogs.go:55`, `session.go:96`. (This is
  *provable* dead, not suspected: grep `warningCount` in `internal/session` returns the signature
  and nothing else.) Deleting it is part of this change.
- `sessions.warning_count` is re-derived from the `warnings` table inside `reconcileSessionTx`
  (`store.go:746-751`): `SELECT COUNT(*) FROM warnings WHERE event_id IN (SELECT id FROM events
  WHERE session_id = ?)`. It is not fed by the pass's return value.
- The pass is a pure function of the session's rows at the moment it runs, and it *
  upserts* — `UpsertWarnings` → `upsertWarningsTx` (`store.go:537-555`) is
  `INSERT … ON CONFLICT(event_id, kind) DO UPDATE` and **never retracts**. So the warning row set
  is a function of the *states that were passed*, unioned.

That last point is the honest caveat, and it is a **behaviour change**, so it is a decision rather
than a claim of equivalence:

> **The dropped passes are prefix-state passes.** If a rule fires against rows `1..N` but not
> against rows `1..N+k`, the current code writes that warning and nothing ever removes it. A rule
> read as a statement about the session — `ruleCacheWriteNeverRead` is literally "a cache write
> that is never read" — fires against a prefix only because the read has not been ingested yet, so
> the row it writes is a **stale false positive**. Running one final pass per batch cannot write
> those; it writes strictly the findings the session's state at that moment supports.

So: the deduped warning set is a **subset** of the current one, and the difference is exactly the
within-window transient false positives. I consider that an improvement and am choosing it — but it
is user-visible (a session's `warning_count` can differ), so it is D1 below and the plan should be
read as making a semantic choice, not as a pure performance refactor. If the reviewer or the user
prefers byte-identical warning output, the fallback is to keep the per-row pass and rely on C2/C3
alone, which is *not* sufficient (§2) and would have to be paired with the history bound this plan
lists as a non-goal.

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
- **`internal/cli/serve.go`**: `startPprof(cfg.PprofAddr)`, replacing the `os.Getenv` read. Its own
  loopback refusal stays as the second layer, and it keeps its rationale comment.
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
| `internal/consumer/consumer.go` | `flush` → three phases; `runSessionRule` returns nothing; delete the `warningCount` local; `Store` iface `:44` renamed |
| `internal/consumer/analyzer.go` | `SessionAggregator.RecordCall` (`:31-32`) loses `warningCount` |
| `internal/jsonlogs/jsonlogs.go` | `insert` → returns `(sessionID, ok)`; tailer loop `:344-351` gains the pass + fold; same `warningCount` deletion; ifaces `:35`, `:55` |
| `internal/session/session.go` | `RecordCall` (`:96`) loses the dead `warningCount` parameter |
| `internal/store/store.go` | `schemaVersion = 2` (`:128`); `migrations[1]`; `SessionEvents` → `SessionEventsForRules` on the rules projection; `rulesOmittedColumns` + derived list + `scanEventForRules` |
| `internal/store/schema.sql` | line 68: single-column index → composite |
| `internal/api/publishing_store.go` | doc comment `:10-25` only — the promoted method's name and its "needs no override" rationale |
| `internal/config/config.go` | `PprofAddr` + `Default` note + `fieldsByEnv` + `applyKV` + `applyFlags` + `Validate` |
| `internal/cli/serve.go` | `startPprof(cfg.PprofAddr)` |
| `internal/cli/doctor.go` | one printed line |
| `README.md` | profiling runbook subsection |
| tests | see §6 |

No new files. No new dependencies.

---

## 5. Decision log

- **D1 — the pass runs once per distinct session per batch, accepting a subset of warning rows.**
  Justified in §3.1. The dropped rows are prefix-state findings that the session's later state
  contradicts and that nothing ever retracted. Chosen over byte-identical output because
  byte-identical output is not achievable at acceptable cost without the history bound that §8
  lists as a non-goal. **This is the decision to challenge at the checkpoint.**
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

---

## 6. Test strategy

The load-bearing tests, each of which fails if the thing it names stops being true:

1. **`EXPLAIN QUERY PLAN` asserts no temp B-tree** for the session-scoped SELECT. This is the
   runnable check for the whole story: SQLite does not spill a sort it does not perform, and no
   other test would notice the index being dropped or the query drifting. Assert the property
   (no `USE TEMP B-TREE FOR ORDER BY`), not the chosen index name.
2. **`SessionEventsForRules` returns the same rows, in the same order, as the full-width read** —
   compared against `ListEventsFull`-shaped output or a hand-built expectation on a fixture, so the
   projection cannot silently drop a column the rules use.
3. **`TestRulesProjectionCoversEveryColumnTheRulesRead`** — the guard that makes (2) durable. The
   rules' column needs are a fact about `analyze`; this test pins the projection against a
   representative fixture per rule and fails if a rule starts reading a column the projection omits.
   The existing `TestSummaryColumnsAreTheFullSetMinusBodies` /
   `TestSummaryScanMatchesSummaryColumns` pair is the precedent for this shape.
4. **The dedupe's semantics, pinned deliberately**: a fixture whose batch/file contains a cache
   write and then a read of it produces **no** warning for that row after one pass — asserting D1's
   chosen behaviour rather than leaving it implicit. Plus its converse: a session whose write is
   genuinely never read still warns.
5. **`sessions.warning_count` still equals the `warnings` rows for the session** after a multi-event
   batch — the invariant that survives deleting the dead parameter, asserted through
   `reconcileSessionTx`'s own derivation so it cannot pass by both sides being wrong together.
6. **Config**: `PprofAddr` resolves from flag, env and file at the documented precedence; `Validate`
   rejects a non-loopback `PprofAddr` **even when `AllowRemote` is true** (D5's test — the one that
   would catch the `validateLoopback` reuse being "tidied" to pass `c.AllowRemote`); empty
   `PprofAddr` validates clean, since it is the default.
7. **Migration**: a store opened at `user_version = 1` with rows present reaches version 2, holds the
   composite index, has no `idx_events_session_id`, and still returns its rows — plus a fresh DB at
   the same final shape, so the two homes for the DDL cannot drift.
8. **Regression on the hot path**: `go test ./internal/proxy/` unchanged — the TTFB gate. This story
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

- **Bounding the history the rules scan** — a `LIMIT`, a window, a rule-specific row set, or an
  incremental fold. This is the decision GI-9 deferred (`GI-9-…:1654-1668`) and it stays deferred:
  a `LIMIT` changes which findings are produced, which is a rules-semantics question, not a
  performance one. **The ceiling this leaves is real and should be stated in the code**: after this
  story a single pass still costs `rows × req_body bytes`, so a long enough session still makes one
  pass expensive. C2's index is what makes any future bound cheap, and naming that is the point.
- **`runtime.SetBlockProfileRate` / `SetMutexProfileFraction`** (§3.4).
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
distinction is the whole design: GI-7's blocker was a rule losing its input, and no rule loses an
input here. `internal/analyze` is not modified by this story at all.

The name does change (D3, `SessionEventsForRules`). That is a rename in service of the same
principle GI-7 applied — the shape is in the name — and it is called out here so the round-1
reviewer can check it rather than infer it.

## 10. Proposed bead decomposition

Phase 3 owns the final cut. This is the shape the dependencies suggest:

| # | Bead | Depends on | Why this unit |
|---|---|---|---|
| 01 | The pass runs once per distinct session; delete the dead `warningCount` threading | — | One build-atomic change: the `RecordCall` signature and both its call sites must land together or the tree does not compile |
| 02 | `(session_id, started_at)`, drop the redundant index, `schemaVersion = 2` | — | One migration, one DDL concept, both homes |
| 03 | `SessionEventsForRules`: the rules-shaped projection, `SessionEvents` deleted | 01 | 01 is what moves both callers onto the one method, so the rename has exactly two sites to touch |
| 04 | `--pprof-addr` as a config-resolved, loopback-locked profiler + README runbook | — | Independent of 01-03; the local patch in the tree is 80% of it |
| 05 | Re-profile against the same store and record the result (R5) | 01, 02, 03, 04 | The story's own instrument, applied to the story's own fix |

01, 02 and 04 are mutually independent and can land in any order. 05 is the only one that must be
last.
