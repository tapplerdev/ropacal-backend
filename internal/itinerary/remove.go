package itinerary

import "github.com/jmoiron/sqlx"

// RemoveByIDs soft-deletes the given route_tasks (is_deleted=true) with an audit
// trail (deleted_at / deleted_by / deletion_reason) — never a hard delete, so the
// row survives for shift history. This is the one audited removal primitive; the
// scattered soft-delete-with-audit blocks across the shift lifecycle migrate onto
// it. Cross-domain side effects of a removal (releasing a move to backlog +
// logging, unassigning a potential_location) remain the caller's responsibility.
// Runs inside the caller's transaction (ext). No-op for an empty id list.
func RemoveByIDs(ext sqlx.Ext, ids []string, by, reason string, now int64) error {
	if len(ids) == 0 {
		return nil
	}
	q, args, err := sqlx.In(`
		UPDATE route_tasks
		SET is_deleted = true, deleted_at = ?, deleted_by = ?, deletion_reason = ?, updated_at = ?
		WHERE id IN (?) AND is_deleted = false`, now, by, reason, now, ids)
	if err != nil {
		return err
	}
	_, err = ext.Exec(ext.Rebind(q), args...)
	return err
}
