package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fixtureAccounts is the seam every test in this file wires, so the assertions
// are about the route rather than about a config file.
func fixtureAccounts(t *testing.T, h *api) {
	t.Helper()
	used := time.Now().Add(-2 * time.Hour).UTC()
	h.SetAccounts(func(context.Context) (Accounts, error) {
		return Accounts{
			List: []Account{
				{Name: "max", BillingMode: "subscription", Plan: "max20x"},
				{Name: "work", BillingMode: "api"},
			},
			Credentials: map[string]Credential{
				"sessionKey": {Present: true, LastUsed: &used},
				"admin":      {Present: false},
			},
		}, nil
	})
}

// postOrigin posts to path with a loopback Host and a same-origin Origin --
// the allowlisted path. origin_test.go owns the guard's own cases.
func postOrigin(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798"+path, strings.NewReader(body))
	req.Header.Set("Origin", "http://127.0.0.1:8798")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestAccountsReportsModesAndCredentialPresence is the bead's GET clause: the
// configured accounts and their billing modes come back, and a present
// credential shows as present.
func TestAccountsReportsModesAndCredentialPresence(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureAccounts(t, handler)

	got := decodeJSON[Accounts](t, getOK(t, handler, "/api/accounts").Body)
	if len(got.List) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got.List))
	}
	byName := map[string]Account{}
	for _, a := range got.List {
		byName[a.Name] = a
	}
	if byName["max"].BillingMode != "subscription" || byName["max"].Plan != "max20x" {
		t.Errorf("subscription account came back wrong: %+v", byName["max"])
	}
	if byName["work"].BillingMode != "api" {
		t.Errorf("api account came back wrong: %+v", byName["work"])
	}
	if !got.Credentials["sessionKey"].Present {
		t.Error("the session credential is stored but reports as absent")
	}
	if got.Credentials["admin"].Present {
		t.Error("the admin credential is absent but reports as present")
	}
	if got.Credentials["sessionKey"].LastUsed == nil {
		t.Error("a credential that has been used reports no last-used time")
	}
}

// TestAccountsNeverReturnsACredential is the bead's containment clause. The
// guard is the absence of a field, so this scans the *serialized* response for
// any value the seam knew -- not the decoded struct, which could not carry one
// anyway.
func TestAccountsNeverReturnsACredential(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	const secretValue = "sk-ant-session-placeholder"
	handler.SetAccounts(func(context.Context) (Accounts, error) {
		// A seam that (wrongly) tries to hand the value over still cannot get
		// it into the response: there is nowhere to put it.
		return Accounts{
			List:        []Account{{Name: "max", BillingMode: "subscription", Plan: "max20x"}},
			Credentials: map[string]Credential{"sessionKey": {Present: true}},
		}, nil
	})

	body := getOK(t, handler, "/api/accounts").Body.String()
	if strings.Contains(body, secretValue) {
		t.Fatalf("the response carries a credential value: %s", body)
	}
	// And the JSON has no key that could hold one.
	var raw struct {
		Credentials map[string]map[string]json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for slot, fields := range raw.Credentials {
		for field := range fields {
			if field != "present" && field != "last_used" {
				t.Errorf("credential %q exposes field %q; only presence and last use may leave this package", slot, field)
			}
		}
	}
}

// TestSaveAccountsWritesThroughTheStubAndReturnsNoCredential is the bead's
// POST clause: a valid Origin calls the stub writer, and the response still
// carries no credential.
func TestSaveAccountsWritesThroughTheStubAndReturnsNoCredential(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureAccounts(t, handler)

	writes := 0
	handler.SetAccountWriter(func() error { writes++; return nil })

	rr := postOrigin(t, handler, "/api/accounts", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/accounts: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if writes != 1 {
		t.Fatalf("the account writer ran %d time(s), want 1", writes)
	}
	got := decodeJSON[Accounts](t, rr.Body)
	if len(got.List) != 2 {
		t.Errorf("the save response does not reflect the re-read accounts: %+v", got.List)
	}
	if strings.Contains(rr.Body.String(), "sk-ant") {
		t.Errorf("the save response carries a credential: %s", rr.Body.String())
	}
	// The restart caveat belongs to the save, not to the read.
	if got.Note == "" {
		t.Error("the save response does not say a new account needs a restart")
	}
	if read := decodeJSON[Accounts](t, getOK(t, handler, "/api/accounts").Body); read.Note != "" {
		t.Errorf("the plain read carries a save note: %q", read.Note)
	}
}

func TestSaveAccountsUnwiredAnswers503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureAccounts(t, handler)

	rr := postOrigin(t, handler, "/api/accounts", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/accounts with no writer: status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

// TestSaveAccountsRejectsAForeignOrigin: the writer must not be reachable from
// a page that is not this dashboard.
func TestSaveAccountsRejectsAForeignOrigin(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureAccounts(t, handler)
	writes := 0
	handler.SetAccountWriter(func() error { writes++; return nil })

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/accounts", nil)
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status = %d, want 403", rr.Code)
	}
	if writes != 0 {
		t.Fatalf("a cross-origin request reached the writer %d time(s)", writes)
	}
}

// TestSaveAccountsPropagatesAValidationFailure: a save that found the file
// malformed reports it, rather than answering 200 over a file it could not
// parse.
func TestSaveAccountsPropagatesAValidationFailure(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	fixtureAccounts(t, handler)
	handler.SetAccountWriter(func() error { return errors.New("accounts: line 3: unknown billing_mode \"flat\"") })

	rr := postOrigin(t, handler, "/api/accounts", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "billing_mode") {
		t.Fatalf("the validation error was not carried through: %s", rr.Body.String())
	}
}

// TestAccountsUnwiredAnswers503: /api/quota reads the same seam, so an
// unwired accounts reader has to be reported rather than answered with an
// empty account list (which would render as "no subscription account", a
// different and wrong statement).
func TestAccountsUnwiredAnswers503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	for _, path := range []string{"/api/accounts", "/api/quota"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with no accounts seam: status = %d, want 503", path, rr.Code)
		}
	}
}
