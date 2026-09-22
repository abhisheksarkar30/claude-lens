# GI-11 — Cost is rounded to a cent per call, and a truncated capture is recorded as complete

**Issue**: GI#11 (GitHub) · **Branch**: `GI-11-cost-and-capture-fidelity` · **Beads**: `.beads/GI-11/`

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
the rounding destroy money. There is no downstream dependency on cent granularity: the CLI already
formats `$%.4f` ([format.go:78](../../internal/cli/format.go#L78)) and the dashboard already has a
sub-cent branch (`n < 0.01 ? toFixed(6) : toFixed(2)`, [app.js:28](../../internal/web/app.js#L28)).

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
`!respBuf.truncated` alone, and the merge undoes the fix. Measured across all 4,921 proxy rows,
cross-tabulated against the client's own `Content-Length` header:

| `source_refs` | `capture_complete` | rows | truly truncated |
|---|---|---|---|
| `proxy,jsonl` | 1 | 3,405 | **2,613** |
| proxy-only | 1 | 1,289 | 0 |
| proxy-only | 0 | 237 | 230 |

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

- The proxy captured 100% of traffic — 8,129 client requests − 144 `count_tokens` − 401 responses
  = 2,257 = exactly the proxy's `/v1/messages` row count.
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
| [internal/store/merge.go](../../internal/store/merge.go) | `CaptureComplete` follows the body: the surviving row reports the flag of whichever side supplied the bodies it holds, not the `||` of both ([:241](../../internal/store/merge.go#L241)). Rewrite the comment at [:176-183](../../internal/store/merge.go#L176-L183), whose "the record that is whole is the safer one to quote" argument the `||` contradicts. |
| [internal/store/merge_test.go](../../internal/store/merge_test.go) | Four cases: proxy-first with the proxy truncated (flag stays false), proxy-first with the proxy whole, rekey collision where the JSONL taker takes the proxy's bodies (flag follows to false), and `--body-policy off` (no bodies, flag stays true). |
| [internal/config/config.go](../../internal/config/config.go) | `BodyCapBytes` default 262,144 → **2,097,152** ([:59](../../internal/config/config.go#L59)); update the doc comment at [:319](../../internal/config/config.go#L319) and the flag help ([:222](../../internal/config/config.go#L222)). |
| [internal/config/config_test.go](../../internal/config/config_test.go) | Pin the new default, and that an explicit `CLENS_BODY_CAP_BYTES` / `--body-cap-bytes` still overrides it. |
| [internal/cli/reprice.go](../../internal/cli/reprice.go) *(new)* | `clens reprice` — recompute `cost_usd` / `api_equivalent_cost_usd` / `cost_source` for stored rows from the current table. Mirrors [purge.go](../../internal/cli/purge.go): `--dry-run` prints what `--yes` would change, and nothing happens without `--yes`. Scoped to `cost_source IN ('shipped','provisional')` — exactly the bundled table's own output, i.e. the rows RC-A corrupted — and reports `user` / `approximate:` / `unpriced` counts as skipped rather than rewriting a user override or inventing a label it cannot reconstruct (`TTLUnknown` is not a stored column, so `approximate:cache_ttl_unknown` is not derivable from a row; leave those rows alone). |
| [cmd/clens/main.go](../../cmd/clens/main.go) | Register `"reprice": cli.Reprice` in the command table. |

### Why a reprice command is in scope

RC-A leaves every stored cost wrong. Without a reprice path, the fix is invisible for all
historical data — including the 2026-09-21 view that produced this report — and the user's next
action would be a manual `sqlite3` session. It is bounded (the pricing package is pure, and every
input `Compute` needs — model, the five token columns, `speed`, `service_tier`, `started_at`,
`billing_mode` — is already a column on the row), it is idempotent by construction (recomputing
yields the same answer), and it needs no schema change and no marker column.

### Docs

| File | Change |
|---|---|
| [docs/context/cost-and-quota.md](../../docs/context/cost-and-quota.md) | §"Exact money, rounded per token class" ([:72-81](../../docs/context/cost-and-quota.md#L72-L81)) documents the rounding as an invariant and links the two tests by name; rewrite it to the round-at-display rule. Update the `Compute` description at [:97-100](../../docs/context/cost-and-quota.md#L97-L100). |
| [docs/context/storage-schema.md](../../docs/context/storage-schema.md) | The `capture_complete` paragraph ([:95-103](../../docs/context/storage-schema.md#L95-L103)) is **stale against the code**: it claims the flag also covers "ended without a `message_stop` event", but [proxy.go:107](../../internal/proxy/proxy.go#L107) computes it from the two buffers' `truncated` bits alone. Fix that, and state the merge rule. |
| [docs/context/build-and-run.md](../../docs/context/build-and-run.md) | The body-cap default and its storage trade-off. |
| [README.md](../../README.md) | Any 256 KB / `body-cap` mention, and the `clens` command list for `reprice`. |

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
- Merge: the four `CaptureComplete` cases above, plus an assertion that the merged row's bodies
  and its flag agree — the invariant the `||` broke. `TestBillingModeInvariants` and the
  `source_mismatch` tests must stay green, since RC-B touches the same function.
- Config: new default + override still wins.
- Reprice: a store fixture with a known-wrong row reprices to the exact value; `--dry-run`
  changes nothing; a `user` row is skipped and counted.

**Integration.**

- The proxy cap test at `bodyCap = 64` ([proxy_test.go:462](../../internal/proxy/proxy_test.go#L462))
  stays valid — it is cap-relative, not cap-specific. Add a case at the *real* default asserting a
  1.2 MB request body is captured whole and flagged complete, which is the end-to-end form of RC-C.
- The TTFB gate must stay green: the cap change is a buffer size, and a larger buffer is
  marginally more work on the hot path, so this is exactly the invariant to re-assert rather than
  assume.

**Acceptance — against a copy of the live store, per this repo's convention** (GI-7 and GI-9 both
ran against copies):

1. `SELECT SUM(cost_usd) FROM events WHERE source='proxy' AND day = 2026-09-21` moves from
   **0.82** to **3.46 ± 0.05**, i.e. within 7% of DeepSeek's 3.25.
2. `capture_complete=0` count on that day rises from 233 to ~2,800 (honest truncation) *before*
   the cap raise, and falls again once the cap is 2 MB and the day is re-captured — the two fixes
   are only correct together.
3. Zero rows satisfy `length(req_body) = cap AND capture_complete = 1` (the RC-B invariant, as a
   query).

## 6. Risks and edge cases

| Risk | Handling |
|---|---|
| **RC-B alone makes the dashboard look worse** — 2,613 rows stop claiming completeness, so `stream_incomplete` jumps before it falls. | RC-B and RC-C are ordered in the same story and shipped together; the warning count is honest at every point, and the cap raise is what actually removes the truncation. |
| **float64 accumulation** across 2,257 rows in SQLite `REAL`. | Error is ~1e-16 relative per operation; the exact-$3.46 target is asserted to ±$0.05, four orders of magnitude above the noise. `big.Rat` stays the compute type; only the stored value is a float, as today. |
| **`roundHalfUp` deletion** breaks a caller I have not found. | Grepped: its only production reference is [pricing.go:133](../../internal/pricing/pricing.go#L133); the rest are comments. The compiler is the check. |
| **Reprice rewrites a cost the user deliberately overrode.** | Scoped to `shipped`/`provisional` only; `user` rows are counted and skipped. |
| **Reprice cannot reconstruct `approximate:cache_ttl_unknown`.** | `TTLUnknown` is not a stored column. Those rows are skipped, not relabelled. |
| **Cap raise and storage.** | 2 MB bound, ~0.45 GB projected growth, moved to `D:`. `BodyCapBytes` stays configurable so it can be lowered without a rebuild. |
| **A larger cap changes merge precedence.** | The winner rule reads `CaptureComplete`; with RC-B the flag becomes trustworthy, so fewer proxy rows lose the pick to the transcript. Intended, and covered by the merge tests. |
| **Historical rows.** | Only `reprice` fixes them, and only for the rows in its scope. A day whose bodies are already truncated cannot be re-captured — RC-C is forward-only. This must be said plainly in the plan and the PR. |

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
either blank it or thrash it; the rule must be "the flag of the side that supplied the bodies,
defaulting to the existing flag when neither side has one". Error paths: a reprice with
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

```sql
-- RC-A: the stored total, and the exact total at the same rates/peak window
SELECT SUM(cost_usd) FROM events
 WHERE source='proxy' AND started_at BETWEEN <day_start> AND <day_end>;
-- → 0.82 ; the exact recomputation gives 3.46

-- RC-A: rows stored at zero while carrying cache reads
SELECT COUNT(*), SUM(cache_read_tokens) FROM events
 WHERE source='proxy' AND cost_usd = 0 AND started_at BETWEEN <day_start> AND <day_end>;
-- → 2352 rows, 196.68M cache-read tokens

-- RC-B: the flag is laundered only by merges
WITH x AS (
  SELECT source_refs, capture_complete AS cc, length(req_body) AS stored,
         CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) AS true_len
    FROM events WHERE source='proxy')
SELECT source_refs, cc, COUNT(*),
       SUM(CASE WHEN true_len > stored THEN 1 ELSE 0 END) AS trunc
  FROM x GROUP BY source_refs, cc;
-- → proxy,jsonl / cc=1 / 3405 rows / 2613 truncated
--   proxy-only / cc=1 / 1289 rows /    0 truncated
--   proxy-only / cc=0 /  237 rows /  230 truncated

-- RC-C: how far past the cap real bodies go
WITH x AS (
  SELECT CAST(json_extract(req_headers,'$."Content-Length"[0]') AS INTEGER) AS n
    FROM events WHERE source='proxy')
SELECT SUM(n > 262144), SUM(n > 1048576), SUM(n > 2097152), MAX(n), CAST(AVG(n) AS INT)
  FROM x WHERE n IS NOT NULL;
-- → 2845, 106, 0, 1246222, 356659
```
