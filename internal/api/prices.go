package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"

	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
)

// priceModel is one model's row in GET /api/prices's response. Rate fields
// are pointers so an unset rate marshals as JSON null -- never an implicit
// 0, which would claim the rate is free rather than unconfigured.
type priceModel struct {
	Model            string   `json:"model"`
	InputRate        *float64 `json:"input_rate"`
	OutputRate       *float64 `json:"output_rate"`
	CacheWrite5mRate *float64 `json:"cache_write_5m_rate"`
	CacheWrite1hRate *float64 `json:"cache_write_1h_rate"`
	CacheReadRate    *float64 `json:"cache_read_rate"`
	FastInputRate    *float64 `json:"fast_input_rate,omitempty"`
	FastOutputRate   *float64 `json:"fast_output_rate,omitempty"`
	Source           string   `json:"source"`
}

type pricesResponse struct {
	Models []priceModel `json:"models"`
}

// getPrices is GET /api/prices: the effective price table (shipped rates
// merged with any user override), resolved fresh via the wired Loader's own
// mtime-based reload. POST /api/prices writes one model's override through
// the same Loader's Path, so a write here prices the consumer's very next
// request.
func (a *api) getPrices(w http.ResponseWriter, r *http.Request) {
	if a.priceLoader == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing is unavailable: no price loader is wired")
		return
	}
	writeJSON(w, http.StatusOK, renderPrices(a.priceLoader.Table()))
}

// setPricesRequest is POST /api/prices's body: one model's rates, in exactly
// the shape GET returns that model's row in. Decoding into priceModel itself
// (rather than a parallel type) is what keeps the two directions from
// drifting -- an editable Settings table can POST back the row it fetched.
//
// An omitted field and an explicit null both decode to a nil pointer, so both
// mean "unset" with no separate syntax for either.
type setPricesRequest struct {
	Model            string   `json:"model"`
	InputRate        *float64 `json:"input_rate"`
	OutputRate       *float64 `json:"output_rate"`
	CacheWrite5mRate *float64 `json:"cache_write_5m_rate"`
	CacheWrite1hRate *float64 `json:"cache_write_1h_rate"`
	CacheReadRate    *float64 `json:"cache_read_rate"`
	FastInputRate    *float64 `json:"fast_input_rate,omitempty"`
	FastOutputRate   *float64 `json:"fast_output_rate,omitempty"`
}

// setPrices is POST /api/prices: replaces one model's rates wholesale and
// returns the same shape GET does, so the caller re-renders from what was
// actually written rather than from what it sent.
//
// Whole-row, not per-field, because that is what the file format does:
// pricing.Loader.reload merges an override row over the shipped one as a
// single unit (merged[model] = r), so a partial POST would silently take the
// rest of the row's rates with it. Rejecting unknown fields at decode time
// means a typo'd key is a 400 naming the key, not a silently dropped rate.
func (a *api) setPrices(w http.ResponseWriter, r *http.Request) {
	if a.priceLoader == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing is unavailable: no price loader is wired")
		return
	}
	if reason := originReject(r, "prices"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}

	var req setPricesRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("malformed request body: %v", err))
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}

	rate := pricing.Rate{Model: req.Model, Source: "user"}
	for _, f := range []struct {
		name string
		in   *float64
		out  **big.Rat
	}{
		{"input_rate", req.InputRate, &rate.InputRate},
		{"output_rate", req.OutputRate, &rate.OutputRate},
		{"cache_write_5m_rate", req.CacheWrite5mRate, &rate.CacheWrite5mRate},
		{"cache_write_1h_rate", req.CacheWrite1hRate, &rate.CacheWrite1hRate},
		{"cache_read_rate", req.CacheReadRate, &rate.CacheReadRate},
		{"fast_input_rate", req.FastInputRate, &rate.FastInputRate},
		{"fast_output_rate", req.FastOutputRate, &rate.FastOutputRate},
	} {
		rat, err := ratFromFloat(f.in)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s: %v", f.name, err))
			return
		}
		*f.out = rat
	}

	// Written to the Loader's own file, so the reader watching it observes the
	// write on its next stat. Writing elsewhere would make the route a no-op
	// the caller could not tell from success.
	if err := pricing.SetOverride(a.priceLoader.Path(), rate); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, renderPrices(a.priceLoader.Table()))
}

// ratToFloat converts a per-token big.Rat rate to a JSON-friendly float64,
// nil-safe so an unset rate marshals as null rather than 0.
func ratToFloat(r *big.Rat) *float64 {
	if r == nil {
		return nil
	}
	f, _ := r.Float64()
	return &f
}

// ratFromFloat is ratToFloat's inverse, and the one place a rate arriving
// from outside becomes an exact rational. A negative or non-finite rate is
// rejected rather than stored: SetFloat64 returns nil for NaN/±Inf (so an
// infinite rate would otherwise be a nil-pointer panic downstream), and a
// negative rate would make a call's cost negative -- a figure the invoicing
// invariant has no shape for.
func ratFromFloat(f *float64) (*big.Rat, error) {
	if f == nil {
		return nil, nil // unset, not zero
	}
	if *f < 0 {
		return nil, fmt.Errorf("want a non-negative rate, got %v", *f)
	}
	rat := new(big.Rat).SetFloat64(*f)
	if rat == nil {
		return nil, fmt.Errorf("rate %v is not a finite number", *f)
	}
	return rat, nil
}

// renderPrices builds GET /api/prices's response from a loaded table:
// sorted models, so the array does not reshuffle between renders as the
// underlying map iterates.
func renderPrices(tbl pricing.Table) pricesResponse {
	names := make([]string, 0, len(tbl))
	for name := range tbl {
		names = append(names, name)
	}
	sort.Strings(names)

	models := make([]priceModel, 0, len(tbl))
	for _, name := range names {
		r := tbl[name]
		models = append(models, priceModel{
			Model:            name,
			InputRate:        ratToFloat(r.InputRate),
			OutputRate:       ratToFloat(r.OutputRate),
			CacheWrite5mRate: ratToFloat(r.CacheWrite5mRate),
			CacheWrite1hRate: ratToFloat(r.CacheWrite1hRate),
			CacheReadRate:    ratToFloat(r.CacheReadRate),
			FastInputRate:    ratToFloat(r.FastInputRate),
			FastOutputRate:   ratToFloat(r.FastOutputRate),
			Source:           r.Source,
		})
	}
	return pricesResponse{Models: models}
}
