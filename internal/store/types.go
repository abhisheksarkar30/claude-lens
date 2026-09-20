package store

import "time"

// EventSummary is an event row's scalar block: everything Event carries
// except the four header/body blobs. It is the type ListEvents returns, so
// the list path never reads a body out of SQLite — and a caller that needs
// one cannot reach it, because the field is not there to name. That
// compile error is the point: with a flag on the query instead, a caller
// that forgot it would silently receive nil where it needed bytes, and a
// body-less jsonl row is byte-identical on the wire to an unselected one.
//
// TotalPromptTokens is recomputed by the store on every write (invariant 4)
// — a caller-supplied value is ignored, never trusted.
type EventSummary struct {
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

	// CaptureComplete is false when either body was truncated or the stream
	// ended without message_stop — the merge-precedence flag. Which of the
	// two bodies was cut is not recorded on the row; the detail view names
	// it by comparing each stored body's length against the read cap it was
	// configured with.
	CaptureComplete bool

	// Proxy-only, empty for a source="jsonl" row.
	Method string
	Path   string
	Status int
}

// Event is one row in events: one observed turn, from source "proxy" or
// "jsonl".
//
// The scalar block is embedded rather than duplicated because Event's wire
// keys are its Go field names: a second, hand-kept copy could drift from
// EventSummary field-by-field and silently split the list wire from the
// detail wire. encoding/json flattens an embedded struct, so the detail
// route's JSON is unchanged by the split.
//
// The four blobs stay direct fields, which is what keeps them off the list
// path — see EventSummary.
type Event struct {
	EventSummary

	// Proxy-only, empty for a source="jsonl" row.
	ReqHeaders  string // redacted header JSON
	RespHeaders string // redacted header JSON
	ReqBody     []byte
	RespBody    []byte

	// Transcript-only, empty for a source="proxy" row. A transcript excerpt is
	// a reconstruction of intent, not a wire capture: one assistant message,
	// not the request that produced it, and it excludes the system prompt, the
	// tool schemas, and everything the proxy sees. The columns are named for
	// that so a reader cannot mistake one for a capture.
	//
	// A transcript carries no headers at all -- not redacted, absent.
	TranscriptContent []byte
	TranscriptRole    string
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

	// ReplayOf, when non-nil, keeps only rows replayed from that event id.
	// A pointer rather than an int64 because 0 is not a valid event id and
	// "unset" has to be distinguishable from it.
	ReplayOf *int64
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
