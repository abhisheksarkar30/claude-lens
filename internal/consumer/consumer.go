// Package consumer is the cold-path bridge: one goroutine that drains the
// sink and writes rows. The order below is fixed; later beads plug into
// the named seams (Analyzer, SessionResolver, SessionAggregator,
// PriceComputer) rather than rewriting the pipeline.
package consumer

import (
	"context"
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
	defaultBodyCapBytes  = 262144
	defaultBatchSize     = 50
	defaultFlushInterval = 250 * time.Millisecond
	defaultShutdownGrace = 2 * time.Second
)

// Store is the slice of *store.Store the consumer actually calls --
// narrowed to an interface so a test can inject a store that fails on
// command without a second SQLite implementation.
type Store interface {
	InsertEvent(ctx context.Context, ev *store.Event) (int64, error)
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
	for _, pe := range batch {
		id, err := c.st.InsertEvent(ctx, pe.ev)
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

		warningCount := len(pe.warnings)
		if c.sessionRule != nil && pe.ev.SessionID != "" {
			warningCount += c.runSessionRule(ctx, pe.ev.SessionID)
		}

		// Run last, after the session-scoped pass, so warning_count's
		// re-derivation (ReconcileSession) counts findings that pass
		// attached to earlier rows in this same session too.
		if c.aggregator != nil && pe.ev.SessionID != "" {
			if err := c.aggregator.RecordCall(ctx, pe.ev.SessionID, pe.ev, warningCount); err != nil {
				log.Printf("consumer: record session call for %s: %v", pe.ev.SessionID, err)
			}
		}
	}
	if wrote {
		c.lastWriteAt.Store(time.Now().UnixNano())
	}
}

// runSessionRule runs the session-scoped analysis pass over sessionID's
// whole row history and upserts each finding onto the row it names,
// grouped so a row with several findings gets one call. It returns how
// many warnings it wrote, folded into the aggregator's warningCount.
func (c *Consumer) runSessionRule(ctx context.Context, sessionID string) int {
	rows, err := c.st.SessionEvents(ctx, sessionID)
	if err != nil {
		log.Printf("consumer: session rows for %s: %v", sessionID, err)
		return 0
	}
	grouped := map[int64][]store.Warning{}
	for _, w := range c.sessionRule.AnalyzeSession(rows) {
		if w.EventID == 0 {
			continue
		}
		grouped[w.EventID] = append(grouped[w.EventID], w)
	}
	n := 0
	for eventID, warnings := range grouped {
		if err := c.st.UpsertWarnings(ctx, eventID, warnings); err != nil {
			log.Printf("consumer: upsert session warnings for event %d: %v", eventID, err)
			continue
		}
		n += len(warnings)
	}
	return n
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
	if decoded, decodedHeaders, err := decode.Body(respHeaders, respBody, c.bodyCapBytes); err == nil {
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

	return &pendingEvent{ev: ev, warnings: warnings}
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

// buildEvent assembles a store.Event from a captured call's status/timings
// plus the extracted meta and usage. Cost, session, and account are filled
// in by the caller afterward.
func buildEvent(call *sink.CapturedCall, meta parse.Meta, usage parse.Usage) *store.Event {
	endedAt := call.StartedAt.Add(call.Duration)
	ev := &store.Event{
		RequestID:          call.RequestID,
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
		Method:             call.Method,
		Path:               call.Path,
		Status:             call.Status,
		ReqBody:            call.ReqBody,
		RespBody:           call.RespBody,
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
