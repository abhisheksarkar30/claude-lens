// Package cli implements the `clens` command surface: one exported entrypoint
// per subcommand, each writing to os.Stdout and each backed by an unexported,
// testable core that takes an io.Writer and (where relevant) a *store.Store --
// so tests capture output with a bytes.Buffer instead of spawning a process.
//
// This file holds the shared output helpers: human unit formatting, the
// column-aligned table renderer, and the store/config glue every read-only
// subcommand repeats.
package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// collectFlag pulls every occurrence of a repeatable flag out of args, in
// order, leaving the rest untouched: `--set x=1 --set y=2` and `--set=x=1` are
// both accepted. Stdlib flag cannot express a repeatable string flag, so
// prices.go and replay.go route through here rather than each rolling their
// own scan. The value is whatever followed the flag, even if it looks like one.
func collectFlag(args []string, name string) (values, rest []string) {
	prefix := name + "="
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == name && i+1 < len(args):
			i++
			values = append(values, args[i])
		case strings.HasPrefix(a, prefix):
			values = append(values, strings.TrimPrefix(a, prefix))
		default:
			rest = append(rest, a)
		}
	}
	return values, rest
}

// humanTokens formats a token count compactly: a plain integer under 1000,
// otherwise one decimal place with a k/M suffix.
func humanTokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// humanDuration formats a duration at whichever unit keeps it readable.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

// humanCost formats one cost to 4 decimal places, or "—" when it is NULL.
// Never "$0.00": an unpriced row and a free row are different facts
// (invariant 5).
func humanCost(c *float64) string {
	if c == nil {
		return "—"
	}
	return fmt.Sprintf("$%.4f", *c)
}

// costCell renders the two cost columns a row carries, each labelled with the
// billing model it is denominated in. The two figures are never added
// together: an api dollar and a subscription api-equivalent dollar are
// different units (invariant 5), and the `unpriced` column below says how
// much of the row the figures do not cover.
func costCell(api, sub *float64) string {
	return fmt.Sprintf("api %s  sub %s", humanCost(api), humanCost(sub))
}

// costNote qualifies a cost figure with why it is what it is, for the sources
// that mean "not the shipped table" -- see costCell for the same rule on a
// whole row.
func costNote(source string) string {
	if source == "" || source == "shipped" {
		return ""
	}
	return " (" + source + ")"
}

// relTime formats t relative to now: "just now" under a minute, then
// minutes/hours/days ago.
func relTime(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// warnCount renders a ⚠ cell: the warning count, or nothing when there were
// none, so the column stays scannable down a long list.
func warnCount(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func displayOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// isTTY reports whether w is a character-device *os.File. Stdlib only, per
// the project's no-new-dependency rule: any writer that is not a real
// terminal -- a bytes.Buffer, a file, a pipe -- reports false, which is what
// suppresses ANSI escapes when output is piped.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

// tableGutter separates columns.
const tableGutter = "  "

// table renders headers and rows as an aligned, space-padded table. Columns
// are sized to their widest cell; a ragged row (fewer cells than headers)
// renders the missing cells blank.
func table(headers []string, rows [][]string) string {
	n := len(headers)
	widths := make([]int, n)
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i := 0; i < n && i < len(row); i++ {
			if w := utf8.RuneCountInString(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var b strings.Builder
	writeRow(&b, headers, widths)
	for _, row := range rows {
		writeRow(&b, row, widths)
	}
	return b.String()
}

func writeRow(b *strings.Builder, cells []string, widths []int) {
	for i, w := range widths {
		var cell string
		if i < len(cells) {
			cell = cells[i]
		}
		if i > 0 {
			b.WriteString(tableGutter)
		}
		b.WriteString(cell)
		// The last column is never padded, so a table has no trailing
		// whitespace for a diff or a copy-paste to carry along.
		if pad := w - utf8.RuneCountInString(cell); pad > 0 && i < len(widths)-1 {
			b.WriteString(strings.Repeat(" ", pad))
		}
	}
	b.WriteByte('\n')
}

// openStore resolves config from args -- by the time a subcommand calls this
// it has already stripped its own flags, so what remains is config flags --
// and opens the SQLite file. Every read-only subcommand shares it.
func openStore(args []string) (*config.Config, *store.Store, error) {
	cfg, err := config.Load(args)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return cfg, st, nil
}

// intFlag pulls a "--name N" flag, defaulting to def when it is absent. An
// unparseable value is an error rather than a silent fallback: `--limit abc`
// quietly meaning 20 is the kind of thing a user only notices by counting
// rows.
func intFlag(args []string, name string, def int) (int, []string, error) {
	raw, rest := takeFlag(args, name)
	if raw == "" {
		return def, rest, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, rest, fmt.Errorf("invalid %s value %q (want an integer)", name, raw)
	}
	return n, rest, nil
}

// displayModel is the model a row is *labelled* with: what upstream reported
// back if it reported anything, else what was asked for. Every surface that
// names a model goes through this so `ls`, `show` and a replay diff can never
// disagree about which model a call was.
func displayModel(ev *store.EventSummary) string {
	if ev.ModelResolved != "" {
		return ev.ModelResolved
	}
	return displayOrDash(ev.ModelRequested)
}

// parseSince accepts a Go duration ("24h", meaning "that long ago"), a
// RFC3339 timestamp, or "" (the zero time: unbounded).
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since value %q (want a duration like 24h or an RFC3339 timestamp)", s)
}

// parseUntil accepts an RFC3339 timestamp or "" (the zero time: unbounded).
func parseUntil(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --until value %q (want an RFC3339 timestamp)", s)
	}
	return t, nil
}
