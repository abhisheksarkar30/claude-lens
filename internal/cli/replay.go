package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/replay"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// replayCostThresholdUSD is the original's cost above which a replay needs an
// explicit --yes. A var rather than a const so a test can pin the gate without
// a fixture that genuinely costs a quarter.
var replayCostThresholdUSD = 0.25

// Replay is `clens replay <id>`: re-issue a captured call.
//
// The id is the first positional argument -- `clens replay 42 --set …` -- for
// the same reason `clens show`'s is: telling a flag's value apart from a
// positional in a mixed argv is guesswork once config flags are allowed in the
// same list.
//
// Three modes, and only one of them sends anything:
//
//	clens replay 42                 sends, after the cost gate below
//	clens replay 42 --dump          prints the body it would send; sends nothing
//	clens replay 42 --diff 57       compares two captured calls; sends nothing
//
// The send goes through the dashboard's replay endpoint rather than out to
// Anthropic directly, because that is the path that re-issues through the
// proxy's own Handler -- same transport, same tee, same capture -- and applies
// the server's own guards. The opt-in (`clens serve --replay`), the Origin
// allowlist and the disabled-by-default posture are all the endpoint's; this
// command reports what it answers rather than duplicating the check, since a
// client that cannot see the server's config can only guess at it.
func Replay(args []string) error {
	return runReplay(args, os.Stdout)
}

func runReplay(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("replay: missing request id (usage: clens replay <id> [--set path=value] [--dump] [--diff <id>] [--yes])")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("replay: invalid request id %q", args[0])
	}
	rest := args[1:]

	diffOf, rest := takeFlag(rest, "--diff")
	addr, rest := takeFlag(rest, "--addr")
	dump, rest := hasFlag(rest, "--dump")
	yes, rest := hasFlag(rest, "--yes")
	noCapture, rest := hasFlag(rest, "--no-capture")
	sets, rest := collectFlag(rest, "--set")

	// Parsed before the store is opened and long before anything is sent: a
	// malformed --set is a typo in the command the user just typed, and the
	// cheapest place to catch it is here, with nothing else having happened.
	edits, err := replay.ParseSets(sets)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}

	cfg, st, err := openStore(rest)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	defer st.Close()
	ctx := context.Background()

	if diffOf != "" {
		other, err := strconv.ParseInt(diffOf, 10, 64)
		if err != nil {
			return fmt.Errorf("replay: invalid --diff id %q", diffOf)
		}
		return replayDiff(ctx, w, st, id, other)
	}

	orig, err := getEvent(ctx, st, id, "replay")
	if err != nil {
		return err
	}
	if len(orig.ReqBody) == 0 {
		return fmt.Errorf("replay: request %d has no stored body to replay (capture may be off, or the body was truncated)", id)
	}

	if dump {
		return replayDump(w, orig, edits)
	}

	// The cost gate. Below it, not above: --dump has already returned, so
	// looking at a body is never gated -- only spending is.
	if reason, gated := replayGate(orig); gated && !yes {
		return fmt.Errorf("replay: %s; re-run with --yes to send it anyway, or --dump to see the body without sending", reason)
	}

	// --addr is the test seam and the override; the configured dashboard
	// address is what makes a plain `clens replay 42` work against the
	// `clens serve` the user is already running.
	if addr == "" {
		addr = cfg.DashboardAddr
	}
	res, err := sendReplay(ctx, addr, id, sets, noCapture)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	printReplayResult(w, id, res)
	return nil
}

// replayGate decides whether re-sending ev needs an explicit --yes, and says
// why. This is the CLI's half of the replay footgun: the endpoint's guards stop
// an unauthorized caller, and this stops an authorized one from re-spending by
// reflex.
//
// An unpriced original is gated for the same reason an expensive one is: with
// neither cost figure present there is nothing to bound the replay with, and
// "we cannot tell what this will cost" is not a reason to send it.
func replayGate(ev *store.Event) (string, bool) {
	if ev.CostUSD == nil && ev.ApiEquivalentCostUSD == nil {
		return fmt.Sprintf("request %d is unpriced, so there is nothing to bound the replay's cost with", ev.ID), true
	}
	worst := 0.0
	for _, c := range []*float64{ev.CostUSD, ev.ApiEquivalentCostUSD} {
		if c != nil && *c > worst {
			worst = *c
		}
	}
	if worst > replayCostThresholdUSD {
		return fmt.Sprintf("request %d cost $%.4f (threshold $%.2f)", ev.ID, worst, replayCostThresholdUSD), true
	}
	return "", false
}

// replayDump prints the body this replay would send, and the edit list the
// server would record alongside it. Nothing leaves the process.
func replayDump(w io.Writer, orig *store.Event, edits []replay.Edit) error {
	edited, err := replay.ApplyEdits(orig.ReqBody, edits)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	// MarshalEdits is called after ApplyEdits so each edit carries the Old
	// value the walk recorded -- the dump shows what would be stored on the
	// replay row, not just what was asked for.
	editsJSON, err := replay.MarshalEdits(edits)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}

	fmt.Fprintf(w, "would send %s %s (from request %d)\n", orig.Method, orig.Path, orig.ID)
	fmt.Fprintf(w, "  model   %s\n", displayModel(orig))
	if editsJSON == "" {
		fmt.Fprintln(w, "  edits   none")
	} else {
		fmt.Fprintf(w, "  edits   %s\n", editsJSON)
	}
	fmt.Fprintf(w, "\nbody (%d bytes, was %d):\n%s\n", len(edited), len(orig.ReqBody), edited)
	return nil
}

// replayDiff compares two captured calls field by field and prints every row,
// marking the ones that differ. Nothing is sent: both sides are already in the
// store, which is what makes a diff possible on a machine with no upstream
// reachable at all.
func replayDiff(ctx context.Context, w io.Writer, st *store.Store, baseID, otherID int64) error {
	base, err := replayOutcome(ctx, st, baseID, "replay")
	if err != nil {
		return err
	}
	other, err := replayOutcome(ctx, st, otherID, "replay --diff")
	if err != nil {
		return err
	}

	rows := [][3]string{
		{"status", strconv.Itoa(base.Status), strconv.Itoa(other.Status)},
		{"model", displayOrDash(base.Model), displayOrDash(other.Model)},
		{"input_tokens", strconv.Itoa(base.InputTokens), strconv.Itoa(other.InputTokens)},
		{"output_tokens", strconv.Itoa(base.OutputTokens), strconv.Itoa(other.OutputTokens)},
		{"cost", humanCost(base.CostUSD), humanCost(other.CostUSD)},
		{"cost_source", displayOrDash(base.CostSource), displayOrDash(other.CostSource)},
		{"duration_ms", strconv.FormatFloat(base.DurationMs, 'f', 0, 64), strconv.FormatFloat(other.DurationMs, 'f', 0, 64)},
		{"warnings", strconv.Itoa(len(base.Warnings)), strconv.Itoa(len(other.Warnings))},
	}

	out := make([][]string, 0, len(rows))
	differs := 0
	for _, r := range rows {
		mark := ""
		if r[1] != r[2] {
			mark = "≠"
			differs++
		}
		out = append(out, []string{mark, r[0], r[1], r[2]})
	}
	fmt.Fprintf(w, "%d vs %d\n", baseID, otherID)
	fmt.Fprint(w, table([]string{"", "FIELD", strconv.FormatInt(baseID, 10), strconv.FormatInt(otherID, 10)}, out))
	fmt.Fprintf(w, "\n%d of %d field(s) differ\n", differs, len(rows))

	// The warning lines themselves, when the counts differ: a count says
	// something changed, and only the lines say what.
	if len(base.Warnings) != len(other.Warnings) {
		fmt.Fprintf(w, "\n%d warnings:\n", baseID)
		printReplayWarnings(w, base.Warnings)
		fmt.Fprintf(w, "%d warnings:\n", otherID)
		printReplayWarnings(w, other.Warnings)
	}
	return nil
}

func printReplayWarnings(w io.Writer, warnings []string) {
	if len(warnings) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, line := range warnings {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

// replayOutcome reads one row and its warnings and folds them into the compact
// comparable view -- the same replay.OutcomeOf the endpoint returns for the
// call it just recorded, so both sides of a comparison are built by one
// function rather than two that could drift.
func replayOutcome(ctx context.Context, st *store.Store, id int64, cmd string) (replay.Outcome, error) {
	ev, err := getEvent(ctx, st, id, cmd)
	if err != nil {
		return replay.Outcome{}, err
	}
	warnings, err := st.EventWarnings(ctx, id)
	if err != nil {
		return replay.Outcome{}, fmt.Errorf("%s: warnings: %w", cmd, err)
	}
	return replay.OutcomeOf(ev, warnings), nil
}

// getEvent reads one row, turning the store's ErrNoRows into "not found"
// rather than leaking a database error to the terminal. cmd names the
// subcommand for the message, so a --diff says which of its two ids was
// missing.
func getEvent(ctx context.Context, st *store.Store, id int64, cmd string) (*store.Event, error) {
	ev, err := st.GetEvent(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%s: request %d not found", cmd, id)
		}
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	return ev, nil
}

// sendReplay POSTs to the dashboard's replay endpoint and decodes its answer.
//
// The edits travel as repeated ?set= query parameters in exactly the syntax
// replay.ParseSet accepts, which is what lets the CLI validate them locally
// with the same parser the server will use -- and means a --set that survives
// the local parse cannot be a 400 waiting to happen on the other side.
func sendReplay(ctx context.Context, addr string, id int64, sets []string, noCapture bool) (*replay.Result, error) {
	target := strings.TrimRight(addr, "/") + "/api/requests/" + strconv.FormatInt(id, 10) + "/replay"
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid dashboard address %q", addr)
	}
	q := u.Query()
	for _, s := range sets {
		q.Add("set", s)
	}
	if noCapture {
		q.Set("no_capture", "true")
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: replayHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w (is `clens serve` running?)", u.Redacted(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, replayBodyLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, apiErrorMessage(body))
	}
	var res replay.Result
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decoding response from %s: %w", u.Redacted(), err)
	}
	return &res, nil
}

const (
	// replayHTTPTimeout bounds the whole round trip. A replay is a real API
	// call, and a long one is a stream that has to finish -- so this is
	// generous rather than tight, and is here to stop a hung connection from
	// hanging the terminal forever.
	replayHTTPTimeout = 10 * time.Minute
	// replayBodyLimit caps how much of an error body is read back. The
	// success body is a handful of fields; only a proxy or a misconfigured
	// address could return anything large, and that gets truncated.
	replayBodyLimit = 64 * 1024
)

// apiErrorMessage pulls the message out of the dashboard's error envelope,
// falling back to the raw body when it is not that shape (a reverse proxy's
// HTML error page, say) so the user sees something rather than nothing.
func apiErrorMessage(body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		return env.Error
	}
	if len(body) == 0 {
		return "no response body"
	}
	return strings.TrimSpace(string(body))
}

func printReplayResult(w io.Writer, origID int64, res *replay.Result) {
	if !res.Captured {
		fmt.Fprintf(w, "replayed %d: sent, not captured (status %d)\n", origID, res.Status)
		return
	}
	fmt.Fprintf(w, "replayed %d -> %d (status %d)\n", origID, res.ID, res.Status)
	if res.Outcome == nil {
		return
	}
	o := res.Outcome
	fmt.Fprintf(w, "  model     %s\n", displayOrDash(o.Model))
	fmt.Fprintf(w, "  tokens    in=%s out=%s\n", humanTokens(o.InputTokens), humanTokens(o.OutputTokens))
	fmt.Fprintf(w, "  cost      %s%s\n", humanCost(o.CostUSD), costNote(o.CostSource))
	fmt.Fprintf(w, "  duration  %dms\n", int64(o.DurationMs))
	if len(o.Warnings) > 0 {
		fmt.Fprintf(w, "  warnings  %d\n", len(o.Warnings))
		for _, line := range o.Warnings {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
}
