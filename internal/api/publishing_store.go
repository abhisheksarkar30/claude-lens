package api

import (
	"context"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// PublishingStore wraps a *store.Store and satisfies internal/consumer's
// narrow Store interface (InsertEvent, UpsertWarnings, SessionEvents),
// publishing a Broker event after each successful write.
//
// consumer.New's Store parameter is already a narrow interface anything
// satisfies structurally, so this decorator gets an event per committed
// write by wrapping the store consumer.go writes through, instead of
// changing consumer.go itself -- the same deviation deepseek-lens's own
// PublishingStore documents, for the same reason. The composition root
// (a later slice) constructs one of these and hands it to consumer.New in
// place of the bare *store.Store; the read-only internal/api handlers keep
// using the bare *store.Store directly, so a read can never trigger a
// publish.
//
// SessionEvents needs no override: it is a read call consumer.Store
// requires (for the session-scoped analyzer pass) and is promoted
// unchanged from the embedded *store.Store.
type PublishingStore struct {
	*store.Store
	broker *Broker
}

// NewPublishingStore returns a PublishingStore wrapping st, publishing to b.
func NewPublishingStore(st *store.Store, b *Broker) *PublishingStore {
	return &PublishingStore{Store: st, broker: b}
}

// InsertEvent inserts ev through the wrapped store, then publishes a
// {type:"event", id} event on success. It returns the written row's session
// alongside the id, unchanged from the wrapped store.
func (p *PublishingStore) InsertEvent(ctx context.Context, ev *store.Event) (int64, string, error) {
	id, sessionID, err := p.Store.InsertEvent(ctx, ev)
	if err != nil {
		return id, sessionID, err
	}
	p.broker.Publish(Event{Type: "event", ID: id})
	return id, sessionID, nil
}

// UpsertWarnings attaches warnings through the wrapped store, then
// publishes a {type:"warnings", id, warnings} event on success. A call with
// no warnings (the common case) is a no-op in the wrapped store and
// publishes nothing.
func (p *PublishingStore) UpsertWarnings(ctx context.Context, eventID int64, warnings []store.Warning) error {
	if err := p.Store.UpsertWarnings(ctx, eventID, warnings); err != nil {
		return err
	}
	if len(warnings) == 0 {
		return nil
	}
	p.broker.Publish(Event{Type: "warnings", ID: eventID, Warnings: warnings})
	return nil
}
