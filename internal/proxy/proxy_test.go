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
func TestNoBufferingSSE(t *testing.T) {
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

	sk := sink.New(16)
	h, err := New(testConfig(upstream.URL), sk)
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
