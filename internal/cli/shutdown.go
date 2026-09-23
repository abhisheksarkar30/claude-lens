package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/config"
)

const (
	// shutdownHTTPTimeout bounds the round trip to POST /api/shutdown. The
	// handler writes its response before the process begins stopping
	// (internal/api's shutdown), so this call returns almost immediately
	// either way -- generous only to absorb a slow loopback hop, not to wait
	// out the shutdown itself.
	shutdownHTTPTimeout = 10 * time.Second
	// shutdownBodyLimit caps how much of an error body is read back. The
	// success body is one field; only a misconfigured address could answer
	// with anything large.
	shutdownBodyLimit = 4096
)

// Shutdown is `clens shutdown`: the CLI half of br-GI-13-09's remote-triggered
// graceful stop. The only other way to end a foreground `clens serve` from a
// second shell is a hard kill (Stop-Process / taskkill on Windows, since
// there is no portable way to deliver Ctrl+C to another process's console),
// which skips the consumer's drain, the sink's flush and the store's close.
func Shutdown(args []string) error {
	return runShutdown(args, os.Stdout)
}

func runShutdown(args []string, w io.Writer) error {
	// Resolved the same way `serve` resolves it: flag > CLENS_* env >
	// ~/.clens/config.toml > default. No store is opened -- this command
	// never touches SQLite, only the dashboard's own HTTP listener.
	cfg, err := config.Load(args)
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	addr := dialableDashboardAddr(cfg.DashboardAddr)
	target := "http://" + addr + "/api/shutdown"

	ctx, cancel := context.WithTimeout(context.Background(), shutdownHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("shutdown: could not reach %s: %w (is `clens serve` running?)", addr, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, shutdownBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown: %s: %s", resp.Status, apiErrorMessage(body))
	}

	fmt.Fprintf(w, "contacted %s; the process will stop once its in-flight requests drain (up to %s) -- it has not stopped yet\n", addr, shutdownGrace)
	return nil
}

// dialableDashboardAddr applies normalizeAddr, as serve.go does before
// comparing addresses, then -- if the resulting host is any wildcard form
// (0.0.0.0, ::, or empty) -- substitutes the loopback alias. normalizeAddr
// alone does not do this: it maps only "localhost" to 127.0.0.1 and leaves a
// wildcard host untouched, which is correct for a *listener* address but not
// for one this CLI has to dial. This repo's own operator config
// (~/.clens/config.toml) sets DashboardAddr = 0.0.0.0:8798, so the wildcard
// case is the common one, not an edge case.
func dialableDashboardAddr(raw string) string {
	addr := normalizeAddr(raw)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
