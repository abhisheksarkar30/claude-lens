# GI#16: Calls custom range, `restart`/`reload`, and body archival

**Version:** 1
**Status:** draft — awaiting Phase 2 approval, then cross-review
**Issue:** [GI#16](https://github.com/abhisheksarkar30/claude-lens/issues/16) (anchor). Workstreams B and C
have no issue of their own; the user chose one story, one plan, one PR (2026-09-24).
**Branch:** `GI-16-calls-range-restart-archival`

## Why one story, three workstreams

| | Workstream | Source of the work | Independent of |
|---|---|---|---|
| **A** | Calls tab custom `from`–`to` range | issue #16 | B, C |
| **B** | `clens restart` + a small live `clens reload` | `br-GI-13-09` explicitly deferred it ("this bead stops at `shutdown` and adds no reload (YAGNI)") | A, C |
| **C** | Body archival: hot window + on-demand load | the live store is **2.7 GB** and growing ~0.7 GB/day | A, B |

The beads are grouped so each workstream can be reverted alone. B and C meet in exactly one place:
`serve` schedules the archiver and `reload` changes its window live (bead 04 ↔ bead 09).

## Decisions taken with the user (2026-09-24)

1. **Archive bodies, not rows.** Whole-row archival would force every aggregate (`/api/stats`,
   sessions, quota, reconcile, warnings) to fan out across files or silently lose history.
2. **Hot window: 7 days, configurable** (`--hot-days`).
3. **One story**, three workstreams.
4. **Restart plus a small live reload**, not restart alone.

## Established facts (each re-verified against code in Phase 1, not taken from the docs)

- **The Calls backend already takes a range.** `listRequests` parses `since` and `until` with
  `parseTimeBoundParam` and puts both in `store.EventFilter` ([internal/api/api.go:357-373](../../internal/api/api.go#L357)).
  The issue's line number (363) is off by a few lines; the claim holds. **No backend change for A.**
- **Bytes are bodies.** Live `D:/clens/lens.db`: 2.77 GB across ~55K rows; `req_headers` + `resp_headers`
  total **12.7 MB** for all of them. Bodies were first stored on 2026-09-20 and run 284–906 MB/day.
  Row metadata is ~0.5% of the file, which is why bodies-only archival loses nothing that a
  dashboard aggregate needs.
- **Retention today is a hard delete.** `purgeOnStartup` → `Store.PurgeOlderThan`
  ([internal/cli/serve.go:374](../../internal/cli/serve.go#L374)); `RetentionDays` defaults to 0 (keep
  forever), so on this machine nothing is ever removed.
- **One connection.** `Open` sets `SetMaxOpenConns(1)`; the proxy consumer, collectors, dashboard reads
  and any archiver share it. GI-13's hang was exactly a long hold of this connection. **Any new
  background writer must hold it in short batches.**
- **`events.id` is `AUTOINCREMENT`**, so an id is a stable, never-reused key across files.
- **Shutdown exists, restart does not.** `POST /api/shutdown` (loopback-caller + `originReject`) drives
  `serve`'s `stop`; `clens shutdown` dials it. There is no state file, no relaunch, no reload.
- **Accounts do not hot-reload.** `consumer.New` copies the account list and exposes no setter
  ([internal/consumer/consumer.go:55,81](../../internal/consumer/consumer.go#L55)); `reloadAccounts`
  only validates. Prices already hot-reload (`newPriceLoader`).
- **The merge treats an empty body as absent.** `mergeEvents` backfills `ReqBody`/`RespBody`/
  `TranscriptContent` from `incoming` whenever `existing`'s is empty
  ([internal/store/merge.go:388-446](../../internal/store/merge.go#L388)), and derives
  `capture_complete` from which side supplied a body. An archived row's hot bodies are empty, so an
  incoming row *with* a body would be backfilled beside an archived copy and could flip the flag.
- **`docs/context/**` is generated.** Only Phase 5.6 writes there. New ADRs and behaviour docs are
  therefore on the "Context docs to refresh" list below, not in any bead. Operator runbooks go in
  `README.md`.

---

## Workstream A — Calls custom range (issue #16)

### A.1 Behaviour

Selecting `custom` in the Calls window select reveals two `datetime-local` inputs, **from** and **to**.
Both are *hour* selections and both are inclusive of the hour picked:
`since = timeWindow('hour', from).since`, `until = timeWindow('hour', to).until`.

| from | to | Filter |
|---|---|---|
| set | set | `[from-hour start, to-hour end)`; if `to` < `from` show an inline message and apply **no** window |
| set | empty | `since` only (open-ended) |
| empty | set | `until` only |
| empty | empty | no window (same as "any time") |

Reusing `timeWindow()` twice is the whole point: it is the one place the `±hh:mm` offset is built by
hand. **No `toISOString()`, no second offset implementation** (issue requirement, `dashboard.md` warning).

### A.2 Changes

| File | Change |
|---|---|
| `internal/web/index.html` | add `<option value="custom">custom</option>` to `c-window-gran`; add `<label>from <input id="c-from" type="datetime-local"></label>` and the `to` twin, both `hidden` initially; rewrite the comment above the select that says there is deliberately no `custom` |
| `internal/web/app.js` | `mountWindowPicker($('c-window-gran'), …, [c-from label, c-to label], …)` — the `freeLabels` parameter already exists and already hides/reveals on `custom`; `callFilter()` gains a `custom` branch calling a new `customWindow(fromVal, toVal)` helper that calls `timeWindow` twice; `change` listeners on the two inputs reload; offset reset to 0 on any window change (already true for the picker — verify for the new inputs) |
| `internal/web/assets_test.go` | invert the two Calls-has-no-`custom` assertions in `TestAssetsThePickerMountsBothTabs` (lines ~654-656 and the "selected" default check stays: the default remains `any time`); add: `c-from`/`c-to` mounted as `datetime-local`, `callFilter` reaches `customWindow`, `customWindow` calls `timeWindow(` and contains no `toISOString` |

### A.3 Acceptance

- `custom` on Calls reveals from/to with hour precision; the default stays `any time` (a native
  `<select>` cannot display an unset state — the existing test's reason still applies).
- **Cross-check (issue AC):** a Calls `custom` range returns the same rows as the Stats `custom` range for
  the same instants. Semantic check is manual + a fixture: two `since`/`until` strings from
  `customWindow` equal what typing the same RFC3339 pair into `s-since`/`s-until` sends. The store
  side is the same `EventFilter`, so equality of the query string is equality of the rows.
- **No backend or API change.** If implementation finds one is needed, that changes this workstream's
  scope: stop and re-plan (issue AC).

### A.4 Risks

- No JS runtime in the toolchain, so behaviour is asserted by source-shape guards only. The DST /
  rollover cases are manual, as for the existing picker.
- `datetime-local` has no seconds and no timezone; both are correct here (local wall time in, offset
  appended by `timeWindow`).

---

## Workstream B — `clens restart` and `clens reload`

### B.1 What each command is *for*

- **`restart`** exists because of one operational fact: the proxy fronts a live Claude Code session and
  on Windows a running `clens.exe` cannot be replaced. `shutdown` alone leaves the session with no proxy
  until someone starts a new one by hand. `restart` makes that gap short, measured, and self-healing.
- **`reload`** exists for the cheap subset that *can* change without rebinding a listener.

Anything bound at boot (addresses, upstream URL, DB path, body policy/cap, pprof) needs a restart. That
is a property of the process, not a gap in `reload`; `reload` says so instead of pretending.

### B.2 Serve runtime state file

`serve` writes `<dir of DBPath>/serve.state.json` once both listeners are up:

```json
{"pid": 4242, "exe": "D:/clens/clens.exe", "args": ["serve", "--replay"],
 "started_at": "2026-09-24T06:22:00Z", "proxy_addr": "127.0.0.1:8797",
 "dashboard_addr": "0.0.0.0:8798", "log_path": "D:/clens/serve.log"}
```

- It records how *this* process was launched (`os.Executable()`, `os.Args[1:]`), which is the only way a
  separate `restart` invocation can relaunch "the same thing". Config alone cannot: flags are not in it.
- Removed on graceful exit. **A leftover file is a hint, never the truth:** liveness is decided by
  dialing the dashboard's `/api/health`, never by the file or the pid.
- Contains no secret (paths and addresses only). Written `0600` (a no-op on Windows; same directory as the
  DB, so no weaker than the DB itself).
- `log_path` is where `restart`'s child writes; default `<dir of DBPath>/serve.log` (the file already
  sitting next to the live DB).

### B.3 `clens restart [--exe PATH] [--timeout 30s]`

1. Resolve `config.Load(args)`; read the state file (missing ⇒ no exe/args to reuse; `--exe` then required
   or `os.Executable()` is used with `serve` and forwarded flags).
2. **Running?** `GET /api/health` on the dialable dashboard address (reuse `dialableDashboardAddr`).
   Not running ⇒ skip to step 4 and say "was not running".
3. `POST /api/shutdown` (reuse `runShutdown`'s round trip; do not duplicate it), then wait until **both**
   the dashboard and proxy ports refuse a dial (bounded by `--timeout`). This is what proves the drain
   finished and the store is closed.
4. Spawn `exe serve args…` **detached**, stdout+stderr appended to `log_path`.
   - Windows: `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS`; Unix: `Setsid`. Two small build-tagged
     files (`detach_windows.go`, `detach_unix.go`).
5. Poll `/api/health` until 200 or `--timeout`. Report pid, exe, log path, and **the measured gap**
   (time from shutdown request to healthy).
6. **Rollback.** If `--exe` was given and the new process does not become healthy: kill the child, respawn
   the *previous* exe from the state file, wait for health, print the log tail, and exit **non-zero**
   naming which binary is now serving. Without a rollback a bad build leaves the operator's session with
   no proxy at all. If no previous exe is known there is nothing to fall back to and the message says so.

`--exe` is the answer to the Windows locked-binary problem: build to any other path while the old one runs,
then `clens restart --exe D:\build\clens.exe`. Replacing the *installed* binary stays the operator's call.

### B.4 `clens reload`

`POST /api/reload` — same guard as `shutdown` (`originReject` **and** a loopback caller). Handler calls an
injected `reloadFunc`, wired in `serve.go`:

1. `config.Load(bootArgs)` again (flag > env > `config.toml` > default), then `Validate()`.
2. Diff against the boot `cfg` field-by-field.
3. **Live-apply set** — exactly these, no others: `Accounts`, `RetentionDays`, `HotDays`.
   - `Accounts` needs a mutex-guarded `consumer.SetAccounts` (the consumer goroutine reads it in
     `resolveAccount`, consumer.go:518).
   - `RetentionDays`/`HotDays` move into one small `atomic`-guarded struct the purge/archive ticker reads.
4. Everything else that differs is reported under `restart_required`. Prices are already hot-reloaded and
   unaffected.
5. Response: `{"applied":[…],"restart_required":[…],"unchanged":true|false}`. An invalid file changes
   nothing and returns 400 with the validation error — **reload is all-or-nothing**.

`clens reload` prints that report. The dashboard's `POST /api/accounts` currently *validates only* and
tells the user to restart (`api/accounts.go:87`, `serve.go:391`, both marked `ponytail:`); once
`SetAccounts` exists that route calls the same apply path and the message changes. That removes two
`ponytail:` ceilings, and it is the only edit outside B's own files.

### B.5 Risks

| Risk | Handling |
|---|---|
| **Testing restart against the live proxy kills the operator's Claude session** (handover trap 1) | Every test uses ephemeral ports and a temp DB. No bead's verification runs `restart`/`shutdown` against `127.0.0.1:8797`. Manual live verification is the user's, with the rollback path as the safety net. |
| Child inherits the console and dies with it | Detached process flags + a test that the child survives its parent's exit (helper-process pattern via `TestMain`) |
| `restart` forwards stale args | State file is written by the *running* serve, so args are exactly what it was started with; a `--exe` build that rejects a flag fails health and rolls back |
| Two restarts race | Second sees the dashboard down/unhealthy mid-way and waits for the ports; if both spawn, the loser fails to bind and exits non-zero — fail-closed on the port, never two proxies |
| `reload` half-applies | Validate before apply; apply the three fields under one lock; tests assert all-or-nothing on a bad file |
| New write route widens the attack surface | Same two guards as `shutdown`; the write-guard count in the docs moves three → four |
| Wildcard dashboard bind (`0.0.0.0`, this machine's config) exposes `/api/reload` to the LAN | The loopback-*caller* check is the control, exactly as for `shutdown`; a LAN caller gets 403 |

---

## Workstream C — Body archival

### C.1 Design in one paragraph

After `HotDays`, the three body columns (`req_body`, `resp_body`, `transcript_content`) of a row move into a
**per-UTC-day, zstd-compressed SQLite file** under `<dir of DBPath>/archive/`. The row itself — every
scalar, both header blobs, all cost and token columns — **stays in the hot DB forever**, so every
aggregate, session, warning, quota and reconcile query is untouched and still whole-history. A small
side table records which rows are archived. Reads that need a body (call detail, replay, `show`, export)
fetch it from the archive transparently. `--retention-days` continues to mean "delete", now including the
archived bodies.

### C.2 Why a side table, not a column on `events`

`events` rows carry multi-MB blobs in overflow pages, and a column that follows them in the record
(every `ALTER … ADD COLUMN` lands last — see `req_tool_names` on any migrated store) can force a walk of
the overflow chain merely to be read. That is the mechanism GI-13's profile showed
(`_vdbeColumnFromOverflow`). A marker column would sit on the summary projection that every list read
uses. A separate `body_archive(event_id PRIMARY KEY, …)` keeps `events` byte-for-byte as it is and is read
only by paths that already need a body. *(Hypothesis about the trailing-column cost: bead 06 measures it on
a copy of the live store before relying on it; the side-table design does not depend on it being true.)*

### C.3 Schema and file format

**Hot DB** — migration `4 → 5` and `schema.sql` (both homes, per the repo's rule):

```sql
CREATE TABLE IF NOT EXISTS body_archive (
    event_id    INTEGER PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    day         TEXT    NOT NULL,   -- UTC 'YYYY-MM-DD' of started_at; names the archive file
    archived_at INTEGER NOT NULL    -- unix ns
);
CREATE INDEX IF NOT EXISTS idx_body_archive_day ON body_archive(day);
```

`ON DELETE CASCADE` matters: `PurgeOlderThan` deleting an event removes its marker in the same statement.
`foreign_keys(ON)` is already in the DSN.

**Archive file** `archive/bodies-YYYY-MM-DD.db`:

```sql
CREATE TABLE bodies (
    event_id INTEGER PRIMARY KEY,
    codec    TEXT NOT NULL,          -- 'zstd' | 'raw' (raw if compression would not shrink it)
    req_len INTEGER NOT NULL, resp_len INTEGER NOT NULL, tc_len INTEGER NOT NULL,   -- ORIGINAL lengths
    req_body BLOB, resp_body BLOB, transcript_content BLOB
);
```

- Each blob is compressed independently (`klauspost/compress/zstd`, already a dependency), so one body is
  readable without decompressing its neighbours. NULL stays NULL (a body that was never captured is not
  an empty archived one).
- The stored `*_len` are the **uncompressed** lengths — they are what the verification step and the
  `Completeness`/cap comparisons need, and they let a reader confirm a round trip.
- One file per UTC day: the day in `body_archive.day` *is* the file name, so lookup needs no index file.
  A late row for an already-archived day appends to that day's file (`INSERT OR REPLACE`, idempotent).
- Opened read-only on demand through a tiny LRU (cap 4 handles); the writer opens read-write per batch.
- Directory `0700`, files `0600`. **These files are exactly as sensitive as `lens.db` and exactly as
  protected — no better on Windows** (same inherited ACL as the DB directory). Stated, not solved.

### C.4 The archiver (`internal/store`, `Archiver`)

Candidate rows: `started_at < now − HotDays·24h`, at least one body column non-NULL, and **not** already in
`body_archive`. Selected with `typeof(col) != 'null'` (reads the record header, not the blob) and
`NOT EXISTS (SELECT 1 FROM body_archive …)`.

One batch (≤ 20 rows **or** ≤ 8 MiB of raw body, whichever is first), all within a single UTC day:

1. **Read** the batch's bodies from the hot DB (short hold of the single connection).
2. **Compress and write** into the day file in one archive transaction, with `orig` lengths.
3. **Verify inside the same archive transaction, before commit:** `count(*)` and the three length sums
   read back from the file equal what was written. Commit (fsync via SQLite).
4. **Only then** one hot transaction per batch: `INSERT OR IGNORE INTO body_archive`, then
   `UPDATE events SET req_body=NULL, resp_body=NULL, transcript_content=NULL WHERE id=? AND EXISTS (marker)`.
5. Sleep a short yield (default 50 ms) so the consumer flush and dashboard reads interleave.

**Crash safety is ordering, not a transaction spanning two files:** crash after 3 ⇒ duplicated bodies
(archive has them, hot still has them, no marker) — harmless, the next run redoes the batch idempotently.
Crash after 4's commit ⇒ done. There is no state in which a body exists in neither place.

- **Rows awaiting `backfill-tool-names`** (`req_body` present, `req_tool_names IS NULL`) are **skipped and
  counted**, not archived. Archiving them would strand them: the backfill reads `req_body`.
- Cancellation (`ctx`) is checked between batches; a half-run leaves a consistent store.
- **No `VACUUM`.** Freed pages go to SQLite's freelist and are reused by new inserts, so the hot file
  plateaus near `HotDays × daily volume` (~5 GB at 7 days and today's rate) instead of growing forever; it
  does **not** shrink on its own. Shrinking once is the existing operator step (`clens shutdown`;
  `clens purge --vacuum --yes`; `clens restart`) — a `VACUUM` on the live single connection would stall
  the proxy consumer and drop captures, so serve never does it. README says so.
- With today's data nothing is archivable on the first run (bodies start 2026-09-20, window is 7 days):
  the first real archival lands on 2026-09-27. Tests therefore use synthetic aged rows, never the live DB.

### C.5 Reads: transparent hydration

`Store.GetEvent` and `Store.ListEventsFull` call `hydrate(events…)` after their scan:

- For an event whose three hot bodies are all empty **and** that has a `body_archive` row, group by `day`,
  open each day file once, and fill the bodies (decompressing).
- New read-only field `Event.BodiesArchived string`: `""` (hot / never archived), `"restored"` (filled
  from the archive), `"missing"` (marker exists but file/row absent or unreadable). It is **never written**
  by `InsertEvent`.
- `"missing"` is what keeps an operator who deleted or moved `archive/` from being told a call had no
  body: the dashboard and `show` say *"archived — archive file for 2026-09-20 not found"*, not
  *"not captured"*.
- `ListEvents`, `SessionEvents*`, every `Stats*` and `Session*` query are **unchanged** — they never
  select a body.
- Callers needing no change: `api` detail, `replay`, `cli show`, `export`, `ls --json`, the replay poll.
  A **large export** over an archived range opens one file per day, not one per row.

### C.6 Writers that must learn about archived rows

| Writer | Interaction | Rule |
|---|---|---|
| `mergeEvents` | an archived `existing` has empty hot bodies | If `existing` has a `body_archive` row, treat all three body columns as **present**: no backfill, no `bodySides` recomputation, `capture_complete` keeps `existing`'s. The store method that loads `existing` reports the marker; `mergeEvents` stays a pure function of its arguments. |
| `PurgeOlderThan` / `PurgeUnpriced` / `DeleteJSONLKeyedEvents` | delete `events` | `ON DELETE CASCADE` removes markers; then `Store.GCArchive` deletes archive rows whose `event_id` no longer has a marker and removes day files that become empty. **A purge that leaves an archived body behind is a privacy defect** and has its own test. |
| `backfill-tool-names` | reads `req_body` | never sees archived rows (the archiver skips un-backfilled ones); a row archived *after* backfill is already `req_tool_names`-populated |
| `rekey` pass 1/2/3 | reads `resp_body` / `req_headers` | pass 1 needs `resp_body`: it **reports and skips** archived rows (`clens archive restore` first); pass 2 reads headers only (unaffected); pass 3's precondition is unchanged |
| `reflag` | `Content-Length` witness vs stored body length | archived rows are **reported as unverifiable** (the stored `*_len` could serve, but reflag is a one-off historical repair — not extended) |
| `reprice` | usage columns only | unaffected |
| `redact self-test` (`checkRedaction`) | reads `req_headers` | unaffected (headers stay hot) |

### C.7 Config

`HotDays int` — flag `--hot-days`, env `CLENS_HOT_DAYS`, file key `HotDays`, default **7**; `0` disables
archival (bodies stay hot forever, the pre-GI-16 behaviour). Negative is rejected. **`HotDays >
RetentionDays` when both are > 0 is rejected by `Validate`** ("nothing would ever be archived before it is
deleted"). `clens doctor` prints it and a WARN when the archive directory is unwritable.

### C.8 CLI and scheduling

- `clens archive status` — hot-window boundary, archived/unarchived row counts, archive dir size and file
  count, rows skipped for the backfill reason, and any `missing` markers.
- `clens archive run [--dry-run] [--yes]` — same gate as every writer: `--yes` to write, `--dry-run` reports.
  Runs the archiver against the DB from a second process (WAL allows it; small batches keep
  `busy_timeout(5000)` from being exhausted).
- `clens archive restore --since X --until Y [--dry-run] [--yes]` — the inverse: decompress bodies back
  into the hot rows and drop their markers. Intended with `serve` stopped (like `rekey`); warns otherwise.
- **`serve`**: one goroutine, started after the listeners are up (never on the boot path — the redaction
  self-test and retention purge already run there), runs the archiver at boot and on the existing
  24-hour ticker, after `purgeOnStartup`, then `GCArchive`. Fail-open: an error is logged, never fatal.
- Subcommand count 23 → 26 (`restart`, `reload`, `archive` with sub-verbs handled inside the one entry).

### C.9 Dashboard

Call detail shows a line when `BodiesArchived != ""`: *"bodies loaded from the archive (2026-09-20)"* or
the `missing` message. Body markers (`captureMarker`, `readPathMarker`) keep working on restored bodies —
they compare lengths against the cap and the restored blob is byte-identical. Settings tab gains a small
archive summary from `GET /api/archive` **only if** bead 10 finds it cheap; otherwise `clens archive status`
is the surface (YAGNI on a new read route).

### C.10 Risks

| Risk | Handling |
|---|---|
| **The archiver starves the single connection (GI-13 again)** | Short batches, a yield between them, `ctx` checks, and a test that runs the archiver against a store while a second goroutine times `ListEvents` and fails if any read waits longer than a stated bound |
| Data loss between the copy and the NULL | Ordering above; verify-before-commit; a fault-injection test that fails after each step and asserts every body is still recoverable |
| Orphaned sensitive bodies after `purge` | `ON DELETE CASCADE` + `GCArchive` + a dedicated test that greps the archive files' bytes for a sentinel |
| A body silently reported "not captured" when it is merely archived | `BodiesArchived` tri-state; `missing` is a distinct visible state |
| `mergeEvents` launders `capture_complete` on an archived row | rule in C.6, test with an archived `existing` and an incoming row carrying a body |
| Migration on the live 2.7 GB store | Per `CLAUDE.md` §Migrations: **back up `lens.db`, `lens.db-wal`, `lens.db-shm` before the first `serve`/`doctor` on the new binary**, state the path. The migration only creates a table and an index, so it is fast, but the rule has no size exemption. Claude does not run it against the live DB; the bead's verification uses a copy. |
| Hot file does not shrink | Documented; one-time `purge --vacuum` with `serve` stopped |
| Archive dir moved/deleted by the operator | `missing` state + `doctor`/`archive status` report it; nothing crashes |
| zstd decode bomb from a corrupt archive | decode with a max output = the recorded `*_len` (and never more than `BodyCapBytes`-scale sanity bound); a mismatch ⇒ `missing`, logged |
| Time-of-day boundary: `started_at` UTC day vs the operator's local day | file names are UTC, documented; `archive status` prints UTC days |

---

## Test strategy

| Layer | What |
|---|---|
| A — web | `assets_test.go` source-shape guards (inverted Calls-`custom` assertions; `customWindow` calls `timeWindow`, no `toISOString`); manual browser pass incl. a DST-adjacent range and `to < from` |
| B — cli/api | table-driven `runRestart` against an in-process fake server on ephemeral ports (running / not running / new exe healthy / new exe unhealthy → rollback / rollback impossible); detached-child-survives-parent via `TestMain` helper process; `/api/reload` guard tests (Origin, non-loopback, malformed body) copied from `shutdown`'s; all-or-nothing on an invalid file; `SetAccounts` under `-race` |
| C — store | round trip (bytes in == bytes out, incl. NULL vs empty); fault injection after every step; idempotent re-run; late row into an archived day; `mergeEvents` with an archived existing; purge leaves no archived byte; `PurgeOlderThan` cascade; `HotDays` boundary at exactly the cutoff; skipped-for-backfill rows; hydration of `missing`; `EXPLAIN QUERY PLAN` guards that the candidate query uses `idx_events_started_at` and never scans blobs (the same shape as GI-13's guards) |
| C — perf | archiver-vs-reader latency test (C.10), and a benchmark over a synthetic 500-row day so the batch size is chosen from a number |
| Invariants | `internal/proxy` still imports only `sink`/`config`; `internal/store` still does not import `pricing`; `go test ./...` and `go vet ./...` on Windows **and** `GOOS=linux go vet ./...` (the detach files are build-tagged) |
| Race | `go test -race` for the consumer `SetAccounts` swap and the archiver/reader test |

Not tested by tests, done by hand and stated as such: a real Claude session across a real `restart`, and
the first real archival on 2026-09-27.

## Self-review

**As a senior engineer.** The load-bearing simplification is *bodies only, side table, per-day files*: it
touches no aggregate query, needs no cross-file SQL, and gives purge a trivial deletion unit. Rejected:
(a) whole-row archival — every aggregate would fan out; (b) `ATTACH DATABASE` per day — capped at 10
attached DBs by default and it binds to the one connection the proxy is using; (c) a marker column —
see C.2; (d) a background `VACUUM`/`incremental_vacuum` — cannot be enabled on an existing DB without a
full `VACUUM`, and that stalls capture. `reload`'s live set is deliberately three fields; growing it is
where this turns into a config framework. `restart` writing a state file is the one piece of new
persistent state in B; it is a hint, never authoritative.

**As a QA engineer.** The dangerous states are the *partial* ones, so each ordering point in the archiver
and in `restart` (shutdown requested / ports free / spawned / healthy / rolled back) has a test that stops
there. Edge cases: a row with only one body; a body that is exactly empty (`[]byte{}`) vs NULL; a day with
one row; a row archived and then re-ingested by `ingest --rebuild`; `HotDays` reduced by `reload` (rows
become archivable mid-run — next tick, not immediately); an archive file present but the marker gone
(orphan → `GCArchive`, not served).

**As a security engineer.** Bodies are the asset the repo exists to protect; archival *copies* them into a
second at-rest location, so the guarantee to preserve is "purge means purge". Archive files get the same
directory as `lens.db` and the same protection — on Windows that is inherited ACL, which is not stronger
than the DB's, and this plan does not claim otherwise. Nothing new is bound to a network: `reload` is a
loopback-caller route behind `originReject`, identical in shape to `shutdown`, and is the fourth write
guard. The state file holds paths and addresses only. `restart`'s child inherits the parent's environment
including any `CLENS_*` values; none is a credential (credentials live in `internal/secret`, outside the
DB and outside `config`). zstd decode is size-bounded by the recorded length.

## Files (planned)

| Area | Files |
|---|---|
| A | `internal/web/{index.html,app.js,assets_test.go}` |
| B | `internal/cli/{restart.go,reload.go,serve.go,detach_windows.go,detach_unix.go,*_test.go}`, `internal/api/{reload.go,api.go,accounts.go}`, `internal/consumer/consumer.go`, `cmd/clens/main.go` |
| C | `internal/config/config.go`, `internal/store/{schema.sql,store.go,merge.go,archive.go,types.go,*_test.go}`, `internal/cli/{archive.go,purge.go,rekey.go,serve.go,doctor.go}`, `internal/api/api.go` (detail field), `internal/web/{app.js,assets_test.go}` |
| Docs | `README.md` (operator runbooks: restart, reload, archive, the one-time vacuum) |

## Bead outline (for Phase 3)

| # | Bead | Depends on |
|---|---|---|
| 01 | A: Calls `custom` range + flipped guards | — |
| 02 | B: `serve` writes/removes `serve.state.json`; `--log` default | — |
| 03 | B: `clens restart` (detach helpers, health, rollback) | 02 |
| 04 | B: `consumer.SetAccounts`, `POST /api/reload`, `clens reload`, accounts route applies live | 02 (boot args capture) |
| 05 | C: `HotDays` config + validation + `doctor` line | — |
| 06 | C: migration 4→5, `body_archive`, archive file format, `hydrate` on `GetEvent`/`ListEventsFull`, `BodiesArchived` | 05 |
| 07 | C: `Archiver` (batched, verified, ordered) + `GCArchive` | 06 |
| 08 | C: archive-aware writers — merge, purge cascade + GC, rekey/reflag/backfill rules | 06, 07 |
| 09 | C: `clens archive status/run/restore` + `serve` scheduling + `reload` applies `HotDays` | 04, 07, 08 |
| 10 | C: dashboard/`show` archived-state display | 06 |
| 11 | Docs: `README.md` runbooks | 03, 04, 09 |
| 12 | Integration: restart + archival end-to-end on temp dirs/ephemeral ports; live-store *copy* migration check | all |

01, 02, 05 are mutually independent and can land first.

## Context docs to refresh (running list — Phase 5.6 is the only phase that writes to `docs/context/`)

Seeded now from what Phase 1 verified; later phases append.

- `dashboard.md:109-113` — "Calls offers no `custom`" is false once bead 01 ships; rewrite (not delete) the
  two-bullet contrast. Also `:100`, the `:24` line-count table, and the Calls row at `:88`.
- `INDEX.md:33` (23-entry dispatch table), `:37` (56 test files — re-measure), `:40` (dashboard line count);
  the `decisions/` count and hint (ten → eleven).
- `cli-and-tooling.md:6,17,39,41-58` — 26 subcommands; `serve` gains the state file; `restart`/`reload`/
  `archive` rows; the `--yes`-gated writers table gains `archive run` and `archive restore` (neither
  *deletes* rows; `archive restore` rewrites bodies).
- `api-surface.md` — `/api/reload`, the write-guard count three → four, the `BodiesArchived` field on the
  detail response.
- `storage-schema.md` — `schemaVersion` 5, `body_archive`, the archive file format, the merge rule for
  archived rows, and the "hydration on `GetEvent`/`ListEventsFull`" read path.
- `data-privacy-and-compliance.md:114-132` — retention section: archives are the same sensitive class;
  `purge` reaches them; `HotDays` vs `RetentionDays`.
- `security-and-permissions.md` — `reload`'s guard (in the AuthN/AuthZ table beside `shutdown`); archive
  dir permissions and the Windows caveat.
- `build-and-run.md` — `--hot-days`/`CLENS_HOT_DAYS`; `serve.state.json` and `serve.log`.
- `architecture.md` — the archiver as a new background actor; the connection-sharing rule now names it.
- `workflows.md` — two new flows (restart; archival pass).
- `glossary.md` — hot window, archived body, `BodiesArchived`, `serve.state.json`.
- `conventions.md` — the `--yes` gate paragraph if it enumerates writers.
- `testing-and-quality.md` — size figures, new invariant rows, the fault-injection pattern.
- `decisions/011-…` — **new ADR**: bodies-only archival with a side table and per-day files; rejected:
  whole-row archival, `ATTACH`, a marker column, background `VACUUM`. Update `decisions/000-index.md`.

## Change history

### v1
Initial plan, after Phase 1 intake and four user decisions (archive scope, hot window, story shape,
reload scope).
