package jsonlogs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func eventsByRequestID(t *testing.T, st *store.Store) map[string]*store.Event {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	out := map[string]*store.Event{}
	for _, ev := range evs {
		out[ev.RequestID] = ev
	}
	return out
}

// subagentParent is a pure function -- test it directly against the path
// shapes test 22 describes.
func TestSubagentParent(t *testing.T) {
	cases := []struct {
		path       string
		wantParent string
		wantSide   bool
	}{
		{filepath.Join("proj1", "sess-parent.jsonl"), "", false},
		{filepath.Join("proj1", "sess-parent", "subagents", "agent-1.jsonl"), "sess-parent", true},
	}
	for _, c := range cases {
		parent, side := subagentParent(c.path)
		if parent != c.wantParent || side != c.wantSide {
			t.Errorf("subagentParent(%q) = (%q, %v), want (%q, %v)", c.path, parent, side, c.wantParent, c.wantSide)
		}
	}
}

// Test 8, end to end: the duplicate-requestId fixture ingests as one row
// with the distinct request's tokens, never the sum of both lines.
func TestPollDuplicateRequestIDFixture(t *testing.T) {
	root := t.TempDir()
	copyFile(t, filepath.Join("testdata", "duplicate_requestid.jsonl"), filepath.Join(root, "log.jsonl"))

	st := newTestStore(t)
	tailer := New(root, st)

	stats, err := tailer.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("RequestsFound = %d, want 1", stats.RequestsFound)
	}
	if stats.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1", stats.Inserted)
	}

	evs := eventsByRequestID(t, st)
	ev, ok := evs["req_dup1"]
	if !ok {
		t.Fatalf("no event for req_dup1: got %v", evs)
	}
	if ev.InputTokens != 100 || ev.OutputTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50 (the distinct request's, not the summed 200/100)", ev.InputTokens, ev.OutputTokens)
	}
}

// Test 9, end to end: the 13 tolerated types are skipped, one unknown
// type and one malformed line are counted (not fatal), and both cache
// shapes ingest with the same prompt total, the flat one costed as
// approximate (cost_source aside, the token total is what must agree).
func TestPollToleranceFixture(t *testing.T) {
	root := t.TempDir()
	copyFile(t, filepath.Join("testdata", "tolerance.jsonl"), filepath.Join(root, "log.jsonl"))

	st := newTestStore(t)
	tailer := New(root, st)

	stats, err := tailer.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stats.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", stats.Unknown)
	}
	if stats.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", stats.Malformed)
	}
	if stats.RequestsFound != 2 {
		t.Errorf("RequestsFound = %d, want 2 (req_split, req_flat)", stats.RequestsFound)
	}
	if stats.Inserted != 2 {
		t.Errorf("Inserted = %d, want 2", stats.Inserted)
	}

	evs := eventsByRequestID(t, st)
	split, ok := evs["req_split"]
	if !ok {
		t.Fatal("no event for req_split")
	}
	flat, ok := evs["req_flat"]
	if !ok {
		t.Fatal("no event for req_flat")
	}
	splitTotal := split.InputTokens + split.CacheWrite5mTokens + split.CacheWrite1hTokens + split.CacheReadTokens
	flatTotal := flat.InputTokens + flat.CacheWrite5mTokens + flat.CacheWrite1hTokens + flat.CacheReadTokens
	if splitTotal != flatTotal {
		t.Errorf("prompt totals differ: split=%d flat=%d, want equal", splitTotal, flatTotal)
	}
}

// Test 22: a top-level transcript is is_sidechain=false; a
// …/<sessionId>/subagents/agent-*.jsonl file is is_sidechain=true and
// attributed to the parent session named in its path, not the (different)
// sessionId its own line carries.
func TestPollSubagentDiscovery(t *testing.T) {
	root := t.TempDir()
	src := os.DirFS(filepath.Join("testdata", "subagents"))
	if err := os.CopyFS(root, src); err != nil {
		t.Fatalf("CopyFS: %v", err)
	}

	st := newTestStore(t)
	tailer := New(root, st)

	stats, err := tailer.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stats.FilesWalked != 2 {
		t.Errorf("FilesWalked = %d, want 2", stats.FilesWalked)
	}
	if stats.Inserted != 2 {
		t.Errorf("Inserted = %d, want 2", stats.Inserted)
	}

	evs := eventsByRequestID(t, st)
	top, ok := evs["req_top"]
	if !ok {
		t.Fatal("no event for req_top")
	}
	if top.IsSidechain {
		t.Error("top-level transcript: IsSidechain = true, want false")
	}
	if top.SessionID != "sess-parent" {
		t.Errorf("top-level SessionID = %q, want sess-parent", top.SessionID)
	}

	sub, ok := evs["req_sub"]
	if !ok {
		t.Fatal("no event for req_sub")
	}
	if !sub.IsSidechain {
		t.Error("subagent file: IsSidechain = false, want true")
	}
	if sub.SessionID != "sess-parent" {
		t.Errorf("subagent SessionID = %q, want sess-parent (the path's owning session, not the line's own sessionId)", sub.SessionID)
	}
}

// Test 10: append then resume never duplicates.
func TestPollResumeAfterAppend(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	writeLine(t, path, `{"type":"assistant","sessionId":"s1","requestId":"req_a","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`)

	st := newTestStore(t)
	tailer := New(root, st)
	ctx := context.Background()

	if _, err := tailer.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	appendLine(t, path, `{"type":"assistant","sessionId":"s1","requestId":"req_b","message":{"model":"m","usage":{"input_tokens":2,"output_tokens":2}}}`)
	stats, err := tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("second Poll RequestsFound = %d, want 1 (only the appended line)", stats.RequestsFound)
	}

	n, err := st.CountEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 2 {
		t.Errorf("event count = %d, want 2 (req_a once, req_b once, no dupes)", n)
	}
}

// Test 10: truncation (or rotation to a smaller file at the same path)
// resets the cursor to 0 rather than erroring, and a re-read of an
// already-seen request_id merges instead of duplicating.
func TestPollResumeAfterTruncate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	line := `{"type":"assistant","sessionId":"s1","requestId":"req_x","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`
	writeLine(t, path, line)

	st := newTestStore(t)
	tailer := New(root, st)
	ctx := context.Background()

	if _, err := tailer.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}

	// Truncate to empty and poll while it's still empty -- this is the
	// moment the cursor must detect "file shrank" and reset to 0, which a
	// same-size rewrite afterward would hide (size == offset looks like
	// "already fully read", not "rotated").
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := tailer.Poll(ctx); err != nil {
		t.Fatalf("Poll (post-truncate, still empty): %v", err)
	}

	// Rewrite the same request -- simulates a rotated file reappearing at
	// the same path with content the tailer has already seen once.
	writeLine(t, path, line)

	stats, err := tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 2 (post-truncate): %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("post-truncate RequestsFound = %d, want 1", stats.RequestsFound)
	}

	n, err := st.CountEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("event count after truncate-and-reread = %d, want 1 (merged, not duplicated)", n)
	}
}

// A line with no trailing newline yet (Claude Code mid-write) is left
// unparsed until the newline arrives -- the boundary-split guarantee.
func TestPollIncompleteLineWaitsForNewline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	full := `{"type":"assistant","sessionId":"s1","requestId":"req_partial","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`
	partial := full[:len(full)/2]

	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	st := newTestStore(t)
	tailer := New(root, st)
	ctx := context.Background()

	stats, err := tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	if stats.RequestsFound != 0 || stats.Malformed != 0 {
		t.Errorf("Poll on a partial line: RequestsFound=%d Malformed=%d, want 0/0 (must wait for the newline)", stats.RequestsFound, stats.Malformed)
	}

	if err := os.WriteFile(path, []byte(full+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile complete: %v", err)
	}
	stats, err = tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("Poll after completing the line: RequestsFound = %d, want 1", stats.RequestsFound)
	}
}

// UTF-8 content in a JSONL field round-trips correctly.
func TestPollUTF8Field(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	writeLine(t, path, `{"type":"assistant","sessionId":"s1","requestId":"req_utf8","cwd":"/home/dev/café-日本語","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`)

	st := newTestStore(t)
	tailer := New(root, st)
	if _, err := tailer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	evs := eventsByRequestID(t, st)
	ev, ok := evs["req_utf8"]
	if !ok {
		t.Fatal("no event for req_utf8")
	}
	if ev.Project != "/home/dev/café-日本語" {
		t.Errorf("Project = %q, want the UTF-8 cwd round-tripped exactly", ev.Project)
	}
}

func writeLine(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func appendLine(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile append %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content + "\n"); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}
