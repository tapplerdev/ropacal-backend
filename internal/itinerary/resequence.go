package itinerary

import "github.com/jmoiron/sqlx"

// Resequence renumbers a shift's live tasks to a dense 1..N in canonical order:
// completed tasks first (they're behind the driver), then every pending stop in
// its existing sequence_order (then created_at). It never reorders the *pending*
// stops among themselves — it closes gaps and normalizes positions only. One
// statement (replaces the per-task renumber loop).
//
// It deliberately does NOT sort warehouse stops last. Warehouse stops are
// optimizer-placed, and a mid-route reload run's position is load-bearing — the
// driver must reload BEFORE the placements it feeds, so sorting reloads to the
// end yields a physically impossible route (place N bins, THEN go load them).
// The end-of-route depot return already carries the highest sequence_order, so
// it stays last on its own. Both callers need this interleaving preserved: the
// reopt renumber (ApplyOrder) and the lock_route_order resequence-only path
// (where warehouse-last would also violate the manager's pinned order).
//
// This is one of the two and only writers of sequence_order — the
// order-preserving one. ApplyOrder is the other (it stamps the optimizer's
// decided order). Runs inside the caller's transaction (ext).
func Resequence(ext sqlx.Ext, shiftID string) error {
	q := ext.Rebind(`
		UPDATE route_tasks AS rt
		SET sequence_order = sub.rn
		FROM (
			SELECT id, ROW_NUMBER() OVER (
				ORDER BY is_completed DESC,
				         sequence_order ASC, created_at ASC
			) AS rn
			FROM route_tasks
			WHERE shift_id = ? AND is_deleted = false
		) sub
		WHERE rt.id = sub.id`)
	_, err := ext.Exec(q, shiftID)
	return err
}
