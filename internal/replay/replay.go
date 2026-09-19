package replay

import "github.com/abhisheksarkar30/claude-lens/internal/store"

// Outcome is the compact, comparable view of one captured call: exactly the
// fields a replay is compared against its original on. It is what the replay
// endpoint returns for the call it just recorded and what `clens replay` builds
// for the baseline it diffs against, so both sides of a comparison come from
// one function rather than from two that could drift apart.
type Outcome struct {
	ID           int64    `json:"id"`
	Status       int      `json:"status"`
	Model        string   `json:"model"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	CostUSD      *float64 `json:"cost_usd"`
	CostSource   string   `json:"cost_source"`
	DurationMs   float64  `json:"duration_ms"`
	// Warnings is one "kind: detail" line per warning, in the order attached.
	// Counts alone would hide which warning changed, which is the usual reason
	// to look at a replay at all.
	Warnings []string `json:"warnings"`
}

// OutcomeOf builds the Outcome for ev given the warnings attached to it. Model
// prefers what the upstream reported back over what the client asked for,
// matching every other surface (internal/cli's displayModel): a replay that
// changed nothing observable but had its model substituted upstream is exactly
// the difference this comparison exists to show.
//
// DurationMs is derived from EndedAt - StartedAt rather than read off a
// duration column: claude-lens stores the two instants and no derived field
// (unlike deepseek-lens's Request.Duration), so the subtraction belongs here
// rather than in the schema. A row with no EndedAt — one still streaming, or
// one whose capture never completed — reports 0.
func OutcomeOf(ev *store.Event, warnings []store.Warning) Outcome {
	model := ev.ModelResolved
	if model == "" {
		model = ev.ModelRequested
	}
	out := Outcome{
		ID:           ev.ID,
		Status:       ev.Status,
		Model:        model,
		InputTokens:  ev.InputTokens,
		OutputTokens: ev.OutputTokens,
		CostUSD:      ev.CostUSD,
		CostSource:   ev.CostSource,
	}
	if ev.EndedAt != nil {
		out.DurationMs = float64(ev.EndedAt.Sub(ev.StartedAt).Milliseconds())
	}
	for _, w := range warnings {
		out.Warnings = append(out.Warnings, w.Kind+": "+w.Detail)
	}
	return out
}

// Result is POST /api/requests/{id}/replay's response body, and the shape
// `clens replay` decodes.
//
// Captured is false for a --no-capture replay: the request was sent, but the
// capture path skipped the sink, so there is no row to point at — Outcome is
// nil and Status is the upstream status read off the live response. ID is 0 in
// that case, because the id a replay is named by is a row id and no row was
// written.
type Result struct {
	ID       int64    `json:"id"`
	Captured bool     `json:"captured"`
	Status   int      `json:"status"`
	Outcome  *Outcome `json:"outcome,omitempty"`
}
