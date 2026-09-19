package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// postPrices sends a POST /api/prices body and returns the recorder. The Host
// is loopback and no Origin is set, which is the CLI's path through
// originReject -- the guard's own cases are covered in replay_test.go.
func postPrices(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/prices", strings.NewReader(body))
	req.Host = "127.0.0.1:8798"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// An unwired seam is a supported state, not a nil dereference: the route that
// consumes it answers 503. This is the same contract every write seam carries.
func TestSetPricesWithoutLoaderIs503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st) // SetPricing deliberately not called

	rr := postPrices(t, handler, `{"model":"claude-custom-1","input_rate":0.000003}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

func TestSetPricesWritesOverrideAndRendersIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.toml")
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetPricing(pricing.NewLoader(path))

	rr := postPrices(t, handler, `{"model":"claude-custom-1","input_rate":0.000003,"output_rate":0.000015}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	// The response is re-rendered from the table the Loader now reads, not from
	// what was sent -- so a write that silently failed to persist would show up
	// here as a missing model rather than as a green 200.
	got := decodeJSON[pricesResponse](t, rr.Body)
	var row *priceModel
	for i, m := range got.Models {
		if m.Model == "claude-custom-1" {
			row = &got.Models[i]
		}
	}
	if row == nil {
		t.Fatal("claude-custom-1 not in the response table after a successful write")
	}
	if row.Source != "user" {
		t.Errorf("Source = %q, want %q: a written override always outranks the shipped row", row.Source, "user")
	}
	if row.InputRate == nil || *row.InputRate != 0.000003 {
		t.Errorf("InputRate = %v, want 0.000003", row.InputRate)
	}
	if row.CacheReadRate != nil {
		t.Errorf("CacheReadRate = %v, want nil (omitted means unset, not 0)", *row.CacheReadRate)
	}

	// And the write is on disk, in the file the Loader is watching -- a fresh
	// Loader over the same path sees it too.
	fresh := pricing.NewLoader(path)
	if _, ok := fresh.Table()["claude-custom-1"]; !ok {
		t.Error("the override is not in the file a fresh Loader reads")
	}
}

// A rate the invoicing invariant has no shape for is a 400 before anything is
// written: a negative rate would make a call cost negative.
func TestSetPricesRejectsNegativeAndNonFiniteRates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.toml")
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetPricing(pricing.NewLoader(path))

	for _, body := range []string{
		`{"model":"claude-custom-1","input_rate":-1}`,
		`{"model":"claude-custom-1","input_rate":1e999}`, // decodes to +Inf
	} {
		rr := postPrices(t, handler, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400", body, rr.Code)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a rejected rate still wrote the override file")
	}
}

func TestSetPricesRejectsMalformedBodies(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))
	handler.SetPricing(pricing.NewLoader(filepath.Join(t.TempDir(), "prices.toml")))

	for _, tc := range []struct{ name, body string }{
		// A typo'd key must be named, not silently dropped: a dropped rate
		// would leave the model priced at the shipped rate while the UI
		// showed the number the user typed.
		{"unknown field", `{"model":"m","inpt_rate":1}`},
		{"not json", `{`},
		{"no model", `{"input_rate":1}`},
	} {
		rr := postPrices(t, handler, tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, rr.Code, rr.Body.String())
		}
	}
}
