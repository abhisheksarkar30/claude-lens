// Package analyze is the T1 rule engine: pure functions over one call's
// metadata/usage (Analyze) or one session's row history (AnalyzeSession),
// with no I/O, no store writes, and no config mutation, so the whole rule
// matrix is testable from synthetic values alone. It imports parse,
// store, config, and pricing -- never consumer, so the direction
// consumer.Analyzer documents (analyze satisfies it structurally) always
// holds.
package analyze

import (
	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// perEventRules is the per-event rule table: one entry per kind, never a
// pipeline change to add another.
var perEventRules = []func(parse.Meta, parse.Usage, *store.Event) (store.Warning, bool){
	ruleCacheBreakpointsExceeded,
	ruleCachePrefixBelowMinimum,
}

// sessionRules is the session-scoped rule table, run over one session's
// full row history (oldest first).
var sessionRules = []func([]*store.Event) []store.Warning{
	ruleCachePrefixInvalidation,
	ruleCacheInvalidatedByTools,
	ruleCacheWriteNeverRead,
	ruleCacheTTLMismatch,
	ruleCacheExpiredBetweenTurns,
	ruleCacheConcurrentWriteRace,
}

// Analyze runs every per-event T1 rule against one call and returns its
// findings, or nil -- never an empty slice -- when nothing fires.
func Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning {
	var warnings []store.Warning
	for _, rule := range perEventRules {
		if w, fired := rule(meta, usage, ev); fired {
			warnings = append(warnings, w)
		}
	}
	return warnings
}

// AnalyzeSession runs every session-scoped T1 rule over rows (oldest
// first) and returns findings. Each returned store.Warning.EventID names
// the row whose pattern it completed -- not necessarily rows' last entry
// -- so a caller attaches it there, not to whichever call triggered this
// pass.
func AnalyzeSession(rows []*store.Event) []store.Warning {
	var warnings []store.Warning
	for _, rule := range sessionRules {
		warnings = append(warnings, rule(rows)...)
	}
	return warnings
}

// Engine adapts the two pure entry points above to consumer's Analyzer
// and SessionRule interfaces without consumer importing this package --
// the composition root instantiates Engine{} and hands it to consumer's
// setters, exactly as consumer.Analyzer's own doc comment describes.
type Engine struct{}

func (Engine) Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning {
	return Analyze(meta, usage, ev)
}

func (Engine) AnalyzeSession(rows []*store.Event) []store.Warning {
	return AnalyzeSession(rows)
}

func warning(kind Kind, sev Severity, detail string) store.Warning {
	return store.Warning{Kind: string(kind), Severity: string(sev), Detail: detail}
}
