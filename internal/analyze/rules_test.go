package analyze

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

func endedAt(t time.Time) *time.Time { return &t }

func warningKinds(warnings []store.Warning) map[string][]int64 {
	out := map[string][]int64{}
	for _, w := range warnings {
		out[w.Kind] = append(out[w.Kind], w.EventID)
	}
	return out
}

func reqBodyWithTools(names ...string) []byte {
	toolsJSON := ""
	for i, n := range names {
		if i > 0 {
			toolsJSON += ","
		}
		toolsJSON += `{"name":"` + n + `"}`
	}
	return []byte(`{"model":"claude-sonnet-5","tools":[` + toolsJSON + `],"messages":[]}`)
}

// --- cache_prefix_invalidation ------------------------------------------

func TestRuleCachePrefixInvalidationFiresOnInvalidatorLoop(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheWrite5mTokens: 4900}},
		{EventSummary: store.EventSummary{ID: 3, StartedAt: at(2), TotalPromptTokens: 5200, CacheWrite5mTokens: 5000}},
	}
	found := warningKinds(ruleCachePrefixInvalidation(rows))
	ids := found[string(KindCachePrefixInvalidation)]
	if len(ids) != 1 || ids[0] != 3 {
		t.Errorf("invalidator loop findings = %v, want exactly [3]", ids)
	}
}

func TestRuleCachePrefixInvalidationSilentOnHealthyLoop(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000, CacheWrite5mTokens: 5000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheReadTokens: 5000}},
		{EventSummary: store.EventSummary{ID: 3, StartedAt: at(2), TotalPromptTokens: 5200, CacheReadTokens: 5100}},
	}
	if got := ruleCachePrefixInvalidation(rows); got != nil {
		t.Errorf("healthy loop raised %v, want nil", got)
	}
}

// --- cache_invalidated_by_tools ------------------------------------------

func TestRuleCacheInvalidatedByToolsFiresOnChangedArray(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000}, ReqBody: reqBodyWithTools("bash", "read")},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheWrite5mTokens: 4900}, ReqBody: reqBodyWithTools("bash", "read", "edit")},
	}
	found := warningKinds(ruleCacheInvalidatedByTools(rows))
	if ids := found[string(KindCacheInvalidatedByTools)]; len(ids) != 1 || ids[0] != 2 {
		t.Errorf("changed tools findings = %v, want exactly [2]", ids)
	}
}

func TestRuleCacheInvalidatedByToolsSilentOnIdenticalArray(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000}, ReqBody: reqBodyWithTools("bash", "read")},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheWrite5mTokens: 4900}, ReqBody: reqBodyWithTools("bash", "read")},
	}
	if got := ruleCacheInvalidatedByTools(rows); got != nil {
		t.Errorf("identical tools raised %v, want nil", got)
	}
}

// --- cache_write_never_read ------------------------------------------

func TestRuleCacheWriteNeverReadFiresWithNoLaterRead(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), CacheWrite5mTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1)}}, // within TTL, no read
	}
	found := warningKinds(ruleCacheWriteNeverRead(rows))
	if ids := found[string(KindCacheWriteNeverRead)]; len(ids) != 1 || ids[0] != 1 {
		t.Errorf("never-read findings = %v, want exactly [1]", ids)
	}
}

func TestRuleCacheWriteNeverReadSilentWhenReadWithinTTL(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), CacheWrite5mTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1), CacheReadTokens: 900}},
	}
	if got := ruleCacheWriteNeverRead(rows); got != nil {
		t.Errorf("read within TTL raised %v, want nil", got)
	}
}

// --- cache_ttl_mismatch ------------------------------------------

func TestRuleCacheTTLMismatchFiresOnEarlyReread(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), CacheWrite1hTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(2), CacheReadTokens: 900}}, // 2 min later, under 5m
	}
	found := warningKinds(ruleCacheTTLMismatch(rows))
	if ids := found[string(KindCacheTTLMismatch)]; len(ids) != 1 || ids[0] != 1 {
		t.Errorf("early re-read findings = %v, want exactly [1]", ids)
	}
}

func TestRuleCacheTTLMismatchSilentWhenReadOutsideFiveMinutes(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), CacheWrite1hTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(30), CacheReadTokens: 900}}, // 30 min later, justifies the 1h TTL
	}
	if got := ruleCacheTTLMismatch(rows); got != nil {
		t.Errorf("read outside 5 minutes raised %v, want nil", got)
	}
}

// --- cache_expired_between_turns ------------------------------------------

func TestRuleCacheExpiredBetweenTurnsFiresOnStaleRewrite(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), PrefixHash: &hash, CacheWrite5mTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(10), PrefixHash: &hash, CacheWrite5mTokens: 1000}}, // 10 min later, past the 5m TTL
	}
	found := warningKinds(ruleCacheExpiredBetweenTurns(rows))
	if ids := found[string(KindCacheExpiredBetweenTurns)]; len(ids) != 1 || ids[0] != 2 {
		t.Errorf("stale re-write findings = %v, want exactly [2]", ids)
	}
}

func TestRuleCacheExpiredBetweenTurnsSilentWhenRewriteWithinTTL(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), PrefixHash: &hash, CacheWrite5mTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(2), PrefixHash: &hash, CacheWrite5mTokens: 1000}}, // still within the 5m TTL
	}
	if got := ruleCacheExpiredBetweenTurns(rows); got != nil {
		t.Errorf("re-write within TTL raised %v, want nil", got)
	}
}

// --- cache_concurrent_write_race ------------------------------------------

func TestRuleCacheConcurrentWriteRaceFiresOnOverlap(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), EndedAt: endedAt(at(2)), PrefixHash: &hash, CacheWrite5mTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1), PrefixHash: &hash, CacheWrite5mTokens: 1000}}, // starts before call 1 finished
	}
	found := warningKinds(ruleCacheConcurrentWriteRace(rows))
	if ids := found[string(KindCacheConcurrentWriteRace)]; len(ids) != 1 || ids[0] != 2 {
		t.Errorf("overlap findings = %v, want exactly [2]", ids)
	}
}

func TestRuleCacheConcurrentWriteRaceSilentWhenSequential(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), EndedAt: endedAt(at(1)), PrefixHash: &hash, CacheWrite5mTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(2), PrefixHash: &hash, CacheReadTokens: 900}}, // starts after call 1 ended
	}
	if got := ruleCacheConcurrentWriteRace(rows); got != nil {
		t.Errorf("sequential calls raised %v, want nil", got)
	}
}

// --- thinking_budget_rejected ------------------------------------------

func TestRuleThinkingBudgetRejectedFiresOn400(t *testing.T) {
	budget := 1024
	meta := parse.Meta{HasThinking: true, ThinkingBudget: &budget}
	ev := &store.Event{EventSummary: store.EventSummary{Status: 400}, RespBody: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"thinking.budget_tokens is not supported"}}`)}
	w, fired := ruleThinkingBudgetRejected(meta, parse.Usage{}, ev)
	if !fired || w.Kind != string(KindThinkingBudgetRejected) {
		t.Errorf("400 with thinking budget did not fire %s: %v/%v", KindThinkingBudgetRejected, w, fired)
	}
}

func TestRuleThinkingBudgetRejectedSilentWhenAccepted(t *testing.T) {
	budget := 1024
	meta := parse.Meta{HasThinking: true, ThinkingBudget: &budget}
	ev := &store.Event{EventSummary: store.EventSummary{Status: 200}, RespBody: []byte(`{"type":"message"}`)}
	if _, fired := ruleThinkingBudgetRejected(meta, parse.Usage{}, ev); fired {
		t.Error("a 200 response fired thinking_budget_rejected, want none")
	}
}

// --- thinking_display_omitted ------------------------------------------

func TestRuleThinkingDisplayOmittedFiresWithNoThinkingBlock(t *testing.T) {
	meta := parse.Meta{HasThinking: true}
	ev := &store.Event{RespBody: []byte(`{"content":[{"type":"text","text":"hi"}]}`)}
	_, fired := ruleThinkingDisplayOmitted(meta, parse.Usage{ThinkingTokens: 50}, ev)
	if !fired {
		t.Error("billed thinking with no thinking block did not fire thinking_display_omitted")
	}
}

func TestRuleThinkingDisplayOmittedSilentWhenSummarized(t *testing.T) {
	meta := parse.Meta{HasThinking: true}
	ev := &store.Event{RespBody: []byte(`{"content":[{"type":"thinking","text":"because..."}]}`)}
	if _, fired := ruleThinkingDisplayOmitted(meta, parse.Usage{ThinkingTokens: 50}, ev); fired {
		t.Error("a present thinking block fired thinking_display_omitted, want none")
	}
}

// --- max_tokens_truncation ------------------------------------------

func TestRuleMaxTokensTruncation(t *testing.T) {
	if _, fired := ruleMaxTokensTruncation(parse.Meta{}, parse.Usage{StopReason: "max_tokens"}, &store.Event{}); !fired {
		t.Error("stop_reason=max_tokens did not fire max_tokens_truncation")
	}
	if _, fired := ruleMaxTokensTruncation(parse.Meta{}, parse.Usage{StopReason: "end_turn"}, &store.Event{}); fired {
		t.Error("stop_reason=end_turn fired max_tokens_truncation, want none")
	}
}

// --- refusal ------------------------------------------

func TestRuleRefusal(t *testing.T) {
	w, fired := ruleRefusal(parse.Meta{}, parse.Usage{StopReason: "refusal", StopCategory: "policy"}, &store.Event{})
	if !fired || !strings.Contains(w.Detail, "policy") {
		t.Errorf("refusal with a category did not fire naming it: %v/%v", w, fired)
	}
	if _, fired := ruleRefusal(parse.Meta{}, parse.Usage{StopReason: "end_turn", StopCategory: "policy"}, &store.Event{}); fired {
		t.Error("a non-refusal stop_reason fired refusal, want none")
	}
}

// --- stream_incomplete ------------------------------------------

func TestRuleStreamIncomplete(t *testing.T) {
	incomplete := &store.Event{EventSummary: store.EventSummary{CaptureComplete: false}}
	w, fired := ruleStreamIncomplete(parse.Meta{}, parse.Usage{IsStream: true}, incomplete)
	if !fired {
		t.Error("an incomplete stream did not fire stream_incomplete")
	}
	if w.Severity != string(SeverityError) {
		t.Errorf("severity = %q, want %q", w.Severity, SeverityError)
	}
	// The rule cannot tell a body cut at the read cap from a stream that ended
	// early -- the cap is runtime config and is not on the event -- and since
	// br-GI-7-08 a truncated *request* body reaches it too. Asserting
	// message_stop would be asserting one of two causes on no evidence, so the
	// wording has to stay the disjunction.
	if strings.Contains(w.Detail, "message_stop") {
		t.Errorf("detail claims the message_stop cause the rule cannot know: %q", w.Detail)
	}
	if !strings.Contains(w.Detail, "truncated") || !strings.Contains(w.Detail, "stream ended early") {
		t.Errorf("detail = %q, want both causes named", w.Detail)
	}
	complete := &store.Event{EventSummary: store.EventSummary{CaptureComplete: true}}
	if _, fired := ruleStreamIncomplete(parse.Meta{}, parse.Usage{IsStream: true}, complete); fired {
		t.Error("a complete stream fired stream_incomplete, want none")
	}
}

// --- rate_limited / overloaded ------------------------------------------

func TestRuleRateLimitedCarriesRetryAfter(t *testing.T) {
	headers := http.Header{}
	headers.Set("Retry-After", "30")
	headers.Set("anthropic-ratelimit-requests-remaining", "0")
	raw, _ := json.Marshal(headers)
	ev := &store.Event{EventSummary: store.EventSummary{Status: 429}, RespHeaders: string(raw)}
	w, fired := ruleRateLimited(parse.Meta{}, parse.Usage{}, ev)
	if !fired {
		t.Fatal("429 did not fire rate_limited")
	}
	if !strings.Contains(w.Detail, "retry-after=30") {
		t.Errorf("detail = %q, want retry-after=30", w.Detail)
	}
	if !strings.Contains(strings.ToLower(w.Detail), "ratelimit-requests-remaining=0") {
		t.Errorf("detail = %q, want the ratelimit header value", w.Detail)
	}
}

func TestRuleOverloaded(t *testing.T) {
	if _, fired := ruleOverloaded(parse.Meta{}, parse.Usage{}, &store.Event{EventSummary: store.EventSummary{Status: 529}}); !fired {
		t.Error("529 did not fire overloaded")
	}
	if _, fired := ruleOverloaded(parse.Meta{}, parse.Usage{}, &store.Event{EventSummary: store.EventSummary{Status: 200}}); fired {
		t.Error("200 fired overloaded, want none")
	}
}

// --- upstream_error (body half) ------------------------------------------

func TestRuleUpstreamErrorBodyFiresOnErrorObject(t *testing.T) {
	ev := &store.Event{EventSummary: store.EventSummary{Status: 200}, RespBody: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)}
	w, fired := ruleUpstreamErrorBody(parse.Meta{}, parse.Usage{}, ev)
	if !fired || !strings.Contains(w.Detail, "overloaded") {
		t.Errorf("error object in a 200 body did not fire naming the message: %v/%v", w, fired)
	}
}

func TestRuleUpstreamErrorBodySilentOnCleanBody(t *testing.T) {
	ev := &store.Event{EventSummary: store.EventSummary{Status: 200}, RespBody: []byte(`{"type":"message","content":[]}`)}
	if _, fired := ruleUpstreamErrorBody(parse.Meta{}, parse.Usage{}, ev); fired {
		t.Error("a clean 200 body fired upstream_error, want none")
	}
}

// --- auth_kind_anomaly ------------------------------------------

func TestRuleAuthKindAnomaly(t *testing.T) {
	cases := []struct {
		name  string
		ev    *store.Event
		fires bool
	}{
		{"admin against messages", &store.Event{EventSummary: store.EventSummary{AuthKind: "admin", Path: "/v1/messages"}}, true},
		{"api_key on subscription account", &store.Event{EventSummary: store.EventSummary{AuthKind: "api_key", BillingMode: "subscription"}}, true},
		{"admin against usage report", &store.Event{EventSummary: store.EventSummary{AuthKind: "admin", Path: "/v1/organizations/usage_report"}}, false},
		{"api_key on api account", &store.Event{EventSummary: store.EventSummary{AuthKind: "api_key", BillingMode: "api"}}, false},
		{"oauth on subscription account", &store.Event{EventSummary: store.EventSummary{AuthKind: "oauth", BillingMode: "subscription"}}, false},
	}
	for _, c := range cases {
		_, fired := ruleAuthKindAnomaly(parse.Meta{}, parse.Usage{}, c.ev)
		if fired != c.fires {
			t.Errorf("%s: fired = %v, want %v", c.name, fired, c.fires)
		}
	}
}

// --- api_equivalent_cost ------------------------------------------

func TestRuleAPIEquivalentCostFiresOnPricedSubscriptionRow(t *testing.T) {
	usd := 0.42
	ev := &store.Event{EventSummary: store.EventSummary{BillingMode: "subscription", ApiEquivalentCostUSD: &usd}}
	w, fired := ruleAPIEquivalentCost(parse.Meta{}, parse.Usage{}, ev)
	if !fired {
		t.Fatal("priced subscription row did not fire api_equivalent_cost")
	}
	if !strings.Contains(w.Detail, "0.42") {
		t.Errorf("detail = %q, want the figure", w.Detail)
	}
}

func TestRuleAPIEquivalentCostSilentOnAPIRow(t *testing.T) {
	usd := 0.42
	ev := &store.Event{EventSummary: store.EventSummary{BillingMode: "api", CostUSD: &usd}}
	if _, fired := ruleAPIEquivalentCost(parse.Meta{}, parse.Usage{}, ev); fired {
		t.Error("an api row fired api_equivalent_cost, want none")
	}
}

func TestRuleAPIEquivalentCostSilentWhenUnpriced(t *testing.T) {
	ev := &store.Event{EventSummary: store.EventSummary{BillingMode: "subscription", CostSource: "unpriced"}}
	if _, fired := ruleAPIEquivalentCost(parse.Meta{}, parse.Usage{}, ev); fired {
		t.Error("an unpriced subscription row fired api_equivalent_cost, want none")
	}
}

// --- multi-rule: truncated and rate-limited fire distinct kinds --------

func TestAnalyzeTruncatedAndRateLimitedFiresTwoKinds(t *testing.T) {
	ev := &store.Event{EventSummary: store.EventSummary{Status: 429}}
	warnings := Analyze(parse.Meta{}, parse.Usage{StopReason: "max_tokens"}, ev)
	seen := map[string]int{}
	for _, w := range warnings {
		seen[w.Kind]++
	}
	if seen[string(KindMaxTokensTruncation)] != 1 {
		t.Errorf("max_tokens_truncation count = %d, want 1", seen[string(KindMaxTokensTruncation)])
	}
	if seen[string(KindRateLimited)] != 1 {
		t.Errorf("rate_limited count = %d, want 1", seen[string(KindRateLimited)])
	}
	for kind, n := range seen {
		if n != 1 {
			t.Errorf("kind %s appeared %d times, want exactly 1", kind, n)
		}
	}
}

// --- AnalyzeSession wiring ------------------------------------------

func TestAnalyzeSessionRunsEveryRule(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0), CacheWrite1hTokens: 1000}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1)}}, // never read: fires cache_write_never_read
	}
	warnings := AnalyzeSession(rows)
	if !hasKind(warnings, KindCacheWriteNeverRead) {
		t.Errorf("AnalyzeSession did not run the cache_write_never_read rule: %v", warnings)
	}
}

func TestAnalyzeSessionCleanSessionReturnsNil(t *testing.T) {
	rows := []*store.Event{
		{EventSummary: store.EventSummary{ID: 1, StartedAt: at(0)}},
		{EventSummary: store.EventSummary{ID: 2, StartedAt: at(1)}},
	}
	if got := AnalyzeSession(rows); got != nil {
		t.Errorf("clean session raised %v, want nil", got)
	}
}
