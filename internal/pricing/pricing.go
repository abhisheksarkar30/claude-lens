// Package pricing computes the per-call cost of a Claude API response.
// Six token classes are priced independently and rounded per class, then
// summed — never rounded once on the total — because the per-class figure
// is the one an invoice line reproduces.
package pricing

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
)

// Rate is one model's per-token pricing. Every rate is $ per token, kept as
// an exact big.Rat so a modifier like 1.25x never introduces binary-float
// drift before rounding. FastInputRate/FastOutputRate are nil for a model
// with no fast-mode rate.
type Rate struct {
	Model            string
	InputRate        *big.Rat
	OutputRate       *big.Rat
	CacheWrite5mRate *big.Rat
	CacheWrite1hRate *big.Rat
	CacheReadRate    *big.Rat
	FastInputRate    *big.Rat
	FastOutputRate   *big.Rat
	EffectiveFrom    time.Time
	Source           string // "shipped" | "provisional" | "user"
	Peak             *PeakWindow
}

// PeakWindow describes a model whose rate varies by time of day. A nil
// *PeakWindow on a Rate means the model is flat-priced, which is why the
// window is per-Rate and never global: ShippedTable() legitimately mixes
// flat-priced Anthropic rows with time-varying DeepSeek ones, and an
// unconditional multiply would double every Claude cost.
type PeakWindow struct {
	Multiplier   *big.Rat            // peak = off-peak x Multiplier (DeepSeek: 2)
	Hours        [][2]int            // [start,end) UTC hour ranges: {{1,4},{6,10}}
	OffPeakDates map[string]struct{} // "2006-01-02" UTC dates excluded from peak
}

// IsPeak reports whether at falls in the peak window: UTC, Monday-Friday, not
// an OffPeakDates date, and inside one of the [start,end) hour ranges. The
// caller is expected to have checked the window is non-nil.
func (w *PeakWindow) IsPeak(at time.Time) bool {
	utc := at.UTC()
	if d := utc.Weekday(); d == time.Saturday || d == time.Sunday {
		return false
	}
	if _, off := w.OffPeakDates[utc.Format("2006-01-02")]; off {
		return false
	}
	h := utc.Hour()
	for _, span := range w.Hours {
		if h >= span[0] && h < span[1] {
			return true
		}
	}
	return false
}

// Table is a rate table keyed by model id.
type Table map[string]Rate

// centsPerUnit is the rounding granularity Compute uses per class: US
// cents, matching what an invoice line actually shows.
var centsPerUnit = big.NewRat(1, 100)

// Compute prices usage against ShippedTable — the convenience entry point
// matching the plan's literal signature. A caller holding a Loader-backed
// or edited Table should call Table.Compute directly instead so a user
// override actually takes effect.
func Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd *float64, costSource string) {
	return ShippedTable().Compute(model, usage, speed, serviceTier, at)
}

// Compute prices usage for model using t. A model absent from t returns
// (nil, "unpriced") — never a numeric 0 a caller could store as $0.00.
// usage.TTLUnknown (the flat-cache-creation fallback) returns
// "approximate:cache_ttl_unknown": the flat count was already folded into
// CacheWrite5mTokens by parse.ExtractUsage, so it prices at the 5-minute
// rate, but the true rate could be 1.25x or 2x and this says so rather
// than picking one silently.
func (t Table) Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd *float64, costSource string) {
	r, ok := t[model]
	if !ok {
		return nil, "unpriced"
	}

	inputRate, outputRate := r.InputRate, r.OutputRate
	if speed == "fast" && r.FastInputRate != nil {
		inputRate, outputRate = r.FastInputRate, r.FastOutputRate
	}

	batch := serviceTier == "batch"
	// service_tier == "priority" applies no rate change: the skill bundle
	// documents no per-token Priority rate, so none is asserted here.

	// Peak is resolved once per call and applied per class, in the same place
	// batch halving sits: two exact multiplications, one rounding. Multiplying
	// the already-rounded total instead would drift, the same way it would for
	// batch (TestComputeBatchRoundsPerClass guards that half).
	peak := r.Peak != nil && r.Peak.IsPeak(at)

	total := new(big.Rat)
	for _, class := range []struct {
		tokens int
		rate   *big.Rat
	}{
		{usage.InputTokens, inputRate},
		{usage.OutputTokens, outputRate}, // ThinkingTokens is a subset of OutputTokens, never priced again
		{usage.CacheWrite5mTokens, r.CacheWrite5mRate},
		{usage.CacheWrite1hTokens, r.CacheWrite1hRate},
		{usage.CacheReadTokens, r.CacheReadRate},
	} {
		if class.tokens == 0 {
			continue
		}
		cost := new(big.Rat).Mul(big.NewRat(int64(class.tokens), 1), class.rate)
		if batch {
			cost.Mul(cost, big.NewRat(1, 2))
		}
		if peak {
			cost.Mul(cost, r.Peak.Multiplier)
		}
		total.Add(total, roundHalfUp(cost, centsPerUnit))
	}

	f, _ := total.Float64()
	source := r.Source
	if usage.TTLUnknown {
		source = "approximate:cache_ttl_unknown"
	}
	return &f, source
}

// roundHalfUp rounds r to the nearest multiple of unit (e.g. 1/100 for
// cents), rounding a tie up. r and unit are always non-negative here (token
// counts and rates never go negative), so exact-integer round-half-up is
// floor((2p+q)/(2q)) for scaled = r/unit = p/q in lowest terms.
func roundHalfUp(r *big.Rat, unit *big.Rat) *big.Rat {
	scaled := new(big.Rat).Quo(r, unit)
	p, q := scaled.Num(), scaled.Denom()
	numerator := new(big.Int).Add(new(big.Int).Lsh(p, 1), q)
	rounded := new(big.Int).Quo(numerator, new(big.Int).Lsh(q, 1))
	return new(big.Rat).Mul(new(big.Rat).SetInt(rounded), unit)
}

// DefaultPath is the user-editable price-override file: a shipped rate
// wins unless a model here overrides it, and an override always carries
// Source "user" (br-GI-1-17's `clens prices --set/--unset/--edit` writes
// here; the CLI wiring is that bead's, these save/validate functions are
// this one's).
func DefaultPath() string {
	home := userHomeDir()
	if home == "" {
		return filepath.Join(".clens", "prices.toml")
	}
	return filepath.Join(home, ".clens", "prices.toml")
}

func userHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// rateFields maps each override file key to the Rate field it fills, all
// expressed in USD-per-million-tokens (the same units the shipped table's
// citations use) so an edited file reads the same way the plan documents
// rates.
var rateFields = map[string]func(*Rate) **big.Rat{
	"input_rate":          func(r *Rate) **big.Rat { return &r.InputRate },
	"output_rate":         func(r *Rate) **big.Rat { return &r.OutputRate },
	"cache_read_rate":     func(r *Rate) **big.Rat { return &r.CacheReadRate },
	"cache_write_5m_rate": func(r *Rate) **big.Rat { return &r.CacheWrite5mRate },
	"cache_write_1h_rate": func(r *Rate) **big.Rat { return &r.CacheWrite1hRate },
	"fast_input_rate":     func(r *Rate) **big.Rat { return &r.FastInputRate },
	"fast_output_rate":    func(r *Rate) **big.Rat { return &r.FastOutputRate },
}

func mtokToPerToken(decimal string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(decimal)
	if !ok {
		return nil, fmt.Errorf("pricing: invalid rate %q", decimal)
	}
	return r.Quo(r, big.NewRat(1_000_000, 1)), nil
}

func perTokenToMTok(r *big.Rat) string {
	return new(big.Rat).Mul(r, big.NewRat(1_000_000, 1)).FloatString(6)
}

// parseOverrideBlock parses one blank-line-delimited `key = value` block —
// the same flat shape internal/config uses for the accounts file.
func parseOverrideBlock(block string) (map[string]string, error) {
	kv := map[string]string{}
	for i, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			return nil, fmt.Errorf("pricing: parse override: line %d: missing '=': %q", i+1, line)
		}
		key := strings.TrimSpace(line[:idx])
		kv[key] = strings.Trim(strings.TrimSpace(line[idx+1:]), `"`)
	}
	return kv, nil
}

// LoadOverrides reads the user price-override file at path. A missing file
// is not an error — it means no overrides are configured yet.
func LoadOverrides(path string) (Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Table{}, nil
		}
		return nil, fmt.Errorf("pricing: LoadOverrides: %w", err)
	}

	t := Table{}
	for _, block := range strings.Split(string(data), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		kv, err := parseOverrideBlock(block)
		if err != nil {
			return nil, fmt.Errorf("pricing: LoadOverrides: %s: %w", path, err)
		}
		model := kv["model"]
		if model == "" {
			continue
		}
		r := Rate{Model: model, Source: "user", EffectiveFrom: time.Now()}
		for key, field := range rateFields {
			v, ok := kv[key]
			if !ok {
				continue
			}
			rate, err := mtokToPerToken(v)
			if err != nil {
				return nil, fmt.Errorf("pricing: LoadOverrides: %s: model %s: %w", path, model, err)
			}
			*field(&r) = rate
		}
		if r.CacheWrite5mRate == nil && r.InputRate != nil {
			r.CacheWrite5mRate = new(big.Rat).Mul(r.InputRate, big.NewRat(5, 4))
		}
		if r.CacheWrite1hRate == nil && r.InputRate != nil {
			r.CacheWrite1hRate = new(big.Rat).Mul(r.InputRate, big.NewRat(2, 1))
		}
		t[model] = r
	}
	return t, nil
}

// SaveOverrides writes t to path in full, replacing whatever was there —
// SetOverride/UnsetOverride call this after editing the in-memory table
// loaded by LoadOverrides, so the file always reflects the whole override
// set, not just one model's edit.
func SaveOverrides(path string, t Table) error {
	models := make([]string, 0, len(t))
	for m := range t {
		models = append(models, m)
	}
	sort.Strings(models)

	var sb strings.Builder
	for i, m := range models {
		if i > 0 {
			sb.WriteString("\n")
		}
		r := t[m]
		fmt.Fprintf(&sb, "model = %q\n", r.Model)
		for key, field := range rateFields {
			if v := *field(&r); v != nil {
				fmt.Fprintf(&sb, "%s = %s\n", key, perTokenToMTok(v))
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("pricing: SaveOverrides: %w", err)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		return fmt.Errorf("pricing: SaveOverrides: %w", err)
	}
	return nil
}

// SetOverride adds or replaces r in path's override file. Source is always
// forced to "user" — a user edit wins over the shipped row unconditionally.
func SetOverride(path string, r Rate) error {
	t, err := LoadOverrides(path)
	if err != nil {
		return err
	}
	r.Source = "user"
	if r.EffectiveFrom.IsZero() {
		r.EffectiveFrom = time.Now()
	}
	t[r.Model] = r
	return SaveOverrides(path, t)
}

// UnsetOverride removes model's override, reverting it to the shipped row
// (or to "unpriced" if it has none).
func UnsetOverride(path, model string) error {
	t, err := LoadOverrides(path)
	if err != nil {
		return err
	}
	delete(t, model)
	return SaveOverrides(path, t)
}

// Loader merges ShippedTable() with path's override file and re-reads the
// file when its mtime moves forward, so a `clens prices --set` takes
// effect without a restart.
type Loader struct {
	path string

	mu    sync.Mutex
	mtime time.Time
	table Table
}

// Path is the override file this Loader reads. A caller that wants to write
// an override (POST /api/prices, `clens prices --set`) needs the file the
// reader is actually watching -- the two must be the same file or a write
// would never be observed.
func (l *Loader) Path() string { return l.path }

// NewLoader returns a Loader for the override file at path, performing an
// initial load immediately.
func NewLoader(path string) *Loader {
	l := &Loader{path: path}
	l.reload()
	return l
}

func (l *Loader) reload() {
	merged := ShippedTable()
	if overrides, err := LoadOverrides(l.path); err == nil {
		for model, r := range overrides {
			merged[model] = r
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.table = merged
	if info, err := os.Stat(l.path); err == nil {
		l.mtime = info.ModTime()
	}
}

func (l *Loader) reloadIfStale() {
	info, err := os.Stat(l.path)
	if err != nil {
		return // no override file yet -- the shipped-only table stands
	}
	l.mu.Lock()
	stale := info.ModTime().After(l.mtime)
	l.mu.Unlock()
	if stale {
		l.reload()
	}
}

// Table returns the current merged table, reloading first if the override
// file changed since the last load.
func (l *Loader) Table() Table {
	l.reloadIfStale()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.table
}

// Compute prices usage against the Loader's current table.
func (l *Loader) Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (*float64, string) {
	return l.Table().Compute(model, usage, speed, serviceTier, at)
}
