package consumer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// transientErr satisfies store.IsTransient the way the driver's error does:
// through a Code method (10 is SQLITE_IOERR).
type transientErr struct{}

func (transientErr) Error() string { return "disk I/O error (266)" }
func (transientErr) Code() int     { return 266 }

var fastBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}

func TestWithRetryRetriesTransientOnly(t *testing.T) {
	var retries atomic.Uint64

	calls := 0
	err := withRetry(context.Background(), fastBackoff, &retries, func() error {
		calls++
		if calls <= 2 {
			return transientErr{}
		}
		return nil
	})
	if err != nil || calls != 3 || retries.Load() != 2 {
		t.Errorf("transient twice then ok: err=%v calls=%d retries=%d, want nil/3/2", err, calls, retries.Load())
	}

	calls = 0
	permanent := errors.New("UNIQUE constraint failed")
	if err := withRetry(context.Background(), fastBackoff, &retries, func() error { calls++; return permanent }); err != permanent || calls != 1 {
		t.Errorf("permanent error: err=%v calls=%d, want it returned after exactly 1 call", err, calls)
	}

	calls = 0
	if err := withRetry(context.Background(), fastBackoff, &retries, func() error { calls++; return transientErr{} }); err == nil || calls != 4 {
		t.Errorf("always transient: err=%v calls=%d, want the error after 4 attempts", err, calls)
	}
}

func TestWithRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var retries atomic.Uint64
	calls := 0
	start := time.Now()
	err := withRetry(ctx, []time.Duration{time.Hour}, &retries, func() error {
		calls++
		cancel() // cancelled while the first backoff is about to be waited out
		return transientErr{}
	})
	if time.Since(start) > 2*time.Second {
		t.Fatal("withRetry waited out the backoff after cancel")
	}
	if _, ok := err.(transientErr); !ok || calls != 1 {
		t.Errorf("err=%v calls=%d, want the store error (not context.Canceled) after 1 call", err, calls)
	}
}

type scriptedStore struct {
	inserts atomic.Int32
}

// The first insert lands in session "s"; every later one fails transiently.
func (s *scriptedStore) InsertEvent(context.Context, *store.Event) (int64, string, error) {
	if s.inserts.Add(1) == 1 {
		return 1, "s", nil
	}
	return 0, "", transientErr{}
}
func (s *scriptedStore) UpsertWarnings(context.Context, int64, []store.Warning) error { return nil }
func (s *scriptedStore) SessionEventsForRules(context.Context, string) ([]*store.Event, error) {
	return nil, nil
}

type countingRule struct{ n atomic.Int32 }

func (r *countingRule) AnalyzeSession([]*store.Event) []store.Warning { r.n.Add(1); return nil }

type countingFold struct{ n atomic.Int32 }

func (f *countingFold) RecordCall(context.Context, string, *store.Event) error {
	f.n.Add(1)
	return nil
}

func TestWithRetryGivesUpOncePerFlush(t *testing.T) {
	st := &scriptedStore{}
	c := New(sink.New(1), st, nil)
	c.SetRetryBackoff(fastBackoff...)
	rule, fold := &countingRule{}, &countingFold{}
	c.SetSessionRule(rule)
	c.SetSessionAggregator(fold)

	batch := []*pendingEvent{{ev: &store.Event{}}, {ev: &store.Event{}}, {ev: &store.Event{}}}
	c.flush(context.Background(), batch)

	// 1 successful insert, then 4 attempts on the second row; the third row is
	// never attempted.
	if got := st.inserts.Load(); got != 5 {
		t.Errorf("InsertEvent calls = %d, want 5", got)
	}
	s := c.Stats()
	if s.Failed != 2 {
		t.Errorf("Failed = %d, want 2 (the exhausted row and the skipped one)", s.Failed)
	}
	if s.TransientGaveUp != 1 || s.TransientRetries != 3 {
		t.Errorf("gave up %d, retries %d, want 1 and 3", s.TransientGaveUp, s.TransientRetries)
	}
	if rule.n.Load() != 0 || fold.n.Load() != 0 {
		t.Errorf("session pass ran %d times, fold %d times, want neither after a give-up", rule.n.Load(), fold.n.Load())
	}
}
