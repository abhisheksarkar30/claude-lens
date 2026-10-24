package consumer

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// defaultBackoff is the wait before each retry, so a call makes at most
// len(backoff)+1 attempts and a persistent failure costs about 5.25s.
var defaultBackoff = []time.Duration{250 * time.Millisecond, time.Second, 4 * time.Second}

// withRetry runs fn, retrying only while the error is a transient store
// failure (store.IsTransient). Each retry bumps retries. It returns the last
// error, and gives up at once when ctx is cancelled during a backoff, still
// returning the store error rather than a cancellation that would mask it.
func withRetry(ctx context.Context, backoff []time.Duration, retries *atomic.Uint64, fn func() error) error {
	err := fn()
	for _, wait := range backoff {
		if err == nil || !store.IsTransient(err) {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		retries.Add(1)
		err = fn()
	}
	return err
}
