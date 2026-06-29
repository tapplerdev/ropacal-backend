package itinerary

import "github.com/jmoiron/sqlx"

// Resequence renumbers a shift's live tasks to a dense 1..N in canonical order:
// completed tasks first (they're behind the driver), warehouse stops last
// (structural), otherwise by current sequence_order (then created_at). It never
// reorders the *pending* stops among themselves — it closes gaps and normalizes
// positions only. One statement (replaces the per-task renumber loop).
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
				         CASE WHEN task_type = 'warehouse_stop' THEN 1 ELSE 0 END ASC,
				         sequence_order ASC, created_at ASC
			) AS rn
			FROM route_tasks
			WHERE shift_id = ? AND is_deleted = false
		) sub
		WHERE rt.id = sub.id`)
	_, err := ext.Exec(q, shiftID)
	return err
}
