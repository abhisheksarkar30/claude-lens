package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/abhisheksarkar30/claude-lens/internal/session"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Rekey is `clens rekey`: the story's second destructive command (see
// purge.go's doc comment for the first) and its only non-re-derivable step.
// It repairs the historical duplicates the forward identity fix
// (br-GI-9-01..03, -07) cannot reach: proxy rows still keyed by a synthetic
// fallback (pass 1), proxy rows still attributed to a minted session instead
// of their real conversation (pass 2), and JSONL rows whose identity was
// never stored at all (pass 3, delete-and-re-ingest).
//
// Nothing happens without --yes; --dry-run reports what --yes would do and
// writes nothing.
func Rekey(args []string) error {
	return runRekey(args, os.Stdout)
}

func runRekey(args []string, w io.Writer) error {
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	if !dryRun && !yes {
		return errors.New("rekey: refusing to write without --yes (add --dry-run to see what would change)")
	}

	cfg, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("rekey: %w", err)
	}
	defer st.Close()
	ctx := context.Background()
	root := jsonlRoot()

	// Pass 3's precondition is checked first, before pass 1: pass 1 never
	// touches a jsonl: row, so the check is valid before any mutation, and
	// a failed check must leave every row untouched -- a refusal, not an
	// exclusion, because the delete below is unconditional over the
	// jsonl: prefix while the recorded cursors are what the precondition
	// verifies against a live file.
	if err := checkRekeyPrecondition(ctx, st, root); err != nil {
		return fmt.Errorf("rekey: refusing: %w", err)
	}

	pass1, err := st.RekeyProxyBodyIDs(ctx, cfg.BodyCapBytes, dryRun)
	if err != nil {
		return fmt.Errorf("rekey: pass 1: %w", err)
	}
	pass2, err := st.RekeyProxySessions(ctx, dryRun)
	if err != nil {
		return fmt.Errorf("rekey: pass 2: %w", err)
	}
	dangling, err := st.DanglingReplayOfCount(ctx)
	if err != nil {
		return fmt.Errorf("rekey: %w", err)
	}

	if dryRun {
		n, err := st.CountJSONLKeyedEvents(ctx)
		if err != nil {
			return fmt.Errorf("rekey: pass 3: %w", err)
		}
		fmt.Fprintf(w, "would re-key %d, would leave %d synthetic (no body id), would re-attribute %d, would leave %d unattributed\n",
			pass1.ReKeyed, pass1.Synthetic, pass2.Reattributed, pass2.Unattributed)
		fmt.Fprintf(w, "would leave %d dangling replay_of\n", dangling)
		if pass1.Skipped > 0 {
			fmt.Fprintf(w, "would skip %d archived row(s) in pass 1 -- run `clens archive restore` first to include them\n", pass1.Skipped)
		}
		fmt.Fprintf(w, "would delete %d jsonl-keyed row(s) and re-derive them from the transcripts, re-pricing against the current rate table\n", n)
		return nil
	}

	fmt.Fprintf(w, "pass 1 (proxy body id): re-keyed %d, left %d synthetic (no body id)\n", pass1.ReKeyed, pass1.Synthetic)
	fmt.Fprintf(w, "pass 2 (proxy session): re-attributed %d, left %d unattributed\n", pass2.Reattributed, pass2.Unattributed)
	fmt.Fprintf(w, "dangling replay_of: %d\n", dangling)
	if pass1.Skipped > 0 {
		fmt.Fprintf(w, "pass 1 skipped %d archived row(s) -- run `clens archive restore` first to include them\n", pass1.Skipped)
	}

	deleted, err := st.DeleteJSONLKeyedEvents(ctx)
	if err != nil {
		return fmt.Errorf("rekey: pass 3: delete: %w", err)
	}
	if err := resetJSONLCursors(ctx, st, root); err != nil {
		return fmt.Errorf("rekey: pass 3: reset cursors: %w", err)
	}
	sess := session.New(st, cfg.SessionGapMinutes)
	tailer := newTailer(cfg, root, st, sess)
	stats, err := tailer.Poll(ctx)
	if err != nil {
		return fmt.Errorf("rekey: pass 3: re-ingest: %w", err)
	}
	fmt.Fprintf(w, "pass 3 (jsonl re-ingest): deleted %d jsonl-keyed row(s), re-inserted %d (re-priced against the current rate table)\n",
		deleted, stats.Inserted)
	return nil
}

// checkRekeyPrecondition is pass 3's precondition, run before any pass
// mutates anything: every recorded "jsonl:<path>" cursor must name a file
// that still exists, is at least as large as the recorded byte offset, and
// lives under root. There is no row->file link in the schema to check
// instead -- the key embeds a sessionId and a uuid, not a path, and a
// sidechain row takes its parent's sessionId -- so the cursor table is the
// only thing this check can walk. A recorded cursor that fails any of these
// is a refusal, not an exclusion: the delete in pass 3 is unconditional over
// the jsonl: prefix, so silently skipping an unreadable cursor would still
// destroy rows nothing could rebuild.
func checkRekeyPrecondition(ctx context.Context, st *store.Store, root string) error {
	entries, err := st.IngestStateKeysWithPrefix(ctx, "jsonl:")
	if err != nil {
		return fmt.Errorf("precondition: %w", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("precondition: resolve jsonl root: %w", err)
	}
	for _, e := range entries {
		path := strings.TrimPrefix(e.Key, "jsonl:")

		rel, err := filepath.Rel(absRoot, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("transcript %q lies outside the jsonl root %q", path, absRoot)
		}

		wantBytes, err := strconv.ParseInt(e.Value, 10, 64)
		if err != nil {
			return fmt.Errorf("transcript %q has an unreadable cursor value %q", path, e.Value)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("transcript %q is unreadable: %w", path, err)
		}
		if info.Size() < wantBytes {
			return fmt.Errorf("transcript %q is %d byte(s), shorter than its recorded cursor at %d", path, info.Size(), wantBytes)
		}
	}
	return nil
}
