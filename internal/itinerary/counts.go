package itinerary

import "ropacal-backend/internal/models"

// StopCounts is a shift's progress, derived from its route_tasks (the single
// source of truth) instead of the hand-maintained shifts.total_bins /
// completed_bins columns, which drift because they're adjusted by ±1 in several
// handlers and aren't always kept in sync (e.g. a reassign decrements the old
// shift but never increments the new one).
type StopCounts struct {
	// TotalBins / CompletedBins exclude warehouse_stop — the existing "bins" metric
	// the driver progress bar reads. Semantics match the old columns exactly: a task
	// counts as done when is_completed == 1.
	TotalBins     int
	CompletedBins int
	// TotalStops / CompletedStops count every task incl. warehouse_stop — the basis
	// for a future "100% only when back at the warehouse" bar. Not wired into the
	// response yet (a product/app decision); computed here so the seam exists.
	TotalStops     int
	CompletedStops int
}

// CountStops derives a shift's progress from the route_tasks the caller already
// fetched (no extra query). The slice is expected to already exclude soft-deleted
// rows (GetShiftTasks filters is_deleted = false).
//
// "Done" is is_completed==1 AND not skipped: SkipTask sets is_completed=1 to take
// the task off the driver's queue, but the stored completed_bins is deliberately
// NOT incremented for a skip — a skip is not a completion. Excluding it keeps this
// derivation faithful to the column it replaces. (Whether a skip should advance the
// progress bar is a separate product decision, not something to change silently here.)
func CountStops(tasks []models.RouteTask) StopCounts {
	var c StopCounts
	for _, t := range tasks {
		done := t.IsCompleted == 1 && !t.Skipped
		c.TotalStops++
		if done {
			c.CompletedStops++
		}
		if t.TaskType != models.TaskTypeWarehouseStop {
			c.TotalBins++
			if done {
				c.CompletedBins++
			}
		}
	}
	return c
}
