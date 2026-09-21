// Package parse turns a captured response body into a stream of frames
// (this file and sse.go), and those frames into the token usage and
// request metadata the rest of the system records (meta.go, usage.go). It
// is a pure-function package: no network I/O, no dependency on the proxy
// or sink — everything here operates on bytes already captured elsewhere.
package parse

// Frame is one parsed unit from a response body: either an SSE event
// (Type is the event's "type" field, e.g. "message_start",
// "content_block_delta", "message_stop") or the single synthetic frame
// NonStreamFrame produces for a non-streaming JSON body. Extraction
// (ExtractUsage) treats both uniformly rather than re-parsing the raw body
// itself.
type Frame struct {
	Type string
	Data []byte // the raw JSON payload
}

// NonStreamFrame converts a complete application/json response body into
// the single Frame that lets extraction treat a non-streaming response the
// same way it treats a streamed one: a non-streaming body already carries
// its usage, stop_reason, and model at the top level, so it needs no
// further framing — just one frame whose Type marks it as such.
func NonStreamFrame(body []byte) Frame {
	return Frame{Type: "message", Data: body}
}

// Usage is the token/metadata summary extracted from a response body,
// whether it arrived as a single non-streaming JSON body or as an SSE
// event stream. InputTokens is the uncached remainder only — the prompt
// total is InputTokens + CacheWrite5mTokens + CacheWrite1hTokens +
// CacheReadTokens (CLAUDE.md's input_tokens invariant). ThinkingTokens is
// a subset of OutputTokens and is never re-added to it.
type Usage struct {
	InputTokens        int
	OutputTokens       int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
	CacheReadTokens    int
	ThinkingTokens     int
	Model              string
	StopReason         string
	StopCategory       string // read only when StopReason == "refusal"
	ServiceTier        string
	Speed              string
	// TTLUnknown is set when the response carried only the flat
	// cache_creation_input_tokens count (no ephemeral_5m/1h split): the
	// whole flat count is recorded into CacheWrite5mTokens so the derived
	// prompt total still counts it, but the true 5m/1h split — and so the
	// true price — is unknown.
	TTLUnknown bool
	IsStream   bool
	// MessageID is the upstream message id: message_start.message.id for a
	// streaming body, or the top-level id for a non-streaming body whose
	// own type is "message". First-seen wins when a body carries more than
	// one message_start. The gate proves shape only — that the body is
	// message-shaped — not that the id is per-request; a stable org or
	// gateway id would pass the same gate. Empty when neither shape is
	// present or the body carries no id.
	MessageID string
}

// Meta is everything the system needs to know about a request before it
// is stored, extracted from the captured request body and headers.
// ClientVersion, Project, GitBranch, IsSidechain, and CliEntrypoint are
// left zero here — only a JSONL-sourced capture (a different collector)
// supplies them; they exist on this type so a merge can preserve
// whichever writer supplied them.
type Meta struct {
	ModelRequested    string
	ServiceTier       string // requested service_tier, if present
	Speed             string // requested speed, if present
	Effort            string // requested effort, if present
	InferenceGeo      string // requested inference_geo, if present
	HasCacheControl   bool
	CacheControlSites []string // e.g. "tools[0]", "system[0]", "messages[3].content[1]"
	ToolCount         int
	ToolNames         []string
	HasThinking       bool
	ThinkingBudget    *int // thinking.budget_tokens (nil if absent)
	SessionHeader     string
	// PrefixHash is always computed by ExtractMeta, regardless of
	// SessionHeader: the session resolver's groupKey checks SessionHeader
	// first, so a present header still wins there — it is not expressed
	// by nil-ing this field. A non-nil "" means the request body did not
	// parse as JSON at all; any other non-nil value is the computed hash.
	// ExtractMeta never leaves this nil (nil only ever reaches
	// store.Event.PrefixHash for a JSONL-sourced row, which has no
	// request body to hash at all).
	PrefixHash *string

	ClientVersion string
	Project       string
	GitBranch     string
	IsSidechain   bool
	CliEntrypoint string
}
