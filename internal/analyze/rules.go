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

// minimumCacheablePrefix is the API's minimum cacheable prefix length per
// model, in tokens. It is deliberately *not* monotonic across generations
// -- Opus 4.6 needs 4096 where Opus 4.8 needs 1024 and Opus 5 needs 512 --
// which is why it is a table and not a rule of thumb: "a newer model needs
// less" is false for Opus 4.6/4.5, and a rule of thumb would mis-diagnose
// exactly those.
//
// Matched by substring against the lowercased model id, first entry wins.
// No shipped id matches two entries.
var minimumCacheablePrefix = []struct {
	match string
	min   int
}{
	{"opus-5", 512}, {"fable-5", 512}, {"mythos-5", 512},
	{"opus-4-8", 1024}, {"sonnet-5", 1024}, {"sonnet-4-6", 1024}, {"sonnet-4-5", 1024},
	{"opus-4-7", 2048},
	{"opus-4-6", 4096}, {"opus-4-5", 4096}, {"haiku-4-5", 4096},
}

// minimumCacheablePrefixFor returns the model's minimum cacheable prefix
// in tokens, and whether the table knows the model at all. An unknown
// model reports no minimum rather than a default, for the same reason an
// unpriced model stores NULL and not $0.00: an invented threshold asserts
// something the evidence does not support.
func minimumCacheablePrefixFor(model string) (int, bool) {
	m := strings.ToLower(model)
	for _, e := range minimumCacheablePrefix {
		if strings.Contains(m, e.match) {
			return e.min, true
		}
	}
	return 0, false
}

// markedPrefixTokens estimates the token count of the prefix carrying the
// cache_control breakpoint, from the captured request body.
//
// ponytail: bytes/4, not a tokenizer -- the table's steps are 512/1024/
// 2048/4096, far coarser than the estimate's error, and both error modes
// are safe. The body is the *longest* prefix (the breakpoint chain runs
// tools -> system -> messages, so the last breakpoint sits at or before the
// end), which makes this an over-estimate for an early breakpoint in a long
// conversation; an over-estimate suppresses the warning rather than
// inventing one. A truncated body under-estimates, but even a body cut at the
// default 2 MB cap is ~520k tokens, still far above the largest minimum.
// Swap in a real tokenizer if a genuine prefix ever lands within a few percent
// of a step.
func markedPrefixTokens(body []byte) int {
	return len(body) / 4
}

// ruleCachePrefixBelowMinimum fires when a cache_control marker was
// present but produced no observable cache effect at all -- no write, no
// read -- *and* the marked prefix is below the model's own minimum
// cacheable length. This is the one T1 rule that genuinely needs the
// captured request body: usage alone shows only the symptom
// (cache_creation_input_tokens: 0), never the cause.
//
// The no-effect condition is not the diagnosis by itself. A marker that
// cached nothing on a prefix long enough to cache has some *other* cause
// -- invalidated, expired, raced -- each with its own rule; firing this
// kind there would name the wrong one, so the rule declines whenever it
// cannot measure the prefix or does not know the model.
func ruleCachePrefixBelowMinimum(meta parse.Meta, usage parse.Usage, ev *store.Event) (store.Warning, bool) {
	if !meta.HasCacheControl {
		return store.Warning{}, false
	}
	if usage.CacheWrite5mTokens > 0 || usage.CacheWrite1hTokens > 0 || usage.CacheReadTokens > 0 {
		return store.Warning{}, false
	}
	// No captured body, no prefix to measure. JSONL rows never set
	// HasCacheControl, so this only ever declines a proxy row whose request
	// body was truncated away entirely.
	if len(ev.ReqBody) == 0 {
		return store.Warning{}, false
	}
	model := usage.Model
	if model == "" {
		model = meta.ModelRequested
	}
	minimum, ok := minimumCacheablePrefixFor(model)
	if !ok {
		return store.Warning{}, false
	}
	if markedPrefixTokens(ev.ReqBody) >= minimum {
		return store.Warning{}, false
	}
	detail := fmt.Sprintf("a cache_control breakpoint marked a prefix under %d tokens; %s caches nothing below %d",
		minimum, model, minimum)
	return warning(KindCachePrefixBelowMinimum, SeverityWarn, detail), true
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

// ruleStreamIncomplete fires on a streamed response the store recorded as an
// incomplete capture -- CaptureComplete is false exactly when a body was
// truncated at the read cap.
//
// The detail states the disjunction rather than picking a cause, because the
// rule cannot tell the two apart and this is the only place that ever claimed
// it could. The cap is runtime configuration and is not on the event, so a
// request body cut at it and a stream that ended early reach here as the same
// flag; naming message_stop would be asserting one of two on no evidence. The
// wording matches what `clens show` and the dashboard already say.
func ruleStreamIncomplete(_ parse.Meta, usage parse.Usage, ev *store.Event) (store.Warning, bool) {
	if !usage.IsStream || ev.CaptureComplete {
		return store.Warning{}, false
	}
	return warning(KindStreamIncomplete, SeverityError,
		"incomplete (truncated, or the stream ended early)"), true
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
//
// The row set is filtered to usage-carrying rows before the walk (D9): a
// usage-less row (a 429, a truncated body) would otherwise abort the walk
// as a zero prevTotal or a zero write and permanently drop a real finding
// -- the walk returns nil on the first violating pair, so one bad row
// anywhere in the session used to cost the whole session's finding. A real
// row that continues the rewrite run past a filtered usage-less row still
// anchors the finding on itself, same as if the usage-less row had never
// arrived; a real row that instead reads from cache still declines it,
// same as today -- filtering changes which rows are walked, not the
// walk's own condition.
func ruleCachePrefixInvalidation(rows []*store.Event) []store.Warning {
	rows = rowsWithUsage(rows)
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
//
// D7 turns a "session" from one gap-window burst of proxy rows into the
// whole conversation, so rows can interleave JSONL rows (which
// structurally never carry a request body) between two proxy rows. The
// row set is filtered to the rows carrying a request body *before* the
// pair walk -- not by skipping a pair mid-walk -- so a JSONL row sitting
// between two proxy calls does not silently break their adjacency: under
// the default body policy the filtered set is exactly the proxy
// population, and because a session is now the whole conversation this
// hands the rule more proxy rows than it saw before D7, never fewer.
func ruleCacheInvalidatedByTools(rows []*store.Event) []store.Warning {
	rows = rowsWithRequestBody(rows)
	var out []store.Warning
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if prev.TotalPromptTokens == 0 {
			continue
		}
		write := cur.CacheWrite5mTokens + cur.CacheWrite1hTokens
		if float64(write) < bigWriteFraction*float64(prev.TotalPromptTokens) {
			continue
		}
		if prev.ToolNames == cur.ToolNames {
			continue
		}
		out = append(out, withEventID(cur.ID, warning(KindCacheInvalidatedByTools, SeverityWarn,
			"tools array changed between turns, invalidating the cached prefix from position 0")))
	}
	return out
}

// rowsWithRequestBody returns the subset of rows carrying a request body,
// in order. A JSONL-sourced row structurally never carries one (the
// transcript records no request), so this is what lets a rule that
// compares consecutive request bodies skip over an interleaved JSONL row
// rather than treating it as an adjacent, body-less pair. Since br-GI-13-07
// the rules projection no longer selects req_body itself, so this reads
// HasReqBody -- the req_tool_names column's own NULL-iff-no-body contract --
// rather than a body length.
func rowsWithRequestBody(rows []*store.Event) []*store.Event {
	out := make([]*store.Event, 0, len(rows))
	for _, r := range rows {
		if r.HasReqBody {
			out = append(out, r)
		}
	}
	return out
}

// rowsWithUsage returns the subset of rows that actually carry usage, in
// order -- the sibling filter to rowsWithRequestBody, for a rule that reads
// token columns rather than bodies. TotalPromptTokens is zero for a
// usage-less row (a 429 or a truncated body; the store recomputes the
// column from the token columns), and unlike rowsWithRequestBody this keeps
// JSONL rows: a transcript line structurally never carries a request body
// but does carry usage.
func rowsWithUsage(rows []*store.Event) []*store.Event {
	out := make([]*store.Event, 0, len(rows))
	for _, r := range rows {
		if r.TotalPromptTokens != 0 {
			out = append(out, r)
		}
	}
	return out
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
