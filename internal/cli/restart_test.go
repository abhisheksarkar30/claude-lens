package cli

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every test here uses ephemeral ports and a temp DB. None may touch
// 127.0.0.1:8797: shutting that down kills the operator's live Claude session.

// fakeServe stands in for a running `clens serve`: a dashboard answering
// /api/health and /api/shutdown, and a proxy port that only accepts.
type fakeServe struct {
	dash, proxy net.Listener
	srv         *http.Server
	stubborn    bool // shutdown is acknowledged but the ports stay open
	onStop      func()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func startFake(t *testing.T, dashAddr, proxyAddr string, stubborn bool, onStop func()) *fakeServe {
	t.Helper()
	f := &fakeServe{stubborn: stubborn, onStop: onStop}
	var err error
	if f.dash, err = net.Listen("tcp", dashAddr); err != nil {
		t.Fatal(err)
	}
	if f.proxy, err = net.Listen("tcp", proxyAddr); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) })
	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"shutting_down":true}`))
		if !f.stubborn {
			go f.stop()
		}
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(f.dash)
	go func() {
		for {
			c, err := f.proxy.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeServe) stop() {
	f.srv.Close()
	f.proxy.Close()
	if f.onStop != nil {
		f.onStop()
	}
}

type spawnCall struct {
	exe  string
	args []string
	log  string
}

type restartRig struct {
	t           *testing.T
	db          string
	dash, proxy string
	flags       []string
	calls       []spawnCall
}

func newRig(t *testing.T) *restartRig {
	t.Helper()
	dir := t.TempDir()
	r := &restartRig{t: t, db: filepath.Join(dir, "lens.db"), dash: freeAddr(t)}
	r.proxy = freeAddr(t)
	r.flags = []string{"--dashboard-addr", r.dash, "--proxy-addr", r.proxy, "--db-path", r.db}
	return r
}

// spawner returns a spawn seam: healthy[exe] says whether that exe brings a
// fake serve up on the rig's ports.
func (r *restartRig) spawner(healthy map[string]bool) spawner {
	return func(exe string, args []string, logPath string) (int, func(), error) {
		r.calls = append(r.calls, spawnCall{exe, args, logPath})
		if healthy[exe] {
			startFake(r.t, r.dash, r.proxy, false, nil)
		}
		return 1234, func() {}, nil
	}
}

func (r *restartRig) writeState(exe string, args ...string) {
	r.t.Helper()
	s := serveState{PID: 1, Exe: exe, Args: args, DashboardAddr: r.dash, ProxyAddr: r.proxy, LogPath: filepath.Join(filepath.Dir(r.db), "serve.log")}
	if err := writeServeState(r.db, s); err != nil {
		r.t.Fatal(err)
	}
}

func (r *restartRig) startRunning() {
	startFake(r.t, r.dash, r.proxy, false, func() { removeServeState(r.db, 1) })
}

func runRig(r *restartRig, sp spawner, extra ...string) (string, error) {
	var out bytes.Buffer
	args := append(append([]string{}, extra...), r.flags...)
	err := runRestart(args, &out, sp)
	return out.String(), err
}

func TestRestartRunningReusesRecordedExeAndArgs(t *testing.T) {
	r := newRig(t)
	r.writeState("old.exe", "serve", "--replay")
	r.startRunning()
	out, err := runRig(r, r.spawner(map[string]bool{"old.exe": true}), "--timeout", "10s")
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if len(r.calls) != 1 || r.calls[0].exe != "old.exe" {
		t.Fatalf("calls = %+v, want one spawn of old.exe", r.calls)
	}
	if got := r.calls[0].args; !reflect.DeepEqual(got, []string{"serve", "--replay"}) {
		t.Fatalf("spawn args = %v, want the recorded args with exactly one leading serve", got)
	}
	for _, want := range []string{"restarted", "pid 1234", "old.exe", "proxy gap"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRestartNotRunningStartsIt(t *testing.T) {
	r := newRig(t)
	exe, _ := os.Executable()
	out, err := runRig(r, r.spawner(map[string]bool{exe: true}), "--timeout", "10s")
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if !strings.Contains(out, "was not running") {
		t.Errorf("output does not say it was not running:\n%s", out)
	}
	if len(r.calls) != 1 || r.calls[0].args[0] != "serve" {
		t.Fatalf("calls = %+v, want one spawn whose args begin with serve", r.calls)
	}
	if n := countArg(r.calls[0].args, "serve"); n != 1 {
		t.Fatalf("spawn args %v contain serve %d times", r.calls[0].args, n)
	}
	// The forwarded config flags reach the new serve.
	if !containsSeq(r.calls[0].args, "--db-path", r.db) {
		t.Errorf("spawn args %v dropped the forwarded flags", r.calls[0].args)
	}
}

func TestRestartWithExeSwapsBinary(t *testing.T) {
	r := newRig(t)
	r.writeState("old.exe", "serve", "--replay")
	r.startRunning()
	out, err := runRig(r, r.spawner(map[string]bool{"new.exe": true}), "--exe", "new.exe", "--timeout", "10s")
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	if r.calls[0].exe != "new.exe" || !reflect.DeepEqual(r.calls[0].args, []string{"serve", "--replay"}) {
		t.Fatalf("spawn = %+v, want new.exe with the recorded args", r.calls[0])
	}
}

func TestRestartUnhealthyNewExeRollsBackFromRetainedArgs(t *testing.T) {
	r := newRig(t)
	r.writeState("old.exe", "serve", "--replay")
	r.startRunning() // its stop removes the state file, as a real serve does
	out, err := runRig(r, r.spawner(map[string]bool{"old.exe": true}), "--exe", "new.exe", "--timeout", "1s")
	if err == nil || !strings.Contains(err.Error(), "PREVIOUS binary old.exe") {
		t.Fatalf("err = %v, want a rollback naming old.exe\n%s", err, out)
	}
	if len(r.calls) != 2 || r.calls[1].exe != "old.exe" {
		t.Fatalf("calls = %+v, want new.exe then old.exe", r.calls)
	}
	// The state file was deleted by the graceful exit; the rollback must have
	// used the args retained beforehand, not a re-read.
	if !reflect.DeepEqual(r.calls[1].args, []string{"serve", "--replay"}) {
		t.Fatalf("rollback args = %v, want the retained args", r.calls[1].args)
	}
	if _, ok, _ := readServeState(r.db); ok {
		t.Fatal("state file still present: the fake did not model the graceful exit")
	}
}

func TestRestartRollbackImpossibleWithoutPreviousExe(t *testing.T) {
	r := newRig(t)
	r.startRunning() // running, but no state file was ever written
	out, err := runRig(r, r.spawner(nil), "--exe", "new.exe", "--timeout", "1s")
	if err == nil || !strings.Contains(err.Error(), "previous exe is unknown") {
		t.Fatalf("err = %v, want it to say the previous exe is unknown\n%s", err, out)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %+v, want exactly the failed spawn", r.calls)
	}
}

func TestRestartTimesOutWaitingForPortsToClose(t *testing.T) {
	r := newRig(t)
	r.writeState("old.exe", "serve")
	startFake(t, r.dash, r.proxy, true, nil)
	_, err := runRig(r, r.spawner(nil), "--timeout", "500ms")
	if err == nil || !strings.Contains(err.Error(), "release its ports") {
		t.Fatalf("err = %v, want a clear port-release timeout", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("spawned despite the old process still holding its ports: %+v", r.calls)
	}
}

func TestRestartRejectsBadTimeout(t *testing.T) {
	r := newRig(t)
	if _, err := runRig(r, r.spawner(nil), "--timeout", "soon"); err == nil {
		t.Fatal("a non-duration --timeout must be an error")
	}
}

func countArg(args []string, a string) (n int) {
	for _, x := range args {
		if x == a {
			n++
		}
	}
	return
}

func containsSeq(args []string, a, b string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return true
		}
	}
	return false
}

// Helper-process pattern: the test binary re-executes itself as one of two
// roles, so the detach flags can be proven against a real exec.
const helperEnv = "CLENS_RESTART_HELPER"

// TestHelperParent spawns a sleeper through spawnDetached, prints its pid and
// exits -- the parent-exit half of "the child survives its parent".
func TestHelperParent(t *testing.T) {
	if os.Getenv(helperEnv) != "parent" {
		t.Skip("helper role")
	}
	os.Setenv(helperEnv, "sleeper")
	pid, _, err := spawnDetached(os.Args[0], []string{"-test.run=TestHelperSleeper"}, filepath.Join(os.TempDir(), "clens-restart-helper.log"))
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	fmt.Println("PID", pid)
}

func TestHelperSleeper(t *testing.T) {
	if os.Getenv(helperEnv) != "sleeper" {
		t.Skip("helper role")
	}
	time.Sleep(30 * time.Second)
}

func TestSpawnDetachedChildSurvivesParentExit(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperParent", "-test.v")
	cmd.Env = append(os.Environ(), helperEnv+"=parent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parent: %v\n%s", err, out)
	}
	var pid int
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "PID "); ok {
			pid, _ = strconv.Atoi(v)
		}
	}
	if pid == 0 {
		t.Fatalf("no child pid in parent output:\n%s", out)
	}
	// The parent has exited (CombinedOutput waited). Kill succeeding means the
	// child was still alive; it is also the cleanup.
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("child %d is gone after its parent exited: %v", pid, err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("child %d did not survive its parent's exit: %v", pid, err)
	}
}
