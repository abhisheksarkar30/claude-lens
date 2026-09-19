# GI-3 — DeepSeek peak/off-peak pricing and third-party rate support

**Ticket**: GI#3 · **Branch**: `GI-3-deepseek-peak-pricing` · **Base**: `main` · **Plan version**: 6 · **Status**: converged

Problem statement: [issue #3](https://github.com/abhisheksarkar30/claude-lens/issues/3).

---

## 1. Summary

`clens` prices only Anthropic models from a flat per-model rate table. The user
routes Claude Code through `clens` to DeepSeek's Anthropic-compatible endpoint on
pay-as-you-go billing, so ~56,984 captured calls currently store
`cost_source='unpriced'` and contribute $0 to every total. DeepSeek additionally
bills by **time of day** — peak windows cost exactly double off-peak — which the
cost model cannot express at all.

This story adds three things and repairs one:

1. **DeepSeek rates** in the shipped table (off-peak base; peak derived at 2x).
2. **A per-model peak window** so a rate can vary by call time — without touching
   the 11 Claude rows, which are flat-priced.
3. **A configurable holiday exclusion list**, shipping the 2026 Chinese public
   holiday calendar as its default.
4. **A `peak_pricing` warning** so a doubled cost is legible per row rather than
   only inferable from the total.
5. **A repair to the billing-mode routing** for third-party models, and to the
   merge path that would otherwise let the repair write an invariant-5 violation.

### Scope decisions taken by the user (Phase 1 checkpoint)

| Question | Decision |
|---|---|
| Where should DeepSeek cost land? | **Fix the billing column in this story.** DeepSeek-modelled JSONL rows move to the `api` account so cost lands in `cost_usd` (real money), not `api_equivalent_cost_usd` (hypothetical). |
| Holiday exclusion | **Configurable, shipping the 2026 list** as the default value. |

---

## 2. Evidence base

Everything below was reverified against code, not carried from a doc.

| Claim | Verified at |
|---|---|
| `Rate` has 7 rate fields, all `*big.Rat`; `Compute` already takes `at time.Time` and **ignores it** | [pricing.go:24-35](internal/pricing/pricing.go#L24-L35), [pricing.go:59](internal/pricing/pricing.go#L59) |
| Per-class loop with batch halving is the exact template for a multiplier | [pricing.go:75-93](internal/pricing/pricing.go#L75-L93) |
| `Loader.Table()` returns the cached table **by reference** | [pricing.go:346-351](internal/pricing/pricing.go#L346-L351) |
| `Loader.reload()` merges overrides wholesale — an override replaces the whole `Rate` | [pricing.go:315-329](internal/pricing/pricing.go#L315-L329) |
| `perMTok` takes **integer cents**; its own comment claims "at most two decimal places" | [table.go:17-19](internal/pricing/table.go#L17-L19) |
| `rate()` derives write rates as 1.25x / 2x input for every row | [table.go:23-35](internal/pricing/table.go#L23-L35) |
| JSONL tailer fixes `account`/`billingMode` for **every row**, defaulting to `subscription` | [jsonlogs.go:76-84](internal/jsonlogs/jsonlogs.go#L76-L84), [:91-93](internal/jsonlogs/jsonlogs.go#L91-L93) |
| The pricing switch keys off `ev.BillingMode`, so setting it earlier propagates | [jsonlogs.go:338-347](internal/jsonlogs/jsonlogs.go#L338-L347) |
| `mergeEvents` adopts the incoming **cost** columns but keeps `billing_mode` via `preferNonEmpty` | [merge.go:171-173](internal/store/merge.go#L171-L173), [:185-187](internal/store/merge.go#L185-L187) |
| `TestBillingModeInvariants` exercises **only** the insert path — never a merge | [store_test.go:180](internal/store/store_test.go#L180) |
| New config key needs **three** touchpoints; `applyKV`'s `default:` rejects unknown keys | [config.go:148-173](internal/config/config.go#L148-L173) |
| `applyKV` **skips empty values entirely** | [config.go:143-146](internal/config/config.go#L143-L146) |
| `TestMinimumCacheablePrefixCoversShippedModels` requires **every** shipped model to have a minimum | [analyze_test.go:54-63](internal/analyze/analyze_test.go#L54-L63) |
| README kind rows are `| kind | severity | emitted-by |`, severity unquoted | [README.md:188-192](README.md#L188-L192) |
| Only issue #1 exists; **PR #2 consumed number 2** | `gh issue list`, `gh pr list` |

### External rates (re-verified against DeepSeek's pricing page this session)

Off-peak / peak, USD per 1M tokens:

| Model | Cache hit | Cache miss | Output |
|---|---|---|---|
| `deepseek-flash` | 0.003 / 0.006 | 0.15 / 0.30 | 0.60 / 1.20 |
| `deepseek-v4-pro` | 0.022 / 0.044 | 0.66 / 1.32 | 1.98 / 3.96 |

Source for both DeepSeek rows: <https://api-docs.deepseek.com/quick_start/pricing/>,
retrieved **2026-09-19**. The same URL + retrieval date is attached per-row in
`internal/pricing/table.go`, matching the existing `// shared/models.md:73` per-row
anchor style ([table.go:43-53](internal/pricing/table.go#L43-L53)).

- Peak windows: **01:00–04:00** and **06:00–10:00 UTC, Monday–Friday**.
- Chinese public holidays are excluded from peak; weekends and holidays are
  off-peak in full.
- Off-peak is **exactly half** of peak, so `peak = 2 x off-peak`.
- **`deepseek-v4-flash` is a retired alias** that still routes to V4.1-Flash and
  bills at the Flash price — and it appears in the captured data, so it needs its
  own row.

---

## 3. Design

### D1 — Peak is a property of the `Rate`, never global

`clens`'s shipped table mixes flat-priced Anthropic rows with time-varying
DeepSeek rows. An unconditional multiply — which is what `deepseek-lens` GI-4
does, correctly, because *its* table holds only DeepSeek — would **double every
Claude cost**. So the window is optional and per-model:

```go
// PeakWindow describes a model whose rate varies by time of day. A nil
// *PeakWindow on a Rate means the model is flat-priced.
type PeakWindow struct {
    Multiplier   *big.Rat             // peak = off-peak x Multiplier (DeepSeek: 2)
    Hours        [][2]int             // [start,end) UTC hour ranges: {{1,4},{6,10}}
    OffPeakDates map[string]struct{}  // "2006-01-02" UTC dates excluded from peak
}
```

`Rate` gains one field: `Peak *PeakWindow`. `ShippedTable()` leaves it nil for
all 11 Claude rows.

`IsPeak` is a method on `*PeakWindow`:

```go
func (w *PeakWindow) IsPeak(at time.Time) bool
// UTC; false on Sat/Sun; false when the date is in OffPeakDates; else true
// when the hour falls in any [start,end) range.
```

### D2 — The multiplier is applied to the exact per-class cost, before rounding

The table rates are stored **off-peak** (the cheaper base, and the common case).
Peak is applied inside the existing per-class loop, exactly where batch halving
already sits:

```go
cost := new(big.Rat).Mul(big.NewRat(int64(class.tokens), 1), class.rate)
if batch {
    cost.Mul(cost, big.NewRat(1, 2))
}
if peak {
    cost.Mul(cost, r.Peak.Multiplier)
}
total.Add(total, roundHalfUp(cost, centsPerUnit))
```

Two exact rational multiplications, **one** rounding. Multiplying the already-
rounded total instead would drift, and this is the identical discipline
`TestComputeBatchRoundsPerClass` already guards for batch — a peak analogue of
that test is required (T3 below).

`peak` is computed once per call: `r.Peak != nil && r.Peak.IsPeak(at)`.

### D3 — `perMTok` must accept a decimal string (load-bearing refactor)

`perMTok` takes **integer cents** per MTok. DeepSeek's off-peak cache-hit rate is
**$0.003/MTok = 0.3 cents**, and pro's is **$0.022 = 2.2 cents**. Neither is an
integer, so the DeepSeek rates **cannot be represented at all** by the current
constructor — and truncating them to `$0.00` would silently price every cache hit
at zero, which is precisely the "never a wrong number shown as a right one"
failure invariant 5 exists to prevent.

`perMTok` therefore changes to accept a **decimal USD-per-MTok string**, reusing
the parsing `mtokToPerToken` already implements ([pricing.go:152-158](internal/pricing/pricing.go#L152-L158)):

```go
// perMTok converts a USD-per-million-tokens price written as a decimal
// string ("10.00", "0.003") to an exact $-per-token big.Rat. It panics on
// an unparseable literal: every caller passes a shipped-table constant, so
// a bad one is a bug that a test must catch at first call, not a runtime
// condition to degrade on.
func perMTok(usdPerMTok string) *big.Rat
```

The 11 Claude rows and the 2 fast-rate rows are updated mechanically
(`1000` → `"10.00"`, `25` → `"0.25"`, …). This is a readability improvement as
well as a capability one: the literal now reads the way the pricing page does.

`rate()` keeps its 1.25x/2x write-rate derivation for Anthropic rows. DeepSeek
rows instead use a new constructor because **DeepSeek charges no separate
cache-write fee** — the cache-miss price is what populates the cache, and the
captured data confirms it: `cache_write_5m_tokens` and `cache_write_1h_tokens`
are **0 on every one of the 56,984 rows**. Their write rates are therefore `"0"`,
which is the documented value rather than an invention.

Each DeepSeek row carries its **own citation** of the form already used for every
shipped Claude row ([table.go:43-53](internal/pricing/table.go#L43-L53)) — a
trailing comment naming the source. The source for both DeepSeek rows (and the
peak window below) is the pricing page
<https://api-docs.deepseek.com/quick_start/pricing/>, retrieved **2026-09-19**;
the row comment records the URL and that retrieval date. `EffectiveFrom` stays
`shippedEffectiveFrom` (2025-01-01): it is the notional date the binary's table
was compiled, not a per-row fetch time — the field's own doc comment says so
([table.go:8-11](internal/pricing/table.go#L8-L11)) — so the DeepSeek rows do not
get a bespoke 2026 value that would falsely claim the *rate* dates from 2026.

**No path may re-invent a cache-write fee for a DeepSeek row.** A zero-write-rate
model must stay zero through both override surfaces:

- `LoadOverrides` ([pricing.go:219-224](internal/pricing/pricing.go#L219-L224))
  otherwise derives `1.25x`/`2x` input for any override that omits the write
  fields — for a DeepSeek model that would price cache writes at 1.25x input
  instead of `0`.
- `POST /api/prices` builds a fresh `Rate` holding **only** the fields the request
  sent ([api/prices.go:94-114](internal/api/prices.go#L94-L114)), so a client
  posting just `input_rate`/`output_rate` for a DeepSeek model reintroduces the
  Anthropic-shaped write premium.

**Rule (narrowed)**: when the shipped table already has a row for the model **and
that row's own write rates are zero** — the DeepSeek shape this section
established — both `LoadOverrides` and the `POST /api/prices` partial-`Rate`
builder **inherit `CacheWrite5mRate` and `CacheWrite1hRate`** from that shipped row
before the `1.25x`/`2x` derivation runs. Otherwise the derivation stands, so a
partial override on a shipped **Claude** row still prices cache writes at
`1.25x`/`2x` of the *effective* (possibly overridden) input rate — the invariant
`rate()` guarantees at [table.go:21-22](internal/pricing/table.go#L21-L22), and
which [pricing.go:219-224](internal/pricing/pricing.go#L219-L224) depends on.
**The derivation therefore applies whenever there is no shipped row, *or* a shipped
row with non-zero write rates** — the write-rate inheritance above is exactly the
zero-write case and nothing wider (the behavioural statement just above is the
authority).

Inheriting the write rates unconditionally would break that invariant: an override
setting `input_rate = 5.00` on a shipped Claude model would otherwise keep the cache
writes derived from the *old* shipped input, silently below `1.25x` of the rate
actually being charged. The zero-write guard is exactly the case the inheritance is
for, and nothing wider.

**`Peak` is never carried by an override.** `LoadOverrides` does **not** stamp
`Peak`; the shipped `Peak` window is re-taken and the configured list applied in
`reload()`, after the merge — D4's *Ordering* paragraph is the single statement of
where and when.

### D4 — Holiday exclusion is config, shipping the 2026 calendar

Two new config keys, following the three-touchpoint rule:

| Field | Env | Format |
|---|---|---|
| `PeakOffPeakDates []string` | `CLENS_PEAK_OFF_PEAK_DATES` | comma-separated `YYYY-MM-DD` |
| `ApiModelPrefixes []string` | `CLENS_API_MODEL_PREFIXES` | comma-separated model prefixes |

Comma-separated is forced by `parseFlatFile`, which is flat `key = value` with no
list syntax.

**One home for the shipped facts**: `internal/pricing` owns both shipped defaults.
The 33 dates live in `internal/pricing/table.go` as the shipped `PeakWindow`'s
`OffPeakDates`, and `internal/pricing` exports
`ShippedAPIModelPrefixes()` returning `[]string{"deepseek-"}`. Facts about what the
tool ships belong next to the rate table, so **D4's nil-means-default rule is
uniform across both fields** — neither needs an exception sentence. The
2026 Chinese public holiday calendar —

```
2026-01-01..03  2026-02-15..23  2026-04-04..06  2026-05-01..05
2026-06-19..21  2026-09-25..27  2026-10-01..07
```

Source: State Council General Office notice of 2025-11-04. Weekends inside those
spans are already off-peak, so listing the full spans is redundant but harmless —
and it makes the list *exactly the official holiday table*, which is the citable
artifact, rather than a derived subset someone would have to re-derive.

The config fields **do not carry a second copy**. `config.Default()` leaves
`PeakOffPeakDates` and `ApiModelPrefixes` **nil**, and nil means *unset — use the
shipped default* ([config.go:45-59](internal/config/config.go#L45-L59) enumerates
every defaulted field explicitly; these two are deliberately absent from it). The
shipped default for the dates is the 33-date list in `table.go`; for the prefixes it
is `pricing.ShippedAPIModelPrefixes()` = `{"deepseek-"}` (F2.1). **Nil resolves to
the shipped default for both fields — no config edit is required for the DeepSeek
repair to fire**, so §1 and D7 stand as written.

**nil vs. empty is a real distinction, recorded here in D4**:

| field | `nil` (the `config.Default()` value; no key set) | **non-nil, zero-length** (`none`) | non-nil list |
|---|---|---|---|
| `PeakOffPeakDates` | unset → use the shipped 2026 list | exclude **no** dates | exactly those dates |
| `ApiModelPrefixes` | unset → use the shipped `{"deepseek-"}` | **no routing at all** | **replaces** the shipped list wholesale |

A **non-nil** prefix list **replaces** the shipped list; it does not extend it:
`CLENS_API_MODEL_PREFIXES=acme-` drops `deepseek-`, so routing stops firing for
DeepSeek until `deepseek-` is re-listed. The `none` sentinel still yields a non-nil
empty slice, i.e. no routing.

`none` is the only way a *file* can produce the empty slice: `applyKV` skips
`val == ""` entirely ([config.go:143-146](internal/config/config.go#L143-L146)),
so a blank value in the file is a no-op, not an empty list. `NewLoader` therefore
receives the date field as-is and treats `nil` as "use the shipped list" and a
non-nil empty slice as "no exclusions" — the three states (`unset` / `none` /
explicit list) are all expressible.

**Two helpers, three sites — the resolution is never hand-rolled (F3.2).** The
**prefix** field's nil→shipped resolution and the loader construction are each
factored into one helper in `internal/cli`, beside the existing shared
`firstAccount` ([ingest.go:70](internal/cli/ingest.go#L70)):

```go
// resolvedAPIPrefixes: nil means "unset -> use the shipped default".
func resolvedAPIPrefixes(cfg *config.Config) []string {
	if cfg.ApiModelPrefixes == nil {
		return pricing.ShippedAPIModelPrefixes()
	}
	return cfg.ApiModelPrefixes
}

// newPriceLoader is the one loader shape every pricer in the process wants.
func newPriceLoader(cfg *config.Config) *pricing.Loader {
	return pricing.NewLoader(pricing.DefaultPath(), cfg.PeakOffPeakDates)
}
```

**Loader agreement across all three sites; prefix agreement across the two tailer
sites only (F4.2).** All three **loader** sites route through `newPriceLoader(cfg)`:
`serve.go:101` (the consumer's loader), `addCollectors`
([refresh.go:90-95](internal/cli/refresh.go#L90-L95), shared by `serve` and
`refresh`) and `runIngest` ([ingest.go:47-51](internal/cli/ingest.go#L47-L51)).
Because `serve` and `refresh` both reach the tailer loader through `addCollectors`,
routing that site and `serve.go:101` through `newPriceLoader` is what makes F2.4's
"two pricers cannot disagree" **structural** rather than a property a reviewer has
to re-verify by hand.

Only the **two tailer sites** additionally take their model prefixes from
`resolvedAPIPrefixes(cfg)`, via `SetModelBilling(...)` (D5): `addCollectors` and
`runIngest`. `serve.go:101` is the **consumer's** loader
(`cons.SetPriceTable(priceLoader)`, [serve.go:102](internal/cli/serve.go#L102)), and
the consumer resolves account and billing mode from the captured credential —
`resolveAccount(c.accounts, call.AuthKind)`
([consumer.go:310](internal/consumer/consumer.go#L310),
[consumer.go:424-432](internal/consumer/consumer.go#L424-L432)) — **never** from a
model prefix. There is no prefix seam in `internal/consumer` to route into, so
`serve.go:101` takes the loader helper and nothing else: a model-prefix list is
consumed only *behind* a tailer, where `SetModelBilling` lives
([jsonlogs.go:57-60](internal/jsonlogs/jsonlogs.go#L57-L60)).

This is not ordinary deduplication, and it is why it earns its own paragraph. A site
that misses the nil→shipped resolution does not fail loudly: the resolved list is
only ever fed to `strings.HasPrefix` (D5's per-row loop), and `strings.HasPrefix`
over a **nil** slice **never matches** — so an unresolved site silently yields *zero
prefixes, no routing at all*. That is the precise silent-failure shape F2.1 was
accepted to remove, and hand-resolving it at each of three call sites is exactly how
it returns at one of them. The helper makes the resolver the one place that can be
wrong.

**Ordering — the configured list is applied *after* the override merge (F2.3).**
`LoadOverrides` does not touch `Peak`; instead `reload()` applies the resolved
off-peak list as its **final** step, over every row that carries a `Peak` window,
*after* the override merge ([pricing.go:315-329](internal/pricing/pricing.go#L315-L329)).
Because `Peak` is re-taken from the shipped row there rather than carried by the
override, an override row can never clobber the configured dates. One place, applied
uniformly — this also covers the `POST /api/prices` path for free, so
`clens prices --set deepseek-flash` and a custom (or `none`) list compose correctly:
the override sets the base rates, the configured list still decides the holidays.

**The re-taken window is fresh per `reload()`, never a package-level value (F3.1).**
The ordering above leaves one thing unstated, and that omission is the defect: *where
the window it writes lives*. The shipped table is rebuilt on every `ShippedTable()`
call as a map of `Rate` **values** ([table.go:41-65](internal/pricing/table.go#L41-L65)),
so `reload()` can only write the resolved dates through the `Peak` **pointer** a row
carries. If `ShippedTable()` handed back a package-level `*PeakWindow` — the style
`shippedEffectiveFrom` already uses ([table.go:8-11](internal/pricing/table.go#L8-L11)) —
then that write would mutate shared state. `serve` runs **two** pricers in one process
(the consumer's loader at [serve.go:101](internal/cli/serve.go#L101) and the tailer's at
[refresh.go:92](internal/cli/refresh.go#L92), both reached through `addCollectors`), and
both call `Table()`→`reloadIfStale()`→`reload()` when the override file's mtime moves,
so a configured-list application would be a **data race**; and `ShippedTable()` itself
(reachable from `pricing.Compute` at
[pricing.go:48-50](internal/pricing/pricing.go#L48-L50) and from
`TestMinimumCacheablePrefixCoversShippedModels`' iteration,
[analyze_test.go:55](internal/analyze/analyze_test.go#L55)) would report the last
configured list instead of the shipped 33. The plan's own gate is `go test ./...` +
`go vet ./...`, neither of which runs `-race`, so this would ship silently.

The rule, stated once here: **`ShippedTable()` constructs a fresh `*PeakWindow` per
call and never shares one instance across calls or via a package-level `var`**;
`reload()` writes the resolved dates into a **freshly-built window** (or a copy of the
re-taken one), never into the shipped instance. `ShippedAPIModelPrefixes()` likewise
returns a copy, matching `AllKinds()` ([kinds.go:87-91](internal/analyze/kinds.go#L87-L91)),
so no caller can mutate the package's own slice.

**`Config.Validate` checks the date format** and rejects a malformed entry,
because a typo'd date silently over-charges at peak. `Validate` also rejects a
malformed entry in `ApiModelPrefixes` (empty or whitespace-only prefixes).

**`clens doctor` shows both keys, resolved (F2.5).** `doctor`'s stated job is
"print effective config", and it prints a hand-enumerated table that would otherwise
omit the two new fields ([doctor.go:47-60](internal/cli/doctor.go#L47-L60)). Add
`peak_off_peak_dates` and `api_model_prefixes`, each **rendered from the effective
value, not from the raw slice**: `33 (default)` / `none` / `N` for the dates, and
`deepseek- (default)` / `none` / the joined list for the prefixes. Printing
`len(cfg.PeakOffPeakDates)` raw would print `0` in exactly the state where the
33-date default is in force — the state the row exists to show. Configurability is
an explicit user requirement of this story, so a key the "print effective config"
command cannot show is an incomplete feature.

### D5 — Third-party models route to the `api` billing model by prefix

`ApiModelPrefixes` lists the model prefixes that bill pay-as-you-go. The tailer
resolves account/billing mode **per row** instead of once:

```go
account, billingMode := t.account, t.billingMode
for _, p := range t.apiPrefixes {
    if strings.HasPrefix(model, p) {
        account, billingMode = t.apiAccount, t.apiBillingMode
        break
    }
}
```

Because the pricing switch reads `ev.BillingMode`
([jsonlogs.go:341](internal/jsonlogs/jsonlogs.go#L341)), assigning it before the
`store.Event` literal propagates correctly with no change to that switch.

**Wiring (`t.apiAccount` / `t.apiBillingMode`)**: `apiBillingMode` is the literal
`"api"`, and `apiAccount` is `firstAccount(cfg, "api").Name` — the same
`firstAccount` helper `refresh.go`/`ingest.go` already use to set the
**subscription** account ([refresh.go:93-95](internal/cli/refresh.go#L93-L95),
[ingest.go:49-51](internal/cli/ingest.go#L49-L51)). `firstAccount` returns a
`config.Account` **value**, or a **zero `Account`** when no account of that mode is
configured ([ingest.go:70-77](internal/cli/ingest.go#L70-L77)) — so `t.apiAccount` is
`""` and `t.apiBillingMode` stays the literal `"api"`; **`billing_mode` is never the
empty string**. (The `("", mode)` shape is `resolveAccount`'s — the consumer path on
an unmatched credential ([consumer.go:424-432](internal/consumer/consumer.go#L424-L432));
`firstAccount` does not produce it.) The empty `account` is
fine: it is the same value the consumer path already permits, and `billing_mode`
is the column invariant 5 keys off.

`t.apiPrefixes` is the **resolved** list — `resolvedAPIPrefixes(cfg)` (D4), i.e.
`cfg.ApiModelPrefixes` if non-nil, else `pricing.ShippedAPIModelPrefixes()`
(`{"deepseek-"}`) — so an unconfigured install routes DeepSeek by default and
`clens ingest --rebuild` alone repairs the 57k rows with no configuration step (D7).

**The whole tailer construction collapses into one helper (escalation (b)).** After
the prerequisite extract lands (bead 4b), a single
`newTailer(cfg, root, st) *jsonlogs.Tailer` builds the tailer once —
`jsonlogs.New(root, st)` › `SetPriceTable(newPriceLoader(cfg))` › the subscription
step, **moved verbatim with its `acct.Name != ""` guard** (F5.1):

```go
if acct := firstAccount(cfg, "subscription"); acct.Name != "" {
    t.SetAccount(acct.Name, acct.BillingMode)
}
```

› `SetModelBilling(resolvedAPIPrefixes(cfg), firstAccount(cfg, "api").Name, "api")` —
and **both** tailer sites (`addCollectors`, `runIngest`) call it. The guard is
load-bearing, not decorative: `SetAccount` assigns unconditionally
([jsonlogs.go:102-105](internal/jsonlogs/jsonlogs.go#L102-L105)) and `New` seeds
`"subscription"` ([jsonlogs.go:91-93](internal/jsonlogs/jsonlogs.go#L91-L93)), so a
zero `Account` on an install with no subscription account would blank the mode and
mis-bill every row into `cost_usd` (the sharp edge recorded in §8). D5's wiring
therefore lands in **one** place; the two callers keep only their own drive logic
(`resetJSONLCursors` + one `Poll` in `runIngest`; collector registration in
`addCollectors`). The helper takes the **root as an explicit parameter** because
`runIngest`'s local `root` and `addCollectors`' `jsonlRoot()` are distinct
expressions and only one is the settled default — a helper that resolved the root
internally would silently drop ingest's.

This is deliberately **config, not a code rule**. "Which models are third-party
pay-as-you-go" is a fact about the user's setup, not about the model — deriving it
from "has a peak window" or "is in the shipped table" would conflate two
independent concepts and break the first time a Claude model gains time-of-day
pricing or a DeepSeek model loses it.

A new seam replaces the fixed-field `ponytail:` comment's ceiling for the
prefix-matched case only; the comment stays and is amended to say so.

### D6 — The merge path must move `account`/`billing_mode` with the cost columns

**This is the sharpest defect in the story, and it is currently silent.**

`mergeEvents` takes `winner = incoming` when both captures are complete, and the
cost columns move with it ([merge.go:171-173](internal/store/merge.go#L171-L173)).
But `account`/`billing_mode` use `preferNonEmpty(existing, incoming)`
([merge.go:185-187](internal/store/merge.go#L185-L187)). Since
`existing.BillingMode` is **always** non-empty, **a merge can never correct
`billing_mode` — while it does adopt the incoming cost.**

Consequence for this story: `clens ingest --rebuild` is the only re-pricing path
(`insertOrMerge` via `--rebuild`, which zeroes every `jsonl:` cursor so the next
poll re-reads from byte 0; the store has **no** reprice function). After D5, the
incoming DeepSeek row has `billing_mode='api'` and a priced `CostUSD`. The merge
would write that `cost_usd` onto a row still marked `billing_mode='subscription'`
— **exactly the pair invariant 5 forbids** — and
`TestBillingModeInvariants` would not catch it, because it exercises the insert
path and never a merge.

**Fix**: move **`billing_mode` only** onto `winner` — the one column a cross-source
merge is now expected to contradict (the JSONL tailer resolves it by model prefix,
D5). `account` stays on `preferNonEmpty`.

```go
merged.BillingMode = winner.BillingMode   // was preferNonEmpty(existing, incoming)
```

`Account` **stays `preferNonEmpty(existing, incoming)`**. The agreement argument
above is about `billing_mode`; `account` is a separate column the JSONL tailer
structurally cannot supply. On the ordinary ordering the proxy row is written live
(`existing`, the side that carries a real account name) and the JSONL row arrives
minutes later (`incoming`), so moving `account` onto `winner` would **blank the
proxy row's account name** for no upside — a regression `preferNonEmpty` was
protecting. The tailer's own comment says a JSONL line "carries no auth signal"
([jsonlogs.go:76-84](internal/jsonlogs/jsonlogs.go#L76-L84)), which is exactly why
`account` cannot win a merge. `AuthKind` likewise stays `preferNonEmpty`: a JSONL
row has nothing to contribute there either.

**Trade-off, recorded rather than buried**: a cross-source merge where the two
sides disagree now takes the JSONL side's `billing_mode`. The "in practice they
agree" argument holds **only** for a DeepSeek call that *also* passed through the
proxy: it carries an `api_key` credential, so `auth_kind` resolves it to `api` and
both sides already say `api`. The case where the two sides genuinely disagree — a
credential that resolves to `subscription` (i.e. `oauth`/`cloud`, per
`billingModeForAuthKind`,
[consumer.go:442-451](internal/consumer/consumer.go#L442-L451)) on a model whose
prefix says `api` — is **not currently surfaced**. `auth_kind_anomaly` fires on
`AuthKind == "api_key" && BillingMode == "subscription"`
([rules.go:239-249](internal/analyze/rules.go#L239-L249)), which is a *different*
case, and D6 removes the only production path that produced its trigger (a merge
keeping a stale `"subscription"` via `preferNonEmpty` while `auth_kind` came from
the other source). **Recorded as a known gap, not an existing mitigation**: a
subscription-resolving credential on a prefix-`api` model is unsurfaced, and the
prefix rule wins the merged mode.

### D7 — `clens ingest --rebuild` is the backfill

The ~56,984 existing rows are re-priced by running `clens ingest --rebuild`, which
re-reads every transcript from byte 0; the `request_id` UNIQUE constraint absorbs
the re-read as a merge. **D6 must land before D7 is run**, or the backfill is what
manufactures the invariant violation.

This is documentation plus a bead ordering constraint — not new code. There is no
reprice function to write, and adding one would duplicate the path.

### D8 — A per-row `peak_pricing` warning, via an *optional* interface

A call billed at peak cost double the same call off-peak, and nothing in the row
says so. This bead adds the sibling's `peak_pricing` warning so the reason for a
cost variance is legible per row.

Severity **warn**, matching `deepseek-lens` GI-4 and the repo's own definition of
warn ("cost/quality/safety divergence") — a peak call is a cost divergence from
the off-peak baseline.

**The plumbing avoids widening any existing seam.** Returning peak-ness from
`Compute` would add a third return value to `Table.Compute`, `pricing.Compute`,
`Loader.Compute`, and **both** `PriceComputer` interfaces, forcing an update to
every fake in `consumer_test`, `jsonlogs_test`, and the CLI tests. Instead, a
separate *optional* interface is type-asserted at the two call sites:

```go
// PeakComputer is an optional refinement of PriceComputer: a pricer that can
// also report whether a given call fell in a model's peak window. A pricer
// that does not implement it simply yields no peak_pricing warning.
type PeakComputer interface {
    PeakAt(model string, at time.Time) bool
}
```

`Table.PeakAt` and `*Loader.PeakAt` are each three lines (`r.Peak != nil &&
r.Peak.IsPeak(at)`; the Loader delegates to its current table). `PriceComputer` is
**unchanged**, every existing fake still satisfies it, and no test fake needs an
edit unless it wants to exercise the warning.

Both call sites already hold `model` and `StartedAt`, so the assertion and the
attach are local: the consumer's pricing block
([consumer.go:325-336](internal/consumer/consumer.go#L325-L336), where the warnings
slice is built), and **`insert`** in the JSONL tailer
([jsonlogs.go:366-381](internal/jsonlogs/jsonlogs.go#L366-L381)) — **not**
`buildEvent` at `:338`, which returns `(*store.Event, parse.Meta, parse.Usage)` and
has no warnings slice to attach to (F2.7). `SetModelBilling` and the tailer's
`PeakComputer` plumbing are named in §4's `jsonlogs.go` row.

```go
if pc, ok := pricer.(PeakComputer); ok && ev.CostSource != "unpriced" && pc.PeakAt(ev.ModelResolved, ev.StartedAt) {
    // attach peak_pricing
}
```

Two conditions matter: the warning fires **only on a priced row** (an unpriced row
was never billed at any rate, so claiming it was billed at peak would be a lie),
and only once per row — `(event_id, kind)` is already unique, so a re-ingest
upserts rather than duplicates.

**Kind bookkeeping** (all mechanically enforced by `readme_test.go`):
`KindPeakPricing` in [kinds.go](internal/analyze/kinds.go) with description and
severity; added to `nonAnalyzeKinds` as `"consumer, jsonlogs"`, since it is emitted
by the capture path rather than by a pure per-event rule; and a README row whose
emitted-by cell is **exactly** `consumer, jsonlogs`. The test requires the cell to
**equal** whatever `nonAnalyzeKinds` holds ([readme_test.go:67-70](internal/analyze/readme_test.go#L67-L70)) —
the existing values are `"consumer (panic recovery)"` and `"store (cross-source
merge)"` ([kinds.go:100-105](internal/analyze/kinds.go#L100-L105)), so the new cell
follows the *rule*, not those two spellings. The prose line under the table both counts
**and enumerates**, so **the enumeration must grow, not only the numeral (F3.3)**:
"Four of these are not emitted by `analyze` at all — `quota_window_approaching` …
`cost_drift` … `source_mismatch` … and `analyzer_panic` …" becomes "**Five** of
these … — `quota_window_approaching` from `internal/quota`, `cost_drift` from
`internal/reconcile`, `source_mismatch` from the store's cross-source merge,
`analyzer_panic` from the consumer's panic recovery, and `peak_pricing` from the
capture path's own pricers"
([README.md:194-199](README.md#L194-L199)). Changing only the numeral would leave a
five-item claim sitting over a four-item list. **Nothing catches this**:
`readme_test.go` asserts only the kind *table*
([readme_test.go:24-89](internal/analyze/readme_test.go#L24-L89)), not this prose, so
the fix is verified by reading the rendered sentence, never by the test passing.

**Noise, stated plainly**: this fires on roughly half of 57k DeepSeek rows.
`warn` is chosen for sibling parity and because it is genuinely actionable (work
can be shifted off-peak), but if the warning list becomes unusable in practice,
the fix is a session-level aggregate rule — not a severity downgrade that would
hide it.

---

## 4. Files changed

| File | Change |
|---|---|
| `internal/pricing/pricing.go` | `PeakWindow` type + `IsPeak`; `Rate.Peak`; peak multiplier in `Compute`'s per-class loop; `Loader.reload()` **re-takes `Peak` from the shipped row after the override merge** and applies the resolved off-peak list as its **final** step **into a freshly-built window, never mutating a shared one** (F2.3, F3.1); `LoadOverrides`/`POST /api/prices` inherit the two write rates **only when the shipped row's own write rates are zero** (F2.2, D3); `NewLoader` gains an off-peak-dates parameter |
| `internal/pricing/table.go` | `perMTok` → decimal string; `rateExact` for models with no cache-write fee; 3 DeepSeek rows; shipped `PeakWindow` + the 33-date 2026 list and `ShippedAPIModelPrefixes()` → `{"deepseek-"}` (**the single home** for both shipped defaults, F2.1); `ShippedTable()` builds a **fresh `*PeakWindow` per call** — never a package-level `var` in the `shippedEffectiveFrom` style ([table.go:8-11](internal/pricing/table.go#L8-L11)) — and `ShippedAPIModelPrefixes()` returns a **copy** (F3.1); per-row DeepSeek source citation (URL + retrieval date) |
| `internal/pricing/pricing_test.go` | Update for the new constructor; add the peak tests below |
| `internal/config/config.go` | 2 fields (both **`nil` in `Default()`** = "use shipped default"; the prefix default resolves via `pricing.ShippedAPIModelPrefixes()`), 2 `fieldsByEnv` entries, 2 `applyKV` cases, `Validate` date/prefix check, `none` sentinel |
| `internal/config/config_test.go` | New-key parse + reject cases |
| `internal/jsonlogs/jsonlogs.go` | Per-row account/billing resolution by model prefix; `SetModelBilling(prefixes, apiAccount, apiBillingMode)`; the **`PeakComputer`** assertion + `peak_pricing` attach in **`insert`** ([jsonlogs.go:366-381](internal/jsonlogs/jsonlogs.go#L366-L381), the site that owns the warnings slice — not `buildEvent`) (F2.7) |
| `internal/jsonlogs/jsonlogs_test.go` | Prefix routing test; `insert`-path `peak_pricing` test |
| `internal/cli/refresh.go` | Wire off-peak dates + the **resolved** prefixes through `newPriceLoader(cfg)` / `resolvedAPIPrefixes(cfg)` (F3.2) + api-account source; **`addCollectors` builds its tailer through the shared `newTailer(cfg, jsonlRoot(), st)`** (escalation (b)); **call `cfg.Validate()` in `runRefresh`** — before `addCollectors` is reached — since only `runRefresh` returns an error and can refuse a malformed date (F5.2) |
| `internal/cli/ingest.go` | Same wiring through the shared `newPriceLoader`/`resolvedAPIPrefixes` helpers, which **live here beside `firstAccount`** ([ingest.go:70](internal/cli/ingest.go#L70)) so all three sites can reach them (F3.2); **hosts `newTailer(cfg, root, st)`** — the pure-extract helper that collapses the tailer-construction block, **root passed explicitly** so `runIngest`'s `root` and `refresh`'s `jsonlRoot()` stay distinct (escalation (b), **lands before the D5 wiring bead**); **call `cfg.Validate()`** |
| `internal/cli/serve.go`, `internal/cli/prices.go`, `internal/cli/models.go` | `NewLoader` call sites (6 production calls across these 5 files); **`serve.go:101` builds the consumer's loader via `newPriceLoader(cfg)`** — the same helper (and so the same resolved value) `addCollectors` gives the tailer, so two pricers in one process cannot disagree (F2.4, F3.2), though it carries **no** prefix resolution (the consumer resolves from `AuthKind`). The three **display-only** sites — `models.go:34`, `prices.go:50`, `prices.go:94` — pass **`nil`** for the new second argument: the shipped list is what a display-only loader wants, and `runPrices` never loads config at all ([prices.go:26-31](internal/cli/prices.go#L26-L31)), so it has no `cfg` to pass (F4.4) |
| `internal/cli/doctor.go` | Add `peak_off_peak_dates` + `api_model_prefixes` to the config table, each **resolved before printing** (`33 (default)`/`none`/`N`; `deepseek- (default)`/`none`/list) (F2.5) |
| `internal/api/prices.go` | The partial-`Rate` builder inherits write rates **only from a zero-write shipped row** (F2.2). **No `peak_multiplier`** field on the row or on `setPricesRequest` (F2.6 — `Peak` is config-derived, so the field would silently no-op *and* break the GET→POST round-trip, which decodes with `DisallowUnknownFields`) |
| `internal/api/api_test.go`, `internal/api/prices_test.go` | `NewLoader` call-site updates (7 sites); plus a zero-write-inheritance behavioural case on the POST builder — a shipped zero-write row, not the no-shipped-row `claude-custom-1` fixture (T5, F5.3) |
| `internal/store/merge.go` | D6 fix — `BillingMode` moves with `winner` (only) |
| `internal/store/store_test.go` | Extend `TestBillingModeInvariants` to cover the **merge** path |
| `internal/consumer/consumer.go` | `PeakComputer` assertion + `peak_pricing` attach |
| `internal/analyze/kinds.go` | `KindPeakPricing` + `nonAnalyzeKinds` entry |
| `README.md` | `peak_pricing` row in the kind table; the prose "Four of these are not emitted by `analyze`" → "**Five**" **and its enumeration gains the fifth kind** — the sentence counts *and* enumerates, so changing the numeral alone leaves a five-item claim over a four-item list (F2.8, F3.3) |
| `internal/analyze/analyze_test.go` | Explicit third-party exception list for the minimum-prefix coverage test (see R3) |
| `docs/context/*` | Phase 5.6 refresh |

**Interfaces untouched**: `PriceComputer` in both packages (D8 adds an *optional*
interface beside it rather than widening it), `Table.Compute`'s signature,
`store.Event`, the SQLite schema, and every migration. `Compute` already receives
`at`; this story is the first to use it.

---

## 5. Test strategy

| # | Test | Guards |
|---|---|---|
| T1 | `IsPeak` boundary table: 00:59 off / 01:00 peak / 03:59 peak / 04:00 off / 05:59 off / 06:00 peak / 09:59 peak / 10:00 off; Sat + Sun off at a peak hour | D1 window edges |
| T2 | A date in `OffPeakDates` is off-peak at a peak hour; removing it from the list makes the same instant peak | D4 wiring |
| T3 | **Peak analogue of `TestComputeBatchRoundsPerClass`**: a fixture where per-class rounding and total-then-multiply give *different* answers, asserted to differ; **and** the same fixture with `serviceTier="batch"` at a peak instant, so batch halving **and** the peak multiplier compose on one call. The composition is provably order-independent (two exact `big.Rat` multiplies before a single `roundHalfUp`, [pricing.go:88-92](internal/pricing/pricing.go#L88-L92)) — T3 asserts it rather than assuming it. | D2 exactness + batch/peak composition |
| T4 | A Claude model at a peak instant is priced identically to the same call off-peak — peak must not leak to flat rows | D1 |
| T5 | `ShippedTable()` parses end-to-end (every rate non-nil, no panic) and the 3 DeepSeek rows carry a peak window while all 11 Claude rows do not; the DeepSeek write rates are exactly `0`; a **partial override** (`input_rate`/`output_rate` only) on a DeepSeek model **through the Loader** still yields write rates of `0` and a non-nil `Peak`; and a partial override on a shipped **Claude** model (`claude-sonnet-5`) keeps `cache_write_5m_rate == 1.25 x input` and `cache_write_1h_rate == 2 x input` of the *override's* input — the F2.2 narrowing. **Extended to the API surface (F5.3)**: the POST builder at [api/prices.go:94-114](internal/api/prices.go#L94-L114) is a separate implementation in another package, so assert the zero-write inheritance there too — POST only `input_rate`/`output_rate` for a model that **has** a zero-write shipped row, with the handler's Loader wired, and assert the rendered row's `cache_write_5m_rate` is `0` (not `1.25 x` the posted input). The existing fixture ([prices_test.go:84](internal/api/prices_test.go#L84)) posts `claude-custom-1`, which has **no** shipped row, so that branch never executes today; the shipped-Claude control (`claude-sonnet-5`) renders `1.25x`/`2x` of the **posted** input. | D2/D3 + override inheritance (both shapes, Loader and API) |
| T6 | DeepSeek cache-hit rates are exactly $0.003 / $0.022 per MTok — the values the old integer-cent constructor could not express | D3 |
| T7 | JSONL row for `deepseek-flash` → `billing_mode='api'`, priced in `cost_usd`, `ApiEquivalentCostUSD` nil; a `claude-*` row from the same tailer still → `subscription` | D5 |
| T8 | **Merge the two**: insert a `subscription` DeepSeek row, merge a priced `api` DeepSeek row over it, assert the result has `cost_usd` set **and** `billing_mode='api'` **and** `api_equivalent_cost_usd` nil | D6 — the defect that is silent today |
| T9 | Merge where the incoming side is *not* complete leaves `billing_mode` unchanged | D6 regression |
| T9b | Merge where the incoming JSONL side has an **empty** `account` preserves the non-empty proxy `account` (D6 leaves `Account` on `preferNonEmpty`) | D6 `Account` non-regression |
| T10 | Config: both keys parse from **file and env** (there are no flags for them); malformed date rejected by `Validate`; `none` yields a non-nil empty slice for either key while an unset key yields `nil`; and **`resolvedAPIPrefixes(cfg)`** resolves an unset `ApiModelPrefixes` to the shipped `{"deepseek-"}` while a non-nil list (`acme-`) **replaces** it wholesale — asserted on the helper, not on any unreachable wiring site (F2.1, F3.2) | D4 |
| T11 | `peak_pricing` fires on a peak-billed priced row **via the `insert` path** ([jsonlogs.go:366-381](internal/jsonlogs/jsonlogs.go#L366-L381)) and via the consumer path, does **not** fire on the same row off-peak, and does **not** fire on an unpriced row at a peak instant (F2.7) | D8 |
| T12 | `readme_test.go` passes unchanged — proving the new kind's spelling, severity, and emitted-by cell all agree | D8 bookkeeping |
| T13 | A **custom one-date** off-peak list plus an override on `deepseek-flash` still excludes exactly that one date (the override does **not** revert the model to the shipped 33); `none` still yields no exclusions (F2.3 ordering) | D4 ordering |
| T14 | **Assert on the helpers the wiring calls — `resolvedAPIPrefixes`/`SetModelBilling` — not on `Serve()`/`addCollectors`, neither of which a test can reach.** T14 asserts the **prefix wiring**: an unset `ApiModelPrefixes` resolves to the shipped `{"deepseek-"}`, and a tailer wired with `SetModelBilling(resolvedAPIPrefixes(cfg), …)` routes a `deepseek-*` row to `api` while a `claude-*` row stays `subscription`. (Reframed per F4.2: `newPriceLoader(cfg)` is a pure function of `cfg`, so "two `newPriceLoader(cfg)` instances price a call identically" **cannot fail** and evidences nothing about the two tailer sites agreeing — that clause is dropped.) | D5/F2.4 wiring |
| T15 | `clens doctor` prints `33 (default)` and `deepseek- (default)` when both keys are unset, and `none`/`N` (and the joined list) when set (F2.5) | D4 doctor rendering |
| T16 | GET a model row and **POST it back verbatim → 200** (no `400` naming an unknown field); the row carries no `peak_multiplier` (F2.6) | D-API round-trip |
| T17 | **Deterministic freshness guard — must not depend on `-race`.** Build a loader with a **non-default** date list (e.g. exactly one date) and assert (a) `ShippedTable()`'s DeepSeek `Peak.OffPeakDates` is still the shipped 33 dates, and (b) a **second** loader built with a *different* list resolves independently — neither the first loader's resolved window nor the shared-window hazard can pass. `go test ./...` does not enable `-race`, so a race-detector-only guard would fail never (F3.1) | D4 freshness (F3.1) |

All tests must pass with `go test ./...` and `go vet ./...` clean.

---

## 6. Risk areas

| # | Risk | Mitigation |
|---|---|---|
| R1 | **Peak leaks onto Claude rows**, doubling every Anthropic cost | Per-model `*PeakWindow` (D1); T4 asserts a flat row is unchanged at a peak instant |
| R2 | **`perMTok` refactor silently changes a Claude rate** (a mis-typed string literal) | T5 asserts every shipped rate is non-nil; the existing `TestComputeSixClassesIndependently` pins sonnet-5's absolute total and would fail on any drift; the panic makes a typo loud |
| R3 | **Adding DeepSeek rows breaks `TestMinimumCacheablePrefixCoversShippedModels`**, which requires *every* shipped model to have a cache minimum | Resolve with an **explicit exception list**, not a derived predicate. The rule the test guards (`ruleCachePrefixBelowMinimum`) fires only when `meta.HasCacheControl` is set **and** the call had zero cache effect — a `cache_control` breakpoint that cached nothing ([rules.go:92-97](internal/analyze/rules.go#L92-L97)). DeepSeek rows have **no cited cache minimum**, and the repo has no source for one, so the coverage test carries a named exception entry per third-party model (`deepseek-flash`, `deepseek-v4-pro`, `deepseek-v4-flash`) with the reason "no cited cache minimum; the endpoint exposes no cache-write billing". Adding another model to that list must be a visible, reviewable edit — it is a literal in the test, not a silent predicate change. This does **not** widen a gap on its own: `minimumCacheablePrefixFor` already reports not-ok for an unknown model, so the rule declines for DeepSeek today ([rules.go:108-111](internal/analyze/rules.go#L108-L111)); the exception list makes that decline explicit rather than implicit. |
| R4 | **A user override silently drops peak pricing or re-invents a cache-write fee**, because `reload()` replaces the whole `Rate` and `LoadOverrides` builds a fresh one with no `Peak` | `reload()` **re-takes `Peak` from the shipped row after the override merge** and applies the resolved off-peak list last (D4 *Ordering*, F2.3); both override surfaces inherit the two write rates (`CacheWrite5mRate`/`CacheWrite1hRate`) from the shipped row **only when the shipped row's own write rates are zero** (F2.2). Without the re-take, `clens prices --set deepseek-flash` would revert the model to flat pricing; without the write-rate inheritance, a partial `POST /api/prices` would price DeepSeek cache writes at 1.25x/2x input instead of `0`. |
| R5 | **The 2026 list goes stale** and silently over-charges from 2027-01-01 | Config key is the fix; `ponytail:` comment on the shipped list names the ceiling and the failure direction (over-charge, never under-charge, since a missing holiday leaves a day in peak) |
| R6 | **调休 make-up working weekends** (2026-01-04, 02-14, 02-28, 05-09, 09-20, 10-10) are treated as off-peak because the window is weekday-based | Mirrors the sibling deliberately. Whether DeepSeek's billing treats them as weekdays is unverified; recorded as a known gap. The config list is the escape hatch if an invoice disagrees. |
| R7 | Backfill run **before** D6 lands manufactures the invariant violation across 57k rows | Bead ordering is a hard dependency (D7) and T8 is the gate |
| R8 | A malformed holiday date silently shifts cost | `Config.Validate` rejects non-`YYYY-MM-DD` entries. **This guarantee depends on the new explicit `Validate()` calls** added to `runIngest` and `runRefresh` (F1.2): `config.Load` does **not** call `Validate` itself ([config.go:203-238](internal/config/config.go#L203-L238)), so the `clens ingest --rebuild` backfill path D7 uses is validated **only** because `runIngest` calls `Validate` before it touches the store. `runServe`/`runDoctor` already call it; without the two new calls, a typo'd date would slip through on the one path this story backfills with. |

---

## 7. Self-review

### As a senior engineer

- **Is the architecture sound?** The seam already exists: `Compute` takes `at` and
  ignores it. This story is the first caller to use it, so no **interface**
  signature changes and no `Compute`-caller churn — the only constructor that
  changes is `NewLoader`, and that is deliberate: a required off-peak-dates
  argument forces the compiler to enumerate every call site rather than trusting a
  human to remember a `SetOffPeakDates` call at each. That set is **6 production
  calls across 5 files** (`serve.go:101`, `models.go:34`, `ingest.go:48`,
  `prices.go:50`, `prices.go:94`, `refresh.go:92`) **plus 9 call sites in
  `internal/*` tests** (`api_test.go:336`;
  `prices_test.go:17,38,82,114,125,143`;
  `pricing_test.go:199,222`). The compile break in the test files is
  the same deliberate signal, not an accident to route around. **The three
  display-only sites pass `nil` for the new second argument** (`models.go:34`,
  `prices.go:50`, `prices.go:94`): the shipped list is what a display-only loader
  wants, and `runPrices` never loads config at all
  ([prices.go:26-31](internal/cli/prices.go#L26-L31)), so it has no `cfg` to pass
  (F4.4). **Loader agreement** holds across the three production loader sites that
  price a live call (`serve.go:101`, `refresh.go:92`, `ingest.go:48`) — all route
  through `newPriceLoader(cfg)`, so the F2.4 "two pricers cannot disagree" is
  structural rather than inspected. **Prefix agreement** holds across the **two
  tailer sites only** (`refresh.go:92`/`ingest.go:48`, reached via
  `addCollectors`/`runIngest`): the consumer behind `serve.go:101` resolves from
  `AuthKind` ([consumer.go:310](internal/consumer/consumer.go#L310)) and has no
  model-prefix seam, so it takes the loader helper and nothing else (F4.2).
- **Simpler approaches?** A global `peak` flag was rejected because the shipped
  table legitimately mixes both kinds. Applying the multiplier to the total
  instead of per class was rejected — it breaks the repo's exact-money discipline
  and T3 is written specifically to fail if someone makes that simplification.
- **The one genuinely refactor-shaped change** is `perMTok`. It is not optional:
  the DeepSeek rates are unrepresentable without it. It is also the highest-risk
  edit in the story, which is why R2 pairs the panic with two existing absolute-
  total tests as backstops.
- **Is D6 correct?** It is the minimal change that keeps the merged `billing_mode`
  and `cost_usd` consistent (invariant 5) — moving **only** the column a merge is
  expected to contradict, and leaving `account` on `preferNonEmpty` so a live
  proxy row is not blanked. The alternative — leaving `preferNonEmpty` and instead
  suppressing the incoming cost when modes disagree — is worse: it would leave 57k
  rows unpriced forever to protect a column from a value that is simply wrong.

### As a QA engineer

- The boundary table (T1) is the highest-value test: off-by-one on an hour range
  is the single most likely bug and produces plausible-looking costs.
- T3 is the money test — it is written so that a *wrong* implementation passes a
  naive equality check, and only the explicitly-asserted inequality catches it.
  This mirrors how `TestComputeBatchRoundsPerClass` guards the identical class of
  bug.
- T7/T8 cover the failure that no existing test can see. T8 is the regression
  that would have caught this story's own defect had it shipped without D6.
- T9 guards the D6 change against over-reach: an incomplete incoming capture must
  not be allowed to rewrite billing mode.
- Error scenarios: malformed date (T10), unknown model still unpriced, empty
  holiday list via `none`.
- **Gap accepted**: no E2E test runs `--rebuild` against a real 57k-row store.
  The merge path is covered by T8 at the unit level, which is where the logic
  lives.

### As a security engineer

- **No new trust boundary.** The rates are compiled-in constants; the two new
  config keys are read from a local, user-owned file and environment. Nothing new
  is parsed from the network.
- **No injection surface.** The holiday list is parsed into `time.Time` and used
  for map lookups — never interpolated into SQL, a shell, or a file path. The
  model-prefix list is used only for `strings.HasPrefix`. Both are validated by
  `Config.Validate`.
- **No secret exposure.** No credential, header, or body is touched; this story
  changes only arithmetic and routing. `req_body`/`resp_body` handling is
  untouched.
- **Fail-open is preserved.** A malformed rate table panics at construction (a
  build/test failure), never mid-request. A malformed config value is rejected by
  `Config.Validate`, which `Load` does **not** call itself
  ([config.go:203-238](internal/config/config.go#L203-L238)) — each command calls
  it explicitly (`runServe`, `runDoctor`, and F1.2's new `runIngest`/`runRefresh`
  calls) before it opens the store or binds the proxy, so a bad date cannot
  degrade a live session.
- **No data loss or migration.** No schema change, no destructive operation. The
  backfill is a re-read; the UNIQUE constraint makes it idempotent.
- **Money-correctness is the security-adjacent property here**: the whole risk of
  this change is *displaying a confidently wrong cost*. That is why the unpriced
  path returns NULL rather than 0, and why D2 rounds once per class.

---

## 8. Out of scope (deliberately deferred)

| Item | Why |
|---|---|
| A **session-level** peak aggregate ("X% of this session ran at peak") | The per-row warning (D8) covers attribution. An aggregate is a dashboard concern and would need a query, not a rule. If D8's warning volume proves unmanageable, this is the replacement — not a severity downgrade. |
| Making `peak_multiplier` settable via `clens prices --set` | The base (off-peak) rates are settable, which covers a DeepSeek price change. A multiplier edit is a rarer need. |
| A `peak` / `off_peak` column on `events` | Needs a schema migration and a decision about whether the split is by row or by aggregate. The cost is already correct without it. |
| Non-UTC timezone handling for the peak window | DeepSeek publishes UTC windows; the config list is UTC dates. Local-time rendering is a dashboard concern. |
| Tightening `jsonlogs.SetAccount` against an empty `billingMode` | **Recorded sharp edge, not a fix (F5.1).** `SetAccount` accepts an empty `billingMode` and assigns it unconditionally ([jsonlogs.go:102-105](internal/jsonlogs/jsonlogs.go#L102-L105)), and the routing switch reads an empty mode as *api*, not as "unset" ([jsonlogs.go:341-346](internal/jsonlogs/jsonlogs.go#L341-L346)) — so the `acct.Name != ""` guard at every call site ([ingest.go:49-51](internal/cli/ingest.go#L49-L51), [refresh.go:93-95](internal/cli/refresh.go#L93-L95), and bead 4b's extracted block) is **load-bearing**, and any future caller that omits it silently mis-bills subscription rows into `cost_usd`. Changing `SetAccount`'s semantics is out of scope: the guard already protects both existing callers, and altering it could reach the proxy path. |

---

## 9. Bead sketch (Phase 3 formalises this)

Ordering is load-bearing: **the merge fix (D6) must land before the backfill (D7)**.

1. `perMTok` → decimal string; `rateExact`; mechanical Claude-row update — *foundation, no behaviour change*
2. `PeakWindow` + `IsPeak` + `Rate.Peak` + peak in `Compute` + `reload()` re-take of `Peak` and the resolved-list final step **into a freshly-built window** (T17's freshness guard) + zero-write-guarded write-rate inheritance in `LoadOverrides`/API (D1, D2, R4, F2.2/F2.3/F3.1)
3. DeepSeek shipped rows + the 2026 holiday list and `ShippedAPIModelPrefixes()` (single home in `table.go`, **fresh window per `ShippedTable()` call**, **copy** on return) + per-row citation + `NewLoader` signature + the 9 `internal/*` test call sites (D3, D4, F2.1/F3.1, F5.4)
4. Config keys (nil = shipped default), `Validate` (date + prefix), `none` sentinel, the new `Validate()` calls in `ingest`/`refresh`, **`newPriceLoader`/`resolvedAPIPrefixes` helpers (homed beside `firstAccount`) with all three sites — `serve.go:101`, `addCollectors`, `runIngest` — routed through them**, `doctor` table rows (resolved rendering) (D4, F2.4, F2.5, F3.2)
4b. **`newTailer` — pure extract (escalation (b)).** Collapse the tailer-construction block — `jsonlogs.New(root, st)`, `SetPriceTable(newPriceLoader(cfg))`, and the subscription step **moved verbatim, guard included**:

    ```go
    if acct := firstAccount(cfg, "subscription"); acct.Name != "" {
        t.SetAccount(acct.Name, acct.BillingMode)
    }
    ```

    — into one helper, homed beside the D4 helpers, **taking the root as an explicit parameter** — `newTailer(cfg, root, st)` — because `runIngest`'s `root` and `addCollectors`' `jsonlRoot()` are distinct expressions and only one is the settled default; a helper that resolved the root internally would silently drop ingest's. **`SetModelBilling` is out of this bead's scope — it joins in bead 5.** The two callers keep their own drive logic (`resetJSONLCursors` + one `Poll` in `runIngest`; collector registration in `addCollectors`) — **only construction collapses**. **Behaviour-preserving: the diff shows only the move, guard included** (F5.1 — the `acct.Name != ""` guard is part of the moved block, so the extraction cannot silently re-route billing). Lands **before** bead 5 so that bead's diff shows only the semantic change and stays bisectable. (F4.2 standing-watch — escalation resolved as option (b))
5. JSONL per-row billing routing + api-account wiring (D5) — the `SetModelBilling(resolvedAPIPrefixes(cfg), …)` attach joins `newTailer` (bead 4b), so the wiring lands in one place rather than mirrored per site
6. `mergeEvents` `BillingMode` move + merge-path invariant test (D6, T8/T9/T9b)
7. API/CLI surface + `analyze` test scoping (R3) + backfill documentation (D7)
8. `peak_pricing` warning: optional `PeakComputer` interface, both call sites (consumer + JSONL **`insert`**), kind + README row + the "Four of these" → "**Five**" prose **with the enumeration grown to five** (D8, T11/T12, F2.7/F2.8/F3.3)

---

## Change History

### v2 — round-1 review (2026-09-19)

Findings F1.1–F1.11 from `review/round-1/critique.md`, all applied.

- **F1.1** — §4 now lists the two `internal/api` test files (`api_test.go`,
  `prices_test.go`) and §7 states the true call-site count (6 production calls
  across 5 files + 7 test call sites), replacing the incorrect "5 call sites".
- **F1.2** — new `cfg.Validate()` calls added to `runIngest` and `runRefresh`;
  R8 and §7 now state the guarantee depends on them (`Load` does not validate).
- **F1.3** — the 33 dates get one home (`pricing/table.go`); `config.Default()`
  leaves both list fields `nil` ("use shipped default"); D4 documents the
  nil-vs-empty distinction and why `none` is the only file-producible empty list.
- **F1.4** — D6 restricted to moving `BillingMode` only; `Account` stays
  `preferNonEmpty`; D5 states `t.apiAccount`'s source (`firstAccount(cfg,"api")`,
  fallback `("", "api")`).
- **F1.5** — D6 trade-off corrected: `auth_kind_anomaly` fires on
  `api_key + subscription`, not the case D6 names; the disagreeing-credential
  case is recorded as an unsurfaced known gap.
- **F1.6** — T10 no longer claims flags; config parses from file and env only.
- **F1.7** — `internal/cli/doctor.go` row added; the two keys appear in its
  config table.
- **F1.8** — R3 restated: an explicit, reviewable third-party exception list
  replaces the derived predicate; the doubtful `cache_control` premise is
  dropped in favour of the rule's real precondition (`meta.HasCacheControl` +
  zero cache effect).
- **F1.9** — override paths (`LoadOverrides`, `POST /api/prices`) inherit
  `CacheWrite5mRate`/`CacheWrite1hRate`/`Peak` from a shipped row before deriving;
  T5 extended.
- **F1.10** — T3 now composes batch + peak on one call and asserts it.
- **F1.11** — DeepSeek rows cite the pricing URL + retrieval date (2026-09-19),
  matching the per-row anchor style; `EffectiveFrom` stays `shippedEffectiveFrom`.

§2's "PR #2 consumed number 2" was independently verified correct and is
unchanged.

### v3 — round-2 review (2026-09-19)

Findings F2.1–F2.8 from `review/round-2/critique.md`. F2.1–F2.3 narrow three
round-1 conductor overrides that were broader than the defect they fixed, or left
an ordering unstated: the overrides were applied correctly in round 1, and these
edits narrow them — no round-1 error is implied.

- **F2.1** — `internal/pricing` exports `ShippedAPIModelPrefixes()` →
  `[]string{"deepseek-"}`; `config.Default()` leaves `ApiModelPrefixes` **nil**
  = "use the shipped default", so D4's nil-means-default rule is uniform across
  both fields. D4 states a non-nil list **replaces** the shipped list wholesale and
  `none` still yields the non-nil empty slice = no routing. §1 and D7 are unchanged
  — `clens ingest --rebuild` alone repairs the 57k rows.
- **F2.2** — the write-rate inheritance is narrowed to **zero-write shipped rows**
  (the DeepSeek shape); otherwise the `1.25x`/`2x` derivation stands, preserving
  `table.go:21-22`'s invariant. T5 extended with a shipped-Claude partial-override
  case.
- **F2.3** — `LoadOverrides` no longer stamps `Peak`; `reload()` re-takes it from
  the shipped row after the merge and applies the resolved off-peak list as its
  **final** step. D4 states the ordering; T13 covers it.
- **F2.4** — §4 names `serve.go:101` as a **wiring** site that passes
  `cfg.PeakOffPeakDates` to the consumer's loader; T14 asserts the two loaders
  agree.
- **F2.5** — `doctor` renders the **resolved** value (`33 (default)`/`none`/`N`;
  `deepseek- (default)`/`none`/list); T15 covers it.
- **F2.6** — `peak_multiplier` is **dropped** from the GET row and is not added to
  `setPricesRequest`; `Peak` stays visible via `doctor`. T16 covers the round-trip.
- **F2.7** — D8 and §4 point the JSONL attach at **`insert`**
  (`jsonlogs.go:366-381`), name the `PeakComputer` interface in §4's row, and T11
  states it covers the `insert` path.
- **F2.8** — the `analyzer_panic`/`source_mismatch` "precedent" wording is dropped
  (those cells are `"consumer (panic recovery)"` and `"store (cross-source
  merge)"`); README's "Four of these are not emitted by `analyze`" → **five**.

### v4 — round-3 review (2026-09-19)

Findings F3.1–F3.3 from `review/round-3/critique.md`. All three sit in the **seams
the v3 edits created**, not in the original design; each was approved by the human
and applied under a binding conductor override. All 3 JUSTIFIED; no round-1 or
round-2 error is implied.

- **F3.1** — D4's *Ordering* now states the re-taken `PeakWindow` is **fresh per
  `reload()` / per loader**, never a package-level `var` in the
  `shippedEffectiveFrom` style ([table.go:8-11](internal/pricing/table.go#L8-L11)).
  `ShippedTable()` builds a fresh `*PeakWindow` per call; `reload()` writes the
  resolved dates into a freshly-built window; `ShippedAPIModelPrefixes()` returns a
  **copy**. T17 added: a **deterministic** guard (a loader with a non-default list
  leaves `ShippedTable()`'s 33 dates unchanged, and a second loader resolves
  independently) — deliberately **not** `-race`-dependent, since `go test ./...`
  does not enable the race detector. §4 `table.go`/`pricing.go` rows and §9 beads
  2/3 updated.
- **F3.2** — the nil→shipped resolution and loader construction are factored into
  `resolvedAPIPrefixes(cfg)` and `newPriceLoader(cfg)` in `internal/cli`, homed
  beside `firstAccount` ([ingest.go:70](internal/cli/ingest.go#L70)); **all three**
  sites (`serve.go:101`, `addCollectors`, `runIngest`) route through them. D4 states
  the *why*: `strings.HasPrefix` over a **nil** slice never matches, so a site that
  misses resolution silently yields zero routing — the F2.1 failure shape. T10 and
  T14 now assert on the **helper**, since no test can reach `Serve()`/`addCollectors`.
  §4 `refresh.go`/`ingest.go`/`serve.go` rows and §9 bead 4 updated.
- **F3.3** — D8 and §4's `README.md` row now require the prose **enumeration** to
  gain the fifth kind, not just the numeral, since
  [README.md:194-199](README.md#L194-L199) both counts and enumerates. The fix is
  verified by reading the rendered prose — `readme_test.go` asserts only the table
  ([readme_test.go:24-89](internal/analyze/readme_test.go#L24-L89)). §9 bead 8
  updated.

### v5 — round-4 review (2026-09-19)

Findings F4.1–F4.4 from `review/round-4/critique.md`, all approved by the human and
applied under binding conductor overrides; plus the round-3 loop rule's escalation,
**resolved as option (b)**. All 4 findings JUSTIFIED; no round-1/2/3 error is
implied — the round-4 reviewer verified every round-3 fix resolved, as written.

- **F4.1** — D3's contradictory closing sentence ("Only a model with **no** shipped
  row may derive") replaced with the correct predicate: **a missing shipped row, *or*
  a shipped row with non-zero write rates, takes the `1.25x`/`2x` derivation**; the
  behavioural statement that a partial override on a shipped Claude row still
  derives is kept as the authority. The old sentence was a stale leftover of the
  pre-narrowing rule and would have suppressed the derivation for every shipped
  Claude override.
- **F4.2** — D4 and §7 stop claiming prefix resolution at `serve.go:101`: **loader**
  agreement is stated across all three sites (`serve.go:101`, `addCollectors`,
  `runIngest` all call `newPriceLoader(cfg)`), **prefix** agreement across the **two
  tailer sites only** (`SetModelBilling`). `serve.go:101` is the consumer's loader
  and the consumer resolves from `AuthKind`
  ([consumer.go:310](internal/consumer/consumer.go#L310)), so there is no
  model-prefix seam there to route into. **T14's first clause dropped**: "two
  `newPriceLoader(cfg)` instances agree" is a pure function and cannot fail, so it
  evidenced nothing — T14 now asserts the prefix wiring (resolver → `SetModelBilling`
  → per-row routing), the part that actually carries risk.
- **F4.3** — D5's stated *reason* corrected (the outcome was right): `firstAccount`
  returns a `config.Account` value — a **zero `Account`** on no match
  ([ingest.go:70-77](internal/cli/ingest.go#L70-L77)) — not `("", mode)`; that shape
  is `resolveAccount`'s ([consumer.go:424-432](internal/consumer/consumer.go#L424-L432)).
- **F4.4** — §4 and §7 now name what the three display-only `NewLoader` sites pass
  for the new second argument: **`nil`** (`models.go:34`, `prices.go:50`,
  `prices.go:94`). `runPrices` never loads config
  ([prices.go:26-31](internal/cli/prices.go#L26-L31)), so it has no `cfg` — not
  merely an inconvenient one.
- **Escalation (b) — `newTailer` bead.** The round-4 reviewer withheld the `internal/cli`
  wiring finding rather than filing a fifth per-round patch, per the conductor's loop
  rule. Resolved as **option (b)**: authorise `newTailer(cfg, root, st)` as its own
  **prerequisite** bead (**4b** in §9), landing **before** the D5 wiring bead. It is a
  **pure extract** — behaviour-preserving, the diff shows only the move — taking the
  **root as an explicit parameter** (so ingest's `root` and refresh's `jsonlRoot()`
  stay distinct); the two callers keep their own drive logic; only construction
  collapses. D5's semantic change then lands in one place. §4's `refresh.go`/`ingest.go`
  rows and D5 updated.

### v6 — round-5 review (2026-09-19)

Findings F5.1–F5.4 from `review/round-5/critique.md`, all approved by the human and
applied under binding conductor overrides. All 4 findings JUSTIFIED; no round-1/2/3/4
error is implied — the round-5 reviewer re-verified every anchor the round-4 fixes
rest on and confirmed them as written.

- **F5.1** — D5's `newTailer` chain and §9 bead 4b now **spell out the subscription
  step as the literal guarded block** (`if acct := firstAccount(cfg, "subscription");
  acct.Name != "" { t.SetAccount(acct.Name, acct.BillingMode) }`), so bead 4b's diff
  shows a **move, not a rewrite**. The guard is load-bearing: `SetAccount` assigns
  unconditionally and `New` seeds `"subscription"`, so a zero `Account` would blank
  the mode and mis-bill into `cost_usd`. **`SetModelBilling` stays out of bead 4b** —
  it joins in bead 5. Separately, §8 gains **one line** recording the underlying sharp
  edge (an empty `billingMode` reads as *api* at `jsonlogs.go:341-346`), so the guard's
  load-bearingness has a home; `SetAccount`'s semantics are **not** changed here.
- **F5.2** — §4's `refresh.go` row now names **`runRefresh`** as the `cfg.Validate()`
  site, matching R8. `addCollectors` has no error return ([refresh.go:90](internal/cli/refresh.go#L90)),
  so a `Validate()` there could only be discarded — the parenthetical claiming it was
  the shared site is dropped.
- **F5.3** — T5 is **extended to the API surface**: the POST builder
  ([api/prices.go:94-114](internal/api/prices.go#L94-L114)) is a separate implementation
  in another package, and the existing fixture posts `claude-custom-1`
  ([prices_test.go:84](internal/api/prices_test.go#L84)), a model with no shipped row,
  so the zero-write branch never executes. T5 now asserts the zero-write inheritance for
  a **shipped** zero-write row via POST, plus the shipped-Claude control. §4's api test
  row names the new behavioural case.
- **F5.4** — §7's call-site count corrected from "plus 7 call sites in `internal/api`'s
  tests" to "plus **9** call sites in `internal/*` tests" — `pricing_test.go:199,222`
  were missing from the enumeration. §9 bead 3's stale echo of "7" corrected to "9".

### v6 — round-6 review: converged (2026-09-19)

Round 6 returned `VERDICT: NO_FURTHER_FINDINGS` — **0 findings**
(`review/round-6/critique.md`). The cross-review loop closed after **6 rounds**, and
the design is **unchanged from v6**: no round-6 finding existed to apply, so no edit
was made to any section of the plan and no version bump was taken. The header now
carries **`Status: converged`** to record that the plan is final and ready for
beadification.
