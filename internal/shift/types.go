// Package shift holds the shift domain's typed values and invariants. For now it
// hosts just the shift_type validator — the seed of a fuller shift domain (shift
// lifecycle logic otherwise still lives in handlers/database). Kept out of models
// so the typed enum + its validation live in one domain place, per the
// domain-driven default (mirrors moverequest.ParseMoveType / itinerary.ParseTaskType).
package shift

import "fmt"

// Type is a shift's kind. Validate inbound strings with ParseType at the request
// boundary so a bad value is a clean 400, not a 500 at the shifts.shift_type CHECK.
type Type string

const (
	Standard Type = "standard"
	Custom   Type = "custom"
)

var validTypes = map[Type]bool{
	Standard: true,
	Custom:   true,
}

// ParseType validates a raw shift_type and returns the typed value, or an error
// suitable for a 400. The accepted set matches the shifts.shift_type CHECK constraint.
func ParseType(s string) (Type, error) {
	if !validTypes[Type(s)] {
		return "", fmt.Errorf("invalid shift_type %q (want standard or custom)", s)
	}
	return Type(s), nil
}
