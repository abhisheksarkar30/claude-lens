# Bead br-GI-11-11: move every repeated count and surface figure, and rewrite the README re-pricing section

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4's Docs table (the
`cli-and-tooling.md`, `build-and-run.md`, `INDEX.md`, `testing-and-quality.md`, `dashboard.md`,
`README.md` and `CLAUDE.md` rows), §4's "database moves to `D:`" rollout block, §4's reflag Risk
paragraph (the `main_test.go` count), §7 (the rejects-alternatives paragraph that reasons from
`purge`/`rekey` and must address the README section), Change History v7 F5.5/v8 F6.5/v9 F7.2 (superseded
by v10 F8.1)

- **Bead ID**: br-GI-11-11
- **Priority**: P1 (high)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-11-07 (the new cap default the README/`CLAUDE.md` figures state), br-GI-11-02
  and br-GI-11-05 (the two new `internal/store` test files), br-GI-11-03 and br-GI-11-06 (the two new
  `internal/cli` shells), br-GI-11-08 (the picker's line counts), br-GI-11-09 (the dispatch count moves
  19 → 21)
- **Blocks**: None

> **Every figure here is repeated at more than one site, and the plan names each site.** Moving one and
> missing its twin leaves a contradiction — sometimes **inside one file** (`dashboard.md:6`'s prose
> sentence versus its `:21-25` table). This is the same count-drift trap F1.2a/F2.9 established for the
> subcommand count. Grep each figure, do not trust the one site a row names.

> **The "destructive command" claim must not be inflated.** After `reprice` and `reflag` there are
> **four `--yes`-gated *writers*, but still exactly two *destructive* commands** — `purge` and `rekey`
> delete rows, while `reprice` and `reflag` only rewrite columns (`cost_usd` /
> `api_equivalent_cost_usd` / `cost_source`; `capture_complete`) and delete nothing. A literal
> "two → four destructive commands" edit would be **wrong**. The four writers share the `--yes` gate,
> not the property of destroying data.

## Description

### `docs/context/cli-and-tooling.md`

- "of 19 subcommands" at `:6` → **21**.
- Add `reprice` **and** `reflag` to the Commands table (`:15-35`).
- Narrow `ingest --rebuild`'s "**the re-pricing path**" at `:19` now that a dedicated reprice command
  exists. **The same claim lives at `workflows.md:140`** ("`clens ingest --rebuild` the re-pricing
  path"), so **both sites move together or both stay.**
- The destructive-command claim at `:37-49` ("**The two destructive commands**", which says purge/rekey
  **delete rows** and calls it "the pattern any future destructive subcommand should follow"): correct
  it to the post-GI-11 fact above — **four writers, two destructive**. **Do not** edit
  `internal/cli/purge.go:19-25` or `internal/cli/rekey.go:17` to say "four"; both remain true.

### `docs/context/testing-and-quality.md`

- The hand-maintained test-file count at `:12` ("**52 test files**") moves by **four** new files —
  `internal/store/reprice_test.go`, `internal/cli/reprice_test.go`, `internal/store/reflag_test.go`, and
  `internal/store/importguard_test.go` — **52 → 56**, **each named with its package** so the count is
  checkable, not just "the reprice file + its test". Re-measure and update.
- **Count by full path, not by basename.** The correct figure is **56**. §4's **live** Docs rows
  already say **four** new files and **52 → 56** (`:444`, `:445`, `:848`), listing the four by full
  path — so there is nothing to reconcile and no "three vs four" discrepancy to rediscover. The
  **"three** new files" phrasing survives only inside the **Change History** (`:840`, `:872-873`),
  where it is the record of v9's F7.2 counting by basename and v10's F8.1 correcting it to the
  by-path list. History is not an instruction: read the four paths above, write **56**, and leave the
  changelog entries as the record they are. There is **no `internal/cli/reflag_test.go`** — `reflag`'s CLI case lives in the existing
  `internal/cli/cli_test.go` (the `purge` precedent; see br-GI-11-06). Write **56**, and do not
  rediscover the "three vs four" wording as a new discrepancy. (§4's "three new files" phrasing is
  superseded: v9's F7.2 counted by basename and v10's F8.1 corrected it to the by-path list.)
- The same "52 test files" figure is repeated at `INDEX.md:37`, so **both sites move together**.

### `docs/context/INDEX.md`

- `:33` "the 19-entry dispatch table" → **21**.
- `:37` "52 test files" (the `testing-and-quality.md` trigger) → **56**, with the four new test files.
- `:40` "1030 lines" (the `dashboard.md` trigger) moves with the picker's edits.

### `docs/context/dashboard.md`

- Add the picker to the Calls and Stats filter rows in the per-tab route table (`:87-96` — "the call log
  with filters", "totals over a window, charted by day/week/month"); the Stats window control and its
  composition with the bucket axis (`s-granularity`); the new `TestAssets*` / `timeWindow()` source
  guards in the test inventory (`:176-183`).
- **Re-measure the `index.html` / `app.js` line counts**, which are repeated at **three** sites:
  `:6` ("1030 lines of hand-written JavaScript"), `:21-25` (the 148 / 1030 table) **and** `INDEX.md:40`
  ("1030 lines"). All three move together — `:6` is the prose sentence, a different sentence in the same
  file from the `:21-25` table, so moving one and not the other leaves a contradiction inside one file.

### `docs/context/workflows.md`

- `:140` — the `--rebuild` "re-pricing path" claim, moving with `cli-and-tooling.md:19`.

### `docs/context/build-and-run.md`

This doc is the operator's home for config, and it owns two edits that no other bead makes:

- The body-cap default at `:85` (`262144 (256 KB)`) and its storage trade-off → the new default
  (2,097,152 / 2 MB) and the larger storage cost. (Same figure as `glossary.md`/`data-privacy…`, which
  are br-GI-11-10's; the *file* is this bead's, so it is not edited twice.)
- **The `DBPath` / `D:/clens/lens.db` note.** §4's "database moves to `D:`" step changes where the store
  lives. Record in this doc that the store's location is `DBPath` in the operator's config (the repo does
  **not** own that file), and that after the move it points at `D:/clens/lens.db`.
  **The six ordered move steps in §4 stay a *rollout action* — an operator procedure documented here,
  not code.** No source file gains a hard-coded `D:\` path, and `clens` keeps resolving the store from
  config exactly as it does today; writing the path into Go would make the binary wrong on every other
  machine.

### `README.md`

- Any 256 KB / `body-cap` mention, and the `clens` command list for `reprice` **and `reflag`** (the table
  at `:88-105`).
- **Rewrite, not merely add to, the section "### Re-pricing rows already captured" (`:111-120`).** It
  currently states there is **no separate reprice command** and that `clens ingest --rebuild` is the
  re-pricing path. Replace it with what `reprice` does and **why `--rebuild` cannot do the job for RC-A's
  rows**: a re-ingest is a **JSONL** row, priced with `speed=""` / `serviceTier=""` (`jsonlogs.go:459`)
  and absorbed by the `request_id` merge, which never replaces the proxy's bodies (`merge.go:323-328`) —
  so it cannot reach the proxy-only rows that carry the zeroed costs. Note alongside it that **`reflag`
  is the matching historical repair for RC-B's flag** (a re-ingest cannot recover that either: the `||`
  destroyed the original bit).
- Fix the sentence immediately below the table at `:107-109`: "`clens purge` is **the one command** that
  destroys data" is **already false today** (since `clens rekey` deletes rows), and the same edit adds
  `reprice`/`reflag` to the table. Correct it to the post-GI-11 fact: **four `--yes`-gated writers, two of
  them destructive**.
- This is also the one place the repo records the *design intent* for having no reprice command, so §7's
  rejects-alternatives paragraph reasons from this section too — the rewrite must leave it consistent.

### `CLAUDE.md`

- `:128` states "policy + 256 KB cap" and is the *enforced-conventions* file; update the number so it
  states a true bound. The surrounding convention (credentials never reach the database; full bodies are
  the asset this repo protects) is **unchanged**.

### What this bead does not do

- **No code.** The `purge.go`/`rekey.go` destructive-count comments stay as they are (br-GI-11-07 reads
  them and leaves them true).
- **No behavior docs.** `cost-and-quota.md`, `storage-schema.md`, `glossary.md`,
  `data-privacy-and-compliance.md` and `decisions/003` are br-GI-11-10's. (`build-and-run.md` is *this*
  bead's, for both its cap figure and its `DBPath` note.)
- **No code, and no move performed.** The six ordered steps in §4 are a **rollout action** documented in
  `build-and-run.md`; this bead writes prose, not a migration, and puts no `D:\` path in a source file.
- **No new doc files.**

## Rationale

Every figure here is duplicated, and each duplicate is a place the story's own claims can silently
disagree with the code. The README rewrite is the load-bearing one: it is the repo's only record of
*why* there was no reprice command, and it currently tells the operator that `--rebuild` is the
re-pricing path — which is exactly the path that cannot reach RC-A's rows. Leaving it would send the
operator to a tool that silently does nothing for the defect GI-11 fixes. The
four-writers/two-destructive correction is the same kind of fix: the claim is already false before GI-11,
and the story is the moment it gets read.

## Outcome Definition

- No `docs/context/*`, `README.md` or `CLAUDE.md` figure contradicts the code: the subcommand count reads
  21 everywhere; the test-file count reads 56 everywhere; the `index.html`/`app.js` line counts agree at
  all three sites; every 256 KB reads 2 MB / 2,097,152 where the default is meant.
- `cli-and-tooling.md` and `workflows.md` both stop calling `--rebuild` "the re-pricing path".
- `README.md`'s "### Re-pricing rows already captured" states what `reprice` does and why `--rebuild`
  cannot reach RC-A's rows, and notes `reflag` as RC-B's matching repair.
- `README.md:107-109` states four `--yes`-gated writers, two destructive; `cli-and-tooling.md`'s
  destructive section agrees; `purge.go`/`rekey.go`'s comments are untouched and still true.
- `CLAUDE.md:128` states a true body-cap bound.
- `build-and-run.md` states the new cap default and records the store's `DBPath` (after the move,
  `D:/clens/lens.db`) as an operator-config fact, with the six move steps as a **rollout action** — and
  no source file contains a hard-coded `D:\` path.
- No `.go` file is touched by this bead.

## Test Specifications

- **Unit Tests:** none — these are prose files, and no Go test reads them.
- **Verification (grep-based, the count-drift trap):**
  - grep `19`-as-subcommand-count (`of 19 subcommands`, `19-entry`, `the 19 subcommands`) across
    `docs/context/*`, `README.md` and `CLAUDE.md` and confirm none remains.
  - grep `52` (`52 test files`) across `docs/context/*` and confirm both sites read 56.
  - grep `1030` across `docs/context/*` and confirm all three sites agree on the re-measured figure
    (measure the real `index.html`/`app.js` line counts after br-GI-11-08).
  - grep `256` / `262144` across `README.md`, `CLAUDE.md` and `docs/context/build-and-run.md` and
    confirm every remaining hit is an intentional historical note.
  - grep `D:\\` and `D:/clens` across the **source tree** (`*.go`, and the config templates) and confirm
    the only hits are in prose docs — the move stays a rollout action, and no Go file names the drive.
  - grep `the one command that destroys data` and `the two destructive commands` and confirm both read
    the four-writers/two-destructive fact.
- **Integration Tests:** none.

## Files to Touch

- `docs/context/cli-and-tooling.md` (modify — the subcommand count; the Commands table; the `--rebuild`
  claim; the destructive-command section)
- `docs/context/testing-and-quality.md` (modify — "52 test files" → 56, with the four files named)
- `docs/context/INDEX.md` (modify — `:33` 19 → 21; `:37` 52 → 56; `:40` the `dashboard.md` line count)
- `docs/context/build-and-run.md` (modify — the body-cap default at `:85` and its trade-off; the
  `DBPath` / `D:/clens/lens.db` note, with the six move steps stated as a **rollout** action, not code)
- `docs/context/dashboard.md` (modify — the per-tab route table; the picker surfaces and its composition
  with the bucket axis; the test inventory; three repeated line-count sites)
- `docs/context/workflows.md` (modify — `:140`'s `--rebuild` re-pricing claim)
- `README.md` (modify — the command table; the "### Re-pricing rows already captured" rewrite; the
  `:107-109` destructive-command correction; any 256 KB mention)
- `CLAUDE.md` (modify — `:128`'s "256 KB cap")
