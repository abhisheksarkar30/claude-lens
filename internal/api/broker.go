package api

import (
	"sync"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// subscriberBuffer is how many events a slow SSE client may lag behind
// before Broker drops it. Large enough to absorb a brief stall, small
// enough that a wedged client is noticed and cut loose quickly rather than
// accumulating unboundedly.
const subscriberBuffer = 64

// Event is one broker message, JSON-encoded verbatim as an SSE "data:"
// line. Type is "event" (a new row was committed, ID is its event id) or
// "warnings" (warnings were attached to event ID).
type Event struct {
	Type     string          `json:"type"`
	ID       int64           `json:"id"`
	Warnings []store.Warning `json:"warnings,omitempty"`
}

// Broker fans out published Events to subscribed SSE clients through
// buffered channels. Publish never blocks: a subscriber whose buffer is
// full is dropped (its channel closed and removed) rather than allowed to
// slow the publisher -- the same non-blocking discipline internal/sink
// applies to the proxy's hot path, applied here to the UI (CLAUDE.md).
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBroker returns an empty Broker ready to use.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Subscribe registers a new subscriber and returns its receive channel plus
// an unsubscribe function. Calling unsubscribe more than once, or after the
// broker has already dropped this subscriber for being slow, is safe.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsubscribe
}

// Publish sends e to every current subscriber. A subscriber whose buffer is
// already full is dropped instead of blocking this call.
func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}
