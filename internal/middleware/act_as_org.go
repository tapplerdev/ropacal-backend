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

// PlatformActorEmail is the identity stamped on a platform request's synthetic
// tenant claims. Deliberately not a real address and not in any `users` table —
// if it ever appears in tenant data, something has written when it should not
// have been able to.
const PlatformActorEmail = "platform-operator@binly.internal"

// PlatformReadOnly restricts the act-as-org surface to safe methods.
//
// THIS IS THE SECURITY BOUNDARY, and it replaces one that turned out not to
// exist. Phase 2 originally claimed "reads work, attribution-requiring writes
// fail closed" — but that boundary was an accident of which handlers happened to
// call GetUserFromContext, not a rule. A review found roughly 20 write routes
// with no acting-user check at all that therefore succeeded silently, including:
//
//	POST /manager/users  — creates a user INSIDE the acted-upon tenant, with
//	                       role:"admin" accepted. An operator could mint a
//	                       permanent tenant admin, log in as it, and do anything
//	                       — unattributed, and invisible to platform_audit_log
//	                       because it is then ordinary tenant traffic. Strictly
//	                       worse than the synthesized identity Phase 2 refused to
//	                       build.
//	DELETE /manager/shifts/clear, .../purge, POST /manager/bins/load-real —
//	                       destructive, unattributed.
//
// Writes also silently dropped their audit rows: a PATCH to a bin committed the
// UPDATE while skipping the bin_change_log INSERT entirely, because the log is
// gated on having a user id.
//
// So the rule is now structural rather than incidental: an operator can SEE
// everything a tenant admin sees and change nothing. Writes return 405 with an
// explanation. Restoring them needs a real identity to attribute them to — a
// designated support user per organization, Phase 2b — not a relaxation here.
func PlatformReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			claims, _ := PlatformFromContext(r.Context())
			email := "(unauthenticated)"
			if claims != nil {
				email = claims.Email
			}
			log.Printf("🚫 [Platform] write refused: %s %s by %s acting as %q",
				r.Method, r.URL.Path, email, r.Header.Get(ActAsOrgHeader))
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w,
				"The platform surface is read-only. Writes must be attributed to a real user, "+
					"and a platform operator is not one — use the tenant's own credentials, or wait "+
					"for per-organization support users.",
				http.StatusMethodNotAllowed)
		}
	})
}

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
// MUST run after PlatformAuth, and PlatformReadOnly must be mounted alongside —
// see that function for why.
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
				auditPlatform(root, claims, "", slug, r, http.StatusNotFound)
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
				auditPlatform(root, claims, org.ID, org.Slug, r, http.StatusForbidden)
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

			// Synthetic tenant claims, so role-gated READS work.
			//
			// Without this, 17 of 73 GET routes returned 401 — including the
			// entire shift surface — because they call GetUserFromContext and
			// bail when it is absent. "The same views that tenant's admin gets"
			// was simply untrue.
			//
			// Safe ONLY because PlatformReadOnly blocks every mutating method:
			// nothing can write, so an empty UserID can never reach a column or
			// an audit row. UserID is deliberately left EMPTY rather than
			// faked — reads that filter by the current user return nothing,
			// which is honest, and if this value ever turns up in tenant data it
			// is proof that a write escaped the read-only guard.
			ctx = context.WithValue(ctx, UserContextKey, UserClaims{
				UserID: "",
				Email:  PlatformActorEmail,
				Role:   "admin",
				OrgID:  org.ID,
			})

			auditPlatform(root, claims, org.ID, org.Slug, r, 0)

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
func auditPlatform(root *sqlx.DB, c *PlatformClaims, orgID, slug string, r *http.Request, outcome int) {
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
	}
}
