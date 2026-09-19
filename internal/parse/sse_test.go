package parse

import (
	"testing"
)

// TestSplitChunkSSE is test 3: one event delivered across two Feed calls
// (an event split at a chunk boundary) must still parse as one frame.
func TestSplitChunkSSE(t *testing.T) {
	p := NewSSEParser()
	part1 := "event: message_start\ndata: {\"type\":\"mess"
	part2 := "age_start\",\"message\":{\"model\":\"claude-sonnet-5\"}}\n\n"

	frames := p.Feed([]byte(part1))
	if len(frames) != 0 {
		t.Fatalf("Feed(part1) = %d frames, want 0 (event not yet complete)", len(frames))
	}
	frames = p.Feed([]byte(part2))
	if len(frames) != 1 {
		t.Fatalf("Feed(part2) = %d frames, want 1", len(frames))
	}
	if frames[0].Type != "message_start" {
		t.Errorf("frame type = %q, want message_start", frames[0].Type)
	}
}

// TestSplitChunkSSEByteAtATime feeds the same event one byte at a time to
// exercise the carry buffer under the most adversarial chunking.
func TestSplitChunkSSEByteAtATime(t *testing.T) {
	p := NewSSEParser()
	raw := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	var got []Frame
	for _, b := range raw {
		got = append(got, p.Feed([]byte{b})...)
	}
	if len(got) != 1 {
		t.Fatalf("byte-at-a-time feed produced %d frames, want 1", len(got))
	}
	if got[0].Type != "message_stop" {
		t.Errorf("frame type = %q, want message_stop", got[0].Type)
	}
}

func TestFullEventSequence(t *testing.T) {
	p := NewSSEParser()
	stream := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-5\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":10}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	frames := p.Feed([]byte(stream))
	wantTypes := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(frames) != len(wantTypes) {
		t.Fatalf("got %d frames, want %d", len(frames), len(wantTypes))
	}
	for i, want := range wantTypes {
		if frames[i].Type != want {
			t.Errorf("frame[%d].Type = %q, want %q", i, frames[i].Type, want)
		}
	}
	rest, complete := p.Finish()
	if len(rest) != 0 {
		t.Errorf("Finish() returned %d leftover frames, want 0", len(rest))
	}
	if !complete {
		t.Error("complete = false, want true after message_stop")
	}
}

// TestStreamEndingWithoutMessageStopIsIncomplete is br-GI-1-10's
// stream_incomplete signal.
func TestStreamEndingWithoutMessageStopIsIncomplete(t *testing.T) {
	p := NewSSEParser()
	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	p.Feed([]byte(stream))
	_, complete := p.Finish()
	if complete {
		t.Error("complete = true, want false — stream ended without message_stop")
	}
}

func TestNonStreamJSONYieldsEquivalentFrame(t *testing.T) {
	body := []byte(`{"id":"msg_1","usage":{"input_tokens":5}}`)
	f := NonStreamFrame(body)
	if f.Type != "message" {
		t.Errorf("NonStreamFrame Type = %q, want message", f.Type)
	}
	if string(f.Data) != string(body) {
		t.Errorf("NonStreamFrame Data = %q, want %q", f.Data, body)
	}
}

// TestMalformedFrameIsSkippedNotFatal covers a "[DONE]" sentinel and a
// data-less event, neither of which should produce a frame or panic.
func TestMalformedFrameIsSkippedNotFatal(t *testing.T) {
	p := NewSSEParser()
	stream := "data: [DONE]\n\n" +
		"event: ping\n\n" + // no data: line at all
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	frames := p.Feed([]byte(stream))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 (the malformed ones must be skipped, not fatal)", len(frames))
	}
	if frames[0].Type != "message_stop" {
		t.Errorf("surviving frame type = %q, want message_stop", frames[0].Type)
	}
}

// TestFrameTypeSniffedFromJSONWhenNoEventLine covers a body that omits the
// SSE "event:" line but still carries "type" in its JSON payload.
func TestFrameTypeSniffedFromJSONWhenNoEventLine(t *testing.T) {
	p := NewSSEParser()
	frames := p.Feed([]byte("data: {\"type\":\"message_start\"}\n\n"))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Type != "message_start" {
		t.Errorf("sniffed type = %q, want message_start", frames[0].Type)
	}
}

func TestFindEventHandlesCRLFAndLoneCR(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"LF", "event: a\ndata: {}\n\n"},
		{"CRLF", "event: a\r\ndata: {}\r\n\r\n"},
		{"lone CR", "event: a\rdata: {}\r\r"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewSSEParser()
			frames := p.Feed([]byte(tt.in))
			if len(frames) != 1 {
				t.Fatalf("%s: got %d frames, want 1", tt.name, len(frames))
			}
		})
	}
}
