# GI#16: Calls custom range, `restart`/`reload`, and body archival

**Version:** 12
**Status:** converged — cross-review closed at round 11 (rounds 10 and 11 both clean; v12)
**Issue:** [GI#16](https://github.com/abhisheksarkar30/claude-lens/issues/16) (anchor). Workstreams B and C
have no issue of their own; the user chose one story, one plan, one PR (2026-09-24).
**Branch:** `GI-16-calls-range-restart-archival`

## Why one story, four workstreams

| | Workstream | Source of the work | Independent of |
|---|---|---|---|
| **A** | Calls tab custom `from`–`to` range | issue #16 | B, C, D |
| **B** | `clens restart` + a small live `clens reload` | `br-GI-13-09` explicitly deferred it ("this bead stops at `shutdown` and adds no reload (YAGNI)") | A, C, D |
| **C** | Body archival: hot window + on-demand load | the live store is **2.7 GB** and growing ~0.7 GB/day | A, B, D |
| **D** | Proxy pass-through conformance test | the auto-mode classifier notice named `127.0.0.1:8797` as incompatible; D proves the notice misattributes | A, B, C |

The beads are grouped so each workstream can be reverted alone. B and C meet in exactly one place:
`serve` schedules the archiver and `reload` changes its window live (bead 04 ↔ bead 09).

**D is test-only and behaves differently from the other three.** It changes no proxy behaviour, because
the diagnosis (below, and in Established facts) found no pass-through defect to change. It adds one
regression test that pins the invariant clens already upholds, so the next such notice can be answered
from a green test rather than from re-reading the hot path. If it turns out to be unwanted, drop bead 13
and the story is unchanged — no other bead references it.

## Decisions taken with the user (2026-09-24)

1. **Archive bodies, not rows.** Whole-row archival would force every aggregate (`/api/stats`,
   sessions, quota, reconcile, warnings) to fan out across files or silently lose history.
2. **Hot window: 7 days, configurable** (`--hot-days`).
3. **One story** — three workstreams at intake; workstream D was added later, see decision 5.
4. **Restart plus a small live reload**, not restart alone.
5. **A fourth workstream, D** (2026-09-24, after the auto-mode notice): the conformance test only —
   not a proxy change. See Established facts for why the notice is not clens's to fix.

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
- **The proxy rewrites nothing.** `Rewrite` only calls `SetURL`, never touching headers or the body
  ([internal/proxy/proxy.go:42-44](../../internal/proxy/proxy.go#L42)); the request body is wrapped in
  `io.TeeReader` before forwarding ([proxy.go:128](../../internal/proxy/proxy.go#L128)) and so is the
  response body ([proxy.go:92](../../internal/proxy/proxy.go#L92)); `ModifyResponse` only wraps the body
  and edits neither headers nor status. The hot path never parses JSON, so an unrecognized request or
  response field cannot be dropped and a tool-use ID cannot be rewritten. Header mutation is limited to
  Go's `ReverseProxy` removing hop-by-hop headers, which RFC 9110 requires.
- **The auto-mode classifier notice misattributes to clens.** Verified 2026-09-24 against
  `~/.clens/config.toml` (`UpstreamURL = "https://api.deepseek.com/anthropic"`) and the session env
  (`ANTHROPIC_BASE_URL=http://127.0.0.1:8797`, every model alias mapped to `deepseek-flash`). Anthropic's
  server-side auto-mode checks are an *upstream* feature; DeepSeek's Anthropic-compatible surface will
  never perform them, so no clens change can make that session eligible. The notice named the gateway by
  base-URL heuristic. Captured traffic agrees: no request body carries a literal `"safeguards"` key and
  no response carries `safeguard_results`. The documented opt-out is
  `CLAUDE_CODE_AUTO_MODE_SERVER=0`; the running client (VS Code ext **2.1.280**) is new enough that
  "upgrade Claude Code" is not the answer.
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
| `internal/web/index.html` | add `<option value="custom">custom</option>` to `c-window-gran`; add `<label>from <input id="c-from" type="datetime-local"></label>` and the `to` twin — the **labels** are what `mountWindowPicker` toggles (`app.js:352`), so do **not** hard-code `hidden` on `c-from`/`c-to`: an `hidden` set on the input itself is permanent (un-hiding the parent label never clears a child's own `hidden`). The Stats pair is the model — its labels carry no `hidden` in HTML (`index.html:115-116`) and are hidden at mount. Rewrite the comment above the select that says there is deliberately no `custom` |
| `internal/web/app.js` | `mountWindowPicker($('c-window-gran'), …, [c-from label, c-to label], …)` — the `freeLabels` parameter already exists and already hides/reveals on `custom`; `callFilter()` gains a `custom` branch calling a new `customWindow(fromVal, toVal)` helper that calls `timeWindow` twice; `change` listeners on the two inputs reload; offset reset to 0 on any window change (already true for the picker — verify for the new inputs) |
| `internal/web/assets_test.go` | invert the Calls-has-no-`custom` assertion at `:654-656` in `TestAssetsThePickerMountsBothTabs`; **rewrite the two prose blocks that assert the opposite of what the edit makes true** — the header comment "a `custom` option on Calls would be a control offering a capability that tab does not have" (`:625-630`) and the defaults comment calling Calls' omission of `custom` "the dead-option misdescription the Calls mount already avoids" (`:661-671`), the same rewrite the plan already makes to `index.html`; leave the default checks at `:672-677` (no `selected`; an empty-valued option present) unchanged — the default remains `any time`; add: `c-from`/`c-to` mounted as `datetime-local`, `callFilter` reaches `customWindow`, `customWindow` calls `timeWindow(` and contains no `toISOString` |

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
- **`args` is `os.Args[1:]`, so it already begins with the subcommand** (`["serve", "…"]`); `restart`
  relaunches as `exe <args…>` and must never prepend a second `serve` (`clens.exe serve serve …` is what
  `main.go:63` would then read as its subcommand). Every consumer reads its own slice:
  - **spawn** reads `exe` + the whole recorded `args`;
  - **`reload`** reads the *post-subcommand* slice — the same `os.Args[2:]` this `serve` received
    (§B.4) — because `config.Load` → `applyFlags` is a plain `flag.Parse`
    (`internal/config/config.go:238-252`) that **stops at the first non-flag argument**. Handed
    `["serve","--replay"]` it would drop `--replay`; a captured `--retention-days 30` or `--db-path X`
    would be silently lost too, so reload would diff against the file/env/default and could conclude
    `RetentionDays` changed `30 → 0` and try to apply `0`.
- Removed on graceful exit. **A leftover file is a hint, never the truth:** liveness is decided by
  dialing the dashboard's `/api/health`, never by the file or the pid.
- Contains no secret (paths and addresses only). Written `0600` (a no-op on Windows; same directory as the
  DB, so no weaker than the DB itself).
- `log_path` is where `restart`'s child writes; default `<dir of DBPath>/serve.log` (the file already
  sitting next to the live DB).

### B.3 `clens restart [--exe PATH] [--timeout 30s]`

1. Strip `restart`'s own flags first — `--exe` and `--timeout` via `takeFlag`
   ([accounts.go:101](../../internal/cli/accounts.go#L101)), exactly as `purge` does
   ([purge.go:31-35](../../internal/cli/purge.go#L31)) — **then** `config.Load(rest)`. The order is
   load-bearing: `config.Load` → `applyFlags` ends in `flag.NewFlagSet(…, flag.ContinueOnError).Parse(args)`
   over a closed flag set that has no `--exe`/`--timeout` ([config.go:238-252](../../internal/config/config.go#L238)),
   and an unknown flag is a returned error — so `config.Load(["--exe", …])` would fail and the feature's
   headline flag would be dead on arrival. Read the state file (missing ⇒ no exe/args to reuse; `--exe` then
   required or `os.Executable()` is used with `serve` and forwarded flags), and **retain the exe and args it
   read** for the step-6 rollback — the state file is deleted on the graceful exit step 3 drives (B.2), so it
   cannot be re-read then.
2. **Running?** `GET /api/health` on the dialable dashboard address (reuse `dialableDashboardAddr`).
   Not running ⇒ skip to step 4 and say "was not running".
3. `POST /api/shutdown` (reuse `runShutdown`'s round trip; do not duplicate it), then wait until **both**
   the dashboard and proxy ports refuse a dial (bounded by `--timeout`). This is what proves the drain
   finished and the store is closed.
4. Spawn `exe <args…>` **detached** (the recorded `args` already begin with `serve`; never add a second),
   stdout+stderr appended to `log_path`.
   - Windows: `CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS`; Unix: `Setsid`. Two small build-tagged
     files (`detach_windows.go`, `detach_unix.go`).
5. Poll `/api/health` until 200 or `--timeout`. Report pid, exe, log path, and **the measured gap**
   (time from shutdown request to healthy).
6. **Rollback.** If `--exe` was given and the new process does not become healthy: kill the child, respawn
   the *previous* exe **from the exe/args step 1 retained** (not by re-reading the state file — B.2 removes it
   on the graceful exit step 3 drove), wait for health, print the log tail, and exit **non-zero** naming which
   binary is now serving. Without a rollback a bad build leaves the operator's session with no proxy at all.
   If no previous exe is known there is nothing to fall back to and the message says so.

`--exe` is the answer to the Windows locked-binary problem: build to any other path while the old one runs,
then `clens restart --exe D:\build\clens.exe`. Replacing the *installed* binary stays the operator's call.

### B.4 `clens reload`

`POST /api/reload` — same guard as `shutdown` (`originReject` **and** a loopback caller). Handler calls an
injected `reloadFunc`, wired in `serve.go`:

1. `config.Load(bootArgs)` again (flag > env > `config.toml` > default), then `Validate()`. `bootArgs` is
   the **post-subcommand** slice `serve` itself received (`os.Args[2:]`), **not** the state file's `args`
   — see B.2 for why `flag.Parse` makes the difference load-bearing.
2. Diff against the boot `cfg` field-by-field.
3. **Live-apply set** — exactly these, no others: `Accounts`, `RetentionDays`, `HotDays`. The set is the
   *contract*; it is implemented in two beads, because `HotDays` does not exist until bead 05 defines it,
   so a `{02}`-only bead 04 cannot implement that arm.
   - `Accounts` needs a mutex-guarded `consumer.SetAccounts` (the consumer goroutine reads it in
     `resolveAccount`, consumer.go:518).
   - `RetentionDays`/`HotDays` move into one small `atomic`-guarded struct the purge/archive ticker reads.
   - **Bead 04** ships the mechanism plus the `Accounts` and `RetentionDays` arms; **bead 09** adds the
     `HotDays` arm (it owns `HotDays` and gains 05 as an explicit dependency).
4. Everything else that differs is reported under `restart_required`. Prices are already hot-reloaded and
   unaffected.
5. Response: `{"applied":[…],"restart_required":[…],"unchanged":true|false}`. An invalid file changes
   nothing and returns 400 with the validation error — **reload is all-or-nothing**.

`clens reload` prints that report. The dashboard's `POST /api/accounts` currently *validates only* and
tells the user to restart (`api/accounts.go:87`, `serve.go:391`, both marked `ponytail:`); once
`SetAccounts` exists that route calls the same apply path and the message changes. That removes two
`ponytail:` ceilings, and `api/accounts.go` is the only pre-existing *route* B edits — B's other touched
files are new, or existing files B wires (`serve.go`, `consumer.go`, `main.go`), which live in B's own
Files row.

### B.5 Risks

| Risk | Handling |
|---|---|
| **Testing restart against the live proxy kills the operator's Claude session** (handover trap 1) | Every test uses ephemeral ports and a temp DB. No bead's verification runs `restart`/`shutdown` against `127.0.0.1:8797`. Manual live verification is the user's, with the rollback path as the safety net. |
| Child inherits the console and dies with it | Detached process flags + a test that the child survives its parent's exit (helper-process pattern via `TestMain`) |
| `restart` forwards stale args | State file is written by the *running* serve, so args are exactly what it was started with; a `--exe` build that rejects a flag fails health and rolls back |
| Two restarts race | Second sees the dashboard down/unhealthy mid-way and waits for the ports; if both spawn, the loser fails to bind and exits non-zero — fail-closed on the port, never two proxies |
| `reload` half-applies | Validate before apply; apply the three fields under one lock; tests assert all-or-nothing on a bad file |
| New write route widens the attack surface | Same two guards as `shutdown`; `/api/reload` is a **second route under the existing loopback-caller guard** — the guard *mechanisms* stay three, and `api-surface.md`'s Writes table gains a row (six → seven) |
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
archived bodies — and purge's dry-run byte estimate must count them too (C.6).

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
    archived_at INTEGER NOT NULL,   -- unix ns
    -- Mirror of the day file's row mask (the `bodies` table below), same
    -- bitmask (1 req_body, 2 resp_body, 4 transcript_content). Step 4 re-reads
    -- that file row and ORs it in (§C.4), so this too is monotone: it can only
    -- lag the file -- under-claiming, which leaves a column hot and visible --
    -- never over-claim a column the file lacks. The merge loader reads it (it
    -- never opens the day file) so a column the archive does not hold is still
    -- backfillable: an archived proxy-only row (mask without bit 4) must still
    -- accept a later transcript backfill.
    body_mask   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_body_archive_day ON body_archive(day);
```

`ON DELETE CASCADE` matters: `PurgeOlderThan` deleting an event removes its marker in the same statement.
`foreign_keys(ON)` is already in the DSN.

**`body_mask` is added to migration `4 → 5` now, and this is deliberately not deferred.** That migration
has not shipped yet (`schemaVersion` is still 4), so the column costs nothing today. Adding it later means a
*second* migration against the live 2.8 GB store, on the very table whose whole design premise (C.2) is that
it stays byte-for-byte untouched. The day file's own `bodies` row carries the same-shaped mask (below), and
step 4 **ORs that file row's mask** into the marker inside its hot transaction (§C.4), so the marker can lag
the file but can never claim a column the file lacks; a marker that could out-run the file would recreate
the C.6 bug with extra steps. Bead 06 owns the column.

**Archive file** `archive/bodies-YYYY-MM-DD.db`:

```sql
CREATE TABLE bodies (
    event_id INTEGER PRIMARY KEY,
    -- Which of the three blobs this day row holds, the same bitmask shape as the hot marker
    -- (1 req_body, 2 resp_body, 4 transcript_content). Monotone: an upsert only ORs bits in.
    body_mask INTEGER NOT NULL DEFAULT 0,
    -- Codec, original length and blob are written and COALESCE-preserved together: all three are NULL
    -- when this row does not hold that blob, so a narrower second archiver cannot drop a wider one's column.
    req_codec TEXT, resp_codec TEXT, tc_codec TEXT,     -- each 'zstd' | 'raw'
    req_len INTEGER, resp_len INTEGER, tc_len INTEGER,  -- ORIGINAL lengths
    req_body BLOB, resp_body BLOB, transcript_content BLOB
);
```

- Each blob is compressed independently (`klauspost/compress/zstd`, already a dependency), so one body is
  readable without decompressing its neighbours. NULL stays NULL (a body that was never captured is not
  an empty archived one).
- **The codec is chosen per blob, not per row.** A single row-level `codec` cannot express a per-blob
  decision: a row can hold a 500 KB compressible request beside a 1 KB incompressible response, and one
  row-level tag would force the wrong choice for one of them — a reader that decompressed a raw blob as
  zstd would error and hydrate would report `"missing"` (C.10). So each blob carries its own codec
  (`req_codec`/`resp_codec`/`tc_codec`), decided per blob ("raw if compression would not shrink it"), and
  the stored codec **always matches the stored bytes** — that agreement is what keeps hydrate from
  manufacturing a bogus `"missing"`. A blob this row does **not** hold writes NULL for the blob, its codec
  and its `*_len` **together**, so the triple stays paired and the monotone upsert's `COALESCE` can preserve
  a wider writer's copy.
- The stored `*_len` are the **uncompressed** lengths — they are what the verification step and the
  `Completeness`/cap comparisons need, and they let a reader confirm a round trip.
- One file per UTC day: the day in `body_archive.day` *is* the file name, so lookup needs no index file.
  A late row for an already-archived day **upserts** into that day's file, and the upsert is **monotone in
  coverage** — `… ON CONFLICT(event_id) DO UPDATE SET body_mask = bodies.body_mask | excluded.body_mask`,
  and `COALESCE(excluded.<col>, bodies.<col>)` for each blob, its `*_len` and its codec. A column is only
  ever **added**, never dropped, and the codec stays paired with the blob it describes. A plain
  `INSERT OR REPLACE` is the **wrong** conflict resolution and is **not** idempotent across two writers:
  `REPLACE` is delete-then-insert, so an archiver whose step-1 read predates a `mergeEvents` backfill (a
  *narrower* mask) would overwrite the day row and drop the transcript a *wider* archiver already wrote —
  the F5.1 loss. `REPLACE` is idempotent only when the *same* writer re-writes the *same* row.
- Opened read-only on demand through a tiny LRU (cap 4 handles); the writer opens read-write per batch.
- Directory `0700`, files `0600`. **These files are exactly as sensitive as `lens.db` and exactly as
  protected — no better on Windows** (same inherited ACL as the DB directory). Stated, not solved.

### C.4 The archiver (`internal/store`, `Archiver`)

**The invariant, stated once — every mechanism below is a corollary of this sentence.** The day-file row is
the authority for what the archive holds; its coverage is monotone (columns are only ever added). The marker
mirrors it and is itself monotone. Step 4 NULLs exactly the columns the file's mask covers. `hydrate` reads
the file row. `restore` (§C.8) is step 4's inverse and maintains the same fact: it writes the columns back and
drops the marker in one hot transaction per row, and only then removes the day-file row. Therefore no body can
exist in neither place, and any stale artifact can only *under*-claim — leaving a column hot and visible —
never over-claim, which is the only direction that produces silent loss. **This is a rule over every path
that touches a marker, not a list of examples.** What sorts the paths into those needing a proof and those
already covered is ***move* versus *discard***: a path that *moves* a body — into the archive (step 4), back
out (`restore`), or into a taker (rekey pass 1's absorb) — must actively maintain the invariant, because after
the move the body and the marker sit in different places and can drift apart.
The archiver's steps and `restore` derive from this sentence; rekey pass 1's
absorb path is §C.6's one **stated exception** (its delete *moves* the absorbed row into the taker rather than
discarding it), and stays safe only by skipping **every** marker-bearing row, so it never touches a marker at
all. A path that instead *discards* the row — §C.6's three CASCADE-deleting writers — leaves no live body to
preserve, so §C.6's vacuity argument already covers it and it needs no proof here. Any new path must likewise
either derive from this sentence or be named here as a stated exception; a change that can do neither is the
signal it needs a new proof, not a new paragraph.

Candidate rows: `started_at < now − HotDays·24h`, at least one body column non-NULL, and **not** already in
`body_archive`. Selected with `typeof(col) != 'null'` (reads the record header, not the blob) and
`NOT EXISTS (SELECT 1 FROM body_archive …)`.

One batch (≤ 20 rows **or** ≤ 8 MiB of raw body, whichever is first), all within a single UTC day:

1. **Read** the batch's bodies from the hot DB (short hold of the single connection).
2. **Compress and write** into the day file in one archive transaction, with `orig` lengths. The
   `body_mask` is derived here, inside this transaction, from the columns actually written non-NULL.
3. **Verify inside the same archive transaction, before commit:** `count(*)` and the three length sums
   read back from the file equal what was written. Commit (fsync via SQLite).
4. **Only then** one hot transaction per batch, and it **re-reads the day file row's `body_mask` inside
   that transaction** — the file row is the authority and the marker mirrors it, never the reverse:
   - re-read `body_mask` from this day file's `bodies` row for the event;
   - `INSERT INTO body_archive (event_id, day, archived_at, body_mask) VALUES (?, ?, ?, :mask)
     ON CONFLICT(event_id) DO UPDATE SET body_mask = body_archive.body_mask | excluded.body_mask` — `:mask`
     is the **re-read** value and the conflict arm **ORs** it into the row, mirroring §C.3's file write. The
     union is what makes the marker monotone: a second archiver whose re-read is *staler* can only add bits,
     never drop the ones a wider archiver already committed. The old plain `SET body_mask =
     excluded.body_mask` could regress the marker below a set step 4 itself NULLed; `INSERT OR IGNORE`
     before it (which silently kept whichever archiver committed first) is gone;
   - NULL **only the columns `:mask` holds** — `UPDATE events SET req_body = CASE WHEN :mask & 1 THEN
     NULL ELSE req_body END, resp_body = CASE WHEN :mask & 2 THEN NULL ELSE resp_body END,
     transcript_content = CASE WHEN :mask & 4 THEN NULL ELSE transcript_content END WHERE id=?` — the
     **same** `:mask`.
   The re-read is against the **day file, a separate SQLite database**; the hot store's write lock never
   ordered it, and no lock is needed — the marker's OR-upsert above is what makes a stale read safe (the
   monotone-coverage argument below). A blanket three-column `NULL` would instead clear a body the archive
   does **not** hold: a `mergeEvents` backfill landing between step 1's read and this write writes a body
   the step-2 mask does not list, and the crash-ordering argument below covers a crash, not a concurrent
   writer.
5. Sleep a short yield (default 50 ms) so the consumer flush and dashboard reads interleave.

**Crash safety is ordering, not a transaction spanning two files:** crash after 3 ⇒ duplicated bodies
(archive has them, hot still has them, no marker) — harmless, the next run redoes the batch idempotently, and
it is exactly the state §C.6's GC predicate must spare: the bodies are still hot, so the true-orphan
predicate does not match and no copy is collected under a concurrent `GCArchive`. Crash after 4's commit ⇒
done. There is no state in which a body exists in neither place — step 4's NULL is mask-keyed, so a column
the archive does not hold is never cleared (it stays hot and is never re-archived, the corollary below), and
the copy the archive *does* hold is not removable while any hot body remains (the GC predicate's second
clause, §C.6). The file's mask (step 2), the marker mask step 4 ORs from the file, the columns step 4 NULLs,
the columns `hydrate` restores (§C.5), the columns `restore` writes back and the markers it drops in one hot
transaction per row (§C.8) and the columns `GCArchive` refuses to collect (§C.6) are all the one
invariant stated at the head of this section: **the archive holds exactly the columns the file row's mask
names, hot or archived, and nothing else decides either way.**

**Two archivers, monotone coverage — why the marker can never out-run the file.** §C.8 keeps `clens archive
run` as a second archiver against the same DB (§C.10), so two writers can hold genuinely different masks for
one event whenever a `mergeEvents` backfill lands between their step-1 reads — the plan already relies on
such a merge landing in that window. This is the head-of-section invariant on its two-writer corner. The
file's coverage is **monotone** (its upsert only ORs mask bits and `COALESCE`s blobs in, so a day row never
loses a column its mask names), and step 4 makes the marker **monotone too** by OR-ing the re-read file mask
into it. So a committed marker is a union of *past reads of a monotone file*: it can lag the file, never run
ahead of it. **The re-read is against the day file — a separate SQLite database that the hot store's write
lock never ordered at all** — so the ordering is not assumed to exist: it is bought by the marker's own
monotonicity. A lagging marker is safe — it only *under*-claims, so a column it does not yet claim stays hot
and visible, and a later archiver's OR catches it up. The marker running *ahead* of the file would be the one
silent loss this workstream exists to prevent (a bit set for a column the file no longer holds, which
`hydrate` reports as `"missing"`) — and OR-ing a past read of a monotone file cannot produce it. This is
exactly why both writes are monotone upserts and not the old `INSERT OR REPLACE` (file) / plain
`SET body_mask = excluded.body_mask` (marker).

**Rejected: ordering the re-read by taking the hot write lock first.** The alternative fix — `_txlock=immediate`,
or an explicit `BEGIN IMMEDIATE`, so the transaction acquires the store's write lock *before* the re-read and
the old "lock across the re-read" claim would become true — is **not taken**. It would change the SQLite
transaction mode for **every** `BeginTx` site in the process (eleven today, all `BeginTx(ctx, nil)`, over a
DSN with no `_txlock`, [store.go:68-71,304,330,…]), i.e. the single-connection serialization and the
`busy_timeout(5000)` behaviour everything else depends on, to buy an ordering the marker's own monotonicity
already gives for free — and it would not even order the thing that matters, since the re-read is on the
*day file*, a different database the hot lock cannot see.

**Corollary (a column that arrives after the row was archived):** because step 4 only NULLs masked columns,
a column the mask does not hold that lands *after* archival — e.g. a late transcript under C.6's mask-keyed
merge (`clens ingest` run after the row was archived) — stays in the hot row and is **never re-archived**
(the candidate query excludes already-marked rows), so the hot file plateaus near `HotDays × daily volume`
*approximately* rather than exactly, bounded by how often a body lands post-archival (rare: the collector
normally writes a transcript well inside the 7-day window). No re-archival machinery is added for it — the
no-`VACUUM` / no-background-compaction argument below applies, and a later round can argue for building it then.

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

- **The gate is mask-driven, and the mask comes from the file row, not the marker.** For a row that has a
  `body_archive` marker, group by `day`, open each day file once, and read the day row's `body_mask` (the
  authority, §C.4) — then fill **exactly the columns that mask holds, and only those that are empty in the
  hot row** (decompressing); a column the mask does not hold, or that is already populated hot, is left
  untouched. The marker supplies only the *day* (which file to open); because the marker is a lagging
  mirror, resolving the columns from the file row takes the marker off the read-correctness path entirely —
  a marker that is behind cannot strand a column `hydrate` is already positioned to restore. F2.4's
  mask-keyed merge can leave a row with `transcript_content` hot while `req_body`/`resp_body` are NULL and
  only in the archive; a "three bodies empty" gate would never open the file for it, the archive would never
  be read, and the archived request/response would be reported as **never captured** with no
  archived/missing indication — the exact silent state C.9/C.10 exist to prevent.
- New read-only field `Event.BodiesArchived string`, describing **the row, not a single column**: `""`
  (hot / never archived), `"restored"` (**at least one** body was loaded from the archive — **including a
  partially restored row**, e.g. a hot transcript beside an archive-restored req/resp — so a restored row
  is never `""`), `"missing"` (the marker's day file or row is absent or undecodable). It is a
  **hydration outcome** and is **never written** by `InsertEvent`.
- `"missing"` is what keeps an operator who deleted or moved `archive/` from being told a call had no
  body: the dashboard and `show` say *"archived — archive file for 2026-09-20 not found"*, not
  *"not captured"*.
- `ListEvents`, `SessionEvents*`, every `Stats*` and `Session*` query are **unchanged** — they never
  select a body.
- The **which-columns-the-archive-holds** fact has two consumers now that the gate is mask-driven, and
  each reads a mask — from a *different* row. **The write path** gets it as a **separate field**,
  `Event.ArchivedBodyMask uint8`, set by the raw tx loader `getEventByRequestIDTx`
  ([internal/store/merge.go:82](../../internal/store/merge.go#L82)) from `body_archive.body_mask` (the
  marker). That path does **not** hydrate, and `BodiesArchived` cannot express "marker present, bodies
  intentionally empty" (`"missing"` would be a lie — the archive file may still exist). **The read path**
  (`hydrate`) reads `body_mask` from the **day file row it opens to fill the columns** (above) — not from
  the marker, and not from `Event.ArchivedBodyMask`. **One field per consumer:** `hydrate` sets
  `BodiesArchived` (the read paths); the merge loader sets `ArchivedBodyMask` (the write path). A zero
  masked `ArchivedBodyMask` means "no marker" (the archiver only archives a row with ≥1 body, so a marker
  always has a non-zero mask).
  **The `json:"-"` carve-out survives the new gate unchanged** — re-checked, not assumed: neither consumer
  reads the mask off `Event` (the write path sets a separate field from the marker; the read path reads the
  day file row), so the mask still never needs to reach the wire. `ArchivedBodyMask` is an internal
  load-path flag, **not wire data**: `Event`'s wire keys are its Go field names
  ([types.go:100-104](../../internal/store/types.go#L100)), so it carries `json:"-"`, and it is the only new
  field on `Event` that must. `BodiesArchived` is the read-side key and does reach the wire (below).
- **`ListEventsFull` gains a no-hydrate mode — a flag, not a sibling method.** The flag is a field on
  `store.EventFilter` (`SkipHydrate bool`, default false), read by `ListEventsFull`. Carrying it on the
  filter keeps `ListEventsFull`'s signature — and so the `api.Store` interface at
  [api.go:55](../../internal/api/api.go#L55) — **unchanged**, which is why this is a flag and not a second
  method: the only consumer is `checkRedaction`, and it holds the concrete `*store.Store`, so the API's read
  slice never needs the mode. The boot
  redaction self-test needs the full projection for its headers but must **never** open archive files:
  `checkRedaction` ([internal/cli/serve.go:347](../../internal/cli/serve.go#L347)) runs on the boot path
  ([serve.go:75](../../internal/cli/serve.go#L75)) via `st.ListEventsFull(… Limit: redactScanLimit)`
  ([serve.go:352](../../internal/cli/serve.go#L352), `redactScanLimit = 500`); a hydrating call would
  zstd-decompress up to 500 archived bodies on the single connection *before the listeners come up*.
  `checkRedaction` sets the flag. (`ls`'s table path already uses the summary `ListEvents`, not
  `ListEventsFull`, so it is unaffected; `export`, `ls --json` and the replay poll want bodies and keep
  hydrating.)
- Callers needing no *behavioural* change, but one *additive contract* change: `api` detail, `replay`,
  `cli show`, `export`, `ls --json`, the replay poll all keep working. `api` detail is the cleanest case:
  `/api/requests/{id}` returns `eventDetail`, which **embeds `*store.Event`**
  ([api.go:394-395](../../internal/api/api.go#L394)), so `BodiesArchived` reaches the detail JSON with **no
  `internal/api/api.go` edit at all** — that file is not in C's Files row. `export` and `ls --json` encode whole
  `*store.Event` values ([export.go:90](../../internal/cli/export.go#L90),
  [ls.go:57](../../internal/cli/ls.go#L57)), so the one new wire key (`BodiesArchived`) appears in their JSON.
  `ArchivedBodyMask` is tagged `json:"-"` (above), so it never reaches the wire and cannot contradict
  `BodiesArchived` there — an archived row serializes as `"BodiesArchived":"restored"`, never with a
  second, always-false key beside it. Additive, but a machine-readable-contract change — named in the
  refresh list and bead 10. A **large export** over an archived range opens one file per day, not one per row.

### C.6 Writers that must learn about archived rows

| Writer | Interaction | Rule |
|---|---|---|
| `mergeEvents` | an archived `existing` has empty hot bodies | Treat a body column as **present** (no backfill from `incoming`, and counted in `bodySides`) **only if the marker's `body_mask` says the archive holds it** — never blanket "all three". The store method that loads `existing` sets `ArchivedBodyMask` (C.5's marker signal — **not** `BodiesArchived`, which is a hydration outcome that load path never computes); `mergeEvents` stays a pure function of its arguments, reading the mask. So an archived proxy-only row (no `transcript_content`) still backfills the transcript from an incoming JSONL row — e.g. `clens ingest` (or `ingest --rebuild`) run after the row was archived. A blanket rule would silently drop that transcript and store it nowhere. **The read path agrees with it:** `hydrate` restores exactly the columns the *file's* mask holds (C.5), and since the marker never exceeds the file (§C.4), the columns the marker claims are a subset of those the file holds — so a column the archive does not hold is backfillable *and* never restored, with no column both claimed-absent and unrestorable. **The merge never opens the day file** — it reads the marker (`body_archive.body_mask`). Per §C.4 that marker is a *lagging* mirror of the file (monotone, OR-ing only): it can only *under*-claim relative to the file, and an under-claim is harmless here — it merely lets a column the archive already holds be re-backfilled (the hot row gets a duplicate, and the archive already has it), never drops an incoming body. The one dangerous direction — an over-claim that treats a body as present and so skips the backfill of a column the archive does *not* hold — is unreachable, because the marker is a union of past reads of a monotone file and so never exceeds the file's mask. Reading the marker rather than the file is unchanged by F5.1. |
| `PurgeOlderThan` / `PurgeUnpriced` / `DeleteJSONLKeyedEvents` | delete `events` | `ON DELETE CASCADE` removes markers; then `Store.GCArchive` deletes the day file's `bodies` rows that are **true orphans** and removes day files that become empty. **A true orphan is: no `body_archive` marker for that `event_id` AND the hot row's `req_body`, `resp_body` and `transcript_content` are all NULL.** A purged event satisfies this vacuously — CASCADE took the marker and the `events` row is gone, so there is no hot body to preserve. **These three writers *discard* the row, and the vacuity holds for all three — but rekey pass 1's collision delete is the one exception, named here on purpose and not overlooked:** it is a `DELETE FROM events` too ([store.go:1788](../../internal/store/store.go#L1788)), yet it **absorbs** the row rather than discarding it — the absorbed row's data is meant to survive in the taker ([store.go:1778](../../internal/store/store.go#L1778)) — and the merge that carries it backfills only the absorbed row's **hot** columns ([merge.go:388](../../internal/store/merge.go#L388), `:405`, `:443`), never opening the day file. So the vacuity argument above does not reach it: a marker-bearing absorbed row would lose its archived bodies to the CASCADE, with no copy left anywhere. A delete whose semantics are *move* rather than *discard* is exactly where the cascade rule stops applying — which is why rekey pass 1 must skip **every** marker-bearing row (its row below), the third path §C.4's head sentence names. Every *reachable* pre-marker state fails it: the only way to be at "no marker yet" for a live row is a crash after step 3, or a second `archive run` between its step 3 and its step 4, and in both the batch's hot bodies are still present — a body that is still hot is not an orphan. **The second clause is load-bearing, not a heuristic:** "no marker **and** the hot bodies already NULLed" is exactly the *body-in-neither-place* state §C.4's head invariant forbids, and **every** mechanism that drops a marker while clearing the columns it owns makes it unreachable for a live row — step 4's per-batch hot transaction (§C.4) **and** `restore`'s per-row hot transaction (§C.8) both couple the marker change and the column NULLs/writes in one commit, so the two facts cannot be observed together. This predicate is a corollary of that one head sentence, not a second proof; a mechanism that could drop a marker *without* the bodies back — the naive `restore` F7.1 named — would falsify it, which is why §C.8 fixes restore's ordering. Drop the clause and the predicate matches the legal intermediate state above, so `GCArchive` can delete a day file's only copy of a body whose event is still live mid-batch (§C.10). **A purge that leaves an archived body behind is a privacy defect** and has its own test. Separately, `Store.PurgeableBytes` ([store.go:921-931](../../internal/store/store.go#L921), feeding `clens purge --dry-run`) **must gain its archived-body term**: it sums only `LENGTH(req_body)+LENGTH(resp_body)` today, so once those columns are NULL a dry run over a fully-archived range would report "0 B" while `--yes` deletes gigabytes. **The archive directory reaches the `Store` the same way it reaches `serve` and `archive run`:** `Store` gains an archive-dir field set at `Open` from `filepath.Dir(dbPath)/archive` ([store.go:62-123](../../internal/store/store.go#L62) — the one construction point every command already funnels through), so `GCArchive` (bead 07) and `PurgeableBytes` (bead 08) read the same path rather than each re-deriving it; the field lands in **bead 06**, the first bead that opens a day file (`hydrate`, C.5). The archived term groups the events below cutoff by `body_archive.day`, opens each day file once, and adds the matching `bodies` row's `req_len + resp_len` — the same two columns the hot term sums (parity; `transcript_content` is already outside `PurgeableBytes`'s scope today), now read from the day row rather than the (NULLed) hot columns. **A day file that is missing or unreadable is skipped and counted, never fatal** — so `clens purge --dry-run` ([purge.go:96](../../internal/cli/purge.go#L96)) cannot error over an operator-moved archive, matching §C.10's "nothing crashes" row. |
| `backfill-tool-names` | reads `req_body` | never sees archived rows (the archiver skips un-backfilled ones); a row archived *after* backfill is already `req_tool_names`-populated |
| `rekey` pass 1/2/3 | reads `resp_body` / `req_headers` | pass 1 **reports and skips every row with a `body_archive` marker** — **marker presence is the criterion, not `resp_body` availability**: a marker-bearing row's `resp_body` may be hot (§C.4's corollary), so a `resp_body`-driven criterion would *not* skip it, and on a collision that row is absorbed-and-deleted (the one exception in the cascade row above), losing the archive-only bodies. **The skip is surfaced, not silent:** `RekeyPass1Report` gains a third count for marker-bearing rows skipped (e.g. `Skipped`), rendered by both existing print sites ([rekey.go:74-76](../../internal/cli/rekey.go#L74) dry-run, `:81` live) alongside `ReKeyed`/`Synthetic` — **with the remedy** `clens archive restore` first, so the operator who is told to run `restore` is first told that rows were skipped and why. pass 2 reads headers only (unaffected); pass 3's precondition is unchanged |
| `reflag` | `Content-Length` witness vs stored body length | archived rows are **excluded from `reflagScope`**, and the excluded count is **reported** in the reflag output with the remedy (`clens archive restore` first) — the same treatment rekey pass 1 gets above, so the two stay consistent. Why exclusion rather than a verdict: an archived row's `req_body`/`resp_body` are NULL, so it can never reach **Flipped** (correct) — but an archived `capture_complete=1` row carrying a `stream_incomplete` warning would otherwise land in **Residual**, the bucket documented as "capture_complete=1, warned, and no provable witness", i.e. a row the merge is *suspected* of laundering. Reporting an archived row there would mislabel a merely-archived body as a laundering suspect, so it leaves the scope entirely and the operator is told how to bring it back. **This narrows the scope; it does not add a fourth `ReflagCounts` bucket** — the three buckets still partition the narrowed scope, and the excluded count is reported separately (outside the scope, not a bucket within it). |
| `reprice` | usage columns only | unaffected |
| `redact self-test` (`checkRedaction`) | reads `req_headers` via `ListEventsFull` | the data is unaffected (headers stay hot) — but the call must use `ListEventsFull`'s **no-hydrate** mode (C.5): it runs on the boot path and would otherwise zstd-decompress archived bodies for rows whose headers it already has |

**`req_tool_names` after archival — stated semantics.** The column's contract reads "`req_tool_names` is
NULL iff the row has no request body" ([schema.sql:55-61](../../internal/store/schema.sql#L55),
[store.go:1459-1462](../../internal/store/store.go#L1459)). Archival NULLs `req_body` while leaving
`req_tool_names` populated, so an archived row becomes `req_body IS NULL AND req_tool_names IS NOT NULL` —
the exact shape `mergeEvents` names as the "NULL iff no body" violation the contract forbids
([merge.go:390-397](../../internal/store/merge.go#L390)). **The new semantics are: `req_tool_names` is
populated iff the call had a request body, hot or archived.** That change is unstated and load-bearing:
`rowsWithRequestBody` ([rules.go:357-373](../../internal/analyze/rules.go#L357)) and `doctor`'s
`toolNamesBackfillCheck` ([doctor.go:211-233](../../internal/cli/doctor.go#L211)) both define "has a request
body" as `req_tool_names IS NOT NULL`, so the session rules survive archival *only* because they route
through `ToolNames`, never `req_body`. Nothing in the archiver changes — its behaviour is correct; only the
stated contract was stale. Bead 08 updates `schema.sql`'s comment to the new wording and re-confirms those
two readers.

### C.7 Config

`HotDays int` — flag `--hot-days`, env `CLENS_HOT_DAYS`, file key `HotDays`, default **7**; `0` disables
archival (bodies stay hot forever, the pre-GI-16 behaviour). Negative is rejected. **`HotDays >
RetentionDays` when both are > 0 is rejected by `Validate`** ("nothing would ever be archived before it is
deleted"). `clens doctor` prints it and a WARN when the archive directory is unwritable.

### C.8 CLI and scheduling

- `clens archive status` — hot-window boundary, archived/unarchived row counts, archive dir size and file
  count, rows skipped for the backfill reason, any `missing` markers, and any **restore duplicates** (a day
  row present with no `body_archive` marker and its hot bodies non-NULL — the harmless leftover a crash
  between `restore`'s two steps can leave, §C.8's restore bullet).
- `clens archive run [--dry-run] [--yes]` — same gate as every writer: `--yes` to write, `--dry-run` reports.
  Runs the archiver against the DB from a second process (WAL allows it; small batches keep
  `busy_timeout(5000)` from being exhausted).
- `clens archive restore --since X --until Y [--dry-run] [--yes]` — the inverse of §C.4 step 4: decompress
  bodies back into the hot rows and drop their markers. Intended with `serve` stopped (like `rekey`); warns
  otherwise. `restore` is the **second mechanism that drops a marker**, so §C.4's head invariant demands it
  preserve the same fact — and its ordering and failure clauses are what make §C.6's true-orphan predicate
  stay unreachable:
  - **One hot transaction per row.** The body `UPDATE`s and the marker `DELETE` commit together or not at
    all, so a crash rolls back to body-and-marker intact. This is what makes "no marker **and** all three hot
    bodies NULL" unreachable for a live row, exactly as step 4's per-batch transaction does for the archiver
    — the same fact §C.6's predicate leans on, now for *both* mechanisms rather than step 4 alone.
  - **Never drop a marker for a body you did not restore.** If the day file is missing or any blob fails to
    decode, the row is **reported** and its marker left **intact** — no partial application. A decode failure
    must not be able to reach the orphan-shaped state, so it aborts before the marker `DELETE`.
  - **Delete the day-file row *after* the hot transaction commits — never before.** `GCArchive` cannot do
    this for you: its true-orphan predicate (§C.6) deliberately does **not** match a restored row (the hot
    bodies are non-NULL), and widening it to match would re-open F4.1 by collecting crashed archiver batches.
    So the direction is fixed: the archive copy is the **last thing to go**, and a crash between the two
    steps leaves a harmless duplicate (hot bodies restored, day row still present, no marker) — never a loss.
    The **reverse** order would leave marker present, hot bodies NULL and no archive copy: a body in neither
    place.
  - **A leftover duplicate is reported, not collected.** When that day-row delete fails (or a crash leaves
    it), the row is hot-populated with no marker while the archive still holds a copy; `archive status`
    reports rows present in the archive with no marker and non-NULL hot bodies, so an operator can see the
    duplicate. No collector is added for it — the state is harmless and self-limiting.
  - **Fault injection** (mirroring the archiver's, §C.10) stops after each step — a crash between the marker
    `DELETE` and the day-row delete, and a decode failure — and asserts the marker is intact (or, post-commit,
    that the body is recoverable from one side).
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
| **Body lost on `restore` (marker dropped without the body back)** — the mirror of the row above, and the reachable path F7.1 named | §C.8's clauses: one hot transaction per row (body writes + marker drop commit together), a marker is **never** dropped for a body that did not restore (a missing/undecodable day file leaves it intact), and the day-file row is deleted **after** the hot commit. A fault-injection test stops after each step (incl. a decode failure) and asserts the marker is present and the body recoverable from one side — the state §C.6's predicate must spare |
| Orphaned sensitive bodies after `purge` | `ON DELETE CASCADE` + `GCArchive` + a dedicated test that greps the archive files' bytes for a sentinel |
| **A second archiver races `serve`'s `GCArchive`** (`clens archive run` from another process — §C.8 keeps that mode supported, WAL and small batches — vs `serve`'s boot/24h `GCArchive`) | What keeps it safe is the **true-orphan predicate** (§C.6), not serialization against `serve`: `GCArchive` collects a day file's body only when its marker is gone **and** all three hot body columns are NULL, so it can never remove the copy a mid-batch archiver is about to NULL. A "no marker" predicate alone would: it matches the legal crash-after-step-3 state and would delete a body between that archiver's step 3 and step 4, leaving the body in neither place permanently (§C.4). Test: run `GCArchive` between an injected step 3 and step 4 and assert the body is still recoverable from one side. |
| A body silently reported "not captured" when it is merely archived | `BodiesArchived` tri-state; `missing` is a distinct visible state |
| `mergeEvents` launders `capture_complete` on an archived row | rule in C.6, test with an archived `existing` and an incoming row carrying a body |
| Migration on the live 2.7 GB store | Per `CLAUDE.md` §Migrations: **back up `lens.db`, `lens.db-wal`, `lens.db-shm` before the first `serve`/`doctor` on the new binary**, state the path. The migration only creates a table and an index, so it is fast, but the rule has no size exemption. Claude does not run it against the live DB; the bead's verification uses a copy. |
| Hot file does not shrink | Documented; one-time `purge --vacuum` with `serve` stopped |
| Archive dir moved/deleted by the operator | `missing` state + `doctor`/`archive status` report it; nothing crashes |
| zstd decode bomb from a corrupt archive | decode with a max output = the recorded `*_len` (and never more than `BodyCapBytes`-scale sanity bound); a mismatch ⇒ `missing`, logged |
| Time-of-day boundary: `started_at` UTC day vs the operator's local day | file names are UTC, documented; `archive status` prints UTC days |

---

## Workstream D — Pass-through conformance

### D.1 What it is for

The auto-mode notice told this operator that their gateway "isn't compatible" and to have it pass
requests and replies through unchanged. Established facts record why that is not clens's defect: the
upstream is DeepSeek's Anthropic-compatible surface, so Anthropic's server-side checks can never run
there, whatever clens does. **D changes no proxy behaviour — there is no defect to fix.**

What is missing is the *evidence*, in the form this repo already uses elsewhere (the TTFB gate, the
`internal/proxy` import guard, the schema invariant test). Today "clens is byte-transparent" is a claim
established by reading the hot path. D makes it a test, so the next such notice costs one command to
answer instead of a fresh investigation — and so a future change that starts parsing or re-serializing
a body fails CI rather than silently breaking every gateway-dependent feature.

### D.2 The invariant

For any request or response byte stream, clens forwards it **unchanged**, with two sanctioned
exceptions, and only two: hop-by-hop headers that RFC 9110 requires `ReverseProxy` to drop, and the
capture-side copy (which is never the bytes on the wire).

Concretely, in `internal/proxy/proxy_test.go`, against the existing fake-upstream fixture — the
upstream records the raw bytes it received and the test compares them byte-for-byte, never
decoded-then-compared:

| # | Case | Assertion |
|---|---|---|
| 1 | Request body carrying an unrecognized top-level field (`"safeguards":[…]`) | upstream received the body **byte-identical** |
| 2 | Response body carrying an unrecognized top-level field (`"safeguard_results":{…}`) | client received the body **byte-identical** |
| 3 | Streamed SSE where the unknown field sits inside a `message_delta` event's `delta` | byte-identical, and event framing/ordering unchanged |
| 4 | Tool-use IDs in the streamed events | unchanged — no rewriting |
| 5 | A request header the proxy has no knowledge of (`anthropic-beta`) | reaches upstream unchanged |
| 6 | A credential header (`x-api-key`/`authorization`) | reaches upstream **unchanged**, even though the capture copy is redacted |

Case 6 is the load-bearing one and the reason this section is worth its beads: redaction is
capture-only (`redactHeaders` feeds `st.reqHeaders`, never `r.Header`), so a future change that
"redacts before forwarding" would break every session while looking like a security improvement. If
that assertion already exists elsewhere in `proxy_test.go`, bead 13 folds into it rather than adding a
duplicate.

### D.3 Non-goals

- **It does not make the session eligible for server-side auto-mode checks.** Nothing clens can do
  does. The operator-facing answer is `CLAUDE_CODE_AUTO_MODE_SERVER=0`, or routing to
  `api.anthropic.com` instead of DeepSeek — mutually exclusive with DeepSeek models.
- No new `doctor` check, no new route, no config key. If a diagnostic is wanted later it is its own
  ticket; bead 13 is the test alone.
- Not a general fuzz/property harness. Six named cases, not a generator.

### D.4 Risks

| Risk | Handling |
|---|---|
| The test passes trivially because it compares decoded values, not bytes | every case compares raw bytes; case 1 and 2 use a field the proxy's own types do not model, so a decode-and-compare would pass while a real drop goes uncaught |
| A hop-by-hop drop is mistaken for a defect | the test names the two sanctioned exceptions explicitly; it asserts equality of the body and of end-to-end headers, not of the full header set |
| Duplicates coverage that already exists | bead 13's first step is to read `proxy_test.go` and extend an existing case if one covers it |

---

## Test strategy

| Layer | What |
|---|---|
| A — web | `assets_test.go` source-shape guards (inverted Calls-`custom` assertions; `customWindow` calls `timeWindow`, no `toISOString`); manual browser pass incl. a DST-adjacent range and `to < from` |
| B — cli/api | table-driven `runRestart` against an in-process fake server on ephemeral ports (running / not running / new exe healthy / new exe unhealthy → rollback / rollback impossible); detached-child-survives-parent via `TestMain` helper process; `/api/reload` guard tests (Origin, non-loopback, malformed body) copied from `shutdown`'s; all-or-nothing on an invalid file; `SetAccounts` under `-race` |
| C — store | round trip (bytes in == bytes out, incl. NULL vs empty); fault injection after every step — the archiver's **and** `restore`'s (a crash between `restore`'s marker drop and its day-row delete, and a decode failure, each leave the marker intact and the body recoverable from one side); idempotent re-run; two archivers writing one event with different masks leave **both** the file's mask and (F6.1) the **marker's** mask the union — the file's is not enough: assert the *marker* never regresses below a set a wider archiver committed, the hole v7's plain `SET body_mask = excluded.body_mask` left; late row into an archived day; `mergeEvents` with an archived existing; purge leaves no archived byte; `PurgeOlderThan` cascade; rekey pass 1 over a marker-bearing collision candidate (a `resp_body`-hot row whose other bodies are archive-only, with a taker already holding its target id) **reports and skips** it — the row is not absorbed, the archive bytes survive, and a following `GCArchive` collects nothing; `HotDays` boundary at exactly the cutoff; skipped-for-backfill rows; hydration of `missing`; `PurgeableBytes` counts an archived range's day-row `req_len+resp_len`, and a dry run over a range whose day file is missing is skipped-and-counted, never an error; `EXPLAIN QUERY PLAN` guards that the candidate query uses `idx_events_started_at` and never scans blobs (the same shape as GI-13's guards) |
| C — perf | archiver-vs-reader latency test (C.10), and a benchmark over a synthetic 500-row day so the batch size is chosen from a number |
| D — proxy | six raw-byte-equality cases (D.2), incl. a credential header surviving to upstream unredacted; a decode-and-compare would pass while a real field drop goes uncaught, so none of them decode |
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
full `VACUUM`, and that stalls capture; (e) `_txlock=immediate`/`BEGIN IMMEDIATE` to order step 4's re-read
— it changes the transaction mode for all eleven `BeginTx` sites to buy an ordering the marker's own
monotonicity already gives, and would not even order the day-file read (§C.4). `reload`'s live set is
deliberately three fields; growing it is
where this turns into a config framework. `restart` writing a state file is the one piece of new
persistent state in B; it is a hint, never authoritative.

**As a QA engineer.** The dangerous states are the *partial* ones, so each ordering point in the archiver,
in `restore` (hot commit / marker drop / day-row delete — §C.8) and in `restart` (shutdown requested /
ports free / spawned / healthy / rolled back) has a test that stops there. Edge cases: a row with only one
body; a body that is exactly empty (`[]byte{}`) vs NULL; a day with one row; a row archived and then
re-ingested by `ingest --rebuild`; `HotDays` reduced by `reload` (rows
become archivable mid-run — next tick, not immediately); an archive file present but the marker gone
(orphan → `GCArchive`, not served); a `restore` crash between the hot commit and the day-row delete (hot
bodies back, no marker, day row still present — a **reported duplicate**, not collected); and a `restore`
that cannot decode a body (marker left intact).

**As a security engineer.** Bodies are the asset the repo exists to protect; archival *copies* them into a
second at-rest location, so the guarantee to preserve is "purge means purge". Archive files get the same
directory as `lens.db` and the same protection — on Windows that is inherited ACL, which is not stronger
than the DB's, and this plan does not claim otherwise. Nothing new is bound to a network: `reload` is a
loopback-caller route behind `originReject`, identical in shape to `shutdown`, and is a **second route
under the third write guard** (the loopback-caller mechanism) — the guard list itself stays three. The state file holds paths and addresses only. `restart`'s child inherits the parent's environment
including any `CLENS_*` values; none is a credential (credentials live in `internal/secret`, outside the
DB and outside `config`). zstd decode is size-bounded by the recorded length.

## Files (planned)

| Area | Files |
|---|---|
| A | `internal/web/{index.html,app.js,assets_test.go}` |
| B | `internal/cli/{restart.go,reload.go,serve.go,detach_windows.go,detach_unix.go,*_test.go}`, `internal/api/{reload.go,api.go,accounts.go}`, `internal/consumer/consumer.go`, `cmd/clens/{main.go,main_test.go}` |
| C | `internal/config/config.go`, `internal/store/{schema.sql,store.go,merge.go,archive.go,types.go,*_test.go}`, `internal/cli/{archive.go,purge.go,rekey.go,reflag.go,serve.go,show.go,doctor.go,*_test.go}`, `cmd/clens/main_test.go`, `internal/analyze/rules.go`, `internal/api/archive.go` (only if bead 10 adds `GET /api/archive` — else not created; **no `internal/api/api.go` edit**: `eventDetail` embeds `*store.Event` so the detail field needs none, and the no-hydrate mode is a filter flag, not an `api.Store` method — F7.2), `internal/web/{app.js,assets_test.go}` |
| D | `internal/proxy/proxy_test.go` (bead 13 reads it first and extends an existing case if one already covers a row of D.2 — see D.4) |
| Docs | `README.md` (operator runbooks: restart, reload, archive, the one-time vacuum; plus the auto-mode notice note) |

## Bead outline (for Phase 3)

| # | Bead | Depends on |
|---|---|---|
| 01 | A: Calls `custom` range + flipped guards | — |
| 02 | B: `serve` writes/removes `serve.state.json`; `--log` default | — |
| 03 | B: `clens restart` (detach helpers, health, rollback) | 02 |
| 04 | B: `consumer.SetAccounts`, `POST /api/reload`, `clens reload`, accounts route applies live | 02 (sequencing only — both edit `serve.go`'s startup, so 02 lands first; reload reads *nothing* from 02's state file, §B.4 step 1) |
| 05 | C: `HotDays` config + validation + `doctor` line | — |
| 06 | C: migration 4→5 (`body_archive` **including `body_mask`** — added now, not deferred: migration 4→5 has not shipped, and deferring means a second migration against the live 2.8 GB store later, on the table whose premise is that it stays byte-for-byte untouched), the per-blob-codec archive file format with its own monotone `body_mask` (the upsert only adds columns), mask-driven `hydrate` on `GetEvent`/`ListEventsFull` (**read the mask from the day file row it opens**, fill only the columns that mask holds, §C.5), row-level `BodiesArchived`, the `EventFilter.SkipHydrate` flag the boot redaction self-test sets (a filter flag, **not** a sibling method: `api.Store` and `internal/api/api.go` are unchanged, §C.5), and **the `Store` archive-dir field** (`Open` sets it from `filepath.Dir(dbPath)/archive` — the F6.3 source that `GCArchive` (07) and `PurgeableBytes` (08) both consume) | 05 |
| 07 | C: `Archiver` (batched, verified, ordered; step 4 re-reads the day file row's `body_mask` inside the hot transaction and **ORs** it into the marker — monotone, never regressing below a wider archiver's set, F6.1 — and drives its NULLs) + `GCArchive` | 06 |
| 08 | C: archive-aware writers — merge (mask-keyed: a column is present only if the marker's `body_mask` holds it; the under-claim direction is a harmless re-backfill, §C.6), purge cascade + GC (incl. `PurgeableBytes`' archived-body term, summing the day rows' `req_len+resp_len` via the archive-dir field bead 06 added; a missing/unreadable day file is skipped and counted, never fatal, so `purge --dry-run` cannot error over a moved archive), rekey/reflag/backfill rules (rekey pass 1 skips **every marker-bearing row** — marker presence, not `resp_body` availability, is the criterion, and the skipped count rides `RekeyPass1Report`'s new `Skipped` field, rendered by `rekey.go`'s two print sites **with the `clens archive restore` remedy**; its collision arm's delete is the cascade row's one stated exception, §C.6), and the `req_tool_names` contract change (`schema.sql` comment; re-confirm `internal/analyze/rules.go`'s `rowsWithRequestBody` and `doctor`'s `toolNamesBackfillCheck`; `ArchivedBodyMask` on the merge load path), plus the rekey pass-1 marker-skip test: a marker-bearing collision candidate is reported and skipped, not absorbed, its archive bytes survive, and a following `GCArchive` collects nothing | 06, 07 |
| 09 | C: `clens archive status/run/restore` — `restore` is **one hot transaction per row**: the bodies go back **and** the marker drops together, a marker is **never** dropped for a body that did not restore (missing/undecodable day file leaves it intact), and the day-file row is deleted **after** the hot commit, never before (§C.8); plus `serve` scheduling, `reload` applies `HotDays`, `restore`'s fault-injection test, and `archive status`'s restore-duplicate line | 04, 05, 07, 08 |
| 10 | C: dashboard + `internal/cli/show.go` archived-state display; note the one new `BodiesArchived` wire key in `ls --json`/`export` (`ArchivedBodyMask` is `json:"-"`); add `GET /api/archive` (`internal/api/archive.go`) **only if** deemed cheap, else drop it | 06 |
| 11 | Docs: `README.md` runbooks (incl. the auto-mode notice note, D.3) | 03, 04, 09 |
| 12 | Integration: restart + archival end-to-end on temp dirs/ephemeral ports; live-store *copy* migration check | all |
| 13 | D: proxy pass-through conformance test | — |

01, 02, 05, 13 are mutually independent and can land first. 13 is test-only and touches no other
bead's files; dropping it leaves the story intact.

**`cmd/clens/main_test.go` is a shared B/C file.** Its `carriedOver` list is written out by hand, not derived
from the `commands` map ([main_test.go:10-30](../../cmd/clens/main_test.go#L10)), precisely so a missing entry
is noticed, and `TestEveryCarriedOverCommandIsDispatched` fails on `len(commands) != len(carriedOver)`
([main_test.go:46-53](../../cmd/clens/main_test.go#L46)). `commands` has 23 entries today
([main.go:16-47](../../cmd/clens/main.go#L16)), so adding a subcommand without its `carriedOver` entry turns
`go test ./...` — the plan's own landing gate — red. The three new names span both workstreams: **bead 03**
adds `restart`, **bead 04** adds `reload`, **bead 09** adds `archive`, taking the list 23 → 26. Whichever bead
lands first also edits this file; each names its own entry.

## Context docs to refresh (running list — Phase 5.6 is the only phase that writes to `docs/context/`)

Seeded now from what Phase 1 verified; later phases append.

- `dashboard.md:109-113` — "Calls offers no `custom`" is false once bead 01 ships; rewrite (not delete) the
  two-bullet contrast. Also `:100`, the `:24` line-count table, and the Calls row at `:88`.
- `INDEX.md:33` — currently reads "**21**-entry dispatch table" (stale against the real 23 in
  `cmd/clens/main.go`); set it to 26. The same 21→26 count is pinned **in code**, by hand, in
  `cmd/clens/main_test.go`'s `carriedOver` (not doc-only, and it fails `go test ./...` if missed — see the
  bead-outline note). `:37` (56 test files — re-measure), `:40` (dashboard line count);
  the `decisions/` count and hint (ten → eleven).
- `cli-and-tooling.md:6,17,39,41-58` — 26 subcommands; `serve` gains the state file; `restart`/`reload`/
  `archive` rows; the `--yes`-gated writers table header at `:41` ("five writers, two destructive") goes
  **five → seven** (destructive stays two) as `archive run` and `archive restore` gain rows — neither
  *deletes* rows; `archive restore` rewrites bodies.
- `api-surface.md` — `/api/reload`: a second route under the existing loopback-caller guard (the guard
  *list* stays three; the Writes table goes six → seven); the `BodiesArchived` field on the detail
  response; and the additive `BodiesArchived` key on `ls --json` / `export` (`ArchivedBodyMask` is
  `json:"-"`, so it is not a wire key and not documented as one).
- `storage-schema.md` — `schemaVersion` 5, `body_archive` (incl. `body_mask`), the archive file format, the merge rule for
  archived rows (mask-keyed), and the "hydration on `GetEvent`/`ListEventsFull`" read path (incl. the `ListEventsFull`
  no-hydrate mode). Also the **`:255` "Cascade is scoped" rule row** and, one screen up, the **ER diagram at `:92`**
  (`events ||--o{ warnings` — `body_archive` adds a second enforced edge) and the sentence at **`:97`** ("Only the
  `events → warnings` edge is enforced" — goes from one to two), all falsified by `body_archive`'s second
  `ON DELETE CASCADE`; and the **`req_tool_names` contract** (C.6: "populated iff the call had a request
  body, hot or archived").
- `data-privacy-and-compliance.md:114-132` **and `:134-136`** — retention section: archives are the same
  sensitive class; `purge` reaches them; `HotDays` vs `RetentionDays`. **`:134`** ("**Cascade is scoped:**
  `ON DELETE CASCADE` appears only on `warnings.event_id`") is falsified by `body_archive`'s second
  `ON DELETE CASCADE` and must be rewritten. **The archive is a second at-rest location for the same
  content**, so this entry also reaches the file's two loudest single-location claims — **`:7`** ("the
  database is the single most sensitive artifact the tool creates") and **`:24`** ("**The threat model is
  the database file.** … `lens.db` contains the content. **Copying it copies everything**") — and the
  `:16-17` sensitive-data inventory rows ("Where stored … `~/.clens/lens.db`") for
  `req_body`/`resp_body`/`transcript_content`, all now incomplete because the day files hold the same
  bodies.
- `security-and-permissions.md` — `reload`'s guard (in the AuthN/AuthZ table beside `shutdown`); archive
  dir permissions and the Windows caveat.
- `build-and-run.md` — `--hot-days`/`CLENS_HOT_DAYS`; `serve.state.json` and `serve.log`.
- `architecture.md` — the archiver as a new background actor; the connection-sharing rule now names it.
- `workflows.md` — two new flows (restart; archival pass).
- `glossary.md` — hot window, archived body, `BodiesArchived`, `serve.state.json`.
- `conventions.md` — the `--yes` gate paragraph if it enumerates writers.
- `testing-and-quality.md` — size figures, new invariant rows, the fault-injection pattern, and the
  pass-through conformance cases (D.2) as a named invariant alongside the TTFB gate.
- `architecture.md` (second entry) — the pass-through guarantee (D.2) is now a tested invariant, not
  just a description of the hot path; state it where the "does no parsing" claim already lives.
- `decisions/011-…` — **new ADR**: bodies-only archival with a side table and per-day files; rejected:
  whole-row archival, `ATTACH`, a marker column, background `VACUUM`. Update `decisions/000-index.md`.

## Change history

### v1
Initial plan, after Phase 1 intake and four user decisions (archive scope, hot window, story shape,
reload scope).

### v2
Fifth decision — workstream D (proxy pass-through conformance, bead 13, test-only). Prompted by the
auto-mode classifier notice naming `127.0.0.1:8797`; the diagnosis in Established facts found the
notice misattributes to clens (the upstream is DeepSeek, so Anthropic's server-side checks can never
run there) and that the hot path rewrites nothing. D therefore adds evidence, not behaviour.

### v3
Round-1 cross-review triage applied — all twelve findings F1.1–F1.12. Edits: the `restart`/`reload`
arg-slice split, one slice per consumer (F1.1); the archived `req_tool_names` contract stated and its
readers (rules.go, doctor) re-confirmed (F1.2, docs + bead file-list only — archiver behaviour unchanged);
`PurgeableBytes` gains its archived-body term (F1.3); `ListEventsFull` gains a no-hydrate mode the boot
redaction self-test uses (F1.4); the write-guard wording corrected to "a second route under the
loopback-caller guard" (F1.5); bead 04/09 dependency split — `HotDays` apply moves to bead 09 (F1.6); the
single inverted Calls-`custom` assertion (F1.7); the from/to `hidden` goes on the labels, never the inputs
(F1.8); the cascade-rule refresh cites extended to `:134-136` / `:255` (F1.9); the `21 → 26` dispatch and
`five → seven` writer counts (F1.10); the additive `ls --json`/`export` keys (F1.11); and the separate
`HasArchivedBodies` marker field for the merge load path (F1.12). See `review/round-1/triage.md` and
`changelog.md`.

### v4
Round-2 cross-review triage applied — F2.1, F2.3–F2.9 (F2.2 withdrawn by the user and fixed by hand; not
re-edited here). Edits: `show.go` added to the C Files row, bead 10 now lists it and the *conditional*
`internal/api/archive.go` (F2.1); the merge load-path signal becomes `Event.ArchivedBodyMask uint8`
(`json:"-"`) and `BodiesArchived` is the **only** new wire key, so an archived row no longer serializes a
self-contradictory always-false key (F2.3); `body_archive` gains `body_mask` — per-column presence written in
the same batch as the bodies, keyed off by `mergeEvents` so a column the archive does not hold stays
backfillable — added to migration 4→5 now rather than deferred (F2.4, per conductor override — the
documentation-only variant was ruled out); `restart` strips `--exe`/`--timeout` with `takeFlag` **before**
`config.Load` (F2.5); the `storage-schema.md` refresh entry names the ER `:92` and `:97` cascade statements
(F2.6); the `restart` rollback respawns from the exe/args step 1 retained, not the deleted state file
(F2.7); the v2 change-history sentence that double-claimed the round-1 findings is dropped (F2.8); and the
A.2 row now also rewrites the two `assets_test.go` comments at `:625-630`/`:661-671` that assert the
opposite (F2.9). Incidentally corrected: the `getEventByRequestIDTx` cite in §C.5 (`merge.go:112` → `:82`,
the declaration; `:112` is its call site). See `review/round-2/triage.md` and `changelog.md`.

### v5
Round-3 cross-review triage applied — all six findings F3.1–F3.6. Edits: **the hydration gate is now
mask-driven, not "three hot bodies empty"** — `hydrate` fires when a row has a marker and at least one
column the mask holds is empty, and fills exactly those columns (the "three-empty" gate was the read-side
half of the F2.4 ordering and re-created the silent "not captured *when merely archived*" state C.9/C.10
exist to prevent); `Event.BodiesArchived` now describes **the row, not a column** — `"restored"` covers a
partially restored row and is never `""` for one, `ArchivedBodyMask` stays `json:"-"` (the carve-out was
re-checked against the new gate, not assumed: both consumers read the mask from `body_archive`'s row);
C.6's mask-keyed merge rule is kept and tied to C.5 (the same mask drives backfill *and* restoration)
(F3.1, per conductor override — both halves move together; reverting F2.4 would reinstate its transcript
loss). §C.4 step 4 now NULLs **only the masked columns** (`CASE WHEN body_mask & N THEN NULL ELSE col END`),
so a body the archive does not hold is never cleared, with a one-sentence corollary that a post-archival
column stays hot and is never re-archived, so the hot file plateaus *approximately* rather than exactly
(bounded, rare; no re-archival machinery added) (F3.2, per conductor override). The archive file format
carries a **per-blob codec** (`req_codec`/`resp_codec`/`tc_codec`) rather than one row-level `codec`, so
the stored codec always matches the stored bytes and hydrate cannot manufacture a bogus `"missing"`
(F3.4, per conductor override). The `data-privacy-and-compliance.md` refresh entry now names `:7`, `:24`
and the `:16-17` inventory rows (the archive is a second at-rest location for the same content) (F3.3);
bead 04's dependency rationale is restated as sequencing, not "boot args capture", which §B.4 step 1
contradicts (F3.5); and §B.4's "the only edit outside B's own files" is reworded to "the only pre-existing
*route* B edits", since `accounts.go` and the other touched files are inside B's Files row (F3.6). See
`review/round-3/triage.md` and `changelog.md`.

### v6
Round-4 cross-review triage applied — all three findings F4.1–F4.3. Edits: **`GCArchive`'s orphan predicate
is now a *true* orphan** — no `body_archive` marker **and** all three hot body columns NULL — with the
soundness stated, not assumed: step 4's marker `INSERT` and its column `NULL`s are one hot transaction per
batch, so "no marker and the hot bodies already NULLed" is unreachable for a row that has an archive copy and
the pre-marker states (§C.4's legal crash/overlap state) all still have the bodies hot, so they are spared;
the missing **C.10 risk row** for the concurrent-archiver race (`clens archive run` vs `serve`'s boot/24h
`GCArchive`) is added, naming the predicate as the control; and §C.4's crash-safety paragraph now ties the
step-2 mask, the step-4 NULLs, §C.5's hydrate and the §C.6 GC predicate into one stated invariant, so the
F3.1/F3.2/F2.4/F4.1 changes read as one story (F4.1, per conductor override — the true-orphan predicate, **not**
mutual exclusion; the second-process `archive run` mode in §C.8 is kept as deliberate). `cmd/clens/main_test.go`
— whose hand-pinned `carriedOver` list fails `go test ./...` on `len(commands) != len(carriedOver)` — is added
to **both** B's and C's Files rows (its three new names span `restart`/`reload` and `archive`), with a
bead-outline note naming which bead adds which entry (F4.2). `reflag` now **excludes** archived rows from
`reflagScope` and **reports the excluded count** with the `clens archive restore` remedy — the same treatment
rekey pass 1 gets, and **not** a fourth `ReflagCounts` bucket (the three still partition the narrowed scope) —
with `internal/cli/reflag.go` and its test added to C's Files row (F4.3). See `review/round-4/triage.md` and
`changelog.md`.

### v7
Round-5 cross-review triage applied — the one finding F5.1. **The day-file write is now monotone in coverage
and the marker mirrors it**, per conductor override (fix (a), extended — mutual exclusion is *not* taken and
the second-process `clens archive run` stays, §C.8). Edits: the `bodies` table gains its own `body_mask`
(1 req/2 resp/4 transcript) and its per-blob `codec`/`*_len` become nullable-but-paired with the blob; the
day-file write becomes `… ON CONFLICT(event_id) DO UPDATE SET body_mask = bodies.body_mask | excluded.body_mask`
with `COALESCE(excluded.<col>, bodies.<col>)` per blob/len/codec, so a column is only ever added, never
dropped (§C.3 — the old `INSERT OR REPLACE`, whose "idempotent" comment was **false** across two writers
with different masks, is gone); §C.4 step 4 now re-reads the day file row's `body_mask` **inside the hot
transaction** and uses it for both the marker upsert (`ON CONFLICT … DO UPDATE SET body_mask = excluded.body_mask`,
the `INSERT OR IGNORE` is gone) and the `CASE WHEN :mask & N` NULLs, with the soundness stated next to the
`neither place` claim: the file's coverage is monotone and the re-read/marker-write are one write-lock-held
transaction, so a committed marker can be *behind* the file (a column it does not yet claim stays hot and
visible) but never *ahead* of it (the over-claim that `hydrate` reads as `"missing"`) (F5.1, per conductor
override); §C.6's merge row confirms explicitly that the merge still never opens the day file, because the
marker it reads is now a mirror of the file row. Bead 06 (file format) and bead 07 (archiver step-4 mirror),
and the C store test row (a two-archiver different-mask case), note the change. See `review/round-5/triage.md`
and `changelog.md`.

### v8
Round-6 cross-review triage applied — F6.1, F6.2, F6.3. **F6.1 (per conductor override — take the requested
(i)+(iii), reject the alternative (ii)):** the v7 claim that "the transaction holds the store's write lock
across the re-read" was **false** and is **deleted, not corrected** — the DSN has no `_txlock` and all eleven
`BeginTx` sites are `BeginTx(ctx, nil)` ([store.go:68-71,304,330,…]), so the `BEGIN` is DEFERRED and the write
lock is taken at the marker `INSERT`, *after* the re-read; and the re-read targets the **day file, a separate
SQLite database** the hot store's lock never ordered at all. The two fixes: **(i)** the marker upsert becomes
`ON CONFLICT(event_id) DO UPDATE SET body_mask = body_archive.body_mask | excluded.body_mask`, mirroring
§C.3's file write, so the marker is monotone and can only *under*-claim (a column stays hot and visible),
never regress below a set step 4 itself NULLed — the v7 `plain SET body_mask = excluded.body_mask` was the
F6.1 hole; **(iii)** `hydrate` now resolves its masked columns from the **day file row it already opens**, not
from the marker (§C.5), taking the marker off the read-correctness path — the marker's `body_mask` now
informs only the merge's **write** path, where a lagging marker is harmless (it merely re-backfills). **(ii)
`_txlock=immediate` is REJECTED** and §C.4 says why: it would change the transaction mode for all eleven
`BeginTx` sites to buy a lock the design no longer needs once (iii) lands, and would not order the day-file
read anyway. §C.4 gains **the invariant stated once** at its head (day-file row is the authority; both masks
monotone; step 4 NULLs the file's mask; `hydrate` reads the file row ⇒ under-claim only, never over-claim),
which the mechanism paragraphs now reference; §C.3's `body_archive.body_mask` comment is restated to the
multi-writer mirror semantics (it still said "the columns the archiver wrote non-NULL", the single-writer SET
reading); §C.6's merge row drops the "same fact" claim for the lagging-mirror one. **F6.2:** §B.2's `reload`
bullet drops "of the recorded args" — §B.4's `os.Args[2:]` (not the state file's `args`) is the settled
statement. **F6.3:** §C.6 now names the mechanism, not just the requirement — `Store` gains an archive-dir
field set at `Open` from `filepath.Dir(dbPath)/archive` (bead 06, the first bead that opens a day file), so
`GCArchive` (07) and `PurgeableBytes` (08) read the same path; the archived-byte term sums the matching day
rows' `req_len+resp_len`; bead 08's term now has its source named. The C store test row gains the
marker-non-regression assertion the v7 two-archiver test could not make (it checked only the file's mask).
See `review/round-6/triage.md` and `changelog.md`.

### v9
Round-7 cross-review triage applied — F7.1, F7.2, F7.3. **F7.1 (graded MAJOR by the conductor over the
reviewer's MINOR — a reachable permanent-loss path contradicting an absolute claim is the F4.1/F5.1 class):**
`archive restore` was a second mechanism that drops a marker, and the plan constrained neither its ordering
nor its failure behaviour, so a crash (or a missing/undecodable day file) could leave a live row at "no
marker **and** all three hot bodies NULL" — the exact state §C.6's true-orphan predicate collects, i.e.
permanent loss. §C.8's restore bullet now carries the clauses: **one hot transaction per row** (bodies back
and marker drop commit together or not at all); **never drop a marker for a body you did not restore**
(missing/undecodable day file ⇒ report the row, leave the marker intact, no partial application); **delete
the day-file row *after* the hot commit, never before** (`GCArchive` cannot collect it — its predicate
deliberately does not match a restored row, and widening it would re-open F4.1; the reverse order would
leave a body in neither place); a **leftover duplicate is reported by `archive status`, not collected**; and
restore's **fault-injection test** and **C.10 risk row** (mirroring the archiver's) are added. §C.6's
true-orphan soundness now derives from §C.4's head sentence and covers **both** mechanisms, and that head
sentence's conclusion (and §C.4's closing restatement) now names `restore` as maintaining the same invariant.
**F7.2:** the C Files row's `internal/api/api.go (detail field)` is **dropped** — `eventDetail` embeds
`*store.Event` ([api.go:394-395](../../internal/api/api.go#L394)), so `BodiesArchived` reaches the detail
JSON with no edit — and §C.5 now **pins the no-hydrate mode as a flag** (`store.EventFilter.SkipHydrate`,
read by `ListEventsFull`), **not** a sibling method on `api.Store`: the filter flag leaves the signature and
the `api.Store` interface unchanged, because the only consumer is `checkRedaction`, which holds the concrete
`*store.Store`. Bead 06 names the flag; no bead claims an `api.go` edit. **F7.3:** §C.6's `PurgeableBytes`
term states the one scoped clause — **a day file that is missing or unreadable is skipped and counted, never
fatal** — so `clens purge --dry-run` ([purge.go:96](../../internal/cli/purge.go#L96)) cannot error over an
operator-moved archive; the C store test row and bead 08 carry it. See `review/round-7/triage.md` and
`changelog.md`.

### v10
Round-8 cross-review triage applied — the one finding F8.1 (MINOR), per conductor override on three points.
**F8.1:** rekey pass 1 has a **fourth** `DELETE FROM events` the plan never named — the collision path's
absorb ([store.go:1788](../../internal/store/store.go#L1788), inside `rekeyOneProxyRow`, reached from
`RekeyProxyBodyIDs` [store.go:1705-1717](../../internal/store/store.go#L1705)) — which CASCADEs the absorbed
row's marker exactly as a purge does, but is **not** a purge: the data is meant to survive in the taker
([store.go:1778](../../internal/store/store.go#L1778)) and the merge that carries it backfills only the
absorbed row's **hot** columns ([merge.go:388](../../internal/store/merge.go#L388), `:405`, `:443`). The
edits: **(1) the rekey row is pinned to marker presence, not `resp_body`** — pass 1 now "reports and skips
**every** row with a `body_archive` marker", because a marker-bearing row's `resp_body` may be hot (§C.4's
corollary) so a `resp_body`-driven criterion would *not* skip it, and on a collision that row is
absorbed-and-deleted; remedy `clens archive restore` first (conductor override; the stated criterion, not the
code, was what leaked). **(2) §C.6's cascade row names rekey's collision delete as the one stated exception,
and does NOT generalize the cascade rule** — the three writers *discard* the row, so "vacuously" holds for all
three, while rekey's delete *moves* it, so the vacuity argument does not reach it; "a delete whose semantics
are *move* rather than *discard* is exactly where the cascade rule stops applying" (conductor override — the
"rule holds for all `DELETE FROM events` paths" option was **rejected as false**, since it would hide the very
thing the finding is about). **(3) §C.4's head sentence now closes its conclusion as a rule over every path
that touches a marker** — the archiver's steps, `restore`, and rekey pass 1's absorb path are the three known
ones; the first two derive from the sentence, the third is a stated exception, and any new path must either
derive from the sentence or be named as a stated exception (conductor override — this is how a fourth proof is
avoided rather than written). A **test** is added to the C-store row: rekey pass 1 over a marker-bearing
collision candidate reports and skips it — the row is not absorbed, the archive bytes survive, and a following
`GCArchive` collects nothing. Rekey is **not** extended to carry the absorbed row's archived bodies to the
survivor: skipping with a stated remedy is the plan's existing convention and hydration inside rekey is new
machinery for a case the skip removes entirely (conductor override). Bead 08 carries the rule. See
`review/round-8/triage.md` and `changelog.md`.

### v11
Round-9 cross-review triage applied — F9.1, F9.2, F9.3, all per conductor override. None is a BLOCKER or
MAJOR; the reviewer could not break §C.4's head sentence and reported no coverage or loss hole. **(1) F9.1:**
**the rekey marker-skip test, previously present only in the Test-strategy C-store row, is now **named in bead 08 — so the bead that owns the rule owns its control, matching
bead 09's precedent of naming restore's fault-injection test. A test in the strategy table but not in the
owning bead is how it is dropped at beadification. **(2) F9.2:** §C.6's rekey row said pass 1 "reports" the
rows it skips, but `RekeyPass1Report` carried only `ReKeyed`/`Synthetic`
([store.go:1672-1675](../../internal/store/store.go#L1672)) and `rekey.go`'s two print sites
([rekey.go:74-76](../../internal/cli/rekey.go#L74), `:81`) rendered only those — so the skip was silent and the
`clens archive restore` remedy unreachable. A **third count** (`Skipped`) is now named on
`RekeyPass1Report`, rendered by both existing print sites **with the remedy** — the way the reflag row already
pins its output. §C.6's rekey row and bead 08 name the channel together, where the claim is made. **(3)
F9.3:** §C.4's head said "every path that touches a marker … **Three are known**", but §C.3's own text
shows the three CASCADE purges *remove* markers too
([store.go:903](../../internal/store/store.go#L903), `:936`, `:1952`), so the count was wrong by the
sentence's own criterion — a list-versus-rule rot the F8.1 fix left in the head. Reworded to a **criterion,
not a count**: a path needs a proof only when it can leave a marker-bearing row whose data survives
elsewhere — the **move-versus-discard** distinction F8.1 established — with the *discarding* writers covered
by §C.6's vacuity argument. A criterion does not rot when a fourth mechanism appears; a count does. The edit
is scoped to the count only; §C.4's other head clauses are untouched. See `review/round-9/triage.md` and
`changelog.md`.

### v12
Round-10 cross-review triage applied — the one finding F10.1 (MINOR), per conductor override: fix it as a
**deletion**. **F10.1:** the definer clause the F9.3 reword added to §C.4's head stated the sort three ways and
two disagreed — its condition ("a path must actively maintain the invariant **only when** it can *leave a
marker-bearing row whose data survives elsewhere*") is false for two of the three members it names: `restore`
**drops** the marker (`plan.md:350-351`) and rekey pass 1's absorb **deletes** the row
([store.go:1788](../../internal/store/store.go#L1788)), so neither leaves a marker-bearing row; the sentence's
own preceding lines say exactly that. The charitable reading ("leaves *other* marker-bearing rows untouched")
is satisfied by every path, so it was vacuous rather than merely loose. The false condition is **dropped, not
hedged** — a hedged version of a vacuous condition is still vacuous — and the sort is stated **purely as
*move* versus *discard*, in the move-reason shape**: a path that *moves* a body — into the archive (step 4),
back out (`restore`), or into a taker (rekey pass 1's absorb) — must actively maintain the invariant, *because
after the move the body and the marker sit in different places and can drift apart*. That is the actual reason
the three are grouped, and it derives from the move, not from any state left behind. **Confined to the definer
clause:** §C.4's head's four original clauses (day-file-is-authority + coverage-monotone, marker-monotone,
step-4, `hydrate`), the under-claim/over-claim conclusion, the "rule over every path" framing, and the following
exception/discarding-writers sentence all read as before. No compensating prose added; bead 09 keeps `05`. See
`review/round-10/triage.md` and `changelog.md`.
