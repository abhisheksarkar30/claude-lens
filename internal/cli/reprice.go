package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// Reprice is `clens reprice`: recompute the stored cost of every event row the
// effective pricing table can reconstruct, and rewrite the ones whose figure
// moved.
//
// reprice and reflag are both "the command that fixes history" and share a
// shape: one job per command, --dry-run prints what --yes would change, and
// nothing happens without --yes. The write loop lives in internal/store, the
// same split purge and rekey use, because the session rollup has to happen in
// the same transaction as the UPDATE.
func Reprice(args []string) error {
	return runReprice(args, os.Stdout)
}

func runReprice(args []string, w io.Writer) error {
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	if !dryRun && !yes {
		return errors.New("reprice: refusing to rewrite costs without --yes (add --dry-run to see what would change)")
	}

	cfg, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("reprice: %w", err)
	}
	defer st.Close()

	// The effective, Loader-backed table. Not pricing.Compute, which is
	// ShippedTable().Compute and would ignore a user override; and not the
	// nil-dates form, which means "unset -> the shipped calendar" and would
	// silently misprice every call on an install that configured its own
	// off-peak dates -- a state distinct from the `none` spelling that
	// excludes nothing. newPriceLoader is the one loader shape every pricer in
	// this process builds, so reprice cannot disagree with serve about them.
	counts, err := st.RepriceCosts(context.Background(), newPriceLoader(cfg).Table(), dryRun)
	if err != nil {
		return fmt.Errorf("reprice: %w", err)
	}

	// The same three counts either way, differing only in the verb: the
	// preview and the repair are the same loop, so they cannot drift.
	verb := "repriced"
	if dryRun {
		verb = "would reprice"
	}
	fmt.Fprintf(w, "%s %d row(s); %d already correct, %d skipped (not reconstructible from the stored columns)\n",
		verb, counts.Moved, counts.Unchanged, counts.Skipped)
	return nil
}
