package reconcile

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

func f64(v float64) *float64 { return &v }

func insertAPIEvent(t *testing.T, st *store.Store, requestID, model string, startedAt time.Time, costUSD float64) int64 {
	t.Helper()
	id, err := st.InsertEvent(context.Background(), &store.Event{
		RequestID:       requestID,
		Source:          "proxy",
		FirstSource:     "proxy",
		StartedAt:       startedAt,
		AuthKind:        "api_key",
		BillingMode:     "api",
		ModelResolved:   model,
		CostUSD:         f64(costUSD),
		CostSource:      "shipped",
		CaptureComplete: true,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	return id
}

var day1 = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

func TestReconcileNoDriftBelowThreshold(t *testing.T) {
	st := newTestStore(t)
	insertAPIEvent(t, st, "req1", "claude-sonnet-5", day1.Add(time.Hour), 10.00)

	billed := []store.AdminCostDay{{DayStart: day1, Model: "claude-sonnet-5", AmountUSD: 10.02, Currency: "USD"}}
	res, err := Reconcile(context.Background(), st, billed, 0.05)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if row.CostDrift {
		t.Errorf("CostDrift = true, want false (0.02 divergence is at/below the 0.05 threshold)")
	}
}

// Test 15: divergence strictly above the threshold fires cost_drift; at
// or below it, none.
func TestReconcileDriftAtThresholdBoundary(t *testing.T) {
	st := newTestStore(t)
	insertAPIEvent(t, st, "req1", "claude-sonnet-5", day1.Add(time.Hour), 10.00)

	// Exactly at the threshold: must NOT fire.
	billed := []store.AdminCostDay{{DayStart: day1, Model: "claude-sonnet-5", AmountUSD: 10.05, Currency: "USD"}}
	res, err := Reconcile(context.Background(), st, billed, 0.05)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Rows[0].CostDrift {
		t.Errorf("CostDrift = true at exactly the threshold, want false")
	}

	// Just above the threshold: must fire.
	billed[0].AmountUSD = 10.06
	res, err = Reconcile(context.Background(), st, billed, 0.05)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Rows[0].CostDrift {
		t.Errorf("CostDrift = false above the threshold, want true")
	}
	if res.Rows[0].DivergedUSD == nil || *res.Rows[0].DivergedUSD != 0.06 {
		t.Errorf("DivergedUSD = %v, want 0.06", res.Rows[0].DivergedUSD)
	}
}

// Computed and billed are returned in separate, labelled fields and are
// never added together.
func TestReconcileComputedAndBilledNeverSummed(t *testing.T) {
	st := newTestStore(t)
	insertAPIEvent(t, st, "req1", "claude-sonnet-5", day1.Add(time.Hour), 5.00)

	billed := []store.AdminCostDay{{DayStart: day1, Model: "claude-sonnet-5", AmountUSD: 3.00, Currency: "USD"}}
	res, err := Reconcile(context.Background(), st, billed, 100)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row := res.Rows[0]
	if row.ComputedUSD == nil || *row.ComputedUSD != 5.00 {
		t.Errorf("ComputedUSD = %v, want 5.00", row.ComputedUSD)
	}
	if row.BilledUSD != 3.00 {
		t.Errorf("BilledUSD = %v, want 3.00", row.BilledUSD)
	}
}

// A subscription-only account supplies no billed rows (there is no Admin
// cost report entry for subscription usage), so no (day, model) pair is
// ever compared and cost_drift can never fire.
func TestReconcileSubscriptionOnlyNoDriftPossible(t *testing.T) {
	st := newTestStore(t)
	_, err := st.InsertEvent(context.Background(), &store.Event{
		RequestID:            "req1",
		Source:               "jsonl",
		FirstSource:          "jsonl",
		StartedAt:            day1.Add(time.Hour),
		AuthKind:             "subscription",
		BillingMode:          "subscription",
		ModelResolved:        "claude-sonnet-5",
		ApiEquivalentCostUSD: f64(5.00),
		CostSource:           "shipped",
		CaptureComplete:      true,
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	res, err := Reconcile(context.Background(), st, nil, 0.01)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("got %d rows for a subscription-only account with no billed rows, want 0", len(res.Rows))
	}
}

// billed's multiple description rows for the same (day, model) are summed
// before comparison, not compared one at a time.
func TestReconcileSumsBilledDescriptionsPerDayModel(t *testing.T) {
	st := newTestStore(t)
	insertAPIEvent(t, st, "req1", "claude-sonnet-5", day1.Add(time.Hour), 7.00)

	billed := []store.AdminCostDay{
		{DayStart: day1, Model: "claude-sonnet-5", Description: "input tokens", AmountUSD: 4.00, Currency: "USD"},
		{DayStart: day1, Model: "claude-sonnet-5", Description: "output tokens", AmountUSD: 3.00, Currency: "USD"},
	}
	res, err := Reconcile(context.Background(), st, billed, 100)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1 (both description rows fold into one day/model)", len(res.Rows))
	}
	if res.Rows[0].BilledUSD != 7.00 {
		t.Errorf("BilledUSD = %v, want 7.00 (4.00 + 3.00)", res.Rows[0].BilledUSD)
	}
}

// A vs B: an already-raised source_mismatch warning is surfaced in the
// reconcile result's count, not re-detected here (that happens in the
// store's own cross-source merge, br-GI-1-06/11).
func TestReconcileSurfacesSourceMismatchCount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := insertAPIEvent(t, st, "req1", "claude-sonnet-5", day1.Add(time.Hour), 1.00)
	if err := st.UpsertWarnings(ctx, id, []store.Warning{
		{Kind: "source_mismatch", Severity: "error", Detail: "token disagreement"},
	}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}

	res, err := Reconcile(ctx, st, nil, 0.01)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.SourceMismatchCount != 1 {
		t.Errorf("SourceMismatchCount = %d, want 1", res.SourceMismatchCount)
	}
}

// A day/model with no priced API events yields a nil ComputedUSD, not a
// $0.00 -- so it cannot be mistaken for "computed cost is zero".
func TestReconcileNoComputedFigureIsNilNotZero(t *testing.T) {
	st := newTestStore(t)
	billed := []store.AdminCostDay{{DayStart: day1, Model: "claude-sonnet-5", AmountUSD: 3.00, Currency: "USD"}}
	res, err := Reconcile(context.Background(), st, billed, 0.01)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	row := res.Rows[0]
	if row.ComputedUSD != nil {
		t.Errorf("ComputedUSD = %v, want nil (no events that day/model)", *row.ComputedUSD)
	}
	if row.CostDrift {
		t.Errorf("CostDrift = true with no computed figure to compare, want false")
	}
}
