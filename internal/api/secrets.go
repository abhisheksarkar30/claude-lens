package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// credentialSlot describes one of the two stored credentials this route
// accepts.
type credentialSlot struct {
	// scope names what the credential can read, in the response instead of
	// the credential's own name or value. It is what a UI needs to confirm
	// the right slot was written, and it deliberately contains neither.
	scope string
	// orgWide marks a credential that reads every member's usage for the
	// whole organization, which is why storing it takes an explicit
	// confirmation -- the same rule br-GI-1-15's `clens accounts` enforces
	// with its --yes flag. This route must not be the weaker way in.
	orgWide bool
}

// credentialSlots is the fixed set POST /api/secrets accepts, keyed by the
// same names internal/secret uses. A general name space would let this route
// write a credential nothing could ever read; two slots is the whole system.
var credentialSlots = map[string]credentialSlot{
	"sessionKey": {scope: "claude.ai usage and quota snapshots"},
	"admin":      {scope: "organization-wide usage and cost reports", orgWide: true},
}

type setSecretRequest struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Confirm bool   `json:"confirm"`
}

// setSecret is POST /api/secrets: the plan's re-auth flow, and the one route
// in this package that takes a credential.
//
// Three things are load-bearing and in this order. originReject runs before
// anything is read out of the request, so a cross-origin page cannot use this
// endpoint to write a credential at all -- the same ordering replay.go
// documents, and for the same reason: a guard that runs after the body has
// been read is a guard that already accepted the request. Then the seam, so
// an install with no writer says 503 rather than taking a secret it cannot
// store. Then the body.
//
// The response carries neither the name nor the value. It states what was
// stored by scope, which is enough to confirm the right slot was written and
// is not a secret on its way back into a browser, a proxy log, or the
// dashboard's own SSE stream.
func (a *api) setSecret(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "secrets"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	if a.credentialWriter == nil {
		writeError(w, http.StatusServiceUnavailable, "credentials cannot be stored: no credential writer is wired")
		return
	}

	var req setSecretRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("malformed request body: %v", err))
		return
	}

	slot, ok := credentialSlots[req.Name]
	if !ok {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("unknown credential %q: want one of %s", req.Name, strings.Join(credentialSlotNames(), ", ")))
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	if slot.orgWide && !req.Confirm {
		// Refused before the write, so a request that only meant to poke at
		// the endpoint cannot have stored an organization-wide key by
		// accident.
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("refusing to store an Admin key without confirm=true (%s)", slot.scope))
		return
	}

	if err := a.credentialWriter(req.Name, req.Value); err != nil {
		// internal/secret's own error text. It is about the file it could not
		// write, not about the value, and the value is not in it.
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": true, "scope": slot.scope})
}

// credentialSlotNames is the sorted slot list, so the rejection message is
// stable rather than in Go's map order.
func credentialSlotNames() []string {
	names := make([]string, 0, len(credentialSlots))
	for name := range credentialSlots {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
