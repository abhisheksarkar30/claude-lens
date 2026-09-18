package pricing

import (
	"math/big"
	"time"
)

// shippedEffectiveFrom is the date the shipped table below was compiled
// from the skill bundle. It is not a per-row fetch time — the table ships
// baked into the binary — so every shipped row carries the same value.
var shippedEffectiveFrom = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// perMTok converts a USD-per-million-tokens price, given in integer cents
// (e.g. $10.00 -> 1000, $0.25 -> 25), to an exact $-per-token big.Rat. Every
// shipped rate in the table below has at most two decimal places, so this
// captures each one exactly rather than through a float64 approximation.
func perMTok(usdPerMillionInCents int64) *big.Rat {
	return big.NewRat(usdPerMillionInCents, 100*1_000_000)
}

// rate builds one shipped Rate: cache write rates are always 1.25x / 2x the
// input rate, so callers pass only input/output/cacheRead in cents-per-MTok.
func rate(model string, inputCents, outputCents, cacheReadCents int64, source string) Rate {
	input := perMTok(inputCents)
	return Rate{
		Model:            model,
		InputRate:        input,
		OutputRate:       perMTok(outputCents),
		CacheWrite5mRate: new(big.Rat).Mul(input, big.NewRat(5, 4)),
		CacheWrite1hRate: new(big.Rat).Mul(input, big.NewRat(2, 1)),
		CacheReadRate:    perMTok(cacheReadCents),
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
		"claude-fable-5-1":  rate("claude-fable-5-1", 1000, 5000, 25, "shipped"),   // shared/models.md:73
		"claude-fable-5":    rate("claude-fable-5", 1000, 5000, 100, "shipped"),    // shared/models.md:74
		"claude-mythos-5":   rate("claude-mythos-5", 1000, 5000, 100, "shipped"),   // shared/models.md:74
		"claude-mythos-5-1": rate("claude-mythos-5-1", 1000, 5000, 25, "shipped"),  // shared/models.md:75, cited via Fable 5.1
		"claude-opus-5":     rate("claude-opus-5", 500, 2500, 50, "shipped"),       // shared/models.md:76
		"claude-opus-4-8":   rate("claude-opus-4-8", 500, 2500, 50, "shipped"),     // shared/models.md:76
		"claude-opus-4-7":   rate("claude-opus-4-7", 500, 2500, 50, "provisional"), // no rate in bundle; verify against Pricing URL
		"claude-opus-4-6":   rate("claude-opus-4-6", 500, 2500, 50, "provisional"),
		"claude-sonnet-5":   rate("claude-sonnet-5", 200, 1000, 20, "shipped"),   // shared/model-migration.md:1291
		"claude-sonnet-4-6": rate("claude-sonnet-4-6", 300, 1500, 30, "shipped"), // shared/model-migration.md:1291
		"claude-haiku-4-5":  rate("claude-haiku-4-5", 100, 500, 10, "provisional"),
	}

	// Fast-mode rates: shipped for Opus 5 / Opus 4.8 only ($10/$50 per MTok).
	for _, model := range []string{"claude-opus-5", "claude-opus-4-8"} {
		r := t[model]
		r.FastInputRate = perMTok(1000)
		r.FastOutputRate = perMTok(5000)
		t[model] = r
	}

	return t
}
