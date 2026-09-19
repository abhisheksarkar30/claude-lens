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
