# Bead br-GI-16-06: Migration 4→5 `body_archive`, per-blob-codec day-file format, mask-driven hydration

**Plan Reference**: `docs/planning/GI-16-calls-range-restart-archival.md` — Workstream C, §C.1-C.3, §C.5 (read path), §C.6 (archive-dir field).

- **Bead ID**: br-GI-16-06
- **Priority**: P1 (high — everything else in workstream C builds on it)
- **Original Estimate**: 2h (at the ceiling; see summary — the outline packs several deliverables here)
- **Dependencies**: br-GI-16-05 (`HotDays` config exists; this bead itself does not read it)
- **Blocks**: br-GI-16-07, br-GI-16-08, br-GI-16-10

## MIGRATION BACKUP RULE (repo CLAUDE.md §Migrations)

This bead bumps `schemaVersion` 4 → 5 and edits `internal/store/schema.sql`. **Before any run of the new
binary against a real `lens.db`, back up `lens.db`, `lens.db-wal` and `lens.db-shm` to a separate path and state
that path.** No size exemption. The implementing agent **must not** run the migration against the live
`D:/clens/lens.db`; verification uses a **copy** of it (or a synthetic store).

## Description

### 1. Schema and migration (both homes, per the repo's rule)

Bump `schemaVersion` to 5 (`internal/store/store.go:128`), add a `4 → 5` step in `migrate` (`store.go:216`) **and**
the same DDL in `schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS body_archive (
    event_id    INTEGER PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    day         TEXT    NOT NULL,   -- UTC 'YYYY-MM-DD' of started_at; names the archive file
    archived_at INTEGER NOT NULL,   -- unix ns
    body_mask   INTEGER NOT NULL DEFAULT 0  -- 1 req_body, 2 resp_body, 4 transcript_content
);
CREATE INDEX IF NOT EXISTS idx_body_archive_day ON body_archive(day);
```

`body_mask` is included **now** (migration 4→5 has not shipped; deferring means a second migration against the
2.8 GB store). It is a lagging, monotone mirror of the day file row's mask (§C.4): it may under-claim, never
over-claim. `events` is left byte-for-byte untouched (no new column). `foreign_keys(ON)` is already in the DSN,
so CASCADE works. Update the existing tests that pin `schemaVersion` (`store_test.go:1175,1235,1257`).

Bead 06 also **measures** the C.2 hypothesis (trailing-column overflow cost) on a copy of the live store and
records the number in the bead's commit body; the side-table design does not depend on it being true.

### 2. Day-file format (`internal/store/archive.go`)

File `<dir of DBPath>/archive/bodies-YYYY-MM-DD.db` (dir `0700`, files `0600`; same protection as `lens.db`, no
better on Windows — state that in a code comment):

```sql
CREATE TABLE bodies (
    event_id INTEGER PRIMARY KEY,
    body_mask INTEGER NOT NULL DEFAULT 0,
    req_codec TEXT, resp_codec TEXT, tc_codec TEXT,     -- each 'zstd' | 'raw'
    req_len INTEGER, resp_len INTEGER, tc_len INTEGER,  -- ORIGINAL (uncompressed) lengths
    req_body BLOB, resp_body BLOB, transcript_content BLOB
);
```

- Each blob compressed independently with `klauspost/compress/zstd` (already a dependency). **Codec is per blob**:
  `raw` if compression would not shrink it; the stored codec **always matches the stored bytes**. A blob the row
  does not hold writes NULL for blob, codec and `*_len` **together** (a triple stays paired).
- NULL stays NULL; an empty-but-non-NULL body (`[]byte{}`) round-trips as empty, not NULL.
- **Write is a monotone upsert**: `INSERT … ON CONFLICT(event_id) DO UPDATE SET body_mask = bodies.body_mask |
  excluded.body_mask` and `COALESCE(excluded.<col>, bodies.<col>)` for every blob, its `*_len` and its codec.
  Never `INSERT OR REPLACE` (delete-then-insert loses a wider writer's column).
- Provide the write helper (`writeDayRows(day, rows)` — one archive transaction; the mask is derived inside it
  from the columns actually written non-NULL) and read helper for bead 07 to reuse, and a small **LRU of
  read-only handles, cap 4**; writer opens read-write per batch.
- **Decode is size-bounded**: max output = the recorded `*_len` (plus a `BodyCapBytes`-scale sanity bound); a
  mismatch or decode error ⇒ treated as `missing`, logged. No decode bombs.

### 3. Store archive-dir field

`Store` gains an archive-dir field set in `Open` from `filepath.Dir(dbPath)/archive` (`store.go:62-123`, the one
construction point). `GCArchive` (bead 07) and `PurgeableBytes` (bead 08) read it — do not re-derive elsewhere.

### 4. Mask-driven `hydrate`

`Store.GetEvent` (`store.go:361`) and `Store.ListEventsFull` (`store.go:457`) call `hydrate(events…)` after scan:

- For a row with a `body_archive` marker: group by `day`, open each day file **once**, read the **day row's
  `body_mask` (the authority — not the marker, not `Event.ArchivedBodyMask`)**, and fill **exactly the columns that
  mask holds and that are empty in the hot row**; leave populated/unheld columns untouched. The marker supplies
  only the day. (A "three bodies empty" gate is wrong: a hot transcript beside archive-only req/resp must still
  hydrate.)
- New read-only `Event.BodiesArchived string` in `types.go`: `""` (hot / never archived), `"restored"` (**at least
  one** body loaded from the archive, including a partially restored row — never `""` for such a row),
  `"missing"` (marker's day file or row absent/undecodable). Never written by `InsertEvent`. Wire key (its Go
  field name).
- New `Event.ArchivedBodyMask uint8` tagged **`json:"-"`** (internal load-path flag for bead 08's merge; not wire
  data). Bead 06 only declares it; bead 08 sets it.
- `store.EventFilter` gains `SkipHydrate bool` (default false), read by `ListEventsFull`. It is a **filter flag,
  not a sibling method**: `ListEventsFull`'s signature and the `api.Store` interface (`api.go:55`) stay
  **unchanged**; no `internal/api/api.go` edit. `checkRedaction` (`internal/cli/serve.go:347`, runs on the boot
  path, `redactScanLimit = 500`) sets `SkipHydrate: true` so boot never decompresses archived bodies.
- `ListEvents`, `SessionEvents*` and every `Stats*`/`Session*` query stay unchanged (they never select a body).
- A large `ListEventsFull` over an archived range opens one file per day, not one per row.

## Rationale

Bodies are 99.5% of the 2.7 GB file; archiving only bodies keeps every aggregate whole-history without fan-out
across files. This bead supplies the schema, the file format and the read path that make an archived call still
show its body; bead 07 fills the archive, bead 08 makes writers aware of it.

## Outcome Definition

- A fresh store and a migrated 4→5 store both have `body_archive`; `PRAGMA user_version = 5`; `schema.sql` and
  `migrate` agree.
- Test-only: a row with a marker and a synthetic day file hydrates to byte-identical bodies (NULL vs empty preserved); a partially hot row hydrates only the missing masked columns.
- `missing` day file / row / undecodable blob ⇒ `BodiesArchived == "missing"`, no panic, no error return.
- `checkRedaction` opens zero archive files (asserted).
- `ArchivedBodyMask` is absent from marshalled `Event` JSON; `BodiesArchived` is present.
- `go build ./... && go vet ./... && go test ./internal/store/ ./internal/cli/` passes, then `go test ./...`; `internal/store` still does not import `pricing`.

## Test Specifications

- Unit Tests (`internal/store/archive_test.go`, `store_test.go`):
  - Migration: v4 DB (existing helper at `store_test.go:1175`) migrates to 5; table+index exist; `events` columns unchanged.
  - Format round trip: bytes in == bytes out for zstd and raw blobs; per-blob codec chosen independently in one row (compressible 500 KB req beside 1 KB incompressible resp); NULL vs `[]byte{}`.
  - Monotone upsert: writing mask 3 then mask 4 for one event yields mask 7 with all three blobs and lens/codecs preserved; writing narrower after wider never drops a column.
  - Decode bound: a blob whose recorded `*_len` is smaller than its decoded output ⇒ error ⇒ `missing`.
  - `hydrate`: full, partial (hot transcript + archived req/resp), already-hot untouched, missing file, missing row, marker with no rows; `BodiesArchived` values for each; one file open per day for a multi-row list.
  - `SkipHydrate`: rows keep NULL bodies and no file is opened.
  - `json:"-"` on `ArchivedBodyMask`.
  - Store archive-dir field equals `filepath.Dir(dbPath)/archive`.
  - File modes: dir `0700`/files `0600` on Unix (skip mode assertions on Windows).
- Unit Tests (`internal/cli/serve_test.go`): `checkRedaction` uses `SkipHydrate`.
- Integration Tests: migrate a **copy** of the live DB (backup path stated first) and confirm `user_version = 5` and row count unchanged.

## Files to Touch

- `internal/store/schema.sql` (modify — `body_archive` + index)
- `internal/store/store.go` (modify — `schemaVersion`, `migrate` 4→5, archive-dir field in `Open`, `hydrate` calls in `GetEvent`/`ListEventsFull`, `SkipHydrate`)
- `internal/store/types.go` (modify — `BodiesArchived`, `ArchivedBodyMask json:"-"`, `EventFilter.SkipHydrate`)
- `internal/store/archive.go` (create — day-file format, codec, monotone upsert, LRU, `hydrate`)
- `internal/store/archive_test.go` (create)
- `internal/store/store_test.go` (modify — schema version pins, migration test)
- `internal/cli/serve.go` (modify — `checkRedaction` sets `SkipHydrate`)
- `internal/cli/serve_test.go` (modify)
