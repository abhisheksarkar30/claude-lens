package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func providerReq(method, url, remote, body string) *http.Request {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.RemoteAddr = remote
	return req
}

func TestGetProviderHappyPath(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetProvider(ProviderSeam{Get: func() (string, string, string, bool) {
		return "cloud", "api.anthropic.com", "127.0.0.1:1234", true
	}})

	got := decodeJSON[map[string]any](t, getOK(t, handler, "/api/provider").Body)
	want := map[string]any{
		"active": "cloud", "cloud_url_host": "api.anthropic.com",
		"local_url_host": "127.0.0.1:1234", "local_configured": true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("response carries %d fields, want exactly %d: %v", len(got), len(want), got)
	}
}

func TestProviderWithoutASeamIsNotFatal(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	for _, req := range []*http.Request{
		providerReq(http.MethodGet, "http://127.0.0.1:8798/api/provider", "127.0.0.1:54321", ""),
		providerReq(http.MethodPost, "http://127.0.0.1:8798/api/provider", "127.0.0.1:54321", `{"active":"cloud"}`),
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503: %s", req.Method, rr.Code, rr.Body.String())
		}
	}
}

func TestSetProviderSetsAndRejectsBadValue(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	var set string
	handler.SetProvider(ProviderSeam{Set: func(active string) error {
		if active != "local" && active != "cloud" {
			return errors.New("unknown provider " + active)
		}
		set = active
		return nil
	}})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, providerReq(http.MethodPost, "http://127.0.0.1:8798/api/provider", "127.0.0.1:54321", `{"active":"local"}`))
	if rr.Code != http.StatusOK || set != "local" {
		t.Fatalf("status = %d, set = %q, want 200 and local: %s", rr.Code, set, rr.Body.String())
	}

	for _, body := range []string{`{"active":"bogus"}`, `not json`} {
		rr = httptest.NewRecorder()
		handler.ServeHTTP(rr, providerReq(http.MethodPost, "http://127.0.0.1:8798/api/provider", "127.0.0.1:54321", body))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400: %s", body, rr.Code, rr.Body.String())
		}
	}
	if set != "local" {
		t.Errorf("a rejected value changed the provider to %q", set)
	}
}

// Both guards are asserted separately so they cannot be collapsed into one:
// a forged loopback Host from an off-box caller, and a rebinding page whose
// own hostname resolves to loopback.
func TestProviderEndpointRejectsNonLoopbackAndForeignOrigin(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	calls := 0
	handler.SetProvider(ProviderSeam{Set: func(string) error { calls++; return nil }})

	for name, req := range map[string]*http.Request{
		"public caller, loopback Host": providerReq(http.MethodPost, "http://127.0.0.1:8798/api/provider", "203.0.113.5:9999", `{"active":"local"}`),
		"loopback caller, public Host": providerReq(http.MethodPost, "http://lens.example/api/provider", "127.0.0.1:54321", `{"active":"local"}`),
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403: %s", name, rr.Code, rr.Body.String())
		}
	}
	if calls != 0 {
		t.Fatalf("a guarded caller reached the seam %d time(s)", calls)
	}
}

// Unlike shutdown, this path has a GET sibling: the read and the write are
// the same path with different methods, and each must reach its own handler.
func TestSetProviderIsNotReachableByGet(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	sets := 0
	handler.SetProvider(ProviderSeam{
		Get: func() (string, string, string, bool) { return "cloud", "", "", false },
		Set: func(string) error { sets++; return nil },
	})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, providerReq(http.MethodGet, "http://127.0.0.1:8798/api/provider?active=local", "127.0.0.1:54321", ""))
	if rr.Code != http.StatusOK || sets != 0 {
		t.Fatalf("GET: status = %d, sets = %d, want the read handler and no write", rr.Code, sets)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, providerReq(http.MethodPost, "http://127.0.0.1:8798/api/provider", "127.0.0.1:54321", `{"active":"local"}`))
	if rr.Code != http.StatusOK || sets != 1 {
		t.Fatalf("POST: status = %d, sets = %d, want the write handler", rr.Code, sets)
	}
}
