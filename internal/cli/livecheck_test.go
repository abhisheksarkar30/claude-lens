package cli

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func liveHealthServer(t *testing.T) (port string, host *string, closeFn func()) {
	t.Helper()
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	_, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		srv.Close()
		t.Fatalf("split the test server's address: %v", err)
	}
	return p, &gotHost, srv.Close
}

func releasedLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close the probe listener: %v", err)
	}
	return addr
}

func writeIngestFixture(t *testing.T, home string) {
	t.Helper()
	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s1","requestId":"req_live","message":{"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "session1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIngestRebuildWarnsWhenServeIsLive(t *testing.T) {
	home := withHome(t)
	writeIngestFixture(t, home)
	openTestStore(t, home)

	port, host, closeFn := liveHealthServer(t)
	defer closeFn()

	var buf bytes.Buffer
	err := runIngest([]string{
		"--rebuild",
		"--dashboard-addr", "0.0.0.0:" + port,
		"--allow-remote",
	}, &buf)
	if err != nil {
		t.Fatalf("runIngest: %v\n%s", err, buf.String())
	}
	if got, want := *host, "127.0.0.1:"+port; got != want {
		t.Errorf("Host = %q, want %q", got, want)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Fatalf("output missing WARN:\n%s", buf.String())
	}
}

func TestIngestRebuildProceedsWhenServeIsDown(t *testing.T) {
	home := withHome(t)
	writeIngestFixture(t, home)
	openTestStore(t, home)

	var buf bytes.Buffer
	err := runIngest([]string{"--rebuild", "--dashboard-addr", releasedLoopbackAddr(t)}, &buf)
	if err != nil {
		t.Fatalf("runIngest: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Fatalf("output contains WARN with nothing listening:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "jsonl:") {
		t.Fatalf("output missing jsonl summary:\n%s", buf.String())
	}
}

func TestIngestWithoutRebuildDoesNotWarnWhenServeIsLive(t *testing.T) {
	home := withHome(t)
	writeIngestFixture(t, home)
	openTestStore(t, home)

	port, _, closeFn := liveHealthServer(t)
	defer closeFn()

	var buf bytes.Buffer
	err := runIngest([]string{"--dashboard-addr", "127.0.0.1:" + port}, &buf)
	if err != nil {
		t.Fatalf("runIngest: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Fatalf("non-rebuild ingest warned:\n%s", buf.String())
	}
}

func TestRepriceYesWarnsWhenServeIsLive(t *testing.T) {
	assertYesWarns(t, func(addr string, w *bytes.Buffer) error {
		return runReprice([]string{"--yes", "--dashboard-addr", addr}, w)
	})
}

func TestReflagYesWarnsWhenServeIsLive(t *testing.T) {
	assertYesWarns(t, func(addr string, w *bytes.Buffer) error {
		return runReflag([]string{"--yes", "--dashboard-addr", addr}, w)
	})
}

func TestPurgeYesWarnsWhenServeIsLive(t *testing.T) {
	assertYesWarns(t, func(addr string, w *bytes.Buffer) error {
		return runPurge([]string{"--yes", "--unpriced", "--dashboard-addr", addr}, w)
	})
}

func TestRepriceDryRunDoesNotWarnWhenServeIsLive(t *testing.T) {
	assertDryRunSilent(t, func(addr string, w *bytes.Buffer) error {
		return runReprice([]string{"--dry-run", "--dashboard-addr", addr}, w)
	})
}

func TestReflagDryRunDoesNotWarnWhenServeIsLive(t *testing.T) {
	assertDryRunSilent(t, func(addr string, w *bytes.Buffer) error {
		return runReflag([]string{"--dry-run", "--dashboard-addr", addr}, w)
	})
}

func TestPurgeDryRunDoesNotWarnWhenServeIsLive(t *testing.T) {
	assertDryRunSilent(t, func(addr string, w *bytes.Buffer) error {
		return runPurge([]string{"--dry-run", "--unpriced", "--dashboard-addr", addr}, w)
	})
}

func assertYesWarns(t *testing.T, run func(addr string, w *bytes.Buffer) error) {
	t.Helper()
	home := withHome(t)
	openTestStore(t, home)
	port, _, closeFn := liveHealthServer(t)
	defer closeFn()

	var buf bytes.Buffer
	if err := run("127.0.0.1:"+port, &buf); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Fatalf("output missing WARN:\n%s", buf.String())
	}
}

func assertDryRunSilent(t *testing.T, run func(addr string, w *bytes.Buffer) error) {
	t.Helper()
	home := withHome(t)
	openTestStore(t, home)
	port, _, closeFn := liveHealthServer(t)
	defer closeFn()

	var buf bytes.Buffer
	if err := run("127.0.0.1:"+port, &buf); err != nil {
		t.Fatalf("run: %v\n%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Fatalf("dry-run output contains WARN:\n%s", buf.String())
	}
}
