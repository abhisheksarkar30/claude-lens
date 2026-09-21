[← INDEX](INDEX.md)

# Workflows

The six flows that span more than one package. Each one's failure modes and transaction boundaries
are called out below the diagram, because those are what a change is most likely to break.

## 1. A proxied call — capture without slowing the client

The defining flow. Everything expensive is deferred off the client's goroutine.

```mermaid
sequenceDiagram
  participant C as Claude client
  participant P as internal/proxy<br/>(hot path)
  participant U as api.anthropic.com
  participant S as internal/sink
  participant K as internal/consumer<br/>(cold path)
  participant D as SQLite

  C->>P: POST /v1/messages
  P->>U: same bytes, unchanged
  U-->>P: response (possibly SSE)
  P-->>C: streamed straight through
  Note over P,C: TTFB never depends on clens
  P-)S: tee req+resp bytes (non-blocking)
  S-)K: wake
  K->>K: decode (br/zstd/gzip) -> parse -> analyze -> price
  K->>D: INSERT events + warnings, fold session
```

**Failure modes / retries / idempotency / transaction boundaries:**

- **A capture failure is logged, never propagated.** Upstream unreachable → the client gets a
  synthesized error; `TestFailOpenOnUpstreamFailure`.
- **The tee must not block the client.** The sink is bounded and drops rather than waits — a slow
  consumer degrades capture, never latency.
- **Zero seams is a supported state.** A consumer with nothing wired still writes rows (zero usage,
  no warnings beyond `upstream_error`/`analyzer_panic`), rather than panicking.
- **`InsertEvent` returns `(id, sessionID, error)`, and `sessionID` may not be `ev.SessionID`.**
  On a `request_id` merge it is the *existing* row's session. Every session-scoped call keys off
  the returned value — see flow 2.
- **`CaptureComplete` covers both teed buffers.** The flag is submitted as
  `!respBuf.truncated && !reqBuf.truncated`; deriving it from the response alone let a request body
  cut at the read cap be stored as a prefix on a row still reporting a whole capture (br-GI-7-08,
  found by the GI#7 manual run rather than by a test — every fixture in that story truncated a
  response). A migration that changes this flag changes what `analyze`'s `stream_incomplete` rule
  fires on and which row the merge prefers; see flow 2.
- **`--body-policy off` skips the tee, not the row.** Under `off` no `boundedBuffer` is allocated and
  no `io.TeeReader` is installed; the response is wrapped in a bare pass-through whose only job is to
  fire the submit when the call ends, so a row is written with both body columns NULL and
  `CaptureComplete` **true** — nothing was narrowed, the body was simply never kept. Before
  br-GI-7-09 the handler returned the bare `ReverseProxy` under `off` and no row was written at all,
  which is the opposite of what `clens serve`'s banner promises. The hot path is unchanged either
  way, and `TestNoBufferingSSE` is a table over both policies to keep it that way.

## 2. Cross-source merge — the one `UPDATE` on `events`

Source A and source B can both describe the same call. `request_id` is `UNIQUE`, so the second
arrival merges instead of inserting.

**The merge now fires** — before GI#9 the proxy and `jsonlogs` rarely produced the same
`request_id` for the same call, because each fell to its own shape of synthetic key when the
`request-id` header was absent. GI#9 gave both writers a shared three-tier identity rule
(`internal/consumer`'s `requestID`, `internal/jsonlogs`'s `requestKey`): **the response body's
`message.id` is the request's identity on both sides**, ranked between the `request-id` header (tier
1) and a namespaced synthetic fallback (tier 3, `proxy:<sha256(body)>:<started_at_ns>:<attempt>` /
`jsonl:<sessionId>:<uuid>`) — see [decisions/008](decisions/008-three-tier-identity-key.md). Two
writers landing on the same message id is what makes this flow a normal occurrence rather than an
edge case only `request-id` could trigger.

```mermaid
sequenceDiagram
  participant J as internal/jsonlogs
  participant M as store.merge
  participant D as SQLite
  participant K as internal/consumer

  J->>M: insert event (request_id=X)
  M->>D: SELECT by request_id
  alt row exists
    M->>M: mergeEvents(existing, incoming)
    Note over M: first_source + session_id are NEVER rewritten
    M->>D: UPDATE events (in the tx)
    M->>D: re-derive the SURVIVING row's session totals
    M-->>K: (id, sessionID of the surviving row, merged=true)
    opt token counts disagree
      M->>D: INSERT warning source_mismatch
    end
  else no row
    M->>D: INSERT events
  end
```

**Failure modes / retries / idempotency / transaction boundaries:**

- **The merge and the session re-derivation are one transaction.** A merge rewrites a row's token
  columns, so an incremental fold would drift from the sum it is supposed to equal. `sessions` is
  therefore *recomputed*, not incremented, on this path.
- **`first_source` and `session_id` are never rewritten** ([internal/store/merge.go:225](../../internal/store/merge.go#L225)).
  Consequence: the surviving row can belong to the **incoming event's** session or the **existing**
  one — which is why `insertOrMerge` returns the session id and every caller uses *that*.
  Reconciling `ev.SessionID` instead creates a session row owning no events and leaves the real
  session out of the analysis. This was an implementation-cross-review finding, fixed in `8650b9a`.
- **The winner is "the more complete record", and both bodies count.** Since br-GI-7-08
  `CaptureComplete` is also false when only the *request* body was cut at the cap, so a
  request-truncated proxy row no longer wins the token pick against a wholly-captured `jsonl` row for
  the same request. The disagreement is not lost — a differing pair still raises `source_mismatch` —
  and with one body known to be a prefix, the whole record is the safer one to quote.
  `TestMergePrefersTheWhollyCapturedRowOverATruncatedRequest` pins it, and its write order is
  load-bearing: with the `jsonl` row second both orderings pick it and the test would pass either
  way.
- **...but a row that never looked cannot win, or be called a liar.** A row captured under
  `--body-policy off` is `CaptureComplete` true with *every* token column zero, because usage is
  parsed out of the response body it never kept. The flag-based pick alone would hand it the win
  whenever it arrives as the `incoming` argument — zeroing the counts of the row that does have them
  — and would set `mismatch`, attaching a `source_mismatch` at `SeverityError` to a disagreement that
  never happened. So the pick is corrected (a winner with no observed usage yields to a loser that has
  some) and so is the mismatch condition (a disagreement needs two measurements, which is what
  `mergeEvents`'s own contract already said: *"a 0-vs-N difference is not a disagreement"*).
  `TestMergeDoesNotLetABodylessRowZeroObservedUsage` pins both halves in **both** orderings, and
  `TestMergeStillWarnsOnATrueDisagreement` pins the warning that must survive — narrowing a condition
  like this is exactly the kind of fix that silently goes too far. Note both orderings take the *same*
  `&&` case, since both rows are complete; only the argument order differs.
- **`billing_mode` moves with the winning cost columns** — it is *not* `preferNonEmpty` like its
  neighbours. This is the one column a merge is expected to contradict: the JSONL tailer resolves it
  per row by model prefix, so re-ingesting a DeepSeek call flips it `subscription` → `api`. Keeping
  the stored mode while adopting the incoming cost would leave a `subscription` row carrying a real
  `cost_usd` — the pair [decisions/001](decisions/001-billing-split-by-column.md) forbids.
  `account` and `auth_kind` stay `preferNonEmpty` deliberately: a JSONL line carries no auth signal,
  and on the ordinary ordering the live proxy row already holds the real account name.
- **A winner with no mode derives one from the column it priced.** An unclassified credential leaves
  `billing_mode` as `''`, which matches neither of the aggregates' `CASE WHEN` branches and would
  drop the row from every total, so the label follows the money: `cost_usd` → `api`,
  `api_equivalent_cost_usd` → `subscription`. A winner that priced nothing keeps the stored mode
  instead, because there is no cost column for an adopted label to contradict.
- **A disagreement is a finding, not an error:** `source_mismatch` is written and both rows survive
  as one merged row.
- **Idempotent:** re-ingesting the same JSONL re-merges to the same result. That is what makes
  `clens ingest --rebuild` the re-pricing path rather than a duplicate-row risk.
- **The session a merged row belongs to is the conversation id, not a heuristic group (D7).** The
  proxy now adopts `x-claude-code-session-id` as its stored `session_id` — the id the request already
  carries — instead of the gap-window `s_…` grouping it used before GI#9. A proxy row and its JSONL
  counterpart therefore agree on `session_id` before they even merge, and `clens sessions` shows real
  conversations rather than proxy-only heuristic groups. See
  [decisions/009](decisions/009-proxy-adopts-the-conversation-id.md).

## 3. `clens refresh` — the collector fan-out

```mermaid
sequenceDiagram
  participant CLI as clens refresh
  participant I as internal/ingest
  participant J as jsonlogs
  participant S as snapshot
  participant A as adminrep
  participant D as SQLite

  CLI->>I: RunOnce(ctx)
  par registration order
    I->>J: Poll()
  and
    I->>S: Poll()
  and
    I->>A: Pull()
  end
  J->>D: upsert events(source=jsonl) + cursor
  S->>D: append quota_snapshots
  A->>D: upsert admin_*_days
  I->>D: write health:<source>:{success,error} to ingest_state
  I-->>CLI: []Outcome
```

**Failure modes / retries / idempotency / transaction boundaries:**

- **The proxy is deliberately not one of `RunOnce`'s collectors** — it runs continuously, not on a
  schedule ([internal/ingest/ingest.go:9](../../internal/ingest/ingest.go#L9)).
- **A broken collector never prevents the others from writing.** `runOne` isolates both errors and
  **panics** per collector; each failure is recorded against its own source.
- **Collectors run in registration order, not concurrently** in the loop — parallelism is the OS
  scheduler's business here, and the store serializes writers anyway (`SetMaxOpenConns(1)`).
- **Idempotent by upsert:** re-running `refresh` re-upserts the same keys.
- **A cursor is a resume point, not a transaction:** `ingest` keeps a byte offset per JSONL file,
  so a crash mid-run re-reads from the last committed cursor.

## 4. Ingest reconciliation — `clens ingest --rebuild`

Same fan-out restricted to source B, with the cursor discarded. `--rebuild` exists precisely
because the cursor is an optimization that can be wrong: if a transcript is rewritten, the byte
offset is meaningless and only a from-zero pass is correct.

**Transaction boundary:** per event, via the same merge path as flow 2. A rebuild therefore
*merges* rather than duplicates, which is what makes `source_mismatch` reachable on a re-run.

## 5. Replay — the one route that spends money

```mermaid
sequenceDiagram
  participant U as user / dashboard
  participant R as api.replay
  participant P as internal/proxy Handler
  participant K as internal/consumer
  participant D as SQLite

  U->>R: POST /api/requests/{id}/replay
  R->>R: guard 1: replayEnabled?
  R->>R: guard 2: originReject (Origin/Host)
  Note over R: both guards run BEFORE anything is sent
  R->>D: read the original event
  R->>R: apply edits -> new payload
  R->>P: send through the LIVE handler
  P->>K: teed + capped + redacted like any call
  K->>D: INSERT events (replay_of, replay_edits set)
  R-->>U: the row the consumer just wrote
```

**Failure modes / retries / idempotency / transaction boundaries:**

- **Both guards run before a single byte reaches upstream.** That ordering is the point of the
  guard.
- **Replay sends through the live proxy handler**, not a transport of its own — so it picks up the
  same tee, body cap, and header redaction, and reaches SQLite through the consumer's single
  writer. A replay is therefore *not* a special write path.
- **Not idempotent, by nature** — it is a real billable call. `replay_of` links it to its original.
- **The cost gate is in the CLI, not the API** — the CLI is what knows whether `--yes` was passed.
- **Every rejection increments `replayRejected`**, visible on `GET /api/health`.

## 6. Quota calibration — learning a limit instead of inventing one

```mermaid
sequenceDiagram
  participant S as snapshot collector
  participant D as SQLite
  participant Q as internal/quota
  participant U as user

  S->>D: append quota_snapshots(utilization_pct, window)
  U->>Q: clens quota / GET /api/quota
  Q->>D: read snapshots + events in the window
  Q->>Q: burn at each snapshot's OWN instant (not now)
  alt a snapshot reached 100%
    Q->>U: candidate limit = the burn observed there
    U->>Q: confirm (never applied silently)
  else no limit known
    Q->>U: render "unconfigured"
  end
```

**Failure modes / retries / idempotency / transaction boundaries:**

- **`CrossCheck` uses each snapshot's own instant**, not `now`, so an old snapshot is compared
  against the window as it stood then.
- **A limit is never guessed.** `Projection.Configured` is false with no limit and the caller must
  render `unconfigured` — reading `UtilizationPct` as a real 0% is the failure this prevents.
- **Calibration proposes; the user disposes.** Candidates are offered for confirmation, never
  applied silently.
- **Read-only:** this flow writes nothing.
