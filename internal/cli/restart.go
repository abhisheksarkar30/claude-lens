package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
)

const (
	defaultRestartTimeout = 30 * time.Second
	restartPoll           = 100 * time.Millisecond
	// logTailBytes is how much of the serve log is echoed when a start fails.
	logTailBytes = 2048
)

// spawner starts exe with args detached, appending its output to logPath, and
// returns its pid plus a kill func for rollback. A seam so tests do not exec.
type spawner func(exe string, args []string, logPath string) (pid int, kill func(), err error)

// Restart is `clens restart [--exe PATH] [--timeout 30s]`: stop the running
// serve, wait for its ports to free, relaunch it detached with the recorded
// flags, and report how long the proxy was down. On Windows a running
// clens.exe cannot be replaced, and `shutdown` alone leaves a live Claude
// session with no proxy -- this is the one command that closes that gap.
func Restart(args []string) error {
	return runRestart(args, os.Stdout, spawnDetached)
}

func spawnDetached(exe string, args []string, logPath string) (int, func(), error) {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, nil, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer log.Close() // the child holds its own handle
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	p := cmd.Process
	return p.Pid, func() { _ = p.Kill() }, nil
}

func runRestart(args []string, w io.Writer, spawn spawner) error {
	// restart's own flags come out first: config.Load's FlagSet is closed, so
	// an unknown --exe/--timeout would be a returned error.
	newExe, rest := takeFlag(args, "--exe")
	timeoutStr, rest := takeFlag(rest, "--timeout")
	timeout := defaultRestartTimeout
	if timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil || d <= 0 {
			return fmt.Errorf("restart: bad --timeout %q", timeoutStr)
		}
		timeout = d
	}
	cfg, err := config.Load(rest)
	if err != nil {
		return fmt.Errorf("restart: %w", err)
	}
	dash := dialableDashboardAddr(cfg.DashboardAddr)
	proxy := dialableDashboardAddr(cfg.ProxyAddr)

	// Retained now: the file is deleted by the graceful exit step 3 drives.
	prev, havePrev, err := readServeState(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(w, "restart: ignoring unreadable %s: %v\n", serveStateFile, err)
	}

	exe, spawnArgs, logPath := newExe, rest, defaultLogPath(cfg.DBPath)
	if havePrev {
		spawnArgs, logPath = prev.Args, prev.LogPath
		if logPath == "" {
			logPath = defaultLogPath(cfg.DBPath)
		}
		if exe == "" {
			exe = prev.Exe
		}
	} else {
		spawnArgs = append([]string{"serve"}, rest...)
	}
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return fmt.Errorf("restart: no exe to start (pass --exe): %w", err)
		}
	}

	// Liveness is decided by health, never by the file or a pid.
	wasRunning := healthy(dash)
	begin := time.Now()
	if wasRunning {
		if err := runShutdown(rest, io.Discard); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
		if err := waitClosed(timeout, dash, proxy); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
	} else {
		fmt.Fprintln(w, "was not running; starting it")
	}

	pid, kill, err := spawn(exe, spawnArgs, logPath)
	if err == nil && waitHealthy(dash, timeout) {
		fmt.Fprintf(w, "restarted: pid %d, exe %s, log %s, proxy gap %s\n",
			pid, exe, logPath, time.Since(begin).Round(time.Millisecond))
		return nil
	}
	if err != nil {
		err = fmt.Errorf("spawn %s: %w", exe, err)
	} else {
		err = fmt.Errorf("%s did not become healthy within %s", exe, timeout)
		kill()
	}
	printLogTail(w, logPath)

	// Rollback only makes sense for a swapped-in binary replacing a running one.
	if newExe == "" || !wasRunning {
		return fmt.Errorf("restart: %w; nothing is serving", err)
	}
	if !havePrev || prev.Exe == "" {
		return fmt.Errorf("restart: %w; the previous exe is unknown, so nothing was rolled back and nothing is serving", err)
	}
	fmt.Fprintf(w, "rolling back to %s\n", prev.Exe)
	if _, _, rerr := spawn(prev.Exe, prev.Args, prev.LogPath); rerr == nil && waitHealthy(dash, timeout) {
		return fmt.Errorf("restart: %w; rolled back, the PREVIOUS binary %s is serving", err, prev.Exe)
	}
	printLogTail(w, prev.LogPath)
	return fmt.Errorf("restart: %w; rollback to %s also failed, nothing is serving", err, prev.Exe)
}

func healthy(addr string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitHealthy(addr string, timeout time.Duration) bool {
	return poll(timeout, func() bool { return healthy(addr) })
}

// waitClosed waits until every address refuses a dial: proof the drain
// finished and the store is closed, not merely that shutdown was requested.
func waitClosed(timeout time.Duration, addrs ...string) error {
	ok := poll(timeout, func() bool {
		for _, a := range addrs {
			if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
				c.Close()
				return false
			}
		}
		return true
	})
	if !ok {
		return errors.New("timed out waiting for the old process to release its ports")
	}
	return nil
}

func poll(timeout time.Duration, cond func() bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(restartPoll):
		}
	}
}

func printLogTail(w io.Writer, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > logTailBytes {
		f.Seek(-logTailBytes, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	fmt.Fprintf(w, "--- tail of %s ---\n%s\n", path, b)
}
