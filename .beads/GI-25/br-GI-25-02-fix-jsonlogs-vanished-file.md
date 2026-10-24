# Bead br-GI-25-02: Skip a vanished transcript file quietly instead of logging an open error

**Plan Reference**: `docs/planning/GI-25-local-llm-toggle-and-store-hardening.md` v8, §2 R3a, §3.3, §5.2 (`TestTailFileSkipsVanishedFileQuietly`). Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-25-02
- **Priority**: P2
- **Status**: pending
- **Original Estimate**: 45m
- **Dependencies**: None
- **Blocks**: None
- **Commit**: `GI#25 fix: skip a vanished jsonl file quietly (br-GI-25-02)`

## Description

`jsonlogs: open … cannot find the file` comes from `tailFile`'s open branch (`internal/jsonlogs/jsonlogs.go:288-291`):

```go
fh, err := os.Open(f.Path)
if err != nil {
	log.Printf("jsonlogs: open %s: %v", f.Path, err)
	return stats
}
```

The file list comes from `walkFiles` (`jsonlogs.go:223,245-262`) one `Poll` iteration earlier; between that walk and this `os.Open`, Claude Code can remove the transcript file (TOCTOU). That is an expected, benign race — not a disk or permissions fault — and today it logs at the same level as a real I/O problem, which is the log-triage noise this bead removes. The cursor row for the removed file is keyed by path (`internal/jsonlogs/cursor.go`) and is left inert; orphan cursor rows are explicitly out of scope for this ticket (plan §4.2) and are not cleaned up here.

Change the open branch to:

```go
fh, err := os.Open(f.Path)
if err != nil {
	if errors.Is(err, fs.ErrNotExist) {
		return stats
	}
	log.Printf("jsonlogs: open %s: %v", f.Path, err)
	return stats
}
```

`fs.ErrNotExist` is already satisfied by the `*PathError` `os.Open` returns on a missing file via `errors.Is`, so no string-matching is needed. `io/fs` is almost certainly already imported in this file (it is used by `filepath.WalkDir`'s `fs.DirEntry` parameter at `jsonlogs.go:247`); add `errors` as a new import if it is not already present. Every other `os.Open` error (permission denied, a directory where a file was expected, disk I/O) still logs exactly as before — this bead narrows one specific, already-identified cause, not every open failure.

## Rationale

A file genuinely vanishing between the walk and the open is not an operator-actionable event — Claude Code manages its own transcript lifecycle — and logging it at the same visibility as a real I/O fault trains the operator to ignore the log line, which is worse than not logging it. `Poll`'s per-file isolation (`jsonlogs.go:218-220`, "a single bad file or line never stops the walk") is preserved unchanged: this bead only changes whether the skip is logged, not whether it happens.

## Outcome Definition

`go test ./internal/jsonlogs/ -count=1 -run TestTailFileSkipsVanishedFileQuietly` passes: a file present at `walkFiles` time and removed before `os.Open` produces zero `Stats`, no error return, and (asserted via a captured `log.Writer`) no log line. `go test ./internal/jsonlogs/ -count=1` stays green otherwise.

## Test Specifications

- `internal/jsonlogs/jsonlogs_test.go`: `TestTailFileSkipsVanishedFileQuietly` — build a `walkedFile` pointing at a path that does not exist (simulating "existed at walk time, gone by open time" without needing an actual race), call `t.tailFile(ctx, f)` directly, assert the returned `Stats` is the zero value and that nothing was written to a `log.SetOutput`-redirected buffer during the call.
- A second case in the same test (or a sibling) with a path whose parent directory does not exist either, still routing through `fs.ErrNotExist` — confirms the check is not accidentally narrower than `os.Open`'s actual error shape.
- **Negative control**: revert the `errors.Is(err, fs.ErrNotExist)` branch — the test must fail on a non-empty log buffer.

## Files to Touch

- `internal/jsonlogs/jsonlogs.go` (modify — `tailFile`'s open branch, lines 288-291; add `errors` import if absent)
- `internal/jsonlogs/jsonlogs_test.go` (modify — add `TestTailFileSkipsVanishedFileQuietly`)

## Review Notes

Implemented. `go test ./internal/jsonlogs/` green. Negative control (disabling the `ErrNotExist` branch) failed the new test. The test also pins that a non-ENOENT open error (an invalid name; a directory opens fine on Windows) still logs.
