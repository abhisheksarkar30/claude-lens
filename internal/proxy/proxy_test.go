package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
)

// proxyServer starts the handler under test with its panic log captured, and
// fails the test if anything panicked while it served.
//
// This exists because net/http RECOVERS a handler panic: it logs the stack to
// the server's ErrorLog and closes the connection. The process survives, the
// test binary survives, and unless a test happens to assert on the response
// body or reach the sink, the suite stays green while the capture path panics
// on every call and writes no row.
//
// That is not hypothetical. br-GI-7-09 shipped a nil-pointer in requestID that
// did exactly this, and the only reason it was caught is that a reviewer
// instrumented the switch by hand. Both of its failure modes are invisible to
// an ordinary assertion: on the success path the row is simply missing, and on
// the upstream-failure path the panic lands before WriteHeader(502), so the
// client sees an aborted connection rather than a status to check. A recovered
// panic is never an acceptable outcome here -- CLAUDE.md's fail-open invariant
// says a broken observer must not affect the client, which a dropped connection
// plainly does.
//
// Every proxy test goes through this, not just the ones about panics: the guard
// is only worth having if it covers the paths nobody suspected.
func proxyServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()

	srv := httptest.NewUnstartedServer(h)
	var mu sync.Mutex
	var logged bytes.Buffer
	srv.Config.ErrorLog = log.New(&lockedWriter{mu: &mu, w: &logged}, "", 0)
	srv.Start()

	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		// The check runs at cleanup, after the body has been read and the
		// handler has finished, so a panic from any request in the test is in
		// the buffer by now.
		if s := logged.String(); strings.Contains(s, "panic") {
			t.Errorf("the proxy handler panicked while serving; net/http recovered it, so nothing "+
				"else in this test would have failed:\n%s", s)
		}
	})
	return srv
}

// lockedWriter serialises the server's log writes: net/http logs from the
// connection's goroutine, and the test reads the buffer from its own.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func testConfig(upstreamURL string) *config.Config {
	cfg := config.Default()
	cfg.UpstreamURL = upstreamURL
	cfg.BodyPolicy = "full"
	// Pinned rather than left at config.Default(), so these cases stay
	// cap-relative: a test that wants the shipped default must ask for it by
	// name (see TestCaptureCompleteAtTheRealDefaultCap), because otherwise
	// every proxy assertion would silently change meaning the next time the
	// default moves.
	cfg.BodyCapBytes = 262144
	return cfg
}

func captureOne(t *testing.T, sk *sink.Sink) *sink.CapturedCall {
	t.Helper()
	select {
	case call := <-sk.Drain():
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("no call submitted to the sink within 2s")
		return nil
	}
}

// TestNoBufferingSSE is the hard gate: with a fake upstream that holds the
// last SSE event until released, the client must see the first event
// before the last one is written — proof the proxy never buffers the
// stream to inspect it.
//
// Both policies, because they are two different wrappers on the response body
// now: the tee under "full", and a bare pass-through closer under "off". The
// off path is the one br-GI-7-09 added to the hot path, so the gate has to
// cover it or the new wrapper is the only one on the stream without proof.
func TestNoBufferingSSE(t *testing.T) {
	for _, policy := range []string{"full", "off"} {
		t.Run(policy, func(t *testing.T) { testNoBufferingSSE(t, policy) })
	}
}

func testNoBufferingSSE(t *testing.T, policy string) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: first\ndata: {}\n\n")
		fl.Flush()
		<-release
		fmt.Fprint(w, "event: last\ndata: {}\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.BodyPolicy = policy
	sk := sink.New(16)
	h, err := New(cfg, sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	firstDone := make(chan string, 1)
	go func() {
		var sb strings.Builder
		for i := 0; i < 3; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			sb.WriteString(line)
		}
		firstDone <- sb.String()
	}()

	select {
	case got := <-firstDone:
		if !strings.Contains(got, "first") {
			t.Fatalf("first frame = %q, want it to contain %q", got, "first")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive the first SSE event before timeout — response appears to be buffered")
	}

	close(release)
	_, _ = io.ReadAll(reader)
}

func TestByteIdentityNonStreaming(t *testing.T) {
	const reqBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	const respBody = `{"id":"msg_1","usage":{"input_tokens":5,"output_tokens":2}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if string(got) != reqBody {
			t.Errorf("upstream received %q, want %q", got, reqBody)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respBody))
	}))
	defer upstream.Close()

	sk := sink.New(16)
	h, err := New(testConfig(upstream.URL), sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != respBody {
		t.Fatalf("client received %q, want %q", got, respBody)
	}

	call := captureOne(t, sk)
	if string(call.ReqBody) != reqBody {
		t.Errorf("captured ReqBody = %q, want %q", call.ReqBody, reqBody)
	}
	if string(call.RespBody) != respBody {
		t.Errorf("captured RespBody = %q, want %q", call.RespBody, respBody)
	}
	if !call.CaptureComplete {
		t.Error("CaptureComplete = false, want true (body under cap)")
	}
}

func TestFailOpenOnUpstreamFailure(t *testing.T) {
	// Nothing listens here — every dial fails.
	sk := sink.New(16)
	h, err := New(testConfig("http://127.0.0.1:1"), sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("client-side request failed (want a 502 response, not a transport error): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}

	call := captureOne(t, sk)
	if call.Err == nil {
		t.Error("CapturedCall.Err = nil, want the transport failure recorded")
	}
}

// TestCaptureCompleteAtTheRealDefaultCap is RC-C's end-to-end form: a request
// body far past the old cap is captured whole under the shipped default, and
// the row says so.
//
// Built from config.Default() rather than testConfig(), which pins its own cap
// -- the case is about the value a real install runs with, so overriding it,
// even to the same number, would make the test agree with itself instead of
// with the default. RC-C matters because truncation was the norm on the install
// this story came from: 58% of request bodies exceeded 256 KB and the dominant
// body is the request, not the response. A revert here shows up as the majority
// of newly captured calls being flagged incomplete.
func TestCaptureCompleteAtTheRealDefaultCap(t *testing.T) {
	// The largest request body the live store holds (measured 2026-09-22T09:06Z),
	// so the fixture is a real shape rather than a round number.
	const bodySize = 1_246_222

	// The upstream must read the whole request body before answering, as a real
	// one does. One that answers immediately makes the reverse proxy stop
	// relaying the body, so the tee captures a prefix of it -- and because the
	// cut is not at the cap, bufferTruncated stays false and the row reports
	// itself complete. The fixture would then be testing the client's pipelining,
	// not the cap.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{"type":"message","usage":{}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.UpstreamURL = upstream.URL
	cfg.BodyPolicy = "full"
	if cfg.BodyCapBytes != 2_097_152 {
		t.Fatalf("the shipped default cap = %d, want 2097152: this case is about the real default", cfg.BodyCapBytes)
	}

	reqBody := strings.Repeat("x", bodySize)
	sk := sink.New(16)
	h, err := New(cfg, sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	call := captureOne(t, sk)
	if len(call.ReqBody) != bodySize {
		t.Errorf("captured ReqBody length = %d, want the whole %d-byte body: a request this size must fit under the default cap",
			len(call.ReqBody), bodySize)
	}
	if !call.CaptureComplete {
		t.Error("CaptureComplete = false at the real default, want true: nothing was cut")
	}
}

func TestBodyCapTruncation(t *testing.T) {
	const bodyCap = 8
	bigBody := strings.Repeat("x", 100)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(bigBody))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.BodyCapBytes = bodyCap
	sk := sink.New(16)
	h, err := New(cfg, sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != bigBody {
		t.Fatalf("client received %d bytes, want the full %d-byte body unaffected by the storage cap", len(got), len(bigBody))
	}

	call := captureOne(t, sk)
	if len(call.RespBody) != bodyCap {
		t.Errorf("captured RespBody length = %d, want %d (the cap)", len(call.RespBody), bodyCap)
	}
	if call.CaptureComplete {
		t.Error("CaptureComplete = true, want false (body exceeded the cap)")
	}
}

// TestCaptureCompleteCoversBothBodies (br-GI-7-08) is the guard for the
// defect the GI#7 manual run found: the flag was submitted as
// !respBuf.truncated, the response buffer's alone, so a request body over the
// cap produced a row reporting a complete capture while holding a prefix of
// the request. Every other fixture in that story truncates a response, which
// is why none of them could reach it.
//
// TestCaptureRecordsUnderPolicyOff is the assertion that fails at the *sink*
// under the defect it was written for, not inside a handler: `New` used to
// return the bare ReverseProxy when BodyPolicy was "off", before the closure
// that injects captureState existed, so no row reached the sink at all and
// nothing downstream could notice -- the row was simply absent.
//
// The banner at cli/serve.go promises "calls are recorded without their
// bodies". This is that promise, in the only place it can be checked: a row
// must arrive, must carry the metadata, and must carry no bodies.
func TestCaptureRecordsUnderPolicyOff(t *testing.T) {
	const reqBody = `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	const respBody = `{"id":"msg_1","usage":{"input_tokens":5,"output_tokens":2}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Id", "req_off_policy")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respBody))
	}))
	defer upstream.Close()

	cfg := testConfig(upstream.URL)
	cfg.BodyPolicy = "off"
	sk := sink.New(16)
	h, err := New(cfg, sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
		strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ant-secret-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The pass-through wrapper must not alter what the client sees: it exists
	// only so onClose can fire, and this is the assertion that fails if it is
	// ever given a buffer.
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != respBody {
		t.Fatalf("client received %q under policy off, want %q", got, respBody)
	}

	// captureOne fails the test on the 2s timeout, which is exactly the
	// off-policy failure mode: no row, ever.
	call := captureOne(t, sk)

	if call.ReqBody != nil || call.RespBody != nil {
		t.Errorf("bodies captured under policy off: ReqBody=%q RespBody=%q, want both nil",
			call.ReqBody, call.RespBody)
	}
	if call.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", call.Status)
	}
	// True because nothing was *narrowed*: an absent body is the policy's
	// doing, not a cap's or a stream's. False here would fire
	// stream_incomplete on every 200 and cost every off-policy row the merge
	// token pick, neither of which has anything to do with a body.
	if !call.CaptureComplete {
		t.Error("CaptureComplete = false under policy off, want true (nothing was truncated)")
	}
	// The metadata half is the whole point of keeping the row: method, path,
	// status and headers must all survive the narrowed capture.
	//
	// TTFB is deliberately not asserted positive. `time.Since` on a loopback
	// round trip under Windows' coarse timer can legitimately measure 0, and a
	// Duration has no presence bit, so "0" is not evidence the field was lost —
	// asserting it made this test fail roughly one run in three at -count=10.
	// What is checkable is that the row was submitted at all, which captureOne
	// already enforces by failing on the sink timeout.
	if call.Method != http.MethodPost || call.Path != "/v1/messages" {
		t.Errorf("metadata lost: method=%q path=%q", call.Method, call.Path)
	}
	if call.ReqHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("req headers lost under policy off: %v", call.ReqHeaders)
	}
	if call.RespHeaders.Get("Request-Id") != "req_off_policy" {
		t.Errorf("resp headers lost under policy off: %v", call.RespHeaders)
	}
	// Redaction is unconditional, and the off path is the one an operator
	// chose *because* they care about what is stored: a route around it here
	// would leak the credential the policy was set to protect.
	if got := call.ReqHeaders.Get("Authorization"); strings.Contains(got, "sk-ant-secret-value") {
		t.Errorf("Authorization not redacted under policy off: %q", got)
	}
}

// TestPolicyOffSurvivesAMissingRequestID is the regression the first version of
// br-GI-7-09 shipped. The proxy used to hash st.reqBody to build a fallback
// key, and under "off" that field is nil -- so any call whose response
// carried no Request-Id panicked on a nil *boundedBuffer. net/http recovers a
// handler panic and logs it, so the suite stayed green while the row was
// silently never submitted. D2 moved key resolution (and the hash) to the
// consumer, so the proxy no longer has a nil-buffer path to guard here --
// this test now only pins the surrounding no-panic / no-lost-row behaviour.
//
// Both paths are here because they reach the same line and only one of them
// looks like an error: a 200 with no Request-Id, and an unreachable upstream,
// where respHeaders is nil so the early return above cannot help and the panic
// lands *before* ErrorHandler's WriteHeader(502) -- turning a failed upstream
// into an aborted request and breaking fail-open for the client.
func TestPolicyOffSurvivesAMissingRequestID(t *testing.T) {
	tests := []struct {
		name       string
		upstream   func(t *testing.T) string
		wantStatus int
		wantRow    bool
	}{
		{
			name: "no Request-Id header on a healthy response",
			upstream: func(t *testing.T) string {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Deliberately no Request-Id: requestID must fall back.
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{}`))
				}))
				t.Cleanup(srv.Close)
				return srv.URL
			},
			wantStatus: http.StatusOK,
			wantRow:    true,
		},
		{
			name: "unreachable upstream, so respHeaders is nil",
			upstream: func(t *testing.T) string {
				// Nothing listens here -- every dial fails.
				return "http://127.0.0.1:1"
			},
			wantStatus: http.StatusBadGateway,
			wantRow:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(tt.upstream(t))
			cfg.BodyPolicy = "off"
			sk := sink.New(16)
			h, err := New(cfg, sk)
			if err != nil {
				t.Fatal(err)
			}
			front := proxyServer(t, h)
			defer front.Close()

			resp, err := http.Get(front.URL + "/v1/messages")
			if err != nil {
				// The fail-open half: a panic inside submit aborts the
				// connection before the error response is written, so the
				// client sees this instead of a 502.
				t.Fatalf("client got an aborted request instead of a response: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			call := captureOne(t, sk)
			if call.Method != http.MethodGet {
				t.Errorf("Method = %q, want GET", call.Method)
			}
		})
	}
}

// All the cases are one table on purpose. The regression this guards is as
// much "the response half stopped being checked" as "the request half is not",
// and a request-only test would let the first through.
func TestCaptureCompleteCoversBothBodies(t *testing.T) {
	const bodyCap = 64

	var respLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the request first: the tee only sees what upstream reads, so a
		// handler that ignored the body would leave the buffer short and pass
		// the over-cap case for the wrong reason.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("draining the request body: %v", err)
		}
		w.Write([]byte(strings.Repeat("y", respLen)))
	}))
	defer upstream.Close()

	cases := []struct {
		name       string
		reqLen     int
		respLen    int
		wantWhole  bool
		wantReqCut bool
		wantRespCt bool
	}{
		{"request over the cap", 200, 8, false, true, false},
		{"request exactly at the cap", bodyCap, 8, true, false, false},
		{"request under the cap", 10, 8, true, false, false},
		{"response over the cap", 10, 200, false, false, true},
		{"both over the cap", 200, 200, false, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			respLen = tc.respLen
			cfg := testConfig(upstream.URL)
			cfg.BodyCapBytes = bodyCap
			sk := sink.New(16)
			h, err := New(cfg, sk)
			if err != nil {
				t.Fatal(err)
			}
			proxySrv := proxyServer(t, h)
			defer proxySrv.Close()

			reqBody := strings.Repeat("x", tc.reqLen)
			resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json",
				strings.NewReader(reqBody))
			if err != nil {
				t.Fatal(err)
			}
			// The client's view is unaffected either way: the cap bounds what
			// clens stores, never what it forwards.
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(got) != tc.respLen {
				t.Fatalf("client received %d bytes, want the full %d", len(got), tc.respLen)
			}

			call := captureOne(t, sk)
			if call.CaptureComplete != tc.wantWhole {
				t.Errorf("CaptureComplete = %v, want %v", call.CaptureComplete, tc.wantWhole)
			}
			wantReq := tc.reqLen
			if wantReq > bodyCap {
				wantReq = bodyCap
			}
			if len(call.ReqBody) != wantReq {
				t.Errorf("ReqBody = %d bytes, want %d", len(call.ReqBody), wantReq)
			}
			if tc.wantReqCut && len(call.ReqBody) != bodyCap {
				t.Errorf("a request reported as cut is not at the cap: %d", len(call.ReqBody))
			}
			wantResp := tc.respLen
			if wantResp > bodyCap {
				wantResp = bodyCap
			}
			if len(call.RespBody) != wantResp {
				t.Errorf("RespBody = %d bytes, want %d", len(call.RespBody), wantResp)
			}
			if tc.wantRespCt && len(call.RespBody) != bodyCap {
				t.Errorf("a response reported as cut is not at the cap: %d", len(call.RespBody))
			}
		})
	}
}

// TestResponseDerivedRequestIDWins asserts the raw upstream header value
// passes through the proxy unresolved. It is no longer the dedup key
// itself (D2 moved key resolution to the consumer, which is the only
// place a parsed body is available) — this test only pins that the proxy
// still tees the header value onto the sink.CapturedCall it submits.
func TestResponseDerivedRequestIDWins(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Id", "req_abc123")
		w.Write([]byte("{}"))
	}))
	defer upstream.Close()

	sk := sink.New(16)
	h, err := New(testConfig(upstream.URL), sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	call := captureOne(t, sk)
	if call.RequestIDHeader != "req_abc123" {
		t.Errorf("RequestIDHeader = %q, want req_abc123", call.RequestIDHeader)
	}
}

// waitForSinkStat polls sk.Stats() until want reports satisfied, or fails the
// test after 2s. Polling rather than draining: draining a capacity-1 sink to
// confirm the first call landed would empty the very slot the test needs full
// for the second call's drop.
func waitForSinkStat(t *testing.T, sk *sink.Sink, want func(accepted, dropped uint64) bool) (accepted, dropped uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		accepted, dropped = sk.Stats()
		if want(accepted, dropped) {
			return accepted, dropped
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink stats did not reach the wanted state within 2s: accepted=%d dropped=%d", accepted, dropped)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSubmitLogsADropWithoutTheBody is F3.1/F4.1's regression test: a dropped
// capture must be logged (closing the fail-open gap this bead exists for),
// and the log line must never carry request/response body or header content
// -- only the negative-containment assertion below actually enforces that; a
// lazy log.Printf("dropped: %+v", call) struct-dump would pass every
// positive assertion here and still leak the body.
func TestSubmitLogsADropWithoutTheBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.Write([]byte("{}"))
	}))
	defer upstream.Close()

	sk := sink.New(1)
	h, err := New(testConfig(upstream.URL), sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := proxyServer(t, h)
	defer proxySrv.Close()

	var mu sync.Mutex
	var logged bytes.Buffer
	prevOutput := log.Writer()
	log.SetOutput(&lockedWriter{mu: &mu, w: &logged})
	t.Cleanup(func() { log.SetOutput(prevOutput) })

	// First request fills the capacity-1 sink. Wait for the sink to actually
	// record it before firing the second -- submit() runs from the response
	// body's Close hook, on the server's own goroutine, so nothing guarantees
	// it has run yet just because the client has read the response.
	resp1, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp1.Body)
	resp1.Body.Close()
	waitForSinkStat(t, sk, func(accepted, _ uint64) bool { return accepted >= 1 })

	// Not an Authorization-header sentinel: redactHeaders replaces that value
	// with "[redacted]" before captureState is even constructed (proxy.go's
	// request handler), so a header-based sentinel would pass this check
	// unconditionally regardless of what the drop log actually does. Only a
	// request-body sentinel exercises the leak this test guards against.
	const sentinel = "sentinel-9f3a7c2e-do-not-leak"
	resp2, err := http.Post(proxySrv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"note":"`+sentinel+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp2.Body)
	resp2.Body.Close()

	_, dropped := waitForSinkStat(t, sk, func(_, dropped uint64) bool { return dropped >= 1 })
	if dropped != 1 {
		t.Fatalf("sk.Stats() dropped = %d, want 1", dropped)
	}

	// The log.Printf call is the statement right after Submit() returns false,
	// on the same goroutine -- but that goroutine is the server's, not this
	// test's, so observing sk.Stats()'s dropped counter above does not by
	// itself guarantee the log write that follows it has landed yet. Poll for
	// it rather than reading the buffer once.
	var logOutput string
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		logOutput = logged.String()
		mu.Unlock()
		if logOutput != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if logOutput == "" {
		t.Fatal("drop log is empty, want a line reporting the dropped capture")
	}
	if !strings.Contains(logOutput, "/v1/messages") {
		t.Errorf("drop log = %q, want it to mention the request path", logOutput)
	}
	if strings.Contains(logOutput, sentinel) {
		t.Errorf("drop log leaked the request body: %q", logOutput)
	}
}
