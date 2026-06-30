package itinerary

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"ropacal-backend/internal/models"
)

// ApplyOrder stamps the optimizer's decided order onto a shift's route_tasks,
// inside the caller's transaction. It is the order-DECIDING sequence_order writer;
// Resequence is the order-PRESERVING one. (Two writers only — see resequence.go.)
//
// Inputs:
//   - orderedIDs: existing route_tasks IDs (collections / placements / move legs) in
//     the optimizer's stop order. The caller resolves optimizer-stop → task-ID, because
//     that matching depends on optimizer ID-format contracts (the "collection-"/"move-"
//     prefix strip) that are not this domain's concern. ApplyOrder only stamps order on
//     already-resolved IDs, and never errors on an ID the caller chose to include.
//   - newTasks: regenerated warehouse_stop rows to INSERT (the optimizer's return-to-
//     warehouse and real warehouse-pickup stops), in optimizer order. The caller drops
//     start stops and the fake "driver-current-warehouse" pickup before passing these.
//   - isFirst: selects the first-optimize persist strategy. Only the reopt strategy
//     (isFirst=false) is implemented today; isFirst=true is wired in a later slice.
//
// It assigns a dense monotonic sequence_order across the existing tasks then the new
// warehouse stops, then calls Resequence to normalize to the canonical 1..N
// (completed-first, warehouse-last). Because Resequence re-sorts warehouse-last, doing
// bins-then-warehouse here is equivalent to the interleaved optimizer walk it replaces.
//
// MUST NOT be called for a lock_route_order shift — the caller guards that boundary
// (the locked path takes the Resequence-only route, never ApplyOrder).
func ApplyOrder(ext sqlx.Ext, shiftID string, orderedIDs []string, newTasks []models.RouteTask, isFirst bool) error {
	if isFirst {
		// The first-optimize strategy (soft-delete-all branch, binsPreloaded auto-complete,
		// optimization_metadata) is asymmetric with reopt and lands in a later slice.
		return fmt.Errorf("ApplyOrder: isFirst=true not yet implemented")
	}

	now := time.Now().Unix()
	seq := 1

	// 1. Stamp existing tasks in optimizer order (in-place UPDATE preserves their IDs).
	updQ := ext.Rebind(`UPDATE route_tasks SET sequence_order = ?, updated_at = ? WHERE id = ?`)
	for _, id := range orderedIDs {
		if _, err := ext.Exec(updQ, seq, now, id); err != nil {
			return fmt.Errorf("ApplyOrder: update sequence for task %s: %w", id, err)
		}
		seq++
	}

	// 2. Insert regenerated warehouse stops after the bins (Resequence sorts them last anyway;
	//    the increasing seq preserves their relative optimizer order through the renumber).
	insQ := ext.Rebind(`
		INSERT INTO route_tasks (
			id, shift_id, task_type, sequence_order, latitude, longitude, address,
			is_completed, is_deleted, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	for _, t := range newTasks {
		if _, err := ext.Exec(insQ, t.ID, shiftID, t.TaskType, seq, t.Latitude, t.Longitude, t.Address, 0, false, now, now); err != nil {
			return fmt.Errorf("ApplyOrder: insert warehouse stop %s: %w", t.ID, err)
		}
		seq++
	}

	// 3. Normalize to the canonical dense 1..N order.
	if err := Resequence(ext, shiftID); err != nil {
		return fmt.Errorf("ApplyOrder: resequence: %w", err)
	}
	return nil
}
