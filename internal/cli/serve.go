package cli

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/analyze"
	"github.com/abhisheksarkar30/claude-lens/internal/api"
	"github.com/abhisheksarkar30/claude-lens/internal/config"
	"github.com/abhisheksarkar30/claude-lens/internal/consumer"
	"github.com/abhisheksarkar30/claude-lens/internal/ingest"
	"github.com/abhisheksarkar30/claude-lens/internal/proxy"
	"github.com/abhisheksarkar30/claude-lens/internal/secret"
	"github.com/abhisheksarkar30/claude-lens/internal/session"
	"github.com/abhisheksarkar30/claude-lens/internal/sink"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
	"github.com/abhisheksarkar30/claude-lens/internal/web"
)

const (
	// shutdownGrace bounds how long Serve waits for both http.Servers to
	// finish in-flight requests, on top of the consumer's own bounded drain.
	shutdownGrace = 5 * time.Second

	// collectorInterval is how often the in-process scheduler runs the
	// non-proxy collectors. `clens refresh` is the cron-shaped alternative;
	// this is what keeps the dashboard's Sources tab moving for someone who
	// just leaves `clens serve` running.
	collectorInterval = 15 * time.Minute

	// redactScanLimit caps how many stored rows the boot self-test reads.
	// It is a smoke test for a regression in the redactor, not an audit of
	// the whole table -- an unbounded scan would make boot time a function
	// of database size.
	redactScanLimit = 500
)

// Serve is `clens serve`: the composition root. It is the one place the hot
// path (the proxy listener), the cold path (the consumer), the collectors and
// the dashboard API are wired to each other, and the one place the dashboard's
// write seams are bound to real implementations. It blocks until SIGINT, then
// shuts both servers down with a bounded drain.
func Serve(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("serve: open store: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Both boot steps log and continue rather than refusing to start: a
	// reachable credential or a failed purge is worth knowing about, but a
	// coding session must not die over the observer's own problem ("fail
	// open", CLAUDE.md).
	checkRedaction(ctx, st, log.Printf)
	purgeOnStartup(ctx, st, cfg.RetentionDays, log.Printf)

	sk := sink.New(sink.DefaultCapacity)
	broker := api.NewBroker()
	// The consumer writes through PublishingStore so an insert also reaches
	// the dashboard's SSE feed. The API's read routes keep the bare
	// *store.Store, so a read can never itself trigger a publish.
	pubStore := api.NewPublishingStore(st, broker)

	// One object is both halves of session grouping: the pre-insert resolver
	// that names the session a call belongs to, and the post-insert
	// aggregator that folds the call into that session's totals. It writes
	// through the bare store -- a session's aggregate moving is not a new
	// call arriving, and the SSE feed is a feed of calls.
	sess := session.New(st, cfg.SessionGapMinutes)

	cons := consumer.New(sk, pubStore, cfg.Accounts)
	cons.SetSessionResolver(sess)
	cons.SetSessionAggregator(sess)
	// The analyzers are registered here rather than defaulted inside
	// consumer.New: the consumer's own tests assert exact warning counts on
	// bodies these rules legitimately fire on, so the tables->rules->rows path
	// stays explicit and Analyze stays a pure function of what it is handed.
	engine := analyze.Engine{}
	cons.SetAnalyzers(engine)
	cons.SetSessionRule(engine)
	// A Loader, not the shipped table: it re-reads prices.toml when the file
	// changes, so `clens prices --set` takes effect without a restart.
	priceLoader := newPriceLoader(cfg)
	cons.SetPriceTable(priceLoader)
	// The decode limit is the same BodyCapBytes the proxy tees with -- without
	// a second cap on the decoded form, a small compressed body would expand
	// past the configured per-body cap.
	cons.SetBodyDecoding(cfg.BodyCapBytes)

	// Diagnostic only, and off unless asked for by name. It exists because a
	// spinning goroutine can only be named from inside a running process:
	// attaching a debugger suspends the proxy and a SIGBREAK stack dump exits
	// it, and both would take down the client whose traffic routes through
	// this process. With PprofAddr unset (--pprof-addr/CLENS_PPROF_ADDR)
	// nothing starts and the listener set is unchanged.
	startPprof(cfg.PprofAddr)

	proxySrv, err := proxy.NewServer(cfg, sk)
	if err != nil {
		return fmt.Errorf("serve: build proxy: %w", err)
	}

	// proxySrv.Handler is handed to the API as well: POST
	// /api/requests/{id}/replay re-issues a captured request through the live
	// proxy Handler, which is what makes a replay pick up the same transport,
	// tee, body cap and header redaction as live traffic. cfg.ReplayEnabled is
	// the opt-in control -- the route exists but answers 403 without --replay.
	dashAPI := api.New(st, sk, cons, broker, web.Files, proxySrv.Handler, cfg.ReplayEnabled)
	// The four write seams. Binding them here, next to the objects they write
	// through, is the point of a composition root: a route reaches a write
	// capability only through a function value injected at boot, so
	// internal/api never imports secret, config or ingest.
	dashAPI.SetPricing(priceLoader)
	dashAPI.SetCredentialWriter(secret.Save)
	dashAPI.SetAccountWriter(reloadAccounts)
	// br-GI-13-09: POST /api/shutdown drives stop, the same NotifyContext
	// cancel func os.Interrupt drives, so `clens shutdown` triggers the one
	// shutdown path this function already has rather than a second one.
	dashAPI.SetShutdown(stop)

	// One Runner, shared by the scheduler below and the dashboard's on-demand
	// trigger -- the same addCollectors source set `clens refresh` runs, so a
	// click in the Sources tab and a cron tick collect the same things.
	runner := ingest.New(pubStore)
	addCollectors(ctx, runner, cfg, pubStore, sess)
	// Runner.RunOnce returns per-source Outcomes rather than an error, because
	// the ingest layer's whole job is to keep one failing source from stopping
	// the others (invariant 6). The seam speaks error, so this collapses the
	// outcomes to the first non-ok one -- the dashboard wants "did it work",
	// and the per-source detail is already on /api/sources.
	dashAPI.SetIngestTrigger(func(ctx context.Context) error {
		for _, o := range runner.RunOnce(ctx) {
			if o.Status != "ok" {
				return fmt.Errorf("%s/%s: %s", o.Source, o.Name, o.Error)
			}
		}
		return nil
	})
	go ingest.NewScheduler(runner, collectorInterval).Run(ctx)

	// The two read seams br-GI-1-18 adds. They are seams for the same reason
	// the write ones are: a collector's health lives in internal/ingest and an
	// account's plan in internal/config, and internal/api may import neither.
	dashAPI.SetSourceHealth(func(ctx context.Context) ([]api.SourceHealth, error) {
		health, err := runner.SourcesHealth(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]api.SourceHealth, 0, len(health))
		for _, h := range health {
			out = append(out, api.SourceHealth{
				Source:        string(h.Source),
				Status:        h.Status,
				LastSuccessAt: atOrNil(h.LastSuccessAt),
				LastErrorAt:   atOrNil(h.LastErrorAt),
				// The error text is the collector's own and never contains a
				// credential: every collector error in this repo is built from
				// a status code, a URL and a parse failure (see internal/ingest).
				LastError:      h.LastError,
				RowsWritten:    h.RowsWritten,
				CursorPosition: h.CursorPosition,
			})
		}
		return out, nil
	})
	// Accounts and credentials both read internal/secret's presence/LastUsed
	// only. Get is never called on this path, so a credential's value has no
	// route from the secrets file to the dashboard.
	dashAPI.SetAccounts(func(context.Context) (api.Accounts, error) {
		accts := api.Accounts{
			List: make([]api.Account, 0, len(cfg.Accounts)),
			Credentials: map[string]api.Credential{
				"sessionKey": credentialState("sessionKey"),
				"admin":      credentialState("admin"),
			},
		}
		for _, acct := range cfg.Accounts {
			accts.List = append(accts.List, api.Account{
				Name: acct.Name, BillingMode: acct.BillingMode, Plan: acct.Plan,
			})
		}
		return accts, nil
	})

	// The read path's decode cap, the same value the consumer decodes with
	// above. internal/api cannot read it itself: internal/config is under the
	// import guard, and the default lives unexported in internal/consumer.
	dashAPI.SetBodyCapBytes(cfg.BodyCapBytes)

	// The mode badge's two facts. Injected rather than read in internal/api
	// because the settings read lives here, in internal/cli, which already
	// imports internal/api -- the reverse edge would be a build cycle.
	dashAPI.SetProxyMode(func(ctx context.Context) (api.ProxyMode, error) {
		at, err := st.LatestProxyStartedAt(ctx)
		if err != nil {
			return api.ProxyMode{}, err
		}
		base, ok := readSettingsBaseURL(filepath.Join(claudeConfigDir(), "settings.json"))
		return api.ProxyMode{
			Configured: configuredAgainst(base, ok, cfg.ProxyAddr),
			Observed:   !at.IsZero() && time.Since(at) <= proxyRecentWindow,
		}, nil
	})

	dashSrv := &http.Server{Addr: cfg.DashboardAddr, Handler: dashAPI}

	// Bind both listeners up front, rather than inside ListenAndServe, so a bind
	// failure surfaces before anything is written and the state file can record
	// the real addresses (a `:0` port is only known after the bind).
	proxyLn, err := net.Listen("tcp", cfg.ProxyAddr)
	if err != nil {
		return fmt.Errorf("serve: proxy listen: %w", err)
	}
	dashLn, err := net.Listen("tcp", cfg.DashboardAddr)
	if err != nil {
		proxyLn.Close()
		return fmt.Errorf("serve: dashboard listen: %w", err)
	}

	printBanner(os.Stdout, cfg)

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		_ = cons.Run(ctx) // Run never returns a non-nil error; see its doc.
	}()

	errCh := make(chan error, 2)
	go func() {
		if err := proxySrv.Serve(proxyLn); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	go func() {
		if err := dashSrv.Serve(dashLn); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("dashboard server: %w", err)
		}
	}()

	// Both listeners are up. Best-effort: a state file that cannot be written
	// costs `clens restart` its record, never the proxy.
	if exe, err := os.Executable(); err == nil {
		state := serveState{
			PID: os.Getpid(), Exe: exe, Args: os.Args[1:], StartedAt: time.Now().UTC(),
			ProxyAddr: proxyLn.Addr().String(), DashboardAddr: dashLn.Addr().String(),
			LogPath: defaultLogPath(cfg.DBPath),
		}
		if err := writeServeState(cfg.DBPath, state); err != nil {
			log.Printf("serve: write %s: %v", serveStateFile, err)
		} else {
			defer removeServeState(cfg.DBPath, state.PID)
		}
	}

	// The 24-hour ticker alongside the startup run: a tool opened and closed
	// around work sessions may never see a 24-hour boundary on its own, but a
	// long-lived process should not purge only once.
	purgeTicker := time.NewTicker(24 * time.Hour)
	defer purgeTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-purgeTicker.C:
				purgeOnStartup(ctx, st, cfg.RetentionDays, log.Printf)
			}
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		log.Printf("serve: %v", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := proxySrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("serve: proxy shutdown: %v", err)
	}
	if err := dashSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("serve: dashboard shutdown: %v", err)
	}

	// ctx is already cancelled by the signal (or by the error path above),
	// which is what makes cons.Run perform its own bounded drain-and-flush.
	<-consumerDone

	return nil
}

// proxyRecentWindow bounds "observed": a proxy row newer than this means the
// proxy is receiving. Without a stated window the state has no boundary and no
// test can pin it.
const proxyRecentWindow = 5 * time.Minute

// configuredAgainst answers the badge's "configured" half: does the client's
// ANTHROPIC_BASE_URL point at this process's ProxyAddr?
//
// readSettingsBaseURL reports ("", false) for a missing or unparseable
// settings.json *or* a missing ANTHROPIC_BASE_URL, and that is Unknown rather
// than a mismatch -- the printed-banner onboarding path tells the user to
// export the variable in their shell, where settings.json cannot see it.
func configuredAgainst(settings string, ok bool, proxyAddr string) api.ProxyConfigured {
	if !ok {
		return api.ProxyConfiguredUnknown
	}
	if normalizeAddr(settings) == normalizeAddr(proxyAddr) {
		return api.ProxyConfiguredMatch
	}
	return api.ProxyConfiguredMismatch
}

// normalizeAddr reduces an address to host:port so the two sides can be
// compared at all.
//
// Without this the check fails on the tool's own output: printBanner writes
// "http://127.0.0.1:8797" while cfg.ProxyAddr is the bare "127.0.0.1:8797",
// and readSettingsBaseURL returns the settings string verbatim. A string
// equality test would report the bad state for a correctly-pointed client --
// the one thing this indicator exists to get right.
//
// A bare host with no port normalizes to itself, so it never matches a
// host:port. That is deliberate: the port is the signal.
func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return strings.ToLower(s)
	}
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(strings.ToLower(host), port)
}

// checkRedaction runs the startup leak self-test over stored request headers
// and reports the first failure through logf. It logs and continues rather
// than refusing to start: a reachable credential is worth knowing about, but
// refusing to boot would make the observer the outage.
//
// Split out from Serve so the wiring is testable -- the self-test's whole
// value is that it actually runs at boot, which is not true of a function no
// one calls. proxy.RedactCheck takes one call's header JSON, so the scan is
// this loop rather than a store method.
func checkRedaction(ctx context.Context, st *store.Store, logf func(string, ...any)) {
	// ListEventsFull, not ListEvents: this check reads ReqHeaders to prove the
	// redactor ran. On the summary projection ReqHeaders is not there to read,
	// and a version that skipped every row would report zero findings — a
	// security control silently disabled, with no error and no log.
	events, err := st.ListEventsFull(ctx, store.EventFilter{Limit: redactScanLimit})
	if err != nil {
		logf("serve: redaction self-test: %v", err)
		return
	}
	for _, ev := range events {
		if ev.ReqHeaders == "" {
			continue
		}
		if err := proxy.RedactCheck([]byte(ev.ReqHeaders)); err != nil {
			logf("serve: %v", err)
			return // one is the finding; the rest are the same bug
		}
	}
}

// purgeOnStartup runs the retention purge and logs the deleted count through
// logf. days <= 0 means "keep forever" (the default) and is a no-op -- nothing
// is deleted on an unconfigured install. Split out from Serve for the same
// reason checkRedaction is: Serve cannot be driven from a test (two real
// listeners, a blocking signal context), so the run itself has to be testable
// against a temp store.
func purgeOnStartup(ctx context.Context, st *store.Store, days int, logf func(string, ...any)) {
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	n, err := st.PurgeOlderThan(ctx, cutoff)
	if err != nil {
		logf("serve: retention purge: %v", err)
		return
	}
	logf("serve: retention purge: deleted %d row(s) older than %s", n, cutoff.Format(time.RFC3339))
}

// reloadAccounts re-reads the config and accounts files after the dashboard's
// save route wrote one, so a malformed file is reported at the moment it is
// saved instead of silently at the next restart.
//
// ponytail: a save validates, it does not re-attribute. The running consumer
// holds the account list it was built with (consumer.New copies it and exposes
// no setter), so a new account starts contributing only after a restart --
// which is what the route's response tells the user.
func reloadAccounts() error {
	_, err := config.Load(nil)
	return err
}

// atOrNil converts ingest's non-pointer timestamps to the API's nullable
// ones. A collector that has never succeeded records the zero time; the
// dashboard must render that as "never", not as the year 1.
func atOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// credentialState is presence and last-use for one slot. It calls Exists and
// LastUsed and never Get: the value is what this whole package arrangement
// exists to keep out of the dashboard.
func credentialState(name string) api.Credential {
	c := api.Credential{Present: secret.Exists(name)}
	if used := secret.LastUsed(name); !used.IsZero() {
		c.LastUsed = &used
	}
	return c
}

// startPprof serves net/http/pprof on addr when addr is non-empty. See the
// call site for why this is gated rather than always on.
//
// This is the second layer, not the only one: config.Validate already
// rejects a non-loopback PprofAddr (even with --allow-remote) before Serve
// ever reaches here. The check is repeated with config.IsLoopbackHost --
// the one home for "what counts as loopback" -- rather than trusted away,
// so a Config built by a caller other than config.Load (a test, a future
// embedder) cannot open a listener whose heap profile contains whatever is
// in memory just by skipping Validate.
func startPprof(addr string) {
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !config.IsLoopbackHost(host) {
		log.Printf("pprof: refusing %q: a profile carries whatever is in memory, so this listener is loopback-only", addr)
		return
	}
	go func() {
		log.Printf("pprof: listening on http://%s/debug/pprof/", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("pprof: %v", err)
		}
	}()
}

// printBanner prints the copy-pasteable ANTHROPIC_BASE_URL line, the dashboard
// URL, and -- when body capture is off -- the standing warning that calls are
// recorded without their bodies. It says "without their bodies" and not
// "nothing is being recorded" because that is what the code does: since
// br-GI-7-09 the proxy still writes a row under "off", carrying method, path,
// status, timing and redacted headers, with both body columns NULL. Before
// that fix this comment and the string below disagreed, and the code matched
// the comment -- an operator setting the most private policy lost the records
// too.
func printBanner(w io.Writer, cfg *config.Config) {
	fmt.Fprintln(w, "claude-lens is running.")
	fmt.Fprintf(w, "  export ANTHROPIC_BASE_URL=http://%s\n", cfg.ProxyAddr)
	fmt.Fprintf(w, "  dashboard:  http://%s\n", cfg.DashboardAddr)
	if cfg.BodyPolicy == "off" {
		fmt.Fprintln(w, "  WARNING: body capture is off — calls are recorded without their bodies.")
	}
}
