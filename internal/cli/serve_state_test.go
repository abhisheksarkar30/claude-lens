package cli

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestServeStateRoundTrip(t *testing.T) {
	db := filepath.Join(t.TempDir(), "lens.db")
	want := serveState{
		PID: 4242, Exe: "D:/clens/clens.exe", Args: []string{"serve", "--replay"},
		StartedAt: time.Date(2026, 9, 24, 6, 22, 0, 0, time.UTC),
		ProxyAddr: "127.0.0.1:8797", DashboardAddr: "0.0.0.0:8798", LogPath: "D:/clens/serve.log",
	}
	if err := writeServeState(db, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readServeState(db)
	if err != nil || !ok {
		t.Fatalf("readServeState = ok %v, err %v", ok, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost a field:\n got %+v\nwant %+v", got, want)
	}
	if got.Args[0] != "serve" {
		t.Fatalf("args must begin with the subcommand, got %v", got.Args)
	}
	// Atomic write: the temp file is renamed away, never left visible.
	if _, err := os.Stat(statePath(db) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind after write: %v", err)
	}
}

func TestServeStateMissingAndGarbage(t *testing.T) {
	db := filepath.Join(t.TempDir(), "lens.db")
	if _, ok, err := readServeState(db); ok || err != nil {
		t.Fatalf("missing file: ok %v, err %v; want not-present and no error", ok, err)
	}
	removeServeState(db, 1) // no-op on a missing file
	if err := os.WriteFile(statePath(db), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readServeState(db); err == nil {
		t.Fatal("garbage JSON must be an error")
	}
}

// A successor may have rewritten the file while its predecessor is still
// draining; the predecessor's cleanup must not delete the successor's record.
func TestRemoveServeStateLeavesAnotherProcessesFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "lens.db")
	if err := writeServeState(db, serveState{PID: 2}); err != nil {
		t.Fatal(err)
	}
	removeServeState(db, 1)
	if _, ok, _ := readServeState(db); !ok {
		t.Fatal("removeServeState deleted a file owned by a different pid")
	}
	removeServeState(db, 2)
	if _, ok, _ := readServeState(db); ok {
		t.Fatal("removeServeState left this process's own file")
	}
}

// Serve is driven in-process on ephemeral ports (never 127.0.0.1:8797) and
// stopped through POST /api/shutdown, the same path `clens shutdown` uses.
func TestServeWritesAndRemovesStateFile(t *testing.T) {
	home := withHome(t)
	db := filepath.Join(home, "lens.db")
	done := make(chan error, 1)
	go func() {
		done <- Serve([]string{"--proxy-addr", "127.0.0.1:0", "--dashboard-addr", "127.0.0.1:0", "--db-path", db})
	}()

	var s serveState
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, ok, err := readServeState(db)
		if err == nil && ok {
			s = got
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Serve returned before writing state: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("state file never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if s.PID != os.Getpid() || s.Exe == "" || len(s.Args) == 0 || s.StartedAt.IsZero() {
		t.Fatalf("state is missing fields: %+v", s)
	}
	if s.LogPath != filepath.Join(home, "serve.log") {
		t.Fatalf("log_path = %q, want the serve.log next to the DB", s.LogPath)
	}
	// The recorded addresses are the real bound ones, not the configured ":0".
	for _, a := range []string{s.ProxyAddr, s.DashboardAddr} {
		if _, port, err := net.SplitHostPort(a); err != nil || port == "0" {
			t.Fatalf("recorded address %q is not a real bound address", a)
		}
	}

	resp, err := http.Post("http://"+s.DashboardAddr+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not stop after /api/shutdown")
	}
	if _, ok, _ := readServeState(db); ok {
		t.Fatal("state file survived graceful shutdown")
	}
}

func TestServeBindFailureWritesNoStateFile(t *testing.T) {
	home := withHome(t)
	db := filepath.Join(home, "lens.db")
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	err = Serve([]string{"--proxy-addr", busy.Addr().String(), "--dashboard-addr", "127.0.0.1:0", "--db-path", db})
	if err == nil {
		t.Fatal("Serve must fail when the proxy address is taken")
	}
	if _, ok, _ := readServeState(db); ok {
		t.Fatal("state file written despite a failed bind")
	}
}
