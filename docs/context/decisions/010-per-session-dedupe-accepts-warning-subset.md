[← INDEX](../INDEX.md)

# ADR 010: the per-session analyzer pass and fold dedupe to once per batch, accepting a subset of warning rows

**Status:** Accepted

**Context:** Before GI#13, `internal/consumer`'s `flush` re-ran the session-scoped analyzer pass
(`SessionEvents`, then re-named `SessionEventsForRules`) and the session fold (`RecordCall`) once
per **row** in a flush batch. A batch of several calls belonging to the same session therefore
re-read and re-walked that session's whole row history — bodies included — once per row in the
batch, on top of once per session. Combined with the missing composite index
([decisions in storage-schema.md](../storage-schema.md)), a large session's flush cost scaled with
`batch_size × session_row_count`, which is what the GI#13 profile caught as the dominant cost in a
live capture (`_vdbePmaWriteBlob`/`_vdbeIncrSwap`/`_vdbePmaReadBlob` — SQLite's sort-to-temp-file
markers, at 31.6%/27.5%/34.2% cumulative).

**Decision:** `flush` now tracks, per distinct session id returned by `InsertEvent` across the whole
batch, the first-inserted row for that session (`internal/consumer/consumer.go`). After every row in
the batch has been inserted, the session-scoped pass and the fold each run **once per distinct
session**, not once per row. `internal/jsonlogs`'s tailer follows the same shape per file. The fold
carries the first-inserted row's `PrefixHash` for that session — not the last, and not the one with
the smallest `started_at` — because `prefix_hash` is insert-only at the store (`ON CONFLICT` never
touches it), so the first-writer-wins row is the only one that reproduces the pre-GI#13 value
(bead's D8; caught as a MAJOR review finding when an earlier draft asserted "last row" instead).

*Rejected alternative:* keep the pass and fold at their original per-row cadence and rely solely on
the new composite index (`idx_events_session_started`, schemaVersion 2 — see
[storage-schema.md](../storage-schema.md)) to make each individual re-run cheap. Rejected because the
index removes the *sort spill* but not the *repetition*: a batch of `N` calls for one session would
still re-walk that session's full row history `N` times instead of once, so the index alone caps the
cost per re-run without capping the number of re-runs. Exact byte-identical warning output (running
the pass after every single row, as before) was also considered and rejected as not achievable at
acceptable cost without a history bound — bounding how much of a session's history a pass scans is
a *rules-semantics* question this story deliberately leaves deferred (see `docs/planning/GI-13-session-pass-cost.md`
§8, and GI-9's own prior deferral of the same question).

**Consequences:**

- **A session-scoped rule can miss or reorder a warning it would have caught under the old per-row
  cadence, in exactly two benign shapes**, once `rowsWithUsage` (bead 05) is in place: a stale
  finding that the session's own later state already contradicts, and a re-anchoring of a finding
  the final, once-per-batch pass still reports on a different row than an intermediate per-row pass
  would have. No finding is silently and permanently lost — the final pass over the batch's full
  effect on the session still runs, and still sees every row.
  See [workflows.md](../workflows.md) flow 1.
- **This is user-visible**, and was flagged as the fork most worth challenging when the story was
  reviewed: a warning a user sees today, mid-batch, on an intermediate row may not appear on that
  same row after this change — it appears on the batch's actual result instead.
- **The fold's row choice (first-inserted, not last) matters only because of `prefix_hash`.** Every
  other session-aggregate column is re-derived from the `events`/`warnings` tables by
  `ReconcileSession`, so folding any row in the batch would otherwise produce an identical result;
  `prefix_hash` is the one column an incremental caller actually owns.
- **No schema change and no new capture** — this decision is about *when* existing reads and writes
  happen, not what is stored. It composes with the composite index
  (`idx_events_session_started`, schemaVersion 2): the index makes each once-per-session re-run
  cheap, and this decision makes there be only one such re-run per batch.
