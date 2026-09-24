package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// br-GI-16-08: the writer commands' reports name archived rows and the remedy.

// archivedProxyRow seeds a proxy row aged 10 days with bodies and archives it.
func archivedProxyRow(t *testing.T, st *store.Store, requestID string, resp []byte, respHeaders string) int64 {
	t.Helper()
	id := seedProxyRow(t, st, &store.Event{
		EventSummary: store.EventSummary{RequestID: requestID, StartedAt: time.Now().Add(-10 * 24 * time.Hour), CaptureComplete: true},
		ReqBody:      []byte(`{"model":"m","tools":[]}`), RespBody: resp, RespHeaders: respHeaders,
	})
	res, err := st.NewArchiver(func() int { return 1 }).Run(context.Background())
	if err != nil || res.Archived != 1 {
		t.Fatalf("archive: %+v, %v", res, err)
	}
	return id
}

func TestRekeyReportsSkippedArchivedRows(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	archivedProxyRow(t, st, "proxy:arch", nonStreamMessageBody("msg_arch"), mustHeaderJSON(t, jsonContentType()))

	var buf bytes.Buffer
	if err := runRekey([]string{"--dry-run"}, &buf); err != nil {
		t.Fatalf("runRekey --dry-run: %v", err)
	}
	if !strings.Contains(buf.String(), "would skip 1 archived row(s)") || !strings.Contains(buf.String(), "clens archive restore") {
		t.Fatalf("dry-run output lacks the skip line: %s", buf.String())
	}
	buf.Reset()
	if err := runRekey([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runRekey --yes: %v", err)
	}
	if !strings.Contains(buf.String(), "pass 1 skipped 1 archived row(s)") {
		t.Fatalf("live output lacks the skip line: %s", buf.String())
	}
	if findEventByRequestID(t, st, "proxy:arch") == nil {
		t.Fatal("archived row was re-keyed")
	}
}

func TestReflagReportsExcludedArchivedRows(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	archivedProxyRow(t, st, "req-reflag", nil, "")

	var buf bytes.Buffer
	if err := runReflag([]string{"--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "excluded 1 archived row(s)") {
		t.Fatalf("reflag output lacks the exclusion line: %s", buf.String())
	}
}

func TestPurgeDryRunReportsSkippedDayFiles(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	archivedProxyRow(t, st, "req-purge", nil, "")
	files, _ := filepath.Glob(filepath.Join(home, ".clens", "archive", "bodies-*.db"))
	if len(files) != 1 {
		t.Fatalf("want one day file under the DB dir, got %v", files)
	}
	st.Close() // release the cached handle so the file can be removed on Windows
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runPurge([]string{"--older-than", "1d", "--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1 archived day file(s) were missing") {
		t.Fatalf("dry-run lacks the skipped-day note: %s", buf.String())
	}
}

// A rebuild re-reads every transcript from byte 0 and merges into the existing
// rows; an archived row must keep its bodies through it.
func TestIngestRebuildKeepsArchivedBodies(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := archivedProxyRow(t, st, "req_a", []byte(`{"resp":true}`), "")

	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s1","requestId":"req_a","message":{"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "session1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runIngest([]string{"--rebuild"}, &buf); err != nil {
		t.Fatalf("runIngest --rebuild: %v\n%s", err, buf.String())
	}

	got, err := st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.ReqBody) != `{"model":"m","tools":[]}` || string(got.RespBody) != `{"resp":true}` {
		t.Fatalf("archived bodies lost across a rebuild: req=%q resp=%q archived=%q", got.ReqBody, got.RespBody, got.BodiesArchived)
	}
	if n, _ := st.CountEvents(context.Background(), store.EventFilter{}); n != 1 {
		t.Fatalf("rebuild produced %d rows, want the one merged row", n)
	}
	if !strings.Contains(strings.Join(got.SourceRefs, ","), "jsonl") {
		t.Fatalf("the transcript did not merge into the archived row: refs=%v", got.SourceRefs)
	}
}
