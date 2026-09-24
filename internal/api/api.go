// Package api is the dashboard's JSON API (plus SSE push): a mostly-read
// surface over internal/store, served by the dashboard's own http.Server
// (CLAUDE.md: "the dashboard listener reads SQLite and pushes SSE").
//
// The read (GET) routes, the SSE broker and the PublishingStore decorator are
// Slice A of br-GI-1-16; the write routes (POST /api/prices,
// /api/requests/{id}/replay), the injected write seams, the Origin/Host
// allowlist and internal/web's asset mount are Slice B. br-GI-1-18 adds eight
// more routes on top: five reads (sources, quota, accounts, models,
// reconcile) and three writes (accounts, secrets, ingest).
//
// Credential containment (br-GI-1-16 test 18) is a mechanical property of this
// package, not a review habit: it never imports internal/secret. A route that
// has to write a credential reaches the write through an injected seam
// (SetCredentialWriter and friends), so there is no import edge a future
// handler could accidentally use to read one back.
//
// The same containment applies to internal/config and internal/ingest, which
// are the two other packages a route here would otherwise have to import
// (asserted by internal/cli/serve_test.go). GET /api/sources and
// GET /api/accounts read through SetSourceHealth and SetAccounts for exactly
// that reason.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/decode"
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
	ListEvents(ctx context.Context, f store.EventFilter) ([]*store.EventSummary, error)

	// ListEventsFull is the replay poll's read: replay.OutcomeOf takes a whole
	// row, so the poll -- one row per interval, not fifty per tab switch --
	// names the full-width read explicitly.
	ListEventsFull(ctx context.Context, f store.EventFilter) ([]*store.Event, error)
	CountEvents(ctx context.Context, f store.EventFilter) (int, error)
	EventWarnings(ctx context.Context, eventID int64) ([]store.Warning, error)
	ListWarnings(ctx context.Context, f store.WarningFilter) ([]store.Warning, error)
	CountWarnings(ctx context.Context, f store.WarningFilter) (int, error)
	WarningSummary(ctx context.Context) ([]store.WarningSummary, error)
	GetSession(ctx context.Context, id string) (*store.Session, error)
	ListSessions(ctx context.Context, limit, offset int) ([]*store.Session, error)
	CountSessions(ctx context.Context) (int, error)
	SessionEventsSummary(ctx context.Context, sessionID string) ([]*store.EventSummary, error)
	StatsSummary(ctx context.Context, f store.EventFilter) (store.StatsSummary, error)
	StatsByModel(ctx context.Context, f store.EventFilter) ([]store.ModelStats, error)
	StatsByPeriod(ctx context.Context, f store.EventFilter, granularity string) ([]store.PeriodStats, error)
	StatsByCostSource(ctx context.Context, f store.EventFilter) ([]store.CostSourceStats, error)

	// br-GI-1-18's two additions. Both were already on *store.Store; they
	// were absent here only because no route read them yet.
	ListQuotaSnapshots(ctx context.Context, account string, limit int) ([]store.QuotaSnapshot, error)
	ListAdminCostDays(ctx context.Context, since, until time.Time) ([]store.AdminCostDay, error)
}

type api struct {
	store    Store
	sink     *sink.Sink
	consumer *consumer.Consumer
	broker   *Broker
	mux      *http.ServeMux

	// priceLoader backs GET /api/prices and, through its Path, the write
	// POST /api/prices performs. nil means unwired: both routes answer 503,
	// exactly as deepseek-lens's do without SetPricing.
	priceLoader *pricing.Loader

	// proxyHandler is the live proxy's own Handler. The replay endpoint sends
	// through it rather than through a transport of its own, so a replay picks
	// up the same transport, tee, body cap and header redaction as live
	// traffic and reaches SQLite through the consumer's single writer.
	// replayEnabled is config.ReplayEnabled -- the endpoint's opt-in control.
	proxyHandler  http.Handler
	replayEnabled bool

	// replayRejected counts every replay turned away by either guard:
	// disabled-by-default, or the Origin/Host allowlist. Without it a rejected
	// probe against the one billable route leaves no server-side trace at all.
	// Surfaced on /api/health alongside the other counters.
	replayRejected atomic.Uint64

	// The three write seams br-GI-1-17's composition root wires. They are
	// declared here, in the bead that owns the write routes, because a seam's
	// setter is a package-level capability with no route attached -- which is
	// what lets the setter ship a bead before br-GI-1-18 adds the routes that
	// call it, with no 17 -> 18 dependency edge. An unwired seam is a
	// supported state: the route that consumes it answers 503, never a nil
	// dereference.
	credentialWriter func(name, value string) error
	accountWriter    func() error
	ingestTrigger    func(ctx context.Context) error

	// The two read seams br-GI-1-18 adds. They exist for the same
	// containment reason as the write seams above, one layer out: a
	// collector's health lives in internal/ingest and an account's plan in
	// internal/config, and this package may import neither. So /api/sources
	// and /api/accounts are declared against api-local types and the
	// composition root converts.
	//
	// Unlike the write seams, an unwired read seam is not silently empty --
	// an empty source list and a broken one look identical in a UI. Both
	// routes answer 503 instead, and the tab shows the error.
	sourceHealth func(ctx context.Context) ([]SourceHealth, error)
	accounts     func(ctx context.Context) (Accounts, error)

	// bodyCapBytes is the read-path decode cap, injected by SetBodyCapBytes.
	// 0 means unwired, which is a supported state -- see decodeRespBody.
	bodyCapBytes int

	// proxyMode backs GET /api/mode. It is a seam for the same reason
	// sourceHealth is one: the settings read lives in internal/cli, and
	// internal/cli already imports this package, so the reverse edge is a
	// build cycle rather than a guard violation.
	proxyMode func(ctx context.Context) (ProxyMode, error)

	// shutdownFunc backs POST /api/shutdown (br-GI-13-09): the same cancel
	// func Ctrl+C drives, wired by SetShutdown from the composition root.
	// Unset is supported: the route answers 503 rather than a nil call.
	shutdownFunc func()

	// reloadFunc backs POST /api/reload (br-GI-16-04), wired by SetReload.
	reloadFunc func(context.Context) (ReloadReport, error)
}

// SetPricing wires GET/POST /api/prices to loader's table and override file.
// Leaving it unset is a supported state (both routes answer 503), not a nil
// dereference.
func (a *api) SetPricing(loader *pricing.Loader) { a.priceLoader = loader }

// SetCredentialWriter wires the route that stores a credential to fn. This
// package never imports internal/secret; the composition root does, and hands
// the write in as a value.
func (a *api) SetCredentialWriter(fn func(name, value string) error) { a.credentialWriter = fn }

// SetAccountWriter wires the route that saves the accounts file to fn.
func (a *api) SetAccountWriter(fn func() error) { a.accountWriter = fn }

// SetIngestTrigger wires the route that runs every collector once to fn.
func (a *api) SetIngestTrigger(fn func(ctx context.Context) error) { a.ingestTrigger = fn }

// SetSourceHealth wires GET /api/sources to fn, which reads per-collector
// health out of internal/ingest. Leaving it unset is supported: the route
// answers 503 rather than an empty list.
func (a *api) SetSourceHealth(fn func(ctx context.Context) ([]SourceHealth, error)) {
	a.sourceHealth = fn
}

// SetAccounts wires GET /api/accounts (and the subscription half of
// GET /api/quota) to fn, which reads the configured accounts out of
// internal/config. Unset is supported: both routes answer 503.
func (a *api) SetAccounts(fn func(ctx context.Context) (Accounts, error)) { a.accounts = fn }

// ServeHTTP delegates to the stored mux, so *api satisfies http.Handler.
func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// New builds the dashboard's http.Handler: a mostly-read JSON API under
// /api/* (including the /api/stream SSE endpoint) plus internal/web's
// embedded assets at every other path. Handlers are thin -- parse query
// params into a store filter, call st, encode JSON -- with no business logic
// here; grouping that has to be correct over the whole table (the warning
// inbox's per-kind totals) therefore lives in SQL, in store.WarningSummary.
//
// Two routes write. POST /api/requests/{id}/replay re-issues a captured
// request through proxyHandler, so the replay is proxied, teed and recorded
// by exactly the code that handles live traffic; POST /api/prices writes the
// price-override file through the Loader SetPricing wired. A nil
// proxyHandler disables replay's send path, which is what tests before
// replay's own rely on.
func New(st Store, sk *sink.Sink, cons *consumer.Consumer, broker *Broker, assets fs.FS, proxyHandler http.Handler, replayEnabled bool) *api {
	a := &api{store: st, sink: sk, consumer: cons, broker: broker, proxyHandler: proxyHandler, replayEnabled: replayEnabled}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/requests", methodGet(a.listRequests))
	mux.HandleFunc("/api/requests/{id}", methodGet(a.getRequest))
	mux.HandleFunc("POST /api/requests/{id}/replay", a.replay)
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
	mux.HandleFunc("POST /api/prices", a.setPrices)

	// br-GI-1-18. The five reads are methodGet-wrapped; the three writes each
	// run originReject before anything else (see secrets.go).
	mux.HandleFunc("/api/sources", methodGet(a.sources))
	mux.HandleFunc("/api/quota", methodGet(a.quota))
	mux.HandleFunc("/api/accounts", methodGet(a.listAccounts))
	mux.HandleFunc("/api/models", methodGet(a.models))
	mux.HandleFunc("/api/reconcile", methodGet(a.reconcile))
	mux.HandleFunc("/api/mode", methodGet(a.mode))
	mux.HandleFunc("POST /api/accounts", a.saveAccounts)
	mux.HandleFunc("POST /api/secrets", a.setSecret)
	mux.HandleFunc("POST /api/ingest", a.triggerIngest)
	mux.HandleFunc("POST /api/shutdown", a.shutdown)
	mux.HandleFunc("POST /api/reload", a.reload)

	mux.Handle("/", http.FileServer(http.FS(assets)))
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

// parseBoolParam reads the named query param as a boolean. Absent means
// false; the accepted spellings are strconv.ParseBool's (1, t, T, TRUE, true,
// True, 0, f, F, FALSE, false, False). Anything else is rejected rather than
// guessed at -- ?no_capture=yes silently meaning false would send a call the
// caller asked not to record.
func parseBoolParam(r *http.Request, key string) (bool, error) {
	s := r.URL.Query().Get(key)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: want a boolean", key, s)
	}
	return b, nil
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
// fields (promoted from the embedded pointer) plus its warnings attached,
// plus the decoded response body and the two fields that keep it honest.
type eventDetail struct {
	*store.Event
	Warnings []store.Warning `json:"warnings"`

	// RespBodyDecoded is the response bytes the view renders: the decoded
	// form when the cap was wired and decoding ran, and the raw RespBody
	// otherwise. Equalling RespBody does NOT by itself mean "undecoded" --
	// that is also true of a body with no Content-Encoding.
	RespBodyDecoded []byte `json:"RespBodyDecoded"`
	// RespBodyCompleteness is what decides the view's marker.
	// NotDecoded means RespBodyDecoded is RespBody unchanged.
	RespBodyCompleteness decode.Completeness `json:"RespBodyCompleteness"`
	// BodyCapBytes == 0 means the read cap is unwired -- never a zero-byte
	// cap. The view selects its "cap not configured" line off this *before*
	// consulting RespBodyCompleteness, so a missing cap can never manufacture
	// the "would not decompress" state.
	BodyCapBytes int `json:"BodyCapBytes"`
}

// decodeRespBody returns the bytes the detail view should render for a stored
// response, how complete they are, and the cap they were read under.
//
// Decoding is display-only and never rewrites the stored row: replay sends
// orig.ReqBody/orig.RespHeaders straight from the row, so nothing here has a
// route back to a replay.
func (a *api) decodeRespBody(ev *store.Event) ([]byte, decode.Completeness, int) {
	if a.bodyCapBytes <= 0 {
		// The seam is unwired. Do not call decode.Body: with a non-positive
		// limit it returns the raw body *and* an error, which the view would
		// render as "would not decompress" for a body that decodes fine.
		// BodyCapBytes == 0 is what says so. The bytes served are the whole
		// stored body, so Complete is accurate and draws no marker.
		return ev.RespBody, decode.Complete, 0
	}
	// A malformed header blob degrades to an empty header set, which carries
	// no Content-Encoding and is therefore Complete: the body is shown raw,
	// never a 500.
	hdr := http.Header{}
	if ev.RespHeaders != "" {
		_ = json.Unmarshal([]byte(ev.RespHeaders), &hdr)
	}
	decoded, _, completeness, err := decode.Body(hdr, ev.RespBody, a.bodyCapBytes)
	if err != nil {
		return ev.RespBody, decode.NotDecoded, a.bodyCapBytes
	}
	return decoded, completeness, a.bodyCapBytes
}

// SetBodyCapBytes wires the read-path decode cap. A non-positive n is
// ignored, so leaving the seam unwired is a supported state and a zero can
// never reach decode.Body.
//
// The cap has to be injected because this package cannot read it: the
// configured BodyCapBytes lives in internal/config, which the import guard
// bans, and the cap's default is unexported in internal/consumer, so the
// consumer import this package already holds still buys no reachable
// fallback.
func (a *api) SetBodyCapBytes(n int) {
	if n > 0 {
		a.bodyCapBytes = n
	}
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

	decoded, completeness, cap := a.decodeRespBody(ev)
	writeJSON(w, http.StatusOK, eventDetail{
		Event:                ev,
		Warnings:             warnings,
		RespBodyDecoded:      decoded,
		RespBodyCompleteness: completeness,
		BodyCapBytes:         cap,
	})
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
	// Calls is chronological: store.SessionEventsSummary already returns
	// oldest-first (bounded by the session resolver's own gap window), so
	// unlike deepseek-lens's getSession this needs no reversal. Summary rows,
	// not full ones: this route renders a call list, and a session's rows can
	// run to fifty 1 MB bodies the list never shows.
	Calls []*store.EventSummary `json:"calls"`
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

	calls, err := a.store.SessionEventsSummary(r.Context(), id)
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
	// ReplayRejected counts replay attempts turned away by either guard. It is
	// the only server-side trace a rejected probe leaves -- see replayRejected.
	ReplayRejected uint64 `json:"replay_rejected"`
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
		ReplayRejected:    a.replayRejected.Load(),
	}
	if !cs.LastWriteAt.IsZero() {
		t := cs.LastWriteAt
		resp.LastWriteAt = &t
		resp.LastWriteAgeMs = time.Since(t).Milliseconds()
	}
	writeJSON(w, http.StatusOK, resp)
}
