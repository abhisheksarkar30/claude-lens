# Bead br-GI-13-07: stop the session rules re-reading every request body

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §3.3 (C3), §5 D2, §7 R5. **This bead is
a post-convergence addition**: it is the measurement §7 R5 asked for, taken before br-GI-13-06 was run,
and it found a term the converged plan does not cover.

- **Bead ID**: br-GI-13-07
- **Priority**: P0 (critical)
- **Original Estimate**: 6h
- **Dependencies**: br-GI-13-03 (it edits the projection that bead created), br-GI-13-05 (adjacent rule)
- **Blocks**: br-GI-13-06 (the re-profile must measure this fix, not just C1/C2/C3)

> **Ordering note for br-GI-13-06.** `br-GI-13-06` predates this post-convergence set and will not be
> updated: its own `Dependencies` line names only `br-GI-13-01…05` and `Blocks: None`. Read that list
> as needing to gain `br-GI-13-07` and `br-GI-13-08`, and run the re-profile **after both** (07 first,
> then 08) — otherwise it measures only C1/C2/C3 and the two fixes this bead and 08 add go unmeasured.

> **The story is not finished without this.** C1, C2 and C3 all landed and all did what they claimed —
> the index is used, the spill is gone, the projection is in place. The dashboard still hangs, because
> the dominant cost was never the sort and never the column count: it is that the pass reads the whole
> session's `req_body` on every flush.

## Description

### The measurement

Taken against the live store (`D:/clens/lens.db`) on session `ef875ca8-74ec-4ff1-959c-a2f1ef1619d0`
(6,097 rows, 790 MB of request bodies), through `modernc.org/sqlite` — the same driver the process
uses — with the queries the store actually issues:

| read | cold | warm (same process, second pass) |
|---|---|---|
| `SessionEventsForRules` minus `req_body` | 0.58 s | **0.037 s** |
| `SessionEventsForRules` with `req_body` | 10.1 s | **1.65 s** |

`req_body` is 94% of the pass in both regimes, and the pass is ~40× cheaper without it. The profile of
the running fixed build agrees: `consumer.flush` 91.4% → `runSessionRule` 88.4% →
`store.SessionEventsForRules` 88.4% → **`_vdbeColumnFromOverflow` 70.6%**, with the pre-fix sort-spill
markers (`_vdbePmaWriteBlob`, `_vdbeIncrSwap`, `_vdbePmaReadBlob`) **absent**. The remaining cost is
payload reads, not sorting.

Because the store has one connection (`SetMaxOpenConns(1)`), the dashboard queues behind that: live
`/api/stats` measured 15.8 s / 5.5 s / 1.9 s while `consumer_flushes` never advanced.

### The whole dependency on `req_body`, in the session rules

Exactly two uses **in the session-rule region** of `internal/analyze/rules.go`:

1. `ruleCacheInvalidatedByTools` (`:336`) compares **consecutive** rows' tool names —
   `toolNamesEqual(prev.ReqBody, cur.ReqBody)` (`:348`) — which calls
   `parse.ExtractMeta(body, …).ToolNames`, a **full JSON unmarshal of the whole body, twice per pair**.
2. `rowsWithRequestBody` (`:365`) filters on `len(r.ReqBody) > 0` — it exists so an interleaved JSONL
   row is skipped rather than treated as an adjacent body-less pair.

Nothing else in the session rules reads `ReqBody`. Both uses are satisfiable from a value far smaller
than the body.

**This is not the whole file's dependency, and `req_body` does not leave `internal/analyze`.** The
per-event rule `ruleCachePrefixBelowMinimum` (`:92`) also reads `ev.ReqBody` — at `:102`
(`len(ev.ReqBody) == 0`) and `:113` (`markedPrefixTokens(ev.ReqBody)`). That rule runs on the **write
path**, against the event the consumer already holds, so it is out of scope here and needs no change:
only the **session** rules stop reading the body.

### The fix: record the tool names once, at write time — for free

**The consumer already computes them.** `processCall` (`internal/consumer/consumer.go:315`) calls
`parse.ExtractMeta(call.ReqBody, call.ReqHeaders)` and holds `meta`; `meta.ToolNames` is already
populated. The derivation costs no extra parse.

- **One new column, `req_tool_names TEXT`.** Contract, stated once in `schema.sql` and once in
  `store.go`:
  - `NULL` — the row carries **no request body** (a JSONL-sourced row, structurally);
  - otherwise — the request's tool names **in body order**, JSON-encoded (`[]` when the body declares
    none).
  One column carries both facts because they are exact complements, not an overload: the store writes
  `NULL` iff `len(ev.ReqBody) == 0`, and writes the JSON otherwise. An empty array and "no body" are
  distinguishable (`'[]'` vs `NULL`), which a single `''`-defaulted column could not do.
- **Store — the one *write* site is `internal/store/merge.go`, not `InsertEvent`/`InsertEvents`.**
  `InsertEvent` (`store.go:273`) and `InsertEvents` (`store.go:301`) write no column at all — both
  delegate to `insertOrMerge` → `insertEventTx`/`updateEventTx`, and the shared column/value lists live
  in `merge.go`: `eventWriteColumns` (`merge.go:14`) and `eventWriteArgs` (`merge.go:28`). Append
  `req_tool_names` to `eventWriteColumns` and its value (encoded `ev.ToolNames` under the rule above)
  to `eventWriteArgs`. The two lists are shared by **insert *and* update** (`insertEventTx`
  `merge.go:59`, `updateEventTx` `merge.go:69`), so this one edit covers both — and the reason is that
  the lists are shared, not "because both insert".
- **Store — the cross-source merge must *derive* the column, not leave it.** A merge can supply a
  request body the survivor lacked: `mergeEvents` backfills `merged.ReqBody = incoming.ReqBody` when
  `len(existing.ReqBody) == 0` (`merge.go:340-347`), and `merged := *existing` (`merge.go:165`) means
  the new column would otherwise stay `existing`'s (NULL). Under the contract the column is a function
  of whichever body the merged row ends up with, so when the merge backfills `ReqBody` from the
  incoming side it must also set `merged.ToolNames = incoming.ToolNames`. Leaving it to
  `existing` alone would produce a row with a body and a NULL `req_tool_names` — the exact "NULL iff no
  body" violation, on the rekey-collision ordering the bead's own backfill section exists to protect.
  `rowsWithRequestBody`→`HasReqBody` would then skip a body-bearing row and the tools rule decline.
  Shape it like the `PrefixHash`/`ReplayOf` carry the code already documents at `merge.go:357-373`.
- **Consumer** — set `ev.ToolNames = encodeToolNames(meta.ToolNames)` where the event is built
  (`buildEvent`, called at `consumer.go:329`). `meta.ToolNames` is `[]string` (`parse/types.go:78`);
  `encodeToolNames` is the **shared encoder** (put it in `internal/store` beside the column contract —
  it takes `[]string` and returns the stored string, so it imports no `parse`) that the backfill calls
  too, so the two writers cannot disagree. That one encoder is what makes the string compare in
  `ruleCacheInvalidatedByTools` valid. No new parse.
- **`internal/jsonlogs`** needs **no change and must not get one**: it writes rows with no request
  body, so `req_body` is empty and the column is `NULL` by the contract. It also does not go through
  the consumer at all — it has its own store interface and writes directly.
- **`internal/store/store.go` — where the column enters the projection machinery: `eventColumnNames`.**
  Append `req_tool_names` to `eventColumnNames` (`:1276`, 47 entries → **48**), placed after `status`
  and before the blob tail (`req_headers … transcript_role`) so `scanEvent`'s extras stay the tail.
  That one append is the whole entry point: every SELECT is *derived* from `eventColumnNames`, so both
  `summaryColumnNames` (`:1308`) and `rulesColumnNames` (`:1310`) gain it automatically, and the scan
  destinations — `eventScanVals.dest` (`:1349`), used by `scanEventSummary` *and* `scanEventForRules` —
  gain it in the matching position, so the SELECT and the Scan cannot drift. **The new fields live on
  `EventSummary`, not `Event`** — `apply`'s receiver is `*EventSummary` (`store.go:1367`) and `Event`
  embeds it — and are `ToolNames string` (the JSON encoding the column contract describes, so
  `prev.ToolNames == cur.ToolNames` is a valid string compare) and `HasReqBody bool`. `dest` carries
  the column as a `sql.NullString` (an `eventScanVals` field, the `:1332` block) and `apply` (`:1367`)
  sets `ev.ToolNames`/`ev.HasReqBody = ns.Valid`; `scanEventForRules` therefore no longer appends its
  own `&reqBody` extra — it scans the rules projection with the shared `dest(&es)`, and the column it
  reads is the same one `rulesColumnNames` names.
- **`internal/store/store.go` — the omitted set.** `rulesOmittedColumns` (`:1304`) becomes
  `columnsMinus(summaryOmittedColumns, nil)`, i.e. **`req_body` rejoins the omitted set**. Delete the
  `rulesOmittedColumns` exception and its comment; the value is now identical to `summaryOmittedColumns`
  (the six blobs including `req_body`), so `rulesColumnNames`/`rulesSelectColumns` are *retained* but
  equal the summary set — **do not** delete the vars, `br-GI-13-03`'s tests assert against them. Both
  projections are therefore **42** columns (48 minus the 6 blobs). The summary side **deliberately**
  gains `req_tool_names` in the same stroke: it is a small text column, not a blob, so
  `summaryOmittedColumns`'s meaning ("the blobs the list never reads") is unchanged and carrying the
  column on the list path costs nothing.
- **`ruleCacheInvalidatedByTools`** compares the stored value directly:
  `if prev.ToolNames == cur.ToolNames { continue }` — after `rowsWithRequestBody` becomes
  `rowsWithRequestBody` filtered on `HasReqBody`. The rule's condition, threshold and anchor row are
  otherwise **unchanged**.

### The historical rows — a backfill, not a silent loss

A row written before this migration has no `req_tool_names`, so under the contract it reads as
"no request body" — which is wrong for real history and would silently decline the tools rule on
every pre-existing session. **That is the same permanent-loss shape D9 exists to prevent**, so the
backfill is part of this bead, not a follow-up.

- **`clens backfill-tool-names`** — a maintenance subcommand in `internal/cli`, following the
  established one-shot pattern of `rekey` / `reprice` / `reflag` (`cmd/clens/main.go:38-42`). It
  pages over rows where `req_body IS NOT NULL AND req_tool_names IS NULL`, parses each with
  `parse.ExtractMeta`, and writes the encoded value. **The SQL lives in `internal/store`, not the
  CLI**: the CLI holds an opaque `*store.Store` (`store.go:52`, `db` is unexported), so it
  cannot run the paging query itself — the store exposes the **paged read**
  (`req_body IS NOT NULL AND req_tool_names IS NULL`) and the **single-column write**, and the
  outstanding **count** `doctor` reports. This mirrors `rekey`/`reprice`, which split their store work
  out the same way (`internal/cli/reflag.go:20-23`: "The one statement and the three counts live in
  internal/store"). This adds one new import edge, `internal/cli` → `internal/parse` (fine —
  `internal/cli` does not import `parse` today). `internal/store` already imports `internal/parse`
  (`store.go:23`) and interprets stored bodies with it (`:1664` `parse.ExtractUsage`, `:1813`
  `parse.ExtractMeta`) — so this split is not about an import edge. The constraint that *is*
  load-bearing is that the **encoding** of tool names has exactly one definition, `parse.ExtractMeta`'s,
  which is why the backfill parses in Go and writes the already-encoded `ToolNames string` (the CLI
  parses and calls `encodeToolNames`) rather than re-deriving the array with `json_extract` in SQL — see
  the migration bullet below.
- **The migration is SQL-only and cheap**: `migrations[2]` =
  `ALTER TABLE events ADD COLUMN req_tool_names TEXT;` and nothing else — the slice currently holds
  indices 0–1 (`store.go:160`), so this is the third entry, and it moves `const schemaVersion`
  (`:128`) from 2 to **3**. Do **not** attempt to backfill in SQL with `json_extract`: it would create
  a second definition of "tool names" alongside `ExtractMeta`'s — this repo's own named defect class.
- **The shared migration fixture must learn to strip the new column.** `schema.sql` is the *current*
  shape, so once it carries `req_tool_names` the fixture `eventsSchemaWithoutTranscriptColumns`
  (`store_test.go:1052`) already has it when the runner reaches `migrations[2]`, and the bare
  `ADD COLUMN` then fails "duplicate column name" on every existing migration test (its callers
  `TestMigrateExistingDatabase` `:1118`, `TestMigrateHealsAPartialDatabase` `:1142`,
  `TestFreshSchemaMatchesTheMigratedShape` `:1248`) — the exact failure the repo documents at
  `store_test.go:1271-1274`. That helper builds its fixture by text-stripping `schemaSQL`
  (`store.go:29-30`) anchored on the "Transcript-only" comment; extend it (or add a sibling text-edit
  helper it calls) to also strip `req_tool_names`, mirroring how it drops the transcript columns.
- **`clens doctor` reports the gap**, so an un-backfilled database is a visible number rather than a
  quiet behaviour change. A row count of "N rows awaiting backfill" is the discovery path. That count
  is a store method too (see the backfill bullet) — `doctor` does not run the SQL.
- **Back up before the migration (CLAUDE.md "Migrations").** `migrations[2]` is an in-place
  `ALTER TABLE` against the live store — the only copy of the captured traffic. Before `Open` runs it,
  copy `lens.db`, `lens.db-wal` and `lens.db-shm` to a separate path and state that backup path in the
  PR body. Regardless of file size; this is the repo's hard convention, not a nicety.
- **Ordering is part of the bead**: back up, then migration, then `backfill-tool-names` against the
  live store, **before** the new build serves traffic. The bead's own verification says so.

### Traps

1. **`ALTER TABLE … ADD COLUMN … GENERATED ALWAYS AS (…) STORED` is not available.** SQLite permits
   adding only `VIRTUAL` generated columns by `ALTER TABLE`, and a `VIRTUAL` one would recompute from
   `req_body` on every read — reading the body the bead exists to stop reading. This is why the
   derivation is in Go and the backfill is a command.
2. **`req_body_len` is not needed.** It looks like the obvious way to preserve body-presence, but
   presence is already exactly `req_tool_names IS NOT NULL` under the contract, and a second column
   for one bit is a second thing to keep true.
3. **`rulesOmittedColumns` must not be deleted** even though it becomes equal to
   `summaryOmittedColumns` — `br-GI-13-03`'s `TestRulesProjectionNamesEveryColumnTheRulesRead`
   asserts against it and against `rulesSelectColumns`.
4. **`summaryOmittedColumns` must not gain `req_tool_names`, and the column is not "omitted" at all.**
   That set means "the blobs the list never reads", and the new column is neither a blob nor omitted
   from the list — it joins `eventColumnNames` itself (see the projection bullet above), so it is
   *selected and scanned* by both the summary and the rules projections rather than excluded by either.
5. **Fixtures on both sides of the rule will stop firing until they are updated.**
   `TestRulesProjectionNamesEveryColumnTheRulesRead` names nine columns including `req_body`; that
   list changes (`req_body` leaves, `req_tool_names` joins), so update it in the same pass or the suite
   fails on a name it was right to assert. The three existing `ruleCacheInvalidatedByTools` cases in
   `internal/analyze/rules_test.go` are the same shape on the analyze side:
   `TestRuleCacheInvalidatedByToolsFiresOnChangedArray` (`:103`),
   `...SilentOnIdenticalArray` (`:114`) and `...FiresAcrossInterleavedJSONLRow` (`:130`) each build
   their rows from `ReqBody: reqBodyWithTools(...)` with no `ToolNames`/`HasReqBody`, so once the rule
   reads the stored column they find nothing and the expected findings `[2]` / `[3]` never appear. They
   must set `ToolNames` (and `HasReqBody`) and drop `ReqBody`. On the store side,
   `TestSessionEventsForRulesReturnsTheSameRowsInOrder` (`store_test.go:829`) is a fourth fixture the
   change breaks: its `ReqBody`-equality term at `:870` compares the rules projection against the full
   projection, so `got.ReqBody` is `nil` once `req_body` leaves the rules projection and the test
   fails. See the Test Specifications and Files to Touch for the required edit.
6. **Do not also drop `req_body` from the summary/list projections.** It is read by the session
   detail and export paths; only the *rules* projection sheds it.

## Rationale

§7 R5 asked for the perf claim to be measured after the fix rather than asserted. Measuring it found
the claim incomplete: the pass is dominated by payload reads that C1/C2/C3 never addressed, because
C3's D2 deliberately kept `req_body` for one rule. The rule does not need the body — it needs the same
handful of tool names it compares between adjacent rows, and those are already computed on the write
path for other reasons. Recording them makes the rules projection body-free, which is what C3's own
premise ("the rules read exactly nine columns") always implied.

## Outcome Definition

- `SessionEventsForRules` no longer selects `req_body`; `rulesOmittedColumns == summaryOmittedColumns`
  and both `rulesColumnNames` and `summaryColumnNames` have **42** entries (48 columns minus the 6
  blobs — `req_tool_names` joined `eventColumnNames`, so the summary side gains it too, deliberately).
- `ruleCacheInvalidatedByTools` compares a stored value, and does so without any `parse.ExtractMeta`
  call — no rule in `internal/analyze` unmarshals a request body at analysis time.
- `req_tool_names` is `NULL` exactly when the row has no request body, and otherwise the JSON array of
  the request's tool names in body order.
- A proxy row written through the consumer carries its tool names without any additional parse of
  `req_body`.
- A JSONL row is stored with `req_tool_names IS NULL` and is skipped by `rowsWithRequestBody` exactly
  as before.
- `clens backfill-tool-names` fills historical rows and is idempotent (a second run is a no-op);
  `clens doctor` reports the outstanding count.
- **Measured**: the pass over session `ef875ca8` is ~0.04 s warm, down from ~1.65 s — re-measure and
  record both in br-GI-13-06's result.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/store/ ./internal/analyze/ ./internal/consumer/ ./internal/cli/`.

## Test Specifications

- Unit Tests (`internal/store/store_test.go`):
  - `TestRulesProjectionOmitsRequestBodies`: `rulesColumnNames` does not contain `req_body`, and
    `rulesOmittedColumns` equals `summaryOmittedColumns` — the mirrored check, in the shape
    `TestSummaryColumnsAreTheFullSetMinusBodies` already uses.
  - `TestReqToolNamesIsNullExactlyWhenThereIsNoBody`: three rows — a proxy row with tools, a proxy row
    whose body declares no tools, and a JSONL row with no body — come back as `["…"]`, `[]` and
    `NULL` respectively. **The middle row is the point**: a `''`-defaulted column passes the other two
    and fails this one.
  - `TestRulesScanMatchesRulesColumns`: the **42**-destination count, mirroring
    `TestSummaryScanMatchesSummaryColumns`. Both projections are 42 (48 minus the 6 blobs), so the
    rules count is *unchanged* at 42 while the summary count moves from 41 → 42 — but the fixtures are
    not automatic. `TestRulesScanMatchesRulesColumns` (`store_test.go:920`) still passes a stale
    `&reqBody` extra (`len(v.dest(&es, &reqBody))`); under this change that is 42 + 1 = 43 against a
    42-column list — a hard failure. It must **drop** the extra
    (`len(v.dest(&es)) == len(rulesColumnNames)`, 42 == 42), because `scanEventForRules` no longer
    appends one. `TestSummaryScanMatchesSummaryColumns` passes no extra and needs no edit.
  - `TestSessionEventsForRulesReturnsTheSameRowsInOrder` (`store_test.go:829`, existing): its
    `ReqBody`-equality term (`:870`, `string(got.ReqBody) != string(want.ReqBody)`) compares the rules
    projection against `GetEvent`'s full projection, so `got.ReqBody` is `nil` once `req_body` leaves
    `rulesColumnNames` and the test fails. Replace that term with a `ToolNames`/`HasReqBody` comparison
    (or drop it), and add `got.ReqBody != nil` to the omitted-blob check at `:873-876` alongside
    `RespBody`.
- Unit Tests (`internal/analyze/rules_test.go`):
  - `TestRuleCacheInvalidatedByToolsReadsTheStoredNames`: two rows whose stored tool names differ fire
    the finding; two whose names are equal do not; **the fixture carries no `ReqBody` at all**, which
    is what proves the rule no longer reads one.
- Unit Tests (`internal/cli/backfill_test.go`):
  - `TestBackfillToolNamesFillsOnlyUnsetRows`: a fixture with one already-set row asserts it is
    untouched and the unset row is filled; a second run changes nothing.
- Integration Tests: none beyond the store fixtures above.
- Manual (recorded in the PR body): `go run ./cmd/clens doctor` showing the outstanding count go to
  zero after the backfill, and the br-GI-13-06 re-profile.

## Files to Touch

- `internal/store/schema.sql` (modify — `req_tool_names TEXT` on `events`, with the NULL contract as
  its comment)
- `internal/store/store.go` (modify — the migration `migrations[2]` and `schemaVersion` 2→3;
  `eventColumnNames`, `rulesOmittedColumns` and its comment; `eventScanVals`/`dest`/`apply`;
  `scanEventForRules`; the shared `encodeToolNames` encoder; the three doc comments the change
  falsifies (`SessionEventsForRules` `:338-345` "`req_body` included", `SessionEventsSummary`
  `:364-367` "compares consecutive request bodies", `scanEventForRules` `:1427-1429` "plus `req_body`,
  the one extra column"); and the **backfill's store-side statements** — a paged read of rows where
  `req_body IS NOT NULL AND req_tool_names IS NULL`, the single-column write, and the outstanding
  count `doctor` reports. Those three live in `internal/store`, not the CLI, following
  `reflag`/`reprice` — the store's `db` is unexported (`store.go:52`) and the repo states the rule at
  `internal/cli/reflag.go:20-23`)
- `internal/store/merge.go` (modify — `eventWriteColumns`/`eventWriteArgs`, the one write site shared
  by insert and update; the `mergeEvents` backfill of `req_tool_names`)
- `internal/store/store_test.go` (modify — the three cases above, including the existing projection
  tests whose counts move (`TestRulesScanMatchesRulesColumns` drops its stale `&reqBody` extra),
  `TestSessionEventsForRulesReturnsTheSameRowsInOrder` (`:829`) whose `ReqBody` comparison at
  `:867-872` breaks once `req_body` leaves the rules projection — compare `ToolNames`/`HasReqBody`
  instead, or drop the term, and add `got.ReqBody != nil` to the omitted-blob check at `:873-876` — and
  the shared migration fixture `eventsSchemaWithoutTranscriptColumns` (`:1052`), which must also strip
  `req_tool_names` so `migrations[2]` can add it — 08's `TestMigrateAddsTheStatsIndexAtVersionFour`
  does NOT reuse this helper; it builds from bare `schemaSQL` (see 08), where `req_tool_names` must
  stay present)
- `internal/store/types.go` (modify — `EventSummary.ToolNames string` and `EventSummary.HasReqBody
  bool`; `Event` embeds `EventSummary`, so `ev.ToolNames` still reads)
- `internal/consumer/consumer.go` (modify — set `ev.ToolNames = encodeToolNames(meta.ToolNames)`)
- `internal/analyze/rules.go` (modify — `ruleCacheInvalidatedByTools` compares the stored value; delete
  the now-dead `toolNamesEqual` (`:389`), whose only caller is `:348`, which this change replaces)
- `internal/analyze/rules_test.go` (modify — the new rule case above, **and** the three existing
  `ruleCacheInvalidatedByTools` fixtures that build from `ReqBody` alone:
  `TestRuleCacheInvalidatedByToolsFiresOnChangedArray` (`:103`), `...SilentOnIdenticalArray` (`:114`)
  and `...FiresAcrossInterleavedJSONLRow` (`:130`) — each must set `ToolNames`/`HasReqBody` and drop
  `ReqBody`)
- `internal/cli/backfill.go`, `internal/cli/backfill_test.go` (create — the maintenance subcommand)
- `internal/cli/doctor.go` (modify — the outstanding-backfill line)
- `cmd/clens/main.go` (modify — register `backfill-tool-names`)

`store.go`, `store_test.go` and `consumer.go` are shared with br-GI-13-01/02/03, which this bead
depends on and which have landed; keep to this bead's own regions. `internal/analyze/rules.go` is
shared with br-GI-13-05 (adjacent rule, different function). `cmd/clens/main.go` is shared with
br-GI-13-09, which adds its own `"shutdown"` key to the same `commands` map (`main.go:16`) — the two
edits are additive keys and do not conflict.
