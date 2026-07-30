package middleware

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
)

// ActAsOrg binds ONE tenant's database handle to a platform request, so the
// ordinary tenant handlers serve that tenant unchanged.
//
// This is the whole of "an operator gets the same views as that org's admin":
// every handler reads its scope from orgdb.From(r), so binding a real org-scoped
// handle here means the handler is not merely imitating the tenant's view — it
// IS the tenant's view, filtered by the same RLS policies, through the same code.
//
// NOTHING HERE BYPASSES RLS. orgdb.System returns a handle that wraps every
// statement in a transaction running set_config('app.org_id', …). A bug in a
// handler still cannot read another tenant, because the database is doing the
// filtering rather than the Go code.
//
// MUST run after PlatformAuth. It refuses to proceed without an authenticated
// platform identity, so mounting it on a route group that lacks PlatformAuth
// fails closed instead of granting anonymous tenant access.
func ActAsOrg(root *sqlx.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := PlatformFromContext(r.Context())
			if !ok {
				// Defence against a wiring mistake: this middleware without
				// PlatformAuth in front of it would otherwise bind a tenant
				// handle for an unauthenticated caller.
				log.Println("🚨 [ActAsOrg] no platform identity in context — refusing " +
					"(this middleware must be mounted behind PlatformAuth)")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			slug := strings.ToLower(strings.TrimSpace(r.Header.Get(ActAsOrgHeader)))
			if slug == "" {
				// FAIL CLOSED. Never fall back to "the first org" or "the only
				// org" — a silent default is how an operator edits the wrong
				// tenant's data believing they are somewhere else.
				http.Error(w,
					"Missing "+ActAsOrgHeader+" header: a platform request must name the organization it acts on",
					http.StatusBadRequest)
				return
			}

			var org struct {
				ID     string `db:"id"`
				Slug   string `db:"slug"`
				Status string `db:"status"`
			}
			// Raw pool: `organizations` carries the permissive org_catalog_read
			// policy, and this lookup is deliberately cross-tenant — it is how
			// we discover which tenant to scope to. Every query AFTER this one
			// goes through the org-bound handle.
			err := root.Get(&org,
				`SELECT id, slug, status FROM organizations WHERE LOWER(slug) = $1`, slug)
			if errors.Is(err, sql.ErrNoRows) {
				log.Printf("🔒 [ActAsOrg] %s named unknown organization %q", claims.Email, slug)
				http.Error(w, "Unknown organization", http.StatusNotFound)
				return
			}
			if err != nil {
				log.Printf("❌ [ActAsOrg] organization lookup failed: %v", err)
				http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			if org.Status != "active" {
				// Operators are told WHY, unlike tenant login where naming a
				// suspended org would leak its existence to an outsider. A
				// platform admin already knows every org exists.
				log.Printf("🔒 [ActAsOrg] %s tried inactive organization %q (%s)", claims.Email, org.Slug, org.Status)
				http.Error(w, "Organization is "+org.Status, http.StatusForbidden)
				return
			}

			d, err := orgdb.System(root, org.ID)
			if err != nil {
				log.Printf("❌ [ActAsOrg] could not bind organization %s: %v", org.ID, err)
				http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			defer d.Release() // reaps parked read-txs, including on panic paths

			// Re-audit with the RESOLVED organization id. PlatformAuth already
			// wrote a row using the raw header, which is a slug — or any
			// unbounded string a caller chose to send — in a column named
			// target_org_id. This row is the authoritative one.
			auditActAsOrg(root, claims, org.ID, org.Slug, r)

			log.Printf("🛰️  [ActAsOrg] %s acting as %q (%s) — %s %s",
				claims.Email, org.Slug, org.ID, r.Method, r.URL.Path)

			next.ServeHTTP(w, r.WithContext(orgdb.NewContext(r.Context(), d)))
		})
	}
}

func auditActAsOrg(root *sqlx.DB, c *PlatformClaims, orgID, slug string, r *http.Request) {
	if _, err := root.Exec(
		`INSERT INTO platform_audit_log (admin_id, admin_email, target_org_id, method, path, at)
		 VALUES ($1, $2, $3, $4, $5, EXTRACT(EPOCH FROM NOW())::BIGINT)`,
		c.AdminID, c.Email, orgID, r.Method, r.URL.Path+" [acting as "+slug+"]"); err != nil {
		log.Printf("🚨 [ActAsOrg] AUDIT WRITE FAILED for %s acting as %s: %v", c.Email, slug, err)
	}
}
