package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// BackfillToolNames is `clens backfill-tool-names`: fills req_tool_names
// (br-GI-13-07) on every row written before that column existed, so
// ruleCacheInvalidatedByTools does not silently decline on real history --
// the same permanent-loss shape D9 exists to prevent.
//
// It follows the established one-shot pattern of rekey / reprice / reflag:
// nothing happens without --yes, --dry-run reports the count only. Unlike
// those, the store work here is a page-at-a-time read-parse-write loop
// rather than a single UPDATE, because the encoding is parse.ExtractMeta's,
// computed in Go -- a json_extract-based SQL backfill would be a second,
// independent definition of "tool names".
func BackfillToolNames(args []string) error {
	return runBackfillToolNames(args, os.Stdout)
}

func runBackfillToolNames(args []string, w io.Writer) error {
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	if !dryRun && !yes {
		return errors.New("backfill-tool-names: refusing to write req_tool_names without --yes (add --dry-run to see what would change)")
	}

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("backfill-tool-names: %w", err)
	}
	defer st.Close()

	ctx := context.Background()

	if dryRun {
		n, err := st.CountEventsAwaitingToolNamesBackfill(ctx)
		if err != nil {
			return fmt.Errorf("backfill-tool-names: %w", err)
		}
		fmt.Fprintf(w, "would fill %d row(s)\n", n)
		return nil
	}

	var filled int
	var afterID int64
	for {
		rows, err := st.EventsAwaitingToolNamesBackfill(ctx, afterID, store.ToolNamesBackfillPageSize)
		if err != nil {
			return fmt.Errorf("backfill-tool-names: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			names := parse.ExtractMeta(r.ReqBody, http.Header{}).ToolNames
			if err := st.SetReqToolNames(ctx, r.ID, store.EncodeToolNames(names)); err != nil {
				return fmt.Errorf("backfill-tool-names: row %d: %w", r.ID, err)
			}
			filled++
			afterID = r.ID
		}
	}
	fmt.Fprintf(w, "filled %d row(s)\n", filled)
	return nil
}
