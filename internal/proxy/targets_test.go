package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetLocalFailsWithNoURL(t *testing.T) {
	cfg := testConfig("http://cloud.invalid")
	tg, err := NewTargets(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.Set("local"); err == nil {
		t.Fatal(`Set("local") succeeded with no LocalUpstreamURL`)
	}
	if tg.Active() != "cloud" {
		t.Errorf("Active() = %q after a rejected Set, want cloud", tg.Active())
	}
	if err := tg.Set("bogus"); err == nil {
		t.Error(`Set("bogus") succeeded`)
	}
}

func TestProviderBootFallsBackToCloud(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig("http://cloud.invalid")
	for name, content := range map[string]string{
		"local with no local url": "local",
		"unrecognised":            "garbage",
		"empty":                   "",
	} {
		if err := os.WriteFile(filepath.Join(dir, "provider"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		tg, err := NewTargets(cfg, dir)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Active() != "cloud" {
			t.Errorf("%s: Active() = %q, want cloud", name, tg.Active())
		}
	}
	// An absent file is the same.
	tg, err := NewTargets(cfg, t.TempDir())
	if err != nil || tg.Active() != "cloud" {
		t.Errorf("absent state file: Active() = %q, err = %v, want cloud", tg.Active(), err)
	}
}

func TestProviderPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig("http://cloud.invalid")
	cfg.LocalUpstreamURL = "http://localhost:1234"

	first, err := NewTargets(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Active() != "cloud" {
		t.Fatalf("fresh Targets Active() = %q, want cloud", first.Active())
	}
	if err := first.Set("local"); err != nil {
		t.Fatal(err)
	}
	second, err := NewTargets(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Active() != "local" {
		t.Errorf("after restart Active() = %q, want local", second.Active())
	}
	if err := second.Set("cloud"); err != nil {
		t.Fatal(err)
	}
	third, _ := NewTargets(cfg, dir)
	if third.Active() != "cloud" {
		t.Errorf("after switching back and restarting Active() = %q, want cloud", third.Active())
	}
	if _, err := os.Stat(filepath.Join(dir, "provider.tmp")); err == nil {
		t.Error("the temp file was left behind")
	}
}

func TestNewTargetsWithEmptyStateDirDoesNoIO(t *testing.T) {
	cfg := testConfig("http://cloud.invalid")
	cfg.LocalUpstreamURL = "http://localhost:1234"
	tg, err := NewTargets(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if tg.path != "" {
		t.Fatalf("path = %q, want empty so nothing touches disk", tg.path)
	}
	if err := tg.Set("local"); err != nil {
		t.Fatalf("Set with no state dir: %v", err)
	}
	if tg.Active() != "local" {
		t.Errorf("Active() = %q, want the in-memory switch to take effect", tg.Active())
	}
}
