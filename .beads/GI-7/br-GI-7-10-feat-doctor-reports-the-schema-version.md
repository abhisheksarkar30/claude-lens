# Bead br-GI-7-10: `clens doctor` reports the database's schema version

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §4 (the `internal/cli` row),
`decisions/007`

- **Bead ID**: br-GI-7-10
- **Priority**: P3 (low)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-7-06 (the migration runner and `schemaVersion`, the thing being reported)
- **Blocks**: None

> **Provenance.** Not from the plan. Raised while verifying br-GI-7-06's migration against a live
> install: the question *"did this database get brought forward?"* had no answer through the tool, so
> it was answered by reading the SQLite file's header directly — which does not answer it at all,
> because in WAL mode the header page can be dirty and living in the `-wal`, so the main file reports
> the pre-migration version. The check came back `user_version: 0` on a database that was already at
> 1. Reporting the version through `doctor` is what makes the cheap, wrong answer unnecessary.

## Description

**This bead adds a diagnostic, not a behaviour.** `store.Open` already reconciles the file's version
with `schemaVersion` on every open, and `br-GI-7-06` pins all four of its paths (T9a–T9d). Nothing
about that changes here.

What is missing is a way to *see* the number. `clens doctor` reports the resolved config, the four
sources' health and five checks, and none of them is the schema version — so the only ways to learn
it are `sqlite3` or reading the file, and the file-reading answer is wrong in a way that looks right.

### The check

```
[PASS] db_schema            version 1 (this binary: 1)
```

Both numbers, because the interesting failure is the pair disagreeing. `store.SchemaVersion()` is
newly exported so the label cannot carry a second copy of a constant the migration runner exists to
own; `(*Store).UserVersion` reads the stored value.

**`db_schema` FAILs on Open's error rather than reporting a mismatch**, and that is deliberate.
`migrate` already refuses a database whose version exceeds `schemaVersion`, and `Open` returns that
error — so by the time a version can be read at all, the two agree. A mismatch branch would be
unreachable code, and an unreachable branch that looks like a safety net is worse than no branch:
the next reader has to prove it dead to know it can be ignored. The FAIL path is Open failing, which
is where a skew actually surfaces.

**It opens its own store connection.** `sourceHealthRows` already opens one, and the two now overlap
in a single `doctor` run. That is accepted rather than refactored: `doctor` is a diagnostic a human
runs by hand, the two live on either side of the checks block, and threading one handle through both
would change the shape of `runChecks`, which currently does no store I/O at all. The cost is one
extra SQLite open on a command that binds two ports and walks the filesystem.

**A side effect worth naming: `clens doctor` migrates.** `store.Open` has always done this, and
`sourceHealthRows` has always triggered it, so this bead changes nothing here — but the check makes
the behaviour visible in the output, which is the right place for it. On a database that predates
the migration, the report shows the *post*-migration version. The pre-migration number is not
recoverable through this path and does not need to be: `doctor`'s question is "is this install
healthy", not "what was it before I asked".

### What this bead does not fix

**`doctor` still reports nothing about the *dashboard*'s schema expectations.** There is no second
consumer of `user_version` in the repo, so there is nothing to compare it against.

**The check does not validate the schema's contents**, only its version. A database whose version is
current but whose columns were dropped by hand still PASSes; `store_test.go`'s T9a–T9d are where
column-level expectations live.

## Rationale

The defect this closes is not in the code — the migration worked correctly throughout. It is that the
correct answer was expensive to get and a wrong one was cheap, which is a reliable way to end up
believing the wrong one. The bead above was written from exactly that: a confident `user_version: 0`
on a healthy database.

One row in an existing diagnostic command is the smallest thing that removes the wrong answer's
advantage. It is filed under GI#7 rather than as standalone work because the migration runner is what
made the version worth asking about at all — before br-GI-7-06 the answer was always 0, so there was
nothing to report and nobody would have read it.

## Outcome Definition

- `clens doctor` prints a `db_schema` check whose detail names both the stored version and this
  binary's, and PASSes on a database this binary can read.
- `store.SchemaVersion()` is the only place the expected version is spelled; `sources:` and the rest
  of the report are unchanged.
- The check FAILs, rather than panicking or reporting a mismatch, when `store.Open` fails.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

- Unit Tests (`internal/cli/doctor_test.go`): `TestDoctorReportsTheSchemaVersion` — asserts the
  `db_schema` row is present, PASSes, and names `store.SchemaVersion()` for both figures.
  - **The reporting half only, deliberately.** Whether a version-0 database is brought forward is
    the runner's behaviour and is pinned in `internal/store` by T9a–T9d, including the two orderings
    whose failure modes are unrecoverable. Re-asserting it here would mean rebuilding a pre-change
    database inside a package that cannot reach `schemaSQL`, so the fixture would only approximate
    the old shape — a worse test of that behaviour than the ones using the real thing.
  - **Mutation-verified**: deleting the `dbSchemaCheck(cfg)` call from `runChecks` fails this test
    and nothing else, which is what makes it a test of the wiring rather than of the function body.
- Integration Tests: none — no API or schema change.
- E2E: none.

## Files to Touch

- `internal/store/store.go` (modify — export `SchemaVersion`, add `(*Store).UserVersion`)
- `internal/cli/doctor.go` (modify — `dbSchemaCheck`, and its call in `runChecks`)
- `internal/cli/doctor_test.go` (modify — the reporting assertion)
- `docs/context/cli-and-tooling.md` (modify — the `doctor` row)
