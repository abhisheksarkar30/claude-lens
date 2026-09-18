# Bead br-GI-1-15: CLI additions (ingest, refresh, quota, accounts, models, reconcile)

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §CLI (the six new subcommands), §Quota engine, §Reconciliation, §Security posture (re-auth)

- **Bead ID**: br-GI-1-15
- **Priority**: P1 (high)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-11, br-GI-1-12, br-GI-1-13, br-GI-1-14
- **Blocks**: br-GI-1-18

> **Dependency note.** The plan lists br-GI-1-11/12/13. This bead **also needs br-GI-1-14**, because
> `clens refresh` and `clens ingest` are wrappers over `ingest.RunOnce` (br-GI-1-14) — the
> orchestration is not re-implemented per command. See the summary flag. The edge points to a
> lower-numbered bead, so the DAG stays executable.

## Description

The six new subcommands, each a thin presentation layer over a package an earlier bead built. Both
lines of the carry-over evidence: deepseek-lens ships 12 subcommands, claude-lens ships 18.

| Command | What it does | Backed by |
|---|---|---|
| `clens ingest` | Backfill from the JSONL sources once, incrementally. `--rebuild` re-reads from byte 0. | br-GI-1-11, br-GI-1-14 |
| `clens refresh` | Run every non-proxy collector once (JSONL tail + snapshots + admin pull) — **the cron / Task Scheduler entry point**. | br-GI-1-14 |
| `clens quota` | Rolling 5h/7d burn vs configured limits, last snapshot, calibration. | br-GI-1-12 |
| `clens accounts` | Configured accounts, `auth_kind` distribution, plan and limit state; the re-auth path for an expired `sessionKey` / a new Admin key. | br-GI-1-01, br-GI-1-12 |
| `clens models` | Model catalogue, and which models are `unpriced`. | br-GI-1-07 |
| `clens reconcile` | Computed vs billed, per day and model, with drift. | br-GI-1-13 |

**Registration.** Add these six entries to the `commands` map in `cmd/clens/main.go` that br-GI-1-01
created with an empty map. This bead's edit is **additive** — it appends six keys and must not rewrite
the table's structure. br-GI-1-17 appends the twelve carried-over names to the same map; the two beads
own disjoint keys, which is what keeps the shared file from being a conflict (see the summary flag).

**Presentation discipline inherited from the carried-over commands**, and these new ones must not
diverge:

- `clens quota` renders the snapshot percentage **only when a limit is configured**; otherwise it
  states **`unconfigured`** — it never renders a percentage of a limit nobody supplied, and never
  guesses that "Max 20x" means a particular number of messages.
- `clens accounts` shows `unconfigured` plan state as a labelled state, not `0%`.
- `clens reconcile` shows **computed** and **billed** in separate labelled columns, **never summed**,
  and states that `cost_drift` applies to API accounts only.
- `clens models` lists `unpriced` models alongside priced ones, never folded into a total.
- `clens accounts` refuses to store an Admin key without `--yes` and states its org-wide read scope.
- `clens refresh` (and the re-auth path) never prints a credential; an error reports a status and a
  reason.

`clens refresh` must be usable from an unattended scheduler: no prompts, non-zero exit on failure, and
a one-line per-source summary so a failed run is visible in cron output.

**Formatting is local to this bead's six command files.** The shared `internal/cli/format.go` helpers
belong to the base CLI (br-GI-1-17, which lands after this bead), so these six commands keep their own
small render helpers rather than creating `format.go` ahead of it — a bead that creates a file a later
bead also creates is a conflict, and a bead that depends on a later bead's file is a broken edge. See
the summary flag.

## Rationale

These are the entry points for the three non-proxy sources and the two accounting surfaces. The
`refresh` command is what makes the collectors usable without `serve` running — the answer to "how do
I get history in" and "how do I keep it current".

## Outcome Definition

- `go test ./internal/cli/... -race` passes.
- All six commands appear in `clens` dispatch and run against a populated store.
- `clens quota` prints `unconfigured` (not a percentage) when no limit is set.
- `clens reconcile` prints two labelled columns and never a summed figure.
- `clens models` lists `unpriced` models distinctly.
- `clens accounts` refuses to store an Admin key without `--yes`.
- `clens refresh` runs unattended, exits non-zero on a collector failure, and prints a per-source line.
- `clens ingest --rebuild` re-reads from byte 0 without duplicating rows.
- No command prints a credential value.

## Test Specifications

- Unit Tests (`internal/cli/additions_test.go`):
  - Each new command dispatches and exits 0 against a temp store.
  - `quota` with an unconfigured limit → output contains `unconfigured` and no `%`.
  - `reconcile` output contains separate computed and billed labels and no sum.
  - `models` marks `unpriced` distinctly.
  - `accounts` without `--yes` refuses the Admin key; with `--yes` stores it.
  - `refresh` returns non-zero when a collector fails and prints the failing source.
  - `ingest --rebuild` re-reads from byte 0 and produces no duplicate rows.
  - Credential containment: no command's stdout/stderr contains a `sessionKey` or `sk-ant-admin…`
    value (test 18's CLI half).
- Integration Tests: with a fixture store, `quota`/`models`/`reconcile` render expected figures.
- E2E: `clens refresh` against a real `~/.claude/projects` tree (opt-in).

## Files to Touch

- `internal/cli/ingest.go` (create)
- `internal/cli/refresh.go` (create)
- `internal/cli/quota.go` (create)
- `internal/cli/accounts.go` (create)
- `internal/cli/models.go` (create)
- `internal/cli/reconcile.go` (create)
- `internal/cli/additions_test.go` (create — the six new commands' tests; br-GI-1-17 creates
  `internal/cli/cli_test.go` for the twelve carried over, so the two beads never write one test file)
- `cmd/clens/main.go` (modify — append the six command entries; br-GI-1-17 appends its twelve to the
  same map literal, disjoint keys, append-only on both sides)
