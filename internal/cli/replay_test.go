package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/replay"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// countingServer stands in for `clens serve` and counts what reaches it, so a
// test can assert on the only question that matters for a command whose whole
// risk is spending money: did anything leave the process.
type countingServer struct {
	*httptest.Server
	hits    atomic.Int64
	lastURL atomic.Value // string
	// respond overrides the answer; the default is a successful replay Result.
	respond func(w http.ResponseWriter)
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.hits.Add(1)
		cs.lastURL.Store(r.URL.String())
		if cs.respond != nil {
			cs.respond(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(replay.Result{ID: 99, Captured: true, Status: 200, Outcome: &replay.Outcome{
			ID: 99, Status: 200, Model: "claude-sonnet-5", InputTokens: 12, OutputTokens: 3,
		}})
	}))
	t.Cleanup(cs.Close)
	return cs
}

// queryOf is the URL the server last saw. Renamed off `url` only because that
// identifier is an import name in replay.go.
func queryOf(t *testing.T, cs *countingServer) string {
	t.Helper()
	v, _ := cs.lastURL.Load().(string)
	return v
}

// cheapRow is a replayable row: a body to send and a cost under the gate's
// threshold, so the plain path is allowed through.
func cheapRow(requestID, body string) *store.Event {
	ev := apiRow(requestID, 0.01)
	ev.ReqBody = []byte(body)
	ev.RespBody = []byte(`{"ok":true}`)
	return ev
}

// TestReplayDumpSendsNothing is the bead's "--dump sends no request" clause:
// the body is printed and the endpoint is never called, even though --addr
// points at a server that would answer.
func TestReplayDumpSendsNothing(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, cheapRow("req_one", `{"model":"claude-sonnet-5","max_tokens":8}`))

	cs := newCountingServer(t)
	var buf bytes.Buffer
	if err := runReplay([]string{itoa(id), "--dump", "--addr", cs.URL}, &buf); err != nil {
		t.Fatalf("runReplay --dump: %v\noutput:\n%s", err, buf.String())
	}
	if n := cs.hits.Load(); n != 0 {
		t.Fatalf("--dump sent %d request(s); it must send none", n)
	}
	out := buf.String()
	if !strings.Contains(out, `{"model":"claude-sonnet-5","max_tokens":8}`) {
		t.Fatalf("--dump did not print the body it would send:\n%s", out)
	}
	if !strings.Contains(out, "edits   none") {
		t.Fatalf("--dump did not say there are no edits:\n%s", out)
	}
}

// TestReplaySetEditsTheBodyThroughApplyEdits is the bead's --set clause. The
// expected body is computed by replay.ApplyEdits rather than written out here,
// so this cannot pass by agreeing with a bug in the CLI's own copy of the logic.
func TestReplaySetEditsTheBodyThroughApplyEdits(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	// Both edited keys already exist: a --set walk never creates structure, so
	// a body without them would fail for a reason this test is not about.
	const original = `{"model":"claude-sonnet-5","max_tokens":8,"temperature":0.1}`
	id := seedEvent(t, st, cheapRow("req_one", original))

	sets := []string{"max_tokens=64", "temperature=0.5"}
	var buf bytes.Buffer
	args := append([]string{itoa(id), "--dump"}, prefixEach(sets, "--set")...)
	if err := runReplay(args, &buf); err != nil {
		t.Fatalf("runReplay --set: %v\noutput:\n%s", err, buf.String())
	}

	want, err := replay.ApplyEdits([]byte(original), mustParseSets(t, sets))
	if err != nil {
		t.Fatalf("ApplyEdits (reference): %v", err)
	}
	if !strings.Contains(buf.String(), string(want)) {
		t.Fatalf("--dump did not print the edited body.\nwant it to contain:\n%s\ngot:\n%s", want, buf.String())
	}
	if strings.Contains(buf.String(), original) {
		t.Fatalf("--dump printed the unedited body:\n%s", buf.String())
	}
}

func prefixEach(items []string, prefix string) []string {
	out := make([]string, 0, len(items)*2)
	for _, it := range items {
		out = append(out, prefix, it)
	}
	return out
}

func mustParseSets(t *testing.T, sets []string) []replay.Edit {
	t.Helper()
	edits, err := replay.ParseSets(sets)
	if err != nil {
		t.Fatalf("ParseSets: %v", err)
	}
	return edits
}

// TestReplayMalformedSetFailsBeforeAnythingHappens is the bead's "a malformed
// --set fails before anything is sent" clause. The second half uses an id that
// was never seeded, so the error coming back says "parse", not "not found" --
// which is what proves the ordering rather than just the outcome.
func TestReplayMalformedSetFailsBeforeAnythingHappens(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, cheapRow("req_one", `{"model":"claude-sonnet-5"}`))

	cs := newCountingServer(t)
	var buf bytes.Buffer
	err := runReplay([]string{itoa(id), "--set", "no-equals-sign", "--addr", cs.URL}, &buf)
	if err == nil {
		t.Fatal("runReplay with a malformed --set: want an error")
	}
	if !strings.Contains(err.Error(), "want <jsonpath>=<value>") {
		t.Fatalf("error does not name the parse problem: %v", err)
	}
	if n := cs.hits.Load(); n != 0 {
		t.Fatalf("a malformed --set still sent %d request(s)", n)
	}

	err = runReplay([]string{itoa(id + 9999), "--set", "no-equals-sign", "--addr", cs.URL}, &buf)
	if err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("a malformed --set reached the store read first: %v", err)
	}
}

// TestReplayRefusesACostlyOriginalWithoutYes is the CLI half of the replay
// footgun, and the refusal is what stops the spend: nothing is posted.
func TestReplayRefusesACostlyOriginalWithoutYes(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	expensive := cheapRow("req_pricey", `{"model":"claude-opus-5"}`)
	cost := 4.0
	expensive.CostUSD = &cost
	id := seedEvent(t, st, expensive)

	cs := newCountingServer(t)
	var buf bytes.Buffer
	err := runReplay([]string{itoa(id), "--addr", cs.URL}, &buf)
	if err == nil {
		t.Fatal("runReplay on a $4.00 original without --yes: want a refusal")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("refusal does not name --yes: %v", err)
	}
	if !strings.Contains(err.Error(), "--dump") {
		t.Fatalf("refusal does not offer the non-spending alternative: %v", err)
	}
	if n := cs.hits.Load(); n != 0 {
		t.Fatalf("a refused replay sent %d request(s)", n)
	}
}

// TestReplayRefusesAnUnpricedOriginalWithoutYes: with neither cost column set
// there is nothing to bound the replay with, so it is gated for the same reason
// an expensive one is -- and --yes is the override that lets it through.
func TestReplayRefusesAnUnpricedOriginalWithoutYes(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	unpriced := cheapRow("req_unpriced", `{"model":"claude-unknown"}`)
	unpriced.CostUSD = nil
	unpriced.CostSource = "unpriced"
	id := seedEvent(t, st, unpriced)

	cs := newCountingServer(t)
	var buf bytes.Buffer
	err := runReplay([]string{itoa(id), "--addr", cs.URL}, &buf)
	if err == nil || !strings.Contains(err.Error(), "unpriced") {
		t.Fatalf("runReplay on an unpriced original: got %v, want a refusal naming it", err)
	}
	if n := cs.hits.Load(); n != 0 {
		t.Fatalf("a refused replay sent %d request(s)", n)
	}

	buf.Reset()
	if err := runReplay([]string{itoa(id), "--yes", "--addr", cs.URL}, &buf); err != nil {
		t.Fatalf("runReplay --yes: %v\noutput:\n%s", err, buf.String())
	}
	if n := cs.hits.Load(); n != 1 {
		t.Fatalf("--yes produced %d request(s), want 1", n)
	}
}

// TestReplaySendsEditsAndNoCaptureAsQueryParams pins the wire format the CLI
// and the endpoint have to agree on. The CLI validates --set with the same
// parser the server uses, so a drift here would turn a working edit into a 400
// on the other side -- the failure would be remote and confusing.
func TestReplaySendsEditsAndNoCaptureAsQueryParams(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, cheapRow("req_one", `{"model":"claude-sonnet-5","max_tokens":8}`))

	cs := newCountingServer(t)
	var buf bytes.Buffer
	if err := runReplay([]string{itoa(id), "--set", "max_tokens=64", "--no-capture", "--addr", cs.URL}, &buf); err != nil {
		t.Fatalf("runReplay: %v\noutput:\n%s", err, buf.String())
	}
	got := queryOf(t, cs)
	if !strings.Contains(got, "/api/requests/"+itoa(id)+"/replay") {
		t.Fatalf("POST went to %q, want the replay endpoint for request %d", got, id)
	}
	if !strings.Contains(got, "set=max_tokens%3D64") {
		t.Fatalf("the --set edit did not travel as a set= query param: %q", got)
	}
	if !strings.Contains(got, "no_capture=true") {
		t.Fatalf("--no-capture did not travel as no_capture=true: %q", got)
	}
	if !strings.Contains(buf.String(), "replayed") {
		t.Fatalf("the result was not reported:\n%s", buf.String())
	}
}

// TestReplayDiffComparesTwoCaptures is the bead's --diff clause. Nothing is
// sent: both sides are already in the store, which is what makes a diff work on
// a machine with no upstream reachable at all.
func TestReplayDiffComparesTwoCaptures(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	base := cheapRow("req_base", `{"model":"claude-sonnet-5","max_tokens":8}`)
	base.OutputTokens = 10
	baseID := seedEvent(t, st, base)

	other := cheapRow("req_other", `{"model":"claude-sonnet-5","max_tokens":8}`)
	other.OutputTokens = 42 // the one field that differs
	otherID := seedEvent(t, st, other)

	cs := newCountingServer(t)
	var buf bytes.Buffer
	if err := runReplay([]string{itoa(baseID), "--diff", itoa(otherID), "--addr", cs.URL}, &buf); err != nil {
		t.Fatalf("runReplay --diff: %v\noutput:\n%s", err, buf.String())
	}
	if n := cs.hits.Load(); n != 0 {
		t.Fatalf("--diff sent %d request(s); it must send none", n)
	}
	out := buf.String()
	if !strings.Contains(out, "output_tokens") {
		t.Fatalf("--diff did not compare output_tokens:\n%s", out)
	}
	// Eight rows: status, model, input_tokens, output_tokens, cost,
	// cost_source, duration_ms, warnings. Only output_tokens was made to
	// differ, so the count is a real check that nothing else drifted.
	if !strings.Contains(out, "1 of 8 field(s) differ") {
		t.Fatalf("--diff miscounted the differing fields:\n%s", out)
	}
	if !strings.Contains(out, "≠") {
		t.Fatalf("--diff did not mark the differing row:\n%s", out)
	}
}

func TestReplayDiffReportsWhichSideIsMissing(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, cheapRow("req_one", `{"model":"claude-sonnet-5"}`))

	var buf bytes.Buffer
	err := runReplay([]string{itoa(id), "--diff", itoa(id + 999)}, &buf)
	if err == nil {
		t.Fatal("--diff against a missing id: want an error")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "--diff") {
		t.Fatalf("error does not name the missing side and the command: %v", err)
	}
}

func TestReplayRejectsANonNumericID(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runReplay([]string{"not-a-number"}, &buf); err == nil {
		t.Fatal("runReplay with a non-numeric id: want an error")
	}
}

// TestReplayReportsTheDashboardsRefusal: the server is the one that knows
// whether --replay was passed, so its message is carried through rather than
// replaced with the CLI's own guess.
func TestReplayReportsTheDashboardsRefusal(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, cheapRow("req_one", `{"model":"claude-sonnet-5"}`))

	cs := newCountingServer(t)
	cs.respond = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "replay is disabled: start `clens serve --replay` to enable it"})
	}

	var buf bytes.Buffer
	err := runReplay([]string{itoa(id), "--addr", cs.URL}, &buf)
	if err == nil {
		t.Fatal("want the endpoint's refusal surfaced")
	}
	if !strings.Contains(err.Error(), "replay is disabled") {
		t.Fatalf("the endpoint's own message was not carried through: %v", err)
	}
}

// TestReplayWithNoStoredBodySaysSo: capture may be off, or the body truncated.
// Replaying nothing would send an empty request, so this refuses first.
func TestReplayWithNoStoredBodySaysSo(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedEvent(t, st, cheapRow("req_bare", ""))

	cs := newCountingServer(t)
	var buf bytes.Buffer
	err := runReplay([]string{itoa(id), "--addr", cs.URL}, &buf)
	if err == nil || !strings.Contains(err.Error(), "no stored body") {
		t.Fatalf("want a 'no stored body' refusal, got %v", err)
	}
	if n := cs.hits.Load(); n != 0 {
		t.Fatalf("a bodyless replay sent %d request(s)", n)
	}
}

// TestReplayGateBounds covers what the gate is for, on a fixture that does not
// have to genuinely cost a quarter: exactly the threshold is allowed (the
// comparison is >, and a user reading "$0.25 (threshold $0.25)" would not
// expect a refusal), a cent over is not, and an unpriced row is gated because
// there is nothing to bound the replay with.
func TestReplayGateBounds(t *testing.T) {
	at := replayCostThresholdUSD
	f := func(v float64) *float64 { return &v }

	if _, gated := replayGate(&store.Event{EventSummary: store.EventSummary{CostUSD: f(at)}}); gated {
		t.Fatalf("a row costing exactly $%.2f was gated; the boundary should be allowed", at)
	}
	if _, gated := replayGate(&store.Event{EventSummary: store.EventSummary{CostUSD: f(at + 0.01)}}); !gated {
		t.Fatal("a row over the threshold was not gated")
	}
	if reason, gated := replayGate(&store.Event{}); !gated || !strings.Contains(reason, "unpriced") {
		t.Fatalf("an unpriced row: got (%q, %v), want it gated and named", reason, gated)
	}
	// A subscription row has no cost_usd at all -- its api-equivalent figure is
	// the only thing that can bound the replay, so it has to be consulted.
	if _, gated := replayGate(&store.Event{EventSummary: store.EventSummary{ApiEquivalentCostUSD: f(9.0)}}); !gated {
		t.Fatal("a subscription row's api-equivalent cost did not gate the replay")
	}
}
