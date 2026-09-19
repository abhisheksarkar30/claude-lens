package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sessions is `clens sessions`: one row per agentic run.
//
// The cost column carries both billing models, labelled -- `api $1.2300` and
// `sub $4.5600` -- and never their sum. A session billed entirely on a
// subscription shows `api —`; it does not show `$0.00`, because "no
// API-billed calls" and "API-billed calls that cost nothing" are different
// facts and only one of them is true (invariant 5).
func Sessions(args []string) error {
	return runSessions(args, os.Stdout)
}

func runSessions(args []string, w io.Writer) error {
	limit, args, err := intFlag(args, "--limit", 20)
	if err != nil {
		return err
	}
	offset, args, err := intFlag(args, "--offset", 0)
	if err != nil {
		return err
	}
	asJSON, args := hasFlag(args, "--json")

	_, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	defer st.Close()

	ctx := context.Background()
	sessions, err := st.ListSessions(ctx, limit, offset)
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(w)
		for _, s := range sessions {
			if err := enc.Encode(s); err != nil {
				return fmt.Errorf("sessions: %w", err)
			}
		}
		return nil
	}

	if len(sessions) == 0 {
		fmt.Fprintln(w, "(no sessions recorded)")
		return nil
	}

	now := time.Now()
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		rows = append(rows, []string{
			s.ID,
			relTime(s.LastSeen, now),
			strconv.Itoa(s.RequestCount),
			humanTokens(s.TotalPromptTokens),
			humanTokens(s.OutputTokens),
			costCell(s.TotalCostUSD, s.TotalApiEquivalentCostUSD),
			warnCount(s.UnpricedCount),
			warnCount(s.WarningCount),
			strings.Join(s.ModelSet, ","),
		})
	}
	fmt.Fprint(w, table(
		[]string{"SESSION", "LAST SEEN", "TURNS", "PROMPT", "OUT", "COST", "UNPRICED", "⚠", "MODELS"},
		rows,
	))
	return nil
}
