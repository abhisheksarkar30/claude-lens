package cli

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// TestBackfillToolNamesFillsOnlyUnsetRows: a fixture with one already-set row
// asserts it is untouched, and a row simulating pre-migration history (a
// request body present, req_tool_names not yet computed) is filled from its
// stored body. A second run changes nothing.
func TestBackfillToolNamesFillsOnlyUnsetRows(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	alreadySet := &store.Event{
		EventSummary: store.EventSummary{
			RequestID: "req-backfill-already-set",
			ToolNames: store.EncodeToolNames([]string{"already"}),
		},
		ReqBody: []byte(`{"tools":[{"name":"already"}]}`),
	}
	idSet := seedEvent(t, st, alreadySet)

	// Simulate a pre-migration row: a real request body, but req_tool_names
	// not yet computed -- the shape every row written before br-GI-13-07 is
	// in. seedEvent's InsertEvent writes req_tool_names from ToolNames
	// unconditionally, so this needs its own UPDATE against a second
	// connection to reach that state.
	unset := &store.Event{
		EventSummary: store.EventSummary{RequestID: "req-backfill-unset"},
		ReqBody:      []byte(`{"tools":[{"name":"bash"},{"name":"read"}]}`),
	}
	idUnset := seedEvent(t, st, unset)

	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+cfg.DBPath)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	if _, err := raw.Exec("UPDATE events SET req_tool_names = NULL WHERE id = ?", idUnset); err != nil {
		raw.Close()
		t.Fatalf("clear req_tool_names: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw connection: %v", err)
	}

	var buf bytes.Buffer
	if err := runBackfillToolNames([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runBackfillToolNames: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "filled 1 row(s)") {
		t.Fatalf("output = %q, want it to report filling exactly 1 row", buf.String())
	}

	gotSet, err := st.GetEvent(ctx, idSet)
	if err != nil {
		t.Fatalf("GetEvent(alreadySet): %v", err)
	}
	if gotSet.ToolNames != `["already"]` {
		t.Errorf("already-set row's ToolNames = %q, want it untouched by the backfill", gotSet.ToolNames)
	}

	gotUnset, err := st.GetEvent(ctx, idUnset)
	if err != nil {
		t.Fatalf("GetEvent(unset): %v", err)
	}
	if gotUnset.ToolNames != `["bash","read"]` {
		t.Errorf("backfilled row's ToolNames = %q, want the tools parsed out of its stored req_body", gotUnset.ToolNames)
	}

	buf.Reset()
	if err := runBackfillToolNames([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("second runBackfillToolNames: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "filled 0 row(s)") {
		t.Errorf("second run output = %q, want filled 0 row(s) (idempotent)", buf.String())
	}
}

// TestBackfillToolNamesWithoutYesRefuses: neither flag, no rewrite -- the
// same default as rekey/reprice/reflag.
func TestBackfillToolNamesWithoutYesRefuses(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runBackfillToolNames(nil, &buf); err == nil {
		t.Fatal("runBackfillToolNames without --yes/--dry-run = nil error, want a refusal")
	}
}
