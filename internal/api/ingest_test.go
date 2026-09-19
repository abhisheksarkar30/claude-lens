package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTriggerIngestCallsTheStub is the bead's POST clause.
func TestTriggerIngestCallsTheStub(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	calls := 0
	handler.SetIngestTrigger(func(context.Context) error { calls++; return nil })

	rr := postOrigin(t, handler, "/api/ingest", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/ingest: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if calls != 1 {
		t.Fatalf("the ingest trigger ran %d time(s), want 1", calls)
	}
	if !strings.Contains(rr.Body.String(), `"triggered":true`) {
		t.Fatalf("the response does not confirm the run: %s", rr.Body.String())
	}
}

// TestTriggerIngestUnwiredAnswers503: with no runner there is nothing to run,
// and a 200 would read as "collection just happened".
func TestTriggerIngestUnwiredAnswers503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := postOrigin(t, handler, "/api/ingest", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

// TestTriggerIngestRejectsAForeignOrigin: this route makes outbound calls with
// whatever credentials are stored, so it is the one most worth guarding.
func TestTriggerIngestRejectsAForeignOrigin(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	calls := 0
	handler.SetIngestTrigger(func(context.Context) error { calls++; return nil })

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/ingest", nil)
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status = %d, want 403", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("a cross-origin request triggered %d collector run(s)", calls)
	}
}

// TestTriggerIngestRejectsANonLoopbackHost covers the other half of the
// allowlist, with no Origin header at all.
func TestTriggerIngestRejectsANonLoopbackHost(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	calls := 0
	handler.SetIngestTrigger(func(context.Context) error { calls++; return nil })

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://lens.example/api/ingest", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host: status = %d, want 403", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("a non-loopback request triggered %d collector run(s)", calls)
	}
}

// TestTriggerIngestReportsAPartialFailure: every collector already ran, so the
// route reports the first failure rather than a bare success -- the tab then
// re-reads /api/sources for the detail.
func TestTriggerIngestReportsAPartialFailure(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetIngestTrigger(func(context.Context) error {
		return errors.New("snapshot/quota: 401 unauthorized")
	})

	rr := postOrigin(t, handler, "/api/ingest", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "snapshot") {
		t.Fatalf("the failing source was not named: %s", rr.Body.String())
	}
}

// TestIngestIsNotReachableByGet: the route is a POST and only a POST; a GET
// would be a link a browser could be talked into following.
func TestIngestIsNotReachableByGet(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	calls := 0
	handler.SetIngestTrigger(func(context.Context) error { calls++; return nil })

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ingest", nil))
	if rr.Code == http.StatusOK {
		t.Fatalf("GET /api/ingest answered 200")
	}
	if calls != 0 {
		t.Fatalf("GET /api/ingest triggered %d collector run(s)", calls)
	}
}
