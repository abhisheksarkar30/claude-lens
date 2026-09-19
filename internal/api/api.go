// Package api is the dashboard's JSON API (plus SSE push): a mostly-read
// surface over internal/store, served by the dashboard's own http.Server
// (CLAUDE.md: "the dashboard listener reads SQLite and pushes SSE").
//
// This is Slice A of br-GI-1-16: the read (GET) routes, the SSE broker, and
// the PublishingStore decorator only. Write routes (POST /api/prices,
// /api/secrets, /api/accounts, /api/ingest), internal/replay, and
// internal/web's asset mount are a later slice -- see the bead file for the
// full route table this package will eventually carry.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Store is the narrow read slice of *store.Store the API needs -- kept as an
// interface only so api_test.go can seed a real temp store without any
// other indirection (the same reason deepseek-lens's internal/api.Store is
// an interface, not a concrete type).
type Store interface {
	GetEvent(ctx context.Context, id int64) (*store.Event, error)
	ListEvents(ctx context.Context, f store.EventFilter) ([]*store.Event, error)
	CountEvents(ctx context.Context, f store.EventFilter) (int, error)
	EventWarnings(ctx context.Context, eventID int64) ([]store.Warning, error)
	ListWarnings(ctx context.Context, f store.WarningFilter) ([]store.Warning, error)
	CountWarnings(ctx context.Context, f store.WarningFilter) (int, error)
	WarningSummary(ctx context.Context) ([]store.WarningSummary, error)
	GetSession(ctx context.Context, id string) (*store.Session, error)
	ListSessions(ctx context.Context, limit, offset int) ([]*store.Session, error)
	CountSessions(ctx context.Context) (int, error)
	SessionEvents(ctx context.Context, sessionID string) ([]*store.Event, error)
	StatsSummary(ctx context.Context, f store.EventFilter) (store.StatsSummary, error)
	StatsByModel(ctx context.Context, f store.EventFilter) ([]store.ModelStats, error)
	StatsByPeriod(ctx context.Context, f store.EventFilter, granularity string) ([]store.PeriodStats, error)
	StatsByCostSource(ctx context.Context, f store.EventFilter) ([]store.CostSourceStats, error)
}

type api struct {
	store    Store
	sink     *sink.Sink
	consumer *consumer.Consumer
	broker   *Broker
	mux      *http.ServeMux

	// priceLoader backs GET /api/prices. nil means unwired: the route
	// answers 503, exactly as deepseek-lens's does without SetPricing.
	priceLoader *pricing.Loader
}

// SetPricing wires GET /api/prices to loader. Leaving it unset is a
// supported state (the route answers 503), not a nil dereference.
func (a *api) SetPricing(loader *pricing.Loader) { a.priceLoader = loader }

// ServeHTTP delegates to the stored mux, so *api satisfies http.Handler.
func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// New builds the dashboard's read API: st, sk and cons back /api/health;
// broker backs /api/stream and is what PublishingStore publishes to. The
// write routes, the asset mount, and their constructor parameters
// (proxyHandler, assets fs.FS, replayEnabled) land in a later slice --
// adding them here now, unused, is exactly the premature scaffolding
// CLAUDE.md's lazy-engineering discipline argues against.
func New(st Store, sk *sink.Sink, cons *consumer.Consumer, broker *Broker) *api {
	a := &api{store: st, sink: sk, consumer: cons, broker: broker}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/requests", methodGet(a.listRequests))
	mux.HandleFunc("/api/requests/{id}", methodGet(a.getRequest))
	mux.HandleFunc("/api/stats", methodGet(a.stats))
	// Registered before /api/warnings, which as a prefix pattern would
	// otherwise also match this path. Go's ServeMux prefers the more
	// specific pattern either way; the order here is for the reader.
	mux.HandleFunc("/api/warnings/summary", methodGet(a.warningsSummary))
	mux.HandleFunc("/api/warnings", methodGet(a.listWarnings))
	mux.HandleFunc("/api/sessions", methodGet(a.listSessions))
	mux.HandleFunc("/api/sessions/{id}", methodGet(a.getSession))
	mux.HandleFunc("/api/stream", methodGet(a.stream))
	mux.HandleFunc("/api/health", methodGet(a.health))
	mux.HandleFunc("/api/prices", methodGet(a.getPrices))
	a.mux = mux
	return a
}

// methodGet rejects every method but GET with a JSON 405 before h runs.
// Every route in this slice is read-only and loopback-bound; write routes
// (a later slice) are registered separately, each behind originReject.
func methodGet(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// parseLimit reads ?limit, defaulting to 0 (which every store list method
// treats as its own capped default -- never unbounded).
func parseLimit(r *http.Request) (int, error) {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid limit %q: want a non-negative integer", s)
	}
	return n, nil
}

// parseOffset reads ?offset, defaulting to 0. A negative offset is rejected
// rather than clamped, mirroring parseLimit.
func parseOffset(r *http.Request) (int, error) {
	s := r.URL.Query().Get("offset")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid offset %q: want a non-negative integer", s)
	}
	return n, nil
}

// effectiveLimit resolves the page size the store will actually apply, so a
// handler can report it while the store's own clamp is out of reach.
func effectiveLimit(limit int) int {
	if limit <= 0 {
		return store.DefaultLimit
	}
	return limit
}

// writePageHeaders sets the pagination metadata that rides on response
// headers rather than in the body, so list responses stay the bare JSON
// arrays every existing consumer already decodes. It must run before
// writeJSON, which calls WriteHeader -- headers set after that are silently
// dropped.
func writePageHeaders(w http.ResponseWriter, total, limit, offset int) {
	h := w.Header()
	h.Set("X-Total-Count", strconv.Itoa(total))
	h.Set("X-Limit", strconv.Itoa(limit))
	h.Set("X-Offset", strconv.Itoa(offset))
}

// parseTimeBoundParam reads the named query param as a Go duration ("24h")
// or an RFC3339 timestamp. Absent means the zero Time, which the store reads
// as "unbounded on that side".
func parseTimeBoundParam(r *http.Request, key string) (time.Time, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid %s %q: want a duration like 24h or an RFC3339 timestamp", key, s)
}

func parseSinceParam(r *http.Request) (time.Time, error) {
	return parseTimeBoundParam(r, "since")
}

// parseGranularityParam reads ?granularity, defaulting to "day" when absent.
func parseGranularityParam(r *http.Request) (string, error) {
	g := r.URL.Query().Get("granularity")
	switch g {
	case "":
		return "day", nil
	case "day", "week", "month":
		return g, nil
	}
	return "", fmt.Errorf("invalid granularity %q: want day, week, or month", g)
}

func (a *api) listRequests(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	since, err := parseTimeBoundParam(r, "since")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	until, err := parseTimeBoundParam(r, "until")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	f := store.EventFilter{
		Limit: limit, Offset: offset, Since: since, Until: until,
		Source: q.Get("source"), Account: q.Get("account"),
		BillingMode: q.Get("billing_mode"), Model: q.Get("model"),
		SessionID: q.Get("session"),
	}

	events, err := a.store.ListEvents(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The same filter, so the total counts exactly the set the page was
	// drawn from -- CountEvents ignores Limit/Offset itself.
	total, err := a.store.CountEvents(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writePageHeaders(w, total, effectiveLimit(limit), offset)
	writeJSON(w, http.StatusOK, events)
}

// eventDetail is /api/requests/{id}'s response shape: the event's own
// fields (promoted from the embedded pointer) plus its warnings attached.
type eventDetail struct {
	*store.Event
	Warnings []store.Warning `json:"warnings"`
}

func (a *api) getRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}

	ev, err := a.store.GetEvent(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	warnings, err := a.store.EventWarnings(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, eventDetail{Event: ev, Warnings: warnings})
}

type statsResponse struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// Granularity is the bucket size actually applied, echoed since an
	// absent ?granularity= means "day".
	Granularity string             `json:"granularity"`
	Summary     store.StatsSummary `json:"summary"`
	// ByModel and ByPeriod may contain two rows for the same model/period
	// -- one per billing_mode -- rather than one merged row, per
	// invariant 5 (test 12b): store.StatsByModel/StatsByPeriod already
	// group by billing_mode in SQL, so this handler does no extra
	// splitting of its own; it only serializes what the store returns.
	ByModel     []store.ModelStats      `json:"by_model"`
	ByPeriod    []store.PeriodStats     `json:"by_period"`
	CostSources []store.CostSourceStats `json:"cost_sources"`
}

// stats serves the Stats tab's one fetch. since/until are independent
// bounds, an absent one being unbounded on its side; since > until is a
// well-formed empty window and every field below answers "no rows"
// correctly for it.
func (a *api) stats(w http.ResponseWriter, r *http.Request) {
	since, err := parseTimeBoundParam(r, "since")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	until, err := parseTimeBoundParam(r, "until")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	granularity, err := parseGranularityParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f := store.EventFilter{Since: since, Until: until}

	summary, err := a.store.StatsSummary(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byModel, err := a.store.StatsByModel(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byPeriod, err := a.store.StatsByPeriod(r.Context(), f, granularity)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	costSources, err := a.store.StatsByCostSource(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		Since: since, Until: until, Granularity: granularity,
		Summary: summary, ByModel: byModel, ByPeriod: byPeriod, CostSources: costSources,
	})
}

// warningsSummary serves the warning inbox's per-kind totals. There is
// deliberately no limit/offset: store.WarningSummary's GROUP BY result is
// bounded by the number of distinct kinds, not by row count, so it cannot
// grow with the table.
func (a *api) warningsSummary(w http.ResponseWriter, r *http.Request) {
	groups, err := a.store.WarningSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (a *api) listWarnings(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f := store.WarningFilter{Kind: r.URL.Query().Get("kind"), Limit: limit, Offset: offset}

	warnings, err := a.store.ListWarnings(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := a.store.CountWarnings(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writePageHeaders(w, total, effectiveLimit(limit), offset)
	writeJSON(w, http.StatusOK, warnings)
}

func (a *api) listSessions(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	sessions, err := a.store.ListSessions(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := a.store.CountSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writePageHeaders(w, total, effectiveLimit(limit), offset)
	writeJSON(w, http.StatusOK, sessions)
}

// sessionDetail is /api/sessions/{id}'s response shape: the session's own
// fields (promoted from the embedded pointer -- including the two labelled
// cost totals, invariant 5) plus its calls and their combined warnings.
type sessionDetail struct {
	*store.Session
	// Calls is chronological: store.SessionEvents already returns
	// oldest-first (bounded by the session resolver's own gap window), so
	// unlike deepseek-lens's getSession this needs no reversal.
	Calls []*store.Event `json:"calls"`
	// Warnings is the union of every warning raised across the session's
	// calls, one entry per occurrence.
	Warnings []store.Warning `json:"warnings"`
}

func (a *api) getSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := a.store.GetSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	calls, err := a.store.SessionEvents(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var warnings []store.Warning
	for _, c := range calls {
		wn, err := a.store.EventWarnings(r.Context(), c.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		warnings = append(warnings, wn...)
	}

	writeJSON(w, http.StatusOK, sessionDetail{Session: sess, Calls: calls, Warnings: warnings})
}

// stream is the SSE endpoint: it subscribes to the broker and forwards
// every Event as a "data:" line until the client disconnects or the broker
// drops it for being slow.
func (a *api) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	ch, unsubscribe := a.broker.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return // dropped by the broker for being slow
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

type healthResponse struct {
	SinkAccepted      uint64     `json:"sink_accepted"`
	SinkDropped       uint64     `json:"sink_dropped"`
	ConsumerProcessed uint64     `json:"consumer_processed"`
	ConsumerFailed    uint64     `json:"consumer_failed"`
	ConsumerDrained   uint64     `json:"consumer_drained"`
	ConsumerFlushes   uint64     `json:"consumer_flushes"`
	LastWriteAt       *time.Time `json:"last_write_at,omitempty"`
	LastWriteAgeMs    int64      `json:"last_write_age_ms,omitempty"`
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	accepted, dropped := a.sink.Stats()
	cs := a.consumer.Stats()

	resp := healthResponse{
		SinkAccepted:      accepted,
		SinkDropped:       dropped,
		ConsumerProcessed: cs.Processed,
		ConsumerFailed:    cs.Failed,
		ConsumerDrained:   cs.Drained,
		ConsumerFlushes:   cs.FlushCount,
	}
	if !cs.LastWriteAt.IsZero() {
		t := cs.LastWriteAt
		resp.LastWriteAt = &t
		resp.LastWriteAgeMs = time.Since(t).Milliseconds()
	}
	writeJSON(w, http.StatusOK, resp)
}
