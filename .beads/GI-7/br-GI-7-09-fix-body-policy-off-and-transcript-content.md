# Bead br-GI-7-09: `--body-policy` means what its docs say — in the proxy and in the transcript collector

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §4 (`internal/proxy` and
`internal/jsonlogs` rows), §10's *"What the run found that no bead covers"*

- **Bead ID**: br-GI-7-09
- **Priority**: P1 (high)
- **Original Estimate**: 6h
- **Dependencies**: br-GI-7-06 (the transcript columns this bead governs), br-GI-7-08 (the same class
  of defect, in the capture flag rather than the policy)
- **Blocks**: None

> **Provenance.** This bead is not from the plan's §9 sketch. It started as the Phase 5.6 refresh's
> finding — `internal/jsonlogs` never reads `BodyPolicy`, so `transcript_content` is stored whole and
> uncapped under `--body-policy off` — and the investigation that finding required turned up a larger
> one underneath it. Both halves are recorded here because the second one decides what the first one
> should do. A third question arrived while answering those two — *what does the policy mean?* — and
> it turned up a value that meant nothing at all; that is decision 9, and it is here rather than in a
> bead of its own because it is the same question the other two are answers to.

## Description

### Half A — `--body-policy off` records *nothing*, and says otherwise in three places

**As it was before this bead** — `New` in `internal/proxy/proxy.go` returned the bare
`httputil.ReverseProxy` before the handler closure that installs `stateKey` and the tees was ever
built:

```go
if cfg.BodyPolicy == "off" {
    return rp, nil
}
```

That closure — which sets `st := &captureState{…}` and injects it into the request context — sat
below that return, so under `off` no `captureState` exists, `ModifyResponse` is never
installed, and `ErrorHandler`'s `r.Context().Value(stateKey{})` type-asserts to `ok == false` and
submits nothing. **Zero rows reach the sink.**

Measured, not read — a throwaway probe posting one request through `proxy.New` with
`BodyPolicy: "off"`:

```
NO ROW under BodyPolicy=off — the call was not captured at all
```

Three places state the opposite:

| Where | Claims |
|---|---|
| `internal/cli/serve.go` `printBanner` | `WARNING: body capture is off — calls are recorded without their bodies.` |
| `docs/context/data-privacy-and-compliance.md` | `off` → "no body is captured" |
| `README.md` | "`--body-policy truncated` and `off` narrow that" |

`printBanner`'s own doc comment in `serve.go` even calls it *"the standing 'nothing is being
recorded' warning"* — the comment and the string it describes disagree, and the code agrees with the
comment. The failure shape is the bad one: an operator sets `off` to keep latency and status
observability while dropping content, and silently loses the records too. No test covers it —
`serve_test.go` asserts the banner's *text* and nothing about capture.

The README row above carries a second, unrelated defect — `truncated` in that sentence never narrowed
anything either. Both halves of it are corrected under decision 9, which leaves the sentence with
`off` alone.

### Half B — `internal/jsonlogs` never consults the policy at all

**As it was before this bead.** `BodyPolicy` was read at exactly one call site in the repo — `New` in
`internal/proxy/proxy.go` — and `internal/jsonlogs` wrote `ev.TranscriptContent = l.Message.Content`
unconditionally in `buildEvent`, so an install running `--body-policy off` still stored a transcript
line's `content` whole and uncapped. Neither the plan nor `br-GI-7-06` mentions the policy, which is
why this reads as an oversight rather than a choice.

Both of those sentences describe the code this bead replaces. They are kept in the past tense on
purpose: every claim in this section is about the state the bead was written against, and the shipped
code is what the decisions below describe. The distinction is not pedantry — it is the defect three
review rounds kept finding, where a sentence written before the change was read after it.

Half B cannot be specified without half A: if `off` means "no capture" in the proxy, the consistent
reading for the transcript collector is "write no row", which would silently stop `clens ingest`.
If `off` means what its banner says, the transcript's *content* is the body-analogue and the row
still belongs. **The user chose the second reading** — this bead makes the code match the docs.

### The decisions this bead makes

**1. `off` keeps the record and drops the content — in both sources.**

Proxy: `New` no longer returns early. Under `off` no `boundedBuffer` is allocated and no
`io.TeeReader` is installed; `ModifyResponse` is still installed so the row can carry status and
redacted headers, and it wraps the body in a `teeCloser` whose `r` and `c` are the original body and
whose `onClose` submits. That wrapper copies **no bytes** — it exists only to know when the call is
over — so the hot path is unchanged and `TestNoBufferingSSE` is the proof.

`jsonlogs`: the tailer stores no `TranscriptContent`/`TranscriptRole`, so both columns stay NULL —
"no transcript reconstruction", the same meaning a non-assistant line already has.

**2. Under `off`, `CaptureComplete` is `true`, and the merge — not the flag — carries the
consequence.**

The flag is about *narrowing*: a body cut at the cap, or a stream that ended early (`internal/sink/sink.go`, `CaptureComplete`'s doc).
Under `off` nothing was narrowed — the absence is a policy the operator set, uniformly, and visible
in `clens doctor`'s `body_policy` line. Setting it false is the alternative and it is worse: the
response's `Content-Type` survives `off`, so `parse.ExtractUsage` still reports `IsStream` true for a
streamed call and `ruleStreamIncomplete` would fire at `SeverityError` on *every* streamed request,
claiming a truncation that never happened. A flood of false error warnings is not a trade this repo
makes anywhere else.

But the flag alone does not carry the whole decision, and the first draft of this bead got that
backwards. `merge.go`'s pick is:

```go
case existing.CaptureComplete && incoming.CaptureComplete:
    winner = incoming            // <-- the incoming row always wins this case
```

Both rows are complete — the `off` proxy row by decision 2 above, every `jsonl` row by
`jsonlogs`' `buildEvent`, which sets it unconditionally — so **both write orderings take this one case** and only the argument order
differs. (Verified by instrumenting the switch, not by reading it; and confirmed a second way when
removing the guard below failed the test in *both* orderings rather than one.) With `jsonl` first,
`incoming` is the thin `off` row, so it takes the pick and **zeroes the transcript's observed
counts**. With the proxy row first, `incoming` is the `jsonl` row and the same case hands the win to
the side that has the numbers — harmless, but for a different reason than "a different branch runs".

The draft got this wrong twice, which is worth recording because the wrong version was convincing:
it said proxy-first was harmless because `!existing && incoming` fires there. That case cannot fire
at all — it requires `existing.CaptureComplete` false, and neither row is ever false here. The draft
also claimed `false` would cost the `off` row the pick, as if that were a cost, when losing that pick
is exactly what a row with nothing to contribute *should* do. Naming the hazard is not fixing it, so
this bead also corrects the pick.

**2a. The merge will not let an unobserved zero overwrite an observed count, and will not call it a
disagreement either.** Both orderings, because the thin row is the `incoming` argument in one and the
`existing` argument in the other — same `&&` case, opposite roles, so a fix that only handled the
losing ordering would pass half the time.

The rule the flag encodes is "prefer the more complete record", and a row that kept no body has no
record of usage to prefer — its zeros mean *never looked*, not *none*, which is the same distinction
`cost-and-quota.md` draws between an unpriced row and a `$0.00` one. Two changes follow, and the
second is the one the first draft missed:

- **The pick.** After the flag's decision, if the winner has no observed usage and the loser has
  some, the loser wins.
- **The warning, which is a separate bug.** `mismatch` was set from `tokensDiffer` alone in the `&&`
  case, so a 0-vs-N pair counted as a conflict and every `off`-policy merge grew a `source_mismatch`
  at `SeverityError`. That contradicts `mergeEvents`'s own contract — *"a 0-vs-N difference is not a
  disagreement"* — and it fired in the **ordinary proxy-first ordering**, not a race. A disagreement
  needs two measurements, so the condition now requires both sides to have observed usage. The swap
  above deliberately sets no mismatch at all, for the same reason: it happens *because* one side never
  looked, which is an absence rather than a conflict.

Both-zero and both-nonzero are untouched in both halves: the first has nothing to lose, and the
second is the ordinary two-captures case the rule was written for — including the case
`source_mismatch` exists to report, which is pinned by its own test so narrowing the condition cannot
silently stop the warning real disagreements need.

**3. Headers are still captured under `off`.** The flag is `--body-policy`, not `--capture-policy`,
and the banner's promise is "without their bodies". `req_headers`/`resp_headers` are redacted exactly
as before.

**4. The `off` row is thin, and that is the honest consequence.** A body that was never captured
cannot be parsed: `parse.ExtractMeta(nil, …)` yields an empty `Meta` and `parse.ExtractUsage(nil, …)`
yields a zero `Usage` — but *not* an inert one, since `ExtractUsage` switches on the `Content-Type`
header, which `off` still records. So the row carries method/path/status/headers/TTFB/duration and no
model, no tokens, no cost.

**Which warnings survive, read off each rule's own guard rather than from memory** — the rule list
here was wrong in the first draft, which named `upstream_error_body` among the survivors when it is
the one rule that provably cannot fire:

| Rule | Under `off` | Why |
|---|---|---|
| `cache_breakpoints_exceeded`, `cache_prefix_below_minimum` | inert | need `meta.CacheControlSites` / `meta.HasCacheControl` |
| `thinking_budget_rejected`, `thinking_display_omitted` | inert | need `meta.HasThinking` |
| `max_tokens_truncation`, `refusal` | inert | need `usage.StopReason` |
| `api_equivalent_cost` | inert | needs a priced subscription row; nothing was priced |
| `upstream_error_body` | **inert** | returns early on `len(ev.RespBody) == 0`, and `RespBody` is NULL here |
| `stream_incomplete` | inert | needs `!ev.CaptureComplete` — decision 2, not a zeroed input |
| `rate_limited`, `overloaded` | **fires** | key on `ev.Status` (429 / 529) |
| `auth_kind_anomaly` | **fires** | keys on `ev.AuthKind` and `ev.Path` |

Plus `upstream_error`, which is not a rule at all: it comes from `internal/consumer`'s
transport-failure path, and fires on an `off` row whenever the call errored — which is exactly the
path decision 8 repairs.

The session rules are all inert for the first reason: they read the token columns, which are zero.
This is recorded, not worked around: a row with no tokens is the truthful record of a call whose body
was not kept, and the three survivors are the ones that never needed the body.

**5. `jsonlogs` gets the cap too, and a `Set*` seam — not a `config` import.** `capBytes <= 0` means
*no cap*, so the tailer's own default (the seam unwired) never silently truncates to zero bytes. The
seam mirrors `SetPriceTable`/`SetAccount`/`SetModelBilling`, and is wired in `internal/cli/ingest.go`'s
`newTailer` — the one place both `clens ingest` and `addCollectors` build a tailer, so the two cannot
drift.

**6. A capped transcript is *not* a capped capture: `CaptureComplete` is not cleared, and
`transcript_content` is not a body.** `br-GI-7-08` widened the flag to cover both *teed* bodies, and
this is deliberately not a third. `transcript_content` lives in its own columns precisely because it
is a reconstruction rather than a capture (`br-GI-7-06`), and the flag's meaning is "was a *capture*
narrowed" — a reconstruction cut at a cap narrows nothing that was ever captured.

The first draft gave a second reason that 2a then invalidated: that clearing the flag would cost a
`jsonl` row the merge token pick. With the observed-usage guard in place it no longer would — the
guard protects a row with measured counts regardless of the flag. The decision stands on the first
ground alone, and the truncation is made visible the way a body's is — by length against the cap —
which is what decision 7 adds. Pinned by a test, so a later change to it is a decision rather than a
side effect.

**7. The dashboard's transcript section gets the cap marker the bodies have.** `app.js`'s transcript
block already labels its provenance; it gains the same length-against-`BodyCapBytes` comparison
`readPathMarker` uses, so a capped reconstruction cannot look identical to a whole one. This is the
lesson `br-GI-7-08` was about, applied to the third content column rather than discovered again. Its
wording names the measurement and then the inference, because a content exactly the cap's length may
simply have been that long — the same hedge `captureMarker` carries.

**8. `requestID` must tolerate a nil `reqBody`, which the first draft did not.** `captureState.requestID`
hashes `st.reqBody` to build its fallback key, and under `off` that field is nil, so
`(*boundedBuffer).Bytes` dereferenced a nil receiver. It fires on any response with no `Request-Id`
header and **always** on the `ErrorHandler` path, where `respHeaders` is nil and the early return
cannot help — and there the panic lands *before* `WriteHeader(502)`, so a failed upstream returned an
aborted request instead of a bad gateway. That is a fail-open regression against CLAUDE.md's "a
broken observer never breaks the user's coding session", and `net/http` recovers handler panics and
logs them, so the whole suite stayed green while the beacon row was silently never submitted. The
guard goes in `requestID`, not at its call sites, because both paths reach the same line; the hash of
a body we did not keep is a constant, and this branch is already the fallback whose uniqueness comes
from `started_at_ns` and the attempt counter.

**9. The third policy value is removed, not implemented.** Answering *"what does `--body-policy`
mean?"* for half A turned up a value that meant nothing: `config.Validate` accepts `truncated` and
`proxy.go`'s only policy branch is `== "off"`, so `full` and `truncated` executed byte-identical
code, both bounded by `--body-cap-bytes`. Leaving it was the smaller diff and is what this bead
originally recorded. It is rejected instead, because of what the value *reads* as: `truncated` says
*narrow it* to an operator who set it precisely so the bodies would not be stored whole, and the
thing it actually does is store them whole. Making the two genuinely differ is the alternative, and
it is the worse one — with one cap already bounding the capture, `truncated` has nothing left to
control, so implementing it would mean inventing a *second* cap or else redefining `full` as
uncapped, which changes the blast radius of the default. A value that cannot be implemented
coherently should not be accepted. It fails at startup, from `config.toml` or a flag alike, with a
message naming `full` and `off`; the flag's own help text now advertises only those two.

### What this bead does not fix

**A request body upstream never reads in full** is still stored as a prefix with no marker — the
pre-existing gap `br-GI-7-08` names in its own §"What this bead does not fix".

**The fix is not retroactive.** Rows already written under `off`-with-no-capture remain absent, and
rows already holding uncapped `transcript_content` keep it. No client-side logic re-derives either.

## Rationale

Two defects of one shape, found in the order that made them findable: the refresh asked *"does the
policy reach source B?"*, and answering it required asking *"what does the policy mean?"* — which is
where the proxy defect was.

Both are the "a control that looks like it covers X and does not" class the story has already met
once. `CaptureComplete` was derived from one buffer of two; `--body-policy off` governs one source of
two, and its own banner describes a behaviour it does not have. The first was invisible to every
fixture in the story; the second is invisible to every test in the repo, including the one that reads
the string it contradicts.

The second is worth the larger diff for a reason beyond consistency: it is the *safe* direction to be
wrong in. Today an operator reaching for the most private setting gets the most data withheld and
believes the opposite. After this bead the setting does what the flag, the banner, the README, and
the context doc all say.

## Outcome Definition

- A call through `proxy.New` with `BodyPolicy: "off"` produces a row at the sink carrying status,
  redacted `req_headers`/`resp_headers`, method, path, `TTFB` and `Duration` — and `ReqBody` and
  `RespBody` both NULL. Confirmed by a test, and by the probe that found the defect.
- `CaptureComplete` is true on that row, and no `stream_incomplete` warning is produced for it.
- `printBanner`'s banner string is **true as written**: no string change, but its doc comment stops
  calling it a "nothing is being recorded" warning.
- A transcript line ingested with the policy wired to `off` stores no `transcript_content` and no
  `transcript_role`; with the policy wired to `full` and a small cap, the stored content is exactly
  the cap and the row's `CaptureComplete` is **unchanged from what it would otherwise be**.
- `newTailer` wires the policy, so `clens ingest`, `clens serve` and `clens refresh` cannot disagree.
- The dashboard's transcript section draws a cap marker for content whose length equals
  `BodyCapBytes`, and none otherwise — and the assertion covers that the branch *calls* it, not only
  that the function is correct.
- A request with no `Request-Id` header, and an unreachable upstream, both produce a row and the
  latter still answers `502`: no panic escapes `submit`, in either path.
- A merge between a transcript row and a bodyless complete row keeps the transcript's observed
  counts in **both** write orderings, and raises **no** `source_mismatch` — an absent side is not a
  disagreeing one.
- Two complete captures that *do* disagree on tokens still raise `source_mismatch`, so the gate
  above cannot quietly stop the warning the kind exists for.
- `TestNoBufferingSSE` passes unchanged for `full`, and is extended to cover `off` — the gate on
  half A being free, on the policy that adds a second wrapper to the response body.
- `Validate` accepts exactly `full` and `off`, and rejects `truncated` with a message naming those
  two — asserted in **both** directions, since a negative-only assertion would have been green for
  the value's entire inert life.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

- Unit Tests (`internal/proxy/proxy_test.go`):
  - **`TestCaptureRecordsUnderPolicyOff`** — the bead's central assertion, and the one that fails
    today at the *sink read*, not at an assertion inside a handler: with `BodyPolicy: "off"`, post one
    request through the real handler and require a `CapturedCall` within the timeout. Then assert
    `ReqBody == nil`, `RespBody == nil`, `Status == 200`, `CaptureComplete == true`, and that
    `ReqHeaders`/`RespHeaders` are non-empty.
  - **The redaction half is not skipped by the off path** — the same call carrying an
    `Authorization` header must still arrive with it redacted, or `off` becomes a route around the
    one control that must apply unconditionally.
  - **`TestNoBufferingSSE` and the `full`-policy cases unchanged** — the existing suite is the
    regression gate for half A.
- Unit Tests (`internal/jsonlogs/jsonlogs_test.go`):
  - Policy `off`: a transcript line with `message.content` yields an event with
    `TranscriptContent` nil and `TranscriptRole` empty.
  - Policy `full` with a cap smaller than the content: `TranscriptContent` is exactly the cap, and
    `CaptureComplete` is true — decision 6, and the assertion that fails if someone later wires the
    flag to the cap.
  - The seam unwired (`capBytes <= 0`): content is stored whole, so a caller that forgets the seam
    cannot silently truncate every transcript to zero.
- Unit Tests (`internal/cli/cli_test.go`): `newTailer` propagates the configured policy and cap —
  the wiring, which is the half a unit test of either endpoint would miss. *(This bullet named
  `internal/cli/ingest_test.go` in the bead's first draft. That file does not exist — `internal/cli`'s
  test files are `cli_test.go`, `additions_test.go`, `replay_test.go`, `serve_test.go` and
  `doctor_test.go` — and the draft's Files to Touch named no `internal/cli` test file at all, so an
  implementer following it literally would have been sent to a file that isn't there. Caught by the
  implementation cross-review as a spec escalation; the shipped test is
  `TestNewTailerWiresTheBodyPolicy`.)*
- Unit Tests (`internal/store/merge_test.go`), a pair — the second is what stops the first from
  being satisfied by a fix that is too broad:
  - `TestMergeDoesNotLetABodylessRowZeroObservedUsage`: a bodyless complete row and a transcript row
    for the same `request_id`, **in both orderings**, asserting the merged row keeps the observed
    counts *and* that no `source_mismatch` was attached. Both orderings are needed because the thin
    row is `incoming` in one and `existing` in the other, so a fix that handled only the losing one
    would pass half the time. The fixture must zero **all six** token columns, not the two the
    assertion reads: `fullEvent` populates the cache columns, and leaving them set makes the fixture
    a row that *had* been measured — the opposite of the shape under test. That is not hypothetical;
    the first version of this test did exactly that and failed for the wrong reason.
  - `TestMergeStillWarnsOnATrueDisagreement`: two complete captures, both measured, differing on the
    numbers. Narrowing the mismatch condition too far would silently stop reporting real
    disagreements, and nothing else in the suite would notice.
- Unit Tests (`internal/web/assets_test.go`): the transcript section's cap comparison, as a
  source-shape assertion with `TestAssetsTheBodyRendererEscapes`'s stated ceiling unchanged.
- Unit Tests (`internal/config/config_test.go`): `TestBodyPolicyAcceptsExactlyTheValuesThatDoSomething`
  — decision 9. The positive half is the half that matters and the reason the test exists: `Validate`
  accepting `full` and `off` is what the proxy's one policy branch depends on, and no prior test
  asserted it. The negative half covers `truncated` and three shapes that were never advertised
  (`sometimes`, `""`, `FULL`), and requires the `truncated` error to *name* the accepted values —
  someone hitting it needs to be told what to set instead, and the old message advertised the value
  being removed.
- Integration Tests: none — no API or schema change. `transcript_content` is already nullable, so
  half B needs no migration; confirm no `user_version` bump is required.
- E2E: none. The manual-run confirmation, if taken, is against a store copy on alternate ports, the
  way `br-GI-7-07` did it.

## Files to Touch

- `internal/proxy/proxy.go` (modify — remove `New`'s `off` early return; make the buffers and the
  `io.TeeReader` conditional on the policy; guard `reqBody` in **both** `submit` and `requestID`,
  per decision 8)
- `internal/proxy/proxy_test.go` (modify — the off-policy capture and redaction cases, and the
  missing-`Request-Id` / unreachable-upstream pair)
- `internal/jsonlogs/jsonlogs.go` (modify — the `SetBodyPolicy` seam beside the existing `Set*`s, and
  the content write in `buildEvent`)
- `internal/jsonlogs/jsonlogs_test.go` (modify — the three policy cases)
- `internal/cli/ingest.go` (modify — wire the policy in `newTailer`)
- `internal/cli/cli_test.go` (modify — the wiring assertion; see the Test Specifications note)
- `internal/cli/serve.go` (modify — `printBanner`'s doc comment only; the string is
  already correct)
- `internal/store/merge.go` (modify — the observed-usage guard after the completeness pick, decision 2a)
- `internal/store/merge_test.go` (modify — that guard, in both orderings)
- `internal/web/app.js` (modify — the transcript section's cap marker)
- `internal/web/assets_test.go` (modify — the assertion for it, including that it is *called*)
- `internal/config/config.go` (modify — decision 9: the `Validate` case and its reason, and the flag's
  help text, which advertised `truncated`)
- `internal/config/config_test.go` (modify — the accepted-set test, in both directions)
- `README.md` (modify — decision 9; the sentence named only the two values that still work)
