package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A deny rule that does not match is worse than no rule: it reads as a control
// in review and enforces nothing. These tests exist because /manager/shifts/{id}/purge
// carries a path parameter in the MIDDLE, which a prefix match cannot express —
// the first attempt at that entry was written as the literal
// "/manager/shifts/purge" and would never have fired against a real request.

func denyStatus(t *testing.T, method, path string) int {
	t.Helper()
	reached := false
	h := PlatformDenyList(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	if rec.Code == http.StatusOK && !reached {
		t.Fatal("200 without reaching the handler — the test is not measuring what it thinks")
	}
	return rec.Code
}

func TestPlatformDenyList_DeniesPurgeDespiteThePathParameter(t *testing.T) {
	// The shift id is a UUID in the middle of the path.
	const p = "/api/platform/act/manager/shifts/6f1c1e2a-0000-4000-8000-000000000001/purge"
	if got := denyStatus(t, http.MethodDelete, p); got != http.StatusForbidden {
		t.Errorf("purge got %d, want 403 — this route hard-deletes shift history "+
			"and the audit log records only that the path was called", got)
	}
}

func TestPlatformDenyList_DeniesTheRestOfTheList(t *testing.T) {
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/platform/act/manager/users"},
		{http.MethodDelete, "/api/platform/act/manager/shifts/clear"},
		{http.MethodPost, "/api/platform/act/manager/bins/load-real"},
		{http.MethodPost, "/api/platform/act/manager/shifts/cancel-all-active"},
		{http.MethodGet, "/api/platform/act/centrifugo/token"},
		{http.MethodPost, "/api/platform/act/driver/fcm-token"},
	}
	for _, c := range cases {
		if got := denyStatus(t, c.method, c.path); got != http.StatusForbidden {
			t.Errorf("%s %s got %d, want 403", c.method, c.path, got)
		}
	}
}

func TestPlatformDenyList_DoesNotOverBlock(t *testing.T) {
	// The purge entry's prefix is "/manager/shifts/", which on its own would
	// deny the entire shifts surface. The suffix is what keeps these reachable.
	cases := []struct {
		method, path, why string
	}{
		{http.MethodGet, "/api/platform/act/manager/shifts", "listing shifts is the operator's main read"},
		{http.MethodGet, "/api/platform/act/manager/shifts/abc-123", "reading one shift"},
		{http.MethodPatch, "/api/platform/act/manager/shifts/abc-123", "editing a shift is allowed and audited"},
		{http.MethodPost, "/api/platform/act/manager/shifts/abc-123/cancel", "individual cancel stays available by design"},
		{http.MethodDelete, "/api/platform/act/manager/shifts/abc-123", "single delete is not the mass-destructive case"},
		{http.MethodPost, "/api/platform/act/manager/bins/schedule-move", "ordinary write"},
	}
	for _, c := range cases {
		if got := denyStatus(t, c.method, c.path); got == http.StatusForbidden {
			t.Errorf("%s %s was DENIED but should be allowed — %s", c.method, c.path, c.why)
		}
	}
}

func TestPlatformDenyList_MethodIsPartOfTheMatch(t *testing.T) {
	// GET /manager/users must stay available; only the POST that mints a
	// credential is denied.
	if got := denyStatus(t, http.MethodGet, "/api/platform/act/manager/users"); got == http.StatusForbidden {
		t.Error("GET /manager/users was denied — only POST creates a credential")
	}
}
