package analyze

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// maxCacheBreakpoints is the API's hard limit: a fifth cache_control
// marker in one request is a 400, not a degraded cache.
const maxCacheBreakpoints = 4

// --- Per-event rules -------------------------------------------------

func ruleCacheBreakpointsExceeded(meta parse.Meta, _ parse.Usage, _ *store.Event) (store.Warning, bool) {
	if len(meta.CacheControlSites) <= maxCacheBreakpoints {
		return store.Warning{}, false
	}
	return warning(KindCacheBreakpointsExceeded, SeverityError,
		"more than 4 cache_control breakpoints in one request"), true
}

// ruleCachePrefixBelowMinimum fires when a cache_control marker was
// present but produced no observable cache effect at all -- no write, no
// read. This is the one T1 rule that genuinely needs the captured
// request body: no token stream alone can reveal that a marker was
// present but under the model's minimum cacheable length, because the
// symptom (cache_creation_input_tokens: 0) leaves no other trace in
// usage. The per-model minimum table (512/1024/2048/4096 tokens,
// non-monotonic across generations) explains *why* this happens; it is
// not consulted here because the API's own response already encodes it
// in whether any cache tokens landed.
func ruleCachePrefixBelowMinimum(meta parse.Meta, usage parse.Usage, _ *store.Event) (store.Warning, bool) {
	if !meta.HasCacheControl {
		return store.Warning{}, false
	}
	if usage.CacheWrite5mTokens > 0 || usage.CacheWrite1hTokens > 0 || usage.CacheReadTokens > 0 {
		return store.Warning{}, false
	}
	return warning(KindCachePrefixBelowMinimum, SeverityWarn,
		"a cache_control breakpoint was present but produced no cache write or read"), true
}

// ruleThinkingBudgetRejected fires when a request that sent
// thinking.budget_tokens came back a 400 whose body actually mentions
// thinking -- the direct analogue of deepseek-lens's budget_tokens_ignored,
// except the newest generation fails loudly instead of silently dropping
// the field.
func ruleThinkingBudgetRejected(meta parse.Meta, _ parse.Usage, ev *store.Event) (store.Warning, bool) {
	if !meta.HasThinking || meta.ThinkingBudget == nil {
		return store.Warning{}, false
	}
	if ev.Status != 400 || !bytes.Contains(ev.RespBody, []byte("thinking")) {
		return store.Warning{}, false
	}
	return warning(KindThinkingBudgetRejected, SeverityError,
		"thinking.budget_tokens was rejected with a 400 by a model that no longer accepts it"), true
}

// ruleThinkingDisplayOmitted fires when thinking was requested and billed
// (ThinkingTokens > 0) but the response contains no thinking or
// redacted_thinking content block at all -- display defaulted to
// "omitted" rather than "summarized", so the reasoning was paid for but
// never delivered.
func ruleThinkingDisplayOmitted(meta parse.Meta, usage parse.Usage, ev *store.Event) (store.Warning, bool) {
	if !meta.HasThinking || usage.ThinkingTokens == 0 {
		return store.Warning{}, false
	}
	if bytes.Contains(ev.RespBody, []byte(`"type":"thinking"`)) ||
		bytes.Contains(ev.RespBody, []byte(`"type":"redacted_thinking"`)) {
		return store.Warning{}, false
	}
	return warning(KindThinkingDisplayOmitted, SeverityInfo,
		"thinking tokens were billed but no thinking content was returned (display: omitted)"), true
}

func ruleMaxTokensTruncation(_ parse.Meta, usage parse.Usage, _ *store.Event) (store.Warning, bool) {
	if usage.StopReason != "max_tokens" {
		return store.Warning{}, false
	}
	return warning(KindMaxTokensTruncation, SeverityWarn,
		"stop_reason was max_tokens: the turn was cut off mid-thought"), true
}

func ruleRefusal(_ parse.Meta, usage parse.Usage, _ *store.Event) (store.Warning, bool) {
	if usage.StopReason != "refusal" {
		return store.Warning{}, false
	}
	detail := "stop_reason was refusal"
	if usage.StopCategory != "" {
		detail += " (" + usage.StopCategory + ")"
	}
	return warning(KindRefusal, SeverityWarn, detail), true
}

// ruleStreamIncomplete fires on a streamed response the store recorded as
// an incomplete capture -- CaptureComplete is false exactly when the body
// was truncated or the SSE stream ended without message_stop.
func ruleStreamIncomplete(_ parse.Meta, usage parse.Usage, ev *store.Event) (store.Warning, bool) {
	if !usage.IsStream || ev.CaptureComplete {
		return store.Warning{}, false
	}
	return warning(KindStreamIncomplete, SeverityError,
		"the SSE stream ended without a message_stop event"), true
}

func ruleRateLimited(_ parse.Meta, _ parse.Usage, ev *store.Event) (store.Warning, bool) {
	if ev.Status != 429 {
		return store.Warning{}, false
	}
	detail := "HTTP 429 rate limited"
	h := parseRespHeaders(ev.RespHeaders)
	if ra := h.Get("Retry-After"); ra != "" {
		detail += fmt.Sprintf("; retry-after=%s", ra)
	}
	for k, v := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit-") && len(v) > 0 {
			detail += fmt.Sprintf("; %s=%s", k, v[0])
		}
	}
	return warning(KindRateLimited, SeverityWarn, detail), true
}

func ruleOverloaded(_ parse.Meta, _ parse.Usage, ev *store.Event) (store.Warning, bool) {
	if ev.Status != 529 {
		return store.Warning{}, false
	}
	return warning(KindOverloaded, SeverityError, "HTTP 529 overloaded"), true
}

// ruleUpstreamErrorBody covers one of upstream_error's two triggers: an
// error object arriving inside an HTTP 200 body. The other trigger --
// a transport failure -- has no per-event body to inspect and stays where
// br-GI-1-08 already raises it, directly in consumer.go; both write the
// same kind because they mean the same thing to the reader.
func ruleUpstreamErrorBody(_ parse.Meta, _ parse.Usage, ev *store.Event) (store.Warning, bool) {
	if ev.Status != 200 || len(ev.RespBody) == 0 {
		return store.Warning{}, false
	}
	var probe struct {
		Type  string `json:"type"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(ev.RespBody, &probe) != nil {
		return store.Warning{}, false
	}
	if probe.Type != "error" && probe.Error == nil {
		return store.Warning{}, false
	}
	detail := "error object present in a 200 response body"
	if probe.Error != nil && probe.Error.Message != "" {
		detail += ": " + probe.Error.Message
	}
	return warning(KindUpstreamError, SeverityError, detail), true
}

// ruleAuthKindAnomaly fires when a credential was used against the wrong
// plane: an admin credential against the Messages API, or an api_key
// credential attributed to a subscription-billed account.
func ruleAuthKindAnomaly(_ parse.Meta, _ parse.Usage, ev *store.Event) (store.Warning, bool) {
	switch {
	case ev.AuthKind == "admin" && ev.Path == "/v1/messages":
		return warning(KindAuthKindAnomaly, SeverityWarn,
			"an admin credential was used against the Messages API"), true
	case ev.AuthKind == "api_key" && ev.BillingMode == "subscription":
		return warning(KindAuthKindAnomaly, SeverityWarn,
			"an api_key credential was observed on a subscription-billed account"), true
	default:
		return store.Warning{}, false
	}
}

// ruleAPIEquivalentCost fires on every priced subscription row: what this
// call would have cost at API rates, labelled hypothetical and never
// contributing to cost_usd (invariant 5 -- this rule only ever reads
// ApiEquivalentCostUSD, never writes either cost column).
func ruleAPIEquivalentCost(_ parse.Meta, _ parse.Usage, ev *store.Event) (store.Warning, bool) {
	if ev.BillingMode != "subscription" || ev.ApiEquivalentCostUSD == nil {
		return store.Warning{}, false
	}
	return warning(KindAPIEquivalentCost, SeverityInfo,
		fmt.Sprintf("hypothetical API-equivalent cost: $%.6f (not billed)", *ev.ApiEquivalentCostUSD)), true
}

func parseRespHeaders(raw string) http.Header {
	if raw == "" {
		return http.Header{}
	}
	var h http.Header
	if json.Unmarshal([]byte(raw), &h) != nil || h == nil {
		return http.Header{}
	}
	return h
}

// --- Session-scoped rules ---------------------------------------------

// bigWriteFraction is how much of the previous turn's prompt a cache
// write has to reproduce before it counts as "rewriting the whole
// conversation" rather than incremental growth.
const bigWriteFraction = 0.5

// ruleCachePrefixInvalidation fires when every turn after the first
// re-writes roughly the whole prior conversation into the cache instead
// of reading from it -- a healthy session's writes taper off after the
// first turn as later turns read the growing prefix instead.
func ruleCachePrefixInvalidation(rows []*store.Event) []store.Warning {
	if len(rows) < 3 {
		return nil
	}
	for i := 1; i < len(rows); i++ {
		prevTotal := rows[i-1].TotalPromptTokens
		write := rows[i].CacheWrite5mTokens + rows[i].CacheWrite1hTokens
		if prevTotal == 0 || float64(write) < bigWriteFraction*float64(prevTotal) {
			return nil
		}
	}
	last := rows[len(rows)-1]
	return []store.Warning{withEventID(last.ID, warning(KindCachePrefixInvalidation, SeverityWarn,
		"cache write repeated the full conversation on every turn instead of reading from cache"))}
}

// ruleCacheInvalidatedByTools fires when the tools array changed between
// two consecutive calls in a session and the later call then paid a full
// rewrite -- the effect (a big write) is visible in usage alone; the
// cause (which array changed) needs the captured request bodies.
func ruleCacheInvalidatedByTools(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if len(prev.ReqBody) == 0 || len(cur.ReqBody) == 0 {
			continue
		}
		if prev.TotalPromptTokens == 0 {
			continue
		}
		write := cur.CacheWrite5mTokens + cur.CacheWrite1hTokens
		if float64(write) < bigWriteFraction*float64(prev.TotalPromptTokens) {
			continue
		}
		if toolNamesEqual(prev.ReqBody, cur.ReqBody) {
			continue
		}
		out = append(out, withEventID(cur.ID, warning(KindCacheInvalidatedByTools, SeverityWarn,
			"tools array changed between turns, invalidating the cached prefix from position 0")))
	}
	return out
}

func toolNamesEqual(a, b []byte) bool {
	an := parse.ExtractMeta(a, http.Header{}).ToolNames
	bn := parse.ExtractMeta(b, http.Header{}).ToolNames
	if len(an) != len(bn) {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return false
		}
	}
	return true
}

// ruleCacheWriteNeverRead fires on a write with no subsequent read of any
// prefix within the TTL the write was made at -- the premium was paid
// for nothing.
func ruleCacheWriteNeverRead(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i, r := range rows {
		if r.CacheWrite5mTokens == 0 && r.CacheWrite1hTokens == 0 {
			continue
		}
		ttl := 5 * time.Minute
		if r.CacheWrite1hTokens > 0 {
			ttl = time.Hour
		}
		deadline := r.StartedAt.Add(ttl)
		read := false
		for j := i + 1; j < len(rows) && !rows[j].StartedAt.After(deadline); j++ {
			if rows[j].CacheReadTokens > 0 {
				read = true
				break
			}
		}
		if !read {
			out = append(out, withEventID(r.ID, warning(KindCacheWriteNeverRead, SeverityWarn,
				"cache write recorded with no subsequent read within its TTL")))
		}
	}
	return out
}

// ruleCacheTTLMismatch fires on a 1h write whose first subsequent read
// landed under 5 minutes later -- the cheaper 5m TTL would have covered
// it, so the doubled write premium bought nothing extra.
func ruleCacheTTLMismatch(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i, r := range rows {
		if r.CacheWrite1hTokens == 0 {
			continue
		}
		for j := i + 1; j < len(rows); j++ {
			gap := rows[j].StartedAt.Sub(r.StartedAt)
			if gap > time.Hour {
				break
			}
			if rows[j].CacheReadTokens == 0 {
				continue
			}
			if gap < 5*time.Minute {
				out = append(out, withEventID(r.ID, warning(KindCacheTTLMismatch, SeverityInfo,
					"1h cache write was read again within 5 minutes; the 5-minute TTL would have sufficed")))
			}
			break
		}
	}
	return out
}

// ruleCacheExpiredBetweenTurns fires when a prefix is re-written instead
// of read after its previous write's TTL has already elapsed.
func ruleCacheExpiredBetweenTurns(rows []*store.Event) []store.Warning {
	type writeInfo struct {
		at  time.Time
		ttl time.Duration
	}
	last := map[string]writeInfo{}
	var out []store.Warning
	for _, r := range rows {
		if r.PrefixHash == nil {
			continue
		}
		key := *r.PrefixHash
		wrote := r.CacheWrite5mTokens > 0 || r.CacheWrite1hTokens > 0
		if !wrote {
			continue
		}
		ttl := 5 * time.Minute
		if r.CacheWrite1hTokens > 0 {
			ttl = time.Hour
		}
		if prev, ok := last[key]; ok && r.StartedAt.Sub(prev.at) > prev.ttl {
			out = append(out, withEventID(r.ID, warning(KindCacheExpiredBetweenTurns, SeverityInfo,
				"cached prefix was re-written after its TTL expired instead of being read")))
		}
		last[key] = writeInfo{at: r.StartedAt, ttl: ttl}
	}
	return out
}

// ruleCacheConcurrentWriteRace fires when a call overlaps in time with an
// earlier call sharing the same prefix that was also still writing --
// a cache entry only becomes readable once the first response begins
// streaming, so every overlapping call pays full price.
func ruleCacheConcurrentWriteRace(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i := range rows {
		if rows[i].PrefixHash == nil || rows[i].CacheReadTokens > 0 {
			continue
		}
		if rows[i].CacheWrite5mTokens == 0 && rows[i].CacheWrite1hTokens == 0 {
			continue
		}
		for j := 0; j < i; j++ {
			if rows[j].PrefixHash == nil || *rows[j].PrefixHash != *rows[i].PrefixHash {
				continue
			}
			if rows[j].CacheReadTokens > 0 {
				continue
			}
			if rows[j].CacheWrite5mTokens == 0 && rows[j].CacheWrite1hTokens == 0 {
				continue
			}
			endJ := rows[j].StartedAt
			if rows[j].EndedAt != nil {
				endJ = *rows[j].EndedAt
			}
			if rows[i].StartedAt.Before(endJ) {
				out = append(out, withEventID(rows[i].ID, warning(KindCacheConcurrentWriteRace, SeverityInfo,
					"overlapped with another call on the same prefix that was also still writing")))
				break
			}
		}
	}
	return out
}

func withEventID(id int64, w store.Warning) store.Warning {
	w.EventID = id
	return w
}
