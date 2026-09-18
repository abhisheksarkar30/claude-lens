package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// sensitiveHeaders are replaced with redactedValue before a captured
// request or response is submitted to the sink, regardless of how many
// values they carry. http.Header.Get returns only the first value of a
// multi-valued header — checking with Values and rewriting with Set (which
// replaces every value with the single redacted one) is what keeps a
// second x-api-key from slipping through unredacted.
var sensitiveHeaders = []string{"X-Api-Key", "Authorization", "Cookie", "Sessionkey"}

const redactedValue = "[redacted]"

// adminKeyPattern is the shape of an Anthropic Admin API key. A header
// value containing it is redacted outright regardless of which header
// carried it — an admin key must never reach the sink under any name.
const adminKeyPattern = "sk-ant-admin"

// redactHeaders returns a clone of h with every sensitive header replaced
// by "[redacted]" and any header value containing an admin-key-shaped
// substring replaced the same way. h itself is never mutated — the caller
// still sends the original, unredacted copy to the client or upstream.
func redactHeaders(h http.Header) http.Header {
	clone := h.Clone()
	if clone == nil {
		clone = http.Header{}
	}
	for _, name := range sensitiveHeaders {
		if len(clone.Values(name)) > 0 {
			clone.Set(name, redactedValue)
		}
	}
	for name, values := range clone {
		for _, v := range values {
			if strings.Contains(v, adminKeyPattern) {
				clone.Set(name, redactedValue)
				break
			}
		}
	}
	return clone
}

// RedactCheck is the startup self-test invoked by `serve` and `doctor`: it
// scans a captured call's stored header JSON for a reachable x-api-key,
// sessionKey, or sk-ant-admin… value that redaction should already have
// removed, so a regression in redactHeaders is caught before traffic
// flows, not after.
func RedactCheck(headerJSON []byte) error {
	var h map[string][]string
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return fmt.Errorf("proxy: RedactCheck: %w", err)
	}
	for name, values := range h {
		lname := strings.ToLower(name)
		for _, v := range values {
			if strings.Contains(strings.ToLower(v), adminKeyPattern) {
				return fmt.Errorf("proxy: RedactCheck: header %q carries an unredacted admin key", name)
			}
			switch lname {
			case "x-api-key", "authorization", "cookie", "sessionkey":
				if v != redactedValue {
					return fmt.Errorf("proxy: RedactCheck: header %q is not redacted: %q", name, v)
				}
			}
		}
	}
	return nil
}

// OriginAllowed reports whether r's Origin is safe for a state-changing
// route to honor while the server binds loopback: an absent Origin (a
// same-origin fetch never sets a cross-origin one) is allowed, a loopback
// Origin is allowed, and anything else is rejected — the browser still
// sends a cross-origin request even though CORS would block it from
// reading the response, so the guard has to reject it server-side.
func OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := origin
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, ":/"); i >= 0 {
		host = host[:i]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
