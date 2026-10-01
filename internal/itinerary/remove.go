package itinerary

import "github.com/jmoiron/sqlx"

// RemoveByIDs soft-deletes the given route_tasks (is_deleted=true) with an audit
// trail (deleted_at / deleted_by / deletion_reason) — never a hard delete, so the
// row survives for shift history. This is the one audited removal primitive; the
// scattered soft-delete-with-audit blocks across the shift lifecycle migrate onto
// it. Cross-domain side effects of a removal (releasing a move to backlog +
// logging, unassigning a potential_location) remain the caller's responsibility.
// Runs inside the caller's transaction (ext). No-op for an empty id list.
//
// The `is_deleted = false` guard makes re-removal idempotent: removing an
// already-removed task is a no-op that PRESERVES the original deletion's audit
// fields rather than overwriting them.
//
// It also records each removal on the shift's edit timeline — in the SAME
// statement. The UPDATE's RETURNING feeds the history INSERT, so:
//   - a task cannot be removed without being logged, even by a caller that is
//     not in a transaction (SyncPlacementRemoval passes the bare pool);
//   - exactly the rows THIS call removed are logged. The idempotency guard
//     applies to both halves, so re-removing a task neither re-deletes nor
//     re-logs it.
//
// Warehouse stops are removed but not logged: they are optimizer artifacts,
// regenerated on every optimize, not edits anyone made. Postgres runs a
// data-modifying CTE exactly once whether or not the outer query reads its
// rows, so a call that removes ONLY warehouse stops still removes them.
func RemoveByIDs(ext sqlx.Ext, ids []string, by, reason string, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	q, args, err := sqlx.In(`
		WITH removed AS (
			UPDATE route_tasks
			SET is_deleted = true, deleted_at = ?, deleted_by = ?, deletion_reason = ?, updated_at = ?
			WHERE id IN (?) AND is_deleted = false
			RETURNING id::text AS id, shift_id, task_type, bin_number, move_request_id, organization_id
		)
		INSERT INTO shift_edit_history
			(organization_id, shift_id, event_type, task_id, task_type, bin_number,
			 move_request_id, actor_id, reason, created_at)
		SELECT organization_id, shift_id, ?, id, task_type, bin_number,
			   move_request_id, ?, ?, ?
		  FROM removed
		 WHERE task_type <> 'warehouse_stop'`,
		now, by, reason, now, ids,
		string(EditTaskRemoved), actorOrNull(by), reasonOrNull(reason), now)
	if err != nil {
		return err
	}
	_, err = ext.Exec(ext.Rebind(q), args...)
	return err
}

// reasonOrNull keeps an empty reason out of the timeline as "", which the
// dashboard would otherwise render as a blank "because".
func reasonOrNull(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}
