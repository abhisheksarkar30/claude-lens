// Package jsonlogs is source B's collector: a recursive tailer over
// Claude Code's own JSONL transcripts (~/.claude/projects/**/*.jsonl).
// Unlike internal/proxy, this package is not on the hot path, so it is
// free to depend directly on store, session, analyze, and pricing
// (CLAUDE.md's "proxy depends only on sink+config" restriction names
// internal/proxy specifically, not this collector). It still defines its
// own narrow seam interfaces below, mirroring internal/consumer's pattern,
// so a test can inject a fake without pulling in the real analyzer/pricer.
package jsonlogs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/parse"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Store is the slice of *store.Store the tailer calls.
type Store interface {
	// InsertEvent returns the written row's id and the session that row
	// belongs to -- on a request_id merge, the *existing* row's session,
	// not ev's. The tailer must key its session-scoped work to that id.
	InsertEvent(ctx context.Context, ev *store.Event) (int64, string, error)
	UpsertWarnings(ctx context.Context, eventID int64, warnings []store.Warning) error
	SessionEvents(ctx context.Context, sessionID string) ([]*store.Event, error)
	CursorStore
}

// Analyzer mirrors consumer.Analyzer; internal/analyze.Engine satisfies it
// structurally without this package importing analyze.
type Analyzer interface {
	Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning
}

// SessionRule mirrors consumer.SessionRule.
type SessionRule interface {
	AnalyzeSession(rows []*store.Event) []store.Warning
}

// SessionRecorder is the post-insert half of session.Resolver. Its
// Resolve half is deliberately unused here: a JSONL line already carries
// Claude Code's own session id, so this package only needs the persist
// side -- ensuring the session row exists and re-deriving its totals.
type SessionRecorder interface {
	RecordCall(ctx context.Context, sessionID string, ev *store.Event, warningCount int) error
}

// PriceComputer mirrors consumer.PriceComputer.
type PriceComputer interface {
	Compute(model string, usage parse.Usage, speed, serviceTier string, at time.Time) (usd *float64, costSource string)
}

// peakComputer mirrors pricing.PeakComputer, the way PriceComputer mirrors the
// same seam: an optional refinement a pricer may or may not implement, so the
// assertion is what decides whether a peak_pricing warning is possible at all.
type peakComputer interface {
	PeakAt(model string, at time.Time) bool
}

// peakWarning reports whether ev was billed at a peak rate, as a warning. Two
// conditions gate it: the pricer must implement peakComputer, and the row must
// actually be priced -- an unpriced row was never billed at any rate, so
// claiming it was billed at peak would be a lie (invariant 5).
func peakWarning(pricer PriceComputer, ev *store.Event) (store.Warning, bool) {
	pc, ok := pricer.(peakComputer)
	if !ok || ev.CostSource == "unpriced" || !pc.PeakAt(ev.ModelResolved, ev.StartedAt) {
		return store.Warning{}, false
	}
	return store.Warning{
		Kind:      "peak_pricing",
		Severity:  "warn",
		Detail:    fmt.Sprintf("%s billed at peak; the same call off-peak costs less", ev.ModelResolved),
		CreatedAt: ev.StartedAt,
	}, true
}

// Tailer walks a Claude Code projects directory and ingests every
// distinct request found there through the same cold-path steps a proxy
// row goes through in internal/consumer -- session, cost, analyzer -- via
// a smaller, JSONL-shaped pipeline of its own, since sink.CapturedCall's
// shape (method/path/headers) does not fit a transcript line.
type Tailer struct {
	root string
	st   Store

	recorder    SessionRecorder
	analyzers   []Analyzer
	sessionRule SessionRule
	pricer      PriceComputer

	// account/billingMode are the default for every row this tailer writes.
	// ponytail: a JSONL line carries no auth signal to disambiguate
	// between multiple configured accounts, so this is one value for the
	// whole tailer rather than a per-row resolution; ceiling: a multi-
	// account setup with more than one subscription account can't tell
	// their JSONL history apart -- revisit if that becomes real.
	//
	// The api* fields below are the one exception, and they do not reopen that
	// ceiling: a model prefix is a signal the line *does* carry, so a
	// prefix-matched row resolves per row rather than taking the default.
	account     string
	billingMode string

	apiPrefixes    []string
	apiAccount     string
	apiBillingMode string
}

// New returns a Tailer walking root, writing to st. Every optional seam
// (analyzers, session recorder, price table) starts unset, and the
// account defaults to billing_mode "subscription" with no account name --
// Claude Code's own transcripts are written by the subscription client in
// the common case.
func New(root string, st Store) *Tailer {
	return &Tailer{root: root, st: st, billingMode: "subscription"}
}

func (t *Tailer) SetSessionRecorder(r SessionRecorder) { t.recorder = r }
func (t *Tailer) SetAnalyzers(analyzers ...Analyzer)   { t.analyzers = analyzers }
func (t *Tailer) SetSessionRule(r SessionRule)         { t.sessionRule = r }
func (t *Tailer) SetPriceTable(p PriceComputer)        { t.pricer = p }

// SetAccount fixes the account/billing_mode every row from this tailer
// gets. It assigns unconditionally, so a caller that passes a zero Account
// blanks the billing mode New seeded.
func (t *Tailer) SetAccount(name, billingMode string) {
	t.account = name
	t.billingMode = billingMode
}

// Account reports the account and billing mode every row from this tailer
// gets, unless a model prefix routes it otherwise.
func (t *Tailer) Account() (name, billingMode string) {
	return t.account, t.billingMode
}

// SetModelBilling lists the model prefixes that bill pay-as-you-go, and the
// account/billing mode such a model's rows get. A prefix list consumed only
// behind a tailer.
func (t *Tailer) SetModelBilling(prefixes []string, apiAccount, apiBillingMode string) {
	t.apiPrefixes = prefixes
	t.apiAccount = apiAccount
	t.apiBillingMode = apiBillingMode
}

// ModelBilling reports the prefixes that route a row to the api account, and
// the account/billing mode such a row gets. prefixes is a copy, so a caller
// cannot mutate the tailer's own slice -- the same rule AllKinds() follows.
func (t *Tailer) ModelBilling() (prefixes []string, account, billingMode string) {
	return slices.Clone(t.apiPrefixes), t.apiAccount, t.apiBillingMode
}

// Stats reports one Poll's work, for logging and tests.
type Stats struct {
	FilesWalked   int
	LinesRead     int
	RequestsFound int
	Unknown       int
	Malformed     int
	Inserted      int
	Failed        int
}

func (s *Stats) add(o Stats) {
	s.FilesWalked += o.FilesWalked
	s.LinesRead += o.LinesRead
	s.RequestsFound += o.RequestsFound
	s.Unknown += o.Unknown
	s.Malformed += o.Malformed
	s.Inserted += o.Inserted
	s.Failed += o.Failed
}

// Poll walks root once, tails every *.jsonl file it finds from its stored
// cursor, and ingests every distinct request. A single bad file or line
// never stops the walk (CLAUDE.md invariant 6, fail open): it is logged
// and counted, and the tail continues.
func (t *Tailer) Poll(ctx context.Context) (Stats, error) {
	var total Stats
	files, err := walkFiles(t.root)
	if err != nil {
		return total, fmt.Errorf("jsonlogs: walk %s: %w", t.root, err)
	}
	total.FilesWalked = len(files)
	for _, f := range files {
		total.add(t.tailFile(ctx, f))
	}
	return total, nil
}

// walkedFile is one discovered transcript, with its subagent attribution
// already resolved from its path.
type walkedFile struct {
	Path            string
	ParentSessionID string
	IsSidechain     bool
}

// walkFiles recursively finds every *.jsonl file under root, top-level
// session transcripts and …/<sessionId>/subagents/agent-*.jsonl sidechains
// alike (test 22). An unreadable directory entry is skipped, not fatal.
func walkFiles(root string) ([]walkedFile, error) {
	var out []walkedFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		parent, isSidechain := subagentParent(path)
		out = append(out, walkedFile{Path: path, ParentSessionID: parent, IsSidechain: isSidechain})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// subagentParent reports whether path is a
// …/<sessionId>/subagents/agent-*.jsonl sidechain, and if so, the
// sessionId directory that owns it -- the attribution test 22 checks.
func subagentParent(path string) (parentSessionID string, isSidechain bool) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, p := range parts {
		if p == "subagents" && i > 0 {
			return parts[i-1], true
		}
	}
	return "", false
}

// tailFile reads f from its stored cursor to the file's current end,
// ingests every distinct request found, and advances the cursor by
// however many bytes of complete lines it consumed.
func (t *Tailer) tailFile(ctx context.Context, f walkedFile) Stats {
	var stats Stats

	offset, err := loadCursor(ctx, t.st, f.Path)
	if err != nil {
		log.Printf("jsonlogs: load cursor %s: %v", f.Path, err)
	}

	fh, err := os.Open(f.Path)
	if err != nil {
		log.Printf("jsonlogs: open %s: %v", f.Path, err)
		return stats
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		log.Printf("jsonlogs: stat %s: %v", f.Path, err)
		return stats
	}
	// Truncation or rotation: the file is now smaller than the recorded
	// cursor, so the offset can no longer be valid -- restart from 0. The
	// request_id UNIQUE constraint absorbs the resulting re-read as a
	// merge, never a duplicate row (test 10).
	if info.Size() < offset {
		offset = 0
	}

	if _, err := fh.Seek(offset, io.SeekStart); err != nil {
		log.Printf("jsonlogs: seek %s: %v", f.Path, err)
		return stats
	}

	raw, err := io.ReadAll(fh)
	if err != nil {
		log.Printf("jsonlogs: read %s: %v", f.Path, err)
		return stats
	}

	consumed, rawLines := splitCompleteLines(raw)

	var assistantLines []*line
	for _, lb := range rawLines {
		if len(bytes.TrimSpace(lb)) == 0 {
			continue
		}
		stats.LinesRead++
		l, err := parseLine(lb)
		if err != nil {
			stats.Malformed++
			continue
		}
		if l.Type == "assistant" {
			assistantLines = append(assistantLines, l)
			continue
		}
		if !toleratedTypes[l.Type] {
			stats.Unknown++
		}
	}

	distinct := dedupeAssistantLines(assistantLines)
	stats.RequestsFound = len(distinct)

	for _, l := range distinct {
		ev, meta, usage := t.buildEvent(l, f)
		if t.insert(ctx, ev, meta, usage) {
			stats.Inserted++
		} else {
			stats.Failed++
		}
	}

	newOffset := offset + int64(consumed)
	if err := saveCursor(ctx, t.st, f.Path, newOffset, "ok", ""); err != nil {
		log.Printf("jsonlogs: %v", err)
	}
	return stats
}

// splitCompleteLines splits raw into complete newline-terminated lines
// and reports how many bytes they consumed. A trailing partial line (no
// terminating \n yet -- Claude Code mid-write) is left for the next poll
// rather than parsed early, which is what keeps a line split across a
// read boundary from ever reaching json.Unmarshal half-written. Splitting
// only at '\n' is UTF-8 safe: a continuation byte of a multi-byte rune is
// never mistaken for it.
func splitCompleteLines(raw []byte) (consumed int, lines [][]byte) {
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	return start, lines
}

// buildEvent turns one distinct assistant line into a store.Event plus
// the parse.Meta/parse.Usage the analyzer seam needs.
func (t *Tailer) buildEvent(l *line, f walkedFile) (*store.Event, parse.Meta, parse.Usage) {
	sessionID := f.ParentSessionID
	if sessionID == "" {
		sessionID = l.SessionID
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(f.Path), ".jsonl")
	}

	usage := usageFromShape(l.Message.Usage)
	usage.Model = l.Message.Model
	usage.StopReason = l.Message.StopReason

	meta := parse.Meta{
		ClientVersion: l.Version,
		Project:       l.CWD,
		GitBranch:     l.GitBranch,
		IsSidechain:   f.IsSidechain,
		CliEntrypoint: l.CliEntrypoint,
	}

	// Account and billing mode resolve per row, not once for the tailer: one
	// transcript tree holds both Claude and DeepSeek traffic, and a model whose
	// prefix is configured pay-as-you-go must land in the api account with a
	// real cost rather than the hypothetical column.
	account, billingMode := t.account, t.billingMode
	for _, p := range t.apiPrefixes {
		if strings.HasPrefix(usage.Model, p) {
			account, billingMode = t.apiAccount, t.apiBillingMode
			break
		}
	}

	startedAt := parseTimestamp(l.Timestamp)
	ev := &store.Event{EventSummary: store.EventSummary{
		RequestID:          requestKey(l),
		Source:             "jsonl",
		FirstSource:        "jsonl",
		StartedAt:          startedAt,
		Account:            account,
		BillingMode:        billingMode,
		ModelRequested:     usage.Model,
		ModelResolved:      usage.Model,
		InputTokens:        usage.InputTokens,
		OutputTokens:       usage.OutputTokens,
		CacheWrite5mTokens: usage.CacheWrite5mTokens,
		CacheWrite1hTokens: usage.CacheWrite1hTokens,
		CacheReadTokens:    usage.CacheReadTokens,
		ThinkingTokens:     usage.ThinkingTokens,
		StopReason:         usage.StopReason,
		IsSidechain:        f.IsSidechain,
		SessionID:          sessionID,
		Project:            l.CWD,
		GitBranch:          l.GitBranch,
		ClientVersion:      l.Version,
		CliEntrypoint:      l.CliEntrypoint,
		CaptureComplete:    true,
	}}

	if t.pricer != nil {
		usd, costSource := t.pricer.Compute(ev.ModelResolved, usage, "", "", ev.StartedAt)
		ev.CostSource = costSource
		switch ev.BillingMode {
		case "subscription":
			ev.ApiEquivalentCostUSD = usd
		default:
			ev.CostUSD = usd
		}
	}

	return ev, meta, usage
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	return time.Now()
}

// insert writes ev, runs the analyzer seam, and folds the result into
// ev's session. It never returns an error: a per-request failure is
// logged and counted by the caller, the same fail-open discipline
// internal/consumer uses.
func (t *Tailer) insert(ctx context.Context, ev *store.Event, meta parse.Meta, usage parse.Usage) bool {
	id, sessionID, err := t.st.InsertEvent(ctx, ev)
	if err != nil {
		log.Printf("jsonlogs: insert event %s: %v", ev.RequestID, err)
		return false
	}

	var warnings []store.Warning
	for _, a := range t.analyzers {
		warnings = append(warnings, t.runAnalyzer(a, meta, usage, ev)...)
	}
	// Attached here rather than in buildEvent, which returns no warnings slice
	// to append to.
	if w, ok := peakWarning(t.pricer, ev); ok {
		warnings = append(warnings, w)
	}
	if len(warnings) > 0 {
		if err := t.st.UpsertWarnings(ctx, id, warnings); err != nil {
			log.Printf("jsonlogs: upsert warnings %d: %v", id, err)
		}
	}

	// sessionID is the written row's session, which after a cross-source
	// merge is the first-written row's, not ev's -- see consumer.Store.
	warningCount := len(warnings)
	if t.sessionRule != nil && sessionID != "" {
		warningCount += t.runSessionRule(ctx, sessionID)
	}

	if t.recorder != nil && sessionID != "" {
		if err := t.recorder.RecordCall(ctx, sessionID, ev, warningCount); err != nil {
			log.Printf("jsonlogs: record session call %s: %v", sessionID, err)
		}
	}
	return true
}

func (t *Tailer) runAnalyzer(a Analyzer, meta parse.Meta, usage parse.Usage, ev *store.Event) (warnings []store.Warning) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("jsonlogs: analyzer %T panicked: %v", a, r)
			warnings = []store.Warning{{
				Kind:      "analyzer_panic",
				Severity:  "error",
				Detail:    fmt.Sprintf("%T: %v", a, r),
				CreatedAt: time.Now(),
			}}
		}
	}()
	return a.Analyze(meta, usage, ev)
}

func (t *Tailer) runSessionRule(ctx context.Context, sessionID string) int {
	rows, err := t.st.SessionEvents(ctx, sessionID)
	if err != nil {
		log.Printf("jsonlogs: session rows %s: %v", sessionID, err)
		return 0
	}
	grouped := map[int64][]store.Warning{}
	for _, w := range t.sessionRule.AnalyzeSession(rows) {
		if w.EventID == 0 {
			continue
		}
		grouped[w.EventID] = append(grouped[w.EventID], w)
	}
	n := 0
	for eventID, warnings := range grouped {
		if err := t.st.UpsertWarnings(ctx, eventID, warnings); err != nil {
			log.Printf("jsonlogs: upsert session warnings %d: %v", eventID, err)
			continue
		}
		n += len(warnings)
	}
	return n
}
