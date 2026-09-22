# GI-11 — Cost is rounded to a cent per call, and a truncated capture is recorded as complete

**Issue**: GI#11 (GitHub) · **Branch**: `GI-11-cost-and-capture-fidelity` · **Beads**: `.beads/GI-11/` · **Plan version**: 4

## 1. The report

The DeepSeek platform usage page for **2026-09-21** reads **$3.25 / 1,994 requests / 183,854,144
tokens**. clens, on the same day, reads **$0.82 / 2,257 proxy calls / 209,153,629 tokens**.
Both figures are wrong in opposite directions, and they have three unrelated causes.

The investigation was read-only against the live install (`~/.clens/lens.db`, 1.47 GB) and the
real transcripts under `~/.claude/projects/**/*.jsonl`. Every number below is measured, not
inferred; the queries are reproduced in §8 so they can be re-run.

## 2. Three root causes

### RC-A — `Compute` rounds every token class to the nearest cent, per call

[pricing.go:133](../../internal/pricing/pricing.go#L133) sums **already-rounded** classes:

```go
total.Add(total, roundHalfUp(cost, centsPerUnit))   // inside the per-class loop
```

with `centsPerUnit = 1/100` ([:74](../../internal/pricing/pricing.go#L74)). A DeepSeek call costs
roughly $0.001 split across classes — input $0.0003, output $0.0005, cache-read $0.0002. **Each
class rounds to $0.00 on its own**, so the row stores $0.00 however many tokens it carried.

Measured on the live store for 2026-09-21 (`source='proxy'`):

| rounding scheme | Sept 21 total |
|---|---|
| current — per class, per call | **$0.820000** ← exactly what `SUM(cost_usd)` holds |
| once per call | $1.05 |
| exact | **$3.46** |

2,352 of 2,426 rows stored exactly `$0.000000` while carrying 196.68 M of the day's 201.09 M
cache-read tokens. Re-running the same rates, the same peak window and the same rounding in SQL
reproduces the stored figure to the cent, so this is the whole of the cost gap and not a
contributor to it.

**$3.46 is clens's own captured tokens priced at clens's own shipped rates, and DeepSeek invoices
$3.25** — a 6% agreement. The stored $0.82 is the outlier. Note the middle row: rounding once per
call still loses 70%, because a single sub-cent call rounds to zero either way. The rounding has
to leave `Compute` entirely, not just move.

The defect is deliberate, documented and pinned — the package doc at
[pricing.go:1-4](../../internal/pricing/pricing.go#L1-L4) states it, and
`TestComputeBatchRoundsPerClass` / `TestComputePeakRoundsPerClass` assert it. `TestComputePeakRoundsPerClass`
in fact asserts that **an off-peak call costs $0.00**
([pricing_test.go:457-465](../../internal/pricing/pricing_test.go#L457-L465)), which is the defect
written down as a requirement. The premise — "the per-class figure is the one an invoice line
reproduces" — is false: DeepSeek invoices per *month*, and the per-call granularity is what makes
the rounding destroy money. No display path depends on *cent granularity*: the CLI already formats
`$%.4f` ([format.go:78](../../internal/cli/format.go#L78)) and the dashboard already has a sub-cent
branch (`n < 0.01 ? toFixed(6) : toFixed(2)`, [app.js:28](../../internal/web/app.js#L28)). Three
paths do format at `%.2f` — `clens reconcile`'s computed/diverged cost
([reconcile.go:57](../../internal/cli/reconcile.go#L57), [:61](../../internal/cli/reconcile.go#L61))
and the replay gate's threshold message
([replay.go:141](../../internal/cli/replay.go#L141), the `%.2f` is the fixed
`replayCostThresholdUSD = 0.25` at [:24](../../internal/cli/replay.go#L24)) — so the RC-A fix changes
what those two show and, through the magnitude test at
[replay.go:130-143](../../internal/cli/replay.go#L130-L143), which replays trip `--yes`. That is the
intended effect, not a regression: those two paths consume the *value*, and the value is being
corrected; none of them depend on it being *rounded to a cent*.

### RC-B — the cross-source merge launders the completeness flag

[merge.go:241](../../internal/store/merge.go#L241):

```go
merged.CaptureComplete = existing.CaptureComplete || incoming.CaptureComplete
```

`CaptureComplete` is set on the proxy side as `!respBuf.truncated && !st.reqBody.truncated`
([proxy.go:107](../../internal/proxy/proxy.go#L107)) — it means "some body hit the read cap". A
JSONL row has no bodies at all, so it always reports `true`. The `||` therefore turns a truncated
proxy capture into a complete one, **while the merged row keeps the proxy's truncated body**
([merge.go:323-328](../../internal/store/merge.go#L323-L328) backfills bodies only from whichever
side has them — the transcript has none).

This is br-GI-7-08's defect reintroduced one layer up: that bead fixed the proxy submitting
`!respBuf.truncated` alone, and the merge undoes the fix. Measured over the rows a
`WHERE source='proxy'` filter reaches — 4,931 of them — cross-tabulated against the client's own
`Content-Length` header:

| `source_refs` | `capture_complete` | rows | truly truncated |
|---|---|---|---|
| `proxy,jsonl` | 1 | 3,405 | **2,613** |
| proxy-only | 1 | 1,289 | 0 |
| proxy-only | 0 | 237 | 230 |

(3,405 + 1,289 + 237 = 4,931.) Note the scope: `source` is the **first** writer's and is never
rewritten by a merge ([merge.go:165](../../internal/store/merge.go#L165) `merged := *existing`,
[:244](../../internal/store/merge.go#L244)), and `source_refs` is unioned existing-first
([:166](../../internal/store/merge.go#L166)) — so a transcript-first merge stores `source='jsonl'`
with `source_refs='jsonl,proxy'` and is **invisible** to `WHERE source='proxy'`. The plan's own §2
bead text calls proxy-first "the common ordering", not the only one, so this cross-tab measures the
proxy-first half of the merged population; the jsonl-first half (21 rows, all truncated) is not
counted here. The reproduction query in §8 and acceptance #3 are therefore widened to
`instr(source_refs,'proxy') > 0` so both orderings are covered.

**Every one of the 2,613 false-complete rows is a merged row.** Proxy-only rows never lie: 0
false-complete, 230 honest truncations.

The consequence is the opposite of what the warning suggests. `ruleStreamIncomplete` fires on
`!ev.CaptureComplete`, so on merged rows the truncation warning **under-fires** — the merge hides
the very condition the flag exists to report. The 1,274 Sept-21 rows carrying `stream_incomplete`
are the ones that *escaped* merging, not the ones that are damaged.

### RC-C — the 256 KB cap is too small for this workload

`BodyCapBytes` defaults to 262,144 ([config.go:59](../../internal/config/config.go#L59)) and
bounds **both** bodies ([proxy.go:88](../../internal/proxy/proxy.go#L88) for the response,
[:126](../../internal/proxy/proxy.go#L126) for the request).

Measured against the client's `Content-Length` across 4,873 proxy rows:

| | |
|---|---|
| request bodies over 256 KB | **2,845 (58%)** |
| request bodies over 1 MB | 106 |
| request bodies over 2 MB | **0** |
| largest request body seen | **1,246,222 bytes** |
| mean request body | 356,659 bytes |
| responses stored at exactly the cap | 599 |

So truncation is the norm, not the exception — and the dominant body is the **request**, not the
response the report named. A Claude Code request carries the system prompt, the full tool schemas
and the conversation history, and crosses 256 KB routinely; 58% of calls are affected against 12%
of responses.

**A 2 MB cap (2,097,152) captures every body this install has ever seen**, with headroom.

## 3. What is *not* a defect (explicitly out of scope)

The **token** gap is not attributable to a bug in clens and is not fixed here.

- The proxy captured 100% of traffic: the day's 2,802 client requests break down as 2,257
  `/v1/messages` calls, 144 `count_tokens` calls and 401 other rows — 2,257 + 144 + 401 = 2,802 —
  and 2,257 is exactly the proxy's `/v1/messages` row count.
- 297,080,674 is an all-source total that includes 407 `claude-sonnet-5` subscription rows from
  session `90300bd3` (12:21–13:28), which never touched DeepSeek at all. **The DeepSeek
  comparison is proxy-only: 209,153,629 vs 183,854,144.**
- The proxy includes 659 calls with no transcript line (32.02 M tokens). A transcript flushes on
  exit, so a session killed mid-flight loses it permanently while the proxy still captured the
  traffic; those rows are the only record and must not be dropped.
- The residual ~25.3 M is unattributed. Bounding it needs DeepSeek-side per-request data that the
  usage page does not expose (it publishes aggregates only). **No fix without a root cause**, so
  this is a follow-up, not a bead.

The **billing split** is already correct and is what makes the day look worse than it is: the
$20.59 in `api_equivalent_cost_usd` on those 407 subscription rows is never summed with
`cost_usd` (invariant 5), so it is not part of the $0.82 and must not be compared to the DeepSeek
page.

## 4. Changes

### Code

| File | Change |
|---|---|
| [internal/pricing/pricing.go](../../internal/pricing/pricing.go) | Drop the per-class `roundHalfUp` from the sum loop ([:133](../../internal/pricing/pricing.go#L133)); delete `centsPerUnit` ([:72-74](../../internal/pricing/pricing.go#L72-L74)) and `roundHalfUp` ([:173-183](../../internal/pricing/pricing.go#L173-L183)), now dead; rewrite the package doc ([:1-4](../../internal/pricing/pricing.go#L1-L4)) and the stale comment at [:106-109](../../internal/pricing/pricing.go#L106-L109) that argues for the rounding. |
| [internal/pricing/pricing_test.go](../../internal/pricing/pricing_test.go) | Rewrite the two tests that pin the defect (`TestComputeBatchRoundsPerClass`, `TestComputePeakRoundsPerClass`) and `TestComputePeakAndBatchCompose`; add a regression test that a call whose every class is sub-cent prices above zero and sums exactly. |
| [internal/store/merge.go](../../internal/store/merge.go) | `CaptureComplete` follows the body: the surviving row reports the flag of whichever side supplied the bodies it holds, not the `||` of both. **Assign it *after* the body backfill at [:323-328](../../internal/store/merge.go#L323-L328), not at its current site [:241](../../internal/store/merge.go#L241).** At `:241` the backfill has not run and `merged := *existing` ([:165](../../internal/store/merge.go#L165)) means the test reads `existing`'s *pre-merge* bodies: on the jsonl-first / rekey shape (`existing` is the JSONL taker with **no** bodies, `incoming` is the proxy row holding them) the "any body" test reads false and falls through to `existing.CaptureComplete` = the JSONL row's `true` — the very laundering the `||` did. The rule is undefined by the `||` when **both** sides carry bodies, and `ReqBody`/`RespBody` are backfilled *independently* ([:323-328](../../internal/store/merge.go#L323-L328)), so the request body's owner can differ from the response body's. After the backfill, derive the owner explicitly from whichever side actually supplied each retained body — request from one side, response from the other, both from one side, or neither — and take that owner's flag; when the merged row holds **no** body, keep `existing.CaptureComplete`, which is what makes the `--body-policy off` row (no bodies, flag `true`) and a proxy error row with no bodies both stay honest. The §4 rekey test case ("flag follows to false") must pass at the new location; if it cannot, the rule is wrong, not the test. Rewrite the comment at [:176-183](../../internal/store/merge.go#L176-L183), whose "the record that is whole is the safer one to quote" argument the `||` contradicts. |
| [internal/store/merge_test.go](../../internal/store/merge_test.go) | Six cases: proxy-first with the proxy truncated (flag stays false), proxy-first with the proxy whole (flag stays true), rekey collision where the JSONL taker takes the proxy's bodies (flag follows to false), `--body-policy off` (no bodies, flag stays true), the **both-sides-carry-bodies** case the existing fixtures already produce (`fullEvent` sets both `ReqBody` and `RespBody`, [store_test.go:72-73](../../internal/store/store_test.go#L72-L73)) — pinning that the surviving flag follows the side the kept bodies came from — and a proxy row that errored with no bodies ([proxy.go:49-53](../../internal/proxy/proxy.go#L49-L53) passes `false`) so the "neither side has a body" default is pinned in the errored direction too. **`TestMergePrecedenceTruncatedVsComplete` must be inverted, not deleted:** its fixture inserts a truncated `fullEvent` first, so under the new rule the surviving bodies are that side's and line [merge_test.go:297-298](../../internal/store/merge_test.go#L297-L298) becomes `CaptureComplete == false`; same reasoning §5 already applies to `TestComputePeakRoundsPerClass`. Re-grep the tree for other assertions on the merged flag — [merge_test.go:873-875](../../internal/store/merge_test.go#L873-L875) asserts the same `true` and survives only because its `existing` side carries bodies. |
| [internal/config/config.go](../../internal/config/config.go) | `BodyCapBytes` default 262,144 → **2,097,152** ([:59](../../internal/config/config.go#L59)); add the **missing** doc comment on the `BodyCapBytes` field ([:36](../../internal/config/config.go#L36) — the field is currently undocumented) and update the flag help ([:222](../../internal/config/config.go#L222)). The earlier "doc comment at [:319]" reference is dropped: [:318-326](../../internal/config/config.go#L318-L326) is the `BodyPolicy: "truncated"` rejection paragraph, which names `BodyCapBytes` only as the bound both policies shared — nothing there documents the field. While there, correct that paragraph's "both bounded by `BodyCapBytes`" phrasing only if it reads as still true of the new default; it is a historical note about the removed `truncated` policy. |
| [internal/config/config_test.go](../../internal/config/config_test.go) | Pin the new default, and that an explicit `CLENS_BODY_CAP_BYTES` / `--body-cap-bytes` still overrides it. |
| [internal/cli/reprice.go](../../internal/cli/reprice.go) *(new)* | `clens reprice` — **the CLI shell only**: flags, `--dry-run`/`--yes`, and reporting, exactly as `merge` and `rekey` split their CLI from their store work. The write loop lives in `internal/store` (new row below). **The pricing table:** recompute against the **effective, Loader-backed table** — `pricing.NewLoader(pricing.DefaultPath(), nil).Table()`, matching [models.go:34](../../internal/cli/models.go#L34) and [prices.go:50](../../internal/cli/prices.go#L50),[:94](../../internal/cli/prices.go#L94). **Do not use `pricing.Compute`** — it is `ShippedTable().Compute` ([pricing.go:76-82](../../internal/pricing/pricing.go#L76-L82)) and would silently ignore a user override, mispricing precisely the rows a user tuned by hand. **Routing (invariant 5):** a `subscription` row's new figure goes to `api_equivalent_cost_usd` and leaves `cost_usd` NULL; any other `billing_mode` goes to `cost_usd` — exactly the switch at [consumer.go:321-326](../../internal/consumer/consumer.go#L321-L326) and [jsonlogs.go:461-466](../../internal/jsonlogs/jsonlogs.go#L461-L466). **Scoping:** price every row whose `model_resolved` resolves in the effective table. A `user` row **is** included — its cost was produced by the same `Compute` with the same per-class rounding, so RC-A corrupted it identically, and against the effective table it is reconstructible (its rate lives in the override file, [pricing.go:185-189](../../internal/pricing/pricing.go#L185-L189)); "the user overrode it" is not a reason to skip it. Only two kinds are skipped and counted: `approximate:cache_ttl_unknown` rows (`TTLUnknown` is not a stored column, so the label is not derivable from a row) and rows whose recompute returns `unpriced` (their `model_resolved` is no longer in the table, [pricing.go:93-94](../../internal/pricing/pricing.go#L93-L94)) — the latter keep their stored cost and `cost_source`, never nulled, so `reprice --yes` is not silently destructive on exactly the rows it cannot price. Mirrors [purge.go](../../internal/cli/purge.go): `--dry-run` prints what `--yes` would change, and nothing happens without `--yes`. |
| [internal/store](../../internal/store) *(new method)* | `Store.RepriceCosts(...)` — the tx-taking write loop the CLI shell calls. **One transaction, store-owned:** in a single `BeginTx`, `UPDATE events SET cost_usd / api_equivalent_cost_usd / cost_source …` for the rows in scope, then re-derive every distinct owning `session_id` whose cost columns moved via `reconcileSessionTx`, and `Commit` — mirroring `InsertEvents`' distinct-session `reconcileSessionTx` loop before `Commit` ([store.go:301-316](../../internal/store/store.go#L301-L316)). The two `sessions.total_cost_usd` / `total_api_equivalent_cost_usd` columns are *materialized* ([schema.sql:89-90](../../internal/store/schema.sql#L89-L90)) and re-derived, never incremented; `reconcileSessionTx`'s own routing is the `SUM(CASE WHEN billing_mode='api' THEN cost_usd END)` / `SUM(CASE WHEN billing_mode='subscription' THEN api_equivalent_cost_usd END)` pair at [store.go:731-732](../../internal/store/store.go#L731-L732). The merge already re-derives in one transaction (`applyMergeTx`, pinned by `TestMergeRederivesSessionTotals`), and reprice must too — otherwise `clens stats`, `clens sessions` and `/api/sessions` keep showing the pre-fix session totals while `events` is corrected. **Do NOT call `ReconcileSession` from inside the reprice transaction:** it opens its **own** `BeginTx` ([store.go:695-705](../../internal/store/store.go#L695-L705)) and the pool is pinned to one connection ([store.go:76](../../internal/store/store.go#L76), `SetMaxOpenConns(1)`), so a nested `BeginTx` blocks on the pool with **no deadline — a hang, not an error**. Use the tx-taking `reconcileSessionTx`; that it is unexported, and therefore unreachable from `internal/cli`, is exactly why the write loop must live in `internal/store` — the same reasoning recorded in [docs/planning/GI-9-merge-jsonl-and-proxy-rows.md:744-752](../../docs/planning/GI-9-merge-jsonl-and-proxy-rows.md#L744-L752). |
| [cmd/clens/main.go](../../cmd/clens/main.go) | Register `"reprice": cli.Reprice` in the command table. |
| [cmd/clens/main_test.go](../../cmd/clens/main_test.go) | The new dispatch entry breaks `TestEveryCarriedOverCommandIsDispatched`: `carriedOver` is a hand-written allowlist and the test fails hard on any count mismatch ([main_test.go:40-47](../../cmd/clens/main_test.go#L40-L47)). Add `"reprice"` to the list (19 → 20) and update the **two** doc comments that name the count — [:10-14](../../cmd/clens/main_test.go#L10-L14) and [:26](../../cmd/clens/main_test.go#L26) both read "the 19 subcommands" — so neither is left stale at 19 while the list goes to 20. |

### Why a reprice command is in scope

RC-A leaves every stored cost wrong. Without a reprice path, the fix is invisible for all
historical data — including the 2026-09-21 view that produced this report — and the user's next
action would be a manual `sqlite3` session. It is bounded (the pricing package is pure, and every
input `Compute` needs — model, the five token columns, `speed`, `service_tier`, `started_at`,
`billing_mode` — is already a column on the row). It is idempotent **on every row it rewrites** —
recomputing a priced row yields the same answer, so a second `--yes` run is a no-op — with one
carve-out that must be honoured rather than glossed: a row whose `model_resolved` no longer resolves
recomputes to `(nil, "unpriced")` ([pricing.go:93-94](../../internal/pricing/pricing.go#L93-L94)), so
writing the result back would NULL a figure that was previously stored and drop the row out of the
priced population forever. That row is therefore left untouched — its
stored cost and `cost_source` kept, and counted as skipped — so a repeated run reports it as skipped
every time rather than destroying it. It needs no schema change and no marker column.

### The time-window picker on Calls and Stats

**A scope addition, requested mid-story. It earns its place on the merits.** The report that opened
GI-11 was a comparison against a *day* — DeepSeek's page for 21 Sept — and C-1 established that no
shipped surface can express that day: `clens stats --by day` buckets by **UTC**
([store.go:1252](../../internal/store/store.go#L1252)) while the dashboard renders **local** time
(`d.toLocaleString()`, [app.js:31-36](../../internal/web/app.js#L31-L36)), and the Calls tab has no
time filter at all. The one window §5's entire acceptance section is written against is therefore
the one window the UI cannot ask for. The picker makes it expressible, and makes the reprice's
effect visible on the surface the operator was reading when the numbers did not match.

| File | Change |
|---|---|
| [internal/web/index.html](../../internal/web/index.html) | Add the shared picker to the Calls filter row ([:54-63](../../internal/web/index.html#L54-L63)). On Stats ([:89-98](../../internal/web/index.html#L89-L98)) it **replaces** the free-text `s-since`/`s-until` inputs, which the `custom` granularity keeps reachable. |
| [internal/web/app.js](../../internal/web/app.js) | One `timeWindow()` helper (granularity + value → `{since, until}` RFC3339 strings); `callFilter()` ([:275-286](../../internal/web/app.js#L275-L286)) sets them; `loadStats()` ([:495-499](../../internal/web/app.js#L495-L499)) reads the picker; a change handler mirroring `f-apply` ([:928](../../internal/web/app.js#L928)). |
| [internal/web/assets_test.go](../../internal/web/assets_test.go) | The `timeWindow()` cases in §5. `TestAssetsEveryLookupHasAMount` ([:35](../../internal/web/assets_test.go#L35)) already covers the new element ids — it extracts every `$('…')` in `app.js` and asserts `index.html` mounts it, so a typo'd id fails the build instead of silently no-op'ing. |
| [internal/web/style.css](../../internal/web/style.css) | Only if the new controls need it; the existing `.row`/`label` classes already carry the filter rows. |

**The control — one implementation, two mounts.** A granularity `<select>` of
`hour | date | month | custom`, and the input it reveals:

| Granularity | Input | Window (half-open) |
|---|---|---|
| `hour` | `<input type="datetime-local">` | `[H:00, H+1:00)` |
| `date` | `<input type="date">` | `[D 00:00, D+1 00:00)` |
| `month` | `<input type="month">` | `[M-01 00:00, M+1-01 00:00)` |
| `custom` | the existing free-text `since`/`until` pair | whatever the operator types |

**Native inputs, no library.** `type="date"`, `type="month"` and `type="datetime-local"` are platform
features. The dashboard is hand-written vanilla JS with no build step and no dependencies — the same
reasoning that made `chartByPeriod` an inline SVG rather than a charting library
([app.js:538-542](../../internal/web/app.js#L538-L542): "a library is a build step or a CDN, both of
which `embed.go`'s rationale rules out"). A calendar library is exactly that, so there is not one.

**Wire format: RFC3339 carrying the local offset — never a duration.** `parseTimeBoundParam` accepts
both ([api.go:293-305](../../internal/api/api.go#L293-L305)), but a duration is resolved against
`time.Now()` *at request time* ([:298-300](../../internal/api/api.go#L298-L300)), and `24h` cannot
express "the local day of 21 September". The picker therefore sends an absolute instant carrying its
offset, e.g. `since=2026-09-21T00:00:00+05:30`. The failure mode here is loud rather than silent, and
that is worth stating because it is the opposite of this story's other two defects: `RFC3339`
*requires* an offset, so a bare local time is rejected by the parser and surfaces as a 400, not as a
window quietly shifted by 5h30m.

Verified directly against `time.Parse(time.RFC3339, …)`, and it catches more than the offset case:

| Input | Result |
|---|---|
| `2026-09-21T00:00:00` | **rejected** — `cannot parse "" as "Z07:00"` |
| `2026-09-21` (raw `<input type="date">`) | **rejected** — `cannot parse "" as "T"` |
| `2026-09` (raw `<input type="month">`) | **rejected** — `cannot parse "" as "-"` |
| `2026-09-21T00:00:00+05:30` | accepted |

**So the control's raw value is never a valid query param** — not even for `date` and `month`, whose
`<input>` values are bare `YYYY-MM-DD` / `YYYY-MM`. `timeWindow()` must expand the picked value into
a full instant **with an offset** before it reaches the query string. This is the single detail most
likely to be got wrong, and the saving grace is that it fails loudly: every filter returns a 400
rather than filtering to a subtly wrong window.

**Local time, deliberately — this is the C-1 fix.** The window is computed in the browser's zone and
sent with its offset, so the range the operator picked is the range the server filters on. Today
those two disagree, and that disagreement is precisely how a local-day $0.82 came to be read against
a UTC-day $0.75. The `date` granularity therefore means the **local** day, and the `started_at` range
it produces for 21 Sept is the same half-open IST range §5's acceptance is written against. One
window, computed in one place — otherwise the UI and the acceptance query disagree and the plan has
rebuilt the bug it is fixing.

**No server, store, or API change.** `/api/requests` parses `since`/`until` today
([api.go:351-367](../../internal/api/api.go#L351-L367)) and plumbs them into
`EventFilter.Since/Until` ([types.go:168-169](../../internal/store/types.go#L168-L169)); the Calls tab
simply never sets them ([app.js:275-286](../../internal/web/app.js#L275-L286)). This is a
front-end-only change, and that is why it belongs here rather than in a follow-up.

**Two behaviours that must not be lost.** A picker change resets `callState.offset = 0` before
reloading, exactly as `f-apply` does ([:928](../../internal/web/app.js#L928)) — page 4 of the old
window is not page 4 of the new one. And `custom` keeps free text working on Stats, so nothing that
works today stops working; replacing the text inputs outright would silently narrow `24h` and
arbitrary RFC3339 ranges out of existence.

### Docs

| File | Change |
|---|---|
| [docs/context/cost-and-quota.md](../../docs/context/cost-and-quota.md) | §"Exact money, rounded per token class" ([:72-81](../../docs/context/cost-and-quota.md#L72-L81)) documents the rounding as an invariant and links **one** test by name — `TestComputeBatchRoundsPerClass` at [:78](../../docs/context/cost-and-quota.md#L78) (the plan's "links the two tests" overstates it; the second test lives only in `docs/planning/GI-3-deepseek-peak-pricing.md`); rewrite it to the round-at-display rule. Update the `Compute` description at [:97-100](../../docs/context/cost-and-quota.md#L97-L100). |
| [docs/context/storage-schema.md](../../docs/context/storage-schema.md) | The `capture_complete` paragraph ([:95-103](../../docs/context/storage-schema.md#L95-L103)) is **stale against the code**: it claims the flag also covers "ended without a `message_stop` event", but [proxy.go:107](../../internal/proxy/proxy.go#L107) computes it from the two buffers' `truncated` bits alone. Fix that, and state the merge rule. **Also record the cap-raise consequence (F1.8):** the length-vs-cap truncation *marker* compares a stored body against the **current** cap, so raising the default to 2 MB silently reclassifies every historical at-cap row (262,144 bytes) as `Complete` — on exactly the rows RC-B has just started flagging honestly. Document that this is a known, accepted limitation of the marker (a per-row recorded cap would be required to mark old rows, and the plan does not add one) and that the authoritative signal is the `capture_complete` flag, not the marker. |
| [docs/context/build-and-run.md](../../docs/context/build-and-run.md) | The body-cap default at [:85](../../docs/context/build-and-run.md#L85) (`262144 (256 KB)`) and its storage trade-off. |
| [docs/context/cli-and-tooling.md](../../docs/context/cli-and-tooling.md) | Adding a subcommand — the module [INDEX.md:33](../../docs/context/INDEX.md#L33) routes to for exactly this change. "of 19 subcommands" at [:6](../../docs/context/cli-and-tooling.md#L6) → 20; add `reprice` to the Commands table ([:15-35](../../docs/context/cli-and-tooling.md#L15-L35)); revisit "**The two destructive commands**" ([:37-49](../../docs/context/cli-and-tooling.md#L37-L49)), which will read as three once `reprice` — a cost-column writer gated on `--yes` — is added; and narrow `ingest --rebuild`'s "**the re-pricing path**" at [:19](../../docs/context/cli-and-tooling.md#L19) now that a dedicated command exists. |
| [docs/context/INDEX.md](../../docs/context/INDEX.md) | [:33](../../docs/context/INDEX.md#L33) "the 19-entry dispatch table" → 20. |
| [docs/context/testing-and-quality.md](../../docs/context/testing-and-quality.md) | the hand-maintained test-file count at [:12](../../docs/context/testing-and-quality.md#L12) ("**52 test files**") moves with the new `reprice` file + its test; re-measure and update. |
| [docs/context/data-privacy-and-compliance.md](../../docs/context/data-privacy-and-compliance.md) | the 256 KB default as fact at [:37](../../docs/context/data-privacy-and-compliance.md#L37) and [:61](../../docs/context/data-privacy-and-compliance.md#L61); this is the module INDEX routes "before changing what is captured" to, and §7 requires the larger-cap privacy cost be stated here rather than discovered. Note [:66-68](../../docs/context/data-privacy-and-compliance.md#L66-L68) already records that the marker inference is "only as good as the cap not having changed" — it is the doc-side half of F1.8. |
| [docs/context/glossary.md](../../docs/context/glossary.md) | "**body cap**" entry ([:51](../../docs/context/glossary.md#L51)) states `default 262144` → 2,097,152. |
| [docs/context/decisions/003-full-bodies-stored.md](../../docs/context/decisions/003-full-bodies-stored.md) | "bounded by a 256 KB cap" at [:15](../../docs/context/decisions/003-full-bodies-stored.md#L15) and the `--body-cap-bytes` row at [:20](../../docs/context/decisions/003-full-bodies-stored.md#L20) — note the new default; the decision itself (full bodies under a bounded cap) is unchanged. |
| [CLAUDE.md](../../CLAUDE.md) | [:128](../../CLAUDE.md#L128) states "policy + 256 KB cap" and is the *enforced-conventions* file; update the number so it states a true bound. |
| [README.md](../../README.md) | Any 256 KB / `body-cap` mention, and the `clens` command list for `reprice` (the table at [:88-105](../../README.md#L88-L105)). **The section "### Re-pricing rows already captured" ([:111-120](../../README.md#L111-L120)) must be rewritten, not just added to:** it currently states that there is *no separate reprice command* and that `clens ingest --rebuild` is the re-pricing path. Replace it with what `reprice` does and *why `--rebuild` cannot do the job for RC-A's rows*: a re-ingest is a **JSONL** row, priced with `speed=""`/`serviceTier=""` ([jsonlogs.go:459](../../internal/jsonlogs/jsonlogs.go#L459)) and absorbed by the `request_id` merge, which never replaces the proxy's bodies ([merge.go:323-328](../../internal/store/merge.go#L323-L328)) — so it cannot reach the proxy-only rows that carry the zeroed costs. This is also the one place the repo records the *design intent* for having no reprice command, so §7's rejects-alternatives paragraph reasons from `purge`/`rekey` and must address this section too. |
| stale code comments (grep sweep) | `internal/consumer/consumer.go:25` — `defaultBodyCapBytes = 262144`, the consumer's own independent fallback, which silently disagrees with the new default whenever the `SetBodyPolicy`/cap seam is left unwired; `internal/analyze/rules.go:72` and `internal/decode/decode.go:90` — both state "256 KB" in prose; `internal/cli/export.go:133` — "reading the blobs here would be 256 KB per row", stale the moment the default is 2 MB. |

### Not code: the database moves to `D:` — run as part of this story

`DBPath` is already configurable (`CLENS_DB_PATH`, `--db-path`,
[internal/config/config.go:34](../../internal/config/config.go#L34)), so this is **configuration,
not a change**: set `DBPath = "D:/clens/lens.db"` in `~/.clens/config.toml` and move the file. It
belongs in this story's rollout because RC-C grows the store — projected **1.44 GB → ~1.9 GB**
(2,845 request bodies gain ~95 KB on average, plus 106 rows up to +1 MB) — and that growth is
much better spent on `D:` than on `C:`.

**Agreed: this is performed as part of the story, not merely documented.** Steps, in order, with
`clens` stopped:

1. Stop the running `clens serve`.
2. **Copy** (never move) `~/.clens/lens.db` to `D:/clens/lens.db` — the original stays as the
   fallback until step 6.
3. Set `DBPath = "D:/clens/lens.db"` in `~/.clens/config.toml`.
4. Start `clens serve` and confirm the new path is the one in use: `clens doctor`.
5. Confirm the store is intact and the row count matches the original.
6. Only then remove the copy on `C:`.

There is no code in this step and no bead for it beyond the docs update; it is a rollout action
recorded here so it is not lost between the cap change and the reprice run. It is **destructive
only at step 6**, which is why the copy is verified before the original is deleted.

## 5. Test strategy

**Unit — the defect is pinned at the boundary, not the symptom.**

- `TestComputePricesSubCentClassesExactly`: a call whose *every* class is sub-cent
  (e.g. 300 tokens at $10/MTok per class) must return a non-zero total equal to the exact sum.
  This is the case `TestComputePeakRoundsPerClass` currently asserts is $0.00; it must invert.
- `TestComputeBatchHalvesExactly` / peak equivalents: keep the batch and peak semantics
  (halve, double, compose) while asserting **exact** values, so the modifier tests stop being
  rounding tests. `TestComputePeakAndBatchCompose`'s "halving and doubling cancel exactly" claim
  becomes literally true rather than true-by-rounding.
- `TestComputeSixClassesIndependently` is unaffected (whole-dollar classes) and stays as the
  thinking/subset guard.
- Merge: the six `CaptureComplete` cases above, plus an assertion that the merged row's bodies and
  its flag agree — the invariant the `||` broke. `TestMergePrecedenceTruncatedVsComplete`
  **inverts** (`CaptureComplete` becomes `false`; see §4) rather than being deleted, on the same
  reasoning this section applies to `TestComputePeakRoundsPerClass`. `TestBillingModeInvariants`
  and the `source_mismatch` tests must stay green, since RC-B touches the same function.
- Config: new default + override still wins.
- Reprice: a store fixture with a known-wrong row reprices to the exact value; `--dry-run` changes
  nothing; a `user` row reprices against the effective table (its override rate applies) rather than
  being skipped; an `approximate:` row is skipped and counted; a row whose model no longer resolves
  keeps its stored cost/source and is counted as skipped; and the **owning session's**
  `total_cost_usd` moves with the row in the same transaction (the F1.1 rollup). The write loop is
  exercised against `Store.RepriceCosts` (the tx-taking store method), so the same-transaction
  rollup is a store test, not a CLI one.
- Picker (§4): `timeWindow()` cases for each granularity, and the three a naive implementation gets
  wrong — month rollover (Dec → Jan); the month width **not** being a constant (28–31 days, so a
  `+30d` shortcut is wrong); and **DST**, where a local `date` window must be built with the `Date`
  constructor (`new Date(y, m, d)` → `new Date(y, m, d + 1)`) rather than as `start + 86_400_000`,
  because a day in a zone with a transition is 23 or 25 hours long. IST has no DST, but the
  operator's zone is not something this code gets to assume. Also: the emitted `since`/`until`
  round-trip through `parseTimeBoundParam` without a 400, and a `date` window for 21 Sept equals
  §5's acceptance range exactly — the assertion that keeps the UI and the acceptance query from
  drifting apart.

**Integration.**

- The proxy cap test at `bodyCap = 64` ([proxy_test.go:462](../../internal/proxy/proxy_test.go#L462))
  stays valid — it is cap-relative, not cap-specific. Add a case at the *real* default asserting a
  1.2 MB request body is captured whole and flagged complete, which is the end-to-end form of RC-C.
- **A newly captured day** after the cap change shows the `incomplete` count falling again — the
  forward half of acceptance #3, on a store whose bodies were captured under the new 2 MB cap
  (the copied historical store cannot show this; see §6).
- The TTFB gate must stay green: the cap change is a buffer size, and a larger buffer is
  marginally more work on the hot path, so this is exactly the invariant to re-assert rather than
  assume.

**Acceptance — against a copy of the live store, per this repo's convention** (GI-7 and GI-9 both
ran against copies):

**The acceptance window is the local IST day 2026-09-21**, written as a half-open `started_at` range
in unix nanoseconds: `[1789929000000000000, 1790015400000000000)` (start inclusive, end exclusive;
the two values differ by exactly 86,400 s). Two things this must state plainly: `events` has **no
`day` column** ([schema.sql:11-66](../../internal/store/schema.sql#L11-L66)); `day` is a derived
expression, and `clens stats --by day` buckets by **UTC**
([store.go:1252](../../internal/store/store.go#L1252) — `DATE(started_at/1e9,'unixepoch')`), so `clens
stats --by day` does **not** reproduce these figures. The acceptance query is a **direct `events`
query, not a shipped subcommand**.

1. `SELECT SUM(cost_usd) FROM events WHERE source='proxy' AND started_at >= 1789929000000000000
   AND started_at < 1790015400000000000` moves from **0.82** to **3.46 ± 0.05**, i.e. within 7% of
   DeepSeek's 3.25.
2. **Session rollup (same transaction, F1.1):** for every session the reprice touched, its
   `sessions.total_cost_usd` (and `total_api_equivalent_cost_usd`) is re-derived in the reprice's
   own transaction and moves with the day's `SUM(events.cost_usd)` for that session — assert one
   affected session's stored total equals the post-reprice `SUM` over its `events`, not the pre-fix
   value.
3. `capture_complete=0` count on that day window rises from **0** to **1,176** (honest truncation)
   *before* the cap raise, over the widened scope `instr(source_refs,'proxy') > 0` so jsonl-first
   merges are counted too. Both figures are measured for the **IST window**: **0** rows are
   `capture_complete=0` today, and **1,176** is the count of false-complete rows
   (`capture_complete=1` yet a stored body that is a strict prefix of its client `Content-Length`)
   that RC-B reclassifies — bounded by the day's 2,426 proxy rows, and **not** the all-time
   `237 → 2,850` figure from §2. The "falls again once the cap is 2 MB" half is **not** runnable
   against the copied store — a past day cannot be re-captured, and §6 says so — so it moves to a
   check on a **newly captured day** after the cap change, in §"Test strategy"'s integration section.
4. **The RC-B invariant, as a query.** Assert that **no in-scope row** has `capture_complete = 1`
   while its stored body is a strict *prefix* of its `Content-Length`:
   `SELECT COUNT(*) FROM events WHERE instr(source_refs,'proxy') > 0 AND capture_complete = 1 AND
   CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) > length(req_body)` — must be
   **0**. The predicate is the prefix relation, **not** `length(req_body) = cap`. A body captured
   whole *at exactly* `cap` bytes stores `length == cap` with `capture_complete = 1` legitimately —
   the proxy sets `truncated` only when a write must drop bytes
   ([proxy.go:272-286](../../internal/proxy/proxy.go#L272-L286)), and §2 reports 599 such responses —
   so a `length = cap` test is not an invariant the code holds. It is also cap-bound: vacuous against
   the new 2 MB default (historical prefixes are 262,144 bytes) and, run against 262,144, it
   re-introduces the length-vs-cap blindness §4/§6 record as an accepted limitation (F1.8). The
   prefix test needs no cap at all.

## 6. Risks and edge cases

| Risk | Handling |
|---|---|
| **RC-B alone makes the dashboard look worse** — 2,613 rows stop claiming completeness, so `stream_incomplete` jumps before it falls. | RC-B and RC-C are ordered in the same story and shipped together; the warning count is honest at every point, and the cap raise is what actually removes the truncation. |
| **float64 accumulation** across 2,257 rows in SQLite `REAL`. | Error is ~1e-16 relative per operation; the exact-$3.46 target is asserted to ±$0.05, four orders of magnitude above the noise. `big.Rat` stays the compute type; only the stored value is a float, as today. |
| **`roundHalfUp` deletion** breaks a caller I have not found. | Grepped: its only production reference is [pricing.go:133](../../internal/pricing/pricing.go#L133); the rest are comments. The compiler is the check. |
| **Reprice rewrites a cost the user deliberately overrode.** | Not a risk once reprice uses the effective table: a user edit overrides a **rate** ([pricing.go:185-189](../../internal/pricing/pricing.go#L185-L189)), not a per-row cost, and reprice prices with the same `NewLoader(DefaultPath(), nil).Table()`, so a `user` row recomputes to that same override rate. Only `approximate:` and unresolvable (`unpriced`) rows are skipped. |
| **Reprice cannot reconstruct `approximate:cache_ttl_unknown`.** | `TTLUnknown` is not a stored column. Those rows are skipped, not relabelled. |
| **Cap raise and storage.** | 2 MB bound, ~0.45 GB projected growth, moved to `D:`. `BodyCapBytes` stays configurable so it can be lowered without a rebuild. |
| **A larger cap changes merge precedence.** | The *stated mechanism was wrong* (F1.4) and is corrected here: the winner rule reads the two **input** flags ([merge.go:184-198](../../internal/store/merge.go#L184-L198)); the merged flag is assigned 43 lines later at [:241](../../internal/store/merge.go#L241) and is never read by `mergeEvents`, so RC-B **cannot** change the pick inside the merge that applies it. A *later* merge of the already-merged row could take the `!existing.CaptureComplete && incoming.CaptureComplete` branch, but `usageObserved` ([merge.go:214-224](../../internal/store/merge.go#L214-L224), [:372-376](../../internal/store/merge.go#L372-L376)) swaps the pick back whenever the complete side has no measurement — always true of a JSONL row. So: no regression, for a different reason than first given, intended, and covered by the merge tests. |
| **Historical at-cap rows lose their truncation *marker*.** | Raising the default cap reclassifies every historical row whose body sits at the old 262,144 boundary as `Complete` in the two length-vs-cap readers — they compare the stored body against the **current** cap ([app.js:214-216](../../internal/web/app.js#L214-L216), [decode.go:50-55](../../internal/decode/decode.go#L50-L55), [show.go:113](../../internal/cli/show.go#L113)). The `capture_complete` flag is unaffected and stays authoritative. This is an **accepted limitation, documented, not fixed** — marking old rows would need a per-row recorded cap, which the plan deliberately does not add. Recorded in §4's storage-schema row and in `data-privacy-and-compliance.md`. |
| **Historical rows.** | Only `reprice` fixes their cost, and only for the rows in its scope. A day whose bodies are already truncated cannot be re-captured — RC-C is forward-only. This must be said plainly in the plan and the PR. |
| **The picker's window and the acceptance window drift apart** (§4 picker). | Both must resolve to the *same* half-open IST range for 21 Sept. The window is computed in exactly one place (`timeWindow()`) and pinned by §5's round-trip assertion; a second implementation of "the day" would rebuild the C-1 defect inside the fix for it. |
| **DST in the operator's zone.** | A local day is 23 or 25 hours across a transition, so the window is built from local midnights with the `Date` constructor, never as `start + 86_400_000`. IST has no DST; the code does not assume that of every operator. |
| **The picker is not free — it widens the story.** | It is a fourth, unrelated change on top of three defect classes, and it is the one part of GI-11 that ships a *feature* rather than a correction. It is front-end-only (no server, store, or API change), which is what keeps it from delaying the three fixes; if the story needs to shrink, this is the part to cut, not RC-A/B/C. |

## 7. Self-review

**As a senior engineer.** The three causes are independent and each has a one-expression fix, so
the risk is not in the edits but in the *sequence*: RC-B without RC-C turns a silent problem into a
loud one, and RC-A without `reprice` fixes only the future. The alternative to `reprice` — a
migration that recomputes on open — was rejected: it would put the pricing table on `Open`'s path,
make every boot O(events), and the repo's existing shape for "rewrite stored rows" is an explicit
`--dry-run`/`--yes` CLI command, twice over (`purge`, `rekey`). The alternative to raising the cap —
storing a ring buffer of the tail — was rejected because for SSE the usage-bearing events straddle
both ends (`message_start` carries input/cache at the head, `message_delta`/`message_stop` carry
output at the tail), so no single-ended retention is correct; 2 MB simply holds the whole thing,
and the largest body this install has produced is 1.19 MB.

**As a QA engineer.** The rounding fix is only proven if the test that *currently asserts $0.00* is
inverted rather than deleted — deleting it would leave the behaviour unpinned and it would come
back. The merge fix needs the `--body-policy off` case explicitly, because that row reports
`CaptureComplete: true` with no bodies at all, and a naive "flag follows the body" rule would
either blank it or thrash it. The rule is therefore written as an **expression**, not prose (F1.3),
and — per F2.2 — it must be assigned **after** the body backfill
([:323-328](../../internal/store/merge.go#L323-L328)), where `merged.ReqBody`/`merged.RespBody` hold
the bodies the row actually keeps; assigned at [:241](../../internal/store/merge.go#L241) the
backfill has not run and the test reads the pre-merge side's bodies, which is how a jsonl-first merge
still launders a truncated proxy capture. The expression derives the owner from whichever side
supplied each retained body (request and response independent): when the merged row holds any body it
takes that owner's flag — the case the existing both-bodies fixtures produce — and when it holds none
it keeps `existing.CaptureComplete`. That covers every reachable shape, including a proxy row that
errored with no bodies. Error paths: a reprice with
`--yes` but a closed store, an unpriced row, a row with no `model_resolved`.

**As a security engineer.** No new credential surface, no new endpoint, no new outbound call. The
reprice command is the only new write path into `events`; it writes cost columns only, touches no
body and no header, and is read-only without `--yes`. The cap raise enlarges stored bodies — and
`CLAUDE.md` is explicit that **the bodies are the asset this repo protects**: a bigger cap stores
more prompt and file content, which is a real privacy cost paid for observability. That is the
correct trade for a loopback-only single-user tool, but it must be stated in the docs rather than
discovered, and the DB move to `D:` should not be done in a way that widens access. The DB carries
no credential (bodies are redacted before insert and `internal/secret` lives outside the DB), so a
larger file is not a larger blast radius for secrets.

## 8. Reproduction queries

Run against a **copy** of the live store. Read-only; each backs a claim above.

The acceptance window is the **local IST day 2026-09-21**, a half-open `started_at` range in unix
nanoseconds: `[1789929000000000000, 1790015400000000000)`. There is no `day` column on `events`
([schema.sql:11-66](../../internal/store/schema.sql#L11-L66)) and `clens stats --by day` buckets by
UTC ([store.go:1252](../../internal/store/store.go#L1252)), so these are direct `events` queries, not
the shipped subcommand.

```sql
-- RC-A: the stored total, and the exact total at the same rates/peak window, over the IST day
SELECT SUM(cost_usd) FROM events
 WHERE source='proxy'
   AND started_at >= 1789929000000000000 AND started_at < 1790015400000000000;
-- → 0.82 ; the exact recomputation gives 3.46

-- RC-A: rows stored at zero while carrying cache reads
SELECT COUNT(*), SUM(cache_read_tokens) FROM events
 WHERE source='proxy' AND cost_usd = 0
   AND started_at >= 1789929000000000000 AND started_at < 1790015400000000000;
-- → 2352 rows, 196.68M cache-read tokens

-- RC-B: the flag is laundered only by merges. Widened to instr(source_refs,'proxy') > 0 so
-- jsonl-first merges (source='jsonl', source_refs='jsonl,proxy') are counted, not just proxy-first.
WITH x AS (
  SELECT source_refs, capture_complete AS cc, length(req_body) AS stored,
         CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) AS true_len
    FROM events WHERE instr(source_refs,'proxy') > 0)
SELECT source_refs, cc, COUNT(*),
       SUM(CASE WHEN true_len > stored THEN 1 ELSE 0 END) AS trunc
  FROM x GROUP BY source_refs, cc;
-- → proxy,jsonl / cc=1 / 3405 rows / 2613 truncated
--   proxy-only / cc=1 / 1289 rows /    0 truncated
--   proxy-only / cc=0 /  237 rows /  230 truncated
--   jsonl,proxy / cc=1 /   21 rows /   21 truncated
--   3,405 + 1,289 + 237 + 21 = 4,952 reached rows
--   (the fourth bucket is the jsonl-first half §2 notes as "not counted" over source='proxy')

-- RC-C: how far past the cap real bodies go
WITH x AS (
  SELECT CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) AS n
    FROM events WHERE source='proxy')
SELECT SUM(n > 262144), SUM(n > 1048576), SUM(n > 2097152), MAX(n), CAST(AVG(n) AS INT)
  FROM x WHERE n IS NOT NULL;
-- → 2845, 106, 0, 1246222, 356659
```

## Change History

### v4 — picker scope addition (author; not a review round)

- **The time-window picker on Calls and Stats** — a mid-story scope addition, requested by the
  operator and approved by them: a `hour | date | month | custom` granularity control with matching
  **native** inputs (`type="datetime-local"`, `type="date"`, `type="month"`), emitting
  **RFC3339 with the local offset** so the window is absolute and local. **Front-end only** —
  `/api/requests` and `/api/stats` already parse `since`/`until`
  ([api.go:351-367](../../internal/api/api.go#L351-L367), [:506-515](../../internal/api/api.go#L506-L515))
  and `EventFilter` already carries them ([types.go:168-169](../../internal/store/types.go#L168-L169));
  the Calls tab simply never set them ([app.js:275-286](../../internal/web/app.js#L275-L286)). Recorded
  in §4 as its own change block, with the `timeWindow()` cases in §5 and three risk rows in §6
  (window drift, DST, story size). **This section has not been through a review round** — round 3
  reviews it.
- **Scope now: three defect classes + one feature.** GI-11 is no longer purely a correction. §6
  records that the picker is the one part that is not a fix, and therefore the part to cut if the
  story needs to shrink.

### v3 — round-2 review

- **F2.1 (BLOCKER, conductor override — mechanism decided)** — the reprice's `events` UPDATE and the
  owning sessions' re-derivation now happen in **one transaction, store-owned**: a new
  `internal/store` row was added to §4's change table (`Store.RepriceCosts`, mirroring `InsertEvents`'
  distinct-session `reconcileSessionTx` loop before `Commit`, [store.go:301-316]); `internal/cli/reprice.go`
  is now the **CLI shell only** (flags, `--dry-run`/`--yes`, reporting), as `merge`/`rekey` split theirs.
  Cites [GI-9 plan:744-752](../../docs/planning/GI-9-merge-jsonl-and-proxy-rows.md#L744-L752). §4.
- **F2.1 addendum (conductor override)** — §4's "calling `ReconcileSession` … in the reprice's own
  transaction" is **deleted**, not reworded. The plan now states that `ReconcileSession` opens its own
  `BeginTx` ([store.go:695-705]) and the pool is pinned to one connection ([store.go:76],
  `SetMaxOpenConns(1)`), so a nested `BeginTx` blocks with **no deadline — a hang, not an error** — so
  nobody reintroduces it. §4.
- **F2.2 (MAJOR, conductor override)** — the `CaptureComplete` assignment moves to **after** the body
  backfill at [merge.go:323-328], not [:241], with the req-from-A / resp-from-B case handled
  explicitly and the §4 rekey test case ("flag follows to false") authoritative. §4, §7.
- **F2.3 (MINOR, conductor-verified figures)** — §8's widened query result gains the fourth bucket
  `jsonl,proxy / cc=1 / 21 rows / 21 truncated`, reopening the total to 4,952; §2's "the jsonl-first
  half is not counted here" now quantifies it (21 rows, all truncated). §2, §8.
- **F2.4 (MINOR, conductor-verified figures)** — §5 acceptance #3's figure is corrected to **0 → 1,176**
  on the IST window (widened scope); "~2,800" is dropped as the all-time figure mislabelled as a day
  figure; §5 also notes the bound (the day's 2,426 proxy rows) and that it is not the all-time
  `237 → 2,850`. §5.
- **F2.5 (MINOR)** — §5 acceptance #4 replaced the cap-bound "`length = cap` and complete → zero rows"
  with the actually-invariant prefix predicate (`Content-Length > length(req_body)`), with the
  599-at-cap rows and the F1.8 cap-bound blindness stated. §5.
- **F2.6 (MINOR)** — §2's stale "acceptance #2" cross-reference corrected to "acceptance #3". §2.
- **F2.7 (MINOR, conductor override)** — §4 states reprice prices with the **effective, Loader-backed
  table** (`pricing.NewLoader(pricing.DefaultPath(), nil).Table()`) and **not** `pricing.Compute`
  (`ShippedTable().Compute`); `user` rows are now **included** (reconstructible against the effective
  table), and only `approximate:` / `unpriced` rows are skipped. §4, §6, §5 test bullet.
- **F2.8 (NIT)** — §4's stale-comment sweep adds `internal/cli/export.go:133`. §4.
- **F2.9 (NIT)** — §4's `cmd/clens/main_test.go` row now names **both** "19 subcommands" comments
  ([:10-14] and [:26]). §4.

### v2 — round-1 review

- **F1.1 (BLOCKER)** — reprice now re-derives the owning sessions' materialized totals via
  `ReconcileSession` in the same transaction, and the plan states the invariant-5 routing reprice
  must reproduce (`subscription` → `api_equivalent_cost_usd`, else `cost_usd`). §4 reprice row, §5
  acceptance #2, §5 reprice test bullet.
- **F1.2 (MAJOR)** — added `cmd/clens/main_test.go` (`carriedOver` 19 → 20) to the §4 table; named
  `TestMergePrecedenceTruncatedVsComplete` as a test to **invert** in §4/§5.
- **F1.3 (MAJOR)** — the merge `CaptureComplete` rule is written as an expression covering the
  both-sides-carry-bodies case; added that case + a no-bodies error case to the test list. §4, §7.
- **F1.4 (MINOR)** — §6 merge-precedence risk restated: the winner reads the *input* flags, the
  merged flag is downstream, a re-merge is guarded by `usageObserved`.
- **F1.5 (MAJOR)** — §4 docs table widened: `cli-and-tooling.md`, `INDEX.md`,
  `testing-and-quality.md`, `data-privacy-and-compliance.md`, `glossary.md`, `decisions/003`,
  `CLAUDE.md`, and the stale code comments (`consumer.go:25`, `rules.go:72`, `decode.go:90`).
- **F1.6 (MAJOR)** — §4 README row now names "### Re-pricing rows already captured" and states why
  `--rebuild` cannot reach RC-A's proxy-only rows.
- **F1.7 (MINOR)** — §2 claim narrowed to *cent granularity*, the three `%.2f` sites listed, the
  replay-gate magnitude dependency noted; the idempotence claim qualified with the `unpriced`
  carve-out. §4.
- **F1.8 (MINOR)** — the historical at-cap marker reclassification is recorded as a documented,
  accepted limitation. §4 storage-schema row, §6.
- **F1.9 (MINOR)** — §3 arithmetic corrected (2,802 client requests; 2,257 + 144 + 401 = 2,802).
- **F1.10 (MINOR)** — §5 acceptance split: the honest count rising runs against the copied store;
  "falls again" moved to a newly captured day (§5 integration).
- **F1.11 (MINOR)** — §2 RC-B total corrected to 4,931 (3,405 + 1,289 + 237); the `source='proxy'`
  scope limitation stated; §5/§8 queries widened to `instr(source_refs,'proxy') > 0`.
- **F1.12 (MINOR)** — the invalid `day = 2026-09-21` predicate replaced with the explicit half-open
  `started_at` range in §5 and §8.
- **F1.14 (NIT)** — §4 config row drops the non-resolving `config.go:319` reference and adds the
  missing `BodyCapBytes` field doc comment.
- **F1.15 (NIT)** — §4 cost-and-quota row says the doc links **one** test
  (`TestComputeBatchRoundsPerClass`), not two.
- **F1.13** — excluded by the conductor: treated as REJECTED (moot; the §2/§3/§8 figures were
  reproduced read-only against the live store and match). No plan change.
- **C-1 (conductor override)** — §5 and §8 now state the IST-day acceptance window explicitly
  (`[1789929000000000000, 1790015400000000000)`) and say plainly that `clens stats --by day` buckets
  UTC and does not reproduce the figures; F1.12's fix folded into the same edit.
