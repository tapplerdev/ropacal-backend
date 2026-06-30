package moverequest

import "testing"

func TestParseDisposalAction(t *testing.T) {
	for _, ok := range []string{"retire", "store"} {
		if _, err := ParseDisposalAction(ok); err != nil {
			t.Errorf("ParseDisposalAction(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"banana", "", "Retire", "dispose"} {
		if _, err := ParseDisposalAction(bad); err == nil {
			t.Errorf("ParseDisposalAction(%q) = nil error, want error", bad)
		}
	}
}
