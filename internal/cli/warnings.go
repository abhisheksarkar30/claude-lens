package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Warnings is `clens warnings`: analyzer findings grouped by kind, or one row
// per occurrence with --detail.
//
// Grouped-by-kind is the default because that is the question worth asking of
// a warning table -- "what keeps happening" -- and a flat list of hundreds of
// rows answers it only by hand. --detail is the drill-down.
func Warnings(args []string) error {
	return runWarnings(args, os.Stdout)
}

func runWarnings(args []string, w io.Writer) error {
	detail, args := hasFlag(args, "--detail")
	kind, args := takeFlag(args, "--kind")
	limit, args, err := intFlag(args, "--limit", 50)
	if err != nil {
		return err
	}

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("warnings: %w", err)
	}
	defer st.Close()
	ctx := context.Background()

	if !detail {
		// WarningSummary is deliberately unpaginated -- it is one row per
		// kind, and truncating it would hide exactly the kind a user is
		// looking for. --kind is a detail-mode filter and is ignored here.
		summary, err := st.WarningSummary(ctx)
		if err != nil {
			return fmt.Errorf("warnings: %w", err)
		}
		if len(summary) == 0 {
			fmt.Fprintln(w, "no warnings")
			return nil
		}
		rows := make([][]string, 0, len(summary))
		total := 0
		for _, s := range summary {
			total += s.Count
			rows = append(rows, []string{s.Kind, strconv.Itoa(s.Count)})
		}
		fmt.Fprint(w, table([]string{"KIND", "COUNT"}, rows))
		fmt.Fprintf(w, "\n%d warning(s) across %d kind(s); --detail for one row per occurrence\n", total, len(summary))
		return nil
	}

	rows, err := st.ListWarnings(ctx, store.WarningFilter{Kind: kind, Limit: limit})
	if err != nil {
		return fmt.Errorf("warnings: %w", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "no warnings")
		return nil
	}
	out := make([][]string, 0, len(rows))
	for _, warn := range rows {
		out = append(out, []string{
			strconv.FormatInt(warn.ID, 10),
			strconv.FormatInt(warn.EventID, 10),
			warn.Severity,
			warn.Kind,
			warn.Detail,
			displayOrDash(warn.Path),
		})
	}
	fmt.Fprint(w, table([]string{"ID", "EVENT", "SEV", "KIND", "DETAIL", "PATH"}, out))
	return nil
}
