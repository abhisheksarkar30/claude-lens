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
// chart function that quietly returns '' is a blank panel with no error.
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
