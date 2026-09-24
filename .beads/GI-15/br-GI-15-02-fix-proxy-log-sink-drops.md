# Bead br-GI-15-02: log a sink-drop in captureState.submit instead of discarding it silently

**Plan Reference**: `docs/planning/GI-15-deepseek-capture-gap.md` — finding 2, "What changes"
table rows 2-3, Test Strategy "Bead 2", Risk areas, self-review (QA/security), change history
v2/v4/v5 (F1.3, F1.4, F1.5, F3.1, F4.1).

- **Bead ID**: br-GI-15-02
- **Priority**: P1 (high — closes a real, currently-silent fail-open gap; forward-looking, does
  not repair historical data)
- **Original Estimate**: 1.5h
- **Dependencies**: None
- **Blocks**: None

## Description

`internal/proxy/proxy.go`'s `captureState.submit` (currently lines 215-244) builds a
`*sink.CapturedCall` inline as an argument expression and calls `st.sk.Submit(&sink.CapturedCall{
... })`, discarding the returned `bool`. `sink.Sink.Submit`'s own doc comment
(`internal/sink/sink.go:84-88`) already anticipates a caller inspecting the result ("callers may
inspect the return value to log a drop, but ignoring it is correct: dropping under load is
expected, not a failure") — no caller currently does. `docs/context/architecture.md`'s Fail-open
invariant #1 states "a capture failure is logged", which is true for the consumer's write
failures (`internal/consumer/consumer.go:218`) but not for a sink-drop. Confirmed live: `GET
/api/health` exposes `sink_dropped`, but it is in-memory and resets on every process restart —
there is currently no durable trace of when or what was dropped.

### The fix

In `captureState.submit`:

1. **Keep the `*sink.CapturedCall` in a local variable** (`call := &sink.CapturedCall{...}`)
   instead of passing the struct literal directly as `Submit`'s argument. This is required so the
   log line below can read fields off it (in particular `call.ID`, which `Submit` assigns
   unconditionally *before* it decides whether to drop — see `sink.go:89-95` — so it is available
   even on the dropped path).
2. **Check `st.sk.Submit(call)`'s return value.** When it returns `false`, `log.Printf` a drop
   line.
3. **The log line carries exactly:** method, path, auth kind, timing (e.g. `call.StartedAt` and
   the elapsed duration — `time.Since(st.start)`, already computed once for `Duration` on the
   call), and the call's assigned ID (`call.ID`). **Never** `call.ReqHeaders`, `call.RespHeaders`,
   `call.ReqBody`, or `call.RespBody` — those are exactly the fields `CapturedCall`'s own comments
   already establish as requiring redaction upstream of this struct
   (`internal/sink/sink.go:36-39`), and a `log.Printf("dropped: %+v", call)`-style struct dump
   would leak them. Do not log `call.Err` either — it is out of scope for a drop line and belongs
   to the separate `callErr` path.
4. Add the stdlib `log` import to `proxy.go`. This is fine under
   `internal/proxy/importguard_test.go`'s existing boundary — that guard restricts
   *internal-package* imports (`sink` + `config` only), not stdlib.

Example shape (adapt to house style, do not literally copy field ordering from elsewhere):

```go
call := &sink.CapturedCall{
    StartedAt:       st.start,
    TTFB:            st.ttfb,
    Duration:        time.Since(st.start),
    Method:          st.method,
    Path:            st.path,
    RemoteAddr:      st.remoteAddr,
    Status:          status,
    AuthKind:        st.authKind,
    ReqHeaders:      st.reqHeaders,
    RespHeaders:     respHeaders,
    ReqBody:         reqBody,
    RespBody:        respBody,
    CaptureComplete: captureComplete,
    ReplayOf:        st.replayOf,
    ReplayEdits:     st.replayEdits,
    RequestIDHeader: requestIDHeader(respHeaders),
    Err:             callErr,
}
if !st.sk.Submit(call) {
    log.Printf("proxy: dropped capture id=%s method=%s path=%s auth=%s started=%s duration=%s",
        call.ID, call.Method, call.Path, call.AuthKind,
        call.StartedAt.Format(time.RFC3339Nano), call.Duration)
}
```

### New test: `internal/proxy/proxy_test.go`

Add a test that builds `sink.New(1)` (capacity 1) and a working handler (a real `httptest.Server`
upstream that returns a normal response, following the pattern of `TestByteIdentityNonStreaming`
/ `TestNoBufferingSSE` — `sink.New(16)` in those tests, use `1` here), then:

1. **Redirect the standard logger's output.** Save `log.Writer()`'s current output before
   calling `log.SetOutput`, and restore it via `t.Cleanup`, matching the discipline `proxyServer`
   already applies to `srv.Config.ErrorLog` (`proxy_test.go:47-61`). This test uses `log.SetOutput`
   directly rather than `proxyServer`'s `ErrorLog` redirection, because that field captures
   `net/http`'s own panic log, not this package's `log.Printf` calls — a different logger
   entirely. Guard the buffer with the same kind of lock `lockedWriter` already provides if the
   log write and the test's read could race (they don't need to here since both requests complete
   and are read sequentially before assertions, but reuse `lockedWriter` for consistency if
   convenient).
2. **Issue two sequential requests through the handler with nothing draining the sink between
   them** (do not call `captureOne`/drain in between) — a capacity-1 sink means the first request
   fills it and the second is guaranteed to be dropped, deterministically, with no
   goroutine-timing flakiness (two *sequential* requests are enough; no concurrency needed).
3. **The second request's body carries a distinctive JSON sentinel value** — a string unlikely to
   appear in any log line by coincidence (e.g. a random-looking token embedded in a JSON body
   field). This is the request whose capture gets dropped and logged.
4. **Assert `sk.Stats()` reports `dropped == 1`** after both requests (this part already passes
   today with no code change — it is the log line that is new).
5. **Positive assertion:** the captured log output is non-empty and contains the request's path
   (e.g. `/v1/messages`).
6. **Negative-containment assertion (the point of this test):** the captured log output does
   **not** contain the distinctive JSON-body sentinel, using the same idiom already established
   at `internal/analyze/rules_test.go:345-347`:
   ```go
   if strings.Contains(logOutput, sentinel) {
       t.Errorf("drop log leaked body: %q", logOutput)
   }
   ```
   This turns the security self-review's prose guarantee into an enforced regression test — a
   lazy `log.Printf("dropped: %s", call.ReqBody)` convenience leak would carry body content and
   would fail this check. (Verified during implementation that the check does **not** catch a
   `log.Printf("dropped: %+v", call)` whole-struct dump specifically: Go's `fmt` renders a
   `[]byte` field under `%+v` as a slice of decimal integers, never as text, so the sentinel never
   appears verbatim in that output even though the raw bytes are technically still present,
   decimal-encoded. The `%s`/string-conversion leak class is the realistic one this check guards
   against; a `%+v`-proof check would need a stricter allow-listed-format assertion, which is a
   separate scope decision, not required by this bead.)
7. **Do not use an `Authorization`-header value as the sentinel.** `redactHeaders` replaces
   `Authorization` with the literal `"[redacted]"` at `proxy.go:122` (inside the handler, before
   `captureState` is constructed), so `call.ReqHeaders.Authorization` (and thus anything a drop
   log could read off it) is always `"[redacted]"` regardless of what the test sends — a
   negative-containment check on the real header value would pass for any implementation,
   including a careless struct-dump, and would provide zero regression coverage. The sentinel
   must be a JSON body value only.
8. **Do not assert an exact `call.ID` string.** `sink.Submit` assigns `call.ID` from an internal
   `StartedAt`/sequence-counter format (`sink.go:90-95`) that is not a stable API — assert the log
   line is non-empty and mentions the path, not a fabricated exact ID.

Existing `TestFailOpenOnUpstreamFailure` and the rest of `proxy_test.go` are unaffected and
continue to cover the invariant that a capture failure never reaches the client; this bead only
adds visibility on a drop, it does not change what the client sees or receives.

## Rationale

`docs/context/architecture.md`'s Fail-open invariant #1 ("a capture failure is logged") is
currently false for exactly one path: a sink-drop under load. `sink.Submit`'s own doc comment
already anticipated this gap being closed by a caller; nothing closed it. Without this fix, a
sustained overload silently drops captures with no trace beyond an in-memory counter that resets
on restart — exactly the blind spot this session's own investigation ran into (confirmed live:
`/api/health`'s `sink_dropped` was unreadable for the historical 09-20→09-23 gap window because
the process had since restarted). This is a forward-looking fix: it cannot recover that already-
elapsed window, only make the next occurrence visible.

## Outcome Definition

- `captureState.submit` holds the `*sink.CapturedCall` in a local variable, checks
  `st.sk.Submit(call)`'s return value, and `log.Printf`s a drop line containing method, path,
  auth kind, timing, and `call.ID` when (and only when) the call is dropped.
- The drop log never contains header or body content (`ReqHeaders`, `RespHeaders`, `ReqBody`,
  `RespBody`) under any code path — enforced by the new test's negative-containment assertion.
- The new log line does not double-count or diverge from `Sink.Stats()`'s existing `dropped`
  counter — both are asserted from the same two-request sequence in the new test.
- `go build ./... && go vet ./... && go test ./internal/proxy/ ./internal/sink/` passes, followed
  by `go test ./...` before opening the PR, per this repo's CLAUDE.md.
- `internal/proxy/importguard_test.go` still passes — the new `log` import is stdlib, not an
  internal-package import, and is not restricted by that guard.

## Test Specifications

- Unit Tests (`internal/proxy/proxy_test.go`):
  - New test (name suggestion: `TestSubmitLogsADropWithoutTheBody`): capacity-1 sink, two
    sequential requests with nothing draining between them, second request's body carrying a
    distinctive JSON sentinel.
    - `sk.Stats()` reports `dropped == 1`.
    - Captured log output is non-empty and contains the request path.
    - Captured log output does **not** contain the JSON-body sentinel (negative-containment,
      mirroring `internal/analyze/rules_test.go:345-347`).
    - No assertion on `call.ID`'s exact value.
  - Existing `TestFailOpenOnUpstreamFailure` (`proxy_test.go:218`) requires no change — it
    already exercises the `callErr` submit path (`st.submit(0, nil, nil, false, err)` from
    `ErrorHandler`, `proxy.go:51`), which is unaffected by this refactor.
- Integration Tests: none beyond the above — this is a same-package, same-bead test per the
  plan's Test Strategy.

## Files to Touch

- `internal/proxy/proxy.go` (modify — `captureState.submit`: local `call` variable, `Submit`
  return-value check, `log.Printf` drop line, new `log` stdlib import)
- `internal/proxy/proxy_test.go` (modify — new test plus its `log.SetOutput`/`t.Cleanup`
  restoration helper, following `proxyServer`'s existing `ErrorLog`-redirection discipline)

No other open bead touches `internal/proxy/`; br-GI-15-01 is confined to doc comments in
`internal/sink`, `internal/store`, `internal/analyze`, and `internal/web`.
