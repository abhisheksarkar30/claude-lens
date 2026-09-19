package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingWriter is the stub credential seam: it records what it was asked to
// store, which is the only place the value is allowed to appear.
type recordingWriter struct {
	names  []string
	values []string
	err    error
}

func (r *recordingWriter) fn(name, value string) error {
	r.names = append(r.names, name)
	r.values = append(r.values, value)
	return r.err
}

func secretBody(name, value string, confirm bool) string {
	b := `{"name":"` + name + `","value":"` + value + `"`
	if confirm {
		b += `,"confirm":true`
	}
	return b + "}"
}

// TestSetSecretWritesThroughTheStubWithoutEchoing is the bead's POST clause:
// a valid Origin calls the stub writer with the name and value, and the
// response contains neither.
func TestSetSecretWritesThroughTheStubWithoutEchoing(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	rec := &recordingWriter{}
	handler.SetCredentialWriter(rec.fn)

	const value = "sk-ant-session-placeholder"
	rr := postOrigin(t, handler, "/api/secrets", secretBody("sessionKey", value, false))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/secrets: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if len(rec.names) != 1 || rec.names[0] != "sessionKey" {
		t.Fatalf("the writer was called with %v, want one call for sessionKey", rec.names)
	}
	if rec.values[0] != value {
		t.Fatalf("the writer received %q, want the submitted value", rec.values[0])
	}

	body := rr.Body.String()
	if strings.Contains(body, value) {
		t.Fatalf("the response echoes the credential value: %s", body)
	}
	if strings.Contains(body, "sessionKey") {
		t.Fatalf("the response echoes the credential name: %s", body)
	}
	if !strings.Contains(body, `"stored":true`) {
		t.Fatalf("the response does not confirm the write: %s", body)
	}
}

// TestSetSecretRefusesAnAdminKeyWithoutConfirmation is the re-auth flow's own
// footgun guard, mirroring `clens accounts`'s --yes rule: an Admin key reads
// every member's usage for the whole organization, so the dashboard must not
// be the quieter way to store one.
func TestSetSecretRefusesAnAdminKeyWithoutConfirmation(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	rec := &recordingWriter{}
	handler.SetCredentialWriter(rec.fn)

	rr := postOrigin(t, handler, "/api/secrets", secretBody("admin", "sk-ant-admin01-placeholder", false))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed Admin key: status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if len(rec.names) != 0 {
		t.Fatalf("an unconfirmed Admin key reached the writer: %v", rec.names)
	}
	if !strings.Contains(rr.Body.String(), "confirm") {
		t.Fatalf("the refusal does not say what is missing: %s", rr.Body.String())
	}
	// The refusal text must not be a second way to leak the key.
	if strings.Contains(rr.Body.String(), "sk-ant-admin01-placeholder") {
		t.Fatalf("the refusal echoes the key: %s", rr.Body.String())
	}

	// With the confirmation, it goes through.
	rr = postOrigin(t, handler, "/api/secrets", secretBody("admin", "sk-ant-admin01-placeholder", true))
	if rr.Code != http.StatusOK {
		t.Fatalf("confirmed Admin key: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if len(rec.names) != 1 || rec.names[0] != "admin" {
		t.Fatalf("the confirmed write reached the writer as %v", rec.names)
	}
}

// TestSetSecretRejectsAnUnknownSlot: only the two slots internal/secret
// actually reads are writable, so this route cannot park a secret nothing
// would ever look for.
func TestSetSecretRejectsAnUnknownSlot(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	rec := &recordingWriter{}
	handler.SetCredentialWriter(rec.fn)

	rr := postOrigin(t, handler, "/api/secrets", secretBody("someNewKey", "value", true))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if len(rec.names) != 0 {
		t.Fatalf("an unknown slot reached the writer: %v", rec.names)
	}
	for _, want := range []string{"admin", "sessionKey"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("the rejection does not list %q as accepted: %s", want, rr.Body.String())
		}
	}
}

func TestSetSecretRejectsAnEmptyValue(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	rec := &recordingWriter{}
	handler.SetCredentialWriter(rec.fn)

	rr := postOrigin(t, handler, "/api/secrets", secretBody("sessionKey", "", false))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if len(rec.names) != 0 {
		t.Fatalf("an empty value reached the writer: %v", rec.names)
	}
}

func TestSetSecretRejectsAnUnknownField(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetCredentialWriter((&recordingWriter{}).fn)

	rr := postOrigin(t, handler, "/api/secrets", `{"name":"sessionKey","value":"v","scope":"admin"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field: %s", rr.Code, rr.Body.String())
	}
}

func TestSetSecretUnwiredAnswers503(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)

	rr := postOrigin(t, handler, "/api/secrets", secretBody("sessionKey", "v", false))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

// TestSetSecretRejectsANonLoopbackHost: the allowlist is two-sided. A Host
// that is not loopback is refused even with no Origin at all, so the guard
// does not depend on the browser having sent one.
func TestSetSecretRejectsANonLoopbackHost(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	rec := &recordingWriter{}
	handler.SetCredentialWriter(rec.fn)

	req := httptest.NewRequest(http.MethodPost, "http://lens.example/api/secrets",
		strings.NewReader(secretBody("sessionKey", "v", false)))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host: status = %d, want 403", rr.Code)
	}
	if len(rec.names) != 0 {
		t.Fatalf("a non-loopback request reached the writer: %v", rec.names)
	}
}

func TestSetSecretRejectsAForeignOrigin(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	rec := &recordingWriter{}
	handler.SetCredentialWriter(rec.fn)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/secrets",
		strings.NewReader(secretBody("sessionKey", "v", false)))
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: status = %d, want 403", rr.Code)
	}
	if len(rec.names) != 0 {
		t.Fatalf("a cross-origin request reached the writer: %v", rec.names)
	}
}

// TestSetSecretGuardRunsBeforeTheBody is the ordering replay.go documents: a
// refused request is refused without the handler having read anything, so a
// cross-origin page cannot even probe which slot names exist.
func TestSetSecretGuardRunsBeforeTheBody(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	handler.SetCredentialWriter((&recordingWriter{}).fn)

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8798/api/secrets",
		strings.NewReader(`{ this is not json`))
	req.Header.Set("Origin", "http://evil.example")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 -- the origin guard must decide before the body is parsed", rr.Code)
	}
}

// TestSetSecretReportsAWriterFailure: internal/secret's error is about the
// file it could not write and never about the value.
func TestSetSecretReportsAWriterFailure(t *testing.T) {
	st := newTestStore(t)
	handler, _, _, _ := newTestAPI(t, st)
	const value = "sk-ant-session-placeholder"
	rec := &recordingWriter{err: errors.New("secret: file is read-only")}
	handler.SetCredentialWriter(rec.fn)

	rr := postOrigin(t, handler, "/api/secrets", secretBody("sessionKey", value, false))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "read-only") {
		t.Fatalf("the writer's error was not carried through: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), value) {
		t.Fatalf("the failure response echoes the value: %s", rr.Body.String())
	}
}
