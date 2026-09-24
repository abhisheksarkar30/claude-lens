package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// br-GI-16-08: the writers that must stay correct once a row's bodies live in
// a day file rather than the hot columns.

func archiveAll(t *testing.T, st *Store, hot int) {
	t.Helper()
	if _, err := newArchiver(st, hot).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A merge into an archived row must not treat its emptied body columns as
// absent: the archived bodies stay authoritative, and only the genuinely
// missing transcript is backfilled from the incoming side.
func TestMergeIntoArchivedRowKeepsArchivedBodies(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, orig := agedEvent(t, st, "merge-arch", 10*24*time.Hour)
	archiveAll(t, st, 7)
	if req, _ := hotBodies(t, st, id); len(req) != 0 {
		t.Fatal("precondition: req_body should be archived out of the hot row")
	}

	in := fullEvent("merge-arch")
	in.Source, in.FirstSource = "jsonl", "jsonl"
	in.StartedAt = orig.StartedAt
	in.ReqBody = []byte("INCOMING-REQ")
	in.RespBody = []byte("INCOMING-RESP")
	in.TranscriptContent = []byte("new transcript")
	gotID, _, err := st.InsertEvent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id {
		t.Fatalf("expected a merge into %d, got id=%d", id, gotID)
	}

	req, resp := hotBodies(t, st, id)
	if len(req) != 0 || len(resp) != 0 {
		t.Fatalf("merge backfilled a body over an archived one: req=%q resp=%q", req, resp)
	}
	wantRecoverable(t, st, id, orig)
	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.TranscriptContent) != "new transcript" {
		t.Fatalf("transcript not backfilled: %q", got.TranscriptContent)
	}
	if !got.HasReqBody {
		t.Fatal("req_tool_names must stay non-NULL for an archived request body")
	}
}

func TestPurgeCollectsArchivedBodies(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// Two events on the same UTC day, purge cutoff between them.
	aID, _ := agedEvent(t, st, "purge-a", 10*24*time.Hour)
	_, _ = agedEvent(t, st, "purge-b", 10*24*time.Hour-2*time.Hour)
	archiveAll(t, st, 7)
	_ = aID

	n, err := st.PurgeOlderThan(ctx, archiverNow.Add(-10*24*time.Hour+time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PurgeOlderThan = %d, %v; want 1", n, err)
	}
	files, _ := filepath.Glob(filepath.Join(st.archiveDir, "bodies-*.db"))
	if len(files) != 1 {
		t.Fatalf("want 1 day file, got %v", files)
	}
	var rows int
	if err := st.arch.with(files[0], func(db *sql.DB) error {
		return db.QueryRow(`SELECT COUNT(*) FROM bodies`).Scan(&rows)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("day file holds %d rows after purge, want 1 (the survivor)", rows)
	}

	// Purging the survivor too removes the file entirely.
	if _, err := st.PurgeOlderThan(ctx, archiverNow); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(st.archiveDir, "bodies-*.db")); len(files) != 0 {
		t.Fatalf("empty day file not removed: %v", files)
	}
}

func TestPurgeUnpricedAndJSONLDeleteCollectArchive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, _ := agedEvent(t, st, "unpriced", 10*24*time.Hour)
	if _, err := st.db.Exec(`UPDATE events SET cost_source = 'unpriced' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	archiveAll(t, st, 7)
	if _, err := st.PurgeUnpriced(ctx); err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(st.archiveDir, "bodies-*.db")); len(files) != 0 {
		t.Fatalf("PurgeUnpriced left a day file: %v", files)
	}

	jid, _ := agedEvent(t, st, "jsonl:keyed", 10*24*time.Hour)
	if _, err := st.db.Exec(`UPDATE events SET source = 'jsonl' WHERE id = ?`, jid); err != nil {
		t.Fatal(err)
	}
	archiveAll(t, st, 7)
	if n, err := st.DeleteJSONLKeyedEvents(ctx); err != nil || n != 1 {
		t.Fatalf("DeleteJSONLKeyedEvents = %d, %v", n, err)
	}
	if files, _ := filepath.Glob(filepath.Join(st.archiveDir, "bodies-*.db")); len(files) != 0 {
		t.Fatalf("DeleteJSONLKeyedEvents left a day file: %v", files)
	}
}

func TestPurgeableBytesCountsArchivedAndSkipsMissingDays(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, ev := agedEvent(t, st, "bytes", 10*24*time.Hour)
	want := int64(len(ev.ReqBody) + len(ev.RespBody))
	cutoff := archiverNow.Add(-24 * time.Hour)

	hot, skipped, err := st.PurgeableBytes(ctx, cutoff)
	if err != nil || skipped != 0 || hot != want {
		t.Fatalf("hot PurgeableBytes = %d,%d,%v; want %d,0,nil", hot, skipped, err, want)
	}

	archiveAll(t, st, 7)
	got, skipped, err := st.PurgeableBytes(ctx, cutoff)
	if err != nil || skipped != 0 || got != want {
		t.Fatalf("archived PurgeableBytes = %d,%d,%v; want %d,0,nil", got, skipped, err, want)
	}

	files, _ := filepath.Glob(filepath.Join(st.archiveDir, "bodies-*.db"))
	st.arch.evict(files[0])
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	got, skipped, err = st.PurgeableBytes(ctx, cutoff)
	if err != nil || skipped != 1 || got != 0 {
		t.Fatalf("missing-file PurgeableBytes = %d,%d,%v; want 0,1,nil", got, skipped, err)
	}
}

// Rekey pass 1 must skip a marker-bearing row rather than re-key or absorb it:
// its resp_body is not in the hot row, and a merge could not carry the archive.
func TestRekeyPass1SkipsArchivedRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, _ := agedEvent(t, st, "proxy:archived", 10*24*time.Hour)
	hdr := `{"Content-Type":["application/json"]}`
	body := []byte(`{"id":"msg_taken","type":"message","model":"m","usage":{"input_tokens":1,"output_tokens":1}}`)
	if _, err := st.db.Exec(`UPDATE events SET resp_body = ?, resp_headers = ? WHERE id = ?`, body, hdr, id); err != nil {
		t.Fatal(err)
	}
	// A taker holding the target key, so an unguarded pass would merge.
	taker := fullEvent("msg_taken")
	taker.Source, taker.FirstSource = "jsonl", "jsonl"
	if _, _, err := st.InsertEvent(ctx, taker); err != nil {
		t.Fatal(err)
	}
	archiveAll(t, st, 7)

	rep, err := st.RekeyProxyBodyIDs(ctx, 1<<20, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.ReKeyed != 0 || rep.Synthetic != 0 {
		t.Fatalf("report = %+v; want Skipped=1 only", rep)
	}
	got, err := st.GetEvent(ctx, id)
	if err != nil || got.RequestID != "proxy:archived" || !bytes.Equal(got.RespBody, body) {
		t.Fatalf("archived row was touched: %v %+v", err, got)
	}
	res, err := st.GCArchive(ctx)
	if err != nil || res.RowsDeleted != 0 || res.FilesRemoved != 0 {
		t.Fatalf("GCArchive after skip = %+v, %v; want nothing collected", res, err)
	}
}

func TestReflagExcludesArchivedRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	ev := reflagRow("reflag-arch", true, []byte("0123456789"), "9999")
	ev.StartedAt = archiverNow.Add(-10 * 24 * time.Hour)
	if _, _, err := st.InsertEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	archiveAll(t, st, 7)
	c, err := st.ReflagIncompleteCaptures(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Flipped != 0 || c.Archived != 1 {
		t.Fatalf("counts = %+v; want Flipped 0, Archived 1", c)
	}
}

func TestToolNamesBackfillNeverSeesArchivedRows(t *testing.T) {
	st := newTestStore(t)
	agedEvent(t, st, "tn", 10*24*time.Hour)
	archiveAll(t, st, 7)
	rows, err := st.EventsAwaitingToolNamesBackfill(context.Background(), 0, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("backfill saw %d archived row(s): %v", len(rows), err)
	}
	if n, _ := st.CountEventsAwaitingToolNamesBackfill(context.Background()); n != 0 {
		t.Fatalf("backfill count = %d, want 0", n)
	}
}
