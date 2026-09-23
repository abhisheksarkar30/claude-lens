package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
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

// TestDoctorNamesThePprofFlagWhenUnset: the profiler is off by default, so
// doctor must name the flag that turns it on rather than just printing an
// empty value -- otherwise the feature is discoverable only by reading the
// source that needed it.
func TestDoctorNamesThePprofFlagWhenUnset(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runDoctor(nil, &buf); err != nil {
		t.Fatalf("runDoctor: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "--pprof-addr") {
		t.Errorf("doctor output does not name --pprof-addr when PprofAddr is unset:\n%s", out)
	}
}

// TestDoctorReportsTheSchemaVersion covers the reporting half of the check,
// which is the half doctor owns. Whether a version-0 database is actually
// brought forward is the migration runner's behaviour, and it is pinned in
// internal/store by T9a-T9d -- including the two orderings whose failure modes
// are unrecoverable. Asserting it again here would mean rebuilding a
// pre-change database inside a package that cannot reach schemaSQL, and a
// fixture that only approximates the old shape would be a worse test of it
// than the ones that use the real thing.
//
// What this test does answer is the question that had no answer before it: the
// stored version, without reading the file's header -- which does not answer it
// at all once the pages are dirty in a WAL.
func TestDoctorReportsTheSchemaVersion(t *testing.T) {
	home := withHome(t)
	st, err := store.Open(filepath.Join(home, ".clens", "lens.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Closed before runDoctor opens it again, and before TempDir's cleanup runs:
	// a live handle keeps Windows from removing the file.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(nil, &buf); err != nil {
		t.Fatalf("runDoctor: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()

	want := fmt.Sprintf("version %d (this binary: %d)", store.SchemaVersion(), store.SchemaVersion())
	if !strings.Contains(out, want) {
		t.Errorf("doctor did not report the schema version.\nwant substring: %q\noutput:\n%s", want, out)
	}
	if !strings.Contains(out, "[PASS] db_schema") {
		t.Errorf("db_schema is not PASS on a database this binary can read:\n%s", out)
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

// doctorRow returns the rendered value of one configuration row, found by
// field rather than by exact column position -- the row table's padding is not
// the contract under test.
func doctorRow(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == name {
			return strings.Join(f[1:], " ")
		}
	}
	t.Fatalf("doctor printed no %q row:\n%s", name, out)
	return ""
}

// T15: doctor's job is "print effective config", so both keys render their
// *resolved* value. Printing the raw slice length would print 0 in exactly the
// state where the shipped default is in force -- the state the row exists to
// show.
func TestDoctorPrintsResolvedPeakAndPrefixRows(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runDoctor(nil, &buf); err != nil {
		t.Fatalf("runDoctor: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()

	if got := doctorRow(t, out, "peak_off_peak_dates"); got != "33 (default)" {
		t.Errorf("peak_off_peak_dates = %q, want %q", got, "33 (default)")
	}
	if got := doctorRow(t, out, "api_model_prefixes"); got != "deepseek- (default)" {
		t.Errorf("api_model_prefixes = %q, want %q", got, "deepseek- (default)")
	}
}

func TestDoctorPrintsConfiguredPeakAndPrefixRows(t *testing.T) {
	withHome(t)
	t.Setenv("CLENS_PEAK_OFF_PEAK_DATES", "2026-01-01,2026-01-02")
	t.Setenv("CLENS_API_MODEL_PREFIXES", "acme-,globex-")

	var buf bytes.Buffer
	if err := runDoctor(nil, &buf); err != nil {
		t.Fatalf("runDoctor: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()

	if got := doctorRow(t, out, "peak_off_peak_dates"); got != "2" {
		t.Errorf("peak_off_peak_dates = %q, want %q", got, "2")
	}
	if got := doctorRow(t, out, "api_model_prefixes"); got != "acme-, globex-" {
		t.Errorf("api_model_prefixes = %q, want %q", got, "acme-, globex-")
	}
}

// `none` is a real state, not a synonym for unset -- doctor has to be able to
// tell an operator which one is in force.
func TestDoctorPrintsNoneForExplicitlyEmptyKeys(t *testing.T) {
	withHome(t)
	t.Setenv("CLENS_PEAK_OFF_PEAK_DATES", "none")
	t.Setenv("CLENS_API_MODEL_PREFIXES", "none")

	var buf bytes.Buffer
	if err := runDoctor(nil, &buf); err != nil {
		t.Fatalf("runDoctor: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()

	if got := doctorRow(t, out, "peak_off_peak_dates"); got != "none" {
		t.Errorf("peak_off_peak_dates = %q, want %q", got, "none")
	}
	if got := doctorRow(t, out, "api_model_prefixes"); got != "none" {
		t.Errorf("api_model_prefixes = %q, want %q", got, "none")
	}
}
