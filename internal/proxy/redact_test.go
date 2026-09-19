package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestRedactHeadersBasics(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-oat01-abc")
	h.Set("Cookie", "sessionKey=abc123")
	h.Set("Sessionkey", "abc123")
	h.Set("Content-Type", "application/json")

	got := redactHeaders(h)
	for _, name := range []string{"Authorization", "Cookie", "Sessionkey"} {
		if got.Get(name) != redactedValue {
			t.Errorf("%s = %q, want %q", name, got.Get(name), redactedValue)
		}
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type was redacted: %q", got.Get("Content-Type"))
	}
	// Original header must be untouched.
	if h.Get("Authorization") != "Bearer sk-ant-oat01-abc" {
		t.Fatal("redactHeaders mutated the original header")
	}
}

// TestRedactHeadersMultiValued is test 19: a second x-api-key value must
// not slip through unredacted just because Header.Get only ever sees the
// first.
func TestRedactHeadersMultiValued(t *testing.T) {
	h := http.Header{}
	h.Add("X-Api-Key", "sk-ant-api03-first")
	h.Add("X-Api-Key", "sk-ant-api03-second")

	got := redactHeaders(h)
	values := got.Values("X-Api-Key")
	for _, v := range values {
		if v != redactedValue {
			t.Fatalf("X-Api-Key values = %v, want every value redacted", values)
		}
	}
}

func TestRedactHeadersAdminKeyAnyHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-Custom-Debug", "leaked sk-ant-admin-placeholder in a debug header")

	got := redactHeaders(h)
	if got.Get("X-Custom-Debug") != redactedValue {
		t.Errorf("X-Custom-Debug = %q, want redacted (admin key pattern present)", got.Get("X-Custom-Debug"))
	}
}

func TestRedactCheckCatchesLeaks(t *testing.T) {
	leaky, _ := json.Marshal(map[string][]string{
		"X-Api-Key": {"sk-ant-api03-placeholder"},
	})
	if err := RedactCheck(leaky); err == nil {
		t.Fatal("RedactCheck: want error for an unredacted x-api-key")
	}

	adminLeak, _ := json.Marshal(map[string][]string{
		"X-Weird-Header": {"sk-ant-admin-placeholder"},
	})
	if err := RedactCheck(adminLeak); err == nil {
		t.Fatal("RedactCheck: want error for an unredacted admin key under any header")
	}

	clean, _ := json.Marshal(map[string][]string{
		"X-Api-Key":    {redactedValue},
		"Content-Type": {"application/json"},
		"Sessionkey":   {redactedValue},
	})
	if err := RedactCheck(clean); err != nil {
		t.Fatalf("RedactCheck: unexpected error on clean headers: %v", err)
	}
}

func TestOriginAllowed(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"", true},
		{"http://localhost:3000", true},
		{"http://127.0.0.1:5173", true},
		{"https://evil.example.com", false},
	}
	for _, tt := range tests {
		r, _ := http.NewRequest("POST", "http://127.0.0.1:8797/api/x", nil)
		if tt.origin != "" {
			r.Header.Set("Origin", tt.origin)
		}
		if got := OriginAllowed(r); got != tt.want {
			t.Errorf("OriginAllowed(Origin=%q) = %v, want %v", tt.origin, got, tt.want)
		}
	}
}
