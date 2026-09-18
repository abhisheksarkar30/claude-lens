// Package cli implements clens's subcommands.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
)

// Doctor is `clens doctor`'s os.Stdout-writing entrypoint. args are config
// flags (--proxy-addr, --db-path, ...), forwarded straight to config.Load
// since doctor's whole job is reporting the resolved configuration's health.
//
// This bead's doctor covers configuration, port collisions, and the
// secrets file's actually observed protection level only. Per-source
// health (the store, the collectors) is added once those exist.
func Doctor(args []string) error {
	return runDoctor(args, os.Stdout)
}

type checkStatus string

const (
	statusPass checkStatus = "PASS"
	statusWarn checkStatus = "WARN"
	statusFail checkStatus = "FAIL"
)

type doctorCheck struct {
	Name   string
	Status checkStatus
	Detail string
}

func runDoctor(args []string, w io.Writer) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "configuration:")
	cfgRows := [][2]string{
		{"proxy_addr", cfg.ProxyAddr},
		{"dashboard_addr", cfg.DashboardAddr},
		{"upstream_url", cfg.UpstreamURL},
		{"db_path", cfg.DBPath},
		{"body_policy", cfg.BodyPolicy},
		{"body_cap_bytes", fmt.Sprintf("%d", cfg.BodyCapBytes)},
		{"allow_remote", fmt.Sprintf("%t", cfg.AllowRemote)},
		{"session_gap_minutes", fmt.Sprintf("%d", cfg.SessionGapMinutes)},
		{"retention_days", fmt.Sprintf("%d", cfg.RetentionDays)},
		{"replay_enabled", fmt.Sprintf("%t", cfg.ReplayEnabled)},
		{"accounts_configured", fmt.Sprintf("%d", len(cfg.Accounts))},
	}
	for _, row := range cfgRows {
		fmt.Fprintf(w, "  %-20s %s\n", row[0], row[1])
	}

	checks := runChecks(cfg)

	fmt.Fprintln(w, "\nchecks:")
	failed := false
	for _, c := range checks {
		fmt.Fprintf(w, "  [%-4s] %-20s %s\n", c.Status, c.Name, c.Detail)
		if c.Status == statusFail {
			failed = true
		}
	}

	if failed {
		return fmt.Errorf("doctor: one or more checks failed")
	}
	return nil
}

func runChecks(cfg *config.Config) []doctorCheck {
	var checks []doctorCheck

	if verr := cfg.Validate(); verr != nil {
		checks = append(checks, doctorCheck{"config_valid", statusFail, verr.Error()})
	} else {
		checks = append(checks, doctorCheck{"config_valid", statusPass, "ok"})
	}

	checks = append(checks, portCheck("proxy_port", cfg.ProxyAddr))
	checks = append(checks, portCheck("dashboard_port", cfg.DashboardAddr))
	if cfg.ProxyAddr == cfg.DashboardAddr {
		checks = append(checks, doctorCheck{"port_collision", statusFail,
			fmt.Sprintf("proxy_addr and dashboard_addr are both %s", cfg.ProxyAddr)})
	} else {
		checks = append(checks, doctorCheck{"port_collision", statusPass, "proxy and dashboard addresses differ"})
	}

	checks = append(checks, secretProtectionCheck())
	checks = append(checks, clientConfigCheck())

	return checks
}

// portCheck reports whether addr can currently be bound. A bind failure
// most often means another process — a running `clens serve`, or an
// existing deepseek-lens install — already holds the port; doctor WARNs
// rather than FAILs, since a running `serve` legitimately holds its own
// ports while doctor runs alongside it.
func portCheck(name, addr string) doctorCheck {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return doctorCheck{name, statusWarn, fmt.Sprintf("%s is already in use: %v", addr, err)}
	}
	ln.Close()
	return doctorCheck{name, statusPass, addr + " is available"}
}

// secretProtectionCheck reports the actually observed protection level of
// secrets.toml. A verification failure — the ACL step failed, or the file's
// permissions do not match what Save's contract requires — is a FAIL naming
// the file unprotected, in plain words; a file that does not exist yet is a
// PASS, since there is nothing to protect.
func secretProtectionCheck() doctorCheck {
	kind, ok := secret.ProtectionLevel()
	if !ok {
		return doctorCheck{"secret_protection", statusFail,
			fmt.Sprintf("secrets.toml is unprotected: %s", kind)}
	}
	if kind == "none" {
		return doctorCheck{"secret_protection", statusPass, "no credentials configured yet"}
	}
	return doctorCheck{"secret_protection", statusPass, kind}
}

// claudeConfigDir resolves Claude Code's config directory by Claude Code's
// own documented precedence, not clens's: on Windows, ~/.claude means
// %USERPROFILE%\.claude, not $HOME\.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	if up := os.Getenv("USERPROFILE"); up != "" {
		return filepath.Join(up, ".claude")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".claude")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".claude")
	}
	return ""
}

// readSettingsBaseURL reads the effective ANTHROPIC_BASE_URL from Claude
// Code's settings.json. Any read or parse failure, or an absent value,
// reports not-found rather than propagating an error: settings.json is
// Claude Code's own file, and every way it can be missing or malformed is a
// "nothing to check" state, never a doctor failure.
func readSettingsBaseURL(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var s struct {
		Env struct {
			AnthropicBaseURL string `json:"ANTHROPIC_BASE_URL"`
		} `json:"env"`
	}
	if err := json.Unmarshal(data, &s); err != nil || s.Env.AnthropicBaseURL == "" {
		return "", false
	}
	return s.Env.AnthropicBaseURL, true
}

// clientConfigCheck reports the effective ANTHROPIC_BASE_URL from Claude
// Code's settings.json, because that env block overrides whatever the
// user's shell has set. It can only PASS: a missing or unmanaged
// settings.json is not this tool's problem to fail on.
func clientConfigCheck() doctorCheck {
	dir := claudeConfigDir()
	if dir == "" {
		return doctorCheck{"client_config", statusPass, "could not resolve a Claude Code config directory — nothing to check"}
	}
	path := filepath.Join(dir, "settings.json")
	url, ok := readSettingsBaseURL(path)
	if !ok {
		return doctorCheck{"client_config", statusPass, "no ANTHROPIC_BASE_URL set in " + path}
	}
	return doctorCheck{"client_config", statusPass, fmt.Sprintf("%s declares ANTHROPIC_BASE_URL=%s", path, url)}
}
