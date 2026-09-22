# Bead br-GI-11-10: reconcile the context docs that describe the three fixes

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4's Docs table (the
`cost-and-quota.md`, `storage-schema.md`, `glossary.md`, `data-privacy-and-compliance.md` and
`decisions/003-full-bodies-stored.md` rows), §6 (the historical at-cap marker row), §7 (the security
paragraph: the privacy cost must be **stated**, not discovered)

- **Bead ID**: br-GI-11-10
- **Priority**: P1 (high)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-11-01 (the rounding rule the docs now state), br-GI-11-04 (the merge rule),
  br-GI-11-05 (`capture_complete` is a backfillable fact), br-GI-11-07 (the cap default)
- **Blocks**: None

> **The docs are the map, and the code wins wherever they disagree — which is why this bead exists.** Each
> file below states a figure or a rule that GI-11 changes; leaving one stale leaves a doc that
> contradicts the code it routes to. Cite the **section** in the plan, and name the doc's own section
> headings rather than line ranges — the plan's line-number self-pointers were corrected repeatedly and
> got the number wrong.

## Description

### `docs/context/cost-and-quota.md`

- §"Exact money, rounded per token class" (`:72-81`) documents the per-class rounding as an invariant and
  links **one** test **by name** — `TestComputeBatchRoundsPerClass` at `:78`. (The plan's "links the two
  tests" overstates it; the second test lives only in `docs/planning/GI-3-deepseek-peak-pricing.md`.)
  **Rewrite the section to the round-at-display rule**: `Compute` sums exact `big.Rat` per class and
  rounds nowhere; only the stored `REAL` is approximate; the display paths round. **The linked test name
  moves with the rename** — br-GI-11-01 rewrites `TestComputeBatchRoundsPerClass` to
  `TestComputeBatchHalvesExactly`, so the doc must link the new name (a doc linking a deleted test name is
  a stale pointer).
- Update the `Compute` description at `:97-100` to match.

### `docs/context/storage-schema.md`

- The `capture_complete` paragraph (`:95-103`) is **stale against the code**: it claims the flag also
  covers "ended without a `message_stop` event", but `proxy.go:107` computes it from the two buffers'
  `truncated` bits **alone**. Fix that.
- **State the merge rule**: the surviving row's flag follows the owner(s) of the bodies it holds, the
  `&&` of two owners when the retained bodies have different owners, and `existing`'s flag when it holds
  none (br-GI-11-04).
- **Record the cap-raise consequence (F1.8):** the length-vs-cap truncation *marker* compares a stored
  body against the **current** cap, so raising the default to 2 MB silently reclassifies every historical
  at-cap row (262,144 bytes) as `Complete` — on exactly the rows RC-B has just started flagging honestly.
  **Document that this is a known, accepted limitation** (a per-row recorded cap would be required to
  mark old rows, and the plan does not add one) **and that the authoritative signal is the
  `capture_complete` flag, not the marker.**

> **`docs/context/build-and-run.md` is deliberately *not* in this bead** — it moves with the other
> repeated figures to **br-GI-11-11**, which owns both its body-cap default (`:85`) and the operator's
> `DBPath` / `D:/clens/lens.db` note. Splitting one doc across two beads would leave the file argued
> over by two writers; the single-owner rule wins. Do not edit it here.

### `docs/context/glossary.md`

- The "**body cap**" entry (`:51`) states `default 262144` → 2,097,152.

### `docs/context/data-privacy-and-compliance.md`

- The 256 KB default as fact at `:37` and `:61`; this is the module INDEX routes "before changing what is
  captured" to, and §7 requires the larger-cap privacy cost be **stated** here rather than discovered.
- Note `:66-68` already records that the marker inference is "only as good as the cap not having changed"
  — it is the doc-side half of F1.8; keep it consistent with the `storage-schema.md` note.

### `docs/context/decisions/003-full-bodies-stored.md`

- "bounded by a 256 KB cap" at `:15` and the `--body-cap-bytes` row at `:20` — note the new default. The
  **decision itself** (full bodies under a bounded cap) is **unchanged**; only the number moves.

### What this bead does not do

- **No code comments.** The stale "256 KB" **code** comments (`consumer.go:25`, `rules.go:72`,
  `decode.go:90`, `export.go:133`) are br-GI-11-07's.
- **No count or surface figures, and no `build-and-run.md`.** `INDEX.md`,
  `build-and-run.md`, `testing-and-quality.md`, `cli-and-tooling.md`, `workflows.md`, `dashboard.md`,
  `README.md` and `CLAUDE.md` are br-GI-11-11's — they move with the new subcommands, the new test
  files, the picker and the deployment note, and are grouped there so every repeated figure moves
  together.
- **No new doc files.**

## Rationale

`docs/context/INDEX.md` routes each of these modules to a specific surface ("before changing what is
captured", "before changing anything in `internal/web`", and so on), and each states a figure or rule
GI-11 changes. A doc that documents the per-class rounding as an invariant is worse than silent — it
tells the next reader the defect is intended. The privacy note is not cosmetic either: §7 is explicit
that a bigger cap stores more prompt and file content, which is a real cost paid for observability and
must be stated rather than discovered.

## Outcome Definition

- Every doc above agrees with the code as of the end of br-GI-11-01/04/05/07.
- `cost-and-quota.md` states the round-at-display rule and links the **new** test name
  (`TestComputeBatchHalvesExactly`), not the deleted one.
- `storage-schema.md` states the `capture_complete` meaning (the two buffers' truncation bits), the merge
  rule, and the cap-raise marker limitation with the `capture_complete` flag as the authoritative signal.
- All 256 KB figures in these files read 2 MB / 2,097,152.
- `data-privacy-and-compliance.md` states the larger-cap privacy cost.
- No doc in this bead links a symbol or test that no longer exists under that name.
- No `.go` file is touched by this bead.

## Test Specifications

- **Unit Tests:** none — these are prose files, and the repo's Go tests do not read `docs/context/*`.
- **Verification:** grep the edited files for `262144`, `256 KB` and `256KB` and confirm every remaining
  hit is an intentional historical note (e.g. §6's "historical at-cap rows are 262,144 bytes"). Grep for
  the old test name `TestComputeBatchRoundsPerClass` and confirm no `docs/context/*` file still links it.
- **Integration Tests:** none.

## Files to Touch

- `docs/context/cost-and-quota.md` (modify — the rounding section → round-at-display; the `Compute`
  description; the linked test name)
- `docs/context/storage-schema.md` (modify — the `capture_complete` paragraph; the merge rule; the F1.8
  marker limitation)
- `docs/context/glossary.md` (modify — the "body cap" entry)
- `docs/context/data-privacy-and-compliance.md` (modify — the default at two sites; the larger-cap
  privacy cost)
- `docs/context/decisions/003-full-bodies-stored.md` (modify — the cap figure and the `--body-cap-bytes`
  row; the decision is unchanged)
