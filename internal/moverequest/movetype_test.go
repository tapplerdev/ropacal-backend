package moverequest

import "testing"

func TestParseMoveType(t *testing.T) {
	for _, s := range []string{"store", "pickup_only", "relocation", "redeployment"} {
		got, err := ParseMoveType(s)
		if err != nil {
			t.Errorf("ParseMoveType(%q) error = %v, want nil", s, err)
		}
		if string(got) != s {
			t.Errorf("ParseMoveType(%q) = %q, want %q", s, got, s)
		}
	}
	for _, s := range []string{"", "Store", "pickup", "move", "deliver", "relocation "} {
		if _, err := ParseMoveType(s); err == nil {
			t.Errorf("ParseMoveType(%q) = nil error, want error", s)
		}
	}
}
