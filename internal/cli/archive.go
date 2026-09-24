package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Archive is `clens archive status|run|restore`: inspect, run and undo body
// archival. `run` and `restore` follow every other writer's gate -- nothing is
// written without --yes, and --dry-run reports what --yes would do.
func Archive(args []string) error {
	return runArchive(args, os.Stdout)
}

const archiveUsage = "usage: clens archive status | run [--dry-run] [--yes] | restore --since X --until Y [--dry-run] [--yes]"

func runArchive(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("archive: " + archiveUsage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "status":
		return archiveStatus(rest, w)
	case "run":
		return archiveRun(rest, w)
	case "restore":
		return archiveRestore(rest, w)
	}
	return fmt.Errorf("archive: unknown verb %q (%s)", verb, archiveUsage)
}

func archiveStatus(args []string, w io.Writer) error {
	cfg, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer st.Close()

	s, err := st.ArchiveStatus(context.Background())
	if err != nil {
		return fmt.Errorf("archive: status: %w", err)
	}
	if cfg.HotDays > 0 {
		boundary := time.Now().UTC().Add(-time.Duration(cfg.HotDays) * 24 * time.Hour)
		fmt.Fprintf(w, "hot window:   %d day(s); rows started before %s UTC are archived\n", cfg.HotDays, boundary.Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintln(w, "hot window:   archival is disabled (hot_days = 0)")
	}
	fmt.Fprintf(w, "rows:         %d archived, %d not archived\n", s.Archived, s.Events-s.Archived)
	fmt.Fprintf(w, "archive dir:  %d day file(s), %s\n", s.DayFiles, humanBytes(s.DirBytes))
	fmt.Fprintf(w, "held back:    %d row(s) awaiting `clens backfill-tool-names`\n", s.AwaitingBackfill)
	fmt.Fprintf(w, "missing:      %d marker(s) whose day file or day-file row is gone\n", s.Missing)
	fmt.Fprintf(w, "duplicates:   %d row(s) restored but still in a day file (harmless: the hot row is authoritative and hydration never overwrites it)\n", s.Duplicates)
	return nil
}

func archiveRun(args []string, w io.Writer) error {
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	if !dryRun && !yes {
		return errors.New("archive: refusing to move bodies without --yes (add --dry-run to see what would move)")
	}
	cfg, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer st.Close()
	if cfg.HotDays <= 0 {
		fmt.Fprintln(w, "archival is disabled (hot_days = 0); nothing to do")
		return nil
	}

	ctx := context.Background()
	if dryRun {
		n, err := st.CountArchivable(ctx, cfg.HotDays, time.Now())
		if err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		fmt.Fprintf(w, "would archive the bodies of %d row(s) older than %d day(s)\n", n, cfg.HotDays)
		return nil
	}
	res, err := st.NewArchiver(func() int { return cfg.HotDays }).Run(ctx)
	if err != nil {
		return fmt.Errorf("archive: run: %w (archived %d row(s) first)", err, res.Archived)
	}
	fmt.Fprintf(w, "archived %d row(s) in %d batch(es)", res.Archived, res.Batches)
	if res.SkippedBackfill > 0 {
		fmt.Fprintf(w, "; held back %d row(s) awaiting `clens backfill-tool-names`", res.SkippedBackfill)
	}
	fmt.Fprintln(w)
	return nil
}

func archiveRestore(args []string, w io.Writer) error {
	sinceRaw, args := takeFlag(args, "--since")
	untilRaw, args := takeFlag(args, "--until")
	dryRun, args := hasFlag(args, "--dry-run")
	yes, args := hasFlag(args, "--yes")
	if sinceRaw == "" && untilRaw == "" {
		return errors.New("archive: restore needs --since and/or --until (a duration like 720h, or an RFC3339 timestamp)")
	}
	if !dryRun && !yes {
		return errors.New("archive: refusing to restore without --yes (add --dry-run to see what would be restored)")
	}
	since, err := parseSince(sinceRaw)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	until, err := parseUntil(untilRaw)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}

	cfg, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer st.Close()

	if _, running, _ := readServeState(cfg.DBPath); running && !dryRun {
		fmt.Fprintln(w, "warning: a serve state file exists, so `clens serve` may be running; restore is meant to run with it stopped")
	}

	res, err := st.RestoreBodies(context.Background(), since, until, dryRun, nil)
	if err != nil {
		return fmt.Errorf("archive: restore: %w", err)
	}
	verb := "restored"
	if dryRun {
		verb = "would restore"
	}
	fmt.Fprintf(w, "%s %d row(s)\n", verb, res.Restored)
	for _, f := range res.Failed {
		fmt.Fprintf(w, "left event %d (%s) archived: %s\n", f.EventID, f.Day, f.Reason)
	}
	if len(res.Failed) > 0 {
		return fmt.Errorf("archive: %d row(s) could not be restored and were left untouched", len(res.Failed))
	}
	return nil
}
