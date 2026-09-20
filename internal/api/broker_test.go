package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

func TestBrokerPublishReachesSubscriber(t *testing.T) {
	b := NewBroker()
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()

	b.Publish(Event{Type: "event", ID: 42})

	select {
	case e := <-ch:
		if e.ID != 42 || e.Type != "event" {
			t.Fatalf("got %+v, want {event 42}", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the published event")
	}
}

func TestBrokerUnsubscribeIsIdempotentAndDoesNotLeak(t *testing.T) {
	b := NewBroker()
	_, unsubscribe := b.Subscribe()
	unsubscribe()
	unsubscribe() // must not panic

	if len(b.subs) != 0 {
		t.Fatalf("subs = %d after unsubscribe, want 0", len(b.subs))
	}
}

func TestBrokerDropsSlowSubscriberRatherThanBlocking(t *testing.T) {
	b := NewBroker()
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()

	// Fill the subscriber's buffer, then publish one more: Publish must
	// return rather than block, and the channel is closed once dropped.
	for i := 0; i < subscriberBuffer+1; i++ {
		b.Publish(Event{Type: "event", ID: int64(i)})
	}

	// Drain what made it through, then confirm the channel was closed.
	drained := 0
	for range ch {
		drained++
		if drained > subscriberBuffer {
			t.Fatal("channel was not closed after the subscriber was dropped")
		}
	}
}

// A read through the bare store must never publish: /api/requests/{id}
// serves a read-only *store.Store directly, never the PublishingStore
// decorator, so this route triggering a broker event would be a wiring bug.
func TestReadRouteDoesNotPublish(t *testing.T) {
	st := newTestStore(t)
	seedEvent(t, st, nil)
	handler, _, _, broker := newTestAPI(t, st)

	ch, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/requests", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	select {
	case e := <-ch:
		t.Fatalf("a read published an event: %+v", e)
	case <-time.After(50 * time.Millisecond):
		// no event: correct.
	}
}

func TestPublishingStoreInsertEventPublishes(t *testing.T) {
	st := newTestStore(t)
	broker := NewBroker()
	ps := NewPublishingStore(st, broker)

	ch, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	id, _, err := ps.InsertEvent(context.Background(), &store.Event{EventSummary: store.EventSummary{
		RequestID: "req_publish_1", Source: "proxy", FirstSource: "proxy",
		StartedAt: time.Now(), BillingMode: "api"}})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	select {
	case e := <-ch:
		if e.Type != "event" || e.ID != id {
			t.Fatalf("got %+v, want {event %d}", e, id)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the publish")
	}
}

func TestPublishingStoreUpsertWarningsPublishesOnlyWhenNonEmpty(t *testing.T) {
	st := newTestStore(t)
	broker := NewBroker()
	ps := NewPublishingStore(st, broker)

	id, _, err := ps.InsertEvent(context.Background(), &store.Event{EventSummary: store.EventSummary{
		RequestID: "req_publish_2", Source: "proxy", FirstSource: "proxy",
		StartedAt: time.Now(), BillingMode: "api"}})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	ch, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	if err := ps.UpsertWarnings(context.Background(), id, nil); err != nil {
		t.Fatalf("UpsertWarnings(nil): %v", err)
	}
	select {
	case e := <-ch:
		t.Fatalf("an empty warnings upsert published an event: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}
