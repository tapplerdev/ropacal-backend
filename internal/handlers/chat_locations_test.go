package handlers

import "testing"

// normalizePlace decides which checks belong to a bin's CURRENT location, so a
// false split invents a move that never happened (halving the sample) and a false
// merge averages two different pitches into one rate. These cases are taken from
// real bin_address_snapshot pairs in production and are kept in lockstep with
// ropacal-placement's relocation.normalize_place — if one changes, change both.
func TestNormalizePlace(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
		why  string
	}{
		{"900 North 1st Street, San Jose, 95112", "911 N First St, San Jose", true,
			"bin 74: same pitch, HERE re-geocoded the number and spelled out the direction"},
		{"710 Leigh Avenue, San Jose", "755 Leigh Ave, San Jose", true,
			"bin 46: same plaza, drifting house number"},
		{"2116 W El Camino Real", "West El Camino Real", true,
			"bin 76: one snapshot simply lacks a house number"},
		{"2205 Middlefield Road, Redwood City", "24790 Amador St, Hayward, 94544", false,
			"bin 71: a genuine relocation across cities"},
		{"3478 Depot Rd, Hayward", "1776 N Milpitas Blvd, Milpitas, 95035", false,
			"bin 40: warehouse spell vs its field pitch — must not merge"},
		{"2560 W El Camino Real", "8311 Central Ave, Newark", false,
			"bin 31: Mountain View vs Newark, ~15 miles"},
		{"1410 S Winchester Blvd", "790 Montague Expy", false,
			"bin 91: two different San Jose pitches"},
	}
	for _, c := range cases {
		got := normalizePlace(c.a) == normalizePlace(c.b)
		if got != c.same {
			t.Errorf("normalizePlace(%q)=%q vs normalizePlace(%q)=%q: same=%v, want %v — %s",
				c.a, normalizePlace(c.a), c.b, normalizePlace(c.b), got, c.same, c.why)
		}
	}
}

func TestNormalizePlaceEdgeCases(t *testing.T) {
	if got := normalizePlace(""); got != "" {
		t.Errorf("empty snapshot should normalise to empty, got %q", got)
	}
	// A bare number must not be stripped into nothing — the house-number rule only
	// applies when something follows it.
	if got := normalizePlace("1234"); got != "1234" {
		t.Errorf("bare number should survive, got %q", got)
	}
	if got := normalizePlace("790 Montague Expressway, San Jose"); got != "montague expy" {
		t.Errorf("want %q, got %q", "montague expy", got)
	}
}
