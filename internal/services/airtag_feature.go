package services

import (
	"errors"
	"fmt"

	"ropacal-backend/internal/orgdb"
)

// AirtagTrackingEnabled reports whether the bound organization uses AirTag
// tracking — organizations.airtag_tracking (migration 00010), on for ropacal only.
//
// The FindMy bridge is single-tenant: it serves one company's Apple accounts and
// knows nothing about organizations. So drift checks, battery reports, the
// AirTag endpoints and bridge-location ownership all gate on this flag.
//
// The read filters by the org's own id. organizations carries a catalogue read
// policy beside its isolation policy, so an unfiltered read is not scoped by RLS
// the way a tenant table is (anchor_chains.go learned this the hard way).
func AirtagTrackingEnabled(db *orgdb.DB) (bool, error) {
	orgID := db.OrgID()
	if orgID == "" {
		if orgdb.Migrated() {
			// An unbound handle under live tenancy is a caller bug; answering
			// "on" would open the AirTag paths without a tenant.
			return false, errors.New("airtag_tracking: needs an org-bound handle")
		}
		// Tenancy dark: there is one organization, the one that has always
		// used AirTags. Pre-tenancy behaviour, unchanged.
		return true, nil
	}
	var on bool
	if err := db.Get(&on, `SELECT airtag_tracking FROM organizations WHERE id = $1`, orgID); err != nil {
		return false, fmt.Errorf("read airtag_tracking for organization %s: %w", orgID, err)
	}
	return on, nil
}
