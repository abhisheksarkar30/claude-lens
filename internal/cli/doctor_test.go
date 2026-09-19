package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/secret"
)

func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return dir
}

func TestDoctorRunsCleanOnEmptyInstall(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runDoctor(nil, &buf); err != nil {
		t.Fatalf("runDoctor: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "configuration:") || !strings.Contains(out, "checks:") {
		t.Fatalf("doctor output missing expected sections:\n%s", out)
	}
}

func TestDoctorFailsWhenSecretIsUnprotected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("this test simulates the Windows ACL failure path")
	}
	home := withHome(t)
	clensDir := filepath.Join(home, ".clens")
	if err := os.MkdirAll(clensDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A secrets.toml exists, but nothing ever ran icacls against it — the
	// fake icacls binary the harness swaps in for these tests always fails
	// read-back for a path it never granted, which is exactly the state a
	// pre-existing, unprotected file would produce.
	secretsPath := filepath.Join(clensDir, "secrets.toml")
	if err := os.WriteFile(secretsPath, []byte("[sessionKey]\nvalue = \"v\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	kind, ok := secret.ProtectionLevel()
	if ok {
		t.Skipf("secret.ProtectionLevel unexpectedly reports protected (%s) in this test environment — cannot exercise the FAIL path", kind)
	}

	var buf bytes.Buffer
	err := runDoctor(nil, &buf)
	if err == nil {
		t.Fatalf("runDoctor: want error when secret is unprotected, output:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "unprotected") {
		t.Fatalf("doctor output missing FAIL/unprotected for secret_protection:\n%s", out)
	}
}

func TestClientConfigCheckReadsFixture(t *testing.T) {
	home := withHome(t)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := `{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8797"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	c := clientConfigCheck()
	if c.Status != statusPass {
		t.Fatalf("clientConfigCheck status = %v, want PASS", c.Status)
	}
	if !strings.Contains(c.Detail, "http://127.0.0.1:8797") {
		t.Fatalf("clientConfigCheck detail = %q, want it to mention the configured base URL", c.Detail)
	}
}

func TestClientConfigCheckNoSettingsFile(t *testing.T) {
	withHome(t)
	c := clientConfigCheck()
	if c.Status != statusPass {
		t.Fatalf("clientConfigCheck with no settings.json: status = %v, want PASS", c.Status)
	}
}
