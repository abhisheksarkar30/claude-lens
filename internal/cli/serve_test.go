package cli

import (
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/api"
	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Serve itself cannot be driven from a test -- it binds two real listeners and
// blocks on a signal context -- so these cover the three boot steps it calls,
// each of which is split out for exactly this reason.

// TestCheckRedactionReportsALeakAndContinues is both halves of the fail-open
// rule: the leak is reported, and boot is not refused over it.
func TestCheckRedactionReportsALeakAndContinues(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	leaked := apiRow("req_leaked", 1.0)
	leaked.ReqHeaders = `{"X-Api-Key":["sk-ant-leaked-never-redacted"]}`
	seedEvent(t, st, leaked)
	clean := apiRow("req_clean", 1.0)
	clean.ReqHeaders = `{"X-Api-Key":["[redacted]"]}`
	seedEvent(t, st, clean)

	var logged []string
	checkRedaction(context.Background(), st, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if len(logged) != 1 {
		t.Fatalf("checkRedaction logged %d line(s), want exactly 1 (one finding, not one per row):\n%s",
			len(logged), strings.Join(logged, "\n"))
	}
	if !strings.Contains(logged[0], "not redacted") {
		t.Fatalf("checkRedaction did not report the leak: %s", logged[0])
	}
	// The leak value itself must not ride along in the log line: writing the
	// credential into the log is the same failure one layer out.
	if strings.Contains(logged[0], "sk-ant-leaked-never-redacted") {
		t.Fatalf("checkRedaction printed the credential it found: %s", logged[0])
	}
}

func TestCheckRedactionSilentOnCleanHeaders(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	clean := apiRow("req_clean", 1.0)
	clean.ReqHeaders = `{"X-Api-Key":["[redacted]"],"Content-Type":["application/json"]}`
	seedEvent(t, st, clean)

	var logged []string
	checkRedaction(context.Background(), st, func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if len(logged) != 0 {
		t.Fatalf("checkRedaction logged on clean headers:\n%s", strings.Join(logged, "\n"))
	}
}

// TestPurgeOnStartupHonoursRetentionDays: the default (0) keeps everything, and
// a positive value deletes only what is past the cutoff.
func TestPurgeOnStartupHonoursRetentionDays(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	old := apiRow("req_old", 1.0)
	old.StartedAt = time.Now().Add(-40 * 24 * time.Hour)
	seedEvent(t, st, old)
	seedEvent(t, st, apiRow("req_new", 1.0))

	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	purgeOnStartup(context.Background(), st, 0, logf)
	if len(logged) != 0 {
		t.Fatalf("RetentionDays=0 (keep forever) logged and did work:\n%s", strings.Join(logged, "\n"))
	}
	n, err := st.CountEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 2 {
		t.Fatalf("event count after a retention no-op = %d, want 2", n)
	}

	purgeOnStartup(context.Background(), st, 8, logf)
	if len(logged) != 1 || !strings.Contains(logged[0], "deleted 1 row(s)") {
		t.Fatalf("purgeOnStartup(8 days) logged %q, want a 'deleted 1 row(s)' line", logged)
	}
	n, _ = st.CountEvents(context.Background(), store.EventFilter{})
	if n != 1 {
		t.Fatalf("event count after a 8-day purge = %d, want the newer row to survive", n)
	}
}

// TestPrintBannerNamesTheBaseURLAndWarnsWhenCaptureIsOff is the bead's
// "prints the ANTHROPIC_BASE_URL line" clause, plus the warning that stops a
// user from believing a body-capture-off install is recording anything.
func TestPrintBannerNamesTheBaseURLAndWarnsWhenCaptureIsOff(t *testing.T) {
	cfg := &config.Config{ProxyAddr: "127.0.0.1:8797", DashboardAddr: "127.0.0.1:8798", BodyPolicy: "all"}

	var buf bytes.Buffer
	printBanner(&buf, cfg)
	out := buf.String()
	if !strings.Contains(out, "ANTHROPIC_BASE_URL=http://127.0.0.1:8797") {
		t.Fatalf("banner missing the copy-pasteable base URL line:\n%s", out)
	}
	if !strings.Contains(out, "http://127.0.0.1:8798") {
		t.Fatalf("banner missing the dashboard URL:\n%s", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Fatalf("banner warns about body capture while it is on:\n%s", out)
	}

	cfg.BodyPolicy = "off"
	buf.Reset()
	printBanner(&buf, cfg)
	if !strings.Contains(buf.String(), "WARNING") {
		t.Fatalf("banner does not warn that capture is off:\n%s", buf.String())
	}
}

// TestWriteSeamsDoNotImportConfigOrIngest is the remaining half of the
// containment rule in CLAUDE.md: a dashboard write reaches config or a
// collector only through an injected function-value seam, so neither package
// may import them.
//
// The other half -- that neither internal/api nor internal/web imports
// internal/secret -- is asserted by internal/api/importguard_test.go and is not
// repeated here. (The bead phrases it as "only cli/serve imports secret"; that
// was already untrue when the bead was written, since accounts.go and
// refresh.go import it too. cli importing secret is fine and intended -- the
// rule is about the packages that serve HTTP requests.)
func TestWriteSeamsDoNotImportConfigOrIngest(t *testing.T) {
	banned := []string{"/internal/config", "/internal/ingest"}
	for _, dir := range []string{filepath.Join("..", "api"), filepath.Join("..", "web")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		for _, entry := range entries {
			// Test files are skipped: this is a guard on what the served
			// packages can reach, and a _test.go file is not compiled into the
			// dashboard. (internal/api's secret guard does not need the skip --
			// nothing there imports secret even in a test.)
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("ParseFile %s: %v", path, err)
			}
			for _, imp := range f.Imports {
				importPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					continue
				}
				for _, bad := range banned {
					if strings.HasSuffix(importPath, bad) {
						t.Errorf("%s imports %q: a dashboard write must reach config/ingest through an injected seam, not an import", path, importPath)
					}
				}
			}
		}
	}
}

// --- the proxy-mode seam (br-GI-7-05) -------------------------------------

// TestProxyModeNormalizesTheServeBannerString is the case a string comparison
// gets wrong on the tool's own output: printBanner writes "http://" +
// cfg.ProxyAddr while cfg.ProxyAddr is the bare host:port, and
// readSettingsBaseURL returns the settings string verbatim. Reporting the bad
// state for a correctly-pointed client is the one answer this indicator must
// not give.
func TestProxyModeNormalizesTheServeBannerString(t *testing.T) {
	const proxyAddr = "127.0.0.1:8797"
	if got := configuredAgainst("http://"+proxyAddr, true, proxyAddr); got != api.ProxyConfiguredMatch {
		t.Errorf("serve banner string: got %v, want Match", got)
	}
}

func TestProxyModeBaseURLSpellings(t *testing.T) {
	const proxyAddr = "127.0.0.1:8797"
	tests := []struct {
		name     string
		settings string
		ok       bool
		want     api.ProxyConfigured
	}{
		{"exact host:port", proxyAddr, true, api.ProxyConfiguredMatch},
		{"https scheme", "https://" + proxyAddr, true, api.ProxyConfiguredMatch},
		{"trailing slash", "http://" + proxyAddr + "/", true, api.ProxyConfiguredMatch},
		{"trailing path", "http://" + proxyAddr + "/v1/messages", true, api.ProxyConfiguredMatch},
		{"localhost for loopback", "http://localhost:8797", true, api.ProxyConfiguredMatch},
		{"a different port", "http://127.0.0.1:8787", true, api.ProxyConfiguredMismatch},
		// The port is the signal, so a bare host is never a match.
		{"bare host, no port", "http://127.0.0.1", true, api.ProxyConfiguredMismatch},
		// readSettingsBaseURL reports not-found for both an absent file and an
		// absent key; either way it is Unknown, never a mismatch.
		{"no setting at all", "", false, api.ProxyConfiguredUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := configuredAgainst(tc.settings, tc.ok, proxyAddr); got != tc.want {
				t.Errorf("configuredAgainst(%q, %v) = %v, want %v", tc.settings, tc.ok, got, tc.want)
			}
		})
	}
}

// TestProxyModeObservedWindow pins the boundary. Without a stated window
// "receiving" has no definition and nothing to test against; the badge reports
// on a live proxy, and a row from last week is not evidence it is running now.
func TestProxyModeObservedWindow(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	observed := func() bool {
		at, err := st.LatestProxyStartedAt(ctx)
		if err != nil {
			t.Fatalf("LatestProxyStartedAt: %v", err)
		}
		return !at.IsZero() && time.Since(at) <= proxyRecentWindow
	}

	// No proxy row has ever been written: the zero time, not "now minus a
	// week", so this must read as not receiving.
	if observed() {
		t.Fatal("an empty store reads as receiving")
	}

	// A jsonl-only store must also read as not receiving: the consumer's
	// LastWriteAt counts every source, which is why it is not the source here.
	sub := apiRow("req_jsonl", 1.0)
	sub.Source = "jsonl"
	sub.FirstSource = "jsonl"
	seedEvent(t, st, sub)
	if observed() {
		t.Fatal("a transcript-only store reads as receiving")
	}

	seedEvent(t, st, apiRow("req_proxy", 1.0))
	if !observed() {
		t.Fatal("a fresh proxy row does not read as receiving")
	}

	// Older than the window: written, but not evidence of a live proxy now.
	old := apiRow("req_old", 1.0)
	old.StartedAt = time.Now().Add(-2 * proxyRecentWindow)
	seedEvent(t, st, old)
	// LatestProxyStartedAt takes the max, so the fresh row above still wins;
	// add a store whose only row is stale instead.
	st2 := openTestStore(t, withHome(t))
	stale := apiRow("req_stale", 1.0)
	stale.StartedAt = time.Now().Add(-2 * proxyRecentWindow)
	seedEvent(t, st2, stale)
	at, err := st2.LatestProxyStartedAt(ctx)
	if err != nil {
		t.Fatalf("LatestProxyStartedAt: %v", err)
	}
	if !at.IsZero() && time.Since(at) <= proxyRecentWindow {
		t.Errorf("a row %v old reads as receiving (window %v)", time.Since(at), proxyRecentWindow)
	}
}
