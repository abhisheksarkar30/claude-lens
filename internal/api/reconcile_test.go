package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// reconcileDay is a fixed UTC midnight, so the Admin cost row's day and a
// seeded event's day land on the same key.
var reconcileDay = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// seedBilled records what the Admin cost report says was billed for one
// (day, model).
func seedBilled(t *testing.T, st *store.Store, day time.Time, model string, usd float64) {
	t.Helper()
	if err := st.UpsertAdminCostDays(context.Background(), []store.AdminCostDay{{
		DayStart: day, WindowStart: day, WindowEnd: day.Add(24 * time.Hour),
		Model: model, Description: "usage", AmountUSD: usd, Currency: "USD",
	}}); err != nil {
		t.Fatalf("UpsertAdminCostDays: %v", err)
	}
}

// seedComputed records what this tool's own pricing computed for one call.
func seedComputed(t *testing.T, st *store.Store, model string, usd float64) {
	t.Helper()
	seedEvent(t, st, func(e *store.Event) {
		e.ModelResolved = model
		e.ModelRequested = model
		e.StartedAt = reconcileDay.Add(time.Hour)
		e.CostUSD = &usd
		e.CostSource = "shipped"
	})
}

func reconcileRowFor(t *testing.T, res reconcileResponse, model string) reconcileRow {
	t.Helper()
	for _, row := range res.Rows {
		if row.Model == model {
			return row
		}
	}
	t.Fatalf("no row for %s in %+v", model, res.Rows)
	return reconcileRow{}
}

// TestReconcileKeepsComputedAndBilledSeparate is the bead's first clause. The
// two figures come from different sources about the same usage, so the route
// reports both and never their sum -- and the raw JSON is checked for a field
// that could hold one.
func TestReconcileKeepsComputedAndBilledSeparate(t *testing.T) {
	st := newTestStore(t)
	seedComputed(t, st, "claude-sonnet-5", 5.00)
	seedBilled(t, st, reconcileDay, "claude-sonnet-5", 3.00)
	handler, _, _, _ := newTestAPI(t, st)

	body := getOK(t, handler, "/api/reconcile").Body.Bytes()
	got := decodeJSON[reconcileResponse](t, strings.NewReader(string(body)))
	row := reconcileRowFor(t, got, "claude-sonnet-5")

	if row.ComputedUSD == nil || *row.ComputedUSD != 5.00 {
		t.Errorf("computed_usd = %v, want 5.00", row.ComputedUSD)
	}
	if row.BilledUSD != 3.00 {
		t.Errorf("billed_usd = %v, want 3.00", row.BilledUSD)
	}
	if row.DivergedUSD == nil || *row.DivergedUSD != 2.00 {
		t.Errorf("diverged_usd = %v, want 2.00", row.DivergedUSD)
	}
	// 8.00 would be the sum. The allowlist below is what enforces its absence.
	if got.Scope == "" {
		t.Error("the response states no scope, so an empty row list reads as agreement")
	}

	var raw struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range raw.Rows {
		for field := range r {
			switch field {
			case "day", "model", "computed_usd", "billed_usd", "diverged_usd", "cost_drift":
			default:
				t.Errorf("row exposes unexpected field %q", field)
			}
		}
	}
}

// TestReconcileDriftFiresAboveTheThresholdOnly is the bead's second clause,
// pinned at the boundary: strictly above the threshold fires, at it does not.
func TestReconcileDriftFiresAboveTheThresholdOnly(t *testing.T) {
	st := newTestStore(t)
	seedComputed(t, st, "claude-sonnet-5", 10.00)
	seedBilled(t, st, reconcileDay, "claude-sonnet-5", 10.05) // diverges by exactly 0.05
	handler, _, _, _ := newTestAPI(t, st)

	got := decodeJSON[reconcileResponse](t, getOK(t, handler, "/api/reconcile?threshold=0.05").Body)
	if got.ThresholdUSD != 0.05 {
		t.Errorf("threshold_usd = %v, want the value asked for, echoed back", got.ThresholdUSD)
	}
	row := reconcileRowFor(t, got, "claude-sonnet-5")
	if row.CostDrift {
		t.Errorf("cost_drift fired at exactly the threshold; the rule is strictly greater")
	}
	if row.DivergedUSD == nil || *row.DivergedUSD != 0.05 {
		t.Fatalf("diverged_usd = %v, want 0.05", row.DivergedUSD)
	}

	// A hair further apart, and it fires.
	seedBilled(t, st, reconcileDay, "claude-sonnet-5", 10.06)
	got = decodeJSON[reconcileResponse](t, getOK(t, handler, "/api/reconcile?threshold=0.05").Body)
	row = reconcileRowFor(t, got, "claude-sonnet-5")
	if !row.CostDrift {
		t.Errorf("cost_drift did not fire at 0.06 against a 0.05 threshold")
	}
	if row.DivergedUSD == nil || *row.DivergedUSD != 0.06 {
		t.Errorf("diverged_usd = %v, want 0.06", row.DivergedUSD)
	}
}

// TestReconcileDefaultThresholdIsTheCLIs: the dashboard and `clens reconcile`
// must not disagree about where the line is.
func TestReconcileDefaultThresholdIsTheCLIs(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	got := decodeJSON[reconcileResponse](t, getOK(t, handler, "/api/reconcile").Body)
	if got.ThresholdUSD != costDriftThresholdUSD {
		t.Errorf("default threshold_usd = %v, want %v", got.ThresholdUSD, costDriftThresholdUSD)
	}
}

func TestReconcileRejectsABadThreshold(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	for _, q := range []string{"threshold=-1", "threshold=cheap", "threshold="} {
		// An empty threshold is the documented "use the default", not an error.
		if q == "threshold=" {
			if code := getRaw(t, handler, "/api/reconcile?"+q); code != http.StatusOK {
				t.Errorf("?%s: status = %d, want 200 (absent means default)", q, code)
			}
			continue
		}
		if code := getRaw(t, handler, "/api/reconcile?"+q); code != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400", q, code)
		}
	}
}

// TestReconcileNoComparableFigureIsNullNotZero: a billed day with no priced API
// row of ours to compare against has no computed figure at all. $0.00 would
// read as "we computed zero cost", which is a claim we cannot make.
func TestReconcileNoComparableFigureIsNullNotZero(t *testing.T) {
	st := newTestStore(t)
	seedBilled(t, st, reconcileDay, "claude-sonnet-5", 12.00)
	handler, _, _, _ := newTestAPI(t, st)

	var raw struct {
		Rows []map[string]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(getOK(t, handler, "/api/reconcile").Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Rows) != 1 {
		t.Fatalf("got %d rows, want the one billed day", len(raw.Rows))
	}
	for _, field := range []string{"computed_usd", "diverged_usd"} {
		if got := string(raw.Rows[0][field]); got != "null" {
			t.Errorf("%s = %s, want null -- no priced API row to compute from", field, got)
		}
	}
	if got := string(raw.Rows[0]["cost_drift"]); got != "false" {
		t.Errorf("cost_drift = %s, want false: nothing was comparable", got)
	}
}

// TestReconcileReportsSourceMismatchesSeparately: the A-vs-B warning count has
// no day or model of its own, so it is a count on the response rather than a
// row that would need both.
func TestReconcileReportsSourceMismatchesSeparately(t *testing.T) {
	st := newTestStore(t)
	ev := seedEvent(t, st, nil)
	if err := st.UpsertWarnings(context.Background(), ev.ID, []store.Warning{
		{Kind: "source_mismatch", Severity: "warn", Detail: "proxy and jsonl disagree on input_tokens"},
	}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	got := decodeJSON[reconcileResponse](t, getOK(t, handler, "/api/reconcile").Body)
	if got.SourceMismatchCount != 1 {
		t.Errorf("source_mismatch_count = %d, want 1", got.SourceMismatchCount)
	}
	if len(got.Rows) != 0 {
		t.Errorf("got %d rows, want none: a mismatch is not a (day, model)", len(got.Rows))
	}
}
