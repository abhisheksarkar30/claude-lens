# GI#15: DeepSeek residual capture gap — close the two proxy-side blind spots

**Issue:** [GI#15](https://github.com/abhisheksarkar30/claude-lens/issues/15)
**Branch:** `GI-15-deepseek-capture-gap`
**Origin:** `memory/project_deepseek_capture_gap.md` from the reconciliation session that
diagnosed this (2026-09-23), continued here.

## Background (what's already established, not re-litigated by this plan)

A prior session reconciled `D:/clens/lens.db` against the user's real DeepSeek provider billing
export and found:

- The **pre-09-20 gap** is fully explained by routing history (traffic split across
  bypass / `deepseek-lens` / `clens` while the user experimented) — closed, not in scope here.
- A **3-12% residual gap persists 09-20 onward**, even after full cutover to the `clens` proxy.
  The user has now confirmed (this session) that **all DeepSeek traffic is routed through
  `127.0.0.1:8797`** — so the residual is not a routing misconfiguration. Per the memory's own
  "how to apply" note, that rules in favor of investigating `clens`'s own proxy-side capture
  completeness rather than `internal/jsonlogs`.
- On 09-21, `clens` recorded **more** proxy events than the provider's real request count (105%)
  while total cost was still **under** (95%) — so this is not simply "rows are missing"; something
  more specific is going on.
- `clens`'s `source='jsonl'` slice remains non-trivial after cutover (09-22: 435 rows / $0.64 of
  ~$5.49, ~12%) — some DeepSeek calls reach `clens` only via the JSONL collector, meaning
  `internal/proxy` never captured them at all.

## This session's additional findings (code + live data)

1. **`internal/sink/sink.go:41-49`'s doc comment on `CapturedCall.CaptureComplete` is wrong.** It
   says the flag is false "when ... the stream ended without `message_stop`." It isn't and
   structurally can't be — `internal/proxy` never parses SSE (that's the entire point of the
   hot/cold split, see `docs/context/decisions/002-hot-path-never-parses.md`), and
   `docs/context/storage-schema.md:116-126` already correctly documents that the flag is driven
   *only* by the two body-cap `truncated` bits (`proxy.go:107`). This is the same defect class
   `docs/context/INDEX.md`'s refresh history keeps naming: a control that looks like it covers a
   path (SSE framing) and doesn't. It cost real time in this session's own investigation and will
   cost more in the next one if left as-is.

2. **A dropped capture leaves zero trace.** `internal/proxy/proxy.go:225` calls
   `st.sk.Submit(&sink.CapturedCall{...})` and discards the returned bool.
   `internal/sink/sink.go`'s own doc comment anticipates this ("callers may inspect the return
   value to log a drop"), but no caller does. `docs/context/architecture.md`'s Fail-open invariant
   #1 states "a capture failure is logged" — true for the consumer's write failures
   (`consumer.go:218`), **not true for a sink-drop**, which is the one failure mode that would
   directly produce the "proxy never saw this call" signature the memory describes. Confirmed live:
   `GET /api/health` exposes `sink_dropped`/`consumer_failed`, but both are in-memory and reset on
   every restart — the current process (started after today's migrations) shows `sink_dropped: 0`
   with no way to know what it was during the actual 09-20→09-23 gap window. That window's evidence
   is permanently gone; this fix is about the *next* occurrence, not this one.

3. **Existing data-repair commands find nothing to fix.** Per this repo's convention that
   `reprice`/`reflag` are the reusable, general-purpose tools for exactly this class of question
   (not a new one-off command), both were run read-only against the live db:
   - `clens reflag --dry-run` → `would flip 0 row(s) to incomplete; 3993 already honest, 165
     residual (laundered, no Content-Length witness — not repairable)`.
   - `clens reprice --dry-run` → `would reprice 0 row(s); 52969 already correct, 254 skipped (not
     reconstructible from the stored columns)`.

   **This is the plan's key scoping fact: every row that *is* stored is already internally
   consistent with its own inputs.** There is no mis-flagged or mis-priced data sitting in the db
   waiting to be corrected — confirming the residual gap is exclusively about captures that were
   never written at all (or arrived from DeepSeek's API already under-reporting usage, which no
   repair command can detect without an external ground truth). **No new command, and no data
   migration/correction, is needed or justified by the evidence.**

4. **`internal/reconcile` cannot cover this.** It only compares `proxy`/`jsonl` cost against
   Anthropic's Admin API reports — there is no equivalent billing-report endpoint for DeepSeek, so
   clens structurally cannot self-reconcile third-party cost. The only way to check the residual
   gap against real numbers is what the origin session already did: a one-off comparison against a
   manually downloaded provider CSV. That stays a one-off, uncommitted script (this repo's own
   convention: reusable commands for repeatable needs, one-off scripts for a complex one-time
   comparison against an external, manually-obtained file) — not a plan deliverable.

## What changes

| File | Change | Why |
|---|---|---|
| `internal/sink/sink.go` | Correct the `CapturedCall.CaptureComplete` doc comment (finding 1) | Stop the comment from asserting behavior the hot path cannot perform; align it with `storage-schema.md`'s already-correct description |
| `internal/proxy/proxy.go` | In `captureState.submit`, keep the `*sink.CapturedCall` in a local variable, check `st.sk.Submit(call)`'s return value, and `log.Printf` a drop with the fields already available at that point (method, path, auth kind, started_at, the call's assigned ID) (finding 2) | Restores the "a capture failure is logged" invariant for the one path it doesn't currently cover; gives the next investigation a timestamped trace instead of an unrecoverable in-memory counter |
| `internal/proxy/proxy_test.go` | New test: a capacity-1 sink, two sequential requests with nothing draining between them, asserting the second is dropped (`sk.Stats()`) **and** that the redirected standard-library log output contains the drop | Same-bead verification, following this package's existing pattern of redirecting log output in a test (`proxyServer`'s `srv.Config.ErrorLog` redirection is the precedent, though that captures `net/http`'s panic log rather than this package's own `log.Printf`, so this test uses `log.SetOutput` for the standard logger instead) |

**Profiler flag — already shipped, not a bead here.** This session was asked to add an optional
`--pprof-addr` CLI flag as part of this PR. Before implementing it, this branch was fast-forwarded
onto `origin/main` (it had fallen 29 commits behind since being cut) and that merge brought in
`GI#13`/PR [#14](https://github.com/abhisheksarkar30/claude-lens/pull/14), whose commit `021e65d`
("br-GI-13-04") already ships exactly this: a config-resolved, loopback-locked `--pprof-addr` /
`CLENS_PPROF_ADDR` flag, not widened by `--allow-remote`, reported by `doctor`, with its own tests
in `config_test.go`/`serve_test.go`/`doctor_test.go` — all present and green on `main` right now
(`internal/cli/serve.go:421`, `internal/config/config.go:251`). There is nothing left to build:
re-implementing it here would fork an already-merged, already-vetted design. No bead is needed for
this ask.

No schema change, no new CLI *command* (bead 2 only checks an existing `Submit` return value), no
new dependency, no new route on the dashboard or proxy listener.

## Architecture / infrastructure changes

None. Both fixes are within `internal/proxy`'s existing import boundary (`sink` + `config` only —
`internal/proxy/importguard_test.go` already enforces this and nothing here needs to add an
import). No config knob, no migration, no workflow change.

## Test strategy

- **Bead 1 (comment fix):** no behavior changes, so no new test — verified by re-reading the
  corrected comment against `storage-schema.md` and `proxy.go:107` for agreement.
- **Bead 2 (drop logging):** the new test described above is the acceptance check: build
  `sink.New(1)`, issue two requests through the handler without draining the sink between them,
  assert `sk.Stats()` reports `dropped == 1` (this part already passes today — it's the log line
  that's new), and assert the log capture contains the dropped call's method/path. Existing
  `TestFailOpenOnUpstreamFailure` and the rest of `proxy_test.go` continue to cover the invariant
  that a capture failure never reaches the client — this bead only adds visibility, it does not
  change what the client sees.
- Full suite: `go build ./...`, `go vet ./...`, `go test ./internal/proxy/ ./internal/sink/`, then
  `go test ./...` before opening the PR, per this repo's CLAUDE.md.

## Risk areas / edge cases

- **The new log line must not become a second, competing definition of drop-visibility.** It logs
  the same event `Sink.Stats()`'s `dropped` counter already increments — it must not double-count
  or diverge from that counter. The test asserts both from the same two-request sequence.
- **Log volume under sustained overload.** A sink that stays full for an extended burst would log
  once per dropped call, same as the consumer already does per failed insert (`consumer.go:218`).
  No rate-limiting is added — this matches existing precedent in the codebase and a real, sustained
  drop storm is exactly the condition an operator most needs the log for. Not treated as a risk
  worth guarding against speculatively.
- **This does not, and cannot, recover the already-elapsed 09-20→09-23 gap.** The plan is explicit
  that this is a forward-looking fix; the historical window's counters are gone. If the user wants
  the historical gap pinned down more precisely than the memory's existing evidence, that requires
  re-running a CSV-based comparison (finding 4) — a separate, manual, one-off activity, not a code
  deliverable of this story.

## Context docs to refresh

Checked `docs/context/architecture.md` (Fail-open section) and `docs/context/storage-schema.md`
(`capture_complete` section) against both changes:

- **`storage-schema.md`** already states the correct `CaptureComplete` semantics — nothing to
  update there; the bug was only in the source comment.
- **`architecture.md`'s Fail-open section** states the invariant ("a capture failure is logged")
  as a rule, not as a claim tied to a specific, exhaustively-enumerated set of paths — this change
  makes an already-stated rule true for one more path rather than changing what the rule says.
  No wording in that section becomes inaccurate.

**No context doc changes are needed.** This story adds no entity, endpoint, permission, flow, or
convention a context doc would claim — Phase 5.6 should confirm this against the final diff and
skip explicitly rather than run the refresh skill for a no-op scope.

## Self-review

**As a senior engineer:** The scope is intentionally small — two code changes, both root causes of
observed defects (a wrong comment, a genuinely silent failure path), not speculative hardening.
The alternative of resizing the sink's capacity or adding retry logic was considered and rejected:
there is no evidence (drops, failures) that either is currently a problem — `/api/health` shows
zero of both right now — so doing either would be tuning without a measured target, which is
exactly the kind of thing this repo's `decisions/` folder exists to catch as a rejected-without-data
change. The log-line fix's whole value is producing that evidence *if and when* it happens again.

**As a QA engineer:** The bead-2 test needs to be careful about how `submit` obtains the call's
`ID` post-hoc — `sink.Submit` assigns `call.ID` unconditionally before it decides to drop, so the
log line has a real ID to report even on a drop; the test should assert the log line is non-empty
and mentions the request's path, not fabricate an exact ID string (message-id concatenation is an
internal format, not stable API). Edge case covered: two *sequential* (not concurrent) requests are
enough to force a capacity-1 sink to drop deterministically — no goroutine-timing flakiness.

**As a security engineer:** The new log line must not leak anything the row-level redaction
already protects — it should log method, path, timing, and the assigned capture ID, never header
or body content (which `CapturedCall`'s comments already establish are the values requiring
redaction upstream of this struct). The bead description says exactly this so implementation
doesn't accidentally log `call.ReqHeaders` or `call.ReqBody` for convenience.
