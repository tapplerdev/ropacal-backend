package handlers

import "testing"

func TestIsNullIsland(t *testing.T) {
	cases := []struct {
		name     string
		lat, lng float64
		want     bool
	}{
		{"exact zero sentinel", 0, 0, true},
		{"sub-epsilon both", 1e-9, -1e-9, true},
		{"real fix (san jose)", 37.3382, -121.8863, false},
		{"zero lat, real lng", 0, -121.8863, false},
		{"real lat, zero lng", 37.3382, 0, false},
		{"just past epsilon", 1e-5, 0, false},
	}
	for _, c := range cases {
		if got := isNullIsland(c.lat, c.lng); got != c.want {
			t.Errorf("%s: isNullIsland(%v, %v) = %v, want %v", c.name, c.lat, c.lng, got, c.want)
		}
	}
}
