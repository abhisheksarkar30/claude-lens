# Bead br-GI-13-06: Re-profile the fixed build against the same store and record the result

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §7 R5, §3.4 C4, §6 test 10

- **Bead ID**: br-GI-13-06
- **Priority**: P2 (medium)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-13-01, br-GI-13-02, br-GI-13-03, br-GI-13-04, br-GI-13-05 (every code change
  must have landed)
- **Blocks**: None

> **This bead is a manual verification step, not code.** It has no files to touch and no in-repo test:
> the instrument it uses (br-GI-13-04's profiler) is aimed at the fix, and the artifact is a recorded
> result, not a new source file. It exists because the perf claim is otherwise **unmeasured after the
> fix** (§7 R5) — the profile proves the *current* cost and the mechanism, not the new one.

## Description

After br-GI-13-01…05 land, take the **same 30-second CPU profile**, against the **same store** and
under the **same traffic**, that produced §1's signature:

1. Run `clens serve` with the profiler enabled (`--pprof-addr`/`CLENS_PPROF_ADDR`, br-GI-13-04) against
   the store that showed the hang.
2. Drive the same load that reproduced it, and capture
   `/debug/pprof/profile?seconds=30`.
3. `go tool pprof -top` the capture and read the markers.

**The success criterion is structural, not a wall-clock number:** the spill markers
`_vdbePmaWriteBlob`, `_vdbeIncrSwap` and `_vdbePmaReadBlob` are gone from the top of the profile, and
the per-row re-read/re-fold that fed them is no longer issued once per written row. Do **not** pin the
result to a live-store row count or a quoted millisecond figure — both are moving targets.

Record the result — the `-top` excerpt before and after, and the store's size/shape at capture — in
the story's PR body (or the bead's tracking comment), so the claim is checkable rather than asserted.

## Rationale

`_vdbePmaWriteBlob` + `_vdbeIncrSwap` + `_vdbePmaReadBlob` are the sort spilling to a temp file; their
disappearance is exactly the property br-GI-13-02's index and br-GI-13-01's dedupe are supposed to
produce. This is the story's own instrument, applied to the story's own fix (§7 R5). Without it, the
fix ships on the strength of an argument, not a measurement.

## Outcome Definition

- A 30-second CPU profile of the fixed build, against the same store and load, is captured.
- `_vdbePmaWriteBlob`, `_vdbeIncrSwap` and `_vdbePmaReadBlob` do **not** appear in the profile's top
  (they were 31.6%, 27.5% and 34.2% respectively in the recorded pre-fix signature).
- The result — pre/post `-top` excerpts and the store's shape at capture — is recorded in the story's
  PR body or the bead's tracking comment.
- If the markers **do not** disappear, that is a finding: report it and reopen the relevant bead
  rather than recording a pass.
- **Verification** (from the repo root, before profiling): `go build ./... && go vet ./... && go test
  ./... -count=1` is green, and `go test ./internal/proxy/` (the TTFB gate, §6 test 10) is
  **unchanged** — this story touches nothing in the hot path and the gate says so.

## Test Specifications

- Unit Tests: none — this bead adds no code and changes no behaviour.
- Integration / manual: the re-profile described above. It is run once by hand; it is not an in-repo
  test, because it needs the live store and a real 30 seconds of traffic.

## Files to Touch

- None. The artifact is a recorded profile result (PR body / tracking comment), not a file in the tree.
