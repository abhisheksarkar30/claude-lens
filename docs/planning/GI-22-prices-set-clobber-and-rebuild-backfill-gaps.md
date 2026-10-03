<!-- version=8, status=converged -->
# GI-22 — `clens prices --set` clobbers a brand-new model's fields; rebuild-based backfill is unreliable and unguarded against a live `serve`

Issue: https://github.com/abhisheksarkar30/claude-lens/issues/22
Branch: `GI-22-prices-set-clobber-and-rebuild-backfill-gaps` (off `main`)
Beads: `.beads/GI-22/` (filled in at Phase 3)

## 1. Problem

Four related defects surfaced while adding rates for two previously-unpriced models
(`claude-sonnet-5-5`, `claude-haiku-4-5-20251001`) through the documented CLI path:

- **R1 (defect, reproduced)**: `clens prices --set model:field=value` repeated for the *same
  model* in one invocation silently drops every field but the last. This is not limited to a
  brand-new model. Two or more `--set`s against a model that already has an effective rate — a
  shipped rate, or a rate previously overridden — do the same thing: the earlier field edit is
  replaced by the value in the frozen snapshot, and only the last `--set` survives.
- **R2 (defect, reproduced; mechanism unconfirmed)**: after adding the Haiku rate and running
  `clens ingest --rebuild`, 6 of 18 `claude-haiku-4-5-20251001` rows stayed `cost_source='unpriced'`
  despite the rebuild reporting `failed=0`.
- **R3 (process gap, reproduced)**: running `ingest --rebuild` against a live `clens serve`
  produced ~2 minutes of sustained `SQLITE_BUSY` in serve's own capture path.
- **R4 (product gap, design-confirmed)**: there is no narrow, reliable way to backfill one
  newly-priced model's history; `reprice` refuses by design, and `ingest --rebuild` is the only
  documented alternative.

## 2. Requirements and verified root causes

| # | Requirement | Root cause (verified in code) | Decision |
|---|---|---|---|
| R1 | `clens prices --set` must apply every field given for a model in one invocation, not just the last | `internal/cli/prices.go:89-113` (`applyPriceEdits`): `effective := pricing.NewLoader(path, nil).Table()` (`prices.go:94`) is a single snapshot taken *before* the `--set` loop. Every iteration reads `r, ok := effective[model]` (`prices.go:101`) and writes `overrides[model] = r` (`prices.go:112`) without ever reading `overrides[model]` back. The stale-snapshot defect is not conditional on the model being new — it reproduces for any model targeted by two or more `--set`s in one invocation, whether that model is absent from `effective`, already has a shipped rate, or already has a prior override merged into the snapshot. The fresh-zero branch (`r = pricing.Rate{Model: model}` when the model is absent from `effective`) is one *symptom* of that lookup, not the cause. On an existing model the same lookup restarts each iteration from the pristine snapshot, so an earlier field edit in the same call is overwritten by the snapshot's value. Reproduced for the new-model symptom: 5 `--set`s for `claude-sonnet-5-5` in one call left only `cache_write_1h_rate` set. | Check `overrides[model]` before falling back to `effective[model]` (§3.1) |
| R2 | Adding a rate for a model must apply to that model's already-captured rows via `ingest --rebuild`, as documented | **Mechanism open for the 6 stuck rows; candidate (a) is already-documented fact, not an open hypothesis equal to the others.** `internal/store/merge.go`'s `mergeEvents` cost-column winner (`CostUSD`/`ApiEquivalentCostUSD`/`CostSource`, lines 299-301) follows capture-completeness (lines 250-290). Since JSONL rows set `CaptureComplete=true` unconditionally (`internal/jsonlogs/jsonlogs.go:462`), the completeness branch normally makes `incoming` win on every rebuild — meaning `mergeEvents` is likely *not* the cause of the 6 stuck rows (bead 02 will confirm). Bead 02's reproduction stays a `mergeEvents` regression guard; it does not adjudicate three equally-open candidates. **(a) is settled:** `docs/context/cli-and-tooling.md` already states that ingest cannot reach proxy-only rows (the `request_id` merge never replaces the proxy's bodies). `runIngest` (`internal/cli/ingest.go:26-68`) only reaches rows through `tailer.Poll` at line 61 — lines 48-52 are `resetJSONLCursors`, not the poll — so a row with no JSONL counterpart never enters `mergeEvents`. Whether these 6 rows are that kind of row is a manual `source`/`source_refs` check, not an open mechanism question. The still-open candidate is **(b)** an exact-string mismatch between the override-file key and the stuck rows' `model_resolved` value (`internal/pricing/pricing.go:94-97` does a plain map lookup with no normalization). The `is_sidechain` correlation and cross-file `request_id` duplication were ruled out as causes by live investigation (no query result cited in this plan — treated as unverified rather than proven-false). | Bead 02 reproduces the `mergeEvents` mechanism as a regression guard (candidate (a)'s reachability is already documented and is not a question that bead decides); its result — combined with a manual check of the stuck rows' `source`/`source_refs` and `model_resolved` — gates whether a code fix for R2 exists this ticket or R2 is re-scoped (§3.2, §7) |
| R3 | A long write-heavy CLI command should not silently degrade a live `serve`'s capture | `internal/cli/restart.go:152` already probes `GET /api/health` on the dashboard address to decide liveness — the pattern exists, just isn't reused by `ingest`/`reprice`/`reflag`/`purge`. `storage-schema.md:280` and `workflows.md:194` document `SetMaxOpenConns(1)` as a *per-process* writer-serialization invariant; neither claims cross-process safety, so this is an undocumented gap, not a stale doc. | Reuse the existing health-probe pattern as a pre-flight warning (§3.3) |
| R4 | A newly-priced model's already-captured history should be backfillable without touching unrelated history or contending with `serve` | `internal/store/store.go:2140-2146` (`repriceInScope`) deliberately excludes `cost_source='unpriced'` rows from `reprice` — "never newly priced even where the model resolves today" is a stated invariant, not an oversight. No narrower tool exists. | Opt-in `--model <name>` on `reprice` is that narrower backfill (§3.4, §9): skip every row whose `model_resolved` is not the named model, in-scope rows of other models included, and override the unpriced exclusion only for the named model. Unrelated history stays untouched, and the pass does not walk the whole table the way an unqualified `reprice` does, so it does not contend with `serve`. Lowest priority; bead 05 stays first-to-cut (§8) |

Decision taken with the user: keep all four in this one issue/ticket rather than splitting R3/R4
into a separate lower-priority ticket (confirmed in Phase 1 checkpoint).

## 3. Design

### 3.1 R1 — `applyPriceEdits` merge order

`internal/cli/prices.go`, inside the `for _, s := range sets` loop (currently line ~101):

```go
r, ok := effective[model]
if !ok {
    fmt.Fprintf(w, "prices: %s has no shipped rate; defining it from scratch\n", model)
    r = pricing.Rate{Model: model}
}
```

becomes:

```go
r, ok := overrides[model]
if !ok {
    r, ok = effective[model]
}
if !ok {
    fmt.Fprintf(w, "prices: %s has no shipped rate; defining it from scratch\n", model)
    r = pricing.Rate{Model: model}
}
```

`overrides` is already loaded once at the top of `applyPriceEdits` via `pricing.LoadOverrides(path)`
and is the same map every iteration writes `overrides[model] = r` back into — so checking it first
picks up whatever the *previous* `--set` in this call already produced, for a brand-new model and
for a model that already has an effective rate. Falling back to `effective[model]` (and then to a
fresh zero value) stays as it is for the first `--set` of a model this call has not yet written.
A single `--set` against a shipped rate, or against a prior-session override already merged into
`effective`, is already correct before this change: one iteration never re-reads a write it just
made. Two or more `--set`s in the same invocation against that same existing model are not. Each
iteration restarts from the frozen `effective` snapshot, so only the last field edit survives.
The `overrides[model]`-first lookup is what fixes that case too.
`TestPricesSetMultipleFieldsExistingModelInOneInvocation` (§5.2) is the test that proves it.
No change to `setRateField`, `splitSet`, or the unset path. The code snippet above is the whole
lookup change.

**Behavior change:** with the bug, the "no shipped rate; defining it from scratch" message fires on
*every* `--set` for a brand-new model (the buggy `effective[model]` lookup misses every time).
After the fix it fires only on the *first* `--set` for that model in a given invocation, because
subsequent iterations find `overrides[model]` already populated. This is correct behavior;
`TestPricesSetMultipleFieldsNewModelInOneInvocation` should assert the message prints exactly once
(not five times) across five `--set`s.

### 3.2 R2 — reproduce the merge mechanism before designing a fix

New test in `internal/store/merge_test.go` (file exists — `merge.go`'s sibling): construct an
`existing` event with `cost_source='unpriced'`, realistic non-zero tokens,
`model_resolved='claude-haiku-4-5-20251001'`, and an explicit `CaptureComplete` (the ordinary
complete proxy row is `true`). Construct an `incoming` event with the same non-zero token
columns, `CaptureComplete = true`, and costs computed against a `Table` that now carries a real
rate for that model (`CostUSD`/`ApiEquivalentCostUSD` set, `CostSource='user'`). Both sides stay
non-zero so the `usageObserved` swap at `merge.go:280-290` does not apply. Call
`mergeEvents(existing, incoming)` and assert the result's `CostSource` is the incoming priced
source (`'user'`).

Based on code inspection (`merge.go:250-264`), the incoming row wins the completeness-based pick
whenever `incoming.CaptureComplete` is true — which JSONL rows always satisfy (`jsonlogs.go:462`) —
making **`'user'` (priced) the expected result**. Bead 02 is therefore expected to find the
mergeEvents path is *not* the cause of the 6 stuck rows. Its test remains a legitimate
`mergeEvents` regression guard on its own terms, and it is not sufficient to diagnose the live
symptom. It does not adjudicate three equally-open candidates: candidate (a) is
already-documented fact, not a hypothesis the test is choosing among.

**Candidate (a) is already-documented fact.** `docs/context/cli-and-tooling.md` states that the
`request_id` merge never replaces the proxy's bodies, so ingest cannot reach proxy-only rows.
That reachability is settled independently of bead 02.

**After bead 02 runs, the next diagnostic step is to check the stuck rows directly:** query
`source`, `source_refs`, and `model_resolved` for the 6 rows (a manual step, not automatable in
CI). The check asks whether these 6 rows are proxy-only (membership only — the reachability
fact is already documented) and whether candidate (b) holds:

1. **Proxy-only rows (reachability already settled):** `runIngest` (`ingest.go:26-68`) drives
   only `tailer.Poll` (line 61), which walks JSONL files and merges lines it finds there. Lines
   48-52 are the `resetJSONLCursors` block, not the poll. A row whose `request_id` has no
   matching JSONL line never enters `mergeEvents` at all — repricing it requires R4's
   `reprice --model` scoping (§3.4), not a `mergeEvents` fix. If all 6 stuck rows are
   `source='proxy'` with no JSONL counterpart, R2's real fix is the same tool-gap R4 already
   names; document in §7 and close bead 03 as not applicable. That mapping does not change
   R4's priority: bead 05 stays the lowest-priority bead and the one to drop first (§8).

2. **Exact model-string mismatch:** `pricing.go:94-97` does a plain map lookup (`t[model]`) with
   no prefix normalization. If the key written via `clens prices --set` (e.g.
   `claude-haiku-4-5-20251001`) does not byte-for-byte match the `model_resolved` stored in those
   6 rows (a dated-snapshot variant, a suffix difference), `Compute` returns `(nil, "unpriced")`
   for the incoming JSONL row too — even a fixed winner-pick would still prefer a still-unpriced
   incoming row. Check: run `clens models` (or `SELECT DISTINCT model_resolved FROM events WHERE
   cost_source='unpriced'`) and compare against the override-file key exactly.

- A result of `'unpriced'` counts as the cause only when `incoming.CaptureComplete` is already
  true. With both flags at Go's zero value `false`, `winner` stays `existing`
  (`merge.go:250-264`) and `CostSource` is copied from that winner (`merge.go:299-301`), so the
  result is `'unpriced'` even when incoming carries a real price. That is a broken fixture, not
  a confirmed root cause, and it must not start bead 03. JSONL rows set `CaptureComplete: true`
  (`jsonlogs.go:462`); a fixture that omits the flag does not reproduce that path. When the
  fixture above is valid and `mergeEvents` still returns `'unpriced'`, the completeness-based
  winner pick is the cause: stop keying the cost-column pick to `CaptureComplete` /
  `usageObserved` alone, so a `winner` whose `CostSource == "unpriced"` loses to a loser that
  priced successfully, mirroring the existing `usageObserved` swap-back at `merge.go:280-290`.
  That swap is not redesigned. A wholesale reassignment of `winner` in that style also copies
  tokens and `ModelResolved` from the new winner (`merge.go:292-306`), not the cost columns
  alone; the note is recorded here and the swap's design stays as it is.

This bead's outcome gates whether a code fix for R2 exists in this ticket at all, or whether R2 is
re-scoped to "diagnosed, mechanism documented, fix tracked separately" — see §7.

### 3.3 R3 — pre-flight live-serve warning

`internal/cli/restart.go:152`'s `GET /api/health` probe (short timeout, dashboard address from the
resolved config) is extracted into a small shared helper (e.g. `internal/cli/livecheck.go`,
`probeServeHealth(dashboardAddr string, timeout time.Duration) bool`). Before dialing, the helper
**must call `dialableDashboardAddr(dashboardAddr)`** (defined in `internal/cli/shutdown.go:78-88`)
to rewrite wildcard bind hosts (`0.0.0.0`, `::`, empty) to `127.0.0.1` — exactly as
`restart.go:70-71`, `shutdown.go:46`, and `reload.go:138` already do. Skipping this normalization
would produce a false "serve is not live" negative on the repo's own real deployment config
(`~/.clens/config.toml` sets `DashboardAddr = 0.0.0.0:8798`).

The helper is called from `runIngest` (`internal/cli/ingest.go`, only on `--rebuild`) and
`runReprice` (`internal/cli/reprice.go`) — both already have `cfg` in scope. For `runReflag`
(`internal/cli/reflag.go:35`) and `runPurge` (`internal/cli/purge.go:50`), the current call is
`_, st, err := openStore(args)` which discards the `*config.Config` return value. These two need
a one-line change first: `_, st, err` → `cfg, st, err` — then the probe call can be wired in
exactly as for `runReprice`. On a live hit, print a `WARN`-level line to the same writer the
command already uses (matching `doctor`'s PASS/WARN/FAIL vocabulary) naming the contention risk,
and proceed — this is a warning, not a new confirmation gate, so it does not change any of the
four commands' existing `--yes` semantics.

**When the warning fires (decided):** gate the probe behind `!dryRun`. `reprice`, `reflag`, and
`purge` already refuse to write unless `--yes` or `--dry-run` is set (`internal/cli/reprice.go:27-29`,
`reflag.go:31-33`, `purge.go:46-48`). Call the helper only in the `!dryRun` branch, after
`openStore` has put `cfg` in scope, so the WARN line prints only when the command is about to
write. A `--dry-run` against a live `serve` prints no WARN line. That is the right scope for R3:
the reproduced harm is sustained write-lock contention (`SQLITE_BUSY` on serve's capture path),
and a dry-run opens the store, reads, and writes nothing (`purge` also skips `VACUUM` under
`--dry-run`). Warning that preview would be a false alarm on the common read-only workflow.
`ingest --rebuild` has no `--dry-run` and always writes, so `runIngest` always probes on
`--rebuild`. A bare invocation of `reprice`/`reflag`/`purge` (neither `--dry-run` nor `--yes`)
still exits at the existing refuse, before the probe.

### 3.4 R4 — scoped reprice backfill (lowest priority; see §9 before committing to this)

`clens reprice --model <name>` (new flag, `internal/cli/reprice.go`) is the narrower backfill §2
and §9 describe: one named model's history, unrelated history left untouched, without the
full-table write that contends with `serve`. It is not "plain `reprice`, plus this model's
previously-unpriced rows."

Before `openStore`, take `--model` off `args` with `takeFlag` (`internal/cli/accounts.go:95-110`),
the same way `--dry-run` and `--yes` are removed at `reprice.go:25-26`. `openStore` forwards the
remaining args to `config.Load`, and `applyFlags`' `FlagSet` has no `model` flag, so a flag left
on `args` is rejected and the command errors before any reprice runs.

When the flag is set, pass the name through to `store.RepriceCosts`. The pass skips every row
whose `model_resolved` is not that name, in-scope rows of other models included. Filter the
candidate `SELECT` (`store.go:2242-2249`) with `model_resolved = ?` so the one `BeginTx`
(`store.go:2232-2236`) covers that model only, not every event an unqualified `reprice` walks.
For rows that match, bypass `repriceInScope` (`store.go:2280-2283`) only for the named model's
`cost_source='unpriced'` rows. Every other model's rows stay out of the pass: an in-scope row
for a different model is not `Moved`, and an unpriced row for a different model stays skipped.
The narrower backfill is unchanged: the pass still skips every row whose `model_resolved` is
not the named model.

Those bypassed rows are priced by the existing `Table.Compute` (`internal/pricing/pricing.go`).
Once the model is in the table, `Compute` multiplies every non-zero token class by that class's
`*big.Rat` with no nil check (`pricing.go:130`: `new(big.Rat).Mul(..., class.rate)`).
`LoadOverrides` fills a nil cache-write rate from `input_rate` (`pricing.go:295-300`) and leaves
`CacheReadRate` nil. A from-scratch override that leaves a used class unset therefore panics
inside `RepriceCosts`'s transaction. `Rat.Mul` dereferences the rate; a nil `class.rate` is not
a skipped class. This is the first pass that calls `Compute` on previously-unpriced rows of a
from-scratch model. Agentic rows of the models this ticket is about carry `cache_read_tokens`.

The nil-rate guard lives in `Table.Compute`. If a class has tokens > 0 and a nil rate, return
`(nil, "unpriced")` before the multiply — the same answer as an absent model (`pricing.go:96-98`),
not a partial sum and not a panic. `RepriceCosts` already skips a nil `usd` and keeps the stored
row (`store.go:2286-2292`), so the backfill does not need a store-side rate check. The live
capture path already calls this `Compute` (`internal/consumer/consumer.go:356-361`) with no
`recover`. The guard stays on that path because it is in `Compute`; it is not a reprice-only
check. This does not raise R4's priority.

Printed output says the pass is limited to the named model
("`--model claude-haiku-4-5-20251001` is backfilling previously-unpriced rows for this model only"),
so a dry-run does not present any other model's row as moving.

R4's priority is unchanged. Bead 05 stays the one to drop first (§8).

## 4. Change list

### 4.1 Files
Modified: `internal/cli/prices.go`, `internal/cli/ingest.go`, `internal/cli/reprice.go`,
`internal/cli/reflag.go` (also: `_, st` → `cfg, st` in `openStore` call), `internal/cli/purge.go`
(also: `_, st` → `cfg, st` in `openStore` call), `internal/store/merge.go` (only if §3.2 confirms
the hypothesis), `internal/store/store.go` (only if R4 / bead 05 ships — `RepriceCosts` takes no
model argument today; `--model` skips every row whose `model_resolved` is not the named model,
in-scope rows of other models included, and bypasses the `repriceInScope` skip at `store.go:2281`
only for the named model's `cost_source='unpriced'` rows; listing the file does not raise R4's
priority, and bead 05 stays the one to drop first, §8), `internal/pricing/pricing.go` (the
nil-rate guard in `Table.Compute`, §3.4: a class with tokens > 0 and a nil rate returns
`(nil, "unpriced")` before the multiply at `pricing.go:130`. `LoadOverrides` leaves
`CacheReadRate` nil. The guard lives in `Compute`, which the live capture path already calls
at `internal/consumer/consumer.go:356-361`; do not put it only on the reprice path. Listing
this file does not raise R4's priority, and bead 05 stays the one to drop first, §8)
New: `internal/cli/livecheck.go`, `internal/cli/prices_test.go` (does not exist today), a new test
in `internal/store/merge_test.go`

### 4.2 Explicitly not touched
- **`internal/cli/restart.go`** — already has the correct health-probe pattern; R3 extracts and
  reuses it, never changes `restart`'s own behavior.
- **The shipped rate table (`internal/pricing/table.go`)** — adding `claude-sonnet-5-5` /
  `claude-haiku-4-5-20251001` as `shipped` rows (instead of `user` overrides) was raised in the
  prior session and explicitly deferred; this ticket is about the CLI/merge defects, not about
  promoting those two rates into the binary.
- **`clens doctor`** — R3's warning is a per-command pre-flight check, not a `doctor` check; doctor
  already covers port collisions, which is a different signal (a port in use, not a transaction in
  flight).

## 5. Test strategy

### 5.1 What is genuinely at risk

| Risk | Coverage |
|---|---|
| R1's single `--set` on an existing shipped or previously overridden model drops untouched fields | `TestPricesSetSingleFieldExistingModelUnaffected` and `TestPricesSetSingleFieldAgainstPriorSessionOverridePreserved` — one `--set`; untouched fields stay |
| R1's repeated `--set` on a model that already has an effective rate keeps only the last field | `TestPricesSetMultipleFieldsExistingModelInOneInvocation` — two or more `--set`s in one call, shipped or previously overridden; both field edits survive |
| R3's warning becomes a hard failure and blocks legitimate single-user, serve-not-running usage | Test asserts the probe is skipped/false when nothing listens on the dashboard address, and the command proceeds unchanged |
| R4 moves a row whose `model_resolved` is not the named model | `TestRepriceModelFlagBackfillsOnlyNamedModel` asserts an in-scope row for a different model is not `Moved`. An unpriced row for a different model stays skipped too. The flag is the narrower backfill in §2, §3.4, and §9, not a full `reprice` plus extra unpriced rows |
| R4 sends a from-scratch unpriced row through `Table.Compute`, which panics when a used class has a nil rate | `TestRepriceModelFlagBackfillsOnlyNamedModel` seeds `filepath.Join(home, ".clens", "prices.toml")` with an override for a model `ShippedTable` does not contain, `input_rate` and `output_rate` set and `cache_read_rate` omitted, so the model is in the effective table with `CacheReadRate` nil and `cache_read_tokens > 0`. A missing override row does not count. A direct `Table.Compute` assertion returns `(nil, "unpriced")` with no panic, in `internal/pricing/pricing_test.go` or against the table `runReprice` loaded; a `recover` around `RepriceCosts` does not satisfy it. The complete-rate subscription case stays on a row whose used classes all have non-nil rates (`cache_read_tokens == 0` when `cache_read_rate` is the unset class) |

### 5.2 New tests

Each prices test below calls `withHome(t)` (`internal/cli/doctor_test.go:17-24`) and uses the
returned `home` before `runPrices`. `runPrices` reads and writes `pricing.DefaultPath()`
(`internal/cli/prices.go:31`), which joins `userHomeDir()` with `.clens/prices.toml`.
`userHomeDir` returns `$HOME` when it is non-empty (`internal/pricing/pricing.go`), so setting
only `USERPROFILE` does not isolate the test when `HOME` is already set. `withHome` sets `HOME`
to `t.TempDir()` and clears `USERPROFILE`. A prior-session override is seeded at
`filepath.Join(home, ".clens", "prices.toml")` — the path `DefaultPath` resolves once `HOME` is
that temp dir. Assert against that file, not the real home directory. The brand-new-model test
does not seed an override for the model under test; seeding one there would no longer be the
brand-new case.

Every new `runIngest` / `runReprice` / `runReflag` / `runPurge` test in this section calls
`withHome(t)` before the command, and seeds or asserts only under that temp home. Those
commands open the default database (`config.Default`'s `DBPath`, `~/.clens/lens.db` under
`$HOME`) when `--db-path` is absent (`ingest.go:39`, `reprice.go:31`, `reflag.go:35`,
`purge.go:50`). On Windows, `ingest` walks `%USERPROFILE%\.claude\projects` unless
`USERPROFILE` is cleared; `withHome` clears it (`doctor_test.go:17-24`).

- `TestPricesSetMultipleFieldsNewModelInOneInvocation` (`internal/cli/prices_test.go`) — five
  `--set`s for one brand-new model in one `runPrices` call; asserts all five fields are present in
  the resulting table at `filepath.Join(home, ".clens", "prices.toml")`, **and** asserts the
  "no shipped rate; defining it from scratch" message appears exactly once (not five times).
- `TestPricesSetMultipleFieldsExistingModelInOneInvocation` (same file) — two or more `--set`s
  in one `runPrices` call against a model that already has an effective rate, each touching a
  different field, asserting **both** edits are present afterward (not just the last). Cover
  both kinds of existing rate in that test: a shipped rate, and a rate previously overridden
  (seed that prior override at `filepath.Join(home, ".clens", "prices.toml")`, the same way the
  prior-session test does). Fields already on that rate, and not named by either `--set`, must
  stay at their pre-invocation values. Assert against that temp-home file. This is the case the
  stale `effective` snapshot breaks for models that are not brand-new. The single-`--set` tests
  below do not exercise it.
- `TestPricesSetSingleFieldExistingModelUnaffected` (same file) — one `--set` against an
  existing shipped model. A single `--set` never re-reads its own write, so this path is
  already correct before the fix; the test asserts untouched fields are preserved, read back
  from `filepath.Join(home, ".clens", "prices.toml")`.
- `TestPricesSetSingleFieldAgainstPriorSessionOverridePreserved` (same file) — seeds
  `filepath.Join(home, ".clens", "prices.toml")` with an existing user override for a model
  (simulating a prior-session state where `overrides[model]` is already populated before this
  invocation begins), then calls `runPrices` with a single `--set` for a *different* field on
  that same model, and asserts all pre-existing fields plus the new one are present in that
  file. This is the third case the §10 QA self-review requires bead 01 to cover.
- `TestMergeEventsPrefersPricedOverUnpriced` (`internal/store/merge_test.go`) — the §3.2
  reproduction. Sets `incoming.CaptureComplete = true` and an explicit `existing.CaptureComplete`
  (the ordinary complete proxy row is `true`), with non-zero tokens on both sides so the
  `usageObserved` swap does not apply, and asserts `CostSource` is the incoming priced source.
  An `'unpriced'` result counts as the cause only when that incoming flag is already true. A
  zero-value `false` is a broken fixture and must not start bead 03. The test's outcome, read
  that way, decides whether a `merge.go` fix bead exists.
- `TestIngestRebuildWarnsWhenServeIsLive` / equivalents for `reprice`/`reflag`/`purge`
  (`internal/cli/*_test.go`) — each calls `withHome(t)` before the command. Fake listener
  standing in for `serve`'s health endpoint. Those WARN assertions are the write path:
  `--rebuild` for ingest, `--yes` for `reprice`/`reflag`/`purge`. The ingest test builds its
  JSONL tree at `filepath.Join(home, ".claude", "projects", ...)`, the same way
  `TestIngestRebuildRereadsWithoutDuplicating` does (`internal/cli/additions_test.go:54-64`), so
  `--rebuild` does not read the operator's transcripts. The purge WARN test passes a selector
  (`--unpriced` or `--older-than`) with `--yes`: `runPurge` returns "nothing to do" at
  `purge.go:43-44` before `openStore` when neither selector is set, so a bare `--yes` never
  reaches the probe. The wildcard-bind case is carried by
  `TestIngestRebuildWarnsWhenServeIsLive` (`ingest --rebuild`), matching
  `TestShutdownDialsLoopbackForAWildcardDashboardAddr` (`internal/cli/shutdown_test.go:17-41`):
  the listener is loopback (`httptest.NewServer`, or `127.0.0.1`), the command is passed
  `--dashboard-addr 0.0.0.0:<port>`, and the assertion is the request `Host` header
  `127.0.0.1:<port>`. A WARN line may be a second check. A WARN line from a listener bound on
  `0.0.0.0` is not proof the rewrite is wired in: `healthy` reports live for any HTTP 200 from
  `http://` plus the configured address (`internal/cli/restart.go:150-157`), and a wildcard bind
  accepts a dial of that same address. That ingest command also passes `--allow-remote`, because
  `runIngest` calls `cfg.Validate()` (`internal/cli/ingest.go:34-37`) and `0.0.0.0` fails
  `IsLoopbackHost` (`internal/config/config.go:435-441`) unless `AllowRemote` is set
  (`config.go:410-421`). `runReprice`, `runReflag`, and `runPurge` do not call `Validate`.
  Separately, a `--dry-run` of `reprice`, `reflag`, and `purge` against a live fake listener
  must print **no** WARN line (the probe is skipped when `dryRun` is set). `ingest --rebuild`
  has no `--dry-run` and is not part of that assertion.
- `TestRepriceModelFlagBackfillsOnlyNamedModel` (`internal/cli/reprice_test.go`) — only if R4 is
  built this ticket (§9). Calls `withHome(t)` before `runReprice`. Asserts an in-scope row for a
  different model is not `Moved`, and that an unpriced row for a different model stays skipped.
  Must also include a complete-rate subscription case: an `unpriced` row for the named model
  with `billing_mode = "subscription"` whose used classes all have non-nil rates
  (`cache_read_tokens == 0` when `cache_read_rate` is the unset class). The backfilled figure
  lands in `api_equivalent_cost_usd` with `cost_usd` left NULL (the billing-split invariant
  from `storage-schema.md`). Must also include the from-scratch cache-read case. Seed
  `filepath.Join(home, ".clens", "prices.toml")` with an override for a model `ShippedTable`
  does not contain, with `input_rate` and `output_rate` set and `cache_read_rate` omitted, so
  the loaded table contains that model and `CacheReadRate` is nil. The row has
  `cache_read_tokens > 0`. A missing override row does not count: an absent model returns
  `(nil, "unpriced")` at the lookup (`internal/pricing/pricing.go:96-98`) and never reads a
  rate. The command returns, the row stays `cost_source='unpriced'`, and both cost columns
  stay NULL. Those stored-row assertions do not prove the nil-rate branch ran. Require a
  direct `Table.Compute` assertion on that rate and usage — `(nil, "unpriced")`, no panic —
  in `internal/pricing/pricing_test.go` or against the table `runReprice` loaded. A `recover`
  around `RepriceCosts` does not satisfy it. `repriceSeedRow` sets input and output tokens
  only (`internal/cli/reprice_test.go:16-29`); a case that prices those two classes and never
  sets `cache_read_tokens` does not satisfy this requirement. `LoadOverrides` leaves
  `CacheReadRate` nil (`internal/pricing/pricing.go:295-300`), and the guard in
  `Table.Compute` returns `(nil, "unpriced")` before the multiply at `pricing.go:130` (§3.4).
  The guard stays in `Table.Compute`, including the live capture path. Bead 05 stays
  first-to-cut. The narrower `--model` backfill is unchanged.

### 5.3 Not covered by any automated test
- The actual cross-process `SQLITE_BUSY` contention under R3 is not reproduced in CI (would need
  two real OS processes against one SQLite file) — the test only covers the warning's trigger
  condition, not the contention itself, which was already confirmed by hand this session.
- Whether the §3.2 hypothesis is even the *complete* explanation for all 6 stuck rows in the live
  database — the fix (if any) is verified by the unit test, not by re-querying the live `lens.db`
  again, since the earlier live-DB investigation already produced one false lead.

### 5.4 Negative control (required)
- R1: revert the `overrides[model]` check back to the current `effective[model]`-only lookup —
  `TestPricesSetMultipleFieldsNewModelInOneInvocation` must fail, asserting exactly the observed
  symptom (only the last of five fields present). The same revert must also fail
  `TestPricesSetMultipleFieldsExistingModelInOneInvocation`: only the last `--set`'s field
  differs from the frozen snapshot, and the earlier field edit is gone. The brand-new-model
  failure alone does not prove the existing-model path. A fix that only special-cases a model
  absent from `effective` would still pass the new-model test while leaving a shipped or
  previously overridden model's earlier `--set` in the same call reverted to the snapshot.
- R2 (if a fix lands): revert the winner-pick change in `mergeEvents` —
  `TestMergeEventsPrefersPricedOverUnpriced` must fail by returning `'unpriced'` instead of the
  priced source. That `'unpriced'` counts only when the fixture's `incoming.CaptureComplete` is
  already true (§3.2). A zero-value `false` is a broken fixture, not this negative control, and
  must not start bead 03.
- R3: skip starting the fake listener — the warning test must fail to find the `WARN` line in the
  command's output.

## 6. Risk areas
- **R2 — the hypothesis may be wrong.** Do **not** write a `merge.go` fix before bead 02's test
  result comes back. That result gates bead 03 only (§7); bead 01 does not wait on it. If the
  reproduction shows the completeness-based pick is *not* the cause, stop and re-scope R2 to a
  diagnosis-only outcome for this ticket (§7) rather than guessing at a second mechanism under
  time pressure.
- **R3 — do not turn a warning into a hard gate.** `ingest --rebuild`/`reprice`/`reflag`/`purge`
  must still run with no `serve` listening (the single-user common case); the probe only adds a
  printed line when a listener answers, never a new confirmation prompt or exit code change.
- **R4 — scope creep risk.** If built, `--model` is the narrower backfill in §2, §3.4, and §9.
  It skips every row whose `model_resolved` is not the named model, in-scope rows of other models
  included, and the name must match `model_resolved` verbatim. It must not become a general
  "reprice unpriced rows" escape hatch, and it must not stay a full-table `reprice` that still
  rewrites other models' stored costs.

## 7. Pre-flight (bead 02's result; the checkpoint gates bead 03 only)

Bead 02 (§3.2's reproduction test) decides whether R2 ships a code fix in this ticket or is
re-scoped to "diagnosed, mechanism documented, fix tracked separately" if the hypothesis is wrong.
Whoever picks this plan up in the next session should treat bead 02's result as a checkpoint
that gates bead 03 only, before beadifying the rest of R2, not assume the fix in §3.2 is already
correct. An `'unpriced'` result confirms the hypothesis only when the fixture's
`incoming.CaptureComplete` is already true (§3.2). A zero-value `false` is a broken fixture and
does not start bead 03. Bead 01 (the prices-set fix) stays independent of this diagnostic, as §8
already says.

## 8. Beads

| # | Bead | Priority | Depends on | Purpose |
|---|---|---|---|---|
| 01 | `fix-prices-set-merge-order` | P0 | — | R1: the `overrides[model]`-first lookup fix, plus its four tests (new model multi-set, existing shipped model single-set, prior-session override preserved, existing-model multi-set for a shipped or previously overridden rate) |
| 02 | `test-merge-events-cost-winner-reproduction` | P0 | — | R2 diagnostic: the `mergeEvents` reproduction test from §3.2 plus manual check of stuck rows' `source`/`model_resolved`; its result gates bead 03 |
| 03 | `fix-merge-events-cost-winner` | P1 | 02 (only if confirmed; **must not start until bead 02's test result is recorded**) | R2 fix: stop keying the cost-column winner to completeness alone, if bead 02 confirms the hypothesis. Confirmation is an `'unpriced'` result with `incoming.CaptureComplete` already true (§3.2); a zero-value `false` does not start this bead. The `usageObserved` swap's design is unchanged |
| 04 | `feat-livecheck-warn-write-commands` | P1 | — | R3: extract the health-probe helper and wire it into `ingest --rebuild`, `reprice`, `reflag`, `purge` |
| 05 | `feat-reprice-model-scoped-backfill` | P2 | — | R4: the opt-in `--model` flag on `reprice`. The nil-rate guard is in `Table.Compute` (`internal/pricing/pricing.go`, §3.4), which the live capture path already calls; this bead's priority is unchanged |

Beads 01, 02, and 04 are independent and can run in any order or in parallel. Bead 03 is
conditional on bead 02's finding — if the hypothesis is wrong, close bead 03 as "not applicable,
see bead 02's result" rather than forcing a fix. Bead 05 is the lowest-priority, most optional
bead in this set (see §9) and is the one to drop first if this ticket needs to ship smaller.
The nil-rate guard does not move it up. If bead 05 is built, that guard is in `Table.Compute`
(`internal/pricing/pricing.go`, §3.4), which the live capture path already calls, so the same
change covers live capture.
No bead is destructive; none touches migrations, schema, or already-captured data directly — a
future, explicit `--model` invocation touches only the named model's rows (including its
previously-unpriced ones) and leaves every other model untouched, and ships with no default
behavior change.

## 9. Alternatives considered and rejected

**Fix R2 by making `ingest --rebuild` always win cost columns for the incoming row.** Rejected:
would silently discard a *better* existing capture's price in the ordinary (non-buggy) case the
current completeness rule exists to protect, per `merge.go:235-249`'s own stated rationale.

**Make R3 a hard failure instead of a warning.** Rejected: the single-user, serve-not-running case
is the common one, and these commands (`reprice`, `reflag`, `purge`) already require `--yes`; adding
a second blocking gate on top would make routine maintenance commands fail intermittently against
a false positive (e.g. another local tool briefly answering the same loopback port).

**Build R4 as a new `clens backfill` subcommand instead of a `reprice` flag.** Rejected: `reprice`
already owns "recompute stored costs from current rates" end to end (`docs/context/cli-and-tooling.md:36`);
`--model` is that same operation on a narrower row set — only rows whose `model_resolved` equals
the named model. In-scope rows of other models are not touched, so unrelated history stays
untouched and the pass does not contend with `serve` the way a full-table reprice does. A sibling
command for that narrower row set duplicates the dry-run/`--yes` plumbing for no real gain.
Revisit only if `--model`'s semantics turn out to need a genuinely different confirmation flow
than `reprice`'s existing one. R4's priority is unchanged; bead 05 stays first-to-cut (§8).

## 10. Self-review

**Senior engineer.** The one real design call is R2: ship a speculative fix, or gate it behind a
reproduction test first. Shipping speculatively was rejected — the earlier live-DB investigation
already produced one false lead (self-referential grep contamination against the live session's
own transcript), so a second guess without a controlled reproduction is the same mistake again,
just in Go instead of SQL.

**QA engineer.** The edge case most likely to go silently untested: R1's fix for a model that
*already has a user override* from a prior session (not a fresh one) — `overrides[model]` would
already be populated before this invocation's loop even starts, which is a third case (beyond
"brand-new" and "shipped-only") the current two new tests don't separately name. Bead 01 should
add it as a third case rather than assuming the fresh-model test covers it by extension. That
prior-session single-`--set` case is now named
(`TestPricesSetSingleFieldAgainstPriorSessionOverridePreserved`). A fourth case is still
required, and it is the one the stale snapshot actually breaks for an existing model: two or
more `--set`s in one invocation against a model that already has an effective rate (shipped or
previously overridden). A single `--set` does not exercise it. Bead 01's
`TestPricesSetMultipleFieldsExistingModelInOneInvocation` covers it (§5.2, §5.4).

**Security engineer.** No new trust surface: no network input, no auth, no new persistence beyond
what `reprice`/`ingest` already write, and `--model` in R4 only narrows an existing write path's
scope rather than widening who can trigger it (still local-CLI-only, same as every other `clens`
subcommand).

## 11. Context docs to refresh (running list)

- `docs/context/cli-and-tooling.md:19` — currently states `ingest --rebuild` "...so an added rate
  row or a changed prefix is applied to rows already captured without duplicating them" as an
  unqualified guarantee. Once bead 02/03 lands, this line needs a correction. **The exact
  correction depends on bead 02's finding:**
  - *If the hypothesis is confirmed (mergeEvents picks the wrong winner):* add a caveat describing
    the winner-pick condition under which it does not hold, and note the bead 03 fix.
  - *If the hypothesis is not confirmed and the real cause is proxy-only rows (the more structurally
    likely outcome per §3.2):* the correction is different in kind — "`ingest --rebuild` only ever
    reaches rows that have (or acquire) a JSONL counterpart; a pure proxy-sourced call is outside
    its reach regardless of merge correctness." This is more specific than `workflows.md`'s existing
    flow-4/flow-2 caveat ("it is not the general re-pricing path") and should be added as a named
    limitation, not just a parenthetical.
  - *If the cause is a model-string mismatch:* add a note that the `--set` key must exactly match
    `model_resolved` in the database (no normalization applied).

- `docs/context/cli-and-tooling.md` (ingest row, the closing sentence "Use `reprice` for stored
  costs") — **unconditional.** Not gated on bead 02, and not gated on R4 / bead 05 shipping.
  That sentence is wrong for unpriced rows today: `repriceInScope`
  (`internal/store/store.go:2139-2151`) returns false for `cost_source == "unpriced"` ("never
  newly priced even where the model resolves today"), so plain `reprice` counts those rows in
  `Skipped` and does not price them. The same cell's earlier clause — ingest cannot reach
  proxy-only rows — stays accurate; only the remedy is wrong for the unpriced case. Correct
  the sentence either way; shipping or dropping bead 05 changes the wording, not whether the
  correction is owed. R4's priority is unchanged (§8: lowest priority, the bead to drop first).
  - *If bead 05 ships:* the sentence should say: use `reprice` for stored costs on rows whose
    `cost_source` is reconstructible; `cost_source='unpriced'` rows are skipped by plain
    `reprice`; `reprice --model <name>` is the opt-in that backfills previously-unpriced rows
    for that model only.
  - *If bead 05 does not ship:* the sentence should say: use `reprice` for stored costs on rows
    whose `cost_source` is reconstructible; `cost_source='unpriced'` rows are skipped
    (`repriceInScope` never newly prices them); no current command backfills an unpriced
    model's stored cost, and `ingest --rebuild` still cannot reach proxy-only rows.

- `docs/context/workflows.md` (flow 4 — ingest reconciliation) — *Conditional on R2's mechanism
  turning out to be proxy-only rows:* add a cross-reference to `reprice --model` as the first
  command able to reach a pure proxy-sourced row for repricing, since none of the current flow-4
  text names this gap or its fix.

- `docs/context/cost-and-quota.md` (zero-write / unpriced rule) — *Conditional on R4 shipping:*
  add a note that `reprice --model <name>` is an opt-in override of the unpriced-row exclusion,
  applicable only to the named model, to avoid contradicting the general rule.

## Change History

### v1 (draft)
Written from this session's live investigation of claude-lens's pricing CLI and the `ingest
--rebuild` backfill path: the R1 bug was reproduced directly, R2 was investigated but not
root-caused (two hypotheses ruled out, one left open), and R3 was reproduced by running `ingest
--rebuild` against a live `serve`. Filed as GitHub issue #22 per Phase 1 checkpoint with the user,
who confirmed keeping all four items in one ticket. Not yet cross-reviewed — implementation to
proceed in a later session starting from Phase 2.5.

### v2 (round-1 review applied)
All 11 findings accepted (JUSTIFIED). Key changes:

- **§2 R2 row**: reframed the open mechanism as three ranked candidates (proxy-only rows, exact
  model-string mismatch, mergeEvents completeness pick) based on code inspection confirming the
  original hypothesis is unlikely to hold; softened the `is_sidechain`/request_id "ruled out" claim
  to "unverified from code alone."
- **§3.1**: added a note that the fix changes the "no shipped rate" message from N occurrences to
  one, and pinned it as a test assertion.
- **§3.2**: expanded with expected bead 02 outcome (likely 'user'/priced), two concrete candidates
  to check via manual query after bead 02, and how each outcome maps to a fix or re-scope.
- **§3.3**: added mandatory `dialableDashboardAddr` call to the extracted helper (0.0.0.0 wildcard
  is the real operator config); added explicit note that `reflag.go` and `purge.go` currently
  discard `cfg` via `_` and need a one-line change before the probe can be wired in.
- **§4.1**: named the `_, st` → `cfg, st` change in `reflag.go` and `purge.go` explicitly.
- **§5.2**: added third R1 test (prior-session override preserved), pinned message-count assertion
  in multi-set test, required wildcard-bind fake listener in R3 tests, added subscription
  billing-mode assertion to R4 test.
- **§8**: bead 01 scope updated to three tests; bead 03 description now explicitly names the
  must-not-start-before-bead-02 gate.
- **§11**: rewritten to cover three conditional outcomes for `cli-and-tooling.md:19`, added
  `workflows.md` flow 4 and `cost-and-quota.md` as conditional additions.

### v3 (round-2 review applied)
F2.2 and F2.3 accepted (JUSTIFIED). F2.1 accepted in part (PARTIAL): candidate (a) recorded as
already-documented fact, and an unconditional doc-correction note added. The suggestion to raise
R4's priority was not applied; bead 05 stays first-to-cut.

- **§2 R2 row / §3.2**: candidate (a) — ingest cannot reach proxy-only rows — is recorded as
  already-documented fact (`cli-and-tooling.md`), not an open hypothesis equal to (b) or the
  mergeEvents pick. Bead 02 stays a `mergeEvents` regression guard. The `ingest.go:48-52`
  citation is corrected to `runIngest` (`ingest.go:26-68`), with `tailer.Poll` at line 61;
  lines 48-52 stay named as `resetJSONLCursors`.
- **§3.3**: the live-serve probe is gated behind `!dryRun` for `reprice`/`reflag`/`purge` (WARN
  only when the command is about to write). `ingest --rebuild` always probes. Justified because
  R3's harm is write-lock contention and a dry-run writes nothing.
- **§5.2**: `--dry-run` of `reprice`/`reflag`/`purge` against a live listener must print no WARN
  line.
- **§11**: unconditional note that "Use `reprice` for stored costs" is wrong for unpriced rows
  today, with the sentence to write if bead 05 ships and the sentence to write if it does not.
  §8's "lowest priority... the one to drop first" is unchanged.

### v4 (round-3 review applied)
F3.1 and F3.2 accepted (JUSTIFIED) and applied in full. R4's priority was not raised; bead 05
stays first-to-cut. Round-1 and round-2 edits were not reopened.

- **§1 R1 bullet**: the defect is repeated `--set`s on the same model in one invocation, including
  a model that already has a shipped or previously overridden rate. Only the last field survives;
  the earlier edit is replaced by the frozen snapshot.
- **§2 R1 root-cause cell**: the stale `effective` snapshot (`prices.go:94`, read again at
  `prices.go:101`) is the cause. It is not conditional on the model being new. The fresh-zero
  branch is one symptom of that lookup, not the cause.
- **§3.1**: the sentence that treated the existing-model path as already correct now splits the
  cases. A single `--set` against an existing effective rate is already correct. Two or more
  `--set`s in the same call against that model are not, and the `overrides[model]`-first lookup
  fixes them. The code snippet is unchanged.
- **§5.1 / §5.2 / §8 / §10**: added `TestPricesSetMultipleFieldsExistingModelInOneInvocation` —
  two or more `--set`s in one `runPrices` call against a shipped rate and against a previously
  overridden rate, asserting both field edits survive. Bead 01 now names four tests.
- **§5.4**: reverting the `overrides[model]` check must also fail the existing-model multi-set
  test. The brand-new-model failure alone does not prove that path.
- **§4.1**: `internal/store/store.go` is on the Modified list only if R4 / bead 05 ships
  (`RepriceCosts` has no model argument; `--model` bypasses the `repriceInScope` skip at
  `store.go:2281`). Same conditional-listing style as `merge.go`. Bead 05 stays the one to
  drop first.

### v5 (round-4 review applied)
F4.1, F4.2, F4.3, and F4.4 accepted (JUSTIFIED) and applied in full. Conductor overrides fixed
the wording. R4's priority was not raised; bead 05 stays first-to-cut. Round-3 edits were not
reopened, and the F2.1 priority rebuttal stays settled.

- **§5.2**: each prices test calls `withHome(t)` before `runPrices`. A prior-session override is
  seeded at `filepath.Join(home, ".clens", "prices.toml")`, and assertions read that file, not
  the real home directory. The brand-new-model test does not seed an override for the model
  under test.
- **§2 / §3.4 / §4.1 / §5.1 / §5.2 / §6 / §8 / §9**: `--model` is the narrower backfill. The pass
  skips every row whose `model_resolved` is not the named model, in-scope rows of other models
  included, and the candidate `SELECT` is filtered with `model_resolved = ?`.
  `TestRepriceModelFlagBackfillsOnlyNamedModel` asserts an in-scope row for a different model is
  not `Moved`. The untouched-history and no-contention promise in §2 and §9 stays. Bead 05 stays
  first-to-cut.
- **§3.4**: `--model` is taken off `args` with `takeFlag` before `openStore`, the same way
  `--dry-run` and `--yes` are removed.
- **§6 / §7**: the pre-flight checkpoint is bead 02's result and gates bead 03 only. Bead 01 stays
  independent, as §8 already says. §6 no longer says "before Bead 1's test."

### v6 (round-5 review applied)
F5.1, F5.2, and F5.3 accepted (JUSTIFIED) and applied in full. Conductor overrides fixed the
wording. R4's priority was not raised; bead 05 stays first-to-cut. The narrower `--model`
backfill from v5 was not reopened, and the F2.1 priority rebuttal stays settled.

- **§3.2 / §5.2 / §5.4 / §7 / §8**: `TestMergeEventsPrefersPricedOverUnpriced` sets
  `incoming.CaptureComplete = true` and an explicit `existing.CaptureComplete` (the ordinary
  complete proxy row is `true`), with non-zero tokens on both sides so the `usageObserved` swap
  does not apply, and asserts `CostSource` is the incoming priced source. An `'unpriced'` result
  counts as the cause only when that incoming flag is already true. A zero-value `false` is a
  broken fixture and must not start bead 03. The winner swap is not redesigned. A wholesale
  `usageObserved`-style swap also copies tokens and `ModelResolved` (`merge.go:292-306`); that
  note is recorded and the swap's design stays as it is.
- **§5.2**: the wildcard-bind case is `TestIngestRebuildWarnsWhenServeIsLive` (`ingest --rebuild`).
  The listener is loopback, the command passes `--dashboard-addr 0.0.0.0:<port>` and
  `--allow-remote`, and the proof is the request `Host` header `127.0.0.1:<port>`, matching
  `TestShutdownDialsLoopbackForAWildcardDashboardAddr`. A WARN line from a listener bound on
  `0.0.0.0` is not that proof. `--allow-remote` is required because `runIngest` calls
  `cfg.Validate()` and `0.0.0.0` fails `IsLoopbackHost` unless `AllowRemote` is set.
- **§5.2**: every new `runIngest` / `runReprice` / `runReflag` / `runPurge` test calls
  `withHome(t)` before the command. The ingest WARN test builds its JSONL tree under that temp
  home. The purge WARN test passes `--unpriced` or `--older-than` so it reaches the probe. The
  prices-test `withHome` paragraph is unchanged.

### v7 (round-6 review applied)
F6.1 accepted (JUSTIFIED) and applied in full. Conductor override fixed the verdict and the
wording. R4's priority was not raised; bead 05 stays first-to-cut. The narrower `--model`
backfill from v5 was not reopened, and the F2.1 priority rebuttal stays settled.

- **§3.4**: `reprice --model` sends the named model's previously-unpriced rows through
  `Table.Compute`. A class with tokens > 0 and a nil rate returns `(nil, "unpriced")` before the
  multiply at `internal/pricing/pricing.go:130` — the same answer as an absent model, not a
  partial sum and not a panic. `LoadOverrides` leaves `CacheReadRate` nil (`pricing.go:295-300`).
  `RepriceCosts` already skips a nil `usd` (`store.go:2286-2292`), so there is no store-side rate
  check. The guard lives in `Compute`, which the live capture path already calls
  (`internal/consumer/consumer.go:356-361`). It is not a reprice-only check.
- **§4.1**: `internal/pricing/pricing.go` is on the Modified list for that guard. Listing the
  file does not raise R4's priority.
- **§5.1 / §5.2 / §8**: `TestRepriceModelFlagBackfillsOnlyNamedModel` requires the from-scratch
  cache-read case — named model absent from the shipped table, `cache_read_tokens > 0`,
  `cache_read_rate` unset — so the command returns, the row stays `cost_source='unpriced'`, and
  both cost columns stay NULL. Input and output tokens alone do not satisfy it. The complete-rate
  subscription case, which lands the figure in `api_equivalent_cost_usd`, stays. Bead 05's
  priority is unchanged.

### v8 (round-7 review applied)
F7.1 accepted (JUSTIFIED) and applied in full. Conductor override fixed the verdict and the
wording. The nil-rate guard stays in `Table.Compute`. R4's priority was not raised; bead 05
stays first-to-cut. The narrower `--model` backfill from v5 was not reopened.

- **§5.1 / §5.2**: the from-scratch cache-read case seeds
  `filepath.Join(home, ".clens", "prices.toml")` with an override for a model `ShippedTable`
  does not contain, `input_rate` and `output_rate` set and `cache_read_rate` omitted, so the
  model is in the effective table with `CacheReadRate` nil and the row has
  `cache_read_tokens > 0`. A missing override row does not count. A direct `Table.Compute`
  assertion returns `(nil, "unpriced")` with no panic, in `internal/pricing/pricing_test.go`
  or against the table `runReprice` loaded. A `recover` around `RepriceCosts` does not satisfy
  it. The complete-rate subscription case stays on a row whose used classes all have non-nil
  rates (`cache_read_tokens == 0` when `cache_read_rate` is the unset class), and the figure
  still lands in `api_equivalent_cost_usd` with `cost_usd` left NULL.

