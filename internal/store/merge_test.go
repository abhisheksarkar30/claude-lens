package store

import (
	"context"
	"testing"
)

// Merge: two inserts with the same request_id collapse into one row,
// source_refs lists both, first_source is preserved, and tokens are the
// complete capture's.
func TestMergeCollidingRequestID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := fullEvent("req-merge-1")
	first.Source = "proxy"
	first.FirstSource = "proxy"
	first.InputTokens = 10

	id1, err := st.InsertEvent(ctx, first)
	if err != nil {
		t.Fatalf("InsertEvent 1: %v", err)
	}

	second := fullEvent("req-merge-1")
	second.Source = "jsonl"
	second.FirstSource = "jsonl" // must not win — first_source is never rewritten
	second.InputTokens = 20

	id2, err := st.InsertEvent(ctx, second)
	if err != nil {
		t.Fatalf("InsertEvent 2 (merge): %v", err)
	}
	if id1 != id2 {
		t.Fatalf("merge produced a new row: id1=%d id2=%d, want equal", id1, id2)
	}

	n, err := st.CountEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("event count after merge = %d, want 1", n)
	}

	got, err := st.GetEvent(ctx, id1)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.FirstSource != "proxy" {
		t.Errorf("FirstSource = %q, want proxy (never rewritten by a merge)", got.FirstSource)
	}
	wantRefs := map[string]bool{"proxy": true, "jsonl": true}
	if len(got.SourceRefs) != 2 {
		t.Fatalf("SourceRefs = %v, want 2 entries", got.SourceRefs)
	}
	for _, r := range got.SourceRefs {
		if !wantRefs[r] {
			t.Errorf("unexpected SourceRefs entry %q", r)
		}
	}
	// Both sides are capture_complete, so the second (incoming) capture's
	// tokens win, and the differing InputTokens is a real disagreement.
	if got.InputTokens != 20 {
		t.Errorf("InputTokens = %d, want 20 (the winning capture's)", got.InputTokens)
	}

	warnings, err := st.ListWarnings(ctx, id1)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	foundMismatch := false
	for _, w := range warnings {
		if w.Kind == "source_mismatch" {
			foundMismatch = true
		}
	}
	if !foundMismatch {
		t.Error("two complete sources disagreeing on tokens did not raise source_mismatch")
	}
}

// Merge precedence: a truncated capture merged against a complete one
// never wins, and is not treated as a disagreement (0-vs-N is not a
// mismatch).
func TestMergePrecedenceTruncatedVsComplete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	truncated := fullEvent("req-merge-2")
	truncated.CaptureComplete = false
	truncated.InputTokens = 0
	truncated.OutputTokens = 0

	id1, err := st.InsertEvent(ctx, truncated)
	if err != nil {
		t.Fatalf("InsertEvent truncated: %v", err)
	}

	complete := fullEvent("req-merge-2")
	complete.CaptureComplete = true
	complete.InputTokens = 500
	complete.OutputTokens = 250

	id2, err := st.InsertEvent(ctx, complete)
	if err != nil {
		t.Fatalf("InsertEvent complete (merge): %v", err)
	}
	if id1 != id2 {
		t.Fatalf("merge produced a new row")
	}

	got, err := st.GetEvent(ctx, id1)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.InputTokens != 500 || got.OutputTokens != 250 {
		t.Errorf("tokens = %+v, want the complete capture's (500/250)", got)
	}
	if !got.CaptureComplete {
		t.Error("CaptureComplete = false after merging in a complete capture, want true")
	}

	warnings, err := st.ListWarnings(ctx, id1)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	for _, w := range warnings {
		if w.Kind == "source_mismatch" {
			t.Error("truncated-vs-complete merge raised source_mismatch, want none (0-vs-N is not a disagreement)")
		}
	}

	// Same scenario in the opposite insertion order: complete first, then
	// a truncated retry — the complete capture must still win.
	id3, err := st.InsertEvent(ctx, fullEvent("req-merge-3"))
	if err != nil {
		t.Fatalf("InsertEvent complete first: %v", err)
	}
	truncatedRetry := fullEvent("req-merge-3")
	truncatedRetry.CaptureComplete = false
	truncatedRetry.InputTokens = 0
	if _, err := st.InsertEvent(ctx, truncatedRetry); err != nil {
		t.Fatalf("InsertEvent truncated retry (merge): %v", err)
	}
	got3, err := st.GetEvent(ctx, id3)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got3.InputTokens != fullEvent("req-merge-3").InputTokens {
		t.Errorf("InputTokens = %d, want the earlier complete capture's %d", got3.InputTokens, fullEvent("req-merge-3").InputTokens)
	}
}

// Session re-derivation: a merge that rewrites a row's tokens leaves the
// session totals equal to the recomputed sum over events, not an
// increment of the pre-merge totals.
func TestMergeRederivesSessionTotals(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sessionID := "s_rederive"
	if err := st.UpsertSession(ctx, sessionID, "", fullEvent("seed").StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	other := fullEvent("req-other")
	other.SessionID = sessionID
	other.InputTokens = 1000
	if _, err := st.InsertEvent(ctx, other); err != nil {
		t.Fatalf("InsertEvent other: %v", err)
	}
	if err := st.ReconcileSession(ctx, sessionID); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	truncated := fullEvent("req-merge-session")
	truncated.SessionID = sessionID
	truncated.CaptureComplete = false
	truncated.InputTokens = 5
	if _, err := st.InsertEvent(ctx, truncated); err != nil {
		t.Fatalf("InsertEvent truncated: %v", err)
	}
	if err := st.ReconcileSession(ctx, sessionID); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	sessAfterFirst, err := st.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sessAfterFirst.InputTokens != 1005 {
		t.Fatalf("InputTokens before merge = %d, want 1005", sessAfterFirst.InputTokens)
	}

	// The merge itself re-derives the session inline (invariant 3), so no
	// explicit ReconcileSession call is needed here.
	rewrite := fullEvent("req-merge-session")
	rewrite.SessionID = sessionID
	rewrite.CaptureComplete = true
	rewrite.InputTokens = 400
	if _, err := st.InsertEvent(ctx, rewrite); err != nil {
		t.Fatalf("InsertEvent rewrite (merge): %v", err)
	}

	sessAfterMerge, err := st.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// 1000 (other, unchanged) + 400 (the merge's winning tokens), not
	// 1005 + 400 (which would be the old total incremented).
	if sessAfterMerge.InputTokens != 1400 {
		t.Errorf("InputTokens after merge = %d, want 1400 (recomputed, not incremented)", sessAfterMerge.InputTokens)
	}
}
