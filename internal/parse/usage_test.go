package parse

import (
	"os"
	"testing"
)

func totalPromptTokens(u Usage) int {
	return u.InputTokens + u.CacheWrite5mTokens + u.CacheWrite1hTokens + u.CacheReadTokens
}

func TestExtractUsageFullSSEStream(t *testing.T) {
	stream := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\",\"service_tier\":\"standard\",\"speed\":\"fast\",\"usage\":{\"input_tokens\":100,\"cache_creation\":{\"ephemeral_5m_input_tokens\":30,\"ephemeral_1h_input_tokens\":20},\"cache_read_input_tokens\":10}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":55,\"output_tokens_details\":{\"thinking_tokens\":12}}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	u := ExtractUsage([]byte(stream), "text/event-stream")

	if u.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want claude-sonnet-5", u.Model)
	}
	if u.InputTokens != 100 || u.CacheWrite5mTokens != 30 || u.CacheWrite1hTokens != 20 || u.CacheReadTokens != 10 {
		t.Errorf("cache classes = %+v, want input=100 5m=30 1h=20 read=10", u)
	}
	if u.OutputTokens != 55 {
		t.Errorf("OutputTokens = %d, want 55", u.OutputTokens)
	}
	if u.ThinkingTokens != 12 {
		t.Errorf("ThinkingTokens = %d, want 12", u.ThinkingTokens)
	}
	if u.ThinkingTokens >= u.OutputTokens {
		t.Errorf("ThinkingTokens (%d) must be a strict subset of OutputTokens (%d)", u.ThinkingTokens, u.OutputTokens)
	}
	if u.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q, want max_tokens", u.StopReason)
	}
	if u.StopCategory != "" {
		t.Errorf("StopCategory = %q, want empty (StopReason is not refusal)", u.StopCategory)
	}
	if u.ServiceTier != "standard" || u.Speed != "fast" {
		t.Errorf("ServiceTier/Speed = %q/%q, want standard/fast", u.ServiceTier, u.Speed)
	}
	if !u.IsStream {
		t.Error("IsStream = false, want true")
	}
}

func TestExtractUsageRefusalPopulatesStopCategory(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\",\"stop_details\":{\"category\":\"policy\"}},\"usage\":{\"output_tokens\":1}}\n\n"
	u := ExtractUsage([]byte(stream), "text/event-stream")
	if u.StopReason != "refusal" {
		t.Fatalf("StopReason = %q, want refusal", u.StopReason)
	}
	if u.StopCategory != "policy" {
		t.Errorf("StopCategory = %q, want policy", u.StopCategory)
	}
}

func TestExtractUsageNonRefusalIgnoresStopDetails(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_details\":{\"category\":\"should-be-ignored\"}},\"usage\":{\"output_tokens\":1}}\n\n"
	u := ExtractUsage([]byte(stream), "text/event-stream")
	if u.StopCategory != "" {
		t.Errorf("StopCategory = %q, want empty when StopReason != refusal", u.StopCategory)
	}
}

// TestFlatAndSplitCacheCreationYieldSameTotal is test 9's tolerance half:
// a body carrying the ephemeral_*/1h split and a body carrying only the
// flat cache_creation_input_tokens must parse to the same prompt total,
// with the flat shape flagged TTLUnknown.
func TestFlatAndSplitCacheCreationYieldSameTotal(t *testing.T) {
	split, err := os.ReadFile("testdata/cache_split.json")
	if err != nil {
		t.Fatal(err)
	}
	flat, err := os.ReadFile("testdata/cache_flat.json")
	if err != nil {
		t.Fatal(err)
	}

	splitUsage := ExtractUsage(split, "application/json")
	flatUsage := ExtractUsage(flat, "application/json")

	if splitUsage.TTLUnknown {
		t.Error("split fixture: TTLUnknown = true, want false")
	}
	if !flatUsage.TTLUnknown {
		t.Error("flat fixture: TTLUnknown = false, want true")
	}

	splitTotal := totalPromptTokens(splitUsage)
	flatTotal := totalPromptTokens(flatUsage)
	if splitTotal != flatTotal {
		t.Fatalf("split total = %d, flat total = %d, want equal", splitTotal, flatTotal)
	}
}

func TestExtractUsageNonStreamJSON(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2}}`)
	u := ExtractUsage(body, "application/json")
	if u.Model != "claude-sonnet-5" || u.InputTokens != 7 || u.OutputTokens != 3 || u.CacheReadTokens != 2 {
		t.Errorf("non-stream usage = %+v, unexpected", u)
	}
	if u.IsStream {
		t.Error("IsStream = true, want false for a non-streaming body")
	}
	if u.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", u.StopReason)
	}
}

func TestExtractUsageUnrecognizedContentTypeYieldsZeroUsage(t *testing.T) {
	u := ExtractUsage([]byte("whatever"), "text/plain")
	if u != (Usage{}) {
		t.Errorf("Usage = %+v, want zero value", u)
	}
}

func TestExtractUsageStreamingMessageIDPopulated(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_abc123\",\"model\":\"m\",\"usage\":{}}}\n\n"
	u := ExtractUsage([]byte(stream), "text/event-stream")
	if u.MessageID != "msg_abc123" {
		t.Errorf("MessageID = %q, want msg_abc123", u.MessageID)
	}
}

func TestExtractUsageNonStreamMessageIDPopulatedWhenTypeIsMessage(t *testing.T) {
	body := []byte(`{"id":"msg_xyz789","type":"message","model":"m","usage":{}}`)
	u := ExtractUsage(body, "application/json")
	if u.MessageID != "msg_xyz789" {
		t.Errorf("MessageID = %q, want msg_xyz789", u.MessageID)
	}
}

func TestExtractUsageNonStreamMessageIDGatedOnBodyType(t *testing.T) {
	body := []byte(`{"id":"not_a_message_id","type":"error","model":"m","usage":{}}`)
	u := ExtractUsage(body, "application/json")
	if u.MessageID != "" {
		t.Errorf("MessageID = %q, want empty — body type is not %q", u.MessageID, "message")
	}
}

func TestExtractUsageTwoMessageStartFramesFirstIDWins(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_first\",\"model\":\"m\",\"usage\":{}}}\n\n" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_second\",\"model\":\"m\",\"usage\":{}}}\n\n"
	u := ExtractUsage([]byte(stream), "text/event-stream")
	if u.MessageID != "msg_first" {
		t.Errorf("MessageID = %q, want msg_first (first-seen wins)", u.MessageID)
	}
}

func TestExtractUsageNoIDYieldsEmptyMessageID(t *testing.T) {
	streamBody := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"boom"}}`)
	u := ExtractUsage(streamBody, "application/json")
	if u.MessageID != "" {
		t.Errorf("MessageID = %q, want empty for an error body", u.MessageID)
	}

	nonStream := ExtractUsage([]byte(`{"model":"m","usage":{}}`), "application/json")
	if nonStream.MessageID != "" {
		t.Errorf("MessageID = %q, want empty when the body carries no id and no type", nonStream.MessageID)
	}
}
