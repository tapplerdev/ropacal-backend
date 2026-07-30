package middleware

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
)

// ActAsOrg binds ONE tenant's database handle to a platform request, so the
// ordinary tenant handlers serve that tenant unchanged.
//
// This is the whole of "an operator sees what that org's admin sees": every
// handler reads its scope from orgdb.From(r), so binding a real org-scoped
// handle means the handler is not imitating the tenant's view — it IS the
// tenant's view, filtered by the same RLS policies, through the same code.
//
// NOTHING HERE BYPASSES RLS. The handle wraps every statement in a transaction
// running set_config('app.org_id', …), so a bug in a handler still cannot read
// another tenant; the database does the filtering.
//
// MUST run after PlatformAuth, which is what establishes the operator identity
// and enforces the per-account can_write permission.
func ActAsOrg(root *sqlx.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := PlatformFromContext(r.Context())
			if !ok {
				// Defence against a wiring mistake: this middleware without
				// PlatformAuth in front would bind a tenant handle for an
				// unauthenticated caller.
				log.Println("🚨 [ActAsOrg] no platform identity in context — refusing " +
					"(must be mounted behind PlatformAuth)")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			slug := strings.ToLower(strings.TrimSpace(r.Header.Get(ActAsOrgHeader)))
			if slug == "" {
				// FAIL CLOSED. Never fall back to "the first org" or "the only
				// org" — a silent default is how an operator reads the wrong
				// tenant believing they are somewhere else.
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
			// we discover which tenant to scope to. Every query after this goes
			// through the org-bound handle.
			//
			// LOWER(slug) rather than slug: the unique index is case-sensitive,
			// so two rows could in principle differ only by case and root.Get
			// would take whichever came first. The provisioning API rejects
			// non-lowercase slugs, but a manual INSERT would not.
			err := root.Get(&org,
				`SELECT id, slug, status FROM organizations WHERE LOWER(slug) = $1
				 ORDER BY created_at, id LIMIT 1`, slug)
			if errors.Is(err, sql.ErrNoRows) {
				log.Printf("🔒 [ActAsOrg] %s named unknown organization %q", claims.Email, slug)
				_ = auditPlatform(root, claims, "", slug, r, http.StatusNotFound)
				http.Error(w, "Unknown organization", http.StatusNotFound)
				return
			}
			if err != nil {
				log.Printf("❌ [ActAsOrg] organization lookup failed: %v", err)
				http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			if org.Status != "active" {
				log.Printf("🔒 [ActAsOrg] %s tried inactive organization %q (%s)", claims.Email, org.Slug, org.Status)
				_ = auditPlatform(root, claims, org.ID, org.Slug, r, http.StatusForbidden)
				http.Error(w, "Organization is "+org.Status, http.StatusForbidden)
				return
			}

			// orgdb.New, NOT orgdb.System. System returns an UNSCOPED
			// passthrough when Migrated() is false — and Migrated() is a
			// boot-time snapshot, so a process that booted before the tenancy
			// migration would hand out an unscoped handle here even though we
			// have just proved tenancy is live by reading `organizations`. New
			// returns an error instead. middleware.Org already uses New; this
			// now matches it.
			d, err := orgdb.New(root, org.ID)
			if err != nil {
				log.Printf("❌ [ActAsOrg] could not bind organization %s: %v", org.ID, err)
				http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			defer d.Release() // reaps parked read-txs, panic paths included

			ctx := orgdb.NewContext(r.Context(), d)

			// Attribute this request to the organization's SUPPORT USER — a real
			// row in that tenant's `users` table, created on first use.
			//
			// This is what makes writes possible at all. 28 foreign keys point
			// at `users` and 115 sites read userClaims.UserID, so a fabricated id
			// would violate the constraints and an empty one silently skipped
			// audit writes (a bin edit committed while logging nothing). A real
			// row satisfies both, and the tenant's own history honestly reads
			// "Binly Support" rather than naming one of their own staff. WHICH
			// operator it was lives in platform_audit_log.
			supportUserID, err := ensureSupportUser(root, d, org.ID, org.Slug)
			if err != nil {
				// Fail CLOSED. Serving the request without a valid actor is how
				// unattributed writes happened before.
				log.Printf("❌ [ActAsOrg] could not establish support user for %s: %v", org.Slug, err)
				http.Error(w, "Could not establish an audit identity for this organization",
					http.StatusServiceUnavailable)
				return
			}
			ctx = context.WithValue(ctx, UserContextKey, UserClaims{
				UserID: supportUserID,
				Email:  SupportUserEmail(org.Slug),
				Role:   "admin",
				OrgID:  org.ID,
			})

			// FAIL CLOSED on an unwritable audit trail. This used to log and
			// continue, so a reviewer made the INSERT fail and still created a
			// bin — the record of who touched a customer's data is the entire
			// justification for allowing writes, so proceeding without it
			// contradicts the premise. ensureSupportUser twelve lines above
			// already fails closed for the same reason; this now matches it.
			if err := auditPlatform(root, claims, org.ID, org.Slug, r, 0); err != nil {
				log.Printf("🚨 [ActAsOrg] refusing request — audit trail unwritable: %v", err)
				http.Error(w, "Cannot record this action for audit; request refused",
					http.StatusServiceUnavailable)
				return
			}

			log.Printf("🛰️  [ActAsOrg] %s acting as %q (%s) — %s %s",
				claims.Email, org.Slug, org.ID, r.Method, r.URL.Path)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// auditPlatform writes the authoritative record for one platform request.
//
// One row, not two. PlatformAuth used to write an early row from the raw,
// unvalidated header — a slug, or any string a caller chose — into a column
// named target_org_id, and ActAsOrg wrote a second with the resolved UUID. The
// two were not correlatable (no request id), carried clocks from different
// sources, and left the target_org_id index unable to answer "what did this
// operator do to this tenant".
//
// outcome is the HTTP status when the request was rejected here, or 0 when it
// was passed to a handler (whose status this layer cannot see — the row records
// that access was granted, not what the handler returned).
func auditPlatform(root *sqlx.DB, c *PlatformClaims, orgID, slug string, r *http.Request, outcome int) error {
	var org interface{}
	if orgID != "" {
		org = orgID
	}
	var status interface{}
	if outcome != 0 {
		status = outcome
	}
	reqID := middleware.GetReqID(r.Context())
	path := r.URL.Path
	if slug != "" {
		path += " [as " + slug + "]"
	}
	if reqID != "" {
		path += " {" + reqID + "}"
	}
	if _, err := root.Exec(
		`INSERT INTO platform_audit_log (admin_id, admin_email, target_org_id, method, path, status_code, at)
		 VALUES ($1, $2, $3, $4, $5, $6, EXTRACT(EPOCH FROM NOW())::BIGINT)`,
		c.AdminID, c.Email, org, r.Method, path, status); err != nil {
		log.Printf("🚨 [Platform] AUDIT WRITE FAILED for %s acting as %s: %v", c.Email, slug, err)
		return err
	}
	return nil
}
