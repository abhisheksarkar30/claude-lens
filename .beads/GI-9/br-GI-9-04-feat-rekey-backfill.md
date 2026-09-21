# Bead br-GI-9-04: `clens rekey` — three passes behind one command, the collision path on the factored `applyMergeTx`

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §3 D4 (with D3 and D7), §4
(the `merge.go`, `store.go`, `rekey.go`, `main.go`, `purge.go` and `cli_test.go` rows), §5
(`internal/cli/rekey`), §6 (the destructive act, the two silent shapes, the `replay_of` dangle, the
recorder cost), §9 bead 05 / **br-GI-9-04**

- **Bead ID**: br-GI-9-04
- **Priority**: P0 (critical)
- **Original Estimate**: 8h
- **Dependencies**: br-GI-9-02 (the consumer's identity rule and the sink rename), br-GI-9-03 (the
  JSONL key tier), **br-GI-9-07** (D7: pass 2 assumes the forward session rule D7 puts in place, and
  pass 3's re-ingest writes ids the old resolver would mint differently)
- **Blocks**: br-GI-9-05, br-GI-9-06

> **This is the story's only destructive command and its largest bead.** The forward beads
> (br-GI-9-01…03 and br-GI-9-07) must land before it runs anywhere but a dry run: **the key rule must
> land before the backfill runs**, or the re-ingest recreates the very keys the delete just removed.

> **A requirement list with a named trap, never a call sequence — deliberately.** Five plan-level
> rounds each found a defect one level deeper in a paragraph that described the rekey path's call
> order; round 10 dissolved the last of them by moving the paragraph from a procedure to a requirement
> list. The plan settles **what** and leaves **how** to this bead and its cross-review. Reproduce the
> requirements below; **do not add a step-by-step call list and do not hand-write a merge**.

## Description

`clens rekey` repairs the historical duplicates the forward fix cannot reach — forward-only would
leave an estimated 28,405 surplus rows and every double-counted total in place. It follows the
contract `docs/context/cli-and-tooling.md` states for a destructive subcommand:

- nothing happens without `--yes`;
- `--dry-run` prints what `--yes` would do and writes nothing;
- the three passes run in the order below, and each is reported separately, because a run that did
  one and failed another must not read as "done".

Three passes repair from genuinely different sources, which is why one command runs three of them
rather than a shared loop: pass 1 re-keys rows whose identity is inside `resp_body`, pass 2
re-attributes rows whose session is inside `req_headers`, and pass 3 deletes and re-derives the JSONL
rows whose identity was never stored at all.

### Pass 1 — the proxy half: re-key in place

Each row is processed in its **own transaction** (per-row commits are what make a partial run
resumable; nothing is atomic across a pass, deliberately). The value is already stored inside
`resp_body`, so these rows need no re-read.

1. Select `source='proxy'` rows whose `request_id` is **synthetic** — the predicate is
   `request_id LIKE 'proxy:%'`, and it must be the **prefix**, not "the body yields an id different
   from the row's key". A row keyed by a **header** value (tier 1) is indistinguishable from a
   body-id row by inspection, and rewriting it to its body id would silently split it from the JSONL
   row that keys on the **same** header value — converting D1's working Anthropic case into the
   non-merging case D1 exists to avoid. Restrict additionally to rows whose body yields an id.
2. **Read the id by the same rule the live path applies** — `parse.ExtractUsage`'s `Usage.MessageID`,
   i.e. D2's precedence (the `message_start.message.id`, or the non-stream top-level `id` gated on
   the body's own `type`, **first-seen wins**), applied to the decoded body. **This is a second
   id-extraction site**, and the plan's newest requirement: a hand-written pass-1 read can diverge
   from the live path and produce a **silent non-merge**. §5's rekey-level D2 cases are the only
   fixtures that can catch such a divergence — neither case appears on the measured corpus (0 bodies
   with two ids; 105 of 105 non-stream bodies `type: "message"`).
3. **The body must be decoded first** (`decode.Body`, as `processCall` does), and the scan must pass
   the config **`BodyCapBytes`** as its `limit` — the same value the live path passes. The limit is
   load-bearing twice over: a body that carries a `Content-Encoding` returns an **error** when
   `limit <= 0`, and the caller's `err == nil` guard then keeps the **undecoded** bytes — exactly the
   "silently re-keys nothing" outcome. **145 of the 724** proxy rows that have a body are compressed,
   and a scan that skipped decoding (or passed a non-positive limit) would silently re-key nothing
   for those rows while reporting success for the rest. A row whose body decodes to nothing belongs
   in **M** (the dry-run report below), not **N** — made visible rather than reported as re-keyed.
4. If the new key is free, re-key the row (`UPDATE events SET request_id = ? WHERE id = ?`).
5. If the new key is taken, the **collision path** below.

### The collision path — factoring the merge, never the load

`insertOrMerge`'s collision branch (`internal/store/merge.go:112-131` — `mergeEvents` →
`updateEventTx` → attach `source_mismatch`) becomes an unexported
**`applyMergeTx(ctx, tx, existing, incoming *Event)`** — **minus the load** — called by **both**
`insertOrMerge` and the rekey path, so the two callers cannot drift into two rules. That is the whole
requirement: *there is one merge rule and the rekey path uses it.*

**The load is the one thing the two callers cannot share, so it stays with each caller.**
`insertOrMerge` keeps `getEventByRequestIDTx(ctx, tx, ev.RequestID)` and calls `applyMergeTx(existing,
ev)`; the rekey path loads **by the target key** (`getEventByRequestIDTx(ctx, tx, targetKey)`) and
calls `applyMergeTx(taker, proxyRow)` — so its `existing` seat already holds the key.

> **Factor the helper; do not hand-write a merge, and do not call one with the arguments in the other
> seat.** Call `applyMergeTx` with the proxy row as the **`incoming`** and the taker — **loaded by the
> target key** — as **`existing`**, because it already holds the target key, and **nothing is
> re-keyed**. The load is the caller's; the helper performs none. All three shapes below compile, read
> correctly, and fail silently, so a hand-written version is a regression even when its assertions
> pass:
>
> - **The swapped call.** `mergeEvents` never assigns `RequestID` (`merged := *existing`,
>   `merge.go:147`), so a call that puts the proxy row in the `existing` seat returns the proxy row's
>   **old synthetic key**, `updateEventTx` writes that key straight back (it binds `request_id`
>   first), and deleting the taker then leaves **no row holding the message id**. The run reports
>   success and a **second run then "works"** — the failure looks like the promised no-op and its own
>   correction looks like the first run. With the taker as `existing` the message id is never in
>   question, so this is unreachable *by construction* — which is the argument for factoring rather
>   than describing. **§5's survivor-`request_id` assertion catches this shape.**
> - **A helper that loads by `incoming.RequestID`** — the other mechanism, and a different failure.
>   It finds the proxy row under its own `proxy:` key, merges it into **itself** and lets the caller
>   delete it: the taker is untouched and **still holds the message id**, so nothing is orphaned;
>   what is lost is the proxy row's **content** (its token and cost columns, the `source_refs` union,
>   its `prefix_hash` and replay linkage), permanently, and because **no key is wrong** a second run
>   changes nothing — the failure is *worse* than the first shape and the `request_id` assertion is
>   **blind** to it. **§5's `source_refs` *set* assertion is what catches this one** — **not** the
>   row-count assertion: both silent shapes drop exactly one row, so the row-count half of that
>   claimed pair is **inert**.
> - **The union.** "Merge the two rows" read as a content **union** produces a row no live merge
>   could produce. `mergeEvents` is a **winner pick plus a per-column backfill**: the six token
>   columns, `cost_usd` / `api_equivalent_cost_usd` / `cost_source`, `stop_reason` / `stop_category`,
>   `service_tier`, `speed`, `model_resolved` and `billing_mode` all come from **one** side
>   (`merge.go:166-206`), `capture_complete` is the OR (`:223`), and the columns one side structurally
>   cannot supply are backfilled per column (`:229-326`) — **`source_refs` is the only column actually
>   unioned**. The helper inherits that rule unchanged, so this is true by construction too; the
>   warning stands against hand-writing it.
>
> **The winner pick is a three-part sequence, and the helper inherits all of it** because it **is**
> `mergeEvents` rather than a copy: (1) the complete-frame switch (two complete captures →
> `incoming`, `merge.go:167-180`); (2) the never-observed-usage override (`:196-206`) — a winner
> carrying no observed usage loses the pick to the other side, so a row's zero is "never looked", not
> a measurement; (3) the empty-`billing_mode` derivation (`:272-288`) — when the winner's mode is
> empty, the label follows the winner's cost column (`api` when its `cost_usd` is non-NULL,
> `subscription` when its `api_equivalent_cost_usd` is). A paraphrase drops them, and a stop after
> (1) or (2) ships a defect.

**`applyMergeTx`'s contract is a short requirement list**, because the rekey path is each item's
second caller and every one is **silent** when missed:

- **It performs no load.** The caller supplies `existing`, and on the rekey path that is the row the
  target key names, so the survivor already holds the key.
- **The survivor is the taker's row, by its `id` and `session_id`** — never the proxy row's. The
  taker is whatever holds the key (`getEventByRequestIDTx` filters on `request_id` alone,
  `merge.go:82-84`), and after D1 that is usually the **JSONL row** — the ordinary case, not a
  corner, because the JSONL row for a request is written minutes after the proxy row for it. (The
  deleted set's bead required "the surviving row is the proxy row's `id` and `session_id`"; that is
  the **swapped call** this bead forbids, and it is unimplementable through `mergeEvents`.)
- **`total_prompt_tokens` is re-derived from the survivor's four prompt columns in the same write.**
  It is a derived column the live paths recompute every time (`merge.go:214`, `store.go:261`, `:299`),
  so a helper that copies the winner's six columns and leaves the survivor's stored total ships a row
  whose `total_prompt_tokens` contradicts its own columns — the invariant CLAUDE.md calls "a tested
  invariant, not a convention".
- **The empty-`billing_mode` derivation is part of the pick** (sequence item 3), reachable because
  `billingModeForAuthKind` returns `""` for an unclassified `auth_kind`. An implementer who copies
  the winner's empty mode ships a row whose cost column and mode column disagree, and an empty mode
  matches none of the three aggregates' `CASE WHEN billing_mode = …` branches, so the row silently
  drops out of every cost total — the code's own comment calls the naive alternative "worse than the
  defect".
- **The deleted row's warnings are re-attached to the survivor** (upsert by `(event_id, kind)`).
  `warnings.event_id` is `ON DELETE CASCADE` (`schema.sql:93-102`), so **the deleted row's** warnings
  vanish with it; `insertOrMerge` re-computes the *arriving* side's warnings onto the survivor, so
  the rekey path re-attaches the deleted row's too, so the two paths end with the same warning set.
  **The re-attach lives in the caller (`store.go:1677-1682`), not in `applyMergeTx`** — the operation
  is defined on the row that gets *deleted*, and `insertOrMerge`'s incoming side is a freshly built
  `Event` that was never inserted, so it structurally holds no `warnings` row to carry
  (`store.go:1713-1717`). Reachable kinds on a JSONL
  row are narrow but not zero: `source_mismatch` from an earlier `insertOrMerge`, and the tailer's
  `peak_pricing`.
- **The `incoming` row's `prefix_hash` / `replay_of` / `replay_edits` are carried onto the survivor
  when the survivor's is NULL/empty** (`preferNonEmpty`; a no-op on the live path, where `incoming`
  is the JSONL row and is nil for all three). `mergeEvents` assigns none of the three (`merged :=
  *existing`), so on the rekey path the merge would **drop** the proxy row's hash and replay linkage
  — **durably**: pass 3's re-ingest is a JSONL row, nil for both, and no later merge assigns them.
  **The hash is why it matters**: the two hash-keyed session rules `continue` on a nil one
  (`rules.go:416`, `:444`), so the drop would defeat D7 change 4's fix on exactly the rows the
  backfill merges — the newly computed hash would die in the merge that consumes it.
- **The survivor keeps its own seat's `started_at` / `source` / `first_source` — settled, not open.**
  `mergeEvents` assigns none of the three (`merged := *existing`, `merge.go:147`), so the **seat**
  decides: the JSONL taker's values survive on the rekey path, the proxy row's on the live path.
  `applyMergeTx` must **not** special-case them — an "earlier of the two" rule would make the helper
  diverge from the live path **by rule** on a seat-dependent column. A bounded, stated consequence of
  the seat, not a rule change; `first_source` is rendered (`store.go:1261`) and `started_at` orders
  `clens ls`.

**The reconcile set is the distinct sessions among the two rows, and for a pass-1 collision it is
always two.** Both sessions can move: the absorbed `incoming` row is deleted outright — removing a
row leaves its session's totals high — and the merge copies the **winner's** six token columns onto
the survivor (where the winner can be either side), leaving the survivor's session wrong. So
re-derive the **set**, not one:

- The **deleted row is a *historical* proxy row**: pass 1's predicate selects only pre-D7 rows (a
  post-D7 `proxy:`-keyed row is by construction one whose body yields no id, D2's precedence), and
  before D7 `Resolve` minted **unconditionally** — the only header that ever reached it was
  `x-clens-session`, and it never became the id (`session.go:58`, `:80-84`, `meta.go:38-40`). So that
  row's session is **always** a minted `s_…`.
- The **taker holds the target key** (a body id), which no pre-D1 row can hold — most often it is the
  **JSONL** row, whose `session_id` is the transcript's conversation id, and a minted `s_…` never
  equals a conversation id.
- **Use `reconcileSessionTx`, not `ReconcileSession`.** `ReconcileSession` (`store.go:672-682`) opens
  its **own** `BeginTx`, and the pool is pinned to a single connection (`db.SetMaxOpenConns(1)`,
  `store.go:72`). Calling it from inside the per-row transaction leaves the outer transaction holding
  the only connection and the inner `BeginTx` waiting on the pool with **no deadline** — a **hang**,
  not an error, and one no test reports as a failure rather than a timeout. `reconcileSessionTx`
  (`store.go:684`) is unexported and takes a `*sql.Tx`, which is why the rekey methods must live in
  `internal/store`.
- **Then remove the old `sessions` row once re-deriving shows it empty** — the same clause pass 2
  carries, and the same reason. The merge takes the deleted proxy row out of its own minted `s_…`,
  and nothing later removes it: `reconcileSessionTx` only `UPDATE`s and never deletes a `sessions`
  row, `ListSessions` takes no `request_count` filter, and the row is user-visible in both
  `clens sessions` and `/api/sessions`. `clens purge` already leaves zero-row `sessions` rows, so
  this is consistency with pass 2, **not** a new invariant — but a command whose sibling pass forbids
  the artifact must not leave it.

**And any `replay_of` naming the deleted proxy row is re-pointed to the survivor — settled, not
open.** The requirement is that **no `replay_of` names a row that no longer exists**. This collision
is the run's **only** delete site that can dangle a reference: a `replay_of` can only name a
**body-carrying** row (it is written only from the replay route's `ReplayMeta.Of`, `proxy.go:133-136`,
`:252` → `consumer.go:430`, and that route refuses an original with no stored body,
`api/replay.go:86-88`), and pass 3 deletes only `jsonl:` keys, which never carry a body. Where a
deleted id **is** referenced, re-point the reference to the **survivor** — a replay's original still
logically exists as the merged survivor, so the lineage is preserved (the row keeps `replay_edits`)
rather than lost. The reverse lookup is **unindexed** (`replay_of` carries no index, `schema.sql:68-70`;
the filter is `replay_of = ?`, `store.go:500-505`), so it runs **once per run over the set of deleted
ids**, never per row.

The two settled points this section carries — the survivor's seat columns are the taker's, and the
`replay_of` re-point is to the survivor — are decisions, not open questions. Do not re-open either.

### Pass 2 — re-attribution: the historical proxy rows adopt their conversation id

Pass 1 is about *keys*; this pass is about the *session*. It exists because the forward fix (D7) only
affects rows written after it lands — every proxy row already in the table still carries a minted
`s_…`. Each such row's true conversation id is **already on disk** in its own stored `req_headers`
(`x-claude-code-session-id`, D7), so this pass needs **no new capture and no new retention**: it reads
a field the system already keeps. Each row in its own transaction.

1. For each `source='proxy'` row, read its `session_id` and its `req_headers`, resolving the session
   header by the **same precedence and the same length rule the live `ExtractMeta` applies**:
   `x-clens-session` is read first and `x-claude-code-session-id` fills the value only when the former
   is **absent** (the operator override still wins, exactly as D7 change 1 settles for the live path);
   and a header longer than `maxSessionHeaderLen` is treated as **absent** — the "no such value" case,
   not a second one — so the row keeps its minted `s_…` and is counted in **L**, not reported as
   re-attributed. The pass mirrors the live **length** and **precedence** rules deliberately: it
   exists to reproduce what the live path stores, and a divergence would reintroduce the split class.
2. **`UPDATE events SET session_id = ? WHERE id = ?`** — a direct write, and it **cannot route
   through the merge**: `mergeEvents` never rewrites `session_id` and **that stays true** (D3),
   because this pass is *re-attributing* rows, not merging them. **This is the one write path in the
   story that sets `session_id` on an existing row** — stating it here is what stops a later reader
   concluding D3 was relaxed.
3. Reconcile **both** sessions through `reconcileSessionTx`: the old `s_…` (a row left it) and the new
   conversation id (a row joined it, so **upsert** it first — after pass 1 the conversation session
   may not exist yet). Then **remove the old `sessions` row once re-deriving shows it empty**.

The upsert is not incidental: pass 2 can run before pass 3 has re-ingested anything, so nothing has
created the conversation session yet, and a bare reconcile against a missing row would leave the
aggregate unrepresented.

### Pass 3 — the JSONL half: delete, then re-ingest

A stored JSONL row cannot be re-keyed in place: its old key holds a uuid and the message id it
*should* hold was never stored, and the only place that value exists is the transcript. Delete-and-re-
ingest is also what collapses §2.4's surplus — an in-place re-key would have to reimplement that
collapse, which is the reason this half re-ingests at all.

0. **The precondition is checked first, before pass 1, and a failure is a refusal — settled, not
   open.** Pass 1 does not touch `jsonl:` rows, so the check is valid before any mutation. The check
   is over **every recorded `jsonl:<path>` cursor** (`cursor.go:18`): the file exists and its current
   size is not below the recorded byte cursor. The schema carries **no row→file link** — `events` has
   no transcript-path column (`path` is the request URL path, proxy-only) and the key embeds a
   `sessionId` and a `uuid`, not a path — and the mapping is not a function anyway: a sidechain row
   takes the **parent's** `sessionId`, so one session id owns the parent file *and* every subagent
   file. **A recorded path with no readable file is a refusal, not an exclusion** — the delete is
   unconditional over the `jsonl:` prefix while the walk is root-scoped (`resetJSONLCursors` walks
   `root` and zeroes only the `.jsonl` files it finds, `ingest.go:135-145`), so excluding a cursor
   whose file has been deleted would destroy rows nothing can rebuild. **The root-change case is also
   a refusal**: a row whose transcript lives outside `jsonlRoot()` would be deleted and never
   re-read. A failed precondition aborts the command with **no rows changed**, a **non-zero exit**,
   and `--dry-run` reports the same check and the same refusal. The one residual case the upfront
   check cannot close is a **mid-run race** — a file becoming unreadable between the check and pass 3
   — which leaves passes 1–2 committed; that race is **recorded, not mitigated** (§6) and is **not
   §5-testable**, so a green §5 must not be read as covering it.
1. Delete `source='jsonl' AND request_id LIKE 'jsonl:%'` — **one set-based `DELETE`, committed in one
   transaction**. It is one statement, so there is no per-row model to state, and any `replay_of`
   re-point commits inside that same transaction so no reference is briefly dangling. (No `replay_of`
   can name a `jsonl:`-keyed row, so on this pass there is nothing to re-point — the re-point belongs
   to the collision path above.)
2. Zero the byte cursors and re-run the tailer — the mechanism `clens ingest --rebuild` already owns.

A crash between the delete and the re-ingest leaves the `jsonl:` rows deleted and nothing rebuilt;
they are **re-derivable** (the precondition above), so a re-run rebuilds them — which is why this
pass needs **no per-row resumability of its own**, unlike passes 1 and 2.

**Re-ingest re-prices, and the command's contract must say so.** The tailer prices each row as it
writes it, against the price table in force **now** — not the one in force when the row was first
ingested. So this half does not merely re-key: it restates roughly 70k rows' `cost_usd` /
`api_equivalent_cost_usd`, and `cost_source` moves with it wherever the table changed; it can also
flip `billing_mode`. A total that moves after the backfill is **not** evidence the backfill corrupted
anything — it is the same rows priced by a newer table, and `--dry-run` should say the run re-prices
rather than implying it only re-keys. The proxy half does **not** re-price (it re-keys rows whose
cost was computed at capture time). Rows with a real `requestId` are untouched and re-insert
idempotently, because re-reading them produces the same key and merges.

### Ordering: pass 1, then pass 2, then pass 3

**Fixed for the reconcile set and for determinism, and it decides nothing about the *end state*.** The
survivor is the taker, and its row, key and session are the taker's whichever order runs. The
sequence **does** decide the reconcile **set**: pass 1 runs before pass 2, so the row pass 1 deletes
has not been re-attributed and still carries its minted `s_…`, which is why that collision's set is
always the two sessions named above rather than one.

The reason the ordering *was* argued for — proxy half first so the survivor keeps a session with a
materialised aggregate — is **dead** with the asymmetry D7 removes, and it is **dropped rather than
re-justified**: there is no surviving reason. **There is no taker-source branch** — D7 deletes the
premise that a JSONL survivor lands in an aggregate-less session — and the fixed sequence is kept
**only** because a stated order makes the run's output reproducible and its report readable.

*(The deleted set's bead carried the pre-D7 ordering section — "the ordering rule … must not be
flattened", "step 3 branches on the taker's source", "only proxy sessions have a materialized
aggregate". All three are superseded by D4/D7 and must not be reproduced.)*

### The command's shape and its report

- `--yes` is required to write; `--dry-run` is the safe path, writes nothing, and deletes nothing as
  a side effect of measuring.
- **`--dry-run` prints all three passes' counts plus the re-pricing, re-derivability and
  dangling-`replay_of` notes the operator needs** — and **no row-level output**.
- It must report **"would re-key N, would leave M synthetic (no body id), would re-attribute K, would
  leave L unattributed"**. **4 rows with `resp_body IS NULL`** plus **61 whose body yields no id** stay
  synthetic forever, and the operator needs to see that number before the run. **K and L are pass 2's
  pair**: rows whose `req_headers` carry `x-claude-code-session-id` are re-attributed (**K**), and
  rows whose headers lack it — **including the overlong case, treated as absent** — keep their minted
  `s_…` and are counted in **L**. §5 asserts the **exact** N/M pair, not merely that both are
  non-zero.
- **`--dry-run` reports the dangling count** — rows whose `replay_of` names an id that does not exist
  — **not** the referrer count `replay_of <> ''`, which measures a different population and is the
  wrong number. **Both are zero** on the live database (measured 2026-09-21 against `~/.clens/lens.db`,
  95,603 events): `replay_of` is `''` on every row and NULL on none, so **the replay feature has never
  been exercised there** and the collision path's re-point has **no live data behind it**. **§5's
  seeded third row is therefore the only thing that can ever exercise the re-point** — it is
  **load-bearing, not incidental**: no acceptance run on this DB distinguishes a correct re-point from
  an absent one.
- **The JSONL half must report deleted-vs-inserted**, and that report is the run's own evidence for
  how much duplication there was. It is **not** §2.4's 28,405: §2.4 estimates duplication by grouping
  on `(session_id, token quintuple, 5s bucket)`, while this half deletes and re-inserts whole rows,
  so its ratio counts one row per collapsing key and runs larger (§5).
- Operationally: run it with `clens serve` **stopped**, because a live tailer writing while cursors
  are zeroed is a race the command does not need to have. The selector's prefix `LIKE` **cannot use
  the UNIQUE index** (SQLite will not use it for a prefix `LIKE` under the default `BINARY`
  collation), so the delete scans all ~87k rows — fine for a one-off, and it should **not** be
  "optimized"; do not rewrite the predicate.

### What this bead does not do

- **No merge-path change, and no store bead for it**: §2.6 and D3 show the live merge path needs no
  change — only its collision branch gains a second caller, and the live call keeps today's shape.
- **No schema change and therefore no migration**: every column exists.
- **No read-time dedup view** (D5).

## Rationale

Forward-only would leave §2.4's surplus in place and every historical total double-counted, so the
story carries the backfill. This is the only **non-re-derivable** step in the story: the stored
`resp_body` is the sole source for pass 1, so a botched re-key cannot be replayed from anywhere else
— which is why the design has no heuristic in it, and why the collision path is a requirement list
with a trap rather than a procedure.

**Sweep for sibling copies.** The plan repeatedly fixed a claim in one place and left a copy of it
elsewhere (F14.1, F19.1 — the `source_refs`-vs-row-count guard shipped a live copy after a fix, and
the two collision-failure consequences shipped three). Before this bead is called done, **grep the
moved, renamed and deleted symbols, and the corrected claims, across the repo** rather than trusting
the one site the description names. (The editorially-independent docs sweep belongs to br-GI-9-05;
this instruction is about the code this bead moves.)

## Outcome Definition

- `clens rekey` exists and is registered in `cmd/clens/main.go`; without `--yes` it refuses and
  changes nothing.
- **`cmd/clens/main_test.go`'s `carriedOver` names `rekey`, and
  `TestEveryCarriedOverCommandIsDispatched` passes** — the dispatch map and that list are the same
  size, so registering the command without extending the list is a red test, not a passing one. The
  list is the side that moves; the length assertion is correct and stays.
- `--dry-run` reports N/M (pass 1), K/L (pass 2), the dangling-`replay_of` count, and the re-pricing
  note, and writes nothing.
- Pass 1 re-keys each `proxy:`-keyed row whose body yields an id (read by
  `parse.ExtractUsage`'s `Usage.MessageID`, decoding with the config `BodyCapBytes`); a collision
  merges into the taker via `applyMergeTx`, deletes the proxy row by its own id, reconciles the
  **two** sessions, removes the emptied `sessions` row, and re-points any `replay_of` naming the
  deleted row to the survivor.
- **The survivor is the taker's row** — same `id`, same `session_id`, `request_id` unchanged.
- Pass 2 re-attributes each proxy row from its stored `req_headers` (live precedence, live length
  bound), upserts the new session, reconciles old and new, and removes the emptied old `sessions`
  row.
- Pass 3 checks its precondition **before pass 1** and refuses with a non-zero exit and no mutation
  on failure; otherwise it deletes `request_id LIKE 'jsonl:%'` in one transaction, then zeroes the
  cursors and re-ingests.
- `applyMergeTx` is `insertOrMerge`'s collision branch **moved**, not copied: `insertOrMerge` and the
  rekey path call **one** function.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

All of `internal/cli/rekey_test.go` unless stated. **Name §5's cases by name, never by a plan line
range** — the plan's line-number self-pointers were corrected three times and got the number wrong
twice, so a line range in this bead would be a pointer that does not survive the plan's next edit.

- **Both fixtures seed the pre-fix state by hand.** The backfill's whole input population predates the
  fix and no forward path writes it: after D1 the tailer keys a line by its `message.id`, and after D2
  the consumer keys a row by its body id — so ingesting today's fixture produces **zero**
  `jsonl:`-prefixed rows and no synthetic proxy row. Write the stored rows directly with explicit
  `request_id` values (`jsonl:<sessionId>:<uuid>` for **both** duplicate rows, so the collapse drops a
  row and the deleted count is 2→1 not 1→1; `proxy:<sha256>:<ns>:<attempt>` for the proxy half).
- **`--dry-run`** deletes nothing and asserts the **exact N/M pair** — N = the rows whose bodies yield
  an id, M = the rows that do not (including a `resp_body IS NULL` row and an error-body row) — so
  "would leave M synthetic" is **checked**, not merely promised.
- **`--yes`** re-keys a synthetic proxy row to its body id.
- **The proxy half's remaining obligations each get a case, because each failure is silent:**
  - a **gzip-encoded** stored `resp_body` carrying its `Content-Encoding` header is re-keyed to its
    body id; **the same fixture with a zero/invalid limit, or with the `Content-Encoding` header
    absent on an encoded body, stays synthetic and is counted in M** rather than reported as
    re-keyed;
  - a row **keyed by a header value** whose body carries a **different** id is asserted **unchanged**
    by the run — this is the `proxy:`-prefix predicate, pinned against a "the body yields a different
    id" rewrite;
  - the **surviving row's `total_prompt_tokens` equals the sum of its own four prompt columns**,
    asserted **on the row** (a session-aggregate assertion reads `total_prompt_tokens`, so a damaged
    row can satisfy it by deriving the expectation from the same damaged rows);
  - a collision whose picked side carries **all-zero usage** while the other side does not asserts the
    figures follow the **observed** side — the never-observed-usage override the two-case switch does
    not express;
  - two proxy rows carrying the **same body id** and different `started_at` collapse to **one** row,
    asserted and recorded so the collapse is a decision rather than an accident (a shared id loses a
    call, and this fixture is what makes that visible);
  - **the two D2 cases pinned at the rekey level, not only in `internal/parse`**: a body with **two**
    `message_start` frames re-keys to the **first** id, and a body whose `type` is **not** `"message"`
    but carries a top-level `id` re-keys to **nothing** (the row stays synthetic and is counted in
    **M**).
- **Both collision shapes.** The proxy-vs-proxy fixture pins the reconcile, and it is the shape the
  measured corpus does not produce (a shared body id reaches it only across the D1 boundary). The
  shape that **will** happen is a **JSONL taker**: a proxy row whose body id is already held by a
  `jsonl`-sourced row. Both shapes have **two** sessions, because the row a pass-1 collision deletes
  is always a *historical* proxy row carrying a minted `s_…`. The JSONL-taker fixture: one
  `proxy:`-keyed row in its **own minted session** (its own `sessions` row) with a body carrying a
  `message.id`, and one `jsonl`-sourced row carrying that same id in the **conversation session**.
  Assert:
  - the surviving row is the **taker's** row: `id` unchanged, `session_id` the JSONL seat's
    conversation id (not the proxy row's minted `s_…`), and `source_refs` holds **both** sources —
    asserted **order-agnostically** (as a set `{proxy, jsonl}`) or as the seat's order
    `["jsonl","proxy"]`, because `unionStrings` seeds the union from the `existing` seat first
    (`merge.go:148`, `:350-366`), so the literal `["proxy","jsonl"]` is unsatisfiable here;
  - **the seat columns the merge must never touch**: give the two seats **different** `started_at`
    values (and a differing `source`) and assert the survivor holds the **JSONL seat's**
    `started_at` / `source` / `first_source` — so a rejected "earlier of the two" rule, or a copy of
    `incoming`'s seat columns, goes red;
  - **`request_id` on the surviving row is the target key** (the body's `message.id`) — the taker's
    own key, so the value is **unchanged**, and that is the point: the proxy row is the `incoming`
    and nothing is re-keyed. Assert the value directly, and assert it **before** the second run — a
    silent no-op's signature is a first run that changes nothing and a second run that works;
  - the **proxy** row (the `incoming`) is **gone** (row count drops by one);
  - **a `replay_of` naming the absorbed proxy row's id re-points to the survivor**: seed a third row
    whose `replay_of` names the **deleted proxy row**'s id and assert that after `rekey --yes` it
    names the **survivor** (the JSONL taker's row) — not the deleted id, and not cleared. **This
    fixture is the only thing that can ever exercise the re-point** (both counts are zero on the live
    DB), so it is load-bearing;
  - **two** reconciles cover the collision: assert the **survivor's** session's post-merge aggregate
    **and** that the minted `s_…` session is **reconciled to empty and then removed** (a one-reconcile
    implementation passes the survivor assertion while leaving the emptied `s_…` holding the absorbed
    row's figures — a ghost in `clens sessions`);
  - **pin the token columns so the aggregate assertion can fail**: both sides `capture_complete` with
    **different** token columns, so the winner pick actually moves figures;
  - **a `source_mismatch` warning is attached when the two sides disagree on tokens** — the helper
    owns this decision, so a helper that omits it merges silently, and §6 tells the operator to expect
    a handful of these warnings **from the run**;
  - **the winner's empty-mode derivation is pinned**: a collision whose picked side has an empty
    `billing_mode` and a non-NULL cost column leaves the survivor's mode matching that column (`api`
    for `cost_usd`, `subscription` for `api_equivalent_cost_usd`) rather than empty;
  - **the deleted row's warnings survive**: the **`incoming`** row carries a warning, the collision
    removes it, and the survivor still holds it, asserted by its `event_id` being the **survivor's**
    (seeding the warning on the survivor instead would pass with no re-attach code at all);
  - **the two sides agreeing on tokens but differing on `cost_source` / `billing_mode` raise no
    warning** — `tokensDiffer` is the sole operand of `source_mismatch`, so this ordinary pair is not
    flagged — **and the survivor ends on the `incoming` seat's values**. Assert the **state the
    command leaves**, not the one it passes through: run the *same* request through **both** paths (a
    proxy capture of the id plus a real transcript naming it) and assert the two survivors' `cost_usd`
    / `api_equivalent_cost_usd` / `cost_source` / `billing_mode` **and the aggregate each joins**
    (`store.go:708-709`) **agree**. And assert the rekey survivor's **`prefix_hash` is non-NULL and
    `ruleCacheExpiredBetweenTurns` still evaluates it** (`rules.go:416`);
  - a **second** `rekey --yes` changes nothing — asserted after the `request_id` bullet rather than
    trusted.
- **The JSONL half is the destructive one and needs its own case.** Build it from the fixture
  `TestIngestRebuildRereadsWithoutDuplicating` already establishes (`withHome(t)`'s temp
  `~/.claude/projects/proj1/session1.jsonl` + `runIngest`). Seed the pre-state by hand: two
  `jsonl:`-keyed rows for the **duplicate** pair (same `message.id`, different `uuid`, no
  `requestId`) plus one row keyed by the real `requestId`, and leave the transcript on disk carrying
  those same lines. Run `rekey --yes`, and assert: the `jsonl:`-prefixed rows are **gone**, the
  duplicate collapsed to **one** row keyed by the `message.id`, the `requestId` row survived, and the
  transcript file is **still on disk** (it is the re-ingest source, not how the pre-state arises).
  Then a `--dry-run` on the same fixture deletes nothing.
  - **Pass 3 leaves a reference to a surviving row untouched**: seed a third row whose `replay_of`
    names a surviving `proxy:`-keyed row; after `rekey --yes` assert that row's `replay_of` is
    **unchanged** (neither cleared nor re-pointed). **And assert the `--dry-run`'s reported number is
    the dangle count** — zero on this fixture — **not** the `replay_of <> ''` referrer count;
  - **the re-ingest re-prices, so assert a cost**: wire the fixture's tailer with a **price table**
    (the way `newTailer` does) and a transcript line that table prices, then assert the collapsed
    row's `cost_usd` / `api_equivalent_cost_usd` (or at least its `cost_source`) equals the table's
    value after `rekey --yes` — a rekey that builds its tailer without the price table blanks those
    columns while every other case still passes;
  - **the precondition is stated over the cursors, not over rows**: seed a `jsonl:`-keyed row **and
    its cursor**, make the transcript file **missing** (or shorter than the recorded cursor), **and
    seed one `proxy:`-keyed row the run could re-key** — assert the run **refuses** (non-zero exit,
    **no rows changed**, checked **before pass 1**). The `proxy:` row is what makes the ordering
    failable: a jsonl-only fixture is invisible to passes 1–2, so an implementation that checks the
    precondition *after* pass 1 still satisfies "no rows changed" and stays green; asserting the
    `proxy:` row's `request_id` is **still** `proxy:…` after the refusal is the only thing that fails
    when the check runs late. **And the root case**: seed a recorded cursor whose path is **outside**
    the walk root and assert the run refuses;
  - **report the run's wall clock, beside the conversation's row count** — a **reported measurement,
    not a gate**. A wall-clock bound is flaky, and the wiring can only make the run **faster** where
    it is absent, so no bound can fail on that absence; the wiring's *existence* is gated by
    br-GI-9-07's tailer-wiring case. (This case measures the re-ingest's own cost: `newTailer` wires
    no `SetSessionRule`, so it does not pay the O(n²) session-rule pass, whose real exposure sits on
    the proxy `flush` path this fixture never exercises.)
- **Pass 2 gets its own case**, because it is the one write path that sets `session_id` on an existing
  row. Seed two `proxy:`-keyed rows whose `req_headers` carry `x-claude-code-session-id` but whose
  `session_id` is a minted `s_…` (each with its own `sessions` row), plus one proxy row whose headers
  **lack** the header, plus one whose `x-claude-code-session-id` is **overlong**, **plus one whose
  headers carry both `x-clens-session` and `x-claude-code-session-id`**, **plus one whose
  `x-clens-session` is overlong while its `x-claude-code-session-id` is valid** (the one combination
  where the fill-when-absent rule and the length rule interact: the overlong override does not supply
  the value, so the second header does). Run `rekey --yes`, and assert: the first two rows'
  `session_id` is now the header value; **the overlong-override row's `session_id` is its
  `x-claude-code-session-id` value, not its minted `s_…`** — an implementation that tests presence
  with `headers.Get("x-clens-session") != ""` satisfies the both-present and overlong-alone rows and
  then keys on a value the bound exists to reject; **the both-present row's `session_id` is the
  `x-clens-session` override, not the conversation id** — the precedence pass 2 mirrors from the live
  path; the conversation session exists and its aggregate equals the post-run row set (the **upsert**
  case — nothing created it beforehand); the emptied `s_…` `sessions` rows are **gone** so
  `clens sessions` shows no ghosts; and **both** the header-less row and the overlong-header row keep
  their minted id and are counted in **L**. A `--dry-run` reports the K/L pair without writing.
- **Integration.** A JSONL line with no `requestId` and a proxy capture of the same request, ingested
  through the real paths, produce **one** row — the end-to-end statement of the whole story, and the
  one that fails today. **Assert the row's `session_id`, not the row count** (the count alone passes
  if the two rows unified under the **wrong** id), and add the **subagent-sidechain variant**: a
  `…/<sessionId>/subagents/agent-*.jsonl` line takes the **parent's** `sessionId`, so a sidechain line
  merged with its parent's proxy capture must land in the **parent** conversation. (The equivalent
  assertion in br-GI-9-07 must stay consistent with this one rather than duplicating it.)
- **And the accepted split, which nothing exercises today, with its counterpart.** (a) A seeded JSONL
  line carrying a `requestId` (tier 1) paired with a proxy capture whose body yields the same
  `message.id` but whose stored `resp_headers` hold **no** `request-id` (tier 2) must leave **two**
  rows — D1's named, accepted exception, asserted so it is visible rather than silent. (b) The split
  alone is not enough: a suite that only exercises the exception cannot fail if the header tier
  silently stops **meeting**, which is the behaviour D1's argument for that tier protects. So the
  **meeting** case needs its own fixture: a proxy capture whose `resp_headers` carry `request-id: X`
  and whose body carries a **different** id, paired with a JSONL line whose `requestId` is `X`, must
  produce **one** row whose `request_id` is **`X`** (the header value, not the body id), and a later
  re-key must leave that row alone (the `proxy:`-prefix predicate).
- Unit Tests: all cases above live in `internal/cli/rekey_test.go`, which is where the rekey store
  methods' only caller lives. **No `internal/store` merge-path test**, because there is no merge-path
  change.
- E2E: none.

## Files to Touch

- `internal/store/merge.go` (modify — factor `insertOrMerge`'s collision branch into an unexported
  `applyMergeTx(ctx, tx, existing, incoming *Event)` — `mergeEvents` → `updateEventTx` → attach
  `source_mismatch`, **minus the load**; `insertOrMerge` keeps its load and calls it; the live call is
  otherwise unchanged)
- `internal/store/store.go` (modify — **all three passes' store methods**: pass 1's body-id scan (the
  `proxy:` prefix predicate; `decode.Body` with the config `BodyCapBytes`; the id read by
  `parse.ExtractUsage`'s `Usage.MessageID`), the in-place re-key, and the collision path — load the
  taker by the target key, `applyMergeTx(taker, proxyRow)`, delete the proxy row by its own id,
  reconcile the **set of two** via `reconcileSessionTx`, remove the emptied `sessions` row, re-point a
  referencing `replay_of` to the survivor; pass 2's re-attribution; pass 3's delete + re-ingest. **And
  rewrite `SessionEvents`' doc comment** — its "a session's row count is already bounded by the
  session resolver's own gap window" is made false by D7, so the comment records that no bound
  replaces it)
- `internal/cli/rekey.go` (new — the command, `--yes` / `--dry-run`, all three passes in order, the
  N/M/K/L report, the dangle count and the re-pricing note)
- `internal/cli/rekey_test.go` (new — every case in §5's rekey section, as enumerated above)
- `cmd/clens/main.go` (modify — register `rekey`)
- `cmd/clens/main_test.go` (modify — **a registration is not complete until this file moves with
  it.** `TestEveryCarriedOverCommandIsDispatched` asserts the dispatch map **in both directions**:
  the loop checks each name in `carriedOver` resolves to a non-nil entry, and then
  `len(commands) != len(carriedOver)` fails the test if the two sets differ in **size**. `rekey`
  makes that 19 against 18, so the test goes red on a correct registration — with a message about
  counts, not about anything the implementer changed. Add `"rekey"` to the `carriedOver` list and
  take its comment's "the 18 subcommands" to 19. **Do not resolve it by deleting the length check**
  (it is what notices a key that exists but points at nothing, which would panic at dispatch rather
  than at build) **and do not resolve it by leaving `rekey` out of the map** (then `clens rekey` is
  reported unimplemented). The count check is correct; the list is what is stale.)
- `internal/cli/purge.go` (modify — its doc comment says "This is the one command in the CLI that
  destroys data"; `rekey` is the second, so it becomes the two-cases wording)
- `internal/cli/cli_test.go` (modify — add `rekey` to the **credential-containment** map in
  `TestNoCommandPrintsACredential`, **not** the sibling `cases` slice in
  `TestEveryCarriedOverCommandRunsAgainstATempStore` — that is an **11**-entry set for the GI-1
  carried-over commands, and `rekey` is not one of them; adding it there would misdescribe the set
  rather than extend it. `TestNoCommandPrintsACredential`'s map carries **no count assertion**, so
  extending it is additive and safe. **The same file's seven `newTailer` call sites are
  br-GI-9-07's edit**, D7's resolver parameter; keep the two edits disjoint. **Note the trap this
  file shares with `cmd/clens/main_test.go`: two different tests are called "carried-over"**, and
  the instruction here is the **opposite** of the one there — `main_test.go`'s `carriedOver` must
  gain `rekey`, this file's must not. They are separate files and separate lists; do not carry
  either instruction across.)
