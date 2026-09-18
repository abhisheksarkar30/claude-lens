# Bead br-GI-1-17: CLI subcommand dispatch + the 12 carried-over commands (incl. serve wiring)

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §CLI, §API surface (composition root, injected seams), §Architecture, §Security posture (replay footgun), §Carried over (replay, purge, stats, pagination), §Infrastructure

- **Bead ID**: br-GI-1-17
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-03, br-GI-1-07, br-GI-1-08, br-GI-1-09, br-GI-1-10, br-GI-1-14, br-GI-1-16
- **Blocks**: br-GI-1-19

> **Dependency note.** The plan lists `03, 07, 08, 14, 16`. This bead **also needs br-GI-1-09 and
> br-GI-1-10**, because `serve` is the composition root and is where the analyzer implementations are
> registered on the consumer — br-GI-1-08's step 8 and br-GI-1-10's `Blocks` field both already assume
> 17 performs that wiring. The eleven lower-numbered collector/CLI beads (11, 12, 13, 15) arrive
> transitively through br-GI-1-14. Every added edge points to a lower number, so the DAG stays
> executable. See the summary flag.

## Description

Two things, which are one deliverable because they meet in `serve`: the dispatch table that turns 18
names into functions, and the composition root that builds the whole process from the packages the
earlier beads landed.

**Dispatch (`cmd/clens/main.go`).** br-GI-1-01 created the file with an **empty** `commands` map and a
`main()` that prints `clens <name>: not implemented yet` for an unknown name. This bead appends the
**twelve carried-over names** — `doctor`, `serve`, `ls`, `show`, `tail`, `stats`, `sessions`,
`warnings`, `export`, `prices`, `replay`, `purge` — mirroring `deepseek-lens/cmd/lens/main.go`.
br-GI-1-15 owns the six new names and appends them to the same map literal; the two edits are
append-only and own disjoint keys, which is what keeps one file from being a conflict (see the summary
flag). `doctor` is in the map here but its implementation is br-GI-1-01's, extended by br-GI-1-14.

**`internal/cli/serve.go` — the composition root.** This is the **only** place the whole tree is
assembled, and therefore the only place `config`, `secret`, `ingest`, `api`, and `web` are imported
together. It mirrors `deepseek-lens/internal/cli/serve.go`:

1. `config.Load(args)` + `cfg.Validate()` (loopback unless `--allow-remote`).
2. `store.Open(cfg.DBPath)`; run the `RedactCheck` startup self-test (log-and-continue, never refuse
   to start — fail open); run the retention purge once at startup **and** on a 24-hour ticker (both
   no-op when `RetentionDays <= 0`). Split `checkRedaction` and `purgeOnStartup` into their own
   functions so they are testable without the two listeners.
3. `sink.New(sink.DefaultCapacity)`.
4. `api.NewBroker()` and `api.NewPublishingStore(st, broker)` — the SSE decorator wraps the **bare**
   store so the consumer's writes publish; read endpoints keep the bare `*store.Store` so a read can
   never trigger a publish.
5. `session.New(st, cfg.SessionGapMinutes)` — one object is both halves (pre-insert resolver,
   post-insert aggregator).
6. `consumer.New(sk, pubStore, sess, analyzers)`; then `SetSessionAggregator(sess)`,
   `SetPriceTable(pricing.NewLoader(pricing.DefaultPath()))` (live reload, so `clens prices --set`
   takes effect without a restart), `SetBodyDecoding(cfg.BodyCapBytes)` (the decode limit is the same
   `BodyCapBytes` the proxy tees with). The analyzer implementations from br-GI-1-09/10 are passed in
   here, not defaulted inside `consumer.New`, so tests that build a Consumer without them keep their
   exact warning counts.
7. `proxy.NewServer(cfg, sk)`; keep `proxySrv.Handler` for the replay route.
8. `api.New(st, sk, cons, broker, web.Files, proxySrv.Handler, cfg.ReplayEnabled)` then wire **all
   four** seams added in br-GI-1-16 — `SetPricing(pricing.DefaultPath())`,
   `SetCredentialWriter(secret.Save)`, `SetAccountWriter(accounts save)`,
   `SetIngestTrigger(ingest.RunOnce)`. This is the wiring the plan's *API surface* section names; the
   setters themselves are br-GI-1-16's (so this bead compiles before br-GI-1-18 adds the routes).
9. Start the br-GI-1-14 scheduler; run `cons.Run(ctx)` on its own goroutine.
10. Two `http.Server`s (proxy `8797`, dashboard `8798`), `signal.NotifyContext`, and a bounded
    shutdown (`shutdownGrace`) that shuts both servers down and waits for the consumer's own drain.
11. `printBanner` on stdout: the copy-pasteable `export ANTHROPIC_BASE_URL=http://<proxy-addr>` line,
    the dashboard URL, and (when capture is off) the standing "nothing is being recorded" warning.

**The eleven remaining carried-over commands** (unchanged names and flags from deepseek-lens):

| Command | What it does |
|---|---|
| `clens ls` / `show` / `tail` | Call log, one call's detail, live follow |
| `clens warnings` | Grouped by kind; `--detail` for one row per occurrence |
| `clens sessions` | Sessions with turns, tokens, the **two labelled cost figures**, warning counts |
| `clens stats` | Window totals **split by `billing_mode`**, per-model split, unpriced count; `--by model\|day\|session\|project`, `--period`, granularity |
| `clens export` | Every stored event as JSON lines; `--csv` |
| `clens prices` | Effective table; `--set`, `--unset`, `--edit` (calls br-GI-1-07) |
| `clens replay <id>` | Re-issue a captured call; `--set`, `--diff`, `--dump`, `--yes` |
| `clens purge` | Delete by age or the unpriced predicate; `--older-than`, `--unpriced`, `--dry-run`, `--yes`, `--vacuum` |

`internal/cli/format.go` (shared formatting) lands here. The body-edit and re-issue logic
(`internal/replay`) is **not** this bead's: it ships with br-GI-1-16, whose replay route calls it; the
CLI `replay` command here consumes that package.

**Billing discipline these commands must not break** (invariant 5, F2.2):

- `clens sessions` renders **two labelled figures** — `total_cost_usd` (API-billed) and
  `total_api_equivalent_cost_usd` (subscription), each NULL when that side is empty, with
  `unpriced_count` alongside. It never prints one mixed total and never `$0.00` for a subscription or
  unpriced session.
- `clens stats` reports two labelled totals, never one merged `SUM(cost_usd)`, and shows the
  `unpriced` count alongside the total rather than folded in.
- `clens export` emits both cost columns as they are stored; an unpriced row exports NULL, never `0`.

**Replay safety (carried over verbatim).** Off by default: the endpoint answers 403 until
`clens serve --replay` sets `cfg.ReplayEnabled`, and the CLI path requires the same opt-in. Every
replay re-issues through `proxySrv.Handler` (same transport, tee, and writer), carries the
Origin/Host allowlist, and passes the **cost gate** — a replay whose original was expensive or
`unpriced` is refused without `--yes`. `--dump` never sends anything.

## Rationale

Without `serve` there is no product: it is the one place the hot path, the cold path, the collectors
and the dashboard API are wired to each other, and the one place the write seams are bound. Splitting
dispatch from wiring would leave neither runnable. The billing discipline lives here because the CLI
is the surface where a merged total would be most visible and most wrong.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `clens` with no argument prints usage and exits non-zero; an unknown name prints
  `clens <name>: not implemented yet` and exits non-zero (br-GI-1-01's `main`).
- All 18 subcommands dispatch (`doctor`, `serve`, + this bead's 11, + br-GI-1-15's 6).
- `clens serve` binds `8797`/`8798`, prints the `ANTHROPIC_BASE_URL` line, answers on both listeners,
  and shuts down on SIGINT within the grace bound; `RedactCheck` runs at boot.
- `clens sessions` prints two labelled cost figures; a subscription-only session prints neither
  `$0.00` nor a merged total.
- `clens stats` splits by `billing_mode` and supports `--by project`.
- `clens replay` is refused without `--replay`; a costly/`unpriced` original is refused without
  `--yes`; `--dump` sends no request.
- `clens purge` deletes nothing with `--dry-run`, and refuses to delete without `--yes`.
- `internal/api` and `internal/web` still do not import `internal/secret` (br-GI-1-16's import guard
  stays green) — only `cli/serve` does.

## Test Specifications

- Unit Tests (`internal/cli/cli_test.go`):
  - The dispatch table contains all 18 names; an unknown name exits non-zero.
  - Each of this bead's eleven commands runs against a temp store and exits 0.
  - **Sessions split**: a mixed fixture prints two labelled figures; a subscription-only fixture
    prints no `$0.00` and no merged total.
  - **Stats split**: a mixed fixture yields two labelled totals; `--by project|model|day|session`
    each returns the expected grouping; `--period` is honoured.
  - **Replay `--dump`**: a fake transport records zero requests.
  - **Replay opt-in**: with `ReplayEnabled` false the route refuses; with a costly original and no
    `--yes` the CLI refuses before sending.
  - **Purge**: `--dry-run` deletes nothing; no `--yes` is refused; `--vacuum` runs only with `--yes`.
  - **Export**: JSON-lines round-trips every stored event; `--csv` emits a header row; an unpriced row
    exports an empty cost cell, not `0`.
  - Credential containment: no command's stdout/stderr contains a `sessionKey` or `sk-ant-admin…`
    value (test 18's CLI half).
- Unit Tests (`internal/cli/serve_test.go`):
  - `checkRedaction` logs and continues (never errors out the boot).
  - `purgeOnStartup` no-ops for `RetentionDays <= 0` and deletes for a positive value (split out so
    it is testable — `Serve` itself cannot be driven from a test).
  - `printBanner` prints the `ANTHROPIC_BASE_URL` line and the capture-off warning.
  - Import guard: only `internal/cli/serve.go` imports `internal/secret`; `internal/api` and
    `internal/web` do not.
- Unit Tests (`internal/cli/replay_test.go`):
  - `--set` edits the stored body through br-GI-1-16's `replay.ApplyEdits`; `--diff` compares two
    captures; a malformed `--set` fails before anything is sent.
- Integration Tests: `clens prices --set` then a consumer run sees the new rate without a restart
  (the `Loader` live-reload path).
- E2E (opt-in, also the br-GI-1-19 acceptance step): `clens serve`, point `ANTHROPIC_BASE_URL` at it,
  make one real call, confirm one `events` row and one SSE delivery.

## Files to Touch

- `cmd/clens/main.go` (modify — append the twelve carried-over command entries)
- `internal/cli/serve.go` (create — the composition root; the only importer of `secret`)
- `internal/cli/ls.go`, `internal/cli/show.go`, `internal/cli/tail.go`, `internal/cli/warnings.go`,
  `internal/cli/sessions.go`, `internal/cli/stats.go`, `internal/cli/export.go`,
  `internal/cli/prices.go`, `internal/cli/replay.go`, `internal/cli/purge.go`,
  `internal/cli/format.go` (create)
- `internal/cli/cli_test.go`, `internal/cli/serve_test.go`, `internal/cli/replay_test.go` (create)
