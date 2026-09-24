package api

import (
	"context"
	"net"
	"net/http"
)

// ReloadReport is what POST /api/reload answers with. Applied names the
// settings that took effect live; RestartRequired names those that differ from
// what the process booted with but cannot change without rebinding a listener.
type ReloadReport struct {
	Applied         []string `json:"applied"`
	RestartRequired []string `json:"restart_required"`
	Unchanged       bool     `json:"unchanged"`
}

// SetReload wires POST /api/reload to fn, which re-reads the config and applies
// the live-safe subset. Leaving it unset is supported: the route answers 503.
func (a *api) SetReload(fn func(context.Context) (ReloadReport, error)) { a.reloadFunc = fn }

// reload is POST /api/reload, guarded exactly like shutdown -- originReject
// for the DNS-rebinding case and a loopback caller for the forged-Host case --
// because it changes what a running process does. It is all-or-nothing: an
// invalid config changes nothing and is a 400 carrying the validation error.
func (a *api) reload(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "reload"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackHost(host) {
		writeError(w, http.StatusForbidden, "reload requires a loopback caller, got "+r.RemoteAddr)
		return
	}
	if a.reloadFunc == nil {
		writeError(w, http.StatusServiceUnavailable, "reload is unavailable: no reload func is wired")
		return
	}
	rep, err := a.reloadFunc(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rep.Applied == nil {
		rep.Applied = []string{}
	}
	if rep.RestartRequired == nil {
		rep.RestartRequired = []string{}
	}
	writeJSON(w, http.StatusOK, rep)
}
