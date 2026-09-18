package analyze

import (
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
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

func TestAnalyzeCachePrefixBelowMinimum(t *testing.T) {
	marked := parse.Meta{HasCacheControl: true, CacheControlSites: []string{"system[0]"}}

	// Below the model's minimum: marker present, nothing landed in usage.
	below := Analyze(marked, parse.Usage{InputTokens: 500}, &store.Event{})
	if !hasKind(below, KindCachePrefixBelowMinimum) {
		t.Errorf("marker with no cache effect did not raise %s: %v", KindCachePrefixBelowMinimum, below)
	}

	// At/above the minimum: the write landed.
	above := Analyze(marked, parse.Usage{CacheWrite5mTokens: 3000}, &store.Event{})
	if hasKind(above, KindCachePrefixBelowMinimum) {
		t.Errorf("marker with a landed write raised %s, want none: %v", KindCachePrefixBelowMinimum, above)
	}

	// No marker at all: never fires regardless of usage.
	unmarked := Analyze(parse.Meta{}, parse.Usage{}, &store.Event{})
	if hasKind(unmarked, KindCachePrefixBelowMinimum) {
		t.Errorf("no marker raised %s, want none: %v", KindCachePrefixBelowMinimum, unmarked)
	}

	// The non-monotonic case: the same 3K prefix caches on one model
	// (write lands) but not on another (nothing lands) -- the rule's
	// firing tracks the observed cache effect, not the model name, so
	// both outcomes are exercised explicitly here.
	opus5 := Analyze(marked, parse.Usage{Model: "claude-opus-5", CacheWrite5mTokens: 3000}, &store.Event{})
	if hasKind(opus5, KindCachePrefixBelowMinimum) {
		t.Errorf("Opus 5 case (write landed) raised %s, want none: %v", KindCachePrefixBelowMinimum, opus5)
	}
	opus46 := Analyze(marked, parse.Usage{Model: "claude-opus-4-6"}, &store.Event{})
	if !hasKind(opus46, KindCachePrefixBelowMinimum) {
		t.Errorf("Opus 4.6 case (nothing landed) did not raise %s: %v", KindCachePrefixBelowMinimum, opus46)
	}

	// A read of an already-cached prefix is not "nothing happened".
	readOnly := Analyze(marked, parse.Usage{CacheReadTokens: 4000}, &store.Event{})
	if hasKind(readOnly, KindCachePrefixBelowMinimum) {
		t.Errorf("a cache read raised %s, want none: %v", KindCachePrefixBelowMinimum, readOnly)
	}
}

func TestAnalyzeMultiRuleNoDuplicateKinds(t *testing.T) {
	meta := parse.Meta{
		HasCacheControl:   true,
		CacheControlSites: []string{"a", "b", "c", "d", "e"}, // exceeds AND produces no cache effect
	}
	warnings := Analyze(meta, parse.Usage{}, &store.Event{})
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
