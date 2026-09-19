package pricing

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
)

// TestComputeBatchRoundsPerClass (test 7) ports deepseek-lens's
// TestComputePeakRoundsSumNotTotal to the modifier that actually exists
// here: batch's x0.5 is applied per class, before rounding, and rounding
// the discounted total instead gives a different (wrong) answer.
//
// Fixture: input and output classes each cost exactly $0.01 before the
// batch discount (1000 tokens x $10/MTok). Halved and rounded per class,
// each rounds $0.005 up to $0.01, for a sum of $0.02. Halved and rounded
// once on the $0.02 total instead gives $0.01 -- the two must differ.
func TestComputeBatchRoundsPerClass(t *testing.T) {
	table := Table{
		"test-model": rate("test-model", "10.00", "10.00", "0.20", "shipped"),
	}
	usage := parse.Usage{InputTokens: 1000, OutputTokens: 1000}

	perClass, source := table.Compute("test-model", usage, "", "batch", time.Now())
	if source != "shipped" {
		t.Fatalf("costSource = %q, want shipped", source)
	}
	if perClass == nil {
		t.Fatal("usd = nil, want a priced value")
	}
	if *perClass != 0.02 {
		t.Errorf("per-class-rounded batch total = %v, want 0.02", *perClass)
	}

	// Rounding the discounted total instead: (0.01 + 0.01) * 0.5 = 0.01,
	// rounded once -- the value per-class rounding must NOT match.
	if *perClass == 0.01 {
		t.Error("per-class rounding produced the same result as rounding the discounted total; fixture no longer distinguishes them")
	}
}

// TestPerMTokAcceptsDecimalUSD pins the constructor's decimal-string contract
// against the integer-cent values it replaced: each literal in the shipped
// table moved from cents to a string with the same numeric value, so every
// pre-existing rate must still be bit-identical.
func TestPerMTokAcceptsDecimalUSD(t *testing.T) {
	cases := []struct {
		in   string
		want *big.Rat
	}{
		{"10.00", big.NewRat(1000, 100*1_000_000)}, // the old perMTok(1000)
		{"0.25", big.NewRat(25, 100*1_000_000)},    // the old perMTok(25)
		{"0", big.NewRat(0, 1)},
		// $0.003/MTok -- 0.3 cents. Not an integer number of cents, which is
		// the whole reason the signature is a string.
		{"0.003", big.NewRat(3, 1_000_000_000)},
	}
	for _, c := range cases {
		got := perMTok(c.in)
		if got.Cmp(c.want) != 0 {
			t.Errorf("perMTok(%q) = %s, want %s", c.in, got.RatString(), c.want.RatString())
		}
	}

	if perMTok("0.003").Sign() == 0 {
		t.Error("perMTok(\"0.003\") is zero -- the sub-cent rate was truncated rather than represented")
	}
}

func TestPerMTokPanicsOnUnparseableLiteral(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("perMTok(\"garbage\") did not panic")
		}
	}()
	perMTok("garbage")
}

// TestShippedTableRatesAllNonNil is the backstop for the mechanical row
// update above: a mistyped literal panics at first call, and a nil rate would
// otherwise surface only as a confident wrong cost.
func TestShippedTableRatesAllNonNil(t *testing.T) {
	for model, r := range ShippedTable() {
		for name, v := range map[string]*big.Rat{
			"input_rate":          r.InputRate,
			"output_rate":         r.OutputRate,
			"cache_write_5m_rate": r.CacheWrite5mRate,
			"cache_write_1h_rate": r.CacheWrite1hRate,
			"cache_read_rate":     r.CacheReadRate,
		} {
			if v == nil {
				t.Errorf("%s: %s is nil", model, name)
			}
		}
	}
}

// TestRateExactUsesGivenWriteRates: rateExact exists so a model that bills no
// cache-write premium is expressible at all. The fixture's write rates are
// neither 1.25x nor 2x its input ($12.50 / $20.00), so a regression to rate's
// derived form shows up as a mismatch instead of coincidentally agreeing.
func TestRateExactUsesGivenWriteRates(t *testing.T) {
	r := rateExact("test-exact", "10.00", "50.00", "1.00", "0.07", "0.09", "shipped")

	for _, c := range []struct {
		name string
		got  *big.Rat
		want string
	}{
		{"input_rate", r.InputRate, "10.000000"},
		{"output_rate", r.OutputRate, "50.000000"},
		{"cache_read_rate", r.CacheReadRate, "1.000000"},
		{"cache_write_5m_rate", r.CacheWrite5mRate, "0.070000"}, // not 1.25x input = 12.50
		{"cache_write_1h_rate", r.CacheWrite1hRate, "0.090000"}, // not 2x input = 20.00
	} {
		if got := perTokenToMTok(c.got); got != c.want {
			t.Errorf("%s = %s/MTok, want %s", c.name, got, c.want)
		}
	}

	if r.Source != "shipped" {
		t.Errorf("Source = %q, want shipped", r.Source)
	}
	if !r.EffectiveFrom.Equal(shippedEffectiveFrom) {
		t.Errorf("EffectiveFrom = %v, want %v", r.EffectiveFrom, shippedEffectiveFrom)
	}
}

func TestComputeSixClassesIndependently(t *testing.T) {
	table := ShippedTable()
	usage := parse.Usage{
		InputTokens:        1_000_000,
		OutputTokens:       1_000_000,
		CacheWrite5mTokens: 1_000_000,
		CacheWrite1hTokens: 1_000_000,
		CacheReadTokens:    1_000_000,
		ThinkingTokens:     500_000, // subset of OutputTokens -- must not add to it
	}
	usd, source := table.Compute("claude-sonnet-5", usage, "", "", time.Now())
	if source != "shipped" {
		t.Fatalf("costSource = %q, want shipped", source)
	}
	// input 2.00 + output 10.00 + cache_write_5m (2*1.25) + cache_write_1h (2*2) + cache_read (2*0.1)
	want := 2.00 + 10.00 + 2.50 + 4.00 + 0.20
	if usd == nil || *usd != want {
		t.Errorf("usd = %v, want %v", usd, want)
	}

	// Thinking-only usage (a subset of an unrelated OutputTokens=0 call)
	// must not be priced twice: pricing only ever reads OutputTokens.
	thinkingOnly := parse.Usage{OutputTokens: 100, ThinkingTokens: 100}
	usdThinking, _ := table.Compute("claude-sonnet-5", thinkingOnly, "", "", time.Now())
	usdOutputOnly, _ := table.Compute("claude-sonnet-5", parse.Usage{OutputTokens: 100}, "", "", time.Now())
	if *usdThinking != *usdOutputOnly {
		t.Errorf("usd with ThinkingTokens set = %v, want equal to output-only %v (thinking must not double-count)", *usdThinking, *usdOutputOnly)
	}
}

func TestComputeFastSpeedModifier(t *testing.T) {
	table := ShippedTable()
	usage := parse.Usage{InputTokens: 1_000_000}

	standard, _ := table.Compute("claude-opus-5", usage, "", "", time.Now())
	fast, _ := table.Compute("claude-opus-5", usage, "fast", "", time.Now())
	if standard == nil || fast == nil {
		t.Fatal("expected priced values")
	}
	if *standard != 5.00 {
		t.Errorf("opus-5 standard input cost = %v, want 5.00", *standard)
	}
	if *fast != 10.00 {
		t.Errorf("opus-5 fast input cost = %v, want 10.00", *fast)
	}

	// A model with no shipped fast rate ignores speed="fast".
	sonnetStandard, _ := table.Compute("claude-sonnet-5", usage, "", "", time.Now())
	sonnetFast, _ := table.Compute("claude-sonnet-5", usage, "fast", "", time.Now())
	if *sonnetStandard != *sonnetFast {
		t.Errorf("sonnet-5 fast = %v, standard = %v, want equal (no fast rate shipped)", *sonnetFast, *sonnetStandard)
	}
}

func TestComputePriorityAppliesNoChange(t *testing.T) {
	table := ShippedTable()
	usage := parse.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	standard, _ := table.Compute("claude-sonnet-5", usage, "", "", time.Now())
	priority, _ := table.Compute("claude-sonnet-5", usage, "", "priority", time.Now())
	if standard == nil || priority == nil || *standard != *priority {
		t.Errorf("priority = %v, standard = %v, want equal (no rate change)", priority, standard)
	}
}

func TestCacheReadRatesPerModel(t *testing.T) {
	table := ShippedTable()
	usage := parse.Usage{CacheReadTokens: 1_000_000}

	fable51, _ := table.Compute("claude-fable-5-1", usage, "", "", time.Now())
	fable5, _ := table.Compute("claude-fable-5", usage, "", "", time.Now())
	mythos5, _ := table.Compute("claude-mythos-5", usage, "", "", time.Now())
	mythos51, _ := table.Compute("claude-mythos-5-1", usage, "", "", time.Now())

	if *fable51 != 0.25 {
		t.Errorf("fable-5-1 cache read = %v, want 0.25", *fable51)
	}
	if *fable5 != 1.00 || *mythos5 != 1.00 {
		t.Errorf("fable-5/mythos-5 cache read = %v/%v, want 1.00/1.00", *fable5, *mythos5)
	}
	if *fable5/(*fable51) != 4 {
		t.Errorf("fable-5 / fable-5-1 cache read ratio = %v, want 4", *fable5/(*fable51))
	}
	if *mythos51 != *fable51 {
		t.Errorf("mythos-5-1 cache read = %v, want equal to fable-5-1 %v (same per-token pricing)", *mythos51, *fable51)
	}
}

func TestUnknownModelIsUnpriced(t *testing.T) {
	table := ShippedTable()
	usd, source := table.Compute("claude-does-not-exist", parse.Usage{InputTokens: 100}, "", "", time.Now())
	if source != "unpriced" {
		t.Errorf("costSource = %q, want unpriced", source)
	}
	if usd != nil {
		t.Errorf("usd = %v, want nil (never a numeric 0)", *usd)
	}
}

func TestTTLUnknownReturnsApproximate(t *testing.T) {
	table := ShippedTable()
	usage := parse.Usage{InputTokens: 100, CacheWrite5mTokens: 1000, TTLUnknown: true}
	usd, source := table.Compute("claude-sonnet-5", usage, "", "", time.Now())
	if usd == nil {
		t.Fatal("expected a priced value")
	}
	if source != "approximate:cache_ttl_unknown" {
		t.Errorf("costSource = %q, want approximate:cache_ttl_unknown", source)
	}
}

func TestProvisionalRowCarriesVerificationNote(t *testing.T) {
	table := ShippedTable()
	for _, model := range []string{"claude-opus-4-7", "claude-opus-4-6", "claude-haiku-4-5"} {
		r := table[model]
		if r.Source != "provisional" {
			t.Errorf("%s: Source = %q, want provisional", model, r.Source)
		}
	}
	for _, model := range []string{"claude-sonnet-5", "claude-opus-5", "claude-mythos-5-1"} {
		r := table[model]
		if r.Source != "shipped" {
			t.Errorf("%s: Source = %q, want shipped", model, r.Source)
		}
	}
}

func TestSetOverrideRoundTripWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.toml")

	usage := parse.Usage{InputTokens: 1_000_000}
	shipped, _ := ShippedTable().Compute("claude-sonnet-5", usage, "", "", time.Now())

	custom := Rate{
		Model:     "claude-sonnet-5",
		InputRate: big.NewRat(1, 100_000), // $10/MTok, well above the shipped $2/MTok
	}
	if err := SetOverride(path, custom); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}

	overrides, err := LoadOverrides(path)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	r, ok := overrides["claude-sonnet-5"]
	if !ok {
		t.Fatal("override not found after SetOverride")
	}
	if r.Source != "user" {
		t.Errorf("Source = %q, want user", r.Source)
	}

	loader := NewLoader(path)
	overridden, source := loader.Compute("claude-sonnet-5", usage, "", "", time.Now())
	if source != "user" {
		t.Errorf("costSource = %q, want user", source)
	}
	if overridden == nil || shipped == nil || *overridden == *shipped {
		t.Errorf("overridden = %v, shipped = %v, want the user rate to win (differ)", overridden, shipped)
	}

	if err := UnsetOverride(path, "claude-sonnet-5"); err != nil {
		t.Fatalf("UnsetOverride: %v", err)
	}
	afterUnset, err := LoadOverrides(path)
	if err != nil {
		t.Fatalf("LoadOverrides after unset: %v", err)
	}
	if _, ok := afterUnset["claude-sonnet-5"]; ok {
		t.Error("override still present after UnsetOverride")
	}
}

func TestLoaderReloadsOnFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.toml")
	loader := NewLoader(path)
	usage := parse.Usage{InputTokens: 1_000_000}

	before, _ := loader.Compute("claude-sonnet-5", usage, "", "", time.Now())

	if err := SetOverride(path, Rate{Model: "claude-sonnet-5", InputRate: big.NewRat(1, 100_000)}); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	// Ensure the new mtime is observably later than whatever the
	// filesystem's timestamp resolution already gave the first load.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	after, source := loader.Compute("claude-sonnet-5", usage, "", "", time.Now())
	if source != "user" {
		t.Errorf("costSource after file change = %q, want user (loader did not reload)", source)
	}
	if *after == *before {
		t.Error("Loader did not pick up the override after the file changed")
	}
}
