package bindomain

import "testing"

func TestParseStatus(t *testing.T) {
	for _, ok := range []string{"active", "missing", "retired", "in_storage", "pending_move"} {
		if _, err := ParseStatus(ok); err != nil {
			t.Errorf("ParseStatus(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"banana", "", "Active", "deleted"} {
		if _, err := ParseStatus(bad); err == nil {
			t.Errorf("ParseStatus(%q) = nil error, want error", bad)
		}
	}
}
