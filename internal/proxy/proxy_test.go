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
