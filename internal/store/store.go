// Package store is the one SQLite file claude-lens writes to: schema,
// single ingest writer, cross-source merge, and every read path the
// dashboard and CLI use. See CLAUDE.md's "Architecture essentials" for the
// invariants this package exists to hold: the derived prompt total
// (invariant 4), the two-column billing-mode cost split (invariant 5), and
// the single-writer-per-source merge discipline (invariant 3).
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/proxy"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// DefaultLimit is the page size ListEvents, ListWarnings, and ListSessions
// apply when the caller passes Limit <= 0 -- never unbounded. The API layer
// echoes this same value in the X-Limit pagination header when ?limit is
// absent, so the header always agrees with the rows actually returned.
const DefaultLimit = 100

// RedactCheck is the store's copy of the startup self-test: it scans a
// captured call's stored header JSON for a reachable secret that redaction
// should already have removed. internal/proxy owns the check (it also runs
// it before anything reaches this package); the store re-exposes it here so
// a caller auditing what is actually on disk does not need to import proxy
// itself.
var RedactCheck = proxy.RedactCheck

// Store is the single connection to the claude-lens SQLite file. SQLite
// tolerates only one writer at a time; SetMaxOpenConns(1) below is what
// turns "the schema says single ingest writer" into a guarantee every
// caller gets for free, including the purge writer sharing this same
// connection.
type Store struct {
	db *sql.DB
}

// Open creates (or opens) the SQLite file at dbPath, enables WAL and
// foreign keys, and creates the schema. The returned Store serializes every
// writer onto one connection. dbPath's parent directory is created if
// missing -- the default path is a ~/.clens subdirectory that nothing else
// is guaranteed to have created yet on a completely fresh install (a
// command that never touches internal/secret, e.g. `clens doctor` on its
// first-ever run, would otherwise be the first thing to fail).
func Open(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)",
		dbPath,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)

	// Order matters, and this is the part that is easy to get wrong. The probe
	// decides only whether to stamp; the stamp goes down *before* the exec.
	//
	// A fresh file gets the current version first, then the schema. The exec is
	// not atomic -- it is a sequence of statements -- so it can die part-way
	// through. Stamped first, the version is already current when the process
	// dies, so the next boot's runner applies nothing and the always-run exec
	// repairs the missing tables. Seeded *after* a successful exec instead, the
	// version would still be 0 at a mid-file crash; the next boot would see
	// `events` present (so not fresh, so no stamp), heal the missing tables,
	// then read 0 and apply migration 1 -- ALTER TABLE ADD COLUMN against a
	// table schema.sql had just created with that column. That is a
	// "duplicate column name" error out of Open on every boot thereafter, with
	// no recovery but deleting the database.
	//
	// Stamping early is safe the other way too: if the stamp landed but the
	// first CREATE TABLE did not, the next boot's probe still reads `events`
	// absent, so it re-seeds and re-runs the exec.
	fresh, err := eventsTableAbsent(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if fresh {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: stamp a fresh schema: %w", err)
		}
	}

	// Unconditional, every open. Against an existing database it is a no-op --
	// every statement is IF NOT EXISTS -- and against a partial one it is the
	// repair. Skipping it when `events` exists would strand every other missing
	// table behind "no such table" on every boot.
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: create schema: %w", err)
	}

	// Before Open returns, so no caller can observe a database that has the
	// schema but not its migrations.
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// schemaVersion is the current PRAGMA user_version. It must equal
// len(migrations): migrations[n] upgrades version n to n+1.
const schemaVersion = 1

// migrations holds one entry per schema change, oldest first. Each runs in its
// own transaction with the version bump inside it, so a failure part-way leaves
// the version where it was and the next Open retries rather than skipping.
//
// This is the repo's first migration mechanism. It exists because schema.sql is
// CREATE TABLE IF NOT EXISTS throughout and can never add a column to a
// database that already exists -- GI-1 recorded "Migrations: none in v1" as a
// decision, not an omission, and this is the first change to need one.
var migrations = []string{
	// 0 -> 1: transcript content gets its own columns rather than sharing the
	// wire-capture ones. Nullable and additive, so there is no table rewrite.
	`ALTER TABLE events ADD COLUMN transcript_content BLOB;
	 ALTER TABLE events ADD COLUMN transcript_role TEXT;`,
}

// eventsTableAbsent reports whether this database has no events table yet,
// which is what distinguishes a fresh file from an existing one.
func eventsTableAbsent(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: probe for the events table: %w", err)
	}
	return n == 0, nil
}

// migrate brings db up to schemaVersion.
func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("store: database is at schema version %d but this binary knows %d: "+
			"it was written by a newer clens", version, schemaVersion)
	}
	for v := version; v < schemaVersion; v++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("store: migrate %d->%d: %w", v, v+1, err)
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migrate %d->%d: %w", v, v+1, err)
		}
		// Inside the transaction: a version that moved without its migration
		// landing would strand the change forever, and a migration that landed
		// without the version moving would fail on the next boot.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: stamp %d->%d: %w", v, v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migrate %d->%d: %w", v, v+1, err)
		}
	}
	return nil
}

// Close closes the underlying connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeFromNano(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func joinList(vs []string) string {
	return strings.Join(vs, ",")
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// InsertEvent inserts ev, computing TotalPromptTokens as the store rather
// than trusting the caller (invariant 4). A request_id collision merges
// into the existing row instead of duplicating it (invariant 3): whichever
// side is capture_complete wins the token/cost columns, source_refs gains
// the new source, and the owning session's totals are re-derived in the
// same transaction. It returns the row's id and the session that row
// belongs to — on a merge the *existing* row's session, since a merge never
// rewrites session_id.
func (s *Store) InsertEvent(ctx context.Context, ev *Event) (id int64, sessionID string, err error) {
	ev.TotalPromptTokens = ev.InputTokens + ev.CacheWrite5mTokens + ev.CacheWrite1hTokens + ev.CacheReadTokens

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", fmt.Errorf("store: InsertEvent: begin: %w", err)
	}
	defer tx.Rollback()

	var merged bool
	id, sessionID, merged, err = insertOrMerge(ctx, tx, ev)
	if err != nil {
		return 0, "", fmt.Errorf("store: InsertEvent: %w", err)
	}
	if merged && sessionID != "" {
		if err := reconcileSessionTx(ctx, tx, sessionID); err != nil {
			return 0, "", fmt.Errorf("store: InsertEvent: reconcile session: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, "", fmt.Errorf("store: InsertEvent: commit: %w", err)
	}
	return id, sessionID, nil
}

// InsertEvents inserts every event in one transaction, for the consumer's
// batched flush. It stops and returns the first error.
func (s *Store) InsertEvents(ctx context.Context, evs []*Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: InsertEvents: begin: %w", err)
	}
	defer tx.Rollback()

	// Keyed by the surviving row's session, not the incoming event's: see
	// insertOrMerge.
	sessions := map[string]bool{}
	for _, ev := range evs {
		ev.TotalPromptTokens = ev.InputTokens + ev.CacheWrite5mTokens + ev.CacheWrite1hTokens + ev.CacheReadTokens
		_, sessionID, merged, err := insertOrMerge(ctx, tx, ev)
		if err != nil {
			return fmt.Errorf("store: InsertEvents: request_id %s: %w", ev.RequestID, err)
		}
		if merged && sessionID != "" {
			sessions[sessionID] = true
		}
	}
	for sessionID := range sessions {
		if err := reconcileSessionTx(ctx, tx, sessionID); err != nil {
			return fmt.Errorf("store: InsertEvents: reconcile session %s: %w", sessionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: InsertEvents: commit: %w", err)
	}
	return nil
}

// GetEvent reads back a single event row by id.
func (s *Store) GetEvent(ctx context.Context, id int64) (*Event, error) {
	row := s.db.QueryRowContext(ctx, eventSelectColumns+" FROM events WHERE id = ?", id)
	return scanEvent(row)
}

// SessionEvents returns sessionID's rows oldest-first, for the
// session-scoped analyzer pass (br-GI-1-09) that compares consecutive
// calls. Unlike ListEvents it takes no pagination: a session's row count
// is already bounded by the session resolver's own gap window, since a
// call beyond the gap gets a fresh session id rather than joining this
// one.
func (s *Store) SessionEvents(ctx context.Context, sessionID string) ([]*Event, error) {
	rows, err := s.db.QueryContext(ctx, eventSelectColumns+" FROM events WHERE session_id = ? ORDER BY started_at ASC", sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: SessionEvents: %w", err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: SessionEvents: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// SessionEventsSummary is SessionEvents at the list projection: the same
// rows, same order, without the four header/body blobs. The session route
// renders a call list, so it wants this; the session-scoped analyzer pass
// compares consecutive request bodies and wants SessionEvents.
func (s *Store) SessionEventsSummary(ctx context.Context, sessionID string) ([]*EventSummary, error) {
	rows, err := s.db.QueryContext(ctx, summarySelectColumns+" FROM events WHERE session_id = ? ORDER BY started_at ASC", sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: SessionEventsSummary: %w", err)
	}
	defer rows.Close()

	var out []*EventSummary
	for rows.Next() {
		ev, err := scanEventSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: SessionEventsSummary: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListEvents returns events matching filter, newest first, paginated by
// filter.Limit/Offset.
//
// It reads the scalar projection only. The header/body blobs are not
// selected, so a list never pays to read a body it will not render — on a
// populated store that is the difference between a ~12 MB response and a
// small one. A caller that genuinely needs a body uses ListEventsFull and
// says so by name; it cannot reach one through this type by accident.
func (s *Store) ListEvents(ctx context.Context, filter EventFilter) ([]*EventSummary, error) {
	where, args := filter.whereClause()
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	query := summarySelectColumns + " FROM events" + where + " ORDER BY started_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, filter.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ListEvents: %w", err)
	}
	defer rows.Close()

	var out []*EventSummary
	for rows.Next() {
		ev, err := scanEventSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ListEvents: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListEventsFull is ListEvents at full row width. Exactly four callers need
// it, and each names it explicitly rather than relying on a default: the
// boot-time redaction self-test (which reads headers to prove they were
// redacted), clens export (documented as the complete dump), clens ls
// --json (encodes whole rows), and the replay poll (hands the row to
// replay.OutcomeOf).
func (s *Store) ListEventsFull(ctx context.Context, filter EventFilter) ([]*Event, error) {
	where, args := filter.whereClause()
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	query := eventSelectColumns + " FROM events" + where + " ORDER BY started_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, filter.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ListEventsFull: %w", err)
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ListEventsFull: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LatestProxyStartedAt returns the newest source='proxy' row's started_at, or
// the zero time when no proxy row has ever been written.
//
// It backs the "observed" half of the dashboard's proxy-mode badge. A store
// read rather than an ingest health source, because the proxy is deliberately
// not one of RunOnce's collectors -- internal/ingest defines only jsonl,
// snapshot and admin. The consumer's LastWriteAt on /api/health would not do:
// it counts every source, so a transcript-only install would read as
// "receiving".
func (s *Store) LatestProxyStartedAt(ctx context.Context) (time.Time, error) {
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MAX(started_at) FROM events WHERE source = 'proxy'").Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: LatestProxyStartedAt: %w", err)
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return timeFromNano(at.Int64), nil
}

// CountEvents counts events matching filter, ignoring pagination.
func (s *Store) CountEvents(ctx context.Context, filter EventFilter) (int, error) {
	where, args := filter.whereClause()
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events"+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: CountEvents: %w", err)
	}
	return n, nil
}

func (f EventFilter) whereClause() (string, []any) {
	var conds []string
	var args []any
	if f.Source != "" {
		conds = append(conds, "source = ?")
		args = append(args, f.Source)
	}
	if f.Account != "" {
		conds = append(conds, "account = ?")
		args = append(args, f.Account)
	}
	if f.BillingMode != "" {
		conds = append(conds, "billing_mode = ?")
		args = append(args, f.BillingMode)
	}
	if f.Model != "" {
		conds = append(conds, "model_resolved = ?")
		args = append(args, f.Model)
	}
	if f.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "started_at >= ?")
		args = append(args, f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "started_at < ?")
		args = append(args, f.Until.UnixNano())
	}
	if f.ReplayOf != nil {
		// replay_of is TEXT (schema.sql), so the id is bound in the same
		// decimal form Event.ReplayOf is written with -- an int64 arg here
		// would compare INTEGER against TEXT and never match.
		conds = append(conds, "replay_of = ?")
		args = append(args, strconv.FormatInt(*f.ReplayOf, 10))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// UpsertWarnings attaches warnings to eventID, upserting on (event_id,
// kind) so a second analyzer run (a fresh insert and a cross-source merge
// alike) never duplicates a finding.
func (s *Store) UpsertWarnings(ctx context.Context, eventID int64, warnings []Warning) error {
	if len(warnings) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: UpsertWarnings: begin: %w", err)
	}
	defer tx.Rollback()

	if err := upsertWarningsTx(ctx, tx, eventID, warnings); err != nil {
		return fmt.Errorf("store: UpsertWarnings: %w", err)
	}
	return tx.Commit()
}

func upsertWarningsTx(ctx context.Context, tx *sql.Tx, eventID int64, warnings []Warning) error {
	for _, w := range warnings {
		createdAt := w.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO warnings (event_id, kind, severity, detail, path, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(event_id, kind) DO UPDATE SET
				severity = excluded.severity,
				detail = excluded.detail,
				path = excluded.path,
				created_at = excluded.created_at
		`, eventID, w.Kind, w.Severity, w.Detail, w.Path, unixNano(createdAt))
		if err != nil {
			return err
		}
	}
	return nil
}

// EventWarnings returns warnings for eventID.
func (s *Store) EventWarnings(ctx context.Context, eventID int64) ([]Warning, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event_id, kind, severity, detail, path, created_at
		FROM warnings WHERE event_id = ? ORDER BY id
	`, eventID)
	if err != nil {
		return nil, fmt.Errorf("store: EventWarnings: %w", err)
	}
	defer rows.Close()

	var out []Warning
	for rows.Next() {
		var w Warning
		var createdAt int64
		if err := rows.Scan(&w.ID, &w.EventID, &w.Kind, &w.Severity, &w.Detail, &w.Path, &createdAt); err != nil {
			return nil, fmt.Errorf("store: EventWarnings: %w", err)
		}
		w.CreatedAt = timeFromNano(createdAt)
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListWarnings returns warnings matching filter (Kind, if set), newest
// first, paginated by filter.Limit/Offset -- the GET /api/warnings global
// list, distinct from EventWarnings' per-event lookup above.
func (s *Store) ListWarnings(ctx context.Context, filter WarningFilter) ([]Warning, error) {
	query := "SELECT id, event_id, kind, severity, detail, path, created_at FROM warnings"
	var args []any
	if filter.Kind != "" {
		query += " WHERE kind = ?"
		args = append(args, filter.Kind)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, filter.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ListWarnings: %w", err)
	}
	defer rows.Close()

	var out []Warning
	for rows.Next() {
		var w Warning
		var createdAt int64
		if err := rows.Scan(&w.ID, &w.EventID, &w.Kind, &w.Severity, &w.Detail, &w.Path, &createdAt); err != nil {
			return nil, fmt.Errorf("store: ListWarnings: %w", err)
		}
		w.CreatedAt = timeFromNano(createdAt)
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountWarnings counts warnings matching filter.Kind (all kinds if empty).
func (s *Store) CountWarnings(ctx context.Context, filter WarningFilter) (int, error) {
	query := "SELECT COUNT(*) FROM warnings"
	var args []any
	if filter.Kind != "" {
		query += " WHERE kind = ?"
		args = append(args, filter.Kind)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: CountWarnings: %w", err)
	}
	return n, nil
}

// WarningSummary returns the count of warnings per kind, across all events.
func (s *Store) WarningSummary(ctx context.Context) ([]WarningSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, COUNT(*) FROM warnings GROUP BY kind ORDER BY kind
	`)
	if err != nil {
		return nil, fmt.Errorf("store: WarningSummary: %w", err)
	}
	defer rows.Close()

	var out []WarningSummary
	for rows.Next() {
		var ws WarningSummary
		if err := rows.Scan(&ws.Kind, &ws.Count); err != nil {
			return nil, fmt.Errorf("store: WarningSummary: %w", err)
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// UpsertSession ensures a session row exists for sessionID, seeding
// first_seen/last_seen from at on first sight and otherwise widening
// last_seen. It never touches the aggregate columns — those are always
// re-derived by ReconcileSession, never incremented here.
func (s *Store) UpsertSession(ctx context.Context, sessionID, prefixHash string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, prefix_hash, first_seen, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			first_seen = MIN(first_seen, excluded.first_seen),
			last_seen = MAX(last_seen, excluded.last_seen)
	`, sessionID, prefixHash, unixNano(at), unixNano(at))
	if err != nil {
		return fmt.Errorf("store: UpsertSession: %w", err)
	}
	return nil
}

// ReconcileSession re-derives sessionID's aggregate columns from events and
// warnings — never by increment, so a merge that rewrites a row's tokens is
// reflected exactly, including the NULL-vs-$0.00 billing-mode split
// (invariant 5).
func (s *Store) ReconcileSession(ctx context.Context, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: ReconcileSession: begin: %w", err)
	}
	defer tx.Rollback()
	if err := reconcileSessionTx(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("store: ReconcileSession: %w", err)
	}
	return tx.Commit()
}

func reconcileSessionTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var (
		requestCount                                                                       int
		inputTokens, outputTokens, cacheWrite5m, cacheWrite1h, cacheRead, thinking, prompt int
		pricedCount, unpricedCount                                                         int
		modelSet                                                                           string
		firstSeen, lastSeen                                                                sql.NullInt64
		totalCostUSD, totalApiEquivalentCostUSD                                            sql.NullFloat64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(cache_write_5m_tokens), 0),
			COALESCE(SUM(cache_write_1h_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(thinking_tokens), 0),
			COALESCE(SUM(total_prompt_tokens), 0),
			COALESCE(SUM(CASE WHEN cost_source != '' AND cost_source != 'unpriced' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN cost_source = 'unpriced' THEN 1 ELSE 0 END), 0),
			COALESCE(GROUP_CONCAT(DISTINCT model_resolved), ''),
			MIN(started_at),
			MAX(started_at),
			SUM(CASE WHEN billing_mode = 'api' THEN cost_usd END),
			SUM(CASE WHEN billing_mode = 'subscription' THEN api_equivalent_cost_usd END)
		FROM events WHERE session_id = ?
	`, sessionID).Scan(
		&requestCount,
		&inputTokens, &outputTokens, &cacheWrite5m, &cacheWrite1h, &cacheRead, &thinking, &prompt,
		&pricedCount, &unpricedCount,
		&modelSet,
		&firstSeen, &lastSeen,
		&totalCostUSD, &totalApiEquivalentCostUSD,
	)
	if err != nil {
		return fmt.Errorf("aggregate events: %w", err)
	}

	var warningCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM warnings WHERE event_id IN (SELECT id FROM events WHERE session_id = ?)
	`, sessionID).Scan(&warningCount); err != nil {
		return fmt.Errorf("aggregate warnings: %w", err)
	}

	var costUSD, apiEquivCostUSD any
	if totalCostUSD.Valid {
		costUSD = totalCostUSD.Float64
	}
	if totalApiEquivalentCostUSD.Valid {
		apiEquivCostUSD = totalApiEquivalentCostUSD.Float64
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE sessions SET
			request_count = ?,
			input_tokens = ?, output_tokens = ?,
			cache_write_5m_tokens = ?, cache_write_1h_tokens = ?, cache_read_tokens = ?,
			thinking_tokens = ?, total_prompt_tokens = ?,
			priced_count = ?, unpriced_count = ?,
			model_set = ?,
			warning_count = ?,
			total_cost_usd = ?, total_api_equivalent_cost_usd = ?,
			first_seen = COALESCE(MIN(first_seen, ?), first_seen),
			last_seen = COALESCE(MAX(last_seen, ?), last_seen)
		WHERE id = ?
	`,
		requestCount,
		inputTokens, outputTokens,
		cacheWrite5m, cacheWrite1h, cacheRead,
		thinking, prompt,
		pricedCount, unpricedCount,
		modelSet,
		warningCount,
		costUSD, apiEquivCostUSD,
		nullInt64OrNil(firstSeen), nullInt64OrNil(lastSeen),
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	return nil
}

func nullInt64OrNil(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// GetSession reads back a session by id.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectColumns+" FROM sessions WHERE id = ?", id)
	return scanSession(row)
}

// ListSessions returns sessions newest-first.
func (s *Store) ListSessions(ctx context.Context, limit, offset int) ([]*Session, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	rows, err := s.db.QueryContext(ctx, sessionSelectColumns+" FROM sessions ORDER BY last_seen DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: ListSessions: %w", err)
	}
	defer rows.Close()

	var out []*Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ListSessions: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// CountSessions returns the total number of sessions, ignoring pagination --
// sessions have no filterable column, so the total is simply the whole table.
func (s *Store) CountSessions(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: CountSessions: %w", err)
	}
	return n, nil
}

// LatestSessionByPrefix returns the most recently seen session whose
// prefix_hash matches, or nil if there isn't one — the half of session
// resolution that groups by prefix within the inactivity-gap window.
func (s *Store) LatestSessionByPrefix(ctx context.Context, prefixHash string) (*Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelectColumns+" FROM sessions WHERE prefix_hash = ? ORDER BY last_seen DESC LIMIT 1", prefixHash)
	sess, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return sess, err
}

// PurgeOlderThan deletes events started before cutoff (and, via FK cascade,
// their warnings), returning the number of rows deleted.
func (s *Store) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE started_at < ?", cutoff.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("store: PurgeOlderThan: %w", err)
	}
	return res.RowsAffected()
}

// CountPurgeable counts the events PurgeOlderThan(cutoff) would delete.
func (s *Store) CountPurgeable(ctx context.Context, cutoff time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE started_at < ?", cutoff.UnixNano()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: CountPurgeable: %w", err)
	}
	return n, nil
}

// PurgeableBytes estimates the body bytes PurgeOlderThan(cutoff) would free.
func (s *Store) PurgeableBytes(ctx context.Context, cutoff time.Time) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT SUM(COALESCE(LENGTH(req_body), 0) + COALESCE(LENGTH(resp_body), 0))
		FROM events WHERE started_at < ?
	`, cutoff.UnixNano()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: PurgeableBytes: %w", err)
	}
	return n.Int64, nil
}

// PurgeUnpriced deletes every event whose cost_source is "unpriced",
// regardless of age — clens purge's unpriced predicate.
func (s *Store) PurgeUnpriced(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE cost_source = 'unpriced'")
	if err != nil {
		return 0, fmt.Errorf("store: PurgeUnpriced: %w", err)
	}
	return res.RowsAffected()
}

// Vacuum runs SQLite's VACUUM to reclaim space after a purge.
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("store: Vacuum: %w", err)
	}
	return nil
}

// IngestState is one collector's resume cursor -- a byte offset
// (jsonlogs), a last-fetched window (adminrep), or a last-poll marker
// (snapshot). Value is the collector's own encoding; Status/Error are
// collector-defined progress signals for doctor/dashboard surfacing, never
// interpreted by the store itself. UpdatedAt is this row's last write time,
// which br-GI-1-14's health surface reads as "last success"/"last error"
// depending on which key (a collector's own cursor key, or one of the
// ingest package's separate health:* keys) it was read from.
type IngestState struct {
	Value     string
	Status    string
	Error     string
	UpdatedAt time.Time
}

// GetIngestState reads key's stored cursor. ok is false when the key has
// never been written -- a collector's first poll, not an error.
func (s *Store) GetIngestState(ctx context.Context, key string) (IngestState, bool, error) {
	var st IngestState
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT value, status, error, updated_at FROM ingest_state WHERE key = ?`, key).
		Scan(&st.Value, &st.Status, &st.Error, &updatedAt)
	if err == sql.ErrNoRows {
		return IngestState{}, false, nil
	}
	if err != nil {
		return IngestState{}, false, fmt.Errorf("store: GetIngestState: %w", err)
	}
	st.UpdatedAt = timeFromNano(updatedAt)
	return st, true, nil
}

// SetIngestState upserts key's cursor -- one row per collector-owned key
// (e.g. "jsonl:<path>", "snapshot:<account>"), never appended history.
func (s *Store) SetIngestState(ctx context.Context, key string, state IngestState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ingest_state (key, value, status, error, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value, status = excluded.status, error = excluded.error, updated_at = excluded.updated_at
	`, key, state.Value, state.Status, state.Error, unixNano(time.Now()))
	if err != nil {
		return fmt.Errorf("store: SetIngestState: %w", err)
	}
	return nil
}

// InsertQuotaSnapshot records one quota_snapshots row (source C, one row
// per window per poll).
func (s *Store) InsertQuotaSnapshot(ctx context.Context, q QuotaSnapshot) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO quota_snapshots (observed_at, account, window, utilization_pct, resets_at, status, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, unixNano(q.ObservedAt), q.Account, q.Window, q.UtilizationPct, nullableTime(q.ResetsAt), q.Status, q.Raw)
	if err != nil {
		return fmt.Errorf("store: InsertQuotaSnapshot: %w", err)
	}
	return nil
}

// ListQuotaSnapshots returns the most recent snapshots for account, newest
// first.
func (s *Store) ListQuotaSnapshots(ctx context.Context, account string, limit int) ([]QuotaSnapshot, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, observed_at, account, window, utilization_pct, resets_at, status, raw
		FROM quota_snapshots WHERE account = ? ORDER BY observed_at DESC LIMIT ?
	`, account, limit)
	if err != nil {
		return nil, fmt.Errorf("store: ListQuotaSnapshots: %w", err)
	}
	defer rows.Close()

	var out []QuotaSnapshot
	for rows.Next() {
		var q QuotaSnapshot
		var observedAt int64
		var resetsAt sql.NullInt64
		var util sql.NullFloat64
		if err := rows.Scan(&q.ID, &observedAt, &q.Account, &q.Window, &util, &resetsAt, &q.Status, &q.Raw); err != nil {
			return nil, fmt.Errorf("store: ListQuotaSnapshots: %w", err)
		}
		q.ObservedAt = timeFromNano(observedAt)
		if util.Valid {
			v := util.Float64
			q.UtilizationPct = &v
		}
		if resetsAt.Valid {
			t := timeFromNano(resetsAt.Int64)
			q.ResetsAt = &t
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// UpsertAdminUsageDays UPSERTs each day's usage row on its natural key
// (day_start, model, workspace_id) — a re-fetched overlapping window
// updates in place rather than duplicating (test 20).
func (s *Store) UpsertAdminUsageDays(ctx context.Context, days []AdminUsageDay) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: UpsertAdminUsageDays: begin: %w", err)
	}
	defer tx.Rollback()

	for _, d := range days {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO admin_usage_days
				(day_start, window_start, window_end, model, workspace_id, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, raw, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(day_start, model, workspace_id) DO UPDATE SET
				window_start = excluded.window_start,
				window_end = excluded.window_end,
				input_tokens = excluded.input_tokens,
				output_tokens = excluded.output_tokens,
				cache_read_tokens = excluded.cache_read_tokens,
				cache_write_tokens = excluded.cache_write_tokens,
				raw = excluded.raw,
				fetched_at = excluded.fetched_at
		`, unixNano(d.DayStart), unixNano(d.WindowStart), unixNano(d.WindowEnd), d.Model, d.WorkspaceID,
			d.InputTokens, d.OutputTokens, d.CacheReadTokens, d.CacheWriteTokens, d.Raw, unixNano(d.FetchedAt))
		if err != nil {
			return fmt.Errorf("store: UpsertAdminUsageDays: %w", err)
		}
	}
	return tx.Commit()
}

// UpsertAdminCostDays UPSERTs each day's cost row on its natural key
// (day_start, model, description, currency).
func (s *Store) UpsertAdminCostDays(ctx context.Context, days []AdminCostDay) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: UpsertAdminCostDays: begin: %w", err)
	}
	defer tx.Rollback()

	for _, d := range days {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO admin_cost_days
				(day_start, window_start, window_end, model, description, amount_usd, currency, raw, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(day_start, model, description, currency) DO UPDATE SET
				window_start = excluded.window_start,
				window_end = excluded.window_end,
				amount_usd = excluded.amount_usd,
				raw = excluded.raw,
				fetched_at = excluded.fetched_at
		`, unixNano(d.DayStart), unixNano(d.WindowStart), unixNano(d.WindowEnd), d.Model, d.Description,
			d.AmountUSD, d.Currency, d.Raw, unixNano(d.FetchedAt))
		if err != nil {
			return fmt.Errorf("store: UpsertAdminCostDays: %w", err)
		}
	}
	return tx.Commit()
}

// ListAdminCostDays returns admin_cost_days rows with day_start in
// [since, until), ordered by day_start then model. A zero since or until
// leaves that bound unfiltered. internal/reconcile takes billed rows as a
// caller-supplied parameter rather than reading admin_cost_days itself
// (br-GI-1-13's design), so this is the read its first caller --
// internal/cli's `reconcile` command -- uses to supply them.
func (s *Store) ListAdminCostDays(ctx context.Context, since, until time.Time) ([]AdminCostDay, error) {
	query := `SELECT day_start, window_start, window_end, model, description, amount_usd, currency, raw, fetched_at FROM admin_cost_days`
	var conds []string
	var args []any
	if !since.IsZero() {
		conds = append(conds, "day_start >= ?")
		args = append(args, unixNano(since))
	}
	if !until.IsZero() {
		conds = append(conds, "day_start < ?")
		args = append(args, unixNano(until))
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY day_start, model"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ListAdminCostDays: %w", err)
	}
	defer rows.Close()

	var out []AdminCostDay
	for rows.Next() {
		var d AdminCostDay
		var dayStart, windowStart, windowEnd, fetchedAt int64
		if err := rows.Scan(&dayStart, &windowStart, &windowEnd, &d.Model, &d.Description, &d.AmountUSD, &d.Currency, &d.Raw, &fetchedAt); err != nil {
			return nil, fmt.Errorf("store: ListAdminCostDays: scan: %w", err)
		}
		d.DayStart = timeFromNano(dayStart)
		d.WindowStart = timeFromNano(windowStart)
		d.WindowEnd = timeFromNano(windowEnd)
		d.FetchedAt = timeFromNano(fetchedAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpsertAdminRateLimits replaces the stored rate-limit rows for scope with
// the freshly fetched set (rate limits have no natural key beyond "the
// latest report", so this is delete-then-insert rather than an UPSERT).
func (s *Store) UpsertAdminRateLimits(ctx context.Context, limits []AdminRateLimit) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: UpsertAdminRateLimits: begin: %w", err)
	}
	defer tx.Rollback()

	for _, l := range limits {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM admin_rate_limits WHERE scope = ? AND workspace_id = ? AND model = ? AND group_type = ?
		`, l.Scope, l.WorkspaceID, l.Model, l.GroupType)
		if err != nil {
			return fmt.Errorf("store: UpsertAdminRateLimits: delete: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO admin_rate_limits (scope, workspace_id, model, group_type, limit_value, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, l.Scope, l.WorkspaceID, l.Model, l.GroupType, l.Limit, unixNano(l.FetchedAt))
		if err != nil {
			return fmt.Errorf("store: UpsertAdminRateLimits: insert: %w", err)
		}
	}
	return tx.Commit()
}

// StatsSummary aggregates all events matching filter.
func (s *Store) StatsSummary(ctx context.Context, filter EventFilter) (StatsSummary, error) {
	where, args := filter.whereClause()
	return scanStatsSummary(s.db.QueryRowContext(ctx, statsSelectColumns+" FROM events"+where, args...))
}

// StatsByModel aggregates events matching filter, grouped by
// model_resolved. Every aggregate here groups by billing_mode too, so a
// mixed-mode model never blends a subscription figure with a billed one.
func (s *Store) StatsByModel(ctx context.Context, filter EventFilter) ([]ModelStats, error) {
	where, args := filter.whereClause()
	query := "SELECT model_resolved, billing_mode, " + statsSelectColumnsInner + " FROM events" + where +
		" GROUP BY model_resolved, billing_mode ORDER BY model_resolved"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: StatsByModel: %w", err)
	}
	defer rows.Close()

	var out []ModelStats
	for rows.Next() {
		var m ModelStats
		var billingMode string
		if err := scanStatsRow(rows, &m.StatsSummary, &m.Model, &billingMode); err != nil {
			return nil, fmt.Errorf("store: StatsByModel: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// StatsByPeriod aggregates events matching filter into buckets of the given
// granularity ("day" | "week" | "month"), grouped by billing_mode within
// each bucket for the same reason StatsByModel does. The bucket boundary is
// computed as a Unix-second expression in SQL rather than formatted to a
// string and reparsed, because Go's reference-time layout has no token for
// an ISO week number.
func (s *Store) StatsByPeriod(ctx context.Context, filter EventFilter, granularity string) ([]PeriodStats, error) {
	expr, ok := periodExprs[granularity]
	if !ok {
		return nil, fmt.Errorf("store: StatsByPeriod: unsupported granularity %q", granularity)
	}
	where, args := filter.whereClause()
	query := "SELECT " + expr + " AS period, billing_mode, " +
		statsSelectColumnsInner + " FROM events" + where + " GROUP BY period, billing_mode ORDER BY period"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: StatsByPeriod: %w", err)
	}
	defer rows.Close()

	var out []PeriodStats
	for rows.Next() {
		var p PeriodStats
		var period int64
		var billingMode string
		if err := scanStatsRow(rows, &p.StatsSummary, &period, &billingMode); err != nil {
			return nil, fmt.Errorf("store: StatsByPeriod: %w", err)
		}
		p.PeriodStart = time.Unix(period, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// periodExprs computes each bucket's start as a Unix-second SQL
// expression. "week" starts on Monday: date(d, '-6 days', 'weekday 1')
// lands on d's own Monday whether or not d itself is a Monday, which the
// more obvious 'weekday 1', '-7 days' ordering does not (it skips a week
// when d already is Monday).
var periodExprs = map[string]string{
	"day":   "CAST(STRFTIME('%s', DATE(started_at / 1000000000, 'unixepoch')) AS INTEGER)",
	"week":  "CAST(STRFTIME('%s', DATE(started_at / 1000000000, 'unixepoch', '-6 days', 'weekday 1')) AS INTEGER)",
	"month": "CAST(STRFTIME('%s', DATE(started_at / 1000000000, 'unixepoch', 'start of month')) AS INTEGER)",
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// eventColumnNames is the positional column list every event SELECT uses, in
// scan order. Both scans below take their order from it, and the summary
// projection is *derived* from it rather than hand-kept, so the list SELECT
// cannot drift from the detail SELECT.
var eventColumnNames = []string{
	"id", "request_id", "source", "source_refs", "first_source",
	"started_at", "ended_at",
	"auth_kind", "account", "billing_mode",
	"model_requested", "model_resolved",
	"input_tokens", "output_tokens", "cache_write_5m_tokens", "cache_write_1h_tokens", "cache_read_tokens", "thinking_tokens", "total_prompt_tokens",
	"service_tier", "speed", "effort", "inference_geo",
	"stop_reason", "stop_category",
	"is_sidechain", "session_id", "project", "git_branch", "client_version", "cli_entrypoint",
	"cost_usd", "api_equivalent_cost_usd", "cost_source",
	"prefix_hash", "replay_of", "replay_edits", "capture_complete",
	"method", "path", "status", "req_headers", "resp_headers", "req_body", "resp_body",
	"transcript_content", "transcript_role",
}

// summaryOmittedColumns are the header/body blobs the list path never reads.
// This is the one place the projection names them: adding a column to the
// blob set is a one-line change here, and the list keeps excluding it.
var summaryOmittedColumns = []string{
	"req_headers", "resp_headers", "req_body", "resp_body",
	"transcript_content", "transcript_role",
}

var (
	eventSelectColumns   = selectFrom(eventColumnNames)
	summaryColumnNames   = columnsMinus(eventColumnNames, summaryOmittedColumns)
	summarySelectColumns = selectFrom(summaryColumnNames)
)

func selectFrom(cols []string) string { return "SELECT " + strings.Join(cols, ", ") }

func columnsMinus(all, omit []string) []string {
	drop := make(map[string]bool, len(omit))
	for _, c := range omit {
		drop[c] = true
	}
	out := make([]string, 0, len(all))
	for _, c := range all {
		if !drop[c] {
			out = append(out, c)
		}
	}
	return out
}

// eventScanVals holds the intermediate scan targets shared by both event
// scans, so the two projections cannot decode a column differently.
type eventScanVals struct {
	sourceRefs      string
	startedAt       int64
	endedAt         sql.NullInt64
	isSidechain     int64
	captureComplete int64
	costUSD         sql.NullFloat64
	apiEquivCost    sql.NullFloat64
	prefixHash      sql.NullString
	method, path    sql.NullString
	status          sql.NullInt64
}

// dest returns the Scan destinations for summaryColumnNames, in order,
// followed by any extras the caller's wider projection appends. Writing the
// destination list once is what keeps it in step with the column list; the
// two would otherwise have to be edited together by hand.
func (v *eventScanVals) dest(es *EventSummary, extra ...any) []any {
	d := []any{
		&es.ID, &es.RequestID, &es.Source, &v.sourceRefs, &es.FirstSource,
		&v.startedAt, &v.endedAt,
		&es.AuthKind, &es.Account, &es.BillingMode,
		&es.ModelRequested, &es.ModelResolved,
		&es.InputTokens, &es.OutputTokens, &es.CacheWrite5mTokens, &es.CacheWrite1hTokens, &es.CacheReadTokens, &es.ThinkingTokens, &es.TotalPromptTokens,
		&es.ServiceTier, &es.Speed, &es.Effort, &es.InferenceGeo,
		&es.StopReason, &es.StopCategory,
		&v.isSidechain, &es.SessionID, &es.Project, &es.GitBranch, &es.ClientVersion, &es.CliEntrypoint,
		&v.costUSD, &v.apiEquivCost, &es.CostSource,
		&v.prefixHash, &es.ReplayOf, &es.ReplayEdits, &v.captureComplete,
		&v.method, &v.path, &v.status,
	}
	return append(d, extra...)
}

// apply writes the shared intermediates onto es.
func (v *eventScanVals) apply(es *EventSummary) {
	es.SourceRefs = splitList(v.sourceRefs)
	es.StartedAt = timeFromNano(v.startedAt)
	if v.endedAt.Valid {
		t := timeFromNano(v.endedAt.Int64)
		es.EndedAt = &t
	}
	es.IsSidechain = v.isSidechain != 0
	es.CaptureComplete = v.captureComplete != 0
	if v.costUSD.Valid {
		c := v.costUSD.Float64
		es.CostUSD = &c
	}
	if v.apiEquivCost.Valid {
		c := v.apiEquivCost.Float64
		es.ApiEquivalentCostUSD = &c
	}
	if v.prefixHash.Valid {
		h := v.prefixHash.String
		es.PrefixHash = &h
	}
	es.Method = v.method.String
	es.Path = v.path.String
	es.Status = int(v.status.Int64)
}

func scanEventSummary(row rowScanner) (*EventSummary, error) {
	var es EventSummary
	var v eventScanVals
	if err := row.Scan(v.dest(&es)...); err != nil {
		return nil, err
	}
	v.apply(&es)
	return &es, nil
}

func scanEvent(row rowScanner) (*Event, error) {
	var ev Event
	var v eventScanVals
	var reqHeaders, respHeaders sql.NullString
	var reqBody, respBody, transcriptContent []byte
	var transcriptRole sql.NullString

	// The extras follow eventColumnNames' tail order: the four blobs, then the
	// two transcript columns.
	if err := row.Scan(v.dest(&ev.EventSummary,
		&reqHeaders, &respHeaders, &reqBody, &respBody,
		&transcriptContent, &transcriptRole)...); err != nil {
		return nil, err
	}
	v.apply(&ev.EventSummary)
	ev.ReqHeaders = reqHeaders.String
	ev.RespHeaders = respHeaders.String
	ev.ReqBody = reqBody
	ev.RespBody = respBody
	ev.TranscriptContent = transcriptContent
	ev.TranscriptRole = transcriptRole.String
	return &ev, nil
}

func nullableFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

const sessionSelectColumns = `SELECT
	id, prefix_hash, first_seen, last_seen, request_count,
	input_tokens, output_tokens, cache_write_5m_tokens, cache_write_1h_tokens, cache_read_tokens, thinking_tokens, total_prompt_tokens,
	priced_count, unpriced_count, model_set, warning_count,
	total_cost_usd, total_api_equivalent_cost_usd`

func scanSession(row rowScanner) (*Session, error) {
	var sess Session
	var firstSeen, lastSeen int64
	var modelSet string
	var totalCostUSD, totalApiEquivCostUSD sql.NullFloat64

	err := row.Scan(
		&sess.ID, &sess.PrefixHash, &firstSeen, &lastSeen, &sess.RequestCount,
		&sess.InputTokens, &sess.OutputTokens, &sess.CacheWrite5mTokens, &sess.CacheWrite1hTokens, &sess.CacheReadTokens, &sess.ThinkingTokens, &sess.TotalPromptTokens,
		&sess.PricedCount, &sess.UnpricedCount, &modelSet, &sess.WarningCount,
		&totalCostUSD, &totalApiEquivCostUSD,
	)
	if err != nil {
		return nil, err
	}

	sess.FirstSeen = timeFromNano(firstSeen)
	sess.LastSeen = timeFromNano(lastSeen)
	sess.ModelSet = splitList(modelSet)
	if totalCostUSD.Valid {
		v := totalCostUSD.Float64
		sess.TotalCostUSD = &v
	}
	if totalApiEquivCostUSD.Valid {
		v := totalApiEquivCostUSD.Float64
		sess.TotalApiEquivalentCostUSD = &v
	}
	return &sess, nil
}

const statsSelectColumnsInner = `
	COUNT(*),
	COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_write_5m_tokens), 0), COALESCE(SUM(cache_write_1h_tokens), 0), COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(thinking_tokens), 0), COALESCE(SUM(total_prompt_tokens), 0),
	COALESCE(SUM(CASE WHEN cost_source != '' AND cost_source != 'unpriced' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN cost_source = 'unpriced' THEN 1 ELSE 0 END), 0),
	SUM(CASE WHEN billing_mode = 'api' THEN cost_usd END),
	SUM(CASE WHEN billing_mode = 'subscription' THEN api_equivalent_cost_usd END)
`

const statsSelectColumns = "SELECT" + statsSelectColumnsInner

func scanStatsSummary(row rowScanner) (StatsSummary, error) {
	var s StatsSummary
	var totalCostUSD, totalApiEquivCostUSD sql.NullFloat64
	err := row.Scan(
		&s.RequestCount,
		&s.InputTokens, &s.OutputTokens,
		&s.CacheWrite5mTokens, &s.CacheWrite1hTokens, &s.CacheReadTokens,
		&s.ThinkingTokens, &s.TotalPromptTokens,
		&s.PricedCount, &s.UnpricedCount,
		&totalCostUSD, &totalApiEquivCostUSD,
	)
	if err != nil {
		return StatsSummary{}, err
	}
	if totalCostUSD.Valid {
		v := totalCostUSD.Float64
		s.TotalCostUSD = &v
	}
	if totalApiEquivCostUSD.Valid {
		v := totalApiEquivCostUSD.Float64
		s.TotalApiEquivalentCostUSD = &v
	}
	return s, nil
}

// scanStatsRow scans a "<leading columns...>, <statsSelectColumnsInner>"
// row: leading is the query's own grouping column(s) (e.g. &model,
// &billingMode), scanned in the order the query selects them, ahead of the
// shared aggregate columns into into.
func scanStatsRow(rows *sql.Rows, into *StatsSummary, leading ...any) error {
	var totalCostUSD, totalApiEquivCostUSD sql.NullFloat64
	dest := append(leading,
		&into.RequestCount,
		&into.InputTokens, &into.OutputTokens,
		&into.CacheWrite5mTokens, &into.CacheWrite1hTokens, &into.CacheReadTokens,
		&into.ThinkingTokens, &into.TotalPromptTokens,
		&into.PricedCount, &into.UnpricedCount,
		&totalCostUSD, &totalApiEquivCostUSD,
	)
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	if totalCostUSD.Valid {
		v := totalCostUSD.Float64
		into.TotalCostUSD = &v
	}
	if totalApiEquivCostUSD.Valid {
		v := totalApiEquivCostUSD.Float64
		into.TotalApiEquivalentCostUSD = &v
	}
	return nil
}

// StatsByCostSource groups events matching filter by cost_source
// ("shipped" | "provisional" | "approximate" | "unpriced" | "user" | ...).
func (s *Store) StatsByCostSource(ctx context.Context, filter EventFilter) ([]CostSourceStats, error) {
	where, args := filter.whereClause()
	query := `
		SELECT cost_source, COUNT(*),
			SUM(CASE WHEN billing_mode = 'api' THEN cost_usd END)
		FROM events` + where + `
		GROUP BY cost_source ORDER BY cost_source
	`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: StatsByCostSource: %w", err)
	}
	defer rows.Close()

	var out []CostSourceStats
	for rows.Next() {
		var c CostSourceStats
		var totalCostUSD sql.NullFloat64
		if err := rows.Scan(&c.CostSource, &c.RequestCount, &totalCostUSD); err != nil {
			return nil, fmt.Errorf("store: StatsByCostSource: %w", err)
		}
		if totalCostUSD.Valid {
			v := totalCostUSD.Float64
			c.TotalCostUSD = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
