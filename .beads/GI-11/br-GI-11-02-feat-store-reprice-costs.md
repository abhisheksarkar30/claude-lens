# Bead br-GI-11-02: `Store.RepriceCosts` — the tx-taking reprice write loop, behind the store/pricing seam

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §2 RC-A, §4 Code (the
`internal/store` new-method row, the `internal/store/importguard_test.go` row and the
`internal/store/reprice_test.go` row), §5 Unit (the `Reprice` bullet and the `Store import boundary`
bullet), §6 (the `user`-override and `approximate:cache_ttl_unknown` risk rows), §7 QA

- **Bead ID**: br-GI-11-02
- **Priority**: P0 (critical)
- **Original Estimate**: 5h
- **Dependencies**: br-GI-11-01 (the corrected `Compute` is what the reprice tests assert against)
- **Blocks**: br-GI-11-03 (the `clens reprice` CLI shell calls this method), br-GI-11-11 (the test-file
  count moves by this bead's two new files)

> **This bead is a transaction shape and a seam, not a loop.** Two constraints make or break it, and
> both fail **silently** if missed: the whole reprice is **one** `BeginTx`, and `internal/store` must
> **not** import `internal/pricing`. That second one is enforced by a source-reading test, because the
> compiler cannot see it.

> **Do NOT call `ReconcileSession` from inside this transaction.** It opens its **own** `BeginTx`
> (`store.go:695-705`) and the pool is pinned to one connection (`SetMaxOpenConns(1)`, `store.go:76`),
> so a nested `BeginTx` blocks on the pool with **no deadline — a hang, not an error**, which no test
> reports as a failure rather than a timeout. Use the tx-taking `reconcileSessionTx`. That it is
> unexported, and therefore unreachable from `internal/cli`, is exactly why the write loop must live in
> `internal/store` — the same reasoning GI-9's plan records.

## Description

### The seam — the store computes through an injected interface, never by importing `pricing`

`internal/store` must **not** import `internal/pricing`. This is a hard constraint, not a preference:
the store is the write authority, and the pricing engine stays behind a seam exactly as
`internal/proxy` depends only on `sink` and `config` (that package's doc names `pricing` as a forbidden
import). Reuse the repo's **existing** local mirror interface — `PriceComputer` at
`internal/consumer/analyzer.go:35-40`:

```go
Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd *float64, costSource string)
```

`internal/store` already imports `internal/parse` (`store.go:23`), so it declares its own copy of that
method set, and the CLI passes the **effective Loader-backed table** into `RepriceCosts`. The store
calls `Compute` per row **inside its transaction**, importing nothing from `pricing`. A
precomputed-slice alternative (the CLI computes old→new values in its loop and passes them plus the
distinct session ids) is acceptable, but **the mirror interface is the named choice** — it matches the
existing precedent.

**The constraint is pinned by a failing test, not left as prose.** Add
`internal/store/importguard_test.go`, mirroring `internal/proxy/importguard_test.go:26-49`
one-for-one: `filepath.Glob("*.go")`, skip `_test.go`, `parser.ParseFile(…, parser.ImportsOnly)`,
forbid `/internal/pricing`. `TestStoreDoesNotImportPricing` is what makes the "just import `pricing`
directly, it is the same call" simplification fail the build — the compiler cannot catch an
`import "…/internal/pricing"` in `internal/store`.

### The write loop — one transaction, store-owned

`Store.RepriceCosts(ctx, table, ...)` in a **single** `BeginTx`:

1. Select the rows **in scope** and price each with the injected `Compute`. The scope, per §4:
   - **Price every row whose `model_resolved` resolves in the effective table.**
   - A `user` row **is included** — its cost was produced by the same `Compute` with the same per-class
     rounding, so RC-A corrupted it identically, and against the effective table it is reconstructible
     (its rate lives in the override file). "The user overrode it" is not a reason to skip it.
   - An **`approximate:cache_ttl_unknown`** row is **in scope, not skipped**: the label is a stored,
     **indexed** column (`schema.sql:45`, `:70`), so it is the reconstruction **key** — a row carrying
     it proves `usage.TTLUnknown` was `true` at insert, so reprice sets `TTLUnknown = true`, calls
     `Compute`, and gets back both the corrected amount **and the same label**
     (`pricing.go:138-140`).
   - The **only** skip is a row whose computed inputs cannot be reconstructed from stored columns —
     today that is **`unpriced` alone** (`model_resolved` absent from the table,
     `pricing.go:93-94`). Such rows keep their stored cost and `cost_source`, **never nulled**, and are
     counted **skipped**, so `reprice --yes` is not silently destructive on exactly the rows it cannot
     price. Any *future* `approximate:<reason>` other than `cache_ttl_unknown` joins that skip list for
     the same reason — spell the filter **by reason**, not by the `approximate:` prefix.
2. `UPDATE events SET cost_usd / api_equivalent_cost_usd / cost_source …` for the rows whose value
   moved. **Routing is invariant 5**, exactly the switch at `consumer.go:321-326` and
   `jsonlogs.go:461-466`: a `subscription` row's new figure goes to `api_equivalent_cost_usd` and
   leaves `cost_usd` NULL; any other `billing_mode` goes to `cost_usd`.
3. Re-derive **every distinct owning `session_id` whose cost columns moved** via `reconcileSessionTx`,
   **before** `Commit` — mirroring `InsertEvents`' distinct-session loop (`store.go:301-316`). The
   `sessions.total_cost_usd` / `total_api_equivalent_cost_usd` columns are *materialized* and
   re-derived, never incremented; `reconcileSessionTx`'s own routing is the
   `SUM(CASE WHEN billing_mode='api' THEN cost_usd END)` /
   `SUM(CASE WHEN billing_mode='subscription' THEN api_equivalent_cost_usd END)` pair at
   `store.go:731-732`. Without this, `clens stats`, `clens sessions` and `/api/sessions` keep showing
   the pre-fix session totals while `events` is corrected.
4. `Commit` once.

**The CLI must price with `newPriceLoader(cfg).Table()`, never the `nil` form** — but that is
br-GI-11-03's concern; this bead consumes whatever table it is handed. The store method's own contract
is the interface above.

### What this bead does not do

- **No `--dry-run` flag here.** `--dry-run` is a flag on the CLI shell (br-GI-11-03), not an argument
  to `RepriceCosts`, so it cannot be pinned from `internal/store`; its "changes nothing" case lives in
  `internal/cli/reprice_test.go`.
- **No CLI, no dispatch entry, no reporting.** The command is br-GI-11-03; the dispatch registration is
  br-GI-11-09.
- **No schema change and no marker column.** Every input `Compute` needs — model, the five token
  columns, `speed`, `service_tier`, `started_at`, `billing_mode` — is already a column on the row.
- **No `internal/pricing` import.** See the seam above; the import guard test is the enforcement.
- **No touch of `internal/cli/cli_test.go`.** Its GI-11 changes (the `reflag` `--dry-run` case and both
  credential-map entries) are br-GI-11-06's single edit to that shared file.

## Rationale

RC-A leaves every stored cost wrong, so without a reprice path the fix is invisible for all historical
data — including the 2026-09-21 view that produced the report — and the operator's next action would be
a manual `sqlite3` session. It is bounded (the pricing package is pure, every input is a stored column)
and idempotent on every row it rewrites: recomputing a priced row yields the same answer, so a second
`--yes` run is a no-op — with the one `unpriced` carve-out above, which is left untouched so a repeated
run reports it as skipped rather than destroying a previously stored figure.

The write loop lives in `internal/store` because `reconcileSessionTx` is unexported: a CLI-side loop
could not re-derive the sessions in the same transaction, and a nested `BeginTx` would hang.

## Outcome Definition

- `Store.RepriceCosts` exists, takes the effective pricing table behind the mirror interface, and
  imports nothing from `internal/pricing`.
- It performs the `events` UPDATE, the distinct-session `reconcileSessionTx` rollup, and the `Commit`
  in **one** transaction; a failure rolls all of it back.
- `internal/store/importguard_test.go`'s `TestStoreDoesNotImportPricing` passes, and fails if
  `internal/store` ever imports `internal/pricing` (verify by temporarily adding the import).
- Routing follows invariant 5: a `subscription` row's new figure lands in `api_equivalent_cost_usd`
  with `cost_usd` NULL; any other mode lands in `cost_usd`.
- A `user` row is repriced against the effective table (its override rate applies), not skipped.
- An `approximate:cache_ttl_unknown` row is repriced with its `cost_source` **preserved**.
- A row whose `model_resolved` no longer resolves keeps its stored cost and `cost_source` and is
  counted skipped — never nulled.
- A row priced under a **non-default** `PeakOffPeakDates` recomputes to a different value than the
  shipped calendar gives — the case a `nil`-form caller would pass and therefore never catch.
- The owning session's `total_cost_usd` / `total_api_equivalent_cost_usd` move with the row in the
  same transaction.
- A `RepriceCosts` call against a **closed store** returns an error and writes nothing (no partial
  application) — §7's named error path, pinned rather than assumed.
- `go build ./...`, `go vet ./...`, `go test ./internal/store/` pass.

## Test Specifications

All in `internal/store/reprice_test.go` unless stated. **Name tests by their Go function name, never by
a plan line range.**

- **Unit Tests:**
  - `TestRepriceCostsPricesTheExactValue` — a fixture holding a known-wrong (zeroed) row reprices to
    the exact corrected value; this is the store-level form of §5's reprice case.
  - `TestRepriceCostsIncludesUserRows` — a `user`-sourced row reprices against the effective table
    (its override rate applies) rather than being skipped.
  - `TestRepriceCostsPreservesTheTTLUnknownLabel` — an `approximate:cache_ttl_unknown` row is
    repriced **and** its `cost_source` is the same label afterwards.
  - `TestRepriceCostsUsesTheConfiguredOffPeakCalendar` — a row priced under a non-default
    `PeakOffPeakDates` (the `none` spelling) recomputes to a **different** value than the shipped
    calendar gives; a `nil`-form implementation passes this and the case must not.
  - `TestRepriceCostsSkipsUnresolvableRows` — a row whose `model_resolved` no longer resolves keeps
    its stored cost **and** its `cost_source`, and the count reports it as skipped.
  - `TestRepriceCostsReducesTheOwningSessionTotal` — the F1.1 rollup: a fixture where a row's cost
    moves, asserting the **owning session's** stored `total_cost_usd` (or
    `total_api_equivalent_cost_usd` for a subscription row) equals the post-reprice `SUM` over its
    `events`, not the pre-fix value. Asserted at the store layer because the rollup is in the same
    transaction as the UPDATE.
  - `TestRepriceCostsRoutesSubscriptionToApiEquivalent` — invariant 5: a `subscription` row's new
    figure lands in `api_equivalent_cost_usd` and `cost_usd` stays NULL.
  - `TestRepriceCostsErrorsOnAClosedStore` — the **error path §7 names but §5 leaves uncovered**: close
    the store, call `RepriceCosts`, assert it returns a **non-nil error** and that the in-scope row's
    cost columns are **unchanged** (reopen the same DB path and read the row back). This is the failure
    mode a bulk rewrite must not have — a partial application — and the single `BeginTx` is what makes
    it impossible; §7 asserting it without coverage is exactly the "claim with no test" pattern the plan
    spent eight rounds eliminating. **If the case turns out not to be expressible in
    `internal/store/reprice_test.go`, say so in this bead and downgrade §7's claim rather than leaving
    the two disagreeing.**
  - In `internal/store/importguard_test.go`: `TestStoreDoesNotImportPricing` — the one-for-one mirror
    of `TestProxyImportsAreNarrow`.
- **Integration Tests:** none here. §5's acceptance #1 (the IST-day `SUM(cost_usd)` moving 0.82 →
  3.46 ± 0.05) and acceptance #2 (one affected session's stored total equals its post-reprice `SUM`)
  are run manually against a **frozen copy** of the live store (see br-GI-11-11 and §5's `VACUUM INTO`
  note); they are not in-repo tests.

## Files to Touch

- `internal/store/store.go` (modify — add `Store.RepriceCosts` and the local mirror of the
  `PriceComputer` method set; the rollup reuses the existing `reconcileSessionTx`. **Also touched by
  br-GI-11-05** — keep the two edits disjoint: this bead adds the reprice surface, that one adds
  `ReflagIncompleteCaptures`; do not reorder or reformat the other's region)
- `internal/store/importguard_test.go` (new — `TestStoreDoesNotImportPricing`, mirroring
  `internal/proxy/importguard_test.go:26-49`)
- `internal/store/reprice_test.go` (new — every store-level case above)
