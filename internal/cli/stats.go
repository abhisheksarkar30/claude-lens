package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Stats is `clens stats`: window totals, a per-model split, and --by another
// grouping.
//
// Every total is reported as two labelled figures, never one merged number.
// `TotalCostUSD` sums api-billed rows; `TotalApiEquivalentCostUSD` sums
// subscription rows at list price. Adding them would be adding dollars spent
// to dollars a subscription already covered (invariant 5), so this command
// prints them side by side and lets the reader add whatever they actually
// mean to add.
func Stats(args []string) error {
	return runStats(args, os.Stdout)
}

func runStats(args []string, w io.Writer) error {
	by, args := takeFlag(args, "--by")
	period, args := takeFlag(args, "--period")
	since, args := takeFlag(args, "--since")
	until, args := takeFlag(args, "--until")
	model, args := takeFlag(args, "--model")
	session, args := takeFlag(args, "--session")
	billing, args := takeFlag(args, "--billing")
	asJSON, args := hasFlag(args, "--json")

	// --period is the window ("24h", "7d" as a duration, or an RFC3339 start).
	// It and --since are the same knob spelled two ways; --since wins if both
	// are given, so a script that passes both gets the explicit one.
	if since == "" {
		since = period
	}

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	defer st.Close()

	sinceAt, err := parseSince(since)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	untilAt, err := parseUntil(until)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	filter := store.EventFilter{
		Model:       model,
		SessionID:   session,
		BillingMode: billing,
		Since:       sinceAt,
		Until:       untilAt,
	}

	ctx := context.Background()
	summary, err := st.StatsSummary(ctx, filter)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	if asJSON {
		return writeStatsJSON(w, st, ctx, filter, by, summary)
	}

	fmt.Fprintf(w, "window:   %s\n", windowLabel(sinceAt, untilAt))
	fmt.Fprintf(w, "requests: %d\n", summary.RequestCount)
	fmt.Fprintf(w, "tokens:   %s\n", tokenLine(&store.Event{
		InputTokens:        summary.InputTokens,
		OutputTokens:       summary.OutputTokens,
		CacheWrite5mTokens: summary.CacheWrite5mTokens,
		CacheWrite1hTokens: summary.CacheWrite1hTokens,
		CacheReadTokens:    summary.CacheReadTokens,
		ThinkingTokens:     summary.ThinkingTokens,
		TotalPromptTokens:  summary.TotalPromptTokens,
	}))
	fmt.Fprintf(w, "cost:     %s\n", costCell(summary.TotalCostUSD, summary.TotalApiEquivalentCostUSD))
	fmt.Fprintf(w, "priced:   %d priced, %d unpriced\n", summary.PricedCount, summary.UnpricedCount)

	switch by {
	case "", "model":
		// The per-model split is the default, not only a --by choice: "what
		// did this window cost" is almost always followed by "on which model".
		if err := writeByModel(w, st, ctx, filter); err != nil {
			return err
		}
	case "day", "week", "month":
		periods, err := st.StatsByPeriod(ctx, filter, by)
		if err != nil {
			return fmt.Errorf("stats: by %s: %w", by, err)
		}
		fmt.Fprintf(w, "\nby %s:\n", by)
		rows := make([][]string, 0, len(periods))
		for _, p := range periods {
			rows = append(rows, []string{
				p.PeriodStart.Format("2006-01-02"),
				strconv.Itoa(p.RequestCount),
				humanTokens(p.TotalPromptTokens),
				humanTokens(p.OutputTokens),
				costCell(p.TotalCostUSD, p.TotalApiEquivalentCostUSD),
				warnCount(p.UnpricedCount),
			})
		}
		fmt.Fprint(w, table([]string{"PERIOD", "REQS", "PROMPT", "OUT", "COST", "UNPRICED"}, rows))
	case "session", "project":
		if err := writeGrouped(w, st, ctx, filter, by); err != nil {
			return err
		}
	default:
		return fmt.Errorf("stats: unknown --by value %q (want model, day, week, month, session or project)", by)
	}
	return nil
}

func writeByModel(w io.Writer, st *store.Store, ctx context.Context, filter store.EventFilter) error {
	byModel, err := st.StatsByModel(ctx, filter)
	if err != nil {
		return fmt.Errorf("stats: by model: %w", err)
	}
	fmt.Fprintln(w, "\nby model:")
	rows := make([][]string, 0, len(byModel))
	for _, m := range byModel {
		rows = append(rows, []string{
			m.Model,
			strconv.Itoa(m.RequestCount),
			humanTokens(m.TotalPromptTokens),
			humanTokens(m.OutputTokens),
			costCell(m.TotalCostUSD, m.TotalApiEquivalentCostUSD),
			warnCount(m.UnpricedCount),
		})
	}
	fmt.Fprint(w, table([]string{"MODEL", "REQS", "PROMPT", "OUT", "COST", "UNPRICED"}, rows))
	return nil
}

// writeGrouped groups by a column the store has no aggregate for (session and
// project are both per-row values, not grouped in SQL), so this reads the
// filtered rows and folds them in Go.
//
// ponytail: the whole filtered set is held in memory. That is one row per API
// call, so a heavy month is tens of thousands of rows -- fine here, and the
// alternative is two new store aggregate methods. Add StatsBySession /
// StatsByProject to the store if a window ever spans more than a season.
//
// The rows are folded by summing each row's own cost column into whichever
// side it belongs on, so the grouped figure obeys invariant 5 exactly as the
// per-row figure does: an api row adds to the api total, a subscription row
// to the equivalent total, and neither ever adds to the other.
func writeGrouped(w io.Writer, st *store.Store, ctx context.Context, filter store.EventFilter, by string) error {
	events, err := st.ListEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("stats: read events: %w", err)
	}

	type group struct {
		requests int
		prompt   int
		out      int
		apiCost  float64
		subCost  float64
		hasAPI   bool
		hasSub   bool
		unpriced int
	}
	groups := map[string]*group{}
	for _, ev := range events {
		key := ev.SessionID
		if by == "project" {
			key = ev.Project
		}
		if key == "" {
			key = "(none)"
		}
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		g.requests++
		g.prompt += ev.TotalPromptTokens
		g.out += ev.OutputTokens
		switch {
		case ev.CostUSD != nil:
			g.apiCost += *ev.CostUSD
			g.hasAPI = true
		case ev.ApiEquivalentCostUSD != nil:
			g.subCost += *ev.ApiEquivalentCostUSD
			g.hasSub = true
		default:
			g.unpriced++
		}
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintf(w, "\nby %s:\n", by)
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		var api, sub *float64
		if g.hasAPI {
			v := g.apiCost
			api = &v
		}
		if g.hasSub {
			v := g.subCost
			sub = &v
		}
		rows = append(rows, []string{
			k,
			strconv.Itoa(g.requests),
			humanTokens(g.prompt),
			humanTokens(g.out),
			costCell(api, sub),
			warnCount(g.unpriced),
		})
	}
	label := "SESSION"
	if by == "project" {
		label = "PROJECT"
	}
	fmt.Fprint(w, table([]string{label, "REQS", "PROMPT", "OUT", "COST", "UNPRICED"}, rows))
	return nil
}

// windowLabel describes the window in words, saying "all time" rather than
// printing a zero timestamp, which reads as 0001-01-01 and means nothing.
func windowLabel(since, until time.Time) string {
	switch {
	case since.IsZero() && until.IsZero():
		return "all time"
	case since.IsZero():
		return "up to " + until.Format(time.RFC3339)
	case until.IsZero():
		return "since " + since.Format(time.RFC3339)
	default:
		return since.Format(time.RFC3339) + " → " + until.Format(time.RFC3339)
	}
}

// statsJSON is the --json shape. It is defined here rather than reusing
// store.StatsSummary because the summary has no JSON tags (it is an internal
// row shape, not a wire format) and a field-for-field mirror of it would be
// the same boilerplate one indirection later.
type statsJSON struct {
	Window struct {
		Since *time.Time `json:"since,omitempty"`
		Until *time.Time `json:"until,omitempty"`
	} `json:"window"`
	Summary store.StatsSummary `json:"summary"`
	By      any                `json:"by,omitempty"`
}

func writeStatsJSON(w io.Writer, st *store.Store, ctx context.Context, filter store.EventFilter, by string, summary store.StatsSummary) error {
	out := statsJSON{Summary: summary}
	if !filter.Since.IsZero() {
		out.Window.Since = &filter.Since
	}
	if !filter.Until.IsZero() {
		out.Window.Until = &filter.Until
	}
	switch by {
	case "", "model":
		rows, err := st.StatsByModel(ctx, filter)
		if err != nil {
			return fmt.Errorf("stats: %w", err)
		}
		out.By = rows
	case "day", "week", "month":
		rows, err := st.StatsByPeriod(ctx, filter, by)
		if err != nil {
			return fmt.Errorf("stats: %w", err)
		}
		out.By = rows
	case "session", "project":
		// The Go-side grouping above is display-shaped; the JSON form leaves
		// it out rather than emitting a second, differently-shaped grouping
		// that a consumer would have to be told about. `clens export` is the
		// machine-readable path for per-row data.
	default:
		return fmt.Errorf("stats: unknown --by value %q", by)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	return nil
}
