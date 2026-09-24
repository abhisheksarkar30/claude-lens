# Bead br-GI-16-08: Archive-aware writers — merge, purge cascade + GC, `PurgeableBytes`, rekey, reflag, `req_tool_names` contract

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream C, §C.6 (whole section), §C.4 head (move-vs-discard), §C.10.

- **Bead ID**: br-GI-16-08
- **Priority**: P1 (high — without it archival silently loses data or mislabels rows)
- **Original Estimate**: 2h (at the ceiling; see summary)
- **Dependencies**: br-GI-16-06 (`ArchivedBodyMask`, archive-dir field, schema), br-GI-16-07 (`GCArchive`)
- **Blocks**: br-GI-16-09

## MIGRATION BACKUP RULE (repo CLAUDE.md §Migrations)

This bead touches `internal/store/schema.sql` **comment text only** (no DDL, no `schemaVersion` change), but the
file is under the rule's scope: **if any run of the resulting binary is made against a real `lens.db`, first back
up `lens.db`, `lens.db-wal` and `lens.db-shm` to a separate path and state it.** Tests use temp stores; never the live DB.

## Description

Every writer that could lose, mislabel or leak an archived body learns about markers. Each rule below is a
required behaviour with its own test.

1. **`mergeEvents`** (`internal/store/merge.go:388-446`): the raw tx loader `getEventByRequestIDTx`
   (`merge.go:82`) sets `Event.ArchivedBodyMask` from `body_archive.body_mask` (the marker; it never opens the day
   file and does **not** set `BodiesArchived`, which is a hydration outcome). `mergeEvents` stays a pure function
   of its arguments. A body column is treated as **present** (no backfill from `incoming`; counted in
   `bodySides` for `capture_complete`) **only if the mask holds that column** — never a blanket "all three". So an
   archived proxy-only row (mask lacks bit 4) still backfills a transcript from an incoming JSONL row (e.g. `clens
   ingest --rebuild`). The marker lags the file (monotone): an under-claim only causes a harmless re-backfill; an
   over-claim is unreachable. Zero mask = no marker.
2. **`PurgeOlderThan` / `PurgeUnpriced` / `DeleteJSONLKeyedEvents`** (`store.go:903,936,1952`): these *discard*
   rows; `ON DELETE CASCADE` removes markers. After each, run `Store.GCArchive` so the day files hold no orphan
   bytes and empty day files are removed. **A purge that leaves an archived body behind is a privacy defect** —
   dedicated test greps the archive files' raw bytes for a sentinel.
3. **`Store.PurgeableBytes`** (`store.go:921-931`, feeds `clens purge --dry-run`): add the archived-body term.
   Group events below the cutoff by `body_archive.day`, open each day file once (archive-dir field from bead 06),
   and add the matching `bodies` row's `req_len + resp_len` (parity with the hot term; `transcript_content` stays
   out of scope). **A missing or unreadable day file is skipped and counted, never fatal**, so
   `clens purge --dry-run` (`purge.go:96`) cannot error over a moved archive; surface the skipped count in the dry-run output.
4. **`rekey` pass 1**: **skip and report every row with a `body_archive` marker — marker presence is the
   criterion, not `resp_body` availability** (a marker-bearing row's `resp_body` may be hot, and on a collision
   the absorbed row is deleted by `rekeyOneProxyRow`, `store.go:1778,1788`, losing archive-only bodies through the
   CASCADE — the one *move*-not-*discard* delete). Add `Skipped` to `RekeyPass1Report` (`store.go:1672-1675`) and
   render it in **both** print sites in `internal/cli/rekey.go` (`:74-76` dry-run, `:81` live) alongside
   `ReKeyed`/`Synthetic`, **with the remedy `clens archive restore` first**. Pass 2 (headers only) and pass 3 unchanged.
   Do **not** extend rekey to carry archived bodies to the survivor.
5. **`reflag`** (`internal/cli/reflag.go`): exclude archived rows from `reflagScope` and **report the excluded
   count with the remedy** (`clens archive restore` first). This narrows the scope; it does **not** add a fourth
   `ReflagCounts` bucket (three buckets still partition the narrowed scope).
6. **`backfill-tool-names`**: no code change; assert (test) it never sees archived rows and that a row archived
   after backfill is already `req_tool_names`-populated.
7. **`req_tool_names` contract**: archival leaves `req_body IS NULL AND req_tool_names IS NOT NULL`, the shape
   `merge.go:390-397` calls a violation. New stated semantics: **`req_tool_names` is populated iff the call had a
   request body, hot or archived.** Update the `schema.sql` comment (`:55-61`), the matching comment at
   `store.go:1459-1462` and `merge.go:390-397` (make it archive-aware), and **re-confirm** (read, and add a test
   or assertion where cheap) that `rowsWithRequestBody` (`internal/analyze/rules.go:357-373`) and `doctor`'s
   `toolNamesBackfillCheck` (`internal/cli/doctor.go:211-233`) define "has a request body" as `req_tool_names IS NOT NULL`.
8. **`reprice`**, **`redact self-test`**: no change (usage columns / headers only; `SkipHydrate` came in bead 06).

**Named test (owned here, per plan F9.1): rekey pass 1 over a marker-bearing collision candidate** — a
`resp_body`-hot row whose other bodies are archive-only, with a taker already holding its target id — is
**reported and skipped**: not absorbed, archive bytes survive, and a following `GCArchive` collects nothing.

## Rationale

The cascade rule ("delete removes markers") is only sound for *discarding* writers. Merge, rekey and purge-dry-run
each quietly assumed bodies live in `events`; without this bead archival would drop a transcript, absorb-and-lose
archived bodies, or report "0 B" for a gigabytes purge.

## Outcome Definition

- Merge with an archived existing row: bodies neither backfilled when the mask holds them nor dropped when it does not; `capture_complete` not laundered.
- After any purge, no byte of a purged body remains in `archive/`.
- `purge --dry-run` over a fully archived range reports the archived bytes; over a range with a missing day file it reports the rest plus a skipped count and does not error.
- Rekey pass 1 and reflag exclude archived rows and print counts with the `clens archive restore` remedy.
- The `req_tool_names` comment states the new semantics and the two readers are confirmed.
- `go build ./... && go vet ./... && go test ./internal/store/ ./internal/cli/ ./internal/analyze/` passes, then `go test ./...`.

## Test Specifications

- Unit Tests (`internal/store/archive_writers_test.go`, `merge_test.go`, `internal/cli/*_test.go`):
  - Merge: archived proxy-only existing + incoming JSONL with transcript ⇒ transcript backfilled; archived existing with mask 7 + incoming with a body ⇒ no backfill, `capture_complete` unchanged; mask lacking bit 1 ⇒ `req_body` backfilled; zero mask ⇒ current behaviour (regression).
  - `ArchivedBodyMask` set by `getEventByRequestIDTx`; `BodiesArchived` untouched on that path.
  - `PurgeOlderThan` cascade removes markers; `GCArchive` after it leaves no sentinel bytes in day files (byte grep); same for `PurgeUnpriced` and `DeleteJSONLKeyedEvents`; empty day file removed.
  - `PurgeableBytes` counts an archived range's `req_len+resp_len`; missing day file skipped-and-counted, no error; `purge --dry-run` prints the skipped count.
  - Rekey pass 1 marker-skip test (named above); `Skipped` rendered in both `rekey.go` print sites with the remedy.
  - Reflag: archived `capture_complete=1` warned row leaves scope (not counted in Residual), excluded count printed with remedy; the three buckets unchanged.
  - `backfill-tool-names` never selects an archived row.
  - `rowsWithRequestBody`/`toolNamesBackfillCheck` treat an archived row (`req_body` NULL, `req_tool_names` set) as having a request body.
- Integration Tests: `ingest --rebuild` after archival on a synthetic store keeps the archived body and backfills the new transcript.

## Files to Touch

- `internal/store/merge.go` (modify — loader sets `ArchivedBodyMask`; mask-keyed presence; comment at `:390-397`)
- `internal/store/store.go` (modify — purge writers call `GCArchive`; `PurgeableBytes`; `RekeyPass1Report.Skipped` + marker skip; comment at `:1459-1462`)
- `internal/store/schema.sql` (modify — `req_tool_names` comment only; see backup rule)
- `internal/store/archive_writers_test.go` (create)
- `internal/store/merge_test.go` (modify)
- `internal/cli/rekey.go` (modify — print `Skipped` at both sites)
- `internal/cli/reflag.go` (modify — exclude archived, report count)
- `internal/cli/purge.go` (modify — show skipped-day-file count in dry run)
- `internal/cli/doctor.go` (modify only if `toolNamesBackfillCheck` needs a comment/assertion; otherwise re-confirm, no change)
- `internal/cli/rekey_test.go`, `internal/cli/reflag_test.go`, `internal/cli/purge_test.go` (modify)
- `internal/analyze/rules.go` (re-confirm; comment only if needed) and `internal/analyze/rules_test.go` (modify)
