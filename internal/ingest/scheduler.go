package ingest

import (
	"context"
	"sync/atomic"
	"time"
)

// Scheduler runs a Runner's RunOnce on a fixed interval until its context
// is cancelled, started by `clens serve` (br-GI-1-17).
type Scheduler struct {
	runner   *Runner
	interval time.Duration
	running  atomic.Bool
}

// NewScheduler returns a Scheduler that calls r.RunOnce every interval.
func NewScheduler(r *Runner, interval time.Duration) *Scheduler {
	return &Scheduler{runner: r, interval: interval}
}

// Run blocks until ctx is done, calling RunOnce once per tick. A tick that
// arrives while the previous run is still in flight is skipped, never
// stacked -- a slow collector delays the next run instead of piling up
// concurrent runs against the same single-writer store connection.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		return // previous run still in flight -- skip this tick
	}
	defer s.running.Store(false)
	s.runner.RunOnce(ctx)
}
