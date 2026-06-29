package moverequest

// AssignmentKind is the single coherent assignment outcome an edit resolves to.
// The handler's old fragment-append logic could produce incoherent combinations
// (e.g. a shift AND a user both set); classifying into exactly one kind and
// routing it through the matching guarded transition makes those impossible.
type AssignmentKind int

const (
	// AssignNoChange: the edit doesn't change assignment (fields-only, or the
	// requested assignment equals the current one).
	AssignNoChange AssignmentKind = iota
	// AssignToShiftKind: attach to a shift (AssignToShift — nulls the user).
	AssignToShiftKind
	// AssignToDriverKind: put on a driver's backlog (AssignToDriver — nulls the shift).
	AssignToDriverKind
	// AssignUnassignKind: return to the unassigned pool (ClearAssignment).
	AssignUnassignKind
)

// AssignmentChange is the resolved assignment intent of an edit.
type AssignmentChange struct {
	Kind    AssignmentKind
	ShiftID string // set when Kind == AssignToShiftKind
	UserID  string // set when Kind == AssignToDriverKind
}

// StatusForAssignment is the status a move takes when attached to a shift: an
// ACTIVE shift means the work is in progress; a ready/future shift means merely
// assigned. This is the derivation the handler used to do via a post-write
// re-read; AssignToShift takes this as its caller-decided status.
func StatusForAssignment(shiftActive bool) Status {
	if shiftActive {
		return StatusInProgress
	}
	return StatusAssigned
}

// nonEmpty reports whether a request pointer field carries a real value (provided
// and not the empty string). A nil pointer means "field not provided" (no change);
// a pointer to "" means an explicit clear.
func nonEmpty(p *string) bool { return p != nil && *p != "" }

// explicitClear reports whether a request pointer field was provided as "" (an
// explicit "remove this") rather than omitted (nil).
func explicitClear(p *string) bool { return p != nil && *p == "" }

// PlanAssignment resolves an edit's assignment fields — interpreted against the
// move's CURRENT assignment — into exactly one coherent AssignmentChange.
//
// Precedence: a requested shift wins (→ shift, user nulled), else a requested
// user (→ driver backlog, shift nulled), else an explicit clear of any assignment
// field or assignment_type (→ unassign to pool), else no change. Each branch is
// also gated on actually differing from the current state, so re-sending the same
// assignment is a no-op (no spurious "reassigned" history).
//
// curShiftID/curUserID are the move's current assignment; req* are the request's
// assigned_shift_id / assigned_user_id / assignment_type pointers (nil = omitted).
func PlanAssignment(curShiftID, curUserID *string, reqShiftID, reqUserID, reqAssignmentType *string) AssignmentChange {
	switch {
	case nonEmpty(reqShiftID):
		if curShiftID != nil && *curShiftID == *reqShiftID {
			return AssignmentChange{Kind: AssignNoChange}
		}
		return AssignmentChange{Kind: AssignToShiftKind, ShiftID: *reqShiftID}

	case nonEmpty(reqUserID):
		// Already on this driver's backlog with no shift → no change.
		if curUserID != nil && *curUserID == *reqUserID && curShiftID == nil {
			return AssignmentChange{Kind: AssignNoChange}
		}
		return AssignmentChange{Kind: AssignToDriverKind, UserID: *reqUserID}

	case explicitClear(reqAssignmentType) || explicitClear(reqShiftID) || explicitClear(reqUserID):
		// Explicit unassign. No-op if already fully unassigned.
		if curShiftID == nil && curUserID == nil {
			return AssignmentChange{Kind: AssignNoChange}
		}
		return AssignmentChange{Kind: AssignUnassignKind}

	default:
		return AssignmentChange{Kind: AssignNoChange}
	}
}
