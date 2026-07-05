package itinerary

import (
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ── Phase 6: lifecycle writes folded from the handlers ────────────────────────
//
// Warehouse stops are SYSTEM ROUTING ARTIFACTS (born at shift creation or by the
// optimizer, regenerated on every (re)optimize). They get the package's only
// sanctioned HARD delete — soft-deleting regenerated artifacts would spam the
// task-history audit view with phantom rows. Everything driver- or manager-
// authored goes through RemoveByIDs (audited soft-delete).

// PurgeIncompleteWarehouseStops hard-deletes a shift's incomplete, non-deleted
// warehouse stops so the optimizer's regenerated ones can replace them.
// Completed ones (the driver actually visited) are kept. Callers on the
// first-opt path must run this BEFORE selecting existing tasks — stale
// warehouse rows must not survive into the renumber.
func PurgeIncompleteWarehouseStops(ext sqlx.Ext, shiftID string) (int64, error) {
	res, err := ext.Exec(ext.Rebind(`
		DELETE FROM route_tasks
		WHERE shift_id = ? AND task_type = 'warehouse_stop' AND is_completed = 0 AND is_deleted = false`), shiftID)
	if err != nil {
		return 0, fmt.Errorf("purge warehouse stops: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CompleteWarehouseStops completes a shift's incomplete warehouse stops — the
// GPS arrival-detection twin of PurgeIncompleteWarehouseStops (driver is at
// the warehouse with everything else done; the artifact is satisfied).
func CompleteWarehouseStops(ext sqlx.Ext, shiftID string, now int64) (int64, error) {
	res, err := ext.Exec(ext.Rebind(`
		UPDATE route_tasks SET is_completed = 1, completed_at = ?, updated_at = ?
		WHERE shift_id = ? AND task_type = 'warehouse_stop' AND is_completed = 0 AND is_deleted = false`),
		now, now, shiftID)
	if err != nil {
		return 0, fmt.Errorf("complete warehouse stops: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AutoCompletePreloadedRedeploymentPickups completes a shift's redeployment
// pickup legs when the driver starts with the bins already on the truck —
// the warehouse fetch is physically done, so the persisted route must not
// send the driver back for them. First-optimization path only.
func AutoCompletePreloadedRedeploymentPickups(ext sqlx.Ext, shiftID string, now int64) (int64, error) {
	res, err := ext.Exec(ext.Rebind(`
		UPDATE route_tasks SET is_completed = 1, completed_at = ?, updated_at = ?
		WHERE shift_id = ? AND task_type = 'pickup' AND is_deleted = false AND is_completed = 0
		  AND move_request_id IN (SELECT id FROM bin_move_requests WHERE move_type = 'redeployment')`),
		now, now, shiftID)
	if err != nil {
		return 0, fmt.Errorf("auto-complete preloaded pickups: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Skip marks a task skipped-with-reason (processed: is_completed=1 +
// skipped=true, so progress advances — see CountStops). When the task is a
// pickup with a move, the paired incomplete dropoff is cascade-skipped: the
// handler must not know how legs pair, and a skipped pickup must never
// strand an orphan dropoff. skipData is the caller-marshaled task_data JSON
// (e.g. {"skip_reason": ...}). Returns how many tasks were skipped (1 or 2).
func Skip(ext sqlx.Ext, shiftID, taskID string, taskType TaskType, moveRequestID *string, skipData []byte, now int64) (int, error) {
	const skipQ = `
		UPDATE route_tasks
		SET skipped = true, is_completed = 1, completed_at = ?, task_data = ?, updated_at = ?
		WHERE id = ?`
	if _, err := ext.Exec(ext.Rebind(skipQ), now, skipData, now, taskID); err != nil {
		return 0, fmt.Errorf("skip task: %w", err)
	}
	skipped := 1

	if taskType == Pickup && moveRequestID != nil {
		var dropoffID string
		err := sqlx.Get(ext, &dropoffID, ext.Rebind(`
			SELECT id FROM route_tasks
			WHERE shift_id = ? AND move_request_id = ? AND task_type = 'dropoff' AND is_completed = 0`),
			shiftID, *moveRequestID)
		switch {
		case err == sql.ErrNoRows:
			// no live paired dropoff — nothing to cascade
		case err != nil:
			return skipped, fmt.Errorf("find paired dropoff: %w", err)
		default:
			if _, err := ext.Exec(ext.Rebind(skipQ), now, skipData, now, dropoffID); err != nil {
				return skipped, fmt.Errorf("skip paired dropoff: %w", err)
			}
			skipped++
		}
	}
	return skipped, nil
}

// RemoveAllLive audited-soft-deletes EVERY live task on a shift. It serves
// the legacy full-recreate reoptimization branch (isFirstOptimization=false
// in optimizeRouteWithMapbox) — DEAD with today's single caller (StartShift
// passes true) and kept only until that branch is deleted. Prefer the
// ID-preserving ApplyOrder path for anything new.
func RemoveAllLive(ext sqlx.Ext, shiftID, by, reason string, now int64) error {
	_, err := ext.Exec(ext.Rebind(`
		UPDATE route_tasks
		SET is_deleted = true, deleted_at = ?, deleted_by = ?, deletion_reason = ?, updated_at = ?
		WHERE shift_id = ? AND is_deleted = false`),
		now, by, reason, now, shiftID)
	if err != nil {
		return fmt.Errorf("remove all live tasks: %w", err)
	}
	return nil
}

// SyncBinTasks refreshes the address/coordinates of a bin's incomplete tasks
// on active/scheduled shifts after a manager edits the bin's location — so
// drivers navigate to the new spot, not the old one. Returns shiftID→taskIDs
// so the caller can notify each affected shift. Runs in the caller's tx
// (atomic with the bin update itself).
func SyncBinTasks(ext sqlx.Ext, binID, address string, lat, lng *float64, now int64) (map[string][]string, error) {
	var affected []struct {
		ShiftID string `db:"shift_id"`
		TaskID  string `db:"task_id"`
	}
	if err := sqlx.Select(ext, &affected, ext.Rebind(`
		SELECT rt.shift_id, rt.id as task_id
		FROM route_tasks rt
		JOIN shifts s ON rt.shift_id = s.id
		WHERE rt.bin_id = ? AND rt.is_completed = 0 AND s.status IN ('active', 'scheduled')`),
		binID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("select bin tasks: %w", err)
	}
	byShift := make(map[string][]string, len(affected))
	for _, a := range affected {
		if _, err := ext.Exec(ext.Rebind(`
			UPDATE route_tasks SET address = ?, latitude = ?, longitude = ?, updated_at = ? WHERE id = ?`),
			address, lat, lng, now, a.TaskID); err != nil {
			return nil, fmt.Errorf("sync bin task %s: %w", a.TaskID, err)
		}
		byShift[a.ShiftID] = append(byShift[a.ShiftID], a.TaskID)
	}
	return byShift, nil
}

// SyncPlacementRemoval audited-soft-deletes the incomplete placement tasks
// referencing a potential location on active/scheduled shifts — used when the
// location is deleted or converted to a live bin (the placement is moot
// either way). Returns shiftID→taskIDs for driver notifications. Delegates
// the delete to RemoveByIDs (identical audit shape).
func SyncPlacementRemoval(ext sqlx.Ext, potLocID, by, reason string, now int64) (map[string][]string, error) {
	var affected []struct {
		ShiftID string `db:"shift_id"`
		TaskID  string `db:"task_id"`
	}
	if err := sqlx.Select(ext, &affected, ext.Rebind(`
		SELECT rt.shift_id, rt.id as task_id
		FROM route_tasks rt
		JOIN shifts s ON rt.shift_id = s.id
		WHERE rt.potential_location_id = ? AND rt.task_type = 'placement'
		  AND rt.is_completed = 0 AND s.status IN ('active', 'scheduled')`),
		potLocID); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("select placement tasks: %w", err)
	}
	if len(affected) == 0 {
		return map[string][]string{}, nil
	}
	byShift := make(map[string][]string, len(affected))
	ids := make([]string, 0, len(affected))
	for _, a := range affected {
		byShift[a.ShiftID] = append(byShift[a.ShiftID], a.TaskID)
		ids = append(ids, a.TaskID)
	}
	if err := RemoveByIDs(ext, ids, by, reason, now); err != nil {
		return nil, err
	}
	return byShift, nil
}
