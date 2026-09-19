package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
)

func TestGetPricesRendersShippedTable(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetPricing(pricing.NewLoader(filepath.Join(t.TempDir(), "prices.toml")))

	rr := getOK(t, handler, "/api/prices")
	got := decodeJSON[pricesResponse](t, rr.Body)
	if len(got.Models) == 0 {
		t.Fatal("got no models, want the shipped table rendered")
	}
	for _, m := range got.Models {
		if m.Source == "" {
			t.Errorf("model %s has no Source label", m.Model)
		}
	}
}

func TestGetPricesReflectsUserOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.toml")
	if err := pricing.SetOverride(path, pricing.Rate{Model: "claude-custom-1"}); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetPricing(pricing.NewLoader(path))

	got := decodeJSON[pricesResponse](t, getOK(t, handler, "/api/prices").Body)
	found := false
	for _, m := range got.Models {
		if m.Model == "claude-custom-1" {
			found = true
			if m.Source != "user" {
				t.Errorf("Source = %q, want %q", m.Source, "user")
			}
		}
	}
	if !found {
		t.Fatal("override model claude-custom-1 not present in the rendered table")
	}
}

func TestPricesRouteMethodNotAllowed(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetPricing(pricing.NewLoader(filepath.Join(t.TempDir(), "prices.toml")))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/prices", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (POST /api/prices is a later slice)", rr.Code)
	}
}
