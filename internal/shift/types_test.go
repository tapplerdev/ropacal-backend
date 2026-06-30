package shift

import "testing"

func TestParseType(t *testing.T) {
	for _, ok := range []string{"standard", "custom"} {
		if _, err := ParseType(ok); err != nil {
			t.Errorf("ParseType(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"banana", "", "Standard"} {
		if _, err := ParseType(bad); err == nil {
			t.Errorf("ParseType(%q) = nil error, want error", bad)
		}
	}
}
