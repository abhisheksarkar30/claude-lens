<!-- version=7, status=converged -->
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
| R1 | Two parallel targets (cloud + local), seamless UI switch, local tracked separately with projected cost | One upstream baked into a closure: `proxy.go:33-44`. `UpstreamURL` is not a live field: `cli/reload.go:81` (`liveFields` = Accounts, RetentionDays, HotDays). No toggle control exists in `index.html`; `/api/mode` is read-only (`api.go:216`). Billing sums key on `billing_mode = 'api'` / `'subscription'` only (`store.go:805-806,1674-1675,1739`), so a third mode is excluded from both sums by construction | New `local` billing mode. Active provider is a live-swappable value read in `Rewrite`; persisted in a state file; switched by `POST /api/provider`. Local cost goes to `api_equivalent_cost_usd` (projected column), surfaced through a **third, separately labelled aggregate**. Every local token is priced as `deepseek-flash` at its off-peak base rate (user decision, §3.4) |
| R1a | Local money must never reach `cost_usd` | Three `default:` branches route every non-`subscription` mode to `cost_usd`: `consumer.go:359-364`, `jsonlogs.go:487-492`, `store.go:2203-2208` (`routed()`), and `merge.go:361-377` derives a missing mode from the cost columns. A bare new constant would leak `local` money into the real-spend column | Each of the four sites gets an explicit `local` case routed to `api_equivalent_cost_usd`; a schema invariant test pins the pair |
| R1b | Local rows out of quota burn | Burn is keyed by account name (`api/quota.go:119,125` → `quota.ComputeBurn(…, acct.Name, "", …)`), and only `billing_mode == "subscription"` accounts are iterated (`quota.go:114`, `cli/quota.go:47`). A local row carries account `local`, which matches no subscription account | Fixed account name `local`; no quota code change, one regression test |
| R1c | Local JSONL transcripts must not be double-counted as `subscription` | The tailer classifies per row by model prefix (`jsonlogs.go:431-437`); an unlisted local model lands in `subscription` with its own (likely unpriced) cost, and `merge.go:341` takes the *winner's* mode, so a JSONL winner would flip a proxy `local` row | `LocalModelPrefixes` config mirroring `ApiModelPrefixes`; tailer checks `ApiModelPrefixes` before `LocalModelPrefixes` — a model prefix matching both lists is classified `api` (preserves real spend when cloud and local both serve DeepSeek-family models); merge: see §3.4 |
| R2 | Truncated captures handled | **`httputil: ReverseProxy read error during body copy` is logged when `src.Read` fails, `$GOROOT/src/net/http/httputil/reverseproxy.go:648-650`: `src` is the upstream body, so this is the upstream (3.173.21.63:443, the DeepSeek edge) dropping the stream mid-response. A client cancel is excluded via `errors.Is(err, context.Canceled)`. `ErrorHandler` does not fire after headers are sent (`proxy.go:50-55`). The tee never records the read error, so `onClose` submits `captureComplete = !respBuf.truncated && !reqBody.truncated` (`proxy.go:107-108`), i.e. `true`, for a body cut mid-stream.** The body cap (8 MiB per `~/.clens/config.toml`) is a different, already-marked mechanism | Capture the read error in `teeCloser`; submit `captureComplete=false`; log from `onClose` with method, path, and bytes received. The existing `ruleStreamIncomplete` rule (`analyze/rules.go:183-189`) fires on `usage.IsStream && !ev.CaptureComplete`, so the row gets the `stream_incomplete` warning for free — no new warning kind. Client cancel (`errors.Is(err, context.Canceled)`) deliberately keeps `CaptureComplete=true`: a client that stopped the stream received what the session produced, which is not an upstream truncation. Retrying a half-streamed response is impossible and is **not** attempted |
| R2a | Retention duration | Archival is `--hot-days` (default 7, `config.go:86`), live-reloadable; deletion is `--retention-days` (default 0). Neither touches what a capture stores. The live DB is 19.6 GB with a 6.5 GB archive, and no `VACUUM` ever runs, so archiving does not shrink the file | **Not a code change.** Operator recommendation in §7: lower `HotDays`. Defaults unchanged |
| R3 | SQLite `disk I/O error (266)`, `cannot allocate … bytes`, `out of memory (7)` | `Open` sets only `busy_timeout`, `foreign_keys`, `journal_mode(WAL)` (`store.go:74-82`) and one connection. `busy_timeout` retries only lock contention. No retry exists for IOERR/NOMEM anywhere in `internal/store` or `internal/consumer`; `flush` logs and drops (`consumer.go:233-238`, `287-291`, `272-274`). Error 266 is `SQLITE_IOERR_READ`; the burst of IOERR + NOMEM within seconds points at host resource exhaustion on a 19.6 GB file with 15 GB RAM, **hypothesis, unchecked** | Bounded retry with backoff for transient classes on the consumer write sites; classified counters on the consumer's `Stats`, exposed through `/api/health`, so a burst is visible, not just a log scroll. `doctor` has no in-process access to consumer counters (it opens the store directly — `doctor.go:139-161`); the counters are reachable only through `GET /api/health` (`api.go:712-745`). No pragma change on speculation (§9) |
| R3a | `jsonlogs: open … cannot find the file` | TOCTOU between `walkFiles` (`jsonlogs.go:223,245-262`) and `os.Open` (`288-291`): Claude Code removed the file between the two. The cursor row is keyed by path; a vanished file simply stops appearing | `errors.Is(err, fs.ErrNotExist)` → silent skip. Other open errors still log |
| R3b | `archived the bodies of N row(s)` | Informational line, `internal/cli/serve.go:416`, once at boot and every 24 h | No change |

Decisions taken with the user: local rows have their own billing mode. Local server is currently
**LM Studio**.

## 3. Design

### 3.1 R2 — record a mid-stream upstream abort

`teeCloser` (`proxy.go:302-316`) gains a `readErr error` field, set in `Read` for any error that is
neither `io.EOF` nor `errors.Is(err, context.Canceled)`, plus an `aborted() error` accessor that
returns the field. Both `onClose` closures in `ModifyResponse` check `t.aborted()`: the capturing
path (`proxy.go:91-110`) and the `--body-policy off` pass-through path (`proxy.go:84-86`). If
`t.aborted()` is non-nil, the closure forces `captureComplete=false`, passes the error as `callErr`,
and logs one labelled line: `proxy: upstream stream aborted method=%s path=%s captured=%d err=%v`
(using `len(respBuf.Bytes())` for the capturing path; the `off` path logs `captured=0`). `ReverseProxy.ErrorLog` is set to a discarding logger to
suppress the duplicate stdlib line (`httputil: ReverseProxy read error ...`). `onClose` passes
`readErr` as `callErr`; `consumer.go:368-375` emits an `upstream_error` warning for any non-nil
`callErr`. A stream abort yields **two warnings**: `upstream_error` (severity `error`, detail =
err.Error()) and `stream_incomplete` (from `ruleStreamIncomplete`, `rules.go:183-189`, fires on
`usage.IsStream && !ev.CaptureComplete`). A non-stream (JSON body) abort yields only
`upstream_error` (ruleStreamIncomplete requires `usage.IsStream`). Both combinations are correct
and intended; a stream abort has `warning_count = 2`. No new warning kind.
Comment updates needed: `sink.CapturedCall.CaptureComplete` (`sink.go:41`) is the canonical
definition — updated to "false when either teed body was cut at the cap, or the upstream response
body ended with a read error other than a client cancel"; `store/types.go:74`, `analyze/rules.go:174`,
`web/app.js:206`, `web/app.js:247-248` ("teed bodies" wording), and `jsonlogs/jsonlogs.go:474-475`
("teed bodies" wording) are updated to point to this definition ("see `sink.CapturedCall.CaptureComplete`") — they currently state the cap-only or teed-bodies-only form; `analyze/kinds.go:74`'s user-facing description is rewritten to the disjunction the rule
uses ("truncated at the read cap, or the upstream stream was aborted before the response was
complete"). Merge interaction (for the record): an aborted proxy row keeps `capture_complete=0`
through `captureCompleteFromBodies` (`merge.go:493-504`) in both arrival orders because it owns
the bodies — no merge change needed.
Client cancel decision: a client that stopped
the stream received what the session produced, so `errors.Is(err, context.Canceled)` is excluded from
the abort path and `CaptureComplete` stays true for those rows. This is an explicit decision, not a
side effect. Hot-path rule: no parsing, no blocking, no new import (proxy still depends only on
`sink` and `config`).

### 3.2 R3 — transient-error retry in the consumer

New `internal/store/retry.go`: `IsTransient(err) bool` matches SQLite primary codes IOERR (10) and
NOMEM (7) using `*sqlite.Error` from `modernc.org/sqlite`. BUSY (5) is **not** retried: `store.go:75`
already sets `busy_timeout(5000)` which blocks up to 5 s inside each attempt; adding explicit BUSY
retries would multiply the maximum block time by the attempt count. Note: SQLite's NOMEM surfaces as
`out of memory` (not `cannot allocate N bytes`); bead 03's first step confirms the exact error string
and `*sqlite.Error` accessor before writing the matcher. `internal/store` is the only package that
imports the driver (`store.go:26`), so `IsTransient` lives there — not in `consumer`, which avoids a
new direct driver dependency in consumer.

New `internal/consumer/retry.go`: `withRetry(ctx, fn)` makes up to 4 attempts with 250 ms / 1 s / 4
s backoff, abandoning on `ctx` cancel. Applied at `InsertEvent`, `UpsertWarnings`,
`SessionEventsForRules` and `RecordCall`. A persistent error gives up after 4 attempts at the
per-call level. Flush-wide gave-up flag: once any retried call gives up (in any phase), the flush
stops — remaining `InsertEvent`s are counted in `c.failed`, and phases 2 (`runSessionRule`) and 3
(`RecordCall`) are skipped for that flush. Bound: one give-up (~5.25 s) per flush, regardless of
the number of events, sessions, or RecordCall invocations in the batch. Bead 03's first step also confirms that `InsertEvent`'s merge path (the `UPDATE`
before `INSERT` in `store.go`) is a single atomic transaction so a retry cannot double-apply a
partial write. The consumer goroutine blocks during backoff; the sink is bounded so a long burst drops
captures at the sink with the existing `proxy: dropped capture` log. Counters: `transientRetries`,
`transientGaveUp` on `Stats`, exposed through `GET /api/health` as JSON fields `store_transient_retries` and `store_transient_gave_up` in the existing `healthResponse` struct (`api.go:712-724`).

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
| `merge.go:341-377` | After the existing completeness pick (`:250-264`) and usageObserved swap (`:280-290`), apply the following when modes disagree (`existing.BillingMode != incoming.BillingMode`) and either side is `local`. **"Proxy-involved side"** is defined as the side where `Source == "proxy" \|\| contains(SourceRefs, "proxy")` — the `reflagScope` idiom (`store.go:2367`). A JSONL-first merged row keeps `Source=="jsonl"` but gains `SourceRefs=[jsonl,proxy]` at `:225`, so it counts as proxy-involved on a subsequent re-observation; bare Source-equality would miss it. No-op cases: if neither side is proxy-involved (both `Source=="jsonl"`, e.g. `--rebuild`) — do nothing (both-jsonl = no-op); if both sides are proxy-involved — do nothing (both-proxy = no-op). When both sides carry `local` (normal case: `LocalModelPrefixes` set, proxy target = local), modes agree — no override fires, existing guards run unmodified. **Token-supplying side** (unchanged from round 3): reassign `winner` to the proxy-involved side only when `usageObserved(proxySide) && (proxySide.CaptureComplete || !other.CaptureComplete)`; this preserves both existing guards. Token columns (`:292-298`), `model_resolved`, `StopReason`, `StopCategory`, `ServiceTier`, `Speed`, and the `billing_mode == ""` fallback block (`:361-377`) all read from this winner. **Identity override** (applied after `:341`, runs before the `:361` fallback block): when `proxySide.BillingMode != ""`, `BillingMode` and `Account` come from the proxy-involved side. `merged.BillingMode = proxySide.BillingMode`; `merged.Account = proxySide.Account` (real account when the proxy side is `api` or `subscription`; `config.LocalAccount` when it is `local`). An empty `proxySide.BillingMode` (unclassified credential) is treated like `preferNonEmpty` — the override is skipped. This overrides both `:341` and `:319` for these two columns when the override fires. **Cost alignment** (applied after the identity override): when the identity override was skipped (`proxySide.BillingMode == ""`), cost alignment is also skipped — the winner's cost columns are kept as-is (no identity mode exists to compare against). When the identity override fired: if the token-supplying winner's mode matches the identity mode (proxy-involved side also won tokens), keep the winner's cost columns (`:299-301`) unchanged. If the winner's mode differs from the identity mode (token side is not the proxy-involved side), set `CostUSD=nil`, `ApiEquivalentCostUSD=nil`, and `CostSource="unpriced"` — the existing NULL convention; `clens reprice --model <m>` fills them. The cost cell is never `$0.00` and never a label that burns quota. **Cases**: (1) Guard fires: proxy-involved wins tokens AND identity; same mode → cost kept. (2) Guard does not fire (proxy row has zero usage or is incomplete while JSONL is complete): JSONL wins tokens; proxy-involved side supplies identity; JSONL mode ≠ identity mode → nil cost, `CostSource="unpriced"`. (3) Proxy `api` or `subscription` + JSONL prefix-classified `local` (inverse hazard): proxy-involved supplies identity = api/subscription + real account; if guard fires, cost kept; if guard does not fire (e.g. aborted proxy stream, the R2 case), nil cost, `CostSource="unpriced"`. **Account invariant post-condition** (applied after all of the above, for every merge output, whether or not any override fired): `if merged.BillingMode == config.BillingLocal { merged.Account = config.LocalAccount }` — a both-JSONL no-op (`--rebuild`) can set mode `local` via `:341` while leaving the old subscription account via `preferNonEmpty` (`:319`); the post-condition repairs that. `else if merged.Account == config.LocalAccount { merged.Account = (the non-local side's Account; "" if neither has one) }` — the inverse: a subscription re-ingest that wins mode must not carry a leftover `local` account, which would silently drop real subscription tokens from burn. This makes `BillingMode == local ⟺ Account == LocalAccount` a property of every merge output, independent of which branch ran. For ordinary non-local merges (modes agree), no override fires — Account stays `preferNonEmpty` at `:319`, BillingMode follows winner at `:341`, all unchanged. |
| `store.go:1674-1675` (`statsSelectColumnsInner`) | add `SUM(CASE WHEN billing_mode = 'local' THEN api_equivalent_cost_usd END)` as a third aggregate; `StatsSummary` gains `TotalLocalProjectedCostUSD`; both scan helpers (`1680-1731`) extended. `store.go:805-806` is inside `reconcileSessionTx` (`:781-864`) and is intentionally untouched — see Sessions note below |

**Column-keyed consumers** — several Go-side display and fold sites key on which cost *column* is
non-nil rather than on `billing_mode`; a local row's `api_equivalent_cost_usd` would be absorbed
into the subscription figure or labelled "sub" without these changes:

| Site | Change |
|---|---|
| `cli/stats.go:194-203` (`writeGrouped`) | add `case ev.BillingMode == "local" && ev.ApiEquivalentCostUSD != nil: g.localCost += *ev.ApiEquivalentCostUSD; g.hasLocal = true` bucket **before** the existing `case ev.ApiEquivalentCostUSD != nil:` at `:198` (otherwise a local row with a non-nil api_equivalent_cost_usd is caught by the subscription case first); a local row with nil cost (no-fire branch, unpriced model, zero-usage) falls through to `default: g.unpriced++`; print a labelled `local (projected)` column beside `api` and `sub`; at `:230` the print reads `g.localCost`/`g.hasLocal`; the column is omitted when zero so existing output is unchanged |
| `cli/format.go:86-88` (`costCell`) | Change signature to `costCell(api, sub, local *float64)`; add `rowCosts(ev *store.EventSummary) (api, sub, local *float64)` helper (`store.EventSummary` is the type `ls.go` and `tail.go` iterate; `store.Event` embeds `EventSummary`, so `show.go` passes `&ev.EventSummary`): returns `ev.ApiEquivalentCostUSD` as `local` (and nil as `sub`) when `ev.BillingMode == config.BillingLocal`; otherwise `ev.ApiEquivalentCostUSD` as `sub` and nil as `local`. The 3 per-row call sites (`ls.go:95`, `tail.go:101`, `show.go:72`) call `rowCosts(ev)`. `sessions.go:72` passes `(s.TotalCostUSD, s.TotalApiEquivalentCostUSD, nil)` directly — session money sums exclude local rows (§3.4 Sessions note), so the `local` argument is always nil. Renders `local (proj) $x` for local rows; `api - / sub $x` otherwise. Aggregate sites (`stats.go:89,112,140`) pass `TotalLocalProjectedCostUSD` from `StatsSummary`/`PeriodStats` via the scan change in the routing table row above — they do not call `rowCosts`. |
| `web/app.js` per-row and summary cells | render `local (proj) $x` when `billing_mode == "local"` in one `fmtCost(e)` helper (`:429, :570, :966` per-row; `:552, :581, :652, :659, :663, :956, :1128` summaries all read `TotalApiEquivalentCostUSD` — summaries gain a parallel `TotalLocalProjectedCostUSD` tile, added at `store.go:1674-1675` above) |
| `store.go:1345` + `api/models.go:96-104` (`coverage`) | `StatsByModel` sets `ModelStats.PriceModel string` on each result row: `config.LocalPriceModel` when `billing_mode = 'local'`, else `s.Model` — computed in the store where `config` is importable, replacing the discarded billingMode scan at `:1345`. `api/models.go` folds this as `row.priceModel` (`models.go:66-80`); `coverage` substitutes `row.priceModel` for the catalogue look-up (`models.go:96-104`) — `api/models.go` never names a `config` constant (enforced by `TestAPIWebImportGuard` at `importguard_test.go:36`). `cli/models.go` reads `s.PriceModel` directly to choose the catalogue key. No `BillingMode string` field on `ModelStats`, no `local bool` field on `modelCoverage`. |

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

Sessions: `reconcileSessionTx` sums (`store.go:805-806`) stay as they are — no `local` aggregate is added there — so a session's real and subscription totals exclude local rows (invariant 5 holds). Session *token* totals still include local tokens; a session
that spans a mid-session toggle is the one place the two mix, recorded in §6 R4 and not solved.
No schema change: `billing_mode` has no CHECK (`schema.sql:21`), so no migration and no
backup-before-migration step. `Compute()` is untouched (`pricing.go:94-152`).

### 3.5 R1 — the provider switch

`config.Config` gains `LocalUpstreamURL` and `LocalModelPrefixes` (flat keys, env
`CLENS_LOCAL_*`, flags — the shape of `config.go:206-207,249`). The cloud target stays
`UpstreamURL`. `Validate` requires `LocalUpstreamURL` to parse as http(s) when set and rejects a
non-loopback host unless `AllowRemote`, mirroring the existing listener guard (the proxy carries
credentials). Live-reload caveat: `cli/reload.go:81` `liveFields` is `{Accounts, RetentionDays,
HotDays}`; `LocalUpstreamURL` and `LocalModelPrefixes` are **not** live fields and will be reported
as `restart_required` by `/api/reload`. The "no restart" claim in the UI (R1) applies to the active
provider toggle only — the URLs themselves require a restart.

`internal/proxy`: `type Targets struct{ cloud, local *url.URL; path string; mu sync.Mutex; active atomic.Pointer[string] }`
with `Active() string` (one atomic load, no lock — hot path) and `Set(string) error` (`"cloud"` | `"local"`; `local` rejected when no
local URL is configured; `mu` held inside `Set` only, serialising concurrent `POST /api/provider` calls so the file write and pointer store cannot interleave). Constructor: `NewTargets(cfg *config.Config, stateDir string) (*Targets, error)` — parses both URLs; when `stateDir != ""` stores `filepath.Join(stateDir, "provider")` as `path`, otherwise leaves `path` empty; reads the state file only when `path != ""`; applies the `local`-without-URL fallback; writes nothing (read-only at boot). `Set` skips the file write when `path == ""`. `New(cfg, sk)` is unchanged; `NewWithTargets(cfg, sk, t *Targets)` is added — `New` calls `NewTargets(cfg, "")` (empty stateDir = no state file, no state-file I/O) and delegates to `NewWithTargets`. Every existing `New(cfg, sk)` call site in `proxy_test.go` and `api/replay_test.go:57` compiles unchanged. `NewServer(cfg, sk, t *Targets)` takes the `*Targets` as a new third parameter (single production caller: `serve.go:120`). `Rewrite` reads `targets.Active()` instead of closing over `target` (`proxy.go:42-44`). Each request stamps `st.provider` from the same read; `sink.CapturedCall` gains `Provider string`.
`serve.go:120` wiring: `targets, err := proxy.NewTargets(cfg, filepath.Dir(cfg.DBPath))`; on error `return fmt.Errorf("serve: build provider targets: %w", err)`; pass `targets` to `proxy.NewServer(cfg, sk, targets)`; after `api.New(...)`, wire `dashAPI.SetProvider(api.ProviderSeam{Get: ..., Set: targets.Set})`. `ProviderSeam` follows the existing `SetReload` pattern (`reload.go:20,36-38`): nil seam answers 503; `serve.go:130-145` wires every seam via `dashAPI.SetXxx(...)` after `api.New`. `api.New`'s existing signature is unchanged; `SetProvider` is the new setter. The seam follows the injected-seam pattern (`SetReload`); `api/provider.go` must not import `internal/config` (`importguard_test.go:36`), so `ProviderSeam.Get` returns plain strings/bools. `internal/cli/ingest.go:90-102` is the wiring point for the
tailer's account, prefix, and model-billing settings; `LocalModelPrefixes` needs a
`Tailer.SetLocalPrefixes([]string)` setter added to `jsonlogs.go` and called from `ingest.go`
alongside the existing `SetModelBilling` call. Tailer prefix precedence: `ApiModelPrefixes` is
checked before `LocalModelPrefixes`; a model matching both is classified `api` (see §2 R1c).

Persistence: `<dir of DBPath>/provider` holds one word, written atomically on `Set`, read at boot;
absent, unrecognised, or `local` when `LocalUpstreamURL` is not configured → `cloud`. Not a
config-file rewrite (the file is hand-annotated, `serve`'s `config.toml` comments would be
destroyed). `Set("local")` also returns an error at the API level when no local URL is configured,
not just at boot — same gate, two call sites.

API: `GET /api/provider` → `{active, cloud_url_host, local_url_host, local_configured}`;
`POST /api/provider {"active":"local"}` guarded exactly like `reload`/`shutdown`
(`api/reload.go:26-35`: `originReject` + loopback caller). The loopback-guarded routes in
`api-surface.md`/`security-and-permissions.md` go two → three (shutdown, reload, provider); the
three *write-guard types* (same-origin, opt-in for replay, loopback caller) are unchanged in count.
**The toggle is loopback-caller only**: a dashboard opened from another host (the listener binds
`0.0.0.0`) will receive `403` on `POST /api/provider`, so the "seamless UI switch" works only from
the host running `clens`. The loopback-caller check is load-bearing here because switching providers
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
`internal/store/types.go`, `internal/config/config.go`, `internal/cli/serve.go`,
`internal/cli/ingest.go`, `internal/api/api.go` (`/api/health` is at `api.go:726`, no separate
`health.go`), `internal/web/index.html`, `internal/web/app.js`, `internal/web/style.css`,
`internal/cli/stats.go`, `internal/cli/ls.go`, `internal/cli/tail.go`, `internal/cli/show.go`,
`internal/cli/sessions.go`, `internal/api/models.go`, `internal/cli/models.go`,
`internal/analyze/kinds.go`, `internal/analyze/rules.go`.
New: `internal/consumer/retry.go`, `internal/store/retry.go`, `internal/proxy/targets.go`,
`internal/api/provider.go`, and a test file beside each.

### 4.2 Explicitly not touched
- **`internal/pricing`** — `Compute()` is unchanged (`pricing.go:94-152`); local pricing is a model
  and instant substitution *before* the call, never a table edit. The test in §5.2 reads the
  table but nothing in the package changes.
- **`internal/quota`** — keyed by account name; the fixed `local` account is the whole exclusion.
- **Import cycle**: `store` and `jsonlogs` will import `config` for `LocalPriceModel`/`LocalPriceAt`/`LocalAccount`. `config` has no internal imports (`config.go:1-12`), so no cycle is introduced. `store` already imports `proxy` (`store.go:24`); config is a lower-level package than proxy, so that is also fine.
- **`internal/api` / `internal/web` import guard**: non-test files in `internal/api` may not import `internal/config` (enforced by `TestAPIWebImportGuard`, `importguard_test.go:36`). All `config` references in `api/models.go` are eliminated by routing the local-price substitution through `ModelStats.PriceModel` set in the store (see §3.4).
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
| Merge flips `local` → `subscription`, wrong account, or real spend erased by prefix-inferred `local` | `TestMergeKeepsLocalSticky` (both arrival orders, see §3.4); `TestMergeLocalModeMismatch` (proxy `api` + JSONL prefix-classified `local` → merged row keeps `cost_usd`, mode `api`, real account, both orders); `TestMergeLocalAccountInvariant` (both-JSONL rebuild, both orders; account invariant holds across every merge output); `TestLocalRowsAreAbsentFromQuotaBurn` extended to the merged case |
| Hot path blocks or buffers | Existing TTFB gate must stay green; new abort test uses a slow-streamed fake upstream |
| Switch races a request | `-race` test: concurrent `Set` and requests; each stamped provider equals the URL that served it |
| Retry masks a permanent error | `store.IsTransient` table test, incl. a constraint error that must not retry |
| Retry stalls shutdown | Cancel-during-backoff test |

### 5.2 New tests
- `TestMidStreamUpstreamAbortIsMarkedIncomplete` — fake upstream sends headers, part of the body,
  then closes the connection; asserts `CaptureComplete=false`, the labelled log line, and the exact
  warning set: both `upstream_error` (severity `error`) and `stream_incomplete` for a stream abort.
  A companion sub-case for a non-stream (JSON body) abort asserts only `upstream_error`.
  A third sub-case runs with `--body-policy off` (the pass-through closure) and asserts `CaptureComplete=false` and `upstream_error` (verifying the sibling path also reads `t.aborted()`).
- `TestCancelledClientIsNotAnAbort` — `context.Canceled` (via `errors.Is`) leaves `CaptureComplete=true`,
  no `stream_incomplete` warning. Tests the explicit decision in §3.1.
- `TestLocalRowsPriceIntoApiEquivalentColumn`, `TestLocalSumIsNeverMergedIntoApiOrSub`.
- `TestLocalRowIsPricedAsDeepseekFlashBase` — an unknown local model name still prices (not
  `unpriced`) to exactly the `deepseek-flash` figure, and a call timestamped inside the peak window
  still prices at base with no `peak_pricing` warning. Negative control: price with the real `at` →
  the figure doubles and the warning appears.
- `TestLocalPriceModelIsTheCheapestShippedDeepseekRow` — the guard from §3.4.
- `TestLocalRowsAreAbsentFromQuotaBurn` — a `local` row inside the 5 h window leaves a subscription
  account's burn unchanged. Extended to cover a **merged** row: proxy `local` + JSONL `subscription`,
  both arrival orders; the merged row must keep `account = config.LocalAccount` and must not appear
  in burn.
- `TestMergeKeepsLocalSticky` — proxy `local` + JSONL `subscription`, both arrival orders; asserts tokens, cost, account = `config.LocalAccount`, BillingMode = `local` (see §3.4). Three-event sub-case: JSONL first, then proxy `local`, then JSONL again; the merged row must stay `local`, account = `config.LocalAccount`, `cost_usd` nil and cost in `api_equivalent_cost_usd` only. Negative control: Source-only proxy identification flips the row to `subscription` on the third observation.
- `TestMergeLocalZeroUsageTokensSurvive` — proxy `local` with zero usage (`usageObserved=false`) + JSONL `subscription` with real tokens; asserts JSONL tokens survive (guard does not fire), BillingMode stays `local`, Account = `config.LocalAccount`, cost cell is NULL, `CostSource=="unpriced"`. Negative control: unconditional reassignment zeroes the tokens.
- `TestMergeLocalIncompleteTokensSurvive` — proxy `local` with `CaptureComplete=false` + complete JSONL `subscription` with real tokens; asserts JSONL tokens survive (guard does not fire), BillingMode stays `local`, `CostSource=="unpriced"`. Negative control: unconditional reassignment overrides completeness pick.
- `TestMergeLocalModeMismatch` — proxy `api` + JSONL prefix-classified `local`, both arrival orders; asserts the merged row keeps `cost_usd` (not nil), `BillingMode == "api"`, and a real (non-LocalAccount) account when the guard fires (observed, complete proxy row). Additional case: incomplete or zero-usage proxy `api` row against a complete JSONL `local` row; expected result: mode `api`, real account, `cost_usd` nil, `CostSource=="unpriced"`, JSONL tokens kept. Guards the inverse hazard: real account and mode must not be erased, and an aborted api stream leaves a priced-label-free row.
- `TestMergeLocalAccountInvariant` — both-JSONL no-op: JSONL `subscription`/`Max` (existing) + JSONL `local`/`LocalAccount` (incoming re-ingest after `LocalModelPrefixes` set), both orders; assert `Account == config.LocalAccount` iff `BillingMode == "local"`. Reverse case: JSONL `local`/`LocalAccount` (existing) + JSONL `subscription`/`Max` (incoming re-ingest after prefix removed), both orders; assert `Account != config.LocalAccount` when `BillingMode == "subscription"`. Negative control: remove the post-condition — account stays `Max` when mode is `local`; the quota-burn test sees the row.
- `TestRepriceRoutesLocalToApiEquivalent`.
- `TestWriteGroupedLocalBucket` — a `local` row with a non-nil cost must not change the `sub` figure of `writeGrouped`; it must appear only in the `local (projected)` column. A `local` row with nil cost (no-fire branch, unpriced model, or zero-usage) must fall to `default` (counted as `unpriced`), not panic.
- `TestCostCellLocalLabel` — `rowCosts(ev *store.EventSummary)` with a `local` row returns `local != nil`, `sub == nil`;
  `costCell(nil, nil, &x)` renders `local (proj) $x`; `rowCosts(ev)` with a non-local row returns
  `sub != nil`, `local == nil`. Tests the actual `costCell(api, sub, local *float64)` signature.
- `TestModelsCoverageShowsLocalPriced` — `coverage` with a `ModelStats` whose `PriceModel == config.LocalPriceModel` and an unknown real model name returns `pricingPriced` (substitutes `PriceModel`), not `pricingUnpriced`.
- `TestIsTransient` — table test for `store.IsTransient`: IOERR (code 10), NOMEM (code 7) → true; BUSY (code 5), constraint violation, `nil` → false. Pins the primary-code extraction so a `*sqlite.Error` wrapping a constraint error is not misclassified as transient.
- `TestHealthExposesStoreRetryCounters` — after a simulated transient retry, `GET /api/health` returns JSON with `store_transient_retries > 0` and `store_transient_gave_up == 0`; after a give-up, `store_transient_gave_up > 0`.
- `TestValidateLocalUpstreamLoopback` — `Config.Validate` rejects a non-loopback `LocalUpstreamURL` when `AllowRemote` is false; accepts it when `AllowRemote` is true; accepts a loopback URL in both cases.
- `TestTailerLocalPrefixClassification` — a model matching only `LocalModelPrefixes` is classified `local`; a model matching only `ApiModelPrefixes` is classified `api`; a model matching both lists is classified `api` (precedence rule, §2 R1c).
- `TestValidateRejectsLocalAsAccountName` — `Config.Validate` rejects an account named `local`.
- `TestSetLocalFailsWithNoURL` — `targets.Set("local")` returns an error when `LocalUpstreamURL` is
  not configured.
- `TestProviderBootFallsBackToCloud` — `NewTargets` with a state file containing `local` and no
  `LocalUpstreamURL` configured returns `Active() == "cloud"`.
- `TestTargetsSwitchIsLiveAndTagged`, `TestProviderPersistsAcrossRestart` (uses `NewTargets` to reload state),
  `TestProviderEndpointRejectsNonLoopbackAndForeignOrigin`.
- `TestWithRetryRetriesTransientOnly`, `TestWithRetryStopsOnCancel`, `TestWithRetryGivesUpOncePerFlush`
  (asserts: once an InsertEvent gives up, subsequent InsertEvents are counted in `failed`, and
  phases 2/3 — runSessionRule, RecordCall — are not called for that flush).
- `TestTailFileSkipsVanishedFileQuietly`.

### 5.3 Not covered by any automated test
- A real LM Studio `/v1/messages` round trip and the dashboard toggle's look: checked by hand in
  bead 06 against the running server, because neither exists in CI.
- The 2026-10-09 burst itself: not reproducible; only the handling is tested.

### 5.4 Negative control (required)
Each test above is run once with its fix reverted and must fail: abort test fails with
`CaptureComplete == true`; `TestBillingModeInvariants` (extended for the `local` shape) fails when
the explicit `local` case is removed from `routed()` and `consumer.go`'s routing (the local figure
then appears in `cost_usd`); `TestRepriceRoutesLocalToApiEquivalent` fails when the substitution is
removed (it exercises `routed()` directly); `TestMergeKeepsLocalSticky` fails when the guarded reassignment is absent (proxy-source-wins condition removed); `TestMergeLocalModeMismatch` negative control: remove the identity override so JSONL `local` mode leaks through — the test fails on `BillingMode=="local"` (real account erased) or `cost_usd==nil` (real spend erased) depending on the order. For the incomplete/zero-usage proxy api case: remove the cost-alignment step — the test fails on a non-`"unpriced"` CostSource with nil cost (invariant mismatch). `TestMergeLocalAccountInvariant` negative control: remove the account invariant post-condition — account stays `Max` when mode is `local` (both-JSONL order); the quota-burn test sees the row. The unconditional-reassignment control (overrides usageObserved/completeness guards) applies to the two zero-usage and incomplete-capture tests (see §5.2 `TestMergeLocalZeroUsageTokensSurvive` and `TestMergeLocalIncompleteTokensSurvive`); the retry test fails with one attempt instead of
four; `TestWriteGroupedLocalBucket` fails when the `local` bucket is absent from `writeGrouped` or when the nil-cost local row panics instead of falling to `unpriced`.

## 6. Risk areas
- **R1 — Credential redirection.** The toggle changes where the caller's API key is sent. Guard it
  like `reload` (origin + loopback), and do **not** allow `local` to be set to a non-loopback URL
  unless `AllowRemote`. The loopback-caller check means `POST /api/provider` only works from the
  host running `clens`; a remote dashboard (the listener binds `0.0.0.0` on this machine) gets
  `403`.
- **R2 — Hot path.** `Rewrite` reads one atomic pointer. Do **not** add a lock, an allocation per
  request beyond the stamp, or any parse.
- **R3 — Retry stalls the consumer.** Bounded to ~5.25 s per write attempt (250 ms + 1 s + 4 s
  backoff); flush-wide gave-up flag bounds each flush to one give-up (~5.25 s) — skips remaining
  InsertEvents, runSessionRule, and RecordCall once any call gives up; BUSY is not retried
  (`busy_timeout` already handles it). The sink bound turns a long outage into logged drops,
  not unbounded memory.
- **R4 — Sessions spanning a toggle.** Session token totals include local tokens; money does not.
  Accepted and documented rather than adding a per-session breakdown.
- **R5 — A fixed `local` account name.** A user account literally named `local` would collide.
  `Config.Validate` (`config.go:340-408`) gains an account-name check: any account whose `Name`
  equals `config.LocalAccount` is rejected with a validation error.
- **R6 — No new migration.** If a later bead adds a column, the user CLAUDE.md backup rule applies
  to `lens.db`, `-wal`, `-shm` before it runs.

## 7. Pre-flight (needs a decision before bead 01)
- Confirm LM Studio's version serves `/v1/messages` and its default port; if it serves only the
  OpenAI shape, R1 needs a translation layer and the plan stops for a user decision.
- **Check (a) — message id in response body:** send one request and inspect the response JSON for a
  `message.id` field equal to what Claude Code writes in its JSONL transcript. The proxy's dedup
  strategy (`jsonlogs/dedup.go:99-115`) matches on body id; without it the proxy row gets a
  synthetic key (`consumer.go:456-466`) and the JSONL row a `jsonl:` key, producing two rows and
  two projected costs per call. If (a) fails, **stop and ask the user** — the merge design in §3.4
  has no fallback path for missing body ids.
- **Check (b) — usage frames in streamed response:** confirm the streamed SSE response contains a
  usage frame (`input_tokens`, `output_tokens`). Without it `parse.ExtractUsage` returns zeros and
  all local rows are zero-token (priced at $0.00 and listed as `unpriced`).
- **Check (c) — model id accepted:** confirm LM Studio accepts the model name Claude Code sends
  (e.g. `claude-sonnet-4-6`) unchanged, or document the rewrite needed. The proxy does not rewrite
  request bodies; if LM Studio requires its own model id, a body-rewrite step must be added and the
  plan extended.
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
| 03 | `feat-transient-store-retry` | P1 | — | R3: bounded retry, counters in /api/health. First step: confirm `InsertEvent` merge path is a single atomic transaction (no partial-write risk on retry), confirm `*sqlite.Error` accessor and NOMEM message string (`out of memory`) before writing `store.IsTransient` |
| 04 | `feat-local-billing-mode` | P0 | — | R1a–c: config keys, routing at four sites, third aggregate; column-keyed display consumers (`cli/stats.go`, `format.go`, `ls/tail/show/sessions`); `store.StatsByModel`/`ModelStats.PriceModel`; `api/models.go`; `cli/models.go`; `merge.go` billing-mode row; `Config.Validate` account-name check |
| 05 | `feat-provider-switch` | P0 | 04 | R1: `Targets`, stamping, state file. First step: run the three §7 pre-flight checks (a)/(b)/(c) against the running LM Studio; stop if (a) fails |
| 06 | `feat-provider-api-and-ui` | P0 | 05 | R1: `/api/provider`, header toggle, filter, local tile; `app.js` cost display and summary tiles; `index.html` billing-filter option |

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

**QA engineer.** Three merge cases most likely to be silently untested: (a) proxy `local` + JSONL `subscription` — the guarded reassignment must keep account = LocalAccount and mode = local in both arrival orders (`TestMergeKeepsLocalSticky`); (b) proxy `api` + JSONL prefix-classified `local` — see §3.4 for the guard rule; the merged row must keep `cost_usd`, mode `api`, and a real account (`TestMergeLocalModeMismatch`); (c) both-JSONL re-ingest (`--rebuild`) — the account invariant post-condition must hold in both orders (`TestMergeLocalAccountInvariant`).

**Security engineer.** The toggle redirects credentials, so `POST /api/provider` takes the
`originReject` + loopback guard of `reload`; the dashboard binds `0.0.0.0` here, so the guard cannot
be skipped. `LocalUpstreamURL` is validated and refused off-loopback without `AllowRemote`. The state
file holds one non-secret word. Local bodies are captured under the same redaction and cap as cloud.

## 11. Context docs to refresh (running list)
- `docs/context/cost-and-quota.md:19-39` — "all seven plan types fold to two billing modes" → three; add `local`.
- `docs/context/architecture.md` — "one upstream" → two targets, live-swappable.
- `docs/context/api-surface.md:92` — loopback-guarded routes: two → three (shutdown, reload, provider); add `/api/provider` row; note toggle is loopback-caller only.
- `docs/context/security-and-permissions.md` — loopback-guarded routes two → three; credential-redirection note; toggle loopback-only consequence.
- `docs/context/storage-schema.md` — `billing_mode` vocabulary gains `local`.
- `docs/context/workflows.md` — flow 1 gains the abort path (`:52-55` currently describes `CaptureComplete` from the two buffer truncated bits alone; add the read-error path); flow 7 notes provider switch needs no restart.
- `docs/context/build-and-run.md`, `cli-and-tooling.md` — three new config keys; store-error counters in `/api/health` (not `doctor`).
- `docs/context/storage-schema.md:118-128` — "computed from the two buffers' truncated bits alone … `proxy.go:107`" becomes false; update the definition and the line cite.
- `docs/context/dashboard.md` — the header toggle and the local tile; update the `CaptureComplete=false` meaning at `:61`.
- `docs/context/glossary.md` — `local`, `provider`; update `CaptureComplete=false` definition at `:45`.
- `docs/context/INDEX.md:359-363` — refresh entry; update `CaptureComplete=false` meaning and test-file count.
- `docs/context/data-privacy-and-compliance.md:62-64` ("records whether what was stored is the whole thing or was narrowed … covers both bodies") and `:102-103` ("That flag is about the two *teed* bodies") — after this change the flag also reflects an upstream stream abort; update both to "see `sink.CapturedCall.CaptureComplete`" rather than restating the definition.

## Change History

### v7 (round-6 triage)
One JUSTIFIED finding plus three implementer-discretion clarifications from carried items.

**F6.1 (JUSTIFIED, MINOR)** — §3.5: replaced "All 6 existing call sites in `proxy_test.go` (`:134,280,316,372,497,564`) and `api/replay_test.go:57` use `New(cfg, sk)` and compile unchanged" with "Every existing `New(cfg, sk)` call site in `proxy_test.go` and `api/replay_test.go:57` compiles unchanged." The real count is 11 (confirmed by grep: `:134,:189,:221,:280,:316,:372,:497,:564,:626,:683,:784`); removing the count prevents recurrence.

**Carried — stateDir empty-string guard** — §3.5: changed `stateDir+"/provider"` to `filepath.Join(stateDir, "provider")` and added explicit note that `path` is left empty when `stateDir == ""`, and both `Set` and boot-read are no-ops when `path == ""`. Prevents a write to `/provider` when `New` calls `NewTargets(cfg, "")`.

**Carried — cost-alignment when identity override skipped** — §3.4 merge.go row: added an explicit guard: when the identity override is skipped (`proxySide.BillingMode == ""`), cost alignment is also skipped and the winner's cost columns are kept as-is (no identity mode exists to compare against). Prevents undefined comparison producing incorrect `nil` costs.

**Carried — bead-assignment gaps** — §8: bead 04 purpose cell now names the column-keyed display consumers (`cli/stats.go`, `format.go`, `ls/tail/show/sessions`, `store.StatsByModel`/`ModelStats.PriceModel`, `api/models.go`, `cli/models.go`), the `merge.go` billing-mode row, and `Config.Validate` account-name check. Bead 06 purpose cell now names `app.js` cost display and summary tiles and the `index.html` billing-filter option.

### v6 (round-5 triage)
Two findings from round-5 critique, both JUSTIFIED. Major: (1) §3.4 merge.go row: identity override qualified to skip when `proxySide.BillingMode == ""` (unclassified credential treated like `preferNonEmpty`); stated that the override runs before the `:361` fallback block; added **Account invariant post-condition** applied to every merge output (`if merged.BillingMode == config.BillingLocal { merged.Account = config.LocalAccount } else if merged.Account == config.LocalAccount { merged.Account = (non-local side's Account) }`) — closes the both-JSONL `--rebuild` gap where `:341` sets mode `local` but `:319` leaves the old subscription account, leaking tokens into `quota.ComputeBurn` (which filters by account only, no `BillingMode` field passed, `quota.go:63-71`); §5.1 risk table: added `TestMergeLocalAccountInvariant` to the merge risk row; §5.2: added `TestMergeLocalAccountInvariant` (both-JSONL both orders, both modes, with negative control); §5.4: added negative control; §10 QA note: updated to three merge cases (F5.1). Minor: (2) §3.5: reworded ProviderSeam rationale from false "keeps `api` free of any `proxy` import" (refuted by `api/replay.go:15` importing `internal/proxy`) to the real constraints: config import ban (`importguard_test.go:36`) and injected-seam pattern (`SetReload`) (F5.2).

### v5 (round-4 triage)
Five findings from round-4 critique, all JUSTIFIED. Major: (1) §3.4 merge.go row SIMPLIFIED for the fourth consecutive round — "proxy-sourced side" (bare Source equality) replaced with "proxy-involved side" defined as `Source == "proxy" || contains(SourceRefs, "proxy")` (the `reflagScope` idiom, `store.go:2367`); both-proxy added as a no-op alongside both-jsonl; the two-branch no-fire text (local-only identity override, stale "Inverse hazard closed" sentence) replaced with a single three-part rule: (a) guarded reassignment still determines the token-supplying winner; (b) identity (BillingMode + Account) always from the proxy-involved side regardless of its mode; (c) cost/CostSource from the token-supplying winner — if winner's mode differs from identity mode, nil cost + `CostSource="unpriced"`, not "Leave for RepriceCosts"; `TestMergeKeepsLocalSticky` gains three-event sub-case; `TestMergeLocalModeMismatch` gains incomplete/zero-usage proxy api case with `CostSource=="unpriced"` assertion; `TestMergeLocalZeroUsageTokensSurvive` and `TestMergeLocalIncompleteTokensSurvive` pin `CostSource=="unpriced"` (F4.1, F4.2). (2) §3.4 routing table: `store.go:805-806` dropped from the stats-aggregate row (it is inside `reconcileSessionTx`, not `statsSelectColumnsInner`); Sessions paragraph now explicitly states 805-806 is intentionally untouched (F4.3). Minor: (3) §3.4 column-keyed consumers table, cli/stats.go row: nil guard added — `ev.BillingMode == "local" && ev.ApiEquivalentCostUSD != nil`; nil-cost local rows fall to `default: g.unpriced++`; `TestWriteGroupedLocalBucket` gains a nil-cost local row case (F4.4). (4) §2 R1c: "merge: proxy-source wins when modes disagree (see §3.4)" → "merge: see §3.4"; §5.1 risk table: "see §3.4 for the one-reassignment rule" → "see §3.4" (F4.5). §5.4 negative controls updated for the new test cases.

### v4 (round-3 triage)
Nine findings from round-3 critique, all JUSTIFIED. Major: (1) §3.4 merge.go row SIMPLIFIED for the third consecutive round — unconditional "proxy-source wins" reassignment replaced with a guarded one that preserves both existing guards (completeness pick :250-264 and usageObserved swap :280-290): fires only when `usageObserved(proxySide) && (proxySide.CaptureComplete || !other.CaptureComplete)`; when guard does NOT fire, billing identity overwritten to `local`/`LocalAccount`/nil costs but token columns kept from the guards' winner; no-proxy-sourced-side is a no-op; `merged.Account = winner.Account` when guard fires (replaces "iff BillingMode==local" post-assignment); two new tests `TestMergeLocalZeroUsageTokensSurvive` and `TestMergeLocalIncompleteTokensSurvive` (F3.1, F3.2). (2) §3.4 api/models.go row: config refs removed from api/models.go — `StatsByModel` in the store now sets `ModelStats.PriceModel string`; `api/models.go` reads `row.priceModel` only; `cli/models.go` reads `s.PriceModel`; `ModelStats.BillingMode` and `modelCoverage.local` dropped; §4.2 gains api/web import-guard bullet (F3.3). (3) §3.5 wiring: `New(cfg, sk)` kept unchanged; `NewWithTargets(cfg, sk, t)` added; `NewServer` gains third `*Targets` param; `Targets` gains `path string` + `sync.Mutex` on `Set` only; `serve.go:120` wiring specified (`NewTargets` → `NewServer` → `SetProvider` setter); "api.New gains" sentence removed; call site count corrected to 6 proxy_test.go + api/replay_test.go:57 (F3.4). Minor: (4) §3.4 format.go row: `rowCosts` on `*store.EventSummary`; 4→3 per-row call sites; `sessions.go:72` passes nil directly; stats.go aggregate list drops 230; `writeGrouped` `local` case noted to precede `ApiEquivalentCostUSD!=nil`; `TestCostCellLocalLabel` description updated (F3.5). (5) §3.1: teeCloser gains `aborted() error` accessor; both `onClose` closures check it; log says `captured=%d`; `TestMidStreamUpstreamAbortIsMarkedIncomplete` gains `--body-policy off` sub-case (F3.6). (6) §10(b): rule restatement replaced by pointer to §3.4; §5.4 `TestMergeLocalModeMismatch` negative control rewritten (revert/make-local-win → fail on `cost_usd==nil`); unconditional-reassignment control moved to the two new zero-usage/incomplete tests (F3.7). (7) §3.1 comment update list gains `web/app.js:247-248` and `jsonlogs/jsonlogs.go:474-475`; §11 gains `docs/context/data-privacy-and-compliance.md:62-64,:102-103` (F3.8). (8) §3.2 names JSON fields `store_transient_retries`/`store_transient_gave_up` for `healthResponse`; §5.2 gains `TestIsTransient`, `TestHealthExposesStoreRetryCounters`, `TestValidateLocalUpstreamLoopback`, `TestTailerLocalPrefixClassification` (F3.9).

### v3 (round-2 triage)
Six findings from round-2 critique, all JUSTIFIED. Major: (1) §3.4 merge rule SIMPLIFIED —
"local-marked side wins everything, replacing the pick" replaced with a single winner reassignment
placed after the existing completeness pick and usageObserved swap; proxy-source wins when modes
differ and either is `local`; `merged.Account = LocalAccount` iff `merged.BillingMode == local`
set once after winner is final; new test `TestMergeLocalModeMismatch` covers the inverse hazard
(real api cost not erased by JSONL prefix inference) (F2.1); (2) §3.4 column-keyed consumers
table: `costCell` gains a third `local *float64` parameter; `rowCosts` helper added for the 4
per-row call sites; `ModelStats` gains `BillingMode` field (from scanned-but-discarded variable at
`store.go:1345`); `modelCoverage` gains `local bool`; `coverage` substitutes `LocalPriceModel`
when `row.local`; `cli/models.go:55` follows `row.local`, not the api comment; `TestCostCellLocalLabel`
rewritten against real signature; `TestModelsCoverageShowsLocalPriced` added; `internal/store/types.go`
added to §4.1 (F2.2). Minor: (3) §3.1 CaptureComplete canonical definition placed at `sink.go:41`
("false when … cut at cap, or upstream response ended with a read error other than client cancel");
`types.go:74`, `rules.go:174`, `app.js:206` reduced to pointers; `kinds.go:74` description
rewritten; merge interaction noted; §11 gains `storage-schema.md:118-128`, `workflows.md:52-55`,
`dashboard.md:61`, `glossary.md:45`, `INDEX.md:359-363` for CaptureComplete updates (F2.3);
(4) §3.1 states the two-warning combination for stream aborts (`upstream_error` + `stream_incomplete`)
and one-warning for non-stream; `TestMidStreamUpstreamAbortIsMarkedIncomplete` updated to assert
the exact set (F2.4); (5) §3.2 flush-wide gave-up flag: once any call gives up, remaining
InsertEvents, runSessionRule, RecordCall are skipped; "regardless of batch size" removed; §6 R3
updated (F2.5); (6) §3.5 declares `NewTargets(cfg, stateDir) (*Targets, error)` constructor; `proxy.New`
nil-targets means "build from cfg" so 8 existing call sites compile unchanged; api seam is now
`ProviderSeam{Get, Set}` (not just Set); tailer prefix precedence added (`ApiModelPrefixes` first);
§2 R1c decision updated; §5.2 tests reference `NewTargets`; `health.go` hedge replaced with correct
`api.go:726`; `internal/api/health.go` removed from §4.1 (F2.6).

### v2 (round-1 triage)
Ten findings from round-1 critique, all JUSTIFIED. Major fixes: (1) §3.4 gains a "column-keyed
consumers" table for stats.go, format.go, app.js, models.go sites that keyed on column nullness
(F1.1); (2) merge rule for local rows replaced — local side wins everything including tokens and
account is forced to LocalAccount to keep quota exclusion across arrival orders (F1.2); (3) §3.1
ErrorLog-rewriting approach replaced with `onClose` logging from `teeCloser`; warning dropped in
favour of existing `stream_incomplete` rule (F1.3, F1.4); (4) §3.2 BUSY dropped from retry set,
UpsertWarnings added, give-up-once-per-flush, IsTransient moved to store package (F1.6); (5) doctor
removed from R3 counters — only /api/health carries them (F1.7); (6) §3.5 targets.Active() fix,
liveFields caveat, boot edge case, ingest.go/SetLocalPrefixes wiring, "two → three" loopback routes,
loopback-only toggle note (F1.8, F1.9); (7) §7 gains three LM Studio behavioral pre-flight checks;
(a) is stop-and-ask (F1.10); (8) §5 gains new tests for writeGrouped local bucket, costCell label,
ValidateRejectsLocalAccountName, SetLocalFailsWithNoURL, boot fallback, merged-row quota burn (F1.1,
F1.2, F1.5, F1.8); (9) errors.Is for context.Canceled, decision made explicit (F1.5); (10) §4.1
gains 9 new files, removes doctor.go (F1.1, F1.4, F1.7, F1.8).

### v1 (draft)
Pricing rule revised before first review, on the user's instruction: every local token is charged at
DeepSeek's base, cheapest rate (replaces an earlier draft's configurable `LocalPriceAs`). Written against `main` at `a4f1a31` after reading `proxy.go`, `consumer.go`, `merge.go`,
`jsonlogs.go`, the billing sums in `store.go`, `reload.go`, and the Go 1.24.1 `reverseproxy.go`. An
Explore-agent report contributed the config and quota map; its claim that the stdlib line means a
*client* disconnect was wrong and is corrected in R2.
