package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// getRaw is getOK without the 200 assertion, for the routes that must refuse a
// bad query param.
func getRaw(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code
}

// fixtureQuotaAccounts wires one subscription account and one API account, so
// every test here can also assert the API account is left out.
func fixtureQuotaAccounts(t *testing.T, h *api) {
	t.Helper()
	h.SetAccounts(func(context.Context) (Accounts, error) {
		return Accounts{List: []Account{
			{Name: "max", BillingMode: "subscription", Plan: "max20x"},
			{Name: "work", BillingMode: "api"},
		}}, nil
	})
}

// seedBurn inserts n events of tokens each, attributed to account, started
// `ago` before now.
func seedBurn(t *testing.T, st *store.Store, account string, tokens int, ago time.Duration) {
	t.Helper()
	seedEvent(t, st, func(e *store.Event) {
		e.Account = account
		e.BillingMode = "subscription"
		e.InputTokens = tokens
		e.OutputTokens = 0
		e.StartedAt = time.Now().Add(-ago)
		e.CostUSD = nil
		e.CostSource = "unpriced"
	})
}

// TestQuotaReportsUnconfiguredWithoutALimit is the bead's headline clause: no
// limit configured means the literal word "unconfigured", not a percentage of
// a number nobody supplied.
func TestQuotaReportsUnconfiguredWithoutALimit(t *testing.T) {
	st := newTestStore(t)
	seedBurn(t, st, "max", 5000, 30*time.Minute)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureQuotaAccounts(t, handler)

	got := decodeJSON[quotaResponse](t, getOK(t, handler, "/api/quota").Body)
	if got.LimitsConfigured {
		t.Error("limits_configured is true with no limit supplied")
	}
	if len(got.Accounts) != 1 {
		t.Fatalf("got %d accounts, want only the subscription one", len(got.Accounts))
	}
	acct := got.Accounts[0]
	if acct.Account != "max" {
		t.Fatalf("account = %q, want max", acct.Account)
	}
	if len(acct.Windows) != 2 {
		t.Fatalf("got %d window rows, want one per rolling window (5h, 7d)", len(acct.Windows))
	}
	for _, row := range acct.Windows {
		if row.LimitState != limitUnconfigured {
			t.Errorf("window %s: limit_state = %q, want %q", row.Window, row.LimitState, limitUnconfigured)
		}
		if row.UtilizationPct != nil {
			t.Errorf("window %s: utilization_pct = %v, want null with no limit", row.Window, *row.UtilizationPct)
		}
		if row.Approaching {
			t.Errorf("window %s: approaching is true with no limit to approach", row.Window)
		}
	}
	// The burn itself is still reported -- the limit is what is missing, not
	// the measurement.
	if got5h := acct.Windows[0]; got5h.Window != "5h" || got5h.Tokens != 5000 {
		t.Errorf("5h row = %+v, want the 5000 tokens burned", got5h)
	}
}

// TestQuotaPercentOnlyWhenConfigured: the same fixture, with a limit, produces
// a percentage -- and it is a real one.
func TestQuotaPercentOnlyWhenConfigured(t *testing.T) {
	st := newTestStore(t)
	seedBurn(t, st, "max", 2500, 30*time.Minute)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureQuotaAccounts(t, handler)

	got := decodeJSON[quotaResponse](t, getOK(t, handler, "/api/quota?limit_5h=10000").Body)
	if !got.LimitsConfigured {
		t.Error("limits_configured is false with a limit supplied")
	}
	rows := got.Accounts[0].Windows
	byWindow := map[string]QuotaRow{}
	for _, r := range rows {
		byWindow[r.Window] = r
	}
	five := byWindow["5h"]
	if five.LimitState != limitConfigured {
		t.Fatalf("5h limit_state = %q, want %q", five.LimitState, limitConfigured)
	}
	if five.Limit != 10000 {
		t.Fatalf("5h limit = %d, want 10000", five.Limit)
	}
	if five.UtilizationPct == nil {
		t.Fatal("5h utilization_pct is null with a limit configured")
	}
	if got := *five.UtilizationPct; got < 24.9 || got > 25.1 {
		t.Fatalf("5h utilization_pct = %v, want ~25", got)
	}
	// The 7d window had no limit supplied, so it stays unconfigured even
	// though the 5h one is set: the two are independent.
	if seven := byWindow["7d"]; seven.LimitState != limitUnconfigured || seven.UtilizationPct != nil {
		t.Fatalf("7d row = %+v, want unconfigured alongside a configured 5h", seven)
	}
}

func TestQuotaRejectsANonPositiveOrNonNumericLimit(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureQuotaAccounts(t, handler)

	for _, q := range []string{"limit_5h=0", "limit_5h=-5", "limit_5h=lots", "limit_7d=1.5"} {
		rr := getRaw(t, handler, "/api/quota?"+q)
		if rr != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400", q, rr)
		}
	}
}

// TestQuotaReportsSnapshotAndCalibration: the snapshot is the one figure here
// that came from Anthropic, and a snapshot at 100% yields an offered -- never
// applied -- limit candidate.
func TestQuotaReportsSnapshotAndCalibration(t *testing.T) {
	st := newTestStore(t)
	seedBurn(t, st, "max", 4000, 45*time.Minute)
	observedAt := time.Now().Add(-40 * time.Minute).UTC()
	pct := 100.0
	resetsAt := time.Now().Add(4 * time.Hour).UTC()
	if err := st.InsertQuotaSnapshot(context.Background(), store.QuotaSnapshot{
		ObservedAt: observedAt, Account: "max", Window: "5h",
		UtilizationPct: &pct, ResetsAt: &resetsAt, Status: "allowed",
	}); err != nil {
		t.Fatalf("InsertQuotaSnapshot: %v", err)
	}
	// A second snapshot for the other window, so "newest for this window" is
	// a real selection rather than "the only row".
	sevenPct := 12.0
	if err := st.InsertQuotaSnapshot(context.Background(), store.QuotaSnapshot{
		ObservedAt: observedAt, Account: "max", Window: "7d",
		UtilizationPct: &sevenPct, Status: "allowed",
	}); err != nil {
		t.Fatalf("InsertQuotaSnapshot: %v", err)
	}

	handler, _, _, _ := newTestAPI(t, st)
	fixtureQuotaAccounts(t, handler)

	got := decodeJSON[quotaResponse](t, getOK(t, handler, "/api/quota").Body)
	acct := got.Accounts[0]
	byWindow := map[string]QuotaRow{}
	for _, r := range acct.Windows {
		byWindow[r.Window] = r
	}
	if byWindow["5h"].LastSnapshot == nil {
		t.Fatal("the 5h snapshot is missing from the response")
	}
	if got := *byWindow["5h"].LastSnapshot.UtilizationPct; got != 100 {
		t.Errorf("5h snapshot utilization = %v, want 100", got)
	}
	if byWindow["7d"].LastSnapshot == nil || *byWindow["7d"].LastSnapshot.UtilizationPct != sevenPct {
		t.Errorf("the 7d window did not get its own snapshot: %+v", byWindow["7d"].LastSnapshot)
	}

	if len(acct.Calibration) != 1 {
		t.Fatalf("got %d calibration candidates, want 1 (the 100%% snapshot)", len(acct.Calibration))
	}
	learned := acct.Calibration[0]
	if learned.Window != "5h" {
		t.Errorf("candidate window = %q, want 5h", learned.Window)
	}
	if learned.TokensAt100 == 0 {
		t.Error("the candidate carries no token count, so it says nothing")
	}
	// Offered, not applied: the window is still unconfigured.
	if byWindow["5h"].LimitState != limitUnconfigured {
		t.Error("a calibration candidate was silently applied as the limit")
	}
}

// TestQuotaSnapshotNeverRanIsNull: a window with no snapshot must marshal
// last_snapshot as null rather than omit the key or invent a zero row.
func TestQuotaSnapshotNeverRanIsNull(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureQuotaAccounts(t, handler)

	var raw struct {
		Accounts []struct {
			Windows []map[string]json.RawMessage `json:"windows"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(getOK(t, handler, "/api/quota").Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, w := range raw.Accounts[0].Windows {
		if got := string(w["last_snapshot"]); got != "null" {
			t.Errorf("last_snapshot = %s, want null", got)
		}
		if got := string(w["utilization_pct"]); got != "null" {
			t.Errorf("utilization_pct = %s, want null", got)
		}
	}
}
