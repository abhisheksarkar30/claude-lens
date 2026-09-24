package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/api"
	"github.com/abhisheksarkar30/claude-lens/internal/config"
)

// testReloader is a reloader whose "config file" is the variable it returns.
func testReloader(boot config.Config, next *config.Config, loadErr *error) (*reloader, *[]config.Account) {
	var swapped []config.Account
	rl := &reloader{
		load: func() (*config.Config, error) {
			if loadErr != nil && *loadErr != nil {
				return nil, *loadErr
			}
			c := *next
			return &c, nil
		},
		boot: boot, accounts: boot.Accounts,
		live:        &liveSettings{retentionDays: boot.RetentionDays, hotDays: boot.HotDays},
		setAccounts: func(a []config.Account) { swapped = a },
	}
	return rl, &swapped
}

func TestReloadClassifiesAppliedRestartAndUnchanged(t *testing.T) {
	boot := config.Config{ProxyAddr: "127.0.0.1:8797", RetentionDays: 30, Accounts: []config.Account{{Name: "a", BillingMode: "api"}}}

	same := boot
	rl, _ := testReloader(boot, &same, nil)
	if rep, err := rl.Reload(context.Background()); err != nil || !rep.Unchanged {
		t.Fatalf("identical file: rep %+v err %v, want unchanged", rep, err)
	}

	next := boot
	next.RetentionDays = 7
	next.Accounts = []config.Account{{Name: "b", BillingMode: "subscription"}}
	next.ProxyAddr = "127.0.0.1:9999"
	rl, swapped := testReloader(boot, &next, nil)
	rep, err := rl.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Applied, []string{"Accounts", "RetentionDays"}) {
		t.Fatalf("applied = %v", rep.Applied)
	}
	if !reflect.DeepEqual(rep.RestartRequired, []string{"ProxyAddr"}) {
		t.Fatalf("restart_required = %v", rep.RestartRequired)
	}
	if rep.Unchanged {
		t.Fatal("unchanged must be false when something differs")
	}
	if !reflect.DeepEqual(*swapped, next.Accounts) || rl.live.RetentionDays() != 7 {
		t.Fatalf("live values not applied: accounts %v retention %d", *swapped, rl.live.RetentionDays())
	}

	// A second reload with the same file applies nothing again, but the address
	// change still needs a restart.
	rep, _ = rl.Reload(context.Background())
	if len(rep.Applied) != 0 || len(rep.RestartRequired) != 1 {
		t.Fatalf("second reload rep = %+v, want nothing applied and ProxyAddr still pending", rep)
	}
}

// All-or-nothing: an invalid file changes neither field.
func TestReloadInvalidFileAppliesNothing(t *testing.T) {
	boot := config.Config{RetentionDays: 30, Accounts: []config.Account{{Name: "a"}}}
	next := boot
	next.RetentionDays = 1
	next.Accounts = []config.Account{{Name: "z"}}
	bad := errors.New("RetentionDays: invalid")
	rl, swapped := testReloader(boot, &next, &bad)
	if _, err := rl.Reload(context.Background()); err == nil {
		t.Fatal("an invalid file must be an error")
	}
	if *swapped != nil || rl.live.RetentionDays() != 30 || !reflect.DeepEqual(rl.Accounts(), boot.Accounts) {
		t.Fatalf("a failed reload changed state: swapped %v retention %d accounts %v", *swapped, rl.live.RetentionDays(), rl.Accounts())
	}
}

// bootArgs regression: reload re-reads with the post-subcommand args, so an
// unchanged file must not diff RetentionDays back to the default.
func TestReloadKeepsBootFlags(t *testing.T) {
	withHome(t)
	args := []string{"--retention-days", "30"}
	boot, err := config.Load(args)
	if err != nil {
		t.Fatal(err)
	}
	rl := newReloader(args, boot, &liveSettings{retentionDays: boot.RetentionDays, hotDays: boot.HotDays}, func([]config.Account) {})
	rep, err := rl.Reload(context.Background())
	if err != nil || !rep.Unchanged {
		t.Fatalf("rep %+v err %v, want unchanged with the boot flags honoured", rep, err)
	}
}

// End to end against a real Serve: rewrite the accounts file, run `clens
// reload`, and see the running process report the new list.
func TestReloadAppliesAccountsToARunningServe(t *testing.T) {
	home := withHome(t)
	db := filepath.Join(home, "lens.db")
	accounts := filepath.Join(home, "accounts.toml")
	write := func(body string) {
		if err := os.WriteFile(accounts, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("name = old\nbilling_mode = api\n")

	flags := []string{"--proxy-addr", "127.0.0.1:0", "--dashboard-addr", "127.0.0.1:0", "--db-path", db, "--accounts-path", accounts}
	done := make(chan error, 1)
	go func() { done <- Serve(flags) }()

	var st serveState
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if s, ok, _ := readServeState(db); ok {
			st = s
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never came up")
		}
	}
	names := func() []string {
		resp, err := http.Get("http://" + st.DashboardAddr + "/api/accounts")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var a api.Accounts
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range a.List {
			out = append(out, x.Name)
		}
		return out
	}
	if got := names(); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("boot accounts = %v", got)
	}

	write("name = new\nbilling_mode = api\n")
	var out bytes.Buffer
	if err := runReload([]string{"--dashboard-addr", st.DashboardAddr}, &out); err != nil {
		t.Fatalf("runReload: %v", err)
	}
	if !strings.Contains(out.String(), "Accounts") {
		t.Fatalf("output does not report Accounts applied: %q", out.String())
	}
	if got := names(); !reflect.DeepEqual(got, []string{"new"}) {
		t.Fatalf("accounts after reload = %v, want [new]", got)
	}

	// An unreadable file is a 400 and leaves the applied list alone.
	write("name = broken\nbilling_mode = nonsense\nthis line has no equals\n")
	if err := runReload([]string{"--dashboard-addr", st.DashboardAddr}, &out); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a malformed accounts file must be a 400, got %v", err)
	}
	if got := names(); !reflect.DeepEqual(got, []string{"new"}) {
		t.Fatalf("a rejected reload changed the applied accounts to %v", got)
	}

	resp, err := http.Post("http://"+st.DashboardAddr+"/api/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// HotDays is the third live-applied field: reported under applied, and read by
// the archiver on its next cycle.
func TestReloadAppliesHotDays(t *testing.T) {
	boot := config.Config{RetentionDays: 30, HotDays: 7}
	next := boot
	next.HotDays = 3
	rl, _ := testReloader(boot, &next, nil)
	rep, err := rl.Reload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.Applied, []string{"HotDays"}) || len(rep.RestartRequired) != 0 {
		t.Fatalf("rep = %+v, want HotDays applied and nothing restart-required", rep)
	}
	if rl.live.HotDays() != 3 {
		t.Fatalf("live HotDays = %d, want 3", rl.live.HotDays())
	}
	if rep, _ := rl.Reload(context.Background()); !rep.Unchanged {
		t.Fatalf("second reload rep = %+v, want unchanged", rep)
	}
}

// An invalid HotDays (beyond retention) is refused by the loader, so nothing
// is applied.
func TestReloadRejectsHotDaysBeyondRetention(t *testing.T) {
	withHome(t)
	boot := config.Config{RetentionDays: 30, HotDays: 7}
	live := &liveSettings{retentionDays: 30, hotDays: 7}
	rl := newReloader([]string{"--retention-days", "5", "--hot-days", "9"}, &boot, live, func([]config.Account) {})
	if _, err := rl.Reload(context.Background()); err == nil {
		t.Fatal("hot_days > retention_days must be rejected")
	}
	if live.HotDays() != 7 || live.RetentionDays() != 30 {
		t.Fatalf("a rejected reload changed state: hot %d retention %d", live.HotDays(), live.RetentionDays())
	}
}
