package middleware

import (
	"log"
	"net/http"
	"strings"

	"ropacal-backend/pkg/utils"
)

// Routes an operator may NOT reach, even with full write access.
//
// Everything here is reachable in principle — an operator acting as a tenant has
// that tenant's admin rights — but each one produces an effect that OUTLIVES the
// operator's session or escapes the audit trail, which is the property the whole
// design depends on.
//
// This is not about limiting what Binly can do. It is about keeping what Binly
// does attributable and reversible. Anything on this list should be done by the
// tenant with their own credentials, or by direct database access with a human
// deciding.
var platformDeniedRoutes = []struct {
	method string
	// path is matched as a prefix against the path AFTER /api/platform/act.
	path string
	// suffix, when set, must ALSO match the end of the path. This exists for
	// routes with a path parameter in the middle — /manager/shifts/{id}/purge
	// cannot be expressed as a prefix without also denying every other
	// /manager/shifts/ route. Empty means prefix-only, which is every entry
	// that predates this field.
	suffix string
	reason string
}{
	// The big one. A minted user is a SEVEN-DAY tenant credential that outlives
	// the 1-hour platform token, is indistinguishable from the customer's own
	// staff, and — because its later actions are ordinary tenant traffic — never
	// appears in platform_audit_log. There is also no user DELETE route anywhere
	// in this API, so neither the tenant nor Binly can revoke it through the
	// product. A reviewer demonstrated the full chain: create admin, log in as
	// them, mint a second admin, wipe the org's shifts, with the audit count
	// unchanged throughout.
	{http.MethodPost, "/manager/users", "", "creates a persistent tenant credential that cannot be revoked through the product"},

	// Mass-destructive and unattributed. Individual cancels and deletes stay
	// available; these wipe whole collections in one call.
	{http.MethodDelete, "/manager/shifts/clear", "", "deletes every shift in the organization"},
	{http.MethodPost, "/manager/bins/load-real", "", "deletes and replaces the organization's bins with fixture data"},

	// Same class as /manager/shifts/clear, and missed when that one was added:
	// one call cancels every active, paused and ready shift in the org —
	// including shifts drivers are physically mid-route on.
	{http.MethodPost, "/manager/shifts/cancel-all-active", "", "cancels every live shift in the organization in one call"},

	// The audit-trail case, not the volume case: this HARD-deletes route_tasks,
	// shift_history and shifts rows. platform_audit_log records only that the
	// path was called, so the operation destroys the evidence of what it
	// destroyed. Nothing else in this API hard-deletes shift history.
	{http.MethodDelete, "/manager/shifts/", "/purge", "hard-deletes shift history, destroying the record of what was deleted"},

	// Mints a ONE-HOUR realtime credential carrying org_id in its meta, which the
	// Centrifugo proxy endpoints trust for tenant scope. PlatformAuth re-checks
	// the operator on every HTTP request, but it cannot reach a token already
	// issued — so this one keeps working after the platform admin is disabled.
	// Bounded by its TTL rather than by revocation, which is the property this
	// list exists to refuse.
	{http.MethodGet, "/centrifugo/token", "", "mints a realtime credential that survives operator revocation"},

	// Registers a push token against the ACTING user — which under act-as is the
	// support user, an admin. Six admin-notification queries fan out on
	// role='admin', so one stray call puts a Binly device on a customer's admin
	// push list, silently and permanently.
	{http.MethodPost, "/driver/fcm-token", "", "would subscribe a Binly device to the customer's admin notifications"},
}

// PlatformDenyList refuses operator access to routes whose effects outlive the
// session or escape the audit trail.
//
// Mounted on the act-as group only; the tenant's own admins keep every one of
// these routes through their normal login.
func PlatformDenyList(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chi has already stripped the /api/platform/act mount prefix by the time
		// middleware on the sub-router runs, but be explicit rather than rely on
		// it — a future remount must not silently disable this.
		path := r.URL.Path
		if i := strings.Index(path, "/api/platform/act"); i >= 0 {
			path = path[i+len("/api/platform/act"):]
		}

		for _, d := range platformDeniedRoutes {
			if r.Method == d.method && strings.HasPrefix(path, d.path) &&
				(d.suffix == "" || strings.HasSuffix(path, d.suffix)) {
				claims, _ := PlatformFromContext(r.Context())
				who := "(unknown)"
				if claims != nil {
					who = claims.Email
				}
				log.Printf("🚫 [Platform] DENIED %s %s for %s acting as %q — %s",
					r.Method, path, who, r.Header.Get(ActAsOrgHeader), d.reason)
				// JSON, not http.Error: this 403 is consumed by tenant-shaped client
				// code that parses every error body as JSON. As plain text, the
				// dashboard surfaced it as `Unexpected token 'N' ... is not valid
				// JSON` — the block worked, the explanation never reached the
				// operator (seen live, 2026-07-30).
				utils.RespondError(w, http.StatusForbidden,
					"Not available to platform operators: "+d.reason+
						". Ask the organization's own admin to do this, or make the change directly with a human decision.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
