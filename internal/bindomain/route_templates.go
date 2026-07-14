package bindomain

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// PruneFromRouteTemplates removes a bin from every route template (blueprint) it
// belongs to and rewrites the affected templates' denormalized bin_count from the
// surviving route_bins rows. Call it inside the caller's transaction whenever a bin
// leaves the `active` state (→ missing / retired / in_storage): only active bins
// belong on a collection template, so a deactivated bin must drop off the template
// immediately rather than linger as a stale reference. (The shift builder already
// skips inactive bins at build time — route_tasks.go — but the template's membership
// and count would otherwise stay wrong, inflating counts and nagging the resolve
// banner every build.)
//
// Reversal is deliberate: reactivating a bin does NOT re-add it — templates are
// re-curated by hand, matching how they're built in the first place.
//
// bin_count is RECOMPUTED from the surviving rows (not decremented), so it self-heals
// even when it had already drifted from the true row count. Idempotent: a bin that is
// not on any template is a clean no-op. Returns the number of templates pruned (for
// logging). Runs on the caller's `ext` (pool or tx) via Rebind, per the house
// domain-writer convention (mirrors itinerary.RemoveByIDs / RecomputeShiftCounts).
func PruneFromRouteTemplates(ext sqlx.Ext, binID string, now int64) (int, error) {
	// Capture the affected templates BEFORE the delete so we recompute exactly those.
	var routeIDs []string
	if err := sqlx.Select(ext, &routeIDs, ext.Rebind(`
		SELECT DISTINCT route_id FROM route_bins WHERE bin_id = ?`), binID); err != nil {
		return 0, fmt.Errorf("prune templates: find routes for bin %s: %w", binID, err)
	}
	if len(routeIDs) == 0 {
		return 0, nil
	}

	if _, err := ext.Exec(ext.Rebind(`
		DELETE FROM route_bins WHERE bin_id = ?`), binID); err != nil {
		return 0, fmt.Errorf("prune templates: delete route_bins for bin %s: %w", binID, err)
	}

	// Recompute bin_count from the rows that remain, for each affected template.
	q, args, err := sqlx.In(`
		UPDATE routes
		SET bin_count = (SELECT COUNT(*) FROM route_bins WHERE route_id = routes.id),
		    updated_at = ?
		WHERE id IN (?)`, now, routeIDs)
	if err != nil {
		return 0, fmt.Errorf("prune templates: build recount: %w", err)
	}
	if _, err := ext.Exec(ext.Rebind(q), args...); err != nil {
		return 0, fmt.Errorf("prune templates: recount bin_count: %w", err)
	}
	return len(routeIDs), nil
}
