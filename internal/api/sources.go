package api

import (
	"net/http"
	"time"
)

// SourceHealth is one collector's health as GET /api/sources reports it.
//
// It is api's own type rather than internal/ingest.Health because this package
// may not import internal/ingest (see the package doc), so the wire shape is
// declared on this side and the composition root converts. The conversion is
// four field assignments in internal/cli/serve.go; a helper here would need
// the ingest type to be worth having, which is the import this exists to
// avoid.
//
// The two timestamps are pointers so that "never ran" marshals as null rather
// than as the zero time: a collector that has never succeeded is not a
// collector that succeeded in year 1, and a UI rendering the zero time would
// say exactly that.
type SourceHealth struct {
	Source         string     `json:"source"`
	Status         string     `json:"status"` // "ok" | "error" | "unknown"
	LastSuccessAt  *time.Time `json:"last_success_at"`
	LastErrorAt    *time.Time `json:"last_error_at"`
	LastError      string     `json:"last_error,omitempty"`
	RowsWritten    int        `json:"rows_written"`
	CursorPosition string     `json:"cursor_position,omitempty"`
}

type sourcesResponse struct {
	Sources []SourceHealth `json:"sources"`
}

// sources is GET /api/sources: the Sources tab's whole content, and the
// dashboard half of br-GI-1-17's "a failing collector is a visible red row,
// not a quietly short chart".
//
// An unwired seam answers 503 rather than an empty list, because an empty
// list is indistinguishable from "every collector is fine and has simply
// written nothing yet" -- and telling those two apart is this route's only
// job.
func (a *api) sources(w http.ResponseWriter, r *http.Request) {
	if a.sourceHealth == nil {
		writeError(w, http.StatusServiceUnavailable, "source health is unavailable: no collector runner is wired")
		return
	}
	health, err := a.sourceHealth(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if health == nil {
		// A nil slice marshals as null; the tab iterates the array, and an
		// empty array is the shape it is written against.
		health = []SourceHealth{}
	}
	writeJSON(w, http.StatusOK, sourcesResponse{Sources: health})
}
