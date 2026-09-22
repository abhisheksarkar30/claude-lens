// Package decode removes a captured body's transport Content-Encoding.
//
// clens tees the bytes the client and upstream actually exchanged, so a
// captured body arrives exactly as it went over the wire: compressed,
// whenever the client negotiated compression. Claude Code sends
// "Accept-Encoding: gzip, deflate, br, zstd", and api.anthropic.com answers
// with a non-identity Content-Encoding for most responses. Parsing those
// bytes as-is means every compressed call reports zero tokens and an
// unresolved model — internal/parse cannot read a compressed stream, so
// extraction finds no usage event at all.
//
// Decoding belongs here, in the cold path, and deliberately not in
// internal/proxy: CLAUDE.md's "the proxy listener holds the hot path and
// does no parsing; it tees the bytes into a bounded sink and returns" is
// exactly the invariant that keeps decompression off the client's
// goroutine. A decoder inline in the tee would put the work back on the
// hot path, which is the one thing the TTFB test exists to prevent.
package decode

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// ErrUnsupported is returned for a Content-Encoding this package
// implements no decoder for. It is exported so a caller can tell "clens
// cannot read this encoding" from "this body is corrupt" without
// string-matching the error message.
var ErrUnsupported = errors.New("decode: unsupported Content-Encoding")

// Completeness says how much of the decoded body the caller got.
//
// Body has always been able to tell these apart -- it reads one byte past its
// cap, and it already distinguishes "a prefix decoded, then the stream broke"
// from "nothing decoded at all" -- and used to discard the distinction. It
// cannot be reconstructed from err, which means only "nothing decoded".
type Completeness int

const (
	// Complete means the whole body is available: it carried no
	// Content-Encoding, or the stream reached EOF within the cap.
	Complete Completeness = iota
	// TruncatedAtCap means more than limit bytes decoded, and the prefix is
	// what came back. A body decoding to exactly limit is Complete, not this
	// -- which is why the read below asks for limit+1 bytes.
	TruncatedAtCap
	// PartialCorrupt means a non-empty prefix decoded and then a read error
	// ended the stream. The prefix is returned and err is nil.
	PartialCorrupt
	// NotDecoded means nothing was decoded: an encoding this package cannot
	// read, or a compressed body that produced no bytes. body comes back
	// unchanged and err is non-nil.
	NotDecoded
)

// String names the value, so a marker rendered from it and a test failure
// both say which state it is rather than printing an int.
func (c Completeness) String() string {
	switch c {
	case Complete:
		return "Complete"
	case TruncatedAtCap:
		return "TruncatedAtCap"
	case PartialCorrupt:
		return "PartialCorrupt"
	case NotDecoded:
		return "NotDecoded"
	}
	return fmt.Sprintf("Completeness(%d)", int(c))
}

// Body returns body with h's Content-Encoding undone, and a copy of h with
// the headers that described the encoded form (Content-Encoding,
// Content-Length) removed — so the returned pair describes the returned
// bytes consistently. That consistency is what keeps a stored row
// replayable: a replay re-sends a row's stored body with that row's stored
// headers, so leaving a stale Content-Encoding behind would make a replay
// declare an encoding its body no longer has.
//
// Decoded output is capped at limit bytes, because the cap internal/proxy
// applies is on the *compressed* capture: 2MB of brotli can expand to tens of
// megabytes, so capping only the encoded form would let the configured
// per-body cap be exceeded by whatever ratio the upstream chose.
//
// A body that decodes only partially (the proxy's cap cut the encoded
// stream short, common for a large streamed response) yields the decoded
// prefix and no error: some parsed usage beats none, and internal/parse
// already degrades rather than fails on a shape it cannot finish reading.
//
// An encoding this package does not implement returns body and h
// unchanged, along with an error naming it, so the caller stores what was
// captured and can say why.
//
// The Completeness result says how much of the decoded body the caller
// actually got. It is not derivable from err: err means "nothing decoded",
// never "the cap cut this short" or "a clean prefix then a corrupt tail".
// A caller that renders the bytes to a user needs the difference.
func Body(h http.Header, body []byte, limit int) ([]byte, http.Header, Completeness, error) {
	encs := encodings(h.Get("Content-Encoding"))
	if len(encs) == 0 {
		// Already plaintext -- the tool's central case (a plain or
		// SSE-streamed response), and the one most easily misread as
		// NotDecoded because it decodes nothing. The bytes are whole.
		return body, h, Complete, nil
	}
	if limit <= 0 {
		return body, h, NotDecoded, fmt.Errorf("decode: limit must be positive, got %d", limit)
	}

	// Content-Encoding lists codings in the order they were applied
	// (RFC 9110), so the last one listed is the first that has to come off.
	r := io.Reader(bytes.NewReader(body))
	var closers []io.Closer
	for i := len(encs) - 1; i >= 0; i-- {
		wrap, ok := wrapperFor(encs[i])
		if !ok {
			closeAll(closers)
			return body, h, NotDecoded, fmt.Errorf("%w: %q", ErrUnsupported, encs[i])
		}
		next, c, err := wrap(r)
		if err != nil {
			closeAll(closers)
			return body, h, NotDecoded, fmt.Errorf("decode: %s: %w", strings.Join(encs, ", "), err)
		}
		r = next
		if c != nil {
			closers = append(closers, c)
		}
	}

	// One byte past the cap, so a body that exactly fills it is not
	// mistaken for a truncated one.
	out, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	closeAll(closers)

	// The cap is tested against the pre-truncation length: that is the whole
	// reason for reading limit+1 bytes, and it is what makes a body decoding
	// to exactly limit Complete rather than TruncatedAtCap.
	completeness := Complete
	if len(out) > limit {
		completeness, out = TruncatedAtCap, out[:limit]
	}
	if err != nil {
		if len(out) == 0 {
			return body, h, NotDecoded, fmt.Errorf("decode: %s: %w", strings.Join(encs, ", "), err)
		}
		// A clean prefix, then the stream broke. Truncation at the cap is
		// the binding constraint when both happened: the cap is a
		// deliberate cut, the error incidental.
		if completeness == Complete {
			completeness = PartialCorrupt
		}
	}

	stripped := h.Clone()
	stripped.Del("Content-Encoding")
	stripped.Del("Content-Length")
	return out, stripped, completeness, nil
}

// encodings splits a Content-Encoding field value into the codings that
// actually transform the body, lowercased. "identity" and empty elements
// are dropped: both mean the body is already in the form we want.
func encodings(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		e := strings.ToLower(strings.TrimSpace(part))
		if e == "" || e == "identity" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// wrapperFor returns the decoder for one coding. The second result is
// false for a coding this package does not implement.
//
// The returned io.Closer is nil for a decoder that owns no resources
// (brotli's reader is a plain io.Reader); Body collects the closers it is
// given and closes them once the read is done, so a zstd decoder's
// internal state is released even on the partial-decode path.
func wrapperFor(enc string) (func(io.Reader) (io.Reader, io.Closer, error), bool) {
	switch enc {
	case "gzip", "x-gzip":
		return func(r io.Reader) (io.Reader, io.Closer, error) {
			zr, err := gzip.NewReader(r)
			if err != nil {
				return nil, nil, err
			}
			return zr, zr, nil
		}, true

	case "deflate":
		// ponytail: RFC-correct zlib framing only. A non-conformant sender
		// that means raw DEFLATE gets its body stored undecoded and a
		// logged error; add the raw fallback if such a sender ever shows
		// up in a stored row.
		return func(r io.Reader) (io.Reader, io.Closer, error) {
			zr, err := zlib.NewReader(r)
			if err != nil {
				return nil, nil, err
			}
			return zr, zr, nil
		}, true

	case "br":
		return func(r io.Reader) (io.Reader, io.Closer, error) {
			return brotli.NewReader(r), nil, nil
		}, true

	case "zstd":
		return func(r io.Reader) (io.Reader, io.Closer, error) {
			// One goroutine per body rather than the default
			// GOMAXPROCS-many: this decoder is built fresh for every
			// captured call, which is capped at a few hundred KB.
			zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, nil, err
			}
			return zr, zstdCloser{zr}, nil
		}, true
	}
	return nil, false
}

// closeAll releases every decoder opened for one body. Errors are
// dropped: a close failure here cannot change the decoded bytes already
// read, and failing a call because a decoder was unhappy on teardown
// would trade a correct row for none.
func closeAll(closers []io.Closer) {
	for _, c := range closers {
		_ = c.Close()
	}
}

// zstdCloser adapts *zstd.Decoder to io.Closer. The decoder's own Close
// returns no error, so it does not satisfy io.Closer directly.
type zstdCloser struct{ d *zstd.Decoder }

func (c zstdCloser) Close() error {
	c.d.Close()
	return nil
}
