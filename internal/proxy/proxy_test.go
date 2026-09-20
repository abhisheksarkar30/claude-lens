package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
)

func testConfig(upstreamURL string) *config.Config {
	cfg := config.Default()
	cfg.UpstreamURL = upstreamURL
	cfg.BodyPolicy = "full"
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
	proxySrv := httptest.NewServer(h)
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
	proxySrv := httptest.NewServer(h)
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
	proxySrv := httptest.NewServer(h)
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
	proxySrv := httptest.NewServer(h)
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
	proxySrv := httptest.NewServer(h)
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
	// status, timing and headers must all survive the narrowed capture.
	if call.Method != http.MethodPost || call.Path != "/v1/messages" || call.TTFB <= 0 {
		t.Errorf("metadata lost: method=%q path=%q ttfb=%v", call.Method, call.Path, call.TTFB)
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
			proxySrv := httptest.NewServer(h)
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
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	call := captureOne(t, sk)
	if call.RequestID != "req_abc123" {
		t.Errorf("RequestID = %q, want req_abc123", call.RequestID)
	}
}

// TestHashFallbackTwoAttemptsProduceDistinctIDs is the proxy half of test
// 21: a transport failure with no request-id falls back to a synthetic
// key, and two identical bodies on two attempts must not collapse onto
// the same key.
func TestHashFallbackTwoAttemptsProduceDistinctIDs(t *testing.T) {
	sk := sink.New(16)
	h, err := New(testConfig("http://127.0.0.1:1"), sk)
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := httptest.NewServer(h)
	defer proxySrv.Close()

	const body = `{"model":"claude-sonnet-5"}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(proxySrv.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	first := captureOne(t, sk)
	second := captureOne(t, sk)

	if !strings.HasPrefix(first.RequestID, "proxy:") || !strings.HasPrefix(second.RequestID, "proxy:") {
		t.Fatalf("want both fallback IDs prefixed \"proxy:\", got %q and %q", first.RequestID, second.RequestID)
	}
	if first.RequestID == second.RequestID {
		t.Fatalf("two attempts with identical bodies produced the same RequestID %q, want distinct", first.RequestID)
	}
}
