package moverequest

import "fmt"

// MoveType is the kind of bin move. Validate inbound strings with ParseMoveType
// at the request boundary rather than passing a raw string toward the DB: the
// bin_move_requests.move_type CHECK constraint is a last resort, not the
// boundary — an unvalidated value becomes a 500 at write time instead of a clean
// 400. (Mirrors itinerary.ParseTaskType.)
type MoveType string

const (
	// MoveStore: pick the bin up and store it at the warehouse.
	MoveStore MoveType = "store"
	// MovePickupOnly: pick up for disposal/storage (disposal_action decides which).
	MovePickupOnly MoveType = "pickup_only"
	// MoveRelocation: move the bin from its current spot to a new location.
	MoveRelocation MoveType = "relocation"
	// MoveRedeployment: take an in-storage bin back out to a new location.
	MoveRedeployment MoveType = "redeployment"
)

var validMoveTypes = map[MoveType]bool{
	MoveStore:        true,
	MovePickupOnly:   true,
	MoveRelocation:   true,
	MoveRedeployment: true,
}

// ParseMoveType validates a raw move_type string and returns the typed value, or
// an error suitable for a 400. The accepted set is the single source of truth and
// matches the bin_move_requests.move_type CHECK constraint.
func ParseMoveType(s string) (MoveType, error) {
	if !validMoveTypes[MoveType(s)] {
		return "", fmt.Errorf("invalid move_type %q (want store, pickup_only, relocation, or redeployment)", s)
	}
	return MoveType(s), nil
}
