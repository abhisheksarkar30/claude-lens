package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func ok(rows int) Collector {
	return CollectorFunc(func(ctx context.Context) (int, error) { return rows, nil })
}

func failing(msg string) Collector {
	return CollectorFunc(func(ctx context.Context) (int, error) { return 0, errors.New(msg) })
}

// Test 17: with the snapshot collector forced to fail, the JSONL and
// admin collectors still run and write, and the health surface shows
// snapshot failed with a reason.
func TestRunOnceCollectorIsolation(t *testing.T) {
	st := newTestStore(t)
	r := New(st)
	r.Add(SourceJSONL, "root1", "", ok(5))
	r.Add(SourceSnapshot, "acct1", "", failing("snapshot: unreachable"))
	r.Add(SourceAdmin, "org1", "", ok(3))

	outcomes := r.RunOnce(context.Background())
	if len(outcomes) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(outcomes))
	}
	byRows := map[Source]Outcome{}
	for _, o := range outcomes {
		byRows[o.Source] = o
	}
	if byRows[SourceJSONL].Status != "ok" || byRows[SourceJSONL].Rows != 5 {
		t.Errorf("jsonl outcome = %+v, want ok/5", byRows[SourceJSONL])
	}
	if byRows[SourceAdmin].Status != "ok" || byRows[SourceAdmin].Rows != 3 {
		t.Errorf("admin outcome = %+v, want ok/3, want it to keep running despite snapshot's failure", byRows[SourceAdmin])
	}
	if byRows[SourceSnapshot].Status != "error" || byRows[SourceSnapshot].Error != "snapshot: unreachable" {
		t.Errorf("snapshot outcome = %+v, want error with the collector's message", byRows[SourceSnapshot])
	}

	health, err := r.SourcesHealth(context.Background())
	if err != nil {
		t.Fatalf("SourcesHealth: %v", err)
	}
	byHealth := map[Source]Health{}
	for _, h := range health {
		byHealth[h.Source] = h
	}
	if byHealth[SourceJSONL].Status != "ok" || byHealth[SourceJSONL].RowsWritten != 5 {
		t.Errorf("jsonl health = %+v, want ok/5 rows", byHealth[SourceJSONL])
	}
	if byHealth[SourceAdmin].Status != "ok" || byHealth[SourceAdmin].RowsWritten != 3 {
		t.Errorf("admin health = %+v, want ok/3 rows", byHealth[SourceAdmin])
	}
	if byHealth[SourceSnapshot].Status != "error" || byHealth[SourceSnapshot].LastError != "snapshot: unreachable" {
		t.Errorf("snapshot health = %+v, want error with a reason", byHealth[SourceSnapshot])
	}
}

// A panicking collector is recovered, recorded as its error, and the
// others still run -- a broken collector never crashes the process.
func TestRunOnceRecoversPanic(t *testing.T) {
	st := newTestStore(t)
	r := New(st)
	r.Add(SourceJSONL, "root1", "", ok(1))
	r.Add(SourceSnapshot, "acct1", "", CollectorFunc(func(ctx context.Context) (int, error) {
		panic("boom")
	}))
	r.Add(SourceAdmin, "org1", "", ok(2))

	outcomes := r.RunOnce(context.Background())
	for _, o := range outcomes {
		switch o.Source {
		case SourceSnapshot:
			if o.Status != "error" || !strings.Contains(o.Error, "boom") {
				t.Errorf("panicking collector outcome = %+v, want error containing 'boom'", o)
			}
		default:
			if o.Status != "ok" {
				t.Errorf("%s outcome = %+v, want ok despite the sibling panic", o.Source, o)
			}
		}
	}
}

// RunOnce persists a status and updated_at per collector into
// ingest_state.
func TestRunOnceReadBackFromStore(t *testing.T) {
	st := newTestStore(t)
	r := New(st)
	r.Add(SourceJSONL, "root1", "", ok(7))
	r.RunOnce(context.Background())

	got, ok2, err := st.GetIngestState(context.Background(), successKey(SourceJSONL))
	if err != nil || !ok2 {
		t.Fatalf("GetIngestState: ok=%v err=%v", ok2, err)
	}
	if got.Status != "ok" || got.Value != "7" || got.UpdatedAt.IsZero() {
		t.Errorf("got %+v, want Status=ok Value=7 and a non-zero UpdatedAt", got)
	}
}

// The scheduler skips a tick while a run is still in flight, rather than
// stacking runs.
func TestSchedulerSkipsOverlappingTick(t *testing.T) {
	st := newTestStore(t)
	r := New(st)

	var calls int32
	release := make(chan struct{})
	r.Add(SourceJSONL, "root1", "", CollectorFunc(func(ctx context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return 1, nil
	}))

	sched := NewScheduler(r, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	// Let several ticks elapse while the first run is still blocked on
	// release -- only the first should have started.
	time.Sleep(80 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("collector called %d times while blocked, want exactly 1 (later ticks should be skipped)", n)
	}

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Scheduler.Run did not return after context cancellation")
	}
}

// Cancelling the context stops the scheduler within a bound.
func TestSchedulerStopsOnCancel(t *testing.T) {
	st := newTestStore(t)
	r := New(st)
	r.Add(SourceJSONL, "root1", "", ok(1))

	sched := NewScheduler(r, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Scheduler.Run did not stop within 1s of context cancellation")
	}
}
