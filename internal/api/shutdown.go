package api

import (
	"fmt"
	"net"
	"net/http"
)

// SetShutdown wires POST /api/shutdown to fn, the same cancel func Ctrl+C
// already drives (Serve's signal.NotifyContext), so this route triggers no
// second shutdown implementation. Leaving it unset is supported: the route
// answers 503 rather than panicking on a nil func.
func (a *api) SetShutdown(fn func()) { a.shutdownFunc = fn }

// shutdown is POST /api/shutdown: `clens shutdown`'s remote-triggered
// graceful stop, added because the only other way to end a foreground
// `clens serve` from another shell is a hard kill that skips the drain, the
// sink flush and the store close.
//
// Two guards, and originReject alone is not enough. originReject reads only
// the Host header -- its job is the DNS-rebinding case, a page whose own
// hostname resolves to 127.0.0.1. A caller that can already reach a
// dashboard bound to 0.0.0.0 (--allow-remote) can send Host: 127.0.0.1 and
// pass that guard regardless of where the connection actually came from. A
// route that stops the process checks the connection's real origin too:
// r.RemoteAddr, split with net.SplitHostPort (it may be "[::1]:port") and
// tested with loopbackHost -- this package's own copy of the loopback
// predicate, not config.IsLoopbackHost, since internal/api does not import
// internal/config (see origin.go). --allow-remote widens neither guard:
// this route is loopback-only unconditionally.
//
// The response is written before fn runs, on its own goroutine, so a
// shutdown that cancels the server's context first can never race its own
// response out of existence.
func (a *api) shutdown(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "shutdown"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackHost(host) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("shutdown requires a loopback caller, got %q", r.RemoteAddr))
		return
	}
	if a.shutdownFunc == nil {
		writeError(w, http.StatusServiceUnavailable, "shutdown is unavailable: no stop function is wired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shutting_down": true})
	go a.shutdownFunc()
}
