package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// Reflag is `clens reflag`: repair the capture_complete flags a cross-source
// merge laundered, from the one witness a merge cannot destroy -- a stored body
// that is a strict prefix of its client Content-Length.
//
// It is a --yes-gated writer, not a destructive command: it rewrites one column
// and deletes nothing. It touches no body, no header and no cost, so it is
// read-only without --yes and cannot widen the credential or privacy surface.
//
// reprice and reflag are both "the command that fixes history" and share a
// shape: one job per command, --dry-run prints what --yes would change, and
// nothing happens without --yes. The one statement and the three counts live in
// internal/store, exactly as purge and rekey split their CLI from their store
// work.
func Reflag(args []string) error {
	return runReflag(args, os.Stdout)
}

func runReflag(args []string, w io.Writer) error {
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	if !dryRun && !yes {
		return errors.New("reflag: refusing to rewrite capture_complete without --yes (add --dry-run to see what would change)")
	}

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("reflag: %w", err)
	}
	defer st.Close()

	counts, err := st.ReflagIncompleteCaptures(context.Background(), dryRun)
	if err != nil {
		return fmt.Errorf("reflag: %w", err)
	}

	// The same three buckets either way, differing only in the verb: the
	// preview and the repair are the same code, so the numbers an operator
	// reads before --yes are the numbers the repair acts on.
	verb := "flipped"
	if dryRun {
		verb = "would flip"
	}
	// The residual is the honest ceiling, so the report states its cause rather
	// than leaving a number the operator has to interpret: those rows were
	// laundered by the `||` and carry no Content-Length evidence, so nothing in
	// the store can prove which of them was cut. It is not a failure count.
	fmt.Fprintf(w, "%s %d row(s) to incomplete; %d already honest, %d residual (laundered, no Content-Length witness -- not repairable)\n",
		verb, counts.Flipped, counts.AlreadyHonest, counts.Residual)
	return nil
}
