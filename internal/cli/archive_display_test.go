package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// br-GI-16-10: the archived state is shown, never mistaken for "not captured".

func TestShowNamesTheArchivedState(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := archivedProxyRow(t, st, "req-show", nil, "")
	day := time.Now().UTC().Add(-10 * 24 * time.Hour).Format("2006-01-02")
	arg := strconv.FormatInt(id, 10)

	var buf bytes.Buffer
	if err := runShow([]string{arg, "--body"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "bodies loaded from the archive ("+day+")") {
		t.Fatalf("restored row output lacks the message:\n%s", buf.String())
	}

	// Remove the day file: the row must say the archive is missing, not "(not stored)".
	files, _ := filepath.Glob(filepath.Join(home, ".clens", "archive", "bodies-*.db"))
	st.Close()
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	buf.Reset()
	if err := runShow([]string{arg, "--body"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "archived — archive file for "+day+" not found") {
		t.Fatalf("missing row output lacks the message:\n%s", out)
	}
	if strings.Contains(out, "not stored") || strings.Contains(out, "not captured") {
		t.Fatalf("a missing archive was rendered as an absent capture:\n%s", out)
	}
}

func TestShowSaysNothingForAHotRow(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	id := seedAgedProxyRow(t, st, "req-hot", time.Hour)
	var buf bytes.Buffer
	if err := runShow([]string{strconv.FormatInt(id, 10), "--body"}, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "archive") {
		t.Fatalf("a hot row mentions the archive:\n%s", buf.String())
	}
}

// The additive wire contract: ls --json and export carry BodiesArchived and
// never the internal ArchivedBodyMask.
func TestJSONOutputsCarryBodiesArchivedNotTheMask(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	archivedProxyRow(t, st, "req-json", nil, "")
	_ = st

	for name, run := range map[string]func(w *bytes.Buffer) error{
		"ls --json": func(w *bytes.Buffer) error { return runLs([]string{"--json"}, w) },
		"export":    func(w *bytes.Buffer) error { return runExport(nil, w) },
	} {
		var buf bytes.Buffer
		if err := run(&buf); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(buf.String(), `"BodiesArchived":"restored"`) {
			t.Errorf("%s lacks BodiesArchived:\n%s", name, buf.String())
		}
		if strings.Contains(buf.String(), "ArchivedBodyMask") {
			t.Errorf("%s leaks ArchivedBodyMask", name)
		}
	}
	_ = context.Background
}
