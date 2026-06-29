// Package moverequest owns the move-request lifecycle: its state machine, data
// access, and the operations that transition a move between states. See
// DESIGN.md for the full state machine and migration plan.
package moverequest

import "errors"

// ErrInvalidTransition is returned by a transition attempted from a terminal
// state (completed/cancelled). It makes the illegal transitions the state machine
// forbids structurally impossible at the domain level — the UPDATE simply does
// not fire — rather than relying on every caller to pre-check status.
var ErrInvalidTransition = errors.New("moverequest: illegal status transition (move already terminal)")

// Status is a move-request's lifecycle state.
type Status string

const (
	// StatusPending: in the unassigned pool, claimable by anyone.
	StatusPending Status = "pending"
	// StatusAssigned: claimed — either manually by a driver (no shift) or to a
	// not-yet-active shift. This is also the "driver backlog" state a move returns
	// to when its shift is released.
	StatusAssigned Status = "assigned"
	// StatusInProgress: on an active shift, being worked.
	StatusInProgress Status = "in_progress"
	// StatusCompleted: done.
	StatusCompleted Status = "completed"
	// StatusCancelled: no longer needed.
	StatusCancelled Status = "cancelled"
)

// IsTerminal reports whether the move has reached an end state.
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusCancelled
}

// IsOnShift reports whether the status represents work tied to a shift.
func (s Status) IsOnShift() bool {
	return s == StatusAssigned || s == StatusInProgress
}
