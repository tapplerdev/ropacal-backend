package handlers

// FindMy-bridge tenancy (airtag_accounts.go, airtag_locations_internal.go).
//
// The /api/internal/* endpoints authenticate with INTERNAL_API_KEY and carry
// no user JWT, so requests arrive with no organization. Once the tenancy
// migration is live, the owning org is RESOLVED instead: enumerate the active
// organizations (the organizations catalog is readable by the app role —
// migration Part 10's org_catalog_read policy; org count is tiny) and probe,
// org-bound handle by org-bound handle, which tenant's scope contains the
// airtag/account/bin being written. Probes are EXISTS queries on indexed
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

func airtagOrgCachePut(key, orgID string) {
	airtagOrgCache.Lock()
	defer airtagOrgCache.Unlock()
	airtagOrgCache.m[key] = airtagOrgEntry{orgID: orgID, expiresAt: time.Now().Add(airtagOrgCacheTTL)}
}

// airtagOrgHandles returns the DB handles a bridge endpoint must fan out
// over: the single passthrough while tenancy is dark, or one org-bound
// handle per active organization once live.
func airtagOrgHandles(root *sqlx.DB) ([]*orgdb.DB, error) {
	if !orgdb.Migrated() {
		return []*orgdb.DB{orgdb.Passthrough(root)}, nil
	}
	ids, err := orgdb.ActiveOrgIDs(root)
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

	if orgID, ok := airtagOrgCacheGet(cacheKey); ok {
		d, err := orgdb.System(root, orgID)
		if err != nil {
			return nil, false, err
		}
		return d, true, nil
	}

	ids, err := orgdb.ActiveOrgIDs(root)
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
