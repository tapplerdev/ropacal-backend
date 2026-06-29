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

// guardedTransition runs a status-changing UPDATE that fires only when the move
// is NOT already in a terminal state (completed/cancelled), and returns
// ErrInvalidTransition when it isn't applied (already terminal, or absent). This
// is the single enforcement point that makes illegal transitions structurally
// impossible in the domain rather than relying on callers to pre-check. The
// `set` clause uses ? placeholders; Rebind adapts them to the driver. Runs in the
// caller's ext (pool or tx). Terminal-state membership is derived from the typed
// Status constants so the taxonomy has one source of truth.
func guardedTransition(ext sqlx.Ext, id, set string, args ...interface{}) error {
	q := "UPDATE bin_move_requests SET " + set + " WHERE id = ? AND status NOT IN (?, ?)"
	args = append(args, id, string(StatusCompleted), string(StatusCancelled))
	res, err := ext.Exec(ext.Rebind(q), args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidTransition
	}
	return nil
}

// ClearAssignment fully unassigns a move back to the pending POOL: it clears the
// driver, shift, and assignment type and sets status='pending'. This is the
// explicit, human-initiated "drop to pool" escape hatch — the counterpart to
// ReleaseFromShift (the automatic shift-detach that *preserves* driver ownership
// via the backlog). Keeping both transitions side by side is the whole point of
// the domain owning the state machine.
//
// It runs inside the caller's transaction (ext); the caller remains responsible
// for cross-domain cleanup (the shift's route_tasks / total_bins) and history.
func ClearAssignment(ext sqlx.Ext, id string, now int64) error {
	// assignment_type must be NULL (not ''), both to match the model's documented
	// "NULL for unassigned" semantics and because the column has a
	// CHECK(assignment_type IN ('shift','manual')) — '' violates it and 500s the
	// request (a long-standing bug: clear-assignment never actually worked). NULL
	// satisfies the CHECK.
	return guardedTransition(ext, id,
		"assignment_type = NULL, assigned_shift_id = NULL, assigned_user_id = NULL, status = 'pending', updated_at = ?",
		now)
}

// AssignToDriver puts a move on a specific driver's BACKLOG: assignment_type
// 'manual', the given driver, no shift, status 'assigned'. This is the manual
// counterpart to a shift assignment, and the same state a move returns to when
// ReleaseFromShift fires. Runs inside the caller's transaction (ext); detaching
// from any prior shift's route_tasks is the caller's cross-domain concern.
func AssignToDriver(ext sqlx.Ext, id, userID string, now int64) error {
	return guardedTransition(ext, id,
		"assignment_type = 'manual', assigned_user_id = ?, assigned_shift_id = NULL, status = 'assigned', updated_at = ?",
		userID, now)
}

// AssignToShift attaches a move to a shift: assignment_type 'shift', the given
// shift, no manual user, and the caller-decided status ('in_progress' if the
// shift is already active, else 'assigned'). Runs inside the caller's transaction
// (ext); inserting the shift's route_tasks and re-optimizing are the caller's
// cross-domain concern.
func AssignToShift(ext sqlx.Ext, id, shiftID, status string, now int64) error {
	return guardedTransition(ext, id,
		"assignment_type = 'shift', assigned_shift_id = ?, assigned_user_id = NULL, status = ?, updated_at = ?",
		shiftID, status, now)
}

// Cancel marks a move as cancelled (a terminal state). Runs inside the caller's
// transaction or on the pool (ext); the caller handles the cross-domain effects
// of cancelling (freeing the bin, detaching route_tasks, re-optimizing, notifying).
func Cancel(ext sqlx.Ext, id string, now int64) error {
	return guardedTransition(ext, id, "status = 'cancelled', updated_at = ?", now)
}

// Complete marks a move as completed (terminal), stamping completed_at. Runs in
// the caller's transaction or on the pool (ext); the caller applies the resulting
// bin state (retire / store / relocate) and history.
func Complete(ext sqlx.Ext, id string, now int64) error {
	return guardedTransition(ext, id, "status = 'completed', completed_at = ?, updated_at = ?", now, now)
}
