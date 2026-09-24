package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// br-GI-16-09: restore and status.

func dayOfEvent(ev *Event) string { return dayOf(ev.StartedAt.UnixNano()) }

// neitherPlace is the state restore must never create: no marker and no hot
// body. (Cannot be told from a never-archived body-less row, so callers use it
// only on rows known to have had bodies.)
func neitherPlace(t *testing.T, st *Store, id int64) bool {
	t.Helper()
	_, marked := markerMask(t, st, id)
	req, resp := hotBodies(t, st, id)
	return !marked && len(req) == 0 && len(resp) == 0
}

func TestRestoreBodiesRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id1, ev1 := agedEvent(t, st, "r1", 10*24*time.Hour)
	id2, ev2 := agedEvent(t, st, "r2", 11*24*time.Hour)
	archiveAll(t, st, 7)

	res, err := st.RestoreBodies(ctx, time.Time{}, time.Time{}, false, nil)
	if err != nil || res.Restored != 2 || len(res.Failed) != 0 {
		t.Fatalf("RestoreBodies = %+v, %v", res, err)
	}
	for id, ev := range map[int64]*Event{id1: ev1, id2: ev2} {
		req, resp := hotBodies(t, st, id)
		if !bytes.Equal(req, ev.ReqBody) || !bytes.Equal(resp, ev.RespBody) {
			t.Fatalf("event %d not restored byte-identically", id)
		}
		if _, marked := markerMask(t, st, id); marked {
			t.Fatalf("event %d still marked", id)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(st.archiveDir, "bodies-*.db")); len(files) != 0 {
		t.Fatalf("emptied day files not removed: %v", files)
	}
}

func TestRestoreBodiesRespectsTheRange(t *testing.T) {
	st := newTestStore(t)
	oldID, _ := agedEvent(t, st, "old", 20*24*time.Hour)
	newID, _ := agedEvent(t, st, "new", 10*24*time.Hour)
	archiveAll(t, st, 7)
	res, err := st.RestoreBodies(context.Background(), archiverNow.Add(-15*24*time.Hour), time.Time{}, false, nil)
	if err != nil || res.Restored != 1 {
		t.Fatalf("RestoreBodies = %+v, %v", res, err)
	}
	if _, marked := markerMask(t, st, newID); marked {
		t.Fatal("in-range row still marked")
	}
	if _, marked := markerMask(t, st, oldID); !marked {
		t.Fatal("out-of-range row was restored")
	}
}

func TestRestoreBodiesDryRunWritesNothing(t *testing.T) {
	st := newTestStore(t)
	id, _ := agedEvent(t, st, "dry", 10*24*time.Hour)
	archiveAll(t, st, 7)
	res, err := st.RestoreBodies(context.Background(), time.Time{}, time.Time{}, true, nil)
	if err != nil || res.Restored != 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if _, marked := markerMask(t, st, id); !marked {
		t.Fatal("dry run dropped a marker")
	}
	if req, _ := hotBodies(t, st, id); len(req) != 0 {
		t.Fatal("dry run wrote a hot body")
	}
}

// Restore writes back exactly the columns the day row's mask holds.
func TestRestoreBodiesPartialMask(t *testing.T) {
	st := newTestStore(t)
	id, ev := agedEvent(t, st, "partial", 10*24*time.Hour)
	day := dayOfEvent(ev)
	if err := st.writeDayRows(day, []archiveRow{{EventID: id, ReqBody: ev.ReqBody}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO body_archive (event_id, day, archived_at, body_mask) VALUES (?, ?, 1, ?)`, id, day, MaskReqBody); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE events SET req_body = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if res, err := st.RestoreBodies(context.Background(), time.Time{}, time.Time{}, false, nil); err != nil || res.Restored != 1 {
		t.Fatalf("RestoreBodies = %+v, %v", res, err)
	}
	req, resp := hotBodies(t, st, id)
	if !bytes.Equal(req, ev.ReqBody) || !bytes.Equal(resp, ev.RespBody) {
		t.Fatalf("partial restore changed the wrong columns: req %d bytes, resp %d bytes", len(req), len(resp))
	}
}

// A crash after the hot commit but before the day-row delete leaves a duplicate:
// the body is hot, the marker is gone, and nothing is lost or collected.
func TestRestoreCrashBeforeDayRowDeleteLeavesADuplicate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, ev := agedEvent(t, st, "crash", 10*24*time.Hour)
	archiveAll(t, st, 7)

	_, err := st.RestoreBodies(ctx, time.Time{}, time.Time{}, false, func(step int) error {
		if step == 1 {
			return errCrash
		}
		return nil
	})
	if !errors.Is(err, errCrash) {
		t.Fatalf("err = %v, want the injected crash", err)
	}
	req, resp := hotBodies(t, st, id)
	if !bytes.Equal(req, ev.ReqBody) || !bytes.Equal(resp, ev.RespBody) {
		t.Fatal("body not hot after the hot commit")
	}
	if _, marked := markerMask(t, st, id); marked {
		t.Fatal("marker survived the hot commit")
	}
	s, err := st.ArchiveStatus(ctx)
	if err != nil || s.Duplicates != 1 {
		t.Fatalf("status = %+v, %v; want 1 duplicate", s, err)
	}
	gc, err := st.GCArchive(ctx)
	if err != nil || gc.RowsDeleted != 0 {
		t.Fatalf("GCArchive = %+v, %v; a duplicate must not be collected", gc, err)
	}
	wantRecoverable(t, st, id, ev)
}

// The day row is deleted strictly after the hot commit.
func TestRestoreDeletesTheDayRowAfterTheHotCommit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, _ := agedEvent(t, st, "order", 10*24*time.Hour)
	day := dayOf(mustStartedAt(t, st, id))
	archiveAll(t, st, 7)

	var seen []string
	_, err := st.RestoreBodies(ctx, time.Time{}, time.Time{}, false, func(step int) error {
		_, marked := markerMask(t, st, id)
		rows, _ := st.dayRowIDs(ctx, day)
		seen = append(seen, fmt.Sprintf("step%d marker=%v dayrow=%v", step, marked, rows[id]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"step1 marker=false dayrow=true", "step2 marker=false dayrow=false"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", seen, want)
	}
}

func mustStartedAt(t *testing.T, st *Store, id int64) int64 {
	t.Helper()
	var ns int64
	if err := st.db.QueryRow(`SELECT started_at FROM events WHERE id = ?`, id).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	return ns
}

func TestRestoreLeavesUndecodableRowsUntouched(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, ev := agedEvent(t, st, "corrupt", 10*24*time.Hour)
	archiveAll(t, st, 7)

	path := st.dayPath(dayOfEvent(ev))
	st.arch.evict(path)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE bodies SET req_len = req_len + 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	res, err := st.RestoreBodies(ctx, time.Time{}, time.Time{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 0 || len(res.Failed) != 1 {
		t.Fatalf("res = %+v, want one failure", res)
	}
	if _, marked := markerMask(t, st, id); !marked {
		t.Fatal("marker dropped for a body that was not restored")
	}
	if req, resp := hotBodies(t, st, id); len(req) != 0 || len(resp) != 0 {
		t.Fatal("hot row changed by a failed restore")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("day file removed despite a failed restore: %v", err)
	}
}

func TestRestoreMissingDayFileLeavesMarkerIntact(t *testing.T) {
	st := newTestStore(t)
	id, ev := agedEvent(t, st, "gone", 10*24*time.Hour)
	archiveAll(t, st, 7)
	path := st.dayPath(dayOfEvent(ev))
	st.arch.evict(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	res, err := st.RestoreBodies(context.Background(), time.Time{}, time.Time{}, false, nil)
	if err != nil || len(res.Failed) != 1 {
		t.Fatalf("res = %+v, %v; want one failure", res, err)
	}
	if _, marked := markerMask(t, st, id); !marked {
		t.Fatal("marker dropped though the archive copy is gone")
	}
	s, _ := st.ArchiveStatus(context.Background())
	if s.Missing != 1 {
		t.Fatalf("status.Missing = %d, want 1", s.Missing)
	}
}

func TestArchiveStatusCounts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if s, err := st.ArchiveStatus(ctx); err != nil || s != (ArchiveStatus{}) {
		t.Fatalf("empty status = %+v, %v", s, err)
	}
	agedEvent(t, st, "a", 10*24*time.Hour)
	agedEvent(t, st, "b", 1*24*time.Hour)
	archiveAll(t, st, 7)
	s, err := st.ArchiveStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Events != 2 || s.Archived != 1 || s.DayFiles != 1 || s.DirBytes <= 0 || s.Missing != 0 || s.Duplicates != 0 {
		t.Fatalf("status = %+v", s)
	}
	n, err := st.CountArchivable(ctx, 7, archiverNow)
	if err != nil || n != 0 {
		t.Fatalf("CountArchivable after a run = %d, %v; want 0", n, err)
	}
	n, _ = st.CountArchivable(ctx, 0, archiverNow)
	if n != 0 {
		t.Fatal("hot_days 0 must count nothing")
	}
}
