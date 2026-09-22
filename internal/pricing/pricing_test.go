package pricing

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
)

// TestComputeBatchHalvesExactly is the GI-11 rewrite of the test that used to
// pin per-class rounding (TestComputeBatchRoundsPerClass). It keeps what that
// test actually guarded -- the x0.5 lands on each class's own cost, not on the
// summed total -- and drops the rounding it accidentally also asserted.
//
// Fixture: input and output classes each cost exactly $0.01 before the batch
// discount (1000 tokens x $10/MTok). Halved, the call is $0.005 + $0.005 =
// $0.01. Halving the summed total once gives the same $0.01, so the two
// orderings no longer differ at this size -- what distinguishes them is that
// the discount is applied at all: undiscounted the fixture is $0.02.
func TestComputeBatchHalvesExactly(t *testing.T) {
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
	if *perClass != 0.01 {
		t.Errorf("batch total = %v, want the exact halved 0.01", *perClass)
	}

	unbatched, _ := table.Compute("test-model", usage, "", "", time.Now())
	if unbatched == nil || *unbatched != 0.02 {
		t.Fatalf("unbatched total = %v, want 0.02", unbatched)
	}
	if *perClass == *unbatched {
		t.Error("batch priced identically to no modifier; the x0.5 halving is not being applied")
	}
}

// TestComputePricesSubCentClassesExactly is the direct inversion of the
// behaviour GI-11 removed: a call whose every class is sub-cent must price
// above zero. 300 tokens x $10/MTok is $0.003 per class, and under the old
// per-class cent rounding each class became $0.00, so the call stored
// exactly $0.000000 however many tokens it carried.
func TestComputePricesSubCentClassesExactly(t *testing.T) {
	table := Table{
		"test-model": rate("test-model", "10.00", "10.00", "0.20", "shipped"),
	}
	usage := parse.Usage{InputTokens: 300, OutputTokens: 300}

	usd, source := table.Compute("test-model", usage, "", "", time.Now())
	if source != "shipped" {
		t.Fatalf("costSource = %q, want shipped", source)
	}
	if usd == nil {
		t.Fatal("usd = nil, want a priced value")
	}
	if *usd == 0 {
		t.Fatal("a call costing $0.003 per class priced at exactly zero; per-class cent rounding is back")
	}
	if *usd != 0.006 {
		t.Errorf("usd = %v, want the exact sum 0.006", *usd)
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

	loader := NewLoader(path, nil)
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
	loader := NewLoader(path, nil)
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

// --- Peak pricing (br-GI-3-02) ---

// testPeakWindow is the shipped DeepSeek shape: 2x, 01:00-04:00 and
// 06:00-10:00 UTC, no excluded dates.
func testPeakWindow() *PeakWindow {
	return &PeakWindow{
		Multiplier:   big.NewRat(2, 1),
		Hours:        [][2]int{{1, 4}, {6, 10}},
		OffPeakDates: map[string]struct{}{},
	}
}

// fixtureInstant builds a UTC instant, failing the test if that date is not
// the weekday the case assumes -- a boundary table that silently lands on a
// Saturday would pass for the wrong reason.
func fixtureInstant(t *testing.T, y int, m time.Month, d, h, min int, want time.Weekday) time.Time {
	t.Helper()
	if got := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday(); got != want {
		t.Fatalf("fixture date %04d-%02d-%02d is a %s, want %s", y, m, d, got, want)
	}
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

// peakFixtureTable is a one-model table at $10/MTok with a peak window, so
// T3 is self-contained rather than depending on the shipped DeepSeek rows
// br-GI-3-03 adds.
func peakFixtureTable() Table {
	return Table{
		"peak-model": {
			Model:            "peak-model",
			InputRate:        perMTok("10.00"),
			OutputRate:       perMTok("10.00"),
			CacheWrite5mRate: perMTok("0"),
			CacheWrite1hRate: perMTok("0"),
			CacheReadRate:    perMTok("0"),
			Source:           "shipped",
			Peak:             testPeakWindow(),
		},
	}
}

// T1: the hour edge is the single most likely bug here, and it produces
// plausible-looking costs rather than an error.
func TestIsPeakBoundaryTable(t *testing.T) {
	w := testPeakWindow()
	// Monday 2026-09-21; window is [01:00,04:00) and [06:00,10:00) UTC.
	cases := []struct {
		h, min int
		want   bool
	}{
		{0, 59, false},
		{1, 0, true},
		{3, 59, true},
		{4, 0, false},
		{5, 59, false},
		{6, 0, true},
		{9, 59, true},
		{10, 0, false},
	}
	for _, c := range cases {
		at := fixtureInstant(t, 2026, time.September, 21, c.h, c.min, time.Monday)
		if got := w.IsPeak(at); got != c.want {
			t.Errorf("IsPeak(%s) = %v, want %v", at.Format(time.RFC3339), got, c.want)
		}
	}
}

// T1: the weekend half. 02:00 is squarely inside the window on a weekday.
func TestIsPeakWeekendIsOffPeak(t *testing.T) {
	w := testPeakWindow()
	for _, c := range []struct {
		d    int
		want time.Weekday
	}{
		{19, time.Saturday},
		{20, time.Sunday},
	} {
		at := fixtureInstant(t, 2026, time.September, c.d, 2, 0, c.want)
		if w.IsPeak(at) {
			t.Errorf("IsPeak(%s) = true on a %s, want false", at.Format(time.RFC3339), c.want)
		}
	}
}

// T2: a holiday is off-peak at an otherwise in-window hour, and removing it
// from the list is what makes the same instant peak.
func TestIsPeakOffPeakDates(t *testing.T) {
	at := fixtureInstant(t, 2026, time.October, 1, 2, 0, time.Thursday)

	excluded := testPeakWindow()
	excluded.OffPeakDates = map[string]struct{}{"2026-10-01": {}}
	if excluded.IsPeak(at) {
		t.Error("IsPeak on an OffPeakDates date = true, want false")
	}

	if !testPeakWindow().IsPeak(at) {
		t.Error("IsPeak with the date absent from OffPeakDates = false, want true")
	}
}

// T3, rewritten by GI-11: the peak analogue of TestComputeBatchHalvesExactly.
// The multiplier is applied to the exact per-class cost, so the peak figure is
// the exact double of the off-peak one -- and, with no rounding left anywhere,
// the off-peak call is no longer $0.00.
//
// 300 tokens x $10/MTok = $0.003 per class. Off peak the call is $0.006;
// doubled at peak, $0.006 per class, $0.012.
func TestComputePeakMultipliesExactly(t *testing.T) {
	table := peakFixtureTable()
	usage := parse.Usage{InputTokens: 300, OutputTokens: 300}

	peakAt := fixtureInstant(t, 2026, time.September, 21, 2, 0, time.Monday)
	got, source := table.Compute("peak-model", usage, "", "", peakAt)
	if source != "shipped" {
		t.Fatalf("costSource = %q, want shipped", source)
	}
	if got == nil {
		t.Fatal("usd = nil, want a priced value")
	}
	if *got != 0.012 {
		t.Errorf("peak total = %v, want the exact doubled 0.012", *got)
	}

	// The same call off peak: $0.003 per class, summed exactly. The old
	// per-class cent rounding made this $0.00 -- a priced call stored as free.
	offAt := fixtureInstant(t, 2026, time.September, 21, 0, 30, time.Monday)
	off, _ := table.Compute("peak-model", usage, "", "", offAt)
	if off == nil {
		t.Fatal("off-peak usd = nil, want a priced value")
	}
	if *off == 0 {
		t.Fatal("off-peak total = 0; sub-cent classes are being rounded away again")
	}
	if *off != 0.006 {
		t.Errorf("off-peak total = %v, want the exact 0.006", *off)
	}
	if *got == *off {
		t.Error("peak and off-peak priced identically; the fixture no longer distinguishes them")
	}
}

// T3, rewritten by GI-11: batch halving and the peak multiplier compose on one
// call, as two exact multiplications on the class's own cost. 600 tokens x
// $10/MTok = $0.006 per class; halved then doubled cancels exactly, leaving
// $0.006 -- which is now literally equal to the plain off-peak figure rather
// than equal only after both sides were rounded to a cent.
func TestComputePeakAndBatchCompose(t *testing.T) {
	table := peakFixtureTable()
	usage := parse.Usage{InputTokens: 600}

	peakAt := fixtureInstant(t, 2026, time.September, 21, 2, 0, time.Monday)
	offAt := fixtureInstant(t, 2026, time.September, 21, 0, 30, time.Monday)

	batched, _ := table.Compute("peak-model", usage, "", "batch", peakAt)
	plain, _ := table.Compute("peak-model", usage, "", "", offAt)
	if batched == nil || plain == nil {
		t.Fatal("expected priced values")
	}
	if *batched != 0.006 {
		t.Errorf("batch at peak = %v, want 0.006", *batched)
	}
	if *batched != *plain {
		t.Errorf("batch at peak = %v, plain off peak = %v, want equal (halving and doubling cancel exactly)", *batched, *plain)
	}
}

// T4: peak must not leak onto a flat-priced model. claude-sonnet-5 ships with
// no window, so a peak instant must price identically to off peak.
func TestPeakDoesNotLeakOntoFlatModels(t *testing.T) {
	table := ShippedTable()
	if r := table["claude-sonnet-5"]; r.Peak != nil {
		t.Fatal("claude-sonnet-5 carries a Peak window; pick a flat model for this case")
	}

	usage := parse.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadTokens: 1_000_000}
	peakAt := fixtureInstant(t, 2026, time.September, 21, 2, 0, time.Monday)
	offAt := fixtureInstant(t, 2026, time.September, 21, 0, 30, time.Monday)

	at, _ := table.Compute("claude-sonnet-5", usage, "", "", peakAt)
	off, _ := table.Compute("claude-sonnet-5", usage, "", "", offAt)
	if at == nil || off == nil {
		t.Fatal("expected priced values")
	}
	if *at != *off {
		t.Errorf("claude-sonnet-5 at peak = %v, off peak = %v, want equal", *at, *off)
	}
}

// --- Shipped DeepSeek rows (br-GI-3-03) ---

// T5 (shipped half): the table's shape once the DeepSeek rows land. The two
// populations differ in exactly one way -- Peak -- and an unconditional
// multiply would double every Claude row, so both directions are asserted.
func TestShippedTableShape(t *testing.T) {
	table := ShippedTable()
	if len(table) != 14 {
		t.Errorf("ShippedTable() has %d rows, want 14 (11 Claude + 3 DeepSeek)", len(table))
	}

	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash"} {
		r, ok := table[model]
		if !ok {
			t.Errorf("%s missing from ShippedTable()", model)
			continue
		}
		for name, v := range map[string]*big.Rat{
			"input_rate": r.InputRate, "output_rate": r.OutputRate,
			"cache_write_5m_rate": r.CacheWrite5mRate, "cache_write_1h_rate": r.CacheWrite1hRate,
			"cache_read_rate": r.CacheReadRate,
		} {
			if v == nil {
				t.Errorf("%s: %s is nil", model, name)
			}
		}
		if r.Peak == nil {
			t.Errorf("%s: Peak = nil, want the shipped window", model)
		}
		if r.CacheWrite5mRate.Sign() != 0 || r.CacheWrite1hRate.Sign() != 0 {
			t.Errorf("%s: write rates = $%s/$%s per MTok, want 0/0 (DeepSeek bills no cache-write fee)",
				model, perTokenToMTok(r.CacheWrite5mRate), perTokenToMTok(r.CacheWrite1hRate))
		}
	}

	for model, r := range table {
		switch model {
		case "deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash":
			continue
		}
		if r.Peak != nil {
			t.Errorf("%s: Peak is non-nil on a flat-priced model; it would be multiplied", model)
		}
	}
}

// T6: the sub-cent cache-hit rates the integer-cent constructor could not
// express. Truncating either to $0.00 would price every DeepSeek cache hit at
// zero, which is the failure invariant 5 exists to prevent.
func TestDeepseekCacheHitRatesAreExact(t *testing.T) {
	table := ShippedTable()
	for _, c := range []struct{ model, want string }{
		{"deepseek-flash", "0.003000"},
		{"deepseek-v4-flash", "0.003000"},
		{"deepseek-v4-pro", "0.022000"},
	} {
		got := perTokenToMTok(table[c.model].CacheReadRate)
		if got != c.want {
			t.Errorf("%s cache hit = $%s/MTok, want $%s", c.model, got, c.want)
		}
		if table[c.model].CacheReadRate.Sign() == 0 {
			t.Errorf("%s cache hit rate is zero -- the sub-cent value was truncated", c.model)
		}
	}
}

// T17(a): the shipped holiday list, and the freshness rule behind it. The
// boundaries are checked against literals rather than against the slice the
// window was built from, so this is not comparing the fixture to itself.
func TestShippedDeepseekOffPeakDates(t *testing.T) {
	window := ShippedTable()["deepseek-flash"].Peak
	if window == nil {
		t.Fatal("deepseek-flash has no Peak window")
	}
	if len(window.OffPeakDates) != 33 {
		t.Errorf("OffPeakDates holds %d dates, want the shipped 33", len(window.OffPeakDates))
	}
	for d := range window.OffPeakDates {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			t.Errorf("OffPeakDates holds %q, which is not a YYYY-MM-DD date: %v", d, err)
		}
	}
	// Each 2026 span's first and last day.
	for _, d := range []string{
		"2026-01-01", "2026-01-03",
		"2026-02-15", "2026-02-23",
		"2026-04-04", "2026-04-06",
		"2026-05-01", "2026-05-05",
		"2026-06-19", "2026-06-21",
		"2026-09-25", "2026-09-27",
		"2026-10-01", "2026-10-07",
	} {
		if _, ok := window.OffPeakDates[d]; !ok {
			t.Errorf("shipped holiday %s is absent from the window", d)
		}
	}
	if _, ok := window.OffPeakDates["2026-03-15"]; ok {
		t.Error("2026-03-15 is in OffPeakDates; the list is excluding ordinary weekdays")
	}

	// A fresh window per call: distinct instances carrying equal dates. A
	// shared window would let one loader's configured list overwrite another's
	// and make ShippedTable() report the last configuration rather than the
	// shipped 33.
	first := ShippedTable()["deepseek-flash"].Peak
	second := ShippedTable()["deepseek-flash"].Peak
	if first == second {
		t.Error("ShippedTable() returned the same *PeakWindow twice; it must build a fresh one per call")
	}
	if len(first.OffPeakDates) != len(second.OffPeakDates) {
		t.Errorf("successive windows differ: %d vs %d dates", len(first.OffPeakDates), len(second.OffPeakDates))
	}
}

// ShippedAPIModelPrefixes is a copy, so a caller cannot corrupt the package's
// own slice -- the same rule AllKinds() follows.
func TestShippedAPIModelPrefixesIsACopy(t *testing.T) {
	got := ShippedAPIModelPrefixes()
	if len(got) != 1 || got[0] != "deepseek-" {
		t.Fatalf("ShippedAPIModelPrefixes() = %v, want [deepseek-]", got)
	}
	got[0] = "mutated"
	if again := ShippedAPIModelPrefixes(); again[0] != "deepseek-" {
		t.Errorf("mutating the returned slice changed the package's own: next call returned %q", again[0])
	}
}

// --- Loader: window re-take and write-rate inheritance (br-GI-3-04) ---

// T5 (Loader half): a partial override -- input/output only -- must not
// re-invent a cache-write fee for a model that charges none, and must not
// revert the model to flat pricing. Both are things the override file cannot
// express, so both have to be re-taken from the shipped row.
func TestLoaderPartialOverrideKeepsZeroWriteRatesAndPeak(t *testing.T) {
	dir := t.TempDir()

	deepseekPath := filepath.Join(dir, "deepseek.toml")
	if err := os.WriteFile(deepseekPath, []byte("model = \"deepseek-flash\"\ninput_rate = 0.20\noutput_rate = 0.80\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := NewLoader(deepseekPath, nil).Table()["deepseek-flash"]
	if r.CacheWrite5mRate == nil || r.CacheWrite5mRate.Sign() != 0 {
		t.Errorf("deepseek cache_write_5m = %v, want 0 (inherited from the shipped row, not derived at 1.25x input)", r.CacheWrite5mRate)
	}
	if r.CacheWrite1hRate == nil || r.CacheWrite1hRate.Sign() != 0 {
		t.Errorf("deepseek cache_write_1h = %v, want 0 (inherited from the shipped row, not derived at 2x input)", r.CacheWrite1hRate)
	}
	if r.Peak == nil {
		t.Error("Peak = nil after a partial override; the override silently reverted the model to flat pricing")
	}
	if got := perTokenToMTok(r.InputRate); got != "0.200000" {
		t.Errorf("input = $%s/MTok, want the override's $0.200000", got)
	}

	// A shipped Claude row keeps the derivation, so its writes follow the
	// *override's* input rather than the shipped one.
	claudePath := filepath.Join(dir, "claude.toml")
	if err := os.WriteFile(claudePath, []byte("model = \"claude-sonnet-5\"\ninput_rate = 5.00\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewLoader(claudePath, nil).Table()["claude-sonnet-5"]
	if got := perTokenToMTok(c.CacheWrite5mRate); got != "6.250000" {
		t.Errorf("claude cache_write_5m = $%s/MTok, want $6.250000 (1.25x the override's 5.00)", got)
	}
	if got := perTokenToMTok(c.CacheWrite1hRate); got != "10.000000" {
		t.Errorf("claude cache_write_1h = $%s/MTok, want $10.000000 (2x the override's 5.00)", got)
	}
}

// T13: one place applies the configured dates, so an override -- which cannot
// carry a window -- can never clobber them. A `none` list is non-nil and
// empty: it excludes nothing rather than falling back to the shipped 33.
func TestLoaderOverrideKeepsConfiguredOffPeakDates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.toml")
	if err := os.WriteFile(path, []byte("model = \"deepseek-flash\"\ninput_rate = 0.20\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	holiday := time.Date(2026, time.October, 1, 2, 0, 0, 0, time.UTC) // a shipped holiday, in-window
	otherHoliday := time.Date(2026, time.January, 1, 2, 0, 0, 0, time.UTC)

	one := NewLoader(path, []string{"2026-10-01"}).Table()["deepseek-flash"].Peak
	if one == nil {
		t.Fatal("deepseek-flash lost its Peak window behind an override")
	}
	if len(one.OffPeakDates) != 1 {
		t.Fatalf("OffPeakDates = %v, want exactly the one configured date (not the shipped 33)", one.OffPeakDates)
	}
	if one.IsPeak(holiday) {
		t.Error("IsPeak on the configured excluded date = true, want false")
	}
	if !one.IsPeak(otherHoliday) {
		t.Error("IsPeak on 2026-01-01 = false, want true (the custom list replaced the shipped 33)")
	}

	none := NewLoader(path, []string{}).Table()["deepseek-flash"].Peak
	if none == nil {
		t.Fatal("deepseek-flash lost its Peak window under a `none` list")
	}
	if len(none.OffPeakDates) != 0 {
		t.Errorf("OffPeakDates = %v, want empty (`none` excludes nothing)", none.OffPeakDates)
	}
	if !none.IsPeak(holiday) {
		t.Error("IsPeak = false under `none`, want true (no date is excluded)")
	}
}

// T17(b): the freshness rule seen from the Loader side. Deterministic rather
// than -race dependent, because go test ./... does not enable the race
// detector -- a shared window would corrupt A's dates when B was built, and
// nothing else would notice.
func TestLoadersResolveOffPeakDatesIndependently(t *testing.T) {
	dir := t.TempDir()
	a := NewLoader(filepath.Join(dir, "a.toml"), []string{"2026-10-01"})
	b := NewLoader(filepath.Join(dir, "b.toml"), []string{"2026-01-01", "2026-01-02"})

	if n := len(a.Table()["deepseek-flash"].Peak.OffPeakDates); n != 1 {
		t.Errorf("loader A excludes %d dates, want 1", n)
	}
	if n := len(b.Table()["deepseek-flash"].Peak.OffPeakDates); n != 2 {
		t.Errorf("loader B excludes %d dates, want 2", n)
	}
	if n := len(ShippedTable()["deepseek-flash"].Peak.OffPeakDates); n != 33 {
		t.Errorf("ShippedTable() excludes %d dates after two configured loaders were built, want the shipped 33", n)
	}
	if n := len(a.Table()["deepseek-flash"].Peak.OffPeakDates); n != 1 {
		t.Errorf("loader A now excludes %d dates, want 1 (a shared window would let B overwrite A)", n)
	}
}

// PeakComputer's contract: true at a peak instant for a model that has a
// window, false for a flat-priced model and for an unknown one. The last two
// are the same answer because a caller only ever asks in order to decide
// whether to warn, and neither case warrants one.
func TestPeakAtContract(t *testing.T) {
	table := ShippedTable()
	peakAt := fixtureInstant(t, 2026, time.September, 21, 2, 0, time.Monday)
	offAt := fixtureInstant(t, 2026, time.September, 21, 0, 30, time.Monday)

	for _, c := range []struct {
		name  string
		model string
		at    time.Time
		want  bool
	}{
		{"shipped window, peak instant", "deepseek-flash", peakAt, true},
		{"shipped window, off peak", "deepseek-flash", offAt, false},
		{"flat-priced model at a peak instant", "claude-sonnet-5", peakAt, false},
		{"unknown model at a peak instant", "claude-nonesuch-9", peakAt, false},
	} {
		if got := table.PeakAt(c.model, c.at); got != c.want {
			t.Errorf("Table.PeakAt(%s): %s = %v, want %v", c.model, c.name, got, c.want)
		}
	}

	// The Loader delegates to its current table, so a call site holding one
	// gets the same peak answer its Compute just used.
	loader := NewLoader(filepath.Join(t.TempDir(), "prices.toml"), nil)
	if !loader.PeakAt("deepseek-flash", peakAt) {
		t.Error("(*Loader).PeakAt at a peak instant = false, want true")
	}
	if loader.PeakAt("claude-sonnet-5", peakAt) {
		t.Error("(*Loader).PeakAt on a flat-priced model = true, want false")
	}
}
