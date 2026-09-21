package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/analyze"
	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// --- fixture helpers ------------------------------------------------------

// strPtr returns a pointer to s, for the nullable EventSummary columns
// (PrefixHash).
func strPtr(s string) *string { return &s }

// ingestProxyCall drives one CapturedCall through a real consumer.Consumer so
// the resulting proxy row's identity comes from the live precedence (the
// response header over the body id, consumer.requestID), not a hand-set key.
// The consumer is cancelled once the call is queued; its shutdown path drains
// and flushes the buffered call, so one row lands synchronously.
func ingestProxyCall(t *testing.T, st *store.Store, call *sink.CapturedCall) {
	t.Helper()
	sk := sink.New(sink.DefaultCapacity)
	cons := consumer.New(sk, st, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cons.Run(ctx) }()
	if !sk.Submit(call) {
		t.Fatal("sink rejected the call")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("consumer.Run: %v", err)
	}
}

func mustHeaderJSON(t *testing.T, h http.Header) string {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal headers: %v", err)
	}
	return string(b)
}

// nonStreamMessageBody is the smallest non-stream body parse.ExtractUsage
// reads a MessageID out of: type "message", the id at the top level (D2).
func nonStreamMessageBody(id string) []byte {
	return []byte(`{"id":"` + id + `","type":"message","model":"m","usage":{"input_tokens":1,"output_tokens":1}}`)
}

func jsonContentType() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

// findEventByRequestID scans every row for one whose request_id matches --
// there is no indexed lookup exposed to the CLI package, and these fixtures
// are always small enough that a scan is simpler than adding one.
func findEventByRequestID(t *testing.T, st *store.Store, requestID string) *store.Event {
	t.Helper()
	evs, err := st.ListEventsFull(context.Background(), store.EventFilter{Limit: 10000})
	if err != nil {
		t.Fatalf("ListEventsFull: %v", err)
	}
	for _, e := range evs {
		if e.RequestID == requestID {
			return e
		}
	}
	return nil
}

func countAllEvents(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.CountEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	return n
}

// seedProxyRow inserts a minimal source="proxy" row and returns its id.
func seedProxyRow(t *testing.T, st *store.Store, ev *store.Event) int64 {
	t.Helper()
	if ev.Source == "" {
		ev.Source = "proxy"
	}
	if ev.FirstSource == "" {
		ev.FirstSource = ev.Source
	}
	if ev.BillingMode == "" {
		ev.BillingMode = "api"
	}
	return seedEvent(t, st, ev)
}

// --- pass 1: proxy body-id re-key -----------------------------------------

func TestRekeyDryRunReportsExactPass1Counts(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "proxy:aaa", StartedAt: time.Now()},
		RespBody:     nonStreamMessageBody("msg_1"), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "proxy:bbb", StartedAt: time.Now()},
		// resp_body IS NULL.
	})
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "proxy:ccc", StartedAt: time.Now()},
		RespBody:     []byte(`{"type":"error","error":{"type":"overloaded_error"}}`), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})

	var buf bytes.Buffer
	if err := runRekey([]string{"--dry-run"}, &buf); err != nil {
		t.Fatalf("runRekey --dry-run: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "would re-key 1, would leave 2 synthetic (no body id)") {
		t.Fatalf("output missing the exact N/M pair: %s", out)
	}

	// Nothing written.
	if ev := findEventByRequestID(t, st, "msg_1"); ev != nil {
		t.Fatalf("dry-run wrote a re-key: found row keyed msg_1")
	}
	if ev := findEventByRequestID(t, st, "proxy:aaa"); ev == nil {
		t.Fatalf("dry-run mutated proxy:aaa's key")
	}
}

func TestRekeyYesRekeysSyntheticProxyRowToBodyID(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "proxy:xyz", StartedAt: time.Now()},
		RespBody:     nonStreamMessageBody("msg_solo"), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})

	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}
	if ev := findEventByRequestID(t, st, "msg_solo"); ev == nil {
		t.Fatal("row was not re-keyed to its body id")
	}
	if ev := findEventByRequestID(t, st, "proxy:xyz"); ev != nil {
		t.Fatal("old synthetic key is still present after re-key")
	}
}

// TestRekeyBodyDecodingObligations pins pass 1's silent-failure modes: a
// compressed body must be decoded with the config's own BodyCapBytes before
// the id is read, exactly as the live consumer path does.
func TestRekeyBodyDecodingObligations(t *testing.T) {
	t.Run("gzip body with Content-Encoding is decoded and re-keyed", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		headers := jsonContentType()
		headers.Set("Content-Encoding", "gzip")
		seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: "proxy:gz1", StartedAt: time.Now()},
			RespBody:     gzipBody(t, nonStreamMessageBody("msg_gz")), RespHeaders: mustHeaderJSON(t, headers),
		})
		var buf bytes.Buffer
		if err := runRekey([]string{"--yes"}, &buf); err != nil {
			t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
		}
		if ev := findEventByRequestID(t, st, "msg_gz"); ev == nil {
			t.Fatal("gzip-compressed body was not decoded and re-keyed")
		}
	})

	t.Run("gzip body with Content-Encoding absent stays synthetic", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: "proxy:gz2", StartedAt: time.Now()},
			// Content-Encoding is missing, so decode.Body has nothing to undo and
			// the raw gzip bytes are handed to ExtractUsage, which cannot parse them.
			RespBody: gzipBody(t, nonStreamMessageBody("msg_gz2")), RespHeaders: mustHeaderJSON(t, jsonContentType()),
		})
		var buf bytes.Buffer
		if err := runRekey([]string{"--yes"}, &buf); err != nil {
			t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
		}
		if ev := findEventByRequestID(t, st, "proxy:gz2"); ev == nil {
			t.Fatal("row without a Content-Encoding header should stay synthetic, but its key changed")
		}
	})

	t.Run("a non-positive limit leaves a compressed body synthetic", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		headers := jsonContentType()
		headers.Set("Content-Encoding", "gzip")
		id := seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: "proxy:gz3", StartedAt: time.Now()},
			RespBody:     gzipBody(t, nonStreamMessageBody("msg_gz3")), RespHeaders: mustHeaderJSON(t, headers),
		})
		// Exercised directly at the store level: decode.Body errors on a
		// non-positive limit for an encoded body, and the err == nil guard then
		// keeps the undecoded bytes -- the "silently re-keys nothing" outcome.
		report, err := st.RekeyProxyBodyIDs(context.Background(), 0, false)
		if err != nil {
			t.Fatalf("RekeyProxyBodyIDs: %v", err)
		}
		if report.ReKeyed != 0 || report.Synthetic != 1 {
			t.Fatalf("report = %+v, want ReKeyed=0 Synthetic=1", report)
		}
		ev, err := st.GetEvent(context.Background(), id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if ev.RequestID != "proxy:gz3" {
			t.Fatalf("RequestID = %q, want unchanged proxy:gz3", ev.RequestID)
		}
	})
}

// TestRekeyHeaderKeyedRowUnchangedByBodyIDMismatch pins the proxy:-prefix
// predicate: a row keyed by a header value is never rewritten even when its
// body yields a different id -- rewriting it would silently split it from
// the JSONL row keyed on the same header value.
func TestRekeyHeaderKeyedRowUnchangedByBodyIDMismatch(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "hdr_value_abc", StartedAt: time.Now()},
		RespBody:     nonStreamMessageBody("msg_different"), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})
	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}
	if ev := findEventByRequestID(t, st, "hdr_value_abc"); ev == nil {
		t.Fatal("header-keyed row was rewritten by the proxy: predicate")
	}
}

// TestRekeyTwoD2CasesAtRekeyLevel pins the two D2 edge cases at the rekey
// level (not only inside internal/parse): first-seen wins across two
// message_start frames, and a non-"message" body carrying a top-level id
// yields nothing.
func TestRekeyTwoD2CasesAtRekeyLevel(t *testing.T) {
	t.Run("two message_start frames, first id wins", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_first\",\"model\":\"m\",\"usage\":{}}}\n\n" +
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_second\",\"model\":\"m\",\"usage\":{}}}\n\n"
		headers := http.Header{"Content-Type": []string{"text/event-stream"}}
		seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: "proxy:two_starts", StartedAt: time.Now()},
			RespBody:     []byte(stream), RespHeaders: mustHeaderJSON(t, headers),
		})
		var buf bytes.Buffer
		if err := runRekey([]string{"--yes"}, &buf); err != nil {
			t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
		}
		if ev := findEventByRequestID(t, st, "msg_first"); ev == nil {
			t.Fatal("did not re-key to the first-seen message_start id")
		}
	})

	t.Run("non-message type with a top-level id yields nothing", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		body := []byte(`{"id":"not_a_message_id","type":"error","model":"m","usage":{}}`)
		seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: "proxy:not_message", StartedAt: time.Now()},
			RespBody:     body, RespHeaders: mustHeaderJSON(t, jsonContentType()),
		})
		var buf bytes.Buffer
		if err := runRekey([]string{"--dry-run"}, &buf); err != nil {
			t.Fatalf("runRekey --dry-run: %v\noutput:\n%s", err, buf.String())
		}
		if !strings.Contains(buf.String(), "would leave 1 synthetic") {
			t.Fatalf("non-message body with a top-level id was not counted as synthetic: %s", buf.String())
		}
	})
}

// TestRekeyProxyVsProxyCollisionUsesObservedSide is the proxy-vs-proxy
// collision shape: two proxy rows sharing a body id collapse to one row, and
// the never-observed-usage override picks the side that actually measured
// something regardless of which row happened to hold the key first.
func TestRekeyProxyVsProxyCollisionUsesObservedSide(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	body := nonStreamMessageBody("msg_dup")
	headers := mustHeaderJSON(t, jsonContentType())

	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "proxy:dup1", StartedAt: time.Now().Add(-time.Hour),
			CaptureComplete: true, InputTokens: 0, OutputTokens: 0,
		},
		RespBody: body, RespHeaders: headers,
	})
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "proxy:dup2", StartedAt: time.Now(),
			CaptureComplete: true, InputTokens: 7, OutputTokens: 4,
		},
		RespBody: body, RespHeaders: headers,
	})

	before := countAllEvents(t, st)
	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}
	if got, want := countAllEvents(t, st), before-1; got != want {
		t.Fatalf("row count = %d, want %d (the shared id must collapse to one row)", got, want)
	}
	survivor := findEventByRequestID(t, st, "msg_dup")
	if survivor == nil {
		t.Fatal("no surviving row keyed msg_dup")
	}
	if survivor.InputTokens != 7 || survivor.OutputTokens != 4 {
		t.Fatalf("survivor tokens = in=%d out=%d, want the observed side's 7/4", survivor.InputTokens, survivor.OutputTokens)
	}
	if want := survivor.InputTokens + survivor.CacheWrite5mTokens + survivor.CacheWrite1hTokens + survivor.CacheReadTokens; survivor.TotalPromptTokens != want {
		t.Fatalf("TotalPromptTokens = %d, want %d (sum of its own four prompt columns)", survivor.TotalPromptTokens, want)
	}
}

// --- pass 1: the JSONL-taker collision, the shape that will actually
// happen on the live database -----------------------------------------

func TestRekeyProxyVsJSONLTakerCollision(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	const (
		mintedSession = "s_minted1"
		convSession   = "conv_session_1"
		targetKey     = "msg_taken"
		prefixHash    = "deadbeef"
	)
	proxyStarted := time.Now().Add(-2 * time.Hour)
	jsonlStarted := time.Now().Add(-1 * time.Hour)

	// The taker: a jsonl-sourced row already holding the target key, in the
	// real conversation session.
	jsonlID := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: targetKey, Source: "jsonl", FirstSource: "jsonl",
			StartedAt: jsonlStarted, SessionID: convSession,
			CaptureComplete: true, InputTokens: 10, OutputTokens: 5,
			BillingMode: "subscription",
		},
	})
	if err := st.UpsertSession(ctx, convSession, "", jsonlStarted); err != nil {
		t.Fatalf("UpsertSession conv: %v", err)
	}
	if err := st.ReconcileSession(ctx, convSession); err != nil {
		t.Fatalf("ReconcileSession conv: %v", err)
	}

	// The incoming: a historical proxy row, still minted, whose body carries
	// the same id. Its billing_mode is deliberately empty with a non-NULL
	// cost_usd, to pin the empty-mode derivation, and its tokens differ from
	// the taker's, to pin both the winner pick and the source_mismatch
	// warning. It carries a non-nil prefix_hash (which the JSONL taker cannot,
	// types.go) and a cache write, so the merge must carry both onto the
	// survivor and the hash-keyed session rules can still evaluate it.
	proxyCost := 1.23
	proxyID := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "proxy:merge1", Source: "proxy", FirstSource: "proxy",
			StartedAt: proxyStarted, SessionID: mintedSession,
			CaptureComplete: true, InputTokens: 99, OutputTokens: 42,
			CacheWrite5mTokens: 7,
			BillingMode:        "", CostUSD: &proxyCost,
			PrefixHash: strPtr(prefixHash),
		},
		RespBody: nonStreamMessageBody(targetKey), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})
	if err := st.UpsertSession(ctx, mintedSession, "", proxyStarted); err != nil {
		t.Fatalf("UpsertSession minted: %v", err)
	}
	if err := st.ReconcileSession(ctx, mintedSession); err != nil {
		t.Fatalf("ReconcileSession minted: %v", err)
	}
	if err := st.UpsertWarnings(ctx, proxyID, []store.Warning{{Kind: "peak_pricing", Severity: "info", Detail: "off-peak window"}}); err != nil {
		t.Fatalf("UpsertWarnings: %v", err)
	}

	// A replay_of naming the row about to be absorbed.
	replayRowID := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "already_final_key", Source: "proxy", FirstSource: "proxy",
			StartedAt: time.Now(), ReplayOf: strconv.FormatInt(proxyID, 10),
		},
	})

	beforeCount := countAllEvents(t, st)
	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}

	// The proxy row is gone; row count drops by exactly one.
	if got, want := countAllEvents(t, st), beforeCount-1; got != want {
		t.Fatalf("row count = %d, want %d", got, want)
	}

	survivor, err := st.GetEvent(ctx, jsonlID)
	if err != nil {
		t.Fatalf("GetEvent(survivor): %v", err)
	}
	if survivor.RequestID != targetKey {
		t.Fatalf("survivor RequestID = %q, want unchanged %q (the taker's own key)", survivor.RequestID, targetKey)
	}
	if survivor.SessionID != convSession {
		t.Fatalf("survivor SessionID = %q, want the JSONL seat's %q", survivor.SessionID, convSession)
	}
	if survivor.Source != "jsonl" || survivor.FirstSource != "jsonl" {
		t.Fatalf("survivor source/first_source = %s/%s, want the JSONL seat's (seat columns must never move)", survivor.Source, survivor.FirstSource)
	}
	if !survivor.StartedAt.Equal(jsonlStarted) {
		t.Fatalf("survivor StartedAt = %v, want the JSONL seat's %v", survivor.StartedAt, jsonlStarted)
	}
	wantRefs := map[string]bool{"jsonl": true, "proxy": true}
	gotRefs := map[string]bool{}
	for _, r := range survivor.SourceRefs {
		gotRefs[r] = true
	}
	if len(gotRefs) != len(wantRefs) || !gotRefs["jsonl"] || !gotRefs["proxy"] {
		t.Fatalf("source_refs = %v, want both jsonl and proxy", survivor.SourceRefs)
	}
	// Both sides were complete captures with differing tokens -- the winner
	// pick takes the incoming (proxy) side (case 1 of mergeEvents).
	if survivor.InputTokens != 99 || survivor.OutputTokens != 42 {
		t.Fatalf("survivor tokens = in=%d out=%d, want the incoming side's 99/42", survivor.InputTokens, survivor.OutputTokens)
	}
	if want := survivor.InputTokens + survivor.CacheWrite5mTokens + survivor.CacheWrite1hTokens + survivor.CacheReadTokens; survivor.TotalPromptTokens != want {
		t.Fatalf("TotalPromptTokens = %d, want %d", survivor.TotalPromptTokens, want)
	}
	// The winner's billing_mode was empty with a non-NULL cost_usd: the
	// derivation must label it api, not leave it empty.
	if survivor.BillingMode != "api" {
		t.Fatalf("survivor BillingMode = %q, want api (derived from the winner's non-NULL cost_usd)", survivor.BillingMode)
	}
	if survivor.CostUSD == nil || *survivor.CostUSD != proxyCost {
		t.Fatalf("survivor CostUSD = %v, want %v", survivor.CostUSD, proxyCost)
	}

	// The incoming proxy row's prefix_hash must be carried onto the JSONL
	// survivor (a JSONL row is nil for the column, types.go). A regression
	// that drops it bytes both hash-keyed session rules, which `continue` on a
	// nil hash -- so pin that the survivor's hash is non-NULL and that
	// ruleCacheExpiredBetweenTurns still groups the survivor under it: an
	// earlier re-write of the same prefix, beyond its TTL, must fire on the
	// survivor (which only happens if the rule sees the carried hash).
	if survivor.PrefixHash == nil || *survivor.PrefixHash != prefixHash {
		t.Fatalf("survivor PrefixHash = %v, want the incoming proxy row's %q", survivor.PrefixHash, prefixHash)
	}
	priorWrite := &store.Event{EventSummary: store.EventSummary{
		ID: 999999, RequestID: "prior_write", StartedAt: jsonlStarted.Add(-10 * time.Minute),
		PrefixHash: strPtr(prefixHash), CacheWrite5mTokens: 1,
	}}
	fired := false
	for _, w := range analyze.AnalyzeSession([]*store.Event{priorWrite, survivor}) {
		if w.Kind == string(analyze.KindCacheExpiredBetweenTurns) && w.EventID == survivor.ID {
			fired = true
		}
	}
	if !fired {
		t.Fatal("ruleCacheExpiredBetweenTurns did not evaluate the survivor's carried prefix_hash")
	}

	// A source_mismatch warning was attached, and the absorbed row's own
	// warning survived the reattach.
	warnings, err := st.EventWarnings(ctx, jsonlID)
	if err != nil {
		t.Fatalf("EventWarnings: %v", err)
	}
	kinds := map[string]bool{}
	for _, w := range warnings {
		kinds[w.Kind] = true
		if w.EventID != jsonlID {
			t.Fatalf("warning %q has event_id %d, want the survivor's %d", w.Kind, w.EventID, jsonlID)
		}
	}
	if !kinds["source_mismatch"] {
		t.Fatalf("no source_mismatch warning attached; warnings = %v", warnings)
	}
	if !kinds["peak_pricing"] {
		t.Fatalf("the absorbed row's own warning did not survive the reattach; warnings = %v", warnings)
	}

	// The replay_of naming the absorbed row now names the survivor.
	replayRow, err := st.GetEvent(ctx, replayRowID)
	if err != nil {
		t.Fatalf("GetEvent(replayRow): %v", err)
	}
	if replayRow.ReplayOf != strconv.FormatInt(jsonlID, 10) {
		t.Fatalf("replay_of = %q, want the survivor's id %d", replayRow.ReplayOf, jsonlID)
	}

	// The survivor's session is reconciled to the merged figures, and the
	// emptied minted session is gone.
	convSess, err := st.GetSession(ctx, convSession)
	if err != nil {
		t.Fatalf("GetSession(conv): %v", err)
	}
	if convSess == nil {
		t.Fatal("conversation session vanished")
	}
	if convSess.RequestCount != 1 || convSess.InputTokens != 99 {
		t.Fatalf("conv session = %+v, want RequestCount=1 InputTokens=99 (re-derived from the surviving row)", convSess)
	}
	if _, err := st.GetSession(ctx, mintedSession); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSession(minted) = %v, want sql.ErrNoRows (the emptied session must be removed)", err)
	}

	// A second run changes nothing further.
	var buf2 bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf2); err != nil {
		t.Fatalf("second runRekey --yes: %v\noutput:\n%s", err, buf2.String())
	}
	survivor2, err := st.GetEvent(ctx, jsonlID)
	if err != nil {
		t.Fatalf("GetEvent(survivor) after second run: %v", err)
	}
	if survivor2.RequestID != targetKey || survivor2.SessionID != convSession {
		t.Fatalf("second run changed the survivor: %+v", survivor2.EventSummary)
	}
	if got, want := countAllEvents(t, st), beforeCount-1; got != want {
		t.Fatalf("second run changed the row count: got %d, want %d", got, want)
	}
}

// TestRekeyAgreeingTokensNoWarningAndSurvivorOnIncomingSeat is the ordinary
// pair: both sides agree on tokens but differ on cost_source/billing_mode,
// which must raise no source_mismatch, and the survivor must end on the
// incoming seat's values (its own request through both paths agrees).
func TestRekeyAgreeingTokensNoWarningAndSurvivorOnIncomingSeat(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	const convSession = "conv_agree"
	jsonlStarted := time.Now().Add(-time.Hour)
	if err := st.UpsertSession(ctx, convSession, "", jsonlStarted); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	jsonlID := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "msg_agree", Source: "jsonl", FirstSource: "jsonl",
			StartedAt: jsonlStarted, SessionID: convSession,
			CaptureComplete: true, InputTokens: 20, OutputTokens: 8,
			BillingMode: "subscription", CostSource: "shipped",
		},
	})
	if err := st.ReconcileSession(ctx, convSession); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	apiCost := 0.05
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "proxy:agree", Source: "proxy", FirstSource: "proxy",
			StartedAt: time.Now(), SessionID: "s_minted_agree",
			CaptureComplete: true, InputTokens: 20, OutputTokens: 8,
			BillingMode: "api", CostSource: "shipped", CostUSD: &apiCost,
		},
		RespBody: nonStreamMessageBody("msg_agree"), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})
	if err := st.UpsertSession(ctx, "s_minted_agree", "", time.Now()); err != nil {
		t.Fatalf("UpsertSession minted: %v", err)
	}
	if err := st.ReconcileSession(ctx, "s_minted_agree"); err != nil {
		t.Fatalf("ReconcileSession minted: %v", err)
	}

	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}

	warnings, err := st.EventWarnings(ctx, jsonlID)
	if err != nil {
		t.Fatalf("EventWarnings: %v", err)
	}
	for _, w := range warnings {
		if w.Kind == "source_mismatch" {
			t.Fatalf("source_mismatch raised on an agreeing pair: %+v", warnings)
		}
	}

	survivor, err := st.GetEvent(ctx, jsonlID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if survivor.BillingMode != "api" || survivor.CostUSD == nil || *survivor.CostUSD != apiCost {
		t.Fatalf("survivor did not end on the incoming (proxy) seat's billing columns: %+v", survivor.EventSummary)
	}

	sess, err := st.GetSession(ctx, convSession)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.TotalApiEquivalentCostUSD != nil {
		t.Fatalf("session TotalApiEquivalentCostUSD = %v, want nil (the row is now billing_mode=api)", sess.TotalApiEquivalentCostUSD)
	}
	if sess.TotalCostUSD == nil || *sess.TotalCostUSD != apiCost {
		t.Fatalf("session TotalCostUSD = %v, want %v", sess.TotalCostUSD, apiCost)
	}
}

// --- pass 2: session re-attribution ----------------------------------------

func TestRekeyPass2ReattributesFromHeaders(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	const convA = "conv_a"
	const convBoth = "conv_both"
	overlong := strings.Repeat("x", 201)

	seedAndMint := func(requestID string, headers http.Header) int64 {
		mintedID := "s_minted_" + requestID
		if err := st.UpsertSession(ctx, mintedID, "", time.Now()); err != nil {
			t.Fatalf("UpsertSession %s: %v", mintedID, err)
		}
		id := seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: requestID, StartedAt: time.Now(), SessionID: mintedID},
			ReqHeaders:   mustHeaderJSON(t, headers),
		})
		if err := st.ReconcileSession(ctx, mintedID); err != nil {
			t.Fatalf("ReconcileSession %s: %v", mintedID, err)
		}
		return id
	}

	id1 := seedAndMint("row1", http.Header{"X-Claude-Code-Session-Id": []string{convA}})
	id2 := seedAndMint("row2", http.Header{"X-Claude-Code-Session-Id": []string{convA}})
	idNoHeader := seedAndMint("row_no_header", http.Header{})
	idOverlong := seedAndMint("row_overlong", http.Header{"X-Claude-Code-Session-Id": []string{overlong}})
	idBoth := seedAndMint("row_both", http.Header{
		"X-Clens-Session":          []string{convBoth},
		"X-Claude-Code-Session-Id": []string{"conv_wrong_precedence"},
	})
	idOverlongClens := seedAndMint("row_overlong_clens", http.Header{
		"X-Clens-Session":          []string{overlong},
		"X-Claude-Code-Session-Id": []string{"conv_fills_in"},
	})

	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "re-attributed 4, left 2 unattributed") {
		t.Fatalf("output missing the exact K/L pair: %s", out)
	}

	check := func(id int64, wantSessionID string) {
		t.Helper()
		ev, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("GetEvent(%d): %v", id, err)
		}
		if ev.SessionID != wantSessionID {
			t.Fatalf("row %d session_id = %q, want %q", id, ev.SessionID, wantSessionID)
		}
	}
	check(id1, convA)
	check(id2, convA)
	check(idBoth, convBoth)                 // x-clens-session wins over x-claude-code-session-id.
	check(idOverlongClens, "conv_fills_in") // overlong x-clens-session falls back to x-claude-code-session-id.

	noHeaderEv, _ := st.GetEvent(ctx, idNoHeader)
	if noHeaderEv.SessionID != "s_minted_row_no_header" {
		t.Fatalf("header-less row was re-attributed: session_id = %q", noHeaderEv.SessionID)
	}
	overlongEv, _ := st.GetEvent(ctx, idOverlong)
	if overlongEv.SessionID != "s_minted_row_overlong" {
		t.Fatalf("overlong header was treated as present: session_id = %q", overlongEv.SessionID)
	}

	convASess, err := st.GetSession(ctx, convA)
	if err != nil {
		t.Fatalf("GetSession(convA): %v", err)
	}
	if convASess == nil || convASess.RequestCount != 2 {
		t.Fatalf("conv_a session = %+v, want RequestCount=2 (upserted, not requiring a prior row)", convASess)
	}
	for _, minted := range []string{"s_minted_row1", "s_minted_row2", "s_minted_row_both", "s_minted_row_overlong_clens"} {
		if _, err := st.GetSession(ctx, minted); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("GetSession(%s) = %v, want sql.ErrNoRows (emptied session must be removed)", minted, err)
		}
	}
}

func TestRekeyPass2DryRunReportsWithoutWriting(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()
	if err := st.UpsertSession(ctx, "s_minted_dr", "", time.Now()); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "row_dr", StartedAt: time.Now(), SessionID: "s_minted_dr"},
		ReqHeaders:   mustHeaderJSON(t, http.Header{"X-Claude-Code-Session-Id": []string{"conv_dr"}}),
	})
	var buf bytes.Buffer
	if err := runRekey([]string{"--dry-run"}, &buf); err != nil {
		t.Fatalf("runRekey --dry-run: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "would re-attribute 1") {
		t.Fatalf("output missing the K count: %s", buf.String())
	}
	ev, _ := st.GetEvent(ctx, id)
	if ev.SessionID != "s_minted_dr" {
		t.Fatalf("dry-run wrote a re-attribution: session_id = %q", ev.SessionID)
	}
}

// --- pass 3: the JSONL half -------------------------------------------------

func seedJSONLKeyedRow(t *testing.T, st *store.Store, requestID, sessionID string) int64 {
	t.Helper()
	return seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: requestID, Source: "jsonl", FirstSource: "jsonl",
			StartedAt: time.Now(), SessionID: sessionID,
		},
	})
}

func TestRekeyJSONLHalfDeletesDuplicatesAndReingests(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, "session1.jsonl")
	lines := "" +
		`{"type":"assistant","sessionId":"s1","uuid":"u1","message":{"id":"msg_dup","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n" +
		`{"type":"assistant","sessionId":"s1","uuid":"u2","message":{"id":"msg_dup","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n" +
		`{"type":"assistant","sessionId":"s1","uuid":"u3","requestId":"req_real","message":{"id":"msg_real","model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":2}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	seedJSONLKeyedRow(t, st, "jsonl:s1:u1", "s1")
	seedJSONLKeyedRow(t, st, "jsonl:s1:u2", "s1")
	seedJSONLKeyedRow(t, st, "req_real", "s1")

	var dryBuf bytes.Buffer
	if err := runRekey([]string{"--dry-run"}, &dryBuf); err != nil {
		t.Fatalf("runRekey --dry-run: %v\noutput:\n%s", err, dryBuf.String())
	}
	if !strings.Contains(dryBuf.String(), "would delete 2 jsonl-keyed row(s)") {
		t.Fatalf("dry-run did not report the jsonl delete count: %s", dryBuf.String())
	}
	if n := countAllEvents(t, st); n != 3 {
		t.Fatalf("dry-run deleted rows: count = %d, want 3", n)
	}

	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}

	if ev := findEventByRequestID(t, st, "jsonl:s1:u1"); ev != nil {
		t.Fatal("jsonl:-prefixed row jsonl:s1:u1 survived the delete")
	}
	if ev := findEventByRequestID(t, st, "jsonl:s1:u2"); ev != nil {
		t.Fatal("jsonl:-prefixed row jsonl:s1:u2 survived the delete")
	}
	dup := findEventByRequestID(t, st, "msg_dup")
	if dup == nil {
		t.Fatal("the duplicate pair did not collapse to one row keyed by message.id")
	}
	if ev := findEventByRequestID(t, st, "req_real"); ev == nil {
		t.Fatal("the row keyed by its real requestId did not survive")
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("transcript file was removed: %v", err)
	}
}

func TestRekeyPass3LeavesSurvivingReplayOfUntouched(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, "session1.jsonl")
	line := `{"type":"assistant","sessionId":"s1","uuid":"u1","requestId":"req_keep","message":{"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	survivorID := seedJSONLKeyedRow(t, st, "req_keep", "s1")

	referrerID := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "proxy:refs_surviving", StartedAt: time.Now(),
			ReplayOf: strconv.FormatInt(survivorID, 10),
		},
	})

	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}
	referrer, err := st.GetEvent(ctx, referrerID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if referrer.ReplayOf != strconv.FormatInt(survivorID, 10) {
		t.Fatalf("replay_of to a surviving row changed: got %q, want unchanged %q", referrer.ReplayOf, strconv.FormatInt(survivorID, 10))
	}

	var dryBuf bytes.Buffer
	// Re-seed a jsonl: row so the dangling count has something to measure --
	// the transcript still yields no dangling entries.
	if err := runRekey([]string{"--dry-run"}, &dryBuf); err != nil {
		t.Fatalf("runRekey --dry-run: %v\noutput:\n%s", err, dryBuf.String())
	}
	if !strings.Contains(dryBuf.String(), "would leave 0 dangling replay_of") {
		t.Fatalf("dry-run's dangling count is not the dangle count: %s", dryBuf.String())
	}
}

func TestRekeyPreconditionRefusesBeforePass1(t *testing.T) {
	t.Run("missing transcript file", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		ctx := context.Background()

		if err := st.SetIngestState(ctx, "jsonl:"+filepath.Join(home, ".claude", "projects", "proj1", "gone.jsonl"),
			store.IngestState{Value: "10", Status: "ok"}); err != nil {
			t.Fatalf("SetIngestState: %v", err)
		}
		id := seedProxyRow(t, st, &store.Event{
			EventSummary: store.EventSummary{RequestID: "proxy:untouched", StartedAt: time.Now()},
			RespBody:     nonStreamMessageBody("msg_would_rekey"), RespHeaders: mustHeaderJSON(t, jsonContentType()),
		})

		var buf bytes.Buffer
		err := runRekey([]string{"--yes"}, &buf)
		if err == nil {
			t.Fatal("runRekey: want a refusal when a recorded transcript is missing")
		}
		ev, gerr := st.GetEvent(ctx, id)
		if gerr != nil {
			t.Fatalf("GetEvent: %v", gerr)
		}
		if ev.RequestID != "proxy:untouched" {
			t.Fatalf("pass 1 ran despite the refused precondition: RequestID = %q", ev.RequestID)
		}
	})

	t.Run("cursor outside the walk root", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		ctx := context.Background()

		outside := filepath.Join(home, "elsewhere", "stray.jsonl")
		if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := st.SetIngestState(ctx, "jsonl:"+outside, store.IngestState{Value: "0", Status: "ok"}); err != nil {
			t.Fatalf("SetIngestState: %v", err)
		}

		var buf bytes.Buffer
		if err := runRekey([]string{"--dry-run"}, &buf); err == nil {
			t.Fatal("runRekey: want a refusal when a cursor's path lies outside the jsonl root")
		}
	})
}

// TestRekeyReingestReprices wires the fixture's tailer with a price table
// (as newTailer does) and asserts the collapsed row's cost_source reflects
// it -- a rekey that built its tailer without the price table would blank
// that column while every other case still passes.
func TestRekeyReingestReprices(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, "session1.jsonl")
	line := `{"type":"assistant","sessionId":"s1","uuid":"u1","message":{"id":"msg_priced","model":"claude-sonnet-5","usage":{"input_tokens":1000,"output_tokens":500}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	seedJSONLKeyedRow(t, st, "jsonl:s1:u1", "s1")

	var buf bytes.Buffer
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
	}
	ev := findEventByRequestID(t, st, "msg_priced")
	if ev == nil {
		t.Fatal("re-ingested row not found")
	}
	if ev.CostSource == "" || ev.CostSource == "unpriced" {
		t.Fatalf("CostSource = %q, want the re-ingest to price the row against the shipped table", ev.CostSource)
	}
}

// TestRekeyReportsWallClock is a reported measurement, not a gate. The test
// reports the run's wall clock beside the re-ingest's row count, and asserts
// the count is reported (wiring that can silently vanish); it asserts no
// bound on the duration -- a wall-clock bound is flaky, and the wiring's
// absence can only make the run faster, never slower, so no bound can fail on
// it. (The wiring's existence is gated by br-GI-9-07's tailer-wiring case.)
func TestRekeyReportsWallClock(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, "session1.jsonl")
	line := `{"type":"assistant","sessionId":"s1","uuid":"u1","message":{"id":"msg_clock","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	seedJSONLKeyedRow(t, st, "jsonl:s1:u1", "s1")

	start := time.Now()
	var buf bytes.Buffer
	if err := runRekey([]string{"--dry-run"}, &buf); err != nil {
		t.Fatalf("runRekey --dry-run: %v\noutput:\n%s", err, buf.String())
	}
	elapsed := time.Since(start)
	t.Logf("rekey --dry-run over 1 jsonl-keyed row took %s", elapsed)
	if !strings.Contains(buf.String(), "would delete 1 jsonl-keyed row(s)") {
		t.Fatalf("the run did not report its re-ingest row count: %s", buf.String())
	}
}

// --- the command's own contract --------------------------------------------

func TestRekeyRefusesWithoutYes(t *testing.T) {
	home := withHome(t)
	openTestStore(t, home)
	var buf bytes.Buffer
	if err := runRekey(nil, &buf); err == nil {
		t.Fatal("runRekey with neither --yes nor --dry-run: want a refusal")
	}
}

func TestRekeyDryRunAndYesTogetherIsADryRun(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: "proxy:both_flags", StartedAt: time.Now()},
		RespBody:     nonStreamMessageBody("msg_both"), RespHeaders: mustHeaderJSON(t, jsonContentType()),
	})
	var buf bytes.Buffer
	if err := runRekey([]string{"--dry-run", "--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --dry-run --yes: %v\noutput:\n%s", err, buf.String())
	}
	if ev := findEventByRequestID(t, st, "msg_both"); ev != nil {
		t.Fatal("--dry-run --yes wrote a re-key; --dry-run must win")
	}
}

// --- the integration case: the forward fix merges one row ------------------

// TestRekeyForwardFixMergesProxyAndJSONLIntoOneRow is the end-to-end
// statement of the whole story: a proxy capture and a JSONL line for the
// same request, keyed the way the live paths key them (not through rekey),
// merge into one row under the real conversation session -- and the
// subagent-sidechain variant takes its parent's session.
func TestRekeyForwardFixMergesProxyAndJSONLIntoOneRow(t *testing.T) {
	t.Run("top-level transcript", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		ctx := context.Background()
		const conv = "conv_integration"

		// The proxy path's own output, post-D2/D7: keyed by the body's
		// message.id, attributed to the real conversation via the stored
		// x-claude-code-session-id header.
		if _, _, err := st.InsertEvent(ctx, &store.Event{EventSummary: store.EventSummary{
			RequestID: "msg_int1", Source: "proxy", FirstSource: "proxy",
			SessionID: conv, StartedAt: time.Now(), BillingMode: "api",
			ModelRequested: "claude-sonnet-5", ModelResolved: "claude-sonnet-5",
			InputTokens: 3, OutputTokens: 1,
		}, ReqHeaders: mustHeaderJSON(t, http.Header{"X-Claude-Code-Session-Id": []string{conv}})}); err != nil {
			t.Fatalf("InsertEvent (proxy): %v", err)
		}

		projectDir := filepath.Join(home, ".claude", "projects", "proj1")
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatal(err)
		}
		transcript := filepath.Join(projectDir, "top.jsonl")
		line := `{"type":"assistant","sessionId":"` + conv + `","uuid":"u1","message":{"id":"msg_int1","model":"claude-sonnet-5","usage":{"input_tokens":3,"output_tokens":1}}}` + "\n"
		if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if err := runIngest(nil, &buf); err != nil {
			t.Fatalf("runIngest: %v\noutput:\n%s", err, buf.String())
		}

		row := findEventByRequestID(t, st, "msg_int1")
		if row == nil {
			t.Fatal("no row keyed msg_int1 after ingest")
		}
		if row.SessionID != conv {
			t.Fatalf("row.SessionID = %q, want %q -- the merge must land in the real conversation, not just collapse to one row", row.SessionID, conv)
		}
	})

	t.Run("subagent sidechain takes the parent session", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		ctx := context.Background()
		const parentConv = "conv_parent"

		if _, _, err := st.InsertEvent(ctx, &store.Event{EventSummary: store.EventSummary{
			RequestID: "msg_side1", Source: "proxy", FirstSource: "proxy",
			SessionID: parentConv, StartedAt: time.Now(), BillingMode: "api",
			ModelRequested: "claude-sonnet-5", ModelResolved: "claude-sonnet-5",
			InputTokens: 2, OutputTokens: 1,
		}, ReqHeaders: mustHeaderJSON(t, http.Header{"X-Claude-Code-Session-Id": []string{parentConv}})}); err != nil {
			t.Fatalf("InsertEvent (proxy): %v", err)
		}

		sideDir := filepath.Join(home, ".claude", "projects", "proj1", parentConv, "subagents")
		if err := os.MkdirAll(sideDir, 0o755); err != nil {
			t.Fatal(err)
		}
		sideFile := filepath.Join(sideDir, "agent-1.jsonl")
		line := `{"type":"assistant","sessionId":"agent_own_id","uuid":"u1","message":{"id":"msg_side1","model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":1}}}` + "\n"
		if err := os.WriteFile(sideFile, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if err := runIngest(nil, &buf); err != nil {
			t.Fatalf("runIngest: %v\noutput:\n%s", err, buf.String())
		}

		row := findEventByRequestID(t, st, "msg_side1")
		if row == nil {
			t.Fatal("no row keyed msg_side1 after ingest")
		}
		if row.SessionID != parentConv {
			t.Fatalf("row.SessionID = %q, want the parent's %q (sidechain attribution)", row.SessionID, parentConv)
		}
	})
}

// TestRekeyAcceptedSplitAndItsMeeting pins D1's named exception: a JSONL
// line keyed by its own requestId (tier 1) and a proxy capture whose
// response carries no request-id header (so it falls to the body id, tier 2)
// never merge -- and the meeting counterpart, where the proxy row's
// response does carry the header, do.
func TestRekeyAcceptedSplitAndItsMeeting(t *testing.T) {
	t.Run("split: tier 1 on one side, tier 2 on the other, stays two rows", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		ctx := context.Background()

		if _, _, err := st.InsertEvent(ctx, &store.Event{EventSummary: store.EventSummary{
			RequestID: "reqhdr_X", Source: "jsonl", FirstSource: "jsonl", StartedAt: time.Now(),
		}}); err != nil {
			t.Fatalf("InsertEvent (jsonl): %v", err)
		}
		if _, _, err := st.InsertEvent(ctx, &store.Event{EventSummary: store.EventSummary{
			// No response request-id header was captured, so the live consumer
			// path's identity precedence falls through to the body's message id.
			RequestID: "msg_shared", Source: "proxy", FirstSource: "proxy", StartedAt: time.Now(),
		}}); err != nil {
			t.Fatalf("InsertEvent (proxy): %v", err)
		}
		if n := countAllEvents(t, st); n != 2 {
			t.Fatalf("row count = %d, want 2 (the accepted split)", n)
		}
	})

	t.Run("meeting: tier 1 on both sides, merges to one row keyed by the header", func(t *testing.T) {
		home := withHome(t)
		st := openTestStore(t, home)
		ctx := context.Background()

		// The JSONL line's requestId is X (tier 1).
		if _, _, err := st.InsertEvent(ctx, &store.Event{EventSummary: store.EventSummary{
			RequestID: "reqhdr_X", Source: "jsonl", FirstSource: "jsonl", StartedAt: time.Now(),
		}}); err != nil {
			t.Fatalf("InsertEvent (jsonl): %v", err)
		}

		// The proxy capture's response carries the request-id header X (tier 1)
		// while its body carries a *different* message id. Drive it through the
		// real consumer identity (consumer.requestID), not a hand-set key, so
		// this fixture fails if the header tier silently stops *meeting* the
		// JSONL row: a broken tier falls through to the body id and leaves two
		// rows keyed X and msg_different_from_header.
		ingestProxyCall(t, st, &sink.CapturedCall{
			RequestIDHeader: "reqhdr_X",
			StartedAt:       time.Now(),
			Duration:        10 * time.Millisecond,
			Method:          "POST",
			Path:            "/v1/messages",
			Status:          200,
			AuthKind:        "api_key",
			ReqHeaders:      http.Header{"Content-Type": {"application/json"}},
			RespHeaders:     http.Header{"Content-Type": {"application/json"}, "Request-Id": {"reqhdr_X"}},
			ReqBody:         []byte(`{"model":"claude-sonnet-5","messages":[]}`),
			RespBody:        nonStreamMessageBody("msg_different_from_header"),
			CaptureComplete: true,
		})

		if n := countAllEvents(t, st); n != 1 {
			t.Fatalf("row count = %d, want 1 (the meeting case: the header tier must meet)", n)
		}
		if ev := findEventByRequestID(t, st, "msg_different_from_header"); ev != nil {
			t.Fatal("the proxy row was keyed by its body id, not the header: the header tier did not win")
		}
		row := findEventByRequestID(t, st, "reqhdr_X")
		if row == nil {
			t.Fatal("no row keyed by the header value reqhdr_X")
		}

		// A later rekey must leave it alone: its request_id has no proxy: prefix.
		var buf bytes.Buffer
		if err := runRekey([]string{"--yes"}, &buf); err != nil {
			t.Fatalf("runRekey --yes: %v\noutput:\n%s", err, buf.String())
		}
		if ev := findEventByRequestID(t, st, "reqhdr_X"); ev == nil {
			t.Fatal("rekey rewrote a header-keyed row")
		}
	})
}
