// Package adminrep is source D's collector: the Admin API usage and cost
// reports, the only source that carries an actually-billed dollar figure.
// It uses raw stdlib net/http with the Admin API key -- the Admin
// usage/cost report endpoints are documented as curl-only and are
// explicitly not covered by any Anthropic SDK, so this package must not
// gain an SDK dependency to "simplify" it later.
//
// The exact endpoint paths and response shapes are UNVERIFIED against a
// live Admin API key, the same caveat internal/snapshot carries for
// claude.ai's own internal endpoint: every field is extracted permissively
// against an injectable base URL per report, rather than asserted as fact
// anywhere in production code. Before wiring this collector into
// cmd/clens for real polling, confirm the actual paths and field names
// against a real organization Admin API key.
package adminrep

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Store is the admin-table + ingest_state slice this collector needs.
type Store interface {
	UpsertAdminUsageDays(ctx context.Context, days []store.AdminUsageDay) error
	UpsertAdminCostDays(ctx context.Context, days []store.AdminCostDay) error
	UpsertAdminRateLimits(ctx context.Context, limits []store.AdminRateLimit) error
	SetIngestState(ctx context.Context, key string, state store.IngestState) error
}

// HTTPClient is the seam a test substitutes with an httptest.Server's
// client; production wires *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Collector pulls all three Admin API reports for one organization.
type Collector struct {
	usageURL, costURL, rateLimitsURL string
	client                           HTTPClient
	st                                Store
	now                               func() time.Time
	// credential defaults to secret.Get("admin") -- the one place this
	// package (per internal/secret's own doc comment) may read the Admin
	// key. Overridable only so a test can avoid exercising secret's real
	// OS-level ACL machinery just to reach the "credential is set" path;
	// production never overrides it.
	credential func() (string, error)
}

// New returns a Collector pulling usageURL/costURL/rateLimitsURL with the
// Admin key on every collect.
func New(usageURL, costURL, rateLimitsURL string, st Store) *Collector {
	return &Collector{
		usageURL: usageURL, costURL: costURL, rateLimitsURL: rateLimitsURL,
		client:     http.DefaultClient,
		st:         st,
		now:        time.Now,
		credential: func() (string, error) { return secret.Get("admin") },
	}
}

func (c *Collector) SetHTTPClient(h HTTPClient) { c.client = h }

// Result reports one collect call's outcome, for logging and tests.
type Result struct {
	Rows   int
	Status string // "ok" | "unauthorized" | "unavailable" | "unconfigured"
}

const anthropicVersion = "2023-06-01"

// CollectUsage fetches and UPSERTs admin_usage_days for [since, until).
// Like internal/snapshot's Poll, it never returns an error for a
// reachable-but-broken endpoint -- unauthorized, unreachable, and
// unparseable are all fail-soft states reported via Result.Status, not Go
// errors, so a caller's collector loop never needs special-case handling
// to keep the other sources running (CLAUDE.md invariant 6). It returns an
// error only for a store write failure, since that is this package's own
// bug, not the remote endpoint's.
func (c *Collector) CollectUsage(ctx context.Context, since, until time.Time) (Result, error) {
	apiKey, err := c.credential()
	if err != nil {
		return Result{Status: "unconfigured"}, nil
	}

	buckets, status := fetchAllPages(ctx, c.client, c.usageURL, apiKey, since, until)
	if status != "ok" {
		return Result{Status: status}, nil
	}

	fetchedAt := c.now()
	var days []store.AdminUsageDay
	for _, b := range buckets {
		dayStart, windowStart, windowEnd, ok := bucketTimes(b)
		if !ok {
			continue
		}
		for _, raw := range b.Results {
			days = append(days, parseUsageResult(dayStart, windowStart, windowEnd, raw, fetchedAt))
		}
	}
	if len(days) > 0 {
		if err := c.st.UpsertAdminUsageDays(ctx, days); err != nil {
			return Result{}, fmt.Errorf("adminrep: upsert usage days: %w", err)
		}
	}
	if err := c.saveCursor(ctx, "admin:usage", until); err != nil {
		log.Printf("adminrep: save usage cursor: %v", err)
	}
	return Result{Rows: len(days), Status: "ok"}, nil
}

// CollectCost fetches and UPSERTs admin_cost_days for [since, until). Same
// fail-soft contract as CollectUsage.
func (c *Collector) CollectCost(ctx context.Context, since, until time.Time) (Result, error) {
	apiKey, err := c.credential()
	if err != nil {
		return Result{Status: "unconfigured"}, nil
	}

	buckets, status := fetchAllPages(ctx, c.client, c.costURL, apiKey, since, until)
	if status != "ok" {
		return Result{Status: status}, nil
	}

	fetchedAt := c.now()
	var days []store.AdminCostDay
	for _, b := range buckets {
		dayStart, windowStart, windowEnd, ok := bucketTimes(b)
		if !ok {
			continue
		}
		for _, raw := range b.Results {
			days = append(days, parseCostResult(dayStart, windowStart, windowEnd, raw, fetchedAt))
		}
	}
	if len(days) > 0 {
		if err := c.st.UpsertAdminCostDays(ctx, days); err != nil {
			return Result{}, fmt.Errorf("adminrep: upsert cost days: %w", err)
		}
	}
	if err := c.saveCursor(ctx, "admin:cost", until); err != nil {
		log.Printf("adminrep: save cost cursor: %v", err)
	}
	return Result{Rows: len(days), Status: "ok"}, nil
}

// CollectRateLimits fetches and replaces admin_rate_limits -- rate limits
// have no history to accumulate, only "the latest report" (store.
// UpsertAdminRateLimits is delete-then-insert per scope for exactly this
// reason).
func (c *Collector) CollectRateLimits(ctx context.Context) (Result, error) {
	apiKey, err := c.credential()
	if err != nil {
		return Result{Status: "unconfigured"}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.rateLimitsURL, nil)
	if err != nil {
		return Result{Status: "unavailable"}, nil
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := c.client.Do(req)
	if err != nil {
		// Never log err.Error() here -- a transport error can embed the
		// request, which carries the Admin key header.
		return Result{Status: "unavailable"}, nil
	}
	body, readErr := drainAndClose(resp)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Result{Status: "unauthorized"}, nil
	case resp.StatusCode != http.StatusOK || readErr != nil:
		return Result{Status: "unavailable"}, nil
	}

	var rl rateLimitsResponse
	if err := json.Unmarshal(body, &rl); err != nil {
		return Result{Status: "unavailable"}, nil
	}

	fetchedAt := c.now()
	limits := make([]store.AdminRateLimit, 0, len(rl.Limits))
	for _, raw := range rl.Limits {
		limits = append(limits, parseRateLimit(raw, fetchedAt))
	}
	if err := c.st.UpsertAdminRateLimits(ctx, limits); err != nil {
		return Result{}, fmt.Errorf("adminrep: upsert rate limits: %w", err)
	}
	return Result{Rows: len(limits), Status: "ok"}, nil
}

// CollectAll runs all three reports for [since, until) and combines their
// outcome: Rows is the sum, Status is "ok" only when every report was, and
// otherwise the first non-ok status encountered (usage, then cost, then
// rate limits) -- the entry point br-GI-1-14's ingest orchestration drives.
func (c *Collector) CollectAll(ctx context.Context, since, until time.Time) (Result, error) {
	usage, err := c.CollectUsage(ctx, since, until)
	if err != nil {
		return Result{}, err
	}
	cost, err := c.CollectCost(ctx, since, until)
	if err != nil {
		return Result{}, err
	}
	rateLimits, err := c.CollectRateLimits(ctx)
	if err != nil {
		return Result{}, err
	}

	total := Result{Rows: usage.Rows + cost.Rows + rateLimits.Rows, Status: "ok"}
	for _, r := range []Result{usage, cost, rateLimits} {
		if r.Status != "ok" {
			total.Status = r.Status
			break
		}
	}
	return total, nil
}

func (c *Collector) saveCursor(ctx context.Context, key string, at time.Time) error {
	return c.st.SetIngestState(ctx, key, store.IngestState{
		Value:  at.UTC().Format(time.RFC3339),
		Status: "ok",
	})
}

// fetchAllPages GETs baseURL for [since, until), following has_more/
// next_page until the report is exhausted. status is "ok" only once every
// page succeeded; any failure mid-pagination reports that failure and
// discards whatever partial data was already read, since a partial usage
// or cost window is worse than none -- the next collect retries the whole
// window and UPSERTs make that safe.
func fetchAllPages(ctx context.Context, client HTTPClient, baseURL, apiKey string, since, until time.Time) ([]bucket, string) {
	var all []bucket
	pageCursor := ""
	for {
		req, err := buildRequest(ctx, baseURL, apiKey, since, until, pageCursor)
		if err != nil {
			return nil, "unavailable"
		}
		resp, err := client.Do(req)
		if err != nil {
			// Never log err.Error() here -- a transport error can embed
			// the request, which carries the Admin key header.
			return nil, "unavailable"
		}
		body, readErr := drainAndClose(resp)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, "unauthorized"
		}
		if resp.StatusCode != http.StatusOK || readErr != nil {
			return nil, "unavailable"
		}

		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, "unavailable"
		}
		all = append(all, p.Data...)
		if !p.HasMore || p.NextPage == "" {
			break
		}
		pageCursor = p.NextPage
	}
	return all, "ok"
}

func buildRequest(ctx context.Context, baseURL, apiKey string, since, until time.Time, pageCursor string) (*http.Request, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("adminrep: parse url: %w", err)
	}
	q := u.Query()
	q.Set("starting_at", since.UTC().Format(time.RFC3339))
	q.Set("ending_at", until.UTC().Format(time.RFC3339))
	if pageCursor != "" {
		q.Set("page", pageCursor)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	return req, nil
}

func drainAndClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
