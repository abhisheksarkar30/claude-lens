<!-- version=1, status=planning -->
# GI-22 — `clens prices --set` clobbers a brand-new model's fields; rebuild-based backfill is unreliable and unguarded against a live `serve`

Issue: https://github.com/abhisheksarkar30/claude-lens/issues/22
Branch: `GI-22-prices-set-clobber-and-rebuild-backfill-gaps` (off `main`)
Beads: `.beads/GI-22/` (filled in at Phase 3)

## 1. Problem

Four related defects surfaced while adding rates for two previously-unpriced models
(`claude-sonnet-5-5`, `claude-haiku-4-5-20251001`) through the documented CLI path:

- **R1 (defect, reproduced)**: `clens prices --set model:field=value` repeated for the *same
  brand-new model* in one invocation silently drops every field but the last.
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
| R1 | `clens prices --set` must apply every field given for a model in one invocation, not just the last | `internal/cli/prices.go:89-113` (`applyPriceEdits`): `effective := pricing.NewLoader(path, nil).Table()` is a single snapshot taken *before* the `--set` loop. For a model absent from `effective` (no shipped/prior-override rate), each iteration falls into `r = pricing.Rate{Model: model}` — a fresh zero value — because the lookup never sees what the *previous* iteration of this same loop already wrote into `overrides[model]`. Reproduced: 5 `--set`s for `claude-sonnet-5-5` in one call left only `cache_write_1h_rate` set. | Check `overrides[model]` before falling back to `effective[model]` (§3.1) |
| R2 | Adding a rate for a model must apply to that model's already-captured rows via `ingest --rebuild`, as documented | **Hypothesis, unchecked** — `internal/store/merge.go`'s `mergeEvents` picks its cost-column winner (`CostUSD`/`ApiEquivalentCostUSD`/`CostSource`, lines 299-301) from a `winner` selected by capture-completeness (lines 250-290), not by pricing freshness. Since JSONL rows set `CaptureComplete=true` unconditionally (`internal/jsonlogs/jsonlogs.go:462`), the completeness branch should normally prefer the freshly-recomputed `incoming` row — but this was not confirmed against the 6 stuck rows; `is_sidechain` correlation and cross-file `request_id` duplication were both ruled out as the cause. | Bead 1 reproduces the mechanism with a unit test before any fix is designed (§3.2) |
| R3 | A long write-heavy CLI command should not silently degrade a live `serve`'s capture | `internal/cli/restart.go:152` already probes `GET /api/health` on the dashboard address to decide liveness — the pattern exists, just isn't reused by `ingest`/`reprice`/`reflag`/`purge`. `storage-schema.md:280` and `workflows.md:194` document `SetMaxOpenConns(1)` as a *per-process* writer-serialization invariant; neither claims cross-process safety, so this is an undocumented gap, not a stale doc. | Reuse the existing health-probe pattern as a pre-flight warning (§3.3) |
| R4 | A newly-priced model's already-captured history should be backfillable without touching unrelated history or contending with `serve` | `internal/store/store.go:2140-2146` (`repriceInScope`) deliberately excludes `cost_source='unpriced'` rows from `reprice` — "never newly priced even where the model resolves today" is a stated invariant, not an oversight. No narrower tool exists. | Opt-in `--model` scoping on `reprice` that explicitly overrides the exclusion for named models only (§3.4) — lowest priority, see §9 |

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
picks up whatever the *previous* `--set` in this call already produced, while still falling back to
the shipped/effective rate (and then to a fresh zero value) exactly as before for the cases that
already work correctly. No change to `setRateField`, `splitSet`, or the unset path.

### 3.2 R2 — reproduce the merge mechanism before designing a fix

New test in `internal/store/merge_test.go` (file exists — `merge.go`'s sibling): construct an
`existing` event with `cost_source='unpriced'`, realistic non-zero tokens, and
`model_resolved='claude-haiku-4-5-20251001'`; construct an `incoming` event with identical token
columns but computed against a `Table` that now carries a real rate for that model (i.e.
`CostUSD`/`ApiEquivalentCostUSD` set, `CostSource='user'`). Call `mergeEvents(existing, incoming)`
and assert on the result's `CostSource`.

- If the result is `'user'` (priced) — the completeness-based winner pick is *not* the cause, and
  R2's real mechanism is still open; the next diagnostic step is instrumenting
  `internal/cli/ingest.go`'s rebuild path directly (not more live-DB queries) to log which of the
  two `mergeEvents` branches fires for a row that ends up stuck.
- If the result is `'unpriced'` (stuck) — the hypothesis in §2 is confirmed, and the fix is to
  stop keying the cost-column pick to `CaptureComplete`/`usageObserved` alone: a `winner` whose
  `CostSource == "unpriced"` should lose to a loser that priced successfully, mirroring the
  existing `usageObserved` swap-back at `merge.go:280-290`.

This bead's outcome is binary and gates whether a code fix for R2 exists in this ticket at all, or
whether R2 is re-scoped to "diagnosed, fix tracked separately" — see §7.

### 3.3 R3 — pre-flight live-serve warning

`internal/cli/restart.go:152`'s `GET /api/health` probe (short timeout, dashboard address from the
resolved config) is extracted into a small shared helper (e.g. `internal/cli/livecheck.go`,
`probeServeHealth(dashboardAddr string, timeout time.Duration) bool`) and called from `runIngest`
(`internal/cli/ingest.go`, only on `--rebuild`), `runReprice`, `runReflag`, and `runPurge` — each
already has the resolved `cfg.DashboardAddr` in hand after `config.Load`. On a live hit, print a
`WARN`-level line to the same writer the command already uses (matching `doctor`'s PASS/WARN/FAIL
vocabulary) naming the contention risk, and proceed — this is a warning, not a new confirmation
gate, so it does not change any of the four commands' existing `--yes` semantics.

### 3.4 R4 — scoped reprice backfill (lowest priority; see §9 before committing to this)

`clens reprice --model <name>` (new flag, `internal/cli/reprice.go`): when set, passes the name
through to `store.RepriceCosts` so `repriceInScope` is bypassed *only* for `cost_source='unpriced'`
rows whose `model_resolved == name` — every other unpriced-row exclusion stays intact. Printed
output follows `applyPriceEdits`'s existing pattern of saying plainly that scope was widened
("`--model claude-haiku-4-5-20251001` is backfilling previously-unpriced rows for this model only"),
so a reader of `clens reprice --dry-run` output is never surprised by rows moving that the
unqualified command would have left alone.

## 4. Change list

### 4.1 Files
Modified: `internal/cli/prices.go`, `internal/cli/ingest.go`, `internal/cli/reprice.go`,
`internal/cli/reflag.go`, `internal/cli/purge.go`, `internal/store/merge.go` (only if §3.2 confirms
the hypothesis)
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
| R1 regresses for the already-working case (existing shipped/override model) | New test also covers a single `--set` against an existing shipped model, asserting untouched fields are preserved |
| R3's warning becomes a hard failure and blocks legitimate single-user, serve-not-running usage | Test asserts the probe is skipped/false when nothing listens on the dashboard address, and the command proceeds unchanged |
| R4 widens scope beyond the named model | Test asserts an unpriced row for a *different* model is still skipped under `--model` |

### 5.2 New tests
- `TestPricesSetMultipleFieldsNewModelInOneInvocation` (`internal/cli/prices_test.go`) — five
  `--set`s for one brand-new model in one `runPrices` call; asserts all five fields are present in
  the resulting table.
- `TestPricesSetSingleFieldExistingModelUnaffected` (same file) — guards the already-correct path.
- `TestMergeEventsPrefersPricedOverUnpriced` (`internal/store/merge_test.go`) — the §3.2
  reproduction; its outcome decides whether a `merge.go` fix bead exists.
- `TestIngestRebuildWarnsWhenServeIsLive` / equivalents for `reprice`/`reflag`/`purge`
  (`internal/cli/*_test.go`) — fake listener standing in for `serve`'s health endpoint.
- `TestRepriceModelFlagBackfillsOnlyNamedModel` (`internal/cli/reprice_test.go`) — only if R4 is
  built this ticket (§9).

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
  symptom (only the last of five fields present).
- R2 (if a fix lands): revert the winner-pick change in `mergeEvents` —
  `TestMergeEventsPrefersPricedOverUnpriced` must fail by returning `'unpriced'` instead of the
  priced source.
- R3: skip starting the fake listener — the warning test must fail to find the `WARN` line in the
  command's output.

## 6. Risk areas
- **R2 — the hypothesis may be wrong.** Do **not** write a `merge.go` fix before Bead 1's test
  result comes back. If the reproduction shows the completeness-based pick is *not* the cause,
  stop and re-scope R2 to a diagnosis-only outcome for this ticket (§7) rather than guessing at a
  second mechanism under time pressure.
- **R3 — do not turn a warning into a hard gate.** `ingest --rebuild`/`reprice`/`reflag`/`purge`
  must still run with no `serve` listening (the single-user common case); the probe only adds a
  printed line when a listener answers, never a new confirmation prompt or exit code change.
- **R4 — scope creep risk.** If built, `--model` must not become a general "reprice unpriced
  rows" escape hatch — it has to require the exact model name, matching `model_resolved` verbatim.

## 7. Pre-flight (needs a decision before bead 01)

Bead 1 (§3.2's reproduction test) decides whether R2 ships a code fix in this ticket or is
re-scoped to "diagnosed, mechanism documented, fix tracked separately" if the hypothesis is wrong.
Whoever picks this plan up in the next session should treat that bead's result as a checkpoint
before beadifying the rest of R2, not assume the fix in §3.2 is already correct.

## 8. Beads

| # | Bead | Priority | Depends on | Purpose |
|---|---|---|---|---|
| 01 | `fix-prices-set-merge-order` | P0 | — | R1: the `overrides[model]`-first lookup fix, plus its two tests |
| 02 | `test-merge-events-cost-winner-reproduction` | P0 | — | R2 diagnostic: the `mergeEvents` reproduction test from §3.2; its result gates bead 03 |
| 03 | `fix-merge-events-cost-winner` | P1 | 02 (only if confirmed) | R2 fix: stop keying the cost-column winner to completeness alone, if bead 02 confirms the hypothesis |
| 04 | `feat-livecheck-warn-write-commands` | P1 | — | R3: extract the health-probe helper and wire it into `ingest --rebuild`, `reprice`, `reflag`, `purge` |
| 05 | `feat-reprice-model-scoped-backfill` | P2 | — | R4: the opt-in `--model` flag on `reprice` |

Beads 01, 02, and 04 are independent and can run in any order or in parallel. Bead 03 is
conditional on bead 02's finding — if the hypothesis is wrong, close bead 03 as "not applicable,
see bead 02's result" rather than forcing a fix. Bead 05 is the lowest-priority, most optional
bead in this set (see §9) and is the one to drop first if this ticket needs to ship smaller.
No bead is destructive; none touches migrations, schema, or already-captured data directly — R4
only widens what a future, explicit `--model` invocation can touch, and ships with no default
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
a sibling command for the same operation on a narrower row set duplicates the dry-run/`--yes`
plumbing for no real gain. Revisit only if `--model`'s semantics turn out to need a genuinely
different confirmation flow than `reprice`'s existing one.

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
add it as a third case rather than assuming the fresh-model test covers it by extension.

**Security engineer.** No new trust surface: no network input, no auth, no new persistence beyond
what `reprice`/`ingest` already write, and `--model` in R4 only narrows an existing write path's
scope rather than widening who can trigger it (still local-CLI-only, same as every other `clens`
subcommand).

## 11. Context docs to refresh (running list)
- `docs/context/cli-and-tooling.md:19` — currently states `ingest --rebuild` "...so an added rate
  row or a changed prefix is applied to rows already captured without duplicating them" as an
  unqualified guarantee. Once bead 02/03 lands (or R2 is re-scoped per §7), this line needs either
  a correction describing the actual guarantee, or a caveat about the merge winner-pick condition
  under which it does not hold.

## Change History

### v1 (draft)
Written from this session's live investigation of claude-lens's pricing CLI and the `ingest
--rebuild` backfill path: the R1 bug was reproduced directly, R2 was investigated but not
root-caused (two hypotheses ruled out, one left open), and R3 was reproduced by running `ingest
--rebuild` against a live `serve`. Filed as GitHub issue #22 per Phase 1 checkpoint with the user,
who confirmed keeping all four items in one ticket. Not yet cross-reviewed — implementation to
proceed in a later session starting from Phase 2.5.
