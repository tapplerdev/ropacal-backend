package itinerary

import "testing"

func TestParseTimeWindowType(t *testing.T) {
	for _, ok := range []string{"strict", "soft", "soft_start", "soft_end"} {
		if _, err := ParseTimeWindowType(ok); err != nil {
			t.Errorf("ParseTimeWindowType(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"banana", "", "Strict", "hard"} {
		if _, err := ParseTimeWindowType(bad); err == nil {
			t.Errorf("ParseTimeWindowType(%q) = nil error, want error", bad)
		}
	}
}
