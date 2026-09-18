package analyze

import (
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }

func endedAt(t time.Time) *time.Time { return &t }

func warningKinds(warnings []store.Warning) map[string][]int64 {
	out := map[string][]int64{}
	for _, w := range warnings {
		out[w.Kind] = append(out[w.Kind], w.EventID)
	}
	return out
}

func reqBodyWithTools(names ...string) []byte {
	toolsJSON := ""
	for i, n := range names {
		if i > 0 {
			toolsJSON += ","
		}
		toolsJSON += `{"name":"` + n + `"}`
	}
	return []byte(`{"model":"claude-sonnet-5","tools":[` + toolsJSON + `],"messages":[]}`)
}

// --- cache_prefix_invalidation ------------------------------------------

func TestRuleCachePrefixInvalidationFiresOnInvalidatorLoop(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000},
		{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheWrite5mTokens: 4900},
		{ID: 3, StartedAt: at(2), TotalPromptTokens: 5200, CacheWrite5mTokens: 5000},
	}
	found := warningKinds(ruleCachePrefixInvalidation(rows))
	ids := found[string(KindCachePrefixInvalidation)]
	if len(ids) != 1 || ids[0] != 3 {
		t.Errorf("invalidator loop findings = %v, want exactly [3]", ids)
	}
}

func TestRuleCachePrefixInvalidationSilentOnHealthyLoop(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000, CacheWrite5mTokens: 5000},
		{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheReadTokens: 5000},
		{ID: 3, StartedAt: at(2), TotalPromptTokens: 5200, CacheReadTokens: 5100},
	}
	if got := ruleCachePrefixInvalidation(rows); got != nil {
		t.Errorf("healthy loop raised %v, want nil", got)
	}
}

// --- cache_invalidated_by_tools ------------------------------------------

func TestRuleCacheInvalidatedByToolsFiresOnChangedArray(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000, ReqBody: reqBodyWithTools("bash", "read")},
		{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheWrite5mTokens: 4900, ReqBody: reqBodyWithTools("bash", "read", "edit")},
	}
	found := warningKinds(ruleCacheInvalidatedByTools(rows))
	if ids := found[string(KindCacheInvalidatedByTools)]; len(ids) != 1 || ids[0] != 2 {
		t.Errorf("changed tools findings = %v, want exactly [2]", ids)
	}
}

func TestRuleCacheInvalidatedByToolsSilentOnIdenticalArray(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), TotalPromptTokens: 5000, ReqBody: reqBodyWithTools("bash", "read")},
		{ID: 2, StartedAt: at(1), TotalPromptTokens: 5100, CacheWrite5mTokens: 4900, ReqBody: reqBodyWithTools("bash", "read")},
	}
	if got := ruleCacheInvalidatedByTools(rows); got != nil {
		t.Errorf("identical tools raised %v, want nil", got)
	}
}

// --- cache_write_never_read ------------------------------------------

func TestRuleCacheWriteNeverReadFiresWithNoLaterRead(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), CacheWrite5mTokens: 1000},
		{ID: 2, StartedAt: at(1)}, // within TTL, no read
	}
	found := warningKinds(ruleCacheWriteNeverRead(rows))
	if ids := found[string(KindCacheWriteNeverRead)]; len(ids) != 1 || ids[0] != 1 {
		t.Errorf("never-read findings = %v, want exactly [1]", ids)
	}
}

func TestRuleCacheWriteNeverReadSilentWhenReadWithinTTL(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), CacheWrite5mTokens: 1000},
		{ID: 2, StartedAt: at(1), CacheReadTokens: 900},
	}
	if got := ruleCacheWriteNeverRead(rows); got != nil {
		t.Errorf("read within TTL raised %v, want nil", got)
	}
}

// --- cache_ttl_mismatch ------------------------------------------

func TestRuleCacheTTLMismatchFiresOnEarlyReread(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), CacheWrite1hTokens: 1000},
		{ID: 2, StartedAt: at(2), CacheReadTokens: 900}, // 2 min later, under 5m
	}
	found := warningKinds(ruleCacheTTLMismatch(rows))
	if ids := found[string(KindCacheTTLMismatch)]; len(ids) != 1 || ids[0] != 1 {
		t.Errorf("early re-read findings = %v, want exactly [1]", ids)
	}
}

func TestRuleCacheTTLMismatchSilentWhenReadOutsideFiveMinutes(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), CacheWrite1hTokens: 1000},
		{ID: 2, StartedAt: at(30), CacheReadTokens: 900}, // 30 min later, justifies the 1h TTL
	}
	if got := ruleCacheTTLMismatch(rows); got != nil {
		t.Errorf("read outside 5 minutes raised %v, want nil", got)
	}
}

// --- cache_expired_between_turns ------------------------------------------

func TestRuleCacheExpiredBetweenTurnsFiresOnStaleRewrite(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), PrefixHash: &hash, CacheWrite5mTokens: 1000},
		{ID: 2, StartedAt: at(10), PrefixHash: &hash, CacheWrite5mTokens: 1000}, // 10 min later, past the 5m TTL
	}
	found := warningKinds(ruleCacheExpiredBetweenTurns(rows))
	if ids := found[string(KindCacheExpiredBetweenTurns)]; len(ids) != 1 || ids[0] != 2 {
		t.Errorf("stale re-write findings = %v, want exactly [2]", ids)
	}
}

func TestRuleCacheExpiredBetweenTurnsSilentWhenRewriteWithinTTL(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), PrefixHash: &hash, CacheWrite5mTokens: 1000},
		{ID: 2, StartedAt: at(2), PrefixHash: &hash, CacheWrite5mTokens: 1000}, // still within the 5m TTL
	}
	if got := ruleCacheExpiredBetweenTurns(rows); got != nil {
		t.Errorf("re-write within TTL raised %v, want nil", got)
	}
}

// --- cache_concurrent_write_race ------------------------------------------

func TestRuleCacheConcurrentWriteRaceFiresOnOverlap(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), EndedAt: endedAt(at(2)), PrefixHash: &hash, CacheWrite5mTokens: 1000},
		{ID: 2, StartedAt: at(1), PrefixHash: &hash, CacheWrite5mTokens: 1000}, // starts before call 1 finished
	}
	found := warningKinds(ruleCacheConcurrentWriteRace(rows))
	if ids := found[string(KindCacheConcurrentWriteRace)]; len(ids) != 1 || ids[0] != 2 {
		t.Errorf("overlap findings = %v, want exactly [2]", ids)
	}
}

func TestRuleCacheConcurrentWriteRaceSilentWhenSequential(t *testing.T) {
	hash := "abc"
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), EndedAt: endedAt(at(1)), PrefixHash: &hash, CacheWrite5mTokens: 1000},
		{ID: 2, StartedAt: at(2), PrefixHash: &hash, CacheReadTokens: 900}, // starts after call 1 ended
	}
	if got := ruleCacheConcurrentWriteRace(rows); got != nil {
		t.Errorf("sequential calls raised %v, want nil", got)
	}
}

// --- AnalyzeSession wiring ------------------------------------------

func TestAnalyzeSessionRunsEveryRule(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0), CacheWrite1hTokens: 1000},
		{ID: 2, StartedAt: at(1)}, // never read: fires cache_write_never_read
	}
	warnings := AnalyzeSession(rows)
	if !hasKind(warnings, KindCacheWriteNeverRead) {
		t.Errorf("AnalyzeSession did not run the cache_write_never_read rule: %v", warnings)
	}
}

func TestAnalyzeSessionCleanSessionReturnsNil(t *testing.T) {
	rows := []*store.Event{
		{ID: 1, StartedAt: at(0)},
		{ID: 2, StartedAt: at(1)},
	}
	if got := AnalyzeSession(rows); got != nil {
		t.Errorf("clean session raised %v, want nil", got)
	}
}
