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
`deepseek-flash` lines, none carrying one — a line carries a `requestId` when its response came from a
header-sending upstream, and the requests that traverse this proxy do not, so presence and proxying are
inverse **on this install**. The plan's *Cross-source identity* assumption (test 11b) was therefore
never merely unverified — it is unverifiable as written.

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

### 2.2 `requestId` tracks the upstream endpoint, and on this install the endpoint is disjoint from the proxy

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
exceptions. **The measured leg is one-way, and it is its contrapositive that is usable**: every line
that traversed this proxy carries no `requestId`, so every line that *carries* one did **not** traverse
it. The converse — absent ⟹ traversed — is not measured and does not hold, and the next bullet is the
counterexample: no-`requestId` sessions exist that **predate** the proxy's first stored row, so their
absence is not proxy-caused. The field marks the **endpoint** that answered, not the proxy; the requests
this proxy forwards happen to reach a header-less one, and a request sent straight to that same endpoint
would carry none either. That is why presence and proxying are inverse **on this install**, and the
mixed session is the rule at its sharpest:

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
whose id does not match the body id. **The drawer's figures are their own pass, on the grown corpus —
they are not a complement of the 947-of-1,203 above.** They were re-measured separately once the corpus
had grown: of that pass's **303** ids with no verbatim transcript match, **0** mismatched. The three
figures in the ratio paragraph (1,203 ids / 947 matched / 303 unmatched) therefore come from **two
passes on two corpora** and do not close as one census (`1,203 − 947 = 256`, not 303); the 0-mismatch
result is unaffected either way. The discriminator is defined so that the zero means something: for
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

The asymmetry **that D4's ordering argument rested on is what this story ends (D7).** As the proxy is
currently written, the `sessions` table holds **184 rows, precisely the 184 distinct proxy
`session_id`s**, while **0 of 207** JSONL sessions have one — because `SetSessionRecorder`
([internal/jsonlogs/jsonlogs.go:143](../../internal/jsonlogs/jsonlogs.go#L143)) has **no caller anywhere
in the module**, so the JSONL half silently records no `sessions` rows at all. That, not any reconcile
change, is why a merged row presently always lands in a **proxy** session and why an ordering was
argued to matter: a JSONL survivor would land in a session with no aggregate row to hold it.

**The 0-of-184 overlap is a consequence of the proxy's code, not a property of the data.** The proxy
*already receives* the true conversation id: `x-claude-code-session-id` arrives in **1,364** stored
proxy **request** headers, has **3** distinct values, and all **3 are real JSONL `sessionId`s** — and
it is **already on disk**: the consumer stores every header map verbatim on the row
([internal/consumer/consumer.go:442-447](../../internal/consumer/consumer.go#L442-L447),
[schema.sql:55](../../internal/store/schema.sql#L55)), and `redactHeaders` neither lists it nor mutates
the original ([internal/proxy/redact.go:29-48](../../internal/proxy/redact.go#L29-L48)). What the proxy
does **not** do is promote it: the header selects a grouping key and nothing else
([internal/session/session.go:70-78](../../internal/session/session.go#L70-L78)), the row's `session_id`
is a **minted** `s_<unixMilli>_<hex>` ([session.go:58,80-84](../../internal/session/session.go#L80-L84)),
and **0** of the 1,364 values ever reach `events.session_id`. So "only proxy sessions have a
materialised aggregate" and "the two session sets never overlap" hold **only as the proxy is currently
written**; **D7 ends both**, and it is a small change precisely because the value is already captured —
no new capture and no new retention. The sessions consequence that this section records as an
asymmetry is the state D7 removes.

**Nothing follows from this for the store's merge path**, which is why §4 lists no merge-path change:
D7 changes which value a *writer* puts in `session_id`, not any rule about what a merge does with it.

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

**D7, below, changes *which value* that session is; this decision is about the rule that a merge never
changes it, and it holds unchanged.**
Once a JSONL row and a proxy row share a key, one row survives and it belongs to exactly one session.
The rule is already in `mergeEvents` and this story does **not** change it:
`merged.FirstSource = existing.FirstSource` and `merged.SessionID = existing.SessionID`
([internal/store/merge.go:226-227](../../internal/store/merge.go#L226-L227)) — the row keeps the
session it was **first written under**, and the later arrival never displaces it.

That is the right rule here, and **D7 is what makes it right rather than merely conventional.** Once
both sources write the **same conversation id**, the survivor's session is the conversation whichever
source it is, and every session it can land in has an aggregate row, because D7 wires the JSONL half's
recorder. As the proxy is currently written the reason was §2.6's asymmetry instead — on the ordinary
ordering the survivor was always a **proxy** session, the only kind with an aggregate row — and D7 ends
that asymmetry rather than leaning on it. So the rule above is unchanged; the *guarantee* it used to
need an ordering argument to stand on is now a property of the identity.

**That guarantee used to be a property of the ordinary ordering rather than of the merge; D7 makes it a
property of the merge.** As the proxy is currently written, `mergeEvents` keeps `existing` without
checking the *source* of the row it is merging into, so any path where a JSONL row got a key first
produced a JSONL survivor — a row in a session with no aggregate, invisible to `clens sessions` and the
dashboard's Sessions tab. The live merge could not do that because the proxy row always got there first,
and D4's rekey collision could — which is why round 6 gave D4 a branch on the taker's source. **Under
D7 that harm is gone at the root**: the two rows of an ordinary merge are the same conversation — the
proxy row carries the id the JSONL row already does — so the survivor's session is a real conversation
and neither source is privileged. (D4's rekey collision is the exception, and D4 states it: its deleted
row is a *historical* proxy row that keeps its minted `s_…`, so that reconcile is always a set of two.)
D4's source branch is deleted with the rest of the
swapped-call design (D4), and no later path needs to check a survivor's source either.

Two consequences the design depends on, both stated so a later change is a decision rather than a
drift:

- **On the ordinary merge path the survivor's session is the only reconcile target, and only when the
  survivor's token columns actually moved.** `InsertEvent`/`InsertEvents` reconcile `insertOrMerge`'s
  returned session ([internal/store/store.go:260](../../internal/store/store.go#L260),
  [:288](../../internal/store/store.go#L288)) — the **survivor's**, not the incoming event's, which is
  correct precisely *because* a merge rewrites the surviving row's tokens in place while leaving its
  session alone. Do not "consolidate" the two session columns into the incoming row's. This is a
  statement about the **merge** path. D4's rekey collision is a different path — it deletes a row as
  well as merging one — and there the set to reconcile is the **distinct sessions among the two
  rows**, which for a collision is **always two** (the survivor's conversation session and the deleted
  historical row's minted `s_…`); D4 states it and why.
- **No merge can vacate a session.** The incoming INSERT fails on the unique constraint *before* the
  incoming event ever holds a row, so there is no second row to remove and no session left holding
  totals for a row it no longer has. This was this plan's own first answer, and it was wrong; §2.6
  carries the trace. The one path that *does* delete a merged-away row is D4's re-key collision, and
  that path reconciles explicitly for exactly this reason.

### D4 — The backfill is three passes behind one command

Forward-only would leave an estimated 28,405 surplus rows and every historical total double-counted, so
the story carries the backfill. The passes repair from genuinely different sources, which is why one
command runs three of them rather than a shared loop: pass 1 re-keys rows whose identity is inside
`resp_body`, pass 2 re-attributes rows whose session is inside `req_headers`, and pass 3 deletes and
re-derives the JSONL rows whose identity was never stored at all.

**Pass 1 — the proxy half: re-key in place.** The value is already stored, inside `resp_body`, so these
rows need no re-read. Each row is processed in its **own transaction**:

1. for each `source='proxy'` row whose `request_id` is **synthetic** — the predicate is
   `request_id LIKE 'proxy:%'`, and it must be the **prefix**, not "the body yields an id different
   from the row's key": a row keyed by a *header* value (tier 1) is indistinguishable from a body-id
   row by inspection, and rewriting it to its body id would silently split it from the JSONL row that
   keys on the same header value — converting D1's working Anthropic case into the non-merging case D1
   exists to avoid — **and** whose body yields an id: decode the body and extract the id;
2. if the new key is free, re-key the row (`UPDATE events SET request_id = ? WHERE id = ?`);
3. if the new key is taken, **merge into the taker and delete the row that did not survive**, then
   reconcile the distinct sessions among the two rows — through `reconcileSessionTx`, the tx-taking
   form (§4) — and then **remove the old `sessions` row once re-deriving shows it empty**, or `clens
   sessions` shows a ghost session with no rows: the same clause pass 2 step 3 carries, and the same
   reason. The merge takes the deleted proxy row out of its own minted `s_…`, and that session is
   **never** the survivor's: the deleted row is a *historical* proxy row carrying a minted `s_…` (the
   population argument below — pass 1's predicate selects only pre-D7 rows, and pre-D7 `Resolve` minted
   unconditionally), while the taker sits in the conversation session the request names or in a
   *different* minted one. So nothing later removes it (pass 2 iterates `source='proxy'` rows, pass 3
   touches only
   `jsonl:`-keyed rows, and a second run is a no-op), while `reconcileSessionTx` only `UPDATE`s and
   never deletes a `sessions` row ([`store.go:684-767`](../../internal/store/store.go#L684-L767)) and
   `ListSessions` takes no `request_count` filter
   ([`store.go:783-799`](../../internal/store/store.go#L783-L799)), so it is user-visible in both
   `clens sessions` and `/api/sessions` — **the exact outcome pass 2's own rationale rejects**. `clens
   purge` already leaves zero-row `sessions` rows ([`store.go:828-834`](../../internal/store/store.go#L828-L834),
   `:861-867` delete `events` only), so this is consistency with pass 2, **not** a new invariant — but
   a command whose sibling pass forbids the artifact must not leave it. **The collision is the run's only
   delete site that can dangle a `replay_of`, so its re-point is here** (F15.2, F16.1, §6): a
   `replay_of` can only name a **body-carrying** row, and pass 3's delete takes only `jsonl:` keys, which
   never carry a body — so a row whose `replay_of` names the deleted proxy row's id is re-pointed to the
   survivor. An implementation that resolves the reverse lookup over the wrong id set — pass 3's
   `jsonl:`-prefixed ids, the empty one — is caught here. The taker is whatever holds the key (`getEventByRequestIDTx` filters on
   `request_id` alone, [`merge.go:82-84`](../../internal/store/merge.go#L82-L84)), and after D1 that is
   usually the **JSONL row** — the ordinary case, not a corner, because the JSONL row for a request is
   written minutes after the proxy row for it.

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

**Whichever source the taker is, the survivor is the taker, and under D7 that is correct.** Round 6
gave step 3 a branch on the taker's source because a JSONL survivor landed in a session with no
aggregate row. **D7 deletes the premise**: the survivor's session is the conversation the request
names — the id the proxy row itself now adopts (D7) and the JSONL row already carries — so the survivor
is right whichever side it is, and that session's aggregate is materialised by pass 2's upsert and by
pass 3's re-ingest (D7's wiring), not assumed to exist. **The deleted row is a different matter and is
not the survivor**: it is a *historical* proxy row holding a minted `s_…`, so the reconcile is the
**set of two** and step 3 must also remove that emptied session (above). So step 3 needs no branch, and **nothing is re-keyed** — the taker already holds the key the proxy row is claiming; the
proxy row is the `incoming` seat, and the merge never writes it — which is why the caller deletes it
rather than writing it.

**One factored helper owns the merge and not the load.** `insertOrMerge`'s collision branch
([`merge.go:112-131`](../../internal/store/merge.go#L112-L131)) is `mergeEvents` → `updateEventTx` →
attach `source_mismatch`, and those three become an unexported **`applyMergeTx(ctx, tx, existing,
incoming *Event)`** that **both** `insertOrMerge` and the rekey path call, so the two callers cannot
drift into two rules. **The load is the one thing they cannot share, so it stays with each caller**:
`insertOrMerge` loads the row it failed to insert, and the rekey path loads **by the target key**
([`merge.go:82-84`](../../internal/store/merge.go#L82-L84)) — so its `existing` seat already holds the
key. That is the whole rule; the rekey path's call order is `insertOrMerge`'s, not a second sequence
stated here (§9).

**The round-7/8/9 traps stay as warnings against reintroducing them.** Both shapes *are* the silent
failures in this story (§6); neither is reachable through the mechanism above — one helper, one call,
the taker as `existing` — but each is one hand-written line away:

- **F7.1** — `mergeEvents` never assigns `RequestID` (`merged := *existing`,
  [`merge.go:147`](../../internal/store/merge.go#L147)), so a **swapped** call writes the old synthetic
  key straight back and **the message id ends up held by nobody** — the run reports success and a second
  run then "works". A helper that **loads** by `incoming.RequestID` is the other mechanism: it finds the
  proxy row under its own `proxy:` key, merges it into **itself** and lets the caller delete it, so the
  taker still holds the key and **nothing is orphaned** — what is lost is the proxy row's content, and
  because no key is wrong a second run changes nothing (F13.10). The design above is immune by
  construction: the taker is loaded **by the target key**, so the key is never in question and nothing
  needs assigning.
- **F8.2** — "content is the union" is the inverse of `mergeEvents`. The helper inherits the content
  rule rather than restating it, so this holds by construction; the warning stands against
  hand-writing it.
- **F9.10** — the never-observed-usage override is part of `mergeEvents` and travels with it.

**`applyMergeTx`'s contract is a short requirement list**, because the rekey path is each item's second
caller and every one is silent when missed: it performs **no load** (the caller supplies `existing`, and
on the rekey path that is the row the target key names, so the survivor already holds the key and keeps
its `id`, `session_id` and `request_id`), and its content is `mergeEvents`' winner pick plus per-column
backfill, never a union.

- **`total_prompt_tokens` is re-derived from the survivor's four prompt columns in the same write** — it
  is a *derived* column the live paths recompute every time (`merge.go:214`, `store.go:261`, `:299`), so
  a helper that copies the winner's six columns and leaves the survivor's stored total in place ships a
  row whose `total_prompt_tokens` contradicts its own columns, the invariant CLAUDE.md calls "a tested
  invariant, not a convention";
- **`billing_mode`'s empty-mode derivation is part of the pick** (F10.3) — when the winner's mode is
  empty the label follows the winner's cost column (`api` when the winner's `cost_usd` is non-NULL,
  `subscription` when its `api_equivalent_cost_usd` is), and when the winner priced nothing it keeps
  `existing.BillingMode` ([`merge.go:272-288`](../../internal/store/merge.go#L272-L288)). This is a
  **third** rule after the complete-frame switch and the never-observed override, and it is reachable:
  `billingModeForAuthKind` returns `""` for an unclassified `auth_kind`
  ([consumer.go:475-484](../../internal/consumer/consumer.go#L475-L484)). An implementer who copies the
  winner's empty mode ships a row whose cost column and mode column disagree, and an empty mode matches
  none of the three aggregates' `CASE WHEN billing_mode = …` branches, so the row silently drops out of
  every cost total — the code's own comment calls the naive alternative "worse than the defect";
- **the deleted row's warnings** (F10.8) — `warnings.event_id` is `ON DELETE CASCADE`
  ([`schema.sql:93-102`](../../internal/store/schema.sql#L93-L102)), so **the deleted row's** warnings
  vanish with it. `insertOrMerge` re-computes the *arriving* side's warnings onto the survivor's id, so a live merge
  accumulates both sides'; the helper therefore **re-attaches the deleted row's warnings to the
  survivor** (upsert by `(event_id, kind)`), so the two callers agree. Reachable kinds on a JSONL row
  are narrow but not zero: `source_mismatch` from an earlier `insertOrMerge`, and the tailer's
  `peak_pricing` (the tailer is wired without `SetAnalyzers`,
  [`ingest.go:79-97`](../../internal/cli/ingest.go#L79-L97)).
- **the `incoming` row's `prefix_hash` and replay linkage** (F12.2) — `mergeEvents` assigns neither
  `PrefixHash` nor `ReplayOf`/`ReplayEdits` (they ride `merged := *existing`), so on the rekey path the
  merge **drops** the proxy row's hash and replay linkage — **durably**: pass 3's re-ingest is a JSONL
  row, nil for both, and no later merge assigns them. **The hash is why it matters**: the two hash-keyed
  session rules `continue` on a nil one ([`rules.go:416`](../../internal/analyze/rules.go#L416),
  [`:444`](../../internal/analyze/rules.go#L444)), so the drop defeats F11.3's remedy on exactly the rows
  the backfill merges — the newly computed hash dies in the merge that consumes it. The helper therefore
  **carries the `incoming` row's `prefix_hash` / `replay_of` / `replay_edits` onto the survivor when the
  survivor's is NULL/empty** (the `preferNonEmpty` rule; a no-op on the live path, where `incoming` is
  the JSONL row and is nil for all three), stated for the same reason the warnings are.
- **the survivor keeps its own seat's `started_at` / `source` / `first_source`** (F13.4) — `mergeEvents`
  assigns none of the three (`merged := *existing`, [`merge.go:147`](../../internal/store/merge.go#L147)),
  so the **seat** decides: the JSONL taker's values survive on the rekey path, the proxy row's on the live
  path. `applyMergeTx` must **not** special-case them — an "earlier of the two" rule would make the helper
  diverge from the live path *by rule* on a seat-dependent column, the class F12.1 documented and rejected.
  A bounded, stated consequence of the seat, not a rule change; `first_source` is rendered
  ([`store.go:1261`](../../internal/store/store.go#L1261)) and `started_at` orders `clens ls`.

**The only rekey-specific obligations are the two `insertOrMerge` does not have** — the delete of the
`incoming` proxy row (its own incoming never held one) and the reconcile of the distinct sessions among
the two rows — and **br-GI-9-04** carries the whole list (§9).

**"Content" is not a union.** `mergeEvents` is a **winner pick plus a per-column backfill**: the six
token columns, the cost columns, `stop_reason`/`stop_category`, `service_tier`, `speed`,
`model_resolved` and `billing_mode` all come from **one** side (`merge.go:166-206`) — never summed or
taken column-by-column; `capture_complete` is the OR (`merge.go:223`); the columns one side structurally
cannot supply are backfilled per column (`merge.go:229-326`); and **`source_refs` is the only column
actually unioned** — which is why §5's JSONL-taker case can assert the union while nothing else unions.
An implementer who unions the token columns produces a row no live merge could produce, and no CLI
observable and no §5 aggregate bullet distinguishes it, so the contract names the rule, not a paraphrase.

**The winner pick is order-dependent, and for two complete captures it follows `incoming`**
([`merge.go:168-169`](../../internal/store/merge.go#L168-L169)). On the rekey path the proxy row is
`incoming` and takes the pick; on the live ordering the JSONL arrival does. **But that is an
*intermediate* state, not the one the command leaves.** Pass 3's delete is `source='jsonl' AND
request_id LIKE 'jsonl:%'`, so the pass-1 survivor is **spared** and is the `existing` seat when the
re-ingest reproduces its key — the re-merge again takes `winner = incoming`, the **fresh JSONL row** — so
`rekey --yes` **ends where the live path ends for the winner-picked columns**: the cost, label and token
columns are the JSONL seat's, while the seat-dependent `source` / `first_source` / `started_at` stay the
**surviving seat's** (F13.3, F13.4 — §3 D4's contract). The divergence is a transient of pass 1. The
intermediate state is **permanent** only in the residual **mid-run race** — a transcript becoming
unreadable between the upfront precondition check and pass 3, which leaves passes 1–2 committed and the
pass-1 survivor in place (F13.7). A precondition *failure* is not that case: it is checked first and
refuses with **no mutation at all**. §5's pin asserts the state the command leaves; the residual mid-run
race is **not §5-testable** — it needs a transcript to become unreadable between the check and pass 3,
which no fixture can stage without racing the command — so it is **recorded, not mitigated** (§6), and a
green §5 must not be read as covering it.

It is **not flagged**: `tokensDiffer` ([`merge.go:150-155`](../../internal/store/merge.go#L150-L155)) is
computed over the six token columns only and is the sole operand of `source_mismatch`, so a pair that
**agrees on tokens** while differing on `cost_usd` / `cost_source` / `billing_mode` / `model_resolved` /
`stop_reason` / `speed` / `service_tier` takes **opposite column sets** and raises **no** warning — the
ordinary pair, because both captures are complete and the two sources genuinely price and label
differently.

**The bound on the residual disagreement is a rule difference, not a price vintage** (F12.1).
`billing_mode` is winner-picked, and the two sources derive it by structurally different rules — the
proxy from the credential ([`consumer.go:475-484`](../../internal/consumer/consumer.go#L475-L484)), the
tailer per row from the model prefix with an api override
([`jsonlogs.go:398-411`](../../internal/jsonlogs/jsonlogs.go#L398-L411)) — so a prefix-api model called
with an oauth credential is `subscription` + `api_equivalent_cost_usd` on the proxy row and `api` +
`cost_usd` on the JSONL row, **permanently**, and the mode is what selects the aggregate the row joins
([`store.go:708-709`](../../internal/store/store.go#L708-L709)): the row's money moves between the `api`
and `subscription` totals with the survivor. What holds is the **per-row** invariant (mode and cost come
from the same `winner`, so the row never contradicts itself), not any agreement between the two paths —
the code concedes as much ([`merge.go:247-251`](../../internal/store/merge.go#L247-L251)). D4 therefore
adds **no** hand-written winner pick (that would reintroduce F8.2's hand-written merge); reconciling the
two derivations is a different story.

**The switch is not the whole rule.** After it, a winner that carries **no observed usage** loses the
pick to the other side (`merge.go:196-206`) — the `--body-policy off` correction, where a row's zero is
"never looked", not a measurement, so it never takes the pick from a row that has some. The complete
statement is therefore the sequence: the complete-frame switch, then the observed-usage override, then
the empty-`billing_mode` derivation (F10.3). §5 pins the override with a fixture whose picked side
carries all-zero usage while the other side does not, asserting the figures follow the **observed**
side.

**The reconcile set is the distinct sessions among the two rows — and for a pass-1 collision it is
always two.** The deleted row is a *historical* proxy row: pass 1's predicate (`request_id LIKE
'proxy:%'` **and** the body yields an id) selects **only pre-D7 rows**, because a post-D7 row keyed
`proxy:…` is by construction one whose body yields no id (D2's precedence would have keyed it by the
body id), and before D7 `Resolve` minted **unconditionally** — the only header that ever reached it was
`x-clens-session`, and it never became the id ([`session.go:58`](../../internal/session/session.go#L58),
[`:80-84`](../../internal/session/session.go#L80-L84),
[`meta.go:38-40`](../../internal/parse/meta.go#L38-L40)). So that row's session is always a minted
`s_…`. The taker holds the **target key** (a body id), which no pre-D1 row can hold — most often it is
the **JSONL** row, whose `session_id` is the transcript's own conversation id
([`jsonlogs.go:381-386`](../../internal/jsonlogs/jsonlogs.go#L381-L386)) — and a minted `s_…` never
equals a conversation id. **Do not claim the set is one**: it is the survivor's conversation session
**and** the deleted row's minted `s_…`, on every run.

**Both sessions can move, so re-derive the set rather than one.** The absorbed `incoming` row is deleted
outright — removing a row leaves its session's totals high — while `mergeEvents` copies the **winner's**
six token columns onto the survivor ([`merge.go:208-214`](../../internal/store/merge.go#L208-L214)),
where the winner can be either side, leaving the survivor's session wrong; `InsertEvent` already
reconciles exactly the survivor's ([`store.go:275-280`](../../internal/store/store.go#L275-L280)), and
the rekey path reproduces that rule rather than approximating it. That the branch had to be factored out
as `applyMergeTx` rather than reached through `insertOrMerge` — which hard-codes its `existing` as the
row it failed to insert — changes nothing about the live call: **the live merge path is untouched**
(§2.6, §4), only its collision branch gains a second caller.

**Use `reconcileSessionTx`, not `ReconcileSession`.** `ReconcileSession`
([internal/store/store.go:672-682](../../internal/store/store.go#L672-L682)) opens its **own**
`BeginTx`, and the pool is pinned to a single connection (`db.SetMaxOpenConns(1)`,
[internal/store/store.go:72](../../internal/store/store.go#L72)). Calling it from inside the per-row
transaction below leaves the outer transaction holding the only connection and the inner `BeginTx`
waiting on the pool with no deadline — a **hang**, not an error, and one no test would report as a
failure rather than a timeout. `reconcileSessionTx`
([internal/store/store.go:684](../../internal/store/store.go#L684)) is unexported and takes a `*sql.Tx`,
which is why both existing callers use it and why the rekey methods must live in `internal/store` (§4).

**Pass 2 — re-attribution: the historical proxy rows adopt their conversation id.** Pass 1 is about
*keys*; this pass is about the *session*, and it exists because the forward fix (D7) only affects rows
written after it lands — every proxy row already in the table still carries a minted `s_…`. Each such
row's true conversation id is **already on disk** in its own stored `req_headers`
(`x-claude-code-session-id`, D7), so this pass needs **no new capture and no new retention**: it reads a
field the system already keeps.

1. for each `source='proxy'` row, read its `session_id` and its `req_headers`, resolving the session
   header by the **same rule the live path applies**. That is **the same precedence** (F15.4):
   `x-clens-session` is read first and `x-claude-code-session-id` fills the value only when it is
   absent — the operator override still wins, exactly as D7 change 1 settles for the live path (§4's
   `meta.go` row) — so a historical row whose stored `req_headers` carry an `x-clens-session` override
   is re-attributed to the **override** value the live path would have stored, not to the conversation
   id. Pass 2 mirrors the live *length* rule deliberately (F12.7) and mirrors the live *precedence* for
   the same reason — it exists to reproduce what the live path stores, and a divergence here would
   reintroduce the F12.7 class of split. It is **also the same length rule** (F12.7): `ExtractMeta`
   leaves a header longer than `maxSessionHeaderLen` **empty** rather than truncating it, whichever
   header supplies the value ([`meta.go:38`](../../internal/parse/meta.go#L38)), so pass 2 must treat
   overlong exactly as absent — the "no such value" case, not a second one — and the row keeps its
   minted `s_…`, **counted in `--dry-run` as part of **L****, not reported as re-attributed. The settled rule: the overlong-header
   row belongs to **L**, not to pass 1's N/M — L is pass 2's population of rows that keep their minted
   `s_…`, and pass 2 treats an overlong header as absent, so its row is an L row like any header-less one;
2. **`UPDATE events SET session_id = ? WHERE id = ?`** — a direct write, and **it cannot route through
   the merge**: `mergeEvents` never rewrites `session_id`
   ([`merge.go:227`](../../internal/store/merge.go#L227)) and **that stays true** (D3), because this
   pass is *re-attributing* rows, not merging them. **This is the one write path in the story that sets
   `session_id` on an existing row** — stating it here is what stops a later reader concluding D3 was
   relaxed;
3. reconcile **both** sessions through `reconcileSessionTx`: the old `s_…` (a row left it) and the new
   conversation id (a row joined it, so **upsert** it first — after pass 1 the conversation session may
   not exist yet). Then **remove the old `sessions` row once re-deriving shows it empty**, or
   `clens sessions` shows a ghost session with no rows.

The upsert is not incidental: pass 2 can run before pass 3 has re-ingested anything, so nothing has
created the conversation session yet, and a bare reconcile against a missing row would leave the
aggregate unrepresented.

**Pass 3 — the JSONL half: delete, then re-ingest.** A stored JSONL row cannot be re-keyed in place:
its old key holds a uuid and the message id it *should* hold was never stored. The only place that value
exists is the transcript. So the pass is:

0. **the precondition is checked first, before pass 1, and a failure is a refusal** (F13.7). The check is
   over the recorded `jsonl:<path>` cursors below, and **pass 1 does not touch those rows** — it re-keys
   `source='proxy'` rows — so it is valid before any mutation: a failed precondition aborts the command
   with **no rows changed** and a **non-zero exit**, and `--dry-run` reports the same check and the same
   refusal. The
   schema carries **no row→file link** — `events` has no transcript-path column (`path` is the request
   URL path, proxy-only, [`schema.sql:53`](../../internal/store/schema.sql#L53)), and the key embeds a
   `sessionId` and a `uuid`, not a path ([`dedup.go:104`](../../internal/jsonlogs/dedup.go#L104)) — and
   the mapping is not a function anyway: a sidechain row takes the **parent's** `sessionId`
   ([`jsonlogs.go:381-387`](../../internal/jsonlogs/jsonlogs.go#L381-L387)), so one session id owns the
   parent file *and* every subagent file. The check is therefore stated over the set the schema **does**
   carry — **every recorded `jsonl:<path>` cursor** ([`cursor.go:18`](../../internal/jsonlogs/cursor.go#L18)):
   the file exists and its current size is not below the recorded byte cursor. **The qualifier "whose
   path the re-ingest will walk" is dropped** (F11.7): the delete is unconditional over the `jsonl:`
   prefix while the walk is root-scoped, so excluding a cursor whose file has since been deleted would
   destroy rows nothing can rebuild — **a recorded path with no readable file is a refusal, not an
   exclusion**. And the delete is **scoped to what that walk can reproduce**: the
   re-ingest is root-scoped (`resetJSONLCursors` walks `root` and zeroes only the `.jsonl` files it
   finds, [`ingest.go:135-145`](../../internal/cli/ingest.go#L135-L145)), so a row whose transcript
   lives outside `jsonlRoot()` would be deleted and never re-read — **the root-change case is a
   refusal**, and §5 seeds it. Re-derivability is a *checked precondition of the run*, not an
   assumption: §6 states why a shrunk or deleted transcript makes a row non-re-creatable. The one residual
   case the upfront check cannot close is a **mid-run race** — a file becoming unreadable between the
   check and pass 3 — which leaves passes 1–2 committed; F13.7 narrowed F12.3's "permanent intermediate
   state" from a refusal to exactly this race;
1. delete `source='jsonl' AND request_id LIKE 'jsonl:%'`;
2. zero the byte cursors and re-run the tailer — the mechanism `clens ingest --rebuild` already owns.

Step 1 is a **single set-based `DELETE`, committed in one transaction** (F13.8) — it is one statement, so
there is no per-row model to state, and F13.6's `replay_of` re-pointing commits inside that same
transaction so no reference is ever briefly dangling. A crash between the delete and the re-ingest leaves
the `jsonl:` rows deleted and nothing rebuilt; they are **re-derivable** (the precondition above), so a
re-run rebuilds them — which is why this pass needs **no per-row resumability of its own**, unlike passes
1 and 2, whose per-row commits exist because their old→new mapping is otherwise unrecorded.

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
surplus. Naming it is what makes "one command, three passes" an argued decision rather than an
unexplained asymmetry.

**Ordering: pass 1, then pass 2, then pass 3 — fixed for the reconcile set and for determinism.** The
sequence decides nothing about the *end state*: the survivor is the taker, and its row, key and session
are the taker's whichever order runs. It **does** decide the reconcile *set*: pass 1 runs before pass 2,
so the row pass 1 deletes has not been re-attributed and still carries its minted `s_…`, which is why
that collision's set is always the two sessions D4 names rather than one. The reason the ordering *was*
argued for — proxy half first so the survivor keeps a session with a materialised aggregate — is dead
with the asymmetry that made a JSONL session aggregate-less (§2.6, D7), and it is **dropped rather than
re-justified**: there is no surviving reason. The fixed sequence is kept because a stated order makes
the run's output reproducible and its report readable.

*Rejected alternative — have `rekey` upsert the survivor session.* This is now **accepted, and is what
pass 2 does**: the conversation session is upserted so a re-attributed row has somewhere to land. What
the earlier rejection protected — a JSONL-only session appearing in the Sessions view with no rows —
cannot happen, because the upsert is followed immediately by the row's update and the reconcile; the
only upserted session with no rows would be one whose every row then moved again, which the same pass
removes.

**One command, `clens rekey`**, following the contract
[cli-and-tooling.md](../context/cli-and-tooling.md) states for any destructive subcommand — and the
repo currently has exactly one:

- nothing happens without `--yes`;
- `--dry-run` prints what `--yes` would do — all three passes' counts, **plus the re-pricing,
  re-derivability and dangling-`replay_of` notes the operator needs** (below); **no row-level output**;
- the three passes run in the order stated above, and the command reports each separately, because a run
  that did one and failed another must not read as "done".

**Per-row transactional merge, so a partial run is resumable.** Passes 1 and 2 each **overwrite a
column in place** (`request_id`, then `session_id`), so a crash mid-run would otherwise lose the
old→new mapping with no record. Each row's re-key or re-attribution (and any merge it triggers) is
therefore committed in its **own transaction**: a partial run leaves the rows already done correct and
the remainder still synthetic, so re-running picks up where it stopped. Nothing is atomic across a
pass, deliberately.

**`--dry-run` must report "would re-key N, would leave M synthetic (no body id), would re-attribute K,
would leave L unattributed".** **4 rows with `resp_body IS NULL`** plus **61 whose body yields no id**
— 65 of 728, the same population D1 counts — stay synthetic forever, and the operator needs to see that
number before the run. A row whose body is compressed but decodes nothing (a non-positive `limit`, or a
`Content-Encoding` the scan did not undo) belongs in **M**, not in **N** — the same "silently re-keys
nothing" outcome as above, made visible rather than reported as re-keyed. **K and L are pass 2's pair**:
rows whose `req_headers` carry `x-claude-code-session-id` are re-attributed, and rows whose headers
lack it — **including the overlong case, which pass 2 treats as absent (F12.7)** — keep their minted
`s_…` and are counted in **L** — the same "would leave X alone" visibility,
for the same reason. §5 asserts the exact N/M pair, not just that both counts are non-zero.

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
- **`internal/proxy` stays a tee.** It loses a function (D2) and gains nothing. It also gains no
  session logic: nothing in D7 touches the hot path (`ExtractMeta` and `Resolve` both run in the cold
  path, and the recorder is a tailer seam).
- **`mergeEvents` still never rewrites `session_id`.** D3 holds. D7's re-attribution is a **direct
  `UPDATE` on the rekey path**, not a merge rule, and the live merge path is untouched (§2.6, D4).
- **What each source captures** is untouched. The proxy still stores the compressed body it received;
  the tailer still stores the transcript content. **D7 adds no capture and no retention** — it reads
  `x-claude-code-session-id` out of the `req_headers` the consumer already stores verbatim, and reads
  the transcript `sessionId` the tailer already had.

### D6 — The two documents that disagree about test 11(b) are reconciled

`docs/planning/GI-1-claude-lens-v1.md` is **internally consistent and does not contradict**
`docs/acceptance.md`. `:1105` and `:988` read "is an assumption **verified live** *before the merge is
relied on*" — a prospective precondition, not a claim of having verified. `:57` says "not yet evidence"
and `:325-333` says "**Status: still open — not captured.**" Both docs agree.

What this story measured does **not** record a negative result. **The equivalence was never exercised
on this install — the configured upstream sends no `request-id` header — and it is therefore moot
*here*, because no response this install served carries the header.** An endpoint that sends no
`request-id` cannot falsify the Anthropic `request-id` ↔ `requestId` equivalence, so "it does not hold"
would be a false negative. **"Moot" must not be written unscoped**: on an install whose upstream *does*
send `request-id`, the equivalence is **load-bearing** — it is the only tier that can key a body with no
id (D1) — **unverified**, and its failure is **silent** (§6: every merge key misses and the install gets
zero merges, with no error raised). So the wording that goes into both `docs/acceptance.md` and
`docs/planning/GI-1-claude-lens-v1.md` is *never exercised on this install, not falsified, and moot
only where the header is absent* — and test 11(b)'s status is kept distinct from tier 1's unguarded
risk, which the same sentence must not silently resolve.

The genuinely wrong text in GI-1 is elsewhere. Its §Cross-source identity says the documented fallback
"remains the operative identity" and specifies it as `(model, session_id, started_at ±1s, token
quadruple)`. **No such code exists** — the implemented fallback is the namespaced
`jsonl:<sessionId>:<uuid>` / `proxy:<sha256>:<started_at_ns>:<attempt>` pair (§2.1). That, not the
equivalence claim, is the correction worth landing at `:1105`. The same correction goes to the comment
at [internal/jsonlogs/dedup.go:95-99](../../internal/jsonlogs/dedup.go#L95-L99), which records the
assumption as "not yet verified" when what is actually wrong is that it describes a fallback that was
never built.

### D7 — The session is the conversation the request already names

The proxy already receives Claude Code's real conversation id — `x-claude-code-session-id`, present in
**1,364** of ~1,400 stored request headers, **3** distinct values, all 3 real JSONL `sessionId`s (§2.6)
— and already stores it verbatim in every row's `req_headers`. Reading it is **not** sufficient on its
own, because one side already carries that exact value and the other never does:

- **JSONL rows already carry the real conversation id**: `buildEvent` sets `session_id` from the line's
  `sessionId` — the parent's for a sidechain, the filename stem as fallback
  ([internal/jsonlogs/jsonlogs.go:381-386](../../internal/jsonlogs/jsonlogs.go#L381-L386)).
- **Proxy rows always carry a minted `s_<unixMilli>_<hex>`**: `newSessionID`
  ([internal/session/session.go:80-84](../../internal/session/session.go#L80-L84)) is returned
  **unconditionally** by `Resolve` ([session.go:58](../../internal/session/session.go#L58)). The header
  selects only a `groupKey` ([session.go:70-78](../../internal/session/session.go#L70-L78)); it never
  becomes the id.

So four changes, each a **consequence** of that gap or of the change above it rather than a preference:

1. **`ExtractMeta` gains `x-claude-code-session-id` as a second source for `Meta.SessionHeader`**
   ([internal/parse/meta.go:31-40](../../internal/parse/meta.go#L31-L40)). **`x-clens-session` still
   wins** — it is the explicit operator override, so it is read first and the new header only fills the
   value when it is absent. The comment block is updated: it currently documents only `x-clens-session`
   and the `maxSessionHeaderLen` reasoning, and both must now cover the second source — the length bound
   applies to whichever header supplies the value.
2. **`Resolve` returns `meta.SessionHeader` as the id when non-empty**
   ([internal/session/session.go:47-61](../../internal/session/session.go#L47-L61)), instead of minting
   — placed **at the top**, ahead of the `seen`-map logic. A header-carrying call's identity *is* the
   header; the inactivity-gap window is for header-less calls only. No code or doc assumes the `s_`
   prefix (checked), so nothing downstream has to learn a second id shape.
3. **The JSONL tailer is given a `SessionRecorder`.** Today **no caller anywhere in the module wires
   one** — `SetSessionRecorder` ([internal/jsonlogs/jsonlogs.go:143](../../internal/jsonlogs/jsonlogs.go#L143))
   is defined and never called — so `t.recorder` stays nil and the JSONL half silently records no
   `sessions` rows. **That, not a design choice, is the real reason behind §2.6's "0 of 207".** The
   wiring goes where the tailer is built — `newTailer`
   ([internal/cli/ingest.go:78](../../internal/cli/ingest.go#L78)), the one shape both `runIngest`
   ([ingest.go:53](../../internal/cli/ingest.go#L53)) and `addCollectors`
   ([internal/cli/refresh.go:95](../../internal/cli/refresh.go#L95), reached from `serve` and `refresh`)
   use, so the three callers cannot drift. Because the `collectorStore` surface newTailer takes does not
   carry `UpsertSession`/`ReconcileSession`
   ([jsonlogs.go:29-37](../../internal/jsonlogs/jsonlogs.go#L29-L37)), the recorder is **passed in**, not
   constructed inside `newTailer`; §4 lists the files and the plumbing is the implementer's.

4. **`ExtractMeta` stops nil-ing `PrefixHash` when a header is present** (F11.3). Today
   [`:102-109`](../../internal/parse/meta.go#L102-L109) sets `m.PrefixHash = nil` whenever
   `SessionHeader != ""` — an optimisation, on the belief "the hash is not needed", which D7 falsifies:
   the two session rules that key on it (`ruleCacheExpiredBetweenTurns`,
   [`rules.go:416`](../../internal/analyze/rules.go#L416); `ruleCacheConcurrentWriteRace`,
   [`rules.go:444`](../../internal/analyze/rules.go#L444)) `continue` on a nil hash, and D7 makes
   `SessionHeader` non-empty for **every** proxy call — so **the nil-ing silently kills two rules**, and
   nothing in D7, §4, §5 or §6 named it. The fix is to **always compute the hash**: `groupKey`
   ([`session.go:70-78`](../../internal/session/session.go#L70-L78)) checks `SessionHeader` **first**, so
   grouping is unchanged, and `RecordCall`
   ([`session.go:96-97`](../../internal/session/session.go#L96-L97)) then stores a real `prefix_hash` on
   the session row — an improvement, not a regression. The two comments that call the NULL-ness
   load-bearing ([`parse/types.go:74`](../../internal/parse/types.go#L74),
   [`store/types.go:63`](../../internal/store/types.go#L63)) are corrected to name what actually is:
   **the header wins in `groupKey`**, not the nil. `TestExtractMetaPrefixHashNilWhenSessionHeaderPresent`
   (`meta_test.go:100-110`) pins nil-for-header, so its **invariant moves** rather than being deleted —
   it asserts a **computed** hash with a header present, and the "header wins" invariant it was really
   protecting already lives in `session_test.go`'s `TestResolveHeaderOverridesPrefix`.

**Only with (2) does the rest follow.** `ExtractMeta` alone takes 184 heuristic sessions to ~3 and stops
the gap-splitting, **but those 3 stay `s_…` and still never equal a JSONL `sessionId`** — the sets stay
disjoint, and D3, D4's ordering argument, the source branch and all three traps would survive untouched.
Change (2) is what makes the proxy's `session_id` the same value the JSONL side already writes.

**Consequences, each stated as a consequence rather than a preference.**

- **D4 step 3's source branch is gone.** The survivor is the taker, and its session is the conversation
  the request names whichever source it is, so no branch is needed; the collision's *two* sessions (the
  deleted historical row's minted `s_…` and the survivor's conversation) are reconciled as a **set** (D4).
- **The session-scoped analyzer's row set is the rows that carry a request body** (F11.4). D7 turns a
  "session" from one gap-window burst of proxy rows into the whole conversation, so `SessionEvents`
  ([`store.go:325-330`](../../internal/store/store.go#L325-L330)) would return proxy rows and JSONL rows
  interleaved. `ruleCacheInvalidatedByTools`
  ([`rules.go:313-334`](../../internal/analyze/rules.go#L313-L334)) compares **consecutive** pairs and
  skips any pair whose `ReqBody` is empty, and JSONL rows structurally never carry one
  ([`jsonlogs.go:414-437`](../../internal/jsonlogs/jsonlogs.go#L414-L437)) — so the interleaving would
  silently break the adjacency and the rule would fire **less** for no reason anyone chose. The rule
  therefore filters its own row set to **the rows carrying a request body** before the pair walk, which
  preserves today's adjacency: under the default body policy that is exactly the proxy population, and
  because the session is now the whole conversation rather than one gap-window it hands the rule **more**
  proxy rows than it sees today — strictly better, not a narrowing. The pass's cost is a separate
  question, recorded in §6 rather than fixed here.
- **The reconcile set is the *distinct* sessions among the two rows, and for a collision it is always
  two** — the survivor's conversation session and the deleted historical proxy row's minted `s_…` (D4):
  pass 1's population is pre-D7 rows that keep their minted id, and the taker holds a key no pre-D1 row
  could hold, so the two sessions never coincide.
- **D4's ordering argument is dropped, not re-justified.** Its stated reason — "a JSONL session has no
  aggregate row to hold the row" — is dead under (2)+(3): JSONL sessions now have aggregate rows, and
  the survivor (the taker) is right whichever order runs. The order no longer decides the **end state**;
  D4 states that it still decides the reconcile **set**. The fixed sequence is kept **for determinism
  and reporting**, and D4 says plainly that the reason it was argued for no longer holds.
- **F7.1, F8.2 and F9.10 dissolve** — each was a consequence of the swapped call D7 makes unnecessary.
  D4 keeps them only as a warning against reintroducing a swapped call or a hand-written merge.
- **§8's first bullet and §2.6's asymmetry paragraph** described the disjoint-session state as a
  constraint; it is what **this story ends**, and both are rewritten to say so.
- **The visible behaviour change, which §6 and the PR body must name.** Today's **184 heuristic `s_…`
  proxy sessions collapse to the 3 real conversations** — the same conversations the JSONL side already
  knows — so `clens sessions` stops being proxy-only; and **a long conversation stops splitting at the
  inactivity gap**, because one conversation is now one session. **The same wiring grows the view from
  the other side** (F11.8): change (3) gives the JSONL half its `sessions` rows for the first time, so
  the ~**207** JSONL sessions that had none (§2.6's "0 of 207") now appear — the view does not merely
  shrink 184 → 3, it **collapses the proxy duplicates and grows the JSONL sessions**, and the PR body
  must carry **both** numbers rather than the collapse alone. **And `x-clens-session` changes meaning**
  (F11.5): it stops being grouping-only and becomes the stored `session_id`, so a client that sets it to
  an arbitrary label now sees that label in `clens sessions`'s id column — a change in the header's
  documented meaning, not only in the row count. The **~3%** of requests without the header fall back to
  today's gap-window behaviour, and `Resolver.Resolve` keeps its shape.

## 4. Files changed

| File | Change |
|---|---|
| `internal/jsonlogs/dedup.go` | `message.id` on the `line`/`message` structs; the middle tier in `requestKey`; comment corrected |
| `internal/jsonlogs/dedup_test.go` | the new tier's cases (§5) |
| `internal/parse/types.go` | `Usage.MessageID`; and `Meta.PrefixHash`'s doc comment (`:74-77`) corrected — its NULL-ness is no longer "load-bearing for the session resolver" (F11.3): the resolver's `groupKey` keys on `SessionHeader` **first**, so the header — not the nil — is what wins |
| `internal/parse/usage.go` | three struct declarations (`sseMessageStart.Message.ID`, `nonStreamBody.ID`, `nonStreamBody.Type`) and the two assignments — four declarations across three structs with the `Usage.MessageID` above; the non-stream tier gated on the body's own `type` |
| `internal/parse/usage_test.go` | streaming and non-streaming extraction |
| `internal/consumer/consumer.go` | `requestID(call, usage)`; `buildEvent` uses it; the synthetic fallback **moves here with its counter** — a package-level `*uint64` incremented per `requestID` call supplies the `attempt` component, named so the diff is checkable and carrying the reason the deleted code states ([`proxy.go:145-152`](../../internal/proxy/proxy.go#L145-L152), `:262-267`): a server rebuilt mid-process still never reuses a disambiguator, and `started_at_ns` alone is not sufficient because a fast enough retry could share a nanosecond timestamp |
| `internal/consumer/consumer_test.go` | the precedence table |
| `internal/analyze/rules.go` | `ruleCacheInvalidatedByTools` ([`rules.go:313-334`](../../internal/analyze/rules.go#L313-L334)) filters its own row set to the rows carrying a request body **before** the consecutive-pair walk (F11.4, D7): today only proxy rows carry one, and after D7 a session is the whole conversation, so JSONL rows would interleave and break the adjacency the empty-`ReqBody` skip relies on. **No other session rule's *code* changes** — the filter is inside this rule, **not** in `AnalyzeSession` (`analyze.go:60`), so the body-less fixtures in `rules_test.go` are untouched — **but every session rule's *input set* grows under D7, and one other is a consecutive-pair rule too** (F12.5): `ruleCachePrefixInvalidation` ([`rules.go:293-307`](../../internal/analyze/rules.go#L293-L307)) walks the same rows pairwise with **no** `ReqBody` guard, so the JSONL interleaving shifts its result with no code change — verbatim the F11.4 argument — and §5's mixed-adjacency case must cover both rules, or say why the second's degradation is acceptable |
| `internal/analyze/rules_test.go` | the mixed proxy+JSONL session adjacency case (F11.4, §5) |
| `internal/sink/sink.go` | `CapturedCall.RequestID` renamed `RequestIDHeader` — its doc comment at `:57-61` calls the field "the cross-source dedup key", which D2 makes false, and the name would otherwise invite back the two-tier split D2 deletes. The rename's sites are named so the diff is checkable (F11.11): the **field** (`:61`), the **producer** at [`proxy.go:254`](../../internal/proxy/proxy.go#L254), and the single **reader**, [`consumer.go:403`](../../internal/consumer/consumer.go#L403) |
| `internal/proxy/proxy.go` | **three deletions, named so the diff is machine-checkable** (F11.11): the **method** `captureState.requestID` (`:268-291`, doc comment `:259-267`), the per-capture **field** `captureState.fallbackSeq` (`:210`, assigned at `:126`, unused once the method goes) and the package-level **counter** `fallbackSeqCounter` (`:145-152`); `submit`'s `RequestID:` argument (`:254`) passes the header value it saw (`respHeaders.Get("Request-Id")`, or `""`) instead of resolving a key |
| `internal/proxy/proxy_test.go` | three tests, three fates: `TestHashFallbackTwoAttemptsProduceDistinctIDs` (`:583`) **moves** to `internal/consumer` with the code; `TestResponseDerivedRequestIDWins` (`:551`) **changes meaning** (asserts the raw header value passes through, not that it is the dedup key); `TestPolicyOffSurvivesAMissingRequestID`'s synthetic-key assertion (`:456-458`) is **deleted** — it reads the renamed field, which is `""` on these nil-header paths, so the assertion goes red rather than moving |
| `internal/parse/meta.go` | `x-claude-code-session-id` as the **second** source for `Meta.SessionHeader` (`x-clens-session` still wins); the comment block covering both sources and the `maxSessionHeaderLen` bound (D7). **And the `PrefixHash` nil-ing is deleted** (F11.3, D7 change 4): `:102-109` currently sets `m.PrefixHash = nil` whenever `SessionHeader != ""`, so the hash is **always computed** now — `groupKey` ([`session.go:70-78`](../../internal/session/session.go#L70-L78)) checks `SessionHeader` **first**, so grouping is unchanged, and `RecordCall` ([`session.go:96-97`](../../internal/session/session.go#L96-L97)) then stores a real `prefix_hash` on the session row. The nil-ing was an optimisation whose comment ("the hash is not needed") is false once the two cache rules read it |
| `internal/parse/meta_test.go` | the second source, and that `x-clens-session` still overrides it when both are present (D7, §5). **And `TestExtractMetaPrefixHashNilWhenSessionHeaderPresent` (`:100-110`) changes its invariant** (F11.3): it pins nil-for-header today, and the hash is always computed now, so it asserts a **computed** hash with a header present — the "header wins" invariant it was really protecting already lives in `session_test.go`'s `TestResolveHeaderOverridesPrefix` |
| `internal/session/session.go` | `Resolve` returns `meta.SessionHeader` as the id when non-empty, placed ahead of the `seen`-map (D7) |
| `internal/session/session_test.go` | the header's value **is** the id (today's cases assert only distinctness and keep passing with changed meaning); the header-less path still gap-groups (D7, §5) |
| `internal/store/merge.go` | `insertOrMerge`'s collision branch factored into an unexported **`applyMergeTx(ctx, tx, existing, incoming *Event)`** — `mergeEvents` → `updateEventTx` → attach `source_mismatch`, on `insertOrMerge`'s terms ([`merge.go:112-131`](../../internal/store/merge.go#L112-L131)) **minus the load** — called by `insertOrMerge` and by the rekey path, so one implementation, one rule, and the two cannot drift (D4). **The load stays with each caller, because it is the one thing they cannot share**: `insertOrMerge` keeps `getEventByRequestIDTx(ctx, tx, ev.RequestID)` and calls `applyMergeTx(existing, ev)`; the rekey path loads **by the target key** (`getEventByRequestIDTx(ctx, tx, targetKey)`) and calls `applyMergeTx(taker, proxyRow)`. **The live call is otherwise unchanged**, so `insertOrMerge` and its callers keep today's shape |
| `internal/store/types.go` | `EventSummary.PrefixHash`'s doc comment (`:63-66`) corrected the same way as `internal/parse/types.go` — the header wins in `groupKey`, not the nil (F11.3, D7 change 4) |
| `internal/store/store.go` | the `rekey` store methods: pass 1's body-id scan (predicate `request_id LIKE 'proxy:%'`, decoding with the config `BodyCapBytes`), the in-place re-key, and the collision path — `getEventByRequestIDTx(ctx, tx, targetKey)` loads the taker **by the target key**, `applyMergeTx(taker, proxyRow)` merges the proxy row into it, the `incoming` proxy row is then deleted **by its own id**, then the **distinct sessions among the two rows** reconciled via `reconcileSessionTx` (not the exported `ReconcileSession`, which opens its own transaction and would hang against `SetMaxOpenConns(1)`), **the old `sessions` row removed once re-deriving shows it empty** (pass 2 step 3's clause, F15.1 — `reconcileSessionTx` never deletes a `sessions` row and `ListSessions` takes no `request_count` filter, so the emptied `s_…` would show as a ghost), **and any `replay_of` naming the deleted proxy row re-pointed to the survivor** (F15.2, F16.1 — the collision is the run's only delete site that can dangle a `replay_of`: no `replay_of` can name a `jsonl:`-keyed row, §6); **pass 2's** re-attribution — resolve the session header from `req_headers` by the **same precedence and length rule the live `ExtractMeta` applies** (F15.4: `x-clens-session` first, then `x-claude-code-session-id`, the override winning; F12.7: overlong → treated as absent), `UPDATE events SET session_id = ? WHERE id = ?`, upsert + reconcile the new session, reconcile the old and remove its `sessions` row when empty; and pass 3's delete + re-ingest — **passes 1 and 2 per row; pass 3 as D4 states** (one set-based `DELETE` in one transaction, the re-ingest per row, F13.8). **Do not hand-write the merge and do not swap the arguments**: D4 states why the swap re-keys nothing, and `applyMergeTx` is the one rule both paths share. **The live merge path itself is unchanged** — §2.6 shows no row ever changes session, so only the collision branch is factored. **And its `SessionEvents` doc comment is rewritten** (F11.6): it justifies the missing pagination with "a session's row count is already bounded by the session resolver's own gap window" ([`store.go:325-330`](../../internal/store/store.go#L325-L330)), which D7 makes false — the whole point is that a conversation no longer splits at that gap — so the comment records that **no bound replaces it** |
| `internal/cli/ingest.go` | `newTailer` calls `SetSessionRecorder`, so `clens ingest` / `serve` / `refresh` all record JSONL `sessions` rows; its callers pass the resolver, because `collectorStore` does not carry `UpsertSession`/`ReconcileSession` (D7). **`runIngest` must build one** — the parameter cannot come from `st` (no `UpsertSession`/`ReconcileSession` at [`refresh.go:84-88`](../../internal/cli/refresh.go#L84-L88)), but `runIngest` already holds the `*store.Store` ([`ingest.go:38`](../../internal/cli/ingest.go#L38)), so it constructs `session.New(st, …)` ([`session.go:36`](../../internal/session/session.go#L36)) — and passes it to `newTailer`. `addCollectors` ([`refresh.go:94`](../../internal/cli/refresh.go#L94)) gains the resolver `serve` already builds |
| `internal/cli/serve.go` | the resolver carrier: `serve` already builds the session ([`serve.go:89`](../../internal/cli/serve.go#L89)), so its `addCollectors` call ([`serve.go:133`](../../internal/cli/serve.go#L133)) passes it through (D7) |
| `internal/cli/refresh.go` | `addCollectors` ([`refresh.go:94`](../../internal/cli/refresh.go#L94)) takes the resolver as a parameter and passes it to `newTailer`, and **`runRefresh` must build one** ([`refresh.go:61`](../../internal/cli/refresh.go#L61)): there is no `session.New` in this file — `session.New` has no other **non-test** caller (`serve.go:89` is the only one; the sole other construction is `consumer_test.go:454`) — so without a new one the tailer's `SetSessionRecorder` cannot be wired from this path (D7) |
| `internal/cli/rekey.go` (new) | the command, `--yes` / `--dry-run`, all three passes |
| `internal/cli/rekey_test.go` (new) | §5 |
| `cmd/clens/main.go` | register `rekey` |
| `internal/cli/purge.go` | its doc comment says "This is the one command in the CLI that destroys data" (`:19-20`), and **`rekey` is the second** (F12.9): it becomes the two-cases wording the three docs get, so the file those docs *cite* stops contradicting them |
| `internal/cli/cli_test.go` | add `rekey` to `TestNoCommandPrintsACredential` (~`:413`, currently **14 of the 18** dispatch names — it omits `ingest`, `refresh`, `reconcile` and `serve`) for symmetry: `rekey` prints counts, so its leak surface is nil, which is itself worth asserting. It is **not** added to `TestEveryCarriedOverCommandRunsAgainstATempStore` (~`:73`) — that table is the 10-entry *carried-over* set by design, and a new command is the wrong thing to add to a table named for carried-over commands. Neither table is derived from the `commands` map and **neither enumerates every subcommand**, so `rekey`'s registration is a deliberate edit in the credential table rather than a gap a complete list would have caught. **And its seven `newTailer` call sites gain the resolver argument** (`:600`, `:607`, `:612`, `:635`, `:646`, `:663`, `:683`) — D7 gives `newTailer` a resolver parameter, so without this the package does not compile; the only other `newTailer` callers are [`ingest.go:53`](../../internal/cli/ingest.go#L53) and [`refresh.go:95`](../../internal/cli/refresh.go#L95), both already in this table |
| `docs/context/cli-and-tooling.md` | the `rekey` row; "the one destructive command" becomes two; and `:6`'s "map … of 18 subcommands" becomes 19 |
| `docs/context/storage-schema.md` | its own "the only destructive command" claim at `:151` — same correction as `cli-and-tooling.md`, a different file |
| `docs/context/data-privacy-and-compliance.md` | `:111` also asserts purge is the only command that deletes rows — its definite article ("**The destructive command** defaults to the opposite of destructive") becomes misleading with a second one (F12.10; `:106-107` is the retention table and asserts **no** exclusivity, so it is left alone); three files carry the claim, so all three move together. **And `:111-113` is independently wrong**: it says `--dry-run --yes` "reports and deletes in one pass", citing [internal/cli/purge.go:20](../../internal/cli/purge.go#L20) for it — which says the opposite ("`--dry-run --yes` is simply a dry run"), and so does the code (`:45` refuses only when *neither* flag is set, and `purgeByAge` returns before deleting when `dryRun`). The doc misstates both the behaviour and its own citation; fix it here since the file is already being edited. **This row also carries D7's privacy statement**: `rekey`/D7 add **no new capture and no new retention** — the conversation id is read out of the `req_headers` the consumer already stores verbatim, and re-attribution only rewrites a column from a value already on disk |
| `docs/context/workflows.md` | the merge now fires; the identity rule; and the session rule (D7) — the proxy adopts `x-claude-code-session-id`, so `clens sessions` shows conversations rather than heuristic groups |
| `docs/context/testing-and-quality.md` | `:12`'s "**51 test files, 15,051 lines**" and its re-measurement parenthetical are **one clause**: the parenthetical says the GI-7 re-measure "took the count 50 → 51", so bumping only the first number leaves the sentence contradicting its own provenance and leaves 15,051 stale. So the whole clause moves together — **52 test files**, the new line count, and the provenance sentence naming **this** story's new files (`rekey_test.go`, the added session/parse cases) rather than GI-7 |
| `docs/context/INDEX.md` | its "18-entry dispatch table in `cmd/clens/main.go`" becomes 19; **and `:41`'s "seven genuine forks" becomes nine** (008 for D1 and 009 for D7) — `:114`'s "moved six → seven" is dated history and stays; and `:37`'s "trigger: 51 test files" becomes 52 (the new `internal/cli/rekey_test.go`) |
| `docs/context/decisions/000-index.md` | "**Seven** architectural forks" becomes **nine**, with the new 008 (D1) and 009 (D7) — **twice**, at `:5` and again at `:25` ("the status of all seven"), plus a row for 009 in the table |
| `docs/planning/GI-1-claude-lens-v1.md` | §Cross-source identity: the line-1105 fallback claim (the documented fallback was never built), and test 11(b) never exercised (not falsified) — **scoped**: moot on this install, load-bearing, unverified and silently-failing where an upstream sends `request-id` (D6) |
| `docs/acceptance.md` | test 11(b) — never exercised on this install, therefore moot **here**; kept distinct from tier 1's unguarded risk on an install whose upstream sends the header (D6) |
| `docs/context/decisions/008-…` (new) | the identity decision (D1) |
| `docs/context/decisions/009-…` (new) | the session decision (D7): the proxy adopts the conversation id the request already carries |

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
case the measured population (0 bodies with two ids, §2.3) cannot exclude. **And the session header
source (D7):** `x-claude-code-session-id` populates `Meta.SessionHeader` when `x-clens-session` is
absent, **and `x-clens-session` still wins** when both are present — the operator-override case that
fails if the new source is read first. **And the hash is computed even when a header is present**
(F11.3): `TestExtractMetaPrefixHashNilWhenSessionHeaderPresent` (`meta_test.go:100-110`) asserted nil
and now asserts a **computed** hash — the invariant it was really protecting ("the header wins in
`groupKey`") lives in `session_test.go`'s `TestResolveHeaderOverridesPrefix`. This is what keeps the
two hash-keyed session rules live under D7: `ruleCacheExpiredBetweenTurns` and
`ruleCacheConcurrentWriteRace` `continue` on a nil `PrefixHash`
([`rules.go:416`](../../internal/analyze/rules.go#L416), [`:444`](../../internal/analyze/rules.go#L444)),
and the nil-ing would have made every proxy call skip them silently.

**Unit — `internal/analyze` (D7, F11.4).** `ruleCacheInvalidatedByTools` fires on two **consecutive
proxy rows** in a session whose rows include JSONL rows **interleaved** between them — the adjacency the
interleaving breaks, and the assertion that fails if the rule keeps its pre-filter walk. The JSONL rows
carry no `ReqBody`, so without the filter every consecutive pair has an empty side and the rule fires
**nothing**; with it the pair is adjacent and the finding lands. This is the case §5 otherwise has
nowhere: a green §5 must not be able to pass while the rule silently stops firing.

**And the second consecutive-pair rule, whose degradation is accepted rather than fixed (F13.5).**
`ruleCachePrefixInvalidation` ([`rules.go:293-307`](../../internal/analyze/rules.go#L293-L307)) walks the
same rows pairwise with **no** `ReqBody` guard, so a JSONL row interleaved between two proxy rows
contributes a pair whose cache-write side is zero and the rule returns early — interleaving can only make
it fire **less**, with no code change and no warning. §4 takes **no code change** for it (its rule is the
general pairwise shape, not the `ReqBody`-skipping one), so the case asserts the degradation is **real**
(the mixed session does not fire where the proxy-only rows would) and states it as the accepted cost of
D7's whole-conversation session — the "why acceptable" arm §4's row offers, delivered in §5 where §4 says
it goes.

**Unit — `internal/session` (D7).** `Resolve` returns the header value **as the id** when
`Meta.SessionHeader` is non-empty: two header-carrying calls separated by more than the gap window
return the **same** id (a long conversation no longer splits), and that id is the header verbatim, not
`s_…`. Two header-**less** calls beyond the gap still split, so the gap window is not dead — it is
simply scoped to the calls that have no conversation id. `TestResolveHeaderOverridesPrefix` and
`TestResolveHeaderOnOneCallDoesNotGroup` keep passing with **changed meaning**: they asserted
*distinctness*, and now assert *identity* — so each gains the direct assertion too (the value is the
header, not a minted `s_…`).

**Unit — `internal/cli` tailer wiring (D7).** `runIngest` over a fixture transcript writes a `sessions`
row for the transcript's `sessionId` — the case that fails **today**, where `t.recorder` is nil because
no caller wires `SetSessionRecorder`, and the JSONL half records nothing. `TestIngestRebuildRereadsWithoutDuplicating`
([internal/cli/additions_test.go:54](../../internal/cli/additions_test.go#L54)) already builds the
fixture; assert the `sessions` row beside its row assertions.

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
reconcile is a no-op. Then assert the **survivor's** session's post-run aggregate (tokens / cost /
`model_set`) equals the value the post-merge row set implies, and assert the **deleted row's** session
is **gone** — reconciled to empty and then removed (F15.1), the sibling clause pass 2 carries — so a
run that leaves it is a ghost and a run that would read its aggregate is reading a row the commit must
have deleted; a second run is a no-op, and a run with neither flag refuses.

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

**The collision has two shapes, and the second one is the load-bearing case.** The proxy-vs-proxy
fixture above pins the reconcile, and it is also the shape the measured data says cannot happen (no body
id is shared by two proxy rows, §6). The shape that *will* happen is a **JSONL taker**: a proxy row
whose body id is already held by a `jsonl`-sourced row. Both shapes have **two** sessions, because the
row a pass-1 collision deletes is always a *historical* proxy row carrying a minted `s_…` (D4): the
proxy-vs-proxy fixture seeds that directly, and the JSONL-taker fixture **gets the same two-session
seeding** rather than the shared session an earlier draft gave it — one `proxy:`-keyed row in its **own
minted session** (its own `sessions` row) with a body carrying a `message.id`, and one `jsonl`-sourced
row carrying that same id in the **conversation session** (D7). Seed that, and assert:

- the surviving row is the **taker's** row: `id` unchanged, `session_id` the JSONL seat's conversation
  id (not the proxy row's minted `s_…`), and
  `source_refs` holds **both** sources — asserted **order-agnostically** (as a set, `{proxy, jsonl}`), or
  as the seat's order `["jsonl","proxy"]`, because on this path the survivor is the JSONL **taker** and
  `unionStrings` seeds the union from the `existing` seat first ([`merge.go:148`](../../internal/store/merge.go#L148),
  `unionStrings` `:350-366`), so the literal list `["proxy","jsonl"]` is unsatisfiable here (F13.3). This
  one *does* show in the union — the sides genuinely differ, unlike the proxy-vs-proxy fixture. **And
  assert the seat columns the merge must never touch** (F13.4): the survivor's `source` / `first_source` /
  `started_at` are the **taker's own** values (the JSONL seat's), not the proxy row's — `merged := *existing`
  already decides them and `applyMergeTx` must not special-case them, so an implementer who writes the
  rejected "earlier of the two" rule, or copies `incoming`'s seat columns, goes red. Pin it so it can
  fail: give the fixture's two seats **different** `started_at` values (and a differing `source`) and
  assert the survivor holds the **JSONL seat's** `started_at` / `source` / `first_source`. Both
  `first_source` and `source` are rendered ([`store.go:1261`](../../internal/store/store.go#L1261)) and
  `started_at` orders `clens ls`, so all three are observable;
- **`request_id` on the surviving row is the target key** — the body's `message.id`. It is the taker's
  own key, so the value is **unchanged**, and that is the point: the proxy row is the `incoming` and
  nothing is re-keyed. A **swapped** implementation leaves the proxy row's old `proxy:` value in place
  while every other bullet here still holds, so assert the value directly, and assert it **before** the
  second run — a silent no-op's signature is a first run that changes nothing and a second run that
  works;
- the **proxy** row (the `incoming`) is gone (row count drops by one);
- **a `replay_of` naming the absorbed proxy row's id re-points to the survivor** (F15.2) — the
  collision is the run's **only delete site that can dangle a reference** (§6): pass 3's delete takes
  only `jsonl:`-keyed rows, and no `replay_of` can name one. Seed a third row whose `replay_of` names
  the **deleted proxy row**'s id and assert
  that after `rekey --yes` it names the **survivor** (the JSONL taker's row) — not the deleted id, and
  not cleared. An implementation that resolves the reverse lookup over the wrong id set — pass 3's
  `jsonl:`-prefixed ids, the empty one — stays green
  without this bullet and leaves this reference dangling, which §6 calls a silent failure no CLI
  observable reveals;
- **two** reconciles cover the collision, because the two rows sit in **two** sessions — the survivor's
  conversation session and the deleted proxy row's minted `s_…` (D4). Assert the **survivor's**
  session's post-merge aggregate **and** that the minted `s_…` session is **reconciled to empty and then
  removed** (F15.1's clause, applied to the shape that actually happens): a one-reconcile implementation
  passes the survivor assertion while leaving the emptied `s_…` holding the absorbed row's figures, a
  ghost in `clens sessions`. The proxy-vs-proxy fixture above is the type that cannot happen, and it
  carries the same two-session pinning for the same reason — which is why the reconcile is stated as a
  **set**;
- **pin the token columns so the aggregate assertion can fail**: both sides `capture_complete` with
  **different** token columns, so the winner pick actually moves figures. Without the difference the
  reconcile is a no-op and the bullet passes either way — the same pinning the proxy-vs-proxy fixture
  needs, and omitted here in an earlier pass;
- **a `source_mismatch` warning is attached when the two sides disagree on tokens** — the helper owns
  this decision (D4), so a helper that omits it merges silently, and §6 tells the operator to expect a
  handful of these warnings *from the run*. Nothing else in §5 asserts one, and the live-acceptance line
  is the only other place they appear, so without this bullet the obligation is untested;
- **the winner's empty-mode derivation is pinned** (F10.3): a collision whose picked side has an empty
  `billing_mode` and a non-NULL cost column leaves the survivor's mode matching that column (`api` for
  `cost_usd`, `subscription` for `api_equivalent_cost_usd`) rather than empty — the sub-rule a copy of
  the winner's mode ships blank, dropping the row out of every cost aggregate;
- **the deleted row's warnings survive** (F10.8): the **`incoming`** row — the one the collision deletes —
  carries a warning, the collision removes it, and the survivor still holds that warning, asserted by its
  `event_id` being the **survivor's** (not merely that some warning exists) — the `ON DELETE CASCADE` case
  a hand-written delete drops. Seeding the warning on the **survivor** instead would make this case pass
  with no re-attach code at all, because the delete never touches it: the fixture has to fail when the
  re-attach is missing (F13.2);
- **the two sides agreeing on tokens but differing on `cost_source` / `billing_mode` raise no warning,
  and the survivor ends on the `incoming` seat's values** (F11.2): `tokensDiffer`
  ([`merge.go:150-155`](../../internal/store/merge.go#L150-L155)) is the sole operand of
  `source_mismatch`, so this ordinary pair is **not** flagged. **Assert the state the command leaves**
  (F12.3), not the one it passes through: run the *same* request through **both** paths — a proxy capture
  of the id plus a real transcript naming it — and assert the two survivors' `cost_usd` /
  `api_equivalent_cost_usd` / `cost_source` / `billing_mode` **and the aggregate each joins**
  ([`store.go:708-709`](../../internal/store/store.go#L708-L709)) **agree**, because pass 3's re-ingest
  re-merges with `winner = incoming` — the fresh JSONL row — exactly as the live path does (F12.8). And
  assert the rekey survivor's **`prefix_hash` is non-NULL and `ruleCacheExpiredBetweenTurns` still
  evaluates it** ([`rules.go:416`](../../internal/analyze/rules.go#L416)) — the F12.2 inherited-column
  case a survivor that inherited the JSONL taker's nil would fail;
- a **second** `rekey --yes` changes nothing (this is what a silent no-op first run would violate, so
  it is worth asserting after bullet 2 rather than trusting it).

**The branch that used to be here is gone, and the case still earns its place.** Round 6's version
asserted the survivor is the *proxy* row, because a JSONL survivor landed in an aggregate-less session.
Under D7 the survivor is the taker and that is correct — the survivor's session is the conversation the
request names, whose aggregate pass 2's upsert and pass 3's re-ingest materialise (D4) — so the first
bullet now guards the **identity** (that step 3 uses the ordinary merge and re-keys nothing) rather than
a source branch. A test that only ever seeds
proxy-vs-proxy still cannot see the difference, which is why both shapes are kept.

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
deletes nothing.

- **Pass 3 leaves a reference to a surviving row untouched** (F13.6, F16.1). §6's requirement — no
  `replay_of` names a row that no longer exists — binds the run's delete sites, and the collision site
  is the only one that can dangle a reference: no `replay_of` can name a `jsonl:`-keyed row, because a
  `replay_of` names a **body-carrying** row (§6) and pass 3 deletes only `jsonl:` keys. So on this
  fixture seed a third row whose `replay_of` names a surviving `proxy:`-keyed row; after `rekey --yes`
  assert that row's `replay_of` is **unchanged** — neither cleared nor re-pointed. A rekey that clears a
  live reference, or re-points one, fails this assertion. **And assert the `--dry-run`'s reported number
  is the dangle count** — zero on this fixture, where nothing dangles — **not** the `replay_of <> ''`
  referrer count, which measures a different population (§6); a run that reports the referrer count
  instead passes the "non-zero" reading and fails this one.

- **The re-ingest re-prices, so assert a cost** (F10.7). Wire the fixture's tailer with a **price table**
  (the way `newTailer` does, [ingest.go:78-98](../../internal/cli/ingest.go#L78-L98)) and a transcript
  line that table prices, then assert the collapsed row's `cost_usd` / `api_equivalent_cost_usd` (or at
  least its `cost_source`) equals the table's value after `rekey --yes`. D4 spends three bullets on the
  fact that this half restates ~70k rows' cost against the price table in force *now*, and this is the
  only assertion that distinguishes a re-ingest that re-prices from one that re-writes: a rekey that
  builds its tailer without the price table blanks those columns while every other case here still
  passes.
- **The precondition is stated over the cursors, not over rows** (F10.5, F11.7). The schema has no
  row→file link (D4 step 0), so the check is over **all recorded `jsonl:<path>` cursors** — the
  qualifier "whose path the re-ingest will walk" is **dropped** (F11.7), because the delete is
  unconditional over the `jsonl:` prefix while the walk is root-scoped: a recorded cursor whose file has
  been deleted would have its rows destroyed and never rebuilt, so **a recorded path with no readable
  file is a refusal, not an exclusion**. Seed a `jsonl:`-keyed row **and its cursor**, make the
  transcript file **missing** (or shorter than the recorded cursor), **and seed one `proxy:`-keyed row
  the run could re-key** (its body yields an id), and assert the run **refuses** —
  non-zero exit, **no rows changed**, and checked **before pass 1** so neither pass 1 nor pass 2 has run
  (F13.7) — rather than deleting the row. **The `proxy:` row is what makes the ordering failable** (F14.5):
  a jsonl-only fixture is invisible to passes 1–2 (they touch `source='proxy'` rows and `session_id`
  respectively), so an implementation that checks the precondition *after* pass 1 still satisfies "no rows
  changed" and stays green; asserting the `proxy:` row's `request_id` is **still** `proxy:…` after the
  refusal is the only thing that fails when the check runs late. **And the root case:** seed a recorded cursor whose
  path is **outside** the walk root and assert the run refuses, because `resetJSONLCursors` would not
  re-read that file and the delete would remove a row nothing can rebuild. Without both, the run deletes
  ~71k rows on the strength of a precondition nothing checks (§6).
- **Report the run's wall clock, beside the conversation's row count** (F11.4, F15.3). Pass 3
  re-inserts ~70k rows, and once the recorder is wired (D7) each row costs an `UpsertSession` **plus** a
  full aggregate scan of its session (§6's doubled-reconcile risk). **It does *not* pay the O(n²)
  session-rule pass** (F12.6): `newTailer` wires no `SetSessionRule`, so this case measures the
  re-ingest's own cost, and the pass's real exposure sits on the proxy `flush` path this fixture never
  exercises (§6). The case **reports** the run's wall clock beside the row count **N** it was measured
  over — seconds against N², so a quadratic blow-up is a number rather than "the test got slow". It does
  **not** assert a wall-clock bound: a bound is flaky, and the wiring the case is about can only make the
  run **faster** where it is *absent*, so no wall-clock bound can fail on that absence — the wiring's
  *existence* is gated instead by §5's tailer-wiring case (`:1145-1149`), which fails today because
  `t.recorder` is nil. So this case is an observability measurement, not a gate, and §6 says so.

**Pass 2 gets its own case, because it is the one write path that sets `session_id` on an existing row.**
Seed two `proxy:`-keyed rows whose `req_headers` carry `x-claude-code-session-id` but whose `session_id`
is a minted `s_…` (each with its own `sessions` row), plus one proxy row whose headers **lack** the
header, plus one whose `x-claude-code-session-id` is **overlong** (longer than `maxSessionHeaderLen`, so
pass 2 treats it as absent — F12.7), **plus one whose headers carry both `x-clens-session` and
`x-claude-code-session-id`** (F15.4). Run `rekey --yes`, and assert: the first two rows' `session_id` is
now the header value; **the both-present row's `session_id` is the `x-clens-session` override, not the
conversation id** — the precedence pass 2 mirrors from the live path (D7 change 1), so an
implementation that reads `x-claude-code-session-id` alone is caught; the conversation session exists and
its aggregate equals the post-run row set (the
**upsert** case — nothing created it beforehand); the emptied `s_…` `sessions` rows (the two
conversation-id rows' own, and the both-present row's) are **gone**
(reconciled to empty, then removed) so `clens sessions` shows no ghosts; and **both** the header-less row
and the overlong-header row keep their minted id and are counted in **L** — the overlong row is an **L**
row, not re-attributed (F14.9). A `--dry-run` reports the K/L pair without writing.

**Integration.** A JSONL line with no `requestId` and a proxy capture of the same request, ingested
through the real paths, produce **one** row — the end-to-end statement of the whole story, and the one
that fails today. **Assert the row's `session_id`, not the row count** (F11.9): the count alone passes
if the two rows unified under the **wrong** id — the parent's where the subagent's was meant, or the
reverse — and the id is what D7 changes and what §2.6's 184/207/3 arithmetic rests on. So the row's
`session_id` equals the conversation id **both** sources name, and the **subagent-sidechain variant** is
added, because that is the case where parent and child ids are not interchangeable: a
`…/<sessionId>/subagents/agent-*.jsonl` line takes the **parent's** `sessionId`
([`jsonlogs.go:380-387`](../../internal/jsonlogs/jsonlogs.go#L380-L387)), so a sidechain line merged with
its parent's proxy capture must land in the **parent** conversation — pinning the parent/child mapping
the arithmetic depends on.

**And the accepted split, which nothing exercises today.** A seeded JSONL line that carries a
`requestId` (tier 1) paired with a proxy capture whose body yields the same `message.id` but whose
stored `resp_headers` hold **no** `request-id` (tier 2) must leave **two** rows: they key on different
values and do not meet. That is D1's named exception — the proxy capture failed to record the header —
asserted so it is visible rather than silent. **But the split alone is not enough (F10.4)**: it asserts
the *exception*, and a suite that only ever exercises the exception cannot fail if the header tier
silently stops *meeting* — which is the behaviour D1's own argument for that tier protects (demoting it
would take the Anthropic case "from working to broken", D1). So the **meeting** case needs its own
fixture, and it is the only shape that fails if the tier stops meeting: a proxy capture whose
`resp_headers` carry `request-id: X` and whose body carries a **different** id, paired with a JSONL
line whose `requestId` is `X`, must produce **one** row whose `request_id` is `X` — the header value,
not the body id — and a later re-key must leave that row alone (the `proxy:`-prefix predicate, F9.12).
The tier-2/tier-2 meeting (both sides on the body id) is what the JSONL / consumer fixtures above
already cover.

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
  transcript is the same failure worse. So the run checks **every recorded `jsonl:<path>` cursor** — the
  file exists and its current size is not below the recorded cursor — **first, before pass 1** (F13.7),
  and a failure is a **refusal with a non-zero exit and no rows changed**, not a "skip and report" that
  exits 0 and reads as done: the check is over cursors pass 1 does not touch, so it is valid before any
  mutation. The one residual case is a **mid-run race** — a file becoming unreadable between the check and
  pass 3 — which leaves passes 1–2 committed; that race, not a refusal, is F12.3's permanent intermediate
  state (F13.7). It is also a refusal when a recorded cursor's path falls **outside** the
  walk root, where the delete would remove rows the walk can never rebuild (D4 step 0). **The qualifier
  "whose path the re-ingest will walk" is dropped** (F11.7): the delete is unconditional over the
  `jsonl:` prefix while the walk is root-scoped, so a recorded path with **no readable file is a
  refusal, not an exclusion** — otherwise the rows of a cursor whose file is gone are destroyed and
  cannot be re-created. It is a **checked precondition of the run**, not a row→file mapping the
  schema cannot supply; §5 seeds a missing/short file **and** a root-change and asserts both refusals.
  And the command should be run with `clens serve` stopped, because
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
- **The collision path has two ways to be written that fail silently, and D7 removes the reason either
  was ever plausible.** The first is a call with its arguments in the **wrong seat**, or a helper that
  **loads by the incoming row's own key**: `mergeEvents` never assigns `RequestID`
  (`merged := *existing`, [`merge.go:147`](../../internal/store/merge.go#L147)). These are **two
  mechanisms with two different consequences** (F13.10), stated apart because fusing them is wrong for
  one of the two: a **swapped** call writes the old synthetic key straight back, so the message id ends
  up **held by nobody** — the run reports success and a *second* run then "works", because the key is
  free by then — so the failure looks like the promised no-op and its own correction looks like the first
  run, and §5's survivor-`request_id` assertion catches it. A helper **loading `incoming.RequestID`**
  finds the proxy row under its own `proxy:` key, merges it into **itself** and lets the caller delete
  it — the taker **still holds the message id, so nothing is orphaned**, and because no key is wrong a
  second run changes nothing: what is lost is the proxy row's **content** (its token and cost columns,
  the `source_refs` union, its `prefix_hash` and replay linkage), permanently — and §5's `source_refs` /
  row-count assertion is what catches **that** shape, the `request_id` assertion being **blind** to it.
  The second is reading "merge the two rows" as a content **union**: `mergeEvents` picks a **winner** for
  the measurement columns (`merge.go:166-206`) and backfills the rest per column, unioning only
  `source_refs`, so a union produces a row no live merge could produce — and unlike the first, it changes
  figures rather than keys, while leaving every key and session assertion in §5 green. Neither is
  reachable through D4's mechanism (the taker loaded **by the target key**, the merge–write–warn
  sequence in one factored `applyMergeTx`), but each is one hand-written line away, which is why
  **br-GI-9-04 still carries D4's named trap**, and why §5 asserts the surviving row's `request_id`, the
  `source_mismatch` warning, the empty-mode derivation and the deleted row's warnings. Named here because
  these are the defects in this story that no CLI observable would reveal.
- **No `replay_of` may name a row that no longer exists.** `replay_of` is a TEXT reference to an events id
  rendered in decimal ([`proxy.go:213-220`](../../internal/proxy/proxy.go#L213-L220)), and **the run's
  collision path is the only delete site that can dangle a reference** (F16.1). **A `replay_of` can only
  name a body-carrying row**: it is written only from the replay route's `ReplayMeta.Of`
  ([`proxy.go:133-136`](../../internal/proxy/proxy.go#L133-L136), `:252` →
  [`consumer.go:430`](../../internal/consumer/consumer.go#L430)), and that route refuses an original with
  no stored body ([`internal/api/replay.go:86-88`](../../internal/api/replay.go#L86-L88)). No
  `jsonl:`-keyed row carries one — `internal/jsonlogs` assigns no `ReqBody`/`RespBody` — and a merge
  cannot put one there, because the taker's key must equal the incoming's and no proxy key is
  `jsonl:`-prefixed. So **pass 3's bulk delete of the `request_id LIKE 'jsonl:%'` rows cannot dangle a
  reference**; the collision path's delete of the absorbed `incoming` row can. A row whose `replay_of`
  names a deleted id
  becomes a reference the replay lookup
  ([`internal/api/replay.go:224`](../../internal/api/replay.go#L224), `EventFilter.ReplayOf`) can no
  longer resolve — `clens show` prints a replay-of with no target, and no error is raised. **The
  requirement is that no `replay_of` names a row that no longer exists**, and where a deleted id *is*
  referenced the run **re-points the reference to the survivor**: a replay's original still logically
  exists as the merged survivor, so the lineage is preserved rather than lost (the row keeps
  `replay_edits`). The reverse lookup is **unindexed** — `replay_of` carries no index
  ([`schema.sql:68-70`](../../internal/store/schema.sql#L68-L70)) and the filter is `replay_of = ?`
  ([`store.go:500-505`](../../internal/store/store.go#L500-L505)) — so it runs **once per run over the set
  of deleted ids**, never per row; against pass 3's ~71k deletions a per-row resolve would be one
  unindexed scan per delete. **`--dry-run` reports the dangling count** — rows whose `replay_of` names an
  id that does not exist — **not** the referrer count `replay_of <> ''`, which measures something else and
  is the wrong number. The number itself is still pending: this plan's pass had no shell and could not run
  the query, so it belongs in this paragraph once taken.
- **Wiring the recorder doubles the reconcile cost on the JSONL path (D7).** `InsertEvent` already
  reconciles the surviving session on a merge (`store.go:275`); `RecordCall`
  ([`session.go:91-106`](../../internal/session/session.go#L91-L106)) adds an `UpsertSession` **plus a
  second** `ReconcileSession` for **every** row the tailer writes. Pass 3 re-inserts ~70k rows, each
  triggering a full aggregate scan of its session, so this doubles a cost that half already pays — and
  unlike the unwired state, it now pays it on every ordinary `clens ingest` too. §5's re-ingest case
  **reports the wall clock** (it does not assert a bound — F15.3) so a quadratic blow-up is a number, not
  a mystery.
- **The session-scoped analyzer pass is O(n²), and D7 makes the whole conversation its unit (F11.4).**
  `runSessionRule` loads every row of the session via `SessionEvents`
  ([`store.go:325-330`](../../internal/store/store.go#L325-L330)) — bodies included, 256 KB cap each, no
  `LIMIT` — and `ruleCacheConcurrentWriteRace`
  ([`rules.go:441-472`](../../internal/analyze/rules.go#L441-L472)) has an inner loop over the rows
  before it, so one pass costs O(n) to load and O(n²) to compare. **It is not pre-existing on the JSONL
  path** (F12.6): `serve` is `SetSessionRule`'s only caller
  ([`serve.go:100`](../../internal/cli/serve.go#L100)) and `newTailer` wires no session rule
  ([`ingest.go:78-99`](../../internal/cli/ingest.go#L78-L99)), so the tailer's nil-guard
  ([`jsonlogs.go:523`](../../internal/jsonlogs/jsonlogs.go#L523)) never fires and no JSONL write pays it
  today. The exposure D7 creates is on the **proxy `flush` path**
  ([`consumer.go:216-217`](../../internal/consumer/consumer.go#L216-L217)), which runs the pass on every
  insert and where N is now the whole conversation — **and it is not the path §5's pass-3 wall-clock
  case exercises**, so that case bounds only the re-ingest. The row-set filter above is the
  **behaviour** fix, not the cost fix, and it must not ship as an adjective: if N is unacceptable the fix
  is a bound on `SessionEvents` (a `LIMIT` or window) or a rule-specific row set, a decision left to the
  bead rather than taken here.
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
- **A conversation's cost splits across the `api` and `subscription` aggregates with the surviving seat
  (F13.11).** `billing_mode` is winner-picked and the two sources derive it by structurally different
  rules (D4), so after this backfill a prefix-api model called with an oauth credential is `subscription`
  on one seat and `api` on the other — and the mode is what selects the aggregate the row joins
  ([`store.go:708-709`](../../internal/store/store.go#L708-L709)), so one conversation's money lands in
  both totals depending on which seat survived. **Nothing in this story closes that**: D4 names
  reconciling the two derivations as a different story, and it is recorded here rather than fixed. The
  per-row invariant still holds — mode and cost come from the same `winner`, so the row never contradicts
  itself — what moves is which total it lands in.
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
  inside the session it already belonged to; no row changes session and no count moves (§2.6). The
  merged rows are the 511 the proxy already stored under a synthetic key (§1) and whose body yields an
  id — visible in their proxy session today, not rows that existed only as JSONL. What *does* move a
  session's shape is D4 itself: Pass 2 **re-attributes** history and the rekey **collision** deletes a
  row, so both are user-visible and belong in the PR body and the refresh.
- **The story's most visible change is the Sessions view becoming the conversation — and it grows as
  well as shrinks.** Today `clens sessions` is **proxy-only** and shows the proxy's **184** heuristic
  `s_…` sessions; after D7 those collapse to the **3** real conversations the request headers already
  name (§2.6), a long conversation **stops splitting at the inactivity gap**, and the **~3%** of requests
  that carry no `x-claude-code-session-id` fall back to today's gap-window behaviour rather than being
  dropped. **The same wiring that collapses the 184 writes `sessions` rows for the JSONL half for the
  first time** (F11.8): §2.6 measures **0 of 207** JSONL sessions with one, so the post-change view is
  **not 3** — it is the union, roughly **207**, because the JSONL sessions the recorder now materialises
  **grow** the view by about what the collapse removes. A reader (and the PR body, which §5's live
  acceptance says must carry the number) who reports "184 → 3" will then watch the operator see ~207, so
  the honest line carries **both**. **A fifth change belongs beside these** (F11.5): `x-clens-session`
  stops being **grouping-only** and becomes the stored **id**, so a client that sets it to an arbitrary
  label now sees that label as the `clens sessions` id — the header whose documented meaning changes,
  and the one the visible-change list originally omitted. `Resolver.Resolve` keeps its shape, so no
  caller changes. Say all of these in the PR body and the refresh — a reader watching `clens sessions`
  will otherwise read the collapse as data loss.
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
database. **The session change adds no new capture and no new retention either** (D7): the header it
promotes is already stored verbatim on every proxy row — the consumer writes the whole header map, and
`redactHeaders` neither lists `x-claude-code-session-id` nor mutates the original — so the change reads
a value already on disk and uses it as a key, and the backfill's re-attribution (D4 Pass 2) rewrites
`session_id` from those same stored headers. Nothing new is kept, nothing new is sent, and no credential
is involved. The rekey command *does* delete, which is why it takes purge's contract rather than a new
one, and no header is newly captured (the request-id header is already read). The one thing to hold: the
command must refuse without `--yes`, and `--dry-run` must not delete as a side effect of measuring.

## 8. Out of scope

**Two items that were out of scope in earlier rounds are now in scope, and this story ends them.** Both
were deferred on the belief that they were larger than they are; measuring §2.6 showed the reason neither
is, so they moved into D7 rather than staying here. They are listed first, struck as closed, so a reader
of an earlier round can find where they went.

- ~~**Changing which session owns a merged row.**~~ **Closed by D7, and narrowed to what was actually
  deferred.** D3's **merge** rule is unchanged and still out of scope: a merge keeps the session the row
  was first written under, `mergeEvents` never rewrites `session_id` (D5), and no merge moves a row
  between sessions. What D7 does is upstream of the merge — the proxy writes the conversation's own id
  (`x-claude-code-session-id`) instead of minting `s_…`, so the two writers produce the *same* id and
  meet without any rewrite. The reason the deferred version read as small is that the id was never
  missing, it was **discarded**: it arrives in **1,364** proxy request headers with **3** distinct values,
  all **3** real JSONL `sessionId`s, and **0** of them reach `events.session_id` (§2.6). Storing the
  header as the session key is very nearly the whole of it — so it is done here, and only the
  never-rewrite invariant itself stays deferred.
- ~~**`clens sessions` and the dashboard's Sessions tab are proxy-only today.**~~ **Closed by D7.** The
  reason a JSONL session had no `sessions` row was that `SetSessionRecorder`
  ([internal/jsonlogs/jsonlogs.go:143](../../internal/jsonlogs/jsonlogs.go#L143)) had **no caller**, not
  that the JSONL side could not supply one (§2.6). Wiring it in the tailer gives every JSONL session an
  aggregate, so a JSONL-only install stops showing an empty Sessions view; the dashboard's Sessions tab
  reads the same table and follows for free. **That is also why the post-change view is the union, not
  the 3 conversations** (F11.8): the ~**207** JSONL sessions that had no row now get one, so the view
  **grows** by about the count the proxy collapse removes — 184 heuristic sessions fold into the 3
  conversations *and* ~207 JSONL sessions appear, and the PR body carries both numbers. The cost of the
  wiring is a second reconcile per row and is carried as a named risk in §6.
- **Recovering an identity for `--body-policy off` rows.** The proxy did not look, so there is nothing
  to recover.
- **Any read-time dedup view** (D5).
- **Changing what either source captures**, including promoting `x-ds-trace-id`.
- **The dashboard's filters.** No new column means no new filter; `source`/`billing_mode` filters on
  the Stats tab remain the separate, already-offered item.

## 9. Bead sketch (Phase 3 formalises this)

| # | Bead | Depends on |
|---|---|---|
| 01 | `parse`: `Usage.MessageID` from both response shapes, with the non-stream `type` guard | — |
| 02 | `consumer`: identity precedence in one function; the synthetic fallback moves here | 01 |
| 03 | `proxy`: `captureState.requestID` deleted; `submit` passes the header value | 02 |
| 04 | `jsonlogs`: `message.id` as the middle tier of `requestKey` | — |
| 05 | `cli` + `store`: `clens rekey` — three passes, `--yes` / `--dry-run`, and the collision path as the factored `applyMergeTx` (a requirement list plus a named trap, §9's note, D4) | 02, 04, 08 |
| 06 | docs: reconcile test 11(b), the merge flow, the CLI table, the three other "only destructive command" claims, the purge `--dry-run --yes` misstatement, the no-new-capture privacy statement, and add decisions 008 and 009 | 01–05, 08 |
| 07 | live acceptance re-run and the recorded manual run (the §2/§6 decoded-body figures are marked **re-confirmable**, not independently reproduced) | 05 |
| 08 | `parse` + `session` + `cli` + `store`: the conversation id the request already names — `x-claude-code-session-id` as a second source for `Meta.SessionHeader`, `Resolve` returns it, the tailer wires `SetSessionRecorder`, and pass 2 re-attributes history (D7) | 01 |

**The beads exist now** (`.beads/GI-9/`), and this sketch's numbers are not theirs: **br-GI-9-01**
(parse), **br-GI-9-02** (consumer + proxy — this sketch's 02 and 03, folded into one atomic change),
**br-GI-9-03** (jsonlogs, this sketch's 04), **br-GI-9-04** (`clens rekey` — this sketch's bead 05),
**br-GI-9-05** (docs) and **br-GI-9-06** (live acceptance). D4, §4 and §6 therefore refer to the
destructive bead by its real id, **br-GI-9-04**. **The session-attribution work (this sketch's bead 08)
has no bead under `.beads/GI-9/`** — it is new to this plan by the human's ruling (D7) and is the one
piece of §3 with nowhere to land yet; it needs its own bead, and §4's rows are its file list.

**br-GI-9-04 is the largest and the only destructive one.** The forward-fix beads (br-GI-9-01…03) must
land before it runs anywhere but a dry run. There is no store bead for the **merge path**: §2.6 and D3
show it needs no change. The store methods br-GI-9-04 adds are the rekey scan, the re-key, and the
collision path's helper.

**br-GI-9-04 carries a requirement and a named trap rather than a procedure, deliberately.** After
round 11 the requirement is **factoring the merge, never the load**: `insertOrMerge`'s collision branch
([`merge.go:112-131`](../../internal/store/merge.go#L112-L131)) becomes an unexported **`applyMergeTx`**
— `mergeEvents` → `updateEventTx` → `source_mismatch`, **minus the load** — called by **both** callers,
each keeping its own load, so the *what* is "there is one merge rule and the rekey path uses it" and
**D4 carries the full obligation list**. The *how* stays unspecified because five plan-level rounds each
found a defect one level deeper in this one paragraph; round 10 dissolved the last of them, so the trap
below is a **warning against reintroducing** a hand-written merge, not a description of the design:

> **Factor the helper; do not hand-write a merge, and do not call one with the arguments in the other
> seat.** Call `applyMergeTx` with the proxy row as the `incoming` and the taker — **loaded by the target
> key** — as `existing`, because it already holds the target key, and **nothing is re-keyed**. The load
> is the caller's; the helper performs none. Both shapes below compile, read
> correctly, and fail silently, so a hand-written version is a regression even when its assertions pass:
>
> - **The swapped call.** `mergeEvents` never assigns `RequestID` (`merged := *existing`,
>   [`merge.go:147`](../../internal/store/merge.go#L147)), so a call that puts the proxy row in the
>   `existing` seat returns the proxy row's **old synthetic key**, `updateEventTx` writes that key
>   straight back (it binds `request_id` first), and deleting the taker then leaves no row holding the
>   message id. The run reports success and a second run then "works" — the failure looks like the
>   promised no-op and its own correction looks like the first run. With the taker as `existing` the
>   message id is never in question, so this is unreachable *by construction*, which is the argument for
>   factoring rather than describing. **The survivor's `request_id` assertion catches this shape.**
> - **A helper that loads by `incoming.RequestID`, which is the other mechanism and a different
>   failure.** It finds the proxy row under its own `proxy:` key, merges it into **itself** and lets the
>   caller delete it — the taker is untouched and **still holds the message id**, so nothing is orphaned;
>   what is lost is the proxy row's **content** (its token and cost columns, the `source_refs` union, its
>   `prefix_hash` and replay linkage), and because **no key is wrong** a second run changes nothing — the
>   failure is *worse* than the first shape and the `request_id` assertion is **blind** to it.
>   **§5's `source_refs` / row-count assertion is what catches this one.**
> - **The union.** "Merge the two rows" read as a content union produces a row no live merge could
>   produce. `mergeEvents` is a **winner pick plus a per-column backfill**: the six token columns,
>   `cost_usd` / `api_equivalent_cost_usd` / `cost_source`, `stop_reason` / `stop_category`,
>   `service_tier`, `speed`, `model_resolved` and `billing_mode` all come from **one** side
>   ([`merge.go:166-206`](../../internal/store/merge.go#L166-L206)), `capture_complete` is the OR
>   (`:223`), the structurally-impossible columns are backfilled (`:229-326`), and **`source_refs` is
>   the only column that is actually unioned**. The helper inherits that rule unchanged, so this is true
>   by construction too; the warning stands against hand-writing it.
>
> **The winner pick is a three-part sequence, and the helper inherits all of it** because it **is**
> `mergeEvents` rather than a copy: the complete-frame switch (two complete captures → `incoming`,
> [`merge.go:167-180`](../../internal/store/merge.go#L167-L180)), the never-observed-usage override
> ([`merge.go:196-206`](../../internal/store/merge.go#L196-L206)), and the empty-`billing_mode`
> derivation ([`merge.go:272-288`](../../internal/store/merge.go#L272-L288)) — D4's sub-rules, which a
> paraphrase drops and a stop after (1) or (2) ships as a defect.

**§4 and D4/D7 supersede br-GI-9-04 where the bead disagrees** — three items, each corrected when the
bead is rebuilt:

- br-GI-9-04's note tells the implementer to add `rekey` to **both** hand-maintained `cli_test.go`
  tables; §4's `cli_test.go` row is the corrected instruction (the **credential-subcommand** table, not
  the carried-over one).
- br-GI-9-04's **ordering section** (`:31-47`) still carries the **pre-D7** design — "the ordering rule …
  must not be flattened", "step 3 branches on the taker's source", and "only proxy sessions have [a
  materialized aggregate]". D7 deletes that premise: the survivor is the **taker**, there is **no
  taker-source branch**, and the ordering no longer decides the end state (D4). Superseded by D4/D7.
- br-GI-9-04's helper contract (`:102`) requires "the surviving row is the **proxy row's `id` and
  `session_id`**". That is **unimplementable through `mergeEvents`** — `merged := *existing` never
  assigns `RequestID` and the id and the session come from the same seat — and it is exactly the
  **swapped call** this section forbids. The survivor is the **taker's** row, by its `id` and
  `session_id`; the requirement is superseded by D4.

Beads are written from this plan, not the reverse, so **the plan wins** and the bead's wording is
corrected when it is rebuilt.

**The session-attribution work (D7) has no bead under `.beads/GI-9/` yet.** It is new to this plan by the
human's ruling and touches `internal/parse/meta.go`, `internal/session/session.go`, `internal/cli`
(the tailer wiring) and `internal/store` (pass 2), so it needs its **own** bead before implementation —
and it **must land before the backfill runs**, because D4's pass 2 assumes the forward rule D7 puts in
place and pass 3's re-ingest writes ids the old resolver would mint differently.

The implementer's own cross-review (Phase 5.5) is where the factoring gets validated, against the real
`merge.go` rather than against a paragraph about it — the check is that `insertOrMerge` and the rekey path
call **one** function, and that the function is `insertOrMerge`'s collision branch **moved**, not copied.

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
| 2026-09-20 | Round 9 revision (round-9 findings F9.1–F9.19, plus one coordinator-added finding; plan v10). **F9.3/F9.4 (RESOLVED BY MEASUREMENT, applied as directives)** — the proxy stores no `request-id` in **either** direction (0 of 728 responses, **0 of ~1,400** request headers), and **2,807 of 2,807** transcript lines matching a stored proxy body id carry no `requestId`; so `requestId` is present **exactly when the request did not traverse this proxy**. §1 and §2.2 rewritten: the mixed session runs two models against **two endpoints**, and "through the one proxy" is named as the reading the census falsifies; §2.2 records both F9.4 checks as **run**. **F9.7** — D1 keeps the header tier **first** (it is the only tier that can key a body with no id, 65 of 728) and gains the **meeting condition** (the tier is a property of the upstream response, so both sources land on the same tier by construction), the **accepted exception** (a proxy capture with no headers stored stays on tier 2 while a tier-1 transcript does not merge — recorded, not fixed), and the 16,804-row status; §5 gains the tier-pairing fixture asserting the split. **F9.10–F9.13** — D4 keeps its requirement list and **gains** the derived-column obligation (`total_prompt_tokens` re-derived from the survivor's four prompt columns in the same write; §5 asserts it on the row), the third winner-pick rule (the never-observed-usage override, `merge.go:196-206`; §5 asserts an all-zero picked side), the synthetic predicate (`request_id LIKE 'proxy:%'`, with why the prefix rather than "the body yields a different id"; §5 asserts a header-keyed row is unchanged), and the decode limit (config `BodyCapBytes`, with the encoded-body failure mode; §5 asserts gzip re-keyed and an invalid/missing-encoding row **counted in M**). D4 states in the document that the sequencing and field-copy mechanics are **deliberately not specified further**, and why. **F9.12's** predicate and **F9.11's** override also reach §4's `store.go` row and §9's note. **Coordinator-added finding (F9.20)** — `x-claude-code-session-id` arrives in **1,364** proxy request headers with **3** distinct values, all 3 real JSONL `sessionId`s, and **0** reach `events.session_id`: the "0 of 184 overlap / only proxy sessions have an aggregate" facts are a consequence of the proxy **discarding** the true conversation id, not a property of the data, so §2.6, D3, D4 and §8 now say "**as the proxy is currently written**", and §8's split-out story is named as **much smaller than it reads as**, with the header named. **The rest** — §2.3's reproducibility method (population, decode, precedence, scan, the ad-hoc extractor caveat) and its counted residue (N/M/K) (F9.1, F9.2); D2's first-seen precedence with its two-`message_start` fixture and the §6/§5 restatement of what the `type` guard actually buys (F9.2, F9.8); §2.4's floor framing (F9.6); D2's sketch field renamed `RequestIDHeader` (F9.9); the 152-id drawer now a **measured** result — re-measured at **303** ids with **0** mismatches, all true absentees, and the discriminator's definition recorded — superseding this round's first attempt to hand the test to the live-acceptance bead (F9.5); §6's re-derivability turned into a **checked precondition** of the run with a §5 refusal case (F9.14); §4's `cli_test.go` row corrected (14 of 18, and `rekey` joins the credential table, not the carried-over one) (F9.15); §6's retry bullet corrected (`message.id` stability is unmeasured) with a §5 same-body-id case (F9.16); §5's dry-run asserts the **exact N/M pair** (F9.17); §6's "`mergeEvents` unions" replaced with the winner-pick/backfill rule (F9.18, also fixed in D4's JSONL-taker bullet and §9's note); D4's `--dry-run` "nothing else" relaxed to "the counts plus the re-pricing/re-derivability notes; no row-level output" (F9.19). **Bead naming** — the plan's §9 sketch numbers are mapped to the real beads under `.beads/GI-9/`, and the destructive bead is referred to as **br-GI-9-04** throughout (the coordinator's directive named it; the sketch had called it bead 05). **Addendum (same round-9 apply, folded into this row — no v11).** F9.5 was resolved by the coordinator's live-DB measurement: **303** proxy ids with no transcript counterpart, **0** interval+token matches under a **different** id, **303** true absentees, **92** proxy rows yielding no id — so §2.3 states the **result** (with the discriminator's definition) rather than naming a runner, §5's drawer check becomes a re-confirmation, and §6's drawer bullet cross-references it. The premise axis' three MAJORs now read as **RESOLVED**, not as caveats: F9.3/F9.4 as already applied and F9.5 by measurement. F9.1's method paragraph now carries the **query** and the **ratio's stability** — this plan's pass 511 of 663 = 77% against the coordinator's re-measure **947 of 1,203 = 79%** on a corpus **78% larger** — stated as **perishable absolutes over a stable relation**, and §5(1)'s acceptance target moves from a frozen count to that ratio. F9.2 stands as fixed (the first-seen precedence and the counted N/M/K residue). §2.4 now carries the two percentages (**≈32%** against **≈71%**) so the "bound, not agree" framing is explicit (F9.6). §2's preamble states plainly that the premise survived an adversarial round **and** an independent re-measurement with a **stricter extractor**, with the story's own defect's hiding place tested and empty. |
| 2026-09-20 | Round 10 revision (round-10 findings F10.1–F10.12, plus the session-attribution design added by the human's ruling; plan v11). **Part 1 — the round-10 findings.** **F10.1 (MAJOR)** — §2.1/§2.2 read the measured `requestId` leg as a biconditional; it is **one-way**: a line carries a `requestId` when its response came from a header-sending upstream, and the requests that traverse this proxy do not, so presence and proxying are inverse **on this install**. Exactly three places changed (§2.1's heading, §2.2's "precisely when", §1's clause); no figure moved and §2.3's counterexample stays in the same paragraph. **F10.2** — the drawer's 303-id pass labelled as its own later, larger-corpus re-measure, not a complement of the 947-of-1,203 census. **F10.4** — §5 gains the **tier-1/tier-1 meeting fixture** (a proxy row keyed `request-id: X` plus a JSONL line whose `requestId` is `X` → one row keyed `X`), alongside the accepted-split fixture. **F10.5** — D4 step 0's precondition restated over the recorded `jsonl:<path>` cursors (there is no row→file link), the delete scoped to what the walk can reproduce, and the root-change case made a **refusal**; §6 and §5 follow. **F10.7** — §5 asserts a **cost** after `rekey --yes`, because the re-ingest re-prices. **F10.9** — D6's "moot" scoped to this install, with the load-bearing, unverified case on a `request-id`-sending upstream named. **F10.10** — §4's `testing-and-quality.md` row updated as one clause (the counts and the re-measurement parenthetical together). **F10.11** — §9's trap states the full winner-pick **sequence** (complete-frame switch, never-observed override, empty-mode derivation). **F10.12** — §9 records that §4 supersedes br-GI-9-04's two-tables wording. **F10.3/F10.6/F10.8/F10.11** folded into the rewritten D4/§9 rather than into the text they replace. **Part 2 — the session-attribution design (D7, new; the human's ruling).** The proxy already receives `x-claude-code-session-id` (**1,364** of ~1,400 request headers, **3** distinct values, all **3** real JSONL `sessionId`s) and stores it verbatim in every row's `req_headers`; what it does not do is promote it. D7 adds a second source for `Meta.SessionHeader` in `ExtractMeta` (`x-clens-session` still wins), makes `Resolve` return it as the id ahead of the `seen`-map, and **wires `SetSessionRecorder`** — it had **no caller**, which is why the JSONL half recorded no `sessions` rows, the real reason for §2.6's "0 of 207". Consequences: D4's step-3 source branch is **gone** (both rows share one session, so the taker is the survivor whichever side it is and **nothing is re-keyed**); the collision path calls `insertOrMerge`'s factored collision branch, now **`mergeIntoExistingTx`**, so there is **one** merge rule for both callers and F7.1/F8.2/F9.10 **dissolve** into warnings against a hand-written merge; the reconcile set is "the distinct sessions among the two rows", usually one and **never** claimed to be always one; D4's ordering argument is **dropped** (the sequence is kept for determinism and reporting only); a third pass (**re-attribution**) rewrites historical proxy rows' `session_id` from their own stored `req_headers` by direct `UPDATE` — the **one** write path that sets `session_id` on an existing row, with `mergeEvents` still never rewriting it (D3) — upserting the new session and removing the emptied `s_…` ones; §2.6, D3, D4 and §8 keep "as the proxy is currently written" where it still holds and drop it where D7 changes behaviour; §6, §7 and §8 record the **visible** change (**184** heuristic `s_…` sessions → the **3** real conversations, gap-splitting stops, the **~3%** without the header fall back) and the **no new capture, no new retention** statement; §4/§5 gain the parse, session, cli and store rows plus the fixtures, and §9 gains the missing bead (the session work has **no bead** under `.beads/GI-9/`). Two new risks recorded: the recorder wiring **doubles** the JSONL reconcile cost (§5 bounds or reports wall-clock), and a `replay_of` reference can **dangle** after a delete (its count is to be measured on the live DB — this pass had **no shell** and could not, so it is recorded as a measurement the run must take). **Change History** — the previous row was mis-titled "Round 10" and was the round-9 apply; renamed to **Round 9**, and this is the round-10 row's text. **11 rows total.** |
| 2026-09-20 | Round 11 revision (round-11 findings F11.1–F11.11; **plan v12**). **F11.1 (BLOCKER, applied with the human's correction)** — round 10's factoring moved the *load* into the helper, and the load is the one thing the two callers cannot share: a helper that loads by `incoming.RequestID` finds the proxy row under its own `proxy:` key, merges it into **itself**, writes that back and lets the caller delete it — silent row loss on every collision. The collision branch is now factored as **`applyMergeTx(ctx, tx, existing, incoming *Event)`** = `mergeEvents` → `updateEventTx` → `source_mismatch`, **minus the load**: `insertOrMerge` keeps `getEventByRequestIDTx(ctx, tx, ev.RequestID)` and calls `applyMergeTx(existing, ev)`, and the rekey path loads **by the target key** (`getEventByRequestIDTx(ctx, tx, targetKey)`), calls `applyMergeTx(taker, proxyRow)`, then deletes the proxy row **by its own id** and reconciles. The contradiction is deleted (D4's "loads the row holding `incoming.RequestID`" replaced by the target-key contract) and the missing sentence is added and **wrapped into F7.1's warning**: the survivor is loaded by the target key, so it already holds it, and `mergeEvents` never assigns `RequestID` — the same trap seen from the other side. §4's `merge.go`/`store.go` rows and §9's note and trap name `applyMergeTx`; the round-10 row's `mergeIntoExistingTx` is its historical text and reads as the renamed helper. **F11.2 (MAJOR)** — "so it is visible, not silent" is deleted as false: the live path's winner is the JSONL row and the rekey path's is the proxy row, so a pair agreeing on tokens while differing on cost/label columns takes **opposite column sets** and raises **no** flag — and that is the **ordinary** case. D4 now states the divergence honestly (both outcomes self-consistent, neither sums, the difference bounded by the pricing-vintage divergence D4 already accepts), and §5 pins it with a tokens-agree / `cost_source`+`billing_mode`-differ case. **F11.3 (MAJOR, a fix not a decision)** — D7 change 4: `ExtractMeta` **always computes `PrefixHash`** (the nil-ing would silently kill the two hash-keyed session rules once D7 sets `SessionHeader` on every proxy call); `groupKey` keys on the header first, so grouping is unchanged and `RecordCall` gains a real `prefix_hash`; the two "NULL-ness is load-bearing" comments ([`parse/types.go:74`](../../internal/parse/types.go#L74), [`store/types.go:63`](../../internal/store/types.go#L63)) are corrected and `internal/store/types.go` joins §4; `TestExtractMetaPrefixHashNilWhenSessionHeaderPresent` **changes its invariant** rather than being deleted. **F11.4 (MAJOR)** — `ruleCacheInvalidatedByTools` filters its row set to rows carrying a request body before the pair walk (D7's whole-conversation session would otherwise interleave JSONL rows and break the adjacency **silently**); the O(n²) pass cost is recorded as a §6 risk and §5's re-ingest case reports the wall clock **beside the row count N**; `internal/analyze/rules.go` and `rules_test.go` join §4. **F11.5–F11.11** — the fifth visible change, `x-clens-session` now **naming** the session rather than only grouping it, in §6 and the PR body (F11.5); `store.go`'s `SessionEvents` doc-comment rewrite added to §4 with "no bound replaces it" (F11.6); pass 3's precondition qualifier dropped — it is over **all** recorded `jsonl:<path>` cursors, and a recorded path with no readable file is a **refusal, not an exclusion** (F11.7); the Sessions view stated in **both** directions — the 184 collapse to the 3 conversations **and** the ~207 JSONL sessions the wiring materialises — in §6, §8, D7 and the PR body (F11.8); the D7 integration fixture asserts the row's **`session_id`**, with a subagent-sidechain variant (F11.9); the round-10 changelog's F10.1 heading mislabel acknowledged as a **record defect** (F11.10 — the site rewritten is §2.2's heading, not §2.1's; a prior round's artifact is immutable, so it is recorded rather than edited); §4's `proxy.go` row names all **three** deletions (`requestID`, `fallbackSeq`, `fallbackSeqCounter`) and their sites, and the `sink.go` row names the rename's three sites (F11.11). **12 rows total.** |
| 2026-09-20 | Round 12 revision (round-12 findings F12.1–F12.10; **plan v13**). **The structural instruction is applied first: D4's collision path is a requirement list plus its named traps, not a procedure.** Every round that specified the rekey path's call sequence found a MAJOR in it, so the ordered call list is deleted — `applyMergeTx` is now stated as **the one shared rule** (`mergeEvents` → `updateEventTx` → `source_mismatch`, minus the load) called by both callers, each keeping its own load, the rekey path loading **by the target key** — and the "there is no longer an unspecified middle" paragraph, the "content is not a union" block's restatement of `mergeEvents`'s halves, and §9's note's enumeration of the same obligations are cut back to the rule and its traps. **F12.1 (MAJOR, statement correction — the two paths are *not* made to agree)** — "bounded by the pricing-vintage divergence" is deleted: `billing_mode` is winner-picked and each source derives it by a **structurally different rule** (the proxy from the credential, `billingModeForAuthKind`; the tailer per row from the model prefix with an api override), so a prefix-api model called with an oauth credential is `subscription` + `api_equivalent_cost_usd` on one row and `api` + `cost_usd` on the other, permanently; D4 now states that the **per-row** invariant holds (mode and cost from the same `winner`) while the row's **aggregate membership flips** (`store.go:708-709`), and cites the code's own concession at `merge.go:247-251`. No hand-written winner pick is added, and reconciling the two derivations is named as a different story. **F12.2 (MAJOR)** — the *durable* divergence is the set `mergeEvents` never assigns: `prefix_hash`, `replay_of`, `replay_edits` ride `merged := *existing`, so on the rekey path (where `existing` is the JSONL taker) the merge drops the proxy row's hash and replay linkage, and pass 3 does not restore them; **the hash is what matters**, because the two hash-keyed session rules `continue` on a nil one — so the drop defeats F11.3's remedy on exactly the rows the backfill merges. Stated as a fourth requirement on `applyMergeTx`, in the warnings paragraph's shape and with the reason: the helper carries the **`incoming`** row's `prefix_hash` / `replay_of` / `replay_edits` onto the survivor when the survivor's is NULL/empty (a no-op on the live path). **F12.3 (MAJOR)** — the stated property described pass 1's *intermediate* state; D4 now states the **end state** (`rekey --yes` ends where the live path ends — pass 3 spares the pass-1 survivor and re-merges with `winner = incoming`, the fresh JSONL row) and that a **refused pass 3 leaves the intermediate state permanent**, since passes 1–2 are already committed; §5's pin asserts the state the command leaves. **F12.4** — "the taker is deleted outright" (the F7.1 class) becomes "the absorbed `incoming` row is deleted outright". **F12.5** — §4's "No other session rule changes" narrowed to "no other session rule's *code* changes", with `ruleCachePrefixInvalidation` named as a second consecutive-pair rule whose **input set** D7 changes. **F12.6** — §6's "pre-existing on the JSONL path" corrected: `serve` is `SetSessionRule`'s only caller and `newTailer` wires none, so the O(n²) pass is paid on the **proxy `flush`** path — which §5's pass-3 wall-clock case does **not** exercise, and §5 now says so. **F12.7** — pass 2 reads `x-claude-code-session-id` **under the same `maxSessionHeaderLen` bound the live `ExtractMeta` applies** (overlong → treated as absent, the same case), in D4 and §4. **F12.8** — §5's pin becomes a **two-path comparison** (the same request through a proxy capture and a real transcript) asserting both survivors' cost columns and the aggregate each joins agree **after pass 3**, plus the rekey survivor's `prefix_hash` non-NULL with `ruleCacheExpiredBetweenTurns` still evaluating it. **F12.9** — `internal/cli/purge.go` added to §4 as the fourth carrier of the "only destructive command" claim (and the file the docs cite). **F12.10** — §4's privacy citation corrected from `:106-107` (a table asserting no exclusivity) to `:111`, with `:111-113` the misstatement. **Net: the apply is a deletion** — D4's collision path and §9's note each lose their procedure, and the revision is net shorter. **13 rows total.** |
| 2026-09-20 | Round 13 revision (round-13 findings F13.1–F13.11; **plan v14**). **F13.1 + F13.2 (the same inversion; F13.2 is the round's MAJOR)** — "the taker's warnings" was backwards: the taker *is* the survivor and is never deleted, so the row whose `warnings` cascade away (`ON DELETE CASCADE`, `schema.sql:95`) is the **deleted** row. D4's wording is corrected ("**the deleted row's** warnings vanish with it") **and** §5's fixture is re-seeded on the **`incoming`** row with an assertion that the surviving warning's `event_id` is the **survivor's** — the old fixture seeded the warning on the row the delete never touches, so it passed with no re-attach code at all (the vacuous-assertion shape §5 itself condemns at `:1111-1120`). **F13.3** — §5's JSONL-taker bullet asserted `source_refs` as the literal `["proxy","jsonl"]`, unsatisfiable on that path: the survivor is the JSONL **taker**, and `unionStrings` seeds from the `existing` seat first (`merge.go:148`, `:350-366`), so the order is `["jsonl","proxy"]`; the assertion is now order-agnostic (or the seat's order). D4's "ends where the live path ends" is narrowed to the **winner-picked** columns. **F13.4 (settled by the human's ruling)** — the survivor keeps its **own seat's** `started_at` / `source` / `first_source`: `merged := *existing` (`merge.go:147`) already decides it and `applyMergeTx` must **not** special-case it — an "earlier of the two" rule would diverge from the live path *by rule* on a seat-dependent column, the class F12.1 documented and rejected. Added to D4's contract as a bounded consequence of the seat, not a rule change. **F13.5** — §5's mixed-adjacency case covered only `ruleCacheInvalidatedByTools`; §5 now also asserts `ruleCachePrefixInvalidation`'s **degradation** (`rules.go:293-307`, no `ReqBody` guard, so interleaving can only make it fire **less**) and states it as the accepted cost of D7's whole-conversation session — the "why acceptable" arm §4's row offers. **F13.6 (settled)** — the `replay_of` bullet names **both** delete sites (the collision path's absorbed-`incoming` delete and pass 3's bulk `jsonl:` delete), states the requirement (**no `replay_of` names a row that no longer exists**, a deleted id's reference **re-pointed to the survivor**, not cleared), and corrects the count: the reverse lookup is **unindexed** (`store.go:500-505`; no index on the column, `schema.sql:68-70`), so it runs **once per run over the deleted id set**, never per row, and `--dry-run` reports **dangles** — not the `replay_of <> ''` referrer count, which measures the wrong population. **F13.7 (settled)** — the precondition is checked **first, before pass 1** (pass 1 does not touch `jsonl:` rows, so the check is valid before any mutation); a failure is a **refusal with a non-zero exit and no rows changed**, not a "skip and report" that exits 0. The three disagreeing sites (D4 step 0, §6, §5) now state one behaviour, and F12.3's "permanent intermediate state" is narrowed to the residual **mid-run race** (a file becoming unreadable between the check and pass 3). **F13.8** — pass 3's transaction model is stated: passes 1–2 per row, pass 3's delete **one set-based `DELETE` in one transaction** (the re-ingest per row), F13.6's re-point committing inside it; §4's "all in per-row transactions" corrected. **F13.9** — "a **fourth** rule" corrected to "a **third**" (only the complete-frame switch and the never-observed override precede it; §9 calls it a three-part sequence). **F13.10** — §9's trap (and D4's F7.1 bullet) split the two mechanisms' **consequences**: the swapped call orphans the key (a second run then "works", caught by the `request_id` assertion), while the load-by-`incoming.RequestID` helper leaves the taker holding the key and loses only the proxy row's content — **not** corrected by a second run, and caught by §5's `source_refs` / row-count assertion, to which the `request_id` assertion is blind. **F13.11 (recorded)** — §6 gains a bullet on the `api`/`subscription` aggregate split: the winner-picked `billing_mode` lands one conversation's money in both totals depending on which seat survived, which this story does not close (D4 names it a different story). **§4 file-list gaps** — `internal/cli/serve.go` (the resolver carrier `serve` already builds) and `internal/cli/refresh.go` (`addCollectors` takes it; `runRefresh` must build one, since `session.New` exists only at `serve.go:89`) added to §4's table. **14 rows total.** |
| 2026-09-21 | Round 14 revision (round-14 findings F14.1–F14.11; **plan v15**). **MAJOR-free round** (0 BLOCKER / 0 MAJOR / 11 MINOR), the first clean round in this loop — the apply is deliberately small and does not re-grow the collision path's requirement list. **F14.1** — §6 still **fused the two mechanisms' consequences** (the third copy of the round-13 F13.10 residual): for the **load-by-`incoming.RequestID`** shape the key is **not** orphaned — the taker keeps its own key and the loss is permanent — so the fused "held by nobody either way … a second run works" was false for it. §6 now **splits** the two, matching D4 `:561-569` and §9 `:1678-1687` (both already correct); the sweep confirmed those are the only other copies, and §5's swapped-signature line is the swapped call's own. **F14.9 (AMBIGUITY, resolved as a settled rule)** — D4 `:716-717` put the overlong-header row "alongside N and M", contradicting `:839-842` and §5 `:1314`, which put it in **L**; the plan's own definitions resolve it to **L** (N/M are pass 1's body-id populations, and pass 2 treats the overlong header as absent, so the row keeps its minted `s_…`), and **all three sites now say L**. **F14.10 (AMBIGUITY, resolved)** — D4 `:543-545` read literally ("never holds a row of its own") said there was nothing to delete; reworded to "the proxy row is the `incoming` **seat**, and the merge never writes it — which is why the caller deletes it", matching the design (the row **is** inserted, then deleted). **F14.5 (the gate's blind spot)** — §5's precondition case could not fail on "checked before pass 1": a jsonl-only fixture is invisible to passes 1–2, so the fixture now seeds a **pass-1-eligible `proxy:` row** and asserts its `request_id` is **still** `proxy:…` after the refusal. **F14.7 (the gate's other blind spot)** — D4 promised a "race case" in §5 that does not exist; replaced with the accurate position: the residual mid-run race is **not §5-testable** and is **recorded, not mitigated** (§6). **F14.2** — §4's `cli_test.go` row gains its seven `newTailer` call sites (D7's resolver parameter is a compile break at the row's own file); **F14.3** — §4's `ingest.go` row now says **`runIngest` builds** the resolver (`ingest.go:38` holds the `*store.Store`; `st` cannot supply it), and the `refresh.go` row's "`session.New` exists only at `serve.go:89`" is narrowed to **non-test** (`consumer_test.go:454` also constructs one); **F14.4** — §4's `consumer.go` row names the replacement for the deleted `fallbackSeqCounter` — a package-level `*uint64` — with the reason carried; **F14.6** — F13.6's settled requirement gains its §5 case (the `replay_of` **re-point** to the survivor, plus the dry-run's **dangle** count, not the referrer count); **F14.8** — F13.4's settled rule gains its §5 pin (the survivor's `started_at` / `source` / `first_source` are the **taker seat's**, asserted on a fixture whose seats differ so it can fail); **F14.11 (SCOPE, recorded, no design change)** — §5's live acceptance cannot gate §2's absolute figures or §2.6's arithmetic (only the §2.3 ratio is re-derived); recorded so the PR body and the acceptance run are the only gates on those numbers, deliberately. **15 rows total.** |
| 2026-09-21 | Round 15 revision (round-15 findings F15.1–F15.4; **plan v16**). **One MAJOR, three MINOR** (0 BLOCKER / 1 MAJOR / 3 MINOR), so the convergence streak resets to 0 — the MAJOR is a **missing requirement**, not a design decision, so it is applied rather than deferred. **F15.1 (MAJOR, applied)** — the collision path carried only "reconcile the distinct sessions", while its sibling pass 2 step 3 says reconcile **and then remove the old `sessions` row once re-deriving shows it empty**, or `clens sessions` shows a ghost. The merge takes the deleted proxy row out of its own minted `s_…`, and where that session is not the survivor's (the historical proxy row whose `req_headers` lacked the header, the **L** population) nothing later removes it — `reconcileSessionTx` only `UPDATE`s (`store.go:684-767`), `ListSessions` takes no `request_count` filter (`store.go:783-799`), so the ghost is user-visible in both `clens sessions` and `/api/sessions`. D4's pass-1 step 3 and §4's `store.go` row now carry pass 2's clause with its reason, and the mitigation is stated: `clens purge` already leaves zero-row `sessions` rows (`store.go:828-834`, `:861-867` delete `events` only), so this is consistency with pass 2, **not a new invariant** — but a command whose sibling pass forbids the artifact must not leave it. §5's proxy-vs-proxy fixture is **restated**: it asserted each session's post-run aggregate, which pins the ghost and would go red for the correct implementation — it now asserts the **survivor's** aggregate and that the **deleted row's** session is **gone** (reconciled to empty, then removed). **F15.2 (MINOR, applied)** — §6 binds the `replay_of` re-point to **both** delete sites, but §5's only case sat at pass 3's, and D4's collision contract and §4's row never named it, so an implementation resolving the reverse lookup only over pass 3's `jsonl:` id set stayed green while the collision left a dangling reference. The collision-site obligation is added to D4's pass-1 step 3 and §4's row, and a §5 bullet in the JSONL-taker collision fixture seeds a row whose `replay_of` names the absorbed proxy row's id and asserts it names the survivor afterward. **F15.3 (MINOR, applied as a wording fix)** — §5's wall-clock case could carry no assertion ("or at least **report** it"), and the wiring it bounds can only make the run **faster** where it is absent, so no wall-clock bound can fail on that absence; the case is now stated plainly as a **reported measurement** (seconds against N), the flakiness reason is given, and §6's "bounds **or reports**" becomes "**reports**", so the two sites agree on what the case does — the wiring's *existence* stays gated by §5's tailer-wiring case (`:1122-1126`). **F15.4 (MINOR, AMBIGUITY, resolved)** — pass 2 read `x-claude-code-session-id` only, while D7 change 1 settles that `x-clens-session` **wins** when both are present (`:930-935`, asserted at `:1085-1087`), so a historical row carrying the override was re-attributed to the conversation id where the live path would store the override. Resolved in the plan, not deferred: pass 2 now mirrors the live **precedence** as well as the live **length** rule (F12.7), for the same reason — it exists to reproduce what the live path stores — stated in D4's pass 2 step 1 and §4's row, with a §5 case seeding a headers-carry-both row that asserts the override wins. **16 rows total.** |
| 2026-09-21 | Round 16 revision (round-16 findings F16.1–F16.2; **plan v17**). **One MAJOR, one NIT** (0 BLOCKER / 1 MAJOR / 0 MINOR / 1 NIT), so the convergence streak resets to 0. **F16.1 (MAJOR, two limbs, applied as a narrowing)** — pass 3's `replay_of` arm is **unreachable**, and `replay_of` can only name a **body-carrying** row: the reference is written only from the replay route's `ReplayMeta.Of` (`proxy.go:133-136`, `:252` → `consumer.go:430`), and that route refuses an original with no stored body (`internal/api/replay.go:86-88`); `internal/jsonlogs` assigns no `ReqBody`/`RespBody` and a merge cannot put one on a `jsonl:`-keyed row (the taker's key must equal the incoming's and no proxy key is `jsonl:`-prefixed). So **pass 3's `request_id LIKE 'jsonl:%'` delete can never dangle a reference**, and the collision path is the run's **only** delete site that can. The claim is **narrowed, not re-designed**: §6's "the run has two delete sites" becomes "the collision path is the only delete site that can dangle a reference", with the reachability argument stated; §4's `store.go` row is narrowed the same way; D4's pass-1 step 3 (`:530-534`) stops telling the collision site to mirror pass 3 and stands as the exemplar itself; the F15.2 rationale (`:1258-1265`) stops calling pass 3's `jsonl:` id set "the natural way to write it" and names it the empty set it is; and §5's pass-3 bullet is replaced with the **negative form** — seed a replay row naming a surviving `proxy:`-keyed row and assert pass 3 leaves its `replay_of` **untouched** — keeping the `--dry-run` dangle-count assertion. Limb (b) (the asserted target's id is unknowable inside step 1's transaction) **dissolves** with the arm: the collision site's target is the survivor, which exists at commit time. **The reviewer's alternative — keeping the arm and moving the re-point after the re-ingest — is rejected** in the changelog: it would re-grow pass 3's procedure against the standing structural instruction that D4's collision path and the backfill stay requirement lists with named traps, and it arms a site with no referrers. **F16.2 (NIT, applied)** — the wall-clock bullet's cross-reference `(:1122-1126)` pointed at the `internal/analyze` interleaving fixture; corrected to the tailer-wiring case `(:1145-1149)`. **17 rows total.** |
| 2026-09-21 | Round 17 revision (round-17 findings F17.1–F17.3; **plan v18**). **One MAJOR, two MINOR** (0 BLOCKER / 1 MAJOR / 2 MINOR), so the convergence streak stays 0 — the MAJOR is a **stated rule that is false against the code**, not a design change, so it is corrected rather than deferred. **F17.1 (MAJOR, applied as a restatement)** — the plan claimed a pass-1 collision's two rows "share one session" and that the reconcile set is "usually one". They can never share: pass 1's predicate (`request_id LIKE 'proxy:%'` **and** a body id) selects only **pre-D7** rows (a post-D7 `proxy:`-keyed row is by construction one whose body yields no id, D2's precedence), and pre-D7 `Resolve` minted **unconditionally** — the only header that reached it was `x-clens-session`, which never became the id (`session.go:58`, `:80-84`; `meta.go:38-40`); the taker holds the target key, which no pre-D1 row can, so it is a post-D1 row — most often the **JSONL** row, whose `session_id` is the transcript conversation id (`jsonlogs.go:381-386`) — and a minted `s_…` never equals a conversation id. The reconcile set is therefore **always two**: the survivor's conversation session and the deleted row's minted one. The requirement text ("the distinct sessions among the two rows") is left alone; the false **claim**, the ordering rationale, the §3 consequences, D4's pass-1 step 3 (the "where that session is not the survivor's — the **L** population" qualifier becomes unconditional), D4's "D7 deletes the premise" and §2.6's guarantee paragraph are corrected, and **§5's JSONL-taker fixture is re-seeded**: the proxy row now sits in its **own minted session** (its own `sessions` row) and the case asserts the survivor's conversation aggregate **and** that the minted `s_…` is reconciled to empty and removed (F15.1's clause on the shape that actually happens). The two shapes' session seeding was **inverted** — the reachable JSONL-taker case got one session, the unreachable proxy-vs-proxy case got the two-session pinning — and is now fixed; the proxy-vs-proxy fixture is kept as it was. **F17.2 (MINOR, applied)** — D4's premise "that session **has an aggregate row**" is not the state pass 1 meets: today the `sessions` table holds only the 184 proxy-minted ids, D7 wires rows only for later ingests, and pass 2's own text says the conversation session "may not exist yet" and must be upserted (`:752-759`). Reworded to what pass 1 actually meets — the survivor's session is the one the merged row belongs to, materialised by pass 2's upsert and pass 3's re-ingest — with no branch restored. **F17.3 (MINOR, applied)** — §9's supersession of br-GI-9-04 was scoped only to the two-tables wording, but the bead still carries the **pre-D7** design: its ordering section (`:31-47`, "step 3 branches on the taker's source", "only proxy sessions have [a materialized aggregate]") and its helper contract (`:102`, "the surviving row is the proxy row's `id` and `session_id`", unimplementable through `mergeEvents` — `merged := *existing` never assigns `RequestID` — i.e. §9's forbidden swapped call). §9's rebuild note now names both as **superseded by D4/D7**; the bead file is **not** edited (`.beads/` is outside the plan's write boundary), the correction living in §9. **18 rows total.** |
