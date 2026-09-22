package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// readAsset returns one embedded asset's text. The tests here assert on the
// bytes the binary actually serves, not on the files as they happen to sit on
// disk, so a file missing from the go:embed directive fails here too.
func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(Files, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// idRef matches a literal $('some-id') lookup in app.js. The concatenated form
// is deliberately not matched: an id built at runtime is one this test cannot
// resolve, which is why the quota inputs are found by a data attribute instead.
var idRef = regexp.MustCompile(`\$\(\s*'([A-Za-z0-9_-]+)'\s*\)`)

var idDef = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)

// TestAssetsEveryLookupHasAMount is the check that keeps a view from rendering
// into nothing. Every literal getElementById target in app.js must be defined
// somewhere -- in index.html, or in markup app.js itself injects (the call
// detail's replay controls are built at runtime). A loader pointed at an id
// nobody creates throws on the browser's console and nowhere else; nothing in
// the Go test suite would notice.
func TestAssetsEveryLookupHasAMount(t *testing.T) {
	js := readAsset(t, "app.js")

	defined := map[string]bool{}
	for _, m := range idDef.FindAllStringSubmatch(readAsset(t, "index.html"), -1) {
		defined[m[1]] = true
	}
	for _, m := range idDef.FindAllStringSubmatch(js, -1) {
		defined[m[1]] = true
	}

	seen := map[string]bool{}
	for _, m := range idRef.FindAllStringSubmatch(js, -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !defined[id] {
			t.Errorf("app.js looks up %q, but no asset defines id=%q", id, id)
		}
	}
	if len(seen) < 30 {
		// A regex that silently stopped matching would make the loop above
		// vacuous and this test green for the wrong reason. The real count is
		// nearer forty; this is a floor, not an inventory.
		t.Errorf("found only %d id lookups; the pattern has stopped matching app.js's real calls", len(seen))
	}
}

// TestAssetsTheFourNewTabsHaveTheirMountPoints is br-GI-1-18's tab clause. Each
// of the four views named by the bead needs all three of: a tab that selects
// it, a section the tab reveals, and a container inside that section for its
// loader to write into -- and a loader registered under the same key, since a
// tab with no loader stops at "nothing here yet".
func TestAssetsTheFourNewTabsHaveTheirMountPoints(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	tabs := []struct{ view, mount string }{
		{"sources", "sources-table"},
		{"quota", "quota-body"},
		{"reconcile", "reconcile-table"},
		{"models", "models-table"},
	}
	for _, tab := range tabs {
		if !strings.Contains(html, `data-view="`+tab.view+`"`) {
			t.Errorf("no tab button selects the %s view", tab.view)
		}
		if !strings.Contains(html, `id="view-`+tab.view+`"`) {
			t.Errorf("no section carries id=view-%s, so the tab has nothing to reveal", tab.view)
		}
		if !strings.Contains(html, `id="`+tab.mount+`"`) {
			t.Errorf("the %s view has no %s container for its loader to write into", tab.view, tab.mount)
		}
		if !regexp.MustCompile(`\b` + tab.view + `:\s*load`).MatchString(js) {
			t.Errorf("no loader is registered under %q, so the tab stops at its loading placeholder", tab.view)
		}
	}
}

// TestAssetsChartsAreInlineSVG: the charts are built as SVG markup strings in
// app.js -- no charting library, no image file, no canvas. The positive half
// (each chart really emits an <svg>) matters as much as the negative half: a
// chart function that quietly returns an empty string is a blank panel with
// no error.
func TestAssetsChartsAreInlineSVG(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// Charts are drawn inline, so the assets must not pull in an image or a
	// canvas -- both would be a rendering path this repo cannot style, theme,
	// or check.
	for _, bad := range []string{"<img", "<canvas", ".png", ".jpg", ".jpeg", ".gif"} {
		for name, asset := range map[string]string{"index.html": html, "app.js": js, "style.css": css} {
			if strings.Contains(asset, bad) {
				t.Errorf("%s contains %q: the charts are inline SVG, not images or canvas", name, bad)
			}
		}
	}

	for _, fn := range []string{"chartByPeriod", "chartQuota", "chartReconcile"} {
		body, ok := funcBody(js, fn)
		if !ok {
			t.Errorf("no function %s in app.js; the chart list here is stale", fn)
			continue
		}
		if !strings.Contains(body, "<svg") {
			t.Errorf("%s does not emit an <svg>, so its panel renders empty", fn)
		}
		// A <title> child is what gives a bar a hover tooltip, and in an inline
		// SVG it is also the accessible name for the shape.
		if !strings.Contains(body, "<title>") {
			t.Errorf("%s draws shapes with no <title>, so a bar has no label to hover or read", fn)
		}
	}

	if !strings.Contains(css, "svg.chart") {
		t.Error("style.css no longer styles svg.chart, so the inline charts would be unstyled boxes")
	}
}

// TestAssetsTheCallDetailReplacesTheList is br-GI-5-01's wiring contract. A
// drill-down must *replace* the list it was clicked in rather than render
// underneath it, and must not fetch a list it is about to hide. That second
// half is the reported bug, and it is invisible to every other test here: the
// pre-fix app.js satisfied every mount and lookup check in this file.
func TestAssetsTheCallDetailReplacesTheList(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	// Each list is a container so the detail can hide it as a unit. A row list
	// with no wrapper has nothing to hide but the table itself, and anything
	// added beside it (a filter row, a pager) stays on screen under the detail.
	for _, id := range []string{"calls-list", "sessions-list"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("no #%s wrapper: the detail has no list to replace", id)
		}
	}
	// Both details start hidden. One that did not would flash an empty panel on
	// every load of its tab, before any click has chosen anything.
	for _, id := range []string{"call-detail", "session-detail"} {
		if !regexp.MustCompile(`id="` + id + `"[^>]*\shidden`).MatchString(html) {
			t.Errorf("#%s is not declared hidden, so an empty detail panel shows before any click", id)
		}
	}

	// Each setter is two-sided. A one-sided toggle hides the list and shows
	// nothing: a blank panel, not a detail.
	for _, s := range []struct{ fn, list, detail string }{
		{"setCallDetail", "calls-list", "call-detail"},
		{"setSessionDetail", "sessions-list", "session-detail"},
	} {
		body, ok := funcBody(js, s.fn)
		if !ok {
			t.Errorf("no function %s in app.js", s.fn)
			continue
		}
		if !strings.Contains(body, "$('"+s.list+"').hidden") {
			t.Errorf("%s does not hide #%s, so the list stays on screen under its detail", s.fn, s.list)
		}
		if !strings.Contains(body, "$('"+s.detail+"').hidden") {
			t.Errorf("%s does not reveal #%s, so the drill-down has nothing on screen", s.fn, s.detail)
		}
	}

	// A detail needs a way back that does not depend on the tab strip, because
	// a drill-down can be reached from a tab other than the one it renders in.
	for _, s := range []struct{ fn, back, setter string }{
		{"showCall", "call-back", "setCallDetail(false)"},
		{"showSession", "session-back", "setSessionDetail(false)"},
	} {
		body, ok := funcBody(js, s.fn)
		if !ok {
			t.Errorf("no function %s in app.js", s.fn)
			continue
		}
		// The emitted markup, not merely the id as a string: showCall also
		// *looks up* call-back to bind it, so a check for the bare id is
		// satisfied by the lookup alone -- it cannot tell a rendered control
		// from a dangling one.
		if !strings.Contains(body, `id="`+s.back+`"`) {
			t.Errorf("%s renders no %s control, so the only route back is the tab strip", s.fn, s.back)
		}
		if !strings.Contains(body, s.setter) {
			t.Errorf("%s never leaves detail mode, so its back control would hide nothing", s.fn)
		}
	}

	// The [data-call] branch, which is where the bug was. It is nested inside
	// the anonymous click listener, so funcBody -- which reads a whole
	// top-level `function name(` -- cannot reach it, and it is sliced between
	// two literal source anchors instead.
	pre, catch, ok := clickBranch(js, `const call = ev.target.closest('[data-call]')`)
	if !ok {
		t.Fatalf("no [data-call] branch in app.js: the anchors this test slices between have moved")
	}
	// The vacuity guard, first: without it the three assertions below pass for
	// free on an empty slice. A static check that stops matching must fail
	// loudly, never go green for the wrong reason.
	if !strings.Contains(pre, "showCall(") || !strings.Contains(pre, "dataset.call") {
		t.Fatalf("the [data-call] slice is not the branch this test means to check: %q", pre)
	}
	if !strings.Contains(pre, "reveal('calls')") {
		t.Error("the call drill-down does not reveal the Calls view without fetching it")
	}
	if strings.Contains(pre, "loadCalls(") {
		t.Error("the call drill-down runs loadCalls, a fetch of a list it is about to hide")
	}
	// The reported bug, precisely: show('calls') runs the Calls loader. It sits
	// before this slice's end anchor, so the deliberate failure-path fallback --
	// which lives in the catch, after that anchor -- does not trip this.
	if strings.Contains(pre, "show('calls')") {
		t.Error("the call drill-down calls show('calls') on the happy path, which refetches the list instead of showing the call")
	}

	// The catch guards, each scoped to its own branch's catch body. detailSeq
	// is compared in four places in this handler, so a file-wide or
	// branch-wide check could be satisfied by the sibling catch alone -- and
	// pass with one branch unguarded, the state these assertions exist for.
	if !strings.Contains(catch, "setStatus(") {
		t.Fatalf("the [data-call] catch region is not the branch's catch: %q", catch)
	}
	if !strings.Contains(catch, "if (seq === detailSeq)") {
		t.Error("the [data-call] catch acts on a stale failure, switching the view the user has already left and wiping a newer detail")
	}

	preS, catchS, ok := clickBranch(js, `const sess = ev.target.closest('[data-session]')`)
	if !ok {
		t.Fatalf("no [data-session] branch in app.js: the anchors this test slices between have moved")
	}
	if !strings.Contains(preS, "showSession(") {
		t.Fatalf("the [data-session] slice is not the branch this test means to check: %q", preS)
	}
	if !strings.Contains(catchS, "setStatus(") {
		t.Fatalf("the [data-session] catch region is not the branch's catch: %q", catchS)
	}
	if !strings.Contains(catchS, "if (seq === detailSeq)") {
		t.Error("the [data-session] catch acts on a stale failure, overwriting a newer detail's status line")
	}
}

// TestAssetsTheDetailModeFlipFollowsTheFetch pins the two orderings inside the
// detail renderers that a later refactor would silently reverse. Both are
// properties of position, not of presence, so neither can be checked by looking
// for a substring on its own.
func TestAssetsTheDetailModeFlipFollowsTheFetch(t *testing.T) {
	js := readAsset(t, "app.js")

	for _, s := range []struct{ fn, setter string }{
		{"showCall", "setCallDetail(true)"},
		{"showSession", "setSessionDetail(true)"},
	} {
		body, ok := funcBody(js, s.fn)
		if !ok {
			t.Errorf("no function %s in app.js", s.fn)
			continue
		}
		fetch := strings.Index(body, "await api(")
		flip := strings.Index(body, s.setter)
		guard := strings.Index(body, "seq !== detailSeq")
		// The vacuity guard: a rename that defeats the extraction must fail
		// here rather than let the two index comparisons pass on -1.
		if fetch < 0 || flip < 0 || guard < 0 {
			t.Errorf("%s no longer contains the await api( / %s / seq !== detailSeq triple this test pins (at %d, %d, %d)",
				s.fn, s.setter, fetch, flip, guard)
			continue
		}
		if flip < fetch {
			t.Errorf("%s flips to detail mode before its fetch resolves, so a detail request that fails blanks the list for nothing", s.fn)
		}
		if guard < fetch {
			t.Errorf("%s checks its generation before the response exists, so it drops a live detail every time", s.fn)
		}
	}
}

// TestAssetsShowResetsBothModesUnconditionally pins the tab click as the second
// route back to a list. A bare presence check for the two resets stays green
// through the regression it exists to catch, which is what makes this test
// assert a property over show's body rather than two substrings.
func TestAssetsShowResetsBothModesUnconditionally(t *testing.T) {
	js := readAsset(t, "app.js")

	body, ok := funcBody(js, "show")
	if !ok {
		t.Fatalf("no function show in app.js")
	}
	// Vacuity guard first: the negative below passes for free on an empty body.
	if !strings.Contains(body, "setCallDetail(false)") || !strings.Contains(body, "setSessionDetail(false)") {
		t.Fatalf("show does not reset both details, so a tab click leaves one open: %q", body)
	}
	// This is a raw-text assertion, so the literal is banned from show's body
	// entirely, prose included: it cannot tell a re-added guard from a comment
	// quoting one. reveal's body does contain `b.dataset.view === view`, which
	// is why this is scoped to show rather than run over the file.
	if strings.Contains(body, "view ===") {
		t.Error("show's body mentions `view ===`, so at least one of the two resets is conditional again: a detail stays open when the user leaves via Overview or Warnings, both of which render their own data-call links")
	}

	rv, ok := funcBody(js, "reveal")
	if !ok {
		t.Fatalf("no function reveal in app.js")
	}
	if !strings.Contains(rv, "current =") {
		t.Fatalf("reveal's body is not the function this test means to check: %q", rv)
	}
	// Every tab click routes through show -> reveal and every [data-call]
	// drill-down calls reveal directly, so this one site is what supersedes a
	// detail fetch that is still in flight.
	if !strings.Contains(rv, "detailSeq++") {
		t.Error("reveal does not take the next generation, so a detail response that lands after a view change is still rendered under it")
	}
}

// clickBranch splits one drill-down branch of the delegated click listener into
// the region before its `} catch` and the catch body itself, up to the branch's
// terminating return. Both halves are needed: the happy path must not run a
// loader, while the deliberate fail-open fallback that does lives in the catch,
// and slicing to the catch is exactly what keeps the two apart.
func clickBranch(js, anchor string) (pre, catch string, ok bool) {
	i := strings.Index(js, anchor)
	if i < 0 {
		return "", "", false
	}
	tail := js[i:]
	j := strings.Index(tail, "} catch")
	if j < 0 {
		return "", "", false
	}
	c := tail[j:]
	k := strings.Index(c, "return;")
	if k < 0 {
		return "", "", false
	}
	return tail[:j], c[:k], true
}

// funcBody returns the source of a top-level function, from its `function name(`
// to the closing brace in column 0. app.js is written one top-level
// declaration per block with unindented closing braces, which is what makes
// this two-line scan enough; it is a test helper, not a parser.
func funcBody(js, name string) (string, bool) {
	// Normalized to LF before slicing. app.js is LF in the repository, but a
	// Windows checkout with core.autocrlf=true hands this function CRLF -- and
	// then the "\n}\n" terminator below never matches, so the slice silently
	// runs to the end of the file and every caller's assertions are made
	// against that tail instead of one function. They do not fail; they pass
	// vacuously, which is worse. Normalizing here fixes all callers at once
	// rather than asking each one to remember.
	js = strings.ReplaceAll(js, "\r\n", "\n")
	start := strings.Index(js, "function "+name+"(")
	if start < 0 {
		return "", false
	}
	rest := js[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		return rest, true
	}
	return rest[:end], true
}

// TestAssetsTheBadgeIsInTheHeader (br-GI-7-05) is the positional half of the
// badge guard. The mechanical half -- app.js looks up an id that index.html
// defines -- is TestAssetsEveryLookupHasAMount's job; this one asserts *where*
// the badge lives.
//
// It belongs in the header because the incident it exists for was a dashboard
// that looked healthy while Claude Code was pointed at another product's port.
// Putting the answer on the Sources tab would make it depend on the user
// already suspecting something -- which is exactly what they could not do.
func TestAssetsTheBadgeIsInTheHeader(t *testing.T) {
	html := readAsset(t, "index.html")

	start := strings.Index(html, `<header class="app-header">`)
	if start < 0 {
		t.Fatal("index.html has no app-header block")
	}
	end := strings.Index(html[start:], "</header>")
	if end < 0 {
		t.Fatal("the app-header block is never closed")
	}
	header := html[start : start+end]

	if !strings.Contains(header, `id="proxy-mode"`) {
		t.Error("the proxy-mode badge is not inside the app header, so it is not on every tab")
	}
	if !strings.Contains(readAsset(t, "app.js"), "$('proxy-mode')") {
		t.Error("app.js never looks up the proxy-mode badge")
	}
}

// TestAssetsTheBodyRendererEscapes (br-GI-7-04, T6) is a source-shape
// assertion, not a behavioural one.
//
// The ceiling, stated rather than implied: there is no JS runtime in this
// toolchain and no dependency here is a JS engine -- this file is regex and
// text over the embedded bytes -- so what follows proves the escaping *call is
// present in the source*, not that the rendered pixels are safe. That is the
// strongest guarantee the no-build-step, no-browser-automation posture allows.
//
// It is worth having anyway, because the thing it guards is the story's one
// real vulnerability: bodies are arbitrary bytes from a remote endpoint going
// into innerHTML, on a page that also holds a replay button that spends money.
// A body containing </script> or <img onerror=...> is a live injection path,
// and "every body goes through one esc()" is only true while it stays true.
func TestAssetsTheBodyRendererEscapes(t *testing.T) {
	js := readAsset(t, "app.js")

	body, ok := funcBody(js, "bodySection")
	if !ok {
		t.Fatal("app.js has no top-level bodySection: the body renderer must be one function so esc() has one home")
	}
	// The vacuity guard, first: a rename that defeats the extraction would
	// otherwise leave every assertion below passing on an empty string.
	if strings.TrimSpace(body) == "" {
		t.Fatal("bodySection sliced out empty -- the extraction is broken, not the renderer")
	}
	if !strings.Contains(body, "bytesB64") {
		t.Fatalf("the bodySection slice does not mention its body parameter, so it is not the renderer:\n%s", body)
	}
	if !strings.Contains(body, "esc(") {
		t.Error("bodySection never calls esc(): a body goes into innerHTML unescaped")
	}

	// The read-path markers, and the order they are selected in. An unwired cap
	// has to be checked before the completeness is consulted, or a body that
	// decodes fine gets labelled "would not decompress" merely because nobody
	// wired the cap -- the missing cap is not evidence about the bytes. This is
	// positional rather than symbolic because the defect is exactly an
	// ordering one, and no assertion on the strings alone can see it.
	read, ok := funcBody(js, "readPathMarker")
	if !ok {
		t.Fatal("app.js has no top-level readPathMarker")
	}
	if strings.TrimSpace(read) == "" {
		t.Fatal("readPathMarker sliced out empty -- the extraction is broken")
	}
	for _, want := range []string{
		"response shown raw — read cap not configured",
		"response truncated at the read cap of ",
		"response decoded only partially — its tail was corrupt",
		"response shown undecoded — it would not decompress",
	} {
		if !strings.Contains(read, want) {
			t.Errorf("readPathMarker is missing the marker %q", want)
		}
	}
	capAt := strings.Index(read, "BodyCapBytes")
	completenessAt := strings.Index(read, "RespBodyCompleteness")
	if capAt < 0 || completenessAt < 0 {
		t.Fatalf("readPathMarker does not read both BodyCapBytes and RespBodyCompleteness:\n%s", read)
	}
	if capAt > completenessAt {
		t.Error("readPathMarker consults RespBodyCompleteness before BodyCapBytes, so an unwired cap can manufacture the 'would not decompress' state")
	}

	// The capture-incomplete marker, which is the CLI's own wording the plan
	// pins (show.go). CaptureComplete is false for either cause, so the line
	// must not claim the cap unconditionally.
	capture, ok := funcBody(js, "captureMarker")
	if !ok {
		t.Fatal("app.js has no top-level captureMarker")
	}
	if !strings.Contains(capture, "incomplete (truncated, or the stream ended early)") {
		t.Error("captureMarker does not carry the CLI's own wording for an incomplete capture")
	}
	// Both bodies, not just the response (br-GI-7-08). The row does not record
	// which side was cut, so the lengths are the only evidence -- and a version
	// that checked RespBody alone reads a request-truncated row as "does not
	// record which cause" while its stored request body sits at exactly the
	// cap. That is the state the manual run's row 84769 produces, and it is why
	// the request half is asserted here rather than assumed.
	if !strings.Contains(capture, "ReqBody") || !strings.Contains(capture, "RespBody") {
		t.Error("captureMarker does not compare both stored bodies against the read cap")
	}

	// Both transcript states. Both are asserted because both are reachable on
	// the same row type and a single label would silently reclassify the other:
	// a row with content is a reconstruction, a row without is an absence.
	for _, want := range []string{
		"not captured — transcript source",
		"reconstructed from transcript — not a wire capture",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js is missing the transcript state %q", want)
		}
	}

	// The third content column needs the third marker (br-GI-7-09).
	// transcript_content is bounded by the same cap the bodies are, so without
	// this a reconstruction cut at the cap renders identically to a whole one --
	// the defect this story exists to close, on the column the bodies' marker
	// does not reach.
	tcap, ok := funcBody(js, "transcriptCapMarker")
	if !ok {
		t.Fatal("app.js has no top-level transcriptCapMarker: capped transcript content is unmarked")
	}
	if strings.TrimSpace(tcap) == "" {
		t.Fatal("transcriptCapMarker sliced out empty -- the extraction is broken")
	}
	if !strings.Contains(tcap, "BodyCapBytes") || !strings.Contains(tcap, "TranscriptContent") {
		t.Error("transcriptCapMarker does not compare the stored transcript against the read cap")
	}
	// The negative half, which is the half that matters: the marker must not be
	// driven by CaptureComplete. That flag is about the two teed bodies, and a
	// jsonl row whose content was capped has truncated no capture -- wiring the
	// two together would label every capped reconstruction as a broken capture.
	if strings.Contains(tcap, "CaptureComplete") {
		t.Error("transcriptCapMarker keys off CaptureComplete: a capped transcript is not a truncated capture")
	}
	// ...and the same reasoning the bodies' marker follows: an unwired cap is
	// not evidence about the bytes, so it is checked before any comparison.
	if strings.Index(tcap, "BodyCapBytes") > strings.Index(tcap, "TranscriptContent") {
		t.Error("transcriptCapMarker compares TranscriptContent before checking BodyCapBytes is wired")
	}

	// And it must actually be *called*, which is not the same assertion.
	// Deleting the call from the transcript branch leaves this function perfect
	// and never rendered, and every check above still passes -- the marker
	// would simply never appear, which is precisely the defect the marker
	// exists to prevent.
	//
	// Scoped to showCall's body, not the whole file: `transcriptCapMarker(e)`
	// also matches the function's own definition, so a file-wide Contains is
	// satisfied by the definition alone and passes with the call deleted. That
	// vacuous version was written first and the mutation check caught it.
	call, ok := funcBody(js, "showCall")
	if !ok {
		t.Fatal("app.js has no top-level showCall, so the call site cannot be located")
	}
	if !strings.Contains(call, "transcriptCapMarker(e)") {
		t.Error("transcriptCapMarker is defined but never called from showCall: the marker never renders")
	}
}

// TestAssetsTimeWindowBuildsTheOffsetByHand is a source-shape assertion over
// the picker's one window computation, and the ceiling is the same one
// TestAssetsTheBodyRendererEscapes states: there is no JS runtime here, so this
// proves the *source* builds the offset rather than that the emitted string is
// right. The semantic cases -- that the window denotes the picked local instant,
// the Dec->Jan rollover, the month's 28-31 day width, and DST -- are manual
// verification in the PR's test plan.
//
// Why it is worth having anyway: the defect it guards is invisible to a parse
// test. new Date('2026-09-21').toISOString() emits 2026-09-21T00:00:00.000Z,
// Go's time.Parse(time.RFC3339, s) accepts the trailing Z as UTC rather than
// rejecting it, and the window silently shifts by the zone offset -- 19,800 s,
// or 5h30m, on IST: unix 1789948800 against the intended 1789929000. The
// dashboard would then be reporting a different day than the one picked, which
// is the very comparison GI-11 exists to make trustworthy.
func TestAssetsTimeWindowBuildsTheOffsetByHand(t *testing.T) {
	js := readAsset(t, "app.js")

	body, ok := funcBody(js, "timeWindow")
	if !ok {
		t.Fatal("app.js has no top-level `function timeWindow(`: the window must be computed in one place, so this guard has an anchor to slice")
	}
	// The vacuity guard, first: a rename or a restyle that defeats the
	// extraction would otherwise leave both assertions below passing over an
	// empty slice -- which is worse than failing, because it reads as green.
	if strings.TrimSpace(body) == "" {
		t.Fatal("timeWindow sliced out empty -- the extraction is broken, not the function")
	}

	// Both halves are scoped to this slice deliberately. A whole-file
	// !Contains("toISOString") would be a tripwire for any future legitimate use
	// elsewhere in the dashboard, and a whole-file positive match would pass for
	// a timeWindow that computed an offset and then never used it.
	if strings.Contains(body, "toISOString") {
		t.Error("timeWindow calls toISOString(), which emits a trailing Z that Go reads as UTC: the window silently shifts by the zone offset and shows a different day than the one picked")
	}
	if !strings.Contains(body, "getTimezoneOffset") {
		t.Error("timeWindow never reads getTimezoneOffset(): the ±hh:mm suffix has to be built from the local offset, not assumed")
	}
}

// TestAssetsThePickerMountsBothTabs pins the wiring an id-existence check
// cannot see: that both tabs actually route their bounds through timeWindow,
// that only Stats offers `custom`, and that Stats keeps its free-text pair.
//
// Each of those is an Outcome Definition clause with no other coverage. A
// picker mounted but never consulted would leave the id guard green and the
// filter dead; a `custom` option on Calls would be a control offering a
// capability that tab does not have; and dropping the free-text inputs would
// silently narrow `24h` and arbitrary RFC3339 ranges out of existence.
func TestAssetsThePickerMountsBothTabs(t *testing.T) {
	js := readAsset(t, "app.js")
	html := readAsset(t, "index.html")

	for _, fn := range []string{"callFilter", "loadStats"} {
		body, ok := funcBody(js, fn)
		if !ok {
			t.Fatalf("app.js has no top-level %s, so its bounds cannot be checked", fn)
		}
		if !strings.Contains(body, "timeWindow(") {
			t.Errorf("%s never calls timeWindow(): the picker is mounted but its window is not applied", fn)
		}
	}

	// The option sets, sliced per select so the two cannot be confused.
	callsOpts, ok := selectOptions(html, "c-window-gran")
	if !ok {
		t.Fatal("index.html has no c-window-gran select")
	}
	statsOpts, ok := selectOptions(html, "s-window-gran")
	if !ok {
		t.Fatal("index.html has no s-window-gran select")
	}
	if strings.Contains(callsOpts, `value="custom"`) {
		t.Error("the Calls picker offers `custom`, but that row has no free-text pair for it to reveal: a selectable entry that filters nothing is a dead control")
	}
	if !strings.Contains(statsOpts, `value="custom"`) {
		t.Error("the Stats picker has no `custom` option, so the retained free-text pair is unreachable")
	}

	// The defaults, which the bead's three-option phrasing left open and a
	// review round escalated (br-GI-11-08's "Corrected during implementation"
	// note records the ratification). Calls' default is the neutral `any time`,
	// and this is load-bearing rather than cosmetic: a native <select> has no
	// unset state, so a mount offering only hour|date|month would display `hour`
	// while timeWindow() returns null -- the value input is empty, and the
	// function's own `if (!gran || !value) return null` makes that the same
	// no-window path. The control would be advertising a granularity it is not
	// applying, which is the dead-option misdescription the Calls mount already
	// avoids by omitting `custom`. Stats' `custom` is the deliberate exception:
	// it is what reveals the free-text pair.
	if strings.Contains(callsOpts, "selected") {
		t.Error("the Calls picker marks an option `selected`: its default must be the neutral \"any time\" (the first, empty-valued option), or the select displays a granularity that is not being applied")
	}
	if !strings.Contains(callsOpts, `value=""`) {
		t.Error("the Calls picker has no empty-valued option, so its default cannot be \"any time\"")
	}
	if !strings.Contains(statsOpts, `value="custom" selected`) {
		t.Error("the Stats picker's `custom` is not the default, so the retained free-text pair is not the one being used")
	}
	for _, want := range []string{"hour", "date", "month"} {
		if !strings.Contains(callsOpts, `value="`+want+`"`) || !strings.Contains(statsOpts, `value="`+want+`"`) {
			t.Errorf("both pickers must offer %q", want)
		}
	}

	// The free-text pair stays mounted: loadStats falls back to it for `custom`.
	for _, id := range []string{"s-since", "s-until"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html no longer mounts %s: the free-text window is gone, taking `24h` and arbitrary RFC3339 ranges with it", id)
		}
		if !strings.Contains(js, "$('"+id+"')") {
			t.Errorf("app.js no longer reads %s", id)
		}
	}
}

// selectOptions returns the markup between the <select id="id"> tag and its
// closing </select>, so an option-set assertion is made against one control
// rather than the whole document -- where a `custom` in the Calls row and a
// `custom` in the Stats row are indistinguishable.
func selectOptions(html, id string) (string, bool) {
	start := strings.Index(html, `id="`+id+`"`)
	if start < 0 {
		return "", false
	}
	rest := html[start:]
	end := strings.Index(rest, "</select>")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
