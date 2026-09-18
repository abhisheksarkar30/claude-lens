package secret

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fakeACL simulates icacls's observable behavior (grant + read-back) against
// an in-memory table keyed by path, so tests exercise the real
// applyWindowsACL/windowsProtectionLevel parsing and verification logic
// without depending on the current process's actual Windows ACL state.
type fakeACL struct {
	mu           sync.Mutex
	grants       map[string][]string
	failGrant    bool
	failReadback bool
	calls        [][]string
}

func newFakeACL() *fakeACL { return &fakeACL{grants: map[string][]string{}} }

func (f *fakeACL) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))

	path := args[0]
	if len(args) == 1 {
		if f.failReadback {
			return "", errors.New("fake icacls: read-back failure")
		}
		var b strings.Builder
		b.WriteString(path + "\n")
		for _, p := range f.grants[path] {
			b.WriteString("  " + p + ":(F)\n")
		}
		b.WriteString("\nSuccessfully processed 1 files; Failed processing 0 files\n")
		return b.String(), nil
	}
	if f.failGrant {
		return "", errors.New("fake icacls: grant failure")
	}
	grantArg := args[len(args)-1] // "<principal>:F"
	principal := strings.TrimSuffix(grantArg, ":F")
	f.grants[path] = []string{principal}
	return "Successfully processed 1 files; Failed processing 0 files\n", nil
}

func withFakeICACLS(t *testing.T, f *fakeACL) {
	t.Helper()
	orig := runICACLS
	runICACLS = f.run
	t.Cleanup(func() { runICACLS = orig })
}

func withPrincipal(t *testing.T, name string) {
	t.Helper()
	orig := currentPrincipal
	currentPrincipal = func() (string, error) { return name, nil }
	t.Cleanup(func() { currentPrincipal = orig })
}

func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// setup gives every test a fake ACL backend (on Windows) so the suite never
// depends on the test runner's own account having real ACL privileges, and
// a temp $HOME so secrets.toml never touches the real one.
func setup(t *testing.T) string {
	t.Helper()
	home := withHome(t)
	if runtime.GOOS == "windows" {
		withPrincipal(t, "TESTDOMAIN\\tester")
		withFakeICACLS(t, newFakeACL())
	}
	return home
}

func TestSaveGetRoundTrip(t *testing.T) {
	setup(t)
	if err := Save("sessionKey", "sk-value-1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Get("sessionKey")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-value-1" {
		t.Errorf("Get = %q, want sk-value-1", got)
	}
}

func TestSecondSaveOverwrites(t *testing.T) {
	setup(t)
	if err := Save("admin", "first"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save("admin", "second"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Get("admin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "second" {
		t.Errorf("Get = %q, want second", got)
	}
}

func TestGetUnsetReturnsErrUnset(t *testing.T) {
	setup(t)
	_, err := Get("sessionKey")
	if !errors.Is(err, ErrUnset) {
		t.Fatalf("Get on unset name: err = %v, want ErrUnset", err)
	}
}

func TestExists(t *testing.T) {
	setup(t)
	if Exists("sessionKey") {
		t.Fatal("Exists before Save = true, want false")
	}
	if err := Save("sessionKey", "v"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !Exists("sessionKey") {
		t.Fatal("Exists after Save = false, want true")
	}
}

func TestProtectionLevelNoFile(t *testing.T) {
	setup(t)
	kind, ok := ProtectionLevel()
	if kind != "none" || !ok {
		t.Fatalf("ProtectionLevel with no file = (%q, %v), want (none, true)", kind, ok)
	}
}

func TestProtectionLevelReportsAppliedProtection(t *testing.T) {
	setup(t)
	if err := Save("sessionKey", "v"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	kind, ok := ProtectionLevel()
	if !ok {
		t.Fatalf("ProtectionLevel after Save: ok = false, kind = %q", kind)
	}
	if kind == "" || kind == "none" {
		t.Fatalf("ProtectionLevel after Save: kind = %q, want a description of the applied protection", kind)
	}
}

func TestPOSIXModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not a control on Windows")
	}
	home := setup(t)
	if err := Save("sessionKey", "v"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(home, ".clens", "secrets.toml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", got)
	}
}

func TestWindowsDACLReadBackListsExactlyIntendedPrincipal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only")
	}
	home := withHome(t)
	withPrincipal(t, "TESTDOMAIN\\tester")
	acl := newFakeACL()
	withFakeICACLS(t, acl)

	if err := Save("sessionKey", "v"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(home, ".clens", "secrets.toml")

	out, err := runICACLS(path)
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	principals, err := parseICACLSPrincipals(out)
	if err != nil {
		t.Fatalf("parseICACLSPrincipals: %v", err)
	}
	if len(principals) != 1 || !strings.EqualFold(principals[0], "TESTDOMAIN\\tester") {
		t.Fatalf("principals = %v, want exactly [TESTDOMAIN\\tester]", principals)
	}
}

func TestFailClosedGrantFailureLeavesExistingFileUntouched(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only ACL fail-closed behavior")
	}
	home := withHome(t)
	withPrincipal(t, "TESTDOMAIN\\tester")
	acl := newFakeACL()
	withFakeICACLS(t, acl)

	if err := Save("sessionKey", "original"); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	path := filepath.Join(home, ".clens", "secrets.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	acl.failGrant = true
	if err := Save("sessionKey", "poisoned"); err == nil {
		t.Fatal("Save with failing icacls grant: want error, got nil")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("file changed despite fail-closed grant failure:\nbefore: %q\nafter:  %q", before, after)
	}

	got, err := Get("sessionKey")
	if err != nil {
		t.Fatalf("Get after failed Save: %v", err)
	}
	if got != "original" {
		t.Fatalf("Get after failed Save = %q, want original (credential must not have been overwritten)", got)
	}
}

func TestFailClosedReadbackMismatchLeavesExistingFileUntouched(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only ACL fail-closed behavior")
	}
	home := withHome(t)
	withPrincipal(t, "TESTDOMAIN\\tester")
	acl := newFakeACL()
	withFakeICACLS(t, acl)

	if err := Save("sessionKey", "original"); err != nil {
		t.Fatalf("initial Save: %v", err)
	}
	path := filepath.Join(home, ".clens", "secrets.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	acl.failReadback = true
	if err := Save("sessionKey", "poisoned"); err == nil {
		t.Fatal("Save with failing icacls read-back: want error, got nil")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("file changed despite fail-closed read-back mismatch:\nbefore: %q\nafter:  %q", before, after)
	}
}

func TestPrincipalWithSpacesPassedAsSingleArgument(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only ACL principal resolution")
	}
	withHome(t)
	withPrincipal(t, "First Last")
	acl := newFakeACL()
	withFakeICACLS(t, acl)

	if err := Save("sessionKey", "v"); err != nil {
		t.Fatalf("Save with a spaced principal: %v", err)
	}

	found := false
	for _, call := range acl.calls {
		for _, arg := range call {
			if arg == "First Last:F" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected an icacls call with a single argument %q, calls: %v", "First Last:F", acl.calls)
	}
}
