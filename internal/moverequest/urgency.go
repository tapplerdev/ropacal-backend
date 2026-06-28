package moverequest

// Urgency is the live, computed urgency tier of a move-request, derived from its
// status and scheduled date (Unix seconds). `now` is passed in (rather than read
// from the clock) so the calculation is pure and unit-testable.
//
// Completed/cancelled moves are "resolved"; otherwise the tier is time-based:
// overdue (past due) < urgent (<24h) < soon (<72h) < scheduled.
func Urgency(status string, scheduledDate, now int64) string {
	if status == string(StatusCompleted) || status == string(StatusCancelled) {
		return "resolved"
	}

	hoursUntil := float64(scheduledDate-now) / 3600.0
	switch {
	case hoursUntil < 0:
		return "overdue"
	case hoursUntil < 24:
		return "urgent"
	case hoursUntil < 72:
		return "soon"
	default:
		return "scheduled"
	}
}

// ScheduledUrgency is the simplified two-state urgency persisted to the
// bin_move_requests.urgency column at create/update time: "urgent" when the move
// is due within 24 hours, otherwise "scheduled". It deliberately uses a narrower
// set of buckets than Urgency (no "overdue"/"soon"/"resolved") because that is
// all the write path records; the richer tiers are computed for responses.
func ScheduledUrgency(scheduledDate, now int64) string {
	if float64(scheduledDate-now)/3600.0 < 24 {
		return "urgent"
	}
	return "scheduled"
}
