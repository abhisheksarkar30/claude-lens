<!-- version=1, status=planning -->
# GI-25 — Local LLM provider toggle with its own billing mode, mid-stream truncation recorded as complete, and transient store errors dropping events

Issue: https://github.com/abhisheksarkar30/claude-lens/issues/25
Branch: `GI-25-local-llm-toggle-and-store-hardening` (off `main` at `a4f1a31`)
Beads: `.beads/GI-25/` (filled in at Phase 3)

## 1. Problem

Three items, one ticket:

1. **Cloud/local toggle.** The proxy forwards to exactly one upstream, parsed once at construction
   (`internal/proxy/proxy.go:33-44`). The operator wants a second, local upstream (LM Studio today)
   configured *alongside* the cloud one, a dashboard toggle that switches between them without a
   restart, and, for local traffic, tokens plus a **projected** cost (same rate chart as the cloud
   provider) tracked separately from real spend. User decision: local rows get **their own billing
   mode** (`local`), kept out of quota burn and out of every cost sum.
2. **Truncation.** The operator keeps seeing truncated request/response captures and suggested
   shortening the archive/retention window.
3. **Log triage.** The pasted log lines: stdlib `ReverseProxy read error during body copy`,
   `disk I/O error (266)`, `cannot allocate N bytes`/`out of memory (7)`, `jsonlogs: open … cannot
   find the file`, and `archived the bodies of N row(s)`.

Reproduction status: none of the three was reproduced here. Item 2 and the first line of item 3
are one mechanism established by reading the stdlib and `proxy.go` (see R2). The SQLite errors are
a 21-second burst on 2026-10-09 20:51 that I cannot replay; the plan makes them survivable and
visible rather than claiming a root cause for the burst itself (R3).

## 2. Requirements and verified root causes

| # | Requirement | Root cause (verified in code) | Decision |
|---|---|---|---|
| R1 | Two parallel targets (cloud + local), seamless UI switch, local tracked separately with projected cost | One upstream baked into a closure: `proxy.go:33-44`. `UpstreamURL` is not a live field: `reload.go:81` (`liveFields` = Accounts, RetentionDays, HotDays). No toggle control exists in `index.html`; `/api/mode` is read-only (`api.go:216`). Billing sums key on `billing_mode = 'api'` / `'subscription'` only (`store.go:805-806,1674-1675,1739`), so a third mode is excluded from both sums by construction | New `local` billing mode. Active provider is a live-swappable value read in `Rewrite`; persisted in a state file; switched by `POST /api/provider`. Local cost goes to `api_equivalent_cost_usd` (projected column), surfaced through a **third, separately labelled aggregate**. Every local token is priced as `deepseek-flash` at its off-peak base rate (user decision, §3.4) |
| R1a | Local money must never reach `cost_usd` | Three `default:` branches route every non-`subscription` mode to `cost_usd`: `consumer.go:359-364`, `jsonlogs.go:487-492`, `store.go:2203-2208` (`routed()`), and `merge.go:361-377` derives a missing mode from the cost columns. A bare new constant would leak `local` money into the real-spend column | Each of the four sites gets an explicit `local` case routed to `api_equivalent_cost_usd`; a schema invariant test pins the pair |
| R1b | Local rows out of quota burn | Burn is keyed by account name (`api/quota.go:119,125` → `quota.ComputeBurn(…, acct.Name, "", …)`), and only `billing_mode == "subscription"` accounts are iterated (`quota.go:114`, `cli/quota.go:47`). A local row carries account `local`, which matches no subscription account | Fixed account name `local`; no quota code change, one regression test |
| R1c | Local JSONL transcripts must not be double-counted as `subscription` | The tailer classifies per row by model prefix (`jsonlogs.go:431-437`); an unlisted local model lands in `subscription` with its own (likely unpriced) cost, and `merge.go:341` takes the *winner's* mode, so a JSONL winner would flip a proxy `local` row | `LocalModelPrefixes` config mirroring `ApiModelPrefixes`; merge treats `local` as sticky |
| R2 | Truncated captures handled | **`httputil: ReverseProxy read error during body copy` is logged when `src.Read` fails, `$GOROOT/src/net/http/httputil/reverseproxy.go:648-650`: `src` is the upstream body, so this is the upstream (3.173.21.63:443, the DeepSeek edge) dropping the stream mid-response. A client cancel is excluded (`rerr != context.Canceled`). `ErrorHandler` does not fire after headers are sent (`proxy.go:50-55`). The tee never records the read error, so `onClose` submits `captureComplete = !respBuf.truncated && !reqBody.truncated` (`proxy.go:107-108`), i.e. `true`, for a body cut mid-stream.** The body cap (8 MiB per `~/.clens/config.toml`) is a different, already-marked mechanism | Record the read error on the tee; submit `captureComplete=false` and an `upstream_stream_aborted` warning; replace the stdlib's anonymous log line with a labelled one carrying the method, path and bytes received. Retrying a half-streamed response is impossible and is **not** attempted |
| R2a | Retention duration | Archival is `--hot-days` (default 7, `config.go:86`), live-reloadable; deletion is `--retention-days` (default 0). Neither touches what a capture stores. The live DB is 19.6 GB with a 6.5 GB archive, and no `VACUUM` ever runs, so archiving does not shrink the file | **Not a code change.** Operator recommendation in §7: lower `HotDays`. Defaults unchanged |
| R3 | SQLite `disk I/O error (266)`, `cannot allocate … bytes`, `out of memory (7)` | `Open` sets only `busy_timeout`, `foreign_keys`, `journal_mode(WAL)` (`store.go:74-82`) and one connection. `busy_timeout` retries only lock contention. No retry exists for IOERR/NOMEM anywhere in `internal/store` or `internal/consumer`; `flush` logs and drops (`consumer.go:233-238`, `287-291`, `272-274`). Error 266 is `SQLITE_IOERR_READ`; the burst of IOERR + NOMEM within seconds points at host resource exhaustion on a 19.6 GB file with 15 GB RAM, **hypothesis, unchecked** | Bounded retry with backoff for transient classes on the three consumer write sites; classified counters on the consumer's `Stats`, `doctor` and `/api/health` so a burst is visible, not just a log scroll. No pragma change on speculation (§9) |
| R3a | `jsonlogs: open … cannot find the file` | TOCTOU between `walkFiles` (`jsonlogs.go:223,245-262`) and `os.Open` (`288-291`): Claude Code removed the file between the two. The cursor row is keyed by path; a vanished file simply stops appearing | `errors.Is(err, fs.ErrNotExist)` → silent skip. Other open errors still log |
| R3b | `archived the bodies of N row(s)` | Informational line, `internal/cli/serve.go:416`, once at boot and every 24 h | No change |

Decisions taken with the user: local rows have their own billing mode. Local server is currently
**LM Studio**.

## 3. Design

### 3.1 R2 — record a mid-stream upstream abort

`teeCloser` (`proxy.go:302-316`) gets a `readErr error` captured in `Read` for any error that is
neither `io.EOF` nor `context.Canceled` (the same filter the stdlib applies). `onClose` passes it as
`callErr` and forces `captureComplete=false`. `consumer.processCall` already turns `call.Err` into
an `upstream_error` warning (`consumer.go:368-374`); the abort reuses that warning with a
distinguishing `Detail` prefix rather than a new kind, so `nonAnalyzeKinds` and the warnings catalogue
are untouched. `ReverseProxy.ErrorLog` is set to a `*log.Logger` whose writer rewrites the stdlib's
line into `proxy: upstream stream aborted: <err>` (labelled, one line). Hot-path rule: no parsing, no
blocking, no new import (proxy still depends only on `sink` and `config`).

### 3.2 R3 — transient-error retry in the consumer

New `internal/consumer/retry.go`: `isTransient(err)` matches SQLite primary codes IOERR (10), NOMEM (7)
and BUSY (5), using `*sqlite.Error` from `modernc.org/sqlite` (already a dependency; confirm the
exact accessor in bead 03 before writing the matcher). `withRetry(ctx, fn)` makes up to 4 attempts
with 250 ms / 1 s / 4 s backoff, abandoning on `ctx` cancel. Applied at `InsertEvent`, the
`SessionEventsForRules` read, and `RecordCall`. The consumer goroutine blocks during backoff; the
sink is bounded and the hot path never waits on it, so a long burst drops captures at the sink with
the existing `proxy: dropped capture` log. That trade (stall the cold path, never the client) is the
fail-open rule, stated rather than hidden. Counters: `transientRetries`, `transientGaveUp` on
`Stats`.

### 3.3 R3a — vanished transcript

`tailFile` open branch: `errors.Is(err, fs.ErrNotExist)` returns zero `Stats` without logging.

### 3.4 R1 — billing mode `local`

Constants in `internal/config` (`BillingLocal = "local"`, `LocalAccount = "local"`); the proxy cannot
import `store`, so config is the shared home. Routing, all explicit:

| Site | Change |
|---|---|
| `consumer.go:352-365` | `call.Provider == "local"` → account `LocalAccount`, mode `local`; priced as `LocalPriceModel` at `LocalPriceAt` (below), never as the row's own model; `local` → `ApiEquivalentCostUSD` |
| `jsonlogs.go:431-437,487-492` | `LocalModelPrefixes` match → mode `local`, account `local`, same pricing substitution; `local` → `ApiEquivalentCostUSD` |
| `store.go:2203-2208` `routed()`, `2300` | `local` → `(apiEquivalentCostUSD, true)`; the `table.Compute` call at `2300` substitutes `LocalPriceModel`/`LocalPriceAt` for a `local` row so a reprice cannot move a local row onto its own (unpriced) model |
| `merge.go:341-377` | if either side is `local`, merged mode is `local`; cost columns follow the `local`-marked side, not the token winner |
| `store.go:805-806`, `1674-1675` | add `SUM(CASE WHEN billing_mode = 'local' THEN api_equivalent_cost_usd END)` as a third aggregate; `StatsSummary` gains `TotalLocalProjectedCostUSD`; both scan helpers (`1680-1731`) extended |

**Local pricing rule (user decision): every local token is charged at DeepSeek's base, cheapest
rate.** `internal/config` holds `LocalPriceModel = "deepseek-flash"` ($0.15 in / $0.60 out / $0.003
cache-read per MTok, `pricing/table.go:176`; `deepseek-v4-flash` is the same price, `deepseek-v4-pro`
is dearer) and `LocalPriceAt`, a fixed Saturday instant. "Base" means the off-peak rate: the 2x peak
window (`pricing.go:114,140-142`) is cloud demand pricing and is meaningless for a model running on
this machine, and passing a real `at` would also raise a false `peak_pricing` warning
(`consumer.go:396-406`). The consumer, the tailer and `RepriceCosts` all price a `local` row via
these two constants and skip `peakWarning` for it. The model name stored on the row stays the real
local model (`lmstudio-community/…`), so the Models tab still shows what ran; only the *price lookup*
is substituted. No per-model map and no config key: one rule, as asked. The `Compute()` signature is
untouched; the substitution happens in the three callers. A guard test pins that `LocalPriceModel`
exists in `ShippedTable()`, is off-peak at `LocalPriceAt`, and is not dearer than any other shipped
`deepseek-*` row, so a table edit cannot silently re-point it or make the label "cheapest" false. A
user override of `deepseek-flash` in `prices.toml` flows through, which is the intended single place
to change the projection.

Sessions: `reconcileSessionTx` sums stay as they are, so a session's real and subscription totals
exclude local rows (invariant 5 holds). Session *token* totals still include local tokens; a session
that spans a mid-session toggle is the one place the two mix, recorded in §6 R4 and not solved.
No schema change: `billing_mode` has no CHECK (`schema.sql:21`), so no migration and no
backup-before-migration step. `Compute()` is untouched (`pricing.go:94-152`).

### 3.5 R1 — the provider switch

`config.Config` gains `LocalUpstreamURL` and `LocalModelPrefixes` (flat keys, env
`CLENS_LOCAL_*`, flags — the shape of `config.go:206-207,249`). The cloud target stays
`UpstreamURL`. `Validate` requires `LocalUpstreamURL` to parse as http(s) when set and rejects a
non-loopback host unless `AllowRemote`, mirroring the existing listener guard (the proxy carries
credentials).

`internal/proxy`: `type Targets struct{ cloud, local *url.URL; active atomic.Pointer[string] }`
with `Active() string` and `Set(string) error` (`"cloud"` | `"local"`; `local` rejected when no local
URL is configured). `New` reads `targets.Load()` inside `Rewrite` instead of closing over `target`
(`proxy.go:42-44`). Each request stamps `st.provider` from the same read, so the target and the tag
cannot disagree for one request; `sink.CapturedCall` gains `Provider string`.

Persistence: `<dir of DBPath>/provider` holds one word, written atomically on `Set`, read at boot;
absent or unrecognised → `cloud`. Not a config-file rewrite (the file is hand-annotated, `serve`'s
`config.toml` comments would be destroyed).

API: `GET /api/provider` → `{active, cloud_url_host, local_url_host, local_configured}`;
`POST /api/provider {"active":"local"}` guarded exactly like `reload`/`shutdown`
(`reload.go:26-35`: `originReject` + loopback caller). The write-guard count in
`api-surface.md`/`security-and-permissions.md` goes three → four. The dashboard listener is
`0.0.0.0` on this machine, so the loopback-caller check is load-bearing: switching providers
redirects where credentials are sent.

UI: a header segmented control `cloud | local` beside `#proxy-mode` (`index.html:25`), a muted
state when `local_configured` is false, and the call/stat views gain a `local` option in `#f-billing`
(`index.html:57-61`) and a labelled `local (projected)` figure next to the `api` and `sub` totals
(`index.html:17-18`). Vanilla JS, no build step (ADR 006). The credential issue: Claude Code sends
its configured key to whichever upstream is active. LM Studio ignores it, but switching back to cloud
never involves clens holding a key, so nothing is stored.

LM Studio: it must expose an Anthropic-compatible `/v1/messages`. That is **unverified here** and is
the first thing bead 05 checks by hand against the running server before any code depends on it.

## 4. Change list

### 4.1 Files
Modified: `internal/proxy/proxy.go`, `internal/sink/sink.go`, `internal/consumer/consumer.go`,
`internal/jsonlogs/jsonlogs.go`, `internal/store/store.go`, `internal/store/merge.go`,
`internal/config/config.go`, `internal/cli/serve.go`, `internal/cli/doctor.go`, `internal/api/api.go`,
`internal/api/health.go` (or wherever `/api/health` lives), `internal/web/index.html`,
`internal/web/app.js`, `internal/web/style.css`.
New: `internal/consumer/retry.go`, `internal/proxy/targets.go`, `internal/api/provider.go`, and a test
file beside each.

### 4.2 Explicitly not touched
- **`internal/pricing`** — `Compute()` is unchanged (`pricing.go:94-152`); local pricing is a model
  and instant substitution *before* the call, never a table edit. The test in §5.2 reads the
  table but nothing in the package changes.
- **`internal/quota`** — keyed by account name; the fixed `local` account is the whole exclusion.
- **`store/schema.sql`, `migrations`** — no column, no `schemaVersion` bump.
- **SQLite pragmas** — no `cache_size`/`temp_store`/`synchronous` change (§9).
- **`HotDays`/`RetentionDays` defaults** — an operator setting, §7.
- **`sessions.total_*` columns** — local excluded; see R4.
- **`internal/jsonlogs/cursor.go`** — orphan cursor rows for deleted transcripts are inert.

## 5. Test strategy

### 5.1 What is genuinely at risk

| Risk | Coverage |
|---|---|
| Local money lands in `cost_usd` | Extend `TestBillingModeInvariants` to the `local` shape; one case per routing site |
| Merge flips `local` → `subscription` | Merge test: proxy `local` row + JSONL `subscription` row, both winner orders |
| Hot path blocks or buffers | Existing TTFB gate must stay green; new abort test uses a slow-streamed fake upstream |
| Switch races a request | `-race` test: concurrent `Set` and requests; each stamped provider equals the URL that served it |
| Retry masks a permanent error | `isTransient` table test, incl. a constraint error that must not retry |
| Retry stalls shutdown | Cancel-during-backoff test |

### 5.2 New tests
- `TestMidStreamUpstreamAbortIsMarkedIncomplete` — fake upstream sends headers, part of the body, then
  closes the connection; asserts `CaptureComplete=false` and the warning.
- `TestCancelledClientIsNotAnAbort` — `context.Canceled` leaves `CaptureComplete=true`.
- `TestLocalRowsPriceIntoApiEquivalentColumn`, `TestLocalSumIsNeverMergedIntoApiOrSub`.
- `TestLocalRowIsPricedAsDeepseekFlashBase` — an unknown local model name still prices (not
  `unpriced`) to exactly the `deepseek-flash` figure, and a call timestamped inside the peak window
  still prices at base with no `peak_pricing` warning. Negative control: price with the real `at` →
  the figure doubles and the warning appears.
- `TestLocalPriceModelIsTheCheapestShippedDeepseekRow` — the guard from §3.4.
- `TestLocalRowsAreAbsentFromQuotaBurn` — a `local` row inside the 5 h window leaves a subscription
  account's burn unchanged.
- `TestMergeKeepsLocalSticky`, `TestRepriceRoutesLocalToApiEquivalent`.
- `TestTargetsSwitchIsLiveAndTagged`, `TestProviderPersistsAcrossRestart`,
  `TestProviderEndpointRejectsNonLoopbackAndForeignOrigin`.
- `TestWithRetryRetriesTransientOnly`, `TestWithRetryStopsOnCancel`.
- `TestTailFileSkipsVanishedFileQuietly`.

### 5.3 Not covered by any automated test
- A real LM Studio `/v1/messages` round trip and the dashboard toggle's look: checked by hand in
  bead 06 against the running server, because neither exists in CI.
- The 2026-10-09 burst itself: not reproducible; only the handling is tested.

### 5.4 Negative control (required)
Each test above is run once with its fix reverted and must fail: abort test fails with
`CaptureComplete == true`; the `local` invariant test fails when `routed()` is left on `default:`
(the local figure then appears in `cost_usd`); the merge test fails with `merged.BillingMode ==
"subscription"`; the retry test fails with one attempt instead of four.

## 6. Risk areas
- **R1 — Credential redirection.** The toggle changes where the caller's API key is sent. Guard it
  like `reload` (origin + loopback), and do **not** allow `local` to be set to a non-loopback URL
  unless `AllowRemote`.
- **R2 — Hot path.** `Rewrite` reads one atomic pointer. Do **not** add a lock, an allocation per
  request beyond the stamp, or any parse.
- **R3 — Retry stalls the consumer.** Bounded to ~5.25 s per write; the sink bound turns a long
  outage into logged drops, not unbounded memory.
- **R4 — Sessions spanning a toggle.** Session token totals include local tokens; money does not.
  Accepted and documented rather than adding a per-session breakdown.
- **R5 — A fixed `local` account name.** A user account literally named `local` would collide.
  `Validate` rejects it.
- **R6 — No new migration.** If a later bead adds a column, the user CLAUDE.md backup rule applies
  to `lens.db`, `-wal`, `-shm` before it runs.

## 7. Pre-flight (needs a decision before bead 01)
- Confirm LM Studio's version serves `/v1/messages` and its default port; if it serves only the
  OpenAI shape, R1 needs a translation layer and the plan stops for a user decision.
- **Operator action, no code:** the live DB is 19.6 GB. Setting `HotDays = 2` in
  `~/.clens/config.toml` and running `clens reload` is the retention lever you asked about; it does
  not shrink the file without a `VACUUM`, which is a separate, slow, locking operation and is not
  run here.
- Suggest `PRAGMA quick_check` on a copy of the DB: error 266 is a *read* failure, and a 19.6 GB
  file read failing is worth ruling out as corruption.

## 8. Beads

| # | Bead | Priority | Depends on | Purpose |
|---|---|---|---|---|
| 01 | `fix-stream-abort-capture` | P0 | — | R2: record upstream read errors; labelled log |
| 02 | `fix-jsonlogs-vanished-file` | P2 | — | R3a: quiet skip on `ErrNotExist` |
| 03 | `feat-transient-store-retry` | P1 | — | R3: bounded retry, counters in doctor/health |
| 04 | `feat-local-billing-mode` | P0 | — | R1a–c: config keys, routing at four sites, third aggregate |
| 05 | `feat-provider-switch` | P0 | 04 | R1: `Targets`, stamping, state file, `LocalPriceAs` |
| 06 | `feat-provider-api-and-ui` | P0 | 05 | R1: `/api/provider`, header toggle, filter, local tile |

01–04 are independent; 05 needs 04's constants; 06 needs 05. 04 is the riskiest (an invariant), 03 the
largest by surface. Context docs are refreshed in Phase 5.6, not in a bead.

## 9. Alternatives considered and rejected
**Reuse `billing_mode = 'subscription'` for local rows and filter by account.** Rejected by the user,
and on evidence: every `default:` branch plus the quota loop would then rely on a name match.
**Retry a half-streamed response.** Rejected: the client has received bytes; a replay would duplicate
them. **Add a `provider` column.** Rejected: `billing_mode` already carries the distinction and ADR 001
forbids a second flag that could let a query re-merge real and hypothetical cost.
**Tune `cache_size`/`temp_store` pragmas now.** Rejected: no evidence the burst was cache-bound, and
the earlier session-pass defect that did spill is already fixed (`store.go:400-416`). Revisit if the
new counters show a recurring pattern.
**Rewrite `config.toml` from the toggle.** Rejected: destroys the operator's comments; a one-word
state file is enough.
**Shorten retention to cure truncation.** Rejected: retention does not influence what a capture stores.

## 10. Self-review

**Senior engineer.** The real decision is stamping the provider on the request at `Rewrite` time and
carrying it to the row, instead of inferring it later from the model name. Inference was the rejected
alternative: a local model and a cloud model can share a name. The cost is one field on
`CapturedCall`.

**QA engineer.** The case most likely to be silently untested: a *merge* between a `local` proxy row
and the same call's JSONL transcript, when the JSONL side wins the token pick. §5.2 pins both orders.

**Security engineer.** The toggle redirects credentials, so `POST /api/provider` takes the
`originReject` + loopback guard of `reload`; the dashboard binds `0.0.0.0` here, so the guard cannot
be skipped. `LocalUpstreamURL` is validated and refused off-loopback without `AllowRemote`. The state
file holds one non-secret word. Local bodies are captured under the same redaction and cap as cloud.

## 11. Context docs to refresh (running list)
- `docs/context/cost-and-quota.md:19-39` — "all seven plan types fold to two billing modes" → three; add `local`.
- `docs/context/architecture.md` — "one upstream" → two targets, live-swappable.
- `docs/context/api-surface.md:92` — write-guard count three → four; add `/api/provider`.
- `docs/context/security-and-permissions.md` — fourth write guard; credential-redirection note.
- `docs/context/storage-schema.md` — `billing_mode` vocabulary gains `local`.
- `docs/context/workflows.md` — flow 1 gains the abort path; flow 7 notes provider switch needs no restart.
- `docs/context/build-and-run.md`, `cli-and-tooling.md` — three new config keys; `doctor` store-error counters.
- `docs/context/dashboard.md` — the header toggle and the local tile.
- `docs/context/glossary.md` — `local`, `provider`.
- `docs/context/INDEX.md` — refresh entry; test-file count.

## Change History

### v1 (draft)
Pricing rule revised before first review, on the user's instruction: every local token is charged at
DeepSeek's base, cheapest rate (replaces an earlier draft's configurable `LocalPriceAs`). Written against `main` at `a4f1a31` after reading `proxy.go`, `consumer.go`, `merge.go`,
`jsonlogs.go`, the billing sums in `store.go`, `reload.go`, and the Go 1.24.1 `reverseproxy.go`. An
Explore-agent report contributed the config and quota map; its claim that the stdlib line means a
*client* disconnect was wrong and is corrected in R2.
