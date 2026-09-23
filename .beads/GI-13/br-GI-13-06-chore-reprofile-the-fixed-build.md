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

### The recorded pre-fix signature — the "before" half, kept here

The pre-fix capture has been deleted from disk, so the excerpt is recorded here rather than left as a
path. It was taken from the **live** process at `http://127.0.0.1:8899/debug/pprof/profile?seconds=30`
against the live store `D:/clens/lens.db` while the dashboard was hanging — **not** from a probe
instance; those ran on 8897/8898 with their own stores, and both are gone.

`go tool pprof -top -cum` over the 30s capture, total samples 28.95s (96.50% of one core):

```
  96.34%  internal/consumer.(*Consumer).flush
  94.85%  internal/consumer.(*Consumer).runSessionRule
  94.85%  internal/store.(*Store).SessionEvents     <-- the whole story, on one line
  94.23%  modernc.org/sqlite/lib._sqlite3VdbeExec
  89.91%  runtime.cgocall        (89.91% FLAT -- the cgo hop into sqlite)
  59.76%  database/sql.(*DB).queryDC
```

`SessionEvents` is **94.85%** of the profile and its only caller is `runSessionRule`, inside `flush`.
That is C1's and C2's target measured directly: the per-row session re-read. The flat-view markers
(`_vdbePmaWriteBlob` 31.6%, `_vdbeIncrSwap` 27.5%, `_vdbePmaReadBlob`, `_winRead` 57.5%,
`_vdbeColumnFromOverflow` 34.6%) are the temp-file sort spill *inside* that `_sqlite3VdbeExec`.

Compare the after-profile against these rows, not against a wall-clock figure.

### The trap this bead must not fall into

Two of the investigation's captures were of an **idle** process (0.33% and 0.27% of a core, top frame
`runtime.(*timers).run`), and one was written to a file named `cpu-top.txt`, where it read as evidence
that there was no bug at all. **Confirm the process is burning CPU before sampling** — read
`TotalProcessorTime` twice, ten seconds apart; if it is under ~30% of a core, let it run and
re-capture. An after-profile taken against an idle fixed build would "pass" for the wrong reason.

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
