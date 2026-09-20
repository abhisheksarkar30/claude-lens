package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
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

func hashMeta(hash string) parse.Meta {
	return parse.Meta{PrefixHash: &hash}
}

func headerMeta(header string) parse.Meta {
	return parse.Meta{SessionHeader: header}
}

// Test 6: session keys.
func TestResolveGroupsWithinGapWindow(t *testing.T) {
	r := New(newTestStore(t), 30)
	base := time.Unix(1700000000, 0)

	id1 := r.Resolve(hashMeta("abc"), base)
	id2 := r.Resolve(hashMeta("abc"), base.Add(10*time.Minute))
	if id1 != id2 {
		t.Errorf("same prefix within the gap window: id1=%q id2=%q, want equal", id1, id2)
	}
}

func TestResolveSplitsBeyondGapWindow(t *testing.T) {
	r := New(newTestStore(t), 30)
	base := time.Unix(1700000000, 0)

	id1 := r.Resolve(hashMeta("abc"), base)
	id2 := r.Resolve(hashMeta("abc"), base.Add(31*time.Minute))
	if id1 == id2 {
		t.Errorf("same prefix beyond the gap window: id1=%q id2=%q, want different", id1, id2)
	}
}

func TestResolveHeaderOverridesPrefix(t *testing.T) {
	r := New(newTestStore(t), 30)
	base := time.Unix(1700000000, 0)

	// Two calls with the same prefix hash but different explicit headers
	// must not group even within the gap window -- the header wins.
	id1 := r.Resolve(parse.Meta{PrefixHash: strPtr("abc"), SessionHeader: "s1"}, base)
	id2 := r.Resolve(parse.Meta{PrefixHash: strPtr("abc"), SessionHeader: "s2"}, base.Add(time.Minute))
	if id1 == id2 {
		t.Error("different session headers with the same prefix grouped, want different sessions")
	}

	// Same header, same prefix, within window: groups.
	id3 := r.Resolve(parse.Meta{PrefixHash: strPtr("abc"), SessionHeader: "s1"}, base.Add(2*time.Minute))
	if id1 != id3 {
		t.Error("same session header did not group")
	}
}

func TestResolveHeaderOnOneCallDoesNotGroup(t *testing.T) {
	r := New(newTestStore(t), 30)
	base := time.Unix(1700000000, 0)

	withHeader := r.Resolve(parse.Meta{PrefixHash: strPtr("abc"), SessionHeader: "s1"}, base)
	withoutHeader := r.Resolve(hashMeta("abc"), base.Add(time.Minute))
	if withHeader == withoutHeader {
		t.Error("header-on-one-call/absent-on-the-other grouped, want different sessions")
	}
}

// RecordCall folds a row into its session's totals; a NULL session id is
// a no-op.
func TestRecordCallFoldsIntoSessionTotals(t *testing.T) {
	st := newTestStore(t)
	r := New(st, 30)
	ctx := context.Background()

	sessionID := r.Resolve(hashMeta("abc"), time.Unix(1700000000, 0))
	ev := &store.Event{EventSummary: store.EventSummary{
		RequestID:   "req-1",
		Source:      "proxy",
		FirstSource: "proxy",
		StartedAt:   time.Unix(1700000000, 0),
		SessionID:   sessionID,
		InputTokens: 42,
		PrefixHash:  strPtr("abc")}}
	if _, _, err := st.InsertEvent(ctx, ev); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := r.RecordCall(ctx, sessionID, ev, 0); err != nil {
		t.Fatalf("RecordCall: %v", err)
	}

	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.InputTokens != 42 {
		t.Errorf("session InputTokens = %d, want 42", sess.InputTokens)
	}
	if sess.PrefixHash != "abc" {
		t.Errorf("session PrefixHash = %q, want abc", sess.PrefixHash)
	}

	if err := r.RecordCall(ctx, "", ev, 0); err != nil {
		t.Errorf("RecordCall with empty session id = %v, want nil (no-op)", err)
	}
}

func strPtr(s string) *string { return &s }
