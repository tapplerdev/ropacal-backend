// Package bin holds the bin domain's typed values and invariants. For now it hosts
// just the status validator — the seed of a fuller bin domain. Kept out of models so
// the typed enum + its validation live in one domain place, per the domain-driven
// default (mirrors moverequest.ParseMoveType / itinerary.ParseTaskType).
//
// NOTE: handlers that have a local variable named `bin` should import this package
// with an alias (e.g. `bindomain "ropacal-backend/internal/bin"`) to avoid shadowing.
package bindomain

import "fmt"

// Status is a bin's lifecycle state. Validate inbound strings with ParseStatus at the
// request boundary so a bad value is a clean 400, not a 500 at the bins.status CHECK.
type Status string

const (
	Active      Status = "active"
	Missing     Status = "missing"
	Retired     Status = "retired"
	InStorage   Status = "in_storage"
	PendingMove Status = "pending_move"
)

var validStatuses = map[Status]bool{
	Active:      true,
	Missing:     true,
	Retired:     true,
	InStorage:   true,
	PendingMove: true,
}

// ParseStatus validates a raw bin status and returns the typed value, or an error
// suitable for a 400. The accepted set matches the bins.status CHECK constraint.
func ParseStatus(s string) (Status, error) {
	if !validStatuses[Status(s)] {
		return "", fmt.Errorf("invalid status %q (want active, missing, retired, in_storage, or pending_move)", s)
	}
	return Status(s), nil
}
