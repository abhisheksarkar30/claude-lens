package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/adminrep"
	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/ingest"
	"github.com/abhisheksarkar30/claude-lens/internal/jsonlogs"
	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/snapshot"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// The Admin API and claude.ai usage-endpoint URLs below are UNVERIFIED --
// both internal/adminrep and internal/snapshot's own package docs state
// their endpoint shapes have never been confirmed against a live account,
// and ask a real caller to verify before relying on them. Wiring them here
// is what makes `clens refresh` runnable end to end; the paths themselves
// still need that verification before the numbers they produce are trusted.
// var, not const, so a test can point them at an httptest.Server to force
// a specific fail-soft Status without a live account.
var (
	adminUsageURL      = "https://api.anthropic.com/v1/organizations/usage_report/messages"
	adminCostURL       = "https://api.anthropic.com/v1/organizations/cost_report"
	adminRateLimitsURL = "https://api.anthropic.com/v1/organizations/rate_limits"
	claudeAIUsageURL   = "https://claude.ai/api/organizations/usage"
)

// Refresh is `clens refresh`'s entrypoint: the unattended cron/Task
// Scheduler target that runs every non-proxy collector once (JSONL tail +
// snapshots + admin pull) via internal/ingest's orchestration. No prompts;
// a failing collector is a non-zero exit and a named line in the output,
// never a printed credential.
func Refresh(args []string) error {
	return runRefresh(args, os.Stdout)
}

func runRefresh(args []string, w io.Writer) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("refresh: open store: %w", err)
	}
	defer st.Close()

	ctx := context.Background()
	r := ingest.New(st)
	addCollectors(ctx, r, cfg, st)

	outcomes := r.RunOnce(ctx)
	failed := false
	for _, o := range outcomes {
		if o.Status != "ok" {
			failed = true
			fmt.Fprintf(w, "%-8s %-8s FAIL rows=%d %s\n", o.Source, o.Name, o.Rows, o.Error)
			continue
		}
		fmt.Fprintf(w, "%-8s %-8s ok   rows=%d\n", o.Source, o.Name, o.Rows)
	}
	if failed {
		return fmt.Errorf("refresh: one or more sources failed")
	}
	return nil
}

// collectorStore is the write surface every collector needs. Both the bare
// *store.Store and api.PublishingStore satisfy it structurally; `clens serve`
// passes the publishing one so a collector's rows also reach the dashboard's
// SSE feed, while `clens refresh` -- which has no broker -- passes the bare
// store.
type collectorStore interface {
	jsonlogs.Store
	snapshot.Store
	adminrep.Store
}

// addCollectors registers every non-proxy source against r. Shared by
// `clens refresh` (one-shot, on a timer) and `clens serve` (its scheduler and
// the dashboard's on-demand trigger) so the two can never drift into
// different sets of sources.
func addCollectors(ctx context.Context, r *ingest.Runner, cfg *config.Config, st collectorStore) {
	tailer := jsonlogs.New(jsonlRoot(), st)
	tailer.SetPriceTable(pricing.NewLoader(pricing.DefaultPath(), nil))
	if acct := firstAccount(cfg, "subscription"); acct.Name != "" {
		tailer.SetAccount(acct.Name, acct.BillingMode)
	}
	r.Add(ingest.SourceJSONL, "root", "jsonl:"+jsonlRoot(), ingest.CollectorFunc(func(ctx context.Context) (int, error) {
		stats, err := tailer.Poll(ctx)
		return stats.Inserted, err
	}))

	// Snapshot and admin are opt-in sources -- only registered once their
	// credential is actually configured, the same "unconfigured is fine,
	// not a failure" state doctor's secretProtectionCheck and clens
	// accounts already report. Without this, an install that only ever
	// set up a JSONL/subscription-only workflow would FAIL every
	// unattended run on sources nobody asked for.
	if secret.Exists("sessionKey") {
		for _, acct := range cfg.Accounts {
			if acct.BillingMode != "subscription" {
				continue
			}
			poller := snapshot.New(claudeAIUsageURL, acct.Name, st)
			r.Add(ingest.SourceSnapshot, acct.Name, "snapshot:"+acct.Name, adaptResult(poller.Poll))
		}
	}

	if secret.Exists("admin") {
		since, until := adminWindow(ctx, st)
		admin := adminrep.New(adminUsageURL, adminCostURL, adminRateLimitsURL, st)
		r.Add(ingest.SourceAdmin, "org", "admin:usage", ingest.CollectorFunc(func(ctx context.Context) (int, error) {
			res, err := admin.CollectAll(ctx, since, until)
			if err != nil {
				return res.Rows, err
			}
			if res.Status != "ok" {
				return res.Rows, fmt.Errorf("status: %s", res.Status)
			}
			return res.Rows, nil
		}))
	}
}

// adaptResult adapts a fail-soft (Result, error)-returning Poll method to
// ingest.Collector: a non-"ok" Result.Status becomes this source's error,
// mirroring how ingest orchestration isolates a failure (br-GI-1-14).
func adaptResult(poll func(context.Context) (snapshot.Result, error)) ingest.Collector {
	return ingest.CollectorFunc(func(ctx context.Context) (int, error) {
		res, err := poll(ctx)
		if err != nil {
			return res.Rows, err
		}
		if res.Status != "ok" {
			return res.Rows, fmt.Errorf("status: %s", res.Status)
		}
		return res.Rows, nil
	})
}

// adminWindow resumes from the admin usage cursor internal/adminrep saved
// last run (its own "until" becomes this run's "since"), falling back to a
// 30-day lookback on a first run -- the same "empty cursor means start of
// history" convention internal/jsonlogs' cursor.go uses.
func adminWindow(ctx context.Context, st collectorStore) (since, until time.Time) {
	until = time.Now().UTC()
	since = until.Add(-30 * 24 * time.Hour)
	if s, ok, err := st.GetIngestState(ctx, "admin:usage"); err == nil && ok {
		if t, perr := time.Parse(time.RFC3339, s.Value); perr == nil {
			since = t
		}
	}
	return since, until
}
