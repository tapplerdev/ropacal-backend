package middleware

import (
	"log"
	"net/http"

	"ropacal-backend/internal/orgdb"
)

// Org binds the authenticated user's organization to the request as an
// *orgdb.DB handle (picked up by handlers via `db := orgdb.From(r)`).
//
// It MUST run after Auth — it reads the claims Auth put in the context.
// Auth composes Org automatically, so every existing `r.Use(middleware.Auth)`
// group is already covered and cmd/server/main.go needs no route-registration
// changes; registering Org explicitly on a group is harmless (idempotent).
//
// Mode behavior (decided once at boot by database.InitTenancy → orgdb.Init):
//
//   - Tenancy not migrated (dark ship): stash the shared passthrough handle
//     and continue — zero database work, byte-identical behavior.
//   - Tenancy live: the JWT must carry org_id. Tokens minted before the
//     migration (7-day lifetime) have none → 401 "re-login required", which
//     both clients treat as a forced logout. The bound handle's parked
//     read-transactions are released when the request ends (panics included).
func Org(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Idempotent: an outer Org (or a test) already bound a handle.
		if orgdb.FromContext(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}

		if !orgdb.Migrated() {
			if neutral := orgdb.Neutral(); neutral != nil {
				r = r.WithContext(orgdb.NewContext(r.Context(), neutral))
			}
			// orgdb.Init not called (unit tests): pass through untouched —
			// orgdb.From documents what tests must stash themselves.
			next.ServeHTTP(w, r)
			return
		}

		claims, ok := GetUserFromContext(r)
		if !ok {
			log.Println("❌ [Org] No authenticated user in context — Org must run after Auth")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if claims.OrgID == "" {
			// A valid signature but no org claim = token minted before the
			// tenancy migration. It cannot be scoped; force a fresh login.
			log.Printf("🔒 [Org] Pre-tenancy token for %s — re-login required", claims.Email)
			http.Error(w, "Unauthorized: re-login required", http.StatusUnauthorized)
			return
		}

		d, err := orgdb.New(orgdb.Root(), claims.OrgID)
		if err != nil {
			log.Printf("🔒 [Org] Rejecting token with invalid org id for %s: %v", claims.Email, err)
			http.Error(w, "Unauthorized: re-login required", http.StatusUnauthorized)
			return
		}
		defer d.Release() // reaps parked read-txs; runs on panic paths too

		// D4b: the token proves who signed in, not that they still belong here.
		// Re-check membership (cached ~60s) on the org-bound handle — see
		// membership.go for why it cannot run on the root pool.
		switch verdict, dbRole := verifyMembership(d, claims.UserID, claims.OrgID); {
		case verdict == membershipDeny:
			log.Printf("🔒 [Org] %s no longer belongs to org %s (or org inactive) — re-login required",
				claims.Email, claims.OrgID)
			http.Error(w, "Unauthorized: re-login required", http.StatusUnauthorized)
			return
		case verdict == membershipUnavailable:
			// Deliberately NOT a 401: this is our problem, not a stale token,
			// and a 401 would trigger the dashboard's hard logout for what may
			// be a few seconds of pool saturation.
			http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
			return
		case dbRole != claims.Role:
			// RequireRole trusts the claim, so a demoted admin would otherwise
			// keep admin authority for the token's remaining lifetime.
			log.Printf("🔒 [Org] role drift for %s: token says %q, database says %q — re-login required",
				claims.Email, claims.Role, dbRole)
			http.Error(w, "Unauthorized: re-login required", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(orgdb.NewContext(r.Context(), d)))
	})
}
