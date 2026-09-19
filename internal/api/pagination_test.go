package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// listRoutes are the three paginated endpoints, so a header or parameter
// rule is asserted on all of them rather than on whichever one the author
// happened to be editing.
var listRoutes = []string{"/api/requests", "/api/warnings", "/api/sessions"}

func wantHeader(t *testing.T, rr *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rr.Header().Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func seedEventsAt(t *testing.T, st *store.Store, n int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		offset := time.Duration(i) * time.Second
		seedEvent(t, st, func(e *store.Event) { e.StartedAt = base.Add(offset) })
	}
}

func TestPageHeadersPresentOnEveryListRoute(t *testing.T) {
	st := newTestStore(t)
	seedEventsAt(t, st, 5)
	handler, _, _, _ := newTestAPI(t, st)

	for _, path := range listRoutes {
		rr := getOK(t, handler, path)
		for _, name := range []string{"X-Total-Count", "X-Limit", "X-Offset"} {
			if rr.Header().Get(name) == "" {
				t.Errorf("GET %s: %s header is missing", path, name)
			}
		}
	}
}

// TestPageHeaderLimitIsEffective is the regression guard for the X-Limit: 0
// bug. An absent ?limit parses to 0, which the store clamps to
// store.DefaultLimit, so the header must report the *applied* page size.
func TestPageHeaderLimitIsEffective(t *testing.T) {
	st := newTestStore(t)
	seedEventsAt(t, st, 5)
	handler, _, _, _ := newTestAPI(t, st)

	for _, path := range listRoutes {
		rr := getOK(t, handler, path)
		wantHeader(t, rr, "X-Limit", strconv.Itoa(store.DefaultLimit))

		rr = getOK(t, handler, path+"?limit=25")
		wantHeader(t, rr, "X-Limit", "25")
	}
}

func TestListRequestsOffsetWindow(t *testing.T) {
	st := newTestStore(t)
	seedEventsAt(t, st, 10)
	handler, _, _, _ := newTestAPI(t, st)

	page1 := decodeJSON[[]*store.Event](t, getOK(t, handler, "/api/requests?limit=5&offset=0").Body)
	page2 := decodeJSON[[]*store.Event](t, getOK(t, handler, "/api/requests?limit=5&offset=5").Body)
	if len(page1) != 5 || len(page2) != 5 {
		t.Fatalf("page sizes: got %d and %d, want 5 and 5", len(page1), len(page2))
	}

	onPage1 := map[int64]bool{}
	for _, e := range page1 {
		onPage1[e.ID] = true
	}
	for _, e := range page2 {
		if onPage1[e.ID] {
			t.Errorf("event %d appears on both offset=0 and offset=5 -- the windows are not disjoint", e.ID)
		}
	}
}

func TestPageHeaderOffset(t *testing.T) {
	st := newTestStore(t)
	seedEventsAt(t, st, 10)
	handler, _, _, _ := newTestAPI(t, st)

	wantHeader(t, getOK(t, handler, "/api/requests?limit=5"), "X-Offset", "0")
	wantHeader(t, getOK(t, handler, "/api/requests?limit=5&offset=7"), "X-Offset", "7")
}

// TestPageHeaderTotalCountIsWindowIndependent: the total describes the
// whole filtered set, so the page window must not move it.
func TestPageHeaderTotalCountIsWindowIndependent(t *testing.T) {
	st := newTestStore(t)
	seedEventsAt(t, st, 10)
	handler, _, _, _ := newTestAPI(t, st)

	wantHeader(t, getOK(t, handler, "/api/requests?limit=1"), "X-Total-Count", "10")
	wantHeader(t, getOK(t, handler, "/api/requests?limit=1000"), "X-Total-Count", "10")
	wantHeader(t, getOK(t, handler, "/api/requests?offset=50"), "X-Total-Count", "10")
}

func TestPageHeaderTotalCountHonorsPredicate(t *testing.T) {
	st := newTestStore(t)
	seedEventsAt(t, st, 10)
	seedEvent(t, st, func(e *store.Event) {
		e.ModelRequested, e.ModelResolved = "claude-opus-5", "claude-opus-5"
	})
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/requests?model=claude-opus-5")
	wantHeader(t, rr, "X-Total-Count", "1")
	got := decodeJSON[[]*store.Event](t, rr.Body)
	if len(got) != 1 {
		t.Errorf("got %d rows, want 1 -- the total and the body must describe the same set", len(got))
	}
}

func TestWarningsTotalCountHonorsKind(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ev := seedEvent(t, st, nil)
	warnings := []store.Warning{
		{Kind: "alpha", Severity: "warn", Detail: "a"},
		{Kind: "beta", Severity: "warn", Detail: "b"},
	}
	if err := st.UpsertWarnings(ctx, ev.ID, warnings); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/warnings?kind=alpha")
	wantHeader(t, rr, "X-Total-Count", "1")

	got := decodeJSON[[]store.Warning](t, rr.Body)
	if len(got) != 1 {
		t.Errorf("got %d warnings of kind alpha, want 1", len(got))
	}
	for _, w := range got {
		if w.Kind != "alpha" {
			t.Errorf("kind filter returned a %q warning", w.Kind)
		}
	}
}

func TestNegativeOffsetRejected(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	for _, path := range listRoutes {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path+"?offset=-1", nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("GET %s?offset=-1: status = %d, want 400", path, rr.Code)
		}
	}
}

// TestSessionsPageBodyStaysABareArray is the proof that the headers
// approach did not quietly become a response envelope.
func TestSessionsPageBodyStaysABareArray(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Now()
	for i := 0; i < 4; i++ {
		id := "s_" + strconv.Itoa(i)
		if err := st.UpsertSession(ctx, id, "", base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("UpsertSession: %v", err)
		}
	}
	handler, _, _, _ := newTestAPI(t, st)

	rr := getOK(t, handler, "/api/sessions?limit=2")
	wantHeader(t, rr, "X-Total-Count", "4")
	wantHeader(t, rr, "X-Limit", "2")
	wantHeader(t, rr, "X-Offset", "0")

	got := decodeJSON[[]*store.Session](t, rr.Body)
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2 -- the paged body must still be a bare array", len(got))
	}
}
