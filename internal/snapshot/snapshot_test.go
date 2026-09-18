package snapshot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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

// withCookie fixes the poller's credential to a known test value, so the
// suite never has to exercise internal/secret's real OS-level ACL
// machinery just to reach the "credential is set" path.
func withCookie(p *Poller, value string) {
	p.credential = func() (string, error) { return value, nil }
}

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Poller, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	st := newTestStore(t)
	p := New(srv.URL, "acct1", st)
	withCookie(p, "test-session-cookie")
	return srv, p, st
}

// One row per window per poll: three windows in the response yield three
// quota_snapshots rows.
func TestPollWritesOneRowPerWindow(t *testing.T) {
	body := `{"windows":[
		{"window":"5h","utilization_pct":42.5,"resets_at":"2026-01-01T05:00:00Z"},
		{"window":"7d","utilization_pct":10.0,"resets_at":"2026-01-08T00:00:00Z"},
		{"window":"7d:claude-sonnet-5","utilization_pct":5.0,"resets_at":"2026-01-08T00:00:00Z"}
	]}`
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	res, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "ok" || res.Rows != 3 {
		t.Fatalf("Result = %+v, want status=ok rows=3", res)
	}

	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 3 {
		t.Fatalf("stored snapshots = %d, want 3", len(snaps))
	}
}

// Test 16: a renamed utilization field can't be found under any known
// alias -- that window is recorded parse_error, not crashed past.
func TestPollRenamedFieldRecordsParseError(t *testing.T) {
	body := `{"windows":[{"window":"5h","pct_used":42.5}]}`
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})

	if _, err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("stored snapshots = %d, want 1", len(snaps))
	}
	if snaps[0].Status != "parse_error" {
		t.Errorf("Status = %q, want parse_error", snaps[0].Status)
	}
	if snaps[0].UtilizationPct != nil {
		t.Errorf("UtilizationPct = %v, want nil", snaps[0].UtilizationPct)
	}
}

// A missing optional field (resets_at) still writes the row with the
// fields that ARE present.
func TestPollMissingOptionalFieldStillWritesRow(t *testing.T) {
	body := `{"windows":[{"window":"5h","utilization_pct":30}]}`
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})

	if _, err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("stored snapshots = %d, want 1", len(snaps))
	}
	if snaps[0].Status != "ok" {
		t.Errorf("Status = %q, want ok", snaps[0].Status)
	}
	if snaps[0].UtilizationPct == nil || *snaps[0].UtilizationPct != 30 {
		t.Errorf("UtilizationPct = %v, want 30", snaps[0].UtilizationPct)
	}
	if snaps[0].ResetsAt != nil {
		t.Errorf("ResetsAt = %v, want nil (the field was absent)", snaps[0].ResetsAt)
	}
}

// An extra, previously-unseen window name is just another row -- the
// one-row-per-window design already tolerates an arbitrary window set.
func TestPollExtraWindowIsAnExtraRow(t *testing.T) {
	body := `{"windows":[
		{"window":"5h","utilization_pct":1},
		{"window":"a-window-nobody-documented","utilization_pct":2}
	]}`
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})

	res, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Rows != 2 {
		t.Fatalf("Rows = %d, want 2", res.Rows)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	found := false
	for _, s := range snaps {
		if s.Window == "a-window-nobody-documented" {
			found = true
		}
	}
	if !found {
		t.Error("extra window name not found among stored snapshots")
	}
}

// A 401 records unauthorized, not a crash.
func TestPollUnauthorized(t *testing.T) {
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	res, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "unauthorized" {
		t.Errorf("Status = %q, want unauthorized", res.Status)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Status != "unauthorized" {
		t.Fatalf("stored snapshots = %+v, want one unauthorized row", snaps)
	}
}

// An unparseable body records unavailable, not a crash.
func TestPollUnparseableBody(t *testing.T) {
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json at all"))
	})

	res, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", res.Status)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Status != "unavailable" {
		t.Fatalf("stored snapshots = %+v, want one unavailable row", snaps)
	}
}

// An unreachable endpoint (transport failure) also records unavailable,
// and the error the transport produced -- which can embed the request,
// cookie header included -- is never turned into the stored Raw/error
// text.
func TestPollUnreachableNeverLeaksCredential(t *testing.T) {
	st := newTestStore(t)
	p := New("http://127.0.0.1:0/unreachable", "acct1", st)
	withCookie(p, "super-secret-cookie-value")
	p.SetHTTPClient(fakeFailingClient{})

	res, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", res.Status)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("stored snapshots = %d, want 1", len(snaps))
	}
	if strings.Contains(snaps[0].Raw, "super-secret-cookie-value") {
		t.Error("stored Raw contains the sessionKey value")
	}
}

type fakeFailingClient struct{}

func (fakeFailingClient) Do(req *http.Request) (*http.Response, error) {
	// A real net/http transport error's message includes the request URL
	// and sometimes headers; simulate the worst case directly.
	return nil, errors.New("dial tcp 127.0.0.1:0: connect: cookie=super-secret-cookie-value")
}

// No credential configured yet: Poll is a no-op, not a failure or a
// fabricated row.
func TestPollUnconfiguredCredential(t *testing.T) {
	st := newTestStore(t)
	p := New("http://example.invalid", "acct1", st)
	p.credential = func() (string, error) { return "", errUnsetForTest }

	res, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.Status != "unconfigured" {
		t.Errorf("Status = %q, want unconfigured", res.Status)
	}
	snaps, err := st.ListQuotaSnapshots(context.Background(), "acct1", 10)
	if err != nil {
		t.Fatalf("ListQuotaSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("stored snapshots = %d, want 0 (nothing to report)", len(snaps))
	}
}

var errUnsetForTest = errors.New("secret: not set")

// A successful poll advances the snapshot:<account> ingest_state cursor.
func TestPollAdvancesCursor(t *testing.T) {
	_, p, st := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"windows":[{"window":"5h","utilization_pct":1}]}`))
	})

	before, ok, err := st.GetIngestState(context.Background(), cursorKey("acct1"))
	if err != nil {
		t.Fatalf("GetIngestState: %v", err)
	}
	if ok {
		t.Fatalf("cursor already set before any poll: %+v", before)
	}

	if _, err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	after, ok, err := st.GetIngestState(context.Background(), cursorKey("acct1"))
	if err != nil {
		t.Fatalf("GetIngestState: %v", err)
	}
	if !ok || after.Value == "" {
		t.Fatalf("cursor after poll = %+v, want a set value", after)
	}
}

// The request actually carries the sessionKey cookie.
func TestPollSendsSessionKeyCookie(t *testing.T) {
	var gotCookie string
	_, p, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Write([]byte(`{"windows":[]}`))
	})

	if _, err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotCookie != "sessionKey=test-session-cookie" {
		t.Errorf("Cookie header = %q, want sessionKey=test-session-cookie", gotCookie)
	}
}
