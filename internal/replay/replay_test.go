package replay

import (
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// TestOutcomeOfModelFallback covers OutcomeOf's one branch: an event whose
// resolved model is unknown — an unpriced/unparsed upstream response — must
// still name the model by what the client asked for, since every surface that
// shows a model falls back the same way (internal/cli's displayModel).
func TestOutcomeOfModelFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		resolved  string
		want      string
	}{
		{"resolved model wins", "claude-opus-5", "claude-sonnet-5", "claude-sonnet-5"},
		{"unresolved falls back to requested", "claude-opus-5", "", "claude-opus-5"},
		{"both empty stays empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := OutcomeOf(&store.Event{EventSummary: store.EventSummary{
				ModelRequested: tc.requested,
				ModelResolved:  tc.resolved}}, nil)
			if got.Model != tc.want {
				t.Errorf("OutcomeOf().Model = %q, want %q", got.Model, tc.want)
			}
		})
	}
}

// TestOutcomeOfCarriesWarnings asserts one "kind: detail" line per warning,
// in the order attached — the whole reason Outcome carries strings and not a
// count.
func TestOutcomeOfCarriesWarnings(t *testing.T) {
	got := OutcomeOf(&store.Event{}, []store.Warning{
		{Kind: "upstream_error", Detail: "502"},
		{Kind: "analyzer_panic", Detail: "cost: index out of range"},
	})
	want := []string{"upstream_error: 502", "analyzer_panic: cost: index out of range"}
	if len(got.Warnings) != len(want) {
		t.Fatalf("OutcomeOf().Warnings = %v, want %v", got.Warnings, want)
	}
	for i := range want {
		if got.Warnings[i] != want[i] {
			t.Errorf("Warnings[%d] = %q, want %q", i, got.Warnings[i], want[i])
		}
	}
}

// TestOutcomeOfDurationFromInstants pins the one place claude-lens differs from
// the shape this was ported from: Event carries StartedAt/EndedAt and no
// derived duration column, so the subtraction happens here — and an event with
// no EndedAt (still streaming, or a capture that never completed) reports 0
// rather than a bogus or negative duration.
func TestOutcomeOfDurationFromInstants(t *testing.T) {
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	ended := start.Add(1500 * time.Millisecond)
	got := OutcomeOf(&store.Event{EventSummary: store.EventSummary{StartedAt: start, EndedAt: &ended}}, nil)
	if got.DurationMs != 1500 {
		t.Errorf("DurationMs = %v, want 1500", got.DurationMs)
	}

	if got := OutcomeOf(&store.Event{EventSummary: store.EventSummary{StartedAt: start}}, nil); got.DurationMs != 0 {
		t.Errorf("DurationMs = %v, want 0 when EndedAt is nil", got.DurationMs)
	}
}

// TestOutcomeOfCarriesCostAndStatus is the pass-through that makes a replay
// diff meaningful at all: the fields a comparison reads must be the row's, not
// defaults.
func TestOutcomeOfCarriesCostAndStatus(t *testing.T) {
	cost := 0.0042
	got := OutcomeOf(&store.Event{EventSummary: store.EventSummary{
		ID: 7, Status: 200, InputTokens: 11, OutputTokens: 22,
		CostUSD: &cost, CostSource: "shipped"}}, nil)
	if got.ID != 7 || got.Status != 200 || got.InputTokens != 11 || got.OutputTokens != 22 {
		t.Errorf("Outcome = %+v, want the row's id/status/tokens", got)
	}
	if got.CostUSD == nil || *got.CostUSD != cost || got.CostSource != "shipped" {
		t.Errorf("Outcome = %+v, want the row's cost and source", got)
	}
}
