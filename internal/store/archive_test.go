package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// preArchiveSchema is schemaSQL without the body_archive block: the shape of a
// database sitting at user_version = 4.
func preArchiveSchema(t *testing.T) string {
	t.Helper()
	i := strings.Index(schemaSQL, "-- br-GI-16-06")
	const end = "idx_body_archive_day ON body_archive(day);"
	j := strings.Index(schemaSQL, end)
	if i < 0 || j < 0 {
		t.Fatal("schema.sql no longer has the body_archive block")
	}
	return schemaSQL[:i] + strings.TrimLeft(schemaSQL[j+len(end):], "\r\n")
}

func tableColumns(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	return strings.Join(cols, ",")
}

func TestMigrateAddsBodyArchiveAtVersionFive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	db := rawDB(t, path)
	if _, err := db.Exec(preArchiveSchema(t)); err != nil {
		t.Fatalf("build a v4 database: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO events (request_id, source, first_source, started_at) VALUES ('r', 'proxy', 'proxy', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if hasTable(t, db, "body_archive") {
		t.Fatal("the v4 fixture already has body_archive")
	}
	before := tableColumns(t, db, "events")
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open a v4 database: %v", err)
	}
	defer st.Close()
	if got := userVersion(t, st.db); got != 5 {
		t.Errorf("user_version = %d, want 5", got)
	}
	if !hasTable(t, st.db, "body_archive") || !hasIndex(t, st.db, "idx_body_archive_day") {
		t.Error("the migration did not leave body_archive and its day index")
	}
	if after := tableColumns(t, st.db, "events"); after != before {
		t.Errorf("events columns changed by the migration:\n before %s\n after  %s", before, after)
	}
	if n, _ := st.CountEvents(context.Background(), EventFilter{}); n != 1 {
		t.Errorf("row count = %d after migration, want 1", n)
	}
}

func TestFreshStoreHasBodyArchive(t *testing.T) {
	st := newTestStore(t)
	if got := userVersion(t, st.db); got != schemaVersion || schemaVersion != 5 {
		t.Errorf("user_version = %d, schemaVersion = %d, want 5", got, schemaVersion)
	}
	if !hasTable(t, st.db, "body_archive") || !hasIndex(t, st.db, "idx_body_archive_day") {
		t.Error("a fresh store lacks body_archive or its index")
	}
}

func TestArchiveDirIsBesideTheDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "lens.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if want := filepath.Join(filepath.Dir(path), "archive"); st.archiveDir != want {
		t.Errorf("archiveDir = %q, want %q", st.archiveDir, want)
	}
}

// insertArchived inserts an event, writes row to day's file, marks it archived
// and clears the hot columns row carries -- what the archiver will do.
func insertArchived(t *testing.T, st *Store, reqID, day string, hot *Event, row archiveRow) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := st.InsertEvent(ctx, hot)
	if err != nil {
		t.Fatal(err)
	}
	row.EventID = id
	if err := st.writeDayRows(day, []archiveRow{row}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT OR REPLACE INTO body_archive(event_id, day, archived_at, body_mask) VALUES (?,?,?,?)`,
		id, day, time.Now().UnixNano(), int64(row.mask())); err != nil {
		t.Fatal(err)
	}
	for col, held := range map[string]bool{"req_body": row.ReqBody != nil, "resp_body": row.RespBody != nil, "transcript_content": row.TranscriptContent != nil} {
		if held {
			if _, err := st.db.Exec("UPDATE events SET "+col+" = NULL WHERE id = ?", id); err != nil {
				t.Fatal(err)
			}
		}
	}
	return id
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func TestBlobCodecIsChosenPerBlob(t *testing.T) {
	big := bytes.Repeat([]byte("abcd"), 125_000) // 500 KB, compressible
	small := randBytes(1024)                       // incompressible
	st := newTestStore(t)
	if err := st.writeDayRows("2026-09-01", []archiveRow{{EventID: 1, ReqBody: big, RespBody: small}}); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, st.dayPath("2026-09-01"))
	defer db.Close()
	var rc, pc string
	var rl, pl int64
	var tcCodec, tcLen, tcBlob sql.NullString
	if err := db.QueryRow(`SELECT req_codec, resp_codec, req_len, resp_len, tc_codec, tc_len, transcript_content FROM bodies`).
		Scan(&rc, &pc, &rl, &pl, &tcCodec, &tcLen, &tcBlob); err != nil {
		t.Fatal(err)
	}
	if rc != "zstd" || pc != "raw" {
		t.Errorf("codecs = %s/%s, want zstd for the compressible req and raw for the incompressible resp", rc, pc)
	}
	if rl != int64(len(big)) || pl != int64(len(small)) {
		t.Errorf("recorded lengths %d/%d are not the original lengths", rl, pl)
	}
	if tcCodec.Valid || tcLen.Valid || tcBlob.Valid {
		t.Error("a body the row does not hold must be NULL in blob, codec and length together")
	}

	got, err := readOneDayRow(t, st, "2026-09-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Req, big) || !bytes.Equal(got.Resp, small) || got.Mask != MaskReqBody|MaskRespBody || got.Bad != 0 {
		t.Errorf("round trip lost data: mask %d bad %d", got.Mask, got.Bad)
	}
}

func readOneDayRow(t *testing.T, st *Store, day string, id int64) (dayRow, error) {
	t.Helper()
	var out map[int64]dayRow
	err := st.arch.with(st.dayPath(day), func(db *sql.DB) error {
		var err error
		out, err = readDayRows(context.Background(), db, []int64{id})
		return err
	})
	return out[id], err
}

func TestNullAndEmptyBodiesAreDistinct(t *testing.T) {
	st := newTestStore(t)
	if err := st.writeDayRows("2026-09-01", []archiveRow{{EventID: 1, ReqBody: []byte{}}}); err != nil {
		t.Fatal(err)
	}
	got, err := readOneDayRow(t, st, "2026-09-01", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Req == nil || len(got.Req) != 0 {
		t.Errorf("an empty body came back as %v, want empty and non-nil", got.Req)
	}
	if got.Resp != nil || got.TC != nil || got.Mask != MaskReqBody {
		t.Errorf("NULL columns came back non-NULL: mask %d", got.Mask)
	}
}

func TestDayWriteIsAMonotoneUpsert(t *testing.T) {
	st := newTestStore(t)
	day := "2026-09-01"
	req, resp, tc := []byte(strings.Repeat("r", 4000)), []byte(strings.Repeat("s", 4000)), []byte(strings.Repeat("t", 4000))
	if err := st.writeDayRows(day, []archiveRow{{EventID: 1, ReqBody: req, RespBody: resp}}); err != nil {
		t.Fatal(err)
	}
	if err := st.writeDayRows(day, []archiveRow{{EventID: 1, TranscriptContent: tc}}); err != nil {
		t.Fatal(err)
	}
	got, err := readOneDayRow(t, st, day, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mask != 7 || got.Bad != 0 || !bytes.Equal(got.Req, req) || !bytes.Equal(got.Resp, resp) || !bytes.Equal(got.TC, tc) {
		t.Fatalf("mask 3 then mask 4 must give mask 7 with every blob: mask %d bad %d", got.Mask, got.Bad)
	}
	// A narrower write after a wider one never drops a column.
	if err := st.writeDayRows(day, []archiveRow{{EventID: 1, ReqBody: req}}); err != nil {
		t.Fatal(err)
	}
	got, _ = readOneDayRow(t, st, day, 1)
	if got.Mask != 7 || !bytes.Equal(got.Resp, resp) || !bytes.Equal(got.TC, tc) {
		t.Fatalf("a narrower write dropped a column: mask %d", got.Mask)
	}
}

func TestDecodeIsBoundedByTheRecordedLength(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 100_000)
	blob, codec, _ := encodeBlob(big)
	if codec != "zstd" {
		t.Fatalf("fixture must compress, got %v", codec)
	}
	if _, err := decodeBlob(blob.([]byte), "zstd", 10); err == nil {
		t.Error("a frame larger than its recorded length must be an error, not decoded")
	}
	if _, err := decodeBlob([]byte("abc"), "raw", 5); err == nil {
		t.Error("a raw blob whose length disagrees with the record must be an error")
	}
	if _, err := decodeBlob(nil, "lz4", 0); err == nil {
		t.Error("an unknown codec must be an error")
	}
	if _, err := decodeBlob(nil, "raw", maxArchiveBlobBytes+1); err == nil {
		t.Error("a recorded length past the sanity bound must be an error")
	}
}

func TestHydrateFullRow(t *testing.T) {
	st := newTestStore(t)
	ev := fullEvent("r-full")
	ev.TranscriptContent = []byte("hot-transcript-that-will-be-cleared")
	req, resp, tc := []byte(`{"big":"`+strings.Repeat("q", 5000)+`"}`), []byte("resp"), []byte("tc")
	id := insertArchived(t, st, "r-full", "2026-09-01", ev, archiveRow{ReqBody: req, RespBody: resp, TranscriptContent: tc})

	got, err := st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ReqBody, req) || !bytes.Equal(got.RespBody, resp) || !bytes.Equal(got.TranscriptContent, tc) {
		t.Fatal("hydrated bodies are not byte-identical to what was archived")
	}
	if got.BodiesArchived != "restored" {
		t.Errorf("BodiesArchived = %q, want restored", got.BodiesArchived)
	}
}

// A hot transcript beside archive-only req/resp still hydrates the latter, and
// the hot column is left exactly as it is.
func TestHydratePartialRowFillsOnlyTheMissingMaskedColumns(t *testing.T) {
	st := newTestStore(t)
	ev := fullEvent("r-part")
	ev.TranscriptContent = []byte("hot-transcript")
	id := insertArchived(t, st, "r-part", "2026-09-01", ev, archiveRow{ReqBody: []byte("archived-req"), RespBody: []byte("archived-resp")})

	got, _ := st.GetEvent(context.Background(), id)
	if string(got.ReqBody) != "archived-req" || string(got.RespBody) != "archived-resp" {
		t.Errorf("archived req/resp not restored: %q / %q", got.ReqBody, got.RespBody)
	}
	if string(got.TranscriptContent) != "hot-transcript" {
		t.Errorf("the hot transcript was disturbed: %q", got.TranscriptContent)
	}
	if got.BodiesArchived != "restored" {
		t.Errorf("a partially restored row must be restored, got %q", got.BodiesArchived)
	}
}

func TestHydrateLeavesHotRowsAlone(t *testing.T) {
	st := newTestStore(t)
	id, _, err := st.InsertEvent(context.Background(), fullEvent("r-hot"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetEvent(context.Background(), id)
	if got.BodiesArchived != "" || string(got.ReqBody) != `{"model":"claude-sonnet-5"}` {
		t.Errorf("a never-archived row changed: %q %q", got.BodiesArchived, got.ReqBody)
	}

	// A marker whose bodies are all still hot is untouched, and not "missing".
	id2 := insertArchived(t, st, "r-hot2", "2026-09-02", fullEvent("r-hot2"), archiveRow{ReqBody: []byte("archived")})
	if _, err := st.db.Exec("UPDATE events SET req_body = ? WHERE id = ?", []byte("hot-again"), id2); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetEvent(context.Background(), id2)
	if string(got.ReqBody) != "hot-again" || got.BodiesArchived != "" {
		t.Errorf("populated hot column overwritten or mislabelled: %q %q", got.ReqBody, got.BodiesArchived)
	}
}

func TestHydrateMissingFileRowAndBlob(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mark := func(id int64, day string) {
		if _, err := st.db.Exec(`INSERT INTO body_archive(event_id, day, archived_at, body_mask) VALUES (?,?,?,7)`, id, day, 1); err != nil {
			t.Fatal(err)
		}
		st.db.Exec("UPDATE events SET req_body = NULL, resp_body = NULL WHERE id = ?", id)
	}

	// Marker, no day file at all.
	a, _, _ := st.InsertEvent(ctx, fullEvent("r-nofile"))
	mark(a, "2026-01-01")
	// Day file exists but holds no row for the event.
	b, _, _ := st.InsertEvent(ctx, fullEvent("r-norow"))
	if err := st.writeDayRows("2026-01-02", []archiveRow{{EventID: 999_999, ReqBody: []byte("other")}}); err != nil {
		t.Fatal(err)
	}
	mark(b, "2026-01-02")
	// Row exists but its recorded length lies: an undecodable blob.
	c, _, _ := st.InsertEvent(ctx, fullEvent("r-badblob"))
	if err := st.writeDayRows("2026-01-03", []archiveRow{{EventID: c, ReqBody: bytes.Repeat([]byte("z"), 5000)}}); err != nil {
		t.Fatal(err)
	}
	w := rawDB(t, st.dayPath("2026-01-03"))
	w.Exec("UPDATE bodies SET req_len = 3")
	w.Close()
	mark(c, "2026-01-03")

	for name, id := range map[string]int64{"no file": a, "no row": b, "bad blob": c} {
		got, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatalf("%s: GetEvent must not fail on a broken archive: %v", name, err)
		}
		if got.BodiesArchived != "missing" {
			t.Errorf("%s: BodiesArchived = %q, want missing", name, got.BodiesArchived)
		}
	}
}

func TestListEventsFullOpensOneFilePerDayAndSkipHydrateOpensNone(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 6; i++ {
		ev := fullEvent("r-list-" + string(rune('a'+i)))
		ev.StartedAt = time.Unix(1700000000+int64(i), 0)
		insertArchived(t, st, ev.RequestID, "2026-09-01", ev, archiveRow{ReqBody: []byte("body")})
	}
	st.arch.closeAll()
	st.arch.opens = 0

	skipped, err := st.ListEventsFull(context.Background(), EventFilter{SkipHydrate: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.arch.opens != 0 {
		t.Errorf("SkipHydrate opened %d archive file(s), want 0", st.arch.opens)
	}
	for _, e := range skipped {
		if len(e.ReqBody) != 0 || e.BodiesArchived != "" {
			t.Fatalf("SkipHydrate hydrated a row: %+v", e.BodiesArchived)
		}
	}

	got, err := st.ListEventsFull(context.Background(), EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if st.arch.opens != 1 {
		t.Errorf("a six-row list on one archived day opened %d files, want 1", st.arch.opens)
	}
	for _, e := range got {
		if string(e.ReqBody) != "body" || e.BodiesArchived != "restored" {
			t.Fatalf("row %d not hydrated: %q %q", e.ID, e.ReqBody, e.BodiesArchived)
		}
	}
}

func TestArchiveHandleCacheEvictsBeyondItsCap(t *testing.T) {
	st := newTestStore(t)
	for i := 1; i <= archiveHandleCap+2; i++ {
		day := "2026-09-0" + string(rune('0'+i))
		if err := st.writeDayRows(day, []archiveRow{{EventID: 1, ReqBody: []byte("x")}}); err != nil {
			t.Fatal(err)
		}
		if _, err := readOneDayRow(t, st, day, 1); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(st.arch.dbs); n != archiveHandleCap {
		t.Errorf("cache holds %d handles, want the cap %d", n, archiveHandleCap)
	}
}

func TestEventJSONCarriesBodiesArchivedButNotTheMask(t *testing.T) {
	ev := Event{ArchivedBodyMask: 7}
	ev.BodiesArchived = "restored"
	b, _ := json.Marshal(ev)
	if strings.Contains(string(b), "ArchivedBodyMask") {
		t.Errorf("ArchivedBodyMask leaked onto the wire: %s", b)
	}
	if !strings.Contains(string(b), `"BodiesArchived":"restored"`) {
		t.Errorf("BodiesArchived missing from the wire form: %s", b)
	}
}

func TestArchiveFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are a no-op on Windows; the archive is exactly as protected as lens.db")
	}
	st := newTestStore(t)
	if err := st.writeDayRows("2026-09-01", []archiveRow{{EventID: 1, ReqBody: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(st.archiveDir); fi.Mode().Perm() != 0o700 {
		t.Errorf("archive dir mode = %v, want 0700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(st.dayPath("2026-09-01")); fi.Mode().Perm() != 0o600 {
		t.Errorf("day file mode = %v, want 0600", fi.Mode().Perm())
	}
}
