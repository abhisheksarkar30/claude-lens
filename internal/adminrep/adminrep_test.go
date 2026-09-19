package adminrep

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// openRaw opens a second connection to the same SQLite file for
// test-only, read-only verification of the admin_* tables -- internal/
// store deliberately exposes no reader for them (this bead only writes
// them; a reader is a later bead's concern), so idempotence can only be
// asserted by inspecting the file directly.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("openRaw: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func inputTokensFor(t *testing.T, db *sql.DB, dayStart int64, model string) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		"SELECT input_tokens FROM admin_usage_days WHERE day_start = ? AND model = ?", dayStart, model,
	).Scan(&n)
	if err != nil {
		t.Fatalf("inputTokensFor: %v", err)
	}
	return n
}

func withAdminKey(c *Collector, key string) {
	c.credential = func() (string, error) { return key, nil }
}

// usageBucket builds one day's usage-report bucket for day (UTC midnight)
// with a single result row for model.
func usageBucket(day time.Time, model string, inputTokens int) map[string]any {
	return map[string]any{
		"starting_at": day.Format(time.RFC3339),
		"ending_at":   day.Add(24 * time.Hour).Format(time.RFC3339),
		"results": []map[string]any{
			{"model": model, "uncached_input_tokens": inputTokens, "output_tokens": 10, "cache_read_input_tokens": 1},
		},
	}
}

func usagePageJSON(t *testing.T, buckets ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": buckets, "has_more": false, "next_page": ""})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// Test 20: two collects over the same window yield the same row count and
// totals, not double.
func TestCollectUsageIdempotent(t *testing.T) {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Error("request missing x-api-key header")
		}
		w.Write(usagePageJSON(t, usageBucket(day, "claude-sonnet-5", 100)))
	}))
	defer srv.Close()

	st, dbPath := newTestStore(t)
	c := New(srv.URL, srv.URL, srv.URL, st)
	withAdminKey(c, "test-admin-key")
	ctx := context.Background()

	since, until := day, day.Add(24*time.Hour)
	res1, err := c.CollectUsage(ctx, since, until)
	if err != nil || res1.Status != "ok" || res1.Rows != 1 {
		t.Fatalf("first collect: res=%+v err=%v", res1, err)
	}
	res2, err := c.CollectUsage(ctx, since, until)
	if err != nil || res2.Status != "ok" || res2.Rows != 1 {
		t.Fatalf("second collect: res=%+v err=%v", res2, err)
	}

	raw := openRaw(t, dbPath)
	if n := countRows(t, raw, "admin_usage_days"); n != 1 {
		t.Errorf("admin_usage_days has %d rows after two identical collects, want 1", n)
	}
}

// An overlapping re-fetch updates existing (day, model) rows in place and
// inserts only the genuinely new ones -- no duplicates.
func TestCollectUsageOverlappingRefetch(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	model := "claude-sonnet-5"

	st, dbPath := newTestStore(t)
	ctx := context.Background()

	// First collect: days 1-7 (input_tokens=100 each).
	var page1 []map[string]any
	for i := 0; i < 7; i++ {
		page1 = append(page1, usageBucket(base.AddDate(0, 0, i), model, 100))
	}
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(usagePageJSON(t, page1...))
	}))
	c := New(srv1.URL, srv1.URL, srv1.URL, st)
	withAdminKey(c, "k")
	if _, err := c.CollectUsage(ctx, base, base.AddDate(0, 0, 7)); err != nil {
		t.Fatalf("first collect: %v", err)
	}
	srv1.Close()

	// Second collect: days 4-10, days 4-7 now report input_tokens=200
	// (updated), days 8-10 are new.
	var page2 []map[string]any
	for i := 3; i < 10; i++ {
		page2 = append(page2, usageBucket(base.AddDate(0, 0, i), model, 200))
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(usagePageJSON(t, page2...))
	}))
	defer srv2.Close()
	c = New(srv2.URL, srv2.URL, srv2.URL, st)
	withAdminKey(c, "k")
	if _, err := c.CollectUsage(ctx, base.AddDate(0, 0, 3), base.AddDate(0, 0, 10)); err != nil {
		t.Fatalf("second collect: %v", err)
	}

	raw := openRaw(t, dbPath)
	if n := countRows(t, raw, "admin_usage_days"); n != 10 {
		t.Fatalf("admin_usage_days has %d rows, want 10 (7 original + 3 new, days 4-7 updated in place)", n)
	}
	// Day 1 (not touched by the second fetch) keeps its original value.
	if got := inputTokensFor(t, raw, base.UnixNano(), model); got != 100 {
		t.Errorf("day 1 input_tokens = %d, want 100 (untouched by overlap)", got)
	}
	// Day 4 (in both fetches) is updated in place to the second fetch's value.
	day4 := base.AddDate(0, 0, 3)
	if got := inputTokensFor(t, raw, day4.UnixNano(), model); got != 200 {
		t.Errorf("day 4 input_tokens = %d, want 200 (updated by overlap)", got)
	}
}

// Usage and cost land in their own tables, each keyed on its own natural
// key -- collecting one never writes the other's table.
func TestCollectUsageAndCostSeparateTables(t *testing.T) {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(usagePageJSON(t, usageBucket(day, "claude-sonnet-5", 100)))
	}))
	defer usageSrv.Close()
	costSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket := map[string]any{
			"starting_at": day.Format(time.RFC3339),
			"ending_at":   day.Add(24 * time.Hour).Format(time.RFC3339),
			"results": []map[string]any{
				{"model": "claude-sonnet-5", "description": "input tokens", "currency": "USD", "amount": "1.2345"},
			},
		}
		body, _ := json.Marshal(map[string]any{"data": []map[string]any{bucket}, "has_more": false})
		w.Write(body)
	}))
	defer costSrv.Close()

	st, dbPath := newTestStore(t)
	c := New(usageSrv.URL, costSrv.URL, usageSrv.URL, st)
	withAdminKey(c, "k")
	ctx := context.Background()

	if _, err := c.CollectUsage(ctx, day, day.Add(24*time.Hour)); err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if _, err := c.CollectCost(ctx, day, day.Add(24*time.Hour)); err != nil {
		t.Fatalf("CollectCost: %v", err)
	}

	raw := openRaw(t, dbPath)
	if n := countRows(t, raw, "admin_usage_days"); n != 1 {
		t.Errorf("admin_usage_days has %d rows, want 1", n)
	}
	if n := countRows(t, raw, "admin_cost_days"); n != 1 {
		t.Errorf("admin_cost_days has %d rows, want 1", n)
	}
	var amount float64
	var currency string
	if err := raw.QueryRow("SELECT amount_usd, currency FROM admin_cost_days").Scan(&amount, &currency); err != nil {
		t.Fatalf("read admin_cost_days: %v", err)
	}
	if amount != 1.2345 || currency != "USD" {
		t.Errorf("amount=%v currency=%v, want 1.2345 USD", amount, currency)
	}
}

// window_start/window_end are recorded per row, and the admin:usage
// cursor advances to the fetched window's end so a re-run requests only
// the gap.
func TestCollectUsageRecordsWindowAndCursor(t *testing.T) {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(usagePageJSON(t, usageBucket(day, "claude-sonnet-5", 100)))
	}))
	defer srv.Close()

	st, dbPath := newTestStore(t)
	c := New(srv.URL, srv.URL, srv.URL, st)
	withAdminKey(c, "k")
	ctx := context.Background()
	until := day.Add(24 * time.Hour)
	if _, err := c.CollectUsage(ctx, day, until); err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}

	raw := openRaw(t, dbPath)
	var windowStart, windowEnd int64
	if err := raw.QueryRow("SELECT window_start, window_end FROM admin_usage_days").Scan(&windowStart, &windowEnd); err != nil {
		t.Fatalf("read window: %v", err)
	}
	if windowStart != day.UnixNano() || windowEnd != until.UnixNano() {
		t.Errorf("window = [%d, %d), want [%d, %d)", windowStart, windowEnd, day.UnixNano(), until.UnixNano())
	}

	cursor, ok, err := st.GetIngestState(ctx, "admin:usage")
	if err != nil || !ok {
		t.Fatalf("GetIngestState admin:usage: ok=%v err=%v", ok, err)
	}
	if cursor.Status != "ok" || cursor.Value == "" {
		t.Errorf("admin:usage cursor = %+v, want status=ok and a non-empty value", cursor)
	}
}

// A 401/403 records a fail-soft status, never a Go error, and the Admin
// key never appears in it.
func TestCollectUsageUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	st, dbPath := newTestStore(t)
	c := New(srv.URL, srv.URL, srv.URL, st)
	withAdminKey(c, "super-secret-admin-key")
	ctx := context.Background()

	res, err := c.CollectUsage(ctx, time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CollectUsage returned an error for a 401, want a fail-soft status: %v", err)
	}
	if res.Status != "unauthorized" {
		t.Errorf("Status = %q, want unauthorized", res.Status)
	}

	raw := openRaw(t, dbPath)
	if n := countRows(t, raw, "admin_usage_days"); n != 0 {
		t.Errorf("admin_usage_days has %d rows after a 401, want 0", n)
	}
}

// A transport failure (server unreachable) is also fail-soft, and the
// error the transport produced -- which can embed the request, headers
// included -- is never surfaced to the caller.
func TestCollectUsageTransportFailureNeverLeaksKey(t *testing.T) {
	st, _ := newTestStore(t)
	// An address nothing listens on, guaranteed to fail fast.
	c := New("http://127.0.0.1:1/usage", "http://127.0.0.1:1/cost", "http://127.0.0.1:1/limits", st)
	withAdminKey(c, "super-secret-admin-key")
	ctx := context.Background()

	res, err := c.CollectUsage(ctx, time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CollectUsage returned an error for a transport failure: %v", err)
	}
	if res.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", res.Status)
	}
}

func TestCollectUsageUnconfigured(t *testing.T) {
	st, _ := newTestStore(t)
	c := New("http://example.invalid", "http://example.invalid", "http://example.invalid", st)
	c.credential = func() (string, error) { return "", fmt.Errorf("secret: not set") }
	res, err := c.CollectUsage(context.Background(), time.Now(), time.Now())
	if err != nil {
		t.Fatalf("CollectUsage: %v", err)
	}
	if res.Status != "unconfigured" {
		t.Errorf("Status = %q, want unconfigured", res.Status)
	}
}

func TestCollectRateLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"limits": []map[string]any{
				{"scope": "organization", "model": "claude-sonnet-5", "group_type": "requests", "limit": 1000},
			},
		})
		w.Write(body)
	}))
	defer srv.Close()

	st, dbPath := newTestStore(t)
	c := New(srv.URL, srv.URL, srv.URL, st)
	withAdminKey(c, "k")
	res, err := c.CollectRateLimits(context.Background())
	if err != nil || res.Status != "ok" || res.Rows != 1 {
		t.Fatalf("CollectRateLimits: res=%+v err=%v", res, err)
	}

	raw := openRaw(t, dbPath)
	if n := countRows(t, raw, "admin_rate_limits"); n != 1 {
		t.Errorf("admin_rate_limits has %d rows, want 1", n)
	}
}

// CollectAll sums rows across all three reports and reports "ok" only
// when every one of them did.
func TestCollectAll(t *testing.T) {
	day := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(usagePageJSON(t, usageBucket(day, "claude-sonnet-5", 100)))
	}))
	defer usageSrv.Close()
	costSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer costSrv.Close()
	rlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]any{"limits": []map[string]any{}})
		w.Write(body)
	}))
	defer rlSrv.Close()

	st, _ := newTestStore(t)
	c := New(usageSrv.URL, costSrv.URL, rlSrv.URL, st)
	withAdminKey(c, "k")

	res, err := c.CollectAll(context.Background(), day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if res.Status != "unauthorized" {
		t.Errorf("Status = %q, want unauthorized (cost report failed)", res.Status)
	}
	if res.Rows != 1 {
		t.Errorf("Rows = %d, want 1 (usage's single row; cost and rate-limits contributed none)", res.Rows)
	}
}
