[← INDEX](../INDEX.md)

# ADR 009: the proxy adopts the conversation id the request already carries

**Status:** Accepted

**Context:** The proxy already receives Claude Code's real conversation id on almost every request —
`x-claude-code-session-id`, present in 1,364 of ~1,400 stored request headers, 3 distinct values, all
3 real JSONL `sessionId`s — and already stores it verbatim in `req_headers`. It was never promoted
past a grouping key: `Resolve` (`internal/session/session.go`) always minted a fresh
`s_<unixMilli>_<hex>` as the row's `session_id`, using the header only to decide which in-flight
group a call joined. The result was two session sets that never overlapped — 184 minted proxy
`session_id`s, 0 JSONL sessions materialized at all (`SetSessionRecorder` was defined and never
called) — so `clens sessions` showed heuristic gap-window groups rather than conversations, and a
cross-source merge always landed in a proxy-only session with no counterpart on the JSONL side.

**Decision:** when `x-claude-code-session-id` (or the operator override `x-clens-session`, which
still wins when present) is on the request, that value **is** the row's `session_id` — `Resolve`
returns it directly instead of minting, checked ahead of the inactivity-gap logic. The gap-window
heuristic is now used only for the residual ~3% of calls with no session header at all. The JSONL
tailer is wired with a `SessionRecorder` (previously defined, never called), so JSONL sessions are
materialized too. Reading the header also stops disabling `PrefixHash` computation — a pre-existing
optimization ("the hash is not needed" once a header is present) that turned out to silently disable
two cache-timing analyzer rules for the ~97% of calls that carry the header; the hash is now always
computed, and `groupKey` (unchanged) still prefers the header for grouping.

*Rejected alternative:* keep minting `s_…` and rely only on the cross-source merge (D1/008) to fold
rows together. Rejected because a merge only fires when two rows share a key — it cannot make two
sessions the same session. Without this decision, a proxy row and its JSONL counterpart could merge
into one event row while their **sessions** stayed the two disjoint sets they always were, leaving
`clens sessions` no closer to showing real conversations.

**Consequences:**

- `clens sessions` shows the conversations Claude Code itself groups turns into, not proxy-only
  heuristic bursts — the 184 minted sessions collapse to the 3 real conversations this install's
  traffic represents, and the view grows by the JSONL sessions the recorder now materializes.
- **No merge-path rule changes.** `mergeEvents` still never rewrites `session_id`
  ([decision applies at the write, not the merge] — see
  [docs/context/workflows.md](../workflows.md) flow 2) — this decision changes which value a *writer*
  puts there, not what a merge does with it. It is also what makes that unrewritten-`session_id` rule
  correct rather than merely conventional: once both sources write the same conversation id, the
  survivor's session is a real conversation regardless of which source it came from, instead of
  depending on an ordering argument (the proxy row always arriving first) to avoid stranding a
  survivor in a session with no aggregate row.
- **No new capture and no new retention.** The header was already stored, verbatim, before this
  decision — reading it into `session_id` reads a value already on disk; nothing new reaches the
  database, and no protection in
  [data-privacy-and-compliance.md](../data-privacy-and-compliance.md) changes.
- `x-clens-session` stops being grouping-only and becomes the stored id: a client setting it to a
  label now sees that label in the session id column.
