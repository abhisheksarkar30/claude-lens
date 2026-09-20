package store

import (
	"context"
	"strconv"
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
	if _, _, err := st.InsertEvent(ctx, proxy); err != nil {
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
	id, _, err := st.InsertEvent(ctx, jsonl)
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
	id, _, err := st.InsertEvent(ctx, proxy)
	if err != nil {
		t.Fatalf("InsertEvent proxy: %v", err)
	}
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "rate_limited", Severity: "warn"}}); err != nil {
		t.Fatalf("UpsertWarnings proxy: %v", err)
	}

	jsonl := fullEvent("req-warn-1")
	jsonl.Source = "jsonl"
	jsonl.SessionID = sessionID
	id2, _, err := st.InsertEvent(ctx, jsonl)
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

	warnings, err := st.EventWarnings(ctx, id)
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
	id1, _, err := st.InsertEvent(ctx, rateLimited)
	if err != nil {
		t.Fatalf("InsertEvent rateLimited: %v", err)
	}
	if err := st.UpsertWarnings(ctx, id1, []Warning{{Kind: "rate_limited", Severity: "warn"}}); err != nil {
		t.Fatalf("UpsertWarnings rateLimited: %v", err)
	}

	retry := fullEvent("req-retry-2")
	retry.Status = 200
	retry.ReqBody = body
	id2, _, err := st.InsertEvent(ctx, retry)
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

	warnings, err := st.EventWarnings(ctx, id1)
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

	id1, _, err := st.InsertEvent(ctx, first)
	if err != nil {
		t.Fatalf("InsertEvent 1: %v", err)
	}

	second := fullEvent("req-merge-1")
	second.Source = "jsonl"
	second.FirstSource = "jsonl" // must not win — first_source is never rewritten
	second.InputTokens = 20

	id2, _, err := st.InsertEvent(ctx, second)
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

	warnings, err := st.EventWarnings(ctx, id1)
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

	id1, _, err := st.InsertEvent(ctx, truncated)
	if err != nil {
		t.Fatalf("InsertEvent truncated: %v", err)
	}

	complete := fullEvent("req-merge-2")
	complete.CaptureComplete = true
	complete.InputTokens = 500
	complete.OutputTokens = 250

	id2, _, err := st.InsertEvent(ctx, complete)
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

	warnings, err := st.EventWarnings(ctx, id1)
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
	id3, _, err := st.InsertEvent(ctx, fullEvent("req-merge-3"))
	if err != nil {
		t.Fatalf("InsertEvent complete first: %v", err)
	}
	truncatedRetry := fullEvent("req-merge-3")
	truncatedRetry.CaptureComplete = false
	truncatedRetry.InputTokens = 0
	if _, _, err := st.InsertEvent(ctx, truncatedRetry); err != nil {
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
//
// The two sides of the merge carry *different* session ids, which is the
// only shape that tests the rule: a merge never rewrites session_id, so the
// row stays in the first-written session and it is that session -- not the
// incoming event's -- whose totals must be re-derived. Using one id on both
// sides (as this test once did) passes even when the code reconciles the
// wrong session, because the wrong session and the right one are the same
// session.
func TestMergeRederivesSessionTotals(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// owner is the session the row was first written under; incoming is the
	// session the merging event claims.
	const owner, incoming = "s_first_written", "s_incoming"
	if err := st.UpsertSession(ctx, owner, "", fullEvent("seed").StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	other := fullEvent("req-other")
	other.SessionID = owner
	other.InputTokens = 1000
	if _, _, err := st.InsertEvent(ctx, other); err != nil {
		t.Fatalf("InsertEvent other: %v", err)
	}
	if err := st.ReconcileSession(ctx, owner); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	truncated := fullEvent("req-merge-session")
	truncated.SessionID = owner
	truncated.CaptureComplete = false
	truncated.InputTokens = 5
	if _, _, err := st.InsertEvent(ctx, truncated); err != nil {
		t.Fatalf("InsertEvent truncated: %v", err)
	}
	if err := st.ReconcileSession(ctx, owner); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	sessAfterFirst, err := st.GetSession(ctx, owner)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sessAfterFirst.InputTokens != 1005 {
		t.Fatalf("InputTokens before merge = %d, want 1005", sessAfterFirst.InputTokens)
	}

	// The merge itself re-derives the owning session inline (invariant 3),
	// so no explicit ReconcileSession call is needed here -- and the merge
	// names a *different* session, so only a re-derivation keyed to the
	// surviving row can produce the assertion below.
	rewrite := fullEvent("req-merge-session")
	rewrite.SessionID = incoming
	rewrite.CaptureComplete = true
	rewrite.InputTokens = 400
	mergedID, mergedSession, err := st.InsertEvent(ctx, rewrite)
	if err != nil {
		t.Fatalf("InsertEvent rewrite (merge): %v", err)
	}
	if mergedSession != owner {
		t.Errorf("InsertEvent returned session %q for the merged row, want the first-written %q", mergedSession, owner)
	}

	// The row itself kept the first-written session, which is the premise
	// the re-derivation target rests on.
	merged, err := st.GetEvent(ctx, mergedID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if merged.SessionID != owner {
		t.Errorf("merged row session_id = %q, want %q (a merge preserves it)", merged.SessionID, owner)
	}

	sessAfterMerge, err := st.GetSession(ctx, owner)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	// 1000 (other, unchanged) + 400 (the merge's winning tokens), not
	// 1005 + 400 (which would be the old total incremented).
	if sessAfterMerge.InputTokens != 1400 {
		t.Errorf("InputTokens after merge = %d, want 1400 (recomputed, not incremented)", sessAfterMerge.InputTokens)
	}

	// The incoming session owns no rows, so nothing may have created one:
	// a session row here is what the dashboard would render as an agentic
	// run with zero calls.
	if sess, err := st.GetSession(ctx, incoming); err == nil {
		t.Errorf("incoming session %q has a row (request_count=%d); a merge must not fold into it", incoming, sess.RequestCount)
	}
}

// T8: billing_mode moves with the winning cost columns. This is the
// regression for the story's own defect -- before it, preferNonEmpty kept the
// existing mode (always non-empty, so a merge could never change it) while the
// incoming cost landed, producing a row marked subscription carrying a real
// cost_usd. Re-ingesting a DeepSeek call is exactly that shape: the tailer
// routes deepseek-* to the api account, so the incoming side is priced in
// cost_usd.
func TestMergeMovesBillingModeWithCost(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sub := fullEvent("req-merge-billing")
	sub.Source = "proxy"
	sub.ModelResolved = "deepseek-flash"
	sub.BillingMode = "subscription"
	sub.CostUSD = nil
	sub.ApiEquivalentCostUSD = f64(0.42)
	sub.CostSource = "shipped"
	id, _, err := st.InsertEvent(ctx, sub)
	if err != nil {
		t.Fatalf("InsertEvent subscription: %v", err)
	}

	api := fullEvent("req-merge-billing")
	api.Source = "jsonl"
	api.ModelResolved = "deepseek-flash"
	api.BillingMode = "api"
	api.CostUSD = f64(0.42)
	api.ApiEquivalentCostUSD = nil
	api.CostSource = "shipped"
	id2, _, err := st.InsertEvent(ctx, api)
	if err != nil {
		t.Fatalf("InsertEvent api (merge): %v", err)
	}
	if id2 != id {
		t.Fatalf("merge produced a new row: %d vs %d", id, id2)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.BillingMode != "api" {
		t.Errorf("BillingMode = %q, want api (it must move with the cost columns, not stay preferNonEmpty)", got.BillingMode)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.42 {
		t.Errorf("CostUSD = %v, want 0.42 (the winning capture's)", got.CostUSD)
	}
	if got.ApiEquivalentCostUSD != nil {
		t.Errorf("ApiEquivalentCostUSD = %v, want nil (invariant 5: the two are never both set)", *got.ApiEquivalentCostUSD)
	}
}

// T9c: the winner can hand over no mode at all. billingModeForAuthKind returns
// "" for a credential authkind.go does not classify, so a capture-complete proxy
// row can carry an empty mode -- and every cost aggregate matches on
// billing_mode = 'api' / 'subscription', so a blanked mode drops the row out of
// every total. The label follows the money instead: the winner's populated cost
// column names it. The session totals are asserted, not just the row, because
// invisibility in the totals is the harm and that is where it shows.
func TestMergeDerivesBillingModeFromWinningCostColumn(t *testing.T) {
	cases := []struct {
		name            string
		storedMode      string
		storedCost      *float64
		storedEq        *float64
		winCost         *float64
		winEq           *float64
		winSource       string
		wantMode        string
		wantCost        *float64
		wantEq          *float64
		wantSessionCost *float64
		wantSessionEq   *float64
	}{
		{
			name:            "winner priced cost_usd: the cost names it api, not the stored subscription",
			storedMode:      "subscription",
			storedEq:        f64(0.42),
			winCost:         f64(0.42),
			winSource:       "shipped",
			wantMode:        "api",
			wantCost:        f64(0.42),
			wantSessionCost: f64(0.42),
		},
		{
			name:          "winner priced api_equivalent_cost_usd: subscription",
			storedMode:    "api",
			storedCost:    f64(0.42),
			winEq:         f64(0.30),
			winSource:     "shipped",
			wantMode:      "subscription",
			wantEq:        f64(0.30),
			wantSessionEq: f64(0.30),
		},
		{
			name:       "winner priced nothing: the stored mode survives rather than blanking",
			storedMode: "subscription",
			storedEq:   f64(0.42),
			winSource:  "unpriced",
			wantMode:   "subscription",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()

			stored := fullEvent("req-merge-billing-empty")
			stored.BillingMode = tc.storedMode
			stored.CostUSD = tc.storedCost
			stored.ApiEquivalentCostUSD = tc.storedEq
			if err := st.UpsertSession(ctx, stored.SessionID, "", stored.StartedAt); err != nil {
				t.Fatalf("UpsertSession: %v", err)
			}
			id, _, err := st.InsertEvent(ctx, stored)
			if err != nil {
				t.Fatalf("InsertEvent stored: %v", err)
			}

			// CaptureComplete, proxy-sourced, with an empty BillingMode and an
			// unclassified credential -- exactly the combination
			// billingModeForAuthKind produces "", and the ordering that made
			// winner = incoming and blanked the stored mode. (A JSONL row
			// cannot be the source of an empty mode: the tailer always
			// resolves one, by account default or by model prefix.)
			winner := fullEvent("req-merge-billing-empty")
			winner.Source = "proxy"
			winner.AuthKind = "unknown"
			winner.BillingMode = ""
			winner.CostUSD = tc.winCost
			winner.ApiEquivalentCostUSD = tc.winEq
			winner.CostSource = tc.winSource
			id2, _, err := st.InsertEvent(ctx, winner)
			if err != nil {
				t.Fatalf("InsertEvent winner: %v", err)
			}
			if id2 != id {
				t.Fatalf("merge produced a new row: %d vs %d", id, id2)
			}

			got, err := st.GetEvent(ctx, id)
			if err != nil {
				t.Fatalf("GetEvent: %v", err)
			}
			if got.BillingMode != tc.wantMode {
				t.Errorf("BillingMode = %q, want %q", got.BillingMode, tc.wantMode)
			}
			if !sameF64(got.CostUSD, tc.wantCost) {
				t.Errorf("CostUSD = %v, want %v", ptrStr(got.CostUSD), ptrStr(tc.wantCost))
			}
			if !sameF64(got.ApiEquivalentCostUSD, tc.wantEq) {
				t.Errorf("ApiEquivalentCostUSD = %v, want %v", ptrStr(got.ApiEquivalentCostUSD), ptrStr(tc.wantEq))
			}
			if got.CostUSD != nil && got.ApiEquivalentCostUSD != nil {
				t.Errorf("both cost columns set (invariant 5: the two are never both set)")
			}

			sess, err := st.GetSession(ctx, "s_1")
			if err != nil {
				t.Fatalf("GetSession: %v", err)
			}
			if !sameF64(sess.TotalCostUSD, tc.wantSessionCost) {
				t.Errorf("session TotalCostUSD = %v, want %v", ptrStr(sess.TotalCostUSD), ptrStr(tc.wantSessionCost))
			}
			if !sameF64(sess.TotalApiEquivalentCostUSD, tc.wantSessionEq) {
				t.Errorf("session TotalApiEquivalentCostUSD = %v, want %v", ptrStr(sess.TotalApiEquivalentCostUSD), ptrStr(tc.wantSessionEq))
			}
		})
	}
}

func sameF64(got, want *float64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func ptrStr(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

// T9: an incomplete incoming capture wins nothing, so billing_mode is
// unchanged and its cost does not land either. 0-vs-N is not a disagreement.
func TestMergeIncompleteIncomingKeepsBillingMode(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	existing := fullEvent("req-merge-incomplete")
	existing.Source = "proxy"
	existing.BillingMode = "subscription"
	existing.CostUSD = nil
	existing.ApiEquivalentCostUSD = f64(0.10)
	existing.CostSource = "shipped"
	id, _, err := st.InsertEvent(ctx, existing)
	if err != nil {
		t.Fatalf("InsertEvent existing: %v", err)
	}

	incoming := fullEvent("req-merge-incomplete")
	incoming.Source = "jsonl"
	incoming.CaptureComplete = false
	incoming.BillingMode = "api"
	incoming.CostUSD = f64(0.10)
	incoming.ApiEquivalentCostUSD = nil
	incoming.CostSource = "shipped"
	id2, _, err := st.InsertEvent(ctx, incoming)
	if err != nil {
		t.Fatalf("InsertEvent incomplete (merge): %v", err)
	}
	if id2 != id {
		t.Fatalf("merge produced a new row")
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.BillingMode != "subscription" {
		t.Errorf("BillingMode = %q, want subscription (an incomplete incoming capture wins nothing)", got.BillingMode)
	}
	if got.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil (the incomplete side's cost must not land)", got.CostUSD)
	}
}

// T9b: Account and AuthKind stay on preferNonEmpty even when the incoming side
// demonstrably wins the cost columns. The proxy-only columns are backfilled in
// both directions, but these two are not -- the JSONL line carries no auth
// signal, so a winner-based Account would blank the live proxy row's name.
func TestMergeKeepsExistingAccountAndAuthKind(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	existing := fullEvent("req-merge-account")
	existing.Source = "proxy"
	existing.Account = "work"
	existing.AuthKind = "oauth"
	existing.BillingMode = "api"
	existing.CostUSD = f64(0.10)
	id, _, err := st.InsertEvent(ctx, existing)
	if err != nil {
		t.Fatalf("InsertEvent existing: %v", err)
	}

	incoming := fullEvent("req-merge-account")
	incoming.Source = "jsonl"
	incoming.Account = ""         // structurally absent from a JSONL line
	incoming.AuthKind = "api_key" // deliberately different, so a winner-based assignment is visible
	incoming.BillingMode = "api"
	incoming.CostUSD = f64(0.20)
	id2, _, err := st.InsertEvent(ctx, incoming)
	if err != nil {
		t.Fatalf("InsertEvent incoming (merge): %v", err)
	}
	if id2 != id {
		t.Fatalf("merge produced a new row")
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	// The premise: the incoming side did win the cost columns. Without this,
	// the assertions below would pass for the wrong reason.
	if got.CostUSD == nil || *got.CostUSD != 0.20 {
		t.Fatalf("CostUSD = %v, want 0.20 (the incoming capture won)", got.CostUSD)
	}
	if got.Account != "work" {
		t.Errorf("Account = %q, want work (preferNonEmpty keeps the proxy row's name)", got.Account)
	}
	if got.AuthKind != "oauth" {
		t.Errorf("AuthKind = %q, want oauth (preferNonEmpty, even though the incoming side won the costs)", got.AuthKind)
	}
}

// TestMergeFillsTranscriptColumnsBothWays (T8) covers the ordering the design
// as first written dropped: the proxy captures live and the transcript
// reconstruction lands minutes later, so proxy-first is the common case and a
// new column with no merge rule is discarded exactly when it finally shows up.
// Both orderings must leave the row carrying the reconstruction, because the
// proxy side structurally cannot supply it -- a capture has no `message.content`
// to reconstruct from.
func TestMergeFillsTranscriptColumnsBothWays(t *testing.T) {
	const content = `[{"type":"text","text":"the transcript's own content"}]`

	// proxyFirst writes the capture, then the reconstruction.
	proxyFirst := func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()

		proxy := fullEvent("req-t8-proxy-first")
		proxy.Source, proxy.FirstSource = "proxy", "proxy"
		if _, _, err := st.InsertEvent(ctx, proxy); err != nil {
			t.Fatalf("InsertEvent proxy: %v", err)
		}

		jsonl := fullEvent("req-t8-proxy-first")
		jsonl.Source, jsonl.FirstSource = "jsonl", "jsonl"
		jsonl.TranscriptContent = []byte(content)
		jsonl.TranscriptRole = "assistant"
		id, _, err := st.InsertEvent(ctx, jsonl)
		if err != nil {
			t.Fatalf("InsertEvent jsonl (merge): %v", err)
		}

		got, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if string(got.TranscriptContent) != content {
			t.Errorf("TranscriptContent = %q, want the reconstruction to survive a proxy-first merge", got.TranscriptContent)
		}
		if got.TranscriptRole != "assistant" {
			t.Errorf("TranscriptRole = %q, want assistant", got.TranscriptRole)
		}
		// The capture's own columns are untouched by the merge: the
		// reconstruction goes in its own columns by design.
		if string(got.ReqBody) != string(proxy.ReqBody) {
			t.Errorf("ReqBody = %q, want the capture's own %q", got.ReqBody, proxy.ReqBody)
		}
	}

	// jsonlFirst writes the reconstruction, then the capture arrives.
	jsonlFirst := func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()

		jsonl := fullEvent("req-t8-jsonl-first")
		jsonl.Source, jsonl.FirstSource = "jsonl", "jsonl"
		jsonl.TranscriptContent = []byte(content)
		jsonl.TranscriptRole = "assistant"
		if _, _, err := st.InsertEvent(ctx, jsonl); err != nil {
			t.Fatalf("InsertEvent jsonl: %v", err)
		}

		proxy := fullEvent("req-t8-jsonl-first")
		proxy.Source, proxy.FirstSource = "proxy", "proxy"
		id, _, err := st.InsertEvent(ctx, proxy)
		if err != nil {
			t.Fatalf("InsertEvent proxy (merge): %v", err)
		}

		got, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if string(got.TranscriptContent) != content {
			t.Errorf("TranscriptContent = %q, want the reconstruction to survive a jsonl-first merge", got.TranscriptContent)
		}
		if got.TranscriptRole != "assistant" {
			t.Errorf("TranscriptRole = %q, want assistant", got.TranscriptRole)
		}
		if string(got.ReqBody) != string(proxy.ReqBody) {
			t.Errorf("ReqBody = %q, want the capture's own %q", got.ReqBody, proxy.ReqBody)
		}
	}

	t.Run("proxy first", proxyFirst)
	t.Run("jsonl first", jsonlFirst)

	// The both-sides-populated case: a second jsonl row for the same request
	// (a re-read after a truncated cursor, say) must not overwrite the content
	// already stored -- first-written wins, the merge's general rule.
	t.Run("existing reconstruction is not overwritten", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()

		first := fullEvent("req-t8-keep")
		first.Source, first.FirstSource = "jsonl", "jsonl"
		first.TranscriptContent = []byte(content)
		first.TranscriptRole = "assistant"
		if _, _, err := st.InsertEvent(ctx, first); err != nil {
			t.Fatalf("InsertEvent first: %v", err)
		}

		second := fullEvent("req-t8-keep")
		second.Source, second.FirstSource = "jsonl", "jsonl"
		second.TranscriptContent = []byte(`[{"type":"text","text":"a later, different read"}]`)
		second.TranscriptRole = "assistant"
		id, _, err := st.InsertEvent(ctx, second)
		if err != nil {
			t.Fatalf("InsertEvent second: %v", err)
		}

		got, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if string(got.TranscriptContent) != content {
			t.Errorf("TranscriptContent = %q, want the first-written %q", got.TranscriptContent, content)
		}
	})
}

// TestMergePrefersTheWhollyCapturedRowOverATruncatedRequest (br-GI-7-08) pins
// the merge consequence of widening CaptureComplete to cover both bodies.
//
// Before that bead the flag was false only for a truncated *response*, so a
// proxy row with a cut request body counted as complete and -- being the row
// written second -- took the token pick on the `winner = incoming` branch. It
// now counts as incomplete, and the wholly-captured jsonl row already on the
// row takes the pick instead. That is deliberate (with one body known to be a
// prefix, the record that is whole is the safer one to quote) but it is a
// behaviour change, so it is asserted here rather than left to be discovered.
//
// The write order is load-bearing and is why the proxy row is second: with the
// jsonl row second both orderings pick it, and the test would pass whether the
// flag changed or not.
func TestMergePrefersTheWhollyCapturedRowOverATruncatedRequest(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// The transcript's own record of the request: whole.
	jsonl := fullEvent("req-gi7-merge-trunc")
	jsonl.Source, jsonl.FirstSource = "jsonl", "jsonl"
	jsonl.CaptureComplete = true
	jsonl.InputTokens, jsonl.OutputTokens = 111, 55
	if _, _, err := st.InsertEvent(ctx, jsonl); err != nil {
		t.Fatalf("InsertEvent jsonl: %v", err)
	}

	// The proxy capture, arriving after: request body cut at the read cap, so
	// br-GI-7-08 marks the capture incomplete.
	proxy := fullEvent("req-gi7-merge-trunc")
	proxy.Source, proxy.FirstSource = "proxy", "proxy"
	proxy.CaptureComplete = false
	proxy.InputTokens, proxy.OutputTokens = 100, 50
	id, _, err := st.InsertEvent(ctx, proxy)
	if err != nil {
		t.Fatalf("InsertEvent proxy (merge): %v", err)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.InputTokens != 111 || got.OutputTokens != 55 {
		t.Errorf("tokens = %d/%d, want the wholly-captured row's 111/55: a request-truncated "+
			"capture must not win the pick", got.InputTokens, got.OutputTokens)
	}
	if !got.CaptureComplete {
		t.Error("merged CaptureComplete = false, want true: one side captured the request whole")
	}
}

// TestMergeDoesNotLetABodylessRowZeroObservedUsage is the hazard br-GI-7-09
// introduced and its review caught, pinned in both orderings.
//
// Under --body-policy off the proxy writes a row with no bodies, and every
// token column is zero because usage is parsed out of the response body it
// never kept. That row also reports CaptureComplete *true* -- nothing was
// narrowed -- so the flag-based pick hands it the win whenever it arrives
// second, and the zero it carries is not a measurement. The merged row would
// lose the transcript's real counts and gain a spurious source_mismatch.
//
// Both orderings are here because they reach different branches: proxy-first
// takes the `!existing && incoming` case, jsonl-first takes the `&&` case, and
// the second is the one that lost the data.
func TestMergeDoesNotLetABodylessRowZeroObservedUsage(t *testing.T) {
	for _, first := range []string{"jsonl", "proxy"} {
		t.Run(first+" first", func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()

			jsonl := fullEvent("req-gi7-off-merge")
			jsonl.Source, jsonl.FirstSource = "jsonl", "jsonl"
			jsonl.CaptureComplete = true
			jsonl.InputTokens, jsonl.OutputTokens = 111, 55

			// The off-policy row: complete, and empty because nothing was kept.
			// All six token columns, not just the two the assertion reads --
			// fullEvent populates the cache columns, and leaving them set made
			// this fixture a row that *had* been measured, which is the
			// opposite of the shape under test.
			empty := fullEvent("req-gi7-off-merge")
			empty.Source, empty.FirstSource = "proxy", "proxy"
			empty.CaptureComplete = true
			empty.InputTokens, empty.OutputTokens = 0, 0
			empty.CacheWrite5mTokens, empty.CacheWrite1hTokens = 0, 0
			empty.CacheReadTokens, empty.ThinkingTokens = 0, 0
			empty.ReqBody, empty.RespBody = nil, nil

			rows := []*Event{jsonl, empty}
			if first == "proxy" {
				rows[0], rows[1] = rows[1], rows[0]
			}
			if _, _, err := st.InsertEvent(ctx, rows[0]); err != nil {
				t.Fatalf("InsertEvent first: %v", err)
			}
			id, _, err := st.InsertEvent(ctx, rows[1])
			if err != nil {
				t.Fatalf("InsertEvent second (merge): %v", err)
			}

			got, err := st.GetEvent(ctx, id)
			if err != nil {
				t.Fatalf("GetEvent: %v", err)
			}
			if got.InputTokens != 111 || got.OutputTokens != 55 {
				t.Errorf("tokens = %d/%d, want the observed 111/55: a row with no bodies has no "+
					"measurement to contribute, and its zeros are 'never looked', not 'none'",
					got.InputTokens, got.OutputTokens)
			}

			// The other half of the same defect, and the half that fired in the
			// *ordinary* proxy-first ordering rather than a race. mergeEvents's
			// own contract says "a 0-vs-N difference is not a disagreement", but
			// the completeness case set mismatch from tokensDiffer alone, so an
			// absent side counted as a conflicting one and every off-policy
			// merge grew a source_mismatch at SeverityError -- an error warning
			// about a disagreement that never happened, on the row shape `off`
			// produces for every call it records.
			warnings, err := st.ListWarnings(ctx, WarningFilter{})
			if err != nil {
				t.Fatalf("ListWarnings: %v", err)
			}
			for _, w := range warnings {
				if w.Kind == "source_mismatch" {
					t.Errorf("a bodyless row raised %s (%s): no measurement was contradicted -- "+
						"one side never looked", w.Kind, w.Detail)
				}
			}
		})
	}
}

// TestMergeStillWarnsOnATrueDisagreement is the negative half of the gate above,
// which is the half a "stop warning" fix gets wrong: if the mismatch condition
// is narrowed too far, a real disagreement goes unreported and nothing fails.
//
// Both rows are complete *and* both were measured, differing on the numbers --
// the case source_mismatch exists for.
func TestMergeStillWarnsOnATrueDisagreement(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := fullEvent("req-gi7-real-mismatch")
	first.Source, first.FirstSource = "proxy", "proxy"
	first.CaptureComplete = true
	first.InputTokens, first.OutputTokens = 100, 50

	second := fullEvent("req-gi7-real-mismatch")
	second.Source, second.FirstSource = "jsonl", "jsonl"
	second.CaptureComplete = true
	second.InputTokens, second.OutputTokens = 111, 55

	if _, _, err := st.InsertEvent(ctx, first); err != nil {
		t.Fatalf("InsertEvent first: %v", err)
	}
	if _, _, err := st.InsertEvent(ctx, second); err != nil {
		t.Fatalf("InsertEvent second (merge): %v", err)
	}

	// No EventID filter exists on WarningFilter, and none is needed: this store
	// holds one merged event, so every warning it returns belongs to it.
	warnings, err := st.ListWarnings(ctx, WarningFilter{})
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Kind == "source_mismatch" {
			found = true
		}
	}
	if !found {
		t.Errorf("no source_mismatch for two complete captures that disagree on tokens; "+
			"got %d warnings", len(warnings))
	}
}
