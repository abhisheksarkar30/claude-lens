package pricing

import (
	"math/big"
	"slices"
	"time"
)

// shippedAPIModelPrefixes is the default set of model prefixes that bill
// pay-as-you-go rather than through a subscription. It is the shipped default
// only: internal/config's ApiModelPrefixes replaces it wholesale when set.
var shippedAPIModelPrefixes = []string{"deepseek-"}

// ShippedAPIModelPrefixes returns the default pay-as-you-go model prefixes.
// A copy, so no caller can mutate the package's own slice.
func ShippedAPIModelPrefixes() []string {
	return slices.Clone(shippedAPIModelPrefixes)
}

// deepseekOffPeakDates is the 2026 Chinese public holiday calendar: the dates
// excluded from DeepSeek's peak window. DeepSeek bills peak at exactly 2x
// off-peak, so a missing holiday leaves a day charged at peak.
//
// ponytail: this list goes stale on 2027-01-01 -- refresh it from the State
// Council General Office notice each year. The failure direction is
// over-charging, never under-charging, so a stale list is safe but wrong.
// Weekends inside these spans are already off-peak, so listing the full spans
// is redundant but harmless, and it keeps the list exactly the official table
// -- the citable artifact. Source: State Council General Office notice of
// 2025-11-04.
var deepseekOffPeakDates = []string{
	"2026-01-01", "2026-01-02", "2026-01-03",
	"2026-02-15", "2026-02-16", "2026-02-17", "2026-02-18", "2026-02-19",
	"2026-02-20", "2026-02-21", "2026-02-22", "2026-02-23",
	"2026-04-04", "2026-04-05", "2026-04-06",
	"2026-05-01", "2026-05-02", "2026-05-03", "2026-05-04", "2026-05-05",
	"2026-06-19", "2026-06-20", "2026-06-21",
	"2026-09-25", "2026-09-26", "2026-09-27",
	"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05",
	"2026-10-06", "2026-10-07",
}

// deepseekPeakWindow returns a fresh PeakWindow per call -- never a
// package-level var. serve runs two pricers in one process and both call
// Table()->reload() when the override file's mtime moves, so a shared window
// would be a data race; and ShippedTable() would report the last configured
// list instead of the shipped 33.
func deepseekPeakWindow() *PeakWindow {
	dates := make(map[string]struct{}, len(deepseekOffPeakDates))
	for _, d := range deepseekOffPeakDates {
		dates[d] = struct{}{}
	}
	return &PeakWindow{
		Multiplier:   big.NewRat(2, 1),
		Hours:        [][2]int{{1, 4}, {6, 10}},
		OffPeakDates: dates,
	}
}

// shippedEffectiveFrom is the date the shipped table below was compiled
// from the skill bundle. It is not a per-row fetch time — the table ships
// baked into the binary — so every shipped row carries the same value.
var shippedEffectiveFrom = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// perMTok converts a USD-per-million-tokens price, given as a decimal string
// (e.g. "10.00" -> $10.00, "0.25" -> $0.25), to an exact $-per-token big.Rat.
// A string rather than an integer number of cents because DeepSeek's off-peak
// cache-hit rate is $0.003/MTok -- 0.3 cents -- which integer cents cannot
// express, and truncating it would price every DeepSeek cache hit at zero.
//
// It panics on an unparseable literal: every caller passes a shipped-table
// constant, so a bad one is a build/test failure caught at first call, never a
// runtime condition to degrade on. The panic happens at construction, not
// mid-request, so it cannot break a live capture.
func perMTok(usdPerMillion string) *big.Rat {
	r, err := mtokToPerToken(usdPerMillion)
	if err != nil {
		panic(err)
	}
	return r
}

// rate builds one shipped Rate: cache write rates are always 1.25x / 2x the
// input rate, so callers pass only input/output/cacheRead as USD-per-MTok.
func rate(model, input, output, cacheRead, source string) Rate {
	in := perMTok(input)
	return Rate{
		Model:            model,
		InputRate:        in,
		OutputRate:       perMTok(output),
		CacheWrite5mRate: new(big.Rat).Mul(in, big.NewRat(5, 4)),
		CacheWrite1hRate: new(big.Rat).Mul(in, big.NewRat(2, 1)),
		CacheReadRate:    perMTok(cacheRead),
		EffectiveFrom:    shippedEffectiveFrom,
		Source:           source,
	}
}

// rateExact builds a shipped Rate from decimal USD-per-MTok strings, with
// cache-write rates given explicitly rather than derived as 1.25x/2x input --
// for a model like DeepSeek that bills no cache-write premium.
func rateExact(model, input, output, cacheRead, cacheWrite5m, cacheWrite1h, source string) Rate {
	return Rate{
		Model:            model,
		InputRate:        perMTok(input),
		OutputRate:       perMTok(output),
		CacheWrite5mRate: perMTok(cacheWrite5m),
		CacheWrite1hRate: perMTok(cacheWrite1h),
		CacheReadRate:    perMTok(cacheRead),
		EffectiveFrom:    shippedEffectiveFrom,
		Source:           source,
	}
}

// ShippedTable is claude-lens's built-in rate table — populated, unlike
// deepseek-lens's deliberately-empty one. A model absent here is
// "unpriced", never guessed. See docs/planning/GI-1-claude-lens-v1.md
// §Shipped price table for the citation behind every row.
func ShippedTable() Table {
	t := Table{
		"claude-fable-5-1":  rate("claude-fable-5-1", "10.00", "50.00", "0.25", "shipped"),   // shared/models.md:73
		"claude-fable-5":    rate("claude-fable-5", "10.00", "50.00", "1.00", "shipped"),     // shared/models.md:74
		"claude-mythos-5":   rate("claude-mythos-5", "10.00", "50.00", "1.00", "shipped"),    // shared/models.md:74
		"claude-mythos-5-1": rate("claude-mythos-5-1", "10.00", "50.00", "0.25", "shipped"),  // shared/models.md:75, cited via Fable 5.1
		"claude-opus-5":     rate("claude-opus-5", "5.00", "25.00", "0.50", "shipped"),       // shared/models.md:76
		"claude-opus-4-8":   rate("claude-opus-4-8", "5.00", "25.00", "0.50", "shipped"),     // shared/models.md:76
		"claude-opus-4-7":   rate("claude-opus-4-7", "5.00", "25.00", "0.50", "provisional"), // no rate in bundle; verify against Pricing URL
		"claude-opus-4-6":   rate("claude-opus-4-6", "5.00", "25.00", "0.50", "provisional"),
		"claude-sonnet-5":   rate("claude-sonnet-5", "2.00", "10.00", "0.20", "shipped"),   // shared/model-migration.md:1291
		"claude-sonnet-4-6": rate("claude-sonnet-4-6", "3.00", "15.00", "0.30", "shipped"), // shared/model-migration.md:1291
		"claude-haiku-4-5":  rate("claude-haiku-4-5", "1.00", "5.00", "0.10", "provisional"),

		// DeepSeek, billed pay-as-you-go. The rates below are the *off-peak*
		// ones; peak is 2x, applied by the Peak window attached after this
		// literal. DeepSeek charges no separate cache-write fee -- the
		// cache-miss price is what populates the cache, and cache_write_*_tokens
		// are 0 on every captured row -- so both write rates are exactly 0 via
		// rateExact rather than derived at 1.25x/2x.
		"deepseek-flash":    rateExact("deepseek-flash", "0.15", "0.60", "0.003", "0", "0", "shipped"),    // https://api-docs.deepseek.com/quick_start/pricing/ (retrieved 2026-09-19)
		"deepseek-v4-pro":   rateExact("deepseek-v4-pro", "0.66", "1.98", "0.022", "0", "0", "shipped"),   // https://api-docs.deepseek.com/quick_start/pricing/ (retrieved 2026-09-19)
		"deepseek-v4-flash": rateExact("deepseek-v4-flash", "0.15", "0.60", "0.003", "0", "0", "shipped"), // retired alias of V4.1-Flash; still routes there and bills at the Flash price
	}

	// Peak windows: DeepSeek only. Each row gets its own freshly-built window
	// (see deepseekPeakWindow), so one loader's configured dates can never
	// leak into another's.
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro", "deepseek-v4-flash"} {
		r := t[model]
		r.Peak = deepseekPeakWindow()
		t[model] = r
	}

	// Fast-mode rates: shipped for Opus 5 / Opus 4.8 only ($10/$50 per MTok).
	for _, model := range []string{"claude-opus-5", "claude-opus-4-8"} {
		r := t[model]
		r.FastInputRate = perMTok("10.00")
		r.FastOutputRate = perMTok("50.00")
		t[model] = r
	}

	return t
}
