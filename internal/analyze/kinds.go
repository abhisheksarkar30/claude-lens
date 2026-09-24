package analyze

// Kind is a warning kind's one canonical spelling, checked mechanically
// (not by diligence) against the README's kind table by readme_test.go
// (br-GI-1-19). A variant spelling is a build failure, not a typo someone
// eventually finds.
type Kind string

// Severity mirrors the three levels a rule can fire at: error (the
// request may have failed or lost content), warn (cost/quality/safety
// divergence), info (dropped with no practical consequence).
type Severity string

const (
	SeverityError Severity = "error"
	SeverityWarn  Severity = "warn"
	SeverityInfo  Severity = "info"
)

// T1 kinds. Every one is declared here even when its rule ships in a
// later bead (br-GI-1-10's response rules) or lives in another package
// entirely (nonAnalyzeKinds below) -- kinds.go is the single source of
// truth for spellings project-wide, not just for what analyze itself
// emits today.
const (
	KindCachePrefixInvalidation  Kind = "cache_prefix_invalidation"
	KindCachePrefixBelowMinimum  Kind = "cache_prefix_below_minimum"
	KindCacheBreakpointsExceeded Kind = "cache_breakpoints_exceeded"
	KindCacheWriteNeverRead      Kind = "cache_write_never_read"
	KindCacheTTLMismatch         Kind = "cache_ttl_mismatch"
	KindCacheExpiredBetweenTurns Kind = "cache_expired_between_turns"
	KindCacheInvalidatedByTools  Kind = "cache_invalidated_by_tools"
	KindCacheConcurrentWriteRace Kind = "cache_concurrent_write_race"

	KindThinkingBudgetRejected Kind = "thinking_budget_rejected"
	KindThinkingDisplayOmitted Kind = "thinking_display_omitted"
	KindMaxTokensTruncation    Kind = "max_tokens_truncation"
	KindRefusal                Kind = "refusal"
	KindStreamIncomplete       Kind = "stream_incomplete"
	KindRateLimited            Kind = "rate_limited"
	KindOverloaded             Kind = "overloaded"
	KindUpstreamError          Kind = "upstream_error"
	KindAuthKindAnomaly        Kind = "auth_kind_anomaly"
	KindQuotaWindowApproaching Kind = "quota_window_approaching"
	KindCostDrift              Kind = "cost_drift"
	KindAPIEquivalentCost      Kind = "api_equivalent_cost"
	KindSourceMismatch         Kind = "source_mismatch"
	KindAnalyzerPanic          Kind = "analyzer_panic"
	KindPeakPricing            Kind = "peak_pricing"
)

// KindInfo pairs a kind with its severity and one-sentence description --
// the description lives beside the kind, not in a separate markdown file,
// so the two cannot drift apart.
type KindInfo struct {
	Kind        Kind
	Severity    Severity
	Description string
}

var allKinds = []KindInfo{
	{KindCachePrefixInvalidation, SeverityWarn, "cache_creation_input_tokens tracks the full conversation on every turn while input_tokens stays small: a silent invalidator is rewriting the prefix upstream of the breakpoint."},
	{KindCachePrefixBelowMinimum, SeverityWarn, "A cache_control breakpoint is present but the prefix is under the model's minimum cacheable length, so cache_creation_input_tokens is 0 and the marker did nothing."},
	{KindCacheBreakpointsExceeded, SeverityError, "More than 4 cache_control breakpoints in one request, which the API rejects with a 400."},
	{KindCacheWriteNeverRead, SeverityWarn, "Write tokens were recorded with no subsequent read of that prefix within its TTL -- the write premium was paid for nothing."},
	{KindCacheTTLMismatch, SeverityInfo, "An ephemeral_1h write was read again within 5 minutes, where the cheaper 5-minute TTL would have sufficed."},
	{KindCacheExpiredBetweenTurns, SeverityInfo, "A cached prefix was re-written instead of read, with a start-to-start gap longer than the TTL it was written with."},
	{KindCacheInvalidatedByTools, SeverityWarn, "The tools array or top-level system changed between consecutive calls in a session, invalidating the entire cache from position 0."},
	{KindCacheConcurrentWriteRace, SeverityInfo, "Overlapping calls shared an identical prefix before the first response began streaming, so more than one paid the full write price."},
	{KindThinkingBudgetRejected, SeverityError, "thinking.budget_tokens was sent to a model that now rejects it with a 400."},
	{KindThinkingDisplayOmitted, SeverityInfo, "Thinking was on and billed, but thinking.display defaulted to omitted -- reasoning paid for but not received."},
	{KindMaxTokensTruncation, SeverityWarn, "stop_reason was max_tokens: the turn was cut off mid-thought and will be retried at more cost."},
	{KindRefusal, SeverityWarn, "stop_reason was refusal."},
	{KindStreamIncomplete, SeverityError, "The capture is incomplete: a body was truncated at the read cap."},
	{KindRateLimited, SeverityWarn, "HTTP 429 was returned."},
	{KindOverloaded, SeverityError, "HTTP 529 was returned."},
	{KindUpstreamError, SeverityError, "An error object arrived inside a 200 body, or the upstream request failed outright."},
	{KindAuthKindAnomaly, SeverityWarn, "An admin credential was used against the Messages API, or a subscription account sent an api_key credential."},
	{KindQuotaWindowApproaching, SeverityWarn, "A subscription window is projected to exhaust before it resets."},
	{KindCostDrift, SeverityWarn, "Computed cost for a (day, model) diverges from the Admin cost report beyond threshold -- the price table is stale."},
	{KindAPIEquivalentCost, SeverityInfo, "On a subscription row: what this call would have cost at API rates. Hypothetical, never added to a billed total."},
	{KindSourceMismatch, SeverityError, "The same request_id was observed twice with disagreeing token counts -- from two different sources (severity error), or from the same source re-reading with amended counts (severity info)."},
	{KindAnalyzerPanic, SeverityError, "A rule panicked; the panic was recovered and logged as a finding instead of crashing the consumer."},
	{KindPeakPricing, SeverityWarn, "The call was billed at the model's peak rate, which is a cost divergence from the same call off-peak."},
}

// AllKinds returns every declared T1 kind and its description. The result
// is a copy: mutating it cannot corrupt the package's own table.
func AllKinds() []KindInfo {
	out := make([]KindInfo, len(allKinds))
	copy(out, allKinds)
	return out
}

// nonAnalyzeKinds names T1 kinds a README-consistency check should expect
// without finding a corresponding rule in this package, because they are
// emitted by another package entirely. upstream_error has a rule here too
// (br-GI-1-10, for an error object inside a 200 body) but is not listed
// here, because the transport-failure trigger stays in consumer.go
// (br-GI-1-08) rather than moving -- one kind, two triggers, only one of
// which is per-event-pure enough to live in this package.
var nonAnalyzeKinds = map[Kind]string{
	KindAnalyzerPanic:          "consumer (panic recovery)",
	KindSourceMismatch:         "store (cross-source or same-source merge)",
	KindCostDrift:              "reconcile",
	KindQuotaWindowApproaching: "quota",
	KindPeakPricing:            "consumer, jsonlogs",
}
