package config

import (
	"os"
	"path/filepath"
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

func TestValidateRejectsBadBodyPolicy(t *testing.T) {
	cfg := Default()
	cfg.BodyPolicy = "sometimes"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate: want error for invalid BodyPolicy")
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
