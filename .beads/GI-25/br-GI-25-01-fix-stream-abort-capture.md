# Bead br-GI-25-01: Record a mid-stream upstream abort as an incomplete capture

**Plan Reference**: `docs/planning/GI-25-local-llm-toggle-and-store-hardening.md` v8, §2 R2, §3.1, §5.2 (`TestMidStreamUpstreamAbortIsMarkedIncomplete`, `TestCancelledClientIsNotAnAbort`), §5.4. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-25-01
- **Priority**: P0
- **Status**: pending
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: None
- **Commit**: `GI#25 fix: record a mid-stream upstream abort as incomplete (br-GI-25-01)`

## Description

`httputil: ReverseProxy read error during body copy` (Go stdlib, `$GOROOT/src/net/http/httputil/reverseproxy.go:648-650`) fires when `src.Read` fails — `src` is the upstream response body, so this is the upstream dropping the stream mid-response, not a client disconnect. `ReverseProxy.ErrorHandler` does not fire once headers are sent (`internal/proxy/proxy.go:50-55`), and today neither `onClose` closure records the read error, so `st.submit` is always called with `captureComplete = !respBuf.truncated && !st.reqBody.truncated` (`proxy.go:107-108`) — `true` for a body cut mid-stream by the upstream.

**`teeCloser` (`proxy.go:302-316`)** gains a `readErr error` field and an `aborted() error` accessor:

```go
type teeCloser struct {
	r       io.Reader
	c       io.Closer
	onClose func()
	readErr error
}

func (t *teeCloser) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) {
		t.readErr = err
	}
	return n, err
}

func (t *teeCloser) aborted() error { return t.readErr }
```

`context.Canceled` is excluded deliberately (§3.1): a client that stopped the stream received what the session produced, which is not an upstream truncation — `CaptureComplete` stays `true` for that case and no `upstream_error` warning is attached. This is an explicit decision, not a side effect, and the negative-control test depends on it.

**Both `onClose` closures in `ModifyResponse` check `t.aborted()`** — not just the capturing path:
- The capturing path (`proxy.go:91-110`): if `t.aborted() != nil`, force `captureComplete = false` regardless of the buffers' own truncated flags, pass the error as `callErr` to `st.submit`, and log `proxy: upstream stream aborted method=%s path=%s captured=%d err=%v` with `captured=len(respBuf.Bytes())`.
- The `--body-policy off` pass-through path (`proxy.go:84-86`): same check, same log line with `captured=0` (nothing is teed on this path).

The response body wrapped in `teeCloser` at each site is the same `*teeCloser` value `onClose` closes over, so `t.aborted()` inside the closure reads the field `Read` set during the copy that just finished — `Close` always runs after the copy loop to the client is done (`proxy.go:296-301`).

**Suppress the duplicate stdlib line.** Set `rp.ErrorLog` on the `httputil.ReverseProxy` (`proxy.go:41`) to a `log.New(io.Discard, "", 0)` logger, so the stdlib's own `httputil: ReverseProxy read error during body copy` line does not also appear alongside the new labelled one.

**`st.submit`'s existing `callErr` parameter already wires the warning** (`consumer.go:368-375`): a non-nil `call.Err` already produces an `upstream_error` warning at severity `error`. No consumer change is needed — only the proxy-side call to `submit` needs to pass the abort error as `callErr` instead of `nil`.

**`ruleStreamIncomplete`** (`internal/analyze/rules.go:183-189`) already fires on `usage.IsStream && !ev.CaptureComplete` with no `CaptureComplete`-source distinction, so a stream abort automatically gets `stream_incomplete` for free once `CaptureComplete=false` is set — no new warning kind. A stream abort therefore yields **two** warnings (`upstream_error` + `stream_incomplete`); a non-stream (JSON body) abort yields only `upstream_error` (the rule requires `usage.IsStream`).

**Comment updates** (so the canonical definition and every pointer to it stay true):
- `internal/sink/sink.go:41-50` (`CapturedCall.CaptureComplete` doc) — this is the canonical definition. Rewrite to: "false when either teed body was cut at the cap, or the upstream response body ended with a read error other than a client cancel."
- `internal/store/types.go:74-78` (`Event.CaptureComplete` doc) — reduce to a pointer: "see `sink.CapturedCall.CaptureComplete`."
- `internal/analyze/rules.go:174-175` (`ruleStreamIncomplete` doc) — reduce the "truncated at the read cap" half to a pointer at the same definition.
- `internal/web/app.js:206` (`captureMarker` doc comment) — reduce to a pointer.
- `internal/web/app.js:247-248` ("that flag is about the two *teed* bodies" — `transcriptCapMarker` doc) — this line is about a *different* flag's scope (it already correctly says CaptureComplete does NOT drive `transcriptCapMarker`); update only the parenthetical if it restates the old cap-only definition, otherwise leave the "deliberately NOT driven by" statement itself unchanged — it remains true.
- `internal/jsonlogs/jsonlogs.go:474-475` ("teed bodies" wording in the `buildEvent` comment) — reduce to a pointer; this comment also remains structurally correct (a jsonl row owns no teed bodies) but should name the canonical definition rather than restate it.
- `internal/analyze/kinds.go:74` (`KindStreamIncomplete`'s user-facing description, currently `"The capture is incomplete: a body was truncated at the read cap."`) — rewrite to the disjunction the rule now covers: `"The capture is incomplete: truncated at the read cap, or the upstream stream was aborted before the response was complete."`
- `docs/context/` staleness (`storage-schema.md`, `workflows.md`, `dashboard.md`, `glossary.md`, `INDEX.md`, `data-privacy-and-compliance.md`) is tracked separately (plan §11); do not edit `docs/context/` from this bead.

**Hot-path rule.** No parsing, no blocking, no new import: `teeCloser.Read` only compares `err` against `io.EOF` and `context.Canceled` (both already-imported stdlib values once `context` and `errors` are imported; `errors` is a new import to `proxy.go`, `context` is already imported). `internal/proxy` continues to depend only on `sink` and `config`.

**Merge interaction (for the record, no code change needed here):** an aborted proxy row keeps `capture_complete=0` through `captureCompleteFromBodies` (`internal/store/merge.go:493-504`) in both arrival orders, because it owns the bodies.

## Rationale

Without this, a stream the upstream (the DeepSeek edge the proxy forwards to, per this repo's upstream) cut short is recorded as a complete capture with whatever bytes happened to arrive before the drop — the exact "truncated looks identical to complete" defect this story exists to close, on the one path (an upstream failure, not a cap) nothing today detects. The log triage item's first line (`ReverseProxy read error during body copy`) and the truncation complaint are the same underlying mechanism.

## Outcome Definition

`go test ./internal/proxy/ -count=1 -run 'TestMidStreamUpstreamAbort|TestCancelledClientIsNotAnAbort'` passes, covering all four sub-cases (stream abort, non-stream abort, `--body-policy off` abort, client-cancel non-abort). `go test ./internal/proxy/ -count=1` (the TTFB gate, unchanged) stays green. `go vet ./internal/proxy/` is clean.

## Test Specifications

- `internal/proxy/proxy_test.go`: `TestMidStreamUpstreamAbortIsMarkedIncomplete` — fake upstream sends headers and part of the body, then closes the connection mid-stream. At the proxy layer (which cannot import `store`, so it cannot assert warnings) assert on the submitted `sink.CapturedCall`: `CaptureComplete == false` and `Err != nil`; and assert the captured log contains `proxy: upstream stream aborted`. Three sub-cases: (a) a streamed (SSE) body abort, (b) a non-stream (JSON) body abort, (c) `--body-policy off` (the pass-through closure), whose log line must show `captured=0`. The two-warning outcome (`upstream_error` + `stream_incomplete` for a stream, `upstream_error` alone for JSON) is covered on the consumer side: add one consumer test feeding a `CapturedCall{Err: errors.New("x"), CaptureComplete: false}` with a stream usage and assert both kinds.
- `internal/proxy/proxy_test.go`: `TestCancelledClientIsNotAnAbort` — the fake upstream read returns `context.Canceled` (simulated via closing the client's request context mid-copy); asserts `CaptureComplete=true` and `CapturedCall.Err == nil`.
- **Negative control**: revert the `teeCloser.readErr`/`aborted()` change (or revert the `onClose` check) and re-run `TestMidStreamUpstreamAbortIsMarkedIncomplete` — it must fail with `CaptureComplete == true`.

## Files to Touch

- `internal/proxy/proxy.go` (modify — `teeCloser.readErr`/`aborted()`, both `onClose` closures check it, `rp.ErrorLog` discard logger, new `errors` import)
- `internal/proxy/proxy_test.go` (modify — add the two new tests and their sub-cases)
- `internal/consumer/consumer_test.go` (modify — the one test asserting both warning kinds for an aborted stream; no consumer code change)
- `internal/sink/sink.go` (modify — `CapturedCall.CaptureComplete` doc comment, canonical definition)
- `internal/store/types.go` (modify — `Event.CaptureComplete` doc comment, line 74, reduced to a pointer)
- `internal/analyze/rules.go` (modify — `ruleStreamIncomplete` doc comment, lines 174-175, reduced to a pointer)
- `internal/analyze/kinds.go` (modify — `KindStreamIncomplete` description, line 74, rewritten to the disjunction)
- `internal/web/app.js` (modify — `captureMarker` doc comment at line 206 reduced to a pointer)
- `internal/jsonlogs/jsonlogs.go` (modify — `buildEvent` comment near lines 474-475 reduced to a pointer)

## Review Notes

Implemented. `go test ./internal/proxy/ ./internal/consumer/ ./internal/analyze/ ./internal/web/ ./internal/store/` green. Negative control (never setting `readErr`) failed `TestMidStreamUpstreamAbortIsMarkedIncomplete` as the bead required. The abort fixture hijacks the connection and drops it after one chunk; the client-cancel case stayed complete with no error. `app.js:247` and the jsonlogs `buildEvent` comment were left as is: both are still true.
