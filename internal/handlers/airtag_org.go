package handlers

// FindMy-bridge tenancy (airtag_accounts.go, airtag_locations_internal.go).
//
// The /api/internal/* endpoints authenticate with INTERNAL_API_KEY and carry
// no user JWT, so requests arrive with no organization. Once the tenancy
// migration is live, the owning org is RESOLVED instead: enumerate the active
// organizations WITH AIRTAG TRACKING (organizations.airtag_tracking, migration
// 00010 — the bridge serves one company, so no other org may be read from or
// written into; the catalog is readable by the app role via org_catalog_read;
// org count is tiny) and probe, org-bound handle by org-bound handle, which
// tenant's scope contains the airtag/account/bin being written. Probes are EXISTS queries on indexed
// columns and results are cached in-process for an hour, so steady state is
// one map lookup per row.
//
// Exactly one organization may claim an identifier:
//   - zero claims  → not found (404 / row skipped) — never write an unowned row;
//   - multiple claims → ambiguous → treated as not found, with a loud log —
//     fail closed rather than guess a tenant.
//
// While tenancy is dark none of this runs: callers get the single passthrough
// handle immediately and behavior is byte-identical to pre-tenancy.

import (
	"fmt"
	"log"
	"sync"
	"time"

	"ropacal-backend/internal/orgdb"

	"github.com/jmoiron/sqlx"
)

// airtagOrgCacheTTL bounds how long an identifier→org resolution is reused
// before re-probing (covers the rare manual re-homing of a tag/account).
const airtagOrgCacheTTL = time.Hour

type airtagOrgEntry struct {
	orgID     string
	expiresAt time.Time
}

var airtagOrgCache = struct {
	sync.Mutex
	m map[string]airtagOrgEntry
}{m: make(map[string]airtagOrgEntry)}

func airtagOrgCacheGet(key string) (string, bool) {
	airtagOrgCache.Lock()
	defer airtagOrgCache.Unlock()
	e, ok := airtagOrgCache.m[key]
	if !ok || time.Now().After(e.expiresAt) {
		delete(airtagOrgCache.m, key)
		return "", false
	}
	return e.orgID, true
}

func airtagOrgCacheDelete(key string) {
	airtagOrgCache.Lock()
	defer airtagOrgCache.Unlock()
	delete(airtagOrgCache.m, key)
}

func airtagOrgCachePut(key, orgID string) {
	airtagOrgCache.Lock()
	defer airtagOrgCache.Unlock()
	airtagOrgCache.m[key] = airtagOrgEntry{orgID: orgID, expiresAt: time.Now().Add(airtagOrgCacheTTL)}
}

// airtagOrgIDs lists the active organizations with AirTag tracking on
// (organizations.airtag_tracking) — the only ones a bridge endpoint may read
// from or write into. The bridge serves one company and knows nothing about
// organizations; without this filter, another org merely owning a bin with the
// same NUMBER made a tag "ambiguous", and its locations were dropped.
//
// Read on the root pool, like orgdb.ActiveOrgIDs: org_catalog_read permits an
// unscoped read of the catalogue while no tenant is bound.
func airtagOrgIDs(root *sqlx.DB) ([]string, error) {
	var ids []string
	err := root.Select(&ids, `SELECT id FROM organizations WHERE status = 'active' AND airtag_tracking ORDER BY created_at, id`)
	return ids, err
}

// airtagOrgEligible applies airtagOrgIDs' predicate to one org, so a cached
// resolution is re-checked against exactly what a fresh scan would consider.
func airtagOrgEligible(root *sqlx.DB, orgID string) (bool, error) {
	var ok bool
	err := root.Get(&ok, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1 AND status = 'active' AND airtag_tracking)`, orgID)
	return ok, err
}

// airtagOrgHandles returns the DB handles a bridge endpoint must fan out
// over: the single passthrough while tenancy is dark, or one org-bound
// handle per active organization with AirTag tracking once live.
func airtagOrgHandles(root *sqlx.DB) ([]*orgdb.DB, error) {
	if !orgdb.Migrated() {
		return []*orgdb.DB{orgdb.Passthrough(root)}, nil
	}
	ids, err := airtagOrgIDs(root)
	if err != nil {
		return nil, fmt.Errorf("enumerating organizations: %w", err)
	}
	handles := make([]*orgdb.DB, 0, len(ids))
	for _, id := range ids {
		d, err := orgdb.System(root, id)
		if err != nil {
			return nil, err
		}
		handles = append(handles, d)
	}
	return handles, nil
}

// resolveAirtagOrg returns the org-bound handle for the single organization
// whose scope satisfies probe. found=false means no org (or more than one)
// claimed the identifier. While tenancy is dark it returns the passthrough
// immediately — no probing, no caching, pre-tenancy behavior byte for byte.
func resolveAirtagOrg(root *sqlx.DB, cacheKey string, probe func(d *orgdb.DB) (bool, error)) (d *orgdb.DB, found bool, err error) {
	if !orgdb.Migrated() {
		return orgdb.Passthrough(root), true, nil
	}

	// CACHE HIT MUST STILL BE VERIFIED. Returning here unconditionally would skip
	// the ambiguity check below — the only cross-tenant defence on this path — so
	// the guard would run on a MISS and never on a HIT. With a 1h TTL that means:
	// org A resolves bin 56 and caches it; org B later creates its own bin 56;
	// for up to an hour B's AirTag GPS is written into A's scope with no log, and
	// the rows persist after the cache expires. bin_number is NOT unique across
	// tenants (prod holds duplicates 56, 86, 116), and bin numbers restart per
	// tenant by design, so collisions are the expected case rather than the edge.
	//
	// Re-probing the cached org is one indexed lookup and keeps the hit path
	// authoritative: if that org no longer claims the identifier, fall through to
	// the full scan (which re-derives the owner and re-applies the guard).
	if orgID, ok := airtagOrgCacheGet(cacheKey); ok {
		// A cached answer is held to the same predicate the scan uses, so it
		// cannot outlive the org's AirTag tracking being switched off, the org
		// being suspended, or the org being deleted.
		eligible, err := airtagOrgEligible(root, orgID)
		if err != nil {
			return nil, false, fmt.Errorf("re-checking cached org %s: %w", orgID, err)
		}
		if eligible {
			d, err := orgdb.System(root, orgID)
			if err != nil {
				return nil, false, err
			}
			stillOwns, err := probe(d)
			if err != nil {
				return nil, false, fmt.Errorf("re-probing cached org %s: %w", orgID, err)
			}
			if stillOwns {
				return d, true, nil
			}
			log.Printf("⚠️  [AirtagOrg] cached org %s no longer claims %s — re-resolving", orgID, cacheKey)
		} else {
			log.Printf("⚠️  [AirtagOrg] cached org %s is no longer eligible (AirTag tracking off, inactive or gone) — re-resolving %s", orgID, cacheKey)
		}
		airtagOrgCacheDelete(cacheKey)
	}

	ids, err := airtagOrgIDs(root)
	if err != nil {
		return nil, false, fmt.Errorf("enumerating organizations: %w", err)
	}

	var owners []string
	var ownerHandle *orgdb.DB
	for _, id := range ids {
		h, err := orgdb.System(root, id)
		if err != nil {
			return nil, false, err
		}
		owned, err := probe(h)
		if err != nil {
			return nil, false, fmt.Errorf("probing org %s: %w", id, err)
		}
		if owned {
			owners = append(owners, id)
			ownerHandle = h
		}
	}

	switch len(owners) {
	case 0:
		return nil, false, nil
	case 1:
		airtagOrgCachePut(cacheKey, owners[0])
		return ownerHandle, true, nil
	default:
		log.Printf("🚨 [AirtagOrg] %s is claimed by %d organizations (%v) — ambiguous, refusing to guess a tenant",
			cacheKey, len(owners), owners)
		return nil, false, nil
	}
}

// airtagLocationProbe reports whether an org owns the airtag location being
// written: an existing airtag_locations row under the same id, an airtag_keys
// row carrying the tag's name, or (for bin-matched tags) a bin with the
// reported bin_number. All three run RLS-scoped through the org-bound handle.
func airtagLocationProbe(id, name string, binNumber *int) func(d *orgdb.DB) (bool, error) {
	return func(d *orgdb.DB) (bool, error) {
		var owned bool
		err := d.Get(&owned, `
			SELECT EXISTS(SELECT 1 FROM airtag_locations WHERE id = $1)
				OR EXISTS(SELECT 1 FROM airtag_keys WHERE name = $2)
				OR ($3::int IS NOT NULL AND EXISTS(SELECT 1 FROM bins WHERE bin_number = $3::int))
		`, id, name, binNumber)
		return owned, err
	}
}
