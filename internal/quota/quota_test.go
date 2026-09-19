package quota

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

func insertEvent(t *testing.T, st *store.Store, requestID, account, model string, at time.Time, inputTokens int) {
	t.Helper()
	ev := &store.Event{
		RequestID:      requestID,
		Source:         "proxy",
		FirstSource:    "proxy",
		StartedAt:      at,
		Account:        account,
		BillingMode:    "subscription",
		ModelRequested: model,
		ModelResolved:  model,
		InputTokens:    inputTokens,
	}
	if _, _, err := st.InsertEvent(context.Background(), ev); err != nil {
		t.Fatalf("InsertEvent %s: %v", requestID, err)
	}
}

// Rolling window burn is half-open [now-duration, now): a call exactly
// at the start boundary counts, a call exactly at `now` itself does not.
func TestComputeBurnHalfOpenBoundary(t *testing.T) {
	st := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)

	insertEvent(t, st, "req-boundary-start", "acct1", "claude-sonnet-5", now.Add(-5*time.Hour), 100) // inside
	insertEvent(t, st, "req-boundary-end", "acct1", "claude-sonnet-5", now, 200)                     // outside
	insertEvent(t, st, "req-boundary-mid", "acct1", "claude-sonnet-5", now.Add(-1*time.Hour), 50)    // inside

	b, err := ComputeBurn(context.Background(), st, "acct1", "claude-sonnet-5", Window5h, now)
	if err != nil {
		t.Fatalf("ComputeBurn: %v", err)
	}
	if b.Requests != 2 {
		t.Fatalf("Requests = %d, want 2 (the boundary-start and mid calls, not the exactly-now call)", b.Requests)
	}
	if b.Tokens != 150 {
		t.Errorf("Tokens = %d, want 150 (100+50)", b.Tokens)
	}
}

// A longer window sees calls a shorter window would exclude.
func TestComputeBurnWindowScopesToDuration(t *testing.T) {
	st := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)

	insertEvent(t, st, "req-old", "acct1", "m", now.Add(-6*time.Hour), 10) // outside 5h, inside 7d
	insertEvent(t, st, "req-new", "acct1", "m", now.Add(-1*time.Hour), 20)

	b5h, err := ComputeBurn(context.Background(), st, "acct1", "m", Window5h, now)
	if err != nil {
		t.Fatalf("ComputeBurn 5h: %v", err)
	}
	if b5h.Requests != 1 || b5h.Tokens != 20 {
		t.Errorf("5h burn = %+v, want 1 request / 20 tokens", b5h)
	}

	b7d, err := ComputeBurn(context.Background(), st, "acct1", "m", Window7d, now)
	if err != nil {
		t.Fatalf("ComputeBurn 7d: %v", err)
	}
	if b7d.Requests != 2 || b7d.Tokens != 30 {
		t.Errorf("7d burn = %+v, want 2 requests / 30 tokens", b7d)
	}
}

// CrossCheck pairs each snapshot with the burn computed at that
// snapshot's own observed_at instant, not at "now".
func TestCrossCheckPairsSnapshotWithBurnAtItsInstant(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	observedAt := time.Unix(1_800_000_000, 0)

	insertEvent(t, st, "req-1", "acct1", "m", observedAt.Add(-1*time.Hour), 40)
	// This event happens after the snapshot was taken -- it must not be
	// folded into the cross-check's burn for that snapshot.
	insertEvent(t, st, "req-2", "acct1", "m", observedAt.Add(1*time.Hour), 999)

	pct := 20.0
	if err := st.InsertQuotaSnapshot(ctx, store.QuotaSnapshot{
		ObservedAt:     observedAt,
		Account:        "acct1",
		Window:         "5h",
		UtilizationPct: &pct,
		Status:         "ok",
	}); err != nil {
		t.Fatalf("InsertQuotaSnapshot: %v", err)
	}

	results, err := CrossCheck(ctx, st, "acct1", "m", 10)
	if err != nil {
		t.Fatalf("CrossCheck: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Burn.Tokens != 40 {
		t.Errorf("Burn.Tokens = %d, want 40 (only the pre-snapshot event, not the later 999)", results[0].Burn.Tokens)
	}
}

// Calibrate offers a candidate limit from a 100% snapshot and ignores
// anything below 100 -- it never writes anywhere itself.
func TestCalibrateLearnsFromHundredPercentSnapshot(t *testing.T) {
	hundred := 100.0
	fifty := 50.0
	results := []CrossCheckResult{
		{
			Snapshot: store.QuotaSnapshot{Account: "acct1", UtilizationPct: &hundred, ObservedAt: time.Unix(1, 0)},
			Burn:     Burn{Window: Window5h, Tokens: 500000},
		},
		{
			Snapshot: store.QuotaSnapshot{Account: "acct1", UtilizationPct: &fifty, ObservedAt: time.Unix(2, 0)},
			Burn:     Burn{Window: Window5h, Tokens: 250000},
		},
	}

	learned := Calibrate(results)
	if len(learned) != 1 {
		t.Fatalf("learned = %d, want 1 (only the 100%% snapshot)", len(learned))
	}
	if learned[0].TokensAt100 != 500000 {
		t.Errorf("TokensAt100 = %d, want 500000", learned[0].TokensAt100)
	}
	if learned[0].TokensPerPercent() != 5000 {
		t.Errorf("TokensPerPercent = %v, want 5000", learned[0].TokensPerPercent())
	}
}

// Configured limits: a burn rate that would exceed the limit before
// reset fires Approaching.
func TestProjectConfiguredFiresApproaching(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := Burn{Window: Window5h, Now: now, Tokens: 900000} // 900k tokens burned in the last 5h
	limits := Limits{Window5h: 1000000}
	resetsAt := now.Add(4 * time.Hour) // window resets in 4h; at this burn rate, more than enough left to blow past 1M

	p := Project(b, limits, resetsAt)
	if !p.Configured {
		t.Fatal("Configured = false, want true")
	}
	if p.UtilizationPct != 90 {
		t.Errorf("UtilizationPct = %v, want 90", p.UtilizationPct)
	}
	if !p.Approaching {
		t.Error("Approaching = false, want true (continuing at this rate blows past the limit before reset)")
	}
}

// A low, steady burn well under the limit with plenty of window left to
// reset does not fire.
func TestProjectConfiguredDoesNotFireWhenSafelyUnderLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := Burn{Window: Window7d, Now: now, Tokens: 1000}
	limits := Limits{Window7d: 1000000}
	resetsAt := now.Add(1 * time.Hour)

	p := Project(b, limits, resetsAt)
	if p.Approaching {
		t.Error("Approaching = true, want false (1000/1000000 tokens, nowhere near the limit)")
	}
}

// Unconfigured limits: the same computation runs (Burn is populated) but
// Project reports the unconfigured state, never a bare 0%.
func TestProjectUnconfiguredReportsUnconfiguredNotZeroPercent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := Burn{Window: Window5h, Now: now, Tokens: 900000}

	p := Project(b, Limits{}, now.Add(time.Hour))
	if p.Configured {
		t.Error("Configured = true, want false (no limit set)")
	}
	if p.Approaching {
		t.Error("Approaching = true, want false when unconfigured")
	}
	if p.UtilizationPct != 0 {
		t.Errorf("UtilizationPct = %v, want 0 -- but callers must check Configured, never read this as a real percentage", p.UtilizationPct)
	}
}
