package jsonlogs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func eventsByRequestID(t *testing.T, st *store.Store) map[string]*store.EventSummary {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	out := map[string]*store.EventSummary{}
	for _, ev := range evs {
		out[ev.RequestID] = ev
	}
	return out
}

// subagentParent is a pure function -- test it directly against the path
// shapes test 22 describes.
func TestSubagentParent(t *testing.T) {
	cases := []struct {
		path       string
		wantParent string
		wantSide   bool
	}{
		{filepath.Join("proj1", "sess-parent.jsonl"), "", false},
		{filepath.Join("proj1", "sess-parent", "subagents", "agent-1.jsonl"), "sess-parent", true},
	}
	for _, c := range cases {
		parent, side := subagentParent(c.path)
		if parent != c.wantParent || side != c.wantSide {
			t.Errorf("subagentParent(%q) = (%q, %v), want (%q, %v)", c.path, parent, side, c.wantParent, c.wantSide)
		}
	}
}

// Test 8, end to end: the duplicate-requestId fixture ingests as one row
// with the distinct request's tokens, never the sum of both lines.
func TestPollDuplicateRequestIDFixture(t *testing.T) {
	root := t.TempDir()
	copyFile(t, filepath.Join("testdata", "duplicate_requestid.jsonl"), filepath.Join(root, "log.jsonl"))

	st := newTestStore(t)
	tailer := New(root, st)

	stats, err := tailer.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("RequestsFound = %d, want 1", stats.RequestsFound)
	}
	if stats.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1", stats.Inserted)
	}

	evs := eventsByRequestID(t, st)
	ev, ok := evs["req_dup1"]
	if !ok {
		t.Fatalf("no event for req_dup1: got %v", evs)
	}
	if ev.InputTokens != 100 || ev.OutputTokens != 50 {
		t.Errorf("tokens = %d/%d, want 100/50 (the distinct request's, not the summed 200/100)", ev.InputTokens, ev.OutputTokens)
	}
}

// Test 9, end to end: the 13 tolerated types are skipped, one unknown
// type and one malformed line are counted (not fatal), and both cache
// shapes ingest with the same prompt total, the flat one costed as
// approximate (cost_source aside, the token total is what must agree).
func TestPollToleranceFixture(t *testing.T) {
	root := t.TempDir()
	copyFile(t, filepath.Join("testdata", "tolerance.jsonl"), filepath.Join(root, "log.jsonl"))

	st := newTestStore(t)
	tailer := New(root, st)

	stats, err := tailer.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stats.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", stats.Unknown)
	}
	if stats.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", stats.Malformed)
	}
	if stats.RequestsFound != 2 {
		t.Errorf("RequestsFound = %d, want 2 (req_split, req_flat)", stats.RequestsFound)
	}
	if stats.Inserted != 2 {
		t.Errorf("Inserted = %d, want 2", stats.Inserted)
	}

	evs := eventsByRequestID(t, st)
	split, ok := evs["req_split"]
	if !ok {
		t.Fatal("no event for req_split")
	}
	flat, ok := evs["req_flat"]
	if !ok {
		t.Fatal("no event for req_flat")
	}
	splitTotal := split.InputTokens + split.CacheWrite5mTokens + split.CacheWrite1hTokens + split.CacheReadTokens
	flatTotal := flat.InputTokens + flat.CacheWrite5mTokens + flat.CacheWrite1hTokens + flat.CacheReadTokens
	if splitTotal != flatTotal {
		t.Errorf("prompt totals differ: split=%d flat=%d, want equal", splitTotal, flatTotal)
	}
}

// Test 22: a top-level transcript is is_sidechain=false; a
// …/<sessionId>/subagents/agent-*.jsonl file is is_sidechain=true and
// attributed to the parent session named in its path, not the (different)
// sessionId its own line carries.
func TestPollSubagentDiscovery(t *testing.T) {
	root := t.TempDir()
	src := os.DirFS(filepath.Join("testdata", "subagents"))
	if err := os.CopyFS(root, src); err != nil {
		t.Fatalf("CopyFS: %v", err)
	}

	st := newTestStore(t)
	tailer := New(root, st)

	stats, err := tailer.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stats.FilesWalked != 2 {
		t.Errorf("FilesWalked = %d, want 2", stats.FilesWalked)
	}
	if stats.Inserted != 2 {
		t.Errorf("Inserted = %d, want 2", stats.Inserted)
	}

	evs := eventsByRequestID(t, st)
	top, ok := evs["req_top"]
	if !ok {
		t.Fatal("no event for req_top")
	}
	if top.IsSidechain {
		t.Error("top-level transcript: IsSidechain = true, want false")
	}
	if top.SessionID != "sess-parent" {
		t.Errorf("top-level SessionID = %q, want sess-parent", top.SessionID)
	}

	sub, ok := evs["req_sub"]
	if !ok {
		t.Fatal("no event for req_sub")
	}
	if !sub.IsSidechain {
		t.Error("subagent file: IsSidechain = false, want true")
	}
	if sub.SessionID != "sess-parent" {
		t.Errorf("subagent SessionID = %q, want sess-parent (the path's owning session, not the line's own sessionId)", sub.SessionID)
	}
}

// Test 10: append then resume never duplicates.
func TestPollResumeAfterAppend(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	writeLine(t, path, `{"type":"assistant","sessionId":"s1","requestId":"req_a","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`)

	st := newTestStore(t)
	tailer := New(root, st)
	ctx := context.Background()

	if _, err := tailer.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	appendLine(t, path, `{"type":"assistant","sessionId":"s1","requestId":"req_b","message":{"model":"m","usage":{"input_tokens":2,"output_tokens":2}}}`)
	stats, err := tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("second Poll RequestsFound = %d, want 1 (only the appended line)", stats.RequestsFound)
	}

	n, err := st.CountEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 2 {
		t.Errorf("event count = %d, want 2 (req_a once, req_b once, no dupes)", n)
	}
}

// Test 10: truncation (or rotation to a smaller file at the same path)
// resets the cursor to 0 rather than erroring, and a re-read of an
// already-seen request_id merges instead of duplicating.
func TestPollResumeAfterTruncate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	line := `{"type":"assistant","sessionId":"s1","requestId":"req_x","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`
	writeLine(t, path, line)

	st := newTestStore(t)
	tailer := New(root, st)
	ctx := context.Background()

	if _, err := tailer.Poll(ctx); err != nil {
		t.Fatalf("Poll 1: %v", err)
	}

	// Truncate to empty and poll while it's still empty -- this is the
	// moment the cursor must detect "file shrank" and reset to 0, which a
	// same-size rewrite afterward would hide (size == offset looks like
	// "already fully read", not "rotated").
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if _, err := tailer.Poll(ctx); err != nil {
		t.Fatalf("Poll (post-truncate, still empty): %v", err)
	}

	// Rewrite the same request -- simulates a rotated file reappearing at
	// the same path with content the tailer has already seen once.
	writeLine(t, path, line)

	stats, err := tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 2 (post-truncate): %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("post-truncate RequestsFound = %d, want 1", stats.RequestsFound)
	}

	n, err := st.CountEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("event count after truncate-and-reread = %d, want 1 (merged, not duplicated)", n)
	}
}

// A line with no trailing newline yet (Claude Code mid-write) is left
// unparsed until the newline arrives -- the boundary-split guarantee.
func TestPollIncompleteLineWaitsForNewline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	full := `{"type":"assistant","sessionId":"s1","requestId":"req_partial","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`
	partial := full[:len(full)/2]

	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	st := newTestStore(t)
	tailer := New(root, st)
	ctx := context.Background()

	stats, err := tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 1: %v", err)
	}
	if stats.RequestsFound != 0 || stats.Malformed != 0 {
		t.Errorf("Poll on a partial line: RequestsFound=%d Malformed=%d, want 0/0 (must wait for the newline)", stats.RequestsFound, stats.Malformed)
	}

	if err := os.WriteFile(path, []byte(full+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile complete: %v", err)
	}
	stats, err = tailer.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll 2: %v", err)
	}
	if stats.RequestsFound != 1 {
		t.Errorf("Poll after completing the line: RequestsFound = %d, want 1", stats.RequestsFound)
	}
}

// UTF-8 content in a JSONL field round-trips correctly.
func TestPollUTF8Field(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	writeLine(t, path, `{"type":"assistant","sessionId":"s1","requestId":"req_utf8","cwd":"/home/dev/café-日本語","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`)

	st := newTestStore(t)
	tailer := New(root, st)
	if _, err := tailer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	evs := eventsByRequestID(t, st)
	ev, ok := evs["req_utf8"]
	if !ok {
		t.Fatal("no event for req_utf8")
	}
	if ev.Project != "/home/dev/café-日本語" {
		t.Errorf("Project = %q, want the UTF-8 cwd round-tripped exactly", ev.Project)
	}
}

func writeLine(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func appendLine(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile append %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content + "\n"); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// --- Tailer account contract (br-GI-3-06) ---

// New seeds billing_mode "subscription" with no account name: Claude Code's
// own transcripts are written by the subscription client in the common case.
func TestNewSeedsSubscriptionBillingMode(t *testing.T) {
	st := newTestStore(t)
	name, mode := New(t.TempDir(), st).Account()
	if name != "" {
		t.Errorf("account name = %q, want empty (a JSONL line carries no account)", name)
	}
	if mode != "subscription" {
		t.Errorf("billing mode = %q, want subscription (the seed)", mode)
	}
}

// SetAccount assigns unconditionally, including a zero Account -- which blanks
// the mode New seeded. This is the sharp edge the `acct.Name != ""` guard in
// cli.newTailer exists for, and the reason that guard is load-bearing rather
// than decorative: without it, an install with no subscription account would
// blank the mode and mis-bill every row into cost_usd.
func TestSetAccountAssignsUnconditionally(t *testing.T) {
	st := newTestStore(t)
	tailer := New(t.TempDir(), st)

	tailer.SetAccount("", "")
	if _, mode := tailer.Account(); mode != "" {
		t.Errorf("billing mode = %q, want \"\" -- SetAccount must not treat a zero Account as a no-op", mode)
	}

	tailer.SetAccount("work", "api")
	name, mode := tailer.Account()
	if name != "work" || mode != "api" {
		t.Errorf("Account() = (%q, %q), want (work, api)", name, mode)
	}
}

// --- Per-row billing routing (br-GI-3-07) ---

// writeLines writes a JSONL transcript from the given lines.
func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// assistantLine is one assistant-with-usage transcript line, the only shape
// dedup and token extraction consider. With no timestamp, parseTimestamp
// falls back to now.
func assistantLine(requestID, sessionID, model string, inputTokens int) string {
	return assistantLineUsage(requestID, sessionID, model, inputTokens, "")
}

// assistantLineAt is assistantLine pinned to an explicit timestamp, so a test
// can place a row inside or outside a peak window rather than depending on
// when it happens to run.
func assistantLineAt(requestID, sessionID, model string, at time.Time) string {
	return assistantLineUsage(requestID, sessionID, model, 1000, at.UTC().Format(time.RFC3339))
}

func assistantLineUsage(requestID, sessionID, model string, inputTokens int, ts string) string {
	stamp := ""
	if ts != "" {
		stamp = fmt.Sprintf(`"timestamp":%q,`, ts)
	}
	return fmt.Sprintf(
		`{"type":"assistant",%s"sessionId":%q,"uuid":"u-%s","requestId":%q,"message":{"model":%q,"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}}}}`,
		stamp, sessionID, requestID, requestID, model, inputTokens)
}

// T7: one tailer, two models. A prefix-matched row bills to the api account
// with a real cost in cost_usd; a Claude row from the same tailer still bills
// to the subscription account with its cost in the hypothetical column. That
// the two resolve differently from one tailer is the entire point -- resolving
// once per tailer is what the api* seam replaced.
func TestPollRoutesAPIPrefixedModelsPerRow(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "log.jsonl"),
		assistantLine("req_ds", "sess_route", "deepseek-flash", 1_000_000),
		assistantLine("req_cl", "sess_route", "claude-sonnet-5", 1_000_000),
	)

	st := newTestStore(t)
	tailer := New(root, st)
	tailer.SetPriceTable(pricing.ShippedTable())
	// The exact value resolvedAPIPrefixes(cfg) returns on an unconfigured
	// install, so this is the shipped default's behaviour, not a hand-rolled
	// list.
	tailer.SetModelBilling(pricing.ShippedAPIModelPrefixes(), "", "api")

	if _, err := tailer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	evs := eventsByRequestID(t, st)
	ds, ok := evs["req_ds"]
	if !ok {
		t.Fatalf("no event for req_ds: %v", evs)
	}
	if ds.BillingMode != "api" {
		t.Errorf("deepseek row BillingMode = %q, want api", ds.BillingMode)
	}
	if ds.CostUSD == nil {
		t.Error("deepseek row CostUSD = nil, want a real cost in cost_usd")
	}
	if ds.ApiEquivalentCostUSD != nil {
		t.Errorf("deepseek row ApiEquivalentCostUSD = %v, want nil (invariant 5: never both)", *ds.ApiEquivalentCostUSD)
	}

	cl, ok := evs["req_cl"]
	if !ok {
		t.Fatalf("no event for req_cl: %v", evs)
	}
	if cl.BillingMode != "subscription" {
		t.Errorf("claude row BillingMode = %q, want subscription", cl.BillingMode)
	}
	if cl.ApiEquivalentCostUSD == nil {
		t.Error("claude row ApiEquivalentCostUSD = nil, want the hypothetical cost")
	}
	if cl.CostUSD != nil {
		t.Errorf("claude row CostUSD = %v, want nil", *cl.CostUSD)
	}
}

// The nil-list case. SetModelBilling(nil, ...) routes nothing, which is
// exactly how a call site that forgot resolvedAPIPrefixes would fail --
// silently, because strings.HasPrefix over a nil slice simply never matches
// and nothing returns an error.
func TestSetModelBillingNilRoutesNothing(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "log.jsonl"),
		assistantLine("req_nil", "sess_nil", "deepseek-flash", 1000))

	st := newTestStore(t)
	tailer := New(root, st)
	tailer.SetPriceTable(pricing.ShippedTable())
	tailer.SetModelBilling(nil, "payg", "api")

	if _, err := tailer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	ev := eventsByRequestID(t, st)["req_nil"]
	if ev == nil {
		t.Fatal("no event for req_nil")
	}
	if ev.BillingMode != "subscription" {
		t.Errorf("BillingMode = %q, want subscription (a nil prefix list routes nothing)", ev.BillingMode)
	}
}

// ModelBilling returns a copy, so a caller cannot mutate the tailer's own
// slice -- the same rule AllKinds() follows.
func TestModelBillingReturnsACopy(t *testing.T) {
	tailer := New(t.TempDir(), newTestStore(t))
	tailer.SetModelBilling([]string{"deepseek-"}, "payg", "api")

	prefixes, account, mode := tailer.ModelBilling()
	if len(prefixes) != 1 || prefixes[0] != "deepseek-" || account != "payg" || mode != "api" {
		t.Fatalf("ModelBilling() = (%v, %q, %q), want ([deepseek-], payg, api)", prefixes, account, mode)
	}
	prefixes[0] = "mutated"
	if again, _, _ := tailer.ModelBilling(); again[0] != "deepseek-" {
		t.Errorf("mutating the returned slice changed the tailer's own: next call returned %q", again[0])
	}
}

// --- peak_pricing warning (br-GI-3-10) ---

// warningKinds reads back one row's finding kinds.
func warningKinds(t *testing.T, st *store.Store, id int64) map[string]bool {
	t.Helper()
	warnings, err := st.EventWarnings(context.Background(), id)
	if err != nil {
		t.Fatalf("EventWarnings: %v", err)
	}
	out := map[string]bool{}
	for _, w := range warnings {
		out[w.Kind] = true
	}
	return out
}

// T11 (insert path): peak_pricing fires on a peak-billed *priced* row, not on
// the same row off peak, and not on an unpriced row at a peak instant -- an
// unpriced row was never billed at any rate, so claiming it was billed at peak
// would be a lie.
func TestInsertAttachesPeakPricingWarning(t *testing.T) {
	// 2026-09-21 is a Monday; the shipped window is [01:00,04:00) UTC.
	peakAt := time.Date(2026, time.September, 21, 2, 0, 0, 0, time.UTC)
	offAt := time.Date(2026, time.September, 21, 0, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		model string
		at    time.Time
		want  bool
	}{
		{"priced at peak", "deepseek-flash", peakAt, true},
		{"priced off peak", "deepseek-flash", offAt, false},
		{"unpriced at peak", "claude-nonesuch-9", peakAt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeLines(t, filepath.Join(root, "log.jsonl"),
				assistantLineAt("req_peak", "sess_peak", tc.model, tc.at))

			st := newTestStore(t)
			tailer := New(root, st)
			tailer.SetPriceTable(pricing.ShippedTable())

			if _, err := tailer.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			ev := eventsByRequestID(t, st)["req_peak"]
			if ev == nil {
				t.Fatal("no event for req_peak")
			}
			if got := warningKinds(t, st, ev.ID)["peak_pricing"]; got != tc.want {
				t.Errorf("peak_pricing present = %v, want %v (cost_source=%q, model=%q, at=%s)",
					got, tc.want, ev.CostSource, ev.ModelResolved, tc.at.Format(time.RFC3339))
			}
		})
	}
}

// A pricer that does not implement PeakComputer yields no warning rather than
// a panic or a spurious one. This is the property that keeps the interface
// optional, and it is what every existing fake relies on.
func TestPeakWarningSkippedForAPricerWithoutPeakAt(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "log.jsonl"),
		assistantLineAt("req_nopeak", "sess_nopeak", "deepseek-flash",
			time.Date(2026, time.September, 21, 2, 0, 0, 0, time.UTC)))

	st := newTestStore(t)
	tailer := New(root, st)
	tailer.SetPriceTable(plainPricer{})

	if _, err := tailer.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	ev := eventsByRequestID(t, st)["req_nopeak"]
	if ev == nil {
		t.Fatal("no event for req_nopeak")
	}
	if warningKinds(t, st, ev.ID)["peak_pricing"] {
		t.Error("peak_pricing attached for a pricer with no PeakAt method")
	}
}

// plainPricer implements PriceComputer and nothing more, which is the shape
// every existing fake has.
type plainPricer struct{}

func (plainPricer) Compute(string, parse.Usage, string, string, time.Time) (*float64, string) {
	usd := 1.0
	return &usd, "shipped"
}

// T8 (the jsonlogs half): the transcript's own content is the one thing a
// transcript holds that the proxy cannot observe, so it is stored -- in its own
// columns, never in req_body. req_body belongs to the wire capture, and the
// cross-source merge already has precedence rules for that column, so writing a
// reconstruction into it would make the two indistinguishable.
func TestPollStoresTranscriptContentInItsOwnColumns(t *testing.T) {
	const content = `[{"type":"text","text":"the answer"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]`

	root := t.TempDir()
	path := filepath.Join(root, "log.jsonl")
	writeLine(t, path,
		`{"type":"assistant","sessionId":"s1","requestId":"req_content","message":{"role":"assistant","model":"m","content":`+
			content+`,"usage":{"input_tokens":1,"output_tokens":1}}}`)
	// A second assistant line with no content field at all: Message is present,
	// so the row still exists, but the columns must stay empty rather than
	// holding a faked empty array. internal/api renders that as "no
	// reconstruction", which is a different statement from "reconstructed, and
	// it was empty".
	appendLine(t, path,
		`{"type":"assistant","sessionId":"s1","requestId":"req_nocontent","message":{"role":"assistant","model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`)

	st := newTestStore(t)
	ctx := context.Background()
	if _, err := New(root, st).Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	rows, err := st.ListEventsFull(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEventsFull: %v", err)
	}
	byRequest := map[string]*store.Event{}
	for _, r := range rows {
		byRequest[r.RequestID] = r
	}

	got, ok := byRequest["req_content"]
	if !ok {
		t.Fatalf("no row for req_content; got %d rows", len(rows))
	}
	if string(got.TranscriptContent) != content {
		t.Errorf("TranscriptContent = %q, want %q", got.TranscriptContent, content)
	}
	if got.TranscriptRole != "assistant" {
		t.Errorf("TranscriptRole = %q, want assistant", got.TranscriptRole)
	}
	if len(got.ReqBody) != 0 {
		t.Errorf("ReqBody = %q, want empty -- a reconstruction is not a capture", got.ReqBody)
	}
	if len(got.RespBody) != 0 {
		t.Errorf("RespBody = %q, want empty", got.RespBody)
	}

	empty, ok := byRequest["req_nocontent"]
	if !ok {
		t.Fatalf("no row for req_nocontent; got %d rows", len(rows))
	}
	if len(empty.TranscriptContent) != 0 {
		t.Errorf("TranscriptContent = %q for a line with no content, want empty", empty.TranscriptContent)
	}
	if empty.TranscriptRole != "assistant" {
		t.Errorf("TranscriptRole = %q, want assistant -- the role is the transcript's own", empty.TranscriptRole)
	}
}
