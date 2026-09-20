package api

import (
	"context"
	"net/http"
)

// ProxyConfigured is the three-valued "configured" half of the mode signal:
// does Claude Code's ANTHROPIC_BASE_URL point at this process's ProxyAddr?
//
// Three-valued rather than a bool because "no ANTHROPIC_BASE_URL in
// settings.json" is a real and ordinary state -- the printed-banner
// onboarding path tells the user to export the variable in their shell, which
// settings.json never sees. Reporting that as a mismatch would accuse a
// correctly-pointed client.
type ProxyConfigured int

const (
	ProxyConfiguredMatch ProxyConfigured = iota
	ProxyConfiguredMismatch
	ProxyConfiguredUnknown
)

// ProxyMode is what GET /api/mode returns: the two independent facts and the
// label they render to.
//
// Neither half alone catches the failure this exists for. A configured-only
// indicator says "pointed correctly" while the proxy is dead; an observed-only
// indicator says "capturing" while the client is pointed elsewhere. The
// interesting state is the disagreement.
type ProxyMode struct {
	Configured ProxyConfigured `json:"Configured"`
	Observed   bool            `json:"Observed"`

	// Badge is the rendered label. It is filled by the handler from the two
	// facts above, not by the injected seam, so a seam cannot hand the browser
	// a label that disagrees with the state it reported. Keeping the mapping
	// in Go is also what makes it testable: there is no JS runtime in this
	// toolchain, so a mapping in app.js could only be asserted by a
	// source-shape check, and the browser's whole job becomes printing the
	// string it is handed.
	Badge string `json:"Badge"`
}

// badgeFor is the six-row grid, and the reason the mapping lives server-side.
//
// "client elsewhere" is reserved for a definite mismatch. A user who followed
// the `clens serve` banner exports ANTHROPIC_BASE_URL in their shell, which
// settings.json never sees; that case is Unknown and must never read "client
// elsewhere". `clens doctor` cannot tell them apart -- its own doc comment
// says the check "can only PASS" -- which is how the incident this badge
// exists for went unnoticed.
func badgeFor(c ProxyConfigured, observed bool) string {
	switch c {
	case ProxyConfiguredMatch:
		if observed {
			return "proxy: active"
		}
		return "proxy: configured, not receiving"
	case ProxyConfiguredMismatch:
		if observed {
			return "proxy: receiving, client elsewhere"
		}
		return "proxy: off"
	default:
		if observed {
			return "proxy: receiving — base URL not set in settings.json"
		}
		return "proxy: not receiving — base URL not set in settings.json"
	}
}

// SetProxyMode wires GET /api/mode to fn.
//
// An unwired seam answers 503 rather than an empty badge, because an empty
// badge and a broken one look identical in a UI -- the same reason
// SetSourceHealth refuses an empty list.
func (a *api) SetProxyMode(fn func(ctx context.Context) (ProxyMode, error)) {
	a.proxyMode = fn
}

// mode is GET /api/mode: the dashboard header badge's whole content.
func (a *api) mode(w http.ResponseWriter, r *http.Request) {
	if a.proxyMode == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy mode is unavailable: no mode reader is wired")
		return
	}
	m, err := a.proxyMode(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.Badge = badgeFor(m.Configured, m.Observed)
	writeJSON(w, http.StatusOK, m)
}
