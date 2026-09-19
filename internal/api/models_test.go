package api

import (
	"encoding/json"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// TestModelsLabelsUnpricedAndProvisionalDistinctly is the bead's clause. The
// three states are three different claims -- a rate we shipped, a rate we
// shipped with a caveat, and no rate at all -- so folding any two together is
// the failure this test exists to catch.
func TestModelsLabelsUnpricedAndProvisionalDistinctly(t *testing.T) {
	st := newTestStore(t)
	// priced: a shipped rate with a costed call against it.
	seedEvent(t, st, nil)
	// unpriced: a model no table knows, whose call the store recorded as such.
	seedEvent(t, st, func(e *store.Event) {
		e.ModelResolved = "claude-future-9"
		e.ModelRequested = "claude-future-9"
		e.CostUSD = nil
		e.CostSource = "unpriced"
	})
	handler, _, _, _ := newTestAPI(t, st)

	got := decodeJSON[modelsResponse](t, getOK(t, handler, "/api/models").Body)
	byModel := map[string]modelCoverage{}
	for _, m := range got.Models {
		byModel[m.Model] = m
	}

	priced, ok := byModel["claude-sonnet-5"]
	if !ok {
		t.Fatal("the priced model is missing from the response")
	}
	if priced.Pricing != pricingPriced {
		t.Errorf("claude-sonnet-5 pricing = %q, want %q", priced.Pricing, pricingPriced)
	}
	if priced.RateSource != "shipped" {
		t.Errorf("claude-sonnet-5 rate_source = %q, want shipped", priced.RateSource)
	}
	// It is in the catalogue, so it is not merely "observed".
	if priced.Source == modelSourceObserved {
		t.Errorf("claude-sonnet-5 source = %q, want the catalogue's own source", priced.Source)
	}

	unpriced, ok := byModel["claude-future-9"]
	if !ok {
		t.Fatal("the unpriced model is missing -- a model seen only in traffic must still be listed")
	}
	if unpriced.Pricing != pricingUnpriced {
		t.Errorf("claude-future-9 pricing = %q, want %q", unpriced.Pricing, pricingUnpriced)
	}
	if unpriced.RateSource != "" {
		t.Errorf("claude-future-9 carries rate_source %q; there is no rate for it to have come from", unpriced.RateSource)
	}
	if unpriced.Source != modelSourceObserved {
		t.Errorf("claude-future-9 source = %q, want %q", unpriced.Source, modelSourceObserved)
	}
	if unpriced.UnpricedCount != 1 || unpriced.Requests != 1 {
		t.Errorf("claude-future-9 counts = %+v, want one unpriced call", unpriced)
	}

	// provisional: shipped in the table, with a caveat the repo itself records.
	prov, ok := byModel["claude-opus-4-7"]
	if !ok {
		t.Fatal("the provisional model is missing -- the catalogue knows it even with no traffic")
	}
	if prov.Pricing != pricingProvisional {
		t.Errorf("claude-opus-4-7 pricing = %q, want %q", prov.Pricing, pricingProvisional)
	}
	if prov.RateSource != "provisional" {
		t.Errorf("claude-opus-4-7 rate_source = %q, want provisional", prov.RateSource)
	}

	if pricingPriced == pricingUnpriced || pricingUnpriced == pricingProvisional || pricingPriced == pricingProvisional {
		t.Fatal("two coverage labels are the same string, so the tab cannot tell them apart")
	}
}

// TestModelsUnpricedWinsOverPricedOnOneModel: a single unpriced call makes the
// whole model unpriced. The alternative -- reporting "priced" because most of
// its calls were -- would hide the gap the tab exists to surface.
func TestModelsUnpricedWinsOverPricedOnOneModel(t *testing.T) {
	st := newTestStore(t)
	seedEvent(t, st, nil) // claude-sonnet-5, shipped, costed
	seedEvent(t, st, func(e *store.Event) {
		e.CostUSD = nil
		e.CostSource = "unpriced"
	})
	handler, _, _, _ := newTestAPI(t, st)

	got := decodeJSON[modelsResponse](t, getOK(t, handler, "/api/models").Body)
	for _, m := range got.Models {
		if m.Model != "claude-sonnet-5" {
			continue
		}
		if m.Pricing != pricingUnpriced {
			t.Errorf("pricing = %q, want %q once any call on the model was unpriced", m.Pricing, pricingUnpriced)
		}
		if m.PricedCount != 1 || m.UnpricedCount != 1 {
			t.Errorf("counts = %d priced / %d unpriced, want 1/1", m.PricedCount, m.UnpricedCount)
		}
		return
	}
	t.Fatal("claude-sonnet-5 is missing from the response")
}

// TestModelsRendersNoCostColumn pins the wire shape: every row carries a label
// and counts, and nothing that could be read as a dollar figure. The bead's
// "neither is rendered as 0" is enforced by there being no cell to render a
// zero into -- a coverage table states what is known about a rate, not what it
// cost.
func TestModelsRendersNoCostColumn(t *testing.T) {
	st := newTestStore(t)
	seedEvent(t, st, func(e *store.Event) {
		e.ModelResolved = "claude-future-9"
		e.ModelRequested = "claude-future-9"
		e.CostUSD = nil
		e.CostSource = "unpriced"
	})
	handler, _, _, _ := newTestAPI(t, st)

	var raw struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(getOK(t, handler, "/api/models").Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The unpriced row must be here and must say so; every other field is
	// checked against the allowlist below.
	var sawUnpriced bool
	for _, row := range raw.Models {
		if string(row["model"]) == `"claude-future-9"` {
			sawUnpriced = true
			if string(row["pricing"]) != `"`+pricingUnpriced+`"` {
				t.Errorf("the unpriced row's pricing is %s, not the label", row["pricing"])
			}
		}
		for field := range row {
			switch field {
			case "model", "display_name", "source", "pricing", "rate_source",
				"requests", "priced_count", "unpriced_count":
			default:
				t.Errorf("row %s exposes unexpected field %q", row["model"], field)
			}
		}
	}
	if !sawUnpriced {
		t.Fatal("the unpriced model is missing from the raw response")
	}
}
