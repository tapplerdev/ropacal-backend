package handlers

import (
	"net/http/httptest"
	"testing"
)

// The bug this endpoint exists to kill: the browser anchored address search at
// Kansas City, Missouri whenever no location was passed — which nearly every
// caller did. A Toronto tenant typing an address ranked suggestions around
// Missouri, 1,700 km away.
func TestAddressAnchor_NeverKansasCity(t *testing.T) {
	const kansasCity = "39.099700,-94.578600"

	for _, country := range []string{"CAN", "USA", "GBR", "AUS", "MEX", ""} {
		if got := addressAnchor(geocodeScope{Country: country}); got == kansasCity {
			t.Errorf("country %q still anchors at Kansas City", country)
		}
	}
	if got := addressAnchor(geocodeScope{Country: "CAN"}); got != "56.130400,-106.346800" {
		t.Errorf("warehouse-less Canadian org anchored at %q, want the Canadian centroid", got)
	}
	// A real warehouse always wins over the centroid.
	if got := addressAnchor(geocodeScope{Country: "CAN", Lat: 43.6426, Lng: -79.3771}); got != "43.642600,-79.377100" {
		t.Errorf("anchor = %q, want the warehouse", got)
	}
}

// HERE splits the street number from the street name; the dashboard expects
// them joined, exactly as the browser used to join them.
func TestToAddressResult_ComposesStreetAndFallsBack(t *testing.T) {
	full := hereAddress{
		Label:       "100 Queen St W, Toronto, ON M5H 2N2, Canada",
		HouseNumber: "100", Street: "Queen St W", City: "Toronto",
		PostalCode: "M5H 2N2", StateCode: "ON", CountryCode: "CAN",
	}
	got := full.toAddressResult(43.6534, -79.3839)
	if got.Street != "100 Queen St W" {
		t.Errorf("street = %q, want the number joined to the name", got.Street)
	}
	if got.City != "Toronto" || got.Zip != "M5H 2N2" || got.State != "ON" || got.Country != "CAN" {
		t.Errorf("mis-parsed: %+v", got)
	}
	if got.Latitude != 43.6534 || got.Longitude != -79.3839 {
		t.Errorf("coordinates dropped: %+v", got)
	}

	// A street with no number must not gain a leading space.
	noNum := hereAddress{Street: "Queen St W", Label: "Queen St W"}.toAddressResult(0, 0)
	if noNum.Street != "Queen St W" {
		t.Errorf("street = %q, want no leading space", noNum.Street)
	}

	// HERE sometimes sends only the long forms.
	longForm := hereAddress{State: "Ontario", CountryName: "Canada", Label: "x"}.toAddressResult(0, 0)
	if longForm.State != "Ontario" || longForm.Country != "Canada" {
		t.Errorf("long-form fallback failed: %+v", longForm)
	}
}

func TestOptionalLatLng(t *testing.T) {
	cases := []struct {
		query   string
		wantOK  bool
		wantLat float64
	}{
		{"lat=43.65&lng=-79.38", true, 43.65},
		{"", false, 0},
		{"lat=43.65", false, 0},  // half a pair is not a location
		{"lng=-79.38", false, 0}, // ditto
		{"lat=abc&lng=-79.38", false, 0},
		{"lat=91&lng=0", false, 0},  // out of range
		{"lat=0&lng=181", false, 0}, // out of range
		{"lat=0&lng=0", true, 0},    // legal, if unlikely — the caller asked for it
		{"lat=-33.87&lng=151.21", true, -33.87},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/geocode/reverse?"+c.query, nil)
		lat, _, ok := optionalLatLng(r)
		if ok != c.wantOK {
			t.Errorf("%q: ok = %v, want %v", c.query, ok, c.wantOK)
			continue
		}
		if ok && lat != c.wantLat {
			t.Errorf("%q: lat = %v, want %v", c.query, lat, c.wantLat)
		}
	}
}
