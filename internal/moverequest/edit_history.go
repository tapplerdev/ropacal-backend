package moverequest

// AssignmentState is a move's assignment at a point in time, used to decide which
// history event an edit produced. UserName/DriverName are the resolved display
// names when already known (the after-state carries them from its JOIN); a
// previous assignee's name is resolved here so callers don't duplicate the lookup.
type AssignmentState struct {
	ShiftID        *string
	UserID         *string
	AssignmentType *string
	UserName       *string
	DriverName     *string
}

func (s AssignmentState) assigned() bool { return s.ShiftID != nil || s.UserID != nil }

// displayName is the assignee's already-resolved name: a manually-assigned user
// wins, else the shift driver.
func (s AssignmentState) displayName() *string {
	if s.UserName != nil {
		return s.UserName
	}
	return s.DriverName
}

// LogAssignmentChange writes the single history event an assignment edit produced
// — assigned, unassigned, or reassigned — derived from the before/after state.
// It is the one place that resolves a previous assignee's display name (the
// handler used to copy-paste that lookup three times). History is post-commit and
// best-effort: the caller logs the returned error but does not fail the request.
func LogAssignmentChange(db DB, moveReqID, actorID, actorName, actorRole string, before, after AssignmentState) error {
	switch {
	case !before.assigned() && after.assigned():
		atype := ""
		if after.AssignmentType != nil {
			atype = *after.AssignmentType
		}
		return LogAssigned(db, moveReqID, actorID, actorName, actorRole, atype, after.UserID, after.displayName(), after.ShiftID)

	case before.assigned() && !after.assigned():
		return LogUnassigned(db, moveReqID, actorID, actorName, actorRole,
			before.AssignmentType, before.UserID, resolvePreviousName(db, before), before.ShiftID)

	case before.assigned() && after.assigned():
		return LogReassigned(db, moveReqID, actorID, actorName, actorRole,
			before.AssignmentType, after.AssignmentType,
			before.UserID, after.UserID,
			resolvePreviousName(db, before), after.displayName(),
			before.ShiftID, after.ShiftID)
	}
	return nil
}

// resolvePreviousName looks up the display name for a now-previous assignment: the
// manually-assigned user, else the shift's driver. Returns nil if neither resolves.
func resolvePreviousName(db DB, s AssignmentState) *string {
	if s.UserID != nil {
		var name string
		if err := db.Get(&name, `SELECT name FROM users WHERE id = $1`, *s.UserID); err == nil {
			return &name
		}
	} else if s.ShiftID != nil {
		var name string
		if err := db.Get(&name, `SELECT u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`, *s.ShiftID); err == nil {
			return &name
		}
	}
	return nil
}
