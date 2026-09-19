package consumer

import (
	"context"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Analyzer inspects one call's metadata, usage, and built event and returns
// zero or more findings. internal/analyze satisfies this structurally
// (Consumer never imports it — invariant 2's direction holds), so this
// bead's registered analyzer set is empty; br-GI-1-09/10 add
// implementations and br-GI-1-17 wires them in.
type Analyzer interface {
	Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning
}

// SessionResolver assigns a session id before insert. It is deliberately
// read-only: warning_count is only final after the analyzers have run, so
// persisting session aggregates belongs on the other side of the insert,
// through SessionAggregator.
type SessionResolver interface {
	Resolve(meta parse.Meta, now time.Time) (sessionID string)
}

// SessionAggregator persists a processed call into its session's totals.
// A Consumer without one installed behaves exactly as it did before
// SetSessionAggregator was called: no session totals are maintained.
type SessionAggregator interface {
	RecordCall(ctx context.Context, sessionID string, ev *store.Event, warningCount int) error
}

// PriceComputer is the pre-insert cost step's seam (br-GI-1-07): both
// pricing.Table and *pricing.Loader satisfy it.
type PriceComputer interface {
	Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd *float64, costSource string)
}
