package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/quota"
)

// quotaWindows are the rolling windows this route reports, in the order the
// tab renders them. The names are a fixed fact of the plan model, not a set
// read from anywhere.
var quotaWindows = []quota.Window{quota.Window5h, quota.Window7d}

// quotaSnapshotHistory is how many past snapshots to cross-check per account.
// It mirrors internal/cli/quota.go's constant, so the dashboard and `clens
// quota` report the same calibration candidates from the same evidence.
//
// ponytail: CrossCheck recomputes a burn per snapshot, so this route issues
// roughly 2*N+2 aggregate queries per account per request. N is small and
// SQLite is local; if a large snapshot history ever makes this tab slow, the
// fix is one query for all snapshots' burns, not a smaller N.
const quotaSnapshotHistory = 20

// LimitState is the literal word the tab renders. It is a string and not a
// bare bool because "unconfigured" is the answer the bead requires the route
// to state, and a bool would push that wording into every client.
const (
	limitConfigured   = "configured"
	limitUnconfigured = "unconfigured"
)

// QuotaRow is one account's burn over one rolling window.
//
// UtilizationPct is a pointer and is null unless LimitState is "configured".
// A percentage of a limit nobody supplied is the one figure this route must
// never produce -- it would render as "0% of nothing" or, worse, as a real
// burn against an invented ceiling.
type QuotaRow struct {
	Window   string `json:"window"`
	Tokens   int    `json:"tokens"`
	Requests int    `json:"requests"`

	LimitState     string   `json:"limit_state"`
	Limit          int      `json:"limit,omitempty"`
	UtilizationPct *float64 `json:"utilization_pct"`
	Approaching    bool     `json:"approaching"`

	// LastSnapshot is the most recent snapshot for this window, and the one
	// thing here that came from Anthropic rather than from our own rows.
	LastSnapshot *QuotaSnapshotRow `json:"last_snapshot"`
}

// QuotaSnapshotRow is one stored quota snapshot as reported. UtilizationPct
// and ResetsAt stay pointers: the source endpoint omits them when it has no
// figure, and a missing one is not a zero.
type QuotaSnapshotRow struct {
	ObservedAt     time.Time  `json:"observed_at"`
	UtilizationPct *float64   `json:"utilization_pct"`
	ResetsAt       *time.Time `json:"resets_at"`
	Status         string     `json:"status"`
}

// LearnedLimitRow is a candidate limit inferred from a snapshot that reached
// 100%. It is offered in the response, never applied to Limits: an inferred
// ceiling the user did not confirm would be a limit this tool invented, which
// is exactly what quota.Limits' doc forbids.
type LearnedLimitRow struct {
	Window      string    `json:"window"`
	TokensAt100 int       `json:"tokens_at_100"`
	ObservedAt  time.Time `json:"observed_at"`
}

type quotaAccount struct {
	Account     string            `json:"account"`
	Plan        string            `json:"plan,omitempty"`
	Windows     []QuotaRow        `json:"windows"`
	Calibration []LearnedLimitRow `json:"calibration"`
}

type quotaResponse struct {
	LimitsConfigured bool           `json:"limits_configured"`
	Accounts         []quotaAccount `json:"accounts"`
}

// quota is GET /api/quota: per subscription account, one row per rolling
// window, plus the last snapshot and any calibration candidate.
//
// Only subscription accounts appear. An API-key account has no rolling window
// to burn through -- its spend is the cost columns, and /api/stats and
// /api/reconcile are where it is reported.
//
// The burn is computed with an empty model, i.e. account-level, mirroring
// `clens quota`: the 5h and 7d windows Anthropic enforces are per account,
// not per model, and summing a per-model cut of them would understate the
// burn the limit actually applies to.
func (a *api) quota(w http.ResponseWriter, r *http.Request) {
	accts, ok := a.loadAccounts(w, r)
	if !ok {
		return
	}
	limits, err := parseQuotaLimits(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	now := time.Now()
	resp := quotaResponse{LimitsConfigured: len(limits) > 0, Accounts: []quotaAccount{}}
	for _, acct := range accts.List {
		if acct.BillingMode != "subscription" {
			continue
		}
		qa := quotaAccount{Account: acct.Name, Plan: acct.Plan, Windows: []QuotaRow{}, Calibration: []LearnedLimitRow{}}

		results, err := quota.CrossCheck(ctx, a.store, acct.Name, "", quotaSnapshotHistory)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, win := range quotaWindows {
			burn, err := quota.ComputeBurn(ctx, a.store, acct.Name, "", win, now)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			proj := quota.Project(burn, limits, now)
			row := QuotaRow{
				Window:      string(win),
				Tokens:      burn.Tokens,
				Requests:    burn.Requests,
				LimitState:  limitUnconfigured,
				Approaching: proj.Approaching,
				// Project returns early when unconfigured, so Approaching is
				// false there rather than a projection against no limit.
			}
			if proj.Configured {
				pct := proj.UtilizationPct
				row.LimitState, row.Limit, row.UtilizationPct = limitConfigured, proj.Limit, &pct
			}
			row.LastSnapshot = lastSnapshotFor(results, win)
			qa.Windows = append(qa.Windows, row)
		}
		for _, learned := range quota.Calibrate(results) {
			qa.Calibration = append(qa.Calibration, LearnedLimitRow{
				Window:      string(learned.Window),
				TokensAt100: learned.TokensAt100,
				ObservedAt:  learned.ObservedAt,
			})
		}
		resp.Accounts = append(resp.Accounts, qa)
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseQuotaLimits reads the per-window limits from ?limit_5h= and ?limit_7d=.
//
// They arrive as query params because no limit configuration source exists:
// quota.Limits is populated by its caller and never by a config file
// (internal/quota/quota.go says so). Query params keep that property -- the
// route reports what the caller asked about, and invents nothing. A window
// with no param is simply absent from the map, which is what makes
// Projection.Configured false and the row render "unconfigured".
func parseQuotaLimits(r *http.Request) (quota.Limits, error) {
	limits := quota.Limits{}
	for _, win := range quotaWindows {
		key := "limit_" + string(win)
		s := r.URL.Query().Get(key)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid %s %q: want a positive integer number of tokens", key, s)
		}
		limits[win] = n
	}
	return limits, nil
}

// lastSnapshotFor is the newest cross-check result whose window matches, or
// nil when there is none. ListQuotaSnapshots orders newest-first, so the
// first match is the newest.
func lastSnapshotFor(results []quota.CrossCheckResult, win quota.Window) *QuotaSnapshotRow {
	for _, res := range results {
		if res.Snapshot.Window != string(win) {
			continue
		}
		return &QuotaSnapshotRow{
			ObservedAt:     res.Snapshot.ObservedAt,
			UtilizationPct: res.Snapshot.UtilizationPct,
			ResetsAt:       res.Snapshot.ResetsAt,
			Status:         res.Snapshot.Status,
		}
	}
	return nil
}
