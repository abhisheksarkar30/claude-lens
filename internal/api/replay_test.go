package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/proxy"
	"github.com/abhisheksarkar30/claude-lens/internal/replay"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// upstreamReply is what the fake upstream answers every /v1/messages call
// with: a small, complete, non-streaming body the consumer can parse.
const upstreamReply = `{"id":"msg_1","type":"message","model":"claude-sonnet-5",` +
	`"content":[{"type":"text","text":"hi"}],` +
	`"usage":{"input_tokens":11,"output_tokens":7}}`

// captureBody is the body the capturing call sends. It has a field to edit
// (max_tokens) and one to leave alone.
const captureBody = `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

// replayFixture is a fully wired slice of the running system: a fake upstream,
// the real proxy ahead of it, the real consumer draining the sink into a real
// store, and the API on top. Nothing here is a stand-in except the upstream,
// which is the point -- the route is exercised through the same transport,
// tee, body cap and single-writer path live traffic uses.
type replayFixture struct {
	handler  *api
	st       *store.Store
	upstream *httptest.Server
}

func newReplayFixture(t *testing.T, replayOn bool) *replayFixture {
	t.Helper()
	// Each call gets its own request-id, as a real upstream does. This is
	// load-bearing: the store dedups on request_id, so a constant one would
	// fold the replay into the row it was replayed from.
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_upstream_"+strconv.FormatInt(calls.Add(1), 10))
		_, _ = w.Write([]byte(upstreamReply))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{UpstreamURL: upstream.URL, BodyPolicy: "full", BodyCapBytes: 262144}
	sk := sink.New(64)
	proxyHandler, err := proxy.New(cfg, sk)
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	st := newTestStore(t)
	cons := consumer.New(sk, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = cons.Run(ctx) }()
	// Cancel and wait before the store closes: the store's own cleanup was
	// registered first, so it runs last, and Run's final flush needs a live DB.
	t.Cleanup(func() { cancel(); <-done })

	return &replayFixture{
		handler:  New(st, sk, cons, NewBroker(), testAssets(), proxyHandler, replayOn),
		st:       st,
		upstream: upstream,
	}
}

// capture posts captureBody through the proxy and returns the row the consumer
// writes for it -- a real capture, which is what a replay needs to exist.
func (f *replayFixture) capture(t *testing.T) *store.Event {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8797/v1/messages", strings.NewReader(captureBody))
	req.Host = "127.0.0.1:8797"
	req.Header.Set("Content-Type", "application/json")
	// A real credential is present and must not survive into the row -- a
	// replay re-sends the stored headers, so a leak here would be re-sent too.
	req.Header.Set("x-api-key", "sk-ant-secret-value")

	rec := httptest.NewRecorder()
	f.handler.proxyHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capturing call: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := f.st.ListEvents(context.Background(), store.EventFilter{Source: "proxy", Limit: 1})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(rows) > 0 {
			return rows[0]
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the capturing call produced no row")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// postReplay sends POST /api/requests/{id}/replay. An empty host or origin
// leaves that header at its recorder default.
func (f *replayFixture) postReplay(t *testing.T, id int64, host, origin, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/requests/"+strconv.FormatInt(id, 10)+"/replay"+query, nil)
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, req)
	return rr
}

// recordedReplay returns the row the endpoint's own send produced: the newest
// row on the table, which after a replay is the replay.
func (f *replayFixture) recordedReplay(t *testing.T, origID int64) *store.Event {
	t.Helper()
	rows, err := f.st.ListEvents(context.Background(), store.EventFilter{ReplayOf: &origID, Limit: 1})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no replay of %d was recorded", origID)
	}
	return rows[0]
}

// The happy path, end to end: a captured call is re-issued, the replay is
// recorded as its own row linked to the original, and the endpoint returns
// both the row id and the compact Outcome the CLI diffs.
func TestReplayReissuesAndRecordsLinkage(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := f.capture(t)

	rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", "?set=max_tokens=250")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[replay.Result](t, rr.Body)
	if !got.Captured || got.ID == 0 {
		t.Fatalf("result = %+v, want a captured replay with a row id", got)
	}
	if got.Outcome == nil {
		t.Fatal("Outcome is nil, want the recorded row summarized")
	}
	if got.Outcome.Model != "claude-sonnet-5" {
		t.Errorf("Outcome.Model = %q, want claude-sonnet-5", got.Outcome.Model)
	}
	if got.Outcome.InputTokens != 11 || got.Outcome.OutputTokens != 7 {
		t.Errorf("Outcome tokens = %d/%d, want 11/7 (the upstream's usage)",
			got.Outcome.InputTokens, got.Outcome.OutputTokens)
	}

	// The linkage is what makes the replay comparable to its original at all.
	row := f.recordedReplay(t, orig.ID)
	if row.ID != got.ID {
		t.Errorf("recorded replay id = %d, returned id = %d", row.ID, got.ID)
	}
	if row.ReplayOf != strconv.FormatInt(orig.ID, 10) {
		t.Errorf("ReplayOf = %q, want %q", row.ReplayOf, strconv.FormatInt(orig.ID, 10))
	}
	if row.ReplayEdits != `[{"path":"max_tokens","old":100,"new":250}]` {
		t.Errorf("ReplayEdits = %q, want the recorded max_tokens edit", row.ReplayEdits)
	}
	// The edit reached the upstream, not just the recorded blob.
	if !strings.Contains(string(row.ReqBody), `"max_tokens":250`) {
		t.Errorf("the recorded request body = %s, want the edited max_tokens", row.ReqBody)
	}
}

// A replay with no edits records an empty replay_edits, so "nothing changed"
// stays distinguishable from "this row is not a replay at all".
func TestReplayWithoutEditsRecordsNoEdits(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := f.capture(t)

	rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	row := f.recordedReplay(t, orig.ID)
	if row.ReplayEdits != "" {
		t.Errorf("ReplayEdits = %q, want empty for an unedited replay", row.ReplayEdits)
	}
	if !strings.Contains(string(row.ReqBody), `"max_tokens":100`) {
		t.Errorf("the replayed body = %s, want the stored bytes unchanged", row.ReqBody)
	}
}

// The dashboard's own origin is the one browser origin allowed through, and an
// absent Origin is the CLI's path -- both send.
func TestReplayOriginAllowlist(t *testing.T) {
	f := newReplayFixture(t, true)

	for _, tc := range []struct {
		name         string
		host, origin string
		wantStatus   int
	}{
		{"dashboard own origin", "127.0.0.1:8798", "http://127.0.0.1:8798", http.StatusOK},
		{"localhost alias", "localhost:8798", "http://localhost:8798", http.StatusOK},
		// No Origin is not a hole: browsers always send one, so its absence
		// means a non-browser client, which is the CLI.
		{"cli, no origin", "127.0.0.1:8798", "", http.StatusOK},
		{"foreign origin", "127.0.0.1:8798", "http://evil.example", http.StatusForbidden},
		// Another loopback origin is still not THIS dashboard's origin.
		{"other loopback origin", "127.0.0.1:8798", "http://127.0.0.1:9999", http.StatusForbidden},
		{"non-loopback host", "evil.example", "http://evil.example", http.StatusForbidden},
		// DNS rebinding: the page's own origin matches its own Host, so the
		// Host check is the one that has to catch it.
		{"rebinding host with matching origin", "evil.example:8798", "http://evil.example:8798", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := f.capture(t)
			rr := f.postReplay(t, orig.ID, tc.host, tc.origin, "")
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			// A rejected replay must not have reached the upstream at all:
			// any row linked to the original would mean it did.
			if tc.wantStatus != http.StatusOK {
				rows, err := f.st.ListEvents(context.Background(), store.EventFilter{ReplayOf: &orig.ID})
				if err != nil {
					t.Fatalf("ListEvents: %v", err)
				}
				if len(rows) != 0 {
					t.Errorf("a rejected replay was still sent and recorded %d time(s)", len(rows))
				}
			}
		})
	}
}

// Off by default is the endpoint's primary control, so it must reject before
// the origin check and before anything is sent.
func TestReplayDisabledIsForbidden(t *testing.T) {
	f := newReplayFixture(t, false)
	orig := f.capture(t)

	// Deliberately a request that would otherwise be allowed, so a 403 here
	// can only come from the disabled gate.
	rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "disabled") {
		t.Errorf("body = %s, want it to say replay is disabled", rr.Body.String())
	}
}

// Every rejection is counted, so a probe against the one billable route leaves
// a server-side trace.
func TestReplayRejectionsAreCounted(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := f.capture(t)

	before := decodeJSON[healthResponse](t, getOK(t, f.handler, "/api/health").Body).ReplayRejected
	f.postReplay(t, orig.ID, "127.0.0.1:8798", "http://evil.example", "")
	f.postReplay(t, orig.ID, "evil.example", "", "")

	after := decodeJSON[healthResponse](t, getOK(t, f.handler, "/api/health").Body).ReplayRejected
	if after != before+2 {
		t.Errorf("replay_rejected went %d -> %d, want +2", before, after)
	}
}

func TestReplayRejectsBadInputBeforeSending(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := f.capture(t)

	for _, tc := range []struct {
		name, query string
		wantStatus  int
	}{
		{"unknown path", "?set=a.b.c=1", http.StatusBadRequest},
		{"malformed set", "?set=max_tokens", http.StatusBadRequest},
		{"index out of range", "?set=messages.9.content=x", http.StatusBadRequest},
		{"bad no_capture", "?no_capture=yes", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", tc.query)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			rows, err := f.st.ListEvents(context.Background(), store.EventFilter{ReplayOf: &orig.ID})
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("a rejected replay was still sent and recorded %d time(s)", len(rows))
			}
		})
	}
}

func TestReplayUnknownIDIs404(t *testing.T) {
	f := newReplayFixture(t, true)
	rr := f.postReplay(t, 999999, "127.0.0.1:8798", "", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

// A capture with no stored body cannot be replayed, and saying so is better
// than sending an empty body upstream.
func TestReplayWithoutStoredBodyIs400(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := seedEvent(t, f.st, func(ev *store.Event) { ev.ReqBody = nil })

	rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

// --no-capture sends the call and records nothing: the response says so
// rather than naming a row that does not exist.
func TestReplayNoCaptureSendsWithoutRecording(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := f.capture(t)

	rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", "?no_capture=true")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	got := decodeJSON[replay.Result](t, rr.Body)
	if got.Captured {
		t.Errorf("Captured = true, want false for a --no-capture replay")
	}
	if got.ID != 0 || got.Outcome != nil {
		t.Errorf("result = %+v, want no id and no outcome when nothing was recorded", got)
	}
	if got.Status != http.StatusOK {
		t.Errorf("Status = %d, want the upstream's 200", got.Status)
	}

	rows, err := f.st.ListEvents(context.Background(), store.EventFilter{ReplayOf: &orig.ID})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a --no-capture replay recorded %d row(s), want none", len(rows))
	}
}

// The stored headers carry the redaction placeholder, never the key: this is
// the credential-containment property checked at the point a replay would
// re-send them.
func TestReplayResendsRedactedHeaders(t *testing.T) {
	f := newReplayFixture(t, true)
	orig := f.capture(t)

	if strings.Contains(orig.ReqHeaders, "sk-ant-secret-value") {
		t.Fatalf("the captured headers leaked the credential: %s", orig.ReqHeaders)
	}

	var reached []string
	f.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Header.Get("x-api-key"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(upstreamReply))
	})

	if rr := f.postReplay(t, orig.ID, "127.0.0.1:8798", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	for _, k := range reached {
		if strings.Contains(k, "sk-ant-secret-value") {
			t.Fatalf("a replay re-sent the live credential: %q", k)
		}
	}
}
