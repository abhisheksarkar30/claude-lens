# Bead br-GI-1-11: jsonlogs recursive tailer, request_id dedup, merge semantics, ingest_state

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §The four sources (B), §Cross-source identity, §Storage schema, tests 8, 9, 10, 11a, 11b, 21, 22

- **Bead ID**: br-GI-1-11
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-06, br-GI-1-08
- **Blocks**: br-GI-1-14, br-GI-1-15

> **Dependency note.** The plan lists only br-GI-1-06. This bead **also needs br-GI-1-08**, because a
> JSONL row must go through the same cold path as a proxy row — session resolution, cost (br-GI-1-07),
> insert, analyzer attach, session re-derivation — or source B's rows would have no `session_id`, no
> cost, and no warnings, and the merge's session re-derivation would be re-deriving over half a
> pipeline. See the summary flag. Both edges point lower-numbered, so the DAG stays executable.

## Description

Source B: history that predates install, and sessions never routed through the proxy.

**Recursive walk.** `internal/jsonlogs` walks `~/.claude/projects/**/*.jsonl` — top-level session
transcripts **and** `…/<sessionId>/subagents/agent-*.jsonl` sidechains (823 subagent files vs ~205
top-level on the authoring machine). A file under a `subagents/` path sets `is_sidechain = true` on
its rows and is attributed to its parent session; a top-level transcript gets `is_sidechain = false`
(test 22).

**Dedup by `requestId` — the measured defect (test 8).** On the authoring machine, **35 of 47
`requestId`s in a real Claude Code log carry two assistant lines with byte-identical `usage`
objects**, because one line is written per content block. A naive sum inflates token counts by ~1.75×.
The tailer groups lines by `requestId` and takes the **distinct** request's tokens, never the sum of
lines. This is a regression test for a real, measured defect — a tool that inflates its own numbers by
75% is worse than no tool.

**Tolerance (test 9).** The 13 non-assistant line types observed in real logs (`attachment`,
`file-history-snapshot`, `atis-latch`, `file-history-delta`, `ai-title`, `last-prompt`,
`queue-operation`, `mode`, `pr-link`, …) are skipped. An unknown `type` and a malformed line are
**counted** into `ingest_state` and do **not** stop the tail. A fixture in each observed token shape —
the `ephemeral_*` split and the flat `cache_creation_input_tokens` — parses to the same prompt total,
the flat shape flagged `approximate`. `client_version` is recorded per row so a format break is
attributable to a Claude Code version.

**Resume (test 10).** `ingest_state` holds `jsonl:<path>` → byte-offset cursor. The cursor survives
append, truncation and rotation, and re-reading from an offset does not duplicate rows (the
`request_id` UNIQUE absorbs a re-read).

**`request_id` for a JSONL row.** The dedup key is Anthropic's `request-id` response header on the
**assumption** that it is byte-equal to the `requestId` Claude Code writes into its log. That
equivalence is **not established** — one real JSONL line
(`"requestId":"req_011Ceh2nFYY1p63usDZFpJ4q"`) confirms only the JSONL half. So:

- Use the JSONL `requestId` as the key, prefixed `jsonl:<sessionId>:<uuid>` when a line has none.
- **Implement the fallback key too, and put it behind one switch.**
  `(model, session_id, started_at ±1s, input/cache-write/cache-read/output token quadruple)`, with
  `request-id` used when present and matching to strengthen it. If the live verification below shows
  the header is *not* equal to `requestId`, the fallback becomes **primary** — this bead must be able
  to flip without a rewrite.

**Test 11(b) — live id-equivalence verification (a prerequisite step, not a fixture).** Before the
merge is relied on, capture one live response's `request-id` header through the proxy and the matching
Claude Code JSONL line for the same call, and assert they are equal; record the result in the plan.
**This is an operational prerequisite owned by no bead** — it needs a live proxy run plus a real
Claude Code call. Schedule it when this bead lands; the merge's correctness depends on its outcome.
See the summary flag.

**Merge semantics (test 11a, the fixture half).** An insert colliding on `request_id` merges (the
mechanism is br-GI-1-06). This bead owns the **explicit precedence**:

- `source_refs` gains `jsonl`; `first_source` is preserved; token counts are **not** summed.
- **The complete capture wins**: A when `capture_complete`, otherwise B. So a truncated A
  (`capture_complete = false`, empty or partial usage) against a complete B means **B's tokens become
  the row's tokens**, and **no `source_mismatch` fires** — one source having nothing to contribute is
  not a disagreement.
- `source_mismatch` fires **only** when two *complete* sources disagree on the token counts (a real
  parser bug).
- The columns A structurally cannot supply — `client_version`, `project`, `git_branch`,
  `is_sidechain`, `cli_entrypoint` — are preserved from whichever writer supplied them, so B is not a
  no-op participant.
- The merge re-runs the analyzer seam, and its findings join by **upsert keyed `(event_id, kind)`**.
  The fixture raises at least one kind from each source; after both inserts each kind appears
  **exactly once** on the row (`COUNT(*) = COUNT(DISTINCT kind)` per `event_id`), and the session's
  `warning_count` **equals the distinct-kind count** — not the sum of the two analyzer runs. A test
  that only checked the row count would pass on the buggy v4, which is why the assertion is on kinds
  and the session count.

**Retry keeps two rows (test 21).** Two proxy calls with byte-identical bodies but distinct response
`request-id`s (a 429/529 retried) produce **two** `events` rows and two findings, never one merged row
— the `rate_limited`/`overloaded` signal is preserved. (The proxy side is br-GI-1-03's synthetic-key
disambiguator; the store side is br-GI-1-06's UNIQUE-on-`request_id`.)

**The proxy is the only writer for `source='proxy'`; jsonlogs is the only writer for
`source='jsonl'`** (invariant 3). This package must not write a proxy row.

## Rationale

Source B is the only source that can answer "what did I use before I installed this", and the
duplicate-`usage` trap is the single measured defect that would make the tool's own numbers wrong by
75%. The tolerance work is what keeps a Claude Code format change from silently stopping ingestion.

## Outcome Definition

- `go test ./internal/jsonlogs/... -race` passes.
- A fixture with duplicated `requestId` lines yields the tokens of the **distinct** requests, not the
  sum of lines (test 8).
- The 13 non-assistant types are skipped; an unknown type and a malformed line are counted, not fatal
  (test 9).
- Both token shapes parse to the same prompt total; the flat shape is `approximate` (test 9).
- The cursor survives append, truncation and rotation without duplicating rows (test 10).
- A subagent file is discovered, ingested, and attributed with `is_sidechain = true`; a top-level
  transcript gets `false` (test 22).
- The merge fixture asserts one row, `source_refs` = both, tokens not summed, and the warning-ledger
  equality (test 11a).
- A truncated A against a complete B adopts B's tokens and raises no `source_mismatch` (test 11a).
- Two complete sources disagreeing raise `source_mismatch`.
- Two byte-identical bodies with distinct response ids yield two rows (test 21).

## Test Specifications

- Unit Tests (`internal/jsonlogs/jsonlogs_test.go`):
  - **Test 8 — duplicate-usage dedup**: a fixture with 2 lines per `requestId` (byte-identical usage)
    → tokens of the distinct requests.
  - **Test 9 — tolerance**: each of the 13 non-assistant types skipped; an unknown type counted; a
    malformed line counted and the tail continues; the split and flat fixtures give the same prompt
    total, flat flagged `approximate`.
  - **Test 10 — resume**: append then resume (no dupes); truncate then resume; rotate then resume.
  - **Test 22 — subagent discovery**: a `…/<sessionId>/subagents/agent-*.jsonl` is found and
    attributed `is_sidechain=true`; a top-level file `false`.
  - UTF-8 and a line split across a read boundary parse correctly.
- Integration Tests (real temp store + real consumer pipeline):
  - **Test 11a — merge fixture**: insert from each source → one row, `first_source` preserved,
    `source_refs` both, tokens not summed; a second fixture (truncated A, complete B) → B's tokens, no
    `source_mismatch`; two complete sources disagreeing → `source_mismatch`; each finding kind appears
    once and `warning_count == COUNT(DISTINCT kind)`.
  - **Test 21 — retry**: two distinct response ids for one body → two rows and two findings.
  - B-owned metadata columns survive the merge.
- E2E / manual: **test 11b** live `request-id` ↔ `requestId` capture, result recorded in the plan.

## Files to Touch

- `internal/jsonlogs/jsonlogs.go`, `internal/jsonlogs/jsonlogs_test.go` (create)
- `internal/jsonlogs/dedup.go`, `internal/jsonlogs/dedup_test.go` (create)
- `internal/jsonlogs/cursor.go` (create — `ingest_state` cursor)
- `internal/jsonlogs/testdata/` (create — duplicated-requestId, tolerance, subagent fixtures)
- `internal/store/merge.go` (modify — merge precedence: complete-capture-wins, `source_mismatch` gate)
- `internal/store/merge_test.go` (modify)
