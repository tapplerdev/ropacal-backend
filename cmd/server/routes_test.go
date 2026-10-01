package main

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// routes walks one registration function into its own router, so each route
// can be checked against the group — and therefore the middleware — it lives
// in: registerTenantRoutes is mounted behind Auth alone, registerTenantAdminRoutes
// behind Auth + RequireRole("admin").
func routes(t *testing.T, register func(chi.Router, routeDeps)) map[string]bool {
	t.Helper()
	r := chi.NewRouter()
	register(r, routeDeps{})
	have := map[string]bool{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		have[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return have
}

// The HTTP-verb sweep (2026-10): actions are POST, partial updates are PATCH.
// The old PUT verbs stay registered until the driver app release that stops
// sending them is installed everywhere. This pins both halves AND the group
// each lives in, so neither a verb a client still sends nor one a client now
// depends on can be dropped — or slip out from behind the admin check — by
// accident.
func TestVerbSweep_NewVerbsAndLegacyPUTsAreRegisteredInTheRightGroup(t *testing.T) {
	tenant, admin := routes(t, registerTenantRoutes), routes(t, registerTenantAdminRoutes)

	tenantOnly := []string{
		"PATCH /notifications/preferences",
		"PUT /notifications/preferences", // legacy
	}
	adminOnly := []string{
		// Partial updates.
		"PATCH /manager/bins/move-requests/{id}",
		"PATCH /manager/notification-settings",
		// Actions.
		"POST /manager/shifts/{id}/cancel",
		"POST /manager/bins/move-requests/{id}/cancel",
		"POST /manager/bins/move-requests/{id}/assign-to-user",
		"POST /manager/bins/move-requests/{id}/clear-assignment",
		"POST /manager/bins/move-requests/{id}/complete-manually",
		"POST /manager/bins/check-recommendations/{id}/dismiss",
		"POST /manager/ai-recommendations/{id}/accept",
		"POST /manager/ai-recommendations/{id}/dismiss",
		"POST /manager/ai-recommendations/{id}/snooze",
		// Legacy, until the driver app release has replaced the installed base.
		"PUT /manager/shifts/{id}/cancel",
		"PUT /manager/bins/move-requests/{id}",
		"PUT /manager/bins/move-requests/{id}/cancel",
		"PUT /manager/bins/move-requests/{id}/assign-to-user",
		"PUT /manager/bins/move-requests/{id}/clear-assignment",
		"PUT /manager/bins/move-requests/{id}/complete-manually",
		"PUT /manager/bins/check-recommendations/{id}/dismiss",
		"PUT /manager/ai-recommendations/{id}/accept",
		"PUT /manager/ai-recommendations/{id}/dismiss",
		"PUT /manager/ai-recommendations/{id}/snooze",
		"PUT /manager/notification-settings",
	}
	for _, r := range tenantOnly {
		if !tenant[r] {
			t.Errorf("not registered with the tenant routes: %s", r)
		}
	}
	for _, r := range adminOnly {
		if !admin[r] {
			t.Errorf("not registered with the admin routes: %s", r)
		}
		if tenant[r] {
			t.Errorf("admin route also registered WITHOUT the admin check: %s", r)
		}
	}
}

// Nothing under /manager may sit in the tenant group, which has no role check:
// a driver token would reach it. A new /manager route belongs in
// registerTenantAdminRoutes.
func TestNoManagerRouteEscapesTheAdminCheck(t *testing.T) {
	var escaped []string
	for r := range routes(t, registerTenantRoutes) {
		if strings.Contains(r, " /manager/") || strings.HasSuffix(r, " /manager") {
			escaped = append(escaped, r)
		}
	}
	sort.Strings(escaped)
	if len(escaped) > 0 {
		t.Errorf("/manager routes registered without RequireRole(\"admin\"):\n  %s", strings.Join(escaped, "\n  "))
	}
}
