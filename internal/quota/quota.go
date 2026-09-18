// Package quota computes, from events, the token burn inside each
// rolling window and compares it against source C's polled snapshots
// (internal/snapshot). It never invents a limit: Limits is empty by
// default, and a limit only exists once the user configures one or the
// engine learns one from a snapshot that reached 100% (calibration.go) --
// and even then only as an offered candidate, never applied silently.
package quota

import (
	"context"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Store is the read slice quota needs: events for burn, quota_snapshots
// for the cross-check.
type Store interface {
	ListEvents(ctx context.Context, filter store.EventFilter) ([]*store.Event, error)
	ListQuotaSnapshots(ctx context.Context, account string, limit int) ([]store.QuotaSnapshot, error)
}

// Window is one of the plan's rolling windows. ponytail: burn is grouped
// by exact model, not a broader "model family" -- family grouping would
// need a family->models table this repo doesn't have yet; add one if a
// per-family view is ever asked for.
type Window string

const (
	Window5h Window = "5h"
	Window7d Window = "7d"
)

func (w Window) duration() time.Duration {
	switch w {
	case Window5h:
		return 5 * time.Hour
	case Window7d:
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

// Burn is the total tokens consumed inside one rolling window ending at
// Now, for one account and (optionally) one model.
type Burn struct {
	Window   Window
	Account  string
	Model    string
	Now      time.Time
	Tokens   int
	Requests int
}

// ComputeBurn sums every token column (the full billed total, not just
// the uncached remainder -- input + output + both cache-write tiers +
// cache-read) over account/model's rows inside window's rolling
// interval ending at now. The interval is half-open [now-duration, now):
// a call started exactly `duration` before now is inside it, one
// started at `now` itself is not, matching EventFilter's own
// Since-inclusive/Until-exclusive semantics.
func ComputeBurn(ctx context.Context, st Store, account, model string, window Window, now time.Time) (Burn, error) {
	since := now.Add(-window.duration())
	evs, err := st.ListEvents(ctx, store.EventFilter{
		Account: account,
		Model:   model,
		Since:   since,
		Until:   now,
		Limit:   1 << 30,
	})
	if err != nil {
		return Burn{}, err
	}
	b := Burn{Window: window, Account: account, Model: model, Now: now}
	for _, ev := range evs {
		b.Tokens += ev.InputTokens + ev.OutputTokens + ev.CacheWrite5mTokens + ev.CacheWrite1hTokens + ev.CacheReadTokens
		b.Requests++
	}
	return b, nil
}

// CrossCheckResult pairs one polled quota_snapshots row with the burn
// this package computes at that row's own observed_at instant -- not
// "now", so a snapshot from hours ago is still checked against the
// window as it stood then.
type CrossCheckResult struct {
	Snapshot store.QuotaSnapshot
	Burn     Burn
}

// CrossCheck pairs every recent snapshot for account/model with the burn
// computed at its own instant. A snapshot whose window name isn't one of
// the rolling windows this package knows (e.g. a per-model 7-day window
// name this v1 doesn't group by) is skipped, not fatal -- the other
// snapshots still cross-check.
func CrossCheck(ctx context.Context, st Store, account, model string, limit int) ([]CrossCheckResult, error) {
	snaps, err := st.ListQuotaSnapshots(ctx, account, limit)
	if err != nil {
		return nil, err
	}
	var out []CrossCheckResult
	for _, snap := range snaps {
		w := Window(snap.Window)
		if w.duration() == 0 {
			continue
		}
		b, err := ComputeBurn(ctx, st, account, model, w, snap.ObservedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, CrossCheckResult{Snapshot: snap, Burn: b})
	}
	return out, nil
}

// Limits are tokens-allowed-per-window, empty by default. It is never
// populated by guessing what a plan name implies -- only by the user's
// own configuration or a confirmed calibration.LearnedLimit.
type Limits map[Window]int

// Projection is one window's burn-rate projection against its limit.
// Configured is false whenever limits has no entry for Window (or a
// non-positive one) -- the caller must render "unconfigured" in that
// case, never read UtilizationPct as a real 0%.
type Projection struct {
	Window         Window
	Burn           Burn
	Limit          int
	Configured     bool
	UtilizationPct float64
	Approaching    bool
}

// Project reports whether burn's current rate would exhaust window's
// limit before resetsAt. ponytail: the rate is a linear extrapolation of
// tokens-per-second averaged over the whole rolling window, not a full
// simulation of which past requests age out when -- good enough to
// answer "should the user worry", revisit with a real rolling
// simulation if the linear approximation proves too coarse in practice.
// With no limit configured, the same burn computation still ran (the
// caller already has Burn); Project itself just declines to project.
func Project(b Burn, limits Limits, resetsAt time.Time) Projection {
	limit, ok := limits[b.Window]
	p := Projection{Window: b.Window, Burn: b, Limit: limit, Configured: ok && limit > 0}
	if !p.Configured {
		return p
	}
	p.UtilizationPct = float64(b.Tokens) / float64(limit) * 100

	windowSeconds := b.Window.duration().Seconds()
	if windowSeconds <= 0 {
		return p
	}
	rate := float64(b.Tokens) / windowSeconds
	remaining := resetsAt.Sub(b.Now).Seconds()
	if remaining < 0 {
		remaining = 0
	}
	projected := float64(b.Tokens) + rate*remaining
	p.Approaching = projected >= float64(limit)
	return p
}
