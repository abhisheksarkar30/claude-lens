package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "lens.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var seedCounter atomic.Int64

// seedEvent inserts a reasonable default *store.Event, optionally tweaked
// by opts, and returns it with ID set.
func seedEvent(t *testing.T, st *store.Store, opts func(*store.Event)) *store.Event {
	t.Helper()
	cost := 0.01
	ev := &store.Event{
		RequestID:      "req_" + strconv.FormatInt(seedCounter.Add(1), 10),
		Source:         "proxy",
		FirstSource:    "proxy",
		StartedAt:      time.Now().Add(-time.Minute),
		BillingMode:    "api",
		ModelRequested: "claude-sonnet-5",
		ModelResolved:  "claude-sonnet-5",
		InputTokens:    100,
		OutputTokens:   200,
		CostUSD:        &cost,
		CostSource:     "shipped",
		Method:         "POST",
		Path:           "/v1/messages",
		Status:         200,
	}
	if opts != nil {
		opts(ev)
	}
	id, err := st.InsertEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	ev.ID = id
	return ev
}

// newTestAPI builds a handler backed by st, a fresh broker, and a real
// (unstarted) consumer -- Run is never called in these tests, so Stats() is
// fine on a fresh Consumer.
func newTestAPI(t *testing.T, st Store) (*api, *sink.Sink, *consumer.Consumer, *Broker) {
	t.Helper()
	sk := sink.New(16)
	cons := consumer.New(sk, nil, nil)
	broker := NewBroker()
	return New(st, sk, cons, broker), sk, cons, broker
}

func decodeJSON[T any](t *testing.T, body io.Reader) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

func getOK(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200: %s", path, rr.Code, rr.Body.String())
	}
	return rr
}

func TestListRequests(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 5; i++ {
		seedEvent(t, st, nil)
	}
	handler, _, _, _ := newTestAPI(t, st)

	got := decodeJSON[[]*store.Event](t, getOK(t, handler, "/api/requests").Body)
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5", len(got))
	}
}

func TestListRequestsFilters(t *testing.T) {
	st := newTestStore(t)
	seedEvent(t, st, func(e *store.Event) { e.ModelResolved = "claude-opus-5" })
	seedEvent(t, st, func(e *store.Event) { e.ModelResolved = "claude-haiku-4-5" })
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/requests?model=claude-opus-5")
	got := decodeJSON[[]*store.Event](t, rr.Body)
	if len(got) != 1 || got[0].ModelResolved != "claude-opus-5" {
		t.Fatalf("model filter: got %+v, want exactly one claude-opus-5 row", got)
	}
}

func TestGetRequestIncludesWarnings(t *testing.T) {
	st := newTestStore(t)
	ev := seedEvent(t, st, nil)
	if err := st.UpsertWarnings(context.Background(), ev.ID, []store.Warning{
		{Kind: "cache_miss", Severity: "warn", Detail: "d"},
	}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/requests/"+strconv.FormatInt(ev.ID, 10))
	got := decodeJSON[eventDetail](t, rr.Body)
	if len(got.Warnings) != 1 || got.Warnings[0].Kind != "cache_miss" {
		t.Fatalf("got warnings %+v, want one cache_miss", got.Warnings)
	}
}

func TestGetRequestNotFound(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/requests/999", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// Test 12b (route half): a mixed api/subscription fixture through
// /api/stats never collapses into one summed figure. store.StatsByModel
// already groups by billing_mode in SQL, so the two rows for the same
// model are told apart by which cost field is populated.
func TestStatsSplitsBillingModeNeverSums(t *testing.T) {
	st := newTestStore(t)
	apiCost := 1.0
	subCost := 2.0
	seedEvent(t, st, func(e *store.Event) {
		e.BillingMode = "api"
		e.CostUSD = &apiCost
		e.ApiEquivalentCostUSD = nil
	})
	seedEvent(t, st, func(e *store.Event) {
		e.BillingMode = "subscription"
		e.CostUSD = nil
		e.ApiEquivalentCostUSD = &subCost
	})
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/stats")
	got := decodeJSON[statsResponse](t, rr.Body)

	if got.Summary.TotalCostUSD == nil || *got.Summary.TotalCostUSD != apiCost {
		t.Errorf("summary TotalCostUSD = %v, want %v (api-billed only, never summed with subscription)", got.Summary.TotalCostUSD, apiCost)
	}
	if got.Summary.TotalApiEquivalentCostUSD == nil || *got.Summary.TotalApiEquivalentCostUSD != subCost {
		t.Errorf("summary TotalApiEquivalentCostUSD = %v, want %v", got.Summary.TotalApiEquivalentCostUSD, subCost)
	}
	if len(got.ByModel) != 2 {
		t.Fatalf("by_model rows = %d, want 2 (one per billing_mode for the same model)", len(got.ByModel))
	}
}

func TestWarningsSummaryUnpaginated(t *testing.T) {
	st := newTestStore(t)
	ev := seedEvent(t, st, nil)
	if err := st.UpsertWarnings(context.Background(), ev.ID, []store.Warning{
		{Kind: "alpha", Severity: "warn"},
	}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/warnings/summary?limit=1")
	got := decodeJSON[[]store.WarningSummary](t, rr.Body)
	if len(got) != 1 || got[0].Kind != "alpha" || got[0].Count != 1 {
		t.Fatalf("got %+v, want one alpha group of count 1 (limit must be ignored)", got)
	}
	if rr.Header().Get("X-Total-Count") != "" {
		t.Errorf("X-Total-Count present on the unpaginated summary route, want absent")
	}
}

func TestListWarningsFilterByKind(t *testing.T) {
	st := newTestStore(t)
	ev := seedEvent(t, st, nil)
	if err := st.UpsertWarnings(context.Background(), ev.ID, []store.Warning{
		{Kind: "alpha", Severity: "warn"}, {Kind: "beta", Severity: "warn"},
	}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/warnings?kind=alpha")
	got := decodeJSON[[]store.Warning](t, rr.Body)
	if len(got) != 1 || got[0].Kind != "alpha" {
		t.Fatalf("got %+v, want exactly the alpha warning", got)
	}
}

// Test 12b (sessions half): the session's two cost totals are NULL, not
// $0.00, on whichever side has no matching row.
func TestSessionCostSplitNullNotZero(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sessionID := "sess_1"
	apiCost := 5.0
	seedEvent(t, st, func(e *store.Event) {
		e.SessionID = sessionID
		e.BillingMode = "api"
		e.CostUSD = &apiCost
		e.ApiEquivalentCostUSD = nil
	})
	if err := st.UpsertSession(ctx, sessionID, "", time.Now()); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := st.ReconcileSession(ctx, sessionID); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/sessions/"+sessionID)
	got := decodeJSON[sessionDetail](t, rr.Body)
	if got.TotalCostUSD == nil || *got.TotalCostUSD != apiCost {
		t.Errorf("TotalCostUSD = %v, want %v", got.TotalCostUSD, apiCost)
	}
	if got.TotalApiEquivalentCostUSD != nil {
		t.Errorf("TotalApiEquivalentCostUSD = %v, want nil (no subscription row)", *got.TotalApiEquivalentCostUSD)
	}
	if len(got.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(got.Calls))
	}
}

func TestGetSessionNotFound(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHealthReportsSinkAndConsumerStats(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/health")
	got := decodeJSON[healthResponse](t, rr.Body)
	if got.LastWriteAt != nil {
		t.Errorf("LastWriteAt = %v, want nil on a store nothing wrote through yet", got.LastWriteAt)
	}
}

func TestPricesUnwiredReturns503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/prices", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no SetPricing", rr.Code)
	}
}

func TestMethodNotAllowedOnReadRoute(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/requests", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}
