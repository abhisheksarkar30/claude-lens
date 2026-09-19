package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Accounts is `clens accounts`'s entrypoint: configured accounts, their
// plan/billing state, a request-count distribution across them, and the
// re-auth path for the sessionKey cookie and the Admin key.
func Accounts(args []string) error {
	return runAccounts(args, os.Stdout)
}

func runAccounts(args []string, w io.Writer) error {
	sessionValue, args := takeFlag(args, "--set-session")
	adminValue, args := takeFlag(args, "--set-admin-key")
	yes, args := hasFlag(args, "--yes")

	if sessionValue != "" {
		if err := secret.Save("sessionKey", sessionValue); err != nil {
			return fmt.Errorf("accounts: save sessionKey: %w", err)
		}
		fmt.Fprintln(w, "sessionKey stored")
	}
	if adminValue != "" {
		if !yes {
			return fmt.Errorf("accounts: refusing to store an Admin key without --yes (it is organization-wide read)")
		}
		if err := secret.Save("admin", adminValue); err != nil {
			return fmt.Errorf("accounts: save admin key: %w", err)
		}
		fmt.Fprintln(w, "admin key stored (organization-wide read scope)")
	}

	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("accounts: open store: %w", err)
	}
	defer st.Close()

	fmt.Fprintln(w, "accounts:")
	ctx := context.Background()
	for _, a := range cfg.Accounts {
		plan := a.Plan
		if plan == "" {
			plan = "unconfigured"
		}
		// ponytail: distribution is per configured account, not per raw
		// events.auth_kind -- the store has no auth_kind aggregate reader,
		// and every Account already carries exactly one billing_mode by
		// construction (config.Account's own doc comment), so this is the
		// closest available proxy. Add a GroupByAuthKind store method if a
		// finer split ever matters.
		n, err := st.CountEvents(ctx, store.EventFilter{Account: a.Name})
		if err != nil {
			return fmt.Errorf("accounts: count events for %s: %w", a.Name, err)
		}
		fmt.Fprintf(w, "  %-16s billing_mode=%-13s plan=%-12s requests=%d\n", a.Name, a.BillingMode, plan, n)
	}
	if len(cfg.Accounts) == 0 {
		fmt.Fprintln(w, "  (none configured)")
	}

	fmt.Fprintln(w, "\ncredentials:")
	fmt.Fprintln(w, "  "+credentialLine("sessionKey"))
	fmt.Fprintln(w, "  "+credentialLine("admin")+" (organization-wide read)")
	return nil
}

// credentialLine reports whether name is present and when it last worked,
// never its value.
func credentialLine(name string) string {
	if !secret.Exists(name) {
		return fmt.Sprintf("%-10s not configured", name)
	}
	last := secret.LastUsed(name)
	if last.IsZero() {
		return fmt.Sprintf("%-10s configured, never used", name)
	}
	return fmt.Sprintf("%-10s configured, last used %s", name, last.Format("2006-01-02T15:04:05Z07:00"))
}

// takeFlag pulls a "name value" pair (or "name=value") out of args, and
// returns the value plus args with that pair removed. It exists because
// mixing a command's own flags into the same argv config.Load parses would
// make config's flag.FlagSet reject them as unrecognized -- so each
// command-specific flag is stripped out here before the remainder reaches
// config.Load.
func takeFlag(args []string, name string) (value string, rest []string) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], append(append([]string{}, args[:i]...), args[i+2:]...)
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}

// hasFlag reports whether a bare boolean flag is present in args, and
// returns args with it removed.
func hasFlag(args []string, name string) (present bool, rest []string) {
	for i, a := range args {
		if a == name {
			return true, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return false, args
}
