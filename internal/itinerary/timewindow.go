package itinerary

import "fmt"

// TimeWindowType constrains route_tasks.time_window_type. Validate inbound strings
// with ParseTimeWindowType at the request boundary so a bad value is a clean 400,
// not a 500 at the route_tasks.time_window_type CHECK. (Mirrors ParseTaskType.)
type TimeWindowType string

const (
	TimeWindowStrict    TimeWindowType = "strict"
	TimeWindowSoft      TimeWindowType = "soft"
	TimeWindowSoftStart TimeWindowType = "soft_start"
	TimeWindowSoftEnd   TimeWindowType = "soft_end"
)

var validTimeWindowTypes = map[TimeWindowType]bool{
	TimeWindowStrict:    true,
	TimeWindowSoft:      true,
	TimeWindowSoftStart: true,
	TimeWindowSoftEnd:   true,
}

// ParseTimeWindowType validates a raw time_window_type and returns the typed value,
// or an error suitable for a 400. The accepted set matches the
// route_tasks.time_window_type CHECK constraint.
func ParseTimeWindowType(s string) (TimeWindowType, error) {
	if !validTimeWindowTypes[TimeWindowType(s)] {
		return "", fmt.Errorf("invalid time_window_type %q (want strict, soft, soft_start, or soft_end)", s)
	}
	return TimeWindowType(s), nil
}
