package proxy

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
)

// Provider names, as stamped on a request and persisted in the state file.
const (
	ProviderCloud = "cloud"
	ProviderLocal = "local"
)

// Targets is the live-swappable upstream: the cloud API the proxy always had,
// and optionally a local server (LM Studio) the dashboard can switch to. The
// hot path reads Active with one atomic load; Set is the only writer.
type Targets struct {
	cloud, local *url.URL
	path         string     // stateDir/"provider"; "" when stateDir == "", and then nothing touches disk
	mu           sync.Mutex // held inside Set only: serializes the file write and the pointer store
	active       atomic.Pointer[string]
}

// the two possible Active values, so a read never allocates
var (
	activeCloud = ProviderCloud
	activeLocal = ProviderLocal
)

// NewTargets parses both upstream URLs and reads the persisted choice from
// stateDir. It writes nothing. An absent, unreadable or unrecognised state
// file, or "local" with no local URL configured, boots to the cloud: a
// restart must never leave traffic aimed at an upstream that is not there.
func NewTargets(cfg *config.Config, stateDir string) (*Targets, error) {
	cloud, err := url.Parse(cfg.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream URL %q: %w", cfg.UpstreamURL, err)
	}
	t := &Targets{cloud: cloud}
	if cfg.LocalUpstreamURL != "" {
		if t.local, err = url.Parse(cfg.LocalUpstreamURL); err != nil {
			return nil, fmt.Errorf("proxy: invalid local upstream URL %q: %w", cfg.LocalUpstreamURL, err)
		}
	}
	t.active.Store(&activeCloud)
	if stateDir != "" {
		t.path = filepath.Join(stateDir, "provider")
		if b, err := os.ReadFile(t.path); err == nil &&
			strings.TrimSpace(string(b)) == ProviderLocal && t.local != nil {
			t.active.Store(&activeLocal)
		}
	}
	return t, nil
}

// Active is the provider serving new requests: ProviderCloud or ProviderLocal.
func (t *Targets) Active() string { return *t.active.Load() }

// resolve maps a provider name from Active to the URL that serves it.
func (t *Targets) resolve(which string) *url.URL {
	if which == ProviderLocal {
		return t.local
	}
	return t.cloud
}

// Set switches new requests to which, and persists the choice when the
// Targets has a state directory. A request already in flight keeps the
// provider it started with. The file is written before the pointer moves, so
// a failed write leaves both unchanged.
func (t *Targets) Set(which string) error {
	var next *string
	switch which {
	case ProviderCloud:
		next = &activeCloud
	case ProviderLocal:
		if t.local == nil {
			return fmt.Errorf("proxy: no local upstream configured (set LocalUpstreamURL)")
		}
		next = &activeLocal
	default:
		return fmt.Errorf("proxy: unknown provider %q (want %q or %q)", which, ProviderCloud, ProviderLocal)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path != "" {
		tmp := t.path + ".tmp"
		if err := os.WriteFile(tmp, []byte(which), 0o600); err != nil {
			return fmt.Errorf("proxy: persist provider: %w", err)
		}
		if err := os.Rename(tmp, t.path); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("proxy: persist provider: %w", err)
		}
	}
	t.active.Store(next)
	return nil
}
