// Package config loads claude-lens configuration from flags, environment
// variables, and a config file, in that order of precedence, falling back to
// built-in defaults.
package config

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Account is one configured account clens can attribute captured traffic to.
// A user holding both a Claude subscription and a separate API key for the
// same underlying account is two Accounts, one per BillingMode — never one
// Account with two billing modes — because a captured call's auth_kind
// resolves to exactly one billing lineage (see br-GI-1-08).
type Account struct {
	Name        string
	BillingMode string // "subscription" | "api"
	Plan        string // e.g. "pro", "max5x", "max20x", "team", "enterprise"; empty for "api"
}

// Config is the effective, fully-resolved configuration for clens.
type Config struct {
	ProxyAddr     string
	DashboardAddr string
	UpstreamURL   string
	DBPath        string
	BodyPolicy    string // "full" | "off"

	// BodyCapBytes bounds each of the two bodies independently -- the request
	// and the response are teed separately, so one cap applies to each.
	//
	// It is deliberately generous. The dominant body is the *request*, not the
	// response: a Claude Code request carries the system prompt, the full tool
	// schemas and the conversation history, so a cap set for a typical response
	// truncates the majority of calls rather than the exception. A truncated
	// body is not merely less detail -- it is the one input the cost and usage
	// figures cannot be recovered without (GI-11's RC-C). It stays configurable
	// so it can be lowered without a rebuild.
	BodyCapBytes      int
	AllowRemote       bool
	SessionGapMinutes int
	RetentionDays     int
	// HotDays is how many days a body stays in the live DB before the archiver
	// moves it to a day file. 0 disables archival (bodies stay hot forever).
	HotDays       int
	ReplayEnabled     bool
	AccountsPath      string
	Accounts          []Account

	// Both are deliberately absent from Default(): nil means "no key set ->
	// use the shipped default", which is a distinct state from an explicitly
	// empty list (the `none` sentinel in applyKV).
	PeakOffPeakDates []string
	ApiModelPrefixes []string

	// PprofAddr, when set, serves net/http/pprof on this address for the
	// life of the process -- a diagnostic, not a feature to turn on by
	// default, so it is deliberately absent from Default() (the same "unset
	// unless asked for" note PeakOffPeakDates carries above). A profile is a
	// dump of whatever is in memory, which for this process includes prompt
	// and response bodies, so Validate rejects a non-loopback value even
	// when AllowRemote is true (D5).
	PprofAddr string
}

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		ProxyAddr:         "127.0.0.1:8797",
		DashboardAddr:     "127.0.0.1:8798",
		UpstreamURL:       "https://api.anthropic.com",
		DBPath:            defaultPath("lens.db"),
		BodyPolicy:        "full",
		BodyCapBytes:      2097152,
		AllowRemote:       false,
		SessionGapMinutes: 30,
		RetentionDays:     0,
		HotDays:           7,
		ReplayEnabled:     false,
		AccountsPath:      defaultPath("accounts.toml"),
	}
}

// userHomeDir resolves the home directory, preferring $HOME so tests (and
// users) can override it uniformly across platforms.
func userHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func defaultPath(name string) string {
	home := userHomeDir()
	if home == "" {
		return filepath.Join(".clens", name)
	}
	return filepath.Join(home, ".clens", name)
}

func configFilePath() string {
	return defaultPath("config.toml")
}

// fieldsByEnv maps CLENS_* environment variable names to Config field names.
var fieldsByEnv = map[string]string{
	"CLENS_PROXY_ADDR":          "ProxyAddr",
	"CLENS_DASHBOARD_ADDR":      "DashboardAddr",
	"CLENS_UPSTREAM_URL":        "UpstreamURL",
	"CLENS_DB_PATH":             "DBPath",
	"CLENS_BODY_POLICY":         "BodyPolicy",
	"CLENS_BODY_CAP_BYTES":      "BodyCapBytes",
	"CLENS_ALLOW_REMOTE":        "AllowRemote",
	"CLENS_SESSION_GAP_MINUTES": "SessionGapMinutes",
	"CLENS_RETENTION_DAYS":      "RetentionDays",
	"CLENS_HOT_DAYS":            "HotDays",
	"CLENS_REPLAY_ENABLED":      "ReplayEnabled",
	"CLENS_ACCOUNTS_PATH":       "AccountsPath",
	"CLENS_PEAK_OFF_PEAK_DATES": "PeakOffPeakDates",
	"CLENS_API_MODEL_PREFIXES":  "ApiModelPrefixes",
	"CLENS_PPROF_ADDR":          "PprofAddr",
}

// splitList parses a comma-separated value into a slice, trimming blank
// entries. The `none` sentinel yields a non-nil empty slice: applyKV skips a
// blank value entirely, so `none` is the only way a file can say "explicitly
// empty" rather than "unset".
func splitList(val string) []string {
	if val == "none" {
		return []string{}
	}
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envKV() map[string]string {
	kv := map[string]string{}
	for env, field := range fieldsByEnv {
		if v := os.Getenv(env); v != "" {
			kv[field] = v
		}
	}
	return kv
}

// parseFlatFile parses a minimal flat `key = value` file: blank lines and
// lines starting with '#' are ignored, every other line must contain '=',
// and a value may optionally be wrapped in double quotes.
//
// ponytail: hand-rolled flat reader; swap in a real TOML library if nested
// config ever appears beyond the accounts file's section-per-block shape.
func parseFlatFile(data []byte) (map[string]string, error) {
	kv := map[string]string{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			return nil, fmt.Errorf("config: parse flat file: line %d: missing '=': %q", i+1, line)
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if strings.HasPrefix(val, `"`) {
			if len(val) < 2 || !strings.HasSuffix(val, `"`) {
				return nil, fmt.Errorf("config: parse flat file: line %d: unterminated quote: %q", i+1, line)
			}
			val = val[1 : len(val)-1]
		}
		kv[key] = val
	}
	return kv, nil
}

// applyKV sets cfg fields named by kv's keys (Config field names) to kv's
// values, converting to each field's type.
func applyKV(cfg *Config, kv map[string]string) error {
	for key, val := range kv {
		if val == "" {
			continue
		}
		var err error
		switch key {
		case "ProxyAddr":
			cfg.ProxyAddr = val
		case "DashboardAddr":
			cfg.DashboardAddr = val
		case "UpstreamURL":
			cfg.UpstreamURL = val
		case "DBPath":
			cfg.DBPath = val
		case "BodyPolicy":
			cfg.BodyPolicy = val
		case "BodyCapBytes":
			cfg.BodyCapBytes, err = strconv.Atoi(val)
		case "AllowRemote":
			cfg.AllowRemote, err = strconv.ParseBool(val)
		case "SessionGapMinutes":
			cfg.SessionGapMinutes, err = strconv.Atoi(val)
		case "RetentionDays":
			cfg.RetentionDays, err = strconv.Atoi(val)
		case "HotDays":
			cfg.HotDays, err = strconv.Atoi(val)
		case "ReplayEnabled":
			cfg.ReplayEnabled, err = strconv.ParseBool(val)
		case "AccountsPath":
			cfg.AccountsPath = val
		case "PeakOffPeakDates":
			cfg.PeakOffPeakDates = splitList(val)
		case "ApiModelPrefixes":
			cfg.ApiModelPrefixes = splitList(val)
		case "PprofAddr":
			cfg.PprofAddr = val
		default:
			return fmt.Errorf("config: apply: unknown key %q", key)
		}
		if err != nil {
			return fmt.Errorf("config: apply: key %s: invalid value %q: %w", key, val, err)
		}
	}
	return nil
}

// applyFlags overlays cfg with any flags explicitly passed in args. Each
// flag's default is cfg's current (file/env-resolved) value, so a flag the
// caller did not pass never overrides what came before it.
func applyFlags(cfg *Config, args []string) error {
	fs := flag.NewFlagSet("clens", flag.ContinueOnError)
	fs.StringVar(&cfg.ProxyAddr, "proxy-addr", cfg.ProxyAddr, "proxy listen address")
	fs.StringVar(&cfg.DashboardAddr, "dashboard-addr", cfg.DashboardAddr, "dashboard listen address")
	fs.StringVar(&cfg.UpstreamURL, "upstream-url", cfg.UpstreamURL, "upstream Anthropic API URL")
	fs.StringVar(&cfg.DBPath, "db-path", cfg.DBPath, "SQLite database path")
	fs.StringVar(&cfg.BodyPolicy, "body-policy", cfg.BodyPolicy, "body capture policy: full|off")
	fs.IntVar(&cfg.BodyCapBytes, "body-cap-bytes", cfg.BodyCapBytes, "max bytes captured per body (default 2097152)")
	fs.BoolVar(&cfg.AllowRemote, "allow-remote", cfg.AllowRemote, "allow non-loopback bind addresses")
	fs.IntVar(&cfg.SessionGapMinutes, "session-gap-minutes", cfg.SessionGapMinutes, "minutes of inactivity before a new session")
	fs.IntVar(&cfg.RetentionDays, "retention-days", cfg.RetentionDays, "purge requests older than this many days; 0 means keep forever")
	fs.IntVar(&cfg.HotDays, "hot-days", cfg.HotDays, "days a body stays in the live DB before it is archived; 0 disables archival")
	fs.BoolVar(&cfg.ReplayEnabled, "replay", cfg.ReplayEnabled, "enable the replay endpoint")
	fs.StringVar(&cfg.AccountsPath, "accounts-path", cfg.AccountsPath, "accounts file path")
	fs.StringVar(&cfg.PprofAddr, "pprof-addr", cfg.PprofAddr, "serve net/http/pprof on this loopback address (diagnostic; empty disables it)")
	return fs.Parse(args)
}

// Load resolves configuration with precedence flags > env (CLENS_*) > config
// file (~/.clens/config.toml) > defaults, then loads the accounts file (if
// present — accounts are optional).
func Load(args []string) (*Config, error) {
	cfg := Default()

	path := configFilePath()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		kv, perr := parseFlatFile(data)
		if perr != nil {
			return nil, fmt.Errorf("config: load: config file %s: %w", path, perr)
		}
		if aerr := applyKV(cfg, kv); aerr != nil {
			return nil, fmt.Errorf("config: load: config file %s: %w", path, aerr)
		}
	case os.IsNotExist(err):
		// no config file — fine, defaults stand.
	default:
		return nil, fmt.Errorf("config: load: reading config file %s: %w", path, err)
	}

	if err := applyKV(cfg, envKV()); err != nil {
		return nil, fmt.Errorf("config: load: environment: %w", err)
	}

	if err := applyFlags(cfg, args); err != nil {
		return nil, err
	}

	accounts, err := loadAccounts(cfg.AccountsPath)
	if err != nil {
		return nil, fmt.Errorf("config: load: accounts file %s: %w", cfg.AccountsPath, err)
	}
	cfg.Accounts = accounts

	return cfg, nil
}

// loadAccounts reads the accounts file at path, if present. Each account is
// a blank-line-separated block of `key = value` lines; a name configured
// with both a subscription and an API key is written as two blocks sharing
// the same name, and parses as two Accounts — never merged into one, since a
// captured call's billing lineage is always exactly one of the two.
func loadAccounts(path string) ([]Account, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var accounts []Account
	for _, block := range strings.Split(string(data), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		kv, err := parseFlatFile([]byte(block))
		if err != nil {
			return nil, err
		}
		if len(kv) == 0 {
			continue
		}
		accounts = append(accounts, Account{
			Name:        kv["name"],
			BillingMode: kv["billing_mode"],
			Plan:        kv["plan"],
		})
	}
	return accounts, nil
}

// Validate rejects configuration values that would be unsafe or nonsensical.
// Errors name the offending field and value.
func (c *Config) Validate() error {
	if err := validateLoopback("ProxyAddr", c.ProxyAddr, c.AllowRemote); err != nil {
		return err
	}
	if err := validateLoopback("DashboardAddr", c.DashboardAddr, c.AllowRemote); err != nil {
		return err
	}
	switch c.BodyPolicy {
	case "full", "off":
	default:
		// "truncated" was accepted here and read nowhere: full and truncated
		// executed byte-identical code, both bounded by BodyCapBytes, because a
		// single cap leaves a second value nothing to control. It is rejected
		// outright rather than aliased to "full", because on a flag whose whole
		// job is deciding what content reaches the database, a value that reads
		// as "narrow it" and stores the body whole is the one outcome worth
		// failing loudly over. The vocabulary is inherited from deepseek-lens,
		// where the policy is likewise applied only for "off".
		return fmt.Errorf("config: validate: BodyPolicy: invalid value %q (want full or off)", c.BodyPolicy)
	}
	if c.BodyCapBytes <= 0 {
		return fmt.Errorf("config: validate: BodyCapBytes: must be positive, got %d", c.BodyCapBytes)
	}
	u, err := url.Parse(c.UpstreamURL)
	if err != nil {
		return fmt.Errorf("config: validate: UpstreamURL: invalid value %q: %w", c.UpstreamURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: validate: UpstreamURL: invalid value %q (scheme must be http or https)", c.UpstreamURL)
	}
	if u.Host == "" {
		return fmt.Errorf("config: validate: UpstreamURL: invalid value %q (missing host)", c.UpstreamURL)
	}
	if c.SessionGapMinutes <= 0 {
		return fmt.Errorf("config: validate: SessionGapMinutes: must be positive, got %d", c.SessionGapMinutes)
	}
	if c.RetentionDays < 0 {
		return fmt.Errorf("config: validate: RetentionDays: must not be negative, got %d", c.RetentionDays)
	}
	if c.HotDays < 0 {
		return fmt.Errorf("config: validate: HotDays: must not be negative, got %d", c.HotDays)
	}
	if c.HotDays > 0 && c.RetentionDays > 0 && c.HotDays > c.RetentionDays {
		return fmt.Errorf("config: validate: HotDays exceeds RetentionDays (%d > %d): nothing would ever be archived before it is deleted", c.HotDays, c.RetentionDays)
	}
	// A typo'd date silently stays in peak, which over-charges, so it is
	// rejected here rather than absorbed.
	for _, d := range c.PeakOffPeakDates {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return fmt.Errorf("config: validate: PeakOffPeakDates: invalid value %q (want YYYY-MM-DD)", d)
		}
	}
	for _, p := range c.ApiModelPrefixes {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("config: validate: ApiModelPrefixes: empty or whitespace-only prefix")
		}
	}
	if c.PprofAddr != "" {
		// The literal false is the point (D5): validateLoopback returns nil
		// immediately when allowRemote is true, so passing c.AllowRemote
		// here would let --allow-remote open a listener whose heap profile
		// contains whatever is in memory. This listener is loopback-only
		// and --allow-remote cannot widen it.
		if err := validateLoopback("PprofAddr", c.PprofAddr, false); err != nil {
			return err
		}
	}
	return nil
}

func validateLoopback(field, addr string, allowRemote bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: validate: %s: invalid address %q: %w", field, addr, err)
	}
	if allowRemote {
		return nil
	}
	if IsLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("config: validate: %s: non-loopback address %q requires AllowRemote", field, addr)
}

// IsLoopbackHost reports whether host -- the host part of an address, not a
// full "host:port" -- names the local machine: "localhost" or any loopback
// IP (127.0.0.1, ::1, and every other address in the loopback ranges, not
// just the two conventional ones). It is the one home for that predicate:
// both validateLoopback above and internal/cli's pprof listener call it, so
// a legitimately-loopback address like 127.0.0.2 cannot pass Validate and
// then be refused by a second, stricter spelling of the same check.
//
// "localhost" is checked before parsing as an IP because net.ParseIP
// returns nil for it -- an IP-only check would silently reject a working
// --proxy-addr localhost.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
