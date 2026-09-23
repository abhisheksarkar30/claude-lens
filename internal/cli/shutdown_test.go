package cli

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShutdownDialsLoopbackForAWildcardDashboardAddr: DashboardAddr =
// 0.0.0.0:<port> (this repo's own operator config shape) must be dialed at
// 127.0.0.1:<port>, asserted against the Host the test server actually
// received -- the wildcard case is the one that silently produces a
// "connection refused" nobody can explain.
func TestShutdownDialsLoopbackForAWildcardDashboardAddr(t *testing.T) {
	withHome(t)

	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"shutting_down":true}`))
	}))
	defer srv.Close()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split the test server's address: %v", err)
	}

	var buf bytes.Buffer
	if err := runShutdown([]string{"--dashboard-addr", "0.0.0.0:" + port}, &buf); err != nil {
		t.Fatalf("runShutdown: %v\noutput:\n%s", err, buf.String())
	}

	wantHost := "127.0.0.1:" + port
	if gotHost != wantHost {
		t.Errorf("the server received Host = %q, want %q (the wildcard dashboard address should have been dialed as loopback)", gotHost, wantHost)
	}
}

// TestShutdownReportsAnUnreachableDashboard: nothing listening returns an
// error naming the address, not a zero exit.
func TestShutdownReportsAnUnreachableDashboard(t *testing.T) {
	withHome(t)

	// A loopback port nothing is listening on: bind then immediately release
	// it, rather than guessing a fixed number that might be in use.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close the probe listener: %v", err)
	}

	var buf bytes.Buffer
	if err := runShutdown([]string{"--dashboard-addr", addr}, &buf); err == nil {
		t.Fatal("runShutdown against an address nothing listens on = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), addr) {
		t.Errorf("error = %v, want it to name the address %q", err, addr)
	}
}
