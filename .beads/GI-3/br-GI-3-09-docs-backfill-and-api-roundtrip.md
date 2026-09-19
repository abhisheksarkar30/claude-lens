# Bead br-GI-3-09: Backfill documentation (D7) + the `peak_multiplier`-absence/round-trip guard (T16)

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D7, §3 D8 F2.6, §4 (`api/prices.go`, `api/prices_test.go`, `docs/context/*`), §5 T16, §6 R7 (plan sketch §9 bead 7)

- **Bead ID**: br-GI-3-09
- **Priority**: P2 (medium)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-3-04, br-GI-3-08
- **Blocks**: None

## Description

Two closing pieces: the re-pricing runbook, and the guard that keeps `Peak` out of the API
round-trip.

**1. Backfill documentation (D7).** The ~56,984 existing rows are re-priced by running
`clens ingest --rebuild`, which re-reads every transcript from byte 0; the `request_id` UNIQUE
constraint absorbs the re-read as a merge (idempotent — no schema change, no destructive
operation). This is **documentation plus a bead ordering constraint — not new code**; there is no
reprice function to write, and adding one would duplicate the path.

Document, in `docs/context/cost-and-quota.md` (and a short note in `README.md` where the ingest
command is described):

- That `clens ingest --rebuild` is the re-pricing path after a price/routing change.
- That **br-GI-3-08's merge fix must land first** (R7): running the backfill before the merge fix
  would make the backfill itself manufacture the invariant-5 violation across the rows.
- That the run requires no config edit — the shipped `{"deepseek-"}` prefix default routes the rows
  to `api` (br-GI-3-05/07).
- That the run is idempotent via the `request_id` UNIQUE constraint.

(Phase 5.6's `docs/context/*` refresh is a separate Phase-5.6 activity; this bead writes the D7
runbook content it can, and the refresh regenerates the surrounding doc set.)

**2. No `peak_multiplier` on the API surface (F2.6).** `Peak` is config-derived, so a settable
`peak_multiplier` would silently no-op **and** break the GET→POST round-trip, which decodes with
`DisallowUnknownFields` (`internal/api/prices.go:82-88`). Therefore:

- **Do not** add a `peak_multiplier` field to `priceModel` (`api/prices.go:16-26`) or to
  `setPricesRequest` (`api/prices.go:52-61`).
- The GET row must render no `peak_multiplier`, and a GET→POST of that row must decode cleanly.

`Peak` remains visible via `clens doctor` (br-GI-3-05) and is not part of the price row.

**Invariant 5.** The round-trip guard must not turn a rendered `null` rate into a numeric `0` on
POST: omitted and explicit-null both decode to a nil pointer and mean "unset" (`api/prices.go`'s
existing contract). This bead asserts the shape is preserved, it does not change it.

## Rationale

An unreachable runbook is not a deliverable, and a `peak_multiplier` field the user could set but
that never took effect (and that 400s the Settings tab's own round-trip) is worse than no field.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `docs/context/cost-and-quota.md` (and the README note) document the `clens ingest --rebuild`
  backfill, its prerequisites, and its idempotence.
- GET a model row from `GET /api/prices` and POST it back verbatim → **200** (no `400` naming an
  unknown field); the row carries **no** `peak_multiplier` (T16).
- No `peak_multiplier` field exists on `priceModel` or `setPricesRequest`.

## Test Specifications

- Unit Tests (`internal/api/prices_test.go`):
  - **T16**: GET a model row and POST it back verbatim → 200; assert the row has no
    `peak_multiplier` key and that a POST of the GET body does not 400 on an unknown field.
- Unit Tests (`internal/pricing` / docs): the shipped rows are still visible via `doctor`'s
  `peak_off_peak_dates` row (covered in br-GI-3-05's T15).
- Integration Tests: none.
- E2E: none (the plan accepts no E2E `--rebuild` against a real 57k-row store).

## Files to Touch

- `internal/api/prices.go` (verify/modify — confirm no `peak_multiplier` on `priceModel` /
  `setPricesRequest`; comment the F2.6 rationale)
- `internal/api/prices_test.go` (modify — T16)
- `docs/context/cost-and-quota.md` (modify — the D7 backfill runbook)
- `README.md` (modify — a short note at the ingest/cost section)
