package store

import (
	"context"
	"testing"
)

// Test 11a (B-owned metadata columns survive the merge): the columns a
// proxy row structurally cannot supply -- client_version, project,
// git_branch, cli_entrypoint, is_sidechain -- come from whichever source
// (here, jsonl) actually carried them, so a JSONL-sourced merge is not a
// no-op participant.
func TestMergePreservesJSONLOnlyColumns(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	proxy := fullEvent("req-cols-1")
	proxy.Source = "proxy"
	proxy.FirstSource = "proxy"
	if _, err := st.InsertEvent(ctx, proxy); err != nil {
		t.Fatalf("InsertEvent proxy: %v", err)
	}

	jsonl := fullEvent("req-cols-1")
	jsonl.Source = "jsonl"
	jsonl.FirstSource = "jsonl"
	jsonl.ClientVersion = "2.1.272"
	jsonl.Project = "/home/x/repo"
	jsonl.GitBranch = "main"
	jsonl.CliEntrypoint = "cli"
	jsonl.IsSidechain = true
	id, err := st.InsertEvent(ctx, jsonl)
	if err != nil {
		t.Fatalf("InsertEvent jsonl (merge): %v", err)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.ClientVersion != "2.1.272" {
		t.Errorf("ClientVersion = %q, want 2.1.272", got.ClientVersion)
	}
	if got.Project != "/home/x/repo" {
		t.Errorf("Project = %q, want /home/x/repo", got.Project)
	}
	if got.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want main", got.GitBranch)
	}
	if got.CliEntrypoint != "cli" {
		t.Errorf("CliEntrypoint = %q, want cli", got.CliEntrypoint)
	}
	if !got.IsSidechain {
		t.Error("IsSidechain = false, want true (jsonl's value)")
	}
	// The proxy-only columns must still survive too -- a merge backfills
	// in both directions.
	if got.Method != proxy.Method || got.Path != proxy.Path {
		t.Errorf("proxy-only columns lost after merge: Method=%q Path=%q", got.Method, got.Path)
	}
}

// Test 11a (warning ledger equality): the merge re-runs the analyzer
// seam, and each finding kind lands exactly once on the row -- a second
// attach of a kind already present upserts rather than duplicates, and
// warning_count equals the distinct-kind count, not the sum of two
// analyzer passes.
func TestMergeWarningLedgerDedup(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sessionID := "s_warn_dedup"
	proxy := fullEvent("req-warn-1")
	proxy.Source = "proxy"
	proxy.SessionID = sessionID
	id, err := st.InsertEvent(ctx, proxy)
	if err != nil {
		t.Fatalf("InsertEvent proxy: %v", err)
	}
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "rate_limited", Severity: "warn"}}); err != nil {
		t.Fatalf("UpsertWarnings proxy: %v", err)
	}

	jsonl := fullEvent("req-warn-1")
	jsonl.Source = "jsonl"
	jsonl.SessionID = sessionID
	id2, err := st.InsertEvent(ctx, jsonl)
	if err != nil {
		t.Fatalf("InsertEvent jsonl (merge): %v", err)
	}
	if id2 != id {
		t.Fatalf("merge produced a new row")
	}
	// jsonl's own analyzer pass raises a different kind after the merge.
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "cache_write_never_read", Severity: "warn"}}); err != nil {
		t.Fatalf("UpsertWarnings jsonl: %v", err)
	}
	// A re-run of the same rule that already fired must upsert, not
	// duplicate -- a buggy v4 that merely counted rows would still pass
	// without this second attach.
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "rate_limited", Severity: "warn", Detail: "re-run"}}); err != nil {
		t.Fatalf("UpsertWarnings re-run: %v", err)
	}

	warnings, err := st.ListWarnings(ctx, id)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings on row = %d, want 2 (COUNT(*) == COUNT(DISTINCT kind))", len(warnings))
	}
	kinds := map[string]bool{}
	for _, w := range warnings {
		kinds[w.Kind] = true
	}
	if !kinds["rate_limited"] || !kinds["cache_write_never_read"] {
		t.Errorf("unexpected kinds: %v", kinds)
	}

	if err := st.UpsertSession(ctx, sessionID, "", proxy.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := st.ReconcileSession(ctx, sessionID); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	sess, err := st.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.WarningCount != 2 {
		t.Errorf("session WarningCount = %d, want 2 (the distinct-kind count, not the sum of two analyzer runs)", sess.WarningCount)
	}
}

// Test 21: two proxy calls with byte-identical bodies but distinct
// response request-ids (a 429 then a 200 retry) produce two rows and two
// sets of findings, never merged into one -- collapsing them would
// destroy the rate_limited/overloaded signal.
func TestRetryPreservesTwoRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`)

	rateLimited := fullEvent("req-retry-1")
	rateLimited.Status = 429
	rateLimited.ReqBody = body
	id1, err := st.InsertEvent(ctx, rateLimited)
	if err != nil {
		t.Fatalf("InsertEvent rateLimited: %v", err)
	}
	if err := st.UpsertWarnings(ctx, id1, []Warning{{Kind: "rate_limited", Severity: "warn"}}); err != nil {
		t.Fatalf("UpsertWarnings rateLimited: %v", err)
	}

	retry := fullEvent("req-retry-2")
	retry.Status = 200
	retry.ReqBody = body
	id2, err := st.InsertEvent(ctx, retry)
	if err != nil {
		t.Fatalf("InsertEvent retry: %v", err)
	}

	if id1 == id2 {
		t.Fatalf("two distinct request_ids collapsed into one row")
	}
	n, err := st.CountEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 2 {
		t.Fatalf("event count = %d, want 2", n)
	}

	warnings, err := st.ListWarnings(ctx, id1)
	if err != nil {
		t.Fatalf("ListWarnings id1: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Kind != "rate_limited" {
		t.Errorf("first row's findings = %v, want just rate_limited (not merged away)", warnings)
	}
}

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
