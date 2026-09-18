package analyze

import (
	"net/http"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// maxCacheBreakpoints is the API's hard limit: a fifth cache_control
// marker in one request is a 400, not a degraded cache.
const maxCacheBreakpoints = 4

// --- Per-event rules -------------------------------------------------

func ruleCacheBreakpointsExceeded(meta parse.Meta, _ parse.Usage, _ *store.Event) (store.Warning, bool) {
	if len(meta.CacheControlSites) <= maxCacheBreakpoints {
		return store.Warning{}, false
	}
	return warning(KindCacheBreakpointsExceeded, SeverityError,
		"more than 4 cache_control breakpoints in one request"), true
}

// ruleCachePrefixBelowMinimum fires when a cache_control marker was
// present but produced no observable cache effect at all -- no write, no
// read. This is the one T1 rule that genuinely needs the captured
// request body: no token stream alone can reveal that a marker was
// present but under the model's minimum cacheable length, because the
// symptom (cache_creation_input_tokens: 0) leaves no other trace in
// usage. The per-model minimum table (512/1024/2048/4096 tokens,
// non-monotonic across generations) explains *why* this happens; it is
// not consulted here because the API's own response already encodes it
// in whether any cache tokens landed.
func ruleCachePrefixBelowMinimum(meta parse.Meta, usage parse.Usage, _ *store.Event) (store.Warning, bool) {
	if !meta.HasCacheControl {
		return store.Warning{}, false
	}
	if usage.CacheWrite5mTokens > 0 || usage.CacheWrite1hTokens > 0 || usage.CacheReadTokens > 0 {
		return store.Warning{}, false
	}
	return warning(KindCachePrefixBelowMinimum, SeverityWarn,
		"a cache_control breakpoint was present but produced no cache write or read"), true
}

// --- Session-scoped rules ---------------------------------------------

// bigWriteFraction is how much of the previous turn's prompt a cache
// write has to reproduce before it counts as "rewriting the whole
// conversation" rather than incremental growth.
const bigWriteFraction = 0.5

// ruleCachePrefixInvalidation fires when every turn after the first
// re-writes roughly the whole prior conversation into the cache instead
// of reading from it -- a healthy session's writes taper off after the
// first turn as later turns read the growing prefix instead.
func ruleCachePrefixInvalidation(rows []*store.Event) []store.Warning {
	if len(rows) < 3 {
		return nil
	}
	for i := 1; i < len(rows); i++ {
		prevTotal := rows[i-1].TotalPromptTokens
		write := rows[i].CacheWrite5mTokens + rows[i].CacheWrite1hTokens
		if prevTotal == 0 || float64(write) < bigWriteFraction*float64(prevTotal) {
			return nil
		}
	}
	last := rows[len(rows)-1]
	return []store.Warning{withEventID(last.ID, warning(KindCachePrefixInvalidation, SeverityWarn,
		"cache write repeated the full conversation on every turn instead of reading from cache"))}
}

// ruleCacheInvalidatedByTools fires when the tools array changed between
// two consecutive calls in a session and the later call then paid a full
// rewrite -- the effect (a big write) is visible in usage alone; the
// cause (which array changed) needs the captured request bodies.
func ruleCacheInvalidatedByTools(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if len(prev.ReqBody) == 0 || len(cur.ReqBody) == 0 {
			continue
		}
		if prev.TotalPromptTokens == 0 {
			continue
		}
		write := cur.CacheWrite5mTokens + cur.CacheWrite1hTokens
		if float64(write) < bigWriteFraction*float64(prev.TotalPromptTokens) {
			continue
		}
		if toolNamesEqual(prev.ReqBody, cur.ReqBody) {
			continue
		}
		out = append(out, withEventID(cur.ID, warning(KindCacheInvalidatedByTools, SeverityWarn,
			"tools array changed between turns, invalidating the cached prefix from position 0")))
	}
	return out
}

func toolNamesEqual(a, b []byte) bool {
	an := parse.ExtractMeta(a, http.Header{}).ToolNames
	bn := parse.ExtractMeta(b, http.Header{}).ToolNames
	if len(an) != len(bn) {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return false
		}
	}
	return true
}

// ruleCacheWriteNeverRead fires on a write with no subsequent read of any
// prefix within the TTL the write was made at -- the premium was paid
// for nothing.
func ruleCacheWriteNeverRead(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i, r := range rows {
		if r.CacheWrite5mTokens == 0 && r.CacheWrite1hTokens == 0 {
			continue
		}
		ttl := 5 * time.Minute
		if r.CacheWrite1hTokens > 0 {
			ttl = time.Hour
		}
		deadline := r.StartedAt.Add(ttl)
		read := false
		for j := i + 1; j < len(rows) && !rows[j].StartedAt.After(deadline); j++ {
			if rows[j].CacheReadTokens > 0 {
				read = true
				break
			}
		}
		if !read {
			out = append(out, withEventID(r.ID, warning(KindCacheWriteNeverRead, SeverityWarn,
				"cache write recorded with no subsequent read within its TTL")))
		}
	}
	return out
}

// ruleCacheTTLMismatch fires on a 1h write whose first subsequent read
// landed under 5 minutes later -- the cheaper 5m TTL would have covered
// it, so the doubled write premium bought nothing extra.
func ruleCacheTTLMismatch(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i, r := range rows {
		if r.CacheWrite1hTokens == 0 {
			continue
		}
		for j := i + 1; j < len(rows); j++ {
			gap := rows[j].StartedAt.Sub(r.StartedAt)
			if gap > time.Hour {
				break
			}
			if rows[j].CacheReadTokens == 0 {
				continue
			}
			if gap < 5*time.Minute {
				out = append(out, withEventID(r.ID, warning(KindCacheTTLMismatch, SeverityInfo,
					"1h cache write was read again within 5 minutes; the 5-minute TTL would have sufficed")))
			}
			break
		}
	}
	return out
}

// ruleCacheExpiredBetweenTurns fires when a prefix is re-written instead
// of read after its previous write's TTL has already elapsed.
func ruleCacheExpiredBetweenTurns(rows []*store.Event) []store.Warning {
	type writeInfo struct {
		at  time.Time
		ttl time.Duration
	}
	last := map[string]writeInfo{}
	var out []store.Warning
	for _, r := range rows {
		if r.PrefixHash == nil {
			continue
		}
		key := *r.PrefixHash
		wrote := r.CacheWrite5mTokens > 0 || r.CacheWrite1hTokens > 0
		if !wrote {
			continue
		}
		ttl := 5 * time.Minute
		if r.CacheWrite1hTokens > 0 {
			ttl = time.Hour
		}
		if prev, ok := last[key]; ok && r.StartedAt.Sub(prev.at) > prev.ttl {
			out = append(out, withEventID(r.ID, warning(KindCacheExpiredBetweenTurns, SeverityInfo,
				"cached prefix was re-written after its TTL expired instead of being read")))
		}
		last[key] = writeInfo{at: r.StartedAt, ttl: ttl}
	}
	return out
}

// ruleCacheConcurrentWriteRace fires when a call overlaps in time with an
// earlier call sharing the same prefix that was also still writing --
// a cache entry only becomes readable once the first response begins
// streaming, so every overlapping call pays full price.
func ruleCacheConcurrentWriteRace(rows []*store.Event) []store.Warning {
	var out []store.Warning
	for i := range rows {
		if rows[i].PrefixHash == nil || rows[i].CacheReadTokens > 0 {
			continue
		}
		if rows[i].CacheWrite5mTokens == 0 && rows[i].CacheWrite1hTokens == 0 {
			continue
		}
		for j := 0; j < i; j++ {
			if rows[j].PrefixHash == nil || *rows[j].PrefixHash != *rows[i].PrefixHash {
				continue
			}
			if rows[j].CacheReadTokens > 0 {
				continue
			}
			if rows[j].CacheWrite5mTokens == 0 && rows[j].CacheWrite1hTokens == 0 {
				continue
			}
			endJ := rows[j].StartedAt
			if rows[j].EndedAt != nil {
				endJ = *rows[j].EndedAt
			}
			if rows[i].StartedAt.Before(endJ) {
				out = append(out, withEventID(rows[i].ID, warning(KindCacheConcurrentWriteRace, SeverityInfo,
					"overlapped with another call on the same prefix that was also still writing")))
				break
			}
		}
	}
	return out
}

func withEventID(id int64, w store.Warning) store.Warning {
	w.EventID = id
	return w
}
