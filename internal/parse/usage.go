package parse

import (
	"encoding/json"
	"strings"
)

// usageJSON is the narrow shape of an Anthropic "usage" object.
// Unmarshaling into this rather than a full API model means an
// unexpected upstream field can never break parsing.
type usageJSON struct {
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

type stopDetailsJSON struct {
	Category string `json:"category"`
}

// sseMessageStart is message_start's payload: the Message object, carrying
// model, service_tier, speed, and the initial usage snapshot.
type sseMessageStart struct {
	Message struct {
		ID          string    `json:"id"`
		Model       string    `json:"model"`
		ServiceTier string    `json:"service_tier"`
		Speed       string    `json:"speed"`
		Usage       usageJSON `json:"usage"`
	} `json:"message"`
}

// sseMessageDelta is message_delta's payload. Usage.OutputTokens here is
// the running cumulative total, not an increment — the last value seen
// wins; summing across deltas is the classic bug.
type sseMessageDelta struct {
	Delta struct {
		StopReason  string           `json:"stop_reason"`
		StopDetails *stopDetailsJSON `json:"stop_details"`
	} `json:"delta"`
	Usage *usageJSON `json:"usage"`
}

// nonStreamBody is the narrow shape of a complete, non-streaming
// application/json response body: the same fields an SSE stream would
// deliver across message_start and message_delta, all at the top level.
type nonStreamBody struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	Model       string           `json:"model"`
	ServiceTier string           `json:"service_tier"`
	Speed       string           `json:"speed"`
	StopReason  string           `json:"stop_reason"`
	StopDetails *stopDetailsJSON `json:"stop_details"`
	Usage       usageJSON        `json:"usage"`
}

// ExtractUsage is the single-entry convenience over a complete, captured
// response body: it frames the body (via SSEParser for a stream, or
// NonStreamFrame for a complete JSON body) and folds the resulting frames
// into a Usage. An unrecognised content type returns a zero Usage — an
// upstream shape change must degrade to "no tokens recorded", never to
// broken capture.
func ExtractUsage(respBody []byte, contentType string) Usage {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))

	switch ct {
	case "application/json":
		return usageFromFrames([]Frame{NonStreamFrame(respBody)}, false)
	case "text/event-stream":
		p := NewSSEParser()
		frames := p.Feed(respBody)
		rest, _ := p.Finish()
		frames = append(frames, rest...)
		return usageFromFrames(frames, true)
	default:
		return Usage{}
	}
}

// usageFromFrames folds a sequence of Frames into a Usage. A frame whose
// JSON does not match the expected shape is skipped, not fatal — the
// usage accumulated so far stands.
func usageFromFrames(frames []Frame, isStream bool) Usage {
	u := Usage{IsStream: isStream}
	for _, f := range frames {
		switch f.Type {
		case "message_start":
			var ev sseMessageStart
			if json.Unmarshal(f.Data, &ev) != nil {
				continue
			}
			u.Model = ev.Message.Model
			u.ServiceTier = ev.Message.ServiceTier
			u.Speed = ev.Message.Speed
			if u.MessageID == "" {
				u.MessageID = ev.Message.ID
			}
			applyUsageJSON(&u, ev.Message.Usage)

		case "message_delta":
			var ev sseMessageDelta
			if json.Unmarshal(f.Data, &ev) != nil {
				continue
			}
			if ev.Delta.StopReason != "" {
				u.StopReason = ev.Delta.StopReason
			}
			if u.StopReason == "refusal" && ev.Delta.StopDetails != nil {
				u.StopCategory = ev.Delta.StopDetails.Category
			}
			if ev.Usage != nil {
				u.OutputTokens = ev.Usage.OutputTokens
				if ev.Usage.OutputTokensDetails != nil {
					u.ThinkingTokens = ev.Usage.OutputTokensDetails.ThinkingTokens
				}
			}

		case "message":
			var body nonStreamBody
			if json.Unmarshal(f.Data, &body) != nil {
				continue
			}
			u.Model = body.Model
			u.ServiceTier = body.ServiceTier
			u.Speed = body.Speed
			u.StopReason = body.StopReason
			if body.StopReason == "refusal" && body.StopDetails != nil {
				u.StopCategory = body.StopDetails.Category
			}
			if u.MessageID == "" && body.Type == "message" {
				u.MessageID = body.ID
			}
			applyUsageJSON(&u, body.Usage)
		}
	}
	return u
}

// applyUsageJSON folds one usage object into u. It is shared by
// message_start (the SSE form) and the non-stream body (the same shape at
// the top level) so the flat-cache-creation fallback is implemented once.
func applyUsageJSON(u *Usage, uj usageJSON) {
	u.InputTokens = uj.InputTokens
	u.CacheReadTokens = uj.CacheReadInputTokens
	if uj.OutputTokens > 0 {
		u.OutputTokens = uj.OutputTokens
	}
	if uj.OutputTokensDetails != nil {
		u.ThinkingTokens = uj.OutputTokensDetails.ThinkingTokens
	}

	switch {
	case uj.CacheCreation != nil:
		u.CacheWrite5mTokens = uj.CacheCreation.Ephemeral5m
		u.CacheWrite1hTokens = uj.CacheCreation.Ephemeral1h
	case uj.CacheCreationInputTokens > 0:
		// Flat-cache-creation fallback (belt-and-braces): the ephemeral
		// 5m/1h split is absent, so the whole flat count is recorded into
		// CacheWrite5mTokens — the derived prompt total still counts it —
		// and TTLUnknown flags that the true 5m/1h split, and so the true
		// price, is unknown.
		u.CacheWrite5mTokens = uj.CacheCreationInputTokens
		u.TTLUnknown = true
	}
}
