package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/jsonlogs"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Ingest is `clens ingest`'s entrypoint: a one-shot backfill from Claude
// Code's own JSONL transcripts (br-GI-1-11's tailer), incremental by
// default and from byte 0 with --rebuild.
func Ingest(args []string) error {
	return runIngest(args, os.Stdout)
}

func runIngest(args []string, w io.Writer) error {
	rebuild, args := hasFlag(args, "--rebuild")

	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	// config.Load does not call Validate itself, so this path -- the one
	// `clens ingest --rebuild` takes -- is validated only here, and before the
	// store is opened so a bad config cannot half-run against a real database.
	if err := cfg.Validate(); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("ingest: open store: %w", err)
	}
	defer st.Close()

	ctx := context.Background()
	root := jsonlRoot()

	if rebuild {
		if err := resetJSONLCursors(ctx, st, root); err != nil {
			return fmt.Errorf("ingest: rebuild: %w", err)
		}
	}

	tailer := jsonlogs.New(root, st)
	tailer.SetPriceTable(newPriceLoader(cfg))
	if acct := firstAccount(cfg, "subscription"); acct.Name != "" {
		tailer.SetAccount(acct.Name, acct.BillingMode)
	}

	stats, err := tailer.Poll(ctx)
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	fmt.Fprintf(w, "jsonl: files=%d requests=%d inserted=%d failed=%d malformed=%d\n",
		stats.FilesWalked, stats.RequestsFound, stats.Inserted, stats.Failed, stats.Malformed)
	return nil
}

// jsonlRoot resolves Claude Code's own transcript tree, the same
// ~/.claude/projects convention internal/jsonlogs's package doc names.
func jsonlRoot() string {
	return filepath.Join(claudeConfigDir(), "projects")
}

// resolvedAPIPrefixes resolves the pay-as-you-go model prefixes: nil means
// "unset -> use the shipped default". A site that missed this resolution would
// fail silently -- the resolved list is only ever fed to strings.HasPrefix,
// which never matches over a nil slice, giving zero routing with no error.
func resolvedAPIPrefixes(cfg *config.Config) []string {
	if cfg.ApiModelPrefixes == nil {
		return pricing.ShippedAPIModelPrefixes()
	}
	return cfg.ApiModelPrefixes
}

// newPriceLoader is the one loader shape every pricer in this process wants,
// so two pricers cannot disagree about the configured off-peak dates.
func newPriceLoader(cfg *config.Config) *pricing.Loader {
	return pricing.NewLoader(pricing.DefaultPath(), cfg.PeakOffPeakDates)
}

// firstAccount returns the first configured account with the given
// billing mode, or a zero Account if none is configured.
func firstAccount(cfg *config.Config, billingMode string) config.Account {
	for _, a := range cfg.Accounts {
		if a.BillingMode == billingMode {
			return a
		}
	}
	return config.Account{}
}

// resetJSONLCursors zeroes every *.jsonl file's stored byte offset under
// root, so the next Poll re-reads each file from the start. It walks root
// itself rather than calling into internal/jsonlogs (whose walkFiles and
// cursor key are both unexported), using the same "jsonl:"+path key format
// jsonlogs' own cursor.go documents -- the request_id UNIQUE constraint
// absorbs the resulting re-read as a merge, never a duplicate row.
func resetJSONLCursors(ctx context.Context, st *store.Store, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		return st.SetIngestState(ctx, "jsonl:"+path, store.IngestState{Value: "0", Status: "ok"})
	})
}
