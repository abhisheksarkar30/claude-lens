package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/decode"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Show is `clens show <id>`: one call in full.
//
// The id is the first positional argument -- `clens show 42 --db-path …` --
// rather than "the first non-flag argument", because telling a flag's value
// apart from a positional in a mixed argv is guesswork once config flags are
// allowed in the same list.
func Show(args []string) error {
	return runShow(args, os.Stdout)
}

func runShow(args []string, w io.Writer) error {
	withBody, args := hasFlag(args, "--body")
	if len(args) == 0 {
		return errors.New("show: missing request id (usage: clens show <id> [--body])")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("show: invalid request id %q", args[0])
	}
	args = args[1:]

	cfg, st, err := openStore(args)
	if err != nil {
		return fmt.Errorf("show: %w", err)
	}
	defer st.Close()

	ctx := context.Background()
	ev, err := st.GetEvent(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("show: request %d not found", id)
		}
		return fmt.Errorf("show: %w", err)
	}

	now := time.Now()
	fmt.Fprintf(w, "request %d\n", ev.ID)
	kv := [][2]string{
		{"when", ev.StartedAt.Format(time.RFC3339) + " (" + relTime(ev.StartedAt, now) + ")"},
		{"source", ev.Source},
		{"request", ev.RequestID},
		{"session", displayOrDash(ev.SessionID)},
		{"project", displayOrDash(ev.Project)},
		{"branch", displayOrDash(ev.GitBranch)},
		{"model", displayModel(&ev.EventSummary)},
		{"status", statusCell(&ev.EventSummary)},
		{"stop", displayOrDash(ev.StopCategory)},
		{"billing", ev.BillingMode},
		{"account", displayOrDash(ev.Account)},
		{"service", displayOrDash(ev.ServiceTier)},
		{"speed", displayOrDash(ev.Speed)},
		{"tokens", tokenLine(&ev.EventSummary)},
		{"cost", costCell(ev.CostUSD, ev.ApiEquivalentCostUSD) + costNote(ev.CostSource)},
	}
	if ev.ReplayOf != "" {
		kv = append(kv, [2]string{"replay-of", ev.ReplayOf})
	}
	if ev.ReplayEdits != "" {
		kv = append(kv, [2]string{"replay-edits", ev.ReplayEdits})
	}
	if ev.Source == "proxy" {
		capture := "complete"
		if !ev.CaptureComplete {
			capture = "incomplete (truncated, or the stream ended early)"
		}
		kv = append(kv, [2]string{"capture", capture})
	}
	for _, pair := range kv {
		fmt.Fprintf(w, "  %-13s %s\n", pair[0], pair[1])
	}

	warnings, err := st.EventWarnings(ctx, id)
	if err != nil {
		return fmt.Errorf("show: warnings: %w", err)
	}
	if len(warnings) == 0 {
		fmt.Fprintln(w, "\nno warnings")
	} else {
		fmt.Fprintf(w, "\nwarnings (%d):\n", len(warnings))
		for _, warn := range warnings {
			path := ""
			if warn.Path != "" {
				path = "  at " + warn.Path
			}
			fmt.Fprintf(w, "  %-6s %-16s %s%s\n", warn.Severity, warn.Kind, warn.Detail, path)
		}
	}

	if withBody {
		// The request half stays raw: no compressed request body has been
		// observed, and inventing behaviour for an unobserved case is scope
		// this story does not need.
		printBody(w, "request body", ev.ReqBody)
		respBody, marker := responseBody(ev, cfg.BodyCapBytes)
		printBody(w, "response body", respBody)
		if marker != "" {
			fmt.Fprintln(w, marker)
		}
	} else if len(ev.ReqBody) > 0 || len(ev.RespBody) > 0 {
		fmt.Fprintln(w, "\nbodies stored; pass --body to print them")
	}
	return nil
}

// tokenLine spells out every token class, because the classes are priced
// separately (invariant 4): a reader who only sees in/out cannot check a cost.
func tokenLine(ev *store.EventSummary) string {
	return fmt.Sprintf(
		"prompt=%s (in=%s cache-w5m=%s cache-w1h=%s cache-r=%s) out=%s thinking=%s",
		humanTokens(ev.TotalPromptTokens), humanTokens(ev.InputTokens),
		humanTokens(ev.CacheWrite5mTokens), humanTokens(ev.CacheWrite1hTokens),
		humanTokens(ev.CacheReadTokens), humanTokens(ev.OutputTokens),
		humanTokens(ev.ThinkingTokens),
	)
}

// responseBody returns the response half as the terminal should print it,
// plus a marker line naming why it is not the whole body (empty when it is).
//
// The rule matches the dashboard's, deliberately: decode only when the cap is
// positive, and never rewrite the stored bytes. A stored response is routinely
// brotli -- Claude Code advertises "br" and the proxy tees bytes unmodified --
// so printing the raw form is the observed failure this replaces. Those bytes
// render as mojibake rather than as an error, which is why the marker has to
// name the reason rather than leaving a blank.
func responseBody(ev *store.Event, capBytes int) ([]byte, string) {
	if capBytes <= 0 {
		// Same guard as the read path: decode.Body with a non-positive limit
		// returns the raw body *and* an error, which would print "would not
		// decompress" for a body that decodes fine.
		return ev.RespBody, ""
	}
	hdr := http.Header{}
	if ev.RespHeaders != "" {
		// A malformed blob degrades to an empty header set: no
		// Content-Encoding, so the body prints raw rather than erroring.
		_ = json.Unmarshal([]byte(ev.RespHeaders), &hdr)
	}
	decoded, _, completeness, err := decode.Body(hdr, ev.RespBody, capBytes)
	if err != nil {
		return ev.RespBody, "  (shown raw: it would not decompress)"
	}
	switch completeness {
	case decode.TruncatedAtCap:
		return decoded, fmt.Sprintf("  (truncated at the read cap of %d bytes)", capBytes)
	case decode.PartialCorrupt:
		return decoded, "  (decoded only partially: its tail was corrupt)"
	}
	return decoded, ""
}

// printBody writes one stored body, saying so when it was not stored at all
// rather than printing a bare blank.
func printBody(w io.Writer, label string, body []byte) {
	fmt.Fprintf(w, "\n%s (%d bytes):\n", label, len(body))
	if len(body) == 0 {
		fmt.Fprintln(w, "  (not stored)")
		return
	}
	fmt.Fprintln(w, string(body))
}
