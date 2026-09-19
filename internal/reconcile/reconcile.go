// Package reconcile compares locally computed cost (source A, the events
// table) against Anthropic's own billed figure (source D, the Admin cost
// report) per (day, model) -- a side-by-side comparison of two labelled
// figures, never a sum. It cannot silently merge a subscription figure
// with a billed one because the computed side only ever sums API rows
// (events.cost_usd is NULL for subscription rows by the schema rule), and
// it only compares (day, model) pairs the caller's billed rows actually
// name -- a subscription-only account supplies no billed rows, so no
// comparison, and no cost_drift, is ever produced for it.
package reconcile

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Store is the read slice Reconcile needs: the computed side of the cost
// comparison, and the count of A-vs-B mismatches the store's own
// cross-source merge already raised (br-GI-1-06/11) -- this package
// surfaces that count, it does not re-detect the mismatch itself.
type Store interface {
	StatsSummary(ctx context.Context, filter store.EventFilter) (store.StatsSummary, error)
	CountWarnings(ctx context.Context, filter store.WarningFilter) (int, error)
}

// DayModel is one (day, model)'s side-by-side comparison.
type DayModel struct {
	Day   time.Time
	Model string

	// ComputedUSD is SUM(events.cost_usd) for priced API rows that
	// day/model -- nil when there is none to sum (never $0.00).
	ComputedUSD *float64
	// BilledUSD is the Admin cost report's figure for that day/model,
	// summed across every description row the caller supplied. Always
	// present -- a DayModel only exists here because billed named it.
	BilledUSD float64

	// DivergedUSD and CostDrift are only meaningful when ComputedUSD is
	// non-nil; with no computed figure there is nothing to diverge from.
	DivergedUSD *float64
	CostDrift   bool
}

// Result is one Reconcile call's output.
type Result struct {
	Rows []DayModel
	// SourceMismatchCount is how many source_mismatch warnings (A vs B,
	// disagreeing token counts on the same request_id) are currently on
	// record -- surfaced alongside the cost comparison, not folded into
	// any DayModel row, since a mismatch has no day/model of its own.
	SourceMismatchCount int
}

// Reconcile compares, for every (day, model) present in billed, the
// locally computed API cost against the Admin cost report figure billed
// supplies. billed is caller-supplied (fetched by internal/adminrep, or a
// test fixture) rather than read from admin_cost_days by this package,
// since the caller -- having just fetched or upserted it -- already has
// the rows. thresholdUSD is the absolute-dollar divergence beyond which
// cost_drift fires; at or below, none.
func Reconcile(ctx context.Context, st Store, billed []store.AdminCostDay, thresholdUSD float64) (Result, error) {
	grouped := groupBilledByDayModel(billed)

	var res Result
	for _, key := range sortedKeys(grouped) {
		computed, err := st.StatsSummary(ctx, store.EventFilter{
			Model:       key.model,
			BillingMode: "api",
			Since:       key.day,
			Until:       key.day.Add(24 * time.Hour),
		})
		if err != nil {
			return Result{}, err
		}

		dm := DayModel{Day: key.day, Model: key.model, BilledUSD: grouped[key], ComputedUSD: computed.TotalCostUSD}
		if dm.ComputedUSD != nil {
			// Rounded to the nearest hundredth of a cent before
			// comparing: dollar figures accumulated through float64 sums
			// carry binary-representation noise (e.g. 10.05-10.00 does
			// not land on exactly 0.05), which would make an
			// at-the-threshold comparison flap on noise smaller than any
			// real currency unit.
			diverged := math.Round(math.Abs(*dm.ComputedUSD-dm.BilledUSD)*1e4) / 1e4
			dm.DivergedUSD = &diverged
			dm.CostDrift = diverged > thresholdUSD
		}
		res.Rows = append(res.Rows, dm)
	}

	n, err := st.CountWarnings(ctx, store.WarningFilter{Kind: "source_mismatch"})
	if err != nil {
		return Result{}, err
	}
	res.SourceMismatchCount = n

	return res, nil
}

type dayModelKey struct {
	day   time.Time
	model string
}

// groupBilledByDayModel sums billed's amount across every description row
// sharing a (day, model). ponytail: every currency is summed together
// (amount_usd's own column name assumes USD); a mixed-currency account is
// out of scope for v1 -- add a per-currency breakdown if one is ever seen.
func groupBilledByDayModel(billed []store.AdminCostDay) map[dayModelKey]float64 {
	out := map[dayModelKey]float64{}
	for _, b := range billed {
		key := dayModelKey{day: b.DayStart.UTC().Truncate(24 * time.Hour), model: b.Model}
		out[key] += b.AmountUSD
	}
	return out
}

func sortedKeys(m map[dayModelKey]float64) []dayModelKey {
	keys := make([]dayModelKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if !keys[i].day.Equal(keys[j].day) {
			return keys[i].day.Before(keys[j].day)
		}
		return keys[i].model < keys[j].model
	})
	return keys
}
