package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Export is `clens export`: every matching call, as JSON lines or CSV.
//
// The two formats carry different things on purpose. JSON lines marshal the
// stored row as it is -- including the captured bodies, because that is what
// "every stored event" means -- which is the complete, machine-readable dump.
// CSV writes the scalar columns only: bodies are multi-line JSON blobs that a
// spreadsheet cannot use, and a CSV that silently embedded them would be the
// wrong format for both jobs.
//
// Both formats keep the two cost columns separate and a missing one absent
// (null in JSON, empty in CSV). Neither ever writes 0 for an unpriced row:
// a row that cost nothing is not a row whose cost is unknown (invariant 5).
func Export(args []string) error {
	return runExport(args, os.Stdout)
}

func runExport(args []string, w io.Writer) error {
	asCSV, args := hasFlag(args, "--csv")
	since, args := takeFlag(args, "--since")
	until, args := takeFlag(args, "--until")
	model, args := takeFlag(args, "--model")
	source, args := takeFlag(args, "--source")
	billing, args := takeFlag(args, "--billing")
	limit, args, err := intFlag(args, "--limit", 0)
	if err != nil {
		return err
	}

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer st.Close()

	sinceAt, err := parseSince(since)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	untilAt, err := parseUntil(until)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	filter := store.EventFilter{
		Model:       model,
		Source:      source,
		BillingMode: billing,
		Since:       sinceAt,
		Until:       untilAt,
	}

	ctx := context.Background()
	if !asCSV {
		return exportJSON(ctx, w, st, filter, limit)
	}
	return exportCSV(ctx, w, st, filter, limit)
}

// exportPage is how many rows are read at a time when the caller did not cap
// the export. Paging keeps an "export everything" from materializing the
// whole events table -- bodies included -- in one slice.
const exportPage = 500

// exportJSON writes one JSON object per line, paging until the store runs
// out or limit is reached.
func exportJSON(ctx context.Context, w io.Writer, st *store.Store, filter store.EventFilter, limit int) error {
	enc := json.NewEncoder(w)
	written := 0
	for offset := 0; ; offset += exportPage {
		page := filter
		page.Offset = offset
		page.Limit = pageSize(limit, written)
		if page.Limit == 0 {
			return nil
		}
		events, err := st.ListEvents(ctx, page)
		if err != nil {
			return fmt.Errorf("export: %w", err)
		}
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				return fmt.Errorf("export: %w", err)
			}
			written++
		}
		if len(events) < page.Limit {
			return nil
		}
	}
}

// exportColumns is the CSV header, in the order rowValues emits them. Kept as
// one list so a column can never be added to the header without a value to
// fill it.
var exportColumns = []string{
	"id", "request_id", "source", "started_at", "ended_at", "session_id", "project",
	"git_branch", "model_requested", "model_resolved", "billing_mode", "account",
	"status", "stop_category", "input_tokens", "output_tokens",
	"cache_write_5m_tokens", "cache_write_1h_tokens", "cache_read_tokens",
	"thinking_tokens", "total_prompt_tokens",
	"cost_usd", "api_equivalent_cost_usd", "cost_source", "replay_of",
}

func exportCSV(ctx context.Context, w io.Writer, st *store.Store, filter store.EventFilter, limit int) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(exportColumns); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	written := 0
	for offset := 0; ; offset += exportPage {
		page := filter
		page.Offset = offset
		page.Limit = pageSize(limit, written)
		if page.Limit == 0 {
			break
		}
		events, err := st.ListEvents(ctx, page)
		if err != nil {
			return fmt.Errorf("export: %w", err)
		}
		for _, ev := range events {
			if err := cw.Write(rowValues(ev)); err != nil {
				return fmt.Errorf("export: %w", err)
			}
			written++
		}
		if len(events) < page.Limit {
			break
		}
	}
	cw.Flush()
	return cw.Error()
}

// pageSize returns how many rows this page should ask for, honouring a
// caller's limit and returning 0 once it has been reached.
func pageSize(limit, written int) int {
	if limit <= 0 {
		return exportPage
	}
	if written >= limit {
		return 0
	}
	if remaining := limit - written; remaining < exportPage {
		return remaining
	}
	return exportPage
}

func rowValues(ev *store.Event) []string {
	ended := ""
	if ev.EndedAt != nil {
		ended = ev.EndedAt.Format(time.RFC3339)
	}
	return []string{
		strconv.FormatInt(ev.ID, 10),
		ev.RequestID,
		ev.Source,
		ev.StartedAt.Format(time.RFC3339),
		ended,
		ev.SessionID,
		ev.Project,
		ev.GitBranch,
		ev.ModelRequested,
		ev.ModelResolved,
		ev.BillingMode,
		ev.Account,
		statusCell(ev),
		ev.StopCategory,
		strconv.Itoa(ev.InputTokens),
		strconv.Itoa(ev.OutputTokens),
		strconv.Itoa(ev.CacheWrite5mTokens),
		strconv.Itoa(ev.CacheWrite1hTokens),
		strconv.Itoa(ev.CacheReadTokens),
		strconv.Itoa(ev.ThinkingTokens),
		strconv.Itoa(ev.TotalPromptTokens),
		csvCost(ev.CostUSD),
		csvCost(ev.ApiEquivalentCostUSD),
		ev.CostSource,
		ev.ReplayOf,
	}
}

// csvCost writes an exact decimal or an empty cell -- never "0" and never a
// currency symbol, so a spreadsheet parses the column as a number and an
// unpriced row stays visibly blank.
func csvCost(c *float64) string {
	if c == nil {
		return ""
	}
	return strconv.FormatFloat(*c, 'f', -1, 64)
}
