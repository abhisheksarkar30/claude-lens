# Bead br-GI-1-14: Ingest orchestration, scheduler, collector isolation, per-source health

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Invariant 6, §The four sources ("independent by construction"), §API surface (`/api/sources`), test 17

- **Bead ID**: br-GI-1-14
- **Priority**: P1 (high)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-11, br-GI-1-12, br-GI-1-13
- **Blocks**: br-GI-1-15, br-GI-1-17, br-GI-1-18

## Description

The orchestration of the three non-proxy collectors, the scheduler that runs them, and the per-source
health surface that makes their failures visible.

**`internal/ingest`.**

- `RunOnce(ctx) error` runs every non-proxy collector once: the JSONL tail (br-GI-1-11), quota
  snapshots (br-GI-1-12), and the admin pull (br-GI-1-13). It is the entry point for `clens refresh`,
  `POST /api/ingest` (br-GI-1-18) and the scheduler.
- **Invariant 6 extended — a broken collector never prevents the others from writing and never
  crashes the process.** Each collector recovers its own panics; a failure in one leaves the others
  running and is recorded as that source's error state, not as a process failure.
- **Per-source health.** For each of the four sources (`proxy`, `jsonl`, `snapshot`, `admin`) report:
  last success, last error, rows written, cursor position, and status — read from `ingest_state`
  (br-GI-1-06) plus the collectors' own counters. This is what makes the tech plan's core promise
  ("if one collector breaks, the others keep working") trustworthy rather than aspirational: a dead
  collector is a visible red row, not a quietly short chart.
- **Scheduler.** A ticker that runs the collectors on an interval, started by `serve` (br-GI-1-17).
  It must be interruptible and must not stack runs if one is slow (skip or serialize, not overlap).
- Persist each collector's outcome to `ingest_state` (`status`, `error`, `updated_at`), so a failure
  survives a restart and `doctor` can report it.

**`doctor` extension** — some of br-GI-1-01's doctor output needs this bead: the per-source health
section is added here (it reads `ingest_state`), and the port-collision and secret-protection checks
from br-GI-1-01 are unchanged.

## Rationale

The four sources are only independent by construction if something enforces it. Isolation is the
headline promise of the tech plan, so it is implemented as per-collector recovery plus a per-source
health surface, and it is tested rather than asserted.

## Outcome Definition

- `go test ./internal/ingest/... -race` passes.
- With snapshots failing, JSONL and proxy ingestion continue and `GET /api/sources` shows the failure
  (test 17).
- A panic inside one collector is recovered, recorded as that source's error, and the others still run.
- `RunOnce` runs all three collectors and persists their outcomes.
- The scheduler does not stack runs.
- `doctor` reports per-source health after this bead.

## Test Specifications

- Unit Tests (`internal/ingest/ingest_test.go`):
  - **Test 17 — collector isolation**: with the snapshot collector forced to fail, the JSONL and admin
    collectors still run and write, and the health surface shows `snapshot` failed with a reason.
  - A panicking collector is recovered; the others complete; the panic is recorded as its error.
  - `RunOnce` persists a status and `updated_at` per collector into `ingest_state`.
  - Health output includes last success, last error, rows written, cursor position and status per source.
  - The scheduler skips a tick while a run is in flight.
  - Cancelling the context stops the scheduler within a bound.
- Integration Tests: `GET /api/sources` (br-GI-1-18) renders the failure.
- E2E: none.

## Files to Touch

- `internal/ingest/ingest.go`, `internal/ingest/ingest_test.go` (create)
- `internal/ingest/scheduler.go` (create)
- `internal/store/store.go` (modify — `ingest_state` read helpers for the health surface)
- `internal/cli/doctor.go` (modify — per-source health section)
