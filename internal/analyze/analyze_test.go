package analyze

import (
	"bytes"
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

func strPtr(s string) *string { return &s }

func hasKind(warnings []store.Warning, kind Kind) bool {
	for _, w := range warnings {
		if w.Kind == string(kind) {
			return true
		}
	}
	return false
}

func TestAnalyzeCleanEventReturnsNil(t *testing.T) {
	warnings := Analyze(parse.Meta{}, parse.Usage{InputTokens: 100, OutputTokens: 50}, &store.Event{})
	if warnings != nil {
		t.Errorf("Analyze on a clean event = %v, want nil", warnings)
	}
}

func TestAnalyzeCacheBreakpointsExceeded(t *testing.T) {
	five := parse.Meta{CacheControlSites: []string{"a", "b", "c", "d", "e"}, HasCacheControl: true}
	warnings := Analyze(five, parse.Usage{CacheWrite5mTokens: 10}, &store.Event{})
	if !hasKind(warnings, KindCacheBreakpointsExceeded) {
		t.Errorf("5 breakpoints did not raise %s: %v", KindCacheBreakpointsExceeded, warnings)
	}

	four := parse.Meta{CacheControlSites: []string{"a", "b", "c", "d"}, HasCacheControl: true}
	warnings = Analyze(four, parse.Usage{CacheWrite5mTokens: 10}, &store.Event{})
	if hasKind(warnings, KindCacheBreakpointsExceeded) {
		t.Errorf("4 breakpoints raised %s, want none: %v", KindCacheBreakpointsExceeded, warnings)
	}
}

// bodyOfTokens builds a captured request body whose byte/4 estimate -- the
// analyzer's prefix measure -- is about n tokens. The content is
// irrelevant; the size is the whole point.
func bodyOfTokens(n int) []byte { return bytes.Repeat([]byte("x"), n*4) }

// The rule is only as good as its lookup: a typo'd match string silently
// disables it for that one model, and no other test would notice. Driven
// off the shipped price table so a model added there without a minimum
// here fails loudly instead of quietly going uncovered.
func TestMinimumCacheablePrefixCoversShippedModels(t *testing.T) {
	for model := range pricing.ShippedTable() {
		if _, ok := minimumCacheablePrefixFor(model); !ok {
			t.Errorf("shipped model %q has no minimum cacheable prefix", model)
		}
	}
	if _, ok := minimumCacheablePrefixFor("claude-nonesuch-9"); ok {
		t.Error("an unknown model resolved a minimum, want none")
	}
}

// Test 14: the same captured prefix, only the model varies. This is the
// shape that actually tests the minimum table -- a fixture that varies the
// *usage outcome* (as this test once did) passes even when the rule ignores
// the model entirely, because the response's own cache tokens decide the
// outcome unaided. Here the usage is empty on every row, so the model is
// the only input that can change the answer.
func TestAnalyzeCachePrefixBelowMinimum(t *testing.T) {
	marked := parse.Meta{HasCacheControl: true, CacheControlSites: []string{"system[0]"}}
	// 3K tokens sits above Opus 5's 512-token minimum and below Opus 4.6's
	// 4096 -- the non-monotonic case the table exists for.
	threeK := &store.Event{ReqBody: bodyOfTokens(3000)}

	cases := []struct {
		name  string
		meta  parse.Meta
		usage parse.Usage
		ev    *store.Event
		want  bool
	}{
		{"3K prefix on Opus 5 (min 512): long enough to cache",
			marked, parse.Usage{Model: "claude-opus-5"}, threeK, false},
		{"3K prefix on Opus 4.6 (min 4096): too short",
			marked, parse.Usage{Model: "claude-opus-4-6"}, threeK, true},
		{"prefix below the model's minimum",
			marked, parse.Usage{Model: "claude-opus-5"}, &store.Event{ReqBody: bodyOfTokens(400)}, true},
		{"prefix exactly at the minimum",
			marked, parse.Usage{Model: "claude-opus-5"}, &store.Event{ReqBody: bodyOfTokens(512)}, false},
		{"the requested model stands in when the response named none",
			parse.Meta{HasCacheControl: true, ModelRequested: "claude-opus-4-6"},
			parse.Usage{}, &store.Event{ReqBody: bodyOfTokens(400)}, true},
		{"no model anywhere: no minimum to be under",
			marked, parse.Usage{}, &store.Event{ReqBody: bodyOfTokens(400)}, false},
		{"an unknown model has no minimum either",
			marked, parse.Usage{Model: "claude-nonesuch-9"}, &store.Event{ReqBody: bodyOfTokens(4)}, false},
		{"no captured body to measure",
			marked, parse.Usage{Model: "claude-opus-5"}, &store.Event{}, false},
		{"no marker at all",
			parse.Meta{}, parse.Usage{Model: "claude-opus-5"}, &store.Event{ReqBody: bodyOfTokens(400)}, false},
		// Both of these would fire on the table alone (Opus 4.6, 3K prefix),
		// so each isolates the other suppression: an effect already landed,
		// and there is nothing to diagnose.
		{"the write landed",
			marked, parse.Usage{Model: "claude-opus-4-6", CacheWrite5mTokens: 3000}, threeK, false},
		{"a read of an already-cached prefix",
			marked, parse.Usage{Model: "claude-opus-4-6", CacheReadTokens: 3000}, threeK, false},
	}

	for _, tc := range cases {
		got := hasKind(Analyze(tc.meta, tc.usage, tc.ev), KindCachePrefixBelowMinimum)
		if got != tc.want {
			t.Errorf("%s: %s = %v, want %v", tc.name, KindCachePrefixBelowMinimum, got, tc.want)
		}
	}
}

func TestAnalyzeMultiRuleNoDuplicateKinds(t *testing.T) {
	meta := parse.Meta{
		HasCacheControl:   true,
		CacheControlSites: []string{"a", "b", "c", "d", "e"}, // exceeds AND produces no cache effect
		ModelRequested:    "claude-opus-5",
	}
	warnings := Analyze(meta, parse.Usage{}, &store.Event{ReqBody: bodyOfTokens(100)})
	seen := map[string]int{}
	for _, w := range warnings {
		seen[w.Kind]++
	}
	for kind, n := range seen {
		if n != 1 {
			t.Errorf("kind %s appeared %d times, want exactly 1", kind, n)
		}
	}
	if !hasKind(warnings, KindCacheBreakpointsExceeded) || !hasKind(warnings, KindCachePrefixBelowMinimum) {
		t.Errorf("expected both rules to fire: %v", warnings)
	}
}

// --- kinds.go contract -------------------------------------------------

func TestAllKindsReturnsACopy(t *testing.T) {
	out := AllKinds()
	if len(out) == 0 {
		t.Fatal("AllKinds returned nothing")
	}
	out[0].Description = "mutated"
	if allKinds[0].Description == "mutated" {
		t.Error("mutating AllKinds' result corrupted the package's own table")
	}
}

func TestAllKindsDescriptionsAreWellFormed(t *testing.T) {
	for _, k := range AllKinds() {
		if k.Description == "" {
			t.Errorf("kind %s has an empty description", k.Kind)
		}
		if strings.ContainsAny(k.Description, "|\n") {
			t.Errorf("kind %s's description contains a | or newline: %q", k.Kind, k.Description)
		}
	}
}

func TestNonAnalyzeKindsNamesTheMinimumSet(t *testing.T) {
	for _, k := range []Kind{KindAnalyzerPanic, KindSourceMismatch, KindCostDrift, KindQuotaWindowApproaching} {
		if _, ok := nonAnalyzeKinds[k]; !ok {
			t.Errorf("nonAnalyzeKinds is missing %s", k)
		}
	}
}
