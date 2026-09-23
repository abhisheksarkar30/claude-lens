[← INDEX](../INDEX.md)

# Decision records

Ten architectural forks where the code took one path and a real alternative was rejected. Each is
recorded because **the losing side is the one a future change is most likely to reintroduce** —
these are not history for its own sake.

001–006 come from the GI-1 plan's own Decision log (`docs/planning/GI-1-claude-lens-v1.md`
§Decision log), which records the choice and the rejected alternative as they were made. **007 is
different in kind**: it records a GI-1 decision being *superseded*, and it is here because the
rejected side of that original decision — "no migrations" — is the one a reader will still find
quoted in the older docs. **008 and 009 come from the GI-9 plan** (`docs/planning/GI-9-merge-jsonl-and-proxy-rows.md`,
D1 and D7): the identity key and the session id the cross-source merge relies on. **010 comes from
the GI-13 plan** (`docs/planning/GI-13-session-pass-cost.md`, D1): the per-session analyzer pass and
fold dedupe to once per flush batch.

| ADR | Decision | Reintroducing the rejected side would… |
|---|---|---|
| [001](001-billing-split-by-column.md) | the billing split is carried by *which column* a figure lives in | let a query sum a real cost with a hypothetical one — the single arithmetic error this tool must not make |
| [002](002-hot-path-never-parses.md) | the hot path only tees bytes; decode/parse/price live in the cold path | make the client's TTFB depend on this tool |
| [003](003-full-bodies-stored.md) | full request/response bodies are stored, by default | remove the feature that distinguishes this tool from a metadata-only proxy — and change what `lens.db` is |
| [004](004-credentials-outside-the-db.md) | credentials live in a file outside the database, ACL-protected on Windows | put a spendable credential inside the artifact most likely to be copied |
| [005](005-no-develop-branch.md) | v1 lands on `main` via a `GI-<n>-…` branch; no `develop` | break both CI guards, which were adapted specifically for this |
| [006](006-dashboard-with-no-build-step.md) | the dashboard is hand-written and embedded; no bundler, no CDN | add a network dependency and a build step to a loopback tool holding every prompt |
| [007](007-schema-migrations-by-user-version.md) | schema changes go through a `PRAGMA user_version` runner; `schema.sql` never `ALTER`s | recreate (or hand-edit) the user's database to add a column — silently, because it looks safe when the database is a temp file |
| [008](008-three-tier-identity-key.md) | cross-source identity is a three-tier key (`request-id` header → response body `message.id` → namespaced synthetic) | fall back to a header-only key that cannot converge a proxy row with its JSONL counterpart whenever the header is absent from either side |
| [009](009-proxy-adopts-the-conversation-id.md) | the proxy adopts `x-claude-code-session-id` as its stored `session_id` | leave `clens sessions` showing gap-window heuristic groups instead of real conversations |
| [010](010-per-session-dedupe-accepts-warning-subset.md) | the session-scoped analyzer pass and fold run once per distinct session per flush batch, not once per row | re-walk a large session's full row history once per row in a batch again, scaling flush cost with `batch_size × session_row_count` even with the composite index in place |

The status of all ten is **Accepted**. None is superseded — 007 supersedes a *GI-1 decision*
("migrations: none in v1"), not another ADR.
