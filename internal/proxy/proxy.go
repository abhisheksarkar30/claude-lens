// Package proxy is the hot path: a transparent, streaming reverse proxy to
// the configured Anthropic upstream that tees a copy of every request and
// response into the sink for later analysis, without ever buffering the
// stream itself. See CLAUDE.md's "Architecture essentials" for the
// data-flow and latency invariants this package exists to uphold: proxy
// depends only on sink and config, and never on store, analyze, pricing,
// consumer, secret, jsonlogs, snapshot, or adminrep.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
)

// New builds the reverse proxy handler for cfg.UpstreamURL alone, with no
// local provider and no persisted provider choice. See NewWithTargets.
func New(cfg *config.Config, sk *sink.Sink) (http.Handler, error) {
	t, err := NewTargets(cfg, "")
	if err != nil {
		return nil, err
	}
	return NewWithTargets(cfg, sk, t)
}

// NewWithTargets builds the reverse proxy handler for whichever upstream t
// names as active. Every request and response is teed into sk — bodies
// capped at cfg.BodyCapBytes, sensitive headers redacted — without ever
// delaying the client: the hot path copies bytes and does nothing else. Any
// error on the capture side is recorded on the submitted call and the
// response is returned unmodified (invariant 6, fail open).
func NewWithTargets(cfg *config.Config, sk *sink.Sink, t *Targets) (http.Handler, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 100

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The provider was read once, in the handler, and rides on the
			// capture state: the upstream that serves the request and the
			// provider stamped on its row cannot disagree, whatever Set does
			// in between.
			which := t.Active()
			if st, ok := pr.In.Context().Value(stateKey{}).(*captureState); ok {
				which = st.provider
			}
			pr.SetURL(t.resolve(which))
		},
		Transport: transport,
		// The stdlib logs "read error during body copy" here for an upstream
		// body that fails mid-stream. teeCloser records that error and the
		// onClose closures log it with the call's identity, so the stdlib's
		// unlabelled duplicate is discarded.
		ErrorLog: log.New(io.Discard, "", 0),
		// Flush after every write. This is what makes SSE stream through
		// rather than accumulate in a buffer. Non-negotiable — it is the
		// hot-path invariant the TTFB test exists to catch a regression in.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if st, ok := r.Context().Value(stateKey{}).(*captureState); ok {
				st.submit(0, nil, nil, false, err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	bodyCap := cfg.BodyCapBytes

	// "off" narrows what is *kept*, not what is recorded. The row still
	// carries method, path, status, redacted headers and timing; only the two
	// bodies are dropped, which is what --body-policy's own banner promises
	// ("calls are recorded without their bodies"). Returning the bare
	// ReverseProxy here -- which this did until br-GI-7-09 -- installed no
	// captureState, so no row reached the sink at all and an operator reaching
	// for the most private setting silently lost the records too.
	captureBodies := cfg.BodyPolicy != "off"

	rp.ModifyResponse = func(res *http.Response) error {
		st, ok := res.Request.Context().Value(stateKey{}).(*captureState)
		if !ok {
			return nil
		}
		st.ttfb = time.Since(st.start)

		status := res.StatusCode
		respHeaders := redactHeaders(res.Header)
		if !captureBodies {
			// No buffer and no TeeReader: this wrapper copies no bytes, it
			// exists only so onClose knows the call is over. CaptureComplete
			// is true because nothing was narrowed -- an absent body is the
			// policy's doing, not a cap's or a stream's. An upstream abort is
			// neither, so it still clears the flag.
			orig := res.Body
			t := &teeCloser{r: orig, c: orig}
			t.onClose = func() {
				if abort := t.aborted(); abort != nil {
					logAbort(st, 0, abort)
					st.submit(status, respHeaders, nil, false, abort)
					return
				}
				st.submit(status, respHeaders, nil, true, nil)
			}
			res.Body = t
			return nil
		}
		respBuf := newBoundedBuffer(bodyCap)
		orig := res.Body
		t := &teeCloser{r: io.TeeReader(orig, respBuf), c: orig}
		t.onClose = func() {
			// An upstream read error ends the stream early, so whatever was
			// teed is a prefix however small it is: the capture is incomplete
			// whatever the buffers' own flags say, and the error rides on the
			// call so the consumer raises upstream_error.
			if abort := t.aborted(); abort != nil {
				logAbort(st, len(respBuf.Bytes()), abort)
				st.submit(status, respHeaders, respBuf.Bytes(), false, abort)
				return
			}
			// Both buffers, not just the response's. A request body over the
			// cap is stored as a prefix just as a response body is, and
			// reporting only one half leaves the row claiming a complete
			// capture while holding a truncated request -- the exact
			// "truncated looks identical to complete" defect the marker
			// exists to prevent, in the half a response-side fixture never
			// reaches.
			//
			// st.reqBody is non-nil here and needs no guard: this closure
			// runs only on the captureBodies path, where the field is set
			// before the request is sent and the request's body is fully
			// teed by the time the response body closes.
			st.submit(status, respHeaders, respBuf.Bytes(),
				!respBuf.truncated && !st.reqBody.truncated, nil)
		}
		res.Body = t
		return nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authKind := ClassifyAuthKind(r.Header)

		st := &captureState{
			start:      time.Now(),
			method:     r.Method,
			path:       r.URL.Path,
			remoteAddr: r.RemoteAddr,
			authKind:   authKind,
			reqHeaders: redactHeaders(r.Header),
			sk:         sk,
			provider:   t.Active(),
		}
		if captureBodies {
			reqBuf := newBoundedBuffer(bodyCap)
			r.Body = &teeCloser{r: io.TeeReader(r.Body, reqBuf), c: r.Body}
			st.reqBody = reqBuf
		}
		if rm, ok := r.Context().Value(replayKey{}).(ReplayMeta); ok {
			st.noCapture = rm.NoCapture
			if rm.Of != 0 {
				st.replayOf = strconv.FormatInt(rm.Of, 10)
			}
			st.replayEdits = rm.Edits
		}
		r = r.WithContext(context.WithValue(r.Context(), stateKey{}, st))
		rp.ServeHTTP(w, r)
	}), nil
}

// ReplayMeta carries a replay's linkage from the handler that re-issues a
// captured request to the capture path that records it, so a replay
// produces the same kind of row an ordinary proxied call does.
type ReplayMeta struct {
	// Of is the original request's row id.
	Of int64
	// Edits is the serialized [{path, old, new}] array of edits applied to
	// the replayed body, or "" when the replay has no edits.
	Edits string
	// NoCapture is --no-capture: the request is still sent, but nothing
	// reaches the sink, so no row is written at all.
	NoCapture bool
}

type replayKey struct{}

// WithReplay returns r with meta attached for the proxy's capture path to
// pick up, so a replayed request is rewritten, streamed, teed, size-capped
// and redacted by the same handler that serves live traffic.
func WithReplay(r *http.Request, meta ReplayMeta) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), replayKey{}, meta))
}

// NewServer builds the http.Server that fronts NewWithTargets' handler, with
// the timeouts the hot path requires: a bounded ReadHeaderTimeout, no
// WriteTimeout (a streaming response can legitimately run for minutes), and
// an IdleTimeout to reclaim idle keep-alive connections.
func NewServer(cfg *config.Config, sk *sink.Sink, t *Targets) (*http.Server, error) {
	h, err := NewWithTargets(cfg, sk, t)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              cfg.ProxyAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, nil
}

type stateKey struct{}

// captureState carries everything needed to build a sink.CapturedCall for
// one in-flight request, gathered before the request is proxied so that
// neither ModifyResponse nor ErrorHandler need to re-derive it.
type captureState struct {
	start      time.Time
	ttfb       time.Duration
	method     string
	path       string
	remoteAddr string
	authKind   string
	reqHeaders http.Header
	reqBody    *boundedBuffer
	sk         *sink.Sink

	// provider is the Targets.Active value this request was routed by.
	provider string

	noCapture bool

	// replayOf and replayEdits come from ReplayMeta. replayOf is the
	// original's row id rendered in decimal -- the same form
	// Event.ReplayOf is stored in (replay_of is a TEXT column) and the
	// form EventFilter.ReplayOf is matched against. Both stay empty for
	// ordinary traffic, which is what makes "replay_of = ''" mean "not a
	// replay" without a separate flag.
	replayOf    string
	replayEdits string
}

// submit assembles and submits the CapturedCall. It is called at most once
// per request, from whichever of ModifyResponse's response-body Close or
// ErrorHandler fires. It never blocks, so it never delays anything: by the
// time it runs, either the last byte has already reached the client or the
// request has already failed.
func (st *captureState) submit(status int, respHeaders http.Header, respBody []byte, captureComplete bool, callErr error) {
	if st.noCapture {
		return
	}
	// reqBody is nil under --body-policy off, and nil is the point: the column
	// is NULL, which means "no body was kept", never a zero-length body.
	var reqBody []byte
	if st.reqBody != nil {
		reqBody = st.reqBody.Bytes()
	}
	call := &sink.CapturedCall{
		StartedAt:       st.start,
		TTFB:            st.ttfb,
		Duration:        time.Since(st.start),
		Method:          st.method,
		Path:            st.path,
		RemoteAddr:      st.remoteAddr,
		Status:          status,
		AuthKind:        st.authKind,
		ReqHeaders:      st.reqHeaders,
		RespHeaders:     respHeaders,
		ReqBody:         reqBody,
		RespBody:        respBody,
		CaptureComplete: captureComplete,
		ReplayOf:        st.replayOf,
		ReplayEdits:     st.replayEdits,
		RequestIDHeader: requestIDHeader(respHeaders),
		Provider:        st.provider,
		Err:             callErr,
	}
	if !st.sk.Submit(call) {
		log.Printf("proxy: dropped capture id=%s method=%s path=%s auth=%s started=%s duration=%s",
			call.ID, call.Method, call.Path, call.AuthKind,
			call.StartedAt.Format(time.RFC3339Nano), call.Duration)
	}
}

// requestIDHeader returns the upstream Request-Id header value, or "" when
// respHeaders is nil (the ErrorHandler path, which has no response) or the
// header is absent. The proxy resolves no further than this: the identity
// rule that falls back to a parsed body id, and then to a synthetic key,
// lives in the consumer (D2) — the hot path must not parse.
func requestIDHeader(respHeaders http.Header) string {
	if respHeaders == nil {
		return ""
	}
	return respHeaders.Get("Request-Id")
}

// boundedBuffer accumulates up to capacity bytes; writes past that are
// dropped rather than appended, so it caps only what clens stores. Write
// always reports the full length written and never errors, so wrapping it
// in an io.TeeReader never affects the stream being teed.
type boundedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func newBoundedBuffer(capacity int) *boundedBuffer {
	return &boundedBuffer{cap: capacity}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := b.cap - b.buf.Len()
	if room <= 0 {
		if len(p) > 0 {
			b.truncated = true
		}
		return len(p), nil
	}
	if room < len(p) {
		b.truncated = true
		p = p[:room]
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// teeCloser wraps a tee'd reader with the original body's Close, running
// onClose (when set) after that Close returns. For a response body this is
// the fire-and-forget capture submit — it runs only after the copy loop
// that streams to the client has finished, i.e. strictly after the last
// byte reached the client. The hot path never seeks or rewinds this data;
// it is only ever inspected here, in Close, once the stream is done.
type teeCloser struct {
	r       io.Reader
	c       io.Closer
	onClose func()
	readErr error
}

// Read remembers the first read error that is neither a clean EOF nor a
// cancellation. For a response body that error means the upstream dropped
// the stream mid-response. A cancelled context is the client stopping the
// stream, which delivered everything the session produced, so it is not an
// abort. It compares two values and allocates nothing: it is the hot path.
func (t *teeCloser) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.readErr == nil && !errors.Is(err, context.Canceled) {
		t.readErr = err
	}
	return n, err
}

// aborted is the read error Read recorded, or nil for a stream that ended
// cleanly or was cancelled by the client.
func (t *teeCloser) aborted() error { return t.readErr }

func logAbort(st *captureState, captured int, err error) {
	log.Printf("proxy: upstream stream aborted method=%s path=%s captured=%d err=%v",
		st.method, st.path, captured, err)
}

func (t *teeCloser) Close() error {
	err := t.c.Close()
	if t.onClose != nil {
		t.onClose()
	}
	return err
}
