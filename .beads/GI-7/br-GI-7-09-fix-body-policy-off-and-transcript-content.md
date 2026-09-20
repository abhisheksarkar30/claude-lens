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
> should do.

## Description

### Half A — `--body-policy off` records *nothing*, and says otherwise in three places

`internal/proxy/proxy.go:59` returns the bare `httputil.ReverseProxy` before the handler closure that
installs `stateKey` and the tees is ever built:

```go
if cfg.BodyPolicy == "off" {
    return rp, nil
}
```

The closure at `:96-122` — which sets `st := &captureState{…}` and injects it into the request
context — is below that return, so under `off` no `captureState` exists, `ModifyResponse` is never
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
| `internal/cli/serve.go:416` | `WARNING: body capture is off — calls are recorded without their bodies.` |
| `docs/context/data-privacy-and-compliance.md:39` | `off` → "no body is captured" |
| `README.md:252` | "`--body-policy truncated` and `off` narrow that" |

`serve.go:408-409`'s doc comment on `printBanner` even calls it *"the standing 'nothing is being
recorded' warning"* — the comment and the string it describes disagree, and the code agrees with the
comment. The failure shape is the bad one: an operator sets `off` to keep latency and status
observability while dropping content, and silently loses the records too. No test covers it —
`serve_test.go:126` asserts the banner's *text* and nothing about capture.

### Half B — `internal/jsonlogs` never consults the policy at all

`BodyPolicy` is read at exactly one call site in the repo (`proxy.go:59`). `internal/jsonlogs` writes
`ev.TranscriptContent = l.Message.Content` unconditionally (`jsonlogs.go:412`), so an install running
`--body-policy off` still stores a transcript line's `content` whole and uncapped. Neither the plan
nor `br-GI-7-06` mentions the policy, which is why this reads as an oversight rather than a choice.

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

**2. Under `off`, `CaptureComplete` is `true`.** The flag is about *narrowing* — a body cut at the
cap, or a stream that ended early (`sink.go:41-44`) — and under `off` nothing was narrowed; the
absence is a policy the operator set, uniformly, and visible in `clens doctor`'s `body_policy` line.
Setting it false would make `analyze`'s `stream_incomplete` fire on every 200 and would make every
`off` row lose the merge token pick, for a reason that has nothing to do with either rule's subject.

**3. Headers are still captured under `off`.** The flag is `--body-policy`, not `--capture-policy`,
and the banner's promise is "without their bodies". `req_headers`/`resp_headers` are redacted exactly
as before.

**4. The `off` row is thin, and that is the honest consequence.** A body that was never captured
cannot be parsed: `parse.ExtractMeta(nil, …)` yields an empty `Meta` and `parse.ExtractUsage(nil, …)`
yields a zero `Usage`, so the row carries method/path/status/headers/TTFB/duration and no model, no
tokens, no cost. Walking `internal/analyze/rules.go`: every event-level rule keys on `meta`, `usage`,
or `ev.StopReason` and goes inert, leaving only the ones that key on `ev.Status`/`ev.AuthKind`
(`rate_limited`, `overloaded`, `upstream_error_body`, `auth_kind_anomaly`) — which is precisely the
set that remains useful without bodies. This is recorded, not worked around: a row with no tokens is
the truthful record of a call whose body was not kept.

**5. `jsonlogs` gets the cap too, and a `Set*` seam — not a `config` import.** `capBytes <= 0` means
*no cap*, so the tailer's own default (the seam unwired) never silently truncates to zero bytes. The
seam mirrors `SetPriceTable`/`SetAccount`/`SetModelBilling`, and is wired in `internal/cli/ingest.go`'s
`newTailer` — the one place both `clens ingest` and `addCollectors` build a tailer, so the two cannot
drift.

**6. A capped transcript is *not* a capped capture: `CaptureComplete` is not cleared, and
`transcript_content` is not a body.** `br-GI-7-08` widened the flag to cover both *teed* bodies, and
this is deliberately not a third. `transcript_content` lives in its own columns precisely because it
is a reconstruction rather than a capture (`br-GI-7-06`), and clearing the flag would make a `jsonl`
row lose the merge token pick over a column the merge never reads for tokens. The truncation is
instead made visible the way a body's is — by length against the cap — which is what decision 7
adds. Pinned by a test, so a later change to it is a decision rather than a side effect.

**7. The dashboard's transcript section gets the cap marker the bodies have.** `app.js`'s transcript
block already labels its provenance; it gains the same length-against-`BodyCapBytes` comparison
`readPathMarker` uses, so a capped reconstruction cannot look identical to a whole one. This is the
lesson `br-GI-7-08` was about, applied to the third content column rather than discovered again.

### What this bead does not fix

**`--body-policy truncated` is still a no-op.** `config.go:316` accepts it and `proxy.go`'s only
policy branch is `== "off"`, so `truncated` and `full` execute byte-identical code, both bounded by
`--body-cap-bytes`. This bead does not change that: making the two differ means deciding whether
`full` should mean *uncapped*, which is a real design question about the default's blast radius on a
256 KB-bounded capture, and `README.md:252`'s "`--body-policy truncated` … narrow[s] that" stays an
overclaim until it is answered. Recorded here so it is not lost; out of scope.

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
- `serve.go:416`'s banner is **true as written**: no string change, but `printBanner`'s doc comment
  (`:408-409`) stops calling it a "nothing is being recorded" warning.
- A transcript line ingested with the policy wired to `off` stores no `transcript_content` and no
  `transcript_role`; with the policy wired to `full` and a small cap, the stored content is exactly
  the cap and the row's `CaptureComplete` is **unchanged from what it would otherwise be**.
- `newTailer` wires the policy, so `clens ingest`, `clens serve` and `clens refresh` cannot disagree.
- The dashboard's transcript section draws a cap marker for content whose length equals
  `BodyCapBytes`, and none otherwise.
- `TestNoBufferingSSE` passes unchanged — the gate on half A being free.
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
- Unit Tests (`internal/cli/ingest_test.go`): `newTailer` propagates the configured policy and cap —
  the wiring, which is the half a unit test of either endpoint would miss.
- Unit Tests (`internal/web/assets_test.go`): the transcript section's cap comparison, as a
  source-shape assertion with `TestAssetsTheBodyRendererEscapes`'s stated ceiling unchanged.
- Integration Tests: none — no API or schema change. `transcript_content` is already nullable, so
  half B needs no migration; confirm no `user_version` bump is required.
- E2E: none. The manual-run confirmation, if taken, is against a store copy on alternate ports, the
  way `br-GI-7-07` did it.

## Files to Touch

- `internal/proxy/proxy.go` (modify — remove the `off` early return at `:59`; make the buffers and
  the `io.TeeReader` conditional on the policy; guard `reqBody` in `submit` at `:225`)
- `internal/proxy/proxy_test.go` (modify — the off-policy capture and redaction cases)
- `internal/jsonlogs/jsonlogs.go` (modify — the `SetBodyPolicy` seam beside the existing `Set*`s, and
  the content write at `:411-414`)
- `internal/jsonlogs/jsonlogs_test.go` (modify — the three policy cases)
- `internal/cli/ingest.go` (modify — wire the policy in `newTailer`, `:79-93`)
- `internal/cli/serve.go` (modify — `printBanner`'s doc comment at `:408-409` only; the string is
  already correct)
- `internal/web/app.js` (modify — the transcript section's cap marker)
- `internal/web/assets_test.go` (modify — the assertion for it)
