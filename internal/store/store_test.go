package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenCreatesMissingParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "does", "not", "exist", "lens.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
}

func f64(v float64) *float64 { return &v }
func str(v string) *string   { return &v }

func fullEvent(requestID string) *Event {
	return &Event{EventSummary: EventSummary{
		RequestID:          requestID,
		Source:             "proxy",
		FirstSource:        "proxy",
		StartedAt:          time.Unix(1700000000, 0),
		AuthKind:           "api_key",
		Account:            "acct1",
		BillingMode:        "api",
		ModelRequested:     "claude-sonnet-5",
		ModelResolved:      "claude-sonnet-5",
		InputTokens:        100,
		OutputTokens:       50,
		CacheWrite5mTokens: 10,
		CacheWrite1hTokens: 5,
		CacheReadTokens:    2,
		ThinkingTokens:     3,
		ServiceTier:        "standard",
		Speed:              "fast",
		StopReason:         "end_turn",
		SessionID:          "s_1",
		CostUSD:            f64(0.05),
		CostSource:         "shipped",
		PrefixHash:         str("abc123"),
		CaptureComplete:    true,
		Method:             "POST",
		Path:               "/v1/messages",
		Status:             200}, ReqHeaders: `{"x-api-key":["[redacted]"]}`,
		RespHeaders: `{"content-type":["application/json"]}`,
		ReqBody:     []byte(`{"model":"claude-sonnet-5"}`),
		RespBody:    []byte(`{"usage":{}}`)}
}

// Test 5: round trip, WAL, FK cascade, RedactCheck.
func TestRoundTripAllFields(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := fullEvent("req-1")
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}

	if got.RequestID != ev.RequestID || got.Source != ev.Source || got.FirstSource != ev.FirstSource {
		t.Errorf("identity fields = %+v, want request_id/source/first_source of %+v", got, ev)
	}
	if !got.StartedAt.Equal(ev.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, ev.StartedAt)
	}
	if got.InputTokens != ev.InputTokens || got.OutputTokens != ev.OutputTokens {
		t.Errorf("tokens = %+v, want input=%d output=%d", got, ev.InputTokens, ev.OutputTokens)
	}
	if got.CostUSD == nil || *got.CostUSD != *ev.CostUSD {
		t.Errorf("CostUSD = %v, want %v", got.CostUSD, ev.CostUSD)
	}
	if got.PrefixHash == nil || *got.PrefixHash != *ev.PrefixHash {
		t.Errorf("PrefixHash = %v, want %v", got.PrefixHash, ev.PrefixHash)
	}
	if got.Method != ev.Method || got.Path != ev.Path || got.Status != ev.Status {
		t.Errorf("proxy-only fields = %+v, unexpected", got)
	}
	if string(got.ReqBody) != string(ev.ReqBody) || string(got.RespBody) != string(ev.RespBody) {
		t.Errorf("bodies = %+v, unexpected", got)
	}

	// WAL is on.
	var mode string
	if err := st.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	// FK cascade: attach a warning, delete the event, warning goes too.
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "test_kind"}}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, "DELETE FROM events WHERE id = ?", id); err != nil {
		t.Fatalf("delete event: %v", err)
	}
	var count int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM warnings WHERE event_id = ?", id).Scan(&count); err != nil {
		t.Fatalf("count warnings: %v", err)
	}
	if count != 0 {
		t.Errorf("warnings after cascade delete = %d, want 0", count)
	}

	// RedactCheck flags a planted x-api-key.
	planted, _ := json.Marshal(map[string][]string{"X-Api-Key": {"sk-live-plaintext"}})
	if err := RedactCheck(planted); err == nil {
		t.Error("RedactCheck on unredacted x-api-key = nil error, want an error")
	}
	clean, _ := json.Marshal(map[string][]string{"X-Api-Key": {"[redacted]"}})
	if err := RedactCheck(clean); err != nil {
		t.Errorf("RedactCheck on redacted header = %v, want nil", err)
	}
}

// Test 12a: derived total is the four-class sum on every row, regardless
// of what the caller put in TotalPromptTokens.
func TestDerivedPromptTotal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	fixtures := []*Event{
		fullEvent("req-a"),
		fullEvent("req-b"),
	}
	fixtures[1].InputTokens, fixtures[1].CacheWrite5mTokens, fixtures[1].CacheWrite1hTokens, fixtures[1].CacheReadTokens = 200, 0, 0, 0
	fixtures[1].TotalPromptTokens = 999999 // caller-supplied garbage must be ignored

	for _, ev := range fixtures {
		id, _, err := st.InsertEvent(ctx, ev)
		if err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
		got, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		want := got.InputTokens + got.CacheWrite5mTokens + got.CacheWrite1hTokens + got.CacheReadTokens
		if got.TotalPromptTokens != want {
			t.Errorf("request_id %s: TotalPromptTokens = %d, want %d", ev.RequestID, got.TotalPromptTokens, want)
		}
	}
}

// Test 12b: billing-mode invariants at the schema level.
func TestBillingModeInvariants(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		ev          *Event
		wantCostNil bool
		wantApiNil  bool
	}{
		{
			name: "subscription priced",
			ev: func() *Event {
				e := fullEvent("req-sub-priced")
				e.BillingMode = "subscription"
				e.CostUSD = nil
				e.ApiEquivalentCostUSD = f64(0.10)
				e.CostSource = "shipped"
				return e
			}(),
			wantCostNil: true,
			wantApiNil:  false,
		},
		{
			name: "api priced",
			ev: func() *Event {
				e := fullEvent("req-api-priced")
				e.BillingMode = "api"
				e.CostUSD = f64(0.10)
				e.ApiEquivalentCostUSD = nil
				e.CostSource = "shipped"
				return e
			}(),
			wantCostNil: false,
			wantApiNil:  true,
		},
		{
			name: "api unpriced",
			ev: func() *Event {
				e := fullEvent("req-api-unpriced")
				e.BillingMode = "api"
				e.CostUSD = nil
				e.ApiEquivalentCostUSD = nil
				e.CostSource = "unpriced"
				return e
			}(),
			wantCostNil: true,
			wantApiNil:  true,
		},
		{
			name: "subscription unpriced",
			ev: func() *Event {
				e := fullEvent("req-sub-unpriced")
				e.BillingMode = "subscription"
				e.CostUSD = nil
				e.ApiEquivalentCostUSD = nil
				e.CostSource = "unpriced"
				return e
			}(),
			wantCostNil: true,
			wantApiNil:  true,
		},
	}

	for _, tc := range cases {
		id, _, err := st.InsertEvent(ctx, tc.ev)
		if err != nil {
			t.Fatalf("%s: InsertEvent: %v", tc.name, err)
		}
		got, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("%s: GetEvent: %v", tc.name, err)
		}
		if (got.CostUSD == nil) != tc.wantCostNil {
			t.Errorf("%s: CostUSD = %v, want nil=%v", tc.name, got.CostUSD, tc.wantCostNil)
		}
		if (got.ApiEquivalentCostUSD == nil) != tc.wantApiNil {
			t.Errorf("%s: ApiEquivalentCostUSD = %v, want nil=%v", tc.name, got.ApiEquivalentCostUSD, tc.wantApiNil)
		}
	}

	// The merge path. The loop above exercises inserts only, which is why a
	// merge that moved cost_usd without moving billing_mode could write the
	// forbidden pair (billing_mode='subscription' with a real cost_usd) and
	// fail nothing: the merge, not the insert, is what picks which side's mode
	// and which side's cost survive.
	sub := fullEvent("req-merge-invariant")
	sub.BillingMode = "subscription"
	sub.CostUSD = nil
	sub.ApiEquivalentCostUSD = f64(0.10)
	sub.CostSource = "shipped"
	subID, _, err := st.InsertEvent(ctx, sub)
	if err != nil {
		t.Fatalf("merge path: InsertEvent subscription: %v", err)
	}

	api := fullEvent("req-merge-invariant")
	api.BillingMode = "api"
	api.CostUSD = f64(0.10)
	api.ApiEquivalentCostUSD = nil
	api.CostSource = "shipped"
	apiID, _, err := st.InsertEvent(ctx, api)
	if err != nil {
		t.Fatalf("merge path: InsertEvent api: %v", err)
	}
	if apiID != subID {
		t.Fatalf("merge path: rows did not merge (sub=%d api=%d)", subID, apiID)
	}

	merged, err := st.GetEvent(ctx, subID)
	if err != nil {
		t.Fatalf("merge path: GetEvent: %v", err)
	}
	if merged.BillingMode != "api" {
		t.Errorf("merge path: BillingMode = %q, want api (the mode must follow the winning cost)", merged.BillingMode)
	}
	if merged.CostUSD == nil {
		t.Error("merge path: CostUSD = nil, want the winning capture's cost")
	}
	if merged.ApiEquivalentCostUSD != nil {
		t.Errorf("merge path: ApiEquivalentCostUSD = %v, want nil (invariant 5: never both)", *merged.ApiEquivalentCostUSD)
	}
}

// Session split: a mixed session (one api row, one subscription row) has
// both totals non-NULL; an all-unpriced API session has total_cost_usd
// NULL, never $0.00.
func TestSessionCostSplit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	mixed := "s_mixed"
	api := fullEvent("req-mixed-api")
	api.SessionID = mixed
	api.BillingMode = "api"
	api.CostUSD = f64(1.0)
	api.ApiEquivalentCostUSD = nil
	api.CostSource = "shipped"

	sub := fullEvent("req-mixed-sub")
	sub.SessionID = mixed
	sub.BillingMode = "subscription"
	sub.CostUSD = nil
	sub.ApiEquivalentCostUSD = f64(2.0)
	sub.CostSource = "shipped"

	if err := st.UpsertSession(ctx, mixed, "", api.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	for _, ev := range []*Event{api, sub} {
		if _, _, err := st.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}
	if err := st.ReconcileSession(ctx, mixed); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	gotMixed, err := st.GetSession(ctx, mixed)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotMixed.TotalCostUSD == nil || *gotMixed.TotalCostUSD != 1.0 {
		t.Errorf("mixed session TotalCostUSD = %v, want 1.0", gotMixed.TotalCostUSD)
	}
	if gotMixed.TotalApiEquivalentCostUSD == nil || *gotMixed.TotalApiEquivalentCostUSD != 2.0 {
		t.Errorf("mixed session TotalApiEquivalentCostUSD = %v, want 2.0", gotMixed.TotalApiEquivalentCostUSD)
	}

	allUnpriced := "s_unpriced"
	u1 := fullEvent("req-unpriced-1")
	u1.SessionID = allUnpriced
	u1.BillingMode = "api"
	u1.CostUSD = nil
	u1.ApiEquivalentCostUSD = nil
	u1.CostSource = "unpriced"
	if err := st.UpsertSession(ctx, allUnpriced, "", u1.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if _, _, err := st.InsertEvent(ctx, u1); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.ReconcileSession(ctx, allUnpriced); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	gotUnpriced, err := st.GetSession(ctx, allUnpriced)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotUnpriced.TotalCostUSD != nil {
		t.Errorf("all-unpriced session TotalCostUSD = %v, want nil (never $0.00)", *gotUnpriced.TotalCostUSD)
	}
	if gotUnpriced.UnpricedCount != 1 {
		t.Errorf("all-unpriced session UnpricedCount = %d, want 1", gotUnpriced.UnpricedCount)
	}
}

// Warning upsert: the same (event_id, kind) attached twice leaves one row,
// and warning_count re-derives to COUNT(DISTINCT kind).
func TestWarningUpsertIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := fullEvent("req-warn")
	ev.SessionID = "s_warn"
	if err := st.UpsertSession(ctx, "s_warn", "", ev.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "cache_miss", Detail: "first"}}); err != nil {
		t.Fatalf("UpsertWarnings 1: %v", err)
	}
	if err := st.UpsertWarnings(ctx, id, []Warning{{Kind: "cache_miss", Detail: "second"}}); err != nil {
		t.Fatalf("UpsertWarnings 2: %v", err)
	}

	warnings, err := st.EventWarnings(ctx, id)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1", len(warnings))
	}
	if warnings[0].Detail != "second" {
		t.Errorf("Detail = %q, want %q (the upsert should win)", warnings[0].Detail, "second")
	}

	if err := st.ReconcileSession(ctx, "s_warn"); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}
	sess, err := st.GetSession(ctx, "s_warn")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.WarningCount != 1 {
		t.Errorf("WarningCount = %d, want 1", sess.WarningCount)
	}
}

// Test 20 (store half): a second UPSERT of the same admin natural key
// updates in place rather than duplicating.
// IngestState is the resume-cursor slice jsonlogs (and later collectors)
// persist through -- an upsert on key, not appended history, and a
// never-seen key reports ok=false rather than a zero value that looks
// like a real cursor.
func TestIngestStateUpsert(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := st.GetIngestState(ctx, "jsonl:/tmp/does-not-exist.jsonl"); err != nil || ok {
		t.Fatalf("GetIngestState on an unwritten key: ok=%v err=%v, want ok=false, err=nil", ok, err)
	}

	key := "jsonl:/tmp/a.jsonl"
	if err := st.SetIngestState(ctx, key, IngestState{Value: "100", Status: "ok"}); err != nil {
		t.Fatalf("SetIngestState: %v", err)
	}
	got, ok, err := st.GetIngestState(ctx, key)
	if err != nil || !ok {
		t.Fatalf("GetIngestState: ok=%v err=%v", ok, err)
	}
	if got.Value != "100" || got.Status != "ok" {
		t.Errorf("got %+v, want Value=100 Status=ok", got)
	}

	if err := st.SetIngestState(ctx, key, IngestState{Value: "250", Status: "ok"}); err != nil {
		t.Fatalf("SetIngestState (update): %v", err)
	}
	got, _, err = st.GetIngestState(ctx, key)
	if err != nil {
		t.Fatalf("GetIngestState: %v", err)
	}
	if got.Value != "250" {
		t.Errorf("Value = %q after re-set, want 250 (upsert, not a second row)", got.Value)
	}
}

func TestAdminUpsertIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	day := AdminUsageDay{
		DayStart:     time.Unix(1700000000, 0),
		WindowStart:  time.Unix(1700000000, 0),
		WindowEnd:    time.Unix(1700086400, 0),
		Model:        "claude-sonnet-5",
		WorkspaceID:  "ws1",
		InputTokens:  100,
		OutputTokens: 50,
		FetchedAt:    time.Unix(1700100000, 0),
	}
	if err := st.UpsertAdminUsageDays(ctx, []AdminUsageDay{day}); err != nil {
		t.Fatalf("UpsertAdminUsageDays 1: %v", err)
	}
	day.InputTokens = 999 // refetch with an updated count
	if err := st.UpsertAdminUsageDays(ctx, []AdminUsageDay{day}); err != nil {
		t.Fatalf("UpsertAdminUsageDays 2: %v", err)
	}

	var count, inputTokens int
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*), MAX(input_tokens) FROM admin_usage_days WHERE day_start = ? AND model = ? AND workspace_id = ?",
		day.DayStart.UnixNano(), day.Model, day.WorkspaceID).Scan(&count, &inputTokens); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1 (no duplicate)", count)
	}
	if inputTokens != 999 {
		t.Errorf("input_tokens = %d, want 999 (updated in place)", inputTokens)
	}

	costDay := AdminCostDay{
		DayStart:    time.Unix(1700000000, 0),
		WindowStart: time.Unix(1700000000, 0),
		WindowEnd:   time.Unix(1700086400, 0),
		Model:       "claude-sonnet-5",
		Description: "input tokens",
		AmountUSD:   1.23,
		Currency:    "USD",
		FetchedAt:   time.Unix(1700100000, 0),
	}
	if err := st.UpsertAdminCostDays(ctx, []AdminCostDay{costDay}); err != nil {
		t.Fatalf("UpsertAdminCostDays 1: %v", err)
	}
	costDay.AmountUSD = 4.56
	if err := st.UpsertAdminCostDays(ctx, []AdminCostDay{costDay}); err != nil {
		t.Fatalf("UpsertAdminCostDays 2: %v", err)
	}
	var costCount int
	var amount float64
	if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*), MAX(amount_usd) FROM admin_cost_days WHERE day_start = ? AND model = ? AND description = ? AND currency = ?",
		costDay.DayStart.UnixNano(), costDay.Model, costDay.Description, costDay.Currency).Scan(&costCount, &amount); err != nil {
		t.Fatalf("query: %v", err)
	}
	if costCount != 1 {
		t.Errorf("cost row count = %d, want 1 (no duplicate)", costCount)
	}
	if amount != 4.56 {
		t.Errorf("amount_usd = %v, want 4.56 (updated in place)", amount)
	}
}

func TestListAdminCostDays(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	day1 := AdminCostDay{
		DayStart: time.Unix(1700000000, 0), WindowStart: time.Unix(1700000000, 0), WindowEnd: time.Unix(1700086400, 0),
		Model: "claude-sonnet-5", Description: "input tokens", AmountUSD: 1.23, Currency: "USD", FetchedAt: time.Unix(1700100000, 0),
	}
	day2 := AdminCostDay{
		DayStart: time.Unix(1700086400, 0), WindowStart: time.Unix(1700086400, 0), WindowEnd: time.Unix(1700172800, 0),
		Model: "claude-sonnet-5", Description: "output tokens", AmountUSD: 4.56, Currency: "USD", FetchedAt: time.Unix(1700100000, 0),
	}
	if err := st.UpsertAdminCostDays(ctx, []AdminCostDay{day1, day2}); err != nil {
		t.Fatalf("UpsertAdminCostDays: %v", err)
	}

	all, err := st.ListAdminCostDays(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListAdminCostDays (unfiltered): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows, want 2", len(all))
	}
	if all[0].AmountUSD != 1.23 || all[1].AmountUSD != 4.56 {
		t.Errorf("rows out of day_start order: %+v", all)
	}

	filtered, err := st.ListAdminCostDays(ctx, day2.DayStart, time.Time{})
	if err != nil {
		t.Fatalf("ListAdminCostDays (since day2): %v", err)
	}
	if len(filtered) != 1 || filtered[0].Description != "output tokens" {
		t.Fatalf("filtered = %+v, want only day2's row", filtered)
	}
}

func TestPurge(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	old := fullEvent("req-old")
	old.StartedAt = time.Unix(1000, 0)
	recent := fullEvent("req-recent")
	recent.StartedAt = time.Unix(2000000000, 0)
	unpriced := fullEvent("req-unpriced")
	unpriced.StartedAt = time.Unix(2000000000, 0)
	unpriced.CostSource = "unpriced"
	unpriced.CostUSD = nil

	for _, ev := range []*Event{old, recent, unpriced} {
		if _, _, err := st.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}

	cutoff := time.Unix(1500000000, 0)
	n, err := st.CountPurgeable(ctx, cutoff)
	if err != nil {
		t.Fatalf("CountPurgeable: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountPurgeable = %d, want 1", n)
	}
	deleted, err := st.PurgeOlderThan(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	if deleted != 1 {
		t.Errorf("PurgeOlderThan deleted = %d, want 1", deleted)
	}
	remaining, err := st.CountEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining events = %d, want 2", remaining)
	}

	deletedUnpriced, err := st.PurgeUnpriced(ctx)
	if err != nil {
		t.Fatalf("PurgeUnpriced: %v", err)
	}
	if deletedUnpriced != 1 {
		t.Errorf("PurgeUnpriced deleted = %d, want 1", deletedUnpriced)
	}
	remaining, err = st.CountEvents(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if remaining != 1 {
		t.Errorf("remaining events after both purges = %d, want 1", remaining)
	}

	if err := st.Vacuum(ctx); err != nil {
		t.Errorf("Vacuum: %v", err)
	}
}

// -race clean with concurrent readers during a write batch.
func TestConcurrentReadersDuringWriteBatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ev := fullEvent("req-concurrent-" + strconv.Itoa(i))
			if _, _, err := st.InsertEvent(ctx, ev); err != nil {
				t.Errorf("InsertEvent: %v", err)
				return
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := st.CountEvents(ctx, EventFilter{}); err != nil {
					if err != sql.ErrNoRows {
						t.Errorf("CountEvents: %v", err)
					}
					return
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// --- the two event projections -------------------------------------------

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// parseSelectColumns pulls the column names back out of a SELECT string, so a
// test compares what the query actually asks for rather than the slice it was
// built from.
func parseSelectColumns(t *testing.T, sel string) []string {
	t.Helper()
	rest, ok := strings.CutPrefix(sel, "SELECT ")
	if !ok {
		t.Fatalf("not a SELECT: %q", sel)
	}
	parts := strings.Split(rest, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// TestSummaryColumnsAreTheFullSetMinusBodies is the projection guard: the
// list path must select every column the detail path does, except the header
// and body blobs. Parsing the SELECT strings is what makes it a real check --
// comparing the slices they are built from would be tautological.
func TestSummaryColumnsAreTheFullSetMinusBodies(t *testing.T) {
	full := parseSelectColumns(t, eventSelectColumns)
	summary := parseSelectColumns(t, summarySelectColumns)

	if want := len(full) - len(summaryOmittedColumns); len(summary) != want {
		t.Fatalf("summary selects %d columns, want %d (full %d minus %d omitted)",
			len(summary), want, len(full), len(summaryOmittedColumns))
	}
	for _, c := range summaryOmittedColumns {
		if !containsStr(full, c) {
			t.Errorf("summaryOmittedColumns names %q, which the full projection does not select", c)
		}
	}
	for _, c := range summary {
		if containsStr(summaryOmittedColumns, c) {
			t.Errorf("summary projection selects the omitted column %q", c)
		}
		if !containsStr(full, c) {
			t.Errorf("summary projection selects %q, which the full projection does not", c)
		}
	}
}

// TestSummaryScanMatchesSummaryColumns is the other half of the projection
// guard, and the one that would otherwise fail at runtime rather than at
// compile time: the SELECT and the Scan destination list are written in
// different places, so a column added to one and not the other surfaces as a
// "sql: expected N destination arguments" error on every list fetch.
func TestSummaryScanMatchesSummaryColumns(t *testing.T) {
	var es EventSummary
	var v eventScanVals

	if got, want := len(v.dest(&es)), len(summaryColumnNames); got != want {
		t.Errorf("scanEventSummary takes %d destinations for %d columns", got, want)
	}
	// One extra destination per omitted column, derived from the list rather
	// than written out, so adding a column to summaryOmittedColumns extends
	// this automatically instead of failing on a hardcoded count.
	extras := make([]any, 0, len(summaryOmittedColumns))
	for range summaryOmittedColumns {
		extras = append(extras, new(any))
	}
	if got, want := len(v.dest(&es, extras...)), len(eventColumnNames); got != want {
		t.Errorf("scanEvent takes %d destinations for %d columns", got, want)
	}
}

// TestLatestProxyStartedAt: the observed half of the proxy-mode badge. Its
// contract has two edges worth pinning at the store: an empty store is the
// zero time rather than an error, and a transcript-only store is *not*
// evidence the proxy is running -- which is why the consumer's LastWriteAt,
// which counts every source, is not the source for this.
func TestLatestProxyStartedAt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	at, err := st.LatestProxyStartedAt(ctx)
	if err != nil {
		t.Fatalf("LatestProxyStartedAt on an empty store: %v", err)
	}
	if !at.IsZero() {
		t.Errorf("an empty store returned %v, want the zero time", at)
	}

	sub := fullEvent("req_jsonl")
	sub.Source = "jsonl"
	sub.FirstSource = "jsonl"
	if _, _, err := st.InsertEvent(ctx, sub); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if at, err = st.LatestProxyStartedAt(ctx); err != nil || !at.IsZero() {
		t.Errorf("a transcript-only store returned (%v, %v), want the zero time and no error", at, err)
	}

	newest := time.Now().Truncate(time.Second)
	older := newest.Add(-time.Hour)
	for _, when := range []time.Time{older, newest} {
		ev := fullEvent("req_" + when.Format("150405"))
		ev.StartedAt = when
		if _, _, err := st.InsertEvent(ctx, ev); err != nil {
			t.Fatalf("InsertEvent: %v", err)
		}
	}
	at, err = st.LatestProxyStartedAt(ctx)
	if err != nil {
		t.Fatalf("LatestProxyStartedAt: %v", err)
	}
	if !at.Equal(newest) {
		t.Errorf("LatestProxyStartedAt = %v, want the newest row's %v", at, newest)
	}
}

// --- the migration runner (br-GI-7-06, T9) --------------------------------

// rawDB opens a SQLite file directly, bypassing Open's schema and migration
// path, so a test can build the exact starting state it needs.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	return db
}

func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a column name: %v", err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func hasTable(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	if err != nil {
		t.Fatalf("probe for %s: %v", table, err)
	}
	return n == 1
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	return v
}

var transcriptColumns = []string{"transcript_content", "transcript_role"}

// eventsSchemaWithoutTranscriptColumns returns schemaSQL with the transcript
// columns removed from the events table's definition.
//
// The two columns are stripped out of the schema text rather than dropped from
// a built database with ALTER TABLE ... DROP COLUMN, because SQLite implements
// DROP COLUMN by rewriting the stored CREATE TABLE text in place and it mangles
// the comment block above these columns doing so: the drop of transcript_role
// leaves behind a definition that still looks right but that SQLite can no
// longer parse ("error in table events after drop column: incomplete input").
// Editing the text before the table exists sidesteps that entirely, and reading
// the text out of schemaSQL keeps the fixture from drifting from the real
// schema -- a hand-copied CREATE TABLE would not.
func eventsSchemaWithoutTranscriptColumns(t *testing.T) string {
	t.Helper()
	// Anchored on the comment, not on a column name, so the front of the block
	// comes off in one piece. If schema.sql ever drops the comment this fails
	// loudly rather than silently producing a current-shape fixture.
	const marker = "    -- Transcript-only, and deliberately not req_body"
	start := strings.Index(schemaSQL, marker)
	if start < 0 {
		t.Fatal("schema.sql no longer carries the transcript columns' comment block")
	}
	end := strings.Index(schemaSQL[start:], "\n);")
	if end < 0 {
		t.Fatal("the events table in schema.sql has no closing paren after the transcript columns")
	}
	// resp_body's line keeps its comma up to here; the closing paren may not
	// follow one.
	head := strings.TrimSuffix(strings.TrimRight(schemaSQL[:start], " \t\r\n"), ",")
	return head + "\n);" + schemaSQL[start+end+len("\n);"):]
}

// buildPreChangeDB writes a database in the shape the previous binary left
// behind: the current schema with the transcript columns absent and the version
// reset. Optional drops remove a later table, standing in for a first schema
// exec that died part-file.
func buildPreChangeDB(t *testing.T, path string, dropTables ...string) {
	t.Helper()
	db := rawDB(t, path)
	defer db.Close() // runs even on a Fatalf below, so the temp dir can be removed
	if _, err := db.Exec(eventsSchemaWithoutTranscriptColumns(t)); err != nil {
		t.Fatalf("build a pre-change database: %v", err)
	}
	for _, tbl := range dropTables {
		if _, err := db.Exec("DROP TABLE " + tbl); err != nil {
			t.Fatalf("DROP TABLE %s: %v", tbl, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatalf("reset user_version: %v", err)
	}
}

// TestMigrateFreshDatabase (T9a) is the case a runner that blindly applies
// every migration gets wrong: schema.sql has already created the columns, so
// re-ALTERing them is a "duplicate column name" out of Open on a brand new
// install. This is why the fresh path stamps the version *before* the exec.
func TestMigrateFreshDatabase(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open on a path that does not exist: %v", err)
	}
	defer st.Close()

	for _, col := range transcriptColumns {
		if !hasColumn(t, st.db, "events", col) {
			t.Errorf("a fresh database's events table has no %s column", col)
		}
	}
	if got := userVersion(t, st.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}

// TestMigrateExistingDatabase (T9b) is what CREATE TABLE IF NOT EXISTS cannot
// do: add a column to a database that already exists.
func TestMigrateExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	buildPreChangeDB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-change database: %v", err)
	}
	defer st.Close()

	for _, col := range transcriptColumns {
		if !hasColumn(t, st.db, "events", col) {
			t.Errorf("the migration did not add %s", col)
		}
	}
	if got := userVersion(t, st.db); got != schemaVersion {
		t.Errorf("user_version = %d, want the migration to bump it to %d", got, schemaVersion)
	}
}

// TestMigrateHealsAPartialDatabase (T9c): the schema exec is not atomic, so a
// database can exist with events but without a later table. The exec runs on
// every Open precisely so it repairs that, and the migration must still apply
// exactly once alongside it.
func TestMigrateHealsAPartialDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	buildPreChangeDB(t, path, "warnings")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a partial database: %v", err)
	}
	defer st.Close()

	if !hasTable(t, st.db, "warnings") {
		t.Error("the always-run schema exec did not heal the missing warnings table")
	}
	for _, col := range transcriptColumns {
		if !hasColumn(t, st.db, "events", col) {
			t.Errorf("the migration did not add %s", col)
		}
	}
	if got := userVersion(t, st.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}

// TestMigrateDoesNotReAddColumnsOnAPartialNewSchema (T9d) is the database the
// stamp-before-exec ordering exists to prevent, and the reason the ordering is
// load-bearing rather than cosmetic: events already carries the columns (the
// schema exec got that far) but a later table is missing. With the version
// already current the runner applies nothing, and the exec heals the table.
//
// Seeded the other way -- the version written only after a *successful* exec --
// this is the database Open could never repair: it would read 0, re-run the
// ALTERs against a table that has the columns, and fail with "duplicate column
// name" on every boot thereafter with no recovery but deleting the file.
func TestMigrateDoesNotReAddColumnsOnAPartialNewSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-new.db")

	db := rawDB(t, path)
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("build a current-shape database: %v", err)
	}
	if _, err := db.Exec("DROP TABLE warnings"); err != nil {
		t.Fatalf("DROP TABLE warnings: %v", err)
	}
	// The version the fresh-path stamp would already have written.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		t.Fatalf("seed user_version: %v", err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open must succeed without a duplicate-column error: %v", err)
	}
	defer st.Close()

	if !hasTable(t, st.db, "warnings") {
		t.Error("the schema exec did not heal the missing table")
	}
	for _, col := range transcriptColumns {
		if !hasColumn(t, st.db, "events", col) {
			t.Errorf("events lost its %s column", col)
		}
	}
	if got := userVersion(t, st.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}
