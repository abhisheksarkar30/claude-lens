package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/proxy"
	"github.com/abhisheksarkar30/claude-lens/internal/replay"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// replayWait bounds how long the replay endpoint waits for the consumer to
// commit the row for the call it just sent, and replayPollInterval is how
// often it looks. The wait is inherent to the design: the send goes through
// the sink to the consumer's single writer, and the consumer batches -- so
// this is a generous ceiling for "did the row land yet", not a timeout anyone
// should hit.
const (
	replayWait         = 2 * time.Second
	replayPollInterval = 25 * time.Millisecond
)

// replay is POST /api/requests/{id}/replay: the project's only billable
// route, and one of the dashboard's two write routes. It is deliberately not
// covered by the read-GET dashboard's "no auth on loopback" rationale, and
// instead sits behind the two guards below.
//
// Both guards run before anything is sent, so a rejected request is rejected
// without a single byte reaching the upstream API. That ordering is the whole
// point of the guard, and it is the first thing this function does:
//
//  1. replayEnabled (config.ReplayEnabled, `clens serve --replay`): off by
//     default, and when off this route does nothing at all.
//  2. Origin/Host allowlist (originReject): an accidental or malicious
//     browser page cannot make this endpoint spend money. See that function
//     for what "the dashboard's own origin" means and why a request with no
//     Origin at all passes -- that is the CLI's path, deliberately, not a hole.
//
// On success the stored body is edited, re-sent through the proxy's own
// Handler (so it is teed, capped and recorded by the live path, and reaches
// SQLite through the consumer's single writer), and the row the consumer just
// wrote is returned along with the compact Outcome `clens replay` diffs.
//
// The cost gate is deliberately not here: it is internal/cli's, because the
// CLI is what knows whether the caller passed --yes.
func (a *api) replay(w http.ResponseWriter, r *http.Request) {
	if !a.replayEnabled {
		a.replayRejected.Add(1)
		log.Printf("api: replay rejected from %s: replay is disabled", r.RemoteAddr)
		writeError(w, http.StatusForbidden, "replay is disabled: start `clens serve --replay` to enable it")
		return
	}
	if reason := originReject(r, "replay"); reason != "" {
		a.replayRejected.Add(1)
		log.Printf("api: replay rejected from %s: %s", r.RemoteAddr, reason)
		writeError(w, http.StatusForbidden, reason)
		return
	}
	if a.proxyHandler == nil {
		writeError(w, http.StatusServiceUnavailable, "replay is unavailable: no proxy handler is wired")
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request id")
		return
	}
	orig, err := a.store.GetEvent(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "request not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(orig.ReqBody) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("request %d has no stored body to replay", id))
		return
	}

	edits, err := replay.ParseSets(r.URL.Query()["set"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	edited, err := replay.ApplyEdits(orig.ReqBody, edits)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	editsJSON, err := replay.MarshalEdits(edits)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	noCapture, err := parseBoolParam(r, "no_capture")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Snapshot the newest existing replay of this capture before sending, so
	// the row this request produces can be told apart from one a concurrent
	// replay of the same id may already have written.
	before, err := a.newestReplay(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	status, err := a.sendReplay(r, orig, edited, proxy.ReplayMeta{Of: id, Edits: editsJSON, NoCapture: noCapture})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if noCapture {
		// Sent, deliberately not recorded: there is no row and no id to report.
		writeJSON(w, http.StatusOK, replay.Result{Captured: false, Status: status})
		return
	}

	row, err := a.awaitReplayRow(r.Context(), id, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	warnings, err := a.store.EventWarnings(r.Context(), row.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	outcome := replay.OutcomeOf(row, warnings)
	writeJSON(w, http.StatusOK, replay.Result{ID: row.ID, Captured: true, Status: row.Status, Outcome: &outcome})
}

// sendReplay re-issues orig's stored body through the proxy's own Handler and
// returns the upstream status.
//
// Routing the replay through proxy.Handler rather than through an HTTP client
// of its own is the design: the request picks up the same transport, the same
// streaming rewrite, the same tee, the same body cap and the same header
// redaction as live traffic, and the call it produces reaches SQLite through
// the consumer's single writer. proxy.WithReplay is what carries the
// replay_of/replay_edits linkage across, since the Handler sees only the
// request.
//
// The response is written to a statusRecorder, which records the status and
// discards the body -- the proxy's own tee already captured it, so keeping a
// second copy here would only duplicate a stream that can be large.
//
// Header note: the stored request headers are what the capture saved, which
// means the credential in them is the redaction placeholder, not a key. clens
// neither persists nor injects one, so a replay against an upstream that
// requires the captured credential is answered 401 and that 401 is recorded
// faithfully (invariant 6, fail open).
func (a *api) sendReplay(parent *http.Request, orig *store.Event, body []byte, meta proxy.ReplayMeta) (int, error) {
	req, err := http.NewRequestWithContext(parent.Context(), orig.Method, orig.Path, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("replay: build outbound request: %w", err)
	}
	if orig.ReqHeaders != "" {
		// Copied so a malformed header blob degrades to "no headers" rather
		// than failing the send: replay must still be able to try.
		headers := http.Header{}
		if err := json.Unmarshal([]byte(orig.ReqHeaders), &headers); err == nil {
			req.Header = headers
		}
	}
	rec := &statusRecorder{}
	a.proxyHandler.ServeHTTP(rec, proxy.WithReplay(req, meta))
	return rec.status(), nil
}

// statusRecorder is the http.ResponseWriter a replay's upstream response is
// written to. It records the status code and drops the body (see sendReplay).
// Flush is a no-op that keeps the proxy's FlushInterval: -1 path from
// degrading -- the proxy flushes after every write so an SSE response reaches
// the client immediately, and there is no client here to reach.
type statusRecorder struct {
	code   int
	header http.Header
}

func (s *statusRecorder) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	s.WriteHeader(http.StatusOK)
	return len(p), nil
}

func (s *statusRecorder) Flush() {}

func (s *statusRecorder) status() int {
	if s.code == 0 {
		return http.StatusOK // an empty body still means the upstream answered
	}
	return s.code
}

// newestReplay returns the id of the newest recorded replay of origID, or 0
// when that capture has never been replayed.
func (a *api) newestReplay(ctx context.Context, origID int64) (int64, error) {
	rows, err := a.store.ListEventsFull(ctx, store.EventFilter{ReplayOf: &origID, Limit: 1})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].ID, nil
}

// awaitReplayRow waits for the consumer to commit the row for the replay that
// was just sent and returns it.
//
// Polling is how this endpoint can name that row at all: its id is assigned by
// the writer, asynchronously, so the sender has no other handle on it. The
// retry is what makes that a non-issue rather than a race -- see replayWait.
//
// ponytail: a bounded poll, not a completion hook on the consumer. Giving the
// write path a signalling channel for this one caller would be more machinery
// than a 2s deadline, and the deadline is already ~8x the consumer's own batch
// quiet window. Revisit if the write path ever gains a queue depth that makes
// a flat wait wrong.
func (a *api) awaitReplayRow(ctx context.Context, origID, afterID int64) (*store.Event, error) {
	deadline := time.Now().Add(replayWait)
	for {
		rows, err := a.store.ListEventsFull(ctx, store.EventFilter{ReplayOf: &origID, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 && rows[0].ID > afterID {
			return rows[0], nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("replay was sent but its row did not appear within %s; check `clens ls`", replayWait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(replayPollInterval):
		}
	}
}
