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
// SupportUserEmailDomain is reserved, and enforced in two places: CreateUser
// rejects it, and ensureSupportUser refuses to adopt a row under it that carries
// a usable password. Without both, a tenant could pre-create the address and own
// the identity Binly's writes are attributed to.
//
// It is also the marker every notification fan-out filters on. The support user
// holds role='admin' so its WRITES attribute correctly, but it is Binly staff
// rather than the customer's — six queries select admins for push, digests and
// in-app notifications, and without the exclusion it would accumulate a
// permanently-unread inbox row per notification, inflate the customer's
// recipient counts, and (if ever registered for FCM) put a Binly device on their
// admin push list. Grep for binly-platform.internal to find all of them.
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
	var found struct {
		ID       string `db:"id"`
		Password string `db:"password"`
	}
	err := d.Get(&found,
		`SELECT id, password FROM users WHERE organization_id = $1 AND email = $2`, orgID, email)
	if err == nil {
		// NEVER adopt a row we did not write. A tenant admin who created
		// support+{slug}@binly-platform.internal themselves would otherwise have
		// their row adopted as the support identity — giving them a login for the
		// account every Binly write is attributed to. The password column is the
		// proof of provenance: ours is the unusable sentinel, theirs is a real
		// bcrypt hash they chose.
		//
		// CreateUser now rejects the reserved domain, but that does not cover
		// rows created before this guard existed or inserted by direct SQL, so
		// this check stands on its own.
		if found.Password != unusablePasswordHash {
			return "", fmt.Errorf(
				"the support identity for organization %s is occupied by a row this system did not create "+
					"(email %s exists with a usable password) — refusing to attribute platform writes to it",
				orgSlug, email)
		}
		recordSupportUser(root, orgID, found.ID)
		return found.ID, nil
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
	var userID string
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
