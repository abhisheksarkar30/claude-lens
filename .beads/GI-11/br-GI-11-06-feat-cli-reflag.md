# Bead br-GI-11-06: `clens reflag` — the CLI shell for the historical `capture_complete` repair

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4 Code (the
`internal/cli/reflag.go` *(new)* row) and §4's "The historical `capture_complete` backfill — `clens
reflag`" block (the "Separate command with one job" paragraph and the Risk paragraph), §4's Docs row
for `cli-and-tooling.md` / `INDEX.md` / `main_test.go` (the count now moves 19 → 21), §5 Unit (the
`Reflag` bullet)

- **Bead ID**: br-GI-11-06
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-11-05 (`Store.ReflagIncompleteCaptures` is the one statement this shell
  drives), br-GI-11-03 (**this bead owns `internal/cli/cli_test.go`'s GI-11 changes, and the
  credential-containment map it extends names `runReprice` — so `cli.Reprice` must already exist**)
- **Blocks**: br-GI-11-09 (the dispatch registration names `cli.Reflag`), br-GI-11-11 (the README and
  `cli-and-tooling.md` name the command)

> **`reprice` and `reflag` are both "the command that fixes history", and they share a shape — keep them
> separate commands nonetheless.** One job per command (following `purge`, `rekey`, `reprice`), and the
> same `--dry-run` / `--yes` gate. They are separate beads because they repair separate stored facts
> (`reprice` rewrites cost columns; `reflag` rewrites only `capture_complete`), but their shells are the
> same shape and the `purge` precedent is what both mirror. Do **not** fold them into one command.

> **`reflag` is a `--yes`-gated *writer*, not a destructive command.** It rewrites one column and
> deletes nothing. The story's destructive count stays **two** (`purge`, `rekey`); see br-GI-11-11 for
> the four-writers / two-destructive distinction the docs must carry. Do **not** edit
> `internal/cli/purge.go:19-25` or `internal/cli/rekey.go:17` to say "four" — both remain true.

## Description

`clens reflag` is **the CLI shell only** — `--dry-run`/`--yes` and reporting — exactly as `purge` and
`rekey` split their CLI from their store work. The one `UPDATE` and the three counts live in
`internal/store` (br-GI-11-05).

### The command's shape

Mirror `purge.go:26-79`:

- Take `--dry-run` and `--yes` with the existing `hasFlag` helpers.
- Refuse when neither is passed, with the same "refusing to … without `--yes` (add `--dry-run` to see
  what would go)" wording `purge` uses.
- Open the store via `openStore(args)`.
- `--dry-run` prints the **three buckets** (flipped / already honest / residual) and writes nothing;
  `--yes` performs the repair and prints the same three.
- Report the **residual** as the honest ceiling: rows laundered by the `||` but carrying no
  `Content-Length` evidence cannot be repaired, and the command says so rather than guessing.

### The in-repo tests — this bead owns `internal/cli/cli_test.go`

There is **no dedicated `internal/cli/reflag_test.go`**, and adding one would break the story's stated
test-file count. This bead edits `internal/cli/cli_test.go` instead, which is where the repo's
single-purpose command CLI cases already live:

- **`reflag`'s CLI-level `--dry-run` "changes nothing" case goes in `internal/cli/cli_test.go`.** The
  precedent is `purge`: it has **no dedicated test file at all**, and its CLI cases live in
  `cli_test.go` (which calls `runPurge` with `--dry-run`). `rekey` earned its own 45 KB file by size,
  not by convention — the repo's rule is **not** "one test file per command". Fewer files, the closest
  precedent for a single-purpose destructive command, and **no post-convergence change to a reviewed
  number** (the story's count stays **52 → 56**; do not re-open what round 8 settled).
- **Add both `"reprice"` and `"reflag"` to `TestNoCommandPrintsACredential`'s `runs` map**
  (br-GI-9-04 added `rekey` the same way). The map carries **no count assertion**, so extending it is
  additive and safe. This bead owns that edit for **both** commands — which is why it depends on
  br-GI-11-03: the map entry calls `runReprice`, and leaving it to br-GI-11-03 would mean two beads
  editing one map.

> **Critical trap — the two lists in `cli_test.go` take *opposite* instructions, and they live in the
> same file.** `TestNoCommandPrintsACredential`'s `runs` map **must** gain `reprice` and `reflag`.
> The sibling `cases` slice in `TestEveryCarriedOverCommandRunsAgainstATempStore` **must not** — it is
> the **count-asserted 11-entry GI-1 carried-over set**, and adding a GI-11 command there
> *misdescribes* the set rather than extending it. (br-GI-9-04 recorded exactly this pair for `rekey`.)
> Do not carry either instruction across to the other list, and do not carry br-GI-11-09's
> `main_test.go` instruction across either — that file's `carriedOver` list **must** gain both names,
> which is the *opposite* of this file's `cases` slice.

### Risk (recorded so it is not lost)

`reflag` is the only write path that writes `capture_complete` (beside `reprice`, the only other new
write path into `events`), which is why it is gated like `purge`/`rekey`. It touches no body, no header
and no cost, so it is read-only without `--yes` and cannot widen the credential or privacy surface. Its
docs surface is the same `cli-and-tooling.md` / `INDEX.md` / `main_test.go` count change as `reprice`
(the one count now moves **19 → 21**, both `main_test.go` doc comments) — that is br-GI-11-09 and
br-GI-11-11's, not this bead's.

### What this bead does not do

- **No write logic.** The statement and counts are br-GI-11-05's.
- **No dispatch registration.** `cmd/clens/main.go` and `cmd/clens/main_test.go` are br-GI-11-09's single
  fan-in edit.
- **No `reprice` logic.** The reprice shell is br-GI-11-03; this bead only names `runReprice` in the
  shared credential map (hence the dependency).
- **No new test file.** `internal/cli/cli_test.go` is the home — see "The in-repo tests" above. Do not
  add `internal/cli/reflag_test.go`: the story's test-file count is **52 → 56** by full path
  (`internal/store/reprice_test.go`, `internal/cli/reprice_test.go`, `internal/store/reflag_test.go`,
  `internal/store/importguard_test.go`), and it is a number a converged review round settled.

## Rationale

RC-B's fix prevents *new* laundering; the information the `||` destroyed cannot be re-derived, so the
history needs a repair from the one surviving witness. A separate, one-job, `--dry-run`/`--yes`-gated,
idempotent command matches the repo's existing shape for "rewrite stored rows" (`purge`, `rekey`,
`reprice`) rather than inventing a fourth mechanism, and it mirrors `purge`'s shell so the CLI's own
conventions are not re-litigated.

## Outcome Definition

- `clens reflag` exists as `internal/cli/reflag.go`, with `runReflag(args, w)` reachable from a test as
  `purge`/`rekey` are.
- Without `--yes` (and without `--dry-run`) it refuses and writes nothing; with `--dry-run` it prints
  the three buckets and writes nothing; with `--yes` it performs the repair.
- `internal/cli/cli_test.go` names `reflag`'s `--dry-run` case **and** carries `reprice` plus `reflag`
  in `TestNoCommandPrintsACredential`'s `runs` map; the sibling `cases` slice in
  `TestEveryCarriedOverCommandRunsAgainstATempStore` is **unchanged**.
- `go build ./...`, `go vet ./...`, `go test ./internal/cli/` pass.
- **Manual verification (the PR's test plan):** `clens reflag --yes` then a second `clens reflag --yes`
  flips nothing on the second run (idempotence against a real store); the `--dry-run` "changes nothing"
  half is the in-repo case below.

## Test Specifications

All in `internal/cli/cli_test.go` (the `purge` precedent — no dedicated file). The store-level cases
are br-GI-11-05's, in `internal/store/reflag_test.go`.

- **Unit Tests:**
  - `TestReflagDryRunChangesNothing` — `clens reflag --dry-run` prints the three buckets and flips
    nothing: a seeded laundered row's `capture_complete` is still `1` afterwards.
  - Extend `TestNoCommandPrintsACredential`'s `runs` map with `reprice` **and** `reflag` (each invoked
    with `--dry-run`), the way br-GI-9-04 added `rekey`. Additive — the map has no count assertion.
  - **Leave `TestEveryCarriedOverCommandRunsAgainstATempStore`'s `cases` slice alone** — it is the
    11-entry GI-1 carried-over set and is not extended by later stories.
- **Integration Tests:** none in-repo; acceptance #3/#4 are manual against a frozen store copy
  (br-GI-11-05/11) — #3's baseline is **read from this command's own `--dry-run` output**, not a
  hand-written query, and both criteria are **relations on one snapshot**, never quoted totals; their
  scope is this command's own, unfiltered (v12, §5).

## Files to Touch

- `internal/cli/reflag.go` (new — the shell: `--dry-run`/`--yes`, the three-bucket report, and the
  residual note; mirrors `purge.go:26-79`)
- `internal/cli/cli_test.go` (modify — add `reflag`'s `--dry-run` case, and add **both** `reprice` and
  `reflag` to `TestNoCommandPrintsACredential`'s `runs` map. **Do not** touch
  `TestEveryCarriedOverCommandRunsAgainstATempStore`'s `cases` slice — see the trap callout above).
