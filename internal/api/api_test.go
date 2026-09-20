package api

import (
	"bytes"
	"compress/gzip"
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

	"github.com/andybalholm/brotli"

	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/decode"
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
	ev := &store.Event{EventSummary: store.EventSummary{
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
		Status:         200}}
	if opts != nil {
		opts(ev)
	}
	id, _, err := st.InsertEvent(context.Background(), ev)
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
	handler.SetPricing(pricing.NewLoader(filepath.Join(t.TempDir(), "prices.toml"), nil))
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

// --- the list projections (br-GI-7-01) ------------------------------------

// assertNoBodyColumns checks both halves of the projection guarantee at once:
// the response is small, and it names none of the blob columns. The size
// bound is what makes it non-vacuous -- the fixtures below are sized so a
// single leaked body would blow far past it. The key list is the four
// header/body columns plus br-GI-7-06's two transcript columns, because
// EventSummary names none of the six and the list path must not read any of
// them (plan D1/F3.3).
func assertNoBodyColumns(t *testing.T, body []byte, what string) {
	t.Helper()
	if len(body) > 64*1024 {
		t.Errorf("%s response = %d bytes, want under 64 KB", what, len(body))
	}
	for _, key := range []string{
		"ReqBody", "RespBody", "ReqHeaders", "RespHeaders",
		"TranscriptContent", "TranscriptRole",
	} {
		if bytes.Contains(body, []byte(`"`+key+`"`)) {
			t.Errorf("%s response carries a %q key", what, key)
		}
	}
}

// seedFiftyWithBodies stores 50 rows each carrying a 1 MB request/response
// body and a 1 MB transcript reconstruction, which is the fixture both
// projection tests need: 50 MB stored on the wire columns and 50 MB on the
// transcript columns, so the 64 KB bound is a real assertion rather than a
// formality for either class.
func seedFiftyWithBodies(t *testing.T, st *store.Store, opts func(*store.Event)) {
	t.Helper()
	big := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 50; i++ {
		seedEvent(t, st, func(ev *store.Event) {
			ev.ReqBody, ev.RespBody = big, big
			ev.ReqHeaders = `{"authorization":["[redacted]"]}`
			ev.RespHeaders = `{"content-type":["application/json"]}`
			ev.TranscriptContent, ev.TranscriptRole = big, "assistant"
			if opts != nil {
				opts(ev)
			}
		})
	}
}

// TestListRouteOmitsBodies (T1): the Calls list is the dashboard's hottest
// fetch and renders none of the body columns -- the wire bodies or the
// transcript reconstruction.
func TestListRouteOmitsBodies(t *testing.T) {
	st := newTestStore(t)
	seedFiftyWithBodies(t, st, nil)
	handler, _, _, _ := newTestAPI(t, st)

	assertNoBodyColumns(t, getOK(t, handler, "/api/requests?limit=50").Body.Bytes(), "list")
}

// TestSessionRouteOmitsBodies (T2) asserts both halves for the session route.
// The length half is load-bearing: with Calls typed as EventSummary the
// "no body key" half is already guaranteed by the type checker and catches
// nothing new, so a non-empty calls[] is what stops an empty array satisfying
// the size bound vacuously.
func TestSessionRouteOmitsBodies(t *testing.T) {
	st := newTestStore(t)
	const sid = "sess_big"
	ctx := context.Background()
	seedFiftyWithBodies(t, st, func(ev *store.Event) { ev.SessionID = sid })
	// A session's own row is written separately from its events, so the route
	// 404s until both exist.
	if err := st.UpsertSession(ctx, sid, "", time.Now()); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := st.ReconcileSession(ctx, sid); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	body := getOK(t, handler, "/api/sessions/"+sid).Body.Bytes()
	assertNoBodyColumns(t, body, "session")

	var got struct {
		Calls []json.RawMessage `json:"calls"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if len(got.Calls) != 50 {
		t.Errorf("calls = %d, want 50", len(got.Calls))
	}
}

// --- the detail route's decoded response body (br-GI-7-03, T4) ------------

// brotliBody encodes data as a brotli stream. That is what a stored response
// body routinely is: Claude Code advertises "br" and the proxy tees the bytes
// unmodified, so the detail view cannot decode them in the browser --
// DecompressionStream has no br support and the no-build-step rule forbids
// bundling a library.
func brotliBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

// gzipBody encodes data as a gzip stream -- see TestDetailMarksCorruptTailBody
// for why the corrupt-tail case needs this rather than brotli.
func gzipBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// detailFor seeds one stored proxy row and serves its detail. wireCap is
// applied only when non-nil, so the unwired-seam case is expressible.
func detailFor(t *testing.T, body []byte, respHeaders string, wireCap *int) eventDetail {
	t.Helper()
	st := newTestStore(t)
	ev := seedEvent(t, st, func(e *store.Event) {
		e.RespBody = body
		e.RespHeaders = respHeaders
	})
	handler, _, _, _ := newTestAPI(t, st)
	if wireCap != nil {
		handler.SetBodyCapBytes(*wireCap)
	}
	path := "/api/requests/" + strconv.FormatInt(ev.ID, 10)
	return decodeJSON[eventDetail](t, getOK(t, handler, path).Body)
}

func intPtr(n int) *int { return &n }

func TestDetailDecodesResponseBody(t *testing.T) {
	plain := []byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`)
	got := detailFor(t, brotliBody(t, plain), `{"Content-Encoding":["br"]}`, intPtr(1<<20))

	if got.RespBodyCompleteness != decode.Complete {
		t.Errorf("Completeness = %v, want Complete", got.RespBodyCompleteness)
	}
	if !bytes.Equal(got.RespBodyDecoded, plain) {
		t.Errorf("RespBodyDecoded = %q, want the decoded body", got.RespBodyDecoded)
	}
	// The stored bytes are never rewritten: replay reads them straight from
	// the row, so a decode that wrote back would change what a replay sends.
	if !bytes.Equal(got.RespBody, brotliBody(t, plain)) {
		t.Error("RespBody was rewritten; decoding must be display-only")
	}
}

func TestDetailExactCapBodyIsComplete(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 128) // exactly 1024
	got := detailFor(t, brotliBody(t, payload), `{"Content-Encoding":["br"]}`, intPtr(1024))

	if got.RespBodyCompleteness != decode.Complete {
		t.Errorf("Completeness = %v, want Complete (the withdrawn len==cap test would say TruncatedAtCap)",
			got.RespBodyCompleteness)
	}
	if !bytes.Equal(got.RespBodyDecoded, payload) {
		t.Errorf("len(RespBodyDecoded) = %d, want %d", len(got.RespBodyDecoded), len(payload))
	}
}

func TestDetailMarksCapTruncatedBody(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 512)
	got := detailFor(t, brotliBody(t, payload), `{"Content-Encoding":["br"]}`, intPtr(1024))

	if got.RespBodyCompleteness != decode.TruncatedAtCap {
		t.Errorf("Completeness = %v, want TruncatedAtCap", got.RespBodyCompleteness)
	}
	if !bytes.Equal(got.RespBodyDecoded, payload[:1024]) {
		t.Error("RespBodyDecoded is not the decoded prefix")
	}
}

// detailRawFor is detailFor's raw-body twin: the same fixture, but the bytes
// the browser receives rather than the decoded eventDetail. The distinction is
// the whole point of the test below -- decoding into eventDetail compares the
// typed constant, which passes for any encoding at all.
func detailRawFor(t *testing.T, body []byte, respHeaders string, wireCap *int) []byte {
	t.Helper()
	st := newTestStore(t)
	ev := seedEvent(t, st, func(e *store.Event) {
		e.RespBody = body
		e.RespHeaders = respHeaders
	})
	handler, _, _, _ := newTestAPI(t, st)
	if wireCap != nil {
		handler.SetBodyCapBytes(*wireCap)
	}
	return getOK(t, handler, "/api/requests/"+strconv.FormatInt(ev.ID, 10)).Body.Bytes()
}

// TestDetailPinsCompletenessWireValue pins the *numeric* form of
// RespBodyCompleteness that app.js reads. app.js hard-codes
// COMPLETE=0 .. NOT_DECODED=3 (app.js:113-116) and compares them as integers in
// readPathMarker.
//
// Every other Go test decodes the response into eventDetail and compares the
// typed constant, which is enough for a reordered iota -- the constants move
// with the code and those tests fail. What none of them can see is the wire
// *spelling*, because they decode and encode through the same codec: a change
// that is self-consistent on the Go side (a paired Marshal/Unmarshal, or the
// type becoming a string) round-trips through every one of them and turns every
// marker branch in the browser false, rendering a truncated or corrupt body
// with no marker. That is the "absence indistinguishable from a failure" class
// this story exists to close, and this is the only test that reads the bytes
// the browser actually receives.
//
// All four values, not just the one a cap-truncated fixture happens to
// produce: pinning one integer leaves the other three free to drift, and they
// are most of the mapping the browser depends on.
func TestDetailPinsCompletenessWireValue(t *testing.T) {
	// The same payload TestDetailMarksCorruptTailBody uses, and deliberately:
	// the cut points below were read off the real decoder rather than assumed.
	// gzip emits incrementally, so half its stream still yields a prefix; a
	// heavily repetitive payload compresses so far that a naive half of a
	// *smaller* one lands on NotDecoded instead, which is how the first draft
	// of this table was wrong.
	payload := bytes.Repeat([]byte(`{"content":"a longer body "}`), 512)
	gz := gzipBody(t, payload)
	br := brotliBody(t, payload)

	cases := []struct {
		name string
		body []byte
		hdr  string
		cap  *int
		want int
	}{
		// A body with no Content-Encoding returns early and is Complete, so
		// the tool's ordinary case can never be labelled undecodable.
		{"Complete", []byte(`{"ok":true}`), `{"Content-Type":["application/json"]}`, intPtr(1024), 0},
		{"TruncatedAtCap", br, `{"Content-Encoding":["br"]}`, intPtr(1024), 1},
		{"PartialCorrupt", gz[:len(gz)/2], `{"Content-Encoding":["gzip"]}`, intPtr(1 << 20), 2},
		// brotli emits per meta-block, so a stream cut near its start decodes
		// to nothing at all. Cutting it *late* is worse than useless as a
		// fixture -- a prefix of these bytes is one byte short of the whole
		// stream and the reader reports Complete on the six bytes it got.
		{"NotDecoded", br[:10], `{"Content-Encoding":["br"]}`, intPtr(1 << 20), 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := detailRawFor(t, tc.body, tc.hdr, tc.cap)
			want := `"RespBodyCompleteness":` + strconv.Itoa(tc.want)
			if !bytes.Contains(raw, []byte(want)) {
				t.Errorf("detail does not carry %s -- app.js compares %d as an integer and would "+
					"draw no marker:\n%s", want, tc.want, raw)
			}
		})
	}
}

// TestDetailMarksCorruptTailBody uses gzip, not brotli, and that is not a
// convenience: brotli's reader emits per meta-block, so a stream cut mid-way
// decodes to *zero* bytes and lands on NotDecoded rather than here. gzip and
// zstd emit incrementally, so a truncated stream really does yield a clean
// prefix followed by a read error. Claude Code advertises all four codings, so
// both shapes reach a stored row.
func TestDetailMarksCorruptTailBody(t *testing.T) {
	payload := bytes.Repeat([]byte(`{"content":"a longer body "}`), 512)
	whole := gzipBody(t, payload)
	got := detailFor(t, whole[:len(whole)/2], `{"Content-Encoding":["gzip"]}`, intPtr(1<<20))

	if got.RespBodyCompleteness != decode.PartialCorrupt {
		t.Fatalf("Completeness = %v, want PartialCorrupt", got.RespBodyCompleteness)
	}
	if len(got.RespBodyDecoded) == 0 {
		t.Error("RespBodyDecoded is empty, want the decoded prefix")
	}
	if !bytes.Equal(got.RespBodyDecoded, payload[:len(got.RespBodyDecoded)]) {
		t.Error("RespBodyDecoded is not a prefix of the original")
	}
}

func TestDetailFallsBackToRawOnCorruptBody(t *testing.T) {
	raw := append([]byte{0xff, 0xff, 0xff, 0xff}, bytes.Repeat([]byte{0xff}, 32)...)
	got := detailFor(t, raw, `{"Content-Encoding":["br"]}`, intPtr(1<<20))

	if got.RespBodyCompleteness != decode.NotDecoded {
		t.Errorf("Completeness = %v, want NotDecoded", got.RespBodyCompleteness)
	}
	if !bytes.Equal(got.RespBodyDecoded, raw) {
		t.Error("RespBodyDecoded is not the raw body")
	}
	// A body we cannot read is a rendering state, not a server error.
	if got.BodyCapBytes != 1<<20 {
		t.Errorf("BodyCapBytes = %d, want the wired cap", got.BodyCapBytes)
	}
}

func TestDetailUnencodedBodyIsComplete(t *testing.T) {
	plain := []byte("data: {\"type\":\"message_start\"}\n\n")
	got := detailFor(t, plain, `{"Content-Type":["text/event-stream"]}`, intPtr(1<<20))

	if got.RespBodyCompleteness != decode.Complete {
		t.Errorf("Completeness = %v, want Complete -- a body that needs no decompression is not NotDecoded",
			got.RespBodyCompleteness)
	}
	if !bytes.Equal(got.RespBodyDecoded, plain) {
		t.Errorf("RespBodyDecoded = %q, want %q", got.RespBodyDecoded, plain)
	}
}

// TestDetailUnwiredCapServesRawBody is the guard against decode.Body's
// non-positive-limit error being surfaced as a decode failure: on a body that
// decodes fine, that would render "it would not decompress".
func TestDetailUnwiredCapServesRawBody(t *testing.T) {
	plain := []byte(`{"type":"message"}`)
	encoded := brotliBody(t, plain)
	hdr := `{"Content-Encoding":["br"]}`

	for _, tc := range []struct {
		name    string
		wireCap *int
	}{
		{"never wired", nil},
		{"wired to zero", intPtr(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detailFor(t, encoded, hdr, tc.wireCap)
			if got.BodyCapBytes != 0 {
				t.Errorf("BodyCapBytes = %d, want 0", got.BodyCapBytes)
			}
			if got.RespBodyCompleteness == decode.NotDecoded {
				t.Error("Completeness = NotDecoded; a missing cap must not manufacture a decode failure")
			}
			if !bytes.Equal(got.RespBodyDecoded, encoded) {
				t.Error("RespBodyDecoded is not the raw stored body")
			}
		})
	}
}
