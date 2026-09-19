package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Models is `clens models`'s entrypoint: the shipped/override rate
// catalogue (br-GI-1-07), plus which models observed in the store have no
// rate at all -- listed distinctly, never folded into the catalogue's
// count (CLAUDE.md's "no invented numbers").
func Models(args []string) error {
	return runModels(args, os.Stdout)
}

func runModels(args []string, w io.Writer) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("models: open store: %w", err)
	}
	defer st.Close()

	table := pricing.NewLoader(pricing.DefaultPath(), nil).Table()

	fmt.Fprintln(w, "catalogue:")
	names := make([]string, 0, len(table))
	for model := range table {
		names = append(names, model)
	}
	sort.Strings(names)
	for _, model := range names {
		r := table[model]
		fmt.Fprintf(w, "  %-22s %s\n", model, r.Source)
	}

	stats, err := st.StatsByModel(context.Background(), store.EventFilter{})
	if err != nil {
		return fmt.Errorf("models: stats by model: %w", err)
	}

	fmt.Fprintln(w, "\nobserved:")
	for _, s := range stats {
		label := "priced"
		if _, ok := table[s.Model]; !ok || s.UnpricedCount > 0 {
			label = "unpriced"
		}
		fmt.Fprintf(w, "  %-22s %-9s priced=%d unpriced=%d\n", s.Model, label, s.PricedCount, s.UnpricedCount)
	}
	return nil
}
