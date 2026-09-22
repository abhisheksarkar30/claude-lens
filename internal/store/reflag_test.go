package store

import (
	"context"
	"fmt"
	"testing"
)

// reflagRow is a proxy row whose request body may or may not be a strict prefix
// of the Content-Length it was captured against -- the one witness a merge
// cannot destroy.
//
// contentLength is written into the redacted header JSON the way the proxy
// writes it (encoding/json, so the canonical MIME key). An empty string means
// "no Content-Length was recorded at all", which is the residual shape.
func reflagRow(requestID string, complete bool, body []byte, contentLength string) *Event {
	ev := fullEvent(requestID)
	ev.Source, ev.FirstSource = "proxy", "proxy"
	ev.CaptureComplete = complete
	ev.ReqBody, ev.RespBody = body, nil
	ev.RespHeaders = `{}`
	if contentLength == "" {
		ev.ReqHeaders = `{"accept":["application/json"]}`
	} else {
		ev.ReqHeaders = fmt.Sprintf(`{"Content-Length":[%q]}`, contentLength)
	}
	return ev
}

// TestReflagFlipsAProvablePrefix is the repair itself: a row whose flag was
// laundered and whose stored body is a strict prefix of its Content-Length is
// set back to 0.
func TestReflagFlipsAProvablePrefix(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-prefix", true, []byte("0123456789"), "1000"))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.Flipped != 1 {
		t.Fatalf("counts = %+v, want Flipped 1", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CaptureComplete {
		t.Error("CaptureComplete = true, want false: the stored body is a strict prefix of its Content-Length")
	}
}

// TestReflagLeavesHonestRowsAlone: an already-honest capture is counted and not
// rewritten. The witness matches it too, so a repair that forgot the
// capture_complete = 1 gate would still pass every flip assertion -- which is
// why this case exists.
func TestReflagLeavesHonestRowsAlone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-honest", false, []byte("0123456789"), "1000"))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.AlreadyHonest != 1 || counts.Flipped != 0 {
		t.Errorf("counts = %+v, want AlreadyHonest 1 Flipped 0", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CaptureComplete {
		t.Error("CaptureComplete = true: a repaired row was rewritten by a pass that had nothing to do")
	}
}

// TestReflagCountsTheWitnesslessLaunderedRowAsResidual: a laundered row
// carrying a stream_incomplete warning with no Content-Length evidence cannot
// be repaired from what the store holds. It is counted residual and left alone
// -- the honest ceiling, stated rather than guessed at.
//
// This case does NOT pin the warning join by itself: a row that is warned and
// witnessless satisfies both the joined predicate and the naive one. The join
// is pinned by TestReflagCountsAnUnwarnedWitnesslessRowAsHealthy, which is the
// row the two disagree about.
func TestReflagCountsTheWitnesslessLaunderedRowAsResidual(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-residual", true, []byte("0123456789"), ""))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "stream_incomplete"}}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.Residual != 1 || counts.Flipped != 0 {
		t.Errorf("counts = %+v, want Residual 1 Flipped 0", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if !got.CaptureComplete {
		t.Error("CaptureComplete = false: a row with no witness must be left alone, not guessed at")
	}
}

// TestReflagWitnessesTheResponseSide pins the OR's second disjunct. A
// non-streamed response over the cap is a real case -- br-GI-7-08 makes the
// flag cover both bodies -- and the response side adds no rows on the store
// the plan was written against, so nothing else here would notice if
// resp_headers were misspelled and the disjunct never matched.
func TestReflagWitnessesTheResponseSide(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := reflagRow("req-reflag-resp", true, []byte("0123456789"), fmt.Sprint(len("0123456789")))
	ev.RespBody = []byte("short")
	ev.RespHeaders = `{"Content-Length":["4000"]}`
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.Flipped != 1 {
		t.Fatalf("counts.Flipped = %d, want 1: the truncated response is a witness on its own", counts.Flipped)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CaptureComplete {
		t.Error("CaptureComplete = true: the response body is a strict prefix of its Content-Length")
	}
}

// TestReflagCountsAnUnwarnedWitnesslessRowAsHealthy pins the stream_incomplete
// join, which is the whole difference between the residual and a count of
// healthy complete rows.
//
// The row here is a real capture that was NOT cut: its stored body is exactly
// as long as its Content-Length, so it is witnessless, and it carries no
// warning because nothing was wrong with it. The naive
// `residual = cc=1 AND NOT witness` predicate counts every row like this one;
// on the conductor's store that form printed 2,298 against a real residual of
// 138. It must stay out of the bucket.
func TestReflagCountsAnUnwarnedWitnesslessRowAsHealthy(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Stored body and Content-Length agree, so there is no prefix to prove.
	body := []byte("0123456789")
	id, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-healthy", true, body, fmt.Sprint(len(body))))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.Residual != 0 {
		t.Errorf("counts.Residual = %d, want 0: an unwarned, uncut capture is healthy, not the laundering residual", counts.Residual)
	}
	if counts.Flipped != 0 || counts.AlreadyHonest != 0 {
		t.Errorf("counts = %+v, want Flipped 0 AlreadyHonest 0: the row was never suspected", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if !got.CaptureComplete {
		t.Error("CaptureComplete = false: an uncut capture was flipped")
	}
}

// TestReflagDoesNotWitnessABodylessRow pins the `length(...) IS NOT NULL`
// guard so nobody drops it -- and so the COALESCE(length(req_body),0) mis-fix,
// which falsely witnesses the row, cannot land either.
//
// A body-less capture_complete=1 row is a real mode here (BodyPolicy full or
// off), and the row carries no warning, so it is neither flipped nor residual.
func TestReflagDoesNotWitnessABodylessRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-bodyless", true, nil, "1000"))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.Flipped != 0 {
		t.Errorf("counts.Flipped = %d, want 0: a row with no stored body has no prefix to compare", counts.Flipped)
	}
	if counts.Residual != 0 {
		t.Errorf("counts.Residual = %d, want 0: the row carries no warning, so it is not a suspected laundering", counts.Residual)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if !got.CaptureComplete {
		t.Error("CaptureComplete = false: a body-less row was falsely witnessed")
	}
}

// TestReflagIsIdempotent: the repair only ever narrows 1 -> 0, so a second run
// has nothing left to do. Without this the operator cannot tell a completed
// repair from one that silently failed.
func TestReflagIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-idem", true, []byte("0123456789"), "1000")); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	first, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures (first): %v", err)
	}
	if first.Flipped != 1 {
		t.Fatalf("first run counts = %+v, want Flipped 1", first)
	}

	second, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures (second): %v", err)
	}
	if second.Flipped != 0 || second.AlreadyHonest != 1 {
		t.Errorf("second run counts = %+v, want Flipped 0 AlreadyHonest 1", second)
	}
}

// TestReflagDoesNotRewriteSessionTotals records that capture_complete is not
// folded into `sessions`: unlike the cost columns, the flag needs no rollup,
// and a future reader should not assume reprice's transaction shape carries
// over here.
func TestReflagDoesNotRewriteSessionTotals(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := reflagRow("req-reflag-session", true, []byte("0123456789"), "1000")
	ev.InputTokens, ev.OutputTokens = 100, 50
	ev.CostUSD = f64(0.05)
	if err := st.UpsertSession(ctx, ev.SessionID, "", ev.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.ReconcileSession(ctx, ev.SessionID); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	before, err := st.GetSession(ctx, ev.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if _, err := st.ReflagIncompleteCaptures(ctx, false); err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CaptureComplete {
		t.Fatal("the fixture's row was not flipped, so this test would pass vacuously")
	}

	after, err := st.GetSession(ctx, ev.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if after.RequestCount != before.RequestCount ||
		after.InputTokens != before.InputTokens ||
		after.OutputTokens != before.OutputTokens ||
		after.TotalPromptTokens != before.TotalPromptTokens {
		t.Errorf("session totals moved: %+v -> %+v", before, after)
	}
}

// TestReflagDryRunWritesNothing: the dry run's counts come from the same code
// that does the repair, so the buckets an operator reads before `--yes` are the
// buckets the repair acts on.
func TestReflagDryRunWritesNothing(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, reflagRow("req-reflag-dry", true, []byte("0123456789"), "1000"))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, true)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures (dry run): %v", err)
	}
	if counts.Flipped != 1 {
		t.Errorf("dry-run counts.Flipped = %d, want 1 (it reports what --yes would flip)", counts.Flipped)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if !got.CaptureComplete {
		t.Error("CaptureComplete = false: a dry run rewrote the flag")
	}
}

// TestReflagScopeReachesUnmergedProxyRows pins the union predicate's first
// half. A never-merged proxy row has source_refs = ” -- only the merge ever
// assigns it -- so a scope spelled as the bare instr(source_refs,'proxy') > 0
// would silently skip every one of them.
func TestReflagScopeReachesUnmergedProxyRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := reflagRow("req-reflag-unmerged", true, []byte("0123456789"), "1000")
	ev.SourceRefs = nil // never merged
	if _, _, err := st.InsertEvent(ctx, ev); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatalf("ReflagIncompleteCaptures: %v", err)
	}
	if counts.Flipped != 1 {
		t.Errorf("counts.Flipped = %d, want 1: an unmerged proxy row is in scope", counts.Flipped)
	}
}
