[← INDEX](../INDEX.md)

# ADR 007: schema changes go through a `PRAGMA user_version` runner

**Status:** Accepted — **supersedes a GI-1 decision**

**Context:** GI-1 recorded *"Migrations: none in v1"* as a **decision, not an omission**. It was
defensible then: the schema was one file of `CREATE TABLE IF NOT EXISTS` statements, executed on
every `Open`, which is a complete answer while the schema never changes shape. `IF NOT EXISTS`
cannot add a column to a table that already exists, so the moment a story needs a column on a live
database the decision stops being available — and GI#7 needs two (`transcript_content`,
`transcript_role`), for a store that on this machine is 82 MB and 84,765 rows.

The rejected side, then, is not "no migrations forever" — it is the specific tempting alternative:
**have `schema.sql` do the ALTER too**, or **delete the database and recreate it**. The first cannot
work (`IF NOT EXISTS` does not guard a column), and the second is data loss for the user's entire
capture history, which is the asset the tool exists to hold. The second alternative also *looks*
safe in development, where the database is a temp file — which is exactly how it survives review.

**Decision:** a `PRAGMA user_version` runner in [internal/store/store.go](../../../internal/store/store.go).
`schemaVersion` is the current version; `migrations[n]` upgrades version `n` to `n+1`; each runs in
its own transaction with the version bump **inside** it, so a failure part-way leaves the version
where it was and the next `Open` retries rather than skipping the change forever. `schema.sql` keeps
its `IF NOT EXISTS` shape and stays the description of the *current* schema — created whole on a
fresh file, repairing a partial one on every boot — while the runner owns every `ALTER`. No piece of
SQL has two homes.

`Open`'s ordering is the part that is not obvious, and it is load-bearing:

```
probe eventsTableAbsent → (if fresh) stamp user_version = schemaVersion → exec schema.sql → migrate
```

The stamp happens **before** the exec, on a fresh file. The exec is deliberately not atomic, so a
first run that dies mid-file leaves a database with `events` (columns and all) and a later table
missing. Had the version been written only after a *successful* exec, that file would read `0`, the
runner would re-run the `ALTER`s against a table that already has the columns, and every boot
thereafter would fail with `duplicate column name` — with no recovery but deleting the file. The
schema exec runs unconditionally so it can heal the missing table; stamping first is what stops it
and the runner double-applying the same change. A version ahead of the binary is an error, not a
guess: opening a database written by a newer `clens` is refused.

**Consequences:**

- A new column is three edits in one place: the column in `schema.sql`, the `ALTER` in `migrations`,
  and `schemaVersion` bumped. `schemaVersion` **must** equal `len(migrations)`, or `migrate` indexes
  out of range; the constant's own comment says so.
- Existing databases migrate on the next `clens` command that opens them, including `ls` and `show`.
  There is no separate `migrate` subcommand and no prompt — the migration is additive and instant.
- A migration that rewrites a table (not just `ADD COLUMN`) *would* need a table rebuild and is not
  what this runner was designed against; the columns added so far are nullable and additive for that
  reason. A destructive migration needs its own decision, not a new entry in the slice.
- The four paths — fresh, pre-change, partial-with-missing-table, partial-with-new-schema — are
  pinned by `TestMigrateFreshDatabase`, `TestMigrateExistingDatabase`,
  `TestMigrateHealsAPartialDatabase` and `TestMigrateDoesNotReAddColumnsOnAPartialNewSchema`. The
  last is the one that fails if the stamp ordering is ever reversed.
