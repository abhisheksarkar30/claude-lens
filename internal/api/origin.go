package api

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// originReject is the Origin/Host allowlist every write route shares
// (deepseek-lens's replayOriginReject, carried over): it returns "" when
// the request may proceed, or the rejection reason to report as a 403. It
// reads only headers -- never the store, an upstream, or the body -- so a
// rejection costs nothing.
//
// The write routes that call it are the two Slice B ones -- POST /api/prices
// and POST /api/requests/{id}/replay -- plus the four br-GI-1-18 adds
// (/api/secrets, /api/accounts, /api/ingest; see
// docs/planning/GI-1-claude-lens-v1.md §API surface). Read (GET) routes do
// not: they are loopback-bound and change nothing, which is what the
// dashboard's no-auth-on-loopback posture rests on.
//
// action names the calling route in the rejection message ("prices",
// "secrets", ...), since every write route shares this one guard.
//
// Two rejections, both about who can reach a write route from a browser:
//
//   - Host must be loopback. A DNS-rebinding page resolves its own
//     hostname to 127.0.0.1 and then POSTs with its own Host header, so a
//     non-loopback Host is the rebinding case even though the connection
//     itself arrived over loopback.
//   - Origin, when present, must be this request's own origin (compared as
//     the Origin's host:port against the request's own Host, matching a
//     browser's same-origin rule -- not against a configured dashboard
//     address, since the dashboard answers on whichever loopback alias the
//     user browsed to).
//
// A missing Origin passes: browsers always send it, so its absence means a
// non-browser client (e.g. the CLI), which is the deliberate
// credentialless design this guard exists alongside.
func originReject(r *http.Request, action string) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if !loopbackHost(host) {
		return fmt.Sprintf("%s requires a loopback Host, got %q", action, r.Host)
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return fmt.Sprintf("%s rejected origin %q: not a usable origin", action, origin)
	}
	if !strings.EqualFold(u.Host, r.Host) {
		return fmt.Sprintf("%s rejected cross-origin request: origin %q is not this dashboard's own origin %q", action, origin, r.Host)
	}
	return ""
}

// loopbackHost reports whether host -- any port already stripped -- is a
// loopback name or address.
func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
