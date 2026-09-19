package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSourcesReportsAFailingCollector is the bead's clause: a fixture with the
// snapshot collector failed returns that source with a non-OK status and a
// reason, and the others are still OK. That second half is the point -- one
// failing source must not blank the tab (invariant 6).
func TestSourcesReportsAFailingCollector(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	okAt := time.Now().Add(-time.Minute).UTC()
	failedAt := time.Now().Add(-5 * time.Minute).UTC()
	handler.SetSourceHealth(func(context.Context) ([]SourceHealth, error) {
		return []SourceHealth{
			{Source: "jsonl", Status: "ok", LastSuccessAt: &okAt, RowsWritten: 12, CursorPosition: "offset=900"},
			{Source: "snapshot", Status: "error", LastErrorAt: &failedAt, LastError: "claude.ai usage: 401 unauthorized"},
			{Source: "admin", Status: "ok", LastSuccessAt: &okAt, RowsWritten: 3},
		}, nil
	})

	got := decodeJSON[sourcesResponse](t, getOK(t, handler, "/api/sources").Body)
	if len(got.Sources) != 3 {
		t.Fatalf("got %d sources, want 3", len(got.Sources))
	}
	byName := make(map[string]SourceHealth, len(got.Sources))
	for _, s := range got.Sources {
		byName[s.Source] = s
	}

	bad, ok := byName["snapshot"]
	if !ok {
		t.Fatal("the failing snapshot collector is missing from the response")
	}
	if bad.Status == "ok" {
		t.Errorf("the failed collector reports status %q; want a non-OK status", bad.Status)
	}
	if bad.LastError == "" {
		t.Error("the failed collector reports no reason")
	}
	if bad.LastErrorAt == nil {
		t.Error("the failed collector reports no last-error time")
	}
	// It has never succeeded, so a success time would be a lie the UI renders
	// as a date.
	if bad.LastSuccessAt != nil {
		t.Errorf("the failed collector reports a last success of %v; it has never succeeded", bad.LastSuccessAt)
	}

	for _, name := range []string{"jsonl", "admin"} {
		s, ok := byName[name]
		if !ok {
			t.Errorf("source %q is missing: one failing collector must not remove the others", name)
			continue
		}
		if s.Status != "ok" {
			t.Errorf("source %q reports status %q; want ok", name, s.Status)
		}
	}
	if byName["jsonl"].CursorPosition != "offset=900" {
		t.Errorf("the cursor position did not survive: %+v", byName["jsonl"])
	}
}

// TestSourcesNeverRanMarshalsNullTimestamps pins the wire shape rather than
// the Go value: a zero time.Time would encode as "0001-01-01T00:00:00Z" and the
// tab would render it as a date.
func TestSourcesNeverRanMarshalsNullTimestamps(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetSourceHealth(func(context.Context) ([]SourceHealth, error) {
		return []SourceHealth{{Source: "jsonl", Status: "unknown"}}, nil
	})

	var raw struct {
		Sources []map[string]json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(getOK(t, handler, "/api/sources").Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(raw.Sources))
	}
	for _, field := range []string{"last_success_at", "last_error_at"} {
		if got := string(raw.Sources[0][field]); got != "null" {
			t.Errorf("%s = %s, want null for a source that has never run", field, got)
		}
	}
}

// TestSourcesUnwiredAnswers503: an empty list and an unwired seam are not the
// same thing, and this route is the one that has to tell them apart.
func TestSourcesUnwiredAnswers503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/sources with no seam: status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

// TestSourcesPropagatesASeamError: a runner that cannot answer is a 500 with
// its message, not a 200 with a fabricated "everything is fine".
func TestSourcesPropagatesASeamError(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetSourceHealth(func(context.Context) ([]SourceHealth, error) {
		return nil, errors.New("ingest state is unreadable")
	})

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sources", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unreadable") {
		t.Fatalf("the seam's error was not carried through: %s", rr.Body.String())
	}
}

// TestSourcesRejectsNonGet: every read route goes through methodGet.
func TestSourcesRejectsNonGet(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sources", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/sources: status = %d, want 405", rr.Code)
	}
}
