package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Body archival (br-GI-16-06). Bodies are ~99.5% of lens.db, so archiving moves
// only them: an archived event keeps its `events` row -- every aggregate stays
// whole-history -- and its req_body / resp_body / transcript_content live in a
// per-UTC-day file, bodies-YYYY-MM-DD.db, beside the DB. A row in body_archive
// marks the event and names the day.
//
// Protection: the archive directory is 0700 and the day files 0600, the same as
// lens.db and no better -- on Windows those modes are a no-op, exactly as they
// are for the database itself. The files hold every prompt and file the agent
// read, so they need the same care as the DB.

// Which of the three bodies a row holds. The day file row's body_mask is the
// authority for what is in that file; the body_archive marker mirrors it
// lagging and monotone (it may under-claim, never over-claim).
const (
	MaskReqBody           uint8 = 1
	MaskRespBody          uint8 = 2
	MaskTranscriptContent uint8 = 4
)

const (
	// maxArchiveBlobBytes is the sanity bound on any single decoded blob, on
	// top of its recorded length: a corrupt or hostile file cannot make a read
	// allocate more than this.
	maxArchiveBlobBytes = 256 << 20
	archiveHandleCap    = 4
	hydrateChunk        = 500

	codecZstd = "zstd"
	codecRaw  = "raw"
)

const dayFileSchema = `CREATE TABLE IF NOT EXISTS bodies (
    event_id INTEGER PRIMARY KEY,
    body_mask INTEGER NOT NULL DEFAULT 0,
    req_codec TEXT, resp_codec TEXT, tc_codec TEXT,
    req_len INTEGER, resp_len INTEGER, tc_len INTEGER,
    req_body BLOB, resp_body BLOB, transcript_content BLOB
);`

// errDayMissing is a day file that is not on disk.
var errDayMissing = errors.New("store: archive day file is missing")

var (
	archEnc = mustZstdEncoder()
	archDec = mustZstdDecoder()
)

func mustZstdEncoder() *zstd.Encoder {
	e, err := zstd.NewWriter(nil)
	if err != nil {
		panic(err)
	}
	return e
}

func mustZstdDecoder() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxArchiveBlobBytes))
	if err != nil {
		panic(err)
	}
	return d
}

// archiveRow is one event's bodies as written to a day file. A nil slice means
// the row holds no such body (NULL); a non-nil empty slice is an empty body and
// round-trips as empty, not NULL.
type archiveRow struct {
	EventID                              int64
	ReqBody, RespBody, TranscriptContent []byte
}

// mask is derived from the columns actually present, never supplied by the
// caller, so it cannot disagree with what is written.
func (r archiveRow) mask() uint8 {
	var m uint8
	if r.ReqBody != nil {
		m |= MaskReqBody
	}
	if r.RespBody != nil {
		m |= MaskRespBody
	}
	if r.TranscriptContent != nil {
		m |= MaskTranscriptContent
	}
	return m
}

// encodeBlob compresses b independently of any other blob. The codec is chosen
// per blob -- raw when compression would not shrink it -- and always matches the
// stored bytes. A nil b is NULL for blob, codec and length together.
func encodeBlob(b []byte) (blob, codec, length any) {
	if b == nil {
		return nil, nil, nil
	}
	if c := archEnc.EncodeAll(b, nil); len(c) < len(b) {
		return c, codecZstd, int64(len(b))
	}
	if len(b) == 0 {
		b = []byte{}
	}
	return b, codecRaw, int64(len(b))
}

// decodeBlob reverses encodeBlob. The output is bounded by the recorded length:
// a frame that declares or produces any other size is an error, so a corrupt
// file cannot expand into memory.
func decodeBlob(blob []byte, codec string, wantLen int64) ([]byte, error) {
	if wantLen < 0 || wantLen > maxArchiveBlobBytes {
		return nil, fmt.Errorf("recorded length %d out of range", wantLen)
	}
	switch codec {
	case codecRaw:
		if int64(len(blob)) != wantLen {
			return nil, fmt.Errorf("raw blob is %d bytes, recorded %d", len(blob), wantLen)
		}
		if blob == nil {
			blob = []byte{}
		}
		return blob, nil
	case codecZstd:
		var h zstd.Header
		if err := h.Decode(blob); err != nil {
			return nil, err
		}
		if h.HasFCS && int64(h.FrameContentSize) != wantLen {
			return nil, fmt.Errorf("frame declares %d bytes, recorded %d", h.FrameContentSize, wantLen)
		}
		out, err := archDec.DecodeAll(blob, make([]byte, 0, wantLen))
		if err != nil {
			return nil, err
		}
		if int64(len(out)) != wantLen {
			return nil, fmt.Errorf("decoded %d bytes, recorded %d", len(out), wantLen)
		}
		if out == nil {
			out = []byte{}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown codec %q", codec)
	}
}

func (s *Store) dayPath(day string) string { return dayPathIn(s.archiveDir, day) }

func dayPathIn(dir, day string) string { return filepath.Join(dir, "bodies-"+day+".db") }

// validDay guards the file name: a day comes from the DB, but it becomes a path.
func validDay(day string) bool {
	_, err := time.Parse("2006-01-02", day)
	return err == nil
}

// writeDayRows writes rows to the day's file in one archive transaction, as a
// monotone upsert: the mask ORs, and every blob, its length and its codec keep
// the existing value when the new write does not carry that column. Never
// INSERT OR REPLACE -- that deletes then inserts and would lose a wider
// writer's column.
func (s *Store) writeDayRows(day string, rows []archiveRow) error {
	return s.writeDay(day, rows, false, nil)
}

// writeDay is writeDayRows plus, when verify is set, a read-back inside the same
// archive transaction and before commit: every written column's recorded length
// and the mask must match what was just written, or the transaction is rolled
// back. hook (tests) is called with 2 after the upserts and 3 after the commit;
// an error from it aborts, which is how a crash between the steps is simulated.
func (s *Store) writeDay(day string, rows []archiveRow, verify bool, hook func(step int) error) error {
	if !validDay(day) {
		return fmt.Errorf("store: archive: bad day %q", day)
	}
	if err := os.MkdirAll(s.archiveDir, 0o700); err != nil {
		return fmt.Errorf("store: archive: %w", err)
	}
	path := s.dayPath(day)
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		return fmt.Errorf("store: archive: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(dayFileSchema); err != nil {
		return fmt.Errorf("store: archive: init %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600) // a no-op on Windows; see the header comment

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: archive: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO bodies
	    (event_id, body_mask, req_codec, resp_codec, tc_codec, req_len, resp_len, tc_len,
	     req_body, resp_body, transcript_content)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?)
	  ON CONFLICT(event_id) DO UPDATE SET
	    body_mask = bodies.body_mask | excluded.body_mask,
	    req_codec = COALESCE(excluded.req_codec, bodies.req_codec),
	    resp_codec = COALESCE(excluded.resp_codec, bodies.resp_codec),
	    tc_codec = COALESCE(excluded.tc_codec, bodies.tc_codec),
	    req_len = COALESCE(excluded.req_len, bodies.req_len),
	    resp_len = COALESCE(excluded.resp_len, bodies.resp_len),
	    tc_len = COALESCE(excluded.tc_len, bodies.tc_len),
	    req_body = COALESCE(excluded.req_body, bodies.req_body),
	    resp_body = COALESCE(excluded.resp_body, bodies.resp_body),
	    transcript_content = COALESCE(excluded.transcript_content, bodies.transcript_content)`)
	if err != nil {
		return fmt.Errorf("store: archive: %w", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		rb, rc, rl := encodeBlob(r.ReqBody)
		pb, pc, pl := encodeBlob(r.RespBody)
		tb, tc, tl := encodeBlob(r.TranscriptContent)
		if _, err := stmt.Exec(r.EventID, int64(r.mask()), rc, pc, tc, rl, pl, tl, rb, pb, tb); err != nil {
			return fmt.Errorf("store: archive: event %d: %w", r.EventID, err)
		}
	}
	if hook != nil {
		if err := hook(2); err != nil {
			return err
		}
	}
	if verify {
		if err := verifyDayRows(tx, rows); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: archive: %w", err)
	}
	if hook != nil {
		return hook(3)
	}
	return nil
}

// verifyDayRows reads back what writeDay just wrote, in the same transaction.
func verifyDayRows(tx *sql.Tx, rows []archiveRow) error {
	for _, r := range rows {
		var mask int64
		var rl, pl, tl sql.NullInt64
		if err := tx.QueryRow(`SELECT body_mask, req_len, resp_len, tc_len FROM bodies WHERE event_id = ?`, r.EventID).
			Scan(&mask, &rl, &pl, &tl); err != nil {
			return fmt.Errorf("store: archive: verify event %d: %w", r.EventID, err)
		}
		if uint8(mask)&r.mask() != r.mask() {
			return fmt.Errorf("store: archive: verify event %d: mask %d does not cover %d", r.EventID, mask, r.mask())
		}
		for _, c := range []struct {
			got  sql.NullInt64
			want []byte
		}{{rl, r.ReqBody}, {pl, r.RespBody}, {tl, r.TranscriptContent}} {
			if c.want != nil && (!c.got.Valid || c.got.Int64 != int64(len(c.want))) {
				return fmt.Errorf("store: archive: verify event %d: length read back %v, wrote %d", r.EventID, c.got, len(c.want))
			}
		}
	}
	return nil
}

// dayRow is one day-file row, decoded. Mask is what the file claims to hold
// (the authority); Bad is the subset of those columns that could not be decoded.
type dayRow struct {
	Mask, Bad     uint8
	Req, Resp, TC []byte
}

// readDayRows reads and decodes the rows for ids. A blob that fails to decode
// is logged and flagged in Bad rather than failing the read, so one corrupt
// blob cannot hide its row's healthy neighbours.
func readDayRows(ctx context.Context, db *sql.DB, ids []int64) (map[int64]dayRow, error) {
	out := make(map[int64]dayRow, len(ids))
	for start := 0; start < len(ids); start += hydrateChunk {
		end := min(start+hydrateChunk, len(ids))
		chunk := ids[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := db.QueryContext(ctx, `SELECT event_id, body_mask, req_codec, resp_codec, tc_codec,
		    req_len, resp_len, tc_len, req_body, resp_body, transcript_content
		    FROM bodies WHERE event_id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, mask int64
			var codec [3]sql.NullString
			var ln [3]sql.NullInt64
			var blob [3][]byte
			if err := rows.Scan(&id, &mask, &codec[0], &codec[1], &codec[2],
				&ln[0], &ln[1], &ln[2], &blob[0], &blob[1], &blob[2]); err != nil {
				rows.Close()
				return nil, err
			}
			dr := dayRow{Mask: uint8(mask)}
			dst := [3]*[]byte{&dr.Req, &dr.Resp, &dr.TC}
			for i, bit := range [3]uint8{MaskReqBody, MaskRespBody, MaskTranscriptContent} {
				if dr.Mask&bit == 0 {
					continue
				}
				if !codec[i].Valid || !ln[i].Valid {
					dr.Bad |= bit
					continue
				}
				b, err := decodeBlob(blob[i], codec[i].String, ln[i].Int64)
				if err != nil {
					log.Printf("store: archive: event %d body %d: %v", id, bit, err)
					dr.Bad |= bit
					continue
				}
				*dst[i] = b
			}
			out[id] = dr
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// archiveCache is an LRU of read-only day-file handles, so a list over several
// archived days does not reopen a file per row. Reads run under its lock, which
// makes eviction safe: no handle is closed while a query is using it.
type archiveCache struct {
	mu    sync.Mutex
	dir   string
	dbs   map[string]*sql.DB
	order []string // least recently used first
	opens int      // files opened, for tests
}

func newArchiveCache(dir string) *archiveCache {
	return &archiveCache{dir: dir, dbs: map[string]*sql.DB{}}
}

// with runs fn against the read-only handle for path, opening it if needed. A
// path that is not on disk is errDayMissing, and nothing is created.
func (c *archiveCache) with(path string, fn func(*sql.DB) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	db, ok := c.dbs[path]
	if ok {
		c.touch(path)
		return fn(db)
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errDayMissing
		}
		return err
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	c.opens++
	for len(c.order) >= archiveHandleCap {
		c.closeLocked(c.order[0])
	}
	c.dbs[path] = db
	c.order = append(c.order, path)
	return fn(db)
}

func (c *archiveCache) touch(path string) {
	for i, p := range c.order {
		if p == path {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), path)
			return
		}
	}
}

func (c *archiveCache) closeLocked(path string) {
	if db := c.dbs[path]; db != nil {
		db.Close()
	}
	delete(c.dbs, path)
	for i, p := range c.order {
		if p == path {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// evict closes the cached handle for path, which a caller about to delete or
// replace the file needs on Windows.
func (c *archiveCache) evict(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked(path)
}

func (c *archiveCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range append([]string(nil), c.order...) {
		c.closeLocked(p)
	}
}

// hydrate fills the archived bodies of evs in place, and sets BodiesArchived.
//
// The marker supplies only the day. What to load is decided by the day file
// row's own mask: exactly the columns it holds that are empty in the hot row.
// Populated columns are left alone, so a hot transcript beside archive-only
// req/resp still hydrates the latter. It never returns an error: a broken
// archive must not break the read that asked for a row (fail open) -- the row
// says "missing" instead. It opens each day file once per call, not once per
// row.
func (s *Store) hydrate(ctx context.Context, evs []*Event) {
	if len(evs) == 0 {
		return
	}
	byID := make(map[int64]*Event, len(evs))
	ids := make([]int64, 0, len(evs))
	for _, ev := range evs {
		byID[ev.ID] = ev
		ids = append(ids, ev.ID)
	}

	days := map[string][]int64{}
	for start := 0; start < len(ids); start += hydrateChunk {
		chunk := ids[start:min(start+hydrateChunk, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT event_id, day FROM body_archive WHERE event_id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			log.Printf("store: hydrate: %v", err)
			return
		}
		for rows.Next() {
			var id int64
			var day string
			if err := rows.Scan(&id, &day); err == nil {
				days[day] = append(days[day], id)
			}
		}
		rows.Close()
	}

	for day, dayIDs := range days {
		var got map[int64]dayRow
		var err error
		if !validDay(day) {
			err = fmt.Errorf("bad day %q", day)
		} else {
			err = s.arch.with(s.dayPath(day), func(db *sql.DB) error {
				var rerr error
				got, rerr = readDayRows(ctx, db, dayIDs)
				return rerr
			})
		}
		if err != nil && !errors.Is(err, errDayMissing) {
			log.Printf("store: hydrate: day %s: %v", day, err)
		}
		for _, id := range dayIDs {
			ev := byID[id]
			dr, ok := got[id]
			if err != nil || !ok {
				ev.BodiesArchived = "missing"
				continue
			}
			loaded := 0
			for _, c := range []struct {
				bit uint8
				hot *[]byte
				src []byte
			}{
				{MaskReqBody, &ev.ReqBody, dr.Req},
				{MaskRespBody, &ev.RespBody, dr.Resp},
				{MaskTranscriptContent, &ev.TranscriptContent, dr.TC},
			} {
				if dr.Mask&c.bit != 0 && dr.Bad&c.bit == 0 && len(*c.hot) == 0 {
					*c.hot = c.src
					loaded++
				}
			}
			switch {
			case loaded > 0:
				ev.BodiesArchived = "restored"
			case dr.Bad != 0:
				ev.BodiesArchived = "missing"
			}
		}
	}
}
