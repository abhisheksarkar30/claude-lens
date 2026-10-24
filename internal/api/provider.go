package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
)

// ProviderSeam is the dashboard's view of the proxy's live upstream. The
// composition root builds it from proxy.Targets, so this package never
// imports proxy, config or net/url: Get returns plain values, and the two
// hosts are bare host[:port] strings, never the configured URLs, so the page
// shows where traffic is headed without echoing an upstream URL back.
type ProviderSeam struct {
	Get func() (active, cloudHost, localHost string, localConfigured bool)
	Set func(active string) error
}

// SetProvider wires /api/provider. Leaving it unset is supported: both
// methods answer 503 rather than calling a nil func.
func (a *api) SetProvider(seam ProviderSeam) { a.provider = seam }

// getProvider is GET /api/provider: the header toggle's whole content.
func (a *api) getProvider(w http.ResponseWriter, r *http.Request) {
	if a.provider.Get == nil {
		writeError(w, http.StatusServiceUnavailable, "provider is unavailable: no provider reader is wired")
		return
	}
	active, cloudHost, localHost, localConfigured := a.provider.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":           active,
		"cloud_url_host":   cloudHost,
		"local_url_host":   localHost,
		"local_configured": localConfigured,
	})
}

// setProvider is POST /api/provider, guarded exactly like shutdown and
// reload: originReject for the DNS-rebinding case and a loopback caller for
// the forged-Host case. Switching provider redirects where the caller's
// credentials are sent, so --allow-remote widens neither guard. A value the
// seam refuses (unknown, or "local" with no local URL) is a 400: the caller
// and origin were fine, the value was not.
func (a *api) setProvider(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "provider"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackHost(host) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("provider requires a loopback caller, got %q", r.RemoteAddr))
		return
	}
	if a.provider.Set == nil {
		writeError(w, http.StatusServiceUnavailable, "provider is unavailable: no provider writer is wired")
		return
	}
	var body struct {
		Active string `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := a.provider.Set(body.Active); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": body.Active})
}
