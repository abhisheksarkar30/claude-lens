package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// serveState is the record a running `serve` leaves next to the live DB so a
// separate `clens restart` invocation can relaunch what is running: flags are
// not in config, so this file is the only place they survive. It holds paths
// and addresses only, never a credential.
//
// A leftover file is a hint, never the truth -- a killed process leaves one
// behind -- so nothing here decides liveness.
type serveState struct {
	PID           int       `json:"pid"`
	Exe           string    `json:"exe"`
	Args          []string  `json:"args"` // os.Args[1:], so it begins with the subcommand
	StartedAt     time.Time `json:"started_at"`
	ProxyAddr     string    `json:"proxy_addr"`
	DashboardAddr string    `json:"dashboard_addr"`
	LogPath       string    `json:"log_path"`
}

const serveStateFile = "serve.state.json"

// statePath is where the state file lives: the directory of the live DB.
func statePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), serveStateFile)
}

// defaultLogPath is the serve log that already sits next to the live DB.
func defaultLogPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "serve.log")
}

// writeServeState writes atomically (temp file + rename) so a concurrent
// restart never reads a torn file.
func writeServeState(dbPath string, s serveState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	path := statePath(dbPath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readServeState reports (_, false, nil) when there is no file, and an error
// when there is one that does not parse.
func readServeState(dbPath string) (serveState, bool, error) {
	var s serveState
	b, err := os.ReadFile(statePath(dbPath))
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, fmt.Errorf("parse %s: %w", statePath(dbPath), err)
	}
	return s, true, nil
}

// removeServeState deletes the file only if it is still this process's: a
// restarted successor may already have rewritten it while this one finishes
// draining, and removing that would strand the new process without a record.
func removeServeState(dbPath string, pid int) {
	if s, ok, err := readServeState(dbPath); err == nil && ok && s.PID != pid {
		return
	}
	os.Remove(statePath(dbPath))
}
