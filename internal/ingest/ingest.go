// Package ingest orchestrates the three non-proxy collectors -- the JSONL
// tail (internal/jsonlogs), the quota snapshot poll (internal/snapshot),
// and the Admin usage/cost pull (internal/adminrep) -- behind one RunOnce
// entry point, and records each one's outcome so a broken collector is a
// visible red row instead of a quietly short chart (CLAUDE.md invariant 6,
// extended: a broken collector never prevents the others from writing and
// never crashes the process -- enforced here, not just assumed).
//
// The proxy is deliberately not one of RunOnce's collectors: it runs
// continuously in its own goroutine (internal/proxy + internal/consumer),
// outside any poll cycle, so it has no run outcome to record here. Its own
// health signal lives in internal/cli/doctor.go.
package ingest

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Collector is one non-proxy source RunOnce drives. jsonlogs.Tailer.Poll,
// snapshot.Poller.Poll, and adminrep.Collector.CollectAll each adapt to
// this shape with a thin wrapper at the call site (cmd/clens's serve
// wiring) that folds their own (Stats/Result, error) return into a rows
// count plus a single error -- a non-ok fail-soft Status becomes an error
// here, since isolating a failure is this package's job, not theirs.
type Collector interface {
	Run(ctx context.Context) (rows int, err error)
}

// CollectorFunc adapts a plain function to Collector.
type CollectorFunc func(ctx context.Context) (int, error)

func (f CollectorFunc) Run(ctx context.Context) (int, error) { return f(ctx) }

// Source names one of the three collectors RunOnce drives.
type Source string

const (
	SourceJSONL    Source = "jsonl"
	SourceSnapshot Source = "snapshot"
	SourceAdmin    Source = "admin"
)

// Store is the ingest_state slice RunOnce and SourcesHealth need.
type Store interface {
	GetIngestState(ctx context.Context, key string) (store.IngestState, bool, error)
	SetIngestState(ctx context.Context, key string, state store.IngestState) error
}

// entry pairs a registered collector with the source bucket its outcome
// rolls up into. cursorKey, if set, names the collector's own resume
// cursor (e.g. jsonlogs' "jsonl:<root>") so the health surface can display
// it -- Runner never writes to it, only reads it back for display.
type entry struct {
	source    Source
	name      string
	cursorKey string
	c         Collector
}

// Runner holds the registered collectors and drives RunOnce and the
// Scheduler.
type Runner struct {
	st      Store
	entries []entry
}

// New returns a Runner recording outcomes through st.
func New(st Store) *Runner { return &Runner{st: st} }

// Add registers a collector under source, keyed by name (unique per
// source -- e.g. an account name, or "usage"/"cost" for two adminrep
// pulls). cursorKey is optional and only affects what SourcesHealth
// displays as CursorPosition.
func (r *Runner) Add(source Source, name, cursorKey string, c Collector) {
	r.entries = append(r.entries, entry{source: source, name: name, cursorKey: cursorKey, c: c})
}

// Health keys are per-source, not per-collector-name: the tech plan's
// promise is a four-bucket (proxy, jsonl, snapshot, admin) health surface,
// and keying per-source rather than per-account/per-name lets
// internal/cli/doctor read it without knowing what accounts are
// configured -- it can call SourcesHealth on a bare Runner with no
// registered collectors at all. ponytail: two collectors of the same
// source (e.g. two snapshot accounts) overwrite each other's health
// record within one RunOnce pass rather than rolling up separately --
// acceptable while every wired source is single-account; give each
// account its own key if multi-account polling is ever wired in.
func successKey(source Source) string { return fmt.Sprintf("health:%s:success", source) }
func errorKey(source Source) string   { return fmt.Sprintf("health:%s:error", source) }

// Outcome is one collector's RunOnce result.
type Outcome struct {
	Source Source
	Name   string
	Rows   int
	Status string // "ok" | "error"
	Error  string
}

// RunOnce runs every registered collector once, in registration order,
// isolating panics and errors: a collector that panics or returns an
// error is recorded as that source's failure, and every other collector
// still runs (invariant 6). The returned slice always has one Outcome per
// registered collector, in the same order they were added.
func (r *Runner) RunOnce(ctx context.Context) []Outcome {
	out := make([]Outcome, len(r.entries))
	for i, e := range r.entries {
		out[i] = r.runOne(ctx, e)
	}
	return out
}

func (r *Runner) runOne(ctx context.Context, e entry) (out Outcome) {
	out = Outcome{Source: e.source, Name: e.name}
	defer func() {
		if p := recover(); p != nil {
			out.Status = "error"
			out.Rows = 0
			out.Error = fmt.Sprintf("panic: %v", p)
		}
		r.record(ctx, e, out)
	}()

	rows, err := e.c.Run(ctx)
	out.Rows = rows
	if err != nil {
		out.Status = "error"
		out.Error = err.Error()
		return out
	}
	out.Status = "ok"
	return out
}

// record persists out to the source/name's own health key -- the success
// key on an "ok" outcome, the error key otherwise -- so a later
// SourcesHealth call can read back when each last happened.
func (r *Runner) record(ctx context.Context, e entry, out Outcome) {
	if out.Status == "ok" {
		_ = r.st.SetIngestState(ctx, successKey(e.source), store.IngestState{
			Value: strconv.Itoa(out.Rows), Status: "ok",
		})
		return
	}
	_ = r.st.SetIngestState(ctx, errorKey(e.source), store.IngestState{
		Status: "error", Error: out.Error,
	})
}

// Health is one source's aggregated status across every collector
// registered for it (e.g. two snapshot accounts).
type Health struct {
	Source         Source
	Status         string // "ok" | "error" | "unknown" (never run)
	LastSuccessAt  time.Time
	LastErrorAt    time.Time
	LastError      string
	RowsWritten    int
	CursorPosition string
}

// SourcesHealth reports Health for jsonl, snapshot, and admin, in that
// order -- the three sources RunOnce drives. It reads only the fixed
// per-source health keys, so it works on a bare Runner with no registered
// collectors at all (internal/cli/doctor's use case: it knows nothing
// about configured accounts, only that these three sources exist).
// CursorPosition is filled in only when a caller happens to have
// registered a collector for that source with a cursorKey (the serve
// process, once wired, br-GI-1-17) -- otherwise it stays empty.
func (r *Runner) SourcesHealth(ctx context.Context) ([]Health, error) {
	cursorKeys := map[Source]string{}
	for _, e := range r.entries {
		if e.cursorKey != "" {
			cursorKeys[e.source] = e.cursorKey
		}
	}

	var out []Health
	for _, source := range []Source{SourceJSONL, SourceSnapshot, SourceAdmin} {
		h := Health{Source: source, Status: "unknown"}

		sh, ok, err := r.st.GetIngestState(ctx, successKey(source))
		if err != nil {
			return nil, err
		}
		if ok {
			h.RowsWritten, _ = strconv.Atoi(sh.Value)
			h.LastSuccessAt = sh.UpdatedAt
		}

		eh, ok, err := r.st.GetIngestState(ctx, errorKey(source))
		if err != nil {
			return nil, err
		}
		if ok {
			h.LastErrorAt = eh.UpdatedAt
			h.LastError = eh.Error
		}

		if key, ok := cursorKeys[source]; ok {
			if cs, found, err := r.st.GetIngestState(ctx, key); err == nil && found {
				h.CursorPosition = cs.Value
			}
		}

		switch {
		case h.LastSuccessAt.IsZero() && h.LastErrorAt.IsZero():
			h.Status = "unknown"
		case h.LastErrorAt.After(h.LastSuccessAt):
			h.Status = "error"
		default:
			h.Status = "ok"
		}
		out = append(out, h)
	}
	return out, nil
}
