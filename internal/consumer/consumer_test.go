package consumer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/analyze"
	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/session"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
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

func nonStreamBody(model string, inputTokens, outputTokens int) []byte {
	return []byte(fmt.Sprintf(
		`{"model":%q,"stop_reason":"end_turn","usage":{"input_tokens":%d,"output_tokens":%d}}`,
		model, inputTokens, outputTokens,
	))
}

func nonStreamBodyWithCacheWrite5m(model string, write5m int) []byte {
	return []byte(fmt.Sprintf(
		`{"model":%q,"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":%d}}}`,
		model, write5m,
	))
}

func sseBody(model string, inputTokens, outputTokens int) []byte {
	return []byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"" + model +
			"\",\"usage\":{\"input_tokens\":" + fmt.Sprint(inputTokens) + "}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":" + fmt.Sprint(outputTokens) + "}}\n\n",
	)
}

func basicCall(requestID string) *sink.CapturedCall {
	return &sink.CapturedCall{
		RequestIDHeader: requestID,
		StartedAt:       time.Now(),
		Duration:        10 * time.Millisecond,
		Method:          "POST",
		Path:            "/v1/messages",
		Status:          200,
		AuthKind:        "api_key",
		ReqHeaders:      http.Header{"Content-Type": {"application/json"}},
		RespHeaders:     http.Header{"Content-Type": {"application/json"}},
		ReqBody:         []byte(`{"model":"claude-sonnet-5","messages":[]}`),
		RespBody:        nonStreamBody("claude-sonnet-5", 10, 5),
		CaptureComplete: true,
	}
}

// Happy path: a fully-formed call produces one row with all fields
// correct.
func TestConsumerHappyPath(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)
	c.SetPriceTable(pricing.ShippedTable())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	sk.Submit(basicCall("req-happy"))
	waitForEvents(t, st, 1)
	cancel()
	<-done

	evs, err := st.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.RequestID != "req-happy" || ev.ModelResolved != "claude-sonnet-5" {
		t.Errorf("event = %+v, unexpected", ev)
	}
	if ev.InputTokens != 10 || ev.OutputTokens != 5 {
		t.Errorf("tokens = %+v, want input=10 output=5", ev)
	}
	if ev.Status != 200 || ev.Method != "POST" || ev.Path != "/v1/messages" {
		t.Errorf("proxy fields = %+v, unexpected", ev)
	}
}

// Streaming call: an SSE RespBody yields correct token counts.
func TestConsumerStreamingCall(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	call := basicCall("req-stream")
	call.RespHeaders = http.Header{"Content-Type": {"text/event-stream"}}
	call.RespBody = sseBody("claude-sonnet-5", 20, 8)
	sk.Submit(call)

	ev := waitForEvents(t, st, 1)[0]
	if ev.InputTokens != 20 || ev.OutputTokens != 8 {
		t.Errorf("SSE tokens = %+v, want input=20 output=8", ev)
	}
}

// Unparseable request body: still produces a row, no panic, ModelRequested
// empty.
func TestConsumerUnparseableRequestBody(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	call := basicCall("req-badjson")
	call.ReqBody = []byte("not json")
	sk.Submit(call)

	ev := waitForEvents(t, st, 1)[0]
	if ev.ModelRequested != "" {
		t.Errorf("ModelRequested = %q, want empty for an unparseable request body", ev.ModelRequested)
	}
}

// Upstream error call: row exists with an upstream_error finding.
func TestConsumerUpstreamErrorCall(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	call := basicCall("req-upstream-err")
	call.Err = errors.New("upstream connection reset")
	call.RespBody = nil
	sk.Submit(call)

	ev := waitForEvents(t, st, 1)[0]
	warnings, err := st.EventWarnings(context.Background(), ev.ID)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Kind == "upstream_error" {
			found = true
		}
	}
	if !found {
		t.Error("no upstream_error warning attached to an errored call")
	}
}

type panicAnalyzer struct{}

func (panicAnalyzer) Analyze(parse.Meta, parse.Usage, *store.Event) []store.Warning {
	panic("boom")
}

// A panicking analyzer is recovered; the consumer survives and processes
// later calls; an analyzer_panic warning names the analyzer.
func TestConsumerPanickingAnalyzerRecovered(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)
	c.SetAnalyzers(panicAnalyzer{})

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	sk.Submit(basicCall("req-panic"))
	ev := waitForEvents(t, st, 1)[0]

	warnings, err := st.EventWarnings(context.Background(), ev.ID)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Kind == "analyzer_panic" {
			found = true
		}
	}
	if !found {
		t.Fatal("no analyzer_panic warning after a panicking analyzer")
	}

	// The consumer must still be alive for a later call.
	sk.Submit(basicCall("req-after-panic"))
	waitForEvents(t, st, 2)
}

// failingStore fails InsertEvent exactly once (on its first call), then
// behaves like the wrapped store -- simulating "a store error on one call"
// without a second SQLite implementation.
type failingStore struct {
	inner      Store
	failedOnce atomic.Bool
}

func (f *failingStore) InsertEvent(ctx context.Context, ev *store.Event) (int64, string, error) {
	if !f.failedOnce.Swap(true) {
		return 0, "", errors.New("simulated store failure")
	}
	return f.inner.InsertEvent(ctx, ev)
}

func (f *failingStore) UpsertWarnings(ctx context.Context, eventID int64, warnings []store.Warning) error {
	return f.inner.UpsertWarnings(ctx, eventID, warnings)
}

func (f *failingStore) SessionEvents(ctx context.Context, sessionID string) ([]*store.Event, error) {
	return f.inner.SessionEvents(ctx, sessionID)
}

// A store error on one call does not stop processing subsequent calls.
func TestConsumerStoreErrorDoesNotStopProcessing(t *testing.T) {
	real := newTestStore(t)
	fs := &failingStore{inner: real}
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, fs, nil)
	c.batchSize = 1 // flush each call on its own so the second isn't stuck behind the first in the same batch

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	sk.Submit(basicCall("req-fails"))
	sk.Submit(basicCall("req-succeeds"))

	waitForEvents(t, real, 1)
	evs, err := real.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].RequestID != "req-succeeds" {
		t.Errorf("events = %+v, want exactly req-succeeds", evs)
	}
}

// Batching: many rapid calls flush in grouped cycles, not one flush per
// call -- flushCount (the batching policy's actual unit) stays materially
// below the call count. Each event's own write remains isolated (see
// flush's doc comment), so this measures scheduling, not transaction
// count.
func TestConsumerBatchesFlushes(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(4096)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	const n = 200
	for i := 0; i < n; i++ {
		sk.Submit(basicCall(fmt.Sprintf("req-batch-%d", i)))
	}
	waitForEvents(t, st, n)

	flushes := c.Stats().FlushCount
	if flushes == 0 {
		t.Fatal("FlushCount = 0, want at least one flush")
	}
	if flushes >= n {
		t.Errorf("FlushCount = %d, want materially below %d calls", flushes, n)
	}
}

// Flush on quiet: one call is readable within 500ms even without hitting
// the batch-size trigger.
func TestConsumerFlushesOnQuiet(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	sk.Submit(basicCall("req-quiet"))

	deadline := time.After(500 * time.Millisecond)
	for {
		n, err := st.CountEvents(context.Background(), store.EventFilter{})
		if err != nil {
			t.Fatalf("CountEvents: %v", err)
		}
		if n == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("call not flushed within 500ms of quiet")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Throughput: 1000 submitted calls produce 1000 rows.
func TestConsumerThroughput(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(4096)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	const n = 1000
	for i := 0; i < n; i++ {
		sk.Submit(basicCall(fmt.Sprintf("req-throughput-%d", i)))
	}
	waitForEvents(t, st, n)
}

// Shutdown: cancel with calls queued returns within the shutdown bound and
// leaves the store usable.
func TestConsumerShutdownBound(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(4096)
	c := New(sk, st, nil)
	c.shutdownGrace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	for i := 0; i < 100; i++ {
		sk.Submit(basicCall(fmt.Sprintf("req-shutdown-%d", i)))
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within the shutdown bound")
	}

	if _, err := st.CountEvents(context.Background(), store.EventFilter{}); err != nil {
		t.Errorf("store unusable after shutdown: %v", err)
	}
}

// Cost wiring: a priced row carries the right cost_source; an unknown
// model leaves both cost columns NULL, never $0.00.
func TestConsumerCostWiring(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	c := New(sk, st, nil)
	c.SetPriceTable(pricing.ShippedTable())

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	priced := basicCall("req-priced")
	sk.Submit(priced)
	unknown := basicCall("req-unknown-model")
	unknown.RespBody = nonStreamBody("claude-does-not-exist", 10, 5)
	sk.Submit(unknown)

	evs := waitForEvents(t, st, 2)
	var pricedEv, unknownEv *store.EventSummary
	for _, e := range evs {
		switch e.RequestID {
		case "req-priced":
			pricedEv = e
		case "req-unknown-model":
			unknownEv = e
		}
	}
	if pricedEv == nil || pricedEv.CostSource != "shipped" || pricedEv.CostUSD == nil {
		t.Errorf("priced event = %+v, want CostSource=shipped and non-nil CostUSD", pricedEv)
	}
	if unknownEv == nil || unknownEv.CostSource != "unpriced" || unknownEv.CostUSD != nil {
		t.Errorf("unknown-model event = %+v, want CostSource=unpriced and nil CostUSD", unknownEv)
	}
}

// Billing model: a subscription account writes api_equivalent_cost_usd and
// leaves cost_usd NULL; an api account is the reverse.
func TestConsumerBillingModeSplit(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	accounts := []config.Account{
		{Name: "personal", BillingMode: "subscription"},
		{Name: "work", BillingMode: "api"},
	}
	c := New(sk, st, accounts)
	c.SetPriceTable(pricing.ShippedTable())

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	sub := basicCall("req-sub")
	sub.AuthKind = "oauth"
	sk.Submit(sub)

	api := basicCall("req-api")
	api.AuthKind = "api_key"
	sk.Submit(api)

	evs := waitForEvents(t, st, 2)
	for _, e := range evs {
		switch e.RequestID {
		case "req-sub":
			if e.BillingMode != "subscription" || e.CostUSD != nil || e.ApiEquivalentCostUSD == nil {
				t.Errorf("subscription event = %+v, want BillingMode=subscription, CostUSD=nil, ApiEquivalentCostUSD set", e)
			}
		case "req-api":
			if e.BillingMode != "api" || e.ApiEquivalentCostUSD != nil || e.CostUSD == nil {
				t.Errorf("api event = %+v, want BillingMode=api, ApiEquivalentCostUSD=nil, CostUSD set", e)
			}
		}
	}
}

// Session-scoped rule: a finding attaches to the earlier row that
// completed its pattern, not just whichever call triggered the pass, and
// the session's warning_count reflects it.
func TestConsumerSessionRuleAttachesFindingToEarlierRow(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(sink.DefaultCapacity)
	resolver := session.New(st, 30)
	c := New(sk, st, nil)
	c.SetSessionResolver(resolver)
	c.SetSessionAggregator(resolver)
	c.SetSessionRule(analyze.Engine{})

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	sessionHeaders := http.Header{}
	sessionHeaders.Set("x-clens-session", "sess-1")

	first := basicCall("req-cache-write")
	first.ReqHeaders = sessionHeaders
	first.RespBody = nonStreamBodyWithCacheWrite5m("claude-sonnet-5", 1000)
	sk.Submit(first)

	second := basicCall("req-cache-follow-up")
	second.ReqHeaders = sessionHeaders
	sk.Submit(second)

	evs := waitForEvents(t, st, 2)
	var firstID int64
	var sessionID string
	for _, e := range evs {
		if e.RequestID == "req-cache-write" {
			firstID = e.ID
			sessionID = e.SessionID
		}
	}
	if firstID == 0 {
		t.Fatal("could not find req-cache-write's event id")
	}

	warnings, err := st.EventWarnings(context.Background(), firstID)
	if err != nil {
		t.Fatalf("ListWarnings: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Kind == string(analyze.KindCacheWriteNeverRead) {
			found = true
		}
	}
	if !found {
		t.Errorf("cache write's own row missing %s: %v", analyze.KindCacheWriteNeverRead, warnings)
	}

	sess, err := st.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.WarningCount == 0 {
		t.Error("session warning_count did not pick up the session-scoped finding")
	}
}

// -race clean with a concurrent producer.
func TestConsumerConcurrentProducer(t *testing.T) {
	st := newTestStore(t)
	sk := sink.New(4096)
	c := New(sk, st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				sk.Submit(basicCall(fmt.Sprintf("req-concurrent-%d-%d", p, i)))
			}
		}(p)
	}
	wg.Wait()
	waitForEvents(t, st, 200)
}

func waitForEvents(t *testing.T, st *store.Store, want int) []*store.EventSummary {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		evs, err := st.ListEvents(context.Background(), store.EventFilter{Limit: want + 10})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(evs) >= want {
			return evs
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d events, have %d", want, len(evs))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// T11 (consumer path): the same three cases as the tailer's, through the
// consumer's own pricing block. Asserted separately because the attach is a
// second call site -- one implementation covering only the tailer would leave
// every proxied call without the warning.
func TestConsumerAttachesPeakPricingWarning(t *testing.T) {
	// 2026-09-21 is a Monday; the shipped window is [01:00,04:00) UTC.
	peakAt := time.Date(2026, time.September, 21, 2, 0, 0, 0, time.UTC)
	offAt := time.Date(2026, time.September, 21, 0, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		model string
		at    time.Time
		want  bool
	}{
		{"priced at peak", "deepseek-flash", peakAt, true},
		{"priced off peak", "deepseek-flash", offAt, false},
		{"unpriced at peak", "claude-nonesuch-9", peakAt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			sk := sink.New(sink.DefaultCapacity)
			c := New(sk, st, nil)
			c.SetPriceTable(pricing.ShippedTable())

			ctx, cancel := context.WithCancel(context.Background())
			go c.Run(ctx)
			defer cancel()

			call := basicCall("req-peak")
			call.StartedAt = tc.at
			call.ReqBody = []byte(fmt.Sprintf(`{"model":%q,"messages":[]}`, tc.model))
			call.RespBody = nonStreamBody(tc.model, 1000, 10)
			sk.Submit(call)

			evs := waitForEvents(t, st, 1)
			ev := evs[0]
			warnings, err := st.EventWarnings(context.Background(), ev.ID)
			if err != nil {
				t.Fatalf("EventWarnings: %v", err)
			}
			found := false
			for _, w := range warnings {
				if w.Kind == "peak_pricing" {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("peak_pricing present = %v, want %v (cost_source=%q, model=%q, at=%s)",
					found, tc.want, ev.CostSource, ev.ModelResolved, tc.at.Format(time.RFC3339))
			}
		})
	}
}

// TestRequestIDPrecedence is D2's rule, direct: the header wins over the
// body id, which wins over the synthetic fallback.
func TestRequestIDPrecedence(t *testing.T) {
	call := &sink.CapturedCall{RequestIDHeader: "req_header_value", StartedAt: time.Now()}
	usage := parse.Usage{MessageID: "msg_body_id"}

	if got := requestID(call, usage); got != "req_header_value" {
		t.Errorf("requestID = %q, want the header value when both are present", got)
	}
}

func TestRequestIDBodyIDWinsOverSynthetic(t *testing.T) {
	call := &sink.CapturedCall{StartedAt: time.Now()}
	usage := parse.Usage{MessageID: "msg_body_id"}

	if got := requestID(call, usage); got != "msg_body_id" {
		t.Errorf("requestID = %q, want the body id when no header is present", got)
	}
}

func TestRequestIDSyntheticFallbackShape(t *testing.T) {
	call := &sink.CapturedCall{StartedAt: time.Now(), ReqBody: []byte(`{"model":"m"}`)}
	usage := parse.Usage{}

	got := requestID(call, usage)
	if !strings.HasPrefix(got, "proxy:") {
		t.Errorf("requestID = %q, want a proxy: synthetic key when neither header nor body id is present", got)
	}
}

// TestRequestIDSyntheticFallbackNilBodyIsWellFormed is the "off"-policy
// shape: no header, no body id, and a nil request body (--body-policy off
// never captures one). The proxy used to guard this with a nil
// *boundedBuffer check; D2 moved the hash to the consumer, where
// sha256.Sum256(nil) is simply the hash of zero bytes, so there is nothing
// to guard.
func TestRequestIDSyntheticFallbackNilBodyIsWellFormed(t *testing.T) {
	call := &sink.CapturedCall{StartedAt: time.Now()}
	usage := parse.Usage{}

	got := requestID(call, usage)
	if !strings.HasPrefix(got, "proxy:") {
		t.Errorf("requestID = %q, want a well-formed proxy: key with a nil body", got)
	}
}

// TestHashFallbackTwoAttemptsProduceDistinctIDs is test 21's consumer half
// (moved from internal/proxy with the code it exercises, br-GI-9-02): two
// byte-identical bodies in one process must not collapse onto the same
// synthetic key, which would destroy the rate_limited/overloaded signal
// those attempts exist to record.
func TestHashFallbackTwoAttemptsProduceDistinctIDs(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5"}`)
	started := time.Now()

	first := requestID(&sink.CapturedCall{StartedAt: started, ReqBody: body}, parse.Usage{})
	second := requestID(&sink.CapturedCall{StartedAt: started, ReqBody: body}, parse.Usage{})

	if !strings.HasPrefix(first, "proxy:") || !strings.HasPrefix(second, "proxy:") {
		t.Fatalf("want both fallback IDs prefixed \"proxy:\", got %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("two attempts with identical bodies produced the same RequestID %q, want distinct", first)
	}
}
