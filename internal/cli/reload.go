package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/api"
	"github.com/abhisheksarkar30/claude-lens/internal/config"
)

// liveSettings holds the values a running serve reads on every use, so a reload
// can change them without a restart. Everything else in config.Config is bound
// at boot.
type liveSettings struct {
	mu            sync.RWMutex
	retentionDays int
	hotDays       int
}

func (l *liveSettings) RetentionDays() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.retentionDays
}

func (l *liveSettings) HotDays() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.hotDays
}

// reloader re-reads the config and applies the live-safe subset: exactly
// Accounts, RetentionDays and HotDays. Prices already hot-reload through their own
// loader. All-or-nothing: the new config is fully loaded and validated before
// anything is applied.
type reloader struct {
	mu   sync.Mutex
	load func() (*config.Config, error)
	boot config.Config // what the process booted with: restart-only diffs are against this

	accounts    []config.Account // currently applied
	live        *liveSettings
	setAccounts func([]config.Account)
}

// newReloader loads via config.Load(bootArgs) -- the post-subcommand slice
// serve received, not the state file's args: Load's flag.Parse stops at the
// first non-flag argument, so handed ["serve", "--replay"] it would drop every
// flag and a reload would conclude retention_days changed to its default.
func newReloader(bootArgs []string, boot *config.Config, live *liveSettings, setAccounts func([]config.Account)) *reloader {
	return &reloader{
		load: func() (*config.Config, error) {
			c, err := config.Load(bootArgs)
			if err != nil {
				return nil, err
			}
			return c, c.Validate()
		},
		boot: *boot, accounts: boot.Accounts, live: live, setAccounts: setAccounts,
	}
}

// liveFields are the only settings applied live; every other differing field
// is reported as restart_required.
var liveFields = map[string]bool{"Accounts": true, "RetentionDays": true, "HotDays": true}

// Accounts is the currently applied account list, which a reload can change.
func (r *reloader) Accounts() []config.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accounts
}

func (r *reloader) Reload(context.Context) (api.ReloadReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next, err := r.load()
	if err != nil {
		return api.ReloadReport{}, err
	}

	rep := api.ReloadReport{Applied: []string{}, RestartRequired: []string{}}
	bv, nv := reflect.ValueOf(r.boot), reflect.ValueOf(*next)
	for i := 0; i < bv.NumField(); i++ {
		name := bv.Type().Field(i).Name
		if !liveFields[name] && !reflect.DeepEqual(bv.Field(i).Interface(), nv.Field(i).Interface()) {
			rep.RestartRequired = append(rep.RestartRequired, name)
		}
	}
	accountsChanged := !reflect.DeepEqual(next.Accounts, r.accounts)
	retentionChanged := next.RetentionDays != r.live.RetentionDays()
	if accountsChanged {
		r.setAccounts(next.Accounts)
		r.accounts = next.Accounts
		rep.Applied = append(rep.Applied, "Accounts")
	}
	if next.HotDays != r.live.HotDays() {
		r.live.mu.Lock()
		r.live.hotDays = next.HotDays
		r.live.mu.Unlock()
		rep.Applied = append(rep.Applied, "HotDays")
	}
	if retentionChanged {
		r.live.mu.Lock()
		r.live.retentionDays = next.RetentionDays
		r.live.mu.Unlock()
		rep.Applied = append(rep.Applied, "RetentionDays")
	}
	rep.Unchanged = len(rep.Applied) == 0 && len(rep.RestartRequired) == 0
	return rep, nil
}

// Reload is `clens reload`: ask the running serve to re-read its config and
// apply what can change without dropping the proxy.
func Reload(args []string) error {
	return runReload(args, os.Stdout)
}

func runReload(args []string, w io.Writer) error {
	cfg, err := config.Load(args)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	addr := dialableDashboardAddr(cfg.DashboardAddr)
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Post("http://"+addr+"/api/reload", "application/json", nil)
	if err != nil {
		return fmt.Errorf("reload: could not reach %s: %w (is `clens serve` running?)", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, shutdownBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reload: %s: %s", resp.Status, apiErrorMessage(body))
	}
	var rep api.ReloadReport
	if err := json.Unmarshal(body, &rep); err != nil {
		return fmt.Errorf("reload: unreadable response: %w", err)
	}
	if rep.Unchanged {
		fmt.Fprintln(w, "reload: nothing changed")
		return nil
	}
	if len(rep.Applied) > 0 {
		fmt.Fprintf(w, "applied live: %v\n", rep.Applied)
	}
	if len(rep.RestartRequired) > 0 {
		fmt.Fprintf(w, "differs but needs `clens restart`: %v\n", rep.RestartRequired)
	}
	return nil
}
