package api

import (
	"net/http"
	"time"
)

// Credential is whether a stored credential is present and when it last
// worked.
//
// There is deliberately no Value field. That absence is the containment
// property br-GI-1-18 test 18 asks for, expressed as a type rather than as a
// handler's restraint: a handler that wanted to leak a credential here would
// have to add the field first, in a file whose whole comment says not to.
type Credential struct {
	Present  bool       `json:"present"`
	LastUsed *time.Time `json:"last_used"`
}

// Account is one configured account: the name traffic is attributed to, the
// billing model that decides which cost columns its rows may carry
// (invariant 5), and the plan.
type Account struct {
	Name        string `json:"name"`
	BillingMode string `json:"billing_mode"` // "subscription" | "api"
	Plan        string `json:"plan,omitempty"`
}

// Accounts is GET /api/accounts' response: the configured accounts plus the
// install's stored-credential state.
//
// Credentials is keyed by slot name and is not per-account, because
// internal/secret keys a credential by name alone -- there is one claude.ai
// session and one Admin key per install, not one per account row. Reporting
// them per account would state the same fact N times and imply a scoping
// that does not exist.
type Accounts struct {
	List        []Account             `json:"accounts"`
	Credentials map[string]Credential `json:"credentials"`
	// Note carries the one caveat a save produces. Empty on a read.
	Note string `json:"note,omitempty"`
}

// loadAccounts resolves the account list through the read seam, or writes the
// error the caller should return. Two routes read it -- GET /api/accounts and
// the subscription half of GET /api/quota -- which is why it is a helper.
func (a *api) loadAccounts(w http.ResponseWriter, r *http.Request) (Accounts, bool) {
	if a.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "accounts are unavailable: no account reader is wired")
		return Accounts{}, false
	}
	accts, err := a.accounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return Accounts{}, false
	}
	if accts.List == nil {
		accts.List = []Account{}
	}
	if accts.Credentials == nil {
		accts.Credentials = map[string]Credential{}
	}
	return accts, true
}

// listAccounts is GET /api/accounts. It reports *whether* a credential is
// present and when it last worked, and never its value -- see Credential.
func (a *api) listAccounts(w http.ResponseWriter, r *http.Request) {
	accts, ok := a.loadAccounts(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, accts)
}

// saveAccounts is POST /api/accounts: re-read and re-validate the accounts
// file, then return what it now says.
//
// The wired seam, SetAccountWriter, takes no arguments (br-GI-1-16's
// signature, pinned by TestWriteSeamsAreAssignableAndUnsetIsSupported), so
// this route cannot carry a new account in the request body -- the only thing
// it can honestly mean is "reload what is on disk", which is what the
// composition root's reloadAccounts does. Editing accounts is `clens
// accounts`'s job; this is the dashboard's re-read button, and saying so is
// better than a body the handler silently discards.
//
// ponytail: the running consumer keeps the account list it booted with, so a
// newly added account contributes only after a restart -- which is why the
// response says so rather than implying the reload took effect everywhere.
func (a *api) saveAccounts(w http.ResponseWriter, r *http.Request) {
	if reason := originReject(r, "accounts"); reason != "" {
		writeError(w, http.StatusForbidden, reason)
		return
	}
	if a.accountWriter == nil {
		writeError(w, http.StatusServiceUnavailable, "accounts are read-only: no account writer is wired")
		return
	}
	if err := a.accountWriter(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	accts, ok := a.loadAccounts(w, r)
	if !ok {
		return
	}
	accts.Note = "the accounts file was re-read and is valid; a newly added account contributes only after a restart"
	writeJSON(w, http.StatusOK, accts)
}
