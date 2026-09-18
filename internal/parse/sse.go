package parse

import (
	"bytes"
	"encoding/json"
	"strings"
)

// SSEParser incrementally splits an SSE byte stream into Frames. Feed may
// be called any number of times with chunks of any size — including a
// single byte — so an event split across a chunk boundary (test 3) parses
// correctly once its terminating blank line arrives. It tracks the Claude
// event sequence only far enough to answer one question: did the stream
// end with message_stop? A stream that did not is incomplete.
type SSEParser struct {
	buf            []byte
	sawMessageStop bool
}

// NewSSEParser returns an empty parser ready for Feed.
func NewSSEParser() *SSEParser {
	return &SSEParser{}
}

// Feed appends chunk to the internal carry buffer and returns every
// complete frame found so far. Any incomplete tail — including a lone
// trailing '\r' that might be the start of a terminator split across
// calls — is retained for the next Feed or Finish.
func (p *SSEParser) Feed(chunk []byte) []Frame {
	p.buf = append(p.buf, chunk...)
	var frames []Frame
	for {
		event, rest, found := findEvent(p.buf)
		if !found {
			return frames
		}
		p.buf = rest
		if f, ok := parseEvent(event); ok {
			p.track(f)
			frames = append(frames, f)
		}
	}
}

// Finish flushes any buffered remainder (an event with no trailing blank
// line, e.g. a body that ends right after message_stop) and reports
// whether message_stop was seen — the stream_incomplete signal.
func (p *SSEParser) Finish() (frames []Frame, complete bool) {
	if len(bytes.TrimSpace(p.buf)) > 0 {
		if f, ok := parseEvent(p.buf); ok {
			p.track(f)
			frames = append(frames, f)
		}
	}
	p.buf = nil
	return frames, p.sawMessageStop
}

func (p *SSEParser) track(f Frame) {
	if f.Type == "message_stop" {
		p.sawMessageStop = true
	}
}

// findEvent locates the first complete SSE event in buf, delimited by a
// blank line: \n\n, \r\n\r\n, or \r\r (SSE permits all three; a literal
// "\n\n" substring search alone would never match a CRLF stream). found is
// false when buf's tail cannot yet be told apart from an in-progress
// terminator — the whole of buf is then the carry, unchanged, for the
// next call.
func findEvent(buf []byte) (event, rest []byte, found bool) {
	n := len(buf)
	for i := 0; i < n; i++ {
		switch buf[i] {
		case '\n':
			if i+1 >= n {
				return nil, buf, false
			}
			if buf[i+1] == '\n' {
				return buf[:i], buf[i+2:], true
			}
		case '\r':
			if i+1 >= n {
				return nil, buf, false
			}
			if buf[i+1] == '\r' {
				return buf[:i], buf[i+2:], true
			}
			if buf[i+1] == '\n' {
				if i+2 >= n {
					return nil, buf, false
				}
				if buf[i+2] != '\r' {
					continue // ordinary CRLF line ending, not a terminator
				}
				if i+3 >= n {
					return nil, buf, false
				}
				if buf[i+3] == '\n' {
					return buf[:i], buf[i+4:], true
				}
			}
		}
	}
	return nil, buf, false
}

// ponytail: findEvent rescans buf from 0 on every Feed call, so a stream
// fed one byte at a time is O(n^2) over its own length. Response bodies
// are capped and real upstream chunks aren't 1 byte, so this is not a live
// concern; track a resume offset if a future bead feeds this from many
// tiny writes.

// parseEvent splits one event block into its event:/data: lines, skips it
// if there is no data at all or it is the "[DONE]" sentinel, and resolves
// the frame's Type from an explicit "event:" line first, falling back to
// the JSON payload's own "type" field. A frame whose type cannot be
// resolved either way is still returned (with an empty Type) rather than
// dropped — the package degrades rather than fails on a shape it cannot
// finish reading.
func parseEvent(raw []byte) (Frame, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Frame{}, false
	}
	var dataParts []string
	var eventType string
	for _, line := range splitLines(raw) {
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			dataParts = append(dataParts, d)
		}
	}
	if len(dataParts) == 0 {
		return Frame{}, false
	}
	data := strings.Join(dataParts, "\n")
	if data == "[DONE]" {
		return Frame{}, false
	}
	if eventType == "" {
		eventType = sniffType(data)
	}
	return Frame{Type: eventType, Data: []byte(data)}, true
}

// sniffType reads only a JSON payload's "type" field, so a shape this
// package cannot otherwise parse still contributes a frame type when the
// SSE "event:" line was absent.
func sniffType(data string) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return ""
	}
	return probe.Type
}

// splitLines splits an SSE event block into lines, recognising "\n",
// "\r\n", and a lone "\r" as line endings.
func splitLines(b []byte) []string {
	var lines []string
	start := 0
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\n':
			line := b[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, string(line))
			start = i + 1
		case '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				continue // handled by the '\n' case above
			}
			lines = append(lines, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, string(b[start:]))
	}
	return lines
}
