package moverequest

import (
	"github.com/jmoiron/sqlx"
)

// ReleasedMoveRequest carries a move-request's pre-release assignment details, so
// callers can log the unassignment-from-shift history after release.
type ReleasedMoveRequest struct {
	ID               string  `db:"id"`
	AssignmentType   *string `db:"assignment_type"`
	AssignedUserID   *string `db:"assigned_user_id"`
	AssignedUserName *string `db:"assigned_user_name"`
	AssignedShiftID  *string `db:"assigned_shift_id"`
}

// ReleaseFromShift detaches every not-yet-completed move-request from the given
// shifts and returns each to its shift driver's personal BACKLOG
// (status='assigned', assignment_type='manual', assigned_user_id = the shift's
// driver, assigned_shift_id cleared). The driver is derived from the shift itself
// at release time, so the assign flow never needs to pre-record an owner.
//
// This is the single source of truth for the release rule, called by the shift
// lifecycle handlers (EndShift / CancelShift / CancelAllActiveShifts). Callers
// keep their own post-steps (history-note wording, EndShift's route_task
// soft-delete) using the returned slice.
//
// Both 'assigned' (on a ready/not-yet-active shift) and 'in_progress' (active
// shift) moves are released — a move tied to a shift that goes away must not be
// orphaned regardless of which state it was in.
//
// To send a move all the way back to the unassigned pool (drop the driver too),
// use the explicit clear-assignment operation — that is the deliberate escape
// hatch; automatic release always preserves driver ownership.
//
// ext is an sqlx.Ext, satisfied by both *sqlx.DB and *sqlx.Tx, so callers using a
// transaction (Cancel*) and callers on the bare pool (End) both work.
func ReleaseFromShift(ext sqlx.Ext, shiftIDs []string, now int64) ([]ReleasedMoveRequest, error) {
	if len(shiftIDs) == 0 {
		return nil, nil
	}

	// Capture the affected move-requests (with assignee name) BEFORE clearing the
	// assignment, so history logging has the prior values.
	selQuery, selArgs, err := sqlx.In(`
		SELECT mr.id, mr.assignment_type, mr.assigned_user_id, mr.assigned_shift_id,
		       u.name AS assigned_user_name
		FROM bin_move_requests mr
		LEFT JOIN users u ON mr.assigned_user_id = u.id
		WHERE mr.assigned_shift_id IN (?) AND mr.status IN ('assigned', 'in_progress')`, shiftIDs)
	if err != nil {
		return nil, err
	}
	selQuery = ext.Rebind(selQuery)

	var affected []ReleasedMoveRequest
	if err := sqlx.Select(ext, &affected, selQuery, selArgs...); err != nil {
		return nil, err
	}

	// Return each move to its shift driver's backlog. UPDATE ... FROM shifts pulls
	// the correct driver per move, so this is one statement for single- and
	// multi-shift (CancelAll) callers alike.
	updQuery, updArgs, err := sqlx.In(`
		UPDATE bin_move_requests AS mr
		SET assigned_shift_id = NULL,
		    assigned_user_id   = s.driver_id,
		    assignment_type    = 'manual',
		    status             = 'assigned',
		    updated_at         = ?
		FROM shifts s
		WHERE mr.assigned_shift_id = s.id
		  AND mr.assigned_shift_id IN (?)
		  AND mr.status IN ('assigned', 'in_progress')`, now, shiftIDs)
	if err != nil {
		return nil, err
	}
	updQuery = ext.Rebind(updQuery)
	if _, err := ext.Exec(updQuery, updArgs...); err != nil {
		return nil, err
	}

	return affected, nil
}
