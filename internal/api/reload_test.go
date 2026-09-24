package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reloadReq(host, remote string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/reload", nil)
	req.RemoteAddr = remote
	return req
}

func TestReloadReturnsTheReport(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetReload(func(context.Context) (ReloadReport, error) {
		return ReloadReport{Applied: []string{"Accounts"}, RestartRequired: []string{"ProxyAddr"}}, nil
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, reloadReq("127.0.0.1:8798", "127.0.0.1:54321"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[ReloadReport](t, rr.Body)
	if len(got.Applied) != 1 || got.Applied[0] != "Accounts" || len(got.RestartRequired) != 1 || got.Unchanged {
		t.Fatalf("report = %+v", got)
	}
}

func TestReloadNilSlicesSerializeAsEmptyArrays(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetReload(func(context.Context) (ReloadReport, error) { return ReloadReport{Unchanged: true}, nil })
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, reloadReq("127.0.0.1:8798", "127.0.0.1:54321"))
	if body := rr.Body.String(); !strings.Contains(body, `"applied":[]`) || !strings.Contains(body, `"restart_required":[]`) {
		t.Fatalf("nil slices must be [] not null: %s", body)
	}
}

func TestReloadInvalidConfigIs400(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetReload(func(context.Context) (ReloadReport, error) {
		return ReloadReport{}, errors.New("RetentionDays: must not be negative")
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, reloadReq("127.0.0.1:8798", "127.0.0.1:54321"))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "RetentionDays") {
		t.Fatalf("status = %d body = %s, want 400 carrying the validation error", rr.Code, rr.Body.String())
	}
}

// Both guards, kept separate exactly as shutdown's are.
func TestReloadGuards(t *testing.T) {
	for name, req := range map[string]*http.Request{
		"non-loopback caller": reloadReq("127.0.0.1:8798", "203.0.113.5:9999"),
		"non-loopback host":   reloadReq("lens.example", "127.0.0.1:54321"),
	} {
		t.Run(name, func(t *testing.T) {
			handler, _, _, _ := newTestAPI(t, newTestStore(t))
			calls := 0
			handler.SetReload(func(context.Context) (ReloadReport, error) { calls++; return ReloadReport{}, nil })
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden || calls != 0 {
				t.Fatalf("status = %d, calls = %d, want 403 and no reload", rr.Code, calls)
			}
		})
	}
}

func TestReloadBadOriginIs403(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetReload(func(context.Context) (ReloadReport, error) { return ReloadReport{}, nil })
	req := reloadReq("127.0.0.1:8798", "127.0.0.1:54321")
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestReloadUnwiredIs503AndGetIsNotServed(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, reloadReq("127.0.0.1:8798", "127.0.0.1:54321"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired status = %d, want 503", rr.Code)
	}
	get := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8798/api/reload", nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, get)
	if rr.Code == http.StatusOK {
		t.Fatal("GET /api/reload answered 200")
	}
}
