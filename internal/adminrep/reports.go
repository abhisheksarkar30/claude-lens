package adminrep

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// page is one paginated response's envelope, shared by the usage and cost
// report endpoints: a list of time buckets plus a cursor for the next page.
type page struct {
	Data     []bucket `json:"data"`
	HasMore  bool     `json:"has_more"`
	NextPage string   `json:"next_page"`
}

// bucket is one page's time-bucketed slice. Results is left as raw maps
// (not typed structs) so a renamed or added field never breaks decoding --
// parseUsageResult/parseCostResult extract permissively, the same
// tolerant-parsing discipline internal/snapshot applies to its own
// unverified endpoint.
type bucket struct {
	StartingAt string           `json:"starting_at"`
	EndingAt   string           `json:"ending_at"`
	Results    []map[string]any `json:"results"`
}

// rateLimitsResponse is the (also unverified) rate-limit report's envelope
// -- a flat list, not bucketed by day.
type rateLimitsResponse struct {
	Limits []map[string]any `json:"limits"`
}

// bucketTimes parses a bucket's own window into (dayStart, windowStart,
// windowEnd). dayStart is windowStart truncated to a UTC calendar day --
// the natural key every admin table groups on. A bucket whose starting_at
// does not parse is reported via ok=false so the caller can skip just that
// bucket instead of failing the whole page.
func bucketTimes(b bucket) (dayStart, windowStart, windowEnd time.Time, ok bool) {
	start, err := time.Parse(time.RFC3339, b.StartingAt)
	if err != nil {
		return time.Time{}, time.Time{}, time.Time{}, false
	}
	end, err := time.Parse(time.RFC3339, b.EndingAt)
	if err != nil {
		end = start
	}
	return start.UTC().Truncate(24 * time.Hour), start.UTC(), end.UTC(), true
}

// parseUsageResult extracts one admin_usage_days row from a usage report
// result entry. A field this collector cannot find under any known alias
// is left at its zero value rather than aborting the row -- a partial row
// is still useful, and the raw JSON is kept verbatim in Raw for a human to
// check.
func parseUsageResult(dayStart, windowStart, windowEnd time.Time, raw map[string]any, fetchedAt time.Time) store.AdminUsageDay {
	rawJSON, _ := json.Marshal(raw)
	d := store.AdminUsageDay{
		DayStart: dayStart, WindowStart: windowStart, WindowEnd: windowEnd,
		Raw: string(rawJSON), FetchedAt: fetchedAt,
	}
	if v, ok := firstString(raw, "model"); ok {
		d.Model = v
	}
	if v, ok := firstString(raw, "workspace_id"); ok {
		d.WorkspaceID = v
	}
	if v, ok := firstFloat(raw, "uncached_input_tokens", "input_tokens"); ok {
		d.InputTokens = int(v)
	}
	if v, ok := firstFloat(raw, "output_tokens"); ok {
		d.OutputTokens = int(v)
	}
	if v, ok := firstFloat(raw, "cache_read_input_tokens", "cache_read_tokens"); ok {
		d.CacheReadTokens = int(v)
	}
	// admin_usage_days has one combined cache-write column (unlike events'
	// 5m/1h split) -- both cache_creation tiers, and any flat alias, fold
	// into it.
	writeTokens := 0
	if v, ok := nestedFloat(raw, "cache_creation", "ephemeral_5m_input_tokens"); ok {
		writeTokens += int(v)
	}
	if v, ok := nestedFloat(raw, "cache_creation", "ephemeral_1h_input_tokens"); ok {
		writeTokens += int(v)
	}
	if v, ok := firstFloat(raw, "cache_write_tokens", "cache_creation_input_tokens"); ok {
		writeTokens += int(v)
	}
	d.CacheWriteTokens = writeTokens
	return d
}

// parseCostResult extracts one admin_cost_days row. Currency defaults to
// "USD" when the result omits it -- amount_usd's own column name assumes
// USD unless told otherwise.
func parseCostResult(dayStart, windowStart, windowEnd time.Time, raw map[string]any, fetchedAt time.Time) store.AdminCostDay {
	rawJSON, _ := json.Marshal(raw)
	d := store.AdminCostDay{
		DayStart: dayStart, WindowStart: windowStart, WindowEnd: windowEnd,
		Raw: string(rawJSON), FetchedAt: fetchedAt, Currency: "USD",
	}
	if v, ok := firstString(raw, "model"); ok {
		d.Model = v
	}
	if v, ok := firstString(raw, "description"); ok {
		d.Description = v
	}
	if v, ok := firstString(raw, "currency"); ok && v != "" {
		d.Currency = v
	}
	if v, ok := firstNumber(raw, "amount", "amount_usd", "cost"); ok {
		d.AmountUSD = v
	}
	return d
}

// parseRateLimit extracts one admin_rate_limits row.
func parseRateLimit(raw map[string]any, fetchedAt time.Time) store.AdminRateLimit {
	l := store.AdminRateLimit{FetchedAt: fetchedAt}
	if v, ok := firstString(raw, "scope"); ok {
		l.Scope = v
	}
	if v, ok := firstString(raw, "workspace_id"); ok {
		l.WorkspaceID = v
	}
	if v, ok := firstString(raw, "model"); ok {
		l.Model = v
	}
	if v, ok := firstString(raw, "group_type", "type"); ok {
		l.GroupType = v
	}
	if v, ok := firstNumber(raw, "limit", "limit_value"); ok {
		l.Limit = int(v)
	}
	return l
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

// firstNumber is firstFloat plus a string fallback ("1.23"): the cost
// report's amount field is not confirmed to be numeric-typed JSON.
func firstNumber(m map[string]any, keys ...string) (float64, bool) {
	if f, ok := firstFloat(m, keys...); ok {
		return f, true
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func nestedFloat(m map[string]any, parentKey string, keys ...string) (float64, bool) {
	v, ok := m[parentKey]
	if !ok {
		return 0, false
	}
	nested, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	return firstFloat(nested, keys...)
}
