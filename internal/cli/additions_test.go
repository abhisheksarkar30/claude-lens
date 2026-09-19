package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

func writeAccountsFile(t *testing.T, home, content string) {
	t.Helper()
	clensDir := filepath.Join(home, ".clens")
	if err := os.MkdirAll(clensDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clensDir, "accounts.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T, home string) *store.Store {
	t.Helper()
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIngestDispatchesCleanOnEmptyInstall(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runIngest(nil, &buf); err != nil {
		t.Fatalf("runIngest: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "jsonl:") {
		t.Fatalf("output missing jsonl summary line: %s", buf.String())
	}
}

func TestIngestRebuildRereadsWithoutDuplicating(t *testing.T) {
	home := withHome(t)
	projectDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projectDir, "session1.jsonl")
	line := `{"type":"assistant","sessionId":"s1","requestId":"req_a","message":{"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runIngest(nil, &buf); err != nil {
		t.Fatalf("runIngest 1: %v\noutput:\n%s", err, buf.String())
	}

	st := openTestStore(t, home)
	n, err := st.CountEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if n != 1 {
		t.Fatalf("event count after first ingest = %d, want 1", n)
	}

	buf.Reset()
	if err := runIngest([]string{"--rebuild"}, &buf); err != nil {
		t.Fatalf("runIngest --rebuild: %v\noutput:\n%s", err, buf.String())
	}
	n, err = st.CountEvents(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatalf("CountEvents after rebuild: %v", err)
	}
	if n != 1 {
		t.Fatalf("event count after --rebuild re-read = %d, want 1 (request_id UNIQUE absorbs the re-read)", n)
	}
}

func TestRefreshDispatchesCleanOnEmptyInstall(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runRefresh(nil, &buf); err != nil {
		t.Fatalf("runRefresh: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "jsonl") {
		t.Fatalf("output missing a jsonl line: %s", buf.String())
	}
}

// Test 17's CLI half: a failing source is a non-zero exit and a named
// line, and the other sources still ran.
func TestRefreshFailsWhenASourceFails(t *testing.T) {
	home := withHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	orig := claudeAIUsageURL
	claudeAIUsageURL = srv.URL
	defer func() { claudeAIUsageURL = orig }()

	// secret.Save before writeAccountsFile: protecting ~/.clens for a
	// credential locks down the directory's own ACL, and NTFS propagates
	// that to any *existing* child's inherited ACEs -- writing
	// accounts.toml first would otherwise make it unreadable as a side
	// effect (see the same note on TestAccountsNeverPrintsCredentialValues).
	if err := secret.Save("sessionKey", "fake-session-cookie-value"); err != nil {
		t.Fatal(err)
	}
	writeAccountsFile(t, home, "name = work\nbilling_mode = subscription\nplan = max20x\n")

	var buf bytes.Buffer
	err := runRefresh(nil, &buf)
	if err == nil {
		t.Fatalf("runRefresh: want error when a source fails, output:\n%s", buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "snapshot") || !strings.Contains(out, "FAIL") {
		t.Fatalf("output missing a failing snapshot line: %s", out)
	}
	if !strings.Contains(out, "jsonl") {
		t.Fatalf("jsonl should still have run despite snapshot's failure: %s", out)
	}
}

func TestQuotaReportsUnconfiguredNotAPercentage(t *testing.T) {
	home := withHome(t)
	writeAccountsFile(t, home, "name = work\nbilling_mode = subscription\nplan = max20x\n")

	var buf bytes.Buffer
	if err := runQuota(nil, &buf); err != nil {
		t.Fatalf("runQuota: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "unconfigured") {
		t.Fatalf("output missing 'unconfigured' with no limit set: %s", out)
	}
	if strings.Contains(out, "%") {
		t.Fatalf("output contains a %% with no limit configured, want none: %s", out)
	}
}

func TestModelsMarksUnpricedDistinctly(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()
	_, err := st.InsertEvent(ctx, &store.Event{
		RequestID:      "req_unpriced",
		Source:         "proxy",
		FirstSource:    "proxy",
		BillingMode:    "api",
		ModelRequested: "claude-unknown-model",
		ModelResolved:  "claude-unknown-model",
		CostSource:     "unpriced",
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	var buf bytes.Buffer
	if err := runModels(nil, &buf); err != nil {
		t.Fatalf("runModels: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "claude-unknown-model") || !strings.Contains(out, "unpriced") {
		t.Fatalf("output missing the unpriced observed model labelled distinctly: %s", out)
	}
}

func TestReconcileNeverPrintsASummedFigure(t *testing.T) {
	home := withHome(t)
	_ = openTestStore(t, home)

	var buf bytes.Buffer
	if err := runReconcile(nil, &buf); err != nil {
		t.Fatalf("runReconcile: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "computed_usd") || !strings.Contains(out, "billed_usd") {
		t.Fatalf("output missing separate computed/billed labels: %s", out)
	}
	if !strings.Contains(out, "API accounts only") {
		t.Fatalf("output missing the cost_drift API-accounts-only caveat: %s", out)
	}
}

func TestAccountsRefusesAdminKeyWithoutYes(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	err := runAccounts([]string{"--set-admin-key", "fixture-admin-key-value"}, &buf)
	if err == nil {
		t.Fatal("runAccounts --set-admin-key without --yes: want error")
	}
	if strings.Contains(buf.String(), "fixture-admin-key-value") {
		t.Fatalf("refused admin key must never be echoed: %s", buf.String())
	}
}

func TestAccountsStoresAdminKeyWithYes(t *testing.T) {
	withHome(t)
	var buf bytes.Buffer
	if err := runAccounts([]string{"--set-admin-key", "fixture-admin-key-value", "--yes"}, &buf); err != nil {
		t.Fatalf("runAccounts --set-admin-key --yes: %v", err)
	}
	if !secret.Exists("admin") {
		t.Fatal("admin key was not stored")
	}
	if strings.Contains(buf.String(), "fixture-admin-key-value") {
		t.Fatalf("stored admin key value must never be echoed: %s", buf.String())
	}
}

// Credential containment (test 18's CLI half): no command's output
// contains a stored sessionKey or admin key value.
func TestAccountsNeverPrintsCredentialValues(t *testing.T) {
	home := withHome(t)
	// secret.Save runs before the accounts file is written: Windows ACL
	// protection locks down ~/.clens itself, and NTFS propagates that
	// change to any *existing* child's inherited ACEs -- writing
	// accounts.toml first would otherwise make it unreadable as a side
	// effect of protecting an unrelated credential in the same directory.
	if err := secret.Save("sessionKey", "super-secret-session-cookie"); err != nil {
		t.Fatal(err)
	}
	if err := secret.Save("admin", "fixture-admin-key-super-secret"); err != nil {
		t.Fatal(err)
	}
	writeAccountsFile(t, home, "name = work\nbilling_mode = subscription\nplan = max20x\n")

	var buf bytes.Buffer
	if err := runAccounts(nil, &buf); err != nil {
		t.Fatalf("runAccounts: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if strings.Contains(out, "super-secret-session-cookie") || strings.Contains(out, "fixture-admin-key-super-secret") {
		t.Fatalf("credential value leaked into output: %s", out)
	}
	if !strings.Contains(out, "configured") {
		t.Fatalf("output should still report the credentials as configured: %s", out)
	}
}
