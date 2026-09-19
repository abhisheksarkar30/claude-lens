package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/reconcile"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// costDriftThresholdUSD is the absolute-dollar divergence beyond which a
// (day, model) row is flagged cost_drift. ponytail: fixed rather than
// configurable -- no bead adds a config field for it yet; wire one through
// config.Config if a user ever needs a different tolerance.
const costDriftThresholdUSD = 0.01

// Reconcile is `clens reconcile`'s entrypoint: computed (source A/B) vs
// billed (source D) cost per day and model, via internal/reconcile
// (br-GI-1-13). cost_drift only ever applies to API accounts -- a
// subscription-only install supplies no billed rows, so nothing to
// reconcile against, stated in the header rather than left implicit.
func Reconcile(args []string) error {
	return runReconcile(args, os.Stdout)
}

func runReconcile(args []string, w io.Writer) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("reconcile: open store: %w", err)
	}
	defer st.Close()

	ctx := context.Background()
	billed, err := st.ListAdminCostDays(ctx, time.Time{}, time.Time{})
	if err != nil {
		return fmt.Errorf("reconcile: list billed cost days: %w", err)
	}

	result, err := reconcile.Reconcile(ctx, st, billed, costDriftThresholdUSD)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	fmt.Fprintln(w, "cost_drift applies to API accounts only -- computed and billed are never summed:")
	fmt.Fprintf(w, "%-12s %-20s %14s %14s %12s %s\n", "day", "model", "computed_usd", "billed_usd", "diverged_usd", "drift")
	for _, row := range result.Rows {
		computed := "n/a"
		if row.ComputedUSD != nil {
			computed = fmt.Sprintf("%.2f", *row.ComputedUSD)
		}
		diverged := "n/a"
		if row.DivergedUSD != nil {
			diverged = fmt.Sprintf("%.2f", *row.DivergedUSD)
		}
		fmt.Fprintf(w, "%-12s %-20s %14s %14.2f %12s %t\n",
			row.Day.Format("2006-01-02"), row.Model, computed, row.BilledUSD, diverged, row.CostDrift)
	}
	if len(result.Rows) == 0 {
		fmt.Fprintln(w, "(no billed rows yet -- run `clens refresh` to pull the Admin cost report)")
	}
	fmt.Fprintf(w, "\nsource_mismatch warnings on record: %d\n", result.SourceMismatchCount)
	return nil
}
