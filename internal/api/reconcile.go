package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/reconcile"
)

// costDriftThresholdUSD is the default absolute-dollar divergence beyond which
// cost_drift fires. It is the same number as internal/cli's `clens reconcile`
// constant: the two surfaces answer the same question and must not disagree
// about where the line is.
const costDriftThresholdUSD = 0.01

// reconcileScope states the caveat in the response itself, because a bare
// "no drift" over an empty row list reads as agreement when it may only mean
// nothing was comparable.
const reconcileScope = "computed_usd sums priced API rows only; a subscription account's usage produces no computed figure and is never compared here"

// reconcileRow is one (day, model)'s comparison.
//
// ComputedUSD and BilledUSD are separate labelled fields and there is
// deliberately no summed field: they are two different sources' figures for
// the same usage, and their sum would be a number neither source supports
// (invariant 5). ComputedUSD is a pointer so "no priced API row that day"
// marshals as null rather than as $0.00 -- a day with no usage and a day with
// free usage are not the same day.
type reconcileRow struct {
	Day         time.Time `json:"day"`
	Model       string    `json:"model"`
	ComputedUSD *float64  `json:"computed_usd"`
	BilledUSD   float64   `json:"billed_usd"`
	DivergedUSD *float64  `json:"diverged_usd"`
	CostDrift   bool      `json:"cost_drift"`
}

type reconcileResponse struct {
	// ThresholdUSD is echoed so the tab can say what "drift" was measured
	// against rather than rendering a bare boolean.
	ThresholdUSD float64        `json:"threshold_usd"`
	Scope        string         `json:"scope"`
	Rows         []reconcileRow `json:"rows"`
	// SourceMismatchCount is the A-vs-B warning count (two sources
	// disagreeing about one request's tokens). It is a separate count, not a
	// row: a mismatch has no day or model of its own.
	SourceMismatchCount int `json:"source_mismatch_count"`
}

// reconcile is GET /api/reconcile: the computed-vs-billed comparison per day
// and model.
//
// The billed side is read here rather than passed in, mirroring
// `clens reconcile`: the two time bounds are zero, which every Admin cost
// report row satisfies, so the whole table is compared. The comparison itself
// -- including the round-then-compare against the threshold -- lives in
// internal/reconcile, so this route and the CLI cannot drift apart.
func (a *api) reconcile(w http.ResponseWriter, r *http.Request) {
	threshold := costDriftThresholdUSD
	if s := r.URL.Query().Get("threshold"); s != "" {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f < 0 {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("invalid threshold %q: want a non-negative number of dollars", s))
			return
		}
		threshold = f
	}

	billed, err := a.store.ListAdminCostDays(r.Context(), time.Time{}, time.Time{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := reconcile.Reconcile(r.Context(), a.store, billed, threshold)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	rows := make([]reconcileRow, 0, len(res.Rows))
	for _, dm := range res.Rows {
		rows = append(rows, reconcileRow{
			Day:         dm.Day,
			Model:       dm.Model,
			ComputedUSD: dm.ComputedUSD,
			BilledUSD:   dm.BilledUSD,
			DivergedUSD: dm.DivergedUSD,
			CostDrift:   dm.CostDrift,
		})
	}
	writeJSON(w, http.StatusOK, reconcileResponse{
		ThresholdUSD:        threshold,
		Scope:               reconcileScope,
		Rows:                rows,
		SourceMismatchCount: res.SourceMismatchCount,
	})
}
