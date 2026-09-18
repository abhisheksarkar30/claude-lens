package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLivePopulatesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5","max_input_tokens":200000,"max_output_tokens":64000}
		]}`))
	}))
	defer srv.Close()

	entries := Fetch(context.Background(), srv.Client(), srv.URL, "test-key")
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ModelID != "claude-sonnet-5" || e.DisplayName != "Claude Sonnet 5" {
		t.Errorf("entry = %+v, unexpected", e)
	}
	if e.MaxInputTokens != 200000 || e.MaxOutputTokens != 64000 {
		t.Errorf("caps = %+v, want 200000/64000", e)
	}
	if e.Source != "live" {
		t.Errorf("Source = %q, want live", e.Source)
	}
}

func TestFetchUnreachableFallsBackToShipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	srv.Close() // close immediately so the request is genuinely unreachable

	entries := Fetch(context.Background(), srv.Client(), srv.URL, "")
	if len(entries) == 0 {
		t.Fatal("entries empty, want the shipped fallback")
	}
	for _, e := range entries {
		if e.Source != "shipped" {
			t.Errorf("entry %s: Source = %q, want shipped", e.ModelID, e.Source)
		}
	}

	shipped := ShippedCatalog()
	if len(entries) != len(shipped) {
		t.Errorf("fallback entries = %d, want %d (ShippedCatalog)", len(entries), len(shipped))
	}
}

func TestFetchNon200FallsBackToShipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	entries := Fetch(context.Background(), srv.Client(), srv.URL, "")
	if len(entries) != len(ShippedCatalog()) {
		t.Errorf("entries = %d, want the shipped fallback (%d)", len(entries), len(ShippedCatalog()))
	}
}
