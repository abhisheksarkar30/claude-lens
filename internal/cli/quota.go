package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/quota"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// Quota is `clens quota`'s entrypoint: rolling 5h/7d burn per subscription
// account, the last polled snapshot, and calibration -- against whatever
// limits are configured, which today is none (internal/quota.Limits has
// no configuration source yet), so every window reports `unconfigured`
// rather than a guessed percentage. It never renders a percentage of a
// limit nobody supplied (CLAUDE.md "no invented numbers").
func Quota(args []string) error {
	return runQuota(args, os.Stdout)
}

func runQuota(args []string, w io.Writer) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("quota: open store: %w", err)
	}
	defer st.Close()

	ctx := context.Background()
	now := time.Now()
	limits := quota.Limits{} // ponytail: no limit config source exists yet -- every window is unconfigured until one is added (see calibration.LearnedLimit for how a limit is offered).

	accounts := cfg.Accounts
	if len(accounts) == 0 {
		fmt.Fprintln(w, "(no accounts configured)")
		return nil
	}

	for _, acct := range accounts {
		if acct.BillingMode != "subscription" {
			continue
		}
		fmt.Fprintf(w, "%s (plan=%s):\n", acct.Name, orUnconfigured(acct.Plan))
		for _, window := range []quota.Window{quota.Window5h, quota.Window7d} {
			burn, err := quota.ComputeBurn(ctx, st, acct.Name, "", window, now)
			if err != nil {
				return fmt.Errorf("quota: compute burn %s/%s: %w", acct.Name, window, err)
			}
			proj := quota.Project(burn, limits, now)
			if !proj.Configured {
				fmt.Fprintf(w, "  %-3s burn=%d tokens (%d requests), limit=unconfigured\n", window, burn.Tokens, burn.Requests)
				continue
			}
			fmt.Fprintf(w, "  %-3s burn=%d tokens (%d requests), %.1f%% of limit, approaching=%t\n",
				window, burn.Tokens, burn.Requests, proj.UtilizationPct, proj.Approaching)
		}

		snaps, err := st.ListQuotaSnapshots(ctx, acct.Name, 1)
		if err != nil {
			return fmt.Errorf("quota: last snapshot %s: %w", acct.Name, err)
		}
		if len(snaps) == 0 {
			fmt.Fprintln(w, "  last snapshot: none yet")
		} else {
			s := snaps[0]
			pct := "unconfigured"
			if s.UtilizationPct != nil {
				pct = fmt.Sprintf("%.1f%%", *s.UtilizationPct)
			}
			fmt.Fprintf(w, "  last snapshot: %s window=%s utilization=%s status=%s\n", s.ObservedAt.Format(time.RFC3339), s.Window, pct, s.Status)
		}

		results, err := quota.CrossCheck(ctx, st, acct.Name, "", 20)
		if err != nil {
			return fmt.Errorf("quota: cross-check %s: %w", acct.Name, err)
		}
		learned := quota.Calibrate(results)
		if len(learned) == 0 {
			fmt.Fprintln(w, "  calibration: no candidate limit learned yet")
			continue
		}
		for _, l := range learned {
			fmt.Fprintf(w, "  calibration: %s window observed 100%% at %d tokens (%s) -- offered, not applied\n",
				l.Window, l.TokensAt100, l.ObservedAt.Format(time.RFC3339))
		}
	}
	return nil
}

func orUnconfigured(s string) string {
	if s == "" {
		return "unconfigured"
	}
	return s
}
