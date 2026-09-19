package api

import (
	"net/http/httptest"
	"testing"
)

func TestOriginRejectAllowsLoopbackWithNoOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1:9119/api/prices", nil)
	if reason := originReject(r, "prices"); reason != "" {
		t.Fatalf("got rejection %q, want none (no Origin means a non-browser client)", reason)
	}
}

func TestOriginRejectAllowsSameOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost:9119/api/prices", nil)
	r.Header.Set("Origin", "http://localhost:9119")
	if reason := originReject(r, "prices"); reason != "" {
		t.Fatalf("got rejection %q, want none (same-origin browser request)", reason)
	}
}

func TestOriginRejectRejectsForeignOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost:9119/api/prices", nil)
	r.Header.Set("Origin", "http://evil.example:9119")
	if reason := originReject(r, "prices"); reason == "" {
		t.Fatal("got no rejection, want one for a foreign Origin")
	}
}

func TestOriginRejectRejectsNonLoopbackHost(t *testing.T) {
	r := httptest.NewRequest("POST", "http://example.com/api/prices", nil)
	if reason := originReject(r, "prices"); reason == "" {
		t.Fatal("got no rejection, want one for a non-loopback Host")
	}
}
