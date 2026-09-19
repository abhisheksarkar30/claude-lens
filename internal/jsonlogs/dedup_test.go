package jsonlogs

import "testing"

// Test 8 -- duplicate-usage dedup: two assistant lines sharing a
// requestId with byte-identical usage must yield the tokens of the
// distinct request, never the sum of both lines (the measured ~1.75x
// inflation defect).
func TestDedupeAssistantLinesTakesDistinctRequest(t *testing.T) {
	l1 := &line{Type: "assistant", RequestID: "req_dup1", SessionID: "sess-dup", UUID: "a1",
		Message: &message{Model: "claude-sonnet-5", Usage: &usageShape{InputTokens: 100, OutputTokens: 50}}}
	l2 := &line{Type: "assistant", RequestID: "req_dup1", SessionID: "sess-dup", UUID: "a2",
		Message: &message{Model: "claude-sonnet-5", Usage: &usageShape{InputTokens: 100, OutputTokens: 50}}}

	distinct := dedupeAssistantLines([]*line{l1, l2})
	if len(distinct) != 1 {
		t.Fatalf("dedupeAssistantLines returned %d requests, want 1 (two lines, one requestId)", len(distinct))
	}
	usage := usageFromShape(distinct[0].Message.Usage)
	if usage.InputTokens != 100 || usage.OutputTokens != 50 {
		t.Errorf("tokens = %+v, want the distinct request's 100/50, not the summed 200/100", usage)
	}
}

// A line with no requestId falls back to jsonl:<sessionId>:<uuid>, and
// two such lines with different uuids are never collapsed together.
func TestDedupeAssistantLinesFallbackKeyPerUUID(t *testing.T) {
	l1 := &line{Type: "assistant", SessionID: "sess-x", UUID: "u1",
		Message: &message{Usage: &usageShape{InputTokens: 1}}}
	l2 := &line{Type: "assistant", SessionID: "sess-x", UUID: "u2",
		Message: &message{Usage: &usageShape{InputTokens: 2}}}

	distinct := dedupeAssistantLines([]*line{l1, l2})
	if len(distinct) != 2 {
		t.Fatalf("dedupeAssistantLines returned %d requests, want 2 (distinct uuids, no requestId)", len(distinct))
	}
	if requestKey(l1) == requestKey(l2) {
		t.Errorf("fallback keys collided: %q", requestKey(l1))
	}
}

// Non-assistant lines and assistant lines with no usage object never
// enter dedup.
func TestDedupeAssistantLinesSkipsNonUsageLines(t *testing.T) {
	lines := []*line{
		{Type: "user", SessionID: "sess-x"},
		{Type: "assistant", SessionID: "sess-x", RequestID: "req_no_usage"}, // Message nil
		{Type: "assistant", SessionID: "sess-x", RequestID: "req_has_usage", Message: &message{Usage: &usageShape{InputTokens: 1}}},
	}
	distinct := dedupeAssistantLines(lines)
	if len(distinct) != 1 {
		t.Fatalf("dedupeAssistantLines returned %d requests, want 1", len(distinct))
	}
	if distinct[0].RequestID != "req_has_usage" {
		t.Errorf("kept requestId %q, want req_has_usage", distinct[0].RequestID)
	}
}

// Test 9 -- both cache shapes (the ephemeral_5m/1h split and the flat
// cache_creation_input_tokens fallback) parse to the same prompt total;
// the flat shape is flagged TTLUnknown ("approximate").
func TestUsageFromShapeBothCacheShapesAgreeOnPromptTotal(t *testing.T) {
	split := usageFromShape(&usageShape{
		InputTokens: 10, OutputTokens: 5,
		CacheCreation: &struct {
			Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
		}{Ephemeral5m: 20, Ephemeral1h: 0},
	})
	flat := usageFromShape(&usageShape{
		InputTokens: 10, OutputTokens: 5,
		CacheCreationInputTokens: 20,
	})

	splitTotal := split.InputTokens + split.CacheWrite5mTokens + split.CacheWrite1hTokens + split.CacheReadTokens
	flatTotal := flat.InputTokens + flat.CacheWrite5mTokens + flat.CacheWrite1hTokens + flat.CacheReadTokens
	if splitTotal != flatTotal {
		t.Fatalf("prompt totals differ: split=%d flat=%d, want equal", splitTotal, flatTotal)
	}
	if split.TTLUnknown {
		t.Error("split-shape usage flagged TTLUnknown, want false")
	}
	if !flat.TTLUnknown {
		t.Error("flat-shape usage not flagged TTLUnknown, want true (approximate)")
	}
}

// A malformed line is a parse error, never a panic.
func TestParseLineMalformed(t *testing.T) {
	if _, err := parseLine([]byte("{this is not json")); err == nil {
		t.Error("parseLine accepted malformed JSON, want an error")
	}
}

// Every one of the 13 tolerated types is recognised.
func TestToleratedTypesCoversThirteen(t *testing.T) {
	if len(toleratedTypes) != 13 {
		t.Fatalf("toleratedTypes has %d entries, want 13", len(toleratedTypes))
	}
	for _, want := range []string{
		"attachment", "file-history-snapshot", "atis-latch", "file-history-delta",
		"ai-title", "last-prompt", "queue-operation", "mode", "pr-link",
	} {
		if !toleratedTypes[want] {
			t.Errorf("toleratedTypes missing %q", want)
		}
	}
}
