package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	archiveBatchRows    = 20
	archiveBatchBytes   = 8 << 20
	defaultArchivePause = 50 * time.Millisecond
)

// Archiver moves the bodies of rows older than HotDays into day files.
//
// The invariant every step derives from: the day file row is the authority for
// what the archive holds, and its coverage is monotone (columns only added).
// The body_archive marker mirrors it, also monotone, so it may lag the file but
// never claim more. Step 4 NULLs exactly the columns the file's mask covers.
// Hydration reads the file row. So no body ever exists in neither place, and a
// stale artifact can only under-claim.
type Archiver struct {
	st *Store
	// HotDays is read on every Run, so a reload can change the window.
	HotDays func() int
	Now     func() time.Time
	// Pause is the yield between batches, so the consumer's flush and dashboard
	// reads interleave with the archiver on the store's single connection.
	Pause time.Duration
	// AfterStep, if set, is called after each of the four steps of a batch
	// (1 read, 2 write, 3 verify+commit, 4 mark+clear); an error aborts the run
	// there. Test-only: it is how a crash between two steps is simulated.
	AfterStep func(step int) error

	BatchRows  int
	BatchBytes int64
}

// ArchiveResult reports one Run.
type ArchiveResult struct {
	Archived        int // events whose bodies were moved
	Batches         int
	SkippedBackfill int // eligible rows held back because they await backfill-tool-names
}

// NewArchiver returns an Archiver reading its window from hotDays on each Run.
func (s *Store) NewArchiver(hotDays func() int) *Archiver {
	return &Archiver{
		st: s, HotDays: hotDays, Now: time.Now, Pause: defaultArchivePause,
		BatchRows: archiveBatchRows, BatchBytes: archiveBatchBytes,
	}
}

// The candidate query reads only each body column's type and length -- both are
// in the row header, so no blob is touched -- and walks idx_events_started_at.
// Rows awaiting backfill-tool-names are excluded here rather than skipped after
// selection: an excluded row that stayed a candidate would refill every batch
// window and the run would never advance.
const archiveCandidateSQL = `SELECT id, started_at,
	COALESCE(length(req_body),0) + COALESCE(length(resp_body),0) + COALESCE(length(transcript_content),0)
FROM events
WHERE started_at >= ? AND (started_at > ? OR id > ?) AND started_at < ?
  AND (typeof(req_body) != 'null' OR typeof(resp_body) != 'null' OR typeof(transcript_content) != 'null')
  AND NOT (req_body IS NOT NULL AND req_tool_names IS NULL)
  AND NOT EXISTS (SELECT 1 FROM body_archive WHERE event_id = events.id)
ORDER BY started_at ASC, id ASC LIMIT ?`

type archiveCandidate struct {
	id, startedAt, size int64
}

func dayOf(startedAtNs int64) string {
	return time.Unix(0, startedAtNs).UTC().Format("2006-01-02")
}

// Run archives every eligible row and returns. ctx is checked between batches,
// so a cancelled run leaves a consistent store. It never VACUUMs. A column that
// arrives after a row was archived stays hot and is not re-archived.
func (a *Archiver) Run(ctx context.Context) (ArchiveResult, error) {
	var res ArchiveResult
	hot := a.HotDays()
	if hot <= 0 {
		return res, nil // archival disabled
	}
	cutoff := a.Now().Add(-time.Duration(hot) * 24 * time.Hour).UnixNano()

	if err := a.st.db.QueryRowContext(ctx, `SELECT count(*) FROM events
		WHERE started_at < ? AND req_body IS NOT NULL AND req_tool_names IS NULL
		  AND NOT EXISTS (SELECT 1 FROM body_archive WHERE event_id = events.id)`, cutoff).Scan(&res.SkippedBackfill); err != nil {
		return res, fmt.Errorf("store: archive: count skipped: %w", err)
	}

	lastStarted, lastID := int64(-1<<63), int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		cands, err := a.candidates(ctx, cutoff, lastStarted, lastID)
		if err != nil {
			return res, err
		}
		if len(cands) == 0 {
			return res, nil
		}
		batch := a.takeBatch(cands)
		last := batch[len(batch)-1]
		lastStarted, lastID = last.startedAt, last.id

		n, err := a.archiveBatch(ctx, dayOf(batch[0].startedAt), batch)
		res.Archived += n
		res.Batches++
		if err != nil {
			return res, err
		}

		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(a.Pause):
		}
	}
}

func (a *Archiver) candidates(ctx context.Context, cutoff, lastStarted, lastID int64) ([]archiveCandidate, error) {
	rows, err := a.st.db.QueryContext(ctx, archiveCandidateSQL, lastStarted, lastStarted, lastID, cutoff, a.BatchRows)
	if err != nil {
		return nil, fmt.Errorf("store: archive: candidates: %w", err)
	}
	defer rows.Close()
	var out []archiveCandidate
	for rows.Next() {
		var c archiveCandidate
		if err := rows.Scan(&c.id, &c.startedAt, &c.size); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// takeBatch is the longest prefix of cands within one UTC day and the byte cap;
// always at least one row, so an oversized body still makes progress.
func (a *Archiver) takeBatch(cands []archiveCandidate) []archiveCandidate {
	day := dayOf(cands[0].startedAt)
	var total int64
	for i, c := range cands {
		total += c.size
		if i > 0 && (dayOf(c.startedAt) != day || total > a.BatchBytes) {
			return cands[:i]
		}
	}
	return cands
}

func (a *Archiver) step(n int) error {
	if a.AfterStep != nil {
		return a.AfterStep(n)
	}
	return nil
}

func (a *Archiver) archiveBatch(ctx context.Context, day string, batch []archiveCandidate) (int, error) {
	// Step 1: read the bodies. A short hold of the single connection; it is
	// released before anything is compressed or written, because GI-13's hang
	// was a long hold.
	rows, err := a.readBodies(ctx, batch)
	if err != nil {
		return 0, err
	}
	if err := a.step(1); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	// Steps 2 and 3: compress, write and verify in one archive transaction,
	// committed only once the read-back matches.
	if err := a.st.writeDay(day, rows, true, a.step); err != nil {
		return 0, err
	}

	// Step 4, only after the copy is durable.
	n, err := a.markAndClear(ctx, day, rows)
	if err != nil {
		return n, err
	}
	return n, a.step(4)
}

func (a *Archiver) readBodies(ctx context.Context, batch []archiveCandidate) ([]archiveRow, error) {
	args := make([]any, len(batch))
	for i, c := range batch {
		args[i] = c.id
	}
	rs, err := a.st.db.QueryContext(ctx, `SELECT id,
		typeof(req_body) != 'null', typeof(resp_body) != 'null', typeof(transcript_content) != 'null',
		req_body, resp_body, transcript_content
		FROM events WHERE id IN (`+placeholders(len(batch))+`) ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: archive: read bodies: %w", err)
	}
	defer rs.Close()
	var out []archiveRow
	for rs.Next() {
		var r archiveRow
		var hasReq, hasResp, hasTC bool
		if err := rs.Scan(&r.EventID, &hasReq, &hasResp, &hasTC, &r.ReqBody, &r.RespBody, &r.TranscriptContent); err != nil {
			return nil, err
		}
		// A driver may hand a zero-length BLOB back as nil; the type flag is the
		// truth, so an empty body stays empty rather than becoming NULL.
		for _, c := range []struct {
			has bool
			b   *[]byte
		}{{hasReq, &r.ReqBody}, {hasResp, &r.RespBody}, {hasTC, &r.TranscriptContent}} {
			if c.has && *c.b == nil {
				*c.b = []byte{}
			}
			if !c.has {
				*c.b = nil
			}
		}
		if r.mask() != 0 {
			out = append(out, r)
		}
	}
	return out, rs.Err()
}

// markAndClear is step 4: one hot transaction per batch. For each event it
// re-reads the day file row's mask (the authority -- a second archiver may have
// widened it), ORs it into the marker, and NULLs exactly those columns.
//
// It deliberately does not use BEGIN IMMEDIATE / _txlock=immediate: that would
// change the mode of every BeginTx in the store, and it cannot order the
// day-file read anyway. The ordering that matters is copy, verify, commit, and
// only then this -- and the mask read here means an event whose file row has
// vanished (a GC) is simply left hot.
func (a *Archiver) markAndClear(ctx context.Context, day string, rows []archiveRow) (int, error) {
	tx, err := a.st.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: archive: %w", err)
	}
	defer tx.Rollback()

	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.EventID
	}
	masks := map[int64]uint8{}
	err = a.st.arch.with(a.st.dayPath(day), func(db *sql.DB) error {
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		rs, err := db.QueryContext(ctx, `SELECT event_id, body_mask FROM bodies WHERE event_id IN (`+placeholders(len(ids))+`)`, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var id, m int64
			if err := rs.Scan(&id, &m); err != nil {
				return err
			}
			masks[id] = uint8(m)
		}
		return rs.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("store: archive: read day masks: %w", err)
	}

	now := a.Now().UnixNano()
	n := 0
	for _, id := range ids {
		mask := masks[id]
		if mask == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO body_archive (event_id, day, archived_at, body_mask) VALUES (?,?,?,?)
			ON CONFLICT(event_id) DO UPDATE SET body_mask = body_archive.body_mask | excluded.body_mask`,
			id, day, now, int64(mask)); err != nil {
			return 0, fmt.Errorf("store: archive: mark event %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET
			req_body = CASE WHEN ?1 & 1 THEN NULL ELSE req_body END,
			resp_body = CASE WHEN ?1 & 2 THEN NULL ELSE resp_body END,
			transcript_content = CASE WHEN ?1 & 4 THEN NULL ELSE transcript_content END
			WHERE id = ?2`, int64(mask), id); err != nil {
			return 0, fmt.Errorf("store: archive: clear event %d: %w", id, err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: archive: %w", err)
	}
	return n, nil
}

// GCResult reports one GCArchive.
type GCResult struct {
	RowsDeleted  int
	FilesRemoved int
}

// GCArchive deletes day-file rows that are true orphans and removes day files
// that become empty. A true orphan has no body_archive marker AND every body
// column of its hot row is NULL (vacuously so for a purged, now-absent event).
// The second clause is load-bearing: between an archiver's commit of the day
// file and its hot transaction the hot row still holds the bodies and has no
// marker, and without it GC would delete the only durable copy.
//
// Removing a file races an archiver writing to it, but harmlessly: the
// archiver's step 4 re-reads the file row's mask, finds it gone and leaves the
// event hot, and the next run archives it again.
func (s *Store) GCArchive(ctx context.Context) (GCResult, error) {
	var res GCResult
	files, err := filepath.Glob(filepath.Join(s.archiveDir, "bodies-*.db"))
	if err != nil {
		return res, err
	}
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		deleted, removed, err := s.gcFile(ctx, path)
		res.RowsDeleted += deleted
		if removed {
			res.FilesRemoved++
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

func idArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func (s *Store) gcFile(ctx context.Context, path string) (deleted int, removed bool, err error) {
	s.arch.evict(path) // a cached read-only handle would block the removal on Windows
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		return 0, false, err
	}
	db.SetMaxOpenConns(1)
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
	}()

	var ids []int64
	rs, err := db.QueryContext(ctx, `SELECT event_id FROM bodies`)
	if err != nil {
		return 0, false, fmt.Errorf("store: gc %s: %w", filepath.Base(path), err)
	}
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return 0, false, err
		}
		ids = append(ids, id)
	}
	rs.Close()

	var orphans []int64
	for start := 0; start < len(ids); start += hydrateChunk {
		chunk := ids[start:min(start+hydrateChunk, len(ids))]
		live := map[int64]bool{}
		for _, q := range []string{
			`SELECT event_id FROM body_archive WHERE event_id IN (` + placeholders(len(chunk)) + `)`,
			`SELECT id FROM events WHERE id IN (` + placeholders(len(chunk)) + `)
			   AND (typeof(req_body) != 'null' OR typeof(resp_body) != 'null' OR typeof(transcript_content) != 'null')`,
		} {
			hr, err := s.db.QueryContext(ctx, q, idArgs(chunk)...)
			if err != nil {
				return 0, false, fmt.Errorf("store: gc: %w", err)
			}
			for hr.Next() {
				var id int64
				if err := hr.Scan(&id); err == nil {
					live[id] = true
				}
			}
			hr.Close()
		}
		for _, id := range chunk {
			if !live[id] {
				orphans = append(orphans, id)
			}
		}
	}

	for start := 0; start < len(orphans); start += hydrateChunk {
		chunk := orphans[start:min(start+hydrateChunk, len(orphans))]
		r, err := db.ExecContext(ctx, `DELETE FROM bodies WHERE event_id IN (`+placeholders(len(chunk))+`)`, idArgs(chunk)...)
		if err != nil {
			return deleted, false, fmt.Errorf("store: gc %s: %w", filepath.Base(path), err)
		}
		n, _ := r.RowsAffected()
		deleted += int(n)
	}

	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM bodies`).Scan(&left); err != nil {
		return deleted, false, err
	}
	if left > 0 {
		return deleted, false, nil
	}
	db.Close()
	closed = true
	if err := os.Remove(path); err != nil {
		// Still held (Windows) or already gone; either way, retry next run.
		log.Printf("store: gc: remove %s: %v", filepath.Base(path), err)
		return deleted, false, nil
	}
	return deleted, true, nil
}
