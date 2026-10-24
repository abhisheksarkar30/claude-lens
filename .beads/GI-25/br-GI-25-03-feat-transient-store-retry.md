# Bead br-GI-25-03: Bounded retry for transient SQLite errors, with counters on `/api/health`

**Plan Reference**: `docs/planning/GI-25-local-llm-toggle-and-store-hardening.md` v8, §2 R3, §3.2, §4.2, §5.1, §5.2 (`TestIsTransient`, `TestHealthExposesStoreRetryCounters`, `TestWithRetryRetriesTransientOnly`, `TestWithRetryStopsOnCancel`, `TestWithRetryGivesUpOncePerFlush`), §5.4, §6 R3. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-25-03
- **Priority**: P1
- **Status**: pending
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: None
- **Commit**: `GI#25 feat: bounded retry for transient store errors (br-GI-25-03)`

## Description

**First step, before writing the matcher:** confirm two things empirically against the real driver (`modernc.org/sqlite`, imported at `internal/store/store.go:26`), because the plan's R3 (§2) states them as unverified:
1. The exact error string and accessor SQLite's NOMEM surfaces as. SQLite's `SQLITE_NOMEM` (primary code 7) is documented as `"out of memory"`, not `"cannot allocate N bytes"` (that phrase is a Go-runtime OOM panic message, a different failure entirely) — confirm this against `*sqlite.Error`'s `Code()` method (or equivalent field; check the `modernc.org/sqlite` package's exported error type) before writing `IsTransient`.
2. That `insertOrMerge`'s merge path (`internal/store/merge.go:115-134`: `INSERT` first via `insertEventTx`, then on a unique-constraint error a load via `getEventByRequestIDTx` and `applyMergeTx`, all on the one `*sql.Tx` passed in) is a single atomic transaction, so a caller retrying the whole `InsertEvent` call after a transient failure cannot double-apply a partial write — the failed attempt's transaction rolled back wholesale.

**New `internal/store/retry.go`:**

```go
func IsTransient(err error) bool
```

Matches SQLite primary result codes IOERR (10) and NOMEM (7) via `*sqlite.Error` from `modernc.org/sqlite` (`errors.As(err, &sqliteErr)`, then check `sqliteErr.Code() & 0xff` or whatever primary-code extraction the driver's error type actually exposes — confirm the exact accessor in step 1 above). **BUSY (5) is explicitly excluded**: `store.go:75` (`Open`'s DSN) already sets `_pragma=busy_timeout(5000)`, which blocks up to 5 s *inside* a single attempt; adding an explicit BUSY retry on top would multiply the maximum per-call block time by the retry attempt count, which is the wrong trade for a lock-contention case `busy_timeout` already handles. `IsTransient` must not misclassify a wrapped constraint-violation error (e.g. the unique-constraint error `isUniqueConstraintError` at `merge.go:98-100` checks) as transient — a constraint violation is permanent and retrying it is pointless work on the hot write path.

`internal/store` is the only package that imports the driver (`store.go:26`); `IsTransient` lives there, not in `consumer`, specifically so `internal/consumer` does not gain a new direct dependency on `modernc.org/sqlite`.

**New `internal/consumer/retry.go`:**

```go
func withRetry(ctx context.Context, fn func() error) error
```

Up to 4 attempts total (1 initial + 3 retries) with backoff 250ms / 1s / 4s between attempts, calling `store.IsTransient(err)` after each failed attempt to decide whether to retry. Abandons immediately (returns the last error) if `ctx` is cancelled during a backoff sleep — use a `time.Timer` selected against `ctx.Done()`, not a bare `time.Sleep`. A persistent (always-transient) error gives up after the 4th attempt, having consumed at most 250ms+1s+4s ≈ 5.25s.

**Call sites — wrap exactly these four, each individually:**
- `c.st.InsertEvent(ctx, pe.ev)` in `flush` (`internal/consumer/consumer.go:233`)
- `c.st.UpsertWarnings(ctx, id, pe.warnings)` in `flush` (`consumer.go:242`)
- `c.st.SessionEventsForRules(ctx, sessionID)` in `runSessionRule` (`consumer.go:287`)
- `c.aggregator.RecordCall(ctx, sessionID, firstEv[sessionID])` in `flush` (`consumer.go:272`) — `SessionAggregator.RecordCall` is an interface method (`internal/consumer/consumer.go:44`-area seam, not `Store`), but it is backed by the same store and can fail the same way, so it is retried the same way.
- `c.st.UpsertWarnings(ctx, eventID, warnings)` in `runSessionRule` (`consumer.go:300`) — this is a second call site for the same method named above; wrap both occurrences.

Do not add `withRetry` anywhere else (not `processCall`, not the sink, not `drainRemaining`'s own loop structure) — those paths do not call the `Store` interface.

**Flush-wide gave-up flag.** Add a local `gaveUp bool` inside `flush` (`consumer.go:219-281`). Once any retried call exhausts its 4 attempts (returns a non-nil error after `withRetry` gives up), set `gaveUp = true` and:
- the remaining `InsertEvent` calls in the batch loop (`consumer.go:232-257`) are **not attempted** — each remaining `pe` in the batch is counted directly into `c.failed` (via `c.failed.Add(1)`) without calling `InsertEvent` at all, so a give-up does not retry 4 more times per remaining row;
- phase 2 (`runSessionRule`, `consumer.go:261-265`) is skipped entirely for this flush;
- phase 3 (`RecordCall`, `consumer.go:270-276`) is skipped entirely for this flush.

This bounds one flush to at most one give-up (~5.25s), regardless of batch size, session count, or `RecordCall` invocation count in that batch (§6 R3). The consumer goroutine blocks during backoff (it is a single goroutine, `Run`'s own select loop, `consumer.go:171-209`); the sink (`internal/sink`) is bounded and non-blocking (`sink.Submit`'s doc, `sink.go:1-4`), so a long transient outage causes the sink to drop new captures at the `proxy: dropped capture` log line (`proxy.go:246-248`) rather than growing unboundedly — this is the existing fail-open behavior and needs no new code.

**Counters.** Add two `atomic.Uint64` fields to `Consumer` (`consumer.go:70-75`, beside `processed`/`failed`/`drained`/`flushCount`): `transientRetries` (incremented once per retry attempt beyond the first, inside `withRetry`, via a `Consumer` method or a package-level counter `withRetry` is given a pointer to) and `transientGaveUp` (incremented once per give-up event, i.e. once per flush that set `gaveUp=true`, not once per call that failed inside that flush). Add both to the `Stats` struct (`consumer.go:126-133`) and populate them in `Stats()` (`consumer.go:135-149`).

Expose both on `GET /api/health`: add two fields to `healthResponse` (`internal/api/api.go:712-724`) — `StoreTransientRetries uint64 \`json:"store_transient_retries"\`` and `StoreTransientGaveUp uint64 \`json:"store_transient_gave_up"\`` — and populate them from `cs := a.consumer.Stats()` in the `health` handler (`api.go:726-745`), the same place `ConsumerProcessed` etc. are already populated. `doctor` (`internal/cli/doctor.go`) has no in-process access to these counters — it opens the store directly (`doctor.go:139-161`) rather than querying a running consumer — so these counters are reachable only through `GET /api/health`; do not add them to `doctor`'s output.

No SQLite pragma change (`cache_size`, `temp_store`, `synchronous`) — rejected in plan §9, no evidence the 2026-10-09 burst was cache-bound.

## Rationale

`Open` (`store.go:68-90`) sets `busy_timeout` for lock contention only; nothing retries `SQLITE_IOERR` or `SQLITE_NOMEM` anywhere in `internal/store` or `internal/consumer` today — `flush` logs and drops on any error (`consumer.go:234-238`, `272-274`, `287-291`). The 2026-10-09 burst of IOERR/NOMEM within a 21-second window is unreproduced and unexplained (plan §1), but whatever its root cause, a bounded retry turns a short transient burst into a handled blip instead of silently dropped rows, and the new counters make a recurrence visible on `/api/health` rather than only in a log scroll.

## Outcome Definition

`go test ./internal/store/ -count=1 -run TestIsTransient` passes: IOERR (10) and NOMEM (7) → `true`; BUSY (5), a wrapped unique-constraint error, and `nil` → `false`.
`go test ./internal/consumer/ -count=1 -run 'TestWithRetryRetriesTransientOnly|TestWithRetryStopsOnCancel|TestWithRetryGivesUpOncePerFlush'` passes.
`go test ./internal/api/ -count=1 -run TestHealthExposesStoreRetryCounters` passes: after a simulated transient retry, `GET /api/health` reports `store_transient_retries > 0` and `store_transient_gave_up == 0`; after a simulated give-up, `store_transient_gave_up > 0`.
`go build ./... && go vet ./... && go test ./... -count=1` all pass, including the unchanged TTFB gate (`go test ./internal/proxy/`).

## Test Specifications

- `internal/store/retry_test.go`: `TestIsTransient` — table test: a `*sqlite.Error`-wrapping error with primary code 10 (IOERR) → `true`; primary code 7 (NOMEM) → `true`; primary code 5 (BUSY) → `false`; a real unique-constraint violation (produced the same way `merge_test.go` or `store_test.go` already produces one, or `isUniqueConstraintError`'s own fixture) → `false`; `nil` → `false`.
- `internal/consumer/retry_test.go`: `TestWithRetryRetriesTransientOnly` — a `fn` that returns a transient-classified error twice then succeeds is retried and ultimately returns `nil`; a `fn` that returns a non-transient error is not retried (called exactly once).
- `internal/consumer/retry_test.go`: `TestWithRetryStopsOnCancel` — cancel `ctx` during the first backoff sleep; `withRetry` returns promptly (bounded, not waiting out the full backoff schedule) with the underlying error, not a different cancellation error that would mask the original failure.
- `internal/consumer/consumer_test.go` (or `retry_test.go`): `TestWithRetryGivesUpOncePerFlush` — a batch of several events where the injected `Store.InsertEvent` always returns a transient error on the first event; asserts: (a) that first `InsertEvent` is attempted exactly 4 times, (b) every remaining event in the batch is counted into `Stats().Failed` without `InsertEvent` being called again for them, (c) `runSessionRule` (phase 2) is not invoked for this flush, (d) `RecordCall` (phase 3) is not invoked for this flush, (e) `Stats().TransientGaveUp == 1`.
- `internal/api/api_test.go`: `TestHealthExposesStoreRetryCounters` — wire a `Consumer` (or a test double satisfying whatever `api`'s consumer-stats seam requires) whose `Stats()` reports non-zero `TransientRetries`/`TransientGaveUp`; `GET /api/health` returns those exact values in `store_transient_retries`/`store_transient_gave_up`.
- **Manual / recorded**: none — the 2026-10-09 burst itself is not reproducible (plan §5.3); only the handling is tested.

## Files to Touch

- `internal/store/retry.go` (create — `IsTransient`)
- `internal/store/retry_test.go` (create — `TestIsTransient`)
- `internal/consumer/retry.go` (create — `withRetry`)
- `internal/consumer/retry_test.go` (create — `TestWithRetryRetriesTransientOnly`, `TestWithRetryStopsOnCancel`)
- `internal/consumer/consumer.go` (modify — wrap the four call sites in `flush`/`runSessionRule`, flush-wide `gaveUp` flag, two new atomic counters on `Consumer` and `Stats`)
- `internal/consumer/consumer_test.go` (modify — add `TestWithRetryGivesUpOncePerFlush`, or add it to `retry_test.go`)
- `internal/api/api.go` (modify — two new `healthResponse` fields, populated in `health`)
- `internal/api/api_test.go` (modify — add `TestHealthExposesStoreRetryCounters`)

## Review Notes
