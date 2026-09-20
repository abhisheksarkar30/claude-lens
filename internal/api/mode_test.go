package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestModeBadgeGrid is T7. The six-row grid is the whole product of the badge,
// so it is asserted behaviourally -- through the route, from a fixture seam --
// rather than against a string constant nothing produces. The mapping lives
// server-side precisely so this test can exist: app.js has no runtime in this
// toolchain, so a mapping there would be checkable only by a source-shape
// assertion.
func TestModeBadgeGrid(t *testing.T) {
	tests := []struct {
		configured ProxyConfigured
		observed   bool
		want       string
	}{
		{ProxyConfiguredMatch, true, "proxy: active"},
		{ProxyConfiguredMatch, false, "proxy: configured, not receiving"},
		{ProxyConfiguredMismatch, true, "proxy: receiving, client elsewhere"},
		{ProxyConfiguredMismatch, false, "proxy: off"},
		{ProxyConfiguredUnknown, true, "proxy: receiving — base URL not set in settings.json"},
		{ProxyConfiguredUnknown, false, "proxy: not receiving — base URL not set in settings.json"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			handler, _, _, _ := newTestAPI(t, newTestStore(t))
			handler.SetProxyMode(func(context.Context) (ProxyMode, error) {
				return ProxyMode{Configured: tc.configured, Observed: tc.observed}, nil
			})

			got := decodeJSON[ProxyMode](t, getOK(t, handler, "/api/mode").Body)
			if got.Badge != tc.want {
				t.Errorf("Badge = %q, want %q", got.Badge, tc.want)
			}
			// The label must agree with the facts it was rendered from: the
			// handler fills it, so a seam cannot report one state and ship
			// another state's label.
			if got.Configured != tc.configured || got.Observed != tc.observed {
				t.Errorf("state = (%v, %v), want (%v, %v)",
					got.Configured, got.Observed, tc.configured, tc.observed)
			}
		})
	}
}

// TestModeUnknownNeverSaysClientElsewhere pins the one wrong answer the
// three-valued configured state exists to avoid. A user who followed the
// `clens serve` banner exports ANTHROPIC_BASE_URL in their shell, which
// settings.json never sees; that is unknown, not a mismatch, and telling them
// their client is pointed elsewhere would be a false accusation on the
// onboarding path.
func TestModeUnknownNeverSaysClientElsewhere(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))

	for _, observed := range []bool{true, false} {
		handler.SetProxyMode(func(context.Context) (ProxyMode, error) {
			return ProxyMode{Configured: ProxyConfiguredUnknown, Observed: observed}, nil
		})
		body := getOK(t, handler, "/api/mode").Body.String()

		if strings.Contains(body, "client elsewhere") {
			t.Errorf("observed=%v: the unknown state read 'client elsewhere': %s", observed, body)
		}
		if !strings.Contains(body, "not set in settings.json") {
			t.Errorf("observed=%v: the label does not name the missing setting: %s", observed, body)
		}
	}
}

// TestModeRouteUnwiredReturns503: an empty badge and a broken one look
// identical in a UI, so an unwired seam is an error rather than a blank -- the
// same refusal SetSourceHealth makes for an empty source list.
func TestModeRouteUnwiredReturns503(t *testing.T) {
	handler, _, _, _ := newTestAPI(t, newTestStore(t))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/mode", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on an unwired seam", rr.Code)
	}
}
