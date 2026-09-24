# GI#15: DeepSeek residual capture gap — close the two proxy-side blind spots

**Version:** 11
**Status:** converged — beads 1-2 shipped in open PR; beads 3-4 addendum converged (v11), ready for beadification
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
     a merge, never a duplicate row" unconditionally — the UNIQUE constraint merges regardless of
     whether the re-read content matches what was already ingested. Claude Code can amend a
     previously-written transcript line (e.g. finalizing usage once a stream that looked interrupted
     actually completes), and the merge path does not distinguish that case from a byte-identical
     re-read, which is exactly why an amended transcript line surfaces as a same-source mismatch
     instead of being silently deduped. The current code has no branch for
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
| `internal/store/merge.go` | **br-GI-15-03**: in the mismatch branch of `applyMergeTx`, build the `Warning.Detail` from the actual differing fields — only the ones `tokensDiffer` found unequal, each as `field existing_value vs incoming_value` — instead of the current field-free sentence. Extract the six-field list that `tokensDiffer` currently compares (`merge.go:168-173`) into one shared `[]struct{ name string; get func(*Event) int }` slice (or equivalent) and have both `tokensDiffer` and the new helper iterate it, so the two can never drift into checking different fields. A small unexported helper (e.g. `diffTokenFields(existing, incoming *Event) string`) built on that table keeps `applyMergeTx` readable and gives the new behavior one place to test directly. | Turns the warning from "something disagreed, go look" into an actionable diagnostic — this plan's own investigation had to reconstruct this by hand from raw DB/JSONL data because the warning didn't say it; the shared table is load-bearing: a bespoke helper with its own independent six-field enumeration would satisfy the acceptance criteria while silently leaving two independently-maintained lists in `merge.go` |
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
  incoming.Source` string compare on the two merge inputs actually being merged. `Source` is set
  once, at capture time, per `internal/consumer` and `internal/jsonlogs` each writing their own
  literal value, so there is no common path where two genuinely distinct sources share the same
  name. **Accepted residual risk:** a same-source pair can also arise from two independent captures
  that collide on a fallback-derived key — e.g. two DeepSeek completions that happen to reuse the
  same `message.id` (a provider-side anomaly; `internal/jsonlogs/dedup.go:96-106` explains why
  DeepSeek falls back to `message.id`). In that scenario the same-source branch would classify a
  true duplicate/collision as `info` rather than `error`. This is a low-probability risk this addendum surfaces — it depends on the upstream provider
  issuing a duplicate `message.id` across two distinct calls, which is outside `clens`'s control.
  `docs/context/decisions/008-three-tier-identity-key.md` addresses only convergence of the *same*
  request across two writers and does not cover independent-call collision. GI-9 does name a
  mechanistically adjacent risk (a replay/cache response returning the same `message.id` for two
  distinct calls, causing them to collapse into one row) and explicitly accepts that
  collapse-to-one-row outcome as current intended behaviour, pinned by a test
  (`GI-9-merge-jsonl-and-proxy-rows.md:1716-1727`); the specific trigger introduced here — an
  upstream provider independently issuing a duplicate id rather than internal replay — is novel to
  this addendum, while the resulting row-collapse consequence follows the same already-accepted
  pattern. Bead 4 does not change its likelihood, only its presentation — and it was never
  observed in the 48 sampled rows.
- **No database migration.** `warnings.severity` is already a free-text column
  (`internal/store/schema.sql`); writing `"info"` instead of `"error"` for one case requires no
  schema change and is fully backward-compatible with every existing reader (`ListWarnings`, the
  dashboard's warnings tab, `AllKinds()`-driven kind descriptions).

### Context docs to refresh

- `internal/analyze/kinds.go` has two separate `source_mismatch`-related strings that require
  different treatment after bead 4 ships. The `KindSourceMismatch` `Description` string ("The
  same request_id arrived from two sources with disagreeing token counts.") is a source comment,
  not a `docs/context/*.md` file, and is not checked against `README.md` (only `Kind` +
  `Severity` + `nonAnalyzeKinds` membership are, per `readme_test.go`). It stays accurate for
  the common case; Phase 5.6 should check whether it's worth a short addition noting the
  same-source/`info` case exists, but this is optional polish, not a correctness requirement.
  **The `nonAnalyzeKinds[KindSourceMismatch]` entry at `kinds.go:104` ("store (cross-source
  merge)") is different in kind:** it is the user-facing origin label mirrored verbatim into
  `README.md:256` (the kind table's third column) and cited in `README.md:262`, enforced 1:1 by
  `readme_test.go`'s `row[2] != by` check. The test keeps the two in lockstep but does not
  check semantic accuracy against what bead 4 actually does, so CI will not catch the staleness.
  After bead 4 ships, both will describe as exclusively cross-source a warning that can now also
  arise same-source. Phase 5.6 must update `nonAnalyzeKinds[KindSourceMismatch]` (e.g. "store
  (cross-source or same-source merge)") and verify `README.md:256,262` are updated to match —
  the same "cross-source only" → "cross-source or same-source" broadening already required for
  `glossary.md:21` and `cost-and-quota.md:175`.
  `docs/context/testing-and-quality.md` and `architecture.md` were reviewed against beads 3-4: the
  `source_mismatch`/merge-idempotency rows already describe the mechanism at the level these beads
  operate within (a warning's content, not the merge's correctness rule), so Phase 5.6 should
  confirm no wording there becomes inaccurate rather than assume it, per this repo's own
  context-docs convention.
- **`docs/context/glossary.md:21`** — "a second **source** arriving with disagreeing counts for
  the same id is a `source_mismatch` warning" frames `source_mismatch` as an inherently
  cross-source phenomenon. Once bead 4 ships, same-source pairs also raise it. Phase 5.6 must
  update this to cover both cases (e.g. "a second observation of the same id — from another source
  or from the same source re-reading with amended counts — is a `source_mismatch` warning").
- **`docs/context/cost-and-quota.md:175`** — "the same `request_id` arriving from **two sources**
  with disagreeing token counts" is similarly cross-source only. Phase 5.6 must broaden this to
  acknowledge the same-source/self-correction case bead 4 adds.

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

### v11 (round-11 review — F11.1 JUSTIFIED)

- **F11.1:** Expanded the first "Context docs to refresh" bullet to cover `nonAnalyzeKinds
  [KindSourceMismatch]` at `kinds.go:104` and its two README mirrors (`README.md:256,262`). The
  previous text addressed only the `Description` string (optional polish) and missed this
  adjacent map entry, which is user-facing, test-enforced to stay in lockstep with `README.md`,
  and will be materially inaccurate after bead 4 ships (describes as cross-source-only a warning
  that bead 4 makes same-source-capable too). Phase 5.6 is now explicitly tasked with the same
  "cross-source only" → "cross-source or same-source" broadening for these two files as for
  `glossary.md:21` and `cost-and-quota.md:175`.

### v10 (round-9 review — F9.1 JUSTIFIED)

- **F9.1:** Corrected the risk-provenance claim in the same-source fallback-key bullet. The
  previous text said the "two distinct calls colliding on a shared message.id" risk was "not
  discussed or accepted in any prior decision record." That overclaimed against GI-9, which at
  lines 1716-1727 explicitly names a mechanistically adjacent risk (replay/cache returning the
  same `message.id` for two distinct calls, collapsing them to one row) and accepts the
  collapse-to-one-row outcome as current intended behaviour, pinned by a test. The new text
  correctly narrows: `decisions/008` is indeed narrower (same-request convergence only); GI-9
  does accept the general outcome; what is novel to this addendum is the specific trigger — an
  upstream provider independently issuing a duplicate id — not the resulting row-collapse pattern.

### v9 (round-8 review — F8.1–F8.2 both JUSTIFIED)

- **F8.1:** Corrected the shared-table element type in the bead 3 "What changes" row from
  `get func(*Event) int64` to `get func(*Event) int`, matching the actual declared type of all six
  token fields on `EventSummary` (`internal/store/types.go:32-37`). The previous signature would
  not compile without an explicit `int64(...)` cast nowhere mentioned in the plan.
- **F8.2:** Replaced the inaccurate "same risk beads 1-2 already accept" framing in the Risk
  section's same-source fallback-key bullet with accurate language: this is a new, low-probability
  risk the addendum names for the first time. GI-9 and `decisions/008-three-tier-identity-key.md`
  address convergence of the *same* request across two writers (disjoint namespaces, same upstream
  id), not two *independent* calls colliding on a provider-issued id — a structurally different
  scenario that no prior decision record discusses or accepts.

### v8 (round-7 review — F7.1–F7.4 all JUSTIFIED)

- **F7.1:** Added `docs/context/glossary.md:21` and `docs/context/cost-and-quota.md:175` to
  "Context docs to refresh", each with a named stale claim and Phase 5.6 update instruction.
- **F7.2:** Folded the shared-table requirement from the Risk section into the bead 3 "What
  changes" row: implementer must extract `tokensDiffer`'s six-field `||` chain into one shared
  slice and have both `tokensDiffer` and `diffTokenFields` iterate it.
- **F7.3:** Replaced the "there is no third case" assertion in Risk areas with a named accepted
  residual risk (same-source collision on a fallback-derived key, same probability as beads 1-2's
  already-accepted cross-source collision risk).
- **F7.4:** Corrected Findings §2's misquote of `jsonlogs.go:300-303`: the comment makes no
  "byte-identical" precondition; the merge is unconditional, which is precisely why an amended
  transcript produces a same-source mismatch.

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
