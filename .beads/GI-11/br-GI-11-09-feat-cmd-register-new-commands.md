# Bead br-GI-11-09: register `reprice` and `reflag` in the dispatch table — `carriedOver` 19 → 21

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §4 Code (the
`cmd/clens/main.go` and `cmd/clens/main_test.go` rows), §4's reflag Risk paragraph ("the one count now
moves **19 → 21**, both doc comments"), §5 (`main_test.go`'s `carriedOver`), Change History v2 F1.2/v2
F2.9 (the count-drift trap)

- **Bead ID**: br-GI-11-09
- **Priority**: P1 (high)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-11-03 (`cli.Reprice` must exist), br-GI-11-06 (`cli.Reflag` must exist)
- **Blocks**: br-GI-11-11 (`README.md` and `cli-and-tooling.md` state the new count)

> **This is the single edit that touches `cmd/clens/main.go` and `cmd/clens/main_test.go`, and it exists
> precisely so the two new commands do not each edit those files.** §4 is explicit: add **both**
> `"reprice"` and `"reflag"` to `carriedOver` — **19 → 21, since reprice and reflag ship together** —
> and move **every** doc comment that names the count. Registration happens *after* both CLI shells
> exist (`cli.Reprice`, `cli.Reflag`), so a one-command-at-a-time registration would either not compile
> or leave an intermediate count of 20 that a later edit would move again. Do not split this bead back
> into the two command beads.

## Description

`cmd/clens/main.go`'s `commands` map (`:16-39`) is the dispatch table. Register both new subcommands:

```go
"reprice": cli.Reprice,
"reflag":  cli.Reflag,
```

`cmd/clens/main_test.go` then breaks: `carriedOver` (`:15-24`) is a hand-written allowlist and
`TestEveryCarriedOverCommandIsDispatched` fails hard on any count mismatch — it asserts the dispatch map
**in both directions** (each name in `carriedOver` resolves to a non-nil entry, then
`len(commands) != len(carriedOver)` fails the test if the two sets differ in **size**). So:

- Add **both** `"reprice"` and `"reflag"` to the `carriedOver` list (with a `// br-GI-11-03 / br-GI-11-06`
  comment consistent with the list's existing per-bead grouping), taking it **19 → 21**.
- Update **every** doc comment that names the count: `:10-14` and `:26` both read "the 19 subcommands"
  and both go to 21, so neither is left stale. This is the count-drift trap F1.2a/F2.9 established —
  one comment moved, the other missed. **Both sites move in this one edit.**
- **Move the composition sentence too, not just the numeral.** `:10-14` names the count *and its own
  composition* — "doctor and serve, br-GI-1-15's six collectors, br-GI-1-17's ten readers and writers,
  and br-GI-9-04's rekey" (2 + 6 + 10 + 1 = **19**). Moving only the numeral leaves that sentence
  claiming **21** while its enumeration still sums to **19** — a third drift site inside the very
  comment this bead exists to keep true. The sentence therefore **gains `br-GI-11-03`'s `reprice` and
  `br-GI-11-06`'s `reflag`** (the same two groups `carriedOver` gains), so the comment's count and its
  own enumeration agree at 21.

**Do not resolve the red test by deleting the length check** (it is what notices a key that exists but
points at nothing, which would panic at dispatch rather than at build), **and do not resolve it by
leaving a command out of the map** (then `clens reprice` / `clens reflag` is reported unimplemented).
The count check is correct; the list is what is stale.

### What this bead does not do

- **No `internal/cli/cli_test.go` edit.** That file is **br-GI-11-06's** (it adds `reflag`'s
  `--dry-run` case and the `reprice`/`reflag` credential-map entries). **The two files take *opposite*
  instructions and must not be conflated:** this bead's `carriedOver` in `cmd/clens/main_test.go`
  **must** gain both new names (the count moves 19 → 21), while `cli_test.go`'s sibling
  `TestEveryCarriedOverCommandRunsAgainstATempStore` `cases` slice — the GI-1 **11-entry** carried-over
  set — **must not** be extended.
- **No command logic.** The shells are br-GI-11-03/06; the store work is br-GI-11-02/05.
- **No docs.** The README / `cli-and-tooling.md` / `INDEX.md` count edits are br-GI-11-11's, and must
  agree with the 21 set here.

## Rationale

A subcommand that exists but is absent from the dispatch map reports "not implemented yet" at runtime
(`main.go:57-59`) and its store method is unreachable via the CLI; the hand-written allowlist is what
makes that visible at test time rather than at the operator's first invocation. Registering both
commands in one edit keeps the count moving once (19 → 21) and keeps every comment that names it in
agreement, matching the plan's own framing.

## Outcome Definition

- `commands` maps `"reprice" → cli.Reprice` and `"reflag" → cli.Reflag`.
- `carriedOver` names both `"reprice"` and `"reflag"`; its length is 21.
- **Both** doc comments that name the count (`main_test.go:10-14` and `:26`) say 21, not 19 — **and
  `:10-14`'s composition sentence gains `reprice` and `reflag`**, so its enumeration (2 + 6 + 10 + 1)
  sums to 21 and agrees with its own count.
- `TestEveryCarriedOverCommandIsDispatched` passes.
- `clens reprice` and `clens reflag` are dispatched (no "not implemented yet").
- `go build ./...`, `go vet ./...`, `go test ./cmd/...` pass.

## Test Specifications

- **Unit Tests:** `cmd/clens/main_test.go`'s existing `TestEveryCarriedOverCommandIsDispatched` is the
  gate — it goes red on the registration until `carriedOver` reaches 21, which is the requirement, not
  a bug to work around. No new test is added; the plan's §5 clause is "the dispatch map and that list
  are the same size".
- **Integration Tests:** none.

## Files to Touch

- `cmd/clens/main.go` (modify — add the two entries to the `commands` map `:16-39`)
- `cmd/clens/main_test.go` (modify — add `"reprice"` and `"reflag"` to `carriedOver`; move **both**
  "the 19 subcommands" comments `:10-14` and `:26` to 21, and **extend `:10-14`'s composition sentence**
  with `br-GI-11-03`'s `reprice` and `br-GI-11-06`'s `reflag`, so its enumeration sums to 21 too)
