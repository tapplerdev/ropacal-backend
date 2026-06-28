package handlers

import (
	"github.com/jmoiron/sqlx"
)

// ReleasedMoveRequest carries a move-request's pre-release assignment details, so
// callers can log unassignment history after the move has been returned to the
// pending pool.
type ReleasedMoveRequest struct {
	ID               string  `db:"id"`
	AssignmentType   *string `db:"assignment_type"`
	AssignedUserID   *string `db:"assigned_user_id"`
	AssignedUserName *string `db:"assigned_user_name"`
	AssignedShiftID  *string `db:"assigned_shift_id"`
}

// releaseShiftMoveRequests returns every in_progress move-request assigned to any of
// the given shifts back to the pending pool (status='pending', assigned_shift_id
// cleared) and returns their pre-release details so the caller can log history.
//
// This is the uniform, bug-prone core that EndShift / CancelShift /
// CancelAllActiveShifts each had copy-pasted. Each caller keeps its own divergent
// post-steps (history notes wording, and EndShift's route_task soft-delete) using
// the returned slice — so behavior is preserved while the release itself lives in
// one place and can't drift.
//
// ext is an sqlx.Ext, satisfied by both *sqlx.DB and *sqlx.Tx, so callers using a
// transaction (Cancel*) and callers on the bare pool (End) both work.
func releaseShiftMoveRequests(ext sqlx.Ext, shiftIDs []string, now int64) ([]ReleasedMoveRequest, error) {
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
		WHERE mr.assigned_shift_id IN (?) AND mr.status = 'in_progress'`, shiftIDs)
	if err != nil {
		return nil, err
	}
	selQuery = ext.Rebind(selQuery)

	var affected []ReleasedMoveRequest
	if err := sqlx.Select(ext, &affected, selQuery, selArgs...); err != nil {
		return nil, err
	}

	// Return them to the pending pool. Matches the prior behavior of all three
	// callers: clears assigned_shift_id and flips to pending (assigned_user_id is
	// intentionally left untouched, as before).
	updQuery, updArgs, err := sqlx.In(`
		UPDATE bin_move_requests
		SET status = 'pending', assigned_shift_id = NULL, updated_at = ?
		WHERE assigned_shift_id IN (?) AND status = 'in_progress'`, now, shiftIDs)
	if err != nil {
		return nil, err
	}
	updQuery = ext.Rebind(updQuery)
	if _, err := ext.Exec(updQuery, updArgs...); err != nil {
		return nil, err
	}

	return affected, nil
}
