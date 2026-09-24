package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// br-GI-16-09: `clens archive` and the serve-side scheduler. Temp stores and
// ephemeral ports only.

func seedAgedProxyRow(t *testing.T, st *store.Store, requestID string, age time.Duration) int64 {
	t.Helper()
	return seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: requestID, StartedAt: time.Now().Add(-age), CaptureComplete: true},
		ReqBody:      []byte(`{"model":"m","tools":[]}`), RespBody: []byte(`{"resp":"` + requestID + `"}`),
	})
}

func TestArchiveRequiresAVerb(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runArchive(nil, &buf); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no verb: %v", err)
	}
	if err := runArchive([]string{"bogus"}, &buf); err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("bogus verb: %v", err)
	}
}

func TestArchiveRunGating(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedAgedProxyRow(t, st, "req-run", 10*24*time.Hour)

	var buf bytes.Buffer
	if err := runArchive([]string{"run"}, &buf); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("no --yes must refuse, got %v", err)
	}

	buf.Reset()
	if err := runArchive([]string{"run", "--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "would archive the bodies of 1 row(s)") {
		t.Fatalf("dry-run output: %s", buf.String())
	}
	if _, err := st.GetEvent(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.ArchiveStatus(context.Background()); n.Archived != 0 {
		t.Fatalf("--dry-run archived %d row(s)", n.Archived)
	}

	buf.Reset()
	if err := runArchive([]string{"run", "--yes"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "archived 1 row(s)") {
		t.Fatalf("--yes output: %s", buf.String())
	}
	if s, _ := st.ArchiveStatus(context.Background()); s.Archived != 1 {
		t.Fatalf("status after --yes: %+v", s)
	}
}

func TestArchiveRunIsDisabledAtZeroHotDays(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedAgedProxyRow(t, st, "req-off", 10*24*time.Hour)
	var buf bytes.Buffer
	if err := runArchive([]string{"run", "--yes", "--hot-days", "0"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "disabled") {
		t.Fatalf("output: %s", buf.String())
	}
	if s, _ := st.ArchiveStatus(context.Background()); s.Archived != 0 {
		t.Fatal("hot_days=0 archived a row")
	}
}

func TestArchiveStatusReportsEveryField(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedAgedProxyRow(t, st, "req-a", 10*24*time.Hour)
	seedAgedProxyRow(t, st, "req-b", 1*24*time.Hour)

	var buf bytes.Buffer
	if err := runArchive([]string{"run", "--yes"}, &buf); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := runArchive([]string{"status"}, &buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hot window:   7 day(s)", "1 archived, 1 not archived", "1 day file(s)", "held back:", "missing:      0", "duplicates:   0"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestArchiveRestoreGatingAndRoundTrip(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := archivedProxyRow(t, st, "req-restore", nil, "")
	var buf bytes.Buffer

	if err := runArchive([]string{"restore", "--yes"}, &buf); err == nil || !strings.Contains(err.Error(), "--since") {
		t.Fatalf("restore without a range must refuse: %v", err)
	}
	if err := runArchive([]string{"restore", "--since", "720h"}, &buf); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("restore without --yes must refuse: %v", err)
	}

	buf.Reset()
	if err := runArchive([]string{"restore", "--since", "720h", "--dry-run"}, &buf); err != nil || !strings.Contains(buf.String(), "would restore 1 row(s)") {
		t.Fatalf("dry run: %v %s", err, buf.String())
	}
	if s, _ := st.ArchiveStatus(context.Background()); s.Archived != 1 {
		t.Fatal("dry run restored a row")
	}

	buf.Reset()
	if err := runArchive([]string{"restore", "--since", "720h", "--yes"}, &buf); err != nil {
		t.Fatalf("restore: %v %s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "restored 1 row(s)") {
		t.Fatalf("output: %s", buf.String())
	}
	got, err := st.GetEvent(context.Background(), id)
	if err != nil || got.BodiesArchived != "" || string(got.ReqBody) != `{"model":"m","tools":[]}` {
		t.Fatalf("row after restore: %v %+v", err, got)
	}
}

func TestArchiveRestoreFailureIsReportedAndFails(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	archivedProxyRow(t, st, "req-lost", nil, "")
	files, _ := filepath.Glob(filepath.Join(home, ".clens", "archive", "bodies-*.db"))
	st.Close()
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	err := runArchive([]string{"restore", "--since", "720h", "--yes"}, &buf)
	if err == nil || !strings.Contains(buf.String(), "left event") || !strings.Contains(buf.String(), "day file is missing") {
		t.Fatalf("err %v output %s", err, buf.String())
	}
}

// archiveCycle is the scheduler's body: it archives, is a no-op at hot_days 0,
// and logs rather than failing.
func TestArchiveCycle(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedAgedProxyRow(t, st, "req-cycle", 10*24*time.Hour)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }

	archiveCycle(context.Background(), st, func() int { return 0 }, logf)
	if s, _ := st.ArchiveStatus(context.Background()); s.Archived != 0 || len(logs) != 0 {
		t.Fatalf("hot_days=0: archived %d, logs %v", s.Archived, logs)
	}

	archiveCycle(context.Background(), st, func() int { return 7 }, logf)
	if s, _ := st.ArchiveStatus(context.Background()); s.Archived != 1 {
		t.Fatalf("archived %d, want 1", s.Archived)
	}

	// Fail open: a cancelled context is logged, not fatal.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seedAgedProxyRow(t, st, "req-cycle-2", 10*24*time.Hour)
	logs = nil
	archiveCycle(ctx, st, func() int { return 7 }, logf)
	if len(logs) == 0 {
		t.Fatal("an archiver error was not logged")
	}
}

// In-process serve over an aged store: the archiver runs after the listeners
// are up, and a call-detail fetch still returns the bodies.
func TestServeArchivesAgedRowsAndStillServesBodies(t *testing.T) {
	home := withHome(t)
	db := filepath.Join(home, "lens.db")
	flags := []string{"--proxy-addr", "127.0.0.1:0", "--dashboard-addr", "127.0.0.1:0", "--db-path", db}

	seed, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	id := seedAgedProxyRow(t, seed, "req-serve", 10*24*time.Hour)
	seed.Close()

	done := make(chan error, 1)
	go func() { done <- Serve(flags) }()

	var state serveState
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if s, ok, _ := readServeState(db); ok {
			state = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never came up")
		}
	}

	check, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		s, _ := check.ArchiveStatus(context.Background())
		if s.Archived == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the archiver never ran: %+v", s)
		}
	}

	resp, err := http.Get("http://" + state.DashboardAddr + "/api/requests/" + strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	var ev store.Event
	err = json.NewDecoder(resp.Body).Decode(&ev)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(ev.ReqBody) != `{"model":"m","tools":[]}` || string(ev.RespBody) != `{"resp":"req-serve"}` {
		t.Fatalf("archived bodies not served: req=%q resp=%q archived=%q", ev.ReqBody, ev.RespBody, ev.BodiesArchived)
	}

	r, err := http.Post("http://"+state.DashboardAddr+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}
