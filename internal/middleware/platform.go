package middleware

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jmoiron/sqlx"
)

// Platform (cross-tenant) authentication.
//
// A platform admin can reach every tenant's data, so this file is the single
// most security-sensitive boundary in the codebase and is written to fail closed
// at every step.
//
// THREE INDEPENDENT THINGS MUST ALL HOLD for a request to pass:
//
//  1. PLATFORM_JWT_SECRET is configured, and is genuinely DIFFERENT from
//     APP_JWT_SECRET — asserted at boot by AssertPlatformSecretDistinct, which
//     refuses to start otherwise. That assertion is what actually delivers "a
//     leak of the tenant signing key cannot mint platform tokens"; an earlier
//     version of this comment claimed the platform claim below did it, which was
//     wrong: with matching secrets, anyone able to mint a tenant token can add
//     platform:true and be accepted. Unset disables the whole surface.
//  2. The token is signed with that secret AND carries `platform: true`. This
//     separates the two token families; it does NOT survive misconfigured
//     matching secrets on its own (see 1).
//  3. The admin still exists and is active in platform_admins, checked on EVERY
//     request. No caching here, unlike the tenant-side membership check: the
//     traffic is a handful of requests from a handful of humans, and revoking
//     access to every tenant's data should not wait up to 60 seconds.

type platformCtxKey struct{}

// PlatformClaims is the platform token payload. Note what is ABSENT: there is no
// org_id, which is what makes these tokens useless on the tenant routes —
// middleware.Org rejects an empty OrgID outright.
type PlatformClaims struct {
	AdminID string
	Email   string
}

// PlatformFromContext returns the authenticated platform admin, if any.
func PlatformFromContext(ctx context.Context) (*PlatformClaims, bool) {
	c, ok := ctx.Value(platformCtxKey{}).(*PlatformClaims)
	return c, ok
}

// PlatformSecret returns the platform signing secret, or "" when the platform
// surface is disabled.
func PlatformSecret() string { return os.Getenv("PLATFORM_JWT_SECRET") }

// PlatformTokenTTL matches the Centrifugo connection-token TTL from D4a. Short
// on purpose — this credential reaches every tenant.
const PlatformTokenTTL = time.Hour

// PlatformAuth authenticates a platform operator and records the request.
//
// root is the raw pool, NOT an org-bound handle: platform_admins has no
// organization_id and no RLS, so there is no org to bind and nothing for RLS to
// filter. This is the one place in the codebase where using the raw pool is
// correct rather than a bug.
func PlatformAuth(root *sqlx.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secret := PlatformSecret()
			if secret == "" {
				// Disabled, not misconfigured. 404 rather than 401 so a probe
				// cannot distinguish "platform surface exists but I lack
				// credentials" from "no such route".
				http.NotFound(w, r)
				return
			}

			claims, err := parsePlatformToken(r, secret)
			if err != nil {
				// Audited: a rejected token is a signal, not noise. Previously
				// these returned before any audit write, so a revoked operator
				// probing tenant data with a still-live token left ZERO rows —
				// the highest-value event in the log was the one it missed.
				log.Printf("🔒 [Platform] rejected: %v (%s %s)", err, r.Method, r.URL.Path)
				auditPlatformDenied(root, "(unauthenticated)", "(unknown)", r, err.Error())
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Re-check the admin on every request. A disabled or deleted admin
			// loses access immediately, not eventually.
			var row struct {
				Email  string `db:"email"`
				Status string `db:"status"`
			}
			err = root.Get(&row,
				`SELECT email, status FROM platform_admins WHERE id = $1`, claims.AdminID)
			if errors.Is(err, sql.ErrNoRows) {
				log.Printf("🔒 [Platform] token for unknown admin %s — rejected", claims.AdminID)
				auditPlatformDenied(root, claims.AdminID, "(deleted)", r, "admin no longer exists")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if err != nil {
				// Fail CLOSED. A database problem must not become open access to
				// every tenant.
				log.Printf("❌ [Platform] admin lookup failed: %v", err)
				http.Error(w, "Service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			if row.Status != "active" {
				log.Printf("🔒 [Platform] admin %s is %s — rejected", row.Email, row.Status)
				auditPlatformDenied(root, claims.AdminID, row.Email, r, "admin is "+row.Status)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			claims.Email = row.Email

			// No audit row here. ActAsOrg writes the authoritative one once the
			// organization is RESOLVED — this layer only has the raw header,
			// which is an unvalidated caller-supplied string and was previously
			// being stored in a column named target_org_id. Routes that never
			// reach ActAsOrg (whoami) are audited by their own handler.
			ctx := context.WithValue(r.Context(), platformCtxKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ActAsOrgHeader names the tenant a platform request is acting on (Phase 2).
// Declared here so the audit trail can record it from Phase 1, before the
// act-as machinery exists — an early request that names an org should still be
// attributable.
const ActAsOrgHeader = "X-Act-As-Org"

func parsePlatformToken(r *http.Request, secret string) (*PlatformClaims, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, errors.New("no authorization header")
	}
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return nil, errors.New("malformed authorization header")
	}

	token, err := jwt.Parse(parts[1], func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	},
		// REQUIRE exp. Without this, jwt.Parse happily accepts a token that
		// simply omits the claim — verified in review: a forged token with no
		// exp authenticated indefinitely. PlatformTokenTTL was being honoured
		// only by the minter, while a short lifetime is one of the three named
		// mitigations for a leaked platform credential.
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	mc, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, errors.New("invalid token")
	}

	// THE load-bearing check: a token without this claim is not a platform
	// token, whatever else it carries.
	//
	// It is NOT sufficient on its own, and an earlier comment here wrongly
	// implied it was. If PLATFORM_JWT_SECRET equals APP_JWT_SECRET, anyone who
	// can mint a tenant token can add platform:true and be accepted — the review
	// demonstrated exactly that. What actually prevents it is
	// AssertPlatformSecretDistinct, enforced at boot.
	if p, _ := mc["platform"].(bool); !p {
		return nil, errors.New("not a platform token")
	}

	adminID, ok := mc["admin_id"].(string)
	if !ok || adminID == "" {
		return nil, errors.New("missing admin_id claim")
	}
	return &PlatformClaims{AdminID: adminID}, nil
}

// AuditPlatformLogin records an authentication attempt.
//
// Login sits OUTSIDE the PlatformAuth group (it is what mints the token), so it
// was not covered by the per-request audit — meaning the single most important
// event, an operator acquiring a credential that reaches every tenant, existed
// only in stdout. Successes AND failures are recorded: a burst of failures is
// exactly the signal worth having.
//
// adminID is empty when the email matched no admin; the path column carries the
// outcome so a reader can tell attempts apart without a status_code.
func AuditPlatformLogin(root *sqlx.DB, adminID, email, outcome string, r *http.Request) {
	id := adminID
	if id == "" {
		id = "(unknown)"
	}
	if _, err := root.Exec(
		`INSERT INTO platform_audit_log (admin_id, admin_email, target_org_id, method, path, at)
		 VALUES ($1, $2, NULL, $3, $4, $5)`,
		id, email, r.Method, "/api/platform/auth/login ["+outcome+" from "+clientIP(r)+"]",
		time.Now().Unix()); err != nil {
		log.Printf("🚨 [Platform] AUDIT WRITE FAILED for login attempt %q: %v", email, err)
	}
}

// auditPlatformDenied records a REJECTED platform request.
//
// Failures matter more than successes here: a revoked or forged credential
// probing tenant data is exactly what an audit trail is for, and these paths
// previously wrote nothing at all.
func auditPlatformDenied(root *sqlx.DB, adminID, email string, r *http.Request, reason string) {
	if _, err := root.Exec(
		`INSERT INTO platform_audit_log (admin_id, admin_email, target_org_id, method, path, status_code, at)
		 VALUES ($1, $2, NULL, $3, $4, 401, EXTRACT(EPOCH FROM NOW())::BIGINT)`,
		adminID, email, r.Method, r.URL.Path+" [DENIED: "+reason+"]"); err != nil {
		log.Printf("🚨 [Platform] AUDIT WRITE FAILED for denial (%s): %v", reason, err)
	}
}

// AssertPlatformSecretDistinct refuses to boot the platform surface when its
// signing secret is not genuinely separate from the tenant one.
//
// The entire "a leak of the tenant signing key cannot mint platform tokens"
// property rests on the two secrets differing, and nothing was checking it. A
// review set both to the same value and minted a token with the TENANT key
// carrying platform:true — it was accepted, returning every organization.
//
// Fails the boot rather than warning: a warning in a startup log is not a
// control, and the failure mode is silent promotion of every tenant admin to a
// cross-tenant operator.
func AssertPlatformSecretDistinct() error {
	platform := os.Getenv("PLATFORM_JWT_SECRET")
	if platform == "" {
		return nil // platform surface disabled; nothing to protect
	}
	app := os.Getenv("APP_JWT_SECRET")
	if app != "" && subtle.ConstantTimeCompare([]byte(platform), []byte(app)) == 1 {
		return errors.New(
			"PLATFORM_JWT_SECRET must differ from APP_JWT_SECRET — they are identical, " +
				"which would let any tenant token be forged into a platform token with " +
				"cross-tenant access to every organization. Generate a separate one: openssl rand -base64 48")
	}
	if len(platform) < 32 {
		return errors.New(
			"PLATFORM_JWT_SECRET is too short (need at least 32 characters) — it guards " +
				"cross-tenant access to every organization")
	}
	return nil
}
