[← INDEX](../INDEX.md)

# Decision records

Seven architectural forks where the code took one path and a real alternative was rejected. Each is
recorded because **the losing side is the one a future change is most likely to reintroduce** —
these are not history for its own sake.

001–006 come from the GI-1 plan's own Decision log (`docs/planning/GI-1-claude-lens-v1.md`
§Decision log), which records the choice and the rejected alternative as they were made. **007 is
different in kind**: it records a GI-1 decision being *superseded*, and it is here because the
rejected side of that original decision — "no migrations" — is the one a reader will still find
quoted in the older docs.

| ADR | Decision | Reintroducing the rejected side would… |
|---|---|---|
| [001](001-billing-split-by-column.md) | the billing split is carried by *which column* a figure lives in | let a query sum a real cost with a hypothetical one — the single arithmetic error this tool must not make |
| [002](002-hot-path-never-parses.md) | the hot path only tees bytes; decode/parse/price live in the cold path | make the client's TTFB depend on this tool |
| [003](003-full-bodies-stored.md) | full request/response bodies are stored, by default | remove the feature that distinguishes this tool from a metadata-only proxy — and change what `lens.db` is |
| [004](004-credentials-outside-the-db.md) | credentials live in a file outside the database, ACL-protected on Windows | put a spendable credential inside the artifact most likely to be copied |
| [005](005-no-develop-branch.md) | v1 lands on `main` via a `GI-<n>-…` branch; no `develop` | break both CI guards, which were adapted specifically for this |
| [006](006-dashboard-with-no-build-step.md) | the dashboard is hand-written and embedded; no bundler, no CDN | add a network dependency and a build step to a loopback tool holding every prompt |
| [007](007-schema-migrations-by-user-version.md) | schema changes go through a `PRAGMA user_version` runner; `schema.sql` never `ALTER`s | recreate (or hand-edit) the user's database to add a column — silently, because it looks safe when the database is a temp file |

The status of all seven is **Accepted**. None is superseded — 007 supersedes a *GI-1 decision*
("migrations: none in v1"), not another ADR.
