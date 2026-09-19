package api

import (
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
// mtime-based reload -- see pricing.Loader.Table. POST /api/prices (writing
// an override) is a later slice's write route.
func (a *api) getPrices(w http.ResponseWriter, r *http.Request) {
	if a.priceLoader == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing is unavailable: no price loader is wired")
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
