# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Claude Lens: a local observability proxy for Claude traffic. One Go binary, `clens`, sits in front of
the Anthropic endpoint and captures what was sent, what came back, what it cost, and which request
parameters the API silently dropped.

Unlike a single-source proxy it reconciles **four sources** — the proxy itself, Claude Code's own
JSONL transcripts (`~/.claude/projects/**/*.jsonl`), the claude.ai usage endpoint, and the Admin API
usage/cost reports — so it can report usage and cost for **every** Claude subscription tier (Free,
Pro, Max 5x, Max 20x, Team, Enterprise) *and* pay-as-you-go API-key billing from one install.

It is a single-user developer tool — no hosted deployment, no multi-user auth, loopback-only by
default.

See `docs/planning/GI-1-claude-lens-v1.md` for the converged plan (v6, 7 review rounds). The work
items are `.beads/GI-1/br-GI-1-01` … `-19`; each bead names its own file list and outcome definition.

**Before touching any code in this repo — including a one-off ad hoc request, not just a
`/develop-story` run — read `docs/context/INDEX.md` first**, then the specific module doc(s)
covering the area you're about to change. This is the map to the code — architecture, storage
schema, cost model, conventions, the CLI and API surfaces, and the decision records. It states the
*enforced* conventions and the invariants in their shortest form. Treat what it says about the
specific slice you're changing as a hypothesis, not fact, until you check it against the real
source; the code wins wherever they disagree.

**The other half of that rule: track the doc, don't stop to fix it mid-change.** Whether the
staleness turns up mid-read (a doc claim that no longer matches the code) or your own change just
created it (a new, renamed, or removed entity, endpoint, permission, flow, module, or convention),
add the specific `docs/context/*.md` file:line claim to a running list as you go rather than
patching it inline:
- **Ad hoc edit** — at the end of the task, surface that list to the user as a suggestion (name the
  file:line and what's now stale) and offer to update it — don't fix it unprompted.
- **Under `/develop-story`** — append it to a running "Context docs to refresh" list in the story's
  plan file (`docs/planning/{ticket-id}-{slug}.md`) as you go. Phase 5.6 still does its normal
  full-PR-diff scoping (new/changed/removed entities, endpoints, permissions, flows, modules,
  conventions across every commit on the branch) — that list is a guaranteed floor on top of it,
  not a replacement, so a claim spotted mid-story can't fall out of scope if the later diff-based
  pass misses it.

Skip only when the change is purely internal to the code (nothing a context doc would claim), and
say so rather than skipping silently.

## Migrations

Any plan or bead step that runs a schema migration (a `schemaVersion` bump, or any change under
`internal/store/schema.sql` / `migrations`) — whether Claude executes it directly against a real
`lens.db` or the plan/bead only states it for the user to run manually — is preceded by an automatic
backup of the live database file(s) (`lens.db`, `lens.db-wal`, `lens.db-shm`) to a separate path
before the migration runs. This applies regardless of file size: `cp` is cheap, a bad migration
against the only copy of months of captured traffic is not recoverable. State the backup path when
it's made.

## Setup

One step per clone, with no build system to do it for you:

```
git config core.hooksPath .githooks
```

That arms `.githooks/commit-msg` (rejects any commit not starting with `GI#<n>`) and
`.githooks/pre-commit` (delegates to the machine-wide secret scan at
`$XDG_CONFIG_HOME/git/hooks/pre-commit`, and **refuses every commit** until that scan is installed —
`bash git-hooks/install.sh` from your `agentic-ai-artifacts` checkout).

## Commands

```
go build ./...                    # build
go test ./...                     # all tests
go test ./internal/proxy/         # one package
go vet ./...                      # vet
go run ./cmd/clens doctor         # print effective config, bind addresses, warnings
go run ./cmd/clens serve          # proxy + dashboard in one process
```

## Conventions

These are copied from `deepseek-lens` (which took them from `family-monitor`) and are enforced, not
advisory.

- **Ticket prefix `GI#<n>`** — `GI` is the GitHub issue number in this repo. The issue is the
  problem statement; the plan is the design. Everything keys off it.
- **Branches** — `GI-<n>-<kebab-slug>`, cut from `main`. The plan doc, the branch, and the PR title
  all carry the same `GI-<n>-<slug>`.
- **Commit subject** — `GI#<n> <type>: <lowercase summary> (br-GI-<n>-<NN>)`, where `<type>` is one
  of `feat` / `fix` / `docs` / `chore` / `plan` / `beads` / `review`. The body explains *why* the
  change is correct, not what it does — the diff already says what. Wrap at ~76 columns.
- **PR** — title is the same `GI#<n> <type>: <summary>` as the branch's headline commit. Body is
  `## Summary`, `## Verification`, `## Beads`, then `Closes #<n>`. A story branch PRs directly into
  `main`.
- **Commits end with** `Co-Authored-By: Claude Code <noreply@anthropic.com>`; PR bodies end with
  `🤖 Generated with [Claude Code](https://claude.com/claude-code)`.

### Enforcement

`branch-guard.yml` requires every PR into `main` to come from a `GI-<n>-<slug>` branch whose issue
number is one the PR body closes, with a `GI#<n>` title naming a real issue, a closing keyword in
the body, and every non-merge commit prefixed with an issue the body closes. `main-guard.yml`
force-reverts any commit that reaches `main` outside that flow. Neither runs tests — tests are not a
CI gate.

**This is the one place the conventions deliberately differ from `deepseek-lens`.** Its guards
hard-gate PRs into `main` on the head branch being exactly `develop`, and verify each landed commit
arrived via a merged `develop → main` PR. This repo has no `develop` — v1 lands on `main` via a
`GI-<n>-…` branch (the plan's Decision log records this as a deliberate deviation). Keeping those
two predicates verbatim would reject this repo's own pull requests, starting with the first one, so
both were adapted rather than copied. Everything else — the `GI#<n>` title gate, the closing
keyword, the per-commit prefix check, the pre-commit secret scan — is unchanged. If a second story
ever needs an integration branch, introduce `develop` and restore the two-guard flow; until then
there is nothing for it to integrate.

### Layout

| | |
|---|---|
| Plan | `docs/planning/GI-<n>-<slug>.md` |
| Beads | `.beads/GI-<n>/br-GI-<n>-<NN>-<type>-<slug>.md` |
| Context docs | `docs/context/INDEX.md` — the generated map to the code; read this first |
| Cross-review artifacts | `docs/planning/GI-<n>-<slug>/review/round-N/` — **gitignored**, never committed |

## Architecture essentials

- **One process, two `http.Server`s on separate listeners,** plus collector goroutines. The proxy
  listener holds the hot path and does no parsing; it tees the bytes into a bounded sink and returns.
  The consumer goroutine drains the sink, parses, analyzes, and writes to SQLite. The dashboard
  listener reads SQLite and pushes SSE. The three collectors (`jsonlogs`, `snapshot`, `adminrep`)
  write to the same store on their own schedules.
- **The hot path must never buffer the stream to count tokens.** The TTFB test (fake upstream,
  slow-streamed SSE, assert the client sees the first event before upstream sends its last) is the
  hard gate that keeps this true. A buffered stream still returns correct bytes — just late — so no
  other test would catch it.
- **`internal/proxy` depends only on `sink` and `config`.** If it ever imports `analyze`, `store`, or
  `pricing`, the design has eroded. Decode and parse live in the cold path for exactly this reason —
  decompressing on the client's goroutine is what the TTFB gate forbids.
- **`input_tokens` is the uncached remainder only.** Total prompt size is
  `input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens`. A row that
  reports `input_tokens` alone as the prompt size is wrong. This is a tested invariant, not a
  convention.
- **A cost figure's billing model is carried in the column it lives in.** A `subscription` figure is
  written to `api_equivalent_cost_usd` and `cost_usd` is left NULL; an `api` figure is written to
  `cost_usd`. **Two billing models are never summed**, and an unpriced API row is NULL, never
  `$0.00`. This is enforced by a schema invariant test, not by a convention — see
  `docs/planning/GI-1-claude-lens-v1.md` §Billing model for all seven shapes.
- **`events` has one writer *package* per source, serialized by the store's single write
  connection** (`SetMaxOpenConns(1)`), not by the schema. The proxy consumer owns `source='proxy'`;
  `jsonlogs` owns `source='jsonl'`. The cross-source merge is the first `UPDATE` on `events` and
  re-derives the owning session's totals in the same transaction, because a merge can rewrite a
  row's token columns and an incremental fold would drift.
- **Fail open.** A broken observer never breaks the user's coding session, and a broken collector
  never prevents the others from writing. The proxy redacts credentials before insert and never
  makes the call depend on a successful capture.
- **Credentials never reach the database.** Not in headers (redacted before the tee), not in bodies
  (policy + 2 MB cap), and `internal/secret` lives in a file outside the DB — protected by POSIX
  modes on Unix and an explicit Windows ACL on Windows (`0600` is a no-op there). Full bodies *are*
  stored, so the content — every prompt and every file the agent read — is the asset this repo is
  protecting. Loopback binding and redaction are load-bearing defaults, not conveniences.

## Compact Instructions

When compacting, always preserve the working state for continuation:

- Keep the current high-level goal and acceptance criteria.
- Keep the exact list of files modified during this session.
- Do not preserve verbose terminal outputs or tool logs.
