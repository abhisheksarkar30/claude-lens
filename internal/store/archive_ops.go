package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Operator-facing archive operations (br-GI-16-09): the status read and the
// restore, the inverse of the archiver's step 4.

// ArchiveStatus is what `clens archive status` prints.
type ArchiveStatus struct {
	Events           int   // all events
	Archived         int   // events with a body_archive marker
	DayFiles         int   // files under the archive dir
	DirBytes         int64 // their total size
	AwaitingBackfill int   // rows the archiver holds back until backfill-tool-names has run
	Missing          int   // markers whose day file or day-file row is not there
	Duplicates       int   // day-file rows with no marker and hot bodies still present
}

// ArchiveStatus reads the archive's state. It opens each day file once and does
// not decode any body: Missing counts absent files and rows, not corrupt blobs
// (a restore or a read reports those).
func (s *Store) ArchiveStatus(ctx context.Context) (ArchiveStatus, error) {
	var st ArchiveStatus
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&st.Events); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM body_archive`).Scan(&st.Archived); err != nil {
		return st, err
	}
	n, err := s.CountEventsAwaitingToolNamesBackfill(ctx)
	if err != nil {
		return st, err
	}
	st.AwaitingBackfill = n

	files, _ := filepath.Glob(filepath.Join(s.archiveDir, "bodies-*.db"))
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			st.DayFiles++
			st.DirBytes += fi.Size()
		}
	}

	markers, err := s.markersByDay(ctx, "")
	if err != nil {
		return st, err
	}
	for day, ids := range markers {
		present, err := s.dayRowIDs(ctx, day)
		if err != nil { // missing or unreadable file: every marker of the day is missing
			st.Missing += len(ids)
			continue
		}
		for _, id := range ids {
			if !present[id] {
				st.Missing++
			}
		}
	}

	for _, f := range files {
		day := filepath.Base(f)
		day = day[len("bodies-") : len(day)-len(".db")]
		present, err := s.dayRowIDs(ctx, day)
		if err != nil {
			continue
		}
		ids := make([]int64, 0, len(present))
		for id := range present {
			ids = append(ids, id)
		}
		for start := 0; start < len(ids); start += hydrateChunk {
			chunk := ids[start:min(start+hydrateChunk, len(ids))]
			var d int
			err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE id IN (`+placeholders(len(chunk))+`)
				AND (typeof(req_body) != 'null' OR typeof(resp_body) != 'null' OR typeof(transcript_content) != 'null')
				AND NOT EXISTS (SELECT 1 FROM body_archive WHERE event_id = events.id)`, idArgs(chunk)...).Scan(&d)
			if err != nil {
				return st, err
			}
			st.Duplicates += d
		}
	}
	return st, nil
}

// markersByDay lists marked event ids per day. A non-empty day restricts it.
func (s *Store) markersByDay(ctx context.Context, day string) (map[string][]int64, error) {
	q, args := `SELECT day, event_id FROM body_archive`, []any{}
	if day != "" {
		q, args = q+` WHERE day = ?`, append(args, day)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var d string
		var id int64
		if err := rows.Scan(&d, &id); err != nil {
			return nil, err
		}
		out[d] = append(out[d], id)
	}
	return out, rows.Err()
}

// dayRowIDs is the set of event ids the day file holds a row for.
func (s *Store) dayRowIDs(ctx context.Context, day string) (map[int64]bool, error) {
	if !validDay(day) {
		return nil, fmt.Errorf("bad day %q", day)
	}
	out := map[int64]bool{}
	err := s.arch.with(s.dayPath(day), func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, `SELECT event_id FROM bodies`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
		}
		return rows.Err()
	})
	return out, err
}

// RestoreFailure is a marked row that was left exactly as it was.
type RestoreFailure struct {
	EventID int64
	Day     string
	Reason  string
}

// RestoreResult reports one RestoreBodies call. In a dry run Restored counts the
// rows that would be restored.
type RestoreResult struct {
	Restored int
	Failed   []RestoreFailure
}

// RestoreBodies moves the bodies of marked events started in [since, until) back
// into their hot rows. A zero bound is open. It is the inverse of the
// archiver's step 4, and its order is what keeps a body from ever being in
// neither place:
//
//  1. read and decode the day row -- any failure leaves the row untouched;
//  2. in ONE hot transaction, write back exactly the columns the day row's mask
//     holds (only into empty hot columns) and delete the marker;
//  3. only after that commits, delete the day-file row.
//
// A crash between 2 and 3 leaves a harmless duplicate (`archive status`
// reports it); the reverse order would leave a marker with no archive copy.
// hook (tests) is called with 1 after the hot commit and 2 after the day-row
// delete; an error from it aborts.
func (s *Store) RestoreBodies(ctx context.Context, since, until time.Time, dryRun bool, hook func(step int) error) (RestoreResult, error) {
	var res RestoreResult
	q := `SELECT a.day, a.event_id FROM body_archive a JOIN events e ON e.id = a.event_id WHERE 1=1`
	var args []any
	if !since.IsZero() {
		q += ` AND e.started_at >= ?`
		args = append(args, since.UnixNano())
	}
	if !until.IsZero() {
		q += ` AND e.started_at < ?`
		args = append(args, until.UnixNano())
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, err
	}
	byDay := map[string][]int64{}
	for rows.Next() {
		var d string
		var id int64
		if err := rows.Scan(&d, &id); err != nil {
			rows.Close()
			return res, err
		}
		byDay[d] = append(byDay[d], id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	days := make([]string, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, day := range days {
		if err := s.restoreDay(ctx, day, byDay[day], dryRun, hook, &res); err != nil {
			return res, err
		}
	}
	if !dryRun {
		// Removes day files this restore emptied. GC only ever deletes rows with
		// neither a marker nor hot bodies, so a duplicate is never touched.
		if _, err := s.GCArchive(ctx); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (s *Store) restoreDay(ctx context.Context, day string, ids []int64, dryRun bool, hook func(int) error, res *RestoreResult) error {
	failAll := func(reason string) {
		for _, id := range ids {
			res.Failed = append(res.Failed, RestoreFailure{EventID: id, Day: day, Reason: reason})
		}
	}
	if !validDay(day) {
		failAll("bad day in marker")
		return nil
	}
	path := s.dayPath(day)
	if _, err := os.Stat(path); err != nil {
		failAll("day file is missing")
		return nil
	}
	s.arch.evict(path) // a cached read-only handle would sit beside our writer
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		failAll(err.Error())
		return nil
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	got, err := readDayRows(ctx, db, ids)
	if err != nil {
		failAll("day file unreadable: " + err.Error())
		return nil
	}
	cols := []struct {
		bit uint8
		col string
		get func(dayRow) []byte
	}{
		{MaskReqBody, "req_body", func(r dayRow) []byte { return r.Req }},
		{MaskRespBody, "resp_body", func(r dayRow) []byte { return r.Resp }},
		{MaskTranscriptContent, "transcript_content", func(r dayRow) []byte { return r.TC }},
	}
	for _, id := range ids {
		dr, ok := got[id]
		switch {
		case !ok:
			res.Failed = append(res.Failed, RestoreFailure{id, day, "no row in the day file"})
			continue
		case dr.Bad != 0:
			res.Failed = append(res.Failed, RestoreFailure{id, day, "a body could not be decoded"})
			continue
		case dr.Mask == 0:
			res.Failed = append(res.Failed, RestoreFailure{id, day, "the day file row holds no body"})
			continue
		}
		if dryRun {
			res.Restored++
			continue
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, c := range cols {
			if dr.Mask&c.bit == 0 {
				continue
			}
			b := c.get(dr)
			if b == nil {
				b = []byte{}
			}
			// Only into an empty hot column: a transcript a merge backfilled
			// after archival must not be overwritten by the archived one.
			if _, err := tx.ExecContext(ctx, `UPDATE events SET `+c.col+` = CASE WHEN `+c.col+` IS NULL OR length(`+c.col+`) = 0 THEN ? ELSE `+c.col+` END WHERE id = ?`, b, id); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM body_archive WHERE event_id = ?`, id); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if hook != nil {
			if err := hook(1); err != nil {
				return err
			}
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM bodies WHERE event_id = ?`, id); err != nil {
			return fmt.Errorf("store: restore: event %d restored but its day row was not deleted: %w", id, err)
		}
		if hook != nil {
			if err := hook(2); err != nil {
				return err
			}
		}
		res.Restored++
	}
	return nil
}

// CountArchivable counts the rows Archiver.Run would move for a hot window of
// hotDays as of now: the candidate predicate, counted -- `archive run
// --dry-run`.
func (s *Store) CountArchivable(ctx context.Context, hotDays int, now time.Time) (int, error) {
	if hotDays <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-time.Duration(hotDays) * 24 * time.Hour).UnixNano()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events
		WHERE started_at < ?
		  AND (typeof(req_body) != 'null' OR typeof(resp_body) != 'null' OR typeof(transcript_content) != 'null')
		  AND NOT (req_body IS NOT NULL AND req_tool_names IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM body_archive WHERE event_id = events.id)`, cutoff).Scan(&n)
	return n, err
}
