package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withHome points $HOME (and clears CLENS_* env vars) at a fresh temp dir for
// the duration of the test, so Load never touches the real ~/.clens.
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	for env := range fieldsByEnv {
		t.Setenv(env, "")
	}
	return dir
}

func TestDefaults(t *testing.T) {
	withHome(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProxyAddr != "127.0.0.1:8797" {
		t.Errorf("ProxyAddr = %q, want 127.0.0.1:8797", cfg.ProxyAddr)
	}
	if cfg.DashboardAddr != "127.0.0.1:8798" {
		t.Errorf("DashboardAddr = %q, want 127.0.0.1:8798", cfg.DashboardAddr)
	}
	if cfg.UpstreamURL != "https://api.anthropic.com" {
		t.Errorf("UpstreamURL = %q, want https://api.anthropic.com", cfg.UpstreamURL)
	}
	if cfg.BodyPolicy != "full" {
		t.Errorf("BodyPolicy = %q, want full", cfg.BodyPolicy)
	}
}

// TestBodyCapDefaultAndOverrides pins RC-C's default and both override paths.
//
// The default moved 262,144 -> 2,097,152 because truncation was the norm rather
// than the exception on a real install: 58% of request bodies exceeded the old
// cap, and the dominant body is the request, not the response. A silent revert
// would be invisible -- it shows up only as a pile of honest `incomplete` flags
// on newly captured traffic, which is exactly what this story exists to stop
// being normal. The override half is what keeps the cap configurable, so it can
// be lowered without a rebuild.
func TestBodyCapDefaultAndOverrides(t *testing.T) {
	if got := Default().BodyCapBytes; got != 2097152 {
		t.Errorf("Default().BodyCapBytes = %d, want 2097152", got)
	}

	withHome(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BodyCapBytes != 2097152 {
		t.Errorf("BodyCapBytes with nothing configured = %d, want the default 2097152", cfg.BodyCapBytes)
	}

	t.Setenv("CLENS_BODY_CAP_BYTES", "65536")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("Load (env): %v", err)
	}
	if cfg.BodyCapBytes != 65536 {
		t.Errorf("BodyCapBytes from env = %d, want 65536", cfg.BodyCapBytes)
	}

	cfg, err = Load([]string{"--body-cap-bytes", "131072"})
	if err != nil {
		t.Fatalf("Load (flag): %v", err)
	}
	if cfg.BodyCapBytes != 131072 {
		t.Errorf("BodyCapBytes from flag = %d, want 131072 (the flag beats the env var)", cfg.BodyCapBytes)
	}
}

func TestResolutionOrderFlagBeatsFileBeatsEnv(t *testing.T) {
	dir := withHome(t)
	clensDir := filepath.Join(dir, ".clens")
	if err := os.MkdirAll(clensDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(clensDir, "config.toml")
	if err := os.WriteFile(cfgFile, []byte("ProxyAddr = 127.0.0.1:9001\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// env alone should win over the file.
	t.Setenv("CLENS_PROXY_ADDR", "127.0.0.1:9002")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProxyAddr != "127.0.0.1:9002" {
		t.Errorf("env should beat file: ProxyAddr = %q, want 127.0.0.1:9002", cfg.ProxyAddr)
	}

	// a flag should win over both.
	cfg, err = Load([]string{"--proxy-addr", "127.0.0.1:9003"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ProxyAddr != "127.0.0.1:9003" {
		t.Errorf("flag should beat env: ProxyAddr = %q, want 127.0.0.1:9003", cfg.ProxyAddr)
	}
}

func TestValidateRejectsNonLoopbackWithoutAllowRemote(t *testing.T) {
	cfg := Default()
	cfg.ProxyAddr = "0.0.0.0:8797"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate: want error for non-loopback ProxyAddr without AllowRemote")
	}
	cfg.AllowRemote = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: unexpected error with AllowRemote set: %v", err)
	}
}

// TestPprofAddrPrecedence (§6 test 8): flag beats env beats file; unset
// leaves the zero value, the same resolution order every other field uses.
func TestPprofAddrPrecedence(t *testing.T) {
	dir := withHome(t)
	if cfg, err := Load(nil); err != nil {
		t.Fatalf("Load: %v", err)
	} else if cfg.PprofAddr != "" {
		t.Errorf("PprofAddr with nothing configured = %q, want empty (the listener off)", cfg.PprofAddr)
	}

	clensDir := filepath.Join(dir, ".clens")
	if err := os.MkdirAll(clensDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clensDir, "config.toml"), []byte("PprofAddr = 127.0.0.1:6061\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load (file): %v", err)
	}
	if cfg.PprofAddr != "127.0.0.1:6061" {
		t.Errorf("PprofAddr from file = %q, want 127.0.0.1:6061", cfg.PprofAddr)
	}

	t.Setenv("CLENS_PPROF_ADDR", "127.0.0.1:6062")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("Load (env): %v", err)
	}
	if cfg.PprofAddr != "127.0.0.1:6062" {
		t.Errorf("env should beat file: PprofAddr = %q, want 127.0.0.1:6062", cfg.PprofAddr)
	}

	cfg, err = Load([]string{"--pprof-addr", "127.0.0.1:6063"})
	if err != nil {
		t.Fatalf("Load (flag): %v", err)
	}
	if cfg.PprofAddr != "127.0.0.1:6063" {
		t.Errorf("flag should beat env: PprofAddr = %q, want 127.0.0.1:6063", cfg.PprofAddr)
	}
}

// TestValidateRejectsNonLoopbackPprofAddrEvenWithAllowRemote is D5's test:
// a public PprofAddr with AllowRemote = true still fails, because a profile
// is a dump of whatever is in memory. This is the one that catches a later
// "tidy-up" that starts passing c.AllowRemote to validateLoopback here.
func TestValidateRejectsNonLoopbackPprofAddrEvenWithAllowRemote(t *testing.T) {
	cfg := Default()
	cfg.AllowRemote = true
	cfg.PprofAddr = "0.0.0.0:6060"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate: want error for a non-loopback PprofAddr even with AllowRemote set")
	}
}

// TestValidateAcceptsEmptyPprofAddr: the default (the listener off)
// validates clean.
func TestValidateAcceptsEmptyPprofAddr(t *testing.T) {
	cfg := Default()
	if cfg.PprofAddr != "" {
		t.Fatalf("Default().PprofAddr = %q, want empty", cfg.PprofAddr)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected the default empty PprofAddr: %v", err)
	}
}

// TestIsLoopbackHost: accepts every loopback spelling, including one a naive
// ip.IsLoopback()-only predicate would reject ("localhost", since
// net.ParseIP returns nil for it) and one a naive three-spelling allowlist
// would reject (127.0.0.2, a legitimately loopback IP that is neither
// 127.0.0.1 nor ::1).
func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "127.0.0.2", "localhost"} {
		if !IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = false, want true", host)
		}
	}
	if IsLoopbackHost("example.com") {
		t.Error("IsLoopbackHost(\"example.com\") = true, want false")
	}
}

// TestBodyPolicyAcceptsExactlyTheValuesThatDoSomething pins the accepted set in
// both directions. The positive half is the one that matters: "truncated" was
// accepted for the project's whole life and read nowhere, so an assertion that
// only checked the negative would have been green throughout the defect.
//
// It is rejected rather than aliased to "full" deliberately. On a flag that
// decides what content reaches the database, a value reading as "narrow it"
// while storing the body whole is worse than no value at all, and this flag's
// default is the permissive one.
func TestBodyPolicyAcceptsExactlyTheValuesThatDoSomething(t *testing.T) {
	for _, policy := range []string{"full", "off"} {
		cfg := Default()
		cfg.BodyPolicy = policy
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate rejected %q, which the proxy branches on: %v", policy, err)
		}
	}

	for _, policy := range []string{"truncated", "sometimes", "", "FULL"} {
		cfg := Default()
		cfg.BodyPolicy = policy
		err := cfg.Validate()
		if err == nil {
			t.Errorf("Validate accepted %q, which no code path reads", policy)
			continue
		}
		// The message has to name the real choice, not just reject: someone
		// who set "truncated" needs to be told what to set instead, and the
		// old message advertised the value being removed.
		if policy == "truncated" && !strings.Contains(err.Error(), "full or off") {
			t.Errorf("the error for %q does not name the accepted values: %v", policy, err)
		}
	}
}

func TestMixedAccountParsesAsTwoAccounts(t *testing.T) {
	dir := withHome(t)
	clensDir := filepath.Join(dir, ".clens")
	if err := os.MkdirAll(clensDir, 0o755); err != nil {
		t.Fatal(err)
	}
	accountsFile := filepath.Join(clensDir, "accounts.toml")
	content := "name = work\nbilling_mode = subscription\nplan = max20x\n\nname = work\nbilling_mode = api\n"
	if err := os.WriteFile(accountsFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("Accounts = %d entries, want 2: %+v", len(cfg.Accounts), cfg.Accounts)
	}
	var sawSubscription, sawAPI bool
	for _, a := range cfg.Accounts {
		if a.Name != "work" {
			t.Errorf("account name = %q, want work", a.Name)
		}
		switch a.BillingMode {
		case "subscription":
			sawSubscription = true
			if a.Plan != "max20x" {
				t.Errorf("subscription account plan = %q, want max20x", a.Plan)
			}
		case "api":
			sawAPI = true
		}
	}
	if !sawSubscription || !sawAPI {
		t.Fatalf("expected one subscription and one api account, got %+v", cfg.Accounts)
	}
}

func TestNoAccountsFileIsNotAnError(t *testing.T) {
	withHome(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 0 {
		t.Errorf("Accounts = %+v, want empty", cfg.Accounts)
	}
}

// --- Peak / prefix keys (br-GI-3-05) ---

// T10: both keys parse from file and from env. An env value replaces the
// file's wholesale -- the two are never merged, which is the same precedence
// every other key follows.
func TestPeakAndPrefixKeysParseFromFileAndEnv(t *testing.T) {
	dir := withHome(t)
	clensDir := filepath.Join(dir, ".clens")
	if err := os.MkdirAll(clensDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfgFile := filepath.Join(clensDir, "config.toml")
	if err := os.WriteFile(cfgFile, []byte(
		"PeakOffPeakDates = 2026-01-01,2026-01-02\nApiModelPrefixes = acme-,globex-\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.PeakOffPeakDates; len(got) != 2 || got[0] != "2026-01-01" || got[1] != "2026-01-02" {
		t.Errorf("PeakOffPeakDates from file = %#v, want the two configured dates", got)
	}
	if got := cfg.ApiModelPrefixes; len(got) != 2 || got[0] != "acme-" || got[1] != "globex-" {
		t.Errorf("ApiModelPrefixes from file = %#v, want the two configured prefixes", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a file-sourced config: %v", err)
	}

	t.Setenv("CLENS_PEAK_OFF_PEAK_DATES", "none")
	t.Setenv("CLENS_API_MODEL_PREFIXES", "acme-")
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PeakOffPeakDates == nil || len(cfg.PeakOffPeakDates) != 0 {
		t.Errorf("PeakOffPeakDates from `none` = %#v, want a non-nil empty slice", cfg.PeakOffPeakDates)
	}
	if got := cfg.ApiModelPrefixes; len(got) != 1 || got[0] != "acme-" {
		t.Errorf("ApiModelPrefixes from env = %#v, want [acme-] (env replaces the file's list wholesale)", got)
	}
}

// `none` is the only way a file can say "explicitly empty": applyKV skips a
// blank value outright, so unset and empty must stay distinguishable.
func TestPeakAndPrefixKeysUnsetAreNil(t *testing.T) {
	withHome(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PeakOffPeakDates != nil {
		t.Errorf("unset PeakOffPeakDates = %#v, want nil (a distinct state from `none`)", cfg.PeakOffPeakDates)
	}
	if cfg.ApiModelPrefixes != nil {
		t.Errorf("unset ApiModelPrefixes = %#v, want nil", cfg.ApiModelPrefixes)
	}
}

func TestValidateRejectsMalformedDatesAndPrefixes(t *testing.T) {
	withHome(t)

	// A typo'd date silently stays in peak, which over-charges, so it is
	// rejected rather than absorbed.
	c := Default()
	c.PeakOffPeakDates = []string{"2026-01-01", "01/02/2026"}
	err := c.Validate()
	if err == nil {
		t.Error("Validate accepted a malformed date")
	} else if !strings.Contains(err.Error(), "PeakOffPeakDates") {
		t.Errorf("error %q does not name the field", err)
	}

	c = Default()
	c.ApiModelPrefixes = []string{"acme-", "   "}
	err = c.Validate()
	if err == nil {
		t.Error("Validate accepted a whitespace-only prefix")
	} else if !strings.Contains(err.Error(), "ApiModelPrefixes") {
		t.Errorf("error %q does not name the field", err)
	}

	c = Default()
	c.PeakOffPeakDates = []string{"2026-01-01"}
	c.ApiModelPrefixes = []string{"acme-"}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate rejected a valid config: %v", err)
	}

	// The `none` shape is valid: excluding no dates and routing nothing is a
	// choice, not a malformed config.
	c = Default()
	c.PeakOffPeakDates = []string{}
	c.ApiModelPrefixes = []string{}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate rejected the `none` shape: %v", err)
	}
}
