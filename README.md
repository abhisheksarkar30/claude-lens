# Claude Lens

A local observability proxy for Claude traffic.

One Go binary, `clens`, sits in front of the Anthropic endpoint and records
what was sent, what came back, what it cost, and which request parameters the
API silently dropped. It is a single-user developer tool: loopback-only by
default, no hosted service, no accounts to create.

## Why four sources

Most proxies report only what passed through them, which means they see
nothing at all if you point Claude Code straight at the API, and they cannot
see what you were actually billed. Claude Lens reconciles four:

| Source | What it is | What it adds |
|---|---|---|
| A — proxy | this tool's own capture of `/v1/messages` | request/response bodies, exact parameters, what was dropped |
| B — JSONL | Claude Code's own transcripts under `~/.claude/projects/**/*.jsonl` | calls that never went through the proxy, plus session identity |
| C — snapshot | the claude.ai usage endpoint | the plan's own utilization percentage |
| D — admin | the Admin API usage and cost reports | the real billed dollars, per day and model |

A and B are merged on `request_id`; a disagreement between them is itself a
finding (`source_mismatch`). C answers "how much of my plan have I used" and D
answers "what did this actually cost" — neither is derivable from A and B,
which is the whole reason the tool has four.

## Install

```
go install github.com/abhisheksarkar30/claude-lens/cmd/clens@latest
```

Then point your client at it:

```
export ANTHROPIC_BASE_URL=http://127.0.0.1:8797
```

and start it:

```
clens serve          # proxy on 127.0.0.1:8797, dashboard on 127.0.0.1:8798
```

`clens doctor` prints the effective configuration, the resolved bind
addresses, and a PASS/WARN/FAIL per check — run it first when something looks
wrong, because it reports the config the binary actually resolved rather than
the one you meant to write.

There is one binary and no build step for the dashboard: the HTML, CSS, and JS
are `go:embed`-ed into the executable. Nothing is fetched at runtime — not a
font, not a charting library, not a CDN script.

## The seven billing shapes

Two billing models that must never be summed into one number, across seven
plans:

| Plan | `billing_mode` | Cost shown | Quota shown |
|---|---|---|---|
| Free | `subscription` | API-equivalent value, labelled hypothetical | snapshot % + calibrated burn |
| Pro | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Max 5x | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Max 20x | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Team | `subscription` | API-equivalent value | snapshot % + shared-seat context |
| Enterprise | `subscription` | API-equivalent value | snapshot % + configured spend limit |
| Pay-as-you-go | `api` | real billed dollars (source D) and computed dollars (sources A/B) | Admin rate-limit reports |

The separation is carried in the storage schema, not in a convention: a
`subscription` figure is written to `api_equivalent_cost_usd` and `cost_usd`
is left NULL; an `api` figure is written to `cost_usd`. No `SUM(cost_usd)` can
merge them, because they are never in the same column.

Two states are first-class and never collapsed to zero:

- **`unpriced`** — a model with no rate row. An invented rate is worse than no
  rate, because `$0.00` reads as "this was free" while `unpriced` tells you to
  go configure something.
- **`unconfigured`** — a plan with no limit row. Claude Lens shows the
  snapshot percentage and your own token burn, and states that no limit is
  configured. It never guesses that "Max 20x" means a particular number.

## Commands

| Command | Does |
|---|---|
| `clens serve` | proxy + dashboard in one process. Blocks until interrupted. |
| `clens doctor` | print the effective config, bind addresses, and warnings |
| `clens ingest` | backfill from Claude Code's JSONL transcripts (`--rebuild` to start over) |
| `clens refresh` | run every non-proxy collector once — the cron / Task Scheduler target |
| `clens ls` | the call log, newest first |
| `clens show <id>` | one call in full |
| `clens tail` | the last few calls, then every new one as it lands |
| `clens stats` | window totals, a per-model split, and `--by` another grouping |
| `clens sessions` | one row per agentic run |
| `clens warnings` | findings grouped by kind, or one row each with `--detail` |
| `clens export` | every matching call as JSON lines or CSV |
| `clens replay <id>` | re-issue a captured call; `--dump` and `--diff <id>` send nothing |
| `clens quota` | rolling 5h/7d burn per subscription account, and calibration |
| `clens accounts` | configured accounts and their plan/billing state |
| `clens reconcile` | computed (A/B) vs billed (D) cost, per day and model |
| `clens models` | the rate catalogue, and which models have no rate at all |
| `clens prices` | the effective rate table, plus the edit paths |
| `clens purge` | delete captured rows by age or by the unpriced predicate |
| `clens rekey` | the one-off historical id backfill (already run; see its `--dry-run` report) |
| `clens reprice` | re-price stored rows from the current rate table |
| `clens reflag` | re-derive `capture_complete` on rows the old merge laundered |
| `clens restart` | stop and relaunch `serve` with its recorded flags, optionally on a new binary (`--exe`) |
| `clens reload` | apply `Accounts`, `RetentionDays` and `HotDays` from the config without a restart |
| `clens archive` | `status`, `run` and `restore` for archived bodies |

Four commands refuse to write without `--yes`, and the shared gate is not a
shared property: **`clens purge` and `clens rekey` delete rows; `clens reprice`
and `clens reflag` only rewrite columns.** All four default to the opposite of
destructive — nothing is written without `--yes`, and `--dry-run` prints what
`--yes` would have written.

### Re-pricing rows already captured

Editing a rate, or changing which model prefixes bill pay-as-you-go, does
**not** re-price rows already in the database. Re-pricing is `clens reprice`:

```
clens reprice --dry-run     # what would change, and writes nothing
clens reprice --yes         # recompute cost_usd / api_equivalent_cost_usd / cost_source
```

It recomputes each row's cost from the row's own stored inputs against the
current rate table. It inserts nothing and deletes nothing — a wrong run is
repaired by running it again, not by restoring a backup. Rows whose
`cost_source` does not name a reconstructible input set (`unpriced`, and any
`approximate:` reason other than `cache_ttl_unknown`) are skipped and reported
rather than guessed at, and `--dry-run` prints the moved/unchanged/skipped
split first.

**`clens ingest --rebuild` cannot do this job**, which is worth stating because
this section used to say it was the re-pricing path. `--rebuild` zeroes every
JSONL cursor so the next poll re-reads each transcript from byte 0, and the
re-read is absorbed by the `request_id` UNIQUE constraint as a merge — which
makes it **idempotent**, and is why it is safe to run. But a re-ingest produces
a *JSONL* row, priced with no `speed` and no `serviceTier`, and the merge never
replaces the proxy's bodies. The rows whose stored cost is wrong are **proxy**
rows, and a re-ingest cannot reach them. (`--rebuild` is still the right tool
for its own job: a rewritten transcript makes the byte cursor meaningless, and
only a from-zero pass is correct.)

`clens reflag` is the matching repair for the second defect class. A stored
`capture_complete` could be laundered to `1` by the old cross-source merge,
which destroyed the original bit — so a re-ingest cannot recover it either.
`reflag` re-derives the flag from a `Content-Length` witness: a stored body that
is a strict prefix of the length the client declared flips the row to
`incomplete`. It reports flipped / already honest / **residual**, where a
residual row is one the merge laundered with no witness left to prove it — not
repairable, and reported as such rather than guessed at.

Two things to know first:

- **The merge has to be correct before you rely on `--rebuild`.** A re-ingest
  merges the incoming row over the stored one, and `billing_mode` must move
  together with the cost columns. On a build older than GI#3 the merge kept the
  stored `billing_mode` while adopting the incoming cost, which would leave rows
  marked `subscription` carrying a real `cost_usd` — the pair the billing
  invariant forbids. Land the merge fix, then rebuild.
- **No config edit is needed for DeepSeek.** The shipped default already treats
  the `deepseek-` prefix as pay-as-you-go, so an unconfigured install routes
  those rows to the `api` account and prices them on its own. Set
  `CLENS_API_MODEL_PREFIXES` (or `ApiModelPrefixes` in `~/.clens/config.toml`)
  only to route something else as well, or to `none` to route nothing.

Every command takes the config flags — `--proxy-addr`, `--dashboard-addr`,
`--upstream-url`, `--db-path`, `--body-policy`, `--body-cap-bytes`,
`--allow-remote`, `--session-gap-minutes`, `--retention-days`, `--replay`,
`--accounts-path`, `--pprof-addr`, `--hot-days`. Precedence is flags > `CLENS_*` environment
> the config file at `~/.clens/config.toml` > defaults.

## Operating a running serve

### Body cap

Each captured body (request and response separately) is cut at `--body-cap-bytes` /
`CLENS_BODY_CAP_BYTES` / `BodyCapBytes` (default 2 MiB). A cut body marks the call
`capture_complete = 0` and raises `stream_incomplete`; token counts and cost are unaffected.
Long tool-use streams run about 90-130 bytes per output token, so a 20k-token turn reaches
2 MiB. If you see these, raise the cap in `~/.clens/config.toml` (for example
`BodyCapBytes = 8388608`) and `clens restart` — it is not live-reloadable. Rows already cut stay
cut; the flag records that truthfully. The buffer grows only as a body arrives, so a larger cap
costs nothing on ordinary traffic.

### Restart

```
clens restart [--exe PATH] [--timeout 30s]
```

Stops the running `serve`, waits for both ports to free, relaunches it detached
with the flags it was started with, and prints the measured proxy gap. On Windows a
running `clens.exe` cannot be overwritten, so build the new binary elsewhere while
the old one runs and point at it:

```
go build -o D:\build\clens.exe ./cmd/clens
clens restart --exe D:\build\clens.exe
```

`serve` records what `restart` needs in `serve.state.json` beside the database (the
executable, its arguments, its ports) and appends its output to `serve.log` in the same
place. Liveness is decided by `/api/health`, never by the file. If the new binary does
not come up healthy within `--timeout`, `restart` rolls back to the previous one and
exits non-zero; the message says which binary is serving. Replacing the installed
`clens` on your `PATH` stays your call — `restart` only relaunches.

**Do not try `restart` against the `serve` your live Claude session is using**: the
proxy is down for the gap, and a rollback failure leaves nothing serving. Try it on
a scratch database and ports (`--db-path`, `--proxy-addr 127.0.0.1:0`).

### Reload

```
clens reload
```

Asks the running `serve` (over its loopback dashboard address) to re-read its config.
Exactly three settings apply live: `Accounts`, `RetentionDays` and `HotDays`. Anything
else that differs — the listen addresses, upstream URL, database path, body policy and
cap, pprof address — is reported as needing `clens restart`. A file that does not load
or validate (for example `HotDays` above `RetentionDays`) is a 400 and changes
nothing: reload is all-or-nothing. The endpoint refuses non-loopback callers.

### Body archival

Bodies are about 99.5% of `lens.db`, so archival moves only them. Every event row and
every aggregate stays in `lens.db`; a call's request body, response body and transcript
content older than the hot window move to one file per UTC day,
`archive/bodies-YYYY-MM-DD.db`, beside the database, compressed. The dashboard, `show`,
`ls` and `export` read them back transparently and say so
(`bodies loaded from the archive (2026-09-20)`); if a file has been moved or deleted
the row says `archived — archive file for … not found`, not that nothing was captured.

- **Window:** `--hot-days N` / `CLENS_HOT_DAYS` / `HotDays` in the config file. Default
  `7`; `0` disables archival; `HotDays` greater than `RetentionDays` is rejected.
  `serve` archives at boot (after the listeners are up) and every 24 hours, and a
  `clens reload` applies a new window on the next cycle. It never blocks capture.
- **`clens archive status`** — the hot boundary, archived and unarchived row counts, the
  archive size, rows held back until `clens backfill-tool-names` has run, markers whose
  file or row is gone, and restored rows still sitting in a day file.
- **`clens archive run [--dry-run] [--yes]`** — archive now. Nothing is written without
  `--yes`; `--dry-run` reports the count.
- **`clens archive restore --since X --until Y [--dry-run] [--yes]`** — move bodies back
  into `lens.db` (a duration like `720h` or an RFC3339 time). Run it with `serve` stopped.
  A row whose archive copy is missing or unreadable is reported and left exactly as it
  was.
- `--retention-days` deletes archived bodies too: a purge collects the day-file rows
  it orphans and removes emptied files.
- A body that arrives after its row was archived (a late transcript merge) stays hot
  and is not re-archived.
- **The archive is as sensitive as `lens.db`.** It holds every prompt and file the agent
  read, in the same directory, with the same protection — and on Windows that is
  no better than the database's own (the file modes are a no-op there).
- The first `serve` boot with archival on archives everything already older than the
  window, in the background and without blocking capture. Measured on a copy of a real
  store (3.3 GB, 56,857 events): 31,891 rows in about two minutes, into about 21 MiB of
  day files, because the older rows are mostly transcript-only. Check progress with
  `clens archive status`.

### Reclaiming the hot file's space (one time)

Archiving deletes rows' bodies from `lens.db` but a SQLite file does not shrink on its
own, and `serve` never runs `VACUUM` (it would stall capture). After the first archival:

```
clens shutdown
clens purge --vacuum --yes
clens restart
```

### Before the first run of a new binary against a live store

The schema moves from version 4 to 5 (the `body_archive` marker table). Copy `lens.db`,
`lens.db-wal` and `lens.db-shm` to a separate path first; a bad migration against the
only copy is not recoverable.

### The auto-mode notice naming `127.0.0.1:8797`

Claude Code's auto-mode notice can blame the local address for a failed server-side
check. That is a misattribution: `clens`'s upstream here is DeepSeek's
Anthropic-compatible endpoint, so Anthropic's server-side checks can never run there.
Opt out with `CLAUDE_CODE_AUTO_MODE_SERVER=0`, or route to `api.anthropic.com` (which is
mutually exclusive with DeepSeek models). `clens` itself is byte-transparent to what
it forwards — a test pins that — so there is nothing to fix on this side.

## Dashboard

`clens serve` also starts a dashboard on `http://127.0.0.1:8798`: a
server-rendered shell over the same SQLite file, with an SSE stream for live
updates and hand-rolled inline SVG charts.

Its header carries a proxy-mode badge on every tab, because a dashboard that
looks healthy while the client is pointed at another port is the failure it
exists to catch: `proxy: active` when Claude Code's `ANTHROPIC_BASE_URL` names
this process and calls are arriving, `proxy: receiving, client elsewhere` when
calls arrive from a client pointed somewhere else, and `proxy: not receiving —
base URL not set in settings.json` when the variable only exists in your shell
— which is what `clens serve`'s own banner tells you to do.

| Tab | Shows |
|---|---|
| Overview | totals, and the most recent calls |
| Calls | the call log with filters, including a **from / to** date-time range (hour precision, either end optional); the **id** cell link replaces the list with that call's full request and response — including both sides' **headers** and their **bodies**, each in a collapsed box with its byte count. A capture the proxy could not finish says so, and a transcript-sourced row has no wire bodies at all, so it says *"not captured — transcript source"* rather than drawing empty boxes |
| Sessions | one row per run, with both cost models labelled side by side |
| Warnings | findings by kind, and one row per occurrence |
| Stats | totals over a window (a **from / to** date-time range, or `since`/`until` text such as `24h`), charted by day, week, or month |
| Sources | every collector's last success, last error, and rows written |
| Quota | per-account burn against each window, and candidate limits |
| Reconcile | computed vs billed cost, side by side per day and model |
| Models | the catalogue, plus every model traffic used, priced or not |
| Settings | health, and the rate table |

The API behind it:

| Route | |
|---|---|
| `GET /api/requests`, `GET /api/requests/{id}` | the call log and one call |
| `POST /api/requests/{id}/replay` | re-issue a call (opt-in, disabled by default) |
| `GET /api/stats` | totals by period, model, or cost source |
| `GET /api/sessions`, `GET /api/sessions/{id}` | sessions |
| `GET /api/warnings`, `GET /api/warnings/summary` | findings |
| `GET /api/sources` | collector health |
| `GET /api/quota` | burn, snapshots, calibration |
| `GET /api/reconcile` | computed vs billed |
| `GET /api/models` | the catalogue and its pricing state |
| `GET /api/accounts`, `POST /api/accounts` | configured accounts |
| `GET /api/prices`, `POST /api/prices` | the rate table and overrides |
| `POST /api/secrets` | store a credential |
| `POST /api/ingest` | run every collector once |
| `GET /api/health`, `GET /api/stream` | health, and the SSE stream |

`POST` routes reject a cross-origin request and require a same-origin
`Origin` header. A write route whose backing implementation is not wired
answers `503` with a reason — "nothing is wired" and "everything is fine and
nothing happened" are different claims, and a dashboard that renders them
identically is one you cannot trust about the difference.

## Warning kinds

Every kind has exactly one spelling, declared in `internal/analyze/kinds.go`
and checked against this table by `internal/analyze/readme_test.go`, so a
variant spelling here is a build failure rather than a typo someone eventually
finds. What each one means lives beside the kind in that file — one place, so
the two cannot drift apart — and `clens warnings --detail` prints it.

| kind | severity | emitted by |
|---|---|---|
| `cache_prefix_invalidation` | warn | analyze |
| `cache_prefix_below_minimum` | warn | analyze |
| `cache_breakpoints_exceeded` | error | analyze |
| `cache_write_never_read` | warn | analyze |
| `cache_ttl_mismatch` | info | analyze |
| `cache_expired_between_turns` | info | analyze |
| `cache_invalidated_by_tools` | warn | analyze |
| `cache_concurrent_write_race` | info | analyze |
| `thinking_budget_rejected` | error | analyze |
| `thinking_display_omitted` | info | analyze |
| `max_tokens_truncation` | warn | analyze |
| `refusal` | warn | analyze |
| `stream_incomplete` | error | analyze |
| `rate_limited` | warn | analyze |
| `overloaded` | error | analyze |
| `upstream_error` | error | analyze, consumer |
| `auth_kind_anomaly` | warn | analyze |
| `api_equivalent_cost` | info | analyze |
| `quota_window_approaching` | warn | quota |
| `cost_drift` | warn | reconcile |
| `source_mismatch` | error | store (cross-source or same-source merge) |
| `analyzer_panic` | error | consumer (panic recovery) |
| `peak_pricing` | warn | consumer, jsonlogs |

Five of these are not emitted by `analyze` at all — `quota_window_approaching`
comes from `internal/quota`, `cost_drift` from `internal/reconcile`,
`source_mismatch` from the store's cross-source or same-source merge, `analyzer_panic` from the
consumer's panic recovery, and `peak_pricing` from the capture path's own
pricers. They are declared in `kinds.go` because it is the single source of
truth for spellings project-wide, not only for what `analyze` emits today.

## Security posture

The premise is that your traffic and your prompts stay on this machine, and
the defaults are chosen to hold that:

- **Loopback-only.** Both listeners bind `127.0.0.1`. Reaching beyond that
  requires `--allow-remote`, deliberately.
- **Credentials never reach the database.** Not in headers — redacted before
  the bytes are teed. Not in bodies — the body policy plus a 2 MB cap. And
  the credential file itself, `~/.clens/secrets.toml`, lives outside the
  database entirely.
- **Protected by POSIX modes on Unix and an explicit Windows ACL on
  Windows.** Go's file-permission argument is a no-op on Windows, so `0600`
  there would be a false comfort; the Windows path sets an ACL instead.
- **The bodies are stored, and that is the point.** Full request and response
  bodies are captured, and they are readable from the dashboard's call detail
  as well as from `clens show`. So the content — every prompt and every file the
  agent read — is the asset this tool is protecting. `--body-policy off` narrows
  that; the database file is the thing to protect.
- **A transcript row's content is a reconstruction, not a capture.** The JSONL
  collector stores one assistant message's `content` in `transcript_content` /
  `transcript_role` — its own columns, never `req_body`, because a transcript
  excerpt is a reconstruction of intent rather than the request that produced
  it: no system prompt, no tool schemas, and nothing the proxy would have seen.
  A transcript also carries no headers at all, so such a row renders no header
  tables. The dashboard labels it *"reconstructed from transcript — not a wire
  capture"* to keep that provenance visible.
- **Fail open.** A broken observer never breaks your coding session: the proxy
  returns what upstream returned, or a synthesized error if upstream was
  unreachable, and a capture failure is logged rather than propagated.
- **The replay endpoint is off** unless `clens serve --replay` is passed.
- **Nothing is fetched from the network** by the dashboard: no CDN, no
  webfont, no analytics.

## Profiling

`clens` can serve `net/http/pprof` on its own loopback address for live
diagnosis — a hang, a CPU spike, a stuck goroutine — without attaching a
debugger, which would suspend the proxy, or sending it a signal, which would
kill it: either would take down the client whose traffic routes through this
process while you were trying to observe it.

It is off by default. Turn it on with `--pprof-addr 127.0.0.1:6060` (or
`CLENS_PPROF_ADDR`) and `clens doctor` names the flag when it is unset, so
the feature is discoverable rather than folklore. The address must be
loopback — `--allow-remote` does not widen this, because a profile is a dump
of whatever is in memory, which for this process includes prompt and
response bodies.

The four profiles worth pulling for a hang investigation:

```
curl "http://127.0.0.1:6060/debug/pprof/profile?seconds=30" -o cpu.pprof   # CPU
curl "http://127.0.0.1:6060/debug/pprof/heap" -o heap.pprof                # heap
curl "http://127.0.0.1:6060/debug/pprof/goroutine?debug=2" -o goroutines.txt  # every stack
curl "http://127.0.0.1:6060/debug/pprof/trace?seconds=5" -o trace.out      # execution trace
```

`goroutine?debug=2` is the single highest-value one for a hang: it dumps
every goroutine's stack as text, no `go tool pprof` needed to read it.

**The method that separates contention from a slow disk or a starved
runtime**: hit a route that touches no database (`/api/health`) alongside
one that does. If the no-DB route answers in a few milliseconds while every
DB route takes tens of seconds, that gap is the signature of
`SetMaxOpenConns(1)` contention on the store's single write connection, not
a starved runtime or a slow disk — both of those would slow the no-DB route
too.

**Confirm the process is actually busy before you sample.** Read
`TotalProcessorTime` twice, ten seconds apart; if it is under roughly 30% of
a core, the process is idle and a profile taken now will be a near-empty
file whose top frame is `runtime.(*timers).run` — evidence that reads as "no
bug" regardless of whether one exists. Wait for the symptom, then capture.

## Dependencies

Three modules beyond the standard library, and no more:

| Module | Why |
|---|---|
| `modernc.org/sqlite` | the pure-Go SQLite driver — no cgo, so the single static binary story holds |
| `github.com/klauspost/compress` | zstd, for bodies a client sent with `Content-Encoding: zstd` |
| `github.com/andybalholm/brotli` | brotli, for the same reason on `br` |

The compression libraries exist because decoding happens in the cold path: the
proxy tees bytes and returns, and the consumer decompresses on its own
goroutine. A claim that this tool had only one non-stdlib dependency could not
be true alongside "we decode `br`/`zstd`" — see
`docs/planning/GI-1-claude-lens-v1.md` §Infrastructure.

## Documentation

| | |
|---|---|
| Architecture, schema, cost model, conventions | [docs/context/INDEX.md](docs/context/INDEX.md) |
| The design and its decision log | [docs/planning/GI-1-claude-lens-v1.md](docs/planning/GI-1-claude-lens-v1.md) |
| The acceptance run, as executed | [docs/acceptance.md](docs/acceptance.md) |
