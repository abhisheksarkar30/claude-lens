package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// seedEvent inserts one event with the fields the read commands care about.
// StartedAt is always set explicitly: the zero time stores as a large negative
// nanosecond count and would make every age-based assertion meaningless.
func seedEvent(t *testing.T, st *store.Store, ev *store.Event) int64 {
	t.Helper()
	if ev.StartedAt.IsZero() {
		ev.StartedAt = time.Now()
	}
	if ev.Source == "" {
		ev.Source = "proxy"
	}
	if ev.FirstSource == "" {
		ev.FirstSource = ev.Source
	}
	id, _, err := st.InsertEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("InsertEvent %s: %v", ev.RequestID, err)
	}
	return id
}

// apiRow and subRow are the two billing shapes the split tests need. The
// figures are deliberately far apart so a merged total would be obvious.
func apiRow(requestID string, cost float64) *store.Event {
	return &store.Event{EventSummary: store.EventSummary{
		RequestID:      requestID,
		Source:         "proxy",
		BillingMode:    "api",
		ModelRequested: "claude-sonnet-5",
		ModelResolved:  "claude-sonnet-5",
		Project:        "claude-lens",
		InputTokens:    1000,
		OutputTokens:   500,
		CostUSD:        &cost,
		CostSource:     "shipped",
		Status:         200,
		Method:         "POST",
		Path:           "/v1/messages"}, ReqBody: []byte(`{"model":"claude-sonnet-5","max_tokens":1024}`)}
}

func subRow(requestID string, equivalent float64) *store.Event {
	ev := apiRow(requestID, 0)
	ev.BillingMode = "subscription"
	ev.CostUSD = nil
	ev.ApiEquivalentCostUSD = &equivalent
	return ev
}

// TestEveryCarriedOverCommandRunsAgainstATempStore is the bead's "each of the
// eleven commands runs and exits 0" clause. `serve` is not in the list -- it
// binds two real listeners and blocks on a signal, so it is covered by
// serve_test.go's parts instead.
func TestEveryCarriedOverCommandRunsAgainstATempStore(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, apiRow("req_one", 0.5))

	// runTail only returns when its context is cancelled, and cancelling it up
	// front would fail the seed read rather than end the loop. Cancelling on
	// the first write is the deterministic seam: the seed page has already
	// been read by then, and the loop's next select sees Done and returns.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cases := []struct {
		name string
		run  func(w io.Writer) error
	}{
		{"ls", func(w io.Writer) error { return runLs(nil, w) }},
		{"show", func(w io.Writer) error { return runShow([]string{itoa(id)}, w) }},
		{"tail", func(w io.Writer) error { return runTail(ctx, nil, cancellingWriter{w, cancel}, time.Hour) }},
		{"stats", func(w io.Writer) error { return runStats(nil, w) }},
		{"sessions", func(w io.Writer) error { return runSessions(nil, w) }},
		{"warnings", func(w io.Writer) error { return runWarnings(nil, w) }},
		{"export", func(w io.Writer) error { return runExport(nil, w) }},
		{"prices", func(w io.Writer) error { return runPrices(nil, w) }},
		{"replay", func(w io.Writer) error { return runReplay([]string{itoa(id), "--dump"}, w) }},
		{"purge", func(w io.Writer) error { return runPurge([]string{"--older-than", "1d", "--dry-run"}, w) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.run(&buf); err != nil {
				t.Fatalf("clens %s: %v\noutput:\n%s", tc.name, err, buf.String())
			}
			if buf.Len() == 0 {
				t.Fatalf("clens %s produced no output", tc.name)
			}
		})
	}
}

// TestSessionsNeverMergesOrZeroesACost is the bead's sessions-split clause,
// both halves of it: a mixed session shows two labelled figures, and a
// subscription-only session shows neither a $0.00 nor a merged total.
func TestSessionsNeverMergesOrZeroesACost(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	seedSessionEvent(t, st, "sess_mixed", apiRow("req_api", 1.5))
	seedSessionEvent(t, st, "sess_mixed", subRow("req_sub", 9.5))
	seedSessionEvent(t, st, "sess_sub", subRow("req_sub_only", 4.0))

	var buf bytes.Buffer
	if err := runSessions(nil, &buf); err != nil {
		t.Fatalf("runSessions: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"api $1.5000", "sub $9.5000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mixed session missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$11.0000") {
		t.Fatalf("mixed session printed a merged total:\n%s", out)
	}
	if !strings.Contains(out, "api —") {
		t.Fatalf("subscription-only session should show no api figure at all:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Fatalf("a cost of zero was invented for a session with no api-billed calls:\n%s", out)
	}
}

// TestStatsSplitsByBillingMode is the bead's stats-split clause: two labelled
// totals, never one merged SUM(cost_usd), with the unpriced count alongside.
func TestStatsSplitsByBillingMode(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	seedEvent(t, st, apiRow("req_api", 1.5))
	seedEvent(t, st, subRow("req_sub", 9.5))
	unpriced := apiRow("req_unpriced", 0)
	unpriced.CostUSD = nil
	unpriced.CostSource = "unpriced"
	seedEvent(t, st, unpriced)

	var buf bytes.Buffer
	if err := runStats([]string{"--by", "model"}, &buf); err != nil {
		t.Fatalf("runStats: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "api $1.5000") || !strings.Contains(out, "sub $9.5000") {
		t.Fatalf("stats missing the two labelled totals:\n%s", out)
	}
	if strings.Contains(out, "$11.0000") {
		t.Fatalf("stats printed a merged total:\n%s", out)
	}
	if !strings.Contains(out, "1 unpriced") {
		t.Fatalf("stats did not report the unpriced count alongside the totals:\n%s", out)
	}
}

func TestStatsByProjectGroupsRows(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	seedEvent(t, st, apiRow("req_a", 1.0))
	other := apiRow("req_b", 2.0)
	other.Project = "other-project"
	seedEvent(t, st, other)

	var buf bytes.Buffer
	if err := runStats([]string{"--by", "project"}, &buf); err != nil {
		t.Fatalf("runStats --by project: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"by project:", "claude-lens", "other-project"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--by project missing %q:\n%s", want, out)
		}
	}
}

func TestStatsBySessionUsesTheSessionColumn(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedEvent(t, st, withSession(apiRow("req_a", 1.0), "sess_one"))

	var buf bytes.Buffer
	if err := runStats([]string{"--by", "session"}, &buf); err != nil {
		t.Fatalf("runStats --by session: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "sess_one") {
		t.Fatalf("--by session did not group by session id:\n%s", buf.String())
	}
}

// TestStatsPeriodIsTheWindow checks --period is honoured as a lower bound
// rather than accepted and ignored: a row from last month must not appear in
// a 24h window.
func TestStatsPeriodIsTheWindow(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	old := apiRow("req_old", 1.0)
	old.StartedAt = time.Now().Add(-40 * 24 * time.Hour)
	seedEvent(t, st, old)

	var buf bytes.Buffer
	if err := runStats([]string{"--period", "24h"}, &buf); err != nil {
		t.Fatalf("runStats --period 24h: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "requests: 0") {
		t.Fatalf("a 40-day-old row was counted inside a 24h window:\n%s", out)
	}
}

func TestStatsRejectsAnUnknownGrouping(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runStats([]string{"--by", "shoe-size"}, &buf); err == nil {
		t.Fatal("runStats --by shoe-size: want an error naming the valid values")
	}
}

// TestPurgeDryRunDeletesNothing and its siblings are the bead's purge clause.
// The row is 40 days old so an eight-day cutoff selects it.
func TestPurgeDryRunDeletesNothing(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	old := apiRow("req_old", 1.0)
	old.StartedAt = time.Now().Add(-40 * 24 * time.Hour)
	seedEvent(t, st, old)

	var buf bytes.Buffer
	if err := runPurge([]string{"--older-than", "8d", "--dry-run"}, &buf); err != nil {
		t.Fatalf("runPurge --dry-run: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "would delete 1 row(s)") {
		t.Fatalf("dry run did not report what it would delete:\n%s", buf.String())
	}
	n, err := st.CountEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("event count after --dry-run = %d, want 1 (a dry run deletes nothing)", n)
	}
}

func TestPurgeRefusesToDeleteWithoutYes(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedEvent(t, st, apiRow("req_one", 1.0))

	var buf bytes.Buffer
	err := runPurge([]string{"--older-than", "1d"}, &buf)
	if err == nil {
		t.Fatal("runPurge without --yes: want a refusal")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("refusal does not name --yes: %v", err)
	}
	n, _ := st.CountEvents(context.Background(), store.EventFilter{})
	if n != 1 {
		t.Fatalf("event count after a refused purge = %d, want 1", n)
	}
}

func TestPurgeOlderThanDeletesWithYes(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	old := apiRow("req_old", 1.0)
	old.StartedAt = time.Now().Add(-40 * 24 * time.Hour)
	seedEvent(t, st, old)
	seedEvent(t, st, apiRow("req_new", 1.0))

	var buf bytes.Buffer
	if err := runPurge([]string{"--older-than", "8d", "--yes"}, &buf); err != nil {
		t.Fatalf("runPurge --yes: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "deleted 1 row(s)") {
		t.Fatalf("purge did not report the deletion:\n%s", buf.String())
	}
	n, _ := st.CountEvents(context.Background(), store.EventFilter{})
	if n != 1 {
		t.Fatalf("event count after purge = %d, want the newer row to survive", n)
	}
}

func TestPurgeUnpricedDryRunCountsWithoutDeleting(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	unpriced := apiRow("req_unpriced", 0)
	unpriced.CostUSD = nil
	unpriced.CostSource = "unpriced"
	seedEvent(t, st, unpriced)
	seedEvent(t, st, apiRow("req_priced", 1.0))

	var buf bytes.Buffer
	if err := runPurge([]string{"--unpriced", "--dry-run"}, &buf); err != nil {
		t.Fatalf("runPurge --unpriced --dry-run: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "would delete 1 unpriced row(s)") {
		t.Fatalf("unpriced dry run miscounted:\n%s", buf.String())
	}
	n, _ := st.CountEvents(context.Background(), store.EventFilter{})
	if n != 2 {
		t.Fatalf("event count after --unpriced --dry-run = %d, want 2", n)
	}
}

func TestPurgeRefusesTwoSelectorsAtOnce(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runPurge([]string{"--older-than", "1d", "--unpriced", "--yes"}, &buf); err == nil {
		t.Fatal("runPurge with both selectors: want an error rather than a silent union")
	}
}

// TestExportJSONLinesRoundTripsEveryEvent: the JSON-lines form is a dump of
// the stored row, bodies included, one per line -- so N rows is N decodable
// lines and the body survives.
func TestExportJSONLinesRoundTripsEveryEvent(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedEvent(t, st, apiRow("req_one", 1.0))
	seedEvent(t, st, apiRow("req_two", 2.0))

	var buf bytes.Buffer
	if err := runExport(nil, &buf); err != nil {
		t.Fatalf("runExport: %v\noutput:\n%s", err, buf.String())
	}
	lines := nonEmptyLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("export wrote %d JSON lines, want 2:\n%s", len(lines), buf.String())
	}
	var seen int
	for _, line := range lines {
		var ev store.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("export line is not a stored event: %v\n%s", err, line)
		}
		if len(ev.ReqBody) == 0 {
			t.Fatalf("exported row %s lost its body", ev.RequestID)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("decoded %d rows, want 2", seen)
	}
}

// TestExportCSVKeepsAnUnpricedCellEmpty is the CSV half: a header row, and a
// missing cost rendered as nothing at all rather than 0.
func TestExportCSVKeepsAnUnpricedCellEmpty(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	unpriced := apiRow("req_unpriced", 0)
	unpriced.CostUSD = nil
	unpriced.CostSource = "unpriced"
	seedEvent(t, st, unpriced)
	seedEvent(t, st, apiRow("req_api", 1.25))

	var buf bytes.Buffer
	if err := runExport([]string{"--csv"}, &buf); err != nil {
		t.Fatalf("runExport --csv: %v\noutput:\n%s", err, buf.String())
	}
	lines := nonEmptyLines(buf.String())
	if len(lines) != 3 {
		t.Fatalf("CSV wrote %d lines, want a header plus 2 rows:\n%s", len(lines), buf.String())
	}
	if lines[0] != strings.Join(exportColumns, ",") {
		t.Fatalf("CSV header = %q, want the exportColumns list", lines[0])
	}

	// Rows are looked up by request_id rather than by position: ListEvents is
	// newest-first, so where a seeded row lands is an implementation detail of
	// the store, not something this test should be asserting.
	costIdx := indexOf(exportColumns, "cost_usd")
	idIdx := indexOf(exportColumns, "request_id")
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		switch fields[idIdx] {
		case "req_unpriced":
			if fields[costIdx] != "" {
				t.Fatalf("unpriced row's cost_usd cell = %q, want empty (never 0)", fields[costIdx])
			}
		case "req_api":
			if fields[costIdx] != "1.25" {
				t.Fatalf("priced row's cost_usd cell = %q, want 1.25", fields[costIdx])
			}
		default:
			t.Fatalf("unexpected row in the CSV: %q", line)
		}
	}
}

// TestNoCommandPrintsACredential is the credential-containment clause: with
// both credentials stored, no command's output may contain either value.
func TestNoCommandPrintsACredential(t *testing.T) {
	const (
		sessionValue = "super-secret-session-cookie"
		// Spelled with "placeholder" on purpose: the machine-wide pre-commit
		// scan flags any `sk-` run of 20+ characters as a live key, and the
		// hook's own filler list is what exempts a test fixture. A more
		// distinctive value here would be a secret-scan failure on every
		// commit, not a better test.
		adminValue = "sk-ant-admin01-placeholder"
	)
	home := withHome(t)
	// secret.Save before the store is opened and before anything else writes
	// into ~/.clens: protecting a credential locks the directory's ACL, and
	// NTFS propagates that to existing children (see doctor_test.go's note).
	if err := secret.Save("sessionKey", sessionValue); err != nil {
		t.Fatal(err)
	}
	if err := secret.Save("admin", adminValue); err != nil {
		t.Fatal(err)
	}
	st := openTestStore(t, home)
	id := seedEvent(t, st, apiRow("req_one", 0.5))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runs := map[string]func(w io.Writer) error{
		"ls":       func(w io.Writer) error { return runLs(nil, w) },
		"show":     func(w io.Writer) error { return runShow([]string{itoa(id), "--body"}, w) },
		"tail":     func(w io.Writer) error { return runTail(ctx, nil, cancellingWriter{w, cancel}, time.Hour) },
		"stats":    func(w io.Writer) error { return runStats(nil, w) },
		"sessions": func(w io.Writer) error { return runSessions(nil, w) },
		"warnings": func(w io.Writer) error { return runWarnings([]string{"--detail"}, w) },
		"export":   func(w io.Writer) error { return runExport(nil, w) },
		"prices":   func(w io.Writer) error { return runPrices(nil, w) },
		"accounts": func(w io.Writer) error { return runAccounts(nil, w) },
		"quota":    func(w io.Writer) error { return runQuota(nil, w) },
		"models":   func(w io.Writer) error { return runModels(nil, w) },
		"doctor":   func(w io.Writer) error { return runDoctor(nil, w) },
		"purge":    func(w io.Writer) error { return runPurge([]string{"--older-than", "1d", "--dry-run"}, w) },
		"replay":   func(w io.Writer) error { return runReplay([]string{itoa(id), "--dump"}, w) },
	}
	for name, run := range runs {
		var buf bytes.Buffer
		_ = run(&buf) // an error is fine; the assertion is about what was printed
		out := buf.String()
		if strings.Contains(out, sessionValue) || strings.Contains(out, adminValue) {
			t.Fatalf("clens %s leaked a credential value:\n%s", name, out)
		}
	}
}

// cancellingWriter forwards every write to w and cancels the tail's context on
// the first one, so a test can end a follow loop without an interrupt.
type cancellingWriter struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (c cancellingWriter) Write(p []byte) (int, error) {
	c.cancel()
	return c.w.Write(p)
}

// seedSessionEvent inserts ev under sessionID with the session row already
// present and its totals reconciled -- the state `clens sessions` reads.
// Inserting an event with a SessionID alone leaves the sessions table empty,
// because the row is the jsonlogs collector's to create; a fixture has to do
// that half itself.
func seedSessionEvent(t *testing.T, st *store.Store, sessionID string, ev *store.Event) {
	t.Helper()
	if ev.StartedAt.IsZero() {
		ev.StartedAt = time.Now()
	}
	ctx := context.Background()
	if err := st.UpsertSession(ctx, sessionID, "", ev.StartedAt); err != nil {
		t.Fatalf("UpsertSession %s: %v", sessionID, err)
	}
	ev.SessionID = sessionID
	seedEvent(t, st, ev)
	if err := st.ReconcileSession(ctx, sessionID); err != nil {
		t.Fatalf("ReconcileSession %s: %v", sessionID, err)
	}
}

func withSession(ev *store.Event, sessionID string) *store.Event {
	ev.SessionID = sessionID
	return ev
}

// itoa is strconv.FormatInt under a shorter name, so the id-to-argv
// conversions below don't drown the fixtures in noise.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func indexOf(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	panic("indexOf: " + want + " not in list")
}

// --- Peak / prefix config plumbing (br-GI-3-05) ---

// T10 (resolver half), beside the helper it names. nil means "unset -> use the
// shipped default", and a site that missed that resolution would fail
// silently: the resolved list is only ever fed to strings.HasPrefix, which
// never matches over a nil slice, giving zero routing with no error.
func TestResolvedAPIPrefixes(t *testing.T) {
	withHome(t)

	cfg := config.Default()
	if got := resolvedAPIPrefixes(cfg); len(got) != 1 || got[0] != "deepseek-" {
		t.Errorf("unset ApiModelPrefixes resolved to %#v, want the shipped [deepseek-]", got)
	}

	cfg.ApiModelPrefixes = []string{"acme-"}
	if got := resolvedAPIPrefixes(cfg); len(got) != 1 || got[0] != "acme-" {
		t.Errorf("a non-nil list resolved to %#v, want [acme-] (it replaces the shipped list wholesale)", got)
	}

	cfg.ApiModelPrefixes = []string{}
	if got := resolvedAPIPrefixes(cfg); got == nil || len(got) != 0 {
		t.Errorf("`none` resolved to %#v, want a non-nil empty slice (no routing at all)", got)
	}
}

// The malformed-date case through the exported entry points, since
// runIngest/runRefresh are unexported. The DBPath points into a directory that
// does not exist yet, and store.Open creates a missing parent -- so the
// directory still being absent afterwards is direct evidence that Validate ran
// before the store was opened, not merely that it ran.
func TestIngestAndRefreshValidateBeforeOpeningTheStore(t *testing.T) {
	dir := withHome(t)
	dbDir := filepath.Join(dir, "db")
	clensDir := filepath.Join(dir, ".clens")
	if err := os.MkdirAll(clensDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfgFile := filepath.Join(clensDir, "config.toml")
	body := "PeakOffPeakDates = not-a-date\nDBPath = " + filepath.Join(dbDir, "lens.db") + "\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for name, run := range map[string]func([]string) error{
		"ingest":  Ingest,
		"refresh": Refresh,
	} {
		err := run(nil)
		if err == nil {
			t.Errorf("%s: nil error, want the malformed date rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), "PeakOffPeakDates") {
			t.Errorf("%s: error %q does not name the offending field", name, err)
		}
		if _, serr := os.Stat(dbDir); serr == nil {
			t.Errorf("%s: the store directory was created; Validate must run before the store is opened", name)
		}
	}
}

// T18: the `acct.Name != ""` guard in newTailer, read back through the
// Tailer's Account() accessor. Deleting the guard makes the first case read
// ("", ""), so the regression fails a test rather than depending on a reviewer
// noticing it in a diff.
func TestNewTailerGuardKeepsTheSeededBillingMode(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	// No subscription account configured: the guard skips SetAccount entirely,
	// so the mode New seeded survives.
	cfg := config.Default()
	if name, mode := newTailer(cfg, t.TempDir(), st).Account(); name != "" || mode != "subscription" {
		t.Errorf("Account() = (%q, %q), want (\"\", subscription) when no subscription account is configured", name, mode)
	}

	// An api-mode account is not a subscription account, and must not be
	// adopted as one -- that would mis-attribute every JSONL row.
	cfg.Accounts = []config.Account{{Name: "payg", BillingMode: "api"}}
	if name, mode := newTailer(cfg, t.TempDir(), st).Account(); name != "" || mode != "subscription" {
		t.Errorf("an api-only account was adopted: Account() = (%q, %q), want (\"\", subscription)", name, mode)
	}

	cfg.Accounts = []config.Account{{Name: "work", BillingMode: "subscription", Plan: "max5x"}}
	if name, mode := newTailer(cfg, t.TempDir(), st).Account(); name != "work" || mode != "subscription" {
		t.Errorf("Account() = (%q, %q), want (work, subscription)", name, mode)
	}
}

// TestNewTailerWiresTheBodyPolicy is br-GI-7-09's wiring half, read back through
// the Tailer's BodyPolicy() accessor.
//
// The seam is the whole point: internal/jsonlogs deliberately does not import
// internal/config, so the policy can only arrive from this one helper. Deleting
// the wiring line leaves the tailer on its seeded "full" default, which is
// exactly the state the bead was written about -- an install running
// --body-policy off still storing transcript content whole. Asserting the
// tailer's *own* behaviour would pass either way; only reading back what
// newTailer wired can fail.
func TestNewTailerWiresTheBodyPolicy(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	cfg := config.Default()
	cfg.BodyPolicy = "off"
	cfg.BodyCapBytes = 4096

	policy, capBytes := newTailer(cfg, t.TempDir(), st).BodyPolicy()
	if policy != "off" || capBytes != 4096 {
		t.Errorf("BodyPolicy() = (%q, %d), want (off, 4096) -- newTailer did not wire the configured policy",
			policy, capBytes)
	}

	// And it is the configured value, not a constant: a second, different
	// config must come back different, or the assertion above would pass on a
	// hard-coded "off".
	cfg.BodyPolicy = "full"
	cfg.BodyCapBytes = 1024
	policy, capBytes = newTailer(cfg, t.TempDir(), st).BodyPolicy()
	if policy != "full" || capBytes != 1024 {
		t.Errorf("BodyPolicy() = (%q, %d), want (full, 1024)", policy, capBytes)
	}
}

// T14: newTailer consumes resolvedAPIPrefixes rather than a hand-rolled list,
// in both directions. Read through the ModelBilling() accessor beside
// Account(). Dropping the resolver from newTailer -- so that the shipped
// default silently stops being applied -- fails this test rather than being a
// diff a reviewer has to spot.
func TestNewTailerWiresResolvedAPIPrefixes(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	// An unconfigured install: the shipped default, not an empty list.
	cfg := config.Default()
	prefixes, account, mode := newTailer(cfg, t.TempDir(), st).ModelBilling()
	if len(prefixes) != 1 || prefixes[0] != "deepseek-" {
		t.Errorf("ModelBilling prefixes = %v, want the shipped [deepseek-] on an unconfigured install", prefixes)
	}
	// billing_mode is the literal "api" even with no api account configured:
	// it is the column invariant 5 keys off, so it is never the empty string.
	if mode != "api" {
		t.Errorf("ModelBilling billing mode = %q, want the literal api", mode)
	}
	if account != "" {
		t.Errorf("ModelBilling account = %q, want empty (no api account is configured)", account)
	}

	// A configured list replaces the shipped one, and the api account is
	// picked out by billing mode rather than by position.
	cfg.ApiModelPrefixes = []string{"acme-"}
	cfg.Accounts = []config.Account{
		{Name: "work", BillingMode: "subscription", Plan: "max5x"},
		{Name: "payg", BillingMode: "api"},
	}
	prefixes, account, mode = newTailer(cfg, t.TempDir(), st).ModelBilling()
	if len(prefixes) != 1 || prefixes[0] != "acme-" {
		t.Errorf("ModelBilling prefixes = %v, want [acme-] (the resolver's output, replacing the shipped list)", prefixes)
	}
	if account != "payg" || mode != "api" {
		t.Errorf("ModelBilling = (%q, %q), want (payg, api)", account, mode)
	}
}

// TestLsJSONKeepsBodyFields (T3, br-GI-7-01): `ls --json` is a
// machine-readable contract that encodes each whole row, so its read must name
// ListEventsFull explicitly. On the summary projection the four header/body
// keys would vanish from the output with no error and no test failure -- the
// exact class of silent contract change this bead exists to prevent.
//
// It decodes into store.Event rather than into a map, which also pins that
// moving the scalar block into an embedded EventSummary left the wire keys
// alone.
func TestLsJSONKeepsBodyFields(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ev := apiRow("req_bodies", 1.0)
	ev.ReqHeaders = `{"authorization":["[redacted]"]}`
	ev.RespHeaders = `{"content-type":["application/json"]}`
	ev.ReqBody = []byte(`{"model":"claude-sonnet-5"}`)
	ev.RespBody = []byte(`{"type":"message"}`)
	seedEvent(t, st, ev)

	var buf bytes.Buffer
	if err := runLs([]string{"--json"}, &buf); err != nil {
		t.Fatalf("runLs --json: %v", err)
	}
	lines := nonEmptyLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("ls --json wrote %d lines, want 1:\n%s", len(lines), buf.String())
	}
	var got store.Event
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("ls --json line is not a stored event: %v\n%s", err, lines[0])
	}

	for _, c := range []struct {
		name      string
		got, want string
	}{
		{"ReqHeaders", got.ReqHeaders, ev.ReqHeaders},
		{"RespHeaders", got.RespHeaders, ev.RespHeaders},
		{"ReqBody", string(got.ReqBody), string(ev.ReqBody)},
		{"RespBody", string(got.RespBody), string(ev.RespBody)},
	} {
		if c.got != c.want {
			t.Errorf("ls --json %s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// gzipBody encodes data as a gzip stream: a stored response body routinely is
// one, since the proxy tees the bytes unmodified and Claude Code advertises
// gzip among its codings.
func gzipBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestShowBodyDecodesResponse (T5, br-GI-7-03): the terminal shows the same
// decoded response the dashboard does, and names the reason when it cannot
// show all of it. Before this, `clens show --body` printed a stored compressed
// body as mojibake -- the observed failure this story opens with -- because
// printBody writes the stored bytes verbatim.
func TestShowBodyDecodesResponse(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	plain := []byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`)

	decodable := apiRow("req_br", 1.0)
	decodable.RespHeaders = `{"Content-Encoding":["gzip"]}`
	decodable.RespBody = gzipBody(t, plain)
	okID := seedEvent(t, st, decodable)

	var buf bytes.Buffer
	if err := runShow([]string{itoa(okID), "--body"}, &buf); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	if !strings.Contains(buf.String(), string(plain)) {
		t.Errorf("show --body did not print the decoded response:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "would not decompress") {
		t.Error("show --body marked a decodable body as undecodable")
	}

	// A capped body prints its marker rather than presenting a prefix as the
	// whole response.
	big := apiRow("req_big", 1.0)
	big.RespHeaders = `{"Content-Encoding":["gzip"]}`
	big.RespBody = gzipBody(t, bytes.Repeat([]byte("abcdefgh"), 512))
	bigID := seedEvent(t, st, big)

	buf.Reset()
	if err := runShow([]string{itoa(bigID), "--body", "--body-cap-bytes=1024"}, &buf); err != nil {
		t.Fatalf("runShow: %v", err)
	}
	if !strings.Contains(buf.String(), "truncated at the read cap of 1024 bytes") {
		t.Errorf("show --body did not print the truncation marker:\n%s", buf.String())
	}
}
