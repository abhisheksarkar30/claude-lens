// Package secret is the only place claude-lens's credentials — the claude.ai
// sessionKey cookie and the Admin API key — are persisted. They live outside
// the database, in ~/.clens/secrets.toml, and nothing else in the tree may
// import this package: the whole accessor surface is defined here so no
// later change has to touch this file to add one.
//
// On POSIX, protection is a file mode (0600, directory 0700) applied with
// os.Chmod. On Windows those bits are not a control — os.Chmod there only
// toggles the read-only attribute — so protection is an explicit ACL applied
// with icacls via os/exec, read back and verified before the write is
// committed. See applyProtection and its Windows half for the mechanism.
package secret

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrUnset is returned by Get when name has never been saved. It is
// distinct from "" because an empty credential is not a configured one.
var ErrUnset = errors.New("secret: not set")

// runICACLS is the seam production points at the real icacls binary through
// and tests substitute to simulate a failing or stubbed ACL tool without
// touching a real file's permissions.
var runICACLS = func(args ...string) (string, error) {
	out, err := exec.Command("icacls", args...).CombinedOutput()
	return string(out), err
}

// currentPrincipal resolves the identity icacls should grant access to, as
// an exec.Command argument — never interpolated into a shell string, so a
// principal containing spaces (a real Windows display name shape) reaches
// icacls intact instead of being split by a shell that was never invoked.
var currentPrincipal = func() (string, error) {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username, nil
	}
	if un := os.Getenv("USERNAME"); un != "" {
		return un, nil
	}
	return "", errors.New("secret: could not resolve the current user principal")
}

func userHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func secretsPath() (string, error) {
	home := userHomeDir()
	if home == "" {
		return "", errors.New("secret: could not resolve a home directory")
	}
	return filepath.Join(home, ".clens", "secrets.toml"), nil
}

// entry is one named credential's on-disk record.
type entry struct {
	Value    string
	LastUsed time.Time
}

// Save persists value under name ("sessionKey" or "admin"), applying the
// platform's real access-control mechanism before the write is visible at
// its final path. A second Save for the same name overwrites the first.
//
// Existing content is written through a temp file in the same directory,
// protected, verified, and only then renamed over the target — the single
// atomic commit point. That ordering is load-bearing: applying an ACL
// in-place, on the live file, can leave a transient zero-ACE DACL if a later
// icacls step in the same sequence fails, stranding an existing credential
// unreadable. A file that does not yet exist has no prior credential or
// permissions to lose, so it is written and protected in place directly;
// if protection fails for a brand-new file, it is removed rather than left
// unprotected.
func Save(name, value string) error {
	if name == "" {
		return errors.New("secret: Save: name must not be empty")
	}
	path, err := secretsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secret: Save: %w", err)
	}
	if err := applyProtection(dir); err != nil {
		return fmt.Errorf("secret: Save: protecting %s: %w", dir, err)
	}

	entries, err := readEntries(path)
	if err != nil {
		return fmt.Errorf("secret: Save: %w", err)
	}
	if entries == nil {
		entries = map[string]entry{}
	}
	e := entries[name]
	e.Value = value
	entries[name] = e
	data := serialize(entries)

	_, statErr := os.Stat(path)
	preexisting := statErr == nil

	if !preexisting {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("secret: Save: %w", err)
		}
		if err := applyProtection(path); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("secret: Save: protecting %s: %w (credential not written)", path, err)
		}
		return nil
	}

	tmp, err := os.CreateTemp(dir, ".secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("secret: Save: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("secret: Save: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("secret: Save: %w", err)
	}
	if err := applyProtection(tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("secret: Save: protecting temp file: %w (existing credential left untouched)", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("secret: Save: committing %s: %w", path, err)
	}
	return nil
}

// Get returns the value saved under name, or ErrUnset if it was never
// saved. A successful Get also records now as the credential's last-used
// time (best-effort: a failure to persist that timestamp does not fail the
// Get itself) — Get is the read collectors use immediately before putting
// the credential to work, so "was read" stands in for "last worked".
func Get(name string) (string, error) {
	path, err := secretsPath()
	if err != nil {
		return "", err
	}
	entries, err := readEntries(path)
	if err != nil {
		return "", fmt.Errorf("secret: Get: %w", err)
	}
	e, ok := entries[name]
	if !ok {
		return "", ErrUnset
	}
	touch(path, entries, name)
	return e.Value, nil
}

// touch best-effort records now as name's last-used time. Errors are
// dropped: the caller already has the value it asked Get for, and a failure
// to persist bookkeeping must never turn a successful read into an error.
func touch(path string, entries map[string]entry, name string) {
	e := entries[name]
	e.LastUsed = time.Now().UTC()
	entries[name] = e
	_ = os.WriteFile(path, serialize(entries), 0o600)
}

// Exists reports whether name has been saved.
func Exists(name string) bool {
	path, err := secretsPath()
	if err != nil {
		return false
	}
	entries, err := readEntries(path)
	if err != nil {
		return false
	}
	_, ok := entries[name]
	return ok
}

// LastUsed returns the last time Get successfully returned name's value, or
// the zero time if it was never saved or never read.
func LastUsed(name string) time.Time {
	path, err := secretsPath()
	if err != nil {
		return time.Time{}
	}
	entries, err := readEntries(path)
	if err != nil {
		return time.Time{}
	}
	return entries[name].LastUsed
}

// ProtectionLevel reports the actually observed protection of secrets.toml —
// never assumed from what Save intended to apply. "none", true means no
// credential file exists yet, which is a legitimate, unprotected-by-nothing
// state, not a failure. A false ok means the file exists but this package
// could not confirm it is protected the way Save's contract requires; kind
// then names what was found (or the verification failure), for doctor to
// show as a FAIL.
func ProtectionLevel() (kind string, ok bool) {
	path, err := secretsPath()
	if err != nil {
		return err.Error(), false
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "none", true
		}
		return err.Error(), false
	}
	if runtime.GOOS == "windows" {
		return windowsProtectionLevel(path)
	}
	return posixProtectionLevel(path)
}

func posixProtectionLevel(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error(), false
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Sprintf("mode %04o is readable or writable by group/other", mode), false
	}
	return fmt.Sprintf("mode %04o", mode), true
}

// applyProtection applies the platform's access-control mechanism to path
// (a file or a directory) and fails closed: a non-zero icacls exit, or a
// read-back that does not match, is reported as an error and nothing about
// path's prior state is assumed fixed.
func applyProtection(path string) error {
	if runtime.GOOS == "windows" {
		_, err := applyWindowsACL(path)
		return err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return os.Chmod(path, 0o700)
	}
	return os.Chmod(path, 0o600)
}

// applyWindowsACL grants the current user Full Control over path and
// removes inherited ACEs, then reads the DACL back and asserts it now
// contains exactly the intended principal. /inheritance:r is load-bearing,
// not redundant: a newly created file inherits its parent directory's ACEs,
// so without it the profile's SYSTEM/Administrators/user entries would
// remain even after /grant:r ran.
func applyWindowsACL(path string) (string, error) {
	principal, err := currentPrincipal()
	if err != nil {
		return "", err
	}
	if _, err := runICACLS(path, "/inheritance:r", "/grant:r", principal+":F"); err != nil {
		return "", fmt.Errorf("secret: icacls grant failed for %s: %w", path, err)
	}
	out, err := runICACLS(path)
	if err != nil {
		return "", fmt.Errorf("secret: icacls read-back failed for %s: %w", path, err)
	}
	principals, err := parseICACLSPrincipals(out)
	if err != nil {
		return "", fmt.Errorf("secret: icacls read-back for %s: %w", path, err)
	}
	if len(principals) != 1 || !strings.EqualFold(principals[0], principal) {
		return "", fmt.Errorf("secret: icacls read-back for %s: want exactly principal %q, got %v", path, principal, principals)
	}
	return out, nil
}

func windowsProtectionLevel(path string) (string, bool) {
	out, err := runICACLS(path)
	if err != nil {
		return fmt.Sprintf("icacls read-back failed: %v", err), false
	}
	principals, err := parseICACLSPrincipals(out)
	if err != nil {
		return fmt.Sprintf("icacls read-back unparsable: %v", err), false
	}
	if len(principals) != 1 {
		return fmt.Sprintf("ACL grants %d principals, want exactly 1: %v", len(principals), principals), false
	}
	return fmt.Sprintf("Windows ACL: %s:(F)", principals[0]), true
}

// parseICACLSPrincipals extracts the distinct principal names granted an
// ACE in icacls's listing output for a single file. icacls prints the file
// path on the first line, then one "  PRINCIPAL:(PERMS)" line per ACE
// (continuation lines for a second permission on the same principal are
// indented further and carry no ':'), and a trailing blank line plus a
// "Successfully processed..." summary this function ignores.
func parseICACLSPrincipals(out string) ([]string, error) {
	var principals []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Successfully processed") {
			continue
		}
		idx := strings.Index(trimmed, ":(")
		if idx < 0 {
			// Either the leading "file path" line or a continuation of the
			// previous ACE's permissions — neither introduces a principal.
			continue
		}
		name := strings.TrimSpace(trimmed[:idx])
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			principals = append(principals, name)
		}
	}
	if len(principals) == 0 {
		return nil, errors.New("no ACE entries found in icacls output")
	}
	return principals, nil
}

// readEntries reads and parses secrets.toml. A missing file is not an
// error — it returns a nil map, the same "nothing saved yet" state Get and
// Exists already handle.
func readEntries(path string) (map[string]entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parse(data)
}

// parse and serialize implement a hand-rolled, section-per-credential flat
// format ("[name]" followed by "key = value" lines) — no TOML dependency
// for a file this small and this security-sensitive to hand-parse.
func parse(data []byte) (map[string]entry, error) {
	entries := map[string]entry{}
	var current string
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := entries[current]; !ok {
				entries[current] = entry{}
			}
			continue
		}
		if current == "" {
			return nil, fmt.Errorf("secret: parse: line %d: value outside any [name] section: %q", i+1, line)
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			return nil, fmt.Errorf("secret: parse: line %d: missing '=': %q", i+1, line)
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"`)
		e := entries[current]
		switch key {
		case "value":
			e.Value = val
		case "last_used":
			if val != "" {
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					e.LastUsed = t
				}
			}
		}
		entries[current] = e
	}
	return entries, nil
}

func serialize(entries map[string]entry) []byte {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	// Deterministic output (sorted) so a round-trip Save without content
	// changes produces byte-identical files, which the fail-closed test
	// relies on.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	var b strings.Builder
	for _, name := range names {
		e := entries[name]
		b.WriteString("[" + name + "]\n")
		b.WriteString(`value = "` + strings.ReplaceAll(e.Value, `"`, `\"`) + "\"\n")
		if !e.LastUsed.IsZero() {
			b.WriteString(`last_used = "` + e.LastUsed.Format(time.RFC3339) + "\"\n")
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}
