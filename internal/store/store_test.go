package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// explainQueryPlanDetail runs EXPLAIN QUERY PLAN over query and concatenates
// every row's columns into one string a test can substring-match against.
func explainQueryPlanDetail(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN columns: %v", err)
	}
	var out strings.Builder
	for rows.Next() {
		dest := make([]any, len(cols))
		for i := range dest {
			dest[i] = new(sql.RawBytes)
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN scan: %v", err)
		}
		for _, d := range dest {
			out.Write(*d.(*sql.RawBytes))
			out.WriteByte(' ')
		}
		out.WriteByte('\n')
	}
	return out.String()
}

// TestSessionEventsForRulesUsesTheIndexWithoutASort (§6 test 1) is the
// runnable check for the whole story: the session-scoped SELECT the rules
// pass runs must be served by the composite index, not a materialize-and-sort.
func TestSessionEventsForRulesUsesTheIndexWithoutASort(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	q := rulesSelectColumns + " FROM events WHERE session_id = ? ORDER BY started_at ASC"

	// (a) no temp b-tree sort in the plan.
	plan := explainQueryPlanDetail(t, st.db, q, "sess-explain")
	if strings.Contains(strings.ToUpper(plan), "USE TEMP B-TREE FOR ORDER BY") {
		t.Errorf("query plan uses a temp b-tree sort: %s", plan)
	}

	// (b) textual: the query string still carries its own ORDER BY. Once the
	// composite index exists, WHERE session_id = ? returns started_at order
	// off the index even with no ORDER BY at all, so (a) alone would also
	// pass with the ORDER BY silently removed.
	if !strings.Contains(q, "ORDER BY started_at") {
		t.Errorf("session-scoped SELECT lost its ORDER BY started_at: %q", q)
	}

	// (c) a fixture whose rowid order and started_at order disagree still
	// comes back in started_at order, catching a direction flip or a
	// hand-built reordering that (a) and (b) alone would miss.
	base := time.Unix(1700000000, 0)
	newer := fullEvent("req-plan-newer")
	newer.SessionID = "sess-plan-order"
	newer.StartedAt = base.Add(time.Hour)
	older := fullEvent("req-plan-older")
	older.SessionID = "sess-plan-order"
	older.StartedAt = base
	if _, _, err := st.InsertEvent(ctx, newer); err != nil {
		t.Fatalf("InsertEvent newer: %v", err)
	}
	if _, _, err := st.InsertEvent(ctx, older); err != nil {
		t.Fatalf("InsertEvent older: %v", err)
	}

	rows, err := st.SessionEventsForRules(ctx, "sess-plan-order")
	if err != nil {
		t.Fatalf("SessionEventsForRules: %v", err)
	}
	if len(rows) != 2 || rows[0].RequestID != "req-plan-older" || rows[1].RequestID != "req-plan-newer" {
		t.Fatalf("rows = %+v, want [req-plan-older, req-plan-newer] in started_at order", rows)
	}
}

// TestSessionEventsForRulesReturnsTheSameRowsInOrder (§6 test 2): compared
// against GetEvent's full-projection fields on a fixture, so the rules
// projection cannot silently drop a column the rules use.
func TestSessionEventsForRulesReturnsTheSameRowsInOrder(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first := fullEvent("req-rules-first")
	first.SessionID = "sess-rules"
	first.StartedAt = time.Unix(1700000000, 0)
	second := fullEvent("req-rules-second")
	second.SessionID = "sess-rules"
	second.StartedAt = time.Unix(1700000100, 0)

	id1, _, err := st.InsertEvent(ctx, first)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	id2, _, err := st.InsertEvent(ctx, second)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	want1, err := st.GetEvent(ctx, id1)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	want2, err := st.GetEvent(ctx, id2)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}

	rows, err := st.SessionEventsForRules(ctx, "sess-rules")
	if err != nil {
		t.Fatalf("SessionEventsForRules: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for i, want := range []*Event{want1, want2} {
		got := rows[i]
		if got.RequestID != want.RequestID || !got.StartedAt.Equal(want.StartedAt) ||
			got.ModelResolved != want.ModelResolved || got.TotalPromptTokens != want.TotalPromptTokens ||
			got.PrefixHash == nil || want.PrefixHash == nil || *got.PrefixHash != *want.PrefixHash ||
			got.HasReqBody != want.HasReqBody || got.ToolNames != want.ToolNames {
			t.Errorf("row %d = %+v, want the full-projection fields of %+v", i, got, want)
		}
		if got.ReqBody != nil || got.RespBody != nil || got.ReqHeaders != "" || got.RespHeaders != "" ||
			got.TranscriptContent != nil || got.TranscriptRole != "" {
			t.Errorf("row %d retained an omitted blob: %+v", i, got)
		}
	}
}

// TestRulesProjectionNamesEveryColumnTheRulesRead (§6 test 3), two
// assertions: (i) the mirrored full-minus-omitted check TestSummary...
// already runs for the summary projection, applied to the rules one; and
// (ii) the nine columns the rules actually read (§2), named explicitly --
// rulesOmittedColumns derives from summaryOmittedColumns, whose membership
// nothing pins, so (i) alone would pass even if a body column a rule reads
// were accidentally omitted.
func TestRulesProjectionNamesEveryColumnTheRulesRead(t *testing.T) {
	full := parseSelectColumns(t, eventSelectColumns)
	rules := parseSelectColumns(t, rulesSelectColumns)

	if want := len(full) - len(rulesOmittedColumns); len(rules) != want {
		t.Fatalf("rules projection selects %d columns, want %d (full %d minus %d omitted)",
			len(rules), want, len(full), len(rulesOmittedColumns))
	}
	for _, c := range rulesOmittedColumns {
		if !containsStr(full, c) {
			t.Errorf("rulesOmittedColumns names %q, which the full projection does not select", c)
		}
	}
	for _, c := range rules {
		if containsStr(rulesOmittedColumns, c) {
			t.Errorf("rules projection selects the omitted column %q", c)
		}
	}

	mustRead := []string{
		"id", "started_at", "ended_at", "total_prompt_tokens",
		"cache_write_5m_tokens", "cache_write_1h_tokens", "cache_read_tokens",
		"prefix_hash", "req_tool_names",
	}
	for _, c := range mustRead {
		if !containsStr(rules, c) {
			t.Errorf("rules projection does not select %q, which a session rule reads", c)
		}
	}
}

// TestRulesScanMatchesRulesColumns (§6 test 3): the 42-destination count,
// mirroring TestSummaryScanMatchesSummaryColumns.
func TestRulesScanMatchesRulesColumns(t *testing.T) {
	var es EventSummary
	var v eventScanVals

	if got, want := len(v.dest(&es)), len(rulesColumnNames); got != want {
		t.Errorf("scanEventForRules takes %d destinations for %d columns", got, want)
	}
}

// TestRulesProjectionOmitsRequestBodies (br-GI-13-07): the mirrored check
// TestSummaryColumnsAreTheFullSetMinusBodies already runs for the summary
// projection, applied to the rules one -- rulesOmittedColumns is now
// identical to summaryOmittedColumns, req_body included.
func TestRulesProjectionOmitsRequestBodies(t *testing.T) {
	if containsStr(rulesColumnNames, "req_body") {
		t.Error("rulesColumnNames still selects req_body")
	}
	if len(rulesOmittedColumns) != len(summaryOmittedColumns) {
		t.Fatalf("rulesOmittedColumns = %v, want the same set as summaryOmittedColumns = %v", rulesOmittedColumns, summaryOmittedColumns)
	}
	for _, c := range summaryOmittedColumns {
		if !containsStr(rulesOmittedColumns, c) {
			t.Errorf("rulesOmittedColumns is missing %q, present in summaryOmittedColumns", c)
		}
	}
}

// TestReqToolNamesIsNullExactlyWhenThereIsNoBody (br-GI-13-07): three rows --
// a proxy row with tools, a proxy row whose body declares no tools, and a
// JSONL row with no body -- come back as ["…"], [] and NULL respectively.
// The middle row is the point: a ''-defaulted column would pass the other
// two and fail this one.
func TestReqToolNamesIsNullExactlyWhenThereIsNoBody(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	withTools := fullEvent("req-tools-some")
	withTools.ToolNames = EncodeToolNames([]string{"bash"})
	idWithTools, _, err := st.InsertEvent(ctx, withTools)
	if err != nil {
		t.Fatalf("InsertEvent withTools: %v", err)
	}

	noTools := fullEvent("req-tools-none")
	noTools.ToolNames = EncodeToolNames(nil)
	idNoTools, _, err := st.InsertEvent(ctx, noTools)
	if err != nil {
		t.Fatalf("InsertEvent noTools: %v", err)
	}

	// A JSONL row: no request body. ToolNames is deliberately set to a
	// nonsense value here to prove the write derives NULL from len(ReqBody),
	// not from whatever this field happens to hold.
	jsonlRow := fullEvent("req-tools-jsonl")
	jsonlRow.Source = "jsonl"
	jsonlRow.ReqBody = nil
	jsonlRow.RespHeaders = ""
	jsonlRow.ReqHeaders = ""
	jsonlRow.ToolNames = "garbage"
	idJSONL, _, err := st.InsertEvent(ctx, jsonlRow)
	if err != nil {
		t.Fatalf("InsertEvent jsonlRow: %v", err)
	}

	got, err := st.GetEvent(ctx, idWithTools)
	if err != nil {
		t.Fatalf("GetEvent withTools: %v", err)
	}
	if !got.HasReqBody || got.ToolNames != `["bash"]` {
		t.Errorf("withTools: HasReqBody=%v ToolNames=%q, want true and [\"bash\"]", got.HasReqBody, got.ToolNames)
	}

	got, err = st.GetEvent(ctx, idNoTools)
	if err != nil {
		t.Fatalf("GetEvent noTools: %v", err)
	}
	if !got.HasReqBody || got.ToolNames != `[]` {
		t.Errorf("noTools: HasReqBody=%v ToolNames=%q, want true and []", got.HasReqBody, got.ToolNames)
	}

	got, err = st.GetEvent(ctx, idJSONL)
	if err != nil {
		t.Fatalf("GetEvent jsonlRow: %v", err)
	}
	if got.HasReqBody || got.ToolNames != "" {
		t.Errorf("jsonlRow: HasReqBody=%v ToolNames=%q, want false and \"\"", got.HasReqBody, got.ToolNames)
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

func hasIndex(t *testing.T, db *sql.DB, index string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&n)
	if err != nil {
		t.Fatalf("probe for index %s: %v", index, err)
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
	return stripReqToolNamesColumn(t, head+"\n);"+schemaSQL[start+end+len("\n);"):])
}

// stripReqToolNamesColumn removes the req_tool_names column (br-GI-13-07,
// migrations[2]) from a schema text, for a fixture standing in for a
// database at schemaVersion < 3. Shared by eventsSchemaWithoutTranscriptColumns
// (a database at version 0) and preIndexChangeSchema (a database at version
// 1): both predate this column, so a fixture built from the *current*
// schemaSQL text would already carry it, and the ALTER TABLE ADD COLUMN
// migration this bead adds would then fail "duplicate column name" the
// moment either fixture's Open runs every migration from its stamped
// version forward.
func stripReqToolNamesColumn(t *testing.T, schema string) string {
	t.Helper()
	const before = "status                  INTEGER,"
	const after = "req_headers             TEXT,"
	i := strings.Index(schema, before)
	if i < 0 {
		t.Fatal("schema.sql no longer declares status immediately before req_tool_names")
	}
	rest := schema[i+len(before):]
	j := strings.Index(rest, after)
	if j < 0 {
		t.Fatal("schema.sql no longer declares req_headers after req_tool_names")
	}
	return schema[:i+len(before)] + rest[j:]
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
// database can exist with events but without a later table -- or without one
// of the events indexes. The exec runs on every Open precisely so it repairs
// both, and the migration must still apply exactly once alongside it.
func TestMigrateHealsAPartialDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	buildPreChangeDB(t, path, "warnings")

	// An index the schema exec re-creates (schema.sql:69), dropped here to
	// stand in for the other half of a first exec that died mid-file: the
	// missing index heals by the same IF NOT EXISTS mechanism as the table.
	// idx_events_session_id no longer exists in schema.sql (br-GI-13-02
	// replaced it with the composite idx_events_session_started), so this
	// must name an index schema.sql still creates.
	const idx = "idx_events_started_at"
	db := rawDB(t, path)
	if _, err := db.Exec("DROP INDEX " + idx); err != nil {
		t.Fatalf("DROP INDEX %s: %v", idx, err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a partial database: %v", err)
	}
	defer st.Close()

	if !hasTable(t, st.db, "warnings") {
		t.Error("the always-run schema exec did not heal the missing warnings table")
	}
	if !hasIndex(t, st.db, idx) {
		t.Error("the always-run schema exec did not heal the missing index")
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

// preIndexChangeSchema returns schemaSQL with the composite session index
// swapped back for the single-column index it replaced -- the on-disk shape
// of a database one migration behind schemaVersion (br-GI-13-02's "before").
func preIndexChangeSchema(t *testing.T) string {
	t.Helper()
	const oldLine = "CREATE INDEX IF NOT EXISTS idx_events_session_id ON events(session_id);"
	const newLine = "CREATE INDEX IF NOT EXISTS idx_events_session_started ON events(session_id, started_at);"
	if !strings.Contains(schemaSQL, newLine) {
		t.Fatal("schema.sql no longer creates idx_events_session_started with the expected text")
	}
	// A database at user_version = 1 also predates req_tool_names
	// (migrations[2], br-GI-13-07): strip it here too, or Open's forward
	// migration from 1 hits the same "duplicate column name" this bead's
	// other pre-change fixture guards against.
	return stripReqToolNamesColumn(t, strings.Replace(schemaSQL, newLine, oldLine, 1))
}

// TestMigrateAddsTheCompositeIndexAtVersionTwo (§6 test 9): a database at
// user_version = 1 with rows present reaches version 2, holds the composite
// index, has no idx_events_session_id, and still returns its rows.
func TestMigrateAddsTheCompositeIndexAtVersionTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-index.db")
	db := rawDB(t, path)
	if _, err := db.Exec(preIndexChangeSchema(t)); err != nil {
		t.Fatalf("build a pre-index-change database: %v", err)
	}
	const sessionID = "sess-pre-index"
	if _, err := db.Exec(
		`INSERT INTO events (request_id, source, first_source, started_at, session_id) VALUES (?, 'proxy', 'proxy', ?, ?)`,
		"req-pre-index", time.Now().UnixNano(), sessionID,
	); err != nil {
		t.Fatalf("seed a row: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("seed user_version: %v", err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a database one migration behind: %v", err)
	}
	defer st.Close()

	if got := userVersion(t, st.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if !hasIndex(t, st.db, "idx_events_session_started") {
		t.Error("the migration did not create idx_events_session_started")
	}
	if hasIndex(t, st.db, "idx_events_session_id") {
		t.Error("the migration left idx_events_session_id behind")
	}

	rows, err := st.SessionEventsForRules(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("SessionEventsForRules: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "req-pre-index" {
		t.Errorf("SessionEventsForRules = %+v, want the seeded row still returned", rows)
	}
}

// preStatsIndexSchema returns schemaSQL with idx_events_stats removed -- the
// on-disk shape of a database one migration behind schemaVersion (br-GI-13-08's
// "before"). idx_events_cost_source is left alone: it was never dropped, so
// it already appears in schemaSQL ahead of the block this strips out. Built
// from bare schemaSQL, not buildPreChangeDB: that fixture's helper also
// strips req_tool_names (migrations[2]) and the transcript columns
// (migrations[0]), which a database sitting at user_version = 3 already
// carries.
func preStatsIndexSchema(t *testing.T) string {
	t.Helper()
	const marker = "-- Covers three of the four /api/stats aggregate queries"
	i := strings.Index(schemaSQL, marker)
	if i < 0 {
		t.Fatal("schema.sql no longer has the idx_events_stats comment")
	}
	rest := schemaSQL[i:]
	j := strings.Index(rest, ");")
	if j < 0 {
		t.Fatal("schema.sql's idx_events_stats block has no closing paren")
	}
	after := strings.TrimLeft(rest[j+len(");"):], "\r\n")
	return schemaSQL[:i] + after
}

// TestMigrateAddsTheStatsIndexAtVersionFour: a database at user_version = 3
// with a row present reaches version 4, holds idx_events_stats, and still
// returns its rows.
func TestMigrateAddsTheStatsIndexAtVersionFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-stats-index.db")
	db := rawDB(t, path)
	if _, err := db.Exec(preStatsIndexSchema(t)); err != nil {
		t.Fatalf("build a pre-stats-index database: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO events (request_id, source, first_source, started_at) VALUES (?, 'proxy', 'proxy', ?)`,
		"req-pre-stats-index", time.Now().UnixNano(),
	); err != nil {
		t.Fatalf("seed a row: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatalf("seed user_version: %v", err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a database one migration behind: %v", err)
	}
	defer st.Close()

	if got := userVersion(t, st.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if !hasIndex(t, st.db, "idx_events_stats") {
		t.Error("the migration did not create idx_events_stats")
	}
	if !hasIndex(t, st.db, "idx_events_cost_source") {
		t.Error("the migration dropped idx_events_cost_source, want it kept")
	}

	n, err := st.CountEvents(context.Background(), EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Errorf("CountEvents = %d, want the seeded row still present", n)
	}
}

// TestFreshSchemaMatchesTheMigratedShape: a fresh DB reaches the same index
// set as a migrated one, so a future edit to one home alone fails the other.
func TestFreshSchemaMatchesTheMigratedShape(t *testing.T) {
	freshSt, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open fresh: %v", err)
	}
	defer freshSt.Close()

	migratedPath := filepath.Join(t.TempDir(), "migrated.db")
	buildPreChangeDB(t, migratedPath)
	migratedSt, err := Open(migratedPath)
	if err != nil {
		t.Fatalf("Open pre-change: %v", err)
	}
	defer migratedSt.Close()

	for name, st := range map[string]*Store{"fresh": freshSt, "migrated": migratedSt} {
		if !hasIndex(t, st.db, "idx_events_session_started") {
			t.Errorf("%s: missing idx_events_session_started", name)
		}
		if hasIndex(t, st.db, "idx_events_session_id") {
			t.Errorf("%s: idx_events_session_id present, want dropped", name)
		}
		if !hasIndex(t, st.db, "idx_events_stats") {
			t.Errorf("%s: missing idx_events_stats", name)
		}
		if !hasIndex(t, st.db, "idx_events_cost_source") {
			t.Errorf("%s: missing idx_events_cost_source", name)
		}
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

// --- the stats covering index (br-GI-13-08) -------------------------------

// statsFixtureEvents returns a small mixed-billing-mode fixture: one api row
// (cost_usd set), one subscription row (api_equivalent_cost_usd set), and one
// unpriced row -- the shape every branch in statsSelectColumnsInner and
// StatsByCostSource's CASE keys off (billing_mode, cost_source).
func statsFixtureEvents() []*Event {
	api := fullEvent("req-stats-api")

	sub := fullEvent("req-stats-sub")
	sub.BillingMode = "subscription"
	sub.CostUSD = nil
	sub.ApiEquivalentCostUSD = f64(0.08)
	sub.ModelResolved = "claude-opus-5"
	sub.CostSource = "shipped"

	unpriced := fullEvent("req-stats-unpriced")
	unpriced.CostUSD = nil
	unpriced.CostSource = "unpriced"

	return []*Event{api, sub, unpriced}
}

// TestStatsQueriesUseTheCoveringIndex (§6, br-GI-13-08): StatsSummary,
// StatsByModel, and StatsByPeriod must be served off idx_events_stats as a
// covering index rather than a bare table scan, since none of them selects a
// blob column but the table's B-tree carries them on every page.
// StatsByCostSource is the documented exception: cost_source is not a
// leading column of idx_events_stats, so SQLite keeps using the narrower
// idx_events_cost_source for its GROUP BY -- still an index, not a bare
// scan, but not a covering one either. See schema.sql's comment on both
// indexes.
func TestStatsQueriesUseTheCoveringIndex(t *testing.T) {
	st := newTestStore(t)
	where, args := EventFilter{}.whereClause()

	queries := []struct {
		query    string
		covering bool
	}{
		{statsSelectColumns + " FROM events" + where, true},
		{"SELECT model_resolved, billing_mode, " + statsSelectColumnsInner +
			" FROM events" + where + " GROUP BY model_resolved, billing_mode ORDER BY model_resolved", true},
		{"SELECT " + periodExprs["day"] + " AS period, billing_mode, " + statsSelectColumnsInner +
			" FROM events" + where + " GROUP BY period, billing_mode ORDER BY period", true},
		{"SELECT cost_source, COUNT(*), SUM(CASE WHEN billing_mode = 'api' THEN cost_usd END)" +
			" FROM events" + where + " GROUP BY cost_source ORDER BY cost_source", false},
	}
	for _, q := range queries {
		plan := strings.ToUpper(explainQueryPlanDetail(t, st.db, q.query, args...))
		usesIndex := strings.Contains(plan, "USING INDEX") || strings.Contains(plan, "USING COVERING INDEX")
		if !usesIndex {
			t.Errorf("query plan is a bare table scan, want at least an index:\nquery: %s\nplan: %s", q.query, plan)
		}
		if q.covering && !strings.Contains(plan, "COVERING INDEX IDX_EVENTS_STATS") {
			t.Errorf("query plan does not use idx_events_stats as a covering index:\nquery: %s\nplan: %s", q.query, plan)
		}
	}
}

// TestStatsAggregatesAreUnchangedByTheIndex: idx_events_stats changes how the
// four stats queries are served, not what they return. The same fixture rows
// must produce identical results whether the index exists or not.
func TestStatsAggregatesAreUnchangedByTheIndex(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, st *Store) {
		t.Helper()
		for _, ev := range statsFixtureEvents() {
			if _, _, err := st.InsertEvent(ctx, ev); err != nil {
				t.Fatalf("seed InsertEvent: %v", err)
			}
		}
	}

	beforePath := filepath.Join(t.TempDir(), "before-index.db")
	beforeDB := rawDB(t, beforePath)
	defer beforeDB.Close()
	if _, err := beforeDB.Exec(preStatsIndexSchema(t)); err != nil {
		t.Fatalf("build a pre-stats-index database: %v", err)
	}
	beforeSt := &Store{db: beforeDB}
	seed(t, beforeSt)
	if hasIndex(t, beforeDB, "idx_events_stats") {
		t.Fatal("pre-stats-index fixture already has idx_events_stats")
	}

	afterSt := newTestStore(t)
	seed(t, afterSt)
	if !hasIndex(t, afterSt.db, "idx_events_stats") {
		t.Fatal("newTestStore's database is missing idx_events_stats")
	}

	summaryBefore, err := beforeSt.StatsSummary(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("StatsSummary (before): %v", err)
	}
	summaryAfter, err := afterSt.StatsSummary(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("StatsSummary (after): %v", err)
	}
	if !reflect.DeepEqual(summaryBefore, summaryAfter) {
		t.Errorf("StatsSummary changed:\nbefore=%+v\nafter=%+v", summaryBefore, summaryAfter)
	}

	byModelBefore, err := beforeSt.StatsByModel(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("StatsByModel (before): %v", err)
	}
	byModelAfter, err := afterSt.StatsByModel(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("StatsByModel (after): %v", err)
	}
	if !reflect.DeepEqual(byModelBefore, byModelAfter) {
		t.Errorf("StatsByModel changed:\nbefore=%+v\nafter=%+v", byModelBefore, byModelAfter)
	}

	byPeriodBefore, err := beforeSt.StatsByPeriod(ctx, EventFilter{}, "day")
	if err != nil {
		t.Fatalf("StatsByPeriod (before): %v", err)
	}
	byPeriodAfter, err := afterSt.StatsByPeriod(ctx, EventFilter{}, "day")
	if err != nil {
		t.Fatalf("StatsByPeriod (after): %v", err)
	}
	if !reflect.DeepEqual(byPeriodBefore, byPeriodAfter) {
		t.Errorf("StatsByPeriod changed:\nbefore=%+v\nafter=%+v", byPeriodBefore, byPeriodAfter)
	}

	byCostSourceBefore, err := beforeSt.StatsByCostSource(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("StatsByCostSource (before): %v", err)
	}
	byCostSourceAfter, err := afterSt.StatsByCostSource(ctx, EventFilter{})
	if err != nil {
		t.Fatalf("StatsByCostSource (after): %v", err)
	}
	if !reflect.DeepEqual(byCostSourceBefore, byCostSourceAfter) {
		t.Errorf("StatsByCostSource changed:\nbefore=%+v\nafter=%+v", byCostSourceBefore, byCostSourceAfter)
	}
}

// TestPurgeUnpricedUsesTheCostSourceIndex pins the regression idx_events_stats
// caused when idx_events_cost_source was dropped alongside it: cost_source
// sits at position 10 of 13 in idx_events_stats, not a leading column, so a
// DELETE keyed on cost_source alone got no seek out of it and fell back to a
// bare SCAN of the whole table. Verified with EXPLAIN QUERY PLAN, not assumed
// -- see schema.sql's comment on idx_events_cost_source.
func TestPurgeUnpricedUsesTheCostSourceIndex(t *testing.T) {
	st := newTestStore(t)
	plan := strings.ToUpper(explainQueryPlanDetail(t, st.db,
		"DELETE FROM events WHERE cost_source = 'unpriced'"))
	if !strings.Contains(plan, "SEARCH EVENTS USING") || !strings.Contains(plan, "IDX_EVENTS_COST_SOURCE") {
		t.Errorf("PurgeUnpriced's DELETE does not seek via idx_events_cost_source:\nplan: %s", plan)
	}
}
