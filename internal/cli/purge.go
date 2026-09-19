package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Purge is `clens purge`: delete captured rows by age or by the unpriced
// predicate, and optionally reclaim the file's pages afterwards.
//
// This is the one command in the CLI that destroys data, so the default is the
// opposite of destructive: nothing is deleted without --yes, and --dry-run
// prints what --yes would have deleted. There is no "--yes implies --dry-run
// first" dance -- the two flags are independent, so `--dry-run --yes` is
// simply a dry run, and the only way to delete anything is a run that passes
// --yes without --dry-run.
func Purge(args []string) error {
	return runPurge(args, os.Stdout)
}

func runPurge(args []string, w io.Writer) error {
	olderThan, args := takeFlag(args, "--older-than")
	unpriced, args := hasFlag(args, "--unpriced")
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	vacuum, args := hasFlag(args, "--vacuum")

	// Both selectors at once would delete the union while the output claims
	// one reason; refusing is more honest than silently ANDing or ORing them,
	// and the two are separate cleanup jobs anyway.
	if olderThan != "" && unpriced {
		return errors.New("purge: --older-than and --unpriced are separate jobs; pass one")
	}
	if olderThan == "" && !unpriced && !vacuum {
		return errors.New("purge: nothing to do (usage: clens purge --older-than 30d | --unpriced [--vacuum] [--dry-run] [--yes])")
	}
	if !dryRun && !yes {
		return errors.New("purge: refusing to delete without --yes (add --dry-run to see what would go)")
	}

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	defer st.Close()
	ctx := context.Background()

	switch {
	case olderThan != "":
		if err := purgeByAge(ctx, w, st, olderThan, dryRun); err != nil {
			return err
		}
	case unpriced:
		if err := purgeUnpriced(ctx, w, st, dryRun); err != nil {
			return err
		}
	}

	if vacuum {
		if dryRun {
			fmt.Fprintln(w, "vacuum: skipped (nothing is reclaimed during a --dry-run)")
			return nil
		}
		if err := st.Vacuum(ctx); err != nil {
			return fmt.Errorf("purge: vacuum: %w", err)
		}
		fmt.Fprintln(w, "vacuum: done")
	}
	return nil
}

// purgeByAge deletes everything started before the cutoff.
func purgeByAge(ctx context.Context, w io.Writer, st *store.Store, olderThan string, dryRun bool) error {
	cutoff, err := purgeCutoff(olderThan)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	if cutoff.IsZero() {
		return errors.New("purge: --older-than needs a duration like 720h, a day count like 30d, or an RFC3339 timestamp")
	}

	if dryRun {
		count, err := st.CountPurgeable(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("purge: %w", err)
		}
		bytes, err := st.PurgeableBytes(ctx, cutoff)
		if err != nil {
			return fmt.Errorf("purge: %w", err)
		}
		fmt.Fprintf(w, "would delete %d row(s) started before %s, reclaiming about %s\n",
			count, cutoff.Format(time.RFC3339), humanBytes(bytes))
		return nil
	}

	deleted, err := st.PurgeOlderThan(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	fmt.Fprintf(w, "deleted %d row(s) started before %s\n", deleted, cutoff.Format(time.RFC3339))
	return nil
}

// purgeCutoff resolves an --older-than value: a day count ("30d"), a Go
// duration ("720h", meaning that long ago), or an RFC3339 timestamp. The day
// suffix is handled here rather than in parseSince because Go's duration
// parser has no day unit and `--older-than 30d` is what anyone would type;
// parseSince stays the shared "how long ago" rule for the read commands.
func purgeCutoff(s string) (time.Time, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --older-than value %q (want a day count like 30d)", s)
		}
		return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	return parseSince(s)
}

// purgeUnpriced drops the rows whose cost could not be computed -- no rate
// known for the model at capture time. Those rows are the ones a later pricing
// fix can never repair, because the tokens are still there but the rate table
// they were priced against was not.
//
// ponytail: the dry run counts in Go over ListEvents rather than through a
// CountUnpriced store method. Add one if an events table ever grows past what
// a scan can walk without the user noticing the pause.
func purgeUnpriced(ctx context.Context, w io.Writer, st *store.Store, dryRun bool) error {
	if dryRun {
		events, err := st.ListEvents(ctx, store.EventFilter{Limit: purgeScanLimit})
		if err != nil {
			return fmt.Errorf("purge: %w", err)
		}
		unpriced := 0
		for _, ev := range events {
			if ev.CostUSD == nil && ev.ApiEquivalentCostUSD == nil {
				unpriced++
			}
		}
		fmt.Fprintf(w, "would delete %d unpriced row(s) (scanned the %d most recent)\n", unpriced, len(events))
		return nil
	}

	deleted, err := st.PurgeUnpriced(ctx)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	fmt.Fprintf(w, "deleted %d unpriced row(s)\n", deleted)
	return nil
}

// purgeScanLimit bounds the dry-run scan. It is the same newest-first page the
// detail commands read, so the number reported is a floor rather than a
// promise -- which is why the line says how many rows were scanned.
const purgeScanLimit = 1000

// humanBytes formats a byte count at the unit that keeps it readable. Binary
// units (KiB/MiB), because this describes a SQLite file.
func humanBytes(n int64) string {
	const unit = 1024
	switch {
	case n < unit:
		return fmt.Sprintf("%d B", n)
	case n < unit*unit:
		return fmt.Sprintf("%.1f KiB", float64(n)/unit)
	case n < unit*unit*unit:
		return fmt.Sprintf("%.1f MiB", float64(n)/(unit*unit))
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/(unit*unit*unit))
	}
}
