# Bead br-GI-9-06: the live-acceptance run, recorded in the plan

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §5 (Live acceptance: the four
checks), §2.3 and §2.4 (the frozen readings the run re-confirms rather than reproduces), §6 (the
`source_mismatch` warnings the operator should expect), §9 bead 07

- **Bead ID**: br-GI-9-06
- **Priority**: P2 (medium)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-9-04 (the command the run invokes), br-GI-9-05 (the wording the run's
  findings land beside)
- **Blocks**: None

> **This is the only bead that writes the plan.** Everywhere else the plan is read-only input; here the
> run's result is recorded in §5, which is what "recorded manual run" means. Do not edit any other plan
> section to make a number agree — if a number disagrees, record the disagreement.

## Description

A manual run against the live database (`~/.clens/lens.db`), after br-GI-9-04 lands, performing §5's
four checks and recording the outcome in the plan's §5. The §2/§6 **decoded-body figures are marked
`re-confirmable`, not independently reproduced** — the run re-confirms them at the current corpus size;
it does not claim to re-derive them from scratch, and a movement that the story does not explain is a
finding, not a number to overwrite.

**The four checks, in order:**

1. **Premise check.** Re-run the §2.3 comparison **by the method §2.3 states** — the
   `source='proxy' AND resp_body IS NOT NULL` population, the `decode.Body` + config-cap decode, D2's
   id precedence, the transcripts under `~/.claude/projects/` counted distinct per file and per id —
   now using the **production** `parse.ExtractUsage`'s `Usage.MessageID`. **The verbatim-id relation
   must still hold at the same ratio** (~77–79% of proxy body ids; §2.3 states both passes' figures).
   The relation is the invariant; the absolute is not. This measures ids the change does not touch, so
   it confirms the **premise** still holds — it does **not** validate the story.
2. **The check that validates the story.** Count post-rekey rows whose `source_refs` contains **both**
   `proxy` and `jsonl`: **0 today**, expected **the same ratio as check (1)** — ~77–79% of the
   post-rekey window's distinct proxy body ids, i.e. a few hundred to **~950** rows on this DB — and
   **the run reports the number**. The target is stated in **rows** because one id collapses to one row
   under D1, so the row count equals the id count. **The frozen 511 is one pass's absolute from the
   smaller corpus, not the invariant**, and the earlier "238" was a *pair* count over transcript
   **lines** with the unit wrong (§2.3) — neither is the target.
3. **The row count drops, read from the command's own deleted-vs-inserted report** (D4) — **not**
   compared against §2.4's 28,405. Those are two measurements of two different groupings: §2.4 groups
   by `(session_id, token quintuple, 5s bucket)` to estimate how much duplication exists, while the
   rekey drops exactly the rows whose `requestKey` collapses, one per distinct id. The report is the
   larger number, which is the point: a post-rekey JSONL count of at most `16,804 + 42,008 = 58,812`
   rows against today's 88,032 puts the drop at **≥ 29,220**, already more than §2.4's estimate. The
   acceptance line is **"the report shows a drop of tens of thousands of rows"**, and **the run says
   which**.
4. **The drawer is already measured** (§2.3). The discriminator returned **0** mismatches over the
   **303** ids with no verbatim transcript match — every miss a true absentee — so the live run
   **re-confirms the zero** rather than establishing it.

**Then report the expected handful of `source_mismatch` warnings** (§6) — the operator should see
them so they do not read as failure. §6's measured figures: of **90** ids with a complete capture on
both sides, **87** agree exactly on all six token columns and **3** disagree with the signature
`input_tokens +128, cache_read_tokens −128`, raising `source_mismatch` at `SeverityError` on merge. The
counts are that pass's and will move; the **shape** is what the report carries.

**Run it with `clens serve` stopped** — a live tailer writing while cursors are zeroed is a race the
run does not need to have (§6). Take a copy of the database first if the run is against the working
one.

## Rationale

The story's central claim — that the two writers now meet — is only ever true of real data, and the
integration test asserts it on a fixture. The live run is the statement that it holds on the corpus
that motivated the change, and §2's frozen figures are re-confirmed at the grown corpus rather than
trusted. §6 is explicit that the §2/§6 decoded-body figures are re-confirmable, not independently
reproduced, so the run's job is the relation and the zero, not a fresh derivation.

## Outcome Definition

- All four checks run against the live DB after br-GI-9-04, and each result is recorded in §5.
- Check (1) reports the ratio and notes the relation holds; check (2) reports the **row count** of
  rows carrying both sources (0 before) against the ratio; check (3) reports the command's own
  deleted-vs-inserted drop; check (4) re-confirms the zero over the currently-measured 303-id set.
- The `source_mismatch` warning count the run produces is reported.
- Any figure that moved and the story does not explain is **recorded as a finding**, with the frozen
  §2/§6 reading left intact and marked `re-confirmable`.

## Test Specifications

- Unit Tests: none — this is a manual run, not a suite.
- Integration Tests: none.
- E2E: the four checks above against the live database, each recorded in the plan's §5 with its
  query, its result, and the date (as §6's own measurements are dated).

## Files to Touch

- `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` (modify — §5's Live acceptance: record the four
  checks' results and the `source_mismatch` count; the §2/§6 decoded-body figures are marked
  `re-confirmable`, not rewritten)
