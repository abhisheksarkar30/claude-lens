// Package session groups captured calls into agentic-run sessions: the
// pre-insert half (Resolve, read-only) and the post-insert half
// (RecordCall, which persists).
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Resolver groups by prefix_hash within an inactivity-gap window; an
// explicit x-clens-session header overrides the prefix and wins. It
// implements both consumer.SessionResolver and consumer.SessionAggregator.
type Resolver struct {
	st  *store.Store
	gap time.Duration

	mu   sync.Mutex
	seen map[string]lastSession // grouping key -> most recently assigned session
}

type lastSession struct {
	id string
	at time.Time
}

// New returns a Resolver whose sessions close after gapMinutes of
// inactivity on the same grouping key.
func New(st *store.Store, gapMinutes int) *Resolver {
	return &Resolver{
		st:   st,
		gap:  time.Duration(gapMinutes) * time.Minute,
		seen: map[string]lastSession{},
	}
}

// Resolve returns the session id meta belongs to. A call carrying
// SessionHeader (D7: x-clens-session, or the request's own
// x-claude-code-session-id) has that value *as* its identity, verbatim —
// not merely as a grouping key — because the request already names the
// conversation it belongs to. The inactivity-gap window below applies
// only to the residual header-less calls: the same id as the grouping
// key's last call if that call was within the gap window, otherwise a
// freshly minted one.
func (r *Resolver) Resolve(meta parse.Meta, now time.Time) string {
	if meta.SessionHeader != "" {
		return meta.SessionHeader
	}

	key := groupKey(meta)

	r.mu.Lock()
	defer r.mu.Unlock()

	if last, ok := r.seen[key]; ok && now.Sub(last.at) <= r.gap {
		r.seen[key] = lastSession{id: last.id, at: now}
		return last.id
	}

	id := newSessionID(now)
	r.seen[key] = lastSession{id: id, at: now}
	return id
}

// groupKey is the grouping identity a header-less call resolves against
// (Resolve returns SessionHeader directly and never reaches this for a
// header-carrying call). Calls group by prefix_hash's value, including
// the empty string parse.Meta uses for "body did not parse" -- every
// unparseable-body call on a given header-less connection shares that one
// bucket rather than getting a unique session each.
func groupKey(meta parse.Meta) string {
	if meta.PrefixHash != nil {
		return "prefix:" + *meta.PrefixHash
	}
	return "prefix:"
}

func newSessionID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("s_%d_%s", now.UnixMilli(), hex.EncodeToString(b[:]))
}

// RecordCall folds ev into sessionID's totals: it ensures the session row
// exists (creating it on first sight) and then re-derives its aggregates
// from events + warnings, so it stays correct under both a fresh insert
// and a cross-source merge without maintaining two update paths. A NULL
// (empty) session id is a no-op.
func (r *Resolver) RecordCall(ctx context.Context, sessionID string, ev *store.Event, warningCount int) error {
	if sessionID == "" {
		return nil
	}
	var prefixHash string
	if ev.PrefixHash != nil {
		prefixHash = *ev.PrefixHash
	}
	if err := r.st.UpsertSession(ctx, sessionID, prefixHash, ev.StartedAt); err != nil {
		return fmt.Errorf("session: RecordCall: %w", err)
	}
	if err := r.st.ReconcileSession(ctx, sessionID); err != nil {
		return fmt.Errorf("session: RecordCall: %w", err)
	}
	return nil
}
