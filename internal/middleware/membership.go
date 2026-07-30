package middleware

import (
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"ropacal-backend/internal/orgdb"
)

// D4b — close the revocation window without touching token lifetimes.
//
// The app JWT lives 7 days and middleware.Auth does no database lookup, so
// before this check a user removed from an organization kept full org-scoped
// read/write access for up to a week, and a DEMOTED admin kept admin authority
// just as long (RequireRole reads the role straight off the claim).
//
// Shortening the JWT was the obvious alternative and is the wrong lever: there
// is no refresh endpoint, so a short JWT logs dashboard users out on a timer
// and bounces drivers mid-shift, costing GPS tracking and shift state. A cached
// membership re-check gives revocation in ~60 seconds with no client change, no
// new endpoint, and no token-lifetime tradeoff.
//
// Not covered, deliberately, so the limit is stated rather than implied: a
// password change still revokes nothing. That needs a jti or token version on
// the user row, which is a separate change.

const (
	// How long a verified answer is trusted before re-querying. This is the
	// revocation window.
	membershipTTL = 60 * time.Second

	// On database trouble a previously-verified positive is served for at most
	// this long. Unbounded stale-serving would silently restore the 7-day
	// window during any sustained DB problem — precisely the failure this
	// check exists to close — so the staleness is capped and then requests
	// fail instead.
	membershipStaleCap = 5 * time.Minute

	// Entries are keyed per (user, org) and so bounded by real user count, but
	// cap it anyway and sweep on overflow rather than trusting that forever.
	membershipCacheMax = 10000
)

type membershipEntry struct {
	role     string
	member   bool
	verified time.Time // last time the DB actually answered
}

var (
	membershipMu    sync.Mutex
	membershipCache = make(map[string]membershipEntry)
)

type membershipVerdict int

const (
	membershipAllow membershipVerdict = iota
	membershipDeny
	membershipUnavailable
)

func membershipStore(key string, e membershipEntry) {
	membershipMu.Lock()
	defer membershipMu.Unlock()

	if len(membershipCache) >= membershipCacheMax {
		cutoff := time.Now().Add(-membershipStaleCap)
		for k, v := range membershipCache {
			if v.verified.Before(cutoff) {
				delete(membershipCache, k)
			}
		}
		// Still full of live entries: drop the whole map rather than grow
		// without bound. Costs one re-query per active user.
		if len(membershipCache) >= membershipCacheMax {
			log.Printf("⚠️  [Org] membership cache hit %d live entries — clearing", membershipCacheMax)
			membershipCache = make(map[string]membershipEntry)
		}
	}
	membershipCache[key] = e
}

// verifyMembership confirms the token's user still belongs to the token's
// organization, that the organization is still active, and reports the role the
// DATABASE currently holds (which the caller compares against the claim).
//
// The query MUST run on the org-bound handle d, never the root pool. `users`
// carries exactly one policy — org_isolation_users, USING (organization_id =
// current_setting('app.org_id')) — under FORCE ROW LEVEL SECURITY. On the root
// pool app.org_id is unset, so that predicate is never true and the query
// returns zero rows for EVERY user, forever. An earlier draft of this check did
// exactly that and would have 401'd every authenticated request in production.
// Verified empirically before shipping: as binly_app with no org context,
// `SELECT count(*) FROM users` returns 0.
//
// organizations is joinable here because it carries a SECOND, permissive policy
// (org_catalog_read, FOR SELECT USING (true)) that ORs with its isolation
// policy. users has no such policy — that asymmetry is the whole trap, and it is
// why the two precedents in this codebase that DO query on the root pool
// (soleActiveOrgID, CentrifugoProxyAuth.isMultiTenant) are not applicable
// models. Joining lets one round trip answer both questions.
func verifyMembership(d *orgdb.DB, userID, orgID string) (membershipVerdict, string) {
	key := userID + "|" + orgID
	now := time.Now()

	membershipMu.Lock()
	entry, cached := membershipCache[key]
	membershipMu.Unlock()

	if cached && now.Sub(entry.verified) < membershipTTL {
		if entry.member {
			return membershipAllow, entry.role
		}
		return membershipDeny, ""
	}

	var role string
	err := d.Get(&role, `
		SELECT u.role
		FROM users u
		JOIN organizations o ON o.id = u.organization_id
		WHERE u.id = $1 AND u.organization_id = $2 AND o.status = 'active'`,
		userID, orgID)

	switch {
	case err == nil:
		membershipStore(key, membershipEntry{role: role, member: true, verified: now})
		return membershipAllow, role

	case errors.Is(err, sql.ErrNoRows):
		// Definitive: the user is gone, was moved to another tenant, or the
		// organization is no longer active. Cache the negative too, so a
		// revoked token hammering the API cannot turn into a query per request.
		membershipStore(key, membershipEntry{member: false, verified: now})
		return membershipDeny, ""

	default:
		// Database trouble, not an answer. Fail closed, but bounded.
		if cached && !entry.member {
			// We already knew they were out. Denying stays fail-closed and is
			// more accurate than a 503.
			return membershipDeny, ""
		}
		if cached && entry.member && now.Sub(entry.verified) <= membershipStaleCap {
			log.Printf("⚠️  [Org] membership check failed for %s, serving cached positive (age %s): %v",
				userID, now.Sub(entry.verified).Truncate(time.Second), err)
			return membershipAllow, entry.role
		}
		log.Printf("❌ [Org] membership check unavailable for %s: %v", userID, err)
		return membershipUnavailable, ""
	}
}
