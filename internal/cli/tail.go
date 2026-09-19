package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// tailInterval is how often the follow loop looks for new rows. A second is
// the resolution someone watching a coding session can perceive; polling
// faster would only burn queries the store cannot answer differently.
const tailInterval = time.Second

// Tail is `clens tail`: the last few calls, then every new one as it lands.
// It runs until interrupted.
func Tail(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runTail(ctx, args, os.Stdout, tailInterval)
}

// runTail takes its context rather than deriving one from the signal itself:
// the follow loop only ends when that context is cancelled, so a test has to
// be able to cancel it without sending the process a real interrupt.
func runTail(ctx context.Context, args []string, w io.Writer, interval time.Duration) error {
	limit, args, err := intFlag(args, "--limit", 20)
	if err != nil {
		return err
	}
	model, args := takeFlag(args, "--model")
	source, args := takeFlag(args, "--source")

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("tail: %w", err)
	}
	defer st.Close()

	filter := store.EventFilter{Model: model, Source: source, Limit: limit}

	// The seed page, so `clens tail` shows the calls that just happened
	// rather than an empty screen until the next one.
	page, err := st.ListEvents(ctx, filter)
	if err != nil {
		return fmt.Errorf("tail: %w", err)
	}
	if len(page) == 0 {
		fmt.Fprintln(w, "(waiting for calls)")
	}
	lastID := printNew(w, page, 0)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		// ponytail: re-reads the newest `limit` rows and keeps the ones past
		// lastID, instead of a "WHERE id > ?" store method. At 20 rows a
		// second against a local file this is free, and the ceiling is
		// explicit: more than `limit` calls landing inside one tick would
		// drop the oldest of them. Raise --limit, or add an AfterID field to
		// EventFilter, if a tail is ever pointed at a batch workload.
		page, err := st.ListEvents(ctx, filter)
		if err != nil {
			fmt.Fprintf(w, "tail: %v\n", err)
			continue // a transient read error must not end the follow
		}
		lastID = printNew(w, page, lastID)
	}
}

// printNew writes every event newer than since, oldest-first, and returns the
// highest id in the page. ListEvents is newest-first, so the loop walks the
// page backwards to keep the output in arrival order.
func printNew(w io.Writer, events []*store.Event, since int64) int64 {
	var highest int64
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.ID > highest {
			highest = ev.ID
		}
		if ev.ID <= since {
			continue
		}
		fmt.Fprintf(w, "%s %6s %-5s %-24s in=%-7s out=%-7s %s\n",
			ev.StartedAt.Format("15:04:05"),
			strconv.FormatInt(ev.ID, 10),
			statusCell(ev),
			displayModel(ev),
			humanTokens(ev.InputTokens),
			humanTokens(ev.OutputTokens),
			costCell(ev.CostUSD, ev.ApiEquivalentCostUSD),
		)
	}
	return highest
}
