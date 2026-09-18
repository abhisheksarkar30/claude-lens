package store

import "time"

// Event is one row in events: one observed turn, from source "proxy" or
// "jsonl". TotalPromptTokens is recomputed by the store on every write
// (invariant 4) — a caller-supplied value is ignored, never trusted.
type Event struct {
	ID          int64
	RequestID   string
	Source      string
	SourceRefs  []string
	FirstSource string

	StartedAt time.Time
	EndedAt   *time.Time

	AuthKind    string
	Account     string
	BillingMode string // "subscription" | "api"

	ModelRequested string
	ModelResolved  string

	InputTokens        int
	OutputTokens       int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
	CacheReadTokens    int
	ThinkingTokens     int
	TotalPromptTokens  int

	ServiceTier  string
	Speed        string
	Effort       string
	InferenceGeo string

	StopReason   string
	StopCategory string

	IsSidechain   bool
	SessionID     string
	Project       string
	GitBranch     string
	ClientVersion string
	CliEntrypoint string

	// CostUSD and ApiEquivalentCostUSD are mutually exclusive per
	// invariant 5: a subscription row populates ApiEquivalentCostUSD and
	// leaves CostUSD nil; an api row is the reverse; an unpriced row (of
	// either mode) leaves both nil, never a $0.00 value.
	CostUSD              *float64
	ApiEquivalentCostUSD *float64
	CostSource           string

	// PrefixHash's NULL-ness is load-bearing for the session resolver:
	// nil means "keyed by an explicit session header instead", and a
	// non-nil "" means the request body did not parse as JSON.
	PrefixHash *string

	ReplayOf    string
	ReplayEdits string

	// CaptureComplete is false when the body was truncated or the stream
	// ended without message_stop — the merge-precedence flag.
	CaptureComplete bool

	// Proxy-only, empty for a source="jsonl" row.
	Method      string
	Path        string
	Status      int
	ReqHeaders  string // redacted header JSON
	RespHeaders string // redacted header JSON
	ReqBody     []byte
	RespBody    []byte
}

// Warning is one analyzer finding attached to an event. (event_id, kind) is
// unique — a second attach of the same kind upserts rather than duplicates.
type Warning struct {
	ID        int64
	EventID   int64
	Kind      string
	Severity  string
	Detail    string
	Path      string
	CreatedAt time.Time
}

// Session is an agentic-run grouping with totals re-derived (never
// incremented) from events + warnings whenever a merge rewrites a row.
type Session struct {
	ID         string
	PrefixHash string
	FirstSeen  time.Time
	LastSeen   time.Time

	RequestCount int

	InputTokens        int
	OutputTokens       int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
	CacheReadTokens    int
	ThinkingTokens     int
	TotalPromptTokens  int

	PricedCount   int
	UnpricedCount int
	ModelSet      []string
	WarningCount  int

	// Same two-column split as Event: TotalCostUSD sums api-billed rows
	// only, TotalApiEquivalentCostUSD sums subscription rows only, each
	// nil (never $0.00) when the session has no row of that mode.
	TotalCostUSD              *float64
	TotalApiEquivalentCostUSD *float64
}

// EventFilter narrows ListEvents/CountEvents. A zero-valued field is not
// filtered on.
type EventFilter struct {
	Source      string
	Account     string
	BillingMode string
	Model       string
	SessionID   string
	Since       time.Time
	Until       time.Time
	Limit       int
	Offset      int
}

// StatsSummary is the aggregate shape shared by StatsSummary, StatsByModel,
// StatsByPeriod, and StatsByCostSource.
type StatsSummary struct {
	RequestCount int

	InputTokens        int
	OutputTokens       int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
	CacheReadTokens    int
	ThinkingTokens     int
	TotalPromptTokens  int

	PricedCount   int
	UnpricedCount int

	TotalCostUSD              *float64
	TotalApiEquivalentCostUSD *float64
}

// ModelStats is one StatsByModel row.
type ModelStats struct {
	Model string
	StatsSummary
}

// PeriodStats is one StatsByPeriod row, keyed by the requested granularity
// ("day" | "week" | "month").
type PeriodStats struct {
	PeriodStart time.Time
	StatsSummary
}

// CostSourceStats is one StatsByCostSource row.
type CostSourceStats struct {
	CostSource   string
	RequestCount int
	TotalCostUSD *float64
}

// WarningFilter narrows ListWarnings/CountWarnings.
type WarningFilter struct {
	Kind   string
	Limit  int
	Offset int
}

// WarningSummary is one row of ListWarnings' per-kind counts.
type WarningSummary struct {
	Kind  string
	Count int
}

// AdminUsageDay is one admin_usage_days row (source D, usage report).
type AdminUsageDay struct {
	DayStart         time.Time
	WindowStart      time.Time
	WindowEnd        time.Time
	Model            string
	WorkspaceID      string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Raw              string
	FetchedAt        time.Time
}

// AdminCostDay is one admin_cost_days row (source D, cost report).
type AdminCostDay struct {
	DayStart    time.Time
	WindowStart time.Time
	WindowEnd   time.Time
	Model       string
	Description string
	AmountUSD   float64
	Currency    string
	Raw         string
	FetchedAt   time.Time
}

// AdminRateLimit is one admin_rate_limits row.
type AdminRateLimit struct {
	Scope       string
	WorkspaceID string
	Model       string
	GroupType   string
	Limit       int
	FetchedAt   time.Time
}

// QuotaSnapshot is one quota_snapshots row (source C, one row per window
// per poll).
type QuotaSnapshot struct {
	ID             int64
	ObservedAt     time.Time
	Account        string
	Window         string
	UtilizationPct *float64
	ResetsAt       *time.Time
	Status         string
	Raw            string
}
