package api

import (
	"net/http"
	"sort"

	"github.com/abhisheksarkar30/claude-lens/internal/catalog"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// The pricing-coverage labels. Each is a state the Models tab renders as a
// labelled cell: an unpriced model is a known gap and a provisional one is a
// rate with a documented caveat, so neither is ever rendered as a blank cell
// or as 0.
const (
	pricingPriced      = "priced"
	pricingUnpriced    = "unpriced"
	pricingProvisional = "provisional"
)

// modelSourceObserved marks a model that is in no catalogue but has been seen
// in captured traffic.
const modelSourceObserved = "observed"

type modelCoverage struct {
	Model       string `json:"model"`
	DisplayName string `json:"display_name,omitempty"`
	// PriceModel is the rate row this entry is priced from when that is not
	// the model itself: a locally served model is projected at a fixed cloud
	// price, so it is covered by that row, not by its own name. Empty when the
	// model prices as itself.
	PriceModel string `json:"price_model,omitempty"`
	// Source is where the model itself came from: "live" or "shipped" from
	// the catalogue, "observed" for one only ever seen in traffic.
	Source string `json:"source"`
	// Pricing is the coverage label above. RateSource is the rate row's own
	// provenance ("shipped" | "provisional" | "user"), empty when Pricing is
	// unpriced -- there is no row to have come from anywhere.
	Pricing    string `json:"pricing"`
	RateSource string `json:"rate_source,omitempty"`

	Requests      int `json:"requests"`
	PricedCount   int `json:"priced_count"`
	UnpricedCount int `json:"unpriced_count"`
}

type modelsResponse struct {
	Models []modelCoverage `json:"models"`
}

// models is GET /api/models: the rate catalogue unioned with the models
// actually observed, one row each, every row carrying a coverage label.
//
// The union is the point. The catalogue alone would omit a model that traffic
// used and the shipped table does not know -- the exact case that produces an
// unpriced row. The observed set alone would omit a model that is priced and
// simply has not been called yet.
func (a *api) models(w http.ResponseWriter, r *http.Request) {
	observed, err := a.store.StatsByModel(r.Context(), store.EventFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	table := a.priceTable()
	// Keyed by (model, price model): a model served both locally and from the
	// cloud arrives as two rows with different price models, and folding them
	// into one entry would price the pair against whichever row came last.
	byModel := make(map[[2]string]modelCoverage, len(observed))
	for _, e := range catalog.ShippedCatalog() {
		byModel[[2]string{e.ModelID, e.ModelID}] = modelCoverage{Model: e.ModelID, DisplayName: e.DisplayName, Source: e.Source}
	}
	for _, s := range observed {
		// StatsByModel carries one row per (model, billing_mode) -- invariant
		// 5's split -- and they are folded here on purpose: coverage is a
		// property of the model, and this route renders no cost column at
		// all, so there are no two billing figures to mix.
		priceModel := s.PriceModel
		if priceModel == "" {
			priceModel = s.Model
		}
		key := [2]string{s.Model, priceModel}
		row := byModel[key]
		row.Model = s.Model
		if priceModel != s.Model {
			row.PriceModel = priceModel
		}
		if row.Source == "" {
			row.Source = modelSourceObserved
		}
		row.Requests += s.RequestCount
		row.PricedCount += s.PricedCount
		row.UnpricedCount += s.UnpricedCount
		byModel[key] = row
	}

	models := make([]modelCoverage, 0, len(byModel))
	for _, row := range byModel {
		row.Pricing, row.RateSource = coverage(table, row)
		models = append(models, row)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Model != models[j].Model {
			return models[i].Model < models[j].Model
		}
		return models[i].PriceModel < models[j].PriceModel
	})
	writeJSON(w, http.StatusOK, modelsResponse{Models: models})
}

// coverage labels one model's pricing state. It extends `clens models`'s rule
// (absent from the table, or any unpriced call on record -> unpriced) with
// the provisional case, which the CLI folds into "priced": a rate the repo
// itself marks as not fully verified is a different answer from one it
// shipped, and the dashboard has room to say so.
func coverage(table pricing.Table, row modelCoverage) (string, string) {
	priced := row.Model
	if row.PriceModel != "" {
		priced = row.PriceModel
	}
	rate, ok := table[priced]
	if !ok || row.UnpricedCount > 0 {
		return pricingUnpriced, ""
	}
	if rate.Source == pricingProvisional {
		return pricingProvisional, rate.Source
	}
	return pricingPriced, rate.Source
}

// priceTable is the effective rate table: the wired Loader's if there is one,
// else the shipped table, which is what keeps this route answering 200 on an
// install that never called SetPricing.
//
// Unlike `clens models` this makes no network call. The live /v1/models fetch
// is the CLI's and the `refresh` collector's job; a dashboard tab that reached
// out to api.anthropic.com on every click would be a surprising thing for a
// loopback tool to do.
func (a *api) priceTable() pricing.Table {
	if a.priceLoader != nil {
		return a.priceLoader.Table()
	}
	return pricing.ShippedTable()
}
