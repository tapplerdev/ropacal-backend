package moverequest

import (
	"strings"

	"github.com/jmoiron/sqlx"
)

// FieldEdits is the set of plain (non-assignment) fields a move-request edit may
// change. A nil field is left unchanged. MoveType must already be validated by
// the caller (ParseMoveType at the boundary); NewAddress is composed at the
// boundary (street/city/zip → one string). Assignment changes are NOT here —
// those go through AssignToShift / AssignToDriver / ClearAssignment.
type FieldEdits struct {
	ScheduledDate *int64
	MoveType      *string
	Reason        *string
	Notes         *string
	NewAddress    *string
	NewLatitude   *float64
	NewLongitude  *float64
}

// HasChanges reports whether any field is set (i.e. EditFields would do work).
func (f FieldEdits) HasChanges() bool {
	return f.ScheduledDate != nil || f.MoveType != nil || f.Reason != nil ||
		f.Notes != nil || f.NewAddress != nil || f.NewLatitude != nil || f.NewLongitude != nil
}

// EditFields applies the provided field changes to a move request in one UPDATE,
// recomputing urgency from a new scheduled_date inside the domain. It routes
// through guardedTransition, so editing a terminal (completed/cancelled) move is
// refused with ErrInvalidTransition rather than silently mutating a finalized
// move. No-op (returns nil, no query) when nothing is set. Runs in the caller's
// ext (pool or tx).
func EditFields(ext sqlx.Ext, id string, f FieldEdits, now int64) error {
	sets := []string{"updated_at = ?"}
	args := []interface{}{now}

	if f.ScheduledDate != nil {
		sets = append(sets, "scheduled_date = ?", "urgency = ?")
		args = append(args, *f.ScheduledDate, ScheduledUrgency(*f.ScheduledDate, now))
	}
	if f.MoveType != nil {
		sets = append(sets, "move_type = ?")
		args = append(args, *f.MoveType)
	}
	if f.Reason != nil {
		sets = append(sets, "reason = ?")
		args = append(args, *f.Reason)
	}
	if f.Notes != nil {
		sets = append(sets, "notes = ?")
		args = append(args, *f.Notes)
	}
	if f.NewAddress != nil {
		sets = append(sets, "new_address = ?")
		args = append(args, *f.NewAddress)
	}
	if f.NewLatitude != nil {
		sets = append(sets, "new_latitude = ?")
		args = append(args, *f.NewLatitude)
	}
	if f.NewLongitude != nil {
		sets = append(sets, "new_longitude = ?")
		args = append(args, *f.NewLongitude)
	}

	if len(sets) == 1 { // only updated_at — nothing actually changed
		return nil
	}
	return guardedTransition(ext, id, strings.Join(sets, ", "), args...)
}
