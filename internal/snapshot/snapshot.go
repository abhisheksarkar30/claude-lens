// Package snapshot is source C's collector: a cookie-authenticated poll
// of claude.ai's internal usage endpoint, the only quota truth for a
// subscription.
//
// The endpoint's exact URL and response shape are UNVERIFIED. It is an
// undocumented internal API with no public spec, so this package parses
// whatever it gets back schema-tolerantly (test 16) against an
// injectable baseURL, rather than asserting a guessed real path as fact
// anywhere in this code. Before this collector is wired into cmd/clens
// for real polling, its caller must confirm the actual endpoint and
// response shape against a live claude.ai session -- the same
// "verify before relying on it" discipline br-GI-1-11 applies to the
// request-id/requestId equivalence.
package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Store is the ingest_state + quota_snapshots slice the poller needs.
type Store interface {
	InsertQuotaSnapshot(ctx context.Context, q store.QuotaSnapshot) error
	GetIngestState(ctx context.Context, key string) (store.IngestState, bool, error)
	SetIngestState(ctx context.Context, key string, state store.IngestState) error
}

// HTTPClient is the seam a test substitutes with an httptest.Server's
// client; production wires *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Poller polls baseURL for one account's quota windows.
type Poller struct {
	baseURL string
	account string
	client  HTTPClient
	st      Store
	now     func() time.Time
	// credential defaults to secret.Get("sessionKey") -- the one place
	// this package (and the whole tree, per internal/secret's own doc
	// comment) may read the cookie. Overridable only so a test can avoid
	// exercising secret's real OS-level ACL machinery just to reach the
	// "credential is set" path; production never overrides it.
	credential func() (string, error)
}

// New returns a Poller for account, GETting baseURL with the sessionKey
// cookie on every poll.
func New(baseURL, account string, st Store) *Poller {
	return &Poller{
		baseURL:    baseURL,
		account:    account,
		client:     http.DefaultClient,
		st:         st,
		now:        time.Now,
		credential: func() (string, error) { return secret.Get("sessionKey") },
	}
}

func (p *Poller) SetHTTPClient(c HTTPClient) { p.client = c }

// Result reports one Poll's outcome, for logging and tests.
type Result struct {
	Rows   int
	Status string // "ok" | "unauthorized" | "unavailable" | "unconfigured"
}

func cursorKey(account string) string { return "snapshot:" + account }

// Poll fetches the account's current windows and writes one
// quota_snapshots row per window (test: three windows, three rows). It
// never returns an error for a reachable-but-broken endpoint --
// unauthorized, unreachable, and unparseable are all fail-soft states
// recorded in the store, not Go errors, so a caller's poll loop never
// needs special-case handling to keep other collectors running
// (CLAUDE.md invariant 6). It returns an error only for a store write
// failure, since that is this package's own bug, not the remote
// endpoint's.
func (p *Poller) Poll(ctx context.Context) (Result, error) {
	sessionKey, err := p.credential()
	if err != nil {
		// Not configured yet -- nothing to poll, nothing to report.
		return Result{Status: "unconfigured"}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("snapshot: build request: %w", err)
	}
	req.Header.Set("Cookie", "sessionKey="+sessionKey)

	resp, err := p.client.Do(req)
	if err != nil {
		// Never log err.Error() here -- a transport error can embed the
		// request, which carries the cookie header.
		return p.recordStatus(ctx, "unavailable", "")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return p.recordStatus(ctx, "unauthorized", "")
	}
	if resp.StatusCode != http.StatusOK {
		return p.recordStatus(ctx, "unavailable", fmt.Sprintf("http %d", resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return p.recordStatus(ctx, "unavailable", "")
	}

	windows, err := parseWindows(body)
	if err != nil {
		return p.recordStatus(ctx, "unavailable", "")
	}

	observedAt := p.now()
	n := 0
	for _, w := range windows {
		q := store.QuotaSnapshot{
			ObservedAt: observedAt,
			Account:    p.account,
			Window:     w.Name,
			Status:     w.Status,
			Raw:        w.Raw,
		}
		if w.Utilization != nil {
			q.UtilizationPct = w.Utilization
		}
		if w.ResetsAt != nil {
			q.ResetsAt = w.ResetsAt
		}
		if err := p.st.InsertQuotaSnapshot(ctx, q); err != nil {
			return Result{}, fmt.Errorf("snapshot: insert quota snapshot: %w", err)
		}
		n++
	}

	if err := p.saveCursor(ctx, observedAt); err != nil {
		log.Printf("snapshot: save cursor: %v", err)
	}
	return Result{Rows: n, Status: "ok"}, nil
}

// recordStatus writes one status-only row (no window) and returns the
// matching Result. detail is caller-controlled and must never be built
// from anything that could carry the sessionKey value.
func (p *Poller) recordStatus(ctx context.Context, status, detail string) (Result, error) {
	q := store.QuotaSnapshot{ObservedAt: p.now(), Account: p.account, Status: status, Raw: detail}
	if err := p.st.InsertQuotaSnapshot(ctx, q); err != nil {
		return Result{}, fmt.Errorf("snapshot: insert status row: %w", err)
	}
	return Result{Status: status}, nil
}

func (p *Poller) saveCursor(ctx context.Context, at time.Time) error {
	return p.st.SetIngestState(ctx, cursorKey(p.account), store.IngestState{
		Value:  strconv.FormatInt(at.UnixNano(), 10),
		Status: "ok",
	})
}

// parsedWindow is one window entry after tolerant extraction.
type parsedWindow struct {
	Name        string
	Utilization *float64
	ResetsAt    *time.Time
	Status      string
	Raw         string
}

type rawResponse struct {
	Windows []map[string]any `json:"windows"`
}

// parseWindows decodes body's top-level shape (only the "windows" array
// is assumed) and extracts each window permissively -- a renamed or
// wrong-typed utilization field can't be found under any of its known
// aliases and that window is flagged parse_error rather than aborting
// the whole poll; a missing optional field (e.g. resets_at) just leaves
// that column nil.
func parseWindows(body []byte) ([]parsedWindow, error) {
	var resp rawResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make([]parsedWindow, 0, len(resp.Windows))
	for _, raw := range resp.Windows {
		out = append(out, parseOneWindow(raw))
	}
	return out, nil
}

func parseOneWindow(raw map[string]any) parsedWindow {
	rawJSON, _ := json.Marshal(raw)
	w := parsedWindow{Status: "ok", Raw: string(rawJSON)}

	name, ok := firstString(raw, "window", "name", "id")
	if !ok {
		w.Status = "parse_error"
		w.Name = "unknown"
		return w
	}
	w.Name = name

	if pct, ok := firstFloat(raw, "utilization_pct", "utilization", "percent", "pct"); ok {
		w.Utilization = &pct
	} else {
		w.Status = "parse_error"
	}

	if resetsAt, ok := firstTime(raw, "resets_at", "reset_at", "resetsAt"); ok {
		w.ResetsAt = &resetsAt
	}
	return w
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

func firstFloat(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if f, ok := v.(float64); ok {
				return f, true
			}
		}
	}
	return 0, false
}

func firstTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}
