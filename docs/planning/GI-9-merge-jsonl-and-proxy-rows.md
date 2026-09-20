# GI-9 — The same request is stored twice under two keys that cannot match, and the transcript inflates itself when its `requestId` is absent

## 1. Summary

Under proxy mode, one request produces two rows that never merge, and the JSONL tailer additionally
inflates *itself* by about a third. Both are one defect: the cross-source merge keys on
`events.request_id`, and neither writer produces a value the other can produce.

The two writers mint namespaced fallback keys — `jsonl:<sessionId>:<uuid>` and
`proxy:<sha256>:<started_at_ns>:<attempt>` — that are unique to their writer *by construction*, so
`insertOrMerge` never reaches its merge branch. On the live install, **0 rows share a `request_id`
across sources**, and 0 of 77 proxy session ids appear as JSONL sessions.

The proxy's documented key — the upstream `request-id` response header — is never available on this
endpoint: **0 of 264 stored proxy responses carry one**. `requestId` is correspondingly absent from
exactly the transcripts written while proxied (0 of 4,361 usage lines in the current session, against
8,220 of 8,220 in a session that did not go through this proxy; 753 of 1,136 transcript files carry
none, and the switch coincides with the proxy's first stored row). The plan's *Cross-source identity*
assumption (test 11b) was therefore never merely unverified — it is unverifiable as written.

**The identity already exists on both sides and is simply unused.** The upstream mints a message id,
the proxy stores it inside the response body it already keeps, and Claude Code writes the same value
into the transcript as `message.id`. Compared across every same-request pair the data admits
unambiguously: **39 pairs, 39 agreements, 0 disagreements.**

This plan makes that value the key on both sides. It is one rule, applied forward and backward, with
no heuristic anywhere — which matters, because the fallback the plan originally proposed cannot work:
matching a proxy row to a JSONL row by token counts within its own `[started_at, ended_at]` interval
leaves **more than one candidate for 114 of 257 proxy rows**, since cache-read counts repeat across a
session's turns.

Three things change beyond the key itself, and each is load-bearing:

- **The proxy stops deciding identity.** The rule moves to the cold path, in one function, because
  the answer now depends on a parsed response body — which the hot path must not touch.
- **A merge re-derives the session it vacated.** `mergeEvents` discards the incoming row's
  `session_id`, and both callers reconcile only the survivor's session, so the vacated session's
  materialized totals stay stale-high forever. Latent today because merges never fire; routine the
  moment they do.
- **A backfill retires the historical duplicates**, because forward-only would leave 28,405 surplus
  rows and every double-counted total in place.

## 2. Evidence base

Measured against the live install (`~/.clens/lens.db`, read-only) and the real transcripts under
`~/.claude/projects/` on 2026-09-20. The database is written to while it is read — the proxy is
serving the session that produced this plan — so every count is a snapshot.

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
| JSONL rows on the `jsonl:` fallback | 70,536 of 87,340 |
| JSONL rows with a real `requestId` | 16,804 |
| Proxy rows | 260 (all on the `proxy:` fallback) |
| Proxy session ids that appear as JSONL sessions | **0 of 77** |

### 2.2 The upstream sends no `request-id` header

Across 264 stored proxy responses, the observed header set is
`date, server, vary, access-control-allow-credentials, strict-transport-security, via, x-amz-cf-id,
x-amz-cf-pop, x-cache, x-content-type-options, x-ds-trace-id, content-type, cache-control,
content-encoding, content-length, cf-ray, …`. There is no `request-id`. The endpoint
(`https://api.deepseek.com/anthropic`) sends `x-ds-trace-id` instead, and Claude Code does not
promote that to `requestId`.

The transcript evidence confirms the mechanism. `requestId` is populated from the upstream header,
so it is absent exactly where that header is:

- Current session (`ef875ca8…`, proxied): **0 of 4,361** usage lines carry `requestId`.
- A session from before the proxy existed (`1fd1d222…`): **8,220 of 8,220** carry one.
- Across all transcripts: **753 of 1,136 files carry none**, and the switch coincides with the
  proxy's first stored row (2026-09-19 10:17).

### 2.3 The identity is already on both sides

The upstream mints a message id. It appears in the proxy's stored response body as
`message_start.message.id` when streaming and as a top-level `id` when not:

```
event: message_start
data: {"type":"message_start","message":{"id":"f5ebc37a-f2eb-4d1d-b523-fbed4ce72f19", …
{"id":"ccc10259-13ff-4527-9eb8-b5e4ba00b0fb","type":"message","role":"assistant", …
```

**178 of 260 stored proxy rows yield exactly one such id**; the rest are error bodies
(`{"type":"error",…,"request_id":null}`), redirects, and non-JSON payloads, which have none.

Claude Code writes the same value into the transcript as `message.id` — present on **100% of usage
lines** in both the proxied and the non-proxied session. Anthropic mints `msg_011CeqpGzFNPrYtxeB8huZja`;
DeepSeek mints a UUID (`c3c4de24-23c2-44ad-b685-aa98c9d9ee6e`). Same slot, different vendor format.

**The decisive measurement.** For every proxy row whose `(input_tokens, output_tokens)` pair matches
exactly one JSONL row inside its interval — the unambiguous subset — the proxy's body id was compared
against the transcript's `message.id`:

```
unambiguous pairs = 39   →   agree = 39   disagree = 0
  proxy=['2d48cfd0-ffa7-4d00-9687-12bd22804717']  jsonl=['2d48cfd0-ffa7-4d00-9687-12bd22804717']
  proxy=['80bbff96-10d4-49f8-be44-6ace007fc3b6']  jsonl=['80bbff96-10d4-49f8-be44-6ace007fc3b6']
  proxy=['c577fd0c-a596-4328-9fcb-1b4f6b366102']  jsonl=['c577fd0c-a596-4328-9fcb-1b4f6b366102']
  proxy=['d29befdb-0fe3-457b-80e5-86145576b1f8']  jsonl=['d29befdb-0fe3-457b-80e5-86145576b1f8']
```

This is the whole plan's foundation, and it is why the design is a key change rather than a matcher.

### 2.4 The JSONL tailer inflates itself

`dedupeAssistantLines` ([internal/jsonlogs/dedup.go:118-131](../../internal/jsonlogs/dedup.go#L118-L131))
groups by `requestKey` and keeps the last line per key. With a real `requestId` that collapses the
per-content-block duplicate lines correctly — the measured defect test 8 pins. With the fallback key
it does not: the fallback embeds the line's own `uuid`, and each content-block line of one response
carries a **different** uuid, so the pair becomes two rows.

Two independent measurements agree on the size:

- **Statistically.** Grouping by `(session_id, token quintuple, 5s bucket)`: **28,405 surplus rows**
  and **113,016,291 surplus input tokens**. At a 1-second bucket it is still 21,153 surplus rows, so
  the inflation is not an artifact of a loose window.
- **Structurally.** One session's 4,361 usage lines carry only **1,251 distinct `message.id`** — 3.5
  lines per request.

This half is JSONL-internal. No proxy-side change reaches it, and neither does any rule that counts
only proxy rows: inside the proxy's own active window there are 9,334 JSONL rows against 225 proxy
rows, so proxy-only totals would report about 2% of what happened.

### 2.5 The fallback matcher cannot substitute

Matching a proxy row to a JSONL row by `(input_tokens, output_tokens)` within the proxy row's own
`[started_at, ended_at]` interval, padded 2s:

| candidates found | proxy rows |
|---|---|
| 0 | 104 |
| exactly 1 | 39 |
| **more than 1** | **114** |

Cache-read counts repeat heavily across a session's turns, so the fingerprint is not discriminating.
A heuristic backfill would have to skip 44% of rows or guess on them — and it would be guessing about
which rows to *delete*.

### 2.6 A merge does not re-derive the session it vacates

`mergeEvents` sets `merged.SessionID = existing.SessionID`
([internal/store/merge.go:227](../../internal/store/merge.go#L227)) and drops the incoming row's
session. Both callers then reconcile only the survivor's:

```go
id, sessionID, merged, err = insertOrMerge(ctx, tx, ev)
if merged && sessionID != "" {
    reconcileSessionTx(ctx, tx, sessionID)   // insertOrMerge returns the SURVIVOR's session
}
```

`reconcileSessionTx` ([internal/store/store.go:684](../../internal/store/store.go#L684)) recomputes a
session's `request_count`, token sums, `priced_count`/`unpriced_count`, `model_set`, `warning_count`
and both cost columns from `events WHERE session_id = ?`. So a row that leaves a session takes its
tokens with it, but the session it left keeps its old totals.

Latent today, because merges never fire (§2.1). It becomes routine the moment the key is fixed — and
it is guaranteed to bite, because the two sessions a cross-source merge joins are *never* the same
(0 of 77 overlap).

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
the upstream message id, then the synthetic fallback.** Anthropic traffic sends both, and the proxy
stores the header as its `request_id` — so the header must win on both sides or the Anthropic case
would regress from working to broken. DeepSeek traffic sends only the body id, so both sides fall to
tier two and meet there.

The JSONL side needs `ID string \`json:"id"\`` on the `message` struct
([internal/jsonlogs/dedup.go:25-29](../../internal/jsonlogs/dedup.go#L25-L29)); the value is already
in the line and is currently discarded.

This single rule fixes both defects. The content-block duplicates collapse because they share a
`message.id`; and the JSONL row's key becomes the exact value the proxy can produce, so
`insertOrMerge` finally takes its merge branch.

### D2 — The proxy parses identity in the cold path, in one place

`parse.Usage` gains `MessageID`, filled from the frame the extractor already visits
([internal/parse/usage.go:97-105](../../internal/parse/usage.go#L97-L105) for `message_start`,
[`:125-137`](../../internal/parse/usage.go#L125-L137) for the non-stream body). That is four lines:
two struct fields and two assignments. No new pass over the body, no new decode.

Identity precedence then lives in **one function in the consumer**, where the parsed body is:

```go
func requestID(call *sink.CapturedCall, usage parse.Usage) string {
    if call.RequestID != "" { return call.RequestID }   // the upstream header, when sent
    if usage.MessageID != "" { return usage.MessageID } // the upstream message id
    return syntheticRequestID(call)                     // a key unique to this attempt
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

### D3 — A merge re-derives the session it vacated

`insertOrMerge` returns the vacated session alongside the survivor's, and both callers add it to the
reconcile set:

```go
// insertOrMerge: vacated is non-empty only when the incoming row's session
// differs from the survivor's — the row moved out from under it.
id, sessionID, vacated, merged, err = insertOrMerge(ctx, tx, ev)
```

`InsertEvent` reconciles both; `InsertEvents` adds both to its existing `sessions` map, so the batch
path costs one extra map key rather than a second pass.

**`session_id` stays unrewritten.** The invariant ([internal/store/merge.go:225-227](../../internal/store/merge.go#L225-L227))
exists so a merge never moves a row out from under a session whose totals were derived from it — and
D3 removes exactly that hazard. So it *could* be relaxed, and there is a real argument for relaxing it:
the survivor keeps the **proxy's** session, which is a heuristic reconstruction, while the JSONL
session id is Claude Code's own conversation id and is the ground truth. Post-merge the Sessions view
therefore groups rows by the reconstruction rather than by the conversation.

This plan does **not** relax it. Totals are correct either way (global aggregates do not care which
session owns a row), the change is not needed for any outcome in §9, and rewriting a documented
invariant that carries its own test belongs in a story whose goal is that, not this one. The
consequence is recorded in §8.

### D4 — The backfill is two mechanisms behind one command

Forward-only would leave 28,405 surplus rows and every historical total double-counted, so the story
carries the backfill. The two halves re-key from genuinely different sources, which is why one command
has two mechanisms rather than a shared loop:

**JSONL half — delete, then re-ingest.** A stored JSONL row cannot be re-keyed in place: its old key
holds a uuid and the message id it *should* hold was never stored. The only place that value exists is
the transcript. So the half is:

1. delete `source='jsonl' AND request_id LIKE 'jsonl:%'`, plus the warnings attached to those rows;
2. zero the byte cursors and re-run the tailer — the mechanism `clens ingest --rebuild` already owns.

Rows with a real `requestId` are untouched and re-insert idempotently, because re-reading them
produces the same key and merges.

**Proxy half — re-key in place.** The value is already stored, inside `resp_body`, so these rows need
no re-read:

1. for each `source='proxy'` row whose `request_id` is synthetic **and** whose body yields an id:
   decode the body, extract the id, and re-key the row;
2. where the new key is already taken, merge instead — `mergeEvents` unions the content, so nothing
   is lost.

The body must be decoded first (`decode.Body`, as `processCall` does at
[internal/consumer/consumer.go:302](../../internal/consumer/consumer.go#L302)); 65 of 264 proxy
responses are compressed.

**One command, `clens rekey`**, following the contract
[cli-and-tooling.md](../context/cli-and-tooling.md) states for any destructive subcommand — and the
repo currently has exactly one:

- nothing happens without `--yes`;
- `--dry-run` prints what `--yes` would do — both halves' counts, and nothing else;
- the two halves run in the order above, and the command reports each separately, because a run that
  did one and failed the other must not read as "done".

Ordering against the forward fix is strict: **the key rule must land before the backfill runs**, or
the re-ingest recreates the very keys the delete just removed.

### D5 — What deliberately does not change

- **`--body-policy off` stays as it is.** With no body there is no message id, so those rows keep the
  synthetic key and never merge. That is a real and permanent limitation of this design, and it is the
  honest one: under `off` the proxy genuinely did not look at the response. It is documented rather
  than worked around, and the merge is described as body-policy-dependent.
- **No read-time dedup view.** The plan already rejects that shape — it "leaves the inflation bug
  reachable by any query that forgets the view" — and nothing here reopens it. The rows are repaired,
  not filtered.
- **No schema change, and therefore no migration.** Every column this needs already exists
  (`request_id`, `resp_body`, and the `sessions` aggregates). `schemaVersion` stays `1`.
- **`internal/proxy` stays a tee.** It loses a function (D2) and gains nothing.
- **What each source captures** is untouched. The proxy still stores the compressed body it received;
  the tailer still stores the transcript content.

### D6 — The two documents that disagree about test 11(b) are reconciled

`docs/planning/GI-1-claude-lens-v1.md:1105` records the `request-id` ↔ `requestId` equivalence as
**"verified live"**; `docs/acceptance.md:200-220` records it as never run, "Outcome: not captured".
acceptance.md is right and the decision table is wrong. Both are updated with what this story
measured: the equivalence does not hold (0 of 264 responses carry the header), and it does not matter,
because the design stops depending on it. The same correction goes to the comment at
[internal/jsonlogs/dedup.go:95-99](../../internal/jsonlogs/dedup.go#L95-L99), which records the
assumption as "not yet verified" when it is now resolved.

## 4. Files changed

| File | Change |
|---|---|
| `internal/jsonlogs/dedup.go` | `message.id` on the `line`/`message` structs; the middle tier in `requestKey`; comment corrected |
| `internal/jsonlogs/dedup_test.go` | the new tier's cases (§5) |
| `internal/parse/types.go` | `Usage.MessageID` |
| `internal/parse/usage.go` | two struct fields, two assignments |
| `internal/parse/usage_test.go` | streaming and non-streaming extraction |
| `internal/consumer/consumer.go` | `requestID(call, usage)`; `buildEvent` uses it; the synthetic fallback moves here |
| `internal/consumer/consumer_test.go` | the precedence table |
| `internal/proxy/proxy.go` | `captureState.requestID` and `fallbackSeqCounter` deleted; `submit` passes the header value |
| `internal/proxy/proxy_test.go` | the fallback-key test moves to `internal/consumer` |
| `internal/store/merge.go` | `insertOrMerge` returns the vacated session |
| `internal/store/store.go` | `InsertEvent`/`InsertEvents` reconcile it; the rekey's store methods |
| `internal/store/store_test.go` | the vacated-session reconcile |
| `internal/cli/rekey.go` (new) | the command, `--yes` / `--dry-run`, both halves |
| `internal/cli/rekey_test.go` (new) | §5 |
| `cmd/clens/main.go` | register `rekey` |
| `docs/context/cli-and-tooling.md` | the `rekey` row; "the one destructive command" becomes two |
| `docs/context/workflows.md` | the merge now fires; the identity rule |
| `docs/planning/GI-1-claude-lens-v1.md` | §Cross-source identity and the line-1105 claim |
| `docs/acceptance.md` | test 11(b) — measured, with its outcome |
| `docs/context/decisions/008-…` (new) | the identity decision (D1) |

No new dependency. No schema change. No change to `internal/web`.

## 5. Test strategy

**Unit — `internal/jsonlogs`.** The middle tier resolves when `requestId` is absent and `message.id`
is present; two lines sharing a `message.id` and differing in `uuid` collapse to one row
(*the measured defect*); the precedence still prefers `requestId` when both are present; and the
existing distinct-uuid case still yields two rows — that test is correct today and must keep passing,
which is what makes the change a tier insertion rather than a relaxation.

**Unit — `internal/parse`.** `MessageID` is read from a `message_start` frame in an SSE fixture and
from a top-level `id` in a non-streaming fixture; absent in both when the upstream sent none.

**Unit — `internal/consumer`.** The precedence table, all three tiers: a header value wins over a body
id; a body id wins over the synthetic key; no header and no body id yields a synthetic key of the
documented shape. Plus the property the `attempt` counter exists for — two byte-identical bodies in
one process never collapse.

**Unit — `internal/store`.** A merge across two sessions re-derives **both**: the survivor's totals
gain the row and the vacated session's totals lose it. Asserted through `GetSession` on both ids,
because the materialized column is the thing that was wrong.

**Unit — `internal/cli/rekey`.** `--dry-run` deletes nothing (assert row counts unchanged, and that
both halves reported a non-zero count); `--yes` re-keys a synthetic proxy row to its body id; a
re-key that collides merges and leaves one row with `source_refs` unioned and no content lost; a
second run is a no-op; a run with neither flag refuses.

**Integration.** A JSONL line with no `requestId` and a proxy capture of the same request, ingested
through the real paths, produce **one** row — the end-to-end statement of the whole story, and the one
that fails today.

**Live acceptance (manual, recorded in the plan).** Re-run the §2.3 comparison against the live
database after the change: the unambiguous-pair count must still be 39-of-39-shape, and the row count
for the rekeyed window must drop by the surplus §2.4 measures. This is the check that the story's
premise still holds on real data, not just fixtures.

## 6. Risk areas

- **Deleting rows is the destructive act, and it is the point.** The JSONL half deletes; the proxy
  half only updates and merges. Mitigations: the selectors are narrow (`request_id LIKE 'jsonl:%'`);
  `--dry-run` is the default-safe path; the deleted rows are re-derivable from transcripts that are
  still on disk; and the command should be run with `clens serve` stopped, because a live tailer
  writing while cursors are zeroed is a race the command does not need to have.
- **Re-keying the proxy half is not re-derivable.** The stored `resp_body` is the only source, so a
  botched re-key cannot be replayed from anywhere else. The merge path makes it lossless
  (`mergeEvents` unions), but this is the half where "the heuristic has to be right the first time"
  actually applies — and it is why the design has no heuristic in it.
- **A `message.id` collision would silently collapse two requests.** Assessed as remote: DeepSeek's is
  a v4 UUID, Anthropic's is `msg_…`, and the shapes cannot collide with each other or with a
  `proxy:`/`jsonl:` synthetic key. It is a real assumption, so the integration test asserts one row
  per distinct id rather than trusting the format.
- **A retried call now stores two rows, not one.** Each attempt has its own message id, so the two are
  distinct responses and *should* be distinct rows — which is also what the existing synthetic-key
  rationale wants. Flagged because it is a change in row count for retry-heavy traffic.
- **The Sessions view changes shape after the backfill.** Rows leave JSONL sessions and merge into
  proxy sessions (D3, §8), so session row counts and the model set move. Expected, not a defect — but
  it is user-visible, so it belongs in the PR body and the refresh.
- **`--body-policy off` rows never merge** (D5). A user running `off` gets the JSONL half's fix only.
- **Retention may make part of the backfill moot.** If `retention_days` is configured, the oldest
  duplicates would age out anyway; the backfill's value is bounded by that window.
- **The 39-pair sample is small.** It is every pair the data admits unambiguously, which is the
  strongest available evidence, but it is not a proof that every request in the window agrees. §5's
  live acceptance re-runs it after the change, and the integration test asserts the property directly
  rather than sampling it.

## 7. Self-review

**As a senior engineer.** The design's virtue is that it adds no matching machinery: the identity was
already being captured and thrown away on both sides. The structural change (D2) is a *deletion* from
the hot path, and it makes the identity rule readable in one function instead of inferable from two
namespaces. The riskiest part is not the key change but D3 — a latent correctness bug in the merge
that this story activates, which is exactly the kind of thing that would otherwise surface as
"session totals look wrong sometimes" months later.

**As a QA engineer.** The cases that matter are the ones where the rule must *not* fire: two distinct
requests that share a session and token counts (the existing test 8 fixture family), a line with a
`requestId` that must still win, a body that decodes to an error object with no id, and a re-run of
the backfill. The vacated-session assertion is on both sessions because asserting only the survivor
passes even when the bug is present — the survivor was always correct.

**As a security engineer.** No new input reaches a trust boundary: the message id comes from upstream
response bytes already stored, and the only new write path is a CLI command operating on the local
database. The rekey command *does* delete, which is why it takes purge's contract rather than a new
one. No credential is involved, no header is newly captured (the request-id header is already read),
and full bodies are neither newly exposed nor newly retained. The one thing to hold: the command must
refuse without `--yes`, and `--dry-run` must not delete as a side effect of measuring.

## 8. Out of scope

- **Which session owns a merged row** (D3). The survivor keeps the proxy's session, so the Sessions
  view groups by a reconstruction rather than by Claude Code's conversation id. Correct totals,
  debatable grouping; the fix is to relax the never-rewrite-`session_id` invariant, which deserves its
  own story.
- **Recovering an identity for `--body-policy off` rows.** The proxy did not look, so there is nothing
  to recover.
- **Any read-time dedup view** (D5).
- **Changing what either source captures**, including promoting `x-ds-trace-id`.
- **The dashboard's filters.** No new column means no new filter; `source`/`billing_mode` filters on
  the Stats tab remain the separate, already-offered item.

## 9. Bead sketch (Phase 3 formalises this)

| # | Bead | Depends on |
|---|---|---|
| 01 | `parse`: `Usage.MessageID` from both response shapes | — |
| 02 | `consumer`: identity precedence in one function; the synthetic fallback moves here | 01 |
| 03 | `proxy`: `captureState.requestID` deleted; `submit` passes the header value | 02 |
| 04 | `jsonlogs`: `message.id` as the middle tier of `requestKey` | — |
| 05 | `store`: a merge re-derives the session it vacated | — |
| 06 | `cli`: `clens rekey` — both halves, `--yes` / `--dry-run` | 02, 04, 05 |
| 07 | docs: reconcile test 11(b), the merge flow, the CLI table, and add decision 008 | 01–06 |
| 08 | live acceptance re-run and the recorded manual run | 06 |

Bead 06 is the largest and the only destructive one. Beads 01–05 are all forward fixes and must land
before 06 runs anywhere but a dry run.

## Change History

| Date | Change |
|---|---|
| 2026-09-20 | Initial plan from Phase 1 intake. Identity verified live (39/39) before any design was written; the fallback matcher measured and rejected (114 of 257 ambiguous). |
