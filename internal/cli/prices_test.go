package cli

import (
	"bytes"
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
)

func mtokRat(decimal string) *big.Rat {
	r, ok := new(big.Rat).SetString(decimal)
	if !ok {
		panic("mtokRat: " + decimal)
	}
	return r.Quo(r, big.NewRat(1_000_000, 1))
}

func assertRat(t *testing.T, name string, got, want *big.Rat) {
	t.Helper()
	if got == nil || want == nil || got.Cmp(want) != 0 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func loadModel(t *testing.T, path, model string) pricing.Rate {
	t.Helper()
	overrides, err := pricing.LoadOverrides(path)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	r, ok := overrides[model]
	if !ok {
		t.Fatalf("override file %s has no %s", path, model)
	}
	return r
}

func TestPricesSetMultipleFieldsNewModelInOneInvocation(t *testing.T) {
	home := withHome(t)
	var buf bytes.Buffer
	err := runPrices([]string{
		"--set", "claude-sonnet-5-5:input_rate=1.5",
		"--set", "claude-sonnet-5-5:output_rate=2.5",
		"--set", "claude-sonnet-5-5:cache_read_rate=0.15",
		"--set", "claude-sonnet-5-5:cache_write_5m_rate=3.75",
		"--set", "claude-sonnet-5-5:cache_write_1h_rate=9.25",
	}, &buf)
	if err != nil {
		t.Fatalf("runPrices: %v\n%s", err, buf.String())
	}
	path := filepath.Join(home, ".clens", "prices.toml")
	got := loadModel(t, path, "claude-sonnet-5-5")
	assertRat(t, "input_rate", got.InputRate, mtokRat("1.5"))
	assertRat(t, "output_rate", got.OutputRate, mtokRat("2.5"))
	assertRat(t, "cache_read_rate", got.CacheReadRate, mtokRat("0.15"))
	assertRat(t, "cache_write_5m_rate", got.CacheWrite5mRate, mtokRat("3.75"))
	assertRat(t, "cache_write_1h_rate", got.CacheWrite1hRate, mtokRat("9.25"))
	if n := strings.Count(buf.String(), "has no shipped rate; defining it from scratch"); n != 1 {
		t.Errorf("from-scratch message count = %d, want 1\n%s", n, buf.String())
	}
}

func TestPricesSetMultipleFieldsExistingModelInOneInvocation(t *testing.T) {
	t.Run("shipped", func(t *testing.T) {
		home := withHome(t)
		path := filepath.Join(home, ".clens", "prices.toml")
		before := pricing.NewLoader(path, nil).Table()["claude-sonnet-5"]
		var buf bytes.Buffer
		if err := runPrices([]string{
			"--set", "claude-sonnet-5:input_rate=3.00",
			"--set", "claude-sonnet-5:output_rate=12.00",
		}, &buf); err != nil {
			t.Fatalf("runPrices: %v\n%s", err, buf.String())
		}
		got := loadModel(t, path, "claude-sonnet-5")
		assertRat(t, "input_rate", got.InputRate, mtokRat("3.00"))
		assertRat(t, "output_rate", got.OutputRate, mtokRat("12.00"))
		assertRat(t, "cache_read_rate", got.CacheReadRate, before.CacheReadRate)
		assertRat(t, "cache_write_5m_rate", got.CacheWrite5mRate, before.CacheWrite5mRate)
		assertRat(t, "cache_write_1h_rate", got.CacheWrite1hRate, before.CacheWrite1hRate)
	})

	t.Run("prior override", func(t *testing.T) {
		home := withHome(t)
		path := filepath.Join(home, ".clens", "prices.toml")
		seed := pricing.Rate{
			Model:            "claude-sonnet-5",
			InputRate:        mtokRat("4.00"),
			OutputRate:       mtokRat("18.00"),
			CacheReadRate:    mtokRat("0.40"),
			CacheWrite5mRate: mtokRat("5.00"),
			CacheWrite1hRate: mtokRat("8.00"),
		}
		if err := pricing.SaveOverrides(path, pricing.Table{"claude-sonnet-5": seed}); err != nil {
			t.Fatalf("SaveOverrides: %v", err)
		}
		before := pricing.NewLoader(path, nil).Table()["claude-sonnet-5"]
		var buf bytes.Buffer
		if err := runPrices([]string{
			"--set", "claude-sonnet-5:cache_read_rate=0.55",
			"--set", "claude-sonnet-5:cache_write_5m_rate=6.00",
		}, &buf); err != nil {
			t.Fatalf("runPrices: %v\n%s", err, buf.String())
		}
		got := loadModel(t, path, "claude-sonnet-5")
		assertRat(t, "cache_read_rate", got.CacheReadRate, mtokRat("0.55"))
		assertRat(t, "cache_write_5m_rate", got.CacheWrite5mRate, mtokRat("6.00"))
		assertRat(t, "input_rate", got.InputRate, before.InputRate)
		assertRat(t, "output_rate", got.OutputRate, before.OutputRate)
		assertRat(t, "cache_write_1h_rate", got.CacheWrite1hRate, before.CacheWrite1hRate)
	})
}

func TestPricesSetSingleFieldExistingModelUnaffected(t *testing.T) {
	home := withHome(t)
	path := filepath.Join(home, ".clens", "prices.toml")
	before := pricing.NewLoader(path, nil).Table()["claude-sonnet-5"]
	var buf bytes.Buffer
	if err := runPrices([]string{"--set", "claude-sonnet-5:output_rate=18.00"}, &buf); err != nil {
		t.Fatalf("runPrices: %v\n%s", err, buf.String())
	}
	got := loadModel(t, path, "claude-sonnet-5")
	assertRat(t, "output_rate", got.OutputRate, mtokRat("18.00"))
	assertRat(t, "input_rate", got.InputRate, before.InputRate)
	assertRat(t, "cache_read_rate", got.CacheReadRate, before.CacheReadRate)
	assertRat(t, "cache_write_5m_rate", got.CacheWrite5mRate, before.CacheWrite5mRate)
	assertRat(t, "cache_write_1h_rate", got.CacheWrite1hRate, before.CacheWrite1hRate)
}

func TestPricesSetSingleFieldAgainstPriorSessionOverridePreserved(t *testing.T) {
	home := withHome(t)
	path := filepath.Join(home, ".clens", "prices.toml")
	seed := pricing.Rate{
		Model:            "claude-sonnet-5",
		InputRate:        mtokRat("4.00"),
		OutputRate:       mtokRat("18.00"),
		CacheReadRate:    mtokRat("0.40"),
		CacheWrite5mRate: mtokRat("5.00"),
		CacheWrite1hRate: mtokRat("8.00"),
	}
	if err := pricing.SaveOverrides(path, pricing.Table{"claude-sonnet-5": seed}); err != nil {
		t.Fatalf("SaveOverrides: %v", err)
	}
	var buf bytes.Buffer
	if err := runPrices([]string{"--set", "claude-sonnet-5:cache_read_rate=0.55"}, &buf); err != nil {
		t.Fatalf("runPrices: %v\n%s", err, buf.String())
	}
	got := loadModel(t, path, "claude-sonnet-5")
	assertRat(t, "input_rate", got.InputRate, mtokRat("4.00"))
	assertRat(t, "output_rate", got.OutputRate, mtokRat("18.00"))
	assertRat(t, "cache_read_rate", got.CacheReadRate, mtokRat("0.55"))
	assertRat(t, "cache_write_5m_rate", got.CacheWrite5mRate, mtokRat("5.00"))
	assertRat(t, "cache_write_1h_rate", got.CacheWrite1hRate, mtokRat("8.00"))
}
