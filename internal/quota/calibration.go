package quota

import "time"

// LearnedLimit is one candidate limit calibration derives from a
// snapshot that reported 100% utilization -- an empirical observation of
// the plan's real limit for that window, offered for the user's
// confirmation and never written as a limit on its own.
type LearnedLimit struct {
	Window      Window
	Account     string
	TokensAt100 int
	ObservedAt  time.Time
}

// TokensPerPercent is the tokens<->percent-of-plan ratio this repo has
// no other honest way to know -- Anthropic publishes no such conversion.
func (l LearnedLimit) TokensPerPercent() float64 {
	return float64(l.TokensAt100) / 100
}

// Calibrate scans cross-check results (CrossCheck's own output, so it
// reuses the same burn-at-instant computation rather than re-querying)
// for a snapshot that reported utilization_pct >= 100, and returns one
// LearnedLimit candidate per such snapshot. It only reads; nothing here
// writes a limit anywhere -- the caller decides whether to offer it to
// the user (br-GI-1-15/18's job), matching "limits are never invented,
// only offered".
func Calibrate(results []CrossCheckResult) []LearnedLimit {
	var out []LearnedLimit
	for _, r := range results {
		if r.Snapshot.UtilizationPct == nil || *r.Snapshot.UtilizationPct < 100 {
			continue
		}
		out = append(out, LearnedLimit{
			Window:      r.Burn.Window,
			Account:     r.Snapshot.Account,
			TokensAt100: r.Burn.Tokens,
			ObservedAt:  r.Snapshot.ObservedAt,
		})
	}
	return out
}
