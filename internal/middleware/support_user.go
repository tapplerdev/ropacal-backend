package middleware

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/orgdb"
)

// The per-organization support identity that a platform operator's writes are
// attributed to.
//
// SupportUserEmailDomain is reserved. It is not a routable domain and no tenant
// would legitimately create an address under it, which is what keeps the
// convention from colliding with a real person.
const (
	SupportUserEmailDomain = "binly-platform.internal"
	SupportUserName        = "Binly Support"
)

// SupportUserEmail is deterministic per organization, so the row is
// self-identifying even if platform_support_users is ever lost.
func SupportUserEmail(orgSlug string) string {
	return "support+" + orgSlug + "@" + SupportUserEmailDomain
}

// unusablePasswordHash is stored in the support user's password column.
//
// `users.password` is NOT NULL, so a value is required — but this one is not a
// valid bcrypt hash at all, so bcrypt.CompareHashAndPassword returns an error
// for EVERY input. Logging in as the support user is therefore impossible by
// construction rather than by policy: there is no correct password, and no
// secret that could leak because none exists.
const unusablePasswordHash = "!platform-support-no-login"

// ensureSupportUser returns the id of the organization's support user, creating
// it if absent.
//
// Idempotent by design and safe to race: the INSERT relies on the existing
// uq_users_org_email constraint with ON CONFLICT DO NOTHING, so two concurrent
// platform requests converge on one row rather than creating two.
//
// The user row MUST be written through the org-bound handle: `users` is under
// FORCE RLS with a WITH CHECK on organization_id, and the column defaults from
// app.org_id — both of which only hold inside an org-scoped transaction. The
// platform_support_users mapping, by contrast, must be written on the RAW pool,
// because its own policy requires app.org_id to be UNSET. That split is why this
// is two statements rather than one transaction.
func ensureSupportUser(root *sqlx.DB, d *orgdb.DB, orgID, orgSlug string) (string, error) {
	email := SupportUserEmail(orgSlug)

	// `users` is the authority, not the mapping table. Deriving the id from the
	// tenant's own data means a lost or stale mapping row is self-healing rather
	// than a second support user.
	var userID string
	err := d.Get(&userID,
		`SELECT id FROM users WHERE organization_id = $1 AND email = $2`, orgID, email)
	if err == nil {
		recordSupportUser(root, orgID, userID)
		return userID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("look up support user: %w", err)
	}

	newID := uuid.NewString()
	now := time.Now().Unix()
	if _, err := d.Exec(
		`INSERT INTO users (id, email, password, name, role, organization_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'admin', $5, $6, $6)
		 ON CONFLICT ON CONSTRAINT uq_users_org_email DO NOTHING`,
		newID, email, unusablePasswordHash, SupportUserName, orgID, now); err != nil {
		return "", fmt.Errorf("create support user: %w", err)
	}

	// Re-read rather than trusting newID: ON CONFLICT DO NOTHING means a
	// concurrent request may have won, in which case the surviving row's id is
	// the one to use.
	if err := d.Get(&userID,
		`SELECT id FROM users WHERE organization_id = $1 AND email = $2`, orgID, email); err != nil {
		return "", fmt.Errorf("read back support user: %w", err)
	}

	log.Printf("🛠️  [Platform] created support user for organization %s (%s)", orgSlug, userID)
	recordSupportUser(root, orgID, userID)
	return userID, nil
}

// recordSupportUser keeps platform_support_users in step with `users`.
//
// Best-effort: the mapping is a convenience for listing and labelling, never the
// source of truth, so a failure here must not deny an operator access. If it
// drifts, the next call re-derives it from `users` and corrects it.
func recordSupportUser(root *sqlx.DB, orgID, userID string) {
	if _, err := root.Exec(
		`INSERT INTO platform_support_users (organization_id, user_id, created_at)
		 VALUES ($1, $2, EXTRACT(EPOCH FROM NOW())::BIGINT)
		 ON CONFLICT (organization_id) DO UPDATE SET user_id = EXCLUDED.user_id`,
		orgID, userID); err != nil {
		log.Printf("⚠️  [Platform] could not record support-user mapping for %s: %v", orgID, err)
	}
}
