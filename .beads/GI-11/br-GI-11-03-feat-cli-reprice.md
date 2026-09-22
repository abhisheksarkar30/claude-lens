# Bead br-GI-11-03: `clens reprice` — the CLI shell, priced with the effective Loader-backed table

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4 Code (the
`internal/cli/reprice.go` *(new)* and `internal/cli/reprice_test.go` *(new)* rows; the "Why a reprice
command is in scope" block), §5 Unit (the `Reprice` bullet's `--dry-run` sentence), §6 (the
`user`-override risk row), §7 (the rejects-alternatives paragraph), Change History v3 F2.1/v2 F1.1 and
v8 F6.1 (the `nil`-form reversal)

- **Bead ID**: br-GI-11-03
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-11-02 (`Store.RepriceCosts` is the write loop this shell drives)
- **Blocks**: br-GI-11-06 (it owns the shared `internal/cli/cli_test.go` credential map, which names
  `runReprice`), br-GI-11-09 (the dispatch registration names `cli.Reprice`), br-GI-11-11 (the README
  and `cli-and-tooling.md` name the command)

> **The pricing table is the effective, Loader-backed one — `newPriceLoader(cfg).Table()`, never the
> `nil` form.** `newPriceLoader` is *"the one loader shape every pricer in this process wants, so two
> pricers cannot disagree about the configured off-peak dates"* (`internal/cli/ingest.go:119-123`);
> `serve` builds its pricer from it (`serve.go:103`), and `openStore` already hands this CLI the full
> `*config.Config` (`format.go:198-208`). **This supersedes v3's F2.7, which wrote `nil`** — do not undo
> it (see §Change History, v8/F6.1).

> **`nil` is not "as configured".** `config_test.go:201-228` pins `nil` = *unset → the shipped 33-date
> list*, a state distinct from `[]` (the `none` spelling, "never off-peak"), and `resolvedOffPeakDates`
> (`pricing.go:405-416`) maps `nil` → shipped but a non-nil empty list → nothing off-peak. A `nil`-form
> reprice silently misprices every call on an install that has configured `peak_off_peak_dates`.

> **Do not use `pricing.Compute`.** It is `ShippedTable().Compute` (`pricing.go:76-82`) and would
> silently ignore both a user override and the configured calendar, mispricing precisely the rows a user
> tuned by hand.

## Description

`clens reprice` is **the CLI shell only** — flags, `--dry-run`/`--yes`, and reporting — exactly as
`purge` and `rekey` split their CLI from their store work (there is **no** `clens merge`; the store-side
merge runs only inside `insertOrMerge` and the rekey CLI, `merge.go:134`). The write loop lives in
`internal/store` (br-GI-11-02).

### The command's shape — the `purge`/`rekey` precedent and the shared "fixes history" shape

`reprice` and `reflag` (br-GI-11-06) are both "the command that fixes history", and they share a shape:
one job per command, `--dry-run` prints what `--yes` would change, and **nothing happens without
`--yes`**. Mirror `purge.go`'s shell:

- Take `--dry-run` and `--yes` with the existing `hasFlag`/`takeFlag` helpers.
- Refuse with a clear error when neither is passed (`purge`'s "refusing to … without `--yes` (add
  `--dry-run` to see what would go)").
- Open the store via `openStore(args)`, which already resolves the full `*config.Config`.
- `--dry-run` prints the three counts (priced/moved, skipped, unchanged) and writes nothing; `--yes`
  performs the reprice and prints what changed.

### The table this shell hands the store

Build it with `newPriceLoader(cfg).Table()` — i.e.
`pricing.NewLoader(pricing.DefaultPath(), cfg.PeakOffPeakDates).Table()`. Two things this rejects:

- The `nil` form (`pricing.NewLoader(pricing.DefaultPath(), nil).Table()`), which v3's F2.7 wrote and
  F6.1 overturned. It was copied from `models.go:34` / `prices.go:50,:94`, but those are a read-only
  catalogue print and a price-editor printing its own result — neither prices a stored call, so the
  off-peak calendar is irrelevant *there*. reprice **does** price stored calls, so dropping
  `cfg.PeakOffPeakDates` is exactly the disagreement the helper's comment forbids.
- `pricing.Compute`, which is `ShippedTable().Compute` and would ignore a user override entirely.

### What this bead does not do

- **No write loop.** The transaction, the invariant-5 routing and the session rollup are
  br-GI-11-02's.
- **No dispatch registration.** `cmd/clens/main.go` and `cmd/clens/main_test.go` are br-GI-11-09's
  single fan-in edit (see that bead for why the two new commands are registered together).
- **No `reflag`.** The historical `capture_complete` repair is its own command, br-GI-11-05/06.
- **No `internal/cli/cli_test.go` edit.** `reprice`'s entry in `TestNoCommandPrintsACredential`'s
  `runs` map is **br-GI-11-06's**, because that bead owns the shared file's GI-11 changes (it adds
  `reflag`'s `--dry-run` case and both credential-map entries in one edit) — which is why it depends on
  this bead.
- **No docs.** The `README.md` / `cli-and-tooling.md` / `INDEX.md` surfaces are br-GI-11-11's.

## Rationale

Without a reprice path the RC-A fix is invisible for every historical row and the operator's next step
is a manual `sqlite3` session. The alternative — a migration that recomputes on `Open` — was rejected
in §7: it would put the pricing table on `Open`'s path, make every boot O(events), and the repo's
existing shape for "rewrite stored rows" is an explicit `--dry-run`/`--yes` CLI command, now three
times over (`purge`, `rekey`, `reprice`) plus `reflag`. `clens ingest --rebuild` cannot stand in: a
re-ingest is a **JSONL** row, priced with `speed=""`/`serviceTier=""` (`jsonlogs.go:459`) and absorbed
by the `request_id` merge, which never replaces the proxy's bodies (`merge.go:323-328`) — so it cannot
reach the proxy-only rows that carry the zeroed costs.

## Outcome Definition

- `clens reprice` exists as `internal/cli/reprice.go`, with `runReprice(args, w)` reachable from a test
  as `purge`/`rekey` are.
- Without `--yes` (and without `--dry-run`) it refuses and changes nothing; with `--dry-run` it reports
  what `--yes` would change and writes nothing; with `--yes` it reprises.
- It prices with `newPriceLoader(cfg).Table()` — **not** the `nil` form and **not** `pricing.Compute`.
- `clens reprice --dry-run` against a fixture leaves the in-scope rows' cost columns **and** their
  owning sessions' totals unchanged.
- `go build ./...`, `go vet ./...`, `go test ./internal/cli/` pass.

## Test Specifications

All in `internal/cli/reprice_test.go`, mirroring `internal/cli/rekey_test.go`'s `--dry-run` cases.

- **Unit Tests:**
  - `TestRepriceDryRunChangesNothing` — `clens reprice --dry-run` reports what `--yes` would change and
    writes nothing: the in-scope rows' `cost_usd` / `api_equivalent_cost_usd` / `cost_source` and their
    owning sessions' `total_cost_usd` / `total_api_equivalent_cost_usd` are unchanged after the run.
    This is the one §5 reprice case that lives at the CLI level — `--dry-run` is a flag on the shell,
    not an argument to `Store.RepriceCosts`, so it cannot be pinned from `internal/store`.
  - `TestRepriceWithoutYesRefuses` — the command refuses (non-zero exit) and changes nothing when
    neither `--yes` nor `--dry-run` is passed.
  - `TestRepriceUsesTheConfiguredOffPeakCalendar` — with a config whose `PeakOffPeakDates` is the
    `none` spelling, a stored row recomputes to a value that differs from the shipped-calendar result;
    a `nil`-form shell fails this. (The store-level pin is br-GI-11-02's; this asserts the shell wires
    `cfg.PeakOffPeakDates` through rather than dropping it.)
- **Integration Tests:** none here. §5's acceptance #1/#2 are manual, against a frozen store copy
  (br-GI-11-11).

## Files to Touch

- `internal/cli/reprice.go` (new — the shell: `--dry-run`/`--yes`, `newPriceLoader(cfg).Table()`
  passed to `Store.RepriceCosts`, and the counts report; mirrors `purge.go:26-79`)
- `internal/cli/reprice_test.go` (new — the CLI-level cases above)
