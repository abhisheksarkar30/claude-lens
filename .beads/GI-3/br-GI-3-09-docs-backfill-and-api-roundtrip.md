# Bead br-GI-3-09: Backfill documentation (D7) + the `peak_multiplier`-absence/round-trip guard (T16)

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D7, §3 D8 F2.6, §4 (`api/prices.go`, `api/prices_test.go`, `README.md`), §5 T16, §6 R7 (plan sketch §9 bead 7)

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

Document in `README.md`, where the `clens ingest` command is described (`README.md:90`, beside the
`clens prices` row at `:104`):

- That `clens ingest --rebuild` is the re-pricing path after a price/routing change.
- That **br-GI-3-08's merge fix must land first** (R7): running the backfill before the merge fix
  would make the backfill itself manufacture the invariant-5 violation across the rows.
- That the run requires no config edit — the shipped `{"deepseek-"}` prefix default routes the rows
  to `api` (br-GI-3-05/07).
- That the run is idempotent via the `request_id` UNIQUE constraint.

**Not `docs/context/`.** That tree is *generated* — Phase 5.6 runs `document-project-context` in
REFRESH mode across it — so a runbook hand-written there is clobbered by the next refresh. The
README is hand-maintained and already carries the command table, so it is where a runbook survives.

**2. No `peak_multiplier` on the API surface (F2.6).** `Peak` is config-derived, so a settable
`peak_multiplier` would silently no-op **and** break the GET→POST round-trip, which decodes with
`DisallowUnknownFields` (`internal/api/prices.go:82-88`). Therefore:

- **Do not** add a `peak_multiplier` field to `priceModel` (`api/prices.go:16-26`) or to
  `setPricesRequest` (`api/prices.go:52-61`).
- The GET row must render no `peak_multiplier`, and POSTing that row's **rate fields** back must
  decode cleanly.

**The round-trip is of the rate fields, not the whole row — matching the real client.** `priceModel`
carries `source` (`api/prices.go:25`, no `omitempty`) and `setPricesRequest` has no such field
(`:52-61`), while `setPrices` decodes with `DisallowUnknownFields` (`:84`). POSTing a GET body
**verbatim** therefore 400s on `unknown field "source"` — and always has, independent of this story.
The dashboard does not do that: it rebuilds `payload` from the rate inputs alone
(`internal/web/app.js:560-563`). T16 mirrors the dashboard, and the bead's `setPricesRequest` doc
comment, which currently claims a client "can POST back the row it fetched" (`:45-48`), is corrected
to match. `source` is server-derived provenance (shipped / provisional / user), deliberately **not**
client-settable — widening `setPricesRequest` to accept it would be the wrong repair, letting a POST
forge its own provenance.

`Peak` remains visible via `clens doctor` (br-GI-3-05) and is not part of the price row.

**Invariant 5.** The round-trip guard must not turn a rendered `null` rate into a numeric `0` on
POST: omitted and explicit-null both decode to a nil pointer and mean "unset" (`api/prices.go`'s
existing contract). This bead asserts the shape is preserved, it does not change it.

## Rationale

An unreachable runbook is not a deliverable, and a `peak_multiplier` field the user could set but
that never took effect (and that 400s the Settings tab's own round-trip) is worse than no field.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `README.md` documents the `clens ingest --rebuild` backfill, its prerequisites, and its
  idempotence.
- GET a model row from `GET /api/prices` and POST its rate fields back → **200** (no `400` naming an
  unknown field, `peak_multiplier` included); the row carries **no** `peak_multiplier` (T16).
- No `peak_multiplier` field exists on `priceModel` or `setPricesRequest`.

## Test Specifications

- Unit Tests (`internal/api/prices_test.go`):
  - **T16**: GET a model row; assert it carries no `peak_multiplier` key; POST a body built from
    that row's rate fields (dropping `source`, as `app.js:560-563` does) → 200. Add the negative
    half so the guard has teeth: the GET row posted **verbatim** is asserted to 400 on `source`,
    which pins the not-client-settable contract instead of merely tolerating it, and stops a future
    reader from "fixing" the 400 by widening `setPricesRequest`.
- Unit Tests: **none added here.** `doctor`'s `peak_off_peak_dates` row is br-GI-3-05's T15; this
  bead does not duplicate it, and it adds no test outside `internal/api`.
- Integration Tests: none.
- E2E: none (the plan accepts no E2E `--rebuild` against a real 57k-row store).

## Files to Touch

- `internal/api/prices.go` (verify/modify — confirm no `peak_multiplier` on `priceModel` /
  `setPricesRequest`; comment the F2.6 rationale; correct `setPricesRequest`'s doc comment, which
  claims to be "in exactly the shape GET returns that model's row in" and that a client "can POST
  back the row it fetched" — false for `source`, `:45-48`)
- `internal/api/prices_test.go` (modify — T16)
- `README.md` (modify — the D7 backfill runbook, at the `clens ingest` row, `README.md:90`)
