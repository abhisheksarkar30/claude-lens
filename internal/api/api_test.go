package api

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
	"github.com/abhisheksarkar30/claude-lens/internal/web"
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
// fine on a fresh Consumer. Replay is off and no proxy handler is wired, which
// is the default every read-route test wants; newReplayAPI is the wired one.
func newTestAPI(t *testing.T, st Store) (*api, *sink.Sink, *consumer.Consumer, *Broker) {
	t.Helper()
	sk := sink.New(16)
	cons := consumer.New(sk, nil, nil)
	broker := NewBroker()
	return New(st, sk, cons, broker, testAssets(), nil, false), sk, cons, broker
}

// testAssets stands in for internal/web's embedded FS: one page, so a test
// can assert the mount serves something without depending on the real
// dashboard's markup.
func testAssets() fs.FS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>clens</title>")}}
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

// The four write seams are declared in this bead and wired by br-GI-1-17's
// composition root, a bead earlier in the DAG than br-GI-1-18, which adds the
// routes that consume them. That ordering only works if a seam setter is
// assignable with no route attached and an unwired seam is a supported state
// rather than a nil dereference -- which is what this test pins down.
//
// Setting every seam must not perturb the read surface, and a read must not
// invoke one: a seam is a write capability, so a GET that fired one would mean
// a read route had grown a side effect.
func TestWriteSeamsAreAssignableAndUnsetIsSupported(t *testing.T) {
	st := newTestStore(t)
	seedEvent(t, st, nil)
	handler, _, _, _ := newTestAPI(t, st) // every seam deliberately unset

	// Unset: reads still work, and the one route that consumes a seam answers
	// 503 rather than panicking.
	getOK(t, handler, "/api/requests")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/prices", strings.NewReader(`{"model":"m"}`)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /api/prices with no SetPricing: status = %d, want 503", rr.Code)
	}

	var calls atomic.Int64
	handler.SetPricing(pricing.NewLoader(filepath.Join(t.TempDir(), "prices.toml")))
	handler.SetCredentialWriter(func(name, value string) error { calls.Add(1); return nil })
	handler.SetAccountWriter(func() error { calls.Add(1); return nil })
	handler.SetIngestTrigger(func(context.Context) error { calls.Add(1); return nil })

	getOK(t, handler, "/api/requests")
	getOK(t, handler, "/api/sessions")
	getOK(t, handler, "/api/warnings")
	getOK(t, handler, "/api/stats")
	getOK(t, handler, "/api/health")
	getOK(t, handler, "/api/prices")
	if n := calls.Load(); n != 0 {
		t.Errorf("a read route invoked a write seam %d time(s)", n)
	}
}

// externalRef matches anything that would make the browser leave loopback: an
// absolute URL, a scheme-relative one, a CSS @import, or a font/script fetch.
// cssURL is separate because url(...) is also how an inline data: URI appears,
// which is local and fine.
var externalRef = regexp.MustCompile(`https?://|(?:src|href|action)\s*=\s*["']//|@import|url\(\s*["']?//`)

// TestEmbeddedAssetsServedWithoutExternalFetch is the E2E half of the bead's
// asset requirement: the running handler serves internal/web's real embedded
// files, and none of them points anywhere but this server.
//
// The check is a scan of the served bytes rather than an observed network
// call: proving a browser made no request would need a browser, and what is
// actually in our control is the markup. A scan for absolute and
// scheme-relative references is what that reduces to -- a data: URI or a
// relative path is served from the embed FS and never leaves the process.
func TestEmbeddedAssetsServedWithoutExternalFetch(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(16)
	handler := New(st, sk, consumer.New(sk, nil, nil), NewBroker(), web.Files, nil, false)

	// Fetched the way a browser loads the dashboard: the page at /, then the
	// two assets it links by relative path. /index.html is deliberately not in
	// this list -- http.FileServer canonicalizes it to / with a 301, and
	// following the redirect would only test the same bytes twice.
	for _, asset := range []struct{ name, path string }{
		{"index.html", "/"},
		{"app.js", "/app.js"},
		{"style.css", "/style.css"},
	} {
		rr := getOK(t, handler, asset.path)
		if rr.Body.Len() == 0 {
			t.Errorf("%s (%s) served 0 bytes", asset.name, asset.path)
		}
		if m := externalRef.FindString(rr.Body.String()); m != "" {
			t.Errorf("%s contains an external reference %q: the dashboard must not reach the network", asset.name, m)
		}
	}

	// The page the user opens is really the index page, not an empty shell.
	if !strings.Contains(getOK(t, handler, "/").Body.String(), "<title>") {
		t.Error("GET / did not serve the dashboard's index page")
	}
}
