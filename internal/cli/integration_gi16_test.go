package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// br-GI-16-12: behaviours no single bead owns. Temp dirs and free ports only --
// never the live 8797/8798 or the operator's lens.db.

func getJSON(t *testing.T, url string, into any) []byte {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s: %s", url, resp.Status, b)
	}
	if into != nil {
		if err := json.Unmarshal(b, into); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// waitServeState waits for serve's state file, which is written only after both
// listeners are bound, so its presence means the ports are up.
func waitServeState(t *testing.T, db string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok, _ := readServeState(db); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never came up")
		}
	}
}

func stopServe(t *testing.T, dash string, done <-chan error) {
	t.Helper()
	r, err := http.Post("http://"+dash+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// Archival must not change a single aggregate, must serve identical bodies,
// must be undone byte-for-byte by restore, and must leave nothing behind after
// a purge.
func TestArchivalEndToEnd(t *testing.T) {
	home := withHome(t)
	db := filepath.Join(home, "lens.db")
	dash, proxy := freeAddr(t), freeAddr(t)
	flags := []string{"--proxy-addr", proxy, "--dashboard-addr", dash, "--db-path", db, "--hot-days", "0"}

	// Mixed body shapes, all aged past the 7-day window, plus one recent row.
	type shape struct {
		name       string
		req, resp  []byte
		transcript []byte
	}
	shapes := []shape{
		{"all-three", []byte(`{"tools":[],"m":"SENTINEL-REQ-1"}`), []byte(`{"r":"SENTINEL-RESP-1"}`), []byte("SENTINEL-TC-1")},
		{"req-only", []byte(`{"tools":[],"m":"SENTINEL-REQ-2"}`), nil, nil},
		{"empty-resp", []byte(`{"tools":[]}`), []byte{}, nil},
		{"null", nil, nil, nil},
	}
	seed, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ids := map[string]int64{}
	for i, sh := range shapes {
		id := seedProxyRow(t, seed, &store.Event{
			EventSummary: store.EventSummary{
				RequestID: "req-" + sh.name, StartedAt: time.Now().Add(-time.Duration(10+i) * 24 * time.Hour),
				SessionID: "sess-1", InputTokens: 10 + i, OutputTokens: 5, CaptureComplete: true,
			},
			ReqBody: sh.req, RespBody: sh.resp, TranscriptContent: sh.transcript,
		})
		ids[sh.name] = id
	}
	seedProxyRow(t, seed, &store.Event{
		EventSummary: store.EventSummary{RequestID: "req-recent", StartedAt: time.Now().Add(-time.Hour), SessionID: "sess-1", InputTokens: 1, CaptureComplete: true},
		ReqBody:      []byte(`{"tools":[]}`), RespBody: []byte(`{"ok":true}`),
	})
	if err := seed.UpsertSession(ctx, "sess-1", "", time.Now().Add(-11*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := seed.ReconcileSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	seed.Close()

	done := make(chan error, 1)
	go func() { done <- Serve(flags) }()
	t.Cleanup(func() {
		if healthy(dash) { // a failed assertion must not leave serve holding the DB
			if r, err := http.Post("http://"+dash+"/api/shutdown", "application/json", nil); err == nil {
				r.Body.Close()
			}
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
		}
	})
	waitServeState(t, db)

	// Fixed bounds, so the response has nothing that depends on when it is read.
	q := "?since=" + time.Now().Add(-60*24*time.Hour).UTC().Format(time.RFC3339) +
		"&until=" + time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339)
	aggregates := func() map[string]json.RawMessage {
		out := map[string]json.RawMessage{}
		for _, path := range []string{"/api/stats" + q, "/api/sessions"} {
			out[path] = getJSON(t, "http://"+dash+path, nil)
		}
		return out
	}
	before := aggregates()

	var buf bytes.Buffer
	if err := runArchive([]string{"run", "--yes", "--hot-days", "7", "--db-path", db}, &buf); err != nil {
		t.Fatalf("archive run: %v", err)
	}
	if !strings.Contains(buf.String(), "archived 3 row(s)") {
		t.Fatalf("expected the three body-bearing aged rows archived, got %q", buf.String())
	}

	if after := aggregates(); !reflect.DeepEqual(before, after) {
		t.Fatalf("aggregates changed across archival:\nbefore %s\nafter  %s", before, after)
	}

	for _, sh := range shapes {
		var ev store.Event
		getJSON(t, "http://"+dash+"/api/requests/"+strconv.FormatInt(ids[sh.name], 10), &ev)
		if !bytes.Equal(ev.ReqBody, sh.req) || !bytes.Equal(ev.RespBody, sh.resp) || !bytes.Equal(ev.TranscriptContent, sh.transcript) {
			t.Fatalf("%s: bodies differ after archival: req=%q resp=%q tc=%q", sh.name, ev.ReqBody, ev.RespBody, ev.TranscriptContent)
		}
		wantState := "restored"
		if sh.name == "null" {
			wantState = ""
		}
		if ev.BodiesArchived != wantState {
			t.Fatalf("%s: BodiesArchived = %q, want %q", sh.name, ev.BodiesArchived, wantState)
		}
	}
	stopServe(t, dash, done)

	// Restore returns every body to its hot row byte-identically.
	if err := runArchive([]string{"restore", "--since", "720h", "--yes", "--db-path", db}, &buf); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, sh := range shapes {
		got, err := st.GetEvent(ctx, ids[sh.name])
		if err != nil {
			t.Fatal(err)
		}
		if got.BodiesArchived != "" || !bytes.Equal(got.ReqBody, sh.req) || !bytes.Equal(got.RespBody, sh.resp) {
			t.Fatalf("%s: restore was not byte-identical: %+v", sh.name, got)
		}
	}
	if s, _ := st.ArchiveStatus(ctx); s.Archived != 0 || s.DayFiles != 0 {
		t.Fatalf("restore left archive state behind: %+v", s)
	}

	// Archive again, then purge: nothing of the sentinels may remain in archive/.
	if err := runArchive([]string{"run", "--yes", "--hot-days", "7", "--db-path", db}, &buf); err != nil {
		t.Fatal(err)
	}
	if err := runPurge([]string{"--older-than", "1d", "--yes", "--db-path", db}, &buf); err != nil {
		t.Fatalf("purge: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(home, "archive", "bodies-*.db*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("SENTINEL")) {
			t.Fatalf("%s still holds purged body bytes", f)
		}
	}
	if len(files) != 0 {
		t.Fatalf("purge left day files behind: %v", files)
	}
}

// Restart composed with a real serve: the old serve is stopped and the new one
// comes up on the same ports; then an unhealthy replacement rolls back.
func TestRestartAndRollbackWithARealServe(t *testing.T) {
	home := withHome(t)
	db := filepath.Join(home, "lens.db")
	dash, proxy := freeAddr(t), freeAddr(t)
	flags := []string{"--proxy-addr", proxy, "--dashboard-addr", dash, "--db-path", db}

	var runs []chan error
	start := func() {
		d := make(chan error)
		runs = append(runs, d)
		go func() { _ = Serve(flags); close(d) }() // closed, so every waiter sees it
	}
	start()
	waitServeState(t, db)
	t.Cleanup(func() {
		if healthy(dash) {
			r, err := http.Post("http://"+dash+"/api/shutdown", "application/json", nil)
			if err == nil {
				r.Body.Close()
			}
			for _, d := range runs {
				select {
				case <-d:
				case <-time.After(15 * time.Second):
				}
			}
		}
	})
	state, ok, err := readServeState(db)
	if err != nil || !ok {
		t.Fatalf("no state file from a real serve: %v %v", ok, err)
	}

	spawn := func(exe string, args []string, logPath string) (int, func(), error) {
		if exe != "bad-exe" {
			start()
		}
		return 4242, func() {}, nil
	}

	var out bytes.Buffer
	if err := runRestart(append([]string{"--timeout", "20s"}, flags...), &out, spawn); err != nil {
		t.Fatalf("restart: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "proxy gap") || !healthy(dash) {
		t.Fatalf("restart did not report a gap or serve is not up:\n%s", out.String())
	}
	<-runs[0] // the first serve exited

	out.Reset()
	err = runRestart(append([]string{"--exe", "bad-exe", "--timeout", "1s"}, flags...), &out, spawn)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("an unhealthy replacement must roll back and fail, got %v\n%s", err, out.String())
	}
	if !healthy(dash) {
		t.Fatal("nothing is serving after the rollback")
	}
	if state.Exe == "" {
		t.Fatal("the state file recorded no exe to roll back to")
	}
}

// The gated check for the 4 -> 5 migration: point CLENS_LIVE_COPY_DB at a COPY
// of a real lens.db (with its -wal/-shm), never the live file.
func TestMigrationOnACopyOfTheLiveStore(t *testing.T) {
	path := os.Getenv("CLENS_LIVE_COPY_DB")
	if path == "" {
		t.Skip("CLENS_LIVE_COPY_DB not set; this check runs against a copy of a real store only")
	}
	// Before Open migrates anything: the schema version and the counts to preserve.
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	var preVersion, preEvents, preSessions int
	raw.QueryRow("PRAGMA user_version").Scan(&preVersion)
	raw.QueryRow("SELECT COUNT(*) FROM events").Scan(&preEvents)
	raw.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&preSessions)
	var preCols []string
	rows, _ := raw.Query("SELECT name FROM pragma_table_info('events') ORDER BY cid")
	for rows.Next() {
		var n string
		rows.Scan(&n)
		preCols = append(preCols, n)
	}
	rows.Close()
	raw.Close()
	t.Logf("before: user_version=%d events=%d sessions=%d", preVersion, preEvents, preSessions)

	start := time.Now()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	events, err := st.CountEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("opened and migrated the copy in %s (%d events)", time.Since(start), events)
	if events != preEvents {
		t.Fatalf("event count changed across the migration: %d -> %d", preEvents, events)
	}
	raw2, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer raw2.Close()
	var postCols []string
	crs, err := raw2.Query("SELECT name FROM pragma_table_info('events') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	for crs.Next() {
		var n string
		crs.Scan(&n)
		postCols = append(postCols, n)
	}
	crs.Close()
	if !reflect.DeepEqual(preCols, postCols) {
		t.Fatalf("events columns changed:\nbefore %v\nafter  %v", preCols, postCols)
	}
	s, err := st.ArchiveStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Archived != 0 || s.DayFiles != 0 {
		t.Fatalf("a freshly migrated store must have an empty archive: %+v", s)
	}
	if got, err := st.UserVersion(context.Background()); err != nil || got != 5 {
		t.Fatalf("user_version = %d, %v; want 5", got, err)
	}
}
