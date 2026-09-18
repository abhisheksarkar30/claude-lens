# Bead br-GI-1-02: Non-blocking capture sink

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Invariants 1/6, §Carried over, test strategy 4

- **Bead ID**: br-GI-1-02
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-1-01
- **Blocks**: br-GI-1-03, br-GI-1-16

## Description

The bounded, non-blocking handoff between the hot proxy path and the cold consumer path. Carried over
essentially verbatim from deepseek-lens `internal/sink/sink.go`.

- `type CapturedCall struct` — the transport struct. It holds **only cheap references and
  already-read byte slices**; nothing in it may require further I/O to produce. Fields: `ID`,
  `StartedAt`, `TTFB`, `Duration`, `Method`, `Path`, `RemoteAddr`, `Status`, `AuthKind` (the
  credential-shape classification br-GI-1-03 derives), `ReqHeaders` / `RespHeaders` (already
  redacted by the caller), `ReqBody` / `RespBody` (the body-capture policy already applied by the
  caller), `CaptureComplete bool` (false when the body was partial/trimmed — the merge precedence
  flag), `RequestID` (the response-derived key br-GI-1-03 resolves), `Err error` (a transport failure).
- `New(capacity int) *Sink`, with `DefaultCapacity = 4096`.
- `Submit(call *CapturedCall) bool` — assigns `call.ID`, attempts a **non-blocking** send, and
  returns false when the channel is full. `Submit` never blocks and never spawns a goroutine.
  Dropping under load is expected, not a failure.
- `Drain() <-chan *CapturedCall`, `Close()`, and `Stats() (accepted, dropped uint64)` with both
  counters atomic.

The drop counter is the visible half of invariant 6 ("fail open"): a stalled or absent consumer must
never add latency to a client request, and a drop must be countable so `doctor` and
`GET /api/sources` can report it rather than hide it.

## Rationale

This is the seam that keeps the hot path hot. If `Submit` can ever block, a slow SQLite writer
becomes client-visible latency — the exact failure the TTFB hard gate in br-GI-1-03 exists to catch.
It is its own bead because it is the one component both paths touch and it has no dependencies
beyond Go's stdlib.

`ponytail:` a plain buffered channel with an atomic drop counter; no backpressure signalling, no
priority queue. Add either only if a drop is ever observed in practice.

## Outcome Definition

- `go test ./internal/sink/... -race` passes.
- With the consumer stopped, N writes complete without blocking and `Stats().dropped == N`.
- `Submit` never blocks even at capacity.
- A concurrent producer/consumer run under `-race` is clean.

## Test Specifications

- Unit Tests (`internal/sink/sink_test.go`):
  - **Non-blocking under a stopped consumer** (test 4): with capacity C and no drain, submit
    N > C calls; every `Submit` returns within a bounded time, and `dropped == N - C`.
  - Accepted counter increments for each accepted call; dropped for each rejected one.
  - `Submit` returns false exactly when the channel is full and true otherwise.
  - `Drain` delivers calls in submission order.
  - `Close` then `Drain` terminates (no goroutine leak — assert with a finalizer or a done channel).
  - `-race` clean with a concurrent producer and consumer.
- Integration Tests: none (br-GI-1-03 consumes it).
- E2E: none.

## Files to Touch

- `internal/sink/sink.go` (create)
- `internal/sink/sink_test.go` (create)
