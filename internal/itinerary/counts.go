package itinerary

import (
	"fmt"

	"ropacal-backend/internal/models"

	"github.com/jmoiron/sqlx"
)

// StopCounts is a shift's progress, derived from its route_tasks (the single source
// of truth) instead of the hand-maintained shifts.total_bins / completed_bins
// columns, which drift because they're adjusted by ±1 across handlers and aren't
// always kept in sync (e.g. a reassign decrements the old shift but never increments
// the new one).
//
// Two distinct questions, two counts:
//   - Bins: how many BINS is the driver handling. A relocation is ONE bin (the same
//     physical bin), even though it has a pickup and a dropoff; it counts once and is
//     "done" only when all its tasks are done. Collections/placements count once each.
//     warehouse_stop and service are not bins.
//   - Stops: how many PLACES the driver drives to. Every task is one stop (a relocation
//     is two: pickup + dropoff), warehouse stops included. This is the navigation /
//     "how far through my route" count.
type StopCounts struct {
	TotalBins      int
	CompletedBins  int
	TotalStops     int
	CompletedStops int
}

// CountStops derives a shift's progress from the route_tasks the caller already
// fetched (no extra query). The slice is expected to already exclude soft-deleted
// rows (GetShiftTasks filters is_deleted = false).
//
// "Done" is is_completed==1 AND not skipped: SkipTask sets is_completed=1 to take a
// task off the driver's queue, but a skip is not a completion (a skipped bin was not
// collected), so it stays in the total and out of completed. Skips are tracked
// separately via the per-task skipped flag.
func CountStops(tasks []models.RouteTask) StopCounts {
	var c StopCounts
	done := func(t models.RouteTask) bool { return t.IsCompleted == 1 && !t.Skipped }

	// Stops: every task is a place the driver visits.
	for _, t := range tasks {
		c.TotalStops++
		if done(t) {
			c.CompletedStops++
		}
	}

	// Bins (logical): a relocation's pickup+dropoff share a move_request_id and count
	// as one bin, complete only when ALL its tasks are done. Collections/placements
	// count once each. warehouse_stop and service are not bins.
	seenMove := map[string]bool{}
	for _, t := range tasks {
		if t.MoveRequestID != nil && *t.MoveRequestID != "" {
			id := *t.MoveRequestID
			if seenMove[id] {
				continue
			}
			seenMove[id] = true
			c.TotalBins++
			allDone := true
			for _, b := range tasks {
				if b.MoveRequestID != nil && *b.MoveRequestID == id && !done(b) {
					allDone = false
					break
				}
			}
			if allDone {
				c.CompletedBins++
			}
		} else if t.TaskType != models.TaskTypeWarehouseStop && t.TaskType != models.TaskTypeService {
			c.TotalBins++
			if done(t) {
				c.CompletedBins++
			}
		}
	}
	return c
}

// RecomputeShiftCounts rewrites a shift's stored total_bins/completed_bins from its
// route_tasks, using CountStops as the single source of truth (so the stored columns
// can never disagree with the read path). Call it inside the caller's transaction
// after ANY change to the shift's route_tasks (add/remove/complete/skip/reassign/
// optimize), replacing the hand-rolled ±1 adjustments that drift. The ~100 consumers
// that read the stored columns then always match the tasks.
func RecomputeShiftCounts(ext sqlx.Ext, shiftID string, now int64) error {
	var tasks []models.RouteTask
	if err := sqlx.Select(ext, &tasks, ext.Rebind(`
		SELECT id, task_type, is_completed, skipped, move_request_id
		FROM route_tasks
		WHERE shift_id = ? AND is_deleted = false`), shiftID); err != nil {
		return fmt.Errorf("recompute shift counts: fetch tasks: %w", err)
	}
	c := CountStops(tasks)
	if _, err := ext.Exec(ext.Rebind(`
		UPDATE shifts SET total_bins = ?, completed_bins = ?, updated_at = ? WHERE id = ?`),
		c.TotalBins, c.CompletedBins, now, shiftID); err != nil {
		return fmt.Errorf("recompute shift counts: update shift: %w", err)
	}
	return nil
}
