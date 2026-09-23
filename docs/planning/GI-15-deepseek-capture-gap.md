# GI#15: DeepSeek residual capture gap — close the two proxy-side blind spots

**Version:** 7
**Status:** revision pending re-review (beads 1-2 converged v6; beads 3-4 added, not yet reviewed)
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
   (`consumer.go:218`), **not true for a sink-drop**. Note: the observed 09-21 anomaly shows *over*-counting (105%
   proxy events, 95% cost) — a sink-drop produces strictly *fewer* rows, not more, so a drop is
   not the mechanism behind that specific historical shape. Bead 2 is a forward-looking fix that
   closes a real fail-open gap for future occurrences; it does not explain or repair the
   historical 09-20→09-23 numbers, whose actual mechanism remains unidentified. Confirmed live:
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
| `internal/sink/sink.go`, `internal/store/types.go`, `internal/analyze/rules.go`, `internal/analyze/kinds.go`, `internal/web/app.js` | Correct all five doc comments repeating the false claim that `CaptureComplete` can be false because "the SSE stream ended without `message_stop`" (finding 1): `sink.go:41-43`, `store/types.go:74-75`, `rules.go:174-175`, the `KindStreamIncomplete` Description string in `kinds.go:74`, and `app.js:191-193` (`captureMarker`'s doc comment). Note: `docs/context/dashboard.md` and `INDEX.md` both pin `app.js` at exactly 1142 lines; if the corrected comment changes `app.js`'s total line count, those two docs' "1142 lines" figure needs a one-line update (a `grep`/`wc -l` check, not a full context-doc REFRESH pass) in the same commit. | Stop all five from asserting behavior the hot path cannot perform; align them with `storage-schema.md`'s already-correct description — the flag is driven only by the two body-cap `truncated` bits |
| `internal/proxy/proxy.go` | In `captureState.submit`, keep the `*sink.CapturedCall` in a local variable, check `st.sk.Submit(call)`'s return value, and `log.Printf` a drop with the fields already available at that point (method, path, auth kind, started_at, the call's assigned ID) (finding 2) | Restores the "a capture failure is logged" invariant for the one path it doesn't currently cover; gives the next investigation a timestamped trace instead of an unrecoverable in-memory counter |
| `internal/proxy/proxy_test.go` | New test: a capacity-1 sink, two sequential requests with nothing draining between them, asserting the second is dropped (`sk.Stats()`) **and** that the redirected standard-library log output contains the drop | Same-bead verification, following this package's existing pattern of redirecting log output in a test (`proxyServer`'s `srv.Config.ErrorLog` redirection is the precedent, though that captures `net/http`'s panic log rather than this package's own `log.Printf`, so this test uses `log.SetOutput` for the standard logger instead; the test saves `log.Writer()`'s current output before `log.SetOutput` and restores it via `t.Cleanup`, matching the same discipline `proxyServer` already applies to `srv.Config.ErrorLog`) |

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

None. Both fixes stay within `internal/proxy`'s existing *internal-package* import boundary
(`sink` + `config` only — `internal/proxy/importguard_test.go` already enforces this). Bead 2
adds a stdlib `log` import to `proxy.go`, which the import guard does not restrict. No config
knob, no migration, no workflow change.

## Test strategy

- **Bead 1 (comment fix):** no behavior changes, so no new test — verified by re-reading the
  corrected comment against `storage-schema.md` and `proxy.go:107` for agreement.
- **Bead 2 (drop logging):** the new test described above is the acceptance check: build
  `sink.New(1)`, issue two requests through the handler without draining the sink between them,
  assert `sk.Stats()` reports `dropped == 1` (this part already passes today — it's the log line
  that's new), and assert the log capture contains the dropped call's method/path. The test request
  must carry a distinctive JSON body payload as the sentinel — a value not likely to appear in any
  log line by coincidence; after asserting the log line is non-empty and contains the path, assert
  with the negative-containment idiom already established in `internal/analyze/rules_test.go:345-347`
  that the captured log output does **not** contain that distinctive body value:
  `if strings.Contains(logOutput, sentinel) { t.Errorf("drop log leaked body: %q", logOutput) }`.
  This turns the security self-review's prose guarantee into an enforced regression test — a lazy
  `log.Printf("dropped: %s", call.ReqBody)` convenience leak would carry body content and would
  fail this check. (A `%+v` whole-struct dump would not trip this specific check: Go's `fmt`
  renders a `[]byte` field under `%+v` as decimal integers, not text, so the sentinel never
  appears verbatim in that output — the `%s`/string-conversion leak class is the realistic one
  this check guards against; verified during implementation by temporarily reverting to each shape
  and confirming which one the test actually catches.)
  The `Authorization` header is not a valid sentinel: `redactHeaders` replaces it with the literal
  `"[redacted]"` at `proxy.go:122`, before `captureState` is constructed, so `ReqHeaders.Authorization`
  is always `"[redacted]"` inside `submit()` regardless of what the test sends — a negative-containment
  check on the real header value would pass for any implementation, including a careless struct-dump,
  providing zero regression coverage. Existing
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
already protects — it should log method, path, auth kind, timing, and the assigned capture ID,
never header or body content (which `CapturedCall`'s comments already establish are the values
requiring redaction upstream of this struct). The bead description says exactly this so
implementation doesn't accidentally log `call.ReqHeaders` or `call.ReqBody` for convenience.

## Addendum: source_mismatch diagnostics (beads 3-4)

### Findings (this session, live-data investigation)

A dashboard review of the live `D:/clens/lens.db` (post-restart onto the beads-1/2 binary) found
`source_mismatch` at 48 occurrences (0.09% of 53,828 calls) and trending up (13 → 14 → 21 per day,
09-21 through 09-23) — small, but growing, and directly downstream of this story's own subject
(DeepSeek capture reconciliation). Two rows were traced end-to-end: DB row → raw `resp_headers` →
the matching `~/.claude/projects/**/*.jsonl` transcript line.

1. **The key-matching itself is correct and already documented as intentional.** DeepSeek sends no
   `Request-Id` response header at all (confirmed on event `130890`: its stored `resp_headers` has
   `X-Ds-Trace-Id`, never `Request-Id`). `internal/jsonlogs/dedup.go:96-106`'s own comment states
   this is exactly why both `internal/consumer.requestID()` and `jsonlogs.requestKey()` fall back to
   the response body's `message.id` for DeepSeek traffic — "both sides fall to tier two and key
   identically." Nothing here needs fixing; it's confirmation that the 48 merges are landing on the
   right row, not a false collision.

2. **What's actually missing is diagnostic content, and one severity is actively wrong.** Two
   independent gaps, in the same function (`internal/store/merge.go`'s mismatch branch,
   `applyMergeTx`/`mergeEvents`):
   - `Warning.Detail` (`merge.go:143`) reads only `"sources %s and %s disagree on token counts for
     request_id %s"` — it names neither which of the six compared fields (`InputTokens`,
     `OutputTokens`, `CacheWrite5mTokens`, `CacheWrite1hTokens`, `CacheReadTokens`,
     `ThinkingTokens`) differ nor their two values. Confirming an actual disagreement for this plan
     required manually cross-referencing the DB row against the raw JSONL file by hand — the
     warning itself gave no lead.
   - One traced sample had `existing.Source == incoming.Source == "jsonl"` — the *same* collector
     re-observing its own transcript with different numbers on a later tailer pass. This is real
     and structurally different from a true cross-source (`proxy` vs `jsonl`) disagreement:
     `internal/jsonlogs/jsonlogs.go:300-303` documents that a file-rotation re-read is "absorbed as
     a merge, never a duplicate row" **only if the re-read content is byte-identical** to what was
     already ingested. Claude Code can amend a previously-written transcript line (e.g. finalizing
     usage once a stream that looked interrupted actually completes), which breaks that assumption
     and produces a same-source pair that legitimately differs. The current code has no branch for
     this — it reuses the identical `"sources %s and %s disagree..."` phrasing (reading as a
     cross-source conflict when it is a self-correction) at `SeverityError` (reading as urgent when
     it is expected and benign).

   Root cause of the *volume* of proxy-vs-jsonl disagreements (the dominant case, not the one-off
   jsonl-vs-jsonl sample) is one open hypothesis, not confirmed: Claude Code may stop consuming an
   SSE stream once it has a complete `tool_use` block (all 48 samples checked had
   `stop_reason=tool_use`) while `clens`'s cold-path parser processes bytes teed up to a possibly
   different point in the frame sequence — two independent readers of one stream, snapshotting usage
   at slightly different moments. **This plan does not attempt to fix or further diagnose that root
   cause** (self-review below explains why); beads 3-4 make every future occurrence self-explanatory
   instead.

### What changes

| File | Change | Why |
|---|---|---|
| `internal/store/merge.go` | **br-GI-15-03**: in the mismatch branch of `applyMergeTx`, build the `Warning.Detail` from the actual differing fields — only the ones `tokensDiffer` found unequal, each as `field existing_value vs incoming_value` — instead of the current field-free sentence. A small unexported helper (e.g. `diffTokenFields(existing, incoming *Event) string`) keeps `applyMergeTx` readable and gives the new behavior one place to test directly. | Turns the warning from "something disagreed, go look" into an actionable diagnostic — this plan's own investigation had to reconstruct this by hand from raw DB/JSONL data because the warning didn't say it |
| `internal/store/merge.go` | **br-GI-15-04**: in the same branch, when `existing.Source == incoming.Source`, emit a distinct `Detail` phrasing (a same-source re-read/self-correction, naming the shared source) and `Severity: "info"` instead of `"error"`; the existing `"sources %s and %s disagree..."` phrasing and `SeverityError` stay exactly as they are for the `existing.Source != incoming.Source` case. `Kind` stays `"source_mismatch"` in both cases — see rationale below. | A same-source pair is a structurally different, generally benign event (a corrected re-read) from a true two-source parsing disagreement, and today's single wording/severity conflates them, overstating the benign case as an `error` |
| `internal/store/merge_test.go` | New test(s): (a) a fixture that differs in exactly one known field (e.g. `OutputTokens`) asserts `Detail` names that field and both values, and does not name a field that didn't differ; (b) a same-source (`jsonl`/`jsonl`) fixture that differs on tokens asserts `Severity == "info"` and a detail phrasing distinct from the cross-source case; `TestMergeStillWarnsOnATrueDisagreement` (the existing cross-source pin) is checked to still pass unmodified — it only asserts `Kind`, not `Detail` text or `Severity`, so it is not expected to need a change, but the plan record's own convention (bead 2's precedent) is to verify existing tests explicitly rather than assume | Bead 3's acceptance check; bead 4's acceptance check; regression guard on the pre-existing cross-source case |

**Rationale for keeping `Kind: "source_mismatch"` rather than adding a new kind for the same-source
case:** `docs/context/testing-and-quality.md` and `internal/analyze/kinds.go` treat `Kind` as a
single, README-enforced spelling with one declared (static) `Severity` per kind
(`TestReadmeKindTableMatchesAllKinds`, `internal/analyze/readme_test.go:60-61` — checked against
`kinds.go`'s `allKinds` table, not against any individual `Warning` row written at runtime). That
static table's declared severity for `source_mismatch` (`error`) is unchanged by this addendum — it
documents the worst case. Nothing in the schema, the store package, or that test ties a *written*
`Warning.Severity` to the kind's declared value; `warnings.severity` is a free column, and
`internal/store` cannot import `internal/analyze` (its own hardcoded `"source_mismatch"`/`"error"`
string literals at `merge.go:141-142` are a pre-existing example of exactly this — the two packages
already don't share the Kind/Severity constants). Introducing a second `Kind` would additionally
require a new `README.md` table row, a `nonAnalyzeKinds` entry, and dashboard/kind-count surface
changes for what is otherwise the same conceptual finding at a different severity — YAGNI here per
this plan's own self-review (below) rejected it as unjustified surface area for a diagnostics-only
fix.

### Architecture / infrastructure changes

None. Both beads stay inside `internal/store`, touching only `merge.go` (and its test file). No new
import, no schema change, no new CLI command, no new route.

### Test strategy

- **Bead 3:** `diffTokenFields` (or equivalent) is tested directly against a hand-built pair of
  `Event`s differing in one field and, separately, in more than one field, asserting the returned
  string names exactly the differing field(s) with both values and omits identical fields. An
  end-to-end `mergeEvents`/`applyMergeTx`-level test (extending or sitting beside
  `TestMergeStillWarnsOnATrueDisagreement`) asserts the persisted `Warning.Detail` contains that
  same content.
- **Bead 4:** a same-source fixture (`existing.Source = incoming.Source = "jsonl"`, differing
  tokens, both `CaptureComplete`) asserts the persisted warning's `Severity == "info"` and a
  `Detail` that does not read as a cross-source claim (e.g. does not say "sources jsonl and jsonl
  disagree" verbatim — the whole point is that phrasing is misleading here). A cross-source fixture
  (`existing.Source = "proxy"`, `incoming.Source = "jsonl"`) in the same test table asserts
  `Severity == "error"` is unchanged, so the branch is proven both ways rather than only in the new
  direction.
- Full suite: `go build ./...`, `go vet ./...`, `go test ./internal/store/`, then `go test ./...`
  before pushing, per this repo's CLAUDE.md — same as beads 1-2.

### Risk areas / edge cases

- **`diffTokenFields`'s output must stay stable enough to test without being so rigid a future
  sixth token column breaks the test suite for an unrelated reason.** Build it by iterating the
  same six-field comparison `tokensDiffer` already lists (`merge.go:168-173`), naming each field
  from one shared table, so the two can never drift into checking different fields.
- **The `info`-severity same-source path must not swallow a *real* cross-source disagreement that
  happens to reuse a source name coincidentally.** The condition is a plain `existing.Source ==
  incoming.Source` string compare on the two merge inputs actually being merged — there is no third
  case where that could be true except the one it's meant to catch (two `Event`s cannot both claim
  `Source: "proxy"` and reach this branch by a different code path; `Source` is set once, at
  capture time, per `internal/consumer` and `internal/jsonlogs` each writing their own literal
  value).
- **No database migration.** `warnings.severity` is already a free-text column
  (`internal/store/schema.sql`); writing `"info"` instead of `"error"` for one case requires no
  schema change and is fully backward-compatible with every existing reader (`ListWarnings`, the
  dashboard's warnings tab, `AllKinds()`-driven kind descriptions).

### Context docs to refresh

- `internal/analyze/kinds.go`'s `KindSourceMismatch` `Description` string ("The same request_id
  arrived from two sources with disagreeing token counts.") is a source comment, not a
  `docs/context/*.md` file, and is not checked against `README.md` (only `Kind` + `Severity` +
  `nonAnalyzeKinds` membership are, per `readme_test.go`). It stays accurate for the common case;
  Phase 5.6 should check whether it's worth a short addition noting the same-source/`info` case
  exists, but this is optional polish, not a correctness requirement.
  `docs/context/testing-and-quality.md` and `architecture.md` were reviewed against beads 3-4: the
  `source_mismatch`/merge-idempotency rows already describe the mechanism at the level these beads
  operate within (a warning's content, not the merge's correctness rule), so Phase 5.6 should
  confirm no wording there becomes inaccurate rather than assume it, per this repo's own
  context-docs convention.

### Self-review

**As a senior engineer:** Scope is deliberately the two cheapest, highest-signal fixes from the
brainstorm, not the "confirm the SSE-race hypothesis" option. That third option requires
instrumenting a live tool-use call against DeepSeek and diffing byte-for-byte against Claude Code's
own (external, unowned) client behavior — an open-ended investigation with no guaranteed fix at the
end of it, since the divergence may originate entirely in Claude Code's own code. At 0.09% of calls
and no evidence of cost/billing impact (both sides already report `CaptureComplete` and a real,
if disagreeing, measurement — this is not the "zero vs measured" case `usageObserved` already
guards against), spending further investigation budget there is not justified by current evidence;
beads 3-4 are what make a future, better-resourced investigation of that root cause tractable (a
detailed `Detail` string is exactly what such an investigation would want logged already).

**As a QA engineer:** The two new tests must each stand on their own — bead 3's test should not
depend on bead 4's severity branch existing yet, and vice versa, so beadify (Phase 3) should keep
them independently implementable and independently revertible. Edge case worth naming: a mismatch
where *only one* of the six fields differs (the common case, per the sampled data) versus more than
one differing at once (also observed) — bead 3's test table should cover both, not just the
multi-field case, since a naive implementation could hardcode a fixed-width message assuming all
six are always reported.

**As a security engineer:** `diffTokenFields` reports only token *counts* (integers), never body or
header content — there is no new redaction surface here, unlike bead 2's log line. No new data
leaves the process boundary; this only changes what's already written to the local `warnings` table.

## Change History

### v7 (addendum — beads 3-4, source_mismatch diagnostics, not yet cross-reviewed)

- Added the source_mismatch investigation findings, beads 3-4 (`Warning.Detail` enrichment;
  same-source vs cross-source severity split), and their test/risk/context-doc sections. Beads 1-2
  are unchanged and remain converged (v6); this addendum starts its own review cycle.

### v6 (round-6 review — convergence)

- No findings raised. Added `**Status:** converged` to the header block to record that two consecutive clean rounds have been completed and the plan has converged.

### v5 (round-4 review)

- **F4.1 (JUSTIFIED + CONDUCTOR OVERRIDE):** Dropped the `Authorization`-header sentinel option from bead 2's test spec entirely. The sentinel must now be a distinctive JSON body value only. Added explicit rationale in the test spec: `redactHeaders` replaces `Authorization` with `"[redacted]"` at `proxy.go:122` before `captureState` is constructed, so the header value is structurally unreachable inside `submit()` — a negative-containment check on it would always pass regardless of implementation quality and provides zero regression coverage.

### v4 (round-3 review)

- **F3.1 (JUSTIFIED + CONDUCTOR OVERRIDE):** Extended bead-2 test spec to require a negative-containment assertion: the test request must carry a distinctive `Authorization` value or JSON body sentinel; after the positive path/method assertion, the test must assert via `if strings.Contains(logOutput, sentinel) { t.Errorf(...) }` (mirroring `rules_test.go:345-347`) that the drop-log output does not contain that sentinel. Turns the security self-review's prose guarantee into an enforced regression test.

### v3 (round-2 review)

- **F2.1 + CONDUCTOR OVERRIDE:** Widened bead 1 scope to five locations, adding `internal/web/app.js:191-193` (`captureMarker`'s doc comment). Added explicit note in the "What changes" row that `docs/context/dashboard.md` and `INDEX.md` both pin `app.js` at exactly 1142 lines, so if the corrected comment changes `app.js`'s total line count, those two docs' "1142 lines" figure requires a one-line update (grep/wc-l check, not a full REFRESH pass) in the same commit.
- **F2.2 (REJECTED):** No change — conductor directed this optional footnote be skipped; see round-2 changelog for rebuttal.

### v2 (round-1 review)

- **F1.1 + CONDUCTOR OVERRIDE:** Widened bead 1 scope in "What changes" table from `sink.go`
  alone to all four stale-comment locations (`sink.go:41-43`, `store/types.go:74-75`,
  `rules.go:174-175`, `kinds.go:74` Description).
- **F1.2 (PARTIAL):** Fixed the finding-2 narrative to acknowledge the observed 09-21 signature
  is over-counting (not under-counting), clarifying bead 2 as a forward-looking fail-open fix
  rather than the explanation for the historical anomaly.
- **F1.3 (JUSTIFIED):** Corrected "Architecture / infrastructure changes" to say no new
  *internal-package* import, while noting bead 2 does add a stdlib `log` import.
- **F1.4 (JUSTIFIED):** Added `t.Cleanup` restore discipline for `log.SetOutput` to the
  `proxy_test.go` row description.
- **F1.5 (JUSTIFIED):** Unified the drop log field list — added "auth kind" to the security
  self-review section to match the "What changes" table.
