// Package parse turns a captured response body into a stream of frames
// (this file and sse.go). It is a pure-function package: no network I/O,
// no dependency on the proxy or sink — everything here operates on bytes
// already captured elsewhere.
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
