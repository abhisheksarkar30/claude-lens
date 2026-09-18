package decode

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// encode returns data with each named coding applied in order, which is the
// order a Content-Encoding field lists them in (RFC 9110: codings are
// listed in the order they were applied).
func encode(t *testing.T, data []byte, encs ...string) []byte {
	t.Helper()
	out := data
	for _, e := range encs {
		var buf bytes.Buffer
		var w interface {
			Write([]byte) (int, error)
			Close() error
		}
		switch e {
		case "gzip":
			w = gzip.NewWriter(&buf)
		case "deflate":
			w = zlib.NewWriter(&buf)
		case "br":
			w = brotli.NewWriter(&buf)
		case "zstd":
			zw, err := zstd.NewWriter(&buf)
			if err != nil {
				t.Fatalf("zstd.NewWriter: %v", err)
			}
			w = zw
		default:
			t.Fatalf("encode: unknown coding %q", e)
		}
		if _, err := w.Write(out); err != nil {
			t.Fatalf("encode %s: write: %v", e, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("encode %s: close: %v", e, err)
		}
		out = buf.Bytes()
	}
	return out
}

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// TestBodyDecodesEveryAdvertisedCoding pins the set Claude Code can
// negotiate ("Accept-Encoding: gzip, deflate, br, zstd"). A coding missing
// here is a coding whose calls silently report zero tokens.
func TestBodyDecodesEveryAdvertisedCoding(t *testing.T) {
	want := []byte(`{"usage":{"input_tokens":215,"output_tokens":887}}`)
	for _, coding := range []string{"gzip", "deflate", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			h := header("Content-Encoding", coding, "Content-Type", "application/json")
			got, gotHeaders, err := Body(h, encode(t, want, coding), 1<<20)
			if err != nil {
				t.Fatalf("Body: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("body = %q, want %q", got, want)
			}
			if v := gotHeaders.Get("Content-Encoding"); v != "" {
				t.Errorf("Content-Encoding = %q, want removed", v)
			}
			if v := gotHeaders.Get("Content-Length"); v != "" {
				t.Errorf("Content-Length = %q, want removed", v)
			}
			if v := gotHeaders.Get("Content-Type"); v != "application/json" {
				t.Errorf("Content-Type = %q, want preserved", v)
			}
		})
	}
}

// TestBodyUndoesCodingsInReverseOrder covers the multi-coding form.
func TestBodyUndoesCodingsInReverseOrder(t *testing.T) {
	want := []byte(`{"usage":{"input_tokens":1,"output_tokens":2}}`)
	h := header("Content-Encoding", "gzip, br")
	got, _, err := Body(h, encode(t, want, "gzip", "br"), 1<<20)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestBodyLeavesUnencodedBodiesAlone(t *testing.T) {
	for _, ce := range []string{"", "identity", "  ", "identity, identity"} {
		t.Run("encoding="+ce, func(t *testing.T) {
			body := []byte(`{"usage":{"input_tokens":3}}`)
			h := header("Content-Length", "25", "Content-Type", "application/json")
			if ce != "" {
				h.Set("Content-Encoding", ce)
			}
			got, gotHeaders, err := Body(h, body, 1<<20)
			if err != nil {
				t.Fatalf("Body: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("body = %q, want unchanged %q", got, body)
			}
			if v := gotHeaders.Get("Content-Length"); v != "25" {
				t.Errorf("Content-Length = %q, want preserved 25", v)
			}
		})
	}
}

// TestBodyCapsDecodedOutput is the decompression bound: the proxy's cap is
// on the encoded capture, so without a second cap here a small compressed
// body could expand to an unbounded row.
func TestBodyCapsDecodedOutput(t *testing.T) {
	big := bytes.Repeat([]byte("claude-lens "), 4096)
	h := header("Content-Encoding", "gzip")
	got, _, err := Body(h, encode(t, big, "gzip"), 1024)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if len(got) != 1024 {
		t.Errorf("len(body) = %d, want exactly 1024 (the cap)", len(got))
	}
	if !bytes.Equal(got, big[:1024]) {
		t.Error("capped body is not the decoded prefix")
	}
}

// TestBodyKeepsPartialDecodeOfTruncatedStream is the common real case: the
// proxy caps the encoded capture, which cuts a large compressed stream
// mid-way. The decoded prefix still describes part of the call, so it is
// kept.
func TestBodyKeepsPartialDecodeOfTruncatedStream(t *testing.T) {
	payload := bytes.Repeat([]byte(`{"content":"a longer body "}`), 512)
	whole := encode(t, payload, "gzip")
	truncated := whole[:len(whole)/2]

	got, _, err := Body(header("Content-Encoding", "gzip"), truncated, 1<<20)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("len(body) = 0, want the decoded prefix of a truncated stream")
	}
	if !bytes.Equal(got, payload[:len(got)]) {
		t.Error("partial body is not a prefix of the original")
	}
}

func TestBodyRefusesUnknownCodingAndNonpositiveLimit(t *testing.T) {
	body := []byte("not really compressed")

	_, _, err := Body(header("Content-Encoding", "x-clens-made-up"), body, 1<<20)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("unsupported coding: err = %v, want ErrUnsupported", err)
	}

	if _, _, err := Body(header("Content-Encoding", "gzip"), body, 0); err == nil {
		t.Error("limit 0: err = nil, want an error rather than an unbounded read")
	}
}

func TestBodyDoesNotMutateTheCapturedHeaders(t *testing.T) {
	h := header("Content-Encoding", "gzip", "Content-Type", "text/event-stream")
	if _, _, err := Body(h, encode(t, []byte("data: {}\n\n"), "gzip"), 1<<20); err != nil {
		t.Fatalf("Body: %v", err)
	}
	if v := h.Get("Content-Encoding"); v != "gzip" {
		t.Errorf("captured header mutated: Content-Encoding = %q, want gzip", v)
	}
}

func TestBodyHandlesNilHeaderSet(t *testing.T) {
	body := []byte("plain")
	got, _, err := Body(nil, body, 1<<20)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestBodyReportsCorruptBodyWhenNothingDecodes(t *testing.T) {
	h := header("Content-Encoding", "gzip")
	corrupt := append([]byte{0x1f, 0x8b, 0x08, 0x00, 0, 0, 0, 0, 0, 0}, bytes.Repeat([]byte{0xff}, 64)...)
	if _, _, err := Body(h, corrupt, 1<<20); err == nil {
		t.Error("corrupt body: err = nil, want an error naming the encoding")
	} else if !strings.Contains(err.Error(), "gzip") {
		t.Errorf("corrupt body: err = %v, want it to name the coding", err)
	}
}
