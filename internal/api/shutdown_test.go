package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestShutdownTriggersTheStopFunc: a loopback caller gets a written response,
// and the injected func runs after it -- checked by recording the response
// synchronously (writeJSON happens before the func's own goroutine is even
// started) and only then waiting for the func's signal.
func TestShutdownTriggersTheStopFunc(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	called := make(chan struct{}, 1)
	handler.SetShutdown(func() { called <- struct{}{} })

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"shutting_down":true`) {
		t.Fatalf("response does not confirm shutdown: %s", rr.Body.String())
	}

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("the stop func was not called within 1s of the response being written")
	}
}

// TestShutdownRefusesANonLoopbackCaller: a loopback Host header (so
// originReject alone would pass this) with a public RemoteAddr is the case a
// forged Host: 127.0.0.1 from an off-box caller would present -- refused.
func TestShutdownRefusesANonLoopbackCaller(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	calls := 0
	handler.SetShutdown(func() { calls++ })

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/shutdown", nil)
	req.RemoteAddr = "203.0.113.5:9999"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if calls != 0 {
		t.Fatalf("a non-loopback caller triggered %d shutdown(s)", calls)
	}
}

// TestShutdownRefusesANonLoopbackHost is the DNS-rebinding case originReject
// covers, kept so the two guards cannot be collapsed into one.
func TestShutdownRefusesANonLoopbackHost(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	calls := 0
	handler.SetShutdown(func() { calls++ })

	req := httptest.NewRequest(http.MethodPost, "http://lens.example/api/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if calls != 0 {
		t.Fatalf("a non-loopback Host triggered %d shutdown(s)", calls)
	}
}

// TestShutdownRefusesNonPost: the catch-all "/" route means a GET falls
// through to the file server and gets a 404, never the mux's own 405 --
// mirroring ingest_test.go's TestIngestIsNotReachableByGet. Asserted as "not
// 200" rather than pinned to 404 for the same reason that one is.
func TestShutdownRefusesNonPost(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	calls := 0
	handler.SetShutdown(func() { calls++ })

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8798/api/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("GET /api/shutdown answered 200")
	}
	if calls != 0 {
		t.Fatalf("GET /api/shutdown triggered %d shutdown(s)", calls)
	}
}

// TestShutdownWithoutASeamIsNotFatal: with no SetShutdown wired, the route
// refuses (503) rather than panicking on a nil func.
func TestShutdownWithoutASeamIsNotFatal(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}
