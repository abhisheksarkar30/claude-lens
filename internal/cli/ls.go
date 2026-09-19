package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Ls is `clens ls`: the call log, newest first.
func Ls(args []string) error {
	return runLs(args, os.Stdout)
}

func runLs(args []string, w io.Writer) error {
	limit, args, err := intFlag(args, "--limit", 20)
	if err != nil {
		return err
	}
	model, args := takeFlag(args, "--model")
	source, args := takeFlag(args, "--source")
	billing, args := takeFlag(args, "--billing")
	session, args := takeFlag(args, "--session")
	since, args := takeFlag(args, "--since")
	asJSON, args := hasFlag(args, "--json")

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("ls: %w", err)
	}
	defer st.Close()

	sinceAt, err := parseSince(since)
	if err != nil {
		return fmt.Errorf("ls: %w", err)
	}
	ctx := context.Background()
	events, err := st.ListEvents(ctx, store.EventFilter{
		Model:       model,
		Source:      source,
		BillingMode: billing,
		SessionID:   session,
		Since:       sinceAt,
		Limit:       limit,
	})
	if err != nil {
		return fmt.Errorf("ls: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(w)
		for _, ev := range events {
			if err := enc.Encode(ev); err != nil {
				return fmt.Errorf("ls: %w", err)
			}
		}
		return nil
	}

	now := time.Now()
	rows := make([][]string, 0, len(events))
	for _, ev := range events {
		// ponytail: one point query per displayed row rather than a join, so
		// the ⚠ column matches exactly what `clens show` will list. A page is
		// 20 rows against a local SQLite file; add a batched warning count to
		// the store if the default limit ever grows by an order of magnitude.
		warnings, err := st.EventWarnings(ctx, ev.ID)
		if err != nil {
			return fmt.Errorf("ls: warnings for %d: %w", ev.ID, err)
		}
		rows = append(rows, []string{
			strconv.FormatInt(ev.ID, 10),
			relTime(ev.StartedAt, now),
			statusCell(ev),
			displayModel(ev),
			ev.BillingMode,
			humanTokens(ev.InputTokens),
			humanTokens(ev.OutputTokens),
			humanTokens(ev.CacheReadTokens),
			costCell(ev.CostUSD, ev.ApiEquivalentCostUSD) + costNote(ev.CostSource),
			warnCount(len(warnings)),
		})
	}

	if len(events) == 0 {
		fmt.Fprintln(w, "(no calls recorded)")
		return nil
	}
	fmt.Fprint(w, table(
		[]string{"ID", "WHEN", "STATUS", "MODEL", "BILLING", "IN", "OUT", "CACHE-R", "COST", "⚠"},
		rows,
	))
	return nil
}

// statusCell renders a row's status: the HTTP status for a proxy row, and the
// stop category for a jsonl row, which never made an HTTP call of its own.
// A 4xx/5xx is prefixed with "!" so a failure is visible in a column of
// numbers without reading each one.
func statusCell(ev *store.Event) string {
	if ev.Source == "proxy" && ev.Status != 0 {
		s := strconv.Itoa(ev.Status)
		if ev.Status >= 400 {
			s = "!" + s
		}
		return s
	}
	return displayOrDash(ev.StopCategory)
}
