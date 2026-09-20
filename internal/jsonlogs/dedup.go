package jsonlogs

import (
	"encoding/json"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
)

// line is one parsed JSONL record. Only "assistant" lines carry Message;
// every other field decodes tolerantly -- an absent field just zeroes,
// never an error, since a line's shape varies by type.
type line struct {
	Type          string   `json:"type"`
	UUID          string   `json:"uuid"`
	SessionID     string   `json:"sessionId"`
	RequestID     string   `json:"requestId"`
	Timestamp     string   `json:"timestamp"`
	CWD           string   `json:"cwd"`
	GitBranch     string   `json:"gitBranch"`
	Version       string   `json:"version"`
	CliEntrypoint string   `json:"cliEntrypoint"`
	Message       *message `json:"message"`
}

type message struct {
	Model      string      `json:"model"`
	StopReason string      `json:"stop_reason"`
	Usage      *usageShape `json:"usage"`

	// Content is the message's own content array, kept as raw JSON: a real
	// transcript carries text, thinking, tool_use and tool_result blocks in
	// it, and decoding into a fixed shape here would discard whichever block
	// type this struct did not anticipate. It is the one thing a transcript
	// holds that the tool otherwise never sees -- the proxy cannot observe a
	// transcript line and the transcript cannot observe a request.
	Content json.RawMessage `json:"content"`

	// Role comes from the transcript's own shape
	// ({"message":{"role":"assistant","content":[...]}}). Only "assistant"
	// lines carry Message at all (see the line doc above), so this is
	// "assistant" in practice; it is read from the payload rather than
	// hardcoded because the column is named transcript_role and should say
	// what the transcript said.
	Role string `json:"role"`
}

// usageShape mirrors the wire shape of Anthropic's usage object -- the
// same shape parse.usageJSON decodes for the proxy's SSE/non-stream
// bodies. It is reimplemented here in miniature (rather than exported
// from parse) because a JSONL line's usage sits directly under `message`,
// with no SSE framing to share.
type usageShape struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"` // flat fallback shape
	CacheCreation            *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	OutputTokensDetails  *struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// toleratedTypes are the non-assistant line types observed in real Claude
// Code logs -- skipped silently rather than counted as unknown (test 9).
var toleratedTypes = map[string]bool{
	"user":                  true,
	"summary":               true,
	"system":                true,
	"tool-result":           true,
	"attachment":            true,
	"file-history-snapshot": true,
	"atis-latch":            true,
	"file-history-delta":    true,
	"ai-title":              true,
	"last-prompt":           true,
	"queue-operation":       true,
	"mode":                  true,
	"pr-link":               true,
}

// parseLine unmarshals one raw JSONL line. A malformed line returns an
// error -- the caller counts it and keeps tailing, never treats it as
// fatal (test 9).
func parseLine(raw []byte) (*line, error) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// requestKey is the dedup key: the JSONL requestId, or
// jsonl:<sessionId>:<uuid> when a line carries none. This is the primary
// key the plan's cross-source identity section assumes byte-equals the
// proxy's request-id response header -- an assumption not yet verified
// live (see the bead's test 11b flag).
func requestKey(l *line) string {
	if l.RequestID != "" {
		return l.RequestID
	}
	return "jsonl:" + l.SessionID + ":" + l.UUID
}

// isAssistantUsage reports whether l is an assistant line carrying a
// usage object -- the only lines dedup and token extraction consider.
func isAssistantUsage(l *line) bool {
	return l.Type == "assistant" && l.Message != nil && l.Message.Usage != nil
}

// dedupeAssistantLines groups assistant-with-usage lines by their request
// key and returns one representative per key, in first-seen order. The
// measured defect (test 8) is two byte-identical usage lines per
// requestId -- one written per content block -- so this keeps exactly one
// line's tokens per key, the last one seen, never the sum of lines.
func dedupeAssistantLines(lines []*line) []*line {
	order := make([]string, 0, len(lines))
	byKey := map[string]*line{}
	for _, l := range lines {
		if !isAssistantUsage(l) {
			continue
		}
		key := requestKey(l)
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = l
	}
	out := make([]*line, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

// usageFromShape converts u into parse.Usage's token fields, mirroring
// parse's flat-cache-creation fallback rule: the ephemeral 5m/1h split
// wins when present; otherwise the flat count is recorded into
// CacheWrite5mTokens (the derived prompt total still counts it) and
// TTLUnknown flags the true 5m/1h split -- and so the true price -- as
// unknown (the "approximate" cache shape, test 9). Both shapes must yield
// the same prompt total.
func usageFromShape(u *usageShape) parse.Usage {
	if u == nil {
		return parse.Usage{}
	}
	out := parse.Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens,
	}
	if u.OutputTokensDetails != nil {
		out.ThinkingTokens = u.OutputTokensDetails.ThinkingTokens
	}
	switch {
	case u.CacheCreation != nil:
		out.CacheWrite5mTokens = u.CacheCreation.Ephemeral5m
		out.CacheWrite1hTokens = u.CacheCreation.Ephemeral1h
	case u.CacheCreationInputTokens > 0:
		out.CacheWrite5mTokens = u.CacheCreationInputTokens
		out.TTLUnknown = true
	}
	return out
}
