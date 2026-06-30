package moverequest

import "fmt"

// DisposalAction is what happens to a bin on a pickup_only move: retire it or store
// it. Validate inbound strings with ParseDisposalAction at the request boundary so a
// bad value is a clean 400, not a 500 at the bin_move_requests.disposal_action CHECK.
// (Mirrors ParseMoveType.)
type DisposalAction string

const (
	DisposalRetire DisposalAction = "retire"
	DisposalStore  DisposalAction = "store"
)

var validDisposalActions = map[DisposalAction]bool{
	DisposalRetire: true,
	DisposalStore:  true,
}

// ParseDisposalAction validates a raw disposal_action and returns the typed value,
// or an error suitable for a 400. The accepted set matches the
// bin_move_requests.disposal_action CHECK constraint.
func ParseDisposalAction(s string) (DisposalAction, error) {
	if !validDisposalActions[DisposalAction(s)] {
		return "", fmt.Errorf("invalid disposal_action %q (want retire or store)", s)
	}
	return DisposalAction(s), nil
}
