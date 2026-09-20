# GI-9 — The same request is stored twice under two keys that cannot match, and the transcript inflates itself when its `requestId` is absent

## 1. Summary

Under proxy mode, one request produces two rows that never merge, and the JSONL tailer additionally
inflates *itself* by about a third. Both are one defect: the cross-source merge keys on
`events.request_id`, and neither writer produces a value the other can produce.

The two writers mint namespaced fallback keys — `jsonl:<sessionId>:<uuid>` and
`proxy:<sha256>:<started_at_ns>:<attempt>` — that are unique to their writer *by construction*, so
`insertOrMerge` never reaches its merge branch. On the live install, **0 rows share a `request_id`
across sources**, and 0 of 184 proxy session ids appear as JSONL sessions.

The proxy's documented key — the upstream `request-id` response header — is never available on this
endpoint: **0 of 728 stored proxy responses carry one**, and the proxy records **0** `request-id`
**request** headers of ~1,400. `requestId` is correspondingly absent from exactly the transcripts
written through the endpoint that omits it, and the correlation is exact: of the **2,807** transcript
lines whose `message.id` matches a stored proxy body id, **2,807 carry no `requestId`** — zero
exceptions. One mixed session (`4842d0f2…`) runs both models from the one client against **two
endpoints** — 1,380 `claude-sonnet-5` lines, every one carrying a `requestId`, against 113
`deepseek-flash` lines, none carrying one — and `requestId` is present **precisely when the request did
not traverse this proxy**. The plan's *Cross-source identity* assumption (test 11b) was therefore never
merely unverified — it is unverifiable as written.

**The identity already exists on both sides and is simply unused.** The upstream mints a message id,
the proxy stores it inside the response body it already keeps, and Claude Code writes the same value
into the transcript as `message.id`. Of the **663** distinct ids the proxy stores, **511 appear
verbatim as a transcript `message.id`** — and no id appears in more than one transcript file. The
relation is existence, not a match: no window, tolerance, or tie-break is involved.

This plan makes that value the key on both sides. It is one rule, applied forward and backward, with
no heuristic anywhere — which matters, because the fallback the plan originally proposed cannot work:
the documented key matches **none** of the proxy rows at all, and even a deliberately generous variant
of it resolves only **199 of 660** rows to a single candidate, because cache-read counts repeat across
a session's turns.

Two things change beyond the key itself:

- **The proxy stops deciding identity.** The rule moves to the cold path, in one function, because
  the answer now depends on a parsed response body — which the hot path must not touch.
- **A backfill retires the historical duplicates**, because forward-only would leave an estimated
  28,405 surplus rows and every double-counted total in place.

One thing that *looked* like it needed changing does not. A merge cannot move a row between sessions —
the incoming event's insert fails on the unique constraint before it ever holds a row, and the update
lands on the existing row by id — so reconciling the survivor's session is already the whole job. The
inverse is the natural guess, and it was this plan's own first answer.

## 2. Evidence base

Measured against the live install (`~/.clens/lens.db`, read-only) and the real transcripts under
`~/.claude/projects/` on 2026-09-20. Except where a figure is explicitly labelled a **re-measure**
(§2.3), every figure in this section comes from **one read-only pass**, and every denominator below is
that pass's denominator.

**The absolute counts are perishable and the structural facts are not.** The database is written to
while it is read — the proxy is serving the session that produced this plan, including the subagents
that reviewed it — and it grew from 338 to 728 proxy rows across the course of that review. A later
reader who re-measures and gets different numbers should read that as the database having grown, not as
a defect. So the design rests on the facts that do not move: the two writers' key *namespaces* are
disjoint by construction (§2.1), the documented fallback is *structurally* incapable of matching (§2.5),
and the message id is the same value on both sides because it comes from the same upstream response
(§2.3). The counts below are what those facts looked like at one moment — and where a figure has been
re-measured on a larger corpus, the re-measure's *relation* is stated beside it, because the relation is
what the design rests on and the absolute is not (§2.3).

**The premise survived both an adversarial review round and an independent re-measurement using a
stricter extractor, and the one place this story's own defect could have been hiding — a proxy row
whose transcript counterpart exists under a *different* id — is now tested and empty (§2.3). Across ten
review rounds, that is the strongest statement anyone has been able to make about this foundation.**

### 2.1 The two writers mint keys that cannot collide

`requestKey` ([internal/jsonlogs/dedup.go:100-105](../../internal/jsonlogs/dedup.go#L100-L105)) falls
back to `jsonl:<sessionId>:<uuid>`; `captureState.requestID`
([internal/proxy/proxy.go:268-291](../../internal/proxy/proxy.go#L268-L291)) falls back to
`proxy:<sha256(body)>:<started_at_ns>:<attempt>`. Each is namespaced by its own writer. The
namespace is deliberate — the `attempt` counter exists so two byte-identical bodies never collapse —
but the effect is that a proxy row and a JSONL row for the *same* request can never share a key.

Consequences measured:

| | |
|---|---|
| Rows sharing a `request_id` across sources | **0** |
| JSONL rows on the `jsonl:` fallback | 71,228 of 88,032 |
| JSONL rows with a real `requestId` | 16,804 |
| Proxy rows | 728 (all on the `proxy:` fallback) |
| Proxy session ids that appear as JSONL sessions | **0 of 184** |

### 2.2 `requestId` is present exactly when the request did not traverse this proxy

Across all 728 stored proxy responses, the observed header set is
`date, server, vary, access-control-allow-credentials, strict-transport-security, via, x-amz-cf-id,
x-amz-cf-pop, x-cache, x-content-type-options, x-ds-trace-id, content-type, cache-control,
content-encoding, content-length, cf-ray, …`. There is no `request-id`. The endpoint
(`https://api.deepseek.com/anthropic`) sends `x-ds-trace-id` instead, and Claude Code does not
promote that to `requestId`.

That census looked at one direction only, so it was extended to the other, and **both results are
run**: the proxy stores no `request-id` in **either** direction — **0 of 728** responses carry one,
and a scan of the stored proxy **request** headers finds `request-id` in **0 of ~1,400** rows. Claude
Code sends none either, so the response header is the only direction that could supply the value.

`requestId` is populated from that upstream **response** header — an inference about Claude Code's own
behaviour, but one its data now pins down rather than merely suggests. Of the **2,807** transcript
lines whose `message.id` matches a stored proxy body id, **2,807 carry no `requestId`**: zero
exceptions. So `requestId` is present **precisely when the request did not traverse this proxy**, and
the mixed session is the rule at its sharpest:

- One session (`4842d0f2…`) runs both models from the one client against **two endpoints**: **1,380
  `claude-sonnet-5` lines, every one carrying a `requestId`**, against **113 `deepseek-flash` lines,
  none carrying one**. Same client, same machine, two endpoints — and `requestId` tracks the endpoint
  exactly. The `claude-sonnet-5` lines carry one **because they are proxy-absent traffic**, not because
  they are sonnet: an earlier pass of this plan read "through the one client and the one proxy" into
  this bullet, and the 2,807-of-2,807 census is what falsifies that reading.
- Every other observation is the same rule. No-`requestId` sessions exist well before the proxy's first
  stored row — `8e27b473…` was first seen 2026-09-06 and `6fbf02fd…` on 2026-09-15 — so the absence is
  not proxy-caused. The current session (`ef875ca8…`, proxied) has **0 of 4,361** usage lines carrying
  one; a session from before the proxy existed (`1fd1d222…`) has **8,220 of 8,220**; and across all
  transcripts **753 of 1,136 files carry none** — present where the proxy is absent, absent where it
  stands in front.

The inference is a mechanism claim, and "the id comes from the upstream response" predicts exactly
this: a field present wherever that endpoint's response header is, and absent wherever the proxy stands
in front of it. One mechanism could have contradicted the inference, and does not: `redactHeaders`
([internal/proxy/redact.go:29-48](../../internal/proxy/redact.go#L29-L48)) `Clone()`s the header
before redacting and never mutates the original, so the proxy is **not** stripping `Request-Id` from
what the client sees.

### 2.3 The identity is already on both sides

The upstream mints a message id. It appears in the proxy's stored response body as
`message_start.message.id` when streaming and as a top-level `id` when not:

```
event: message_start
data: {"type":"message_start","message":{"id":"f5ebc37a-f2eb-4d1d-b523-fbed4ce72f19", …
{"id":"ccc10259-13ff-4527-9eb8-b5e4ba00b0fb","type":"message","role":"assistant", …
```

**How the id is extracted, and the query it comes from, stated so the figure is reproducible.** The
proxy population is `source='proxy' AND resp_body IS NOT NULL` — 724 rows; the four with a NULL body
are excluded, and `request_id` does **not** filter it. The body is **decoded** first (`decode.Body`,
with the configured `BodyCapBytes` — the same decode `processCall` does at
[internal/consumer/consumer.go:302](../../internal/consumer/consumer.go#L302)); decoding is not
optional, because **145 of those 724 bodies are compressed**. The id is then read by D2's precedence:
a streaming body's `message_start.message.id`, or a non-streaming body's top-level `id` gated on the
body's own `type`. The transcript scan is `~/.claude/projects/**/*.jsonl`, assistant lines,
`message.id`, distinct per file and per id. The relation is set membership — *is this proxy id among the
transcript ids* — so the figure is `|distinct proxy body ids| ∩ |transcript message.id set|` over
`|distinct proxy body ids|`.

**The absolutes are perishable; the ratio is not, and the ratio is the evidence.** Two independent
passes measured the same relation on corpora of different sizes: this plan's own pass found **511 of
663 = 77%**, and the coordinator's re-measure on a corpus **78% larger** found **947 of 1,203 = 79%**.
The corpus nearly doubled and the ratio moved two points — so the load-bearing claim is the **stable
relation** (the large majority of proxy body ids appear verbatim in the transcripts, on **both** passes),
not the frozen 511, which is one pass's absolute and drifts with the database (§2's preamble).

One caveat on provenance: `parse.MessageID` does not exist yet, so neither pass could use the
production extractor — each applied the precedence above ad-hoc, the re-measure with a **stricter**
extractor. That is why the live-acceptance bead (§9 bead 07, br-GI-9-06) re-runs the relation **with**
`parse.MessageID` once it lands: the ratio is re-confirmable, and the production parser is what makes it
reproduced rather than merely re-confirmed.

**663 of the 724 proxy rows that have a body carry an id** (92%). The residue is counted rather than
narrated: **0 bodies carry more than one id** (which is what "exactly one" means) and **61 carry none**
(the coordinator's re-measure, on its larger corpus, counts **92** through the same precedence, §2.3's
table) — the pass classified those as error bodies (`{"type":"error",…,"request_id":null}`), redirects, and
non-JSON payloads, and the live-acceptance bead (br-GI-9-06) re-confirms that classification. A body
that later turns out to carry
two ids is still well-defined rather than ambiguous: D2 fixes **first-seen** as the winner, so the
precedence the extractor uses is stated, not incidental (§3 D2).

Claude Code writes the same value into the transcript as `message.id` — present on **100% of usage
lines** in both the proxied and the non-proxied session. Anthropic mints `msg_011CeqpGzFNPrYtxeB8huZja`;
DeepSeek mints a UUID (`c3c4de24-23c2-44ad-b685-aa98c9d9ee6e`). Same slot, different vendor format.

**The decisive measurement is not a sample at all.** Of the 663 distinct ids the proxy stores, **511
appear verbatim as some transcript's `message.id`** — 77% — and **no id appears in more than one
transcript file**. Existence on both sides *is* the identity claim; nothing has to be matched, and no
window, tolerance, or tie-break enters the argument. That is what makes this a key change rather than a
matcher.

**The misses are absentees, not mismatches — measured, not inferred from row metadata.** The drawer is
exactly where this story's own defect would hide: a response the client received, wrote a line for, and
whose id does not match the body id. The discriminator is defined so that the zero means something: for
each proxy id with **no** verbatim transcript match, take a transcript line in the same session whose
`[started_at, ended_at]` interval — widened by §2.5's **3.2s** median offset — overlaps the row's and
whose token columns match, and ask whether that candidate carries a **different** `message.id`. A
non-zero count would be this premise's counter-example. Run against the live DB:

| | |
|---|---|
| Proxy ids with no transcript counterpart | **303** |
| …of those, an interval+token match under a **different** id (a mismatch) | **0** |
| …of those, no candidate at all (true absentees) | **303** |
| Proxy rows yielding no id at all (error/truncated bodies) | **92** |

**0 mismatches across 303 ids: the drawer is empty.** The old adjectives ("same client,
`auth_kind=api_key`, status 200, mostly non-streaming") are retired rather than re-asserted — they
excluded nothing, and the comparison is what retires them. This plan's own pass counted **152** of its
smaller corpus; the drawer grew to 303 with the corpus, exactly the drift §2's preamble predicts. The
result is now a **measurement**, not a requirement handed to the live-acceptance bead.

**One caveat on units, because an earlier pass of this plan got it wrong.** 511 counts **distinct ids**,
and after D1's collapse one id is exactly one row — so 511 is a *row* count. Transcript **lines** are a
larger number: an earlier pass measured the same relation as 238 lines across 203 ids, and separately
compared 39 unambiguous same-request `(input_tokens, output_tokens)` pairs, all 39 agreeing. Those are
that pass's figures, kept as corroboration of the same relation rather than as the headline, and §5's
acceptance target is deliberately stated in **rows**.

### 2.4 The JSONL tailer inflates itself

`dedupeAssistantLines` ([internal/jsonlogs/dedup.go:118-131](../../internal/jsonlogs/dedup.go#L118-L131))
groups by `requestKey` and keeps the last line per key. With a real `requestId` that collapses the
per-content-block duplicate lines correctly — the measured defect test 8 pins. With the fallback key
it does not: the fallback embeds the line's own `uuid`, and each content-block line of one response
carries a **different** uuid, so the pair becomes two rows.

Two independent measurements **bound** the size — they do **not** agree on it (**≈32%** against
**≈71%**) — and the smaller one is a **floor**, not the other's answer:

- **Statistically — a floor — ≈32%.** Grouping by `(session_id, token quintuple, 5s bucket)` over the
  **88,032** stored JSONL rows: **28,405 surplus rows** — about **a third** — and **113,016,291 surplus
  input tokens**. At a 1-second bucket it is still 21,153 surplus rows (**≈24%**), so the inflation is
  not an artifact of a loose window.
- **Structurally — the larger measure — ≈71%.** One session's 4,361 usage lines carry only **1,251
  distinct `message.id`** — 3.5 lines per request, so **≈71%** of that session's lines are duplicates.

The two differ — **≈32% against ≈71%** — because they count different populations, and the difference
runs the way §5(3) says.
The token-quintuple grouping admits only lines with **no** real `requestId` — a line that has one dedups
correctly at ingest and is excluded from the inflation — while the distinct-id relation counts both,
the correct-keyed lines included. So the quintuple figure undercounts the true inflation and is a
floor; the distinct-id relation is the larger number. Neither is the rekey's own deleted-vs-inserted
report, which counts whole rows collapsing (§5(3)).

This half is JSONL-internal. No proxy-side change reaches it, and neither does any rule that counts
only proxy rows: inside the proxy's own active window there are 9,334 JSONL rows against 225 proxy
rows, so proxy-only totals would report about 2% of what happened.

### 2.5 The documented fallback cannot match anything

The fallback GI-1 actually documents is `(model, session_id, started_at ±1s, token quadruple)`
([GI-1-claude-lens-v1.md:322-323](GI-1-claude-lens-v1.md#L322-L323)). Measured on
the live DB it matches **0 of 728 proxy rows** — and still **0** with the `session_id` conjunct
removed. Two independent reasons, either sufficient on its own:

- **The ±1s window sits below the observed offset.** The proxy row's `[started_at, ended_at]` interval
  and the transcript line's timestamp disagree by a median of **3.2s** and a maximum of **52.9s**, so a
  ±1s window misses the line it is supposed to match.
- **The `session_id` conjunct is circular.** The two sources' session ids never overlap (**0 of 184**),
  which is the very defect this story fixes — so keying on `session_id` can never match. The documented
  fallback is self-defeating, not merely weak.

For contrast, the *generous variant* the plan measured — matching on `(input_tokens, output_tokens)`
alone within the proxy row's own interval, i.e. dropping `model`, the other three token columns and the
session conjunct, and widening the window to ±2s:

**660** stored proxy rows carry both an interval and an observed token pair, which is this table's
population; the other 68 admit no candidate at all.

| candidates found | proxy rows |
|---|---|
| 0 | 278 |
| exactly 1 | 199 |
| **more than 1** | **183** |

Those **199-unambiguous / 183-ambiguous figures are the generous counterfactual the plan measured, not
the documented key**. Even that variant leaves more than one candidate for 183 of the 660 rows, because
cache-read counts repeat heavily across a session's turns — and it is generous precisely by dropping
the conjuncts that make the documented fallback match nothing. **The documented fallback cannot match
anything (0 of 728); a heuristic backfill built on it would have to skip or guess on every row.**

### 2.6 A merge cannot move a row between sessions

This plan's first answer was that a merge changes which session a row belongs to, leaving the old
session's materialised totals stale-high, so the reconcile had to cover both sides. **That is wrong,
and the trace is short.**

`insertOrMerge` ([internal/store/merge.go:103](../../internal/store/merge.go#L103)) attempts the insert
**first**. On a `request_id` collision that insert fails, so **the incoming event never holds a row at
all** — there is nothing of it to remove from anywhere. `mergeEvents` then sets
`merged.SessionID = existing.SessionID` ([internal/store/merge.go:227](../../internal/store/merge.go#L227))
and `updateEventTx` updates **by `existing.ID`**, so the surviving row keeps its id *and* its session.
No row changes session on any path, so no session can be left holding totals for a row it no longer
has. Reconciling the survivor's session — which both callers already do — is the whole job, and it is
required precisely because a merge *does* rewrite the surviving row's token columns in place.

The asymmetry is still worth stating, with its cause, **as the proxy is currently written**: the
`sessions` table holds **184 rows, precisely the 184 distinct proxy `session_id`s**, while **0 of 207**
JSONL sessions have one — because `SetSessionRecorder`
([internal/jsonlogs/jsonlogs.go:143](../../internal/jsonlogs/jsonlogs.go#L143)) has **no caller anywhere
in the module**. So a merged row always lands in a **proxy** session, and that, not any reconcile
change, is what makes D4's ordering necessary: a JSONL session has no aggregate row to hold the row.

**The 0-of-184 overlap is a consequence of the proxy's code, not a property of the data.** The proxy
*sees* the true conversation id: `x-claude-code-session-id` arrives in **1,364** stored proxy **request**
headers, has **3** distinct values, and all **3 are real JSONL `sessionId`s** — yet **0** of them appear
in `events.session_id`, because the proxy discards the header and reconstructs a session id
heuristically instead. So "only proxy sessions have a materialised aggregate" and "the two session sets
never overlap" hold **as the proxy is currently written**, and wherever this document leans on them
(D3, D4's ordering argument, §8's first bullet) it means exactly that. GI-9's scope is unchanged — this
is not the story to store the header — but §8 records how small that follow-on story is. The
Sessions-view consequence of the asymmetry is real and is recorded in §8; it is not this section's
subject.

**Nothing follows from this for the store**, which is why §4 lists no merge-path change.

## 3. Design

### D1 — The message id is the request's identity, on both sides

`requestKey` gains a middle tier, and the proxy's row builder adopts the same value:

```go
// jsonl
func requestKey(l *line) string {
    if l.RequestID != "" { return l.RequestID }
    if l.Message != nil && l.Message.ID != "" { return l.Message.ID }
    return "jsonl:" + l.SessionID + ":" + l.UUID
}
```

The precedence is deliberate and mirrors the proxy's: **the upstream header when there is one, then
the upstream message id, then the synthetic fallback.** The header leads because it is the **only**
tier that can key a row whose **body carries no id** — an error body, a cap-truncated body, or a
response with no content type. That is **65 of 728 proxy rows** (4 with `resp_body IS NULL`
plus 61 whose body yields no id). Demoting the header would leave those
rows permanently unmergeable on an Anthropic-shaped install, where the header is the only identity the
upstream ever sends. Where the upstream sends both (Anthropic), the header must still win on both sides
or the Anthropic case would regress from working to broken. DeepSeek traffic sends only the body id, so
both sides fall to tier two and meet there.

**Where the two sides meet, stated as a condition rather than left to the reader.** The tier is a
property of the **upstream response**, not of the reader: both sources read the same response, so both
land on the same tier and meet **by construction**. They meet on **tier 1** iff the response carried
the header and the transcript recorded it byte-equal; on **tier 2** iff neither side saw a header. They
never meet across tiers. On **this** install tier 1 has never been exercised — **0 of 728** stored
responses carry the header (§2.2), and the proxy sends no request header either — so the tier that *is*
exercised here is tier 2, which is what §2.3 measures and §5's merge cases cover.

**The exceptions are named, not discovered later.** The tier is byte-equality, so if Claude Code's
transcript `requestId` is not byte-equal to the raw header value, an Anthropic-pointed install gets
**zero merges, silently**. A `doctor`/`serve` observability guard for that case was evaluated and **not
adopted**: this install has never served an Anthropic-shaped request, so the guard would watch a case
that cannot occur here. It is recorded as considered-and-deferred in §6. The second exception is
narrower, and it is **accepted, not fixed**: a proxy capture that **fails to record the header** — an
error response with no headers stored — leaves the proxy row on tier 2 while the transcript, which saw
the header, is on tier 1. That row does not merge. §5 asserts exactly this pairing (a tier-1 JSONL row
against a tier-2 proxy row) and asserts the two rows **stay split**, so the accepted exception is
recorded and made visible rather than silent.

**The 16,804 JSONL rows that already carry a real `requestId` (§2.1) are the population this condition
governs.** Under it they have no proxy counterpart **on this install**, because the proxy never saw the
header (§2.2) — the honest statement is *unexercised by the data the design rests on*, not *meeting*.
On an Anthropic-shaped install they meet on tier 1 whenever the header is byte-equal, and stay split on
a header-less proxy capture, which is the accepted exception above.

The JSONL side needs `ID string \`json:"id"\`` on the `message` struct
([internal/jsonlogs/dedup.go:25-29](../../internal/jsonlogs/dedup.go#L25-L29)); the value is already
in the line and is currently discarded.

This single rule fixes both defects. The content-block duplicates collapse because they share a
`message.id`; and the JSONL row's key becomes the exact value the proxy can produce, so
`insertOrMerge` finally takes its merge branch.

### D2 — The proxy parses identity in the cold path, in one place

`parse.Usage` gains `MessageID`, filled from the frames the extractor already visits
([internal/parse/usage.go:97-105](../../internal/parse/usage.go#L97-L105) for `message_start`,
[`:125-137`](../../internal/parse/usage.go#L125-L137) for the non-stream body). That is **four struct
fields and two assignments** — `Usage.MessageID`, an `ID` on `sseMessageStart.Message`, and an `ID` plus
a `Type` on `nonStreamBody` — with no new pass over the body and no new decode.

**When a body carries more than one id, the first wins.** `usageFromFrames` folds every frame, so
without a guard the **last** `message_start` visited would win silently: `MessageID` is therefore
assigned only when it is still empty (`if u.MessageID == ""`) in **both** the `message_start` and the
`message` cases. The `message` case is the non-stream body's synthetic frame, and it is also reachable
through the SSE path — `parseEvent` resolves a frame's type from the payload's own `type` when there is
no `event:` line ([internal/parse/sse.go:121-147](../../internal/parse/sse.go#L121-L147)) — so the guard
belongs on the assignment, not on the caller. The first `message_start` is the message the response
opened with; a later one is a re-emission — a gateway or retry artefact, or a concatenated stream — and
keying the row on its id would mis-key it. On the measured population this changes nothing (**0 bodies
carried two ids**, §2.3), which is exactly why it needs stating: it is the rule for the case the
measurement cannot exclude, and §5 pins it with a two-`message_start` fixture.

The non-stream body's top-level `id` is read **only** when the body's own `type` is `"message"`. The
guard buys **shape**, not per-request-ness: it stops a body that is not message-shaped from
contributing an id at all. What it does **not** prove is that a message-shaped body's top-level `id` is
per-request — a stable org or gateway id carries `type: "message"` too — so that property rests on the
base-rate evidence (§6) and on §5's integration assertion that one id yields one row, **not on this
gate**. The guard must read the **body's** `type`, not the frame's: `NonStreamFrame`
([internal/parse/types.go:24-26](../../internal/parse/types.go#L24-L26)) *synthesises* `Type: "message"`
for every `application/json` body, so a guard phrased against the frame type would admit everything —
which is why `nonStreamBody` needs the extra field rather than reusing the type the extractor already
routes on. Measured here it is safe either way — 105 of 105 non-stream bodies with a top-level `id` are
`type: "message"` — but the guard is cheap and the failure it prevents is silent.

Identity precedence then lives in **one function in the consumer**, where the parsed body is:

```go
func requestID(call *sink.CapturedCall, usage parse.Usage) string {
    if call.RequestIDHeader != "" { return call.RequestIDHeader } // the upstream header, when sent
    if usage.MessageID != "" { return usage.MessageID }           // the upstream message id
    return syntheticRequestID(call)                               // a key unique to this attempt
}
```

`captureState.requestID` is **deleted** from `internal/proxy`, and `st.submit` passes the header value
it saw (`respHeaders.Get("Request-Id")`, or `""`) instead of resolving a key. This is the plan's most
important structural choice, for three reasons:

1. **The answer depends on a parsed body**, and the hot path must not parse — `internal/proxy` is a
   tee by design ([decision 002](../context/decisions/002-hot-path-never-parses.md)). The consumer
   already decodes and extracts usage from that same body two lines earlier.
2. **The policy belongs in one place.** Splitting it — hot path synthesizes, cold path overrides when
   it can — means the synthetic namespace (`proxy:`) has to be *sniffed* to know which tier applies.
   One function reading top-to-bottom has no such question.
3. **It removes work from the hot path.** Today every header-less call hashes its whole request body
   merely to build a fallback key. After this, it hashes nothing.

The synthetic fallback itself keeps its exact shape, its `attempt` counter, and its rationale
([internal/proxy/proxy.go:259-267](../../internal/proxy/proxy.go#L259-L267)); it moves package, and its
counter moves with it.

*Rejected alternative:* leave `captureState.requestID` in place and have the consumer prefer
`usage.MessageID` whenever the stored key starts with `proxy:`. Smaller diff, but it leaves the
identity rule expressed in two packages and makes a string prefix load-bearing for correctness.

### D3 — Which session owns a merged row, and why `session_id` stays unrewritten

Once a JSONL row and a proxy row share a key, one row survives and it belongs to exactly one session.
The rule is already in `mergeEvents` and this story does **not** change it:
`merged.FirstSource = existing.FirstSource` and `merged.SessionID = existing.SessionID`
([internal/store/merge.go:226-227](../../internal/store/merge.go#L226-L227)) — the row keeps the
session it was **first written under**, and the later arrival never displaces it.

That is the right rule here, and the reason is §2.6's asymmetry rather than a preference. On the
ordinary ordering the proxy row is written live and the JSONL row arrives minutes later, so the
survivor is the **proxy** session — which is the only kind of session that has an aggregate row at all
**as the proxy is currently written** (it discards the true conversation id the request header carries,
§2.6), since `SetSessionRecorder` has no caller. A merged row therefore always lands somewhere its
totals can be re-derived, and no JSONL session is asked to own a row it could not account for.

**That guarantee is a property of the ordinary ordering, not of the merge.** It holds because the proxy
row is the one already in the table, and `mergeEvents` keeps `existing`. Nothing in `mergeEvents` checks
the *source* of the row it is merging into, so any path where a JSONL row gets a key first produces a
JSONL survivor — a row in a session with no aggregate, invisible to `clens sessions` and the dashboard's
Sessions tab. The live merge cannot do that, because the proxy row always gets there first. D4's rekey
collision can, and D4 carries the rule that prevents it. Stated here so that a later path which merges
without that rule is a decision rather than an accident.

Two consequences the design depends on, both stated so a later change is a decision rather than a
drift:

- **On the ordinary merge path the survivor's session is the only reconcile target, and only when the
  survivor's token columns actually moved.** `InsertEvent`/`InsertEvents` reconcile `insertOrMerge`'s
  returned session ([internal/store/store.go:260](../../internal/store/store.go#L260),
  [:288](../../internal/store/store.go#L288)) — the **survivor's**, not the incoming event's, which is
  correct precisely *because* a merge rewrites the surviving row's tokens in place while leaving its
  session alone. Do not "consolidate" the two session columns into the incoming row's. This is a
  statement about the **merge** path. D4's rekey collision is a different path — it deletes a row as
  well as merging one — and there the set of sessions to reconcile is larger; D4 states it.
- **No merge can vacate a session.** The incoming INSERT fails on the unique constraint *before* the
  incoming event ever holds a row, so there is no second row to remove and no session left holding
  totals for a row it no longer has. This was this plan's own first answer, and it was wrong; §2.6
  carries the trace. The one path that *does* delete a merged-away row is D4's re-key collision, and
  that path reconciles explicitly for exactly this reason.

### D4 — The backfill is two mechanisms behind one command

Forward-only would leave an estimated 28,405 surplus rows and every historical total double-counted, so
the story carries the backfill. The two halves re-key from genuinely different sources, which is why
one command has two mechanisms rather than a shared loop:

**Proxy half — re-key in place.** The value is already stored, inside `resp_body`, so these rows need
no re-read. Each row is processed in its **own transaction**:

1. for each `source='proxy'` row whose `request_id` is **synthetic** — the predicate is
   `request_id LIKE 'proxy:%'`, and it must be the **prefix**, not "the body yields an id different
   from the row's key": a row keyed by a *header* value (tier 1) is indistinguishable from a body-id
   row by inspection, and rewriting it to its body id would silently split it from the JSONL row that
   keys on the same header value — converting D1's working Anthropic case into the non-merging case D1
   exists to avoid — **and** whose body yields an id: decode the body and extract the id;
2. if the new key is free, re-key the row (`UPDATE events SET request_id = ? WHERE id = ?`);
3. if the new key is taken, resolve the collision by the **taker's source**, then delete whichever row
   did not survive and reconcile every session whose figures moved — through `reconcileSessionTx`, the
   tx-taking form (§4). See "the taker can be a JSONL row" below; it is not the edge case it looks like.

The body must be decoded first (`decode.Body`, as `processCall` does at
[internal/consumer/consumer.go:302](../../internal/consumer/consumer.go#L302)), and the scan must pass
the config **`BodyCapBytes`** as its `limit` — the same value the live path passes. The limit is
load-bearing twice over: a body that carries a `Content-Encoding` returns an **error** when
`limit <= 0` ([internal/decode/decode.go:115-117](../../internal/decode/decode.go#L115-L117)), and the
caller's `err == nil` guard then keeps the **undecoded** bytes — exactly the "silently re-keys nothing"
outcome below. **145 of the 724 proxy rows that have a body are compressed**, and a scan that skipped
decoding — or passed a non-positive limit — would silently re-key nothing for those rows while
reporting success for the rest.

Step 3 has three parts rather than the one it looks like it needs, and this is the story's one real
correctness trap. `mergeEvents` writes the **existing** row and leaves the source row untouched under
its old `proxy:` key, so "merge instead" on its own produces a duplicate — the survivor plus the row
that was supposed to disappear. Hence the delete. The delete is also what makes "a second run is a
no-op" true — the row is gone, so nothing is left for a second pass to find.

**The taker can be a JSONL row, and after D1 it usually is.** The lookup that finds the taker,
`getEventByRequestIDTx`, filters on `request_id` alone — no `source`
([internal/store/merge.go:82-84](../../internal/store/merge.go#L82-L84)). So step 3's "the two rows"
are not two proxy rows: they are the proxy row being re-keyed and **whatever holds the key**. Post-D1
the tailer keys each line by its `message.id`, and the key rule must land **before** the backfill runs
(this section's own ordering rule, below), so by the time anyone runs `clens rekey` the recent JSONL
rows already hold exactly the keys the proxy half is about to claim. That is the ordinary case, not a
corner: the JSONL row for a request is written minutes after the proxy row for the same request.

Merge into that taker and the survivor is a JSONL row, in a JSONL session with no `sessions` row —
the precise harm D4's ordering argument exists to prevent, arriving through the half that argument
assumed was safe. So the surviving row is **always the proxy row**, whichever source the taker is:

- **taker is a `proxy` row** — the taker survives as the merge target, per `mergeEvents`'
  existing-survives rule, and the re-keyed row is merged into it and deleted. Both sessions are proxy
  sessions, and both are reconciled.
- **taker is a `jsonl` row** — the taker is deleted, and the **proxy** row survives carrying the
  target key. Its content follows the winner pick plus per-column backfill (below), not a union; its
  `id`, `session_id` and `first_source` are the proxy row's.

**The requirement is stated and the mechanism deliberately is not, because the obvious mechanism is
wrong and silently so.** The natural reading of "the proxy row survives" is `mergeEvents(proxyRow,
jsonlTaker)` with the arguments swapped, and that **does not work**:

- `mergeEvents` **never assigns `RequestID`** — the function body has no such line; it starts from
  `merged := *existing` ([internal/store/merge.go:147](../../internal/store/merge.go#L147)) and copies
  fields one at a time from there. So the swapped call returns a row still keyed by the proxy row's
  **old synthetic key**, and `updateEventTx` binds `request_id` first
  ([internal/store/merge.go:14-15](../../internal/store/merge.go#L14-L15),
  [:69-79](../../internal/store/merge.go#L69-L79)) — so the write puts the old key straight back.
  Deleting the taker then leaves **no** row holding the message id. The run reports success, re-keys
  nothing on the ordinary case, and *succeeds on a second run* — the exact opposite of the no-op this
  section promises.
- `mergeEvents` also takes the key **and** the session from the same argument (`merged := *existing`).
  The order that yields the right key is the one that yields the wrong session, and vice versa. This
  operation cannot be expressed as a single `mergeEvents` call at all — that is the whole reason it
  needs a helper rather than a call.
- `mergeEvents` does **not** attach the `source_mismatch` warning either; `insertOrMerge` does
  ([internal/store/merge.go:117-131](../../internal/store/merge.go#L117-L131)). A helper calling
  `mergeEvents` directly merges a token-disagreeing collision **silently**, and §6 tells the operator
  to expect a handful of those warnings from the run. The helper owns the warning, or the expectation
  is false.

So the helper's contract is a **requirement list, not an implementation sketch**: the surviving row is
the proxy row's `id` and `session_id`; its `request_id` is the **target key**; its six token columns
come from the winner and **`total_prompt_tokens` is re-derived from them in the same write** — it is a
*derived* column the live paths recompute every time (`merge.go:214`, `store.go:261`, `:299`), so a
helper that copies the winner's six columns and leaves the survivor's stored total in place ships a row
whose `total_prompt_tokens` contradicts its own columns, the invariant CLAUDE.md calls "a tested
invariant, not a convention"; the **winner pick is the full three-rule sequence** `mergeEvents`
performs, including its **never-observed-usage override** (`merge.go:196-206`) — a side whose usage was
never observed never takes the pick from a side that has some, so an implementer who stops at the
two-case complete-frame switch diverges from live behaviour whenever the picked side is all-zero; the
taker's row is **deleted** (freeing the `UNIQUE` key); the `source_mismatch` decision is the helper's to
make on the same terms `insertOrMerge` makes it; and the touched sessions are reconciled.

**How those are sequenced inside the transaction, and which field is copied when, is the implementer's
to work out against the real `merge.go`** — it is the first thing **br-GI-9-04**'s cross-review (Phase
5.5) should be pointed at, and **this paragraph is deliberately not specified further here**. The reason
is in §7: four consecutive rounds of plan-level review each found a defect one level deeper in this one
paragraph, and the fifth was not a design error at all — it was a field-copy detail inside a function
this plan does not own. Keeping on specifying below the resolution a document has is how a confident
wrong mechanism gets written down; the plan settles *what* it can see and moves the *how* to the bead
and its code review, where the real `merge.go` is the arbiter.

**"Content" is not a union, and saying so would specify the inverse of the live behaviour.** The
tempting one-line summary of the content rule — "merge the two rows" — is wrong in a way an implementer
could satisfy exactly and still ship the opposite of what the live merge does. `mergeEvents` is a
**winner pick plus a per-column backfill**, not a union, and the two halves differ per column:

- **A winner takes the measurement columns wholesale.** The six token columns, `cost_usd` /
  `api_equivalent_cost_usd` / `cost_source`, `stop_reason` / `stop_category`, `service_tier`, `speed`,
  `model_resolved` and `billing_mode` are all copied from **one** side, chosen by
  `merge.go:166-206` — they are never summed, averaged, or taken column-by-column from whichever side
  happens to have a non-zero. So the helper's content rule is **"apply the same winner pick"**, and the
  winner pick must be the one the live path would have made had the arrival order been ordinary.
- **The rest is per-column backfill.** `preferNonEmpty` and the empty-cell checks
  (`merge.go:229-326`) fill the columns one side structurally cannot supply — proxy-only capture fields
  from the proxy side, transcript columns from the JSONL side — plus `source_refs` unioned and
  `is_sidechain` OR-ed. **`source_refs` is the only column that is actually unioned**, and it is why
  §5's JSONL-taker case can assert the union while nothing else unions at all.
- **`capture_complete` is its own rule**: the OR of the two (`merge.go:223`).

An implementer who unions the token columns satisfies every other item on the list, produces a row whose
totals no live merge could produce, and no CLI observable and no §5 aggregate bullet distinguishes it.
So the contract names the rule rather than a paraphrase of it.

**The winner pick is order-dependent, and for two complete captures it follows `incoming`.**
`case existing.CaptureComplete && incoming.CaptureComplete: winner = incoming`
([internal/store/merge.go:168-169](../../internal/store/merge.go#L168-L169)) — so with both sides
complete, the measurement columns above follow the **incoming** argument, not `existing`. That is what
makes the argument order in the helper load-bearing for the *figures* as well as for the key and the
session, and it is the third distinct thing the order decides. The direction chosen here is still the
right one — it reproduces the live seat — but the reason is the session and the identity, not
indifference about the content.

**The switch is not the whole rule.** After it, a winner that carries **no observed usage** loses the
pick to the other side (`merge.go:196-206`) — the `--body-policy off` correction, where a row's zero is
"never looked", not a measurement, so it never takes the pick from a row that has some. The complete
statement is therefore the three-rule sequence: the complete-frame switch, then the observed-usage
override. §5 pins the override with a fixture whose picked side carries all-zero usage while the other
side does not, asserting the figures follow the **observed** side.

**The proxy row winning is not a new rule, it is the live behaviour restored.** On the live path the
proxy row is `existing` because it was written first, so the JSONL arrival merges *into* it. The rekey
path is the same merge with the arrival order reversed, and the helper restores the same outcome rather
than inventing a different one. `insertOrMerge` hard-codes which argument is `existing` (it is the row
it failed to insert), which is why this is a `rekey`-only helper in `internal/store` — **the live merge
path is untouched** (§2.6, §4).

**The reconcile set is source-dependent, and it is a set rather than a pair.** It is every session whose
figures moved: the survivor's (the proxy session), and the session of whichever row was deleted. The two
are the same session in the proxy-taker case only when both colliding rows happen to share one. In the
JSONL-taker case the deleted row *is* the taker, so "the deleted row's session" already covers it —
there is no third session, and no `sessions` row exists for it to reconcile anyway (§2.6). Reconcile the
set rather than assuming its size.

**The reconcile covers two sessions, and the second one is easy to miss.** The deleted row's session
needs re-deriving because a proxy row **always** has a `sessions` row (§2.6), so removing one without
re-deriving leaves that session's totals permanently high. But the *survivor's* session needs it too,
and that half is not about the delete at all: `mergeEvents` copies the **winner's** six token columns
onto the surviving row
([internal/store/merge.go:208-214](../../internal/store/merge.go#L208-L214)), and the winner can be the
incoming row — when both sides are complete captures the pick is `winner = incoming`
([internal/store/merge.go:168-169](../../internal/store/merge.go#L168-L169)). So the survivor keeps its
own `session_id` while carrying the other row's tokens, and its session's aggregate moves. `InsertEvent`
already reconciles exactly that session — `insertOrMerge` returns `result.SessionID`, the survivor's
([internal/store/store.go:270-280](../../internal/store/store.go#L275-L280)) — which is the rule the
rekey path has to reproduce rather than approximate. In the **proxy-taker** case the set is
`{survivor.SessionID, deleted.SessionID}`; when the two are equal it is one call, and nothing here
depends on them being equal.

**Use `reconcileSessionTx`, not `ReconcileSession`.** `ReconcileSession`
([internal/store/store.go:672-682](../../internal/store/store.go#L672-L682)) opens its **own**
`BeginTx`, and the pool is pinned to a single connection (`db.SetMaxOpenConns(1)`,
[internal/store/store.go:72](../../internal/store/store.go#L72)). Calling it from inside the per-row
transaction below leaves the outer transaction holding the only connection and the inner `BeginTx`
waiting on the pool with no deadline — a **hang**, not an error, and one no test would report as a
failure rather than a timeout. `reconcileSessionTx`
([internal/store/store.go:684](../../internal/store/store.go#L684)) is unexported and takes a `*sql.Tx`,
which is why both existing callers use it and why the rekey methods must live in `internal/store` (§4).

**JSONL half — delete, then re-ingest.** A stored JSONL row cannot be re-keyed in place: its old key
holds a uuid and the message id it *should* hold was never stored. The only place that value exists is
the transcript. So the half is:

0. **check the precondition the delete rests on**, and refuse (or skip and report) if it fails: every
   source file backing a `jsonl:`-keyed row exists, and its current size is not below the byte cursor
   the tailer last recorded for it. Re-derivability is a *checked precondition of the run*, not an
   assumption — §6 states why a shrunk or deleted transcript makes a row non-re-creatable, and §5 seeds
   a missing/short file and asserts the refusal;
1. delete `source='jsonl' AND request_id LIKE 'jsonl:%'`;
2. zero the byte cursors and re-run the tailer — the mechanism `clens ingest --rebuild` already owns.

Delete-and-re-ingest is not only about the un-stored id. It must **also collapse the surplus §2.4
estimates**, and an in-place re-key would have to reimplement that collapse. The collapse, not the key
storage, is the reason this half re-ingests.

**Re-ingest re-prices, and the command's contract must say so.** The tailer prices each row as it
writes it, against the price table in force *now* — not the one in force when the row was first
ingested. So this half does not merely re-key: it restates roughly 70k rows' `cost_usd` /
`api_equivalent_cost_usd`, and `cost_source` moves with it wherever the table changed. It can also flip
`billing_mode`, because the tailer resolves that per row by model prefix and the merge's rule is
"the winner's mode" (§2.6). Two consequences the operator sees:

- A total that moves after the backfill is **not** evidence the backfill corrupted anything. It is the
  same rows priced by a newer table, and the `--dry-run` output should say the run re-prices rather
  than implying it only re-keys.
- The proxy half does **not** re-price. It re-keys rows whose cost was computed at capture time and
  leaves those columns alone, so after a run the two halves' rows are priced against different vintages
  of the table. That is already true of the table as a whole (any row older than the last price change
  is), so it is a thing to state, not to fix here.

Rows with a real `requestId` are untouched and re-insert idempotently, because re-reading them produces
the same key and merges.

*Rejected alternative — in-place join then re-key.* A stored JSONL row's own key is
`jsonl:<sessionId>:<uuid>`, so the transcript line it came from is look-up-able with **no heuristic at
all**, and an in-place join → re-key would avoid the destructive delete for the majority of rows. It
falls short **only** on the duplicate-collapse problem above: a re-keyed-but-not-collapsed set keeps the
surplus. Naming it is what makes "one command, two mechanisms" an argued decision rather than an
unexplained asymmetry.

**Ordering: the proxy half runs first, then the JSONL half.** The survivor of a merge is whichever row
is already in the table, and the survivor must be the row whose session has a **materialized
aggregate** — and **as the proxy is currently written** only proxy sessions have one (§2.6, §8). Running
the proxy half first means the JSONL
half's re-ingested rows collide with the already-re-keyed proxy rows, the survivor keeps the **proxy**
session, and `source_refs` unions as intended. Running the JSONL half first would make the JSONL row
the survivor, and the merged row would land in a session with no `sessions` row — so it would vanish
from `clens sessions` and the dashboard's Sessions tab.

**Ordering is not sufficient on its own, and the proxy half proves it.** The paragraph above reasons
about the JSONL *half*'s re-ingest, where the proxy rows are unreachable-because-already-there. The
proxy half has no such protection: it re-keys *onto* keys that post-D1 JSONL rows may already hold, so
its own collisions can produce a JSONL survivor unless step 3 branches on the taker's source. Ordering
gets the JSONL half right; step 3 gets the proxy half right. Both are needed, and neither is redundant
— which is why step 3's branch is stated as a rule rather than left to the ordering argument.

*Rejected alternative — have `rekey` upsert the survivor session.* With an upsert, a JSONL session
could be the survivor. It is rejected because it would change what the Sessions view contains, and that
is a separate story's decision (§8).

**One command, `clens rekey`**, following the contract
[cli-and-tooling.md](../context/cli-and-tooling.md) states for any destructive subcommand — and the
repo currently has exactly one:

- nothing happens without `--yes`;
- `--dry-run` prints what `--yes` would do — both halves' counts, **plus the re-pricing and
  re-derivability notes the operator needs** (below); **no row-level output**;
- the two halves run in the order stated above, and the command reports each separately, because a run
  that did one and failed the other must not read as "done".

**Per-row transactional merge, so a partial run is resumable.** The proxy half **overwrites
`request_id` in place**, so a crash mid-run would otherwise lose the old→new mapping with no record.
Each row's re-key (and any merge it triggers) is therefore committed in its **own transaction**: a
partial run leaves the rows already re-keyed correct and the remainder still synthetic, so re-running
picks up where it stopped. Nothing is atomic across the whole half, deliberately.

**`--dry-run` must report "would re-key N, would leave M synthetic (no body id)".** **4 rows with
`resp_body IS NULL`** plus **61 whose body yields no id** — 65 of 728, the same population D1 counts —
stay synthetic forever, and the operator needs to see that number before the run. A row whose body is
compressed but decodes nothing (a non-positive `limit`, or a `Content-Encoding` the scan did not undo)
belongs in **M**, not in **N** — the same "silently re-keys nothing" outcome as above, made visible
rather than reported as re-keyed. §5 asserts the exact pair, not just that both counts are non-zero.

**The JSONL half must report deleted-vs-inserted**, and that report is the run's own evidence for how
much duplication there was. It is **not** §2.4's 28,405: §2.4 estimates duplication by grouping on
`(session_id, token quintuple, 5s bucket)`, while this half deletes and re-inserts whole rows, so its
ratio counts one row per collapsing key and runs larger (§5(3)). `clens ingest --rebuild` provides no
such check, and the plan's own principle — a run that did one half must not read as done — applies
**inside** the half too.

Ordering against the forward fix is strict: **the key rule must land before the backfill runs**, or the
re-ingest recreates the very keys the delete just removed.

### D5 — What deliberately does not change

- **`--body-policy off` stays as it is.** Under `off` the headers are **still captured**
  ([internal/proxy/proxy.go:79-88](../../internal/proxy/proxy.go#L79-L88) passes `respHeaders`), so the
  header tier still keys the row — on an upstream that sends a `request-id` header, those rows **can**
  merge. Only an upstream that sends **no** `request-id` leaves them on a synthetic key that never
  merges — and that is this install (§2.2). So the permanent limitation is narrower than "body-policy
  `off` never merges": it is "body-policy `off` **on an upstream with no `request-id`** never merges",
  and the docs this story writes must not overstate it. What `off` genuinely gives up is the body id,
  not the header.
- **No read-time dedup view.** The plan already rejects that shape — it "leaves the inflation bug
  reachable by any query that forgets the view" — and nothing here reopens it. The rows are repaired,
  not filtered.
- **No schema change, and therefore no migration.** Every column this needs already exists
  (`request_id`, `resp_body`, and the `sessions` aggregates). `schemaVersion` stays `1`.
- **`internal/proxy` stays a tee.** It loses a function (D2) and gains nothing.
- **What each source captures** is untouched. The proxy still stores the compressed body it received;
  the tailer still stores the transcript content.

### D6 — The two documents that disagree about test 11(b) are reconciled

`docs/planning/GI-1-claude-lens-v1.md` is **internally consistent and does not contradict**
`docs/acceptance.md`. `:1105` and `:988` read "is an assumption **verified live** *before the merge is
relied on*" — a prospective precondition, not a claim of having verified. `:57` says "not yet evidence"
and `:325-333` says "**Status: still open — not captured.**" Both docs agree.

What this story measured does **not** record a negative result. **The equivalence was never exercised
on this install — the configured upstream sends no `request-id` header — and it is therefore moot,
because the design stops depending on it.** An endpoint that sends no `request-id` cannot falsify the
Anthropic `request-id` ↔ `requestId` equivalence, so "it does not hold" would be a false negative. That
wording — *never exercised, not falsified, and moot* — goes into both `docs/acceptance.md` and
`docs/planning/GI-1-claude-lens-v1.md`.

The genuinely wrong text in GI-1 is elsewhere. Its §Cross-source identity says the documented fallback
"remains the operative identity" and specifies it as `(model, session_id, started_at ±1s, token
quadruple)`. **No such code exists** — the implemented fallback is the namespaced
`jsonl:<sessionId>:<uuid>` / `proxy:<sha256>:<started_at_ns>:<attempt>` pair (§2.1). That, not the
equivalence claim, is the correction worth landing at `:1105`. The same correction goes to the comment
at [internal/jsonlogs/dedup.go:95-99](../../internal/jsonlogs/dedup.go#L95-L99), which records the
assumption as "not yet verified" when what is actually wrong is that it describes a fallback that was
never built.

## 4. Files changed

| File | Change |
|---|---|
| `internal/jsonlogs/dedup.go` | `message.id` on the `line`/`message` structs; the middle tier in `requestKey`; comment corrected |
| `internal/jsonlogs/dedup_test.go` | the new tier's cases (§5) |
| `internal/parse/types.go` | `Usage.MessageID` |
| `internal/parse/usage.go` | three struct declarations (`sseMessageStart.Message.ID`, `nonStreamBody.ID`, `nonStreamBody.Type`) and the two assignments — four declarations across three structs with the `Usage.MessageID` above; the non-stream tier gated on the body's own `type` |
| `internal/parse/usage_test.go` | streaming and non-streaming extraction |
| `internal/consumer/consumer.go` | `requestID(call, usage)`; `buildEvent` uses it; the synthetic fallback moves here |
| `internal/consumer/consumer_test.go` | the precedence table |
| `internal/sink/sink.go` | `CapturedCall.RequestID` renamed `RequestIDHeader` — its doc comment at `:57-61` calls the field "the cross-source dedup key", which D2 makes false, and the name would otherwise invite back the two-tier split D2 deletes (`internal/consumer/consumer.go:403` is the only reader) |
| `internal/proxy/proxy.go` | `captureState.requestID` and `fallbackSeqCounter` deleted; `submit` passes the header value |
| `internal/proxy/proxy_test.go` | three tests, three fates: `TestHashFallbackTwoAttemptsProduceDistinctIDs` (`:583`) **moves** to `internal/consumer` with the code; `TestResponseDerivedRequestIDWins` (`:551`) **changes meaning** (asserts the raw header value passes through, not that it is the dedup key); `TestPolicyOffSurvivesAMissingRequestID`'s synthetic-key assertion (`:456-458`) is **deleted** — it reads the renamed field, which is `""` on these nil-header paths, so the assertion goes red rather than moving |
| `internal/store/store.go` | the `rekey` store methods: the body-id scan (predicate `request_id LIKE 'proxy:%'`, decoding with the config `BodyCapBytes`), the in-place re-key, and the collision helper — which the surviving row wins, deletes the taker, applies `mergeEvents`' **winner pick and per-column backfill** to the content (not a union) including its never-observed-usage override (`merge.go:196-206`), re-derives the survivor's `total_prompt_tokens` from its own four prompt columns in the same write, owns the `source_mismatch` decision that `insertOrMerge` owns on the live path, and reconciles every session whose figures moved via `reconcileSessionTx` (not the exported `ReconcileSession`, which opens its own transaction and would hang against `SetMaxOpenConns(1)`) — all in one per-row transaction. **Do not implement it as a `mergeEvents` call with the arguments swapped**: D4 states why that re-keys nothing, and it is br-GI-9-04's named trap. `insertOrMerge` hard-codes which argument is `existing`, which is why this is a `rekey`-only helper. **The live merge path itself is unchanged** — §2.6 shows no row ever changes session, so `insertOrMerge` and its callers keep today's shape |
| `internal/cli/rekey.go` (new) | the command, `--yes` / `--dry-run`, both halves |
| `internal/cli/rekey_test.go` (new) | §5 |
| `cmd/clens/main.go` | register `rekey` |
| `internal/cli/cli_test.go` | add `rekey` to `TestNoCommandPrintsACredential` (~`:413`, currently **14 of the 18** dispatch names — it omits `ingest`, `refresh`, `reconcile` and `serve`) for symmetry: `rekey` prints counts, so its leak surface is nil, which is itself worth asserting. It is **not** added to `TestEveryCarriedOverCommandRunsAgainstATempStore` (~`:73`) — that table is the 10-entry *carried-over* set by design, and a new command is the wrong thing to add to a table named for carried-over commands. Neither table is derived from the `commands` map and **neither enumerates every subcommand**, so `rekey`'s registration is a deliberate edit in the credential table rather than a gap a complete list would have caught |
| `docs/context/cli-and-tooling.md` | the `rekey` row; "the one destructive command" becomes two; and `:6`'s "map … of 18 subcommands" becomes 19 |
| `docs/context/storage-schema.md` | its own "the only destructive command" claim at `:151` — same correction as `cli-and-tooling.md`, a different file |
| `docs/context/data-privacy-and-compliance.md` | `:106-107` and `:111` also assert purge is the only command that deletes rows; three files carry the claim, so all three move together. **And `:111-113` is independently wrong**: it says `--dry-run --yes` "reports and deletes in one pass", citing [internal/cli/purge.go:20](../../internal/cli/purge.go#L20) for it — which says the opposite ("`--dry-run --yes` is simply a dry run"), and so does the code (`:45` refuses only when *neither* flag is set, and `purgeByAge` returns before deleting when `dryRun`). The doc misstates both the behaviour and its own citation; fix it here since the file is already being edited |
| `docs/context/workflows.md` | the merge now fires; the identity rule |
| `docs/context/testing-and-quality.md` | `:12`'s "**51 test files, 15,051 lines**" becomes 52 — a present-tense statement of current state, unlike the dated re-measurement note it sits beside, so it moves with the new `rekey_test.go` |
| `docs/context/INDEX.md` | its "18-entry dispatch table in `cmd/clens/main.go`" becomes 19; **and `:41`'s "seven genuine forks" becomes eight** — `:114`'s "moved six → seven" is dated history and stays; and `:37`'s "trigger: 51 test files" becomes 52 (the new `internal/cli/rekey_test.go`) |
| `docs/context/decisions/000-index.md` | "**Seven** architectural forks" becomes eight, with the new 008 — **twice**, at `:5` and again at `:25` ("the status of all seven") |
| `docs/planning/GI-1-claude-lens-v1.md` | §Cross-source identity: the line-1105 fallback claim (the documented fallback was never built), and test 11(b) never exercised (not falsified) |
| `docs/acceptance.md` | test 11(b) — never exercised on this install, therefore moot |
| `docs/context/decisions/008-…` (new) | the identity decision (D1) |

No new dependency. No schema change. No change to `internal/web`.

## 5. Test strategy

**Unit — `internal/jsonlogs`.** The middle tier resolves when `requestId` is absent and `message.id`
is present; two lines sharing a `message.id` and differing in `uuid` collapse to one row (*the measured
defect*); the precedence still prefers `requestId` when both are present; and the existing distinct-uuid
case still yields two rows. That last case is `TestDedupeAssistantLinesFallbackKeyPerUUID`
([internal/jsonlogs/dedup_test.go:27](../../internal/jsonlogs/dedup_test.go#L27)), but it passes only
**incidentally** — it sets no `message.id`, so its comment ("A line with no requestId falls back to
`jsonl:<sessionId>:<uuid>`") becomes **false as a general statement**. Update the comment and the
fixture to "no `requestId` **and** no `message.id`". The new tier also needs a direct **negative** case:
two lines with **different** `message.id`s, identical usage, the same session → **two rows**.

**Unit — `internal/parse`.** `MessageID` is read from a `message_start` frame in an SSE fixture and
from a top-level `id` in a non-streaming fixture; absent in both when the upstream sent none. Plus the
guard: a non-streaming body carrying a top-level `id` but a `type` that is **not** `"message"` yields
**no** `MessageID` (D2) — the case the synthesised frame type would otherwise wave through; what that
case proves is the **shape** gate, not that a message-shaped `id` is per-request (that rests on §6's
base rate and the integration assertion). Plus the **precedence**: a fixture with **two**
`message_start` frames carrying **different** ids yields the **first** — the rule D2 states for the
case the measured population (0 bodies with two ids, §2.3) cannot exclude.

**Unit — `internal/consumer`.** The precedence table, all three tiers: a header value wins over a body
id; a body id wins over the synthetic key; no header and no body id yields a synthetic key of the
documented shape. Plus the property the `attempt` counter exists for — two byte-identical bodies in
one process never collapse.

**Unit — `internal/proxy`.** The fallback-key test moves to `internal/consumer` with the code (D2).
`TestPolicyOffSurvivesAMissingRequestID`
([internal/proxy/proxy_test.go:392](../../internal/proxy/proxy_test.go#L392)) *does* assert the key it
produces, not only the absence of a panic: `proxy_test.go:456-458` asserts
`strings.HasPrefix(call.RequestID, "proxy:")`. Those lines go **red** under D2, not "deleted with the
guard" — the field becomes `RequestIDHeader` and is `""` on exactly these nil-header paths — so the
assertion is **deleted** and the test's surviving half is its `off`-policy no-panic/no-lost-row
coverage. D2 also deletes the method the assertion exercised: `captureState.requestID` goes away, and
the nil-`*boundedBuffer` dereference goes with it — the consumer's fallback hashes `call.ReqBody`, a
`[]byte` for which `sha256.Sum256(nil)` is defined. So the guard
([internal/proxy/proxy.go:285-288](../../internal/proxy/proxy.go#L285-L288)) is **deleted, not moved**:
after D2 there is nothing to guard. State it that way in the bead. A port of this test to the consumer
would compile, pass, and cover nothing — the vacuous-assertion shape §7 warns about — so the
consumer's `off`-policy coverage belongs in the precedence table instead: a call with no header, no
body id **and** a nil body still yields a well-formed synthetic key.
`TestResponseDerivedRequestIDWins`
([internal/proxy/proxy_test.go:551](../../internal/proxy/proxy_test.go#L551)) keeps passing but
silently changes meaning: it now asserts the **raw header value** is passed through, not that it is the
dedup key.

**Unit — `internal/store`.** **No merge-path test, because there is no merge-path change** (§2.6). The
`rekey` store methods are exercised through the `internal/cli/rekey` tests below, which is where their
only caller lives. An earlier draft specified a "both sessions re-derived" assertion **for the merge
path**; it is removed because there it has nothing to catch — the incoming event's insert fails before
it holds a row, so **no row is ever written, moved, or rewritten for the incoming event** and the
session the merge does not keep is left exactly as it was. Re-deriving it is a no-op, so an assertion
on it passes whether or not any reconcile ran. (The one session whose figures *do* move is the
survivor's — §2.6, D3 — and `InsertEvent`/`InsertEvents` already reconcile it, so a test of that half
would only re-assert existing code.) That is the vacuous-assertion shape §7 warns about, and it is worse
than no test because it reads as coverage. The same-sounding assertion **is** required for the rekey collision (D4),
for the opposite reason: there a row genuinely is deleted and the survivor genuinely takes the other
row's tokens, so both aggregates move and the assertion can fail.

**Unit — `internal/cli/rekey`.** **Both fixtures seed the pre-fix state by hand**, because the
backfill's whole input population predates the fix and no forward path writes it: after D1 the tailer
keys a line by its `message.id`, and after D2 the consumer keys a row by its body id — so ingesting
today's fixture produces **zero** `jsonl:`-prefixed rows and no synthetic proxy row. Write the stored
rows directly with explicit `request_id` values (`jsonl:<sessionId>:<uuid>` for **both** duplicate
rows, so the collapse drops a row and the deleted count is 2→1 not 1→1; `proxy:<sha256>:<ns>:<attempt>`
for the proxy half). `--dry-run` deletes nothing, and asserts the **exact pair** it reports — N = the
rows whose bodies yield an id, M = the rows that do not (including a `resp_body IS NULL` row and an
error-body row) — so "would leave M synthetic" is **checked**, not merely promised, and neither count is
satisfied by a bare non-zero. `--yes` re-keys a synthetic proxy row to its body id; a
re-key that collides merges and leaves **one** row with `source_refs` unioned and no content lost —
and the assertion must be on the **row count**, not only on `source_refs`, because this collision is
proxy-vs-proxy: both sides already carry `source`, so a union shows nothing and the test would pass
with the source row still sitting there under its old key. **Pin the collision fixture so the assertion
can fail:** the two colliding rows sit in **two** sessions (each with its own `sessions` row), and the
re-keyed row wins the merge pick — both sides `capture_complete`, the incoming one carrying different
token columns — because otherwise one `reconcileSessionTx` satisfies both halves and the survivor's
reconcile is a no-op. Then assert each session's post-run aggregate (tokens / cost / `model_set`)
equals the value the post-merge row set implies, so **both** sessions were re-derived — the survivor's
as well as the deleted row's (D4) — and that a second run is a no-op; a run with neither flag refuses.

**The proxy half's remaining obligations each get a case, because each failure is silent:**
- a **gzip-encoded** stored `resp_body` carrying its `Content-Encoding` header is re-keyed to its body
  id — the fixture a hand-written plaintext body does not produce, and the one the "145 compressed"
  hazard needs. The **same fixture with a zero/invalid limit, or with the `Content-Encoding` header
  absent on an encoded body, stays synthetic and is counted in M** rather than reported as re-keyed;
- a row **keyed by a header value** whose body carries a **different** id is asserted **unchanged** by
  the run — the `proxy:`-prefix predicate, and the fixture that pins it against a
  "the body yields a different id" rewrite;
- the **surviving row's `total_prompt_tokens` equals the sum of its own four prompt columns**, asserted
  on the *row*; the session-aggregate assertions above read `total_prompt_tokens`, so a damaged row can
  satisfy them by having the expectation derived from the same damaged rows;
- a collision whose picked side carries **all-zero usage** while the other side does not asserts the
  figures follow the **observed** side — `mergeEvents`' never-observed-usage override, which the
  two-case switch does not express (D4);
- two proxy rows carrying the **same body id** and different `started_at` collapse to **one** row —
  asserted and recorded, so the collapse is a decision rather than an accident: it means a shared id
  loses a call, and this fixture is what makes that visible (D2, §6).

**The collision has two shapes and the second one is the load-bearing case.** The proxy-vs-proxy
fixture above pins the reconcile; it is also the shape the measured data says cannot happen (no body id
is shared by two proxy rows, §6). The shape that *will* happen is a **JSONL taker**: a proxy row whose
body id is already held by a `jsonl`-sourced row. Seed that — one `proxy:`-keyed row with a body
carrying a `message.id`, and one `jsonl`-sourced row carrying that same id — and assert:

- the surviving row is the **proxy** row: `id` unchanged, `session_id` still the proxy session, and
  `source_refs` now `["proxy","jsonl"]` (this one *does* show in the union — the sides genuinely
  differ, unlike the proxy-vs-proxy fixture);
- **`request_id` on the surviving row is the target key** — the body's `message.id`, not the proxy
  row's old `proxy:` value. This is the assertion that matters most and the one a naive implementation
  passes without: `mergeEvents` never assigns `RequestID`, so a helper built on a swapped call leaves
  the old key in place, deletes the taker, and hands back a row keyed by a synthetic string while every
  other bullet here still holds. Assert it directly, and assert it **before** the second run, because
  the bug's signature is a first run that does nothing and a second run that works;
- the JSONL taker is gone (row count drops by one);
- the deleted taker's session is untouched and no `sessions` row was created for it (§2.6);
- **pin the token columns so bullet 4's aggregate assertion can fail**: both sides `capture_complete`
  with **different** token columns, so the winner pick actually moves figures. Without the difference
  the reconcile is a no-op and the bullet passes either way — the same pinning the proxy-vs-proxy
  fixture needs, and omitted here in an earlier pass;
- the proxy session's aggregate reflects the post-merge row, not the pre-merge one;
- **a `source_mismatch` warning is attached when the two sides disagree on tokens** — the helper owns
  this decision (D4), so a helper that omits it merges silently, and §6 tells the operator to expect a
  handful of these warnings *from the run*. Nothing else in §5 asserts one, and the live-acceptance line
  is the only other place they appear, so without this bullet the obligation is untested;
- a **second** `rekey --yes` changes nothing (this is what a silent no-op first run would violate, so
  it is worth asserting after bullet 2 rather than trusting it).

Without the source branch in D4 step 3 this case fails on the first bullet: the survivor comes back a
JSONL row in a JSONL session, exactly as the ordering argument warns. A test that only ever seeds
proxy-vs-proxy cannot see that, which is why both shapes are required.

**The JSONL half is the destructive one and needs its own case.** Every test above is proxy-half, and
the half that deletes ~71k rows would otherwise ship on the strength of a printed count. Build it from
the shape the repo already has — `TestIngestRebuildRereadsWithoutDuplicating`
([internal/cli/additions_test.go:54](../../internal/cli/additions_test.go#L54)) writes a real transcript
under `withHome(t)`'s temp `~/.claude/projects/proj1/session1.jsonl` and runs `runIngest`. Seed the
pre-state by hand (above): two `jsonl:`-keyed rows for the **duplicate** pair (same `message.id`,
different `uuid`, no `requestId`) plus one row keyed by the real `requestId`, and leave the transcript
on disk carrying those same lines. Run `rekey --yes`, and assert: the `jsonl:`-prefixed
rows are gone, the duplicate collapsed to **one** row keyed by the `message.id`, the `requestId` row
survived, and the transcript file is still on disk — it is the re-ingest **source**, not how the
pre-state arises (the half is re-derivable, which §6 leans on). Then a `--dry-run` on the same fixture
deletes nothing. **And the precondition case:** seed a `jsonl:`-keyed row whose transcript file is
**missing** (or shorter than the cursor the fixture records for it) and assert the half **refuses** —
or skips and reports — rather than deleting the row. Without it the run deletes ~71k rows on the
strength of a precondition nothing checks (§6).

**Integration.** A JSONL line with no `requestId` and a proxy capture of the same request, ingested
through the real paths, produce **one** row — the end-to-end statement of the whole story, and the one
that fails today.

**And the accepted split, which nothing exercises today.** A seeded JSONL line that carries a
`requestId` (tier 1) paired with a proxy capture whose body yields the same `message.id` but whose
stored `resp_headers` hold **no** `request-id` (tier 2) must leave **two** rows: they key on different
values and do not meet. That is D1's named exception — the proxy capture failed to record the header —
asserted so it is visible rather than silent. The meeting case (both sides tier 2) is what the JSONL /
consumer fixtures above already cover.

**Live acceptance (manual, recorded in the plan).** Four checks against the live database after the
change. (1) **Premise check** — re-run the §2.3 comparison **by the method §2.3 states** (the
`source='proxy' AND resp_body IS NOT NULL` population, the `decode.Body` + config-cap decode, D2's id
precedence, the transcripts under `~/.claude/projects/` distinct per file and per id), now with the
production `parse.MessageID`: the verbatim-id relation must still hold at the same **ratio** (~77–79% of
proxy body ids; §2.3 states both passes' figures) — the relation is the invariant, the absolute is not.
This measures ids
the change does not touch, so it confirms the premise still holds; it does **not** validate the story. (2) **The check that validates the story** — count post-rekey rows
whose `source_refs` contains **both** `proxy` and `jsonl`: **0 today, expected 511**. The target is
stated in **rows**, and 511 is a row count because D1 collapses one id to one row; an earlier pass of
this plan stated the same relation as 238 and got the unit wrong — 238 was a *pair* count over
transcript **lines**, which is why it is not the target (§2.3). (3) The row count for the rekeyed
window drops, and the drop is read from the command's **own deleted-vs-inserted report** (D4) rather
than compared against §2.4's 28,405 — those are two measurements of two different groupings. §2.4
groups by `(session_id, token quintuple, 5s bucket)` to estimate how much duplication exists; the rekey
drops exactly the rows whose `requestKey` collapses, one per distinct id. The second is the larger
number, which is the point: a post-rekey JSONL count of at most `16,804 + 42,008 = 58,812` rows against
today's 88,032 puts the drop at **≥ 29,220**, already more than §2.4's estimate. So the acceptance line
is "the report shows a drop of tens of thousands of rows", and the run says which. Then the run reports
the expected handful of `source_mismatch` warnings (§6). (4) **The drawer is already measured** (§2.3): the discriminator returned **0** mismatches over the
**303** ids with no verbatim transcript match — every miss a true absentee — so the live run
**re-confirms** the zero rather than establishing it.

## 6. Risk areas

- **Deleting rows is the destructive act, and it is the point.** The JSONL half deletes; the proxy
  half only updates and merges. Mitigations: the selectors are narrow (`request_id LIKE 'jsonl:%'`);
  `--dry-run` is the default-safe path; **the delete's re-derivability precondition is checked, not
  assumed** — 88,032 rows and all 207 DB JSONL sessions have their transcript files on disk, but that
  is a snapshot and the code treats the file-changed case as ordinary: a transcript that has **shrunk**
  (truncation or rotation) is re-read from offset 0
  ([internal/jsonlogs/jsonlogs.go:300-306](../../internal/jsonlogs/jsonlogs.go#L300-L306)), so the lost
  prefix's lines are gone and the rows derived from them are **not** re-creatable, and a **deleted**
  transcript is the same failure worse. So before the delete the JSONL half verifies every
  `jsonl:`-keyed row's file exists and its current size is not below the recorded cursor, and refuses
  (or reports and skips) otherwise — a **checked precondition of the run**; §5 seeds a missing/short
  file and asserts the refusal. And the command should be run with `clens serve` stopped, because
  a live tailer writing while cursors are zeroed is a race the command does not need to have. The
  selector's prefix `LIKE` **cannot use the UNIQUE index** — SQLite will not use it for a prefix `LIKE`
  under the default `BINARY` collation — so the delete scans all ~87k rows. That is fine for a one-off
  and should not be "optimized"; flagging it so the implementer does not invent a rewrite of the
  predicate.
- **Re-keying the proxy half is not re-derivable.** The stored `resp_body` is the only source, so a
  botched re-key cannot be replayed from anywhere else. The merge takes the winner's measurement
  columns and backfills the remainder, so it is lossless for the *content* (`mergeEvents`; only
  `source_refs` is unioned), but this is the half where "the heuristic has to be right the first time"
  actually applies — and it is why the design has no heuristic in it.
- **The collision helper can be written in a way that fails silently, and both plausible versions are
  that way.** The first is calling `mergeEvents` with its arguments swapped, which re-keys nothing:
  `mergeEvents` never assigns `RequestID`, so the old synthetic key is written straight back and the
  taker's deletion leaves the message id held by nobody. The run reports success; a *second* run then
  works, because the key is free by then — so the failure looks like the promised no-op and its own
  correction looks like the first run. The second is reading "merge the two rows" as a content **union**:
  `mergeEvents` picks a **winner** for the measurement columns (`merge.go:166-206`) and backfills the
  rest per column, unioning only `source_refs`, so a union produces a row no live merge could produce —
  and unlike the first, this one changes figures rather than keys, while still leaving every key and
  session assertion in §5 green. Mitigations: D4's requirement list and named trap, restated as a
  directive in br-GI-9-04, plus §5's assertions on the surviving row's `request_id` and on the
  `source_mismatch` warning. Named here because these are the defects in this story that no CLI
  observable would reveal.
- **A `message.id` collision would silently collapse two requests.** The real vector is not two vendors'
  UUID formats colliding; it is a *stable*, non-per-request id — an org id or a gateway id sitting at a
  top-level `id` — becoming a row key and collapsing unrelated rows. The guard (D2) reads the non-stream
  `id` only when the **body's** `type` is `"message"` — not the frame type, which `NonStreamFrame`
  synthesises — and it buys the **shape**: a body that is not message-shaped cannot contribute an id at
  all. It does **not** prove per-request-ness — a stable id carries `type: "message"` too, so it passes
  that gate unchanged — and the §5 negative case (a body whose type is not `message`) exercises the
  shape gate, not this property. The per-request property therefore rests on the base rate: across
  **42,008 distinct `message.id`s** in **123,880** usage lines, **none** spans two sessions, **none**
  spans two files, **none** coincides with a `requestId`, and **no** proxy body id is shared by two
  proxy rows. §2.3's measured drawer corroborates the last clause directly: over the **303** proxy ids
  with no verbatim transcript match, **0** had a same-interval, token-matching counterpart under a
  different id — so no id's absence is the signature of a collision, and the base rate holds as a
  measurement, not only as an observation about the ids that *do* match. The integration test still
  asserts one row per distinct id rather than trusting the format.
- **The merge moves tokens on rows the proxy only partially captured.** Measured over the whole id
  relation, not a sample. In an earlier pass this was stated as 238 (id, transcript-line) pairs across
  203 ids, of which **90** had a complete capture on both sides — **87 agreeing exactly on all six token
  columns** and **3 disagreeing** with the signature `input_tokens +128, cache_read_tokens −128`, the
  stored `message_start` usage snapshot diverging from the transcript's final usage; those 3 raise
  `source_mismatch` at `SeverityError` on merge. The remainder merged only because the JSONL side was
  the **sole complete capture**: streamed proxy rows with `output_tokens == 0` were `capture_complete =
  0` and their stored body contained **no `message_delta` frame** (truncated at the 256 KB cap), so
  `mergeEvents`'s winner pick hands those rows to the JSONL side and re-keyed historical rows **gain
  tokens**. Expected, not a defect — but the operator should expect a handful of `source_mismatch`
  warnings so they do not read as failure. The per-column split is that pass's figure; the shape it
  explains is structural and survives the counts moving.
- **The header tier is unexercised here, and its failure mode is silent where it is exercised.** On an
  Anthropic-pointed install, if Claude Code's transcript `requestId` is not byte-equal to the raw
  `request-id` header value, every merge key misses and the install gets **zero merges, silently** — the
  rows simply never merge, with no error raised. An observability guard for this was evaluated and
  **deferred, not adopted**: this install has never served an Anthropic-shaped request (0 of 728 stored
  responses carry the header), so a guard would watch a case that cannot occur here. Recorded, not
  mitigated. The second, narrower failure is the one D1 **accepts**: a proxy capture that fails to
  record the header — an error response with no headers stored — leaves the proxy row on tier 2 while
  the transcript, which saw the header, is on tier 1, so that row never merges. It is **recorded and
  made visible** by §5's tier-pairing fixture rather than fixed here.
- **A retried call keeping two rows is not a change; `message.id` stability across a retry is
  unmeasured.** `TestRetryPreservesTwoRows`
  ([internal/store/merge_test.go:140](../../internal/store/merge_test.go#L140)) inserts two hand-keyed
  rows (`req-retry-1`, `req-retry-2`) and never involves a `message.id` — it passes unchanged if both
  attempts carry the **same** id, which is precisely the risk here. So "each attempt carries its own
  message id" is a hypothesis, not a pinned fact, and the one place this key change can lose a row
  silently is a **shared** id: a cached or replayed response (`ReplayOf`,
  [internal/proxy/proxy.go:157-175](../../internal/proxy/proxy.go#L157-L175)) returning the *same*
  message id for two distinct calls would collapse them into one row and drop a call. Nothing in the
  measured data shows this (no id is shared by two proxy rows, none spans two sessions — the base rate
  above), but §5 now asserts the current intended behaviour (two rows with the same body id collapse to
  one) instead of resting on the base rate alone, and the live-acceptance run measures it.
- **The Sessions view changes shape after the backfill.** On the **merge** path a row gains a second
  source and its **figures** — token totals, cost, `model_set`, `priced_count`, `warning_count` — move
  inside the **proxy** session it already belonged to; no row changes session and no count moves (§2.6).
  The merged rows are the 511 the proxy already stored under a synthetic key (§1) and whose body yields
  an id — visible in their proxy session today, not rows that existed only as JSONL. Keep the one
  exception distinct: D4's rekey **collision** is a different path that **deletes** a row, so it is the
  one operation here that does move a session's count. Expected, not a defect, but
  user-visible, so it belongs in the PR body and the refresh.
- **`off` rows on an upstream that sends no `request-id` never merge** (D5) — which is this install. A
  user running `off` here gets the JSONL half's fix only.
- **Retention may make part of the backfill moot.** If `retention_days` is configured, the oldest
  duplicates would age out anyway; the backfill's value is bounded by that window.
- **Most proxy rows *do* have a JSONL counterpart, and the two groups want different keys.** Of the ids
  the proxy stores in a body, the large majority have a transcript counterpart (77% on this plan's pass,
  **79%** on the coordinator's larger re-measure, §2.3) and are the design's target; the rest are
  **measured to be true absentees, not mismatches** — §2.3's discriminator returned **0** mismatches over
  the **303** ids with no verbatim match, so the "either an auxiliary call or a row whose counterpart
  exists under a *different* `message.id`" fork is **settled in favour of the first arm**: no row in the
  drawer is this story's own defect. The evidence is §2.3's measured result and the **stability of the
  ratio** across the two passes, rather than any frozen count (the old "the 39-pair sample is small"
  hedge understated the finding). §5's live acceptance **re-confirms** the drawer's zero and re-runs the
  merge count after the change.

## 7. Self-review

**As a senior engineer.** The design's virtue is that it adds no matching machinery: the identity was
already being captured and thrown away on both sides. The structural change (D2) is a *deletion* from
the hot path, and it makes the identity rule readable in one function instead of inferable from two
namespaces. The riskiest part is not the key change but the **backfill** — and specifically its proxy
half, which is the only non-re-derivable step in the story (§6). Three things this plan got wrong on the
way here are recorded rather than quietly dropped, because each is the natural first guess: that a
merge changes a row's session (§2.6 — it cannot, and the trace is three lines); that the documented
fallback had to be *measured* before it could be rejected (§2.5 — it matches zero rows, so measuring its
variants was beside the point); and that a re-key collision is "just a merge" (D4 — the merge writes the
survivor and leaves the source row behind, so the collision path has to delete *and* re-derive both
sessions it touched, and it must use the tx-taking reconcile rather than the one that opens its own).
A fourth is subtler and lives in the same place: the collision's taker is **whatever holds the key**,
and after D1 that is usually a JSONL row — so the ordering argument that makes the JSONL half safe
does not make the proxy half safe. A fifth is subtler still, and it is the one that changed how this
plan treats the paragraph: the fix for the fourth was written as a call the plan could describe
("merge with the arguments swapped"), and that call **re-keys nothing** — `mergeEvents` never assigns
`RequestID`, so the old key goes straight back and the taker's deletion leaves the message id held by
nobody, silently, on the ordinary case. Four consecutive rounds found a defect one level deeper in this
one paragraph, and the fifth is not a design error at all — it is a field-copy detail inside a function
this plan does not own. That is what moved the paragraph from a procedure to a requirement list with a
named trap (§9), and it is the clearest evidence in this document for the rule that a plan should
settle *what* and leave *how* to the code and its review.

**As a QA engineer.** The cases that matter are the ones where the rule must *not* fire: two distinct
requests that share a session and token counts **with different `message.id`s** — which the existing
test 8 fixture does *not* exercise, since it shares a `requestId` rather than two distinct requests with
equal tokens — a line with a `requestId` that must still win, a non-stream body whose `type` is not
`message`, a body that decodes to an error object with no id, and a re-run of the backfill. The removed
store test is the lesson worth carrying forward: an assertion whose two sides both start at zero passes
whether or not the code is right, and reads as coverage while proving nothing.

**As a security engineer.** No new input reaches a trust boundary: the message id comes from upstream
response bytes already stored, and the only new write path is a CLI command operating on the local
database. The rekey command *does* delete, which is why it takes purge's contract rather than a new
one. No credential is involved, no header is newly captured (the request-id header is already read),
and full bodies are neither newly exposed nor newly retained. The one thing to hold: the command must
refuse without `--yes`, and `--dry-run` must not delete as a side effect of measuring.

## 8. Out of scope

- **Changing which session owns a merged row.** D3 pins today's rule — the row keeps the session it was
  first written under, which with the proxy half running first (D4) is a **proxy** session — and Claude
  Code's own conversation id, the ground truth, does not get it. Totals are correct either way, because
  global aggregates do not care which session owns a row, but **as the proxy is currently written** the
  Sessions view groups by the proxy's heuristic reconstruction instead of by the conversation. Letting
  the JSONL session own the merged row means relaxing the never-rewrite-`session_id` invariant, which is
  its own story — and a **much smaller story than it reads as**: the conversation id is not missing, it
  is *discarded*. `x-claude-code-session-id` arrives in **1,364** proxy request headers with **3**
  distinct values, all **3** real JSONL `sessionId`s, and **0** of them reach `events.session_id`
  (§2.6). Storing the header as the session key is very nearly the whole of it. D3 states the rule so
  that this is a decision rather than a drift; it does not do it here.
- **Recovering an identity for `--body-policy off` rows.** The proxy did not look, so there is nothing
  to recover.
- **Any read-time dedup view** (D5).
- **Changing what either source captures**, including promoting `x-ds-trace-id`.
- **`clens sessions` and the dashboard's Sessions tab are proxy-only today** — discovered while
  measuring §2.6. Because JSONL sessions have no `sessions` row, a JSONL-only install shows nothing
  there. Pre-existing and not owned by this story; recorded so it is not lost.
- **The dashboard's filters.** No new column means no new filter; `source`/`billing_mode` filters on
  the Stats tab remain the separate, already-offered item.

## 9. Bead sketch (Phase 3 formalises this)

| # | Bead | Depends on |
|---|---|---|
| 01 | `parse`: `Usage.MessageID` from both response shapes, with the non-stream `type` guard | — |
| 02 | `consumer`: identity precedence in one function; the synthetic fallback moves here | 01 |
| 03 | `proxy`: `captureState.requestID` deleted; `submit` passes the header value | 02 |
| 04 | `jsonlogs`: `message.id` as the middle tier of `requestKey` | — |
| 05 | `cli` + `store`: `clens rekey` — both halves, `--yes` / `--dry-run`, and the proxy half's collision helper, specified as a requirement list plus a named trap (§9's note, D4) | 02, 04 |
| 06 | docs: reconcile test 11(b), the merge flow, the CLI table, the three other "only destructive command" claims, the purge `--dry-run --yes` misstatement, and add decision 008 | 01–05 |
| 07 | live acceptance re-run and the recorded manual run (the §2/§6 decoded-body figures are marked **re-confirmable**, not independently reproduced) | 05 |

**The beads exist now** (`.beads/GI-9/`), and this sketch's numbers are not theirs: **br-GI-9-01**
(parse), **br-GI-9-02** (consumer + proxy — this sketch's 02 and 03, folded into one atomic change),
**br-GI-9-03** (jsonlogs, this sketch's 04), **br-GI-9-04** (`clens rekey` — this sketch's bead 05),
**br-GI-9-05** (docs) and **br-GI-9-06** (live acceptance). D4, §4 and §6 therefore refer to the
destructive bead by its real id, **br-GI-9-04**.

**br-GI-9-04 is the largest and the only destructive one.** The forward-fix beads (br-GI-9-01…03) must
land before it runs anywhere but a dry run. There is no store bead for the **merge path**: §2.6 and D3
show it needs no change. The store methods br-GI-9-04 adds are the rekey scan, the re-key, and the
collision path's helper.

**br-GI-9-04 carries a requirement and a named risk rather than a procedure, deliberately.** The
collision helper's *what* is settled above (proxy row survives; `request_id` is the target key; content
follows the winner pick and per-column backfill **with the survivor's `total_prompt_tokens` re-derived
from its own four prompt columns**; taker deleted; `source_mismatch` decided as `insertOrMerge` decides
it; touched sessions reconciled — the full list is D4's) and the *how* is not, because five rounds of
plan-level review each found a defect one level deeper in this one paragraph — which sessions, then who
the taker is, then that `mergeEvents` never assigns `RequestID` at all. That last one is not a design
error; it is a field-copy detail in a function the plan does not own, and it is below the resolution a
document has. So the bead must state the requirement list **and** the trap explicitly:

> **Do not build this by calling `mergeEvents` with the arguments swapped.** It compiles, it reads
> correctly, and it silently does nothing on the ordinary case: `mergeEvents` never assigns
> `RequestID` (`merged := *existing`, `merge.go:147`), so the swapped call returns the proxy row's old
> synthetic key, `updateEventTx` writes that key back (it binds `request_id` first), and deleting the
> taker then leaves no row holding the message id. It also takes the key and the session from the same
> argument, so no single call can give the right key with the right session. **In the JSONL-taker case**
> (the ordinary one) delete the taker first — it holds the `UNIQUE` key — then write the surviving row
> with `request_id` set to the target; **in the proxy-taker case** the taker is the row that survives
> and the re-keyed row is the one deleted, so the order is the reverse. Assert the survivor's
> `request_id` in the test; that assertion is the one that catches this.
>
> **And do not union the content.** `mergeEvents` is a winner pick plus a per-column backfill: the six
> token columns, the cost columns, `stop_reason`, `stop_category`, `service_tier`, `speed`,
> `model_resolved` and `billing_mode` all come from **one** side (`merge.go:166-206`), `capture_complete`
> is the OR (`:223`), the structurally-impossible columns are backfilled (`:229-326`), and
> **`source_refs` is the only column that is actually unioned**. Summing or column-wise-merging the
> token columns satisfies every other requirement and produces a row no live merge could produce, with
> nothing in the CLI or in §5's aggregate bullets able to tell. Apply the same winner pick the live path
> would have made.

The implementer's own cross-review (Phase 5.5) is where the sequencing gets validated, against the real
`merge.go` rather than against a paragraph about it — and br-GI-9-04's diff is what it should be pointed
at first.

## Change History

| Date | Change |
|---|---|
| 2026-09-20 | Initial plan from Phase 1 intake. Identity verified live (39/39) before any design was written; the fallback matcher measured and rejected (114 of 257 ambiguous). |
| 2026-09-20 | Round 1 revision (H1–H3, M1–M5, L1–L8). §2.2's causal claim replaced with the mixed-session evidence and the mechanism labelled an inference; D6 rewritten (GI-1 is internally consistent; test 11(b) never exercised, not falsified); §2.5 rewritten around the documented fallback (0 of 415); M1's honest store-reconcile justification and D4's proxy-half-first ordering; D1's header-tier reason; §6 merge-token deltas and `source_mismatch`; `internal/sink/sink.go` added to §4; §2 counts moved to the decoded-body re-measure; L1–L8 applied. |
| 2026-09-20 | Round 2 revision (F2.1–F2.8). **F2.1** — the merge-reconcile design section deleted outright (a merge changes no row's session, so the reconcile is already complete); §2.6 reduced to the non-defect it always was, and §4 lists no merge-path file; §8's first bullet and §9's bead list lose its references. **F2.2** — §5's `internal/store` test removed as unfailable by construction. **F2.3** — §2 pinned to **one snapshot**; the two-pass table dropped, one timestamp stated once; §2.5's `0 of 415` restated as **0 of 338** and the generous-variant table labelled `257 of the 338`. **F2.4** — 203 distinct ids vs 238 `(id, transcript-line)` pairs stated in §2.3; §6's `90 + 148 = 238` and `~30%` residual pinned to that denominator; §5's acceptance target set to **203 rows** with the 238-vs-203 explanation. **F2.5** — proxy session ids unified to **146** (§1/§2.1/§2.5), the stored-JSONL total to **87,340** (§2.1/§6), and the bodiless-id count to **49** (D1/D4/§2.3). **F2.6** — D2's guard corrected to decode the **body's** own `type` (`NonStreamFrame` synthesises the frame type, so the frame-type guard was a no-op); §4's field count corrected and §5 gains the type-gate negative case. **F2.7** — §6's retry claim replaced with the shared-id risk, referencing `TestRetryPreservesTwoRows`. **F2.8** — §6's `--body-policy` bullet retitled to D5's narrowed wording; both hand-maintained `cli_test.go` tables added to §4. **Renumbering:** §9's old bead 05 (the store reconcile) is deleted and the beads that followed shift down by one (old 06–08 → new 05–07), so round-1 artifacts' "bead 08" reads as **bead 07**. |
| 2026-09-20 | Round 3 revision (F3.1–F3.7, plus the count rewrite). **F3.1 (MAJOR)** — D4's proxy-half collision path was "merge instead; nothing is lost", which leaves the source row behind under its old `proxy:` key; it is now merge → **delete** → `ReconcileSession`, in the row's own transaction, with §5's rekey test asserting the **row count** (a proxy-vs-proxy collision shows no `source_refs` union, so the union assertion alone passes with the duplicate still there). **D3 re-added** in its narrowed form — which session owns a merged row, and why `session_id` stays unrewritten — as F3.1's reconcile rationale needs a home, and §3 no longer skips from D2 to D4. **F3.2/F3.4** — §6's `338 − 238` and §1's bare `257` replaced with same-pass figures. **F3.3** — `storage-schema.md:151` and `data-privacy-and-compliance.md:106-107,111` added to §4; three docs carry the "purge is the only destructive command" claim, not one. **F3.5** — §5's `TestPolicyOffSurvivesAMissingRequestID` corrected: it pins the *absence* of a panic, and after D2 the guard is deleted rather than moved (`sha256.Sum256(nil)` is defined), so a ported test would pass while covering nothing. **F3.7** — D4 now states that the JSONL half **re-prices**: ~70k rows restated against the current price table, `cost_source` moving with it and `billing_mode` able to flip, while the proxy half does not. **Count rewrite** — every §2 figure re-measured in one read-only pass (728 proxy rows, 663 body ids, **511** verbatim in transcripts, 0 of 728 headers, 184 sessions, 88,032 JSONL rows, 42,008 distinct `message.id`s in 123,880 usage lines), the earlier pass's numbers kept only where labelled as such, §5's acceptance target moved from 203 to **511 rows**, and §2's preamble now names the absolute counts perishable and the structural facts not. |
| 2026-09-20 | Round 4 revision (F4.1–F4.7). **F4.1 (MAJOR)** — D4 step 3 reconciled only the **deleted** row's session, but a merge also rewrites the **survivor's** token columns (the winner can be the incoming row when both captures are complete), so the survivor's aggregate moves too. It now reconciles the **set** `{survivor.SessionID, deleted.SessionID}`, and D3's "the survivor's session is the only reconcile target" is scoped to the merge path, which is what it always described. **F4.2 (MAJOR)** — D4 and §4 named the exported `ReconcileSession`, which opens its own `BeginTx`; against `SetMaxOpenConns(1)` that blocks the outer per-row transaction on the pool with no deadline — a hang, not an error. Both now name `reconcileSessionTx`, which is what `InsertEvent`/`InsertEvents` already use. **F4.3** — §5(3) compared the rekey's row drop against §2.4's 28,405, two measurements of two different groupings; it now reads the drop from the command's own deleted-vs-inserted report, with `88,032 − (16,804 + 42,008) ≥ 29,220` as the bound, and the 28,405 uses in §1/D4 are labelled estimates. **F4.4** — D4's stale `49` (8 + 41) corrected to the same-pass `65` (4 + 61) that D1 and §2.3 carry. **F4.5** — §6's heading inverted its own body; 511 of 663 *do* have a counterpart. **F4.6** — §4 missed `INDEX.md:41` and `000-index.md:25`, both of which carry the "seven forks" claim, while `INDEX.md:114` is dated history and stays. **F4.7** — §5 exercised only the proxy half; the destructive JSONL half now has its own case, reusing the transcript fixture `TestIngestRebuildRereadsWithoutDuplicating` already establishes at `internal/cli/additions_test.go:54` (the reviewer believed no fixture existed), and the state-vs-invariant distinction between the merge path's removed assertion and the rekey path's required one is spelled out. |
| 2026-09-20 | Round 5 revision (F5.1–F5.6). **F5.1** — §5 now says **both rekey fixtures seed the pre-fix state by hand** (no forward path writes `jsonl:`-keyed or synthetic-`proxy:` rows after D1/D2), with the transcript plus `runIngest` kept as the re-ingest **step** rather than how the pre-state arises. **F5.2** — §5's collision fixture pins the two colliding rows in **two** sessions and the re-keyed row as the merge **winner** (both `capture_complete`, differing token columns), asserting each session's post-run aggregate — otherwise one `reconcileSessionTx` satisfies both halves and the survivor's reconcile is a no-op. **F5.3** — §5's `TestPolicyOffSurvivesAMissingRequestID` framing corrected (counter to round 4): the test **does** assert the synthetic key at `proxy_test.go:456-458`, which goes red under D2's rename and is **deleted**; §4's `proxy_test.go` row now lists all three test changes (one moves, one is deleted, one changes meaning). **F5.4** — §6's Sessions-view bullet corrected: the merged rows are the 511 the proxy already held under a synthetic key, not JSONL-only rows, and the move is in **figures** not **counts**; D4's row-deleting collision is kept distinct rather than denied. **F5.5** — `cli-and-tooling.md:6` (18 → 19 subcommands) and `INDEX.md:37` (51 → 52 test files) added to §4. **F5.6** — §5's `internal/store` rationale corrected: the incoming event's session is a JSONL session that owns its own rows, so the true reason the removed assertion is vacuous is that **no row is written for the incoming event**, not that the session is empty. |
| 2026-09-20 | Round 6 revision (F6.1–F6.2). **F6.1 (MAJOR)** — D4's collision step named no **taker**, and the reachable one after D1 is a **JSONL** row, not another proxy row: `getEventByRequestIDTx` filters on `request_id` with no `source` (`merge.go:82-84`), the tailer keys on `message.id` once D1 lands, and D4's own ordering rule requires D1 to land **before** the backfill runs — so recent JSONL rows already hold the keys the proxy half claims. `mergeEvents` keeps `existing`, so the survivor would be a JSONL row in a JSONL session with no `sessions` row: exactly the harm D4's ordering argument exists to prevent, arriving through the half that argument assumed was safe. Step 3 now branches on the taker's source; when the taker is JSONL the **proxy** row survives, via `mergeEvents(proxyRow, jsonlTaker)` with the arguments swapped so the proxy row is `existing`. That is the live behaviour restored rather than a new rule — `insertOrMerge` hard-codes which argument is `existing`, which is why this is a `rekey`-only helper and the live merge path stays untouched. D3 gains the matching scoping: "a merged row always lands in a session with an aggregate" is a property of the **ordinary ordering**, not of `mergeEvents`, and D4 states the rule for the path that can violate it. The ordering section now says ordering is necessary but not sufficient. §5 gains the **JSONL-taker** collision as a required second shape — the proxy-vs-proxy fixture pins the reconcile, but it is the shape the measured data says cannot happen, and it cannot see this defect. **F6.2** — `docs/context/testing-and-quality.md:12` ("51 test files") added to §4, the third present-tense count of the same class. |
| 2026-09-20 | Round 7 revision (F7.1–F7.4, F7.6; F7.5 folded in). **F7.1 (BLOCKER)** — the round-6 fix named a mechanism that does not work: `mergeEvents` **never assigns `RequestID`** (`merged := *existing`, `merge.go:147`), so `mergeEvents(proxyRow, jsonlTaker)` returns the proxy row's **old synthetic key**, `updateEventTx` binds `request_id` first and writes it straight back, and deleting the taker then leaves the message id held by nobody. Silent on the ordinary case — the run reports success, re-keys nothing, and *succeeds on a second run*, which is the opposite of D4's promised no-op. `mergeEvents` also takes the key **and** the session from one argument, so no single call expresses this operation at all. **D4's step 3 is now a requirement list with a named trap rather than a procedure**: proxy row survives, `request_id` is the target key, content unioned, taker deleted first (it holds the `UNIQUE` key, which is why write-then-delete is impossible), `source_mismatch` decided as `insertOrMerge` decides it, touched sessions reconciled — with the sequencing left to the implementer and bead 05's cross-review. **F7.2 (MAJOR)** — §5's JSONL-taker case now asserts the surviving row's `request_id` (the one assertion that catches F7.1), asserts it **before** the second run, and pins differing token columns so the aggregate bullet can fail; the second-run no-op moves after it. **F7.3** — "content is a wash either way" deleted as false: the winner pick is order-dependent, so tokens/cost/`billing_mode`/`stop_reason`/`model_resolved` follow the argument order; the direction is still right (it reproduces the live seat), the justification was not. **F7.4** — `source_mismatch` is attached by `insertOrMerge`, not `mergeEvents`, so a helper calling the latter merges a token-disagreeing collision silently while §6 tells the operator to expect those warnings; now part of the helper's contract. **F7.5** — the reconcile-set paragraph named the JSONL taker twice; folded to "the session of whichever row was deleted". **F7.6** — `data-privacy-and-compliance.md:111-113` says `--dry-run --yes` "reports and deletes in one pass", citing `purge.go:20` for the opposite of what it says; §4 now carries that correction alongside the destructive-command claim. §6 gains the F7.1 trap as a named risk — the only defect in this story no CLI observable would reveal. |
| 2026-09-20 | Round 8 revision (F8.1–F8.5), and the **final word** on D4's collision path. **F8.2 (MAJOR, mechanism)** — the round-7 requirement list said "the content is the union of both rows", which is **not** the rule `mergeEvents` applies: it is a **winner pick plus a per-column backfill** (the six token columns, the cost columns, `stop_reason`/`stop_category`, `service_tier`, `speed`, `model_resolved`, `billing_mode` all follow one side; `capture_complete` is the OR; only `source_refs` is unioned). An implementer satisfying the stated rule writes the **inverse** of live behaviour, and no CLI observable and no §5 aggregate bullet distinguishes it — so the reform that removed the broken mechanism had left one of its obligations unstated. D4 now names the winner-pick rule explicitly, lists which halves do which, and notes that for two complete captures the pick is `winner = incoming` (`merge.go:168-169`). **F8.1 (MAJOR, mechanism)** — §4's `internal/store/store.go` row was never updated by round 7 and **still prescribed the exact call D4 forbids** ("calls the existing `mergeEvents` with the arguments swapped"); since §4 becomes beads, that was a live directive to ship the silent no-op. Rewritten. **F8.3** — the round-7 replacement clause was **inverted** (it said the measurement columns follow "whichever argument is `existing`"; for two complete captures they follow `incoming`); corrected against `merge.go:168-169`. **F8.4** — bead 05's directive stated the delete-first step as general when it holds only in the JSONL-taker arm (D4 has the taker surviving in the proxy-taker arm), and §6 cited "§9's bead 05" for a list that lives in D4. **F8.5** — §5 asserted no `source_mismatch` warning even though the helper owns that decision and §6 tells the operator to expect those warnings from the run; a bullet added. §6's named-risk entry now carries **both** silent-failure classes — the key one and the content one — since they are the two defects here that no CLI output would reveal. **Loop closed at 8 rounds / 59 findings, 0 rejected outright; the plan is not marked converged — the collision path is declared converged by human decision after F8.1/F8.2, with the residue carried as named bead-05 traps.** |
| 2026-09-20 | Round 10 revision (round-9 findings F9.1–F9.19, plus one coordinator-added finding; plan v10). **F9.3/F9.4 (RESOLVED BY MEASUREMENT, applied as directives)** — the proxy stores no `request-id` in **either** direction (0 of 728 responses, **0 of ~1,400** request headers), and **2,807 of 2,807** transcript lines matching a stored proxy body id carry no `requestId`; so `requestId` is present **exactly when the request did not traverse this proxy**. §1 and §2.2 rewritten: the mixed session runs two models against **two endpoints**, and "through the one proxy" is named as the reading the census falsifies; §2.2 records both F9.4 checks as **run**. **F9.7** — D1 keeps the header tier **first** (it is the only tier that can key a body with no id, 65 of 728) and gains the **meeting condition** (the tier is a property of the upstream response, so both sources land on the same tier by construction), the **accepted exception** (a proxy capture with no headers stored stays on tier 2 while a tier-1 transcript does not merge — recorded, not fixed), and the 16,804-row status; §5 gains the tier-pairing fixture asserting the split. **F9.10–F9.13** — D4 keeps its requirement list and **gains** the derived-column obligation (`total_prompt_tokens` re-derived from the survivor's four prompt columns in the same write; §5 asserts it on the row), the third winner-pick rule (the never-observed-usage override, `merge.go:196-206`; §5 asserts an all-zero picked side), the synthetic predicate (`request_id LIKE 'proxy:%'`, with why the prefix rather than "the body yields a different id"; §5 asserts a header-keyed row is unchanged), and the decode limit (config `BodyCapBytes`, with the encoded-body failure mode; §5 asserts gzip re-keyed and an invalid/missing-encoding row **counted in M**). D4 states in the document that the sequencing and field-copy mechanics are **deliberately not specified further**, and why. **F9.12's** predicate and **F9.11's** override also reach §4's `store.go` row and §9's note. **Coordinator-added finding (F9.20)** — `x-claude-code-session-id` arrives in **1,364** proxy request headers with **3** distinct values, all 3 real JSONL `sessionId`s, and **0** reach `events.session_id`: the "0 of 184 overlap / only proxy sessions have an aggregate" facts are a consequence of the proxy **discarding** the true conversation id, not a property of the data, so §2.6, D3, D4 and §8 now say "**as the proxy is currently written**", and §8's split-out story is named as **much smaller than it reads as**, with the header named. **The rest** — §2.3's reproducibility method (population, decode, precedence, scan, the ad-hoc extractor caveat) and its counted residue (N/M/K) (F9.1, F9.2); D2's first-seen precedence with its two-`message_start` fixture and the §6/§5 restatement of what the `type` guard actually buys (F9.2, F9.8); §2.4's floor framing (F9.6); D2's sketch field renamed `RequestIDHeader` (F9.9); the 152-id drawer now a **measured** result — re-measured at **303** ids with **0** mismatches, all true absentees, and the discriminator's definition recorded — superseding this round's first attempt to hand the test to the live-acceptance bead (F9.5); §6's re-derivability turned into a **checked precondition** of the run with a §5 refusal case (F9.14); §4's `cli_test.go` row corrected (14 of 18, and `rekey` joins the credential table, not the carried-over one) (F9.15); §6's retry bullet corrected (`message.id` stability is unmeasured) with a §5 same-body-id case (F9.16); §5's dry-run asserts the **exact N/M pair** (F9.17); §6's "`mergeEvents` unions" replaced with the winner-pick/backfill rule (F9.18, also fixed in D4's JSONL-taker bullet and §9's note); D4's `--dry-run` "nothing else" relaxed to "the counts plus the re-pricing/re-derivability notes; no row-level output" (F9.19). **Bead naming** — the plan's §9 sketch numbers are mapped to the real beads under `.beads/GI-9/`, and the destructive bead is referred to as **br-GI-9-04** throughout (the coordinator's directive named it; the sketch had called it bead 05). **Addendum (same round-9 apply, folded into this row — no v11).** F9.5 was resolved by the coordinator's live-DB measurement: **303** proxy ids with no transcript counterpart, **0** interval+token matches under a **different** id, **303** true absentees, **92** proxy rows yielding no id — so §2.3 states the **result** (with the discriminator's definition) rather than naming a runner, §5's drawer check becomes a re-confirmation, and §6's drawer bullet cross-references it. The premise axis' three MAJORs now read as **RESOLVED**, not as caveats: F9.3/F9.4 as already applied and F9.5 by measurement. F9.1's method paragraph now carries the **query** and the **ratio's stability** — this plan's pass 511 of 663 = 77% against the coordinator's re-measure **947 of 1,203 = 79%** on a corpus **78% larger** — stated as **perishable absolutes over a stable relation**, and §5(1)'s acceptance target moves from a frozen count to that ratio. F9.2 stands as fixed (the first-seen precedence and the counted N/M/K residue). §2.4 now carries the two percentages (**≈32%** against **≈71%**) so the "bound, not agree" framing is explicit (F9.6). §2's preamble states plainly that the premise survived an adversarial round **and** an independent re-measurement with a **stricter extractor**, with the story's own defect's hiding place tested and empty. |
