# Bead br-GI-1-19: README, context docs, governance verification, end-to-end acceptance

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Infrastructure, §Analyzer rule catalogue (README-consistency), §Security posture, §Test strategy (11b, acceptance), §Carried over (`readme_test.go`)

- **Bead ID**: br-GI-1-19
- **Priority**: P2 (medium)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-17, br-GI-1-18
- **Blocks**: None

## Description

The documentation and the acceptance run that close the story. Three parts.

**1. README.md — the real one.** br-GI-1-01 left a stub. This bead writes the document: overview,
install and the single-binary story, the four sources and why each exists, the seven billing shapes
(two billing models that are never summed), the 18-command CLI table, the API routes, the dashboard
tabs, the security posture — including the credential file, the POSIX-modes-vs-Windows-ACL split, and
the fail-closed temp-then-rename path — and the **warning-kind table**.

- **`internal/analyze/readme_test.go`** mechanically checks the README's kind table against
  `internal/analyze/kinds.go`: the table's kind set must equal `AllKinds()` (br-GI-1-09) **plus**
  `nonAnalyzeKinds` (the kinds other packages emit: `analyzer_panic`, `source_mismatch`,
  `cost_drift`, `quota_window_approaching`). A variant spelling is a **build failure** — this is what
  pins `cache_prefix_below_minimum` as the one spelling named in the plan's T1 table, `kinds.go`, the
  README, and the tests. A cell containing a `|` or a newline also fails (it would corrupt the table).
- **Correct the dependency claim.** The plan's §Infrastructure says `modernc.org/sqlite` remains the
  only non-stdlib dependency. That is **false as written** and br-GI-1-04's bead says so: Go 1.24's
  stdlib has no brotli or zstd decoder, and Claude Code sends
  `Accept-Encoding: gzip, deflate, br, zstd`, so `internal/decode` imports
  `github.com/andybalholm/brotli` and `github.com/klauspost/compress` (its `zstd` subpackage). The
  README and the context docs must state the three non-stdlib modules and why, so the plan's
  one-dependency sentence does not survive into the shipped docs. (`go.mod` is the source of truth;
  the README's list matches it.)

**2. Context docs.** `docs/context/` — the architecture, storage schema, and the cost/quota model as
they shipped, so a future contributor does not re-derive them from the code. Keep them short and
pointing at the code, not restating it.

**3. Governance — verify, do not re-create.** The plan's bead table puts `.githooks/{commit-msg,pre-commit}`
and `.github/workflows/{branch-guard,main-guard}.yml` in this bead; they were **moved to br-GI-1-01**
so the `GI#<n>` prefix is enforced from the first commit (see the summary flag). This bead verifies
they exist and are installed (`core.hooksPath`), and that `branch-guard`/`main-guard` match the repo's
`GI-1-…`-branch policy (the recorded deviation: this repo has no `develop`, so v1 lands on `main` via
a `GI-1-…` branch). A bad commit message is rejected by the hook.

**4. End-to-end acceptance.** `docs/acceptance.md`, the runbook the plan's §Test strategy closes on,
executed once against a real install and its observed results recorded:

- Start `clens serve`; confirm the banner and that both listeners answer.
- Point Claude Code at it via `ANTHROPIC_BASE_URL`; make a real call; confirm exactly one `events`
  row, the SSE feed updates, and the Sources tab shows the proxy source green.
- Run `clens refresh`; confirm a JSONL row, a quota snapshot, and (with an Admin key) an admin row.
- Confirm the Quota tab shows a percentage when a limit is configured and `unconfigured` otherwise.
- Confirm `clens reconcile` renders computed and billed in separate labelled columns.
- **Record the test-11(b) result.** br-GI-1-11 flags the live `request-id`↔`requestId` equivalence
  verification as an operational prerequisite owned by no bead. This runbook is where it is captured:
  grab one live response's `request-id` through the proxy and the matching Claude Code JSONL line and
  assert equality, then record the outcome in `docs/planning/GI-1-claude-lens-v1.md`'s *Cross-source
  identity* section (equal → the header is the key; not equal → the fallback key is primary and
  br-GI-1-11's switch flips).

## Rationale

The README-consistency test is the mechanism the plan chose to keep spellings settled in one place;
without it the kind catalogue drifts by diligence. The dependency correction is the one place the
shipped docs would otherwise contradict the shipped `go.mod`. The acceptance run is the only thing
that exercises the whole process against a real client, and it is the natural home for the live
id-equivalence check that otherwise has no owner.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `readme_test.go` passes: the README's kind table equals `AllKinds() ∪ nonAnalyzeKinds`, with no
  extras and no variant spellings.
- The README's dependency list matches `go.mod` (the three non-stdlib modules) and the plan's
  one-dependency sentence is corrected in the docs.
- `docs/context/` and `docs/acceptance.md` exist.
- `.githooks/{commit-msg,pre-commit}` are present and installed; a commit message without a `GI#<n>`
  prefix is rejected; `branch-guard`/`main-guard` match the recorded branch policy.
- The acceptance runbook has been executed once and its results (including the test-11b outcome)
  recorded.

## Test Specifications

- Unit Tests (`internal/analyze/readme_test.go`):
  - The README kind table's set equals `AllKinds()` ∪ `nonAnalyzeKinds` — every kind present exactly
    once, no extras.
  - No kind cell contains `|` or a newline.
  - `cache_prefix_below_minimum` appears with that exact spelling (a variant fails the build).
- Unit Tests (`internal/analyze/readme_test.go`, dependency half — or a docs test):
  - Every module `go.mod` requires appears in the README's dependency list.
- Manual (recorded in `docs/acceptance.md`):
  - The full runbook above, including the test-11b live `request-id` ↔ `requestId` capture and its
    result written back into the plan.

## Files to Touch

- `README.md` (modify — the real document; br-GI-1-01 left a stub)
- `internal/analyze/readme_test.go` (create — the README↔`kinds.go` consistency test)
- `docs/context/` (create — architecture, schema, cost/quota model)
- `docs/acceptance.md` (create — the executed acceptance runbook)
- `docs/planning/GI-1-claude-lens-v1.md` (modify — record the test-11b result and the dependency-claim
  correction)
- `.githooks/*`, `.github/workflows/*` (verify only — br-GI-1-01 owns them)
