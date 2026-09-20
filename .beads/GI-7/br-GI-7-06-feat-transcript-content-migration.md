# Bead br-GI-7-06: Transcript content in its own columns — the repo's first schema migration, and the merge rule that keeps it

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D4, §2.5, §4 (the `D4 only` rows and the `store.go` migration row), §5 T8/T9, §6 (the migration-on-a-41-MB-store row and the merge-drops-the-reconstruction row), §9 bead 06

- **Bead ID**: br-GI-7-06
- **Priority**: P1 (high)
- **Original Estimate**: 5h
- **Dependencies**: br-GI-7-01
- **Blocks**: br-GI-7-04 and br-GI-7-07 — both only while this bead is kept (each names it as a
  conditional dependency); cutting this bead drops the edge rather than leaving a dangling one

## Description

Transcripts carry full message content and the collector discards it: `internal/jsonlogs/dedup.go:25-29`
parses each line into `Message{Model, StopReason, Usage}` and never decodes `content`, so it is never
stored. A real transcript on this machine carries `text`, `thinking`, `tool_use` and `tool_result`
blocks under `message.content`. This bead gives transcript rows real content, in **their own columns**,
with explicit provenance.

**This is the largest bead in the set and the one to cut.** If transcript reconstruction is not
wanted, cutting this bead removes the migration runner too, because no other bead changes the schema.
Both things that make it expensive are named below so the cut-point is explicit rather than discovered
mid-implementation.

### What is not possible, and must not be faked

A transcript carries **no headers** — not redacted, absent. There is no request line, no status code,
no `Content-Encoding`. The detail view renders **no** header table for a transcript row (br-GI-7-04).

**Why not reuse `req_body`/`resp_body`.** A transcript excerpt is not a wire capture: it is one
assistant message, not the request that produced it (which is the whole conversation prefix), and it
excludes the system prompt, the tool schemas, and everything the proxy sees. Writing it into
`req_body` would make a reconstruction indistinguishable from a capture **in the same column** — and
the schema's provenance discipline (`first_source`, `source_refs`, the cross-source merge, which
*prefers* a non-empty body) exists precisely to keep those apart.

### `internal/store/schema.sql` — the columns get one SQL home

Add `transcript_content BLOB` and `transcript_role TEXT` to `CREATE TABLE events` (beside `req_body` /
`resp_body` at `schema.sql:51-52`) — a fresh install gets them whole. The file carries **no**
`PRAGMA user_version` line and **not** the `ALTER` statements: each piece of SQL has one home, and the
version is written by the runner (or seeded on the fresh path).

### `internal/store/store.go` — `Open` and the `PRAGMA user_version` runner (F1.8, F2.3, F3.6, F4.1)

Today `Open` runs `db.Exec(schemaSQL)` and nothing else (`store.go:74-77`); `schema.sql` is
`CREATE TABLE IF NOT EXISTS` only and never adds a column to a live database. This bead builds the
mechanism. The current shape `Open` must take:

```go
const schemaVersion = 1 // bump in lockstep with a new entry in migrations

// migrations[n] upgrades user_version n -> n+1.
var migrations = []string{
    `ALTER TABLE events ADD COLUMN transcript_content BLOB;`,
    `ALTER TABLE events ADD COLUMN transcript_role TEXT;`,
}
```

1. **Always run `db.Exec(schemaSQL)`.** Every statement in it is `IF NOT EXISTS`, so the exec is a
   no-op against an existing database and **heals a partial one**: a first `Exec` that died mid-file
   (disk full, power loss between statements — the exec is not atomic) leaves missing tables and
   indexes, and the unconditional exec is what repairs them on the next open. Do **not** add a "skip
   the schema when `events` exists" shortcut: it would strand every missing table behind
   `no such table` on every boot.
2. **The probe decides only whether to stamp, and the fresh stamp is written *before* the exec
   (F4.1).** Before the exec, probe whether `events` exists
   (`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='events'`).
   - **Absent → a fresh file.** Write `PRAGMA user_version = <schemaVersion>` **first**, *then* run
     `db.Exec(schemaSQL)`, which creates the whole current shape (columns included), so the runner
     applies nothing.
   - **Present →** the exec is a no-op for `events`; `user_version` still reads `0` on a pre-change
     database, and the runner applies migration 1 and stamps `1`. The exec always runs; only the
     starting version differs.
3. **The runner sits before `Open` returns the `*Store`**, so no caller observes a database with the
   schema but not the migration. Read `PRAGMA user_version`; for each version above it, run the
   migration and bump `user_version` — migration 1's two `ALTER`s in their own transaction, with the
   version bumped inside it. Guard on the version comparison; the schema exec must never decide
   whether the upgrade runs.

Both nullable and additive, so there is no table rewrite — the point on the live 41 MB / 81k-row store.

**Why the fresh stamp's order is load-bearing, not cosmetic.** The exec is not atomic, and a first
exec on a **new** file can die between `CREATE TABLE events` (which now carries the two columns) and a
later `CREATE TABLE`. With the stamp already written, `user_version` reads the current version when the
process dies, so the next boot's runner applies nothing and the always-run exec heals the missing
table. Seeded *after* a successful exec instead, `user_version` would still be `0` at the crash; the
next boot would see `events` present (not fresh → no stamp), heal the missing table, then read `0` and
apply migration 1 — `ALTER TABLE events ADD COLUMN transcript_content BLOB` against a table that
already has it → `duplicate column name`, returned by `Open` (`store.go:74-77` returns before
anything else can run) on **every** boot thereafter, with no recovery but deleting the database.
Seeding early is also safe in the other direction: if the opening stamp write landed but the very
first `CREATE TABLE` did not, the next boot's probe still reads `events` absent, so it re-seeds and
re-runs the exec.

### The columns through the store's positional lists

- `internal/store/types.go` — `Event` gains `TranscriptContent []byte` and `TranscriptRole string`
  (direct fields beside `ReqBody`/`RespBody`, **not** on `EventSummary`).
- `internal/store/merge.go` — `eventWriteColumns` (`:14-25`) and `eventWriteArgs` (`:27-41`) gain the
  two columns; `eventSelectColumns` (`store.go:983-994`) and `scanEvent` (`store.go:996-1020`) gain
  them.
- **`summaryOmittedColumns` gains both names**, so the list `SELECT` excludes them and the list path
  stays transcript-BLOB-free. br-GI-7-01's `TestSummaryColumnsAreTheFullSetMinusBodies` tracks the
  list, so it demands **six** columns absent after this bead **without being edited** — extend the one
  list, never the assertion.
- `scanEventSummary` needs no change: the summary column list is unchanged in count and content.

### `merge.go` — the backfill rules, and the ordering that is broken without them (F1.9)

`mergeEvents` builds the result as a **copy of the existing row** (`merge.go:145`) and copies a column
from the incoming side only *per explicit rule* (`:246-266`). A new column with no rule is silently
dropped from the incoming side. The common ordering here is **proxy-first**: the capture is written
live, and the transcript reconstruction arrives minutes later as the incoming side — with no rule its
content is discarded.

So the two new columns get backfill rules alongside the existing body backfills (`merge.go:261-266`):

```go
// The columns only B (JSONL) ever supplies -- a reconstruction, never a capture.
if len(existing.TranscriptContent) == 0 {
    merged.TranscriptContent = incoming.TranscriptContent
}
merged.TranscriptRole = preferNonEmpty(existing.TranscriptRole, incoming.TranscriptRole)
```

`transcript_content` is a BLOB, so it takes the same `len(...) == 0` form the body backfills already
use; `preferNonEmpty` is `func(string, string) string` (`merge.go:271-276`) and applies to the TEXT
`transcript_role` only.

**When a capture and a reconstruction disagree.** The two columns are structurally transcript-only:
only `internal/jsonlogs` ever writes them, so a proxy row's value is always empty and "disagreement"
is not reachable *for these columns*. The rule is the merge's general one — the existing
(first-written) side keeps its value and only an empty existing cell is backfilled — so a
reconstruction never overwrites a reconstruction, and a capture (which has no value here) never
competes. Stated so a future column written by both sides is a deliberate decision rather than an
accident of the copy.

### `internal/jsonlogs` — capture the content

- `dedup.go` — `message` (`:25-29`) gains `Content json.RawMessage \`json:"content"\`` and
  `Role string \`json:"role"\``. **The plan names the content field but not the role's source**; the
  transcript's own shape (`{"message":{"role":"assistant","content":[…]}}`) makes `message.role` the
  source, with `line.Type` (already parsed, `dedup.go:13`) the available fallback. Pick one and say
  which in the code comment.
- `jsonlogs.go` — the transcript path persists the message's content and role onto
  `ev.TranscriptContent`/`ev.TranscriptRole`. Only `assistant` lines carry `Message` (`dedup.go:9-11`),
  so a non-assistant line stores nothing — NULL, which means "no transcript reconstruction", never a
  faked zero.

### Provenance in the UI

No UI change lands here: the rendering of transcript content under an explicit *"reconstructed from
transcript — not a wire capture"* label belongs to br-GI-7-04's detail view. This bead's job is that
the content exists in a column whose name says what it is.

**The ceiling, stated honestly.** Transcript content is per-message, so "the request" for a transcript
row is a reconstruction of intent, not a thing that exists on the wire. If a clean "proxy gives
bodies, transcripts give tokens" split is preferred, this is the bead to drop — and dropping it is a
defensible answer, not a gap.

## Rationale

A `jsonl`-sourced row is the only row whose content the tool holds and never shows. The alternative to
new columns is worse in both directions: writing a reconstruction into `req_body` makes it
indistinguishable from a capture in a column the merge already has precedence rules for, and a merge
would then silently prefer a real capture over a reconstruction — or, worse, the reverse.

The migration runner is new machinery and this is the repo's first schema change; GI-1 recorded
"Migrations | None in v1" as a *decision*, not an omission, and this story is the first to need one.
It is built here rather than in a bead of its own because a migration bead with no column to add would
be a commit whose deliverable is unused schema.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `Open` on a path that does not exist succeeds with no `duplicate column name` error, the `events`
  table carries both columns, and `user_version` is the current version (T9a).
- `Open` on a pre-change database (no transcript columns, `user_version == 0`) adds both columns and
  bumps `user_version` (T9b).
- `Open` on a database whose `events` exists but which is missing another table creates the missing
  table **and** adds the two columns exactly once (T9c).
- `Open` on a database built from the *current* schema up to `CREATE TABLE events`, with a later table
  absent and `user_version` at the seeded current version, succeeds with no `duplicate column name`,
  creates the missing table, and does not re-add the columns (T9d).
- A jsonl row with content stores it in `transcript_content`/`transcript_role`, **never** in
  `req_body`; a merge fills the two columns from whichever side has them, in **both** orderings (T8).
- The list route still carries no transcript BLOB, and `TestSummaryColumnsAreTheFullSetMinusBodies`
  passes unedited with six omitted columns.
- No new dependency, no new build step, no change to `internal/proxy`.

## Test Specifications

- Unit Tests (`internal/store` — T9, the migration runner, four paths):
  - **(a) fresh** — `Open` a non-existent path: assert success and no `duplicate column name`, that
    `events` carries both columns, and that `user_version` is the current version. This is the case the
    round-1 draft would have failed, because its runner would have re-`ALTER`ed a table `schema.sql`
    had already created with the columns.
  - **(b) pre-change** — open a temp file carrying the pre-migration schema (no transcript columns,
    `user_version == 0`), run `Open`, assert both columns now exist and `user_version` was bumped —
    the "existing database" case `CREATE TABLE IF NOT EXISTS` cannot satisfy.
  - **(c) partial** — a database whose `events` exists but which is missing another table (a first
    `Exec` that died mid-file): `Open` creates the missing table **and** adds the two columns exactly
    once.
  - **(d) partial-new-schema** — a database built by executing the *current* schema up to and including
    `CREATE TABLE events` (so `events` already carries the columns), with a later table absent and
    `user_version` left at the seeded current version. `Open` **succeeds with no `duplicate column
    name`**, the missing table now exists, and the two columns were not re-added. Built by hand (exec
    the current schema's `events` DDL into a temp file) rather than by racing a real mid-file crash, so
    it is deterministic. Seeded the other way — the fresh stamp written only after a successful exec —
    this is the database `Open` could never repair; case (c) passes either way.
- Unit Tests (`internal/store/merge_test.go` and `internal/jsonlogs/jsonlogs_test.go` — T8):
  - A jsonl fixture with content lands it in `transcript_content`/`transcript_role` and leaves
    `req_body` empty.
  - The merge **both ways**: JSONL-first (the reconstruction is `existing`, the proxy capture arrives
    as `incoming`) and proxy-first (the capture is `existing`, the reconstruction arrives as
    `incoming`) — the second being the common ordering and the one the design as first written
    silently dropped. Assert the two columns fill from whichever side has them and neither side's
    content is lost.
- Integration Tests (`internal/store`): the always-run exec heals a database missing an index as well
  as a missing table.
- Unit Tests (`internal/api`): `GET /api/requests?limit=50` against a fixture with transcript content
  remains under 64 KB and carries no `TranscriptContent`/`TranscriptRole` key.
- E2E: none.

## Files to Touch

- `internal/store/schema.sql` (modify — the two columns in `CREATE TABLE events`; no PRAGMA, no ALTER)
- `internal/store/store.go` (modify — `schemaVersion`, `migrations`, the pre-exec probe, the fresh
  stamp before the always-run `db.Exec(schemaSQL)`, the runner before `Open` returns;
  `eventSelectColumns`/`scanEvent` gain the two columns; `summaryOmittedColumns` gains both)
- `internal/store/types.go` (modify — `Event.TranscriptContent`, `Event.TranscriptRole`)
- `internal/store/merge.go` (modify — `eventWriteColumns`, `eventWriteArgs`, the two `mergeEvents`
  backfill rules)
- `internal/store/store_test.go` (modify — T9 a–d)
- `internal/store/merge_test.go` (modify — T8's merge orderings)
- `internal/jsonlogs/dedup.go` (modify — `message.Content`, `message.Role`)
- `internal/jsonlogs/jsonlogs.go` (modify — persist content and role on the transcript path)
- `internal/jsonlogs/jsonlogs_test.go` (modify — the jsonl content fixture and its assertion)
