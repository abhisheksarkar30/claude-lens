package api

import "net/http"

// triggerIngest is POST /api/ingest: run every collector once, on demand --
// the Sources tab's "collect now" button, and the same source set the
// scheduler runs on a timer.
//
// originReject first, for the reason secrets.go spells out: this route makes
// outbound calls to claude.ai and the Admin API with whatever credentials are
// stored, so a cross-origin page must not be able to set it off.
func (a *api) triggerIngest(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "ingest"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	if a.ingestTrigger == nil {
		writeError(w, http.StatusServiceUnavailable, "ingest is unavailable: no collector runner is wired")
		return
	}
	if err := a.ingestTrigger(r.Context()); err != nil {
		// Every collector already ran; one of them reported a failure. That
		// is the run's outcome, not a reason to hide what the others did --
		// the composition root's trigger returns the first non-ok outcome's
		// error, and the per-source detail is on GET /api/sources, which the
		// tab re-reads next.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggered": true})
}
