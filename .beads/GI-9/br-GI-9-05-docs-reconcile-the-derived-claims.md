# Bead br-GI-9-05: docs — reconcile the cross-source identity claim, the merge flow, the destructive-command claims, and add decisions 008 and 009

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §3 D6 and D7 (the doc
consequences), §4 (every `docs/` row), §5 (the live-acceptance line the PR body carries), §2.5, §6,
§9 bead 06

- **Bead ID**: br-GI-9-05
- **Priority**: P1 (high)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-9-01, br-GI-9-02, br-GI-9-03, br-GI-9-04, br-GI-9-07 (the docs describe the
  behaviour those beads create; the two new decision records cite their designs)
- **Blocks**: None

> **Sweep sibling copies.** This is the editorial half of the story, and the plan's own history is the
> warning: it repeatedly fixed a claim in one place and left a copy elsewhere (F14.1, F19.1 — the
> `source_refs`-vs-row-count guard shipped a live copy after a fix). **Before this bead is called done,
> grep the repo for each corrected phrase**, not just the file §4 names: the "only destructive
> command" claim, the "the hash is not needed" claim, the documented-fallback claim, and the test-file
> count. A corrected claim with a surviving sibling is the exact defect this story keeps producing.
> (The code-side sweep — moved/renamed/deleted symbols — is br-GI-9-04's; this instruction is about the
> prose.)

## Description

Six independent doc corrections plus two new decision records. Each is a claim the code change makes
false or a claim that was already false and is being fixed while the file is open.

### 1. D6 — the cross-source identity claim and test 11(b)

Two documents disagree about test 11(b): one implies it is exercised, the other records it as never
exercised. D6 settles it as **never exercised on this install, therefore moot here — kept distinct
from tier 1's unguarded risk on an install whose upstream sends the header**.

- `docs/planning/GI-1-claude-lens-v1.md` **§Cross-source identity**: two corrections. (a) The line-1105
  fallback claim — the **documented fallback was never built** (the implemented one is the namespaced
  `jsonl:<sessionId>:<uuid>` / `proxy:<sha256>:<started_at_ns>:<attempt>` pair, §2.5). (b) test 11(b)
  was **never exercised**, not falsified — **scoped**: moot on this install, load-bearing and
  unverified, **silently failing** where an upstream sends `request-id` (§6).
- `docs/acceptance.md` — test 11(b): the same scoping, worded the same way, so the two files stop
  disagreeing.

### 2. The merge flow and the identity rule

- `docs/context/workflows.md` — the merge **now fires** (the two writers produce the same key); the
  identity rule (the message id is the request's identity on both sides); and the session rule (D7) —
  the proxy adopts `x-claude-code-session-id`, so `clens sessions` shows conversations rather than
  heuristic groups.

### 3. The destructive-command claims — three files, one correction

`clens rekey` is a **second** command that deletes rows, so every "the only destructive command" claim
is now false. The three files carry the claim independently and **move together**:

- `docs/context/cli-and-tooling.md` — **"the one destructive command" becomes two**; add the `rekey`
  row (its three passes, `--yes` / `--dry-run`); and `:6`'s "map … of 18 subcommands" becomes **19**.
- `docs/context/storage-schema.md` — its own "the only destructive command" claim at `:151`, the same
  correction in a different file.
- `docs/context/data-privacy-and-compliance.md` — `:111` asserts purge is the only command that
  deletes rows; its definite article ("**The destructive command** defaults to the opposite of
  destructive") becomes misleading with a second one. (`:106-107` is the retention table and asserts
  **no** exclusivity, so **it is left alone**.)

The Go doc comment `internal/cli/purge.go` carries, saying the same thing, is **br-GI-9-04's edit** —
this bead corrects the three docs that cite it, so the citation and its target agree.

### 4. The purge `--dry-run --yes` misstatement (independently wrong, fixed while the file is open)

`docs/context/data-privacy-and-compliance.md:111-113` says `--dry-run --yes` "reports and deletes in
one pass", citing `internal/cli/purge.go:20` for it — which says the **opposite** ("`--dry-run --yes`
is simply a dry run"), and so does the code (`:45` refuses only when **neither** flag is set, and
`purgeByAge` returns before deleting when `dryRun`). Fix both the behaviour and its own citation.

### 5. D7's privacy statement

`docs/context/data-privacy-and-compliance.md` also carries: `rekey`/D7 add **no new capture and no new
retention** — the conversation id is read out of the `req_headers` the consumer already stores
verbatim, and re-attribution rewrites a column from a value already on disk. State it here, beside the
destructive-command correction.

### 6. The test-count clauses and the index/decision tables

- `docs/context/testing-and-quality.md` — `:12`'s "**51 test files, 15,051 lines**" and its
  re-measurement parenthetical are **one clause**: the parenthetical says the GI-7 re-measure "took the
  count 50 → 51", so bumping only the first number leaves the sentence contradicting its own
  provenance and leaves 15,051 stale. **The whole clause moves together** — **52 test files**, the new
  line count, and the provenance sentence naming **this** story's new files (`rekey_test.go`, the added
  session/parse cases) rather than GI-7.
- `docs/context/INDEX.md` — "18-entry dispatch table in `cmd/clens/main.go`" becomes **19**; `:41`'s
  "seven genuine forks" becomes **nine** (008 and 009); `:37`'s "trigger: 51 test files" becomes **52**.
  (`:114`'s "moved six → seven" is dated history and **stays**.)
- `docs/context/decisions/000-index.md` — "**Seven** architectural forks" becomes **nine** — **twice**,
  at `:5` and again at `:25` ("the status of all seven") — plus a row for 009 in the table.
- `docs/context/decisions/008-<slug>.md` (new) — **the identity decision (D1)**.
- `docs/context/decisions/009-<slug>.md` (new) — **the session decision (D7)**: the proxy adopts the
  conversation id the request already carries.

### The PR body's visible-change line

§5's live acceptance says the PR body must carry the visible-change numbers, and §6 states them: the
proxy's **184 heuristic `s_…` sessions collapse to the 3 real conversations**, a long conversation
**stops splitting at the inactivity gap**, the **~3%** of header-less requests fall back to the
gap-window behaviour, **and the view grows by the ~207 JSONL sessions the recorder now materialises**
(`clens sessions` stops being proxy-only). **Both numbers, not the collapse alone** — a reader who sees
184 → 3 in the PR and ~207 in `clens sessions` reads the difference as data loss. **A fifth change**:
`x-clens-session` stops being grouping-only and becomes the stored id, so a client setting it to a
label now sees that label in the id column. This bead supplies the wording; the PR body uses it.

## Rationale

Every one of these is a document asserting something the code did not do before and does after — or
never did at all. Leaving them is how a repo ends up with a decision record that contradicts the code
it describes, which is exactly the D6 disagreement this bead closes. The two new records are the
durable form of D1 and D7: the *why*, in the place the next reader looks.

## Outcome Definition

- The D6 "never exercised / moot here" scoping reads identically in `GI-1-…v1.md` and
  `docs/acceptance.md`, and neither implies the documented fallback exists.
- "Only destructive command" no longer appears unqualified in
  `cli-and-tooling.md`, `storage-schema.md` or `data-privacy-and-compliance.md`; the subcommand count
  is 19 wherever it is stated.
- The purge `--dry-run --yes` sentence matches the code and cites a line that says the same.
- The privacy statement records "no new capture, no new retention".
- The test-fixture clause is internally consistent (52 test files, new line count, provenance naming
  this story); `INDEX.md` and `000-index.md` state nine forks and 19 subcommands.
- Decisions 008 and 009 exist, are listed in `000-index.md`'s table, and are written from the plan
  (D1, D7).
- No sibling copy of any corrected claim survives a repo-wide grep.

## Test Specifications

- Unit Tests: none — docs only, no code path.
- Integration Tests: none.
- E2E: none.
- **Doc assertions (reviewer-checkable, not runnable)**: a repo-wide grep for each corrected phrase
  returns only the corrected occurrences — `"only destructive"`, `"the hash is not needed"`,
  the documented-fallback description, `"51 test files"`, `"seven"` forks, `"18"` subcommands. The
  grep is the check; a surviving sibling is the failure.

## Files to Touch

- `docs/planning/GI-1-claude-lens-v1.md` (modify — §Cross-source identity: the fallback claim and the
  test 11(b) scoping)
- `docs/acceptance.md` (modify — test 11(b), the same scoping)
- `docs/context/workflows.md` (modify — the merge now fires; the identity rule; the session rule)
- `docs/context/cli-and-tooling.md` (modify — the `rekey` row; "the one destructive command" → two;
  18 → 19 subcommands)
- `docs/context/storage-schema.md` (modify — its own "only destructive command" claim at `:151`)
- `docs/context/data-privacy-and-compliance.md` (modify — the destructive-command article; the
  `--dry-run --yes` misstatement and its citation; the no-new-capture privacy statement)
- `docs/context/testing-and-quality.md` (modify — the test-file/line clause as one unit)
- `docs/context/INDEX.md` (modify — 19 subcommands; nine forks; 52 test files)
- `docs/context/decisions/000-index.md` (modify — nine forks, twice, plus 009's table row)
- `docs/context/decisions/008-<slug>.md` (create — the identity decision, D1)
- `docs/context/decisions/009-<slug>.md` (create — the session decision, D7)
