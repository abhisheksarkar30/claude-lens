package cli

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// probeServeHealth reports whether a clens dashboard is answering
// GET /api/health. Wildcard binds are dialed on loopback, the same rewrite
// shutdown and reload already use, because 0.0.0.0 is not a dial address.
func probeServeHealth(dashboardAddr string, timeout time.Duration) bool {
	c := http.Client{Timeout: timeout}
	resp, err := c.Get("http://" + dialableDashboardAddr(dashboardAddr) + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// warnIfServeLive prints one line when a live serve would contend with this
// process for the database. It is not a gate: the command continues either way.
func warnIfServeLive(w io.Writer, dashboardAddr string) {
	if !probeServeHealth(dashboardAddr, 2*time.Second) {
		return
	}
	fmt.Fprintf(w, "%s: a live serve is answering dashboard /api/health, so this write can contend with serve's capture (SQLITE_BUSY)\n", statusWarn)
}
