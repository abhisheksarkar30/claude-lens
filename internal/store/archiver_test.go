package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var archiverNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

var errCrash = errors.New("injected crash")

// agedEvent inserts an event started age ago, with distinctive bodies so a
// mix-up between events or columns is visible.
func agedEvent(t *testing.T, st *Store, name string, age time.Duration) (int64, *Event) {
	t.Helper()
	ev := fullEvent(name)
	ev.StartedAt = archiverNow.Add(-age)
	ev.ReqBody = []byte(`{"req":"` + name + strings.Repeat("-r", 300) + `"}`)
	ev.RespBody = []byte(`{"resp":"` + name + strings.Repeat("-s", 300) + `"}`)
	ev.TranscriptContent = nil
	id, _, err := st.InsertEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	return id, ev
}

func newArchiver(st *Store, hot int) *Archiver {
	a := st.NewArchiver(func() int { return hot })
	a.Now = func() time.Time { return archiverNow }
	a.Pause = 0
	return a
}

// hotBodies reads the raw hot columns, bypassing hydration.
func hotBodies(t *testing.T, st *Store, id int64) (req, resp []byte) {
	t.Helper()
	if err := st.db.QueryRow("SELECT req_body, resp_body FROM events WHERE id = ?", id).Scan(&req, &resp); err != nil {
		t.Fatal(err)
	}
	return
}

func markerMask(t *testing.T, st *Store, id int64) (int64, bool) {
	t.Helper()
	var m int64
	err := st.db.QueryRow("SELECT body_mask FROM body_archive WHERE event_id = ?", id).Scan(&m)
	return m, err == nil
}

// wantRecoverable is the never-lose-a-body property: whatever state the store
// is in, the row reads back with both original bodies.
func wantRecoverable(t *testing.T, st *Store, id int64, ev *Event) {
	t.Helper()
	got, err := st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ReqBody, ev.ReqBody) || !bytes.Equal(got.RespBody, ev.RespBody) {
		t.Fatalf("event %d lost a body (archived=%q)", id, got.BodiesArchived)
	}
}

func TestArchiverMovesAgedRowsAndLeavesRecentOnes(t *testing.T) {
	st := newTestStore(t)
	oldID, oldEv := agedEvent(t, st, "old", 10*24*time.Hour)
	newID, _ := agedEvent(t, st, "recent", 24*time.Hour)

	res, err := newArchiver(st, 7).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived != 1 {
		t.Fatalf("archived %d, want 1", res.Archived)
	}
	if req, resp := hotBodies(t, st, oldID); req != nil || resp != nil {
		t.Error("archived row still holds bodies in events")
	}
	if m, ok := markerMask(t, st, oldID); !ok || m != 3 {
		t.Errorf("marker = %d/%v, want mask 3", m, ok)
	}
	wantRecoverable(t, st, oldID, oldEv)
	if req, _ := hotBodies(t, st, newID); req == nil {
		t.Error("a recent row was archived")
	}
	if _, ok := markerMask(t, st, newID); ok {
		t.Error("a recent row was marked")
	}

	// Re-running is idempotent.
	if res, _ := newArchiver(st, 7).Run(context.Background()); res.Archived != 0 {
		t.Errorf("second run archived %d, want 0", res.Archived)
	}
}

func TestArchiverHotDaysBoundary(t *testing.T) {
	st := newTestStore(t)
	exact, _ := agedEvent(t, st, "exact", 7*24*time.Hour)
	older, _ := agedEvent(t, st, "older", 7*24*time.Hour+time.Nanosecond)
	if _, err := newArchiver(st, 7).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := markerMask(t, st, exact); ok {
		t.Error("a row at exactly now - HotDays was archived; the cutoff is strict")
	}
	if _, ok := markerMask(t, st, older); !ok {
		t.Error("a row one ns past the cutoff was not archived")
	}
}

func TestArchiverDisabledAtZeroHotDays(t *testing.T) {
	st := newTestStore(t)
	id, _ := agedEvent(t, st, "old", 400*24*time.Hour)
	if res, err := newArchiver(st, 0).Run(context.Background()); err != nil || res.Archived != 0 {
		t.Fatalf("HotDays 0: %+v %v, want nothing archived", res, err)
	}
	if req, _ := hotBodies(t, st, id); req == nil {
		t.Error("HotDays 0 archived a row")
	}
}

func TestArchiverSkipsAndCountsRowsAwaitingBackfill(t *testing.T) {
	st := newTestStore(t)
	id, ev := agedEvent(t, st, "await", 10*24*time.Hour)
	if _, err := st.db.Exec("UPDATE events SET req_tool_names = NULL WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	res, err := newArchiver(st, 7).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Archived != 0 || res.SkippedBackfill != 1 {
		t.Fatalf("result %+v, want 0 archived and 1 skipped", res)
	}
	wantRecoverable(t, st, id, ev)
	if _, ok := markerMask(t, st, id); ok {
		t.Error("a row awaiting backfill was archived, which would strand it")
	}
}

// Fault injection after every step: each body stays recoverable from one side,
// and re-running finishes the job.
func TestArchiverFaultInjectionAfterEachStep(t *testing.T) {
	for step := 1; step <= 4; step++ {
		t.Run(fmt.Sprintf("after step %d", step), func(t *testing.T) {
			st := newTestStore(t)
			var ids []int64
			var evs []*Event
			for i := 0; i < 3; i++ {
				id, ev := agedEvent(t, st, fmt.Sprintf("e%d", i), 10*24*time.Hour+time.Duration(i)*time.Minute)
				ids, evs = append(ids, id), append(evs, ev)
			}
			a := newArchiver(st, 7)
			a.AfterStep = func(n int) error {
				if n == step {
					return errCrash
				}
				return nil
			}
			if _, err := a.Run(context.Background()); !errors.Is(err, errCrash) {
				t.Fatalf("Run = %v, want the injected crash", err)
			}
			for i, id := range ids {
				wantRecoverable(t, st, id, evs[i])
			}

			res, err := newArchiver(st, 7).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if step == 4 && res.Archived != 0 {
				t.Errorf("after a crash following step 4 the rerun archived %d, want 0", res.Archived)
			}
			for i, id := range ids {
				wantRecoverable(t, st, id, evs[i])
				if req, _ := hotBodies(t, st, id); req != nil {
					t.Errorf("event %d still hot after the recovery run", id)
				}
				if m, ok := markerMask(t, st, id); !ok || m != 3 {
					t.Errorf("event %d marker = %d/%v, want 3", id, m, ok)
				}
			}
			if res, _ := newArchiver(st, 7).Run(context.Background()); res.Archived != 0 {
				t.Errorf("a further run archived %d, want idempotence", res.Archived)
			}
		})
	}
}

// Two archivers with different masks: A reads only req_body, then a backfill
// lands resp_body and B archives both. A's narrower commit must not drop
// anything, and neither the file's mask nor the marker's may regress.
func TestTwoArchiversWithDifferentMasksLeaveTheUnion(t *testing.T) {
	st := newTestStore(t)
	id, ev := agedEvent(t, st, "two", 10*24*time.Hour)
	if _, err := st.db.Exec("UPDATE events SET resp_body = NULL WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}

	a := newArchiver(st, 7)
	ran := false
	a.AfterStep = func(n int) error {
		if n != 1 || ran {
			return nil
		}
		ran = true
		if _, err := st.db.Exec("UPDATE events SET resp_body = ? WHERE id = ?", ev.RespBody, id); err != nil { // the backfill
			return err
		}
		if _, err := newArchiver(st, 7).Run(context.Background()); err != nil { // archiver B, wider
			return err
		}
		return nil
	}
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := readOneDayRow(t, st, dayOf(ev.StartedAt.UnixNano()), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mask != 3 || !bytes.Equal(got.Resp, ev.RespBody) || !bytes.Equal(got.Req, ev.ReqBody) {
		t.Fatalf("file mask = %d, want the union 3 with both bodies", got.Mask)
	}
	if m, _ := markerMask(t, st, id); m != 3 {
		t.Errorf("marker mask = %d, want it never below the wider archiver's 3", m)
	}
	wantRecoverable(t, st, id, ev)
}

// Step 4 NULLs only what the file covers: a body backfilled between the copy
// and the clear stays hot.
func TestStepFourClearsOnlyMaskedColumns(t *testing.T) {
	st := newTestStore(t)
	id, ev := agedEvent(t, st, "late", 10*24*time.Hour)
	if _, err := st.db.Exec("UPDATE events SET resp_body = NULL WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	a := newArchiver(st, 7)
	a.AfterStep = func(n int) error {
		if n == 3 { // file committed with req only; now a backfill writes resp_body
			_, err := st.db.Exec("UPDATE events SET resp_body = ? WHERE id = ?", ev.RespBody, id)
			return err
		}
		return nil
	}
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, resp := hotBodies(t, st, id)
	if req != nil {
		t.Error("the archived req_body is still hot")
	}
	if !bytes.Equal(resp, ev.RespBody) {
		t.Error("a body the file does not hold was cleared")
	}
	wantRecoverable(t, st, id, ev)
}

func TestLateRowIntoAnArchivedDayUpserts(t *testing.T) {
	st := newTestStore(t)
	id1, ev1 := agedEvent(t, st, "first", 10*24*time.Hour)
	if _, err := newArchiver(st, 7).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	id2, ev2 := agedEvent(t, st, "second", 10*24*time.Hour+time.Second)
	if res, err := newArchiver(st, 7).Run(context.Background()); err != nil || res.Archived != 1 {
		t.Fatalf("late row: %+v %v", res, err)
	}
	wantRecoverable(t, st, id1, ev1)
	wantRecoverable(t, st, id2, ev2)
}

func TestArchiverBatchCapsAndDays(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 45; i++ {
		agedEvent(t, st, fmt.Sprintf("b%02d", i), 10*24*time.Hour+time.Duration(i)*time.Second)
	}
	res, err := newArchiver(st, 7).Run(context.Background())
	if err != nil || res.Archived != 45 || res.Batches != 3 {
		t.Fatalf("%+v %v, want 45 rows in 3 batches of at most 20", res, err)
	}

	// The byte cap closes a batch early; an oversized row still gets its own.
	st2 := newTestStore(t)
	for i := 0; i < 6; i++ {
		agedEvent(t, st2, fmt.Sprintf("c%d", i), 10*24*time.Hour+time.Duration(i)*time.Second)
	}
	a := newArchiver(st2, 7)
	a.BatchBytes = 1500 // each row is ~1.2 KB, so one per batch
	if res, err := a.Run(context.Background()); err != nil || res.Archived != 6 || res.Batches != 6 {
		t.Fatalf("%+v %v, want 6 batches under the byte cap", res, err)
	}

	// A batch never spans two UTC days.
	st3 := newTestStore(t)
	agedEvent(t, st3, "d1", 10*24*time.Hour)
	agedEvent(t, st3, "d2", 11*24*time.Hour)
	if res, _ := newArchiver(st3, 7).Run(context.Background()); res.Batches != 2 {
		t.Errorf("two days ran in %d batch(es), want 2", res.Batches)
	}
}

func TestCandidateQueryUsesTheStartedAtIndexAndNoScan(t *testing.T) {
	st := newTestStore(t)
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+archiveCandidateSQL, int64(0), int64(0), int64(0), int64(1<<62), 20)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_events_started_at") {
		t.Errorf("candidate query does not use idx_events_started_at:\n%s", joined)
	}
	if strings.Contains(joined, "SCAN events") {
		t.Errorf("candidate query scans the events table:\n%s", joined)
	}
}

func TestArchiverCancelledBetweenBatchesLeavesAConsistentStore(t *testing.T) {
	st := newTestStore(t)
	var ids []int64
	var evs []*Event
	for i := 0; i < 30; i++ {
		id, ev := agedEvent(t, st, fmt.Sprintf("x%02d", i), 10*24*time.Hour+time.Duration(i)*time.Second)
		ids, evs = append(ids, id), append(evs, ev)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := newArchiver(st, 7)
	a.AfterStep = func(n int) error {
		if n == 4 {
			cancel()
		}
		return nil
	}
	res, err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if res.Archived != 20 {
		t.Errorf("archived %d before the cancel, want the one full batch of 20", res.Archived)
	}
	for i, id := range ids {
		wantRecoverable(t, st, id, evs[i])
	}
}

func TestGCArchive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, ev := agedEvent(t, st, "keep", 10*24*time.Hour)
	if _, err := newArchiver(st, 7).Run(ctx); err != nil {
		t.Fatal(err)
	}
	day := dayOf(ev.StartedAt.UnixNano())

	// A marker-bearing row is spared.
	if res, err := st.GCArchive(ctx); err != nil || res.RowsDeleted != 0 {
		t.Fatalf("GC of a live archive: %+v %v, want nothing deleted", res, err)
	}
	wantRecoverable(t, st, id, ev)

	// A true orphan (no such event) is collected.
	if err := st.writeDayRows(day, []archiveRow{{EventID: 777_777, ReqBody: []byte("orphan")}}); err != nil {
		t.Fatal(err)
	}
	if res, _ := st.GCArchive(ctx); res.RowsDeleted != 1 {
		t.Fatalf("orphan not collected: %+v", res)
	}
	wantRecoverable(t, st, id, ev)

	// After the event is purged its copy goes, and the empty file with it.
	if _, err := st.db.Exec("DELETE FROM events WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	res, err := st.GCArchive(ctx)
	if err != nil || res.RowsDeleted != 1 || res.FilesRemoved != 1 {
		t.Fatalf("after purge: %+v %v, want the row and the file gone", res, err)
	}
	if _, err := os.Stat(st.dayPath(day)); !os.IsNotExist(err) {
		t.Errorf("empty day file still present: %v", err)
	}
}

// GC running in the window between an archiver's step 3 and step 4 -- the day
// file holds the copy, the hot row still holds the bodies and has no marker --
// must delete nothing.
func TestGCBetweenStepThreeAndFourDeletesNothing(t *testing.T) {
	st := newTestStore(t)
	id, ev := agedEvent(t, st, "window", 10*24*time.Hour)
	var gc GCResult
	a := newArchiver(st, 7)
	a.AfterStep = func(n int) error {
		if n != 3 {
			return nil
		}
		var err error
		gc, err = st.GCArchive(context.Background())
		return err
	}
	if _, err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gc.RowsDeleted != 0 || gc.FilesRemoved != 0 {
		t.Fatalf("GC in the step 3/4 window deleted %+v, want nothing", gc)
	}
	if req, _ := hotBodies(t, st, id); req != nil {
		t.Error("the archiver did not finish after the GC")
	}
	wantRecoverable(t, st, id, ev)
}

// GI-13's hang was a long hold of the one connection. A reader must never wait
// long behind the archiver.
func TestArchiverDoesNotStarveAReader(t *testing.T) {
	st := newTestStore(t)
	big := bytes.Repeat([]byte("payload-"), 12_000) // ~96 KB
	for i := 0; i < 200; i++ {
		ev := fullEvent(fmt.Sprintf("lat%03d", i))
		ev.StartedAt = archiverNow.Add(-10*24*time.Hour - time.Duration(i)*time.Second)
		ev.ReqBody, ev.RespBody = big, big
		if _, _, err := st.InsertEvent(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	a := newArchiver(st, 7)
	a.Pause = 5 * time.Millisecond

	var worst time.Duration
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if _, err := st.ListEvents(context.Background(), EventFilter{Limit: 50}); err != nil {
				t.Error(err)
				return
			}
			if d := time.Since(start); d > worst {
				worst = d
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := a.Run(context.Background())
	close(stop)
	wg.Wait()
	if err != nil || res.Archived != 200 {
		t.Fatalf("%+v %v", res, err)
	}
	const bound = 2 * time.Second
	if worst > bound {
		t.Errorf("a ListEvents waited %s behind the archiver, bound %s", worst, bound)
	}
	t.Logf("worst ListEvents wait behind the archiver: %s", worst)
}

// BenchmarkArchiveDay is how the batch size is chosen from a number:
// go test -bench ArchiveDay ./internal/store/
func BenchmarkArchiveDay(b *testing.B) {
	body := bytes.Repeat([]byte(`{"role":"user","content":"benchmark payload "}`), 4000) // ~180 KB
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		st, err := Open(b.TempDir() + "/bench.db")
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 500; j++ {
			ev := fullEvent(fmt.Sprintf("bench-%d", j))
			ev.StartedAt = archiverNow.Add(-10*24*time.Hour - time.Duration(j)*time.Second)
			ev.ReqBody, ev.RespBody = body, body
			if _, _, err := st.InsertEvent(context.Background(), ev); err != nil {
				b.Fatal(err)
			}
		}
		a := newArchiver(st, 7)
		b.StartTimer()
		if _, err := a.Run(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		st.Close()
	}
}
