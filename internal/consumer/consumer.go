// Package consumer is the cold-path bridge: one goroutine that drains the
// sink and writes rows. The order below is fixed; later beads plug into
// the named seams (Analyzer, SessionResolver, SessionAggregator,
// PriceComputer) rather than rewriting the pipeline.
package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/decode"
	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

const (
	// Matches config.Default().BodyCapBytes. This is the consumer's own
	// fallback for a Consumer whose cap seam was never wired, so the two must
	// move together: a fallback that disagreed with config would silently
	// truncate at a different size than `clens doctor` reports.
	defaultBodyCapBytes  = 2097152
	defaultBatchSize     = 50
	defaultFlushInterval = 250 * time.Millisecond
	defaultShutdownGrace = 2 * time.Second
)

// Store is the slice of *store.Store the consumer actually calls --
// narrowed to an interface so a test can inject a store that fails on
// command without a second SQLite implementation.
type Store interface {
	// InsertEvent returns the written row's id and the session that row
	// belongs to -- on a request_id merge, the *existing* row's session,
	// not ev's. The consumer must key its session-scoped work to that id.
	InsertEvent(ctx context.Context, ev *store.Event) (int64, string, error)
	UpsertWarnings(ctx context.Context, eventID int64, warnings []store.Warning) error
	SessionEvents(ctx context.Context, sessionID string) ([]*store.Event, error)
}

// Consumer drains a sink.Sink, builds one store.Event per captured call,
// and writes it. Every per-call failure -- bad JSON, a store error, an
// analyzer panic -- is contained here (invariant 6, fail open): it is
// logged once and the loop continues, because a broken consumer silently
// stops capture, which is worse than any single bad row.
type Consumer struct {
	sk       *sink.Sink
	st       Store
	accounts []config.Account

	resolver     SessionResolver
	aggregator   SessionAggregator
	analyzers    []Analyzer
	sessionRule  SessionRule
	pricer       PriceComputer
	bodyCapBytes int

	batchSize     int
	flushInterval time.Duration
	shutdownGrace time.Duration

	processed   atomic.Uint64
	failed      atomic.Uint64
	drained     atomic.Uint64
	flushCount  atomic.Uint64
	lastWriteAt atomic.Int64 // UnixNano; 0 means "never"
}

// New builds a Consumer draining sk and writing to st, attributing calls
// against the configured accounts. Every seam (analyzers, session
// resolver/aggregator, price table, body decoding) starts unset; a
// Consumer with nothing set still writes rows with zero usage/cost/session
// and no warnings beyond upstream_error/analyzer_panic, which is exactly
// what "before it was installed" means for each seam.
func New(sk *sink.Sink, st Store, accounts []config.Account) *Consumer {
	return &Consumer{
		sk:            sk,
		st:            st,
		accounts:      accounts,
		bodyCapBytes:  defaultBodyCapBytes,
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
		shutdownGrace: defaultShutdownGrace,
	}
}

func (c *Consumer) SetSessionResolver(r SessionResolver)     { c.resolver = r }
func (c *Consumer) SetSessionAggregator(a SessionAggregator) { c.aggregator = a }
func (c *Consumer) SetAnalyzers(analyzers ...Analyzer)       { c.analyzers = analyzers }
func (c *Consumer) SetSessionRule(r SessionRule)             { c.sessionRule = r }
func (c *Consumer) SetPriceTable(p PriceComputer)            { c.pricer = p }

// SetBodyDecoding sets the decode cap applied to a captured response body
// before parsing -- the same cfg.BodyCapBytes the proxy tees with.
func (c *Consumer) SetBodyDecoding(capBytes int) {
	if capBytes > 0 {
		c.bodyCapBytes = capBytes
	}
}

// Stats reports processed/failed/drained counts and the last write time,
// for doctor and GET /api/sources.
type Stats struct {
	Processed   uint64
	Failed      uint64
	Drained     uint64
	Dropped     uint64 // from the sink's own accepted/dropped counters
	FlushCount  uint64
	LastWriteAt time.Time
}

func (c *Consumer) Stats() Stats {
	_, dropped := c.sk.Stats()
	var lastWrite time.Time
	if ns := c.lastWriteAt.Load(); ns != 0 {
		lastWrite = time.Unix(0, ns)
	}
	return Stats{
		Processed:   c.processed.Load(),
		Failed:      c.failed.Load(),
		Drained:     c.drained.Load(),
		Dropped:     dropped,
		FlushCount:  c.flushCount.Load(),
		LastWriteAt: lastWrite,
	}
}

// pendingEvent is one call's fully-built write, ready for the store.
type pendingEvent struct {
	ev       *store.Event
	warnings []store.Warning
}

// SessionRule runs a session-scoped analysis pass over a session's whole
// row history (oldest first) and returns findings whose Warning.EventID
// already names the row each finding completed on -- possibly not the
// row that just triggered this pass. internal/analyze.Engine satisfies
// this structurally; Consumer never imports analyze, the same seam
// discipline as Analyzer (the composition root wires the two together).
type SessionRule interface {
	AnalyzeSession(rows []*store.Event) []store.Warning
}

// Run drains the sink until ctx is cancelled or the sink is closed,
// flushing the buffered batch every batchSize calls or flushInterval of
// quiet, whichever comes first. On cancel it drains whatever is already
// queued (bounded by shutdownGrace), flushes, and returns.
func (c *Consumer) Run(ctx context.Context) error {
	batch := make([]*pendingEvent, 0, c.batchSize)
	timer := time.NewTimer(c.flushInterval)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.flush(ctx, batch)
		batch = batch[:0]
	}

	drainCh := c.sk.Drain()
	for {
		select {
		case <-ctx.Done():
			c.drainRemaining(drainCh, &batch)
			flush()
			return nil

		case call, ok := <-drainCh:
			if !ok {
				flush()
				return nil
			}
			batch = append(batch, c.processCall(call))
			c.processed.Add(1)
			if len(batch) >= c.batchSize {
				flush()
				timer.Reset(c.flushInterval)
			}

		case <-timer.C:
			flush()
			timer.Reset(c.flushInterval)
		}
	}
}

// flush writes one buffered batch. Each event is its own store call rather
// than one shared transaction: a single bad row (a constraint violation,
// a closed store) must not roll back its batch-mates, which is what lets
// "a store error on one call" leave every other call in the same batch
// written. flushCount -- not the per-event write count -- is what the
// batching policy above actually reduces: goroutine wake-ups and analyzer
// runs are still grouped by size/quiet, even though each event's write
// stays isolated.
func (c *Consumer) flush(ctx context.Context, batch []*pendingEvent) {
	c.flushCount.Add(1)
	wrote := false

	// order+firstEv track, per distinct returned session id, the first
	// inserted row seen for it, in arrival order (D8): the fold below must
	// carry that row's PrefixHash, not the last one's -- prefix_hash is
	// first-writer-wins at the store (store.go's ON CONFLICT never touches
	// it), so folding the last row would insert a different hash than the
	// one that actually won the write.
	var order []string
	firstEv := map[string]*store.Event{}

	for _, pe := range batch {
		id, sessionID, err := c.st.InsertEvent(ctx, pe.ev)
		if err != nil {
			log.Printf("consumer: insert event %s: %v", pe.ev.RequestID, err)
			c.failed.Add(1)
			continue
		}
		wrote = true

		if len(pe.warnings) > 0 {
			if err := c.st.UpsertWarnings(ctx, id, pe.warnings); err != nil {
				log.Printf("consumer: upsert warnings for event %d: %v", id, err)
			}
		}

		// sessionID is the written row's session, which after a cross-source
		// merge is the first-written row's, not pe.ev's. Grouping on
		// pe.ev.SessionID would fold into the wrong session, or create a
		// session row owning no events.
		if sessionID != "" {
			if _, ok := firstEv[sessionID]; !ok {
				firstEv[sessionID] = pe.ev
				order = append(order, sessionID)
			}
		}
	}

	// Phase 2: the session-scoped pass, once per distinct session in this
	// batch -- not once per row.
	if c.sessionRule != nil {
		for _, sessionID := range order {
			c.runSessionRule(ctx, sessionID)
		}
	}

	// Phase 3: the fold, once per distinct session, after every pass above
	// so warning_count's re-derivation (ReconcileSession) counts findings
	// the pass attached to any row of this same session.
	if c.aggregator != nil {
		for _, sessionID := range order {
			if err := c.aggregator.RecordCall(ctx, sessionID, firstEv[sessionID]); err != nil {
				log.Printf("consumer: record session call for %s: %v", sessionID, err)
			}
		}
	}

	if wrote {
		c.lastWriteAt.Store(time.Now().UnixNano())
	}
}

// runSessionRule runs the session-scoped analysis pass over sessionID's
// whole row history and upserts each finding onto the row it names,
// grouped so a row with several findings gets one call.
func (c *Consumer) runSessionRule(ctx context.Context, sessionID string) {
	rows, err := c.st.SessionEvents(ctx, sessionID)
	if err != nil {
		log.Printf("consumer: session rows for %s: %v", sessionID, err)
		return
	}
	grouped := map[int64][]store.Warning{}
	for _, w := range c.sessionRule.AnalyzeSession(rows) {
		if w.EventID == 0 {
			continue
		}
		grouped[w.EventID] = append(grouped[w.EventID], w)
	}
	for eventID, warnings := range grouped {
		if err := c.st.UpsertWarnings(ctx, eventID, warnings); err != nil {
			log.Printf("consumer: upsert session warnings for event %d: %v", eventID, err)
		}
	}
}

// drainRemaining opportunistically drains whatever is already buffered in
// ch, without blocking -- bounded by shutdownGrace so a producer that never
// stops cannot hold shutdown open indefinitely. Losing the last few calls
// on a hard cancel is acceptable; hanging on exit is not.
func (c *Consumer) drainRemaining(ch <-chan *sink.CapturedCall, batch *[]*pendingEvent) {
	deadline := time.Now().Add(c.shutdownGrace)
	for time.Now().Before(deadline) {
		select {
		case call, ok := <-ch:
			if !ok {
				return
			}
			*batch = append(*batch, c.processCall(call))
			c.processed.Add(1)
			c.drained.Add(1)
		default:
			return
		}
	}
}

// processCall runs steps 1-6 of the pipeline (meta, usage, event, session,
// account, cost) and the analyzer seam. It never returns an error and
// never panics: every failure degrades the built event rather than
// dropping the call.
func (c *Consumer) processCall(call *sink.CapturedCall) *pendingEvent {
	meta := parse.ExtractMeta(call.ReqBody, call.ReqHeaders)

	respBody, respHeaders := call.RespBody, call.RespHeaders
	if respHeaders == nil {
		respHeaders = http.Header{}
	}
	// Completeness is discarded deliberately: the consumer keys off err alone,
	// because a degraded parse still beats none. A partial prefix is enough to
	// extract usage from, and branching here would change what gets stored.
	if decoded, decodedHeaders, _, err := decode.Body(respHeaders, respBody, c.bodyCapBytes); err == nil {
		respBody, respHeaders = decoded, decodedHeaders
	}
	usage := parse.ExtractUsage(respBody, respHeaders.Get("Content-Type"))

	ev := buildEvent(call, meta, usage)

	if c.resolver != nil {
		ev.SessionID = c.resolver.Resolve(meta, call.StartedAt)
	}

	account, billingMode := resolveAccount(c.accounts, call.AuthKind)
	ev.Account = account
	ev.BillingMode = billingMode

	if c.pricer != nil {
		usd, costSource := c.pricer.Compute(ev.ModelResolved, usage, usage.Speed, ev.ServiceTier, call.StartedAt)
		ev.CostSource = costSource
		switch ev.BillingMode {
		case "subscription":
			ev.ApiEquivalentCostUSD = usd
		default:
			ev.CostUSD = usd
		}
	}

	var warnings []store.Warning
	if call.Err != nil {
		warnings = append(warnings, store.Warning{
			Kind:      "upstream_error",
			Severity:  "error",
			Detail:    call.Err.Error(),
			CreatedAt: call.StartedAt,
		})
	}
	warnings = append(warnings, c.runAnalyzers(meta, usage, ev)...)
	if w, ok := peakWarning(c.pricer, ev, call.StartedAt); ok {
		warnings = append(warnings, w)
	}

	return &pendingEvent{ev: ev, warnings: warnings}
}

// peakComputer mirrors pricing.PeakComputer, the way PriceComputer mirrors the
// same seam: an optional refinement a pricer may or may not implement, so the
// assertion below is what decides whether a peak_pricing warning is possible
// at all.
type peakComputer interface {
	PeakAt(model string, at time.Time) bool
}

// peakWarning reports whether ev was billed at a peak rate, as a warning. Two
// conditions gate it: the pricer must implement peakComputer, and the row must
// actually be priced -- an unpriced row was never billed at any rate, so
// claiming it was billed at peak would be a lie.
func peakWarning(pricer PriceComputer, ev *store.Event, at time.Time) (store.Warning, bool) {
	pc, ok := pricer.(peakComputer)
	if !ok || ev.CostSource == "unpriced" || !pc.PeakAt(ev.ModelResolved, at) {
		return store.Warning{}, false
	}
	return store.Warning{
		Kind:      "peak_pricing",
		Severity:  "warn",
		Detail:    fmt.Sprintf("%s billed at peak; the same call off-peak costs less", ev.ModelResolved),
		CreatedAt: at,
	}, true
}

// runAnalyzers runs every registered analyzer, recovering a panic in any
// one of them into an analyzer_panic warning rather than letting it kill
// the consumer.
func (c *Consumer) runAnalyzers(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning {
	var warnings []store.Warning
	for _, a := range c.analyzers {
		warnings = append(warnings, c.runOneAnalyzer(a, meta, usage, ev)...)
	}
	return warnings
}

func (c *Consumer) runOneAnalyzer(a Analyzer, meta parse.Meta, usage parse.Usage, ev *store.Event) (warnings []store.Warning) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("consumer: analyzer %T panicked: %v", a, r)
			warnings = []store.Warning{{
				Kind:      "analyzer_panic",
				Severity:  "error",
				Detail:    fmt.Sprintf("%T: %v", a, r),
				CreatedAt: time.Now(),
			}}
		}
	}()
	return a.Analyze(meta, usage, ev)
}

// fallbackSeqCounter disambiguates synthetic requestID keys across every
// call this process handles. Package-level (not per-Consumer) so a consumer
// that is rebuilt mid-process still never reuses a disambiguator.
var fallbackSeqCounter = new(uint64)

// requestID resolves the cross-source dedup key (D2): the upstream
// request-id header when the response carried one, then the response
// body's own message id, then a synthetic proxy:<hash>:<started_at_ns>:
// <attempt> key for a call that produced neither. This is the one place
// the identity rule is decided — the hot path (internal/proxy) never
// parses the body, so it cannot resolve past the header tier itself.
func requestID(call *sink.CapturedCall, usage parse.Usage) string {
	if call.RequestIDHeader != "" {
		return call.RequestIDHeader
	}
	if usage.MessageID != "" {
		return usage.MessageID
	}
	return syntheticRequestID(call)
}

// syntheticRequestID builds a proxy:<hash>:<started_at_ns>:<attempt> key.
// The attempt counter is what keeps two byte-identical bodies (e.g. two
// retried attempts of the same call) from collapsing onto the same
// synthetic key, which would destroy the rate_limited/overloaded signal
// those attempts exist to record — started_at_ns alone is not sufficient,
// since a fast enough retry could in principle share a nanosecond
// timestamp.
func syntheticRequestID(call *sink.CapturedCall) string {
	attempt := atomic.AddUint64(fallbackSeqCounter, 1)
	sum := sha256.Sum256(call.ReqBody)
	return fmt.Sprintf("proxy:%x:%d:%d", sum, call.StartedAt.UnixNano(), attempt)
}

// buildEvent assembles a store.Event from a captured call's status/timings
// plus the extracted meta and usage. Cost, session, and account are filled
// in by the caller afterward.
func buildEvent(call *sink.CapturedCall, meta parse.Meta, usage parse.Usage) *store.Event {
	endedAt := call.StartedAt.Add(call.Duration)
	ev := &store.Event{
		EventSummary: store.EventSummary{
			RequestID:          requestID(call, usage),
			Source:             "proxy",
			FirstSource:        "proxy",
			StartedAt:          call.StartedAt,
			EndedAt:            &endedAt,
			AuthKind:           call.AuthKind,
			ModelRequested:     meta.ModelRequested,
			ModelResolved:      usage.Model,
			InputTokens:        usage.InputTokens,
			OutputTokens:       usage.OutputTokens,
			CacheWrite5mTokens: usage.CacheWrite5mTokens,
			CacheWrite1hTokens: usage.CacheWrite1hTokens,
			CacheReadTokens:    usage.CacheReadTokens,
			ThinkingTokens:     usage.ThinkingTokens,
			ServiceTier:        usage.ServiceTier,
			Speed:              usage.Speed,
			Effort:             meta.Effort,
			InferenceGeo:       meta.InferenceGeo,
			StopReason:         usage.StopReason,
			StopCategory:       usage.StopCategory,
			IsSidechain:        meta.IsSidechain,
			Project:            meta.Project,
			GitBranch:          meta.GitBranch,
			ClientVersion:      meta.ClientVersion,
			CliEntrypoint:      meta.CliEntrypoint,
			PrefixHash:         meta.PrefixHash,
			CaptureComplete:    call.CaptureComplete,
			ReplayOf:           call.ReplayOf,
			ReplayEdits:        call.ReplayEdits,
			Method:             call.Method,
			Path:               call.Path,
			Status:             call.Status,
		},
		ReqBody:  call.ReqBody,
		RespBody: call.RespBody,
	}
	if usage.Model == "" {
		ev.ModelResolved = meta.ModelRequested
	}
	if call.ReqHeaders != nil {
		ev.ReqHeaders = headerJSON(call.ReqHeaders)
	}
	if call.RespHeaders != nil {
		ev.RespHeaders = headerJSON(call.RespHeaders)
	}
	return ev
}

// resolveAccount picks the configured account whose billing_mode matches
// authKind's implied mode. auth_kind alone cannot distinguish between two
// configured accounts of the same billing mode (v1 has no other per-call
// signal to key on), so this is a best-effort match, not an identity
// lookup: the first configured account of the implied mode wins, and an
// unmatched authKind still resolves a billing_mode with no account name.
func resolveAccount(accounts []config.Account, authKind string) (name, billingMode string) {
	wantMode := billingModeForAuthKind(authKind)
	for _, a := range accounts {
		if a.BillingMode == wantMode {
			return a.Name, a.BillingMode
		}
	}
	return "", wantMode
}

func headerJSON(h http.Header) string {
	b, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(b)
}

func billingModeForAuthKind(authKind string) string {
	switch authKind {
	case "oauth", "cloud":
		return "subscription"
	case "api_key", "admin":
		return "api"
	default:
		return ""
	}
}
