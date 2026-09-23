// Package cli implements clens's subcommands.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/ingest"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Doctor is `clens doctor`'s os.Stdout-writing entrypoint. args are config
// flags (--proxy-addr, --db-path, ...), forwarded straight to config.Load
// since doctor's whole job is reporting the resolved configuration's health.
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

// effectivePeakDates renders the resolved off-peak date list for doctor: the
// shipped count when the key is unset, "none" when it is explicitly empty,
// otherwise the configured count. Printing len() raw would show 0 in exactly
// the state where the shipped 33-date default is in force -- the state this
// row exists to show.
func effectivePeakDates(configured []string) string {
	switch {
	case configured == nil:
		return fmt.Sprintf("%d (default)", len(pricing.ShippedOffPeakDates()))
	case len(configured) == 0:
		return "none"
	default:
		return fmt.Sprintf("%d", len(configured))
	}
}

// effectiveAPIPrefixes is effectivePeakDates' counterpart for the prefix list,
// which shows the values rather than a count: the resolved default, "none", or
// the configured list.
func effectiveAPIPrefixes(cfg *config.Config) string {
	switch {
	case cfg.ApiModelPrefixes == nil:
		return strings.Join(resolvedAPIPrefixes(cfg), ", ") + " (default)"
	case len(cfg.ApiModelPrefixes) == 0:
		return "none"
	default:
		return strings.Join(cfg.ApiModelPrefixes, ", ")
	}
}

// effectivePprofAddr names the flag that enables the profiler when it is
// unset, so the feature is discoverable from doctor's output rather than
// folklore -- otherwise the only way to learn --pprof-addr exists is to
// have already read the source that needed it.
func effectivePprofAddr(addr string) string {
	if addr == "" {
		return "unset (enable with --pprof-addr/CLENS_PPROF_ADDR)"
	}
	return addr
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
		{"peak_off_peak_dates", effectivePeakDates(cfg.PeakOffPeakDates)},
		{"api_model_prefixes", effectiveAPIPrefixes(cfg)},
		{"pprof_addr", effectivePprofAddr(cfg.PprofAddr)},
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

	fmt.Fprintln(w, "\nsources:")
	for _, row := range sourceHealthRows(cfg) {
		fmt.Fprintf(w, "  %-10s %s\n", row.name, row.detail)
	}

	if failed {
		return fmt.Errorf("doctor: one or more checks failed")
	}
	return nil
}

type sourceRow struct {
	name   string
	detail string
}

// sourceHealthRows reports the four sources' health: proxy from a direct
// event count (it runs continuously outside any poll cycle, so it has no
// ingest_state outcome to read), and jsonl/snapshot/admin from
// internal/ingest's per-source health keys. Opening the store here can
// create db_path's file on a machine that has never run `clens serve` --
// that mirrors store.Open's own "create if missing" contract, and an
// empty database reports every source as pending, not an error.
func sourceHealthRows(cfg *config.Config) []sourceRow {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return []sourceRow{{"error", fmt.Sprintf("could not open %s: %v", cfg.DBPath, err)}}
	}
	defer st.Close()

	ctx := context.Background()
	rows := []sourceRow{{"proxy", proxyHealthDetail(ctx, st)}}

	health, err := ingest.New(st).SourcesHealth(ctx)
	if err != nil {
		rows = append(rows, sourceRow{"ingest", fmt.Sprintf("could not read source health: %v", err)})
		return rows
	}
	for _, h := range health {
		rows = append(rows, sourceRow{string(h.Source), formatHealth(h)})
	}
	return rows
}

func proxyHealthDetail(ctx context.Context, st *store.Store) string {
	n, err := st.CountEvents(ctx, store.EventFilter{Source: "proxy"})
	if err != nil {
		return fmt.Sprintf("could not count captured requests: %v", err)
	}
	return fmt.Sprintf("%d requests captured (runs continuously in `clens serve`, no poll cycle to report)", n)
}

func formatHealth(h ingest.Health) string {
	switch h.Status {
	case "unknown":
		return "never run"
	case "ok":
		return fmt.Sprintf("ok, last success %s, %d rows", h.LastSuccessAt.Format(time.RFC3339), h.RowsWritten)
	default:
		return fmt.Sprintf("FAILING since %s: %s", h.LastErrorAt.Format(time.RFC3339), h.LastError)
	}
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
	checks = append(checks, dbSchemaCheck(cfg))
	checks = append(checks, toolNamesBackfillCheck(cfg))

	return checks
}

// toolNamesBackfillCheck reports how many rows still await `clens
// backfill-tool-names` (br-GI-13-07). A row written before that column
// existed reads as HasReqBody=false under the column's NULL contract until
// backfilled, which would silently decline ruleCacheInvalidatedByTools on
// real history -- WARNs rather than FAILs, since an un-backfilled database is
// a maintenance gap, not a broken one.
func toolNamesBackfillCheck(cfg *config.Config) doctorCheck {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return doctorCheck{"tool_names_backfill", statusFail, err.Error()}
	}
	defer st.Close()

	n, err := st.CountEventsAwaitingToolNamesBackfill(context.Background())
	if err != nil {
		return doctorCheck{"tool_names_backfill", statusFail, err.Error()}
	}
	if n == 0 {
		return doctorCheck{"tool_names_backfill", statusPass, "0 row(s) awaiting backfill"}
	}
	return doctorCheck{"tool_names_backfill", statusWarn,
		fmt.Sprintf("%d row(s) awaiting backfill -- run `clens backfill-tool-names --yes`", n)}
}

// dbSchemaCheck reports the database's schema version beside the one this
// binary knows.
//
// After a successful Open the two are equal by construction -- Open migrates
// before it returns -- so the FAIL branch is Open's own failure, which is where
// a version skew surfaces (a database written by a newer clens is refused
// there, not reported as a mismatch here). The number is still worth printing:
// it answers "did this database get brought forward?", which is otherwise only
// answerable by reading the file's header, and reading a WAL-mode SQLite
// file's bytes does not answer it at all.
//
// It opens its own connection rather than reusing sourceHealthRows': the two
// sit either side of the checks block, and doctor is a diagnostic run once by
// hand, not a hot path. Opening here also means a `clens doctor` on a database
// that has never been migrated brings it forward, which is the same side effect
// sourceHealthRows has always had.
func dbSchemaCheck(cfg *config.Config) doctorCheck {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return doctorCheck{"db_schema", statusFail, err.Error()}
	}
	defer st.Close()

	v, err := st.UserVersion(context.Background())
	if err != nil {
		return doctorCheck{"db_schema", statusFail, err.Error()}
	}
	return doctorCheck{"db_schema", statusPass,
		fmt.Sprintf("version %d (this binary: %d)", v, store.SchemaVersion())}
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
