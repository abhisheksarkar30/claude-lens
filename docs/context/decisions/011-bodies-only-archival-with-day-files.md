[← INDEX](../INDEX.md)

# ADR 011: archive bodies only, with a marker table and per-UTC-day files

**Status:** Accepted

**Context:** Bodies are ~99.5% of `lens.db`; a week-old call is rarely opened but must stay reachable. The GI#16
plan (`docs/planning/GI-16-calls-range-restart-archival.md`) needed the hot file bounded without losing
aggregates or breaking the cross-source merge.

**Decision:** Move only `req_body` / `resp_body` / `transcript_content` to
`<db dir>/archive/bodies-YYYY-MM-DD.db` (zstd or raw), leave the `events` row whole, and record the move in a
`body_archive` marker table (`event_id`, `day`, `archived_at`, `body_mask`). Reads hydrate transparently;
`clens archive restore` reverses it.

**Rejected:**
- *Whole-row archival*: breaks every aggregate query and the merge, which `UPDATE`s `events`.
- *`ATTACH`ing day files*: a per-connection limit and a second write path beside the single write connection.
- *A marker column on `events`*: a rewrite of the wide table for one bit; the side table cascades on purge.
- *Background `VACUUM`*: holds the only write connection for minutes; left as a documented one-time manual step.

**Consequences:** The hot file does not shrink by itself. A copy of `lens.db` alone loses older bodies. Writers that
read bodies (`rekey` pass 1, `reflag`) skip archived rows and name `archive restore`.
