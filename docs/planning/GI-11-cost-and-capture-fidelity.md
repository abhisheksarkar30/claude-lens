# GI-11 — Cost is rounded to a cent per call, and a truncated capture is recorded as complete

<!-- version=15 status=converged -->

**Issue**: GI#11 (GitHub) · **Branch**: `GI-11-cost-and-capture-fidelity` · **Beads**: `.beads/GI-11/` · **Plan version**: 15 · **Status**: converged

## 1. The report

The DeepSeek platform usage page for **2026-09-21** reads **$3.25 / 1,994 requests / 183,854,144
tokens**. clens, on the same day, reads **$0.82 / 2,257 proxy calls / 209,153,629 tokens**.
Both figures are wrong in opposite directions, and they have three unrelated causes.

The investigation was read-only against the live install (`~/.clens/lens.db`, ~1.47 GB at
investigation time) and the real transcripts under `~/.claude/projects/**/*.jsonl`. Every number
below is measured, not inferred; the queries are reproduced in §8 so they can be re-run.

**Every figure in this plan is a *dated snapshot of a live store*.** The store is being written
while this plan is read — it grew from ~1.47 GB at investigation to ~1.73 GB at the
**2026-09-22T06:49:41Z** measurement (and keeps growing) — so no count here is eternal fact. The
**reproducible artifact is the query, not the number**: §8 reproduces every figure, and re-running
a query on a later day yields that day's number. **Every figure measured out of the store is in that
one class** — including §5's acceptance figures, which a `VACUUM INTO` snapshot makes *reproducible*
but not *permanent*: `jsonlogs` backfills past days and every merge rewrites rows in place, so even a
fixed past day drifts (this plan watched its own RC-A row count move 2,352 → 2,327). Acceptance is
therefore written as **relations on a single snapshot** (§5), never as quoted targets. The only figures
here that are *not* snapshots are DeepSeek's invoice figures, which are fixed for a fixed day, and the
arithmetic derived from the captured tokens themselves.

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

2,327 of 2,426 rows stored exactly `$0.000000` while carrying 196.68 M of the day's 201.09 M
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
`!respBuf.truncated` alone, and the merge undoes the fix. Measured across the rows the **union**
predicate `(source = 'proxy' OR instr(source_refs,'proxy') > 0)` reaches, cross-tabulated against
the client's own request `Content-Length` header (a **dated snapshot**, 2026-09-22T06:49:41Z):

| `source_refs` | `capture_complete` | rows | truly truncated |
|---|---|---|---|
| `proxy,jsonl` (proxy-first merged) | 1 | 3,790 | **2,844** |
| `jsonl,proxy` (jsonl-first merged) | 1 | 21 | 21 |
| `''` (unmerged) | 1 | 1,413 | 0 |
| `''` (unmerged) | 0 | 316 | 316 |

These four rows sum to the union total: 3,790 + 1,413 + 316 + 21 = **5,540**.

**The predicate is a UNION, and the naive form is wrong** — it *narrows* rather than widens.
`source_refs` is `TEXT NOT NULL DEFAULT ''` ([schema.sql:15](../../internal/store/schema.sql#L15))
and is assigned by exactly one site in the tree, `merge.go:166`, so **no unmerged row carries a
source in it**: its value is the empty string. Meanwhile `source` is the *first* writer's and a merge
never rewrites it ([merge.go:165](../../internal/store/merge.go#L165) `merged := *existing`,
[:244](../../internal/store/merge.go#L244)), and `source_refs` is unioned existing-first
([:166](../../internal/store/merge.go#L166)) — so a transcript-first merge stores `source='jsonl'`
with `source_refs='jsonl,proxy'` and is **invisible** to `WHERE source='proxy'`, just as an unmerged
proxy row is invisible to `instr(source_refs,'proxy') > 0`. Either predicate alone drops the other
half. Today the naive `instr(...)` form returns 3,811 rows against the 5,519 the bare
`source='proxy'` filter reaches and the **5,540** the union reaches — it silently drops the
**1,729** unmerged proxy rows (`source='proxy' AND source_refs=''`). So the reproduction query in
§8, acceptance #3 and acceptance #4 all use the union predicate.

**Every one of the 2,865 false-complete rows is a merged row** (2,844 proxy-first + 21
jsonl-first). The unmerged bucket never lies: 0 false-complete, 316 honest truncations.

The warning is written exactly once, at **insert** time, against the row's own pre-merge flag
([consumer.go:210-214](../../internal/consumer/consumer.go#L210-L214) → `UpsertWarnings`), and
`ruleStreamIncomplete` fires iff the row is a stream and `!ev.CaptureComplete`
([rules.go:182-187](../../internal/analyze/rules.go#L182-L187), pinned by
[rules_test.go:285-289](../../internal/analyze/rules_test.go#L285-L289)). The merge writes **only**
`source_mismatch` ([merge.go:139-149](../../internal/store/merge.go#L139-L149)); it never deletes or
re-derives a `stream_incomplete`. So the failure shape is **stale warnings sitting on laundered
rows**, not suppressed ones: the merge hides the *flag*
([merge.go:241](../../internal/store/merge.go#L241)) while the pre-merge *warning* stays attached. The
Sept-21 window makes it plain — of its 1,274 `stream_incomplete` warnings, **1,254 already sit on
merged rows** (1,233 `proxy,jsonl` + 21 `jsonl,proxy`) and only **20 genuinely escaped merging**;
all-time, **2,860 of 2,918** sit on merged `capture_complete=1` rows. RC-B therefore does not *add*
warnings — it makes the flag **agree with the warning already attached**, plus the forward-going
guarantee that a truncated proxy row keeps `capture_complete=0` through a merge.

### RC-C — the 256 KB cap is too small for this workload

`BodyCapBytes` defaults to 262,144 ([config.go:59](../../internal/config/config.go#L59)) and
bounds **both** bodies ([proxy.go:88](../../internal/proxy/proxy.go#L88) for the response,
[:126](../../internal/proxy/proxy.go#L126) for the request).

Measured against the client's `Content-Length` across the **6,088** proxy rows that carry one (a
**dated snapshot**, 2026-09-22T09:06Z — the 4,873 figure this section used earlier is a different
population, not this base):

| | |
|---|---|
| request bodies over 256 KB (262,144) | **3,554 (58.4% of 6,088)** |
| request bodies over 1 MB (1,048,576) | 117 |
| request bodies over 2 MB (2,097,152) | **0** |
| largest request body seen | **1,246,222 bytes** |
| mean request body | 351,394 bytes |
| responses stored at exactly the cap (262,144) | 768 (of 6,114 stored responses — 12.6%) |

So truncation is the norm, not the exception — and the dominant body is the **request**, not the
response the report named. A Claude Code request carries the system prompt, the full tool schemas
and the conversation history, and crosses 256 KB routinely; 58% of calls are affected against 12%
of responses.

**A 2 MB cap (2,097,152) captures every body this install has ever seen**, with headroom.

## 3. What is *not* a defect (explicitly out of scope)

The **token** gap is not attributable to a bug in clens and is not fixed here.

- The proxy captured 100% of traffic: on the IST day 2026-09-21 (a **dated snapshot**, 2026-09-22T09:06Z,
  `events` = 48,675) the day's proxy rows total **2,426**, breaking down as 2,257 `/v1/messages` calls,
  144 `/v1/messages/count_tokens` calls and 25 other rows (24 `/api/hello` + 1 `/`) — 2,257 + 144 + 25 =
  2,426 — and 2,257 is exactly the proxy's `/v1/messages` row count, so the day's client requests are
  100% captured.
- 297,172,779 is an all-source total (2026-09-22T09:06Z) that includes 407 `claude-sonnet-5`
  subscription rows from session `90300bd3` (12:21–13:28), which never touched DeepSeek at all. **The
  DeepSeek comparison is proxy-only: 209,153,629 vs 183,854,144.**
- The proxy includes 659 `/v1/messages` calls with no transcript line (32.02 M tokens)
  (2026-09-22T09:06Z). A transcript flushes on exit, so a session killed mid-flight loses it
  permanently while the proxy still captured the traffic; those rows are the only record and must not
  be dropped.
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
| [internal/store/merge.go](../../internal/store/merge.go) | `CaptureComplete` follows the body: the surviving row reports the flag of whichever side supplied the bodies it holds, not the `||` of both. **Assign it *after* the body backfill at [:323-328](../../internal/store/merge.go#L323-L328), not at its current site [:241](../../internal/store/merge.go#L241).** At `:241` the backfill has not run and `merged := *existing` ([:165](../../internal/store/merge.go#L165)) means the test reads `existing`'s *pre-merge* bodies: on the jsonl-first / rekey shape (`existing` is the JSONL taker with **no** bodies, `incoming` is the proxy row holding them) the "any body" test reads false and falls through to `existing.CaptureComplete` = the JSONL row's `true` — the very laundering the `||` did. The rule is undefined by the `||` when **both** sides carry bodies, and `ReqBody`/`RespBody` are backfilled *independently* ([:323-328](../../internal/store/merge.go#L323-L328)), so the request body's owner can differ from the response body's. After the backfill, derive the owner explicitly from whichever side actually supplied each retained body — request from one side, response from the other, both from one side, or neither. **When the two retained bodies have different owners, the surviving flag is the `&&` of the two owners' flags** — `capture_complete` is set only if **both** contributing sides were complete — **never the `||`**, because a single row-level `bool` cannot say *which* body was cut, so an exact answer is impossible and the rule must state which way it errs. It errs toward `false` deliberately: a false `cc=0` is a visible, honest over-report (an `incomplete` flag on a row that was whole), while a false `cc=1` is exactly the laundering this whole story exists to remove. When the **request** and **response** bodies share one owner, that one owner's flag is used; when the merged row holds **no** body, keep `existing.CaptureComplete`, which is what keeps the `--body-policy off` row (no bodies, flag `true`) honest. (An **errored** proxy row is **request-only**, not body-less: its `ErrorHandler` calls `submit(0, nil, nil, false, err)` and `submit` tees the request body it holds ([proxy.go:49-53](../../internal/proxy/proxy.go#L49-L53), [:215-224](../../internal/proxy/proxy.go#L215-L224)), so it is the single-owner case, not the no-body one — the two causes are distinct and must not be conflated.) The §4 rekey test case ("flag follows to false") must pass at the new location; if it cannot, the rule is wrong, not the test. Rewrite the comment at [:176-183](../../internal/store/merge.go#L176-L183), whose "the record that is whole is the safer one to quote" argument the `||` contradicts. |
| [internal/store/merge_test.go](../../internal/store/merge_test.go) | **Seven** cases: proxy-first with the proxy truncated (flag stays false), proxy-first with the proxy whole (flag stays true), rekey collision where the JSONL taker takes the proxy's bodies (flag follows to false), `--body-policy off` (no bodies, flag stays true), the **both-sides-carry-bodies** case the existing fixtures already produce (`fullEvent` sets both `ReqBody` and `RespBody`, [store_test.go:72-73](../../internal/store/store_test.go#L72-L73)) — pinning that the surviving flag follows the side the kept bodies came from — an **errored** proxy row ([proxy.go:49-53](../../internal/proxy/proxy.go#L49-L53) calls `submit(0, nil, nil, false, err)`, and `submit` tees the request body it holds, [proxy.go:215-224](../../internal/proxy/proxy.go#L215-L224), so the row is **request-only** under the default policy — a *different* shape from the fourth case's `--body-policy off` no-body row, not a restatement of it) pinning that a single-body row takes its one body's owner's flag, and a **seventh, mixed-owner** case — `existing` holds only one body and `incoming` the other (request body from one side, response from the other) — pinning the `&&`: the surviving flag is `false` if **either** owner was incomplete. The mixed-owner shape is **defensive, not reachable end-to-end**: the production paths do not obviously build it — a `request_id` has exactly one proxy row, and jsonl rows carry no body at all — so the `&&` is pinned by a **direct `mergeEvents` unit test on constructed inputs**, not by a shape the pipeline produces; the rule is conservative precisely so that *if* the shape is ever reachable the row errs to `false` (**never** the `||`). [merge.go:323-328](../../internal/store/merge.go#L323-L328) backfills the two bodies *independently*, which is what would allow the shape, but no current production path hands one body from each side to a single merge. No other case splits the two retained bodies across two owners (`fullEvent` sets both, [store_test.go:72-73](../../internal/store/store_test.go#L72-L73)), so **none of them exercises the `&&`** — the seventh is what pins it. **`TestMergePrecedenceTruncatedVsComplete` must be inverted, not deleted:** its fixture inserts a truncated `fullEvent` first, so under the new rule the surviving bodies are that side's and line [merge_test.go:297-298](../../internal/store/merge_test.go#L297-L298) becomes `CaptureComplete == false`; same reasoning §5 already applies to `TestComputePeakRoundsPerClass`. Re-grep the tree for other assertions on the merged flag — [merge_test.go:873-875](../../internal/store/merge_test.go#L873-L875) asserts the same `true` and survives only because its `existing` side carries bodies. **That fixture is now load-bearing for its flag assertion, and the plan must say so in a comment at [:873-875]:** its JSONL side is a `fullEvent` that sets **both** bodies ([store_test.go:72-73](../../internal/store/store_test.go#L72-L73)), but a *real* JSONL row has no bodies at all ([jsonlogs.go:436](../../internal/jsonlogs/jsonlogs.go#L436)). Under the new `&&` rule a production-shaped fixture would take the proxy row's bodies and the flag would follow to `false` — the inverse of what the test asserts. The test would stay green while pinning the flag via a body set the JSONL source can never supply. Give the JSONL side no bodies and invert the assertion (the same treatment `TestMergePrecedenceTruncatedVsComplete` gets), or state in the test comment that the bodies are artificial and the flag assertion is incidental. |
| [internal/config/config.go](../../internal/config/config.go) | `BodyCapBytes` default 262,144 → **2,097,152** ([:59](../../internal/config/config.go#L59)); add the **missing** doc comment on the `BodyCapBytes` field ([:36](../../internal/config/config.go#L36) — the field is currently undocumented) and update the flag help ([:222](../../internal/config/config.go#L222)). The earlier "doc comment at [:319]" reference is dropped: [:318-326](../../internal/config/config.go#L318-L326) is the `BodyPolicy: "truncated"` rejection paragraph, which names `BodyCapBytes` only as the bound both policies shared — nothing there documents the field. While there, correct that paragraph's "both bounded by `BodyCapBytes`" phrasing only if it reads as still true of the new default; it is a historical note about the removed `truncated` policy. |
| [internal/config/config_test.go](../../internal/config/config_test.go) | Pin the new default, and that an explicit `CLENS_BODY_CAP_BYTES` / `--body-cap-bytes` still overrides it. |
| [internal/cli/reprice.go](../../internal/cli/reprice.go) *(new)* | `clens reprice` — **the CLI shell only**: flags, `--dry-run`/`--yes`, and reporting, exactly as `purge` and `rekey` split their CLI from their store work (there is **no** `clens merge` — the store-side merge runs only inside `insertOrMerge` and the `rekey` CLI, [merge.go:134](../../internal/store/merge.go#L134); §7 reasons from the same two commands). The write loop lives in `internal/store` (new row below). **The pricing table — the effective, Loader-backed table carrying the CONFIGURED off-peak calendar:** `newPriceLoader(cfg).Table()`, i.e. `pricing.NewLoader(pricing.DefaultPath(), cfg.PeakOffPeakDates).Table()` — **never** the `nil` form. `newPriceLoader` is *"the one loader shape every pricer in this process wants, so two pricers cannot disagree about the configured off-peak dates"* ([ingest.go:119-123](../../internal/cli/ingest.go#L119-L123)); `serve` builds its pricer from it ([serve.go:103](../../internal/cli/serve.go#L103)) and `openStore` already hands the reprice CLI the full `*config.Config` ([format.go:198-208](../../internal/cli/format.go#L198-L208)). **This supersedes v3's F2.7, which wrote `nil`** (see §Change History, v8): F2.7 copied [models.go:34](../../internal/cli/models.go#L34) / [prices.go:50](../../internal/cli/prices.go#L50),[:94](../../internal/cli/prices.go#L94), but those are a read-only catalogue print and a price-editor printing its own result — neither prices a stored call, so the off-peak calendar is irrelevant *there*; reprice **does** price stored calls, so dropping `cfg.PeakOffPeakDates` is exactly the disagreement the helper's comment forbids. **`nil` is not "as configured":** [config_test.go:201-228](../../internal/config/config_test.go#L201-L228) pins `nil` = *unset → the shipped 33-date list* as a state distinct from `[]` (the `none` spelling, "never off-peak"), and [resolvedOffPeakDates](../../internal/pricing/pricing.go#L405-L416) maps `nil` → shipped but a non-nil empty list → nothing off-peak — so a `nil`-form reprice silently misprices every call on an install that has configured `peak_off_peak_dates`. **Do not use `pricing.Compute`** — it is `ShippedTable().Compute` ([pricing.go:76-82](../../internal/pricing/pricing.go#L76-L82)) and would silently ignore both a user override and the configured calendar, mispricing precisely the rows a user tuned by hand. **Routing (invariant 5):** a `subscription` row's new figure goes to `api_equivalent_cost_usd` and leaves `cost_usd` NULL; any other `billing_mode` goes to `cost_usd` — exactly the switch at [consumer.go:321-326](../../internal/consumer/consumer.go#L321-L326) and [jsonlogs.go:461-466](../../internal/jsonlogs/jsonlogs.go#L461-L466). **Scoping:** price every row whose `model_resolved` resolves in the effective table. A `user` row **is** included — its cost was produced by the same `Compute` with the same per-class rounding, so RC-A corrupted it identically, and against the effective table it is reconstructible (its rate lives in the override file, [pricing.go:185-189](../../internal/pricing/pricing.go#L185-L189)); "the user overrode it" is not a reason to skip it. **An `approximate:cache_ttl_unknown` row is *in scope*, not skipped:** the label is a stored, **indexed** column ([schema.sql:45](../../internal/store/schema.sql#L45) `cost_source TEXT NOT NULL DEFAULT ''`; [:70](../../internal/store/schema.sql#L70) `idx_events_cost_source`), so it is the reconstruction **key** — a row carrying it proves `usage.TTLUnknown` was `true` at insert, so reprice rebuilds the exact input (`TTLUnknown = true`), calls `Compute`, and gets back both the corrected amount **and the same label** ([pricing.go:138-140](../../internal/pricing/pricing.go#L138-L140)); RC-A corrupted those rows identically, so they are repriced like any other. The **only** skip is a row whose computed inputs cannot be reconstructed from stored columns — today that is `unpriced` alone (its `model_resolved` is absent from the table, [pricing.go:93-94](../../internal/pricing/pricing.go#L93-L94)); such rows keep their stored cost and `cost_source`, never nulled, so `reprice --yes` is not silently destructive on exactly the rows it cannot price. (Any *future* `approximate:<reason>` other than `cache_ttl_unknown` would join that skip list for the same reason — its input is not a stored column — which is why the filter is spelled by reason, not by the `approximate:` prefix.) Mirrors [purge.go](../../internal/cli/purge.go): `--dry-run` prints what `--yes` would change, and nothing happens without `--yes`. |
| [internal/store](../../internal/store) *(new method)* | `Store.RepriceCosts(...)` — the tx-taking write loop the CLI shell calls. **The seam — the store computes, and `internal/store` must NOT import `internal/pricing`.** This is a hard constraint, not a preference: the store is the write authority, and the pricing engine stays behind a seam exactly as `internal/proxy` depends only on `sink` and `config` (that package's doc names `pricing` as a forbidden import — the repo's standing example of a load-bearing boundary). **The constraint is pinned by a failing test, not left as prose:** a new `internal/store/importguard_test.go` (table row below) mirrors `internal/proxy/importguard_test.go:26-49` (`TestProxyImportsAreNarrow`, whose `forbiddenImports` includes `internal/pricing`) one-for-one — glob `*.go`, skip `_test.go`, forbid `/internal/pricing` — so the "just import `pricing` directly, it is the same call" simplification fails the build. The compiler cannot enforce this (an `import "…/internal/pricing"` in `internal/store` compiles fine), which is exactly why the repo's other load-bearing boundaries each carry a source-reading guard (`internal/api/importguard_test.go:25-30` records that the pattern is deliberately duplicated per package). The repo's **existing** precedent for exactly this shape — a package that must price but must not import `pricing` — is the local mirror interface `PriceComputer` at [analyzer.go:35-40](../../internal/consumer/analyzer.go#L35-L40) (`Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd *float64, costSource string)`, satisfied by both `pricing.Table` and `*pricing.Loader`). **Reuse it, do not reinvent it:** `internal/store` declares its own copy of that method set (it already imports `internal/parse`, [store.go:23](../../internal/store/store.go#L23)), the CLI passes the effective Loader-backed table into `RepriceCosts`, and the store calls `Compute` per row **inside its transaction** — importing nothing from `pricing`. (A precomputed-slice alternative — the CLI computes in its loop and passes old→new values plus the distinct session ids — is acceptable if the implementer prefers it, but **the mirror interface is the named choice here**, matching the existing precedent.) **One transaction, store-owned:** in a single `BeginTx`, `UPDATE events SET cost_usd / api_equivalent_cost_usd / cost_source …` for the rows in scope, then re-derive every distinct owning `session_id` whose cost columns moved via `reconcileSessionTx`, and `Commit` — mirroring `InsertEvents`' distinct-session `reconcileSessionTx` loop before `Commit` ([store.go:301-316](../../internal/store/store.go#L301-L316)). The two `sessions.total_cost_usd` / `total_api_equivalent_cost_usd` columns are *materialized* ([schema.sql:89-90](../../internal/store/schema.sql#L89-L90)) and re-derived, never incremented; `reconcileSessionTx`'s own routing is the `SUM(CASE WHEN billing_mode='api' THEN cost_usd END)` / `SUM(CASE WHEN billing_mode='subscription' THEN api_equivalent_cost_usd END)` pair at [store.go:731-732](../../internal/store/store.go#L731-L732). The merge already re-derives in one transaction (`applyMergeTx`, pinned by `TestMergeRederivesSessionTotals`), and reprice must too — otherwise `clens stats`, `clens sessions` and `/api/sessions` keep showing the pre-fix session totals while `events` is corrected. **Do NOT call `ReconcileSession` from inside the reprice transaction:** it opens its **own** `BeginTx` ([store.go:695-705](../../internal/store/store.go#L695-L705)) and the pool is pinned to one connection ([store.go:76](../../internal/store/store.go#L76), `SetMaxOpenConns(1)`), so a nested `BeginTx` blocks on the pool with **no deadline — a hang, not an error**. Use the tx-taking `reconcileSessionTx`; that it is unexported, and therefore unreachable from `internal/cli`, is exactly why the write loop must live in `internal/store` — the same reasoning recorded in [docs/planning/GI-9-merge-jsonl-and-proxy-rows.md:744-752](../../docs/planning/GI-9-merge-jsonl-and-proxy-rows.md#L744-L752). |
| [internal/store/importguard_test.go](../../internal/store/importguard_test.go) *(new)* | The **enforcement** behind the row above: `TestStoreDoesNotImportPricing` — mirror `internal/proxy/importguard_test.go:26-49` one-for-one (`filepath.Glob("*.go")`, skip `_test.go`, `parser.ParseFile(…, parser.ImportsOnly)`, forbidden `/internal/pricing`). The compiler will not catch an `import "…/internal/pricing"` in `internal/store`, so this source-reading guard is what makes the "must NOT import `internal/pricing`" constraint stick — the same mechanism that pins `internal/proxy`'s `sink`/`config` boundary and `internal/api`/`internal/web`'s write-seam containment. |
| [internal/store/reprice_test.go](../../internal/store/reprice_test.go) *(new)* | The **store-level** reprice cases (§5), exercised against `Store.RepriceCosts` (the tx-taking store method): a fixture holding a known-wrong row reprices to the exact value; a `user` row reprices against the effective table (its override rate applies) rather than being skipped; an `approximate:cache_ttl_unknown` row is repriced with its `cost_source` preserved (the label is the reconstruction key, §4) — not skipped; a row priced with a non-default `PeakOffPeakDates` (the `none` spelling) recomputes to a different value than the shipped calendar gives — the case a `nil`-form reprice passes and therefore never catches (§4, F6.1); a row whose `model_resolved` no longer resolves keeps its stored cost/`cost_source` and is counted skipped, never nulled; and the **owning session's** `total_cost_usd` / `total_api_equivalent_cost_usd` move with the row in the same transaction (the F1.1 rollup). The write loop lives here, so the same-transaction rollup is pinned at the store layer, not the CLI one. |
| [internal/cli/reprice_test.go](../../internal/cli/reprice_test.go) *(new)* | The **CLI shell's** own contract (§5), mirroring `internal/cli/rekey_test.go`'s `--dry-run` cases: `clens reprice --dry-run` reports what `--yes` would change and writes nothing — the in-scope rows' cost columns and their owning sessions' totals are unchanged. `--dry-run` is a flag on the shell, not an argument to `Store.RepriceCosts`, so this case cannot be pinned from the `internal/store` package; it is the one §5 reprice case that lives at the CLI level. |
| [cmd/clens/main.go](../../cmd/clens/main.go) | Register `"reprice": cli.Reprice` and `"reflag": cli.Reflag` in the command table ([:16-39](../../cmd/clens/main.go#L16-L39)). |
| [cmd/clens/main_test.go](../../cmd/clens/main_test.go) | The new dispatch entries break `TestEveryCarriedOverCommandIsDispatched`: `carriedOver` is a hand-written allowlist and the test fails hard on any count mismatch ([main_test.go:40-47](../../cmd/clens/main_test.go#L40-L47)). Add **both** `"reprice"` and `"reflag"` to the list — **19 → 21**, since reprice and reflag ship together — and update **every** doc comment that names the count: [:10-14](../../cmd/clens/main_test.go#L10-L14) and [:26](../../cmd/clens/main_test.go#L26) both read "the 19 subcommands" and both go to 21, so neither is left stale. **The `:10-14` comment names the count *and its own composition* — "doctor and serve, br-GI-1-15's six collectors, br-GI-1-17's ten readers and writers, and br-GI-9-04's rekey" (2 + 6 + 10 + 1 = 19) — so moving only the numeral leaves that sentence claiming *21* while its own enumeration still sums to *19*: a third drift site inside the very comment this row says must not go stale.** The composition sentence therefore **gains `br-GI-11-03`'s `reprice` and `br-GI-11-06`'s `reflag`** (the same two groups the `carriedOver` list gains), so the comment's count and its enumeration agree at 21. This is the count-drift trap F1.2a/F2.9 established — one comment moved, the other missed. |

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
| [internal/web/index.html](../../internal/web/index.html) | Add the shared picker to the Calls filter row as two **new** ids, `c-window-gran` (the `<select>`) and `c-window-value` (the native input it reveals) ([:54-63](../../internal/web/index.html#L54-L63)). On Stats ([:89-98](../../internal/web/index.html#L89-L98)) add `s-window-gran`/`s-window-value` **beside** the picker, and **keep** the free-text `s-since`/`s-until` mounted — hidden unless the granularity is `custom`. The Calls row has no free-text pair at all ([:54-63] is source/model/billing/Apply), so `custom` has nothing to reveal there — **the `<option>` set is therefore built per mount: the Calls `<select>` omits `custom`, and only the Stats `<select>` offers it** (see the decision below). It is **Stats** that keeps `s-since`/`s-until` behind `custom`. The ids must move with the code: `TestAssetsEveryLookupHasAMount` ([:35-56](../../internal/web/assets_test.go#L35-L56)) fails the build on a `$('…')` lookup with no mount, so removing `s-since`/`s-until` while `loadStats` still reads them is a **build failure**, and [:497-498](../../internal/web/app.js#L497-L498) is the only lookup site. |
| [internal/web/app.js](../../internal/web/app.js) | One `timeWindow()` helper (granularity + value → `{since, until}` RFC3339 strings); `callFilter()` ([:275-286](../../internal/web/app.js#L275-L286)) sets them; `loadStats()` ([:495-499](../../internal/web/app.js#L495-L499)) reads the picker; a change handler mirroring `f-apply` ([:928](../../internal/web/app.js#L928)). Populate each `<select>`'s `<option>` set per mount (Calls: `hour\|date\|month`; Stats: `+custom`) — the one shared implementation, with the Calls mount omitting the dead `custom` entry. |
| [internal/web/assets_test.go](../../internal/web/assets_test.go) | The picker's **source-shape** guards (F3.1/F3.2) — no JS runtime exists here, so there are **no executed `timeWindow()` cases**. Keep `TestAssetsEveryLookupHasAMount` ([:35-56](../../internal/web/assets_test.go#L35-L56)) covering every new picker `$('…')` id (it fails on a lookup with no mount, so a typo'd id is a build failure), and add a `timeWindow`-body guard that slices the function with `funcBody(js, "timeWindow")` ([:358-377](../../internal/web/assets_test.go#L358-L377)) and, **scoped to that slice**, asserts both that it builds the `±hh:mm` offset by hand and that it does **not** call `toISOString()`. `funcBody` requires a top-level `function timeWindow(` and terminates at the first column-0 `\n}\n`, so the test must `t.Fatal` on `!ok` (the vacuity trap its own doc comment warns about, [:359-366](../../internal/web/assets_test.go#L359-L366)) — a whole-file regex cannot express "inside `timeWindow`" and would pass a `timeWindow` that ignores its own computed offset (F7.5). Note the mount guard is one-directional: it accepts a mount in **either** `index.html` or `app.js`'s injected markup, and never fails on a mounted-but-unused id. |
| [internal/web/style.css](../../internal/web/style.css) | Only if the new controls need it; the existing `.row`/`label` classes already carry the filter rows. |

**The control — one implementation, two mounts.** A granularity `<select>` of
`hour | date | month | custom`, and the input it reveals:

| Granularity | Input | Window (half-open) |
|---|---|---|
| `hour` | `<input type="datetime-local">` | `[H:00, H+1:00)` |
| `date` | `<input type="date">` | `[D 00:00, D+1 00:00)` |
| `month` | `<input type="month">` | `[M-01 00:00, M+1-01 00:00)` |
| `custom` | the retained free-text `s-since`/`s-until` pair — **Stats mount only; omitted on Calls** | whatever the operator types |

**Decision: the `<option>` set is built per mount, and Calls does not offer `custom`.** "Reveals
nothing" is a description of the gap, not a resolution of it — a selectable entry that filters
nothing is a dead control, so the Calls `<select>` is populated with `hour | date | month` only and
the Stats `<select>` with all four. This is a decision, not an accident: only Stats has the
free-text pair `custom` would surface. **No dead option on either mount.** (The alternative —
disabling the option on Calls — was rejected because a greyed entry still advertises a capability the
tab does not have.)

**Two axes, not one — the picker is the window, `s-granularity` is the bucket.** Stats already
carries a second granularity control, `<select id="s-granularity">` of `day | week | month`
([index.html:92-96](../../internal/web/index.html#L92-L96)), sent as `?granularity=`
([app.js:499](../../internal/web/app.js#L499)) and parsed by `parseGranularityParam`
([api.go:329-338](../../internal/api/api.go#L329-L338)). It is the **bucket** axis: it is unchanged,
and `s-apply` ([:97](../../internal/web/index.html#L97), [app.js:937](../../internal/web/app.js#L937))
stays its trigger. The picker supplies only `since`/`until`; its own `<select>` must **never** be
wired to `granularity=` — `parseGranularityParam` accepts only `day|week|month`, so `hour`/`date`/
`custom` sent as a granularity would 400 on the spot. The two axes compose, and the composition has an
output-shape consequence worth stating: a bounded window (say one hour) bucketed by `day` collapses
the "By period" table and the chart to a **single** bucket — correct, but surprising, since the picker
narrows the *rows* while the bucket control decides how they are grouped.

**Native inputs, no library.** `type="date"`, `type="month"` and `type="datetime-local"` are platform
features. The dashboard is hand-written vanilla JS with no build step and no dependencies — the same
reasoning that made `chartByPeriod` an inline SVG rather than a charting library
([app.js:538-542](../../internal/web/app.js#L538-L542): "a library is a build step or a CDN, both of
which `embed.go`'s rationale rules out"). A calendar library is exactly that, so there is not one.

**Wire format: RFC3339 carrying the local offset — never a duration.** `parseTimeBoundParam` accepts
both ([api.go:293-305](../../internal/api/api.go#L293-L305)), but a duration is resolved against
`time.Now()` *at request time* ([:298-300](../../internal/api/api.go#L298-L300)), and `24h` cannot
express "the local day of 21 September". The picker therefore sends an absolute instant carrying its
offset, e.g. `since=2026-09-21T00:00:00+05:30`.

**This fix can rebuild the C-1 defect inside itself, and the likeliest implementation does.** The
"loud failure" reassurance holds for one failure mode only. A bare local time is rejected — but the
idiomatic JS way to build an ISO instant, `new Date('2026-09-21').toISOString()`, emits
`2026-09-21T00:00:00.000Z`, and Go **accepts** it, fractional seconds and all, reading it as **UTC**
([api.go:301](../../internal/api/api.go#L301) is `time.Parse(time.RFC3339, s)`; the `Z07:00` layout
matches a literal `Z` and the parser accepts a fractional-second field it does not itself print).
Measured: that Z form is unix **1789948800** against the intended **1789929000** —
**19,800 s = 5h30m off, silently**. So the failure that matters is the *silent* one, and
`toISOString()` is exactly how a naive `timeWindow()` produces it. The mechanism must be stated, not
assumed: build the `±hh:mm` suffix from the local offset (e.g. `-date.getTimezoneOffset()`) and
**never** call `toISOString()`. This is the single detail most likely to be got wrong, and — unlike
the bare form below — it does **not** fail loudly.

Verified directly against `time.Parse(time.RFC3339, …)`:

| Input | Result |
|---|---|
| `2026-09-21T00:00:00` | **rejected** — `cannot parse "" as "Z07:00"` |
| `2026-09-21` (raw `<input type="date">`) | **rejected** — `cannot parse "" as "T"` |
| `2026-09` (raw `<input type="month">`) | **rejected** — `cannot parse "" as "-"` |
| `2026-09-21T00:00:00+05:30` | accepted |
| `2026-09-21T00:00:00.000Z` (`toISOString()`) | **accepted** — read as UTC, 5h30m off the intended local instant |

**So the control's raw value is never a valid query param** — not even for `date` and `month`, whose
`<input>` values are bare `YYYY-MM-DD` / `YYYY-MM`. `timeWindow()` must expand the picked value into
a full instant **carrying the local offset** before it reaches the query string, and that offset must
be built by hand — `toISOString()` is the trap above, not a shortcut.

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

### The historical `capture_complete` backfill — `clens reflag`

**Why it is required, stated in the plan's own logic.** RC-B's fix lives in
[merge.go](../../internal/store/merge.go), which runs **only when a merge happens**. Against an
existing store it therefore rewrites **no existing row**: the laundering already happened, and the
merge that would apply the new rule has long since run. Both acceptance #3 (the `cc=0` count rising)
and acceptance #4 ("must be **0**") are consequently **unrunnable as written** — the same defect
class F1.10 caught in round 1, where the "falls again" half was moved to a newly captured day; that
fix covered only half the problem. The repair for history must be its own step.

**Re-merging cannot recover it — the information was destroyed.** The `||` overwrote the proxy row's
original `capture_complete=0` with `true`; no stored bit records which side was truncated, so no
re-merge can reconstruct it. The **only** surviving witness is independent of the flag: a stored
`req_body` that is a strict prefix of its client `Content-Length`. That is what justifies the
mechanism.

**Mechanism: a separate command with one job.** Following the repo's one-command-per-job shape
(`purge`, `rekey`, `reprice`), **`clens reflag`** rewrites exactly one stored fact: it sets
`capture_complete = 0` on every row whose stored body is provably a prefix of what the client sent.
Like `purge`/`rekey` it is gated **`--dry-run` / `--yes`** — it rewrites a stored fact, so it gets
the same gate — and it is **idempotent**: it only ever narrows `1 → 0`, so a second `--yes` run is a
no-op.

**Bead.** This is its **own work item** in `.beads/GI-11/`, beside the reprice bead (beads are cut
after the plan converges) — the story's outcome list gains `clens reflag` as a fourth named
subcommand change, with the file list the table below names.

- **The witness predicate** — a stored body that is a strict prefix of its client `Content-Length`,
  on **either** side, guarded so a body-less row can neither be falsely witnessed nor silently
  dropped. **This is the one predicate; acceptance #4 and §8's query both use it, so they cannot
  drift:**

  ```
  ( (length(req_body)  IS NOT NULL AND COALESCE(CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(req_body))
    OR
    (length(resp_body) IS NOT NULL AND COALESCE(CAST(json_extract(resp_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(resp_body)) )
  ```

  **The `length(...) IS NOT NULL` guard is load-bearing** — a body-less `capture_complete=1` row is
  a *real mode* here (`BodyPolicy: "full" | "off"`, [config.go:35](../../internal/config/config.go#L35)),
  and without the guard `length(req_body)` is NULL, so the comparison is NULL, `NOT witness` is
  NULL, and `SUM` silently drops the row (measured: 62 rows); swapping in `COALESCE(length(req_body),0)`
  instead falsely witnesses it (measured: +33 rows). The guard makes the witness **require a stored
  body**, which is what keeps every bucket summing to the scope total.
  Apply it to the **response** side too where a `Content-Length` exists (`resp_headers`) — a
  non-streamed response over the cap is a real case, and br-GI-7-08 makes the flag cover **both**
  bodies, so dropping it would be wrong in principle. **Measured: the response side adds 0 rows on
  this store** (`flipped` request-only = 2,865 = request-or-response), so a request-only form is
  equivalent *here* — a principled rule, not a speculative one.
- **Report three buckets — and define the residual once, here**, so §4's code row, §5's test row and
  §8's query cannot drift from it:
  - **flipped** = `capture_complete = 1` **and** a provable prefix witness → set to `0`.
  - **already honest** = `capture_complete = 0` already.
  - **residual** = rows with `capture_complete = 1` **and** an existing `stream_incomplete` warning
    **and** no provable prefix witness — the unrecoverable rows, **stated, not hidden**; it is the
    honest limit of the repair.

  *Why the warning is part of the definition:* a `stream_incomplete` warning is written at **insert**
  ([consumer.go:210-214](../../internal/consumer/consumer.go#L210-L214)) and `ruleStreamIncomplete` fires
  iff `IsStream && !CaptureComplete` ([rules.go:182-187](../../internal/analyze/rules.go#L182-L187)), so
  *warning + `cc=1`* is **precisely the laundered population** (the rule fired when the flag was `0`;
  the merge later flipped it to `1` while the warning stayed attached). The witness then splits that
  population into **repairable** (flipped) and **residual**. Without the warning the residual
  swallows every healthy `cc=1` row.

  Measured 2026-09-22T06:49:41Z (a dated snapshot): all-time **2,865 flipped / 316 already honest /
  138 residual** (2,221 healthy rows stay `cc=1`); the IST day 2026-09-21 **1,176 flipped / 128
  already honest / 78 residual** (1,065 stay `cc=1`). The four buckets reconcile to the union total:
  316 + 2,865 + 138 + 2,221 = **5,540**.
- **Warnings are NOT synthesised.** `capture_complete` is the stored *fact*; `stream_incomplete` is
  analyzer output whose inputs are not fully stored, and `ingest --rebuild` is already the repo's
  analysis-refresh path. So the backfill corrects the flag and leaves warnings alone. **The
  consequence, stated — and the 59 is a different bucket from the 138, never summed into it:** rows
  that *flip* to `cc=0` but carry **no** warning — all-time **59** (on the IST day **0**; every one of
  the day's 1,176 flips already carries a `stream_incomplete`, and on the IST day 1,254 of 1,274
  warnings already sit on merged `cc=1` rows, §2/F3.6). **The 59 is a sub-count of the `flipped`
  2,865; the 138 is the `residual`** — the two are disjoint and must never be added (that sum is the
  reconciliation error to avoid). The 59 counts flips that leave a `cc=0` row bearing no warning, i.e.
  a non-stream truncation (`ruleStreamIncomplete` requires `IsStream`,
  [rules.go:182-187](../../internal/analyze/rules.go#L182-L187)); the 138 counts rows the repair
  **cannot** reach (`cc=1` ∧ an existing `stream_incomplete` warning ∧ no witness, the definition
  above). The only sum that closes is the four buckets: `316 + 2,865 + 138 + 2,221 = 5,540`.
  Synthesising the missing
  warnings would require re-deriving analyzer output from inputs the store does not fully hold — the
  analyzer's inputs are *not* cleanly derivable from stored columns here — so `reflag` deliberately
  does not, and the flag-without-warning rows are accepted as the visible, honest over-report they
  are.
- **No session re-derivation is needed.** Unlike the cost columns, `capture_complete` is **not**
  folded into `sessions`, so `reflag` does **not** carry `reprice`'s `reconcileSessionTx` rollup.
  Stated explicitly so nobody assumes that transaction shape carries over.

| File | Change |
|---|---|
| [internal/cli/reflag.go](../../internal/cli/reflag.go) *(new)* | `clens reflag` — the CLI shell: `--dry-run`/`--yes` and reporting, exactly as `purge`/`rekey` split their CLI from their store work. Mirrors [purge.go:26-79](../../internal/cli/purge.go#L26-L79): `--dry-run` prints the three buckets, and nothing is written without `--yes`. |
| [internal/store](../../internal/store) *(new method)* | `Store.ReflagIncompleteCaptures(...)` — one `UPDATE events SET capture_complete = 0 WHERE capture_complete = 1 AND <witness>` (the predicate above) plus the three counts the report needs: **flipped** (`cc=1` ∧ witness), **already honest** (`cc=0`), and **residual** (`cc=1` ∧ an existing `stream_incomplete` warning ∧ no witness — the buckets bullet's definition, via a `warnings.kind='stream_incomplete'` join). **No `reconcileSessionTx` rollup:** `capture_complete` is not folded into `sessions` (the rollup reprice needs is a cost-column concern only). |
| [internal/store/reflag_test.go](../../internal/store/reflag_test.go) *(new)* | A merged row whose flag was laundered and whose `req_body` is a strict prefix of a `Content-Length` header → flipped to `0`; an honest `cc=0` row → untouched, counted already-honest; a laundered `cc=1` row **carrying a `stream_incomplete` warning** with no witness → counted residual and left alone; and a body-less `cc=1` row (`BodyPolicy: off`) with no `Content-Length` → **not** witnessed and, absent a warning, counted healthy, not residual (the `IS NOT NULL` guard, pinned so nobody drops it). Plus an idempotence assertion: a second run flips nothing. |

**Risk.** `reflag` is a second new write path into `events` (beside `reprice`) and the only one that
writes `capture_complete`; it is gated like `purge`/`rekey` for that reason. It touches no body, no header
and no cost, so it is read-only without `--yes` and cannot widen the credential or privacy surface.
Its one real limit is the **residual**: rows laundered by the `||` but carrying no `Content-Length`
evidence cannot be repaired, and the command reports them rather than guessing — the honest ceiling
of a repair from surviving columns. Its docs surface is the same `cli-and-tooling.md` /
`INDEX.md` / `main_test.go` count change as `reprice` (the one count now moves **19 → 21**, both
doc comments, per §4's rows).

### Docs

| File | Change |
|---|---|
| [docs/context/cost-and-quota.md](../../docs/context/cost-and-quota.md) | §"Exact money, rounded per token class" ([:72-81](../../docs/context/cost-and-quota.md#L72-L81)) documents the rounding as an invariant and links **one** test by name — `TestComputeBatchRoundsPerClass` at [:78](../../docs/context/cost-and-quota.md#L78) (the plan's "links the two tests" overstates it; the second test lives only in `docs/planning/GI-3-deepseek-peak-pricing.md`); rewrite it to the round-at-display rule. Update the `Compute` description at [:97-100](../../docs/context/cost-and-quota.md#L97-L100). |
| [docs/context/dashboard.md](../../docs/context/dashboard.md) | `INDEX.md:40` routes "before changing anything in `internal/web`" here, and the picker edits four files in that package. Add the picker to the Calls and Stats filter rows in the per-tab route table ([:87-96](../../docs/context/dashboard.md#L87-L96) — "the call log with filters", "totals over a window, charted by day/week/month"); the Stats window control and its composition with the bucket axis (`s-granularity`); the new `TestAssets*`/`timeWindow()` source guards in the test inventory ([:176-183](../../docs/context/dashboard.md#L176-L183)); and re-measure the `index.html`/`app.js` line counts, which are repeated at **three** sites — [:6](../../docs/context/dashboard.md#L6) ("1030 lines of hand-written JavaScript"), [:21-25](../../docs/context/dashboard.md#L21-L25) (the 148 / 1030 table) **and** [INDEX.md:40](../../docs/context/INDEX.md#L40) ("1030 lines") — so all move together (the same multi-site count-drift trap F1.2a/F2.9 established for the subcommand count; `:6` is the prose sentence, a different sentence in the same file from the `:21-25` table, so moving one and not the other leaves a contradiction inside one file). |
| [docs/context/storage-schema.md](../../docs/context/storage-schema.md) | The `capture_complete` paragraph ([:95-103](../../docs/context/storage-schema.md#L95-L103)) is **stale against the code**: it claims the flag also covers "ended without a `message_stop` event", but [proxy.go:107](../../internal/proxy/proxy.go#L107) computes it from the two buffers' `truncated` bits alone. Fix that, and state the merge rule. **Also record the cap-raise consequence (F1.8):** the length-vs-cap truncation *marker* compares a stored body against the **current** cap, so raising the default to 2 MB silently reclassifies every historical at-cap row (262,144 bytes) as `Complete` — on exactly the rows RC-B has just started flagging honestly. Document that this is a known, accepted limitation of the marker (a per-row recorded cap would be required to mark old rows, and the plan does not add one) and that the authoritative signal is the `capture_complete` flag, not the marker. |
| [docs/context/build-and-run.md](../../docs/context/build-and-run.md) | The body-cap default at [:85](../../docs/context/build-and-run.md#L85) (`262144 (256 KB)`) and its storage trade-off. |
| [docs/context/cli-and-tooling.md](../../docs/context/cli-and-tooling.md) | Adding subcommands — the module [INDEX.md:33](../../docs/context/INDEX.md#L33) routes to for exactly this change. "of 19 subcommands" at [:6](../../docs/context/cli-and-tooling.md#L6) → **21**; add `reprice` **and** `reflag` to the Commands table ([:15-35](../../docs/context/cli-and-tooling.md#L15-L35)); and narrow `ingest --rebuild`'s "**the re-pricing path**" at [:19](../../docs/context/cli-and-tooling.md#L19) — the same claim lives at [workflows.md:140](../../docs/context/workflows.md#L140) ("`clens ingest --rebuild` the re-pricing path"), so both sites move together or both stay — now that a dedicated reprice command exists. **The destructive-command claim is carried at three sites the plan now names, and the edit must not inflate the count:** "**The two destructive commands**" ([:37-49](../../docs/context/cli-and-tooling.md#L37-L49), which says purge/rekey **delete rows** and calls it "the pattern any future destructive subcommand should follow"), [internal/cli/purge.go:19-25](../../internal/cli/purge.go#L19-L25) ("This is one of **two** commands in the CLI that destroy data — `clens rekey` … is the other"), and [internal/cli/rekey.go:17](../../internal/cli/rekey.go#L17) ("the story's **second** destructive command"). **The correct post-GI-11 fact: after `reprice` and `reflag` there are four `--yes`-gated *writers* but still exactly two *destructive* commands** — `purge` and `rekey` delete rows, while `reprice` and `reflag` only rewrite columns (`cost_usd`/`api_equivalent_cost_usd`/`cost_source`; `capture_complete`) and delete nothing. A literal "two → four destructive commands" edit would be **wrong**; the four writers share the `--yes` gate, not the property of destroying data. |
| [docs/context/INDEX.md](../../docs/context/INDEX.md) | Three figures move: [:33](../../docs/context/INDEX.md#L33) "the 19-entry dispatch table" → **21**; [:37](../../docs/context/INDEX.md#L37) "52 test files" (the `testing-and-quality.md` trigger) with the **four** new test files (**52 → 56**); [:40](../../docs/context/INDEX.md#L40) "1030 lines" (the `dashboard.md` trigger) with the picker's edits. |
| [docs/context/testing-and-quality.md](../../docs/context/testing-and-quality.md) | the hand-maintained test-file count at [:12](../../docs/context/testing-and-quality.md#L12) ("**52 test files**") moves by **four** new files — `internal/store/reprice_test.go`, `internal/cli/reprice_test.go`, `internal/store/reflag_test.go`, and `internal/store/importguard_test.go` (§4's store-boundary guard) — **52 → 56**, each named with its package so the count is checkable, not just "the reprice file + its test"; re-measure and update. The same "52 test files" figure is repeated at [INDEX.md:37](../../docs/context/INDEX.md#L37), so both sites move together (the F1.2a/F2.9 count-drift trap). |
| [docs/context/data-privacy-and-compliance.md](../../docs/context/data-privacy-and-compliance.md) | the 256 KB default as fact at [:37](../../docs/context/data-privacy-and-compliance.md#L37) and [:61](../../docs/context/data-privacy-and-compliance.md#L61); this is the module INDEX routes "before changing what is captured" to, and §7 requires the larger-cap privacy cost be stated here rather than discovered. Note [:66-68](../../docs/context/data-privacy-and-compliance.md#L66-L68) already records that the marker inference is "only as good as the cap not having changed" — it is the doc-side half of F1.8. |
| [docs/context/glossary.md](../../docs/context/glossary.md) | "**body cap**" entry ([:51](../../docs/context/glossary.md#L51)) states `default 262144` → 2,097,152. |
| [docs/context/decisions/003-full-bodies-stored.md](../../docs/context/decisions/003-full-bodies-stored.md) | "bounded by a 256 KB cap" at [:15](../../docs/context/decisions/003-full-bodies-stored.md#L15) and the `--body-cap-bytes` row at [:20](../../docs/context/decisions/003-full-bodies-stored.md#L20) — note the new default; the decision itself (full bodies under a bounded cap) is unchanged. |
| [CLAUDE.md](../../CLAUDE.md) | [:128](../../CLAUDE.md#L128) states "policy + 256 KB cap" and is the *enforced-conventions* file; update the number so it states a true bound. |
| [README.md](../../README.md) | Any 256 KB / `body-cap` mention, and the `clens` command list for `reprice` **and `reflag`** (the table at [:88-105](../../README.md#L88-L105)). **The section "### Re-pricing rows already captured" ([:111-120](../../README.md#L111-L120)) must be rewritten, not just added to:** it currently states that there is *no separate reprice command* and that `clens ingest --rebuild` is the re-pricing path. Replace it with what `reprice` does and *why `--rebuild` cannot do the job for RC-A's rows*: a re-ingest is a **JSONL** row, priced with `speed=""`/`serviceTier=""` ([jsonlogs.go:459](../../internal/jsonlogs/jsonlogs.go#L459)) and absorbed by the `request_id` merge, which never replaces the proxy's bodies ([merge.go:323-328](../../internal/store/merge.go#L323-L328)) — so it cannot reach the proxy-only rows that carry the zeroed costs. Note alongside it that `reflag` is the matching historical repair for RC-B's flag (a re-ingest cannot recover that either: the `||` destroyed the original bit). This is also the one place the repo records the *design intent* for having no reprice command, so §7's rejects-alternatives paragraph reasons from `purge`/`rekey` and must address this section too. **Beside the command-table edit, fix [:107-109](../../README.md#L107-L109):** the sentence immediately below the table reads "`clens purge` is **the one command** that destroys data" — **already false today**, since `clens rekey` deletes rows ([internal/cli/rekey.go:17-25](../../internal/cli/rekey.go#L17-L25), [cli-and-tooling.md:37-39](../../docs/context/cli-and-tooling.md#L37-L39)), and the same edit adds `reprice`/`reflag` to the table. Correct it to the post-GI-11 fact: **four `--yes`-gated writers, two of them destructive** (`purge`, `rekey` delete rows; `reprice`/`reflag` rewrite columns only). |
| stale code comments (grep sweep) | `internal/consumer/consumer.go:25` — `defaultBodyCapBytes = 262144`, the consumer's own independent fallback, which silently disagrees with the new default whenever the `SetBodyPolicy`/cap seam is left unwired; `internal/analyze/rules.go:72` and `internal/decode/decode.go:90` — both state "256 KB" in prose; `internal/cli/export.go:133` — "reading the blobs here would be 256 KB per row", stale the moment the default is 2 MB. **Two comments the sweep must read (and leave correct, not rewrite):** [internal/cli/purge.go:19-25](../../internal/cli/purge.go#L19-L25) ("one of **two** commands in the CLI that destroy data — `clens rekey` … is the other") and [internal/cli/rekey.go:17](../../internal/cli/rekey.go#L17) ("the story's **second** destructive command") are **both still true after GI-11** — `reprice` and `reflag` delete no row, so the destructive count stays two. Do not edit them to "four"; the four-writers/two-destructive distinction (cli-and-tooling.md row) is what the docs carry, not these comments. |

### Not code: the database moves to `D:` — run as part of this story

`DBPath` is already configurable (`CLENS_DB_PATH`, `--db-path`,
[internal/config/config.go:34](../../internal/config/config.go#L34)), so this is **configuration,
not a change**: set `DBPath = "D:/clens/lens.db"` in `~/.clens/config.toml` and move the file. It
belongs in this story's rollout because RC-C grows the store. The projection is what *this change*
adds, not what the store weighs today (the store is a live snapshot that drifts — §1): the cap's own
contribution is **+~0.35 GB** (not the earlier ~0.45 GB), and that growth is much better spent on `D:` than on `C:`.

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
- Merge: the seven `CaptureComplete` cases above, plus an assertion that the merged row's bodies and
  its flag agree — the invariant the `||` broke. `TestMergePrecedenceTruncatedVsComplete`
  **inverts** (`CaptureComplete` becomes `false`; see §4) rather than being deleted, on the same
  reasoning this section applies to `TestComputePeakRoundsPerClass`. `TestBillingModeInvariants`
  and the `source_mismatch` tests must stay green, since RC-B touches the same function.
- Config: new default + override still wins.
- Reprice: a store fixture with a known-wrong row reprices to the exact value; `--dry-run` changes
  nothing; a `user` row reprices against the effective table (its override rate applies) rather than
  being skipped; an **`approximate:cache_ttl_unknown` row is *repriced*, its `cost_source` preserved**
  (the label is the reconstruction key, §4) — **not** skipped; a row priced with a **non-default
  `PeakOffPeakDates`** (e.g. the `none` spelling) recomputes to a **different** value than the shipped
  calendar would give — the case a `nil`-form reprice passes and therefore never catches (§4, F6.1); a
  row whose model no longer resolves keeps its stored cost/source and is counted as skipped; and the
  **owning session's** `total_cost_usd` moves with the row in the same transaction (the F1.1 rollup).
  The write loop is exercised against `Store.RepriceCosts` (the tx-taking store method) in
  `internal/store/reprice_test.go`, so the same-transaction rollup is a store test, not a CLI one;
  the `--dry-run` "changes nothing" case is the CLI shell's own, in `internal/cli/reprice_test.go`
  (mirroring `internal/cli/rekey_test.go`'s `--dry-run` cases) — `--dry-run` is a flag on the shell,
  not an argument to `Store.RepriceCosts`, so it cannot be pinned from the store package.
- Store import boundary: `TestStoreDoesNotImportPricing` (`internal/store/importguard_test.go`, §4) —
  the mirror of `internal/proxy/importguard_test.go:26-49` — fails the build if `internal/store` ever
  imports `internal/pricing`. This is what pins the reprice seam: the store prices through the injected
  mirror interface (`PriceComputer`), never by importing `pricing` or re-implementing the billing
  switch by hand (F6.7/F7.2).
- Reflag (the historical backfill, §4): a laundered merged row whose `req_body` is a strict prefix of
  its `Content-Length` flips to `cc=0`; an already-honest `cc=0` row is untouched and counted
  already-honest; a laundered `cc=1` row **carrying a `stream_incomplete` warning** with no witness is
  counted **residual** and left alone (the residual is `cc=1` ∧ warning ∧ no witness — the one
  definition §4 states; without the warning it would be healthy, not residual); and a second run flips
  nothing (idempotence). Exercised against `Store.ReflagIncompleteCaptures`, which does no session
  rollup because `capture_complete` is not folded into `sessions`.
- Picker (§4) — **what the Go harness can actually pin, and nothing more.** There is no JS runtime in
  this toolchain and no JS engine in the module graph
  ([assets_test.go:409-416](../../internal/web/assets_test.go#L409-L416); `go.mod`), so `timeWindow()`
  cannot be *executed* here — a Go test only asserts over the embedded bytes. The Go tests therefore
  pin exactly **two** source-shape facts, and the behavioural cases move to **manual verification in
  the PR's test plan** rather than being implied as automated:
  - **(a) the picker's ids mount.** `TestAssetsEveryLookupHasAMount`
    ([assets_test.go:35-56](../../internal/web/assets_test.go#L35-L56)) fails the build if any `$('…')`
    the picker adds has no matching `id=`, which is what turns a typo'd id into a failure instead of a
    silent no-op.
  - **(b) the offset is built by hand, never `toISOString()`.** **Scoped to `timeWindow`'s body, not
    whole-file** — the technique is `funcBody(js, "timeWindow")` ([assets_test.go:358-377](../../internal/web/assets_test.go#L358-L377)), **not** `TestAssetsEveryLookupHasAMount`'s whole-file regex
    ([:25](../../internal/web/assets_test.go#L25), applied to all of `js` at [:35-56](../../internal/web/assets_test.go#L35-L56)), which cannot express "inside `timeWindow`". `funcBody` requires a literal top-level
    `function timeWindow(` declaration and slices to the first column-0 `\n}\n`, so this is a real
    constraint on the implementation: `timeWindow` must be a top-level `function timeWindow(...)`, not
    a `const timeWindow = (…) => {…}` (which would slice `("", false)`). The test must `t.Fatal` when
    `funcBody` returns `!ok` (the "passes vacuously" trap `funcBody`'s own doc comment warns about,
    [assets_test.go:359-366](../../internal/web/assets_test.go#L359-L366)) — the same `!ok → t.Fatal` shape `TestAssetsTheBodyRendererEscapes` uses
    ([assets_test.go:426-428](../../internal/web/assets_test.go#L426-L428)) — so a renamed or restyled anchor fails loudly rather than asserting over an
    empty slice. **Both halves of the check are scoped to that body:** `timeWindow` must build the
    `±hh:mm` suffix from the local offset (e.g. `-date.getTimezoneOffset()`) and must **not** call
    `toISOString()` — a whole-file `!strings.Contains(js, "toISOString")` would be a tripwire for any
    future legitimate use elsewhere, and a whole-file positive `contains` would pass for a `timeWindow`
    that ignores its own computed offset. The *why* goes in the test
    comment so nobody "simplifies" it back —
    `new Date('2026-09-21').toISOString()` emits `2026-09-21T00:00:00.000Z`, Go accepts it as UTC, and
    that is unix 1789948800 against the intended 1789929000, **19,800 s = 5h30m off, silently** (F3.2).
  - **Manual verification (the PR's test plan), not in-repo tests.** A text/regex test cannot assert an
    *emitted runtime string*, so the **semantic** case belongs here with the rest: `timeWindow()`'s
    emitted `since`/`until` must denote the intended **local** instant — it must carry the local
    offset, not end in `Z`, and denote the picked wall-clock in the browser's zone — checked by eye
    against the emitted query string. "It round-trips through `parseTimeBoundParam` without a 400" is
    **not** the check: the `Z` form parses cleanly and *is* the defect, so a parse-only test is green
    on the bug. Also: Dec→Jan month rollover; the month width **not** being a constant (28–31 days, so
    a `+30d` shortcut is wrong); and **DST**, where a local `date` window is built with
    `new Date(y, m, d)` → `new Date(y, m, d + 1)`, never `start + 86_400_000`, because a DST day is 23
    or 25 hours long. IST has no DST, but the operator's zone is not something this code gets to
    assume. §5 says so plainly rather than promising executed JS tests. (The ceiling is
    [assets_test.go:409-416](../../internal/web/assets_test.go#L409-L416) — this file is regex and
    text over the embedded bytes.)

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

**Acceptance — against a consistent *snapshot* of the live store, per this repo's convention** (GI-7
and GI-9 both ran against copies). **The copy method is specified, because on a live WAL database a
plain file copy is wrong** — it omits committed transactions still sitting in the `-wal` and can
capture main-file pages inconsistent with that WAL. The method, needing **no service interruption**:

- **`VACUUM INTO 'acceptance.db'`** from the live file — one command, consistent and fully
  checkpointed at the instant it runs, no need to stop `clens`, and it compacts (useful at ~1.7 GB).
  SQLite floor: 3.27+ (2019).
- **The alternative, and why it is *not* the default:** stop `clens` **first**, then copy **all
  three** files — `lens.db`, `lens.db-wal`, `lens.db-shm` — together. Copying the main file alone is
  the trap, and stopping the service is the cost that makes it second choice.

The acceptance figures below are measured against a **consistent snapshot**, which is what makes them
reproducible at all. But **the snapshot does not make them hold exactly, and an earlier revision of
this section claimed it did.** That claim was wrong, and this plan's own history disproves it: the
RC-A row count over this same *fixed past day* moved from 2,352 to 2,327 during the plan's life. A
past day is not frozen. `jsonlogs` backfills rows for past days as it ingests transcripts, and every
cross-source merge rewrites a row in place — `source_refs`, bodies, and (until RC-B lands) the
completeness flag. So the day's row population is a moving target too.

**Every acceptance criterion below is therefore written as a relation between two measurements taken
on the *same* snapshot**, and is checkable without knowing today's numbers: the baseline is whatever
the copy reports **immediately before** the step runs. Where a figure appears in parentheses it is the
**dated illustration** — the magnitude and the sign to expect, §8's class of number, never a target.
This is §8's own rule ("the reproducible artifact is the query, not the number") applied to acceptance
as well, and it is the only form that survives a store with a live writer:

**The acceptance window is the local IST day 2026-09-21** — the day DeepSeek invoiced (§1), and the
window the dated illustrations below are drawn over. It bounds **no** criterion: #1 is a per-row
relation over every row the reprice priced, and #2 is a per-session relation over that session's own
all-time `SUM`, so both hold regardless of window; **#3 and #4 run over the command's own unfiltered
scope** (see #3). The window is a half-open
`started_at` range in unix nanoseconds: `[1789929000000000000, 1790015400000000000)` (start inclusive,
end exclusive; the two values differ by exactly 86,400 s). Two things this must state plainly: `events`
has **no `day` column** ([schema.sql:11-66](../../internal/store/schema.sql#L11-L66)); `day` is a
derived expression, and `clens stats --by day` buckets by **UTC**
([store.go:1252](../../internal/store/store.go#L1252) — `DATE(started_at/1e9,'unixepoch')`), so `clens
stats --by day` does **not** reproduce these figures. The acceptance query is a **direct `events`
query, not a shipped subcommand**.

1. **Reprice correctness, as a relation — recompute, don't diff.** For **every row the reprice priced**,
   the stored figure equals the exact arithmetic recomputation over **that row's own stored token
   columns** at the table's rates — with one reconstruction: where the row's `cost_source` is
   `approximate:cache_ttl_unknown`, `usage.TTLUnknown` is *not* a stored column, so the recompute
   rebuilds it from the label exactly as the reprice does (§4) before computing — the acceptance reuses
   the reprice's own input-reconstruction rule. *(On the 2026-09-22T09:06Z store this is vacuous —
   `cost_source` holds only `shipped` (48,477) and `unpriced` (198), zero `approximate:*` rows — but it
   is stated because a recompute from the stored columns alone would read a correct reprice as a miss the
   moment an `approximate:cache_ttl_unknown` row exists; that is a correctness gap, not a
   simplification.)* Check it by recomputing the exact value for every row the reprice priced on the
   snapshot and comparing — **not** by asking which rows the run "wrote", which is unanswerable
   after the fact: the run only writes rows whose value *moved*, and an untouched row satisfies the
   relation trivially. **`unpriced` rows are out of scope** for the recompute relation — their inputs
   cannot be reconstructed, so the clause cannot be evaluated for them; they are checked only for having
   been **left untouched** (stored cost and `cost_source` unchanged), which is the run's stated carve-out
   (§"Why a reprice command is in scope"). This is drift-proof — a row added later was priced at insert
   and was never in the run's scope, so it cannot move the comparison — and it is the actual claim RC-A's
   fix makes, stated so it can be checked without quoting a total.
   *Dated illustration (2026-09-22 snapshot): the IST day's `SUM(cost_usd)` over `source='proxy'`
   moved 0.82 → 3.46.*
   *Invoice corroboration, reported not gated:* that day's total lands within ~7% of DeepSeek's 3.25
   (§1's external fact, fixed for a fixed day). It is a magnitude sanity check on a live number, so it
   is **reported as agreement**, not asserted as a bound that a late-arriving row could break.
2. **Session rollup (same transaction, F1.1):** for every session the reprice touched, its
   `sessions.total_cost_usd` (and `total_api_equivalent_cost_usd`) is re-derived in the reprice's
   own transaction and moves with the session's own all-time `SUM(events.cost_usd)` — assert one
   affected session's stored total equals the post-reprice `SUM` over its `events`, not the pre-fix
   value.
3. **The honest-truncation count, as a relation — with its baseline taken from the command itself.**
   Under the corrected **union** predicate `(source = 'proxy' OR instr(source_refs,'proxy') > 0)` —
   stated here, not implied, so the step is reproducible — the baseline is **read from the command's
   own `--dry-run`**, not from a hand-written query: `clens reflag --dry-run` prints exactly the three
   buckets (`flipped` = `W`, `already honest` = `baseline_cc0`, `residual` = `R`) and writes nothing
   (§4, br-GI-11-06). So the **flip relation's** baseline is measured by **the same code that will do the
   repair, at the instant before it runs** — which is the only way that number cannot be stale, and the
   reason that criterion needs no literal at all. Run `--dry-run` on the frozen copy to read the
   baseline, then `--yes` and compare. The criteria are two relations, both readable off one snapshot:
   - **the flip is exact:** `cc0_after = baseline_cc0 + W` — the count rises by precisely the rows the
     witness identifies, no more and no less;
   - **the buckets partition the scope:** `baseline_cc0 + W + R + H = scope_total`, where `R` is the
     residual (`cc=1` ∧ a stored `stream_incomplete` warning ∧ no witness) and `H` the healthy
     remainder — so the report's buckets are a partition of one snapshot, not four loose numbers that
     happen to be close. **This relation is evaluated with §8's four-bucket query** — the same witness
     predicate, so the two cannot drift — because the command's `--dry-run` prints only **three**
     buckets (`flipped`/`already honest`/`residual`) and carries neither `H` nor `scope_total`;
     `scope_total` is that query's row count over the same union scope.
   **The scope is the command's own** — the union predicate with **no time filter**, because `reflag`
   repairs all of history and an acceptance windowed narrower than the repair would be checking a
   subset of what it did. The IST day is a subset of the scope, not the criterion.
   *Dated illustration (2026-09-22T06:49:41Z snapshot, all-time scope): already honest 316, `W` 2,865,
   `R` 138, healthy 2,221 — the partition closing at **5,540**. **Re-measure — none of these is a
   target.***
   `baseline_cc0` is *not* 0, and it is **two** populations: unmerged proxy rows that carry a
   `stream_incomplete` warning (the stream subset §2 counts) **plus** non-stream truncations that carry
   none (`ruleStreamIncomplete` requires `IsStream`,
   [rules.go:182-187](../../internal/analyze/rules.go#L182-L187)) — so the stream subset is not the
   whole cause. (Within the IST day those two are 20 + 108 of the day's 128.) `R` is the honest
   ceiling: rows the `||` laundered that carry no `Content-Length` evidence, **reported rather than
   guessed** (§4's backfill block). The rise is produced by the **backfill**, not by the code fix
   alone — the code fix only prevents *new* laundering (see #4).
   The "falls again once the cap is 2 MB" half is **not** runnable against the copied store — a past
   day cannot be re-captured, and §6 says so — so it moves to a check on a **newly captured day** after
   the cap change, in §"Test strategy"'s integration section.
4. **The RC-B invariant, as a query — and *why* it is 0.** Assert that **no in-scope row** has
   `capture_complete = 1` while its stored body is a strict *prefix* of its `Content-Length`:
   `SELECT COUNT(*) FROM events WHERE (source = 'proxy' OR instr(source_refs,'proxy') > 0) AND
   capture_complete = 1 AND ( (length(req_body) IS NOT NULL AND
   COALESCE(CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(req_body))
   OR (length(resp_body) IS NOT NULL AND
   COALESCE(CAST(json_extract(resp_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(resp_body)) )`
   — must be **0** **after** `clens reflag --yes`. (This is the **same witness predicate §4 defines
   and §8 uses**, so the invariant and the repair cannot drift; on this store the response side adds
   0 rows, so a request-only form is equivalent — stated in full nonetheless, per §4.) It is 0 **because the backfill ran**, not because the code fix
   alone achieves it: the `merge.go` fix prevents *new* laundering, while `reflag` establishes the
   invariant for *history*. The two must never be conflated — run before `reflag`, this same query
   returns the false-complete rows §2 counts; the code fix does not change that number. The predicate
   is the prefix relation, **not** `length(req_body) = cap`. A body captured whole *at exactly* `cap`
   bytes stores `length == cap` with `capture_complete = 1` legitimately — the proxy sets `truncated`
   only when a write must drop bytes
   ([proxy.go:272-286](../../internal/proxy/proxy.go#L272-L286)), and §2 reports 768 such responses —
   so a `length = cap` test is not an invariant the code holds. It is also cap-bound: vacuous against
   the new 2 MB default (historical prefixes are 262,144 bytes) and, run against 262,144, it
   re-introduces the length-vs-cap blindness §4/§6 record as an accepted limitation (F1.8). The
   prefix test needs no cap at all.

## 6. Risks and edge cases

| Risk | Handling |
|---|---|
| **RC-B changes the dashboard's *flags*, not its warning count** — 2,844 proxy-first + 21 jsonl-first = **2,865** rows stop claiming completeness under the union predicate (a dated snapshot, 2026-09-22T06:49:41Z; the count changed when the scope widened from `source='proxy'` to the union — F4.4). The `stream_incomplete` warnings those rows carry are **already stored**: written at insert ([consumer.go:210-214](../../internal/consumer/consumer.go#L210-L214)) and never deleted or re-derived by a merge ([merge.go:139-149](../../internal/store/merge.go#L139-L149) writes only `source_mismatch`). | Nothing re-runs the analyzer on a merge, so the count does **not** jump. RC-B makes the `capture_complete` flag start **agreeing with the warning already attached** to the row, and guarantees a truncated proxy row keeps `cc=0` through a merge going forward. RC-B and RC-C ship together; the cap raise is what actually removes the truncation. The historical rows already laundered are corrected by `clens reflag` (§4 backfill block), which flips flags but synthesises no warnings. |
| **float64 accumulation** across every row the reprice prices, in SQLite `REAL`. | Error is ~1e-16 relative per operation, far below a cent. §5 #1 asserts no total: it re-runs `Compute` per row and compares the exact value with the stored `REAL`, so error at this scale cannot flip the comparison. `big.Rat` stays the compute type; only the stored value is a float, as today. |
| **`roundHalfUp` deletion** breaks a caller I have not found. | Grepped: its only production reference is [pricing.go:133](../../internal/pricing/pricing.go#L133); the rest are comments. The compiler is the check. |
| **Reprice rewrites a cost the user deliberately overrode.** | Not a risk once reprice uses the effective table: a user edit overrides a **rate** ([pricing.go:185-189](../../internal/pricing/pricing.go#L185-L189)), not a per-row cost, and reprice prices with the same effective table the insert path uses — `newPriceLoader(cfg).Table()`, the configured off-peak calendar included (§4, and *not* the `nil` form v3's F2.7 wrote) — so a `user` row recomputes to that same override rate. Only `unpriced` rows are skipped; `approximate:cache_ttl_unknown` rows are **reconstructed and repriced**, not skipped (next row). |
| **`approximate:cache_ttl_unknown` rows — repriced, not skipped.** | `TTLUnknown` is not a stored column, but the **`cost_source` label is** ([schema.sql:45](../../internal/store/schema.sql#L45), [:70](../../internal/store/schema.sql#L70), `idx_events_cost_source`) — and it is the reconstruction **key**: a row carrying `approximate:cache_ttl_unknown` proves `usage.TTLUnknown` was `true` at insert, so reprice sets `TTLUnknown = true`, calls `Compute`, and gets back both the corrected amount and the same label ([pricing.go:138-140](../../internal/pricing/pricing.go#L138-L140)). Those rows are **in scope**. The only skip is a row whose computed inputs cannot be rebuilt — `unpriced` (`model_resolved` absent from the table), plus any future `approximate:<reason>` other than `cache_ttl_unknown` (§4 scoping). |
| **Cap raise and storage.** | 2 MB bound, **~0.35 GB** projected growth, moved to `D:`. `BodyCapBytes` stays configurable so it can be lowered without a rebuild. |
| **A larger cap changes merge precedence.** | The *stated mechanism was wrong* (F1.4) and is corrected here: the winner rule reads the two **input** flags ([merge.go:184-198](../../internal/store/merge.go#L184-L198)); the merged flag is assigned 43 lines later at [:241](../../internal/store/merge.go#L241) and is never read by `mergeEvents`, so RC-B **cannot** change the pick inside the merge that applies it. A *later* merge of the already-merged row could take the `!existing.CaptureComplete && incoming.CaptureComplete` branch, but `usageObserved` ([merge.go:214-224](../../internal/store/merge.go#L214-L224), [:372-376](../../internal/store/merge.go#L372-L376)) swaps the pick back whenever the complete side has no measurement — always true of a JSONL row. So: no regression, for a different reason than first given, intended, and covered by the merge tests. |
| **Historical at-cap rows lose their truncation *marker*.** | Raising the default cap reclassifies every historical row whose body sits at the old 262,144 boundary as `Complete` in the two length-vs-cap readers — they compare the stored body against the **current** cap ([app.js:214-216](../../internal/web/app.js#L214-L216), [decode.go:50-55](../../internal/decode/decode.go#L50-L55), [show.go:113](../../internal/cli/show.go#L113)). The `capture_complete` flag is unaffected and stays authoritative. This is an **accepted limitation, documented, not fixed** — marking old rows would need a per-row recorded cap, which the plan deliberately does not add. Recorded in §4's storage-schema row and in `data-privacy-and-compliance.md`. |
| **Historical rows.** | `reprice` fixes their cost and `reflag` fixes their laundered `capture_complete` flag, each only for the rows in its scope. A day whose bodies were already truncated cannot be re-captured — RC-C is forward-only, and the rows `reflag` cannot witness (no provable prefix — no `Content-Length`, or a body-less row) stay laundered and, where they also carry a `stream_incomplete` warning, are reported as the residual. This must be said plainly in the plan and the PR. |
| **The picker's window and the acceptance window drift apart** (§4 picker). | Both must resolve to the *same* half-open IST range for 21 Sept. The window is computed in exactly one place (`timeWindow()`) and its emitted bound is checked by §5's **manual** verification — it carries the local offset and denotes the intended local instant (no Go test can assert an emitted runtime string; [assets_test.go:409-416](../../internal/web/assets_test.go#L409-L416) is the ceiling). A second implementation of "the day" would rebuild the C-1 defect inside the fix for it. |
| **DST in the operator's zone.** | A local day is 23 or 25 hours across a transition, so the window is built from local midnights with the `Date` constructor, never as `start + 86_400_000`. IST has no DST; the code does not assume that of every operator. |
| **The picker is not free — it widens the story.** | It is a fourth, unrelated change on top of three defect classes, and the one part of GI-11 that ships a *feature* rather than a correction. Front-end-only (no server, store, or API change), which is what keeps it off the three fixes' critical path. **If the story must shrink, this is the part to cut, not RC-A/B/C — and the cut is clean.** Cutting it removes §4's picker block, §5's `Picker` bullet, and the three §6 picker rows (window drift, DST, this one); **no acceptance step depends on it** — §5's acceptance #1–#4 are direct `events` queries against a store copy, not shipped UI. What is *lost* is the ability to ask the dashboard for the local day C-1 is written against — the exact window §4's opening paragraph exists to make expressible — so the operator falls back to running the acceptance SQL by hand. |

## 7. Self-review

**As a senior engineer.** The three causes are independent and each has a one-expression fix, so
the risk is not in the edits but in the *sequence*: RC-B without RC-C turns a silent problem into a
loud one, and RC-A without `reprice` *and* RC-B without `reflag` each fix only the future — the code
change stops new damage while every historical row stays wrong, which is why acceptance #3/#4 are
unrunnable without the backfill. The alternative to `reprice` — a
migration that recomputes on open — was rejected: it would put the pricing table on `Open`'s path,
make every boot O(events), and the repo's existing shape for "rewrite stored rows" is an explicit
`--dry-run`/`--yes` CLI command, now three times over (`purge`, `rekey`, `reprice`) plus `reflag`. Nor can the repo's existing `clens ingest --rebuild` stand in for it: the README's "### Re-pricing rows already captured" section ([README.md:111-115](../../README.md#L111-L115)) names `--rebuild` as the re-pricing path and must be rewritten, because a re-ingest produces a **JSONL** row, priced with `speed=""`/`serviceTier=""` ([jsonlogs.go:459](../../internal/jsonlogs/jsonlogs.go#L459)) and absorbed by the `request_id` merge, which never replaces the proxy's bodies ([merge.go:323-328](../../internal/store/merge.go#L323-L328)) — so it cannot reach the proxy-only rows that carry the zeroed costs. The alternative to raising the cap —
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
supplied each retained body (request and response independent): when **both** bodies come from one
owner, that owner's flag; when the two bodies have **different** owners, the **`&&`** of the two
owners' flags — complete only if both contributing sides were complete, never the `||`, because a
false `cc=0` is a visible honest over-report while a false `cc=1` is the exact laundering this story
removes (F4.3); and when the merged row holds **no** body it keeps `existing.CaptureComplete`. Together
with the seventh (mixed-owner) case, this covers the body-ownership shapes the merge tests **pin** —
one owner keeping the bodies (the **errored** row is **request-only**; the `--body-policy off` row has
**no** body — two different causes, not one), two owners that disagree (the `&&`, the seventh case —
**defensive**, pinned by a direct `mergeEvents` unit test on constructed inputs, since no production
path is known to build the shape), and the no-body default — which is exactly what §5's seven cases
assert, **no more**. Error paths: a
reprice with `--yes` but a closed store, an unpriced row, a row with no `model_resolved`.

**As a security engineer.** No new credential surface, no new endpoint, no new outbound call. **Two**
new write paths into `events` — `reprice` (cost columns only) and `reflag` (`capture_complete` only);
neither touches a body or a header, and both are read-only without `--yes`. The cap raise enlarges
stored bodies — and
`CLAUDE.md` is explicit that **the bodies are the asset this repo protects**: a bigger cap stores
more prompt and file content, which is a real privacy cost paid for observability. That is the
correct trade for a loopback-only single-user tool, but it must be stated in the docs rather than
discovered, and the DB move to `D:` should not be done in a way that widens access. The DB carries
no credential (bodies are redacted before insert and `internal/secret` lives outside the DB), so a
larger file is not a larger blast radius for secrets.

## 8. Reproduction queries

Run against a **consistent snapshot** of the live store (`VACUUM INTO`, §5). Read-only; each backs a
claim above. Every figure below is a **dated snapshot** — the store is live and grows, so the
reproducible artifact is the **query**, not the number.

**This rule is not confined to this section.** §5's acceptance figures are the same class of number
and are written as relations on one snapshot for the same reason — a past IST day still moves, because
`jsonlogs` backfills it and every merge rewrites rows in place. The only figures in this document that
are *not* snapshots are DeepSeek's invoice figures (§1) and the arithmetic derived from the captured
tokens themselves; those are fixed. Everything measured out of the store is re-measured, never quoted.

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
-- → 2327 rows, 196.68M cache-read tokens

-- RC-B: the flag is laundered only by merges. The predicate is the UNION of the two halves:
-- `source='proxy'` reaches the UNMERGED proxy rows (their source_refs is ''), while
-- `instr(source_refs,'proxy')>0` reaches the MERGED ones. EITHER ALONE NARROWS: source_refs
-- defaults to '' (schema.sql:15) and only merge.go:166 ever assigns it, so the bare instr(...)
-- form silently drops every unmerged proxy row.
WITH x AS (
  SELECT source_refs, capture_complete AS cc, length(req_body) AS stored,
         CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) AS true_len
    FROM events WHERE (source = 'proxy' OR instr(source_refs,'proxy') > 0))
SELECT source_refs, cc, COUNT(*),
       SUM(CASE WHEN true_len > stored THEN 1 ELSE 0 END) AS trunc
  FROM x GROUP BY source_refs, cc;
-- → proxy,jsonl / cc=1 / 3790 rows / 2844 truncated   (proxy-first merged)
--   jsonl,proxy / cc=1 /   21 rows /   21 truncated   (jsonl-first merged)
--   ''          / cc=1 / 1413 rows /    0 truncated   (unmerged -- the key is the EMPTY STRING)
--   ''          / cc=0 /  316 rows /  316 truncated   (unmerged, honestly flagged)
--   the four buckets sum to 5,540 = the union total; the NAIVE instr(...) form reaches only 3,811
--   and drops the 1,729 unmerged proxy rows (source='proxy' AND source_refs=''). The v5/earlier
--   numbers in this block (3,405/2,613/1,289/237; 3,769/1,377/231) were earlier snapshots of the
--   same store -- dated, not wrong. Measured 2026-09-22T06:49:41Z; the store is live and grows, so
--   every figure here is a snapshot and the reproducible artifact is the QUERY, not the number.

-- RC-B: stream_incomplete warnings, and whether the row they sit on is merged.
-- Written once, at insert (consumer.go:210-214); the merge writes only source_mismatch
-- (merge.go:139-149) and never deletes or re-derives a warning. So a warning on a merged
-- row is STALE -- it sits on a flag the merge laundered to capture_complete=1. The count
-- does NOT rise when RC-B lands: nothing re-runs the analyzer on a merge.
SELECT CASE WHEN instr(e.source_refs, ',') > 0 THEN 'merged' ELSE 'unmerged' END AS shape,
       e.capture_complete AS cc, COUNT(*) AS warnings
  FROM warnings w JOIN events e ON e.id = w.event_id
 WHERE w.kind = 'stream_incomplete'
 GROUP BY shape, cc;
-- all-time: 2,918 stream_incomplete warnings; 2,860 sit on a merged row with cc=1.
--   narrowed to the IST day (add: AND e.started_at >= 1789929000000000000
--   AND e.started_at < 1790015400000000000): 1,274 total, 1,254 merged
--   (1,233 source_refs='proxy,jsonl' + 21 'jsonl,proxy'), and 20 unmerged.

-- RC-B backfill (`clens reflag`): the three buckets its report must print, over the union scope.
-- The witness is the SAME predicate acceptance #4 uses -- a stored body that is a strict prefix of
-- its client Content-Length, on EITHER side, guarded by `length(...) IS NOT NULL` so a body-less row
-- is never witnessed AND never silently dropped from SUM (a body-less `cc=1` row is a real mode:
-- BodyPolicy full|off, config.go:35).
--   flipped        = cc=1 AND witness
--   already honest = cc=0
--   residual       = cc=1 AND an existing stream_incomplete warning AND NOT witness
-- The warning is REQUIRED for the residual: it is written at insert (consumer.go:210-214) and
-- ruleStreamIncomplete fires iff IsStream && !CaptureComplete (rules.go:182-187), so warning + cc=1
-- is exactly the laundered population, which the witness then splits into repairable and residual.
-- DO NOT reinstate the old `SUM(cc=1 AND NOT witness)`: with no warning join it measures HEALTHY
-- complete rows -- the conductor measured it printing 2,298 -- not the 138 its comment used to claim.
WITH x AS (
  SELECT e.capture_complete AS cc,
         ( (length(e.req_body)  IS NOT NULL AND COALESCE(CAST(json_extract(e.req_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(e.req_body))
           OR
           (length(e.resp_body) IS NOT NULL AND COALESCE(CAST(json_extract(e.resp_headers,'$."Content-Length"[0]') AS INTEGER),0) > length(e.resp_body)) ) AS witness,
         EXISTS(SELECT 1 FROM warnings w WHERE w.event_id = e.id AND w.kind = 'stream_incomplete') AS warned
    FROM events e WHERE (e.source = 'proxy' OR instr(e.source_refs,'proxy') > 0))
SELECT SUM(cc = 1 AND witness)                AS flipped,
       SUM(cc = 0)                            AS already_honest,
       SUM(cc = 1 AND warned AND NOT witness) AS residual,
       SUM(cc = 1 AND NOT witness AND NOT warned) AS healthy
  FROM x;
-- → all-time (measured 2026-09-22T06:49:41Z): 2,865 flipped / 316 already honest / 138 residual
--   (2,221 healthy stay cc=1; 316 + 2,865 + 138 + 2,221 = 5,540).
--   IST day 1789929000000000000..1790015400000000000: 1,176 flipped / 128 already honest / 78
--   residual (1,065 healthy).
--   The response side adds 0 rows on this store (flipped request-only = 2,865 = request-or-response).

-- RC-C: how far past the cap real bodies go
WITH x AS (
  SELECT CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) AS n
    FROM events WHERE source='proxy')
SELECT SUM(n > 262144), SUM(n > 1048576), SUM(n > 2097152), MAX(n), CAST(AVG(n) AS INT)
  FROM x WHERE n IS NOT NULL;
-- → 3554, 117, 0, 1246222, 351394
--   measured 2026-09-22T09:06Z, over the 6,088 proxy rows carrying a Content-Length
--   (a dated snapshot; the store is live, so the reproducible artifact is the QUERY).
--   768 of 6,114 stored responses sit at exactly the 262,144 cap.
```

## Change History

### v15 — round-11 review + the convergence stamp (author; **converged**)

Round 11 confirmed on v14 with **0 BLOCKER / 0 MAJOR**. It is the **third** consecutive clean round:
rounds 9 and 10 were each clean (v13's `O4` and v14's record both say so), so the two-round window
**closed at rounds 9–10** and round 11 simply confirmed it — no new defect class reopened it. Two small
findings were raised and both applied.

- **F11.1 (MINOR)** — `.beads/GI-11/br-GI-11-04-feat-store-capture-complete-follows-bodies.md`'s
  Rationale carried the union-population figures as a **bare** measurement with no dated-snapshot
  label, while every other carrier of those same figures dates them (plan §2/§6/§8, and the sibling
  `br-GI-11-05`, which calls its identical numbers "a *dated illustration of magnitude and sign*").
  The Rationale now carries the same qualifier: the figures are measured across the union predicate's
  rows **on a dated snapshot, 2026-09-22T06:49:41Z** — the snapshot §2's RC-B table dates. **No figure
  changed** (5,540 / 2,865 / 2,844 + 21 / 316 / 0 are correct *for that snapshot*); only their
  snapshot semantics were missing. `.beads/GI-11/br-GI-11-04`.
- **F11.2 (NIT)** — §4's `cmd/clens/main_test.go` row instructed moving "the 19 subcommands" numeral to
  **21** in both `:10-14` and `:26`, but `:10-14` names the count **and its own composition** ("doctor
  and serve, br-GI-1-15's six collectors, br-GI-1-17's ten readers and writers, and br-GI-9-04's
  rekey" — 2 + 6 + 10 + 1 = 19), so moving only the numeral leaves that sentence claiming *21* while
  its enumeration sums to *19*. The row now instructs moving **both** the numeral **and** the
  composition sentence — the enumeration **gains `br-GI-11-03`'s `reprice` and `br-GI-11-06`'s
  `reflag`** — so the comment's count and its enumeration agree at 21. The same instruction is carried
  into `.beads/GI-11/br-GI-11-09-feat-cmd-register-new-commands.md` (its two `19 → 21` clauses and its
  `Files to Touch` line). §4; `.beads/GI-11/br-GI-11-09`.
- **Convergence.** The header is set to **v15 / `status=converged`** and the `**Status**` field to
  `converged`, following GI-1's convention (the `<!-- version=N status=converged -->` header marker). The
  stamp is earned, not asserted this round: the window was already closed by **two consecutive clean
  reviewed rounds (9 and 10)**, and round 11 is the confirming round — it raised no BLOCKER and no
  MAJOR, so the window stays closed and the plan is declared converged.

### v14 — round-10 review + conductor overrides (author; review-pending, window 2 of 2)

- **F10.1 (MINOR)** — `.beads/GI-11/br-GI-11-07-fix-config-body-cap-default.md`'s Rationale carried the
  RC-C population §2 has disowned ("4,873 proxy rows: 2,845 (58%) over 256 KB, 106 over 1 MB"). It now
  carries §2's dated base and cells: **6,088** proxy rows carrying a `Content-Length`
  (2026-09-22T09:06Z), **3,554 (58%)** over 256 KB, **117** over 1 MB, **0** over 2 MB, largest
  **1,246,222 bytes**, and the responses side **768 of 6,114 (12.6%)**; the "58% of calls against 12%
  of responses" prose is kept unchanged. `.beads/GI-11/br-GI-11-07`.
- **F10.2 (MINOR → PARTIAL, conductor override OV-1)** — §3's "no transcript line" bullet is
  **disambiguated**, not renumbered: the population is now named (`/v1/messages` calls with no
  transcript line), so the **659** figure can be re-measured the same way. The conductor measured the
  disputed population on the plan's own 2026-09-22T09:06Z snapshot: the `/v1/messages` reading is
  **659** rows / 32,019,460 tokens (the all-paths reading is 828 rows / the identical 32,019,460
  tokens — the extra 169 `count_tokens`/`api/hello`/`/` rows carry no tokens). **No number changes;
  828 is not written into the plan.** §3.
- **OV-2 (conductor)** — §3's all-source token total is corrected to the snapshot its sentence
  asserts: **297,080,674 → 297,172,779** (2026-09-22T09:06Z; proxy 209,153,629 + jsonl 88,019,150 over
  2,864 rows). The `209,153,629` half of the same bullet was measured correct and stands; the
  `183,854,144` DeepSeek-side figure is external (an invoice number, not a store measurement) and
  stands; the ~25.3 M residual is the proxy-vs-DeepSeek difference and is unaffected. §3.
- **F10.3 (NIT)** — `.beads/GI-11/br-GI-11-05-feat-store-reflag-backfill.md` named the retired symbol
  `cc0_before` in relation 2; it now reads `baseline_cc0 + W + R + H = scope_total`, matching §5 #3's
  one symbol. The bead's dated all-time illustration (316 / 2,865 / 138 / 2,221 / 5,540 — a labelled
  *dated illustration of magnitude and sign*) is unchanged. `.beads/GI-11/br-GI-11-05`.
- **Conductor overrides OV-1–OV-4** were applied verbatim; the reconciliation is recorded in
  `review/round-10/triage.md`.
  - **OV-1** — F10.2 is PARTIAL and its number does not change: the plan's **659** is the
    `/v1/messages` reading of "calls" on the plan's own 2026-09-22T09:06Z snapshot (conductor-measured:
    659 rows / 32,019,460 tokens); the disambiguation is the only edit, and **828 is not written**.
  - **OV-2** — §3:177's figure is updated to **297,172,779** so it matches the date the sentence
    asserts.
  - **OV-3** — `br-GI-11-07`'s Rationale carries §2's dated base and cells; no other bead touched.
  - **OV-4** — `br-GI-11-05`'s `cc0_before` is unified to `baseline_cc0`; its dated illustration
    stands.
- The header is set to **v14 / `status=review-pending`** (window 2 of 2); no `status=converged` stamp
  is written — the confirming round is not the author's to run.

### v13 — round-9 review + conductor overrides (author; review-pending, window 1 of 2)

- **F9.1 (MINOR)** — §6's `float64 accumulation` row no longer frames `$3.46` as an acceptance
  *target*. v12 removed the day-total-as-target class; the row now states the **per-row** comparison
  (§5 #1 re-runs `Compute` and compares the stored `REAL`), drops the `±$0.05` / "four orders of
  magnitude" arithmetic (which did not reconcile — float error on a ~$3.46 value is ~1e-15 absolute,
  ~13 orders below `0.05`), and drops the `2,257`-row scope (the IST day's proxy count) for the
  reprice's all-time scope. §6.
- **F9.2 (MINOR)** — the criterion scope is now stated once. §5's preamble and #1/#2 disagreed: the
  preamble windowed #1 and #2 to the IST day, while #1's subject was the **all-time** reprice scope and
  #2 read a session's **all-time** `SUM` (`reconcileSessionTx` aggregates with no date predicate,
  [store.go:731-733](../../internal/store/store.go#L731-L733)). #1 now reads "**every row the reprice
  priced**" and #2 "the session's own all-time `SUM`"; the preamble states the window **bounds no
  criterion** and survives only as the window the dated illustrations are drawn over. §5.
- **F9.3 (MINOR)** — §5 #3's second relation now names its source: it is evaluated with **§8's
  four-bucket query** (the same witness predicate, so the two cannot drift), and `scope_total` is that
  query's row count over the same union scope. `clens reflag --dry-run` prints only **three** buckets
  (`flipped`/`already honest`/`residual`, br-GI-11-05) and carries neither `H` nor `scope_total`, so the
  three-bucket print is now stated as the **first** (flip) relation's baseline only. One symbol
  (`baseline_cc0`) is used for the cc=0 count in both relations — it was `cc0_before` in relation 2.
  §5.
- **F9.4 (MINOR)** — the plan's own record no longer contradicts §5 #1. The v12 change-history entry
  described #1 as "the rows the reprice wrote equal the exact recomputation over those same rows" — the
  *subset* framing §5 explicitly rejects ("not by asking which rows the run wrote, which is unanswerable
  after the fact"). It is reworded to §5's form (every in-scope row recomputed and compared; a row whose
  value did not move satisfies the relation trivially). The same framing is repeated downstream in
  `br-GI-11-02`'s "Integration Tests" bullet and is reworded identically there — leaving it would have
  let the contradiction survive the plan. §Change History (v12); `.beads/GI-11/br-GI-11-02`.
- **F9.5 (MINOR)** — §5 #1 states that its recompute reconstructs `usage.TTLUnknown` from the row's
  `cost_source` label when it is `approximate:cache_ttl_unknown`, i.e. it reuses the reprice's own
  input-reconstruction rule (§4). Without it, an acceptance recomputing from the stored columns alone
  would read a correct reprice as a miss on exactly the rows §4 puts in scope. Stated **with its
  caveat**: the reconstruction is **vacuous on the 2026-09-22T09:06Z store** (`cost_source` is `shipped`
  48,477 / `unpriced` 198, zero `approximate:*`), but it is a correctness gap that would bite the moment
  an `approximate:cache_ttl_unknown` row exists — the zero-row fact is **not** a reason to skip it. §5.
- **F9.6 (NIT)** — §1 and §8 now state the same non-snapshot class list. §1 said "the only figures here
  that are *not* snapshots are DeepSeek's invoice figures"; §8 (v12) added "and the arithmetic derived
  from the captured tokens themselves". §1's sentence gains that second class. §1.
- **Conductor overrides O1–O4** were applied verbatim; the reconciliation is recorded in
  `review/round-9/triage.md`.
  - **O1** — #1's recompute scope is "every row the reprice priced"; `unpriced` rows are **out of
    scope** for the recompute relation (their inputs cannot be reconstructed, so the clause cannot be
    evaluated for them) and are checked only for having been **left untouched** (stored cost and
    `cost_source` unchanged). The load-bearing "not the rows the run wrote" reasoning is **kept**.
  - **O2** — §3's stale `2,802 / 401` decomposition (undated) is replaced by the **2026-09-22T09:06Z**
    re-measurement of a `VACUUM INTO` copy: the IST day 2026-09-21's proxy rows total **2,426** =
    2,257 `/v1/messages` + 144 `/v1/messages/count_tokens` + 25 other (24 `/api/hello` + 1 `/`). The
    load-bearing half — 2,257 `/v1/messages` = the proxy's row count, so the day's client requests are
    100% captured — stands. §3's other live-store counts are dated to the same snapshot (`events` =
    48,675).
  - **O3** — §2's RC-C table is dated (2026-09-22T09:06Z, over the **6,088** proxy rows that carry a
    `Content-Length`): over-256 KB **3,554** (58.4%), over-1 MB **117**, over-2 MB **0**, largest
    **1,246,222**, mean **351,394**, responses at the cap **768** (of 6,114 = 12.6%). The counting
    sentence's base is corrected (**6,088**, not "4,873 proxy rows" — a different population). §5 #4's
    cross-reference moves **599 → 768**; §8's RC-C query output is re-labelled to the same snapshot.
  - **O4** — the header is set to **v13 / `status=review-pending`** (window 1 of 2): round 9 was clean,
    but this round's edits have not been reviewed, so the plan is **not** converged. The
    `status=converged` stamp is not written this round — it is earned by two consecutive clean reviewed
    rounds, not by the author.

### v12 — acceptance becomes a relation, not a literal (author; post-convergence)

- **The seam v11 exposed, fixed at its root.** v11 corrected one stale figure. The user's objection is
  the general form of the same defect: *"that figure will always turn stale — consider the number when
  we fire the backfill or modify the db, not before."* §5's acceptance criteria were carrying live-store
  counts as **targets** — `128 → 1,304`, `0.82 → 3.46` — under a preamble claiming they were "the class
  of number that **must hold exactly**, unlike §2/§4/§8's live-store snapshots". **That claim was
  false**, and this plan's own history is the disproof: the RC-A row count over the same *fixed past
  day* moved 2,352 → 2,327 during the plan's life.
- **Why a past day moves, which is the part that was missing.** The intuition "the window is in the
  past, so its rows are frozen" is wrong here. `jsonlogs` **backfills** rows for past days as it
  ingests transcripts, and every cross-source merge **rewrites a row in place** — `source_refs`,
  bodies, and (until RC-B lands) the completeness flag. The day's row population is a moving target,
  so an acceptance criterion pinned to a count is a claim about a moving target.
- **The fix: every acceptance criterion is now a relation between two measurements on one snapshot**,
  checkable without knowing today's numbers, with the baseline measured **immediately before** the step
  runs. #1 asserts every in-scope row of the snapshot is recomputed and compared — **not** the rows the
  run wrote, which is unanswerable after the fact (a row added later was priced at insert and was never
  in scope, so it cannot move the comparison; a row whose value did not move satisfies the relation
  trivially), with the invoice agreement **reported, not gated**. #3 asserts `cc0_after = baseline_cc0 + W` and that the
  report's four buckets **partition** the scope total — both readable off one snapshot. §8's own rule
  ("the reproducible artifact is the query, not the number") now explicitly covers §5, and names the
  only figures in the document that are *not* snapshots: DeepSeek's invoice (§1) and the arithmetic
  derived from captured tokens.
- **The baseline is now read from the command, not measured by hand.** `clens reflag --dry-run` already
  prints exactly the three buckets (br-GI-11-06), so acceptance #3 takes its baseline from **the same
  code that will do the repair, at the instant before it runs** — which is the only form that cannot be
  stale, and the reason the criterion needs no literal at all.
- **One acceptance *subject* widens, deliberately, and this is a change:** #3's scope moves from the IST
  day to **the command's own scope** (the union predicate, no time filter), because `reflag` repairs all
  of history and an acceptance narrower than the repair would be checking a subset of what it did. The
  IST day survives as a subset, and as the window for #1, which is the one criterion genuinely tied to a
  day (DeepSeek's invoice). No mechanism, file list or file changes. v10's convergence stands on the
  same reasoning as v11: the loop converges on BLOCKER/MAJOR *design* findings, and this is the plan
  applying its own stated rule to the last place that had escaped it.

### v11 — Phase 4 polish (author; post-convergence)

- **The stale `2,352` corrected to `2,327`.** §2's RC-A sentence and §8's `COUNT(*)` expected output
  both carried a row count that was wrong from v1 onward. Round 1's conductor measured the correct
  value (`2,327`, same 2,426-row population) and reported the delta — and the finding was then closed
  as moot **without the figure being corrected**, so it survived all eight review rounds and was
  inherited by `br-GI-11-01`. Re-measured directly against the live store at **2026-09-22T07:58:52Z**:
  `rows_total` 2,426, `rows_zero_cost` **2,327**, `cache_read_all` 201,085,056, `cache_read_on_zero_rows`
  196,683,520. The other two figures in the sentence were correct; only the count was not.
- **Why this is not a design change, and why v10's converged status stands.** The loop converges on
  BLOCKER/MAJOR *design* findings; this is a figure correction found by the Phase 4 bead-quality pass,
  applied under §8's own rule that "the reproducible artifact is the **query**, not the number". No
  reviewed claim, mechanism, file list or acceptance criterion changes — the query at §8 already
  returned the right rows, only its transcribed output was stale.
- **The lesson worth keeping:** the review loop can close a *finding* while leaving the *artifact*
  wrong. Both rounds that touched this number (round 1's report, round 5's re-measurement sweep) were
  working from figures measured in separate statements against a live store — exactly the hazard §8's
  snapshot semantics were later introduced to remove. A converged plan is a plan whose *reasoning*
  has converged; it still needs a pass that re-reads every transcribed figure against its own query.

### v10 — round-8 review (converged)

- **F8.1 (MINOR)** — the §4 change-list is made complete and the test-file count checkable. §4's
  Code table named a test file for every other code change but had **no reprice test row**, while the
  docs rows counted `reprice_test.go` as one of exactly **three** new files — and the file's package
  was never stated, so the count could not be checked against §4. §5 requires reprice tests at
  **two** levels (the store-level write loop and same-transaction rollup against `Store.RepriceCosts`,
  and the CLI-level `--dry-run` "changes nothing"), so the reconciliation names **both** homes, each
  with its package: `internal/store/reprice_test.go` *(new)* (the store-level cases, mirroring the
  reflag block's `internal/store/reflag_test.go` row) and `internal/cli/reprice_test.go` *(new)* (the
  CLI `--dry-run` case, mirroring `internal/cli/rekey_test.go`'s `--dry-run` cases — a `--dry-run`
  flag cannot be pinned from the `internal/store` package). The count therefore moves by **four** new
  files, and the `testing-and-quality.md:12` / `INDEX.md:37` rows both state **52 → 56**. §4, §5.
- **Observation (not a finding) — resolved deliberately.** §7's rejects-alternatives paragraph now
  addresses the README's "### Re-pricing rows already captured" section, as §4's README row requires:
  it states why `clens ingest --rebuild` cannot stand in for `reprice` — a re-ingest is a **JSONL**
  row, priced `speed=""`/`serviceTier=""` and absorbed by the `request_id` merge, which never replaces
  the proxy's bodies, so it cannot reach the RC-A proxy-only rows. §4's README row keeps its own
  instruction; the lifted §7 sentence is what closes it. §7.
- **Converge stamp.** Rounds 7 and 8 are both clean (no BLOCKER, no MAJOR), so the header meta line
  gains `**Status**: converged` and `**Plan version**: 10`. The plan has converged.

### v9 — round-7 review

- **F7.1 (MINOR)** — §4's `internal/cli/reprice.go` row no longer cites a nonexistent precedent:
  "exactly as `purge` and `rekey` split their CLI from their store work" replaces "exactly as `merge`
  and `rekey` split their CLI from their store work". There is **no `clens merge`** (no
  `internal/cli/merge.go`; the dispatch table is `cmd/clens/main.go:16-39`), so §4 now agrees with §7's
  "three times over (`purge`, `rekey`, `reprice`) plus `reflag`". The store-side merge is named
  (`merge.go:134`, reachable only from `insertOrMerge` and `rekey`). §4.
- **F7.2 (MINOR)** — the `internal/store` must-not-import-`internal/pricing` constraint is now
  **enforced by a test, not prose**: §4 gains an `internal/store/importguard_test.go` row
  (`TestStoreDoesNotImportPricing`) mirroring `internal/proxy/importguard_test.go:26-49` one-for-one
  (glob `*.go`, skip `_test.go`, forbid `/internal/pricing`), and the `internal/store` method row names
  it. The compiler cannot catch this import, which is exactly why the repo's other load-bearing
  boundaries each carry a source-reading guard. The test-file count (`testing-and-quality.md:12`,
  `INDEX.md:37`) moves by **three** new files now (`reprice_test.go`, `reflag_test.go`,
  `importguard_test.go`). §4, §5.
- **F7.3 (MINOR)** — the "two destructive commands" claim is named at all **three** sites the plan had
  missed: `README.md:107-109` ("the one command that destroys data" — already false today, since
  `rekey` deletes rows), `internal/cli/purge.go:19-25` ("one of two commands … `clens rekey` … is the
  other"), and `internal/cli/rekey.go:17` ("the story's second destructive command"). §4 states the
  correct post-GI-11 fact: **four `--yes`-gated writers** (`purge`, `rekey`, `reprice`, `reflag`) but
  still **two destructive** ones — `reprice`/`reflag` delete no row (they rewrite columns), so a literal
  "two → four destructive commands" edit would be wrong. `README:107-109` is corrected beside the
  command-table edit; the two code comments stay (they remain true). §4.
- **F7.4 (NIT)** — the storage projection no longer mixes units or states a total below its own parts:
  the `≈0.26 GB + ≈0.10 GB → ≈0.36 GiB → ~0.35 GB` decomposition is **dropped** in §4 and §6, leaving
  the coordinator's headline **~0.35 GB** alone (vs the earlier ~0.45 GB). One unit, one number. §4, §6.
- **F7.5 (NIT)** — §5's picker guard (b) and §4's `assets_test.go` row no longer claim the whole-file
  `TestAssetsEveryLookupHasAMount` technique can express "inside `timeWindow`". The guard is scoped via
  `funcBody(js, "timeWindow")` (`assets_test.go:358-377`), which requires a top-level
  `function timeWindow(` and a column-0 `\n}\n`; the test must `t.Fatal` on `!ok` (the vacuity trap
  `funcBody`'s doc comment warns about, `:359-366`, the `!ok → t.Fatal` shape `:426-428` uses), and
  **both** halves — the `toISOString`-absence check and the built-by-hand offset check — are scoped to
  that slice rather than run whole-file. §4, §5.

### v8 — round-6 review + conductor directives

- **F6.1 (MAJOR, conductor override — REVERSES F2.7)** — reprice prices with the **effective,
  Loader-backed table including the CONFIGURED off-peak calendar**: `newPriceLoader(cfg).Table()`,
  i.e. `pricing.NewLoader(pricing.DefaultPath(), cfg.PeakOffPeakDates).Table()` — **not** the `nil`
  form v3's F2.7 wrote. **This supersedes F2.7's `nil` form.** F2.7's reasoning was wrong: it copied
  `models.go:34` / `prices.go:50,:94`, but those are a read-only catalogue print and a price-editor's
  own output — neither prices a stored call, so the off-peak calendar is irrelevant *there*. `nil` is
  **not** "as configured": `config_test.go:201-228` pins `nil` = unset → the shipped list, distinct
  from `[]` (the `none` spelling), and `resolvedOffPeakDates` maps `nil` → shipped while a non-nil
  empty list means *nothing* off-peak. The helper's own comment at `internal/cli/ingest.go:119-123`
  names the invariant — "the one loader shape every pricer in this process wants, so two pricers
  cannot disagree about the configured off-peak dates". §5's reprice bullet gains a case whose config
  sets `PeakOffPeakDates` to a non-default value (e.g. `none`) and asserts the recomputed row differs
  — a `nil`-form test would pass and never catch this. §4, §5, §6.
- **F6.2 (MINOR, conductor override — behaviour change, not just rationale)** — `cost_source` is
  **stored and indexed** ([schema.sql:45], [:70]), so `approximate:cache_ttl_unknown` is the
  **reconstruction key**, not a mere label: a row carrying it proves `usage.TTLUnknown` was `true` at
  insert, so reprice rebuilds the exact input (`TTLUnknown = true`), calls `Compute`, and gets back
  both the corrected amount **and the same label** back. Those rows are therefore **in scope, not
  skipped** — RC-A corrupted them identically. The skip rule is now: skip only rows whose computed
  inputs cannot be reconstructed from stored columns — today `unpriced` alone, plus any *future*
  `approximate:<reason>` other than `cache_ttl_unknown`, each for the named reason. The false
  "`TTLUnknown` is not a stored column, so the label is not derivable" parenthetical is dropped
  (§4:195, §6:621); §5's test bullet flips to "repriced, label preserved". §4, §5, §6.
- **F6.3 (MINOR, conductor directive)** — the all-time **59** is named distinctly and related to the
  **138**: 59 is a sub-count of the **`flipped` 2,865** (flips that leave a `cc=0` row with no warning,
  i.e. a non-stream truncation), while 138 is the **`residual`** the repair cannot reach (`cc=1` ∧
  warning ∧ no witness). The two are **disjoint** and never summed; the only sum that closes is the
  four buckets (`316 + 2,865 + 138 + 2,221 = 5,540`). §4.
- **F6.4 (MINOR, conductor directive)** — the seventh mixed-owner case is **defensive, not reachable
  end-to-end**: production paths do not obviously build a mixed-owner row (a `request_id` has exactly
  one proxy row; jsonl rows carry no body), so the `&&` is pinned by a **direct `mergeEvents` unit
  test on constructed inputs**, and the rule is conservative so that *if* the shape is reachable the
  row errs to `false`. The false "a body-less (`--body-policy off`) request yields a response-only
  shape" clause is dropped. The sixth case is **de-duplicated**: an **errored** proxy row is
  **request-only** under the default policy (`ErrorHandler` → `submit(0, nil, nil, false, err)`,
  [proxy.go:49-53], with `st.reqBody.Bytes()` non-nil at [:215-224]), whereas "no bodies" is the
  **`--body-policy off`** shape — two different causes the plan conflated. §4, §7.
- **F6.5 (MINOR, conductor directive)** — the docs rows name the two repeated sites the plan missed:
  [dashboard.md:6] ("1030 lines of hand-written JavaScript" — the same number as the `:21-25` table,
  a second sentence in the same file) and [workflows.md:140] ("`--rebuild` the re-pricing path" — the
  same claim as `cli-and-tooling.md:19`). §4.
- **F6.6 (NIT, conductor directive)** — the storage projection is restated as **~0.35 GB** (the parts
  sum to ≈0.36 GiB against the former ~0.45 GB). §4, §6.
- **F6.7 (MINOR, conductor override)** — the reprice seam is **named**: `internal/store` must **not**
  import `internal/pricing` (it imports it nowhere today; `internal/proxy`'s package doc is the repo's
  standing example of why the boundary is load-bearing), and **the store computes** via the repo's
  **existing** local mirror interface `PriceComputer` ([analyzer.go:35-40]), reused rather than
  reinvented — the CLI passes the effective Loader-backed table into `RepriceCosts`. A
  precomputed-slice alternative is stated as acceptable but **not** the named choice. §4.
- **Conductor overrides** for F6.1–F6.7 were applied verbatim; the record is in
  `review/round-6/triage.md`.

### v7 — round-5 review + two conductor directives

- **F5.1 (MAJOR, conductor directive)** — the **residual** now has ONE definition, stated identically
  in all four places (§4's backfill prose, §4's store-method code row, §5's reflag test row, §8's
  query): `capture_complete = 1` **and** an existing `stream_incomplete` warning **and** no provable
  prefix witness. The plan records *why* the warning belongs (written at insert, fires iff
  `IsStream && !CaptureComplete`, so *warning + `cc=1`* is precisely the laundered population) and
  *why §8's old form was wrong* (`SUM(cc = 1 AND NOT witness)` measures **healthy** rows — the
  conductor measured it printing 2,298 — not the 138 its comment claimed). §4, §5, §8.
- **F5.1 witness predicate (conductor-refined)** — the witness is the conductor's `IS NOT NULL`-guarded
  request-or-response form, **not** the literal `COALESCE(...) > length(req_body)` text: without the
  guard a body-less row's comparison is NULL, `NOT witness` is NULL and `SUM` drops the row (measured
  62 rows); `COALESCE(length(req_body),0)` instead falsely witnesses it (measured +33 rows). The
  *why* is written into the plan so it is not simplified back. §4, §5, §8.
- **F5.2 (MINOR, conductor directive)** — the response-side witness is **kept** (br-GI-7-08 makes the
  flag cover both bodies, and a non-streamed over-cap response is real), and its measured contribution
  is stated: **0 rows on this store** (`flipped` request-only = 2,865 = request-or-response), so a
  request-only form is equivalent *here* — principled, not speculative. §4.
- **F5.3 (MINOR, conductor directive)** — every figure is framed as a **dated snapshot of a live
  store** (§1), the **query** is the reproducible artifact (§1, §8), and the two classes of number are
  separated: §2/§4/§8 are live snapshots; §5's **acceptance** figures are measured against a **frozen
  copy** and are the ones that must hold exactly. §1, §2, §4, §5, §6, §8.
  **Superseded by v12:** the "must hold exactly" half of this was **wrong** — a frozen copy makes an
  acceptance figure reproducible, not permanent. Even a fixed past day drifts, so §5 is now relations
  on one snapshot. The rest of F5.3 (snapshot framing, query-as-artifact) stands.
- **F5.4 (MINOR, conductor directive)** — §4's `merge_test.go` row gains a **seventh, mixed-owner**
  case (request body from one side, response from the other) pinning the **`&&`** (conservative →
  `false`). It is **reachable**: [merge.go:323-328] backfills the two bodies independently, and an
  errored proxy row is request-only ([proxy.go:49-53]). §7's "covers every reachable shape" is
  narrowed to what the seven tests **pin**. §4, §5, §7.
- **F5.5 (MINOR, conductor directive)** — the docs rows name **both** new test files
  (`reprice_test.go` **and** `reflag_test.go`) and move **every** repeated figure: `INDEX.md:37`
  ("52 test files"), `INDEX.md:40` ("1030 lines") as well as the `testing-and-quality.md` /
  `dashboard.md` rows. §4.
- **F5.6 (NIT, conductor directive)** — acceptance #3's gloss corrected: the IST day's 128 unmerged
  `cc=0` rows are **20 carrying a `stream_incomplete` warning** plus **108 non-stream truncations**
  (no warning) — not all 128 the warning population. §5.
- **F6.x (NEW SCOPE, conductor-supplied)** — §5 specifies the **acceptance copy method**:
  **`VACUUM INTO`** (consistent, no service interruption, SQLite 3.27+) as the default, with the
  stop-`clens`-then-copy-all-three-files alternative stated and *not* the default, and the acceptance
  figures declared measures of a **consistent snapshot**. §5, §8.
- **Storage-projection phrasing (conductor directive)** — §4/§6 no longer state a stale absolute pair
  ("1.44 GB → ~1.9 GB", whose lower bound drifts as the live store grows); they state the cap's own
  contribution, **+~0.45 GB**. §4.
- **Conductor overrides** for F5.1–F5.6, F6.x and the projection phrasing were applied verbatim; the
  record is in `review/round-5/triage.md`.

### v6 — round-4 review + historical-backfill scope addition

- **F4.1 (MAJOR)** — the RC-B predicate is now the **union** `(source = 'proxy' OR
  instr(source_refs,'proxy') > 0)` everywhere (§2's table and paragraph, §8's query, acceptance #3
  and #4). The plan states *why* the naive `instr(...)` form is wrong: `source_refs` is
  `TEXT NOT NULL DEFAULT ''` ([schema.sql:15]) assigned only by `merge.go:166`, so it **narrows**
  the population by silently dropping the 1,611 unmerged proxy rows — 3,783 reached vs the union's
  5,394. The unmerged bucket is labelled by its actual key, the empty string `''`, not "proxy-only".
  §2's table and §8's buckets are re-derived from the 2026-09-22 measurements. §2, §5, §8.
- **F4.2 (MAJOR, conductor directive)** — §5's picker bullet now pins **two** source-shape facts, not
  three; the semantic case ("the instant it denotes equals the intended local instant") is moved to
  the **manual** list, because a text/regex test cannot assert an emitted runtime string
  ([assets_test.go:409-416]). §6's identical over-claim is corrected in the same edit — the
  window-drift row now says the bound is checked by **manual** verification, not a "semantic
  assertion". §5, §6.
- **F4.3 (MINOR, conductor directive)** — §4's `merge.go` rule resolves the two-owner case with the
  **conservative `&&`**: when the retained request and response bodies have different owners, the
  surviving flag is the **AND** of the two owners' flags, never the `||`. The plan says it errs toward
  `false` and why that is safe (a false `cc=0` is a visible honest over-report; a false `cc=1` is the
  defect this story exists to remove). §7 restates it. §4, §7.
- **F4.4 (MINOR)** — §6's "2,613 rows stop claiming completeness" is reconciled to the widened scope:
  **2,825 proxy-first + 21 jsonl-first = 2,846** (union predicate, measured 2026-09-22), so §6 can no
  longer disagree with §5/§8. §6.
- **F4.5 (MINOR, conductor directive)** — the dead `custom` option on Calls is **resolved, not
  described**: the `<option>` set is built **per mount** — Calls offers `hour | date | month`, only
  Stats offers `custom`. §4 states the decision and rejects the disable-alternative. §4.
- **F4.6 (NIT, conductor directive)** — §4's `merge_test.go` row pins the load-bearing fixture: the
  `[:873-875]` test's JSONL side is a `fullEvent` that sets both bodies
  ([store_test.go:72-73]), but a real JSONL row has none ([jsonlogs.go:436]), so under the `&&` rule
  a production-shaped fixture would invert the assertion. §4.
- **NEW SCOPE — the historical `capture_complete` backfill (`clens reflag`)** — added to §4 as its
  own block. RC-B's fix lives in `merge.go` and rewrites **no existing row**, so acceptance #3/#4 are
  unrunnable without a backfill (the F1.10 defect class, fixed for the other half only). Re-merging
  cannot recover it — the `||` destroyed the information — and the only surviving witness is a stored
  `req_body` that is a strict prefix of its `Content-Length`. A separate, `--dry-run`/`--yes`-gated,
  idempotent command (`clens reflag`) rewrites only that flag, reports **three buckets** (flipped /
  already honest / no-witness residual; all-time 2,846 / 241 / 138, IST day 1,176 / 128 / 78),
  synthesises **no** warnings (consequence stated: 59 all-time flip without a warning, 0 on the IST
  day), and needs **no** session re-derivation (`capture_complete` is not folded into `sessions`).
  `carriedOver` moves **19 → 21** (reprice + reflag) and every "N subcommands" comment moves with it.
  §4, §5 (acceptance #3/#4), §7, §8.
- **Acceptance #3/#4 rewritten, not patched** — #3 re-derives *both* baseline and target against the
  union predicate and states it in the step: the IST day's `capture_complete=0` count rises from
  **128** to **1,304** (128 + 1,176) after `clens reflag --yes`. #4 now says the invariant is 0
  **because the backfill ran**, never conflating the code fix (prevents new laundering) with the
  backfill (establishes it for history). The "falls again once the cap is 2 MB" half stays on a newly
  captured day. §5.
  **Superseded by v12:** the *shape* of this rewrite was still wrong — the baseline and target are not
  literals to re-derive but **relations read off the command's own `--dry-run`**, and #3's scope is now
  the command's unfiltered scope. The figures above are the v6-era snapshot (compare the later
  2,865 / 316 / 138): useful history, no longer the definition.
- **Conductor overrides** for F4.1–F4.6 and the new scope were applied verbatim; the record is in
  `review/round-4/triage.md`.

### v5 — round-3 review

- **F3.1 (MAJOR, conductor directive)** — §4's `assets_test.go` row and §5's `Picker` bullet no longer
  promise executed JS tests. `assets_test.go` has no JS runtime and `go.mod` has no JS engine
  ([assets_test.go:409-416](../../internal/web/assets_test.go#L409-L416)), so the Go tests pin only
  three source-shape facts — the picker ids mount, a `toISOString()` source guard, and that `timeWindow()`
  builds semantic local bounds — and the behavioural cases (Dec→Jan rollover, the 28–31-day month
  width, DST) become **manual verification steps in the PR's test plan**, stated as such. §4, §5.
- **F3.2 (MAJOR, conductor directive — blunt rewrite)** — §4's "fails loudly" paragraph is
  **replaced**: a bare local time does 400, but `new Date('2026-09-21').toISOString()` emits
  `2026-09-21T00:00:00.000Z`, Go **accepts** it as UTC, and that is unix **1789948800** against the
  intended **1789929000** — **19,800 s = 5h30m off, silently**, the C-1 defect rebuilt inside its own
  fix. The mechanism is now stated (build `±hh:mm` from `-date.getTimezoneOffset()`, never call
  `toISOString()`) and the RFC3339 table gains the accepted `…000Z` row. §5's round-trip test is
  replaced with a **semantic** assertion — the emitted string must carry the local offset (must not
  end in `Z`) and denote the intended local instant — plus a `toISOString` **source-text guard** in
  `assets_test.go` (regex over the embedded bytes, the `TestAssetsEveryLookupHasAMount` technique),
  with the measured 19,800 s in the test comment. §4, §5.
- **F3.3 (MAJOR)** — §4's Docs table gains a `docs/context/dashboard.md` row: `INDEX.md:40` routes
  "before changing anything in `internal/web`" there and the picker edits four files in that package;
  the row names the per-tab route table, the new picker surfaces, the new `TestAssets*`/`timeWindow()`
  guards, and the re-measured `index.html`/`app.js` line counts. §4.
- **F3.4 (MINOR)** — the `custom` self-contradiction is resolved and the ids are named. Calls has no
  free-text pair (`index.html:54-63` is source/model/billing/Apply), so `custom` reveals nothing there
  and the picker is simply **added** (`c-window-gran`/`c-window-value`); **Stats** keeps
  `s-since`/`s-until` mounted, hidden unless the granularity is `custom` (`s-window-gran`/`s-window-value`
  added beside them). The row now states that `TestAssetsEveryLookupHasAMount` makes a
  removed-but-still-read id a build failure ([app.js:497-498] is the only lookup site). §4.
- **F3.5 (MINOR)** — §4 states the window/bucket composition: `s-granularity`/`s-apply` are unchanged
  and remain the **bucket** axis; the picker supplies only `since`/`until` and must **never** wire its
  select to `granularity=` (which accepts only `day|week|month`, [api.go:329-338]); a bounded window
  bucketed by `day` collapses "By period" to one bucket. §4.
- **F3.6 (MINOR, conductor-verified)** — §2 and the §6 RC-B row are corrected. Warnings are written at
  **insert** ([consumer.go:210-214]); the merge writes **only** `source_mismatch`
  ([merge.go:139-149]); so the shape is **stale warnings on laundered rows**, not suppressed ones.
  §2's "1,274 escaped merging" is backwards: on the IST window **1,254 of 1,274** are merged (1,233
  `proxy,jsonl` + 21 `jsonl,proxy`) and only **20** escaped; all-time **2,860 of 2,918** sit on merged
  `cc=1` rows. §6's "jumps before it falls" is **wrong** — nothing re-runs the analyzer on a merge — and
  the real effect is that the **flag starts agreeing with the warning already attached**. A §8 query
  derives it. §2, §6, §8.
- **F3.7 (MINOR)** — §6's "part to cut" row now says what is **lost** (the ability to ask the dashboard
  for C-1's local day) and states plainly that **no acceptance step depends on the picker**: §5's
  acceptance #1–#4 are direct `events` queries. §6.

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
