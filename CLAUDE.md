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

`docs/context/INDEX.md` is this repo's map to the code — architecture, storage schema, cost
model, conventions, the CLI and API surfaces, and the decision records. It states the *enforced*
conventions and the invariants in their shortest form. The read-first / track-staleness rule for
`docs/context/` lives in the user CLAUDE.md; under `/develop-story`, staleness is tracked in the
story's plan file (`docs/planning/{ticket-id}-{slug}.md`)'s "Context docs to refresh" list.

## Migrations

Applies the user CLAUDE.md's backup-before-migration rule to this repo's specifics: back up
`lens.db`, `lens.db-wal`, and `lens.db-shm` before any `schemaVersion` bump or change under
`internal/store/schema.sql` / `migrations` — whether Claude runs the migration directly against a
real `lens.db` or the plan/bead only states it for the user to run manually.

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
- **AI attribution — the format is fixed here, the identity comes from the session.** Every commit
  ends with a `Co-Authored-By: <agentic tool> (<model>) <noreply@vendor>` trailer naming both the
  tool that produced the change and the model behind it; every PR body ends with a
  `🤖 Generated with [<agentic tool>](<tool url>) using <model>` footer. The *values* — the tool
  name (Claude Code / Cursor / GitHub Copilot / Cline / …) and the model name — are whatever the
  **active session's own instructions** specify; read them from the session every time. Never
  hardcode either in this file, and never copy one in from another repo; if the session names
  neither, omit the trailer and say so.

### Enforcement

`branch-guard.yml` requires every PR into `main` to come from a `GI-<n>-<slug>` branch whose issue
number is one the PR body closes, with a `GI#<n>` title naming a real issue, a closing keyword in
the body, and every non-merge commit prefixed with an issue the body closes. `main-guard.yml`
force-reverts any commit that reaches `main` outside that flow. Neither runs tests — tests are not a
CI gate.

**No repo in this family carries a `develop` branch.** `deepseek-lens` used to hard-gate PRs into
`main` on the head branch being exactly `develop`, and verify each landed commit arrived via a
merged `develop → main` PR; it has since dropped both (commit `1102a01`, GI#27), and this repo never
had them. Every repo now lands a `GI-<n>-<slug>` branch on `main` directly, and both guards are
written for that single hop: the PR head must match `^GI-[0-9]+-[a-z0-9-]+$`, a landed commit must
trace to a merged `GI-…` PR, and the branch's issue number must be one the PR body closes. That last
check exists because one hop to `main` no longer proves provenance the way `develop` did, so the
branch is bound to the same issues the commits already are. Everything else — the `GI#<n>` title
gate, the closing keyword, the per-commit prefix check, the pre-commit secret scan — is unchanged.

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
