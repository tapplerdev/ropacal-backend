package handlers

import (
	"net/url"
	"strings"
	"testing"
)

// Warehouses used across these tests.
var (
	toronto = geocodeScope{Country: "CAN", Lat: 43.6426, Lng: -79.3771}
	hayward = geocodeScope{Country: "USA", Lat: 37.6368, Lng: -122.1269}
)

func labels(out []geocodeSearchResult) []string {
	s := make([]string, len(out))
	for i, r := range out {
		s[i] = r.Label
	}
	return s
}

// The production bug, frozen as a test. Measured against the deployed endpoint
// for the Toronto organization, "London" returned the Thames Centre district
// ABOVE the City of London.
//
// Note the coordinates: Thames Centre is east of London, so it is genuinely
// NEARER Toronto. A pure distance sort reproduces the bug. Only ranking type
// first fixes it, which is the whole reason typeRank exists.
func TestRank_CityBeatsNearerDistrictOfTheSameName(t *testing.T) {
	out := []geocodeSearchResult{
		{Label: "London, Thames Centre, ON, Canada", Type: "district", Lat: 43.0333, Lng: -81.0667},
		{Label: "London, ON, Canada", Type: "city", Lat: 42.9849, Lng: -81.2453},
	}
	rankGeocodeResults(out, toronto)

	if out[0].Label != "London, ON, Canada" {
		t.Errorf("top result is %q — the city must outrank a same-named district", out[0].Label)
	}
	// Guard the reasoning above: if this ever stops holding, the test is no
	// longer exercising the case it claims to.
	dCity := haversineKm(toronto.Lat, toronto.Lng, 42.9849, -81.2453)
	dDist := haversineKm(toronto.Lat, toronto.Lng, 43.0333, -81.0667)
	if dDist >= dCity {
		t.Fatalf("fixture no longer models the bug: district %.0f km, city %.0f km — "+
			"the district must be NEARER for this test to prove type beats distance", dDist, dCity)
	}
}

// A far-away city must still beat a nearby street. This is the Windsor case:
// Windsor ON is 350 km from Toronto, a Windsor street is 2 km away.
func TestRank_CityBeatsNearbyStreet(t *testing.T) {
	out := []geocodeSearchResult{
		{Label: "Windsor St, Toronto, ON, Canada", Type: "street", Lat: 43.6400, Lng: -79.3800},
		{Label: "Windsor, ON, Canada", Type: "city", Lat: 42.3149, Lng: -83.0364},
	}
	rankGeocodeResults(out, toronto)
	if out[0].Label != "Windsor, ON, Canada" {
		t.Errorf("top result is %q — a street should not outrank a city of the same name", out[0].Label)
	}
}

// Within one type rank, proximity decides. This is the determinism that the old
// code claimed from HERE's `at=` bias and did not actually get: switching
// endpoints silently flipped Springfield from CA to MO.
func TestRank_SameTypeFallsBackToDistance(t *testing.T) {
	out := []geocodeSearchResult{
		{Label: "Springfield, MO, United States", Type: "city", Lat: 37.2090, Lng: -93.2923},
		{Label: "Springfield, CA, United States", Type: "city", Lat: 38.0000, Lng: -120.4000},
	}
	rankGeocodeResults(out, hayward)
	if !strings.Contains(out[0].Label, "CA") {
		t.Errorf("top result is %q — the nearer Springfield must win for a Bay Area org", out[0].Label)
	}
}

// Both rules at once, in the order they apply.
func TestRank_TypeThenDistance(t *testing.T) {
	out := []geocodeSearchResult{
		{Label: "street-near", Type: "street", Lat: 43.6400, Lng: -79.3800},
		{Label: "city-far", Type: "city", Lat: 42.3149, Lng: -83.0364},
		{Label: "district-near", Type: "district", Lat: 43.6500, Lng: -79.3900},
		{Label: "city-near", Type: "city", Lat: 43.7000, Lng: -79.4000},
		{Label: "county-near", Type: "county", Lat: 43.6600, Lng: -79.3700},
	}
	rankGeocodeResults(out, toronto)

	want := []string{"city-near", "city-far", "district-near", "county-near", "street-near"}
	got := labels(out)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Provisioning seeds new organizations at 0,0. Sorting Ontario by proximity to
// the Gulf of Guinea would be worse than not sorting: with no warehouse, HERE's
// own order must survive inside each type rank.
func TestRank_NoWarehousePreservesVendorOrderWithinRank(t *testing.T) {
	out := []geocodeSearchResult{
		{Label: "second-per-here", Type: "city", Lat: 43.0, Lng: -79.0},
		{Label: "first-per-here", Type: "city", Lat: 49.0, Lng: -123.0},
		{Label: "a-street", Type: "street", Lat: 43.1, Lng: -79.1},
	}
	rankGeocodeResults(out, geocodeScope{Country: "CAN"})

	got := labels(out)
	want := []string{"second-per-here", "first-per-here", "a-street"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (cities keep HERE's order, street still demoted)", got, want)
		}
	}
}

// typeRank must be total: an unrecognised type sorts last rather than panicking
// or silently landing among the cities.
func TestTypeRank_IsTotalAndOrdered(t *testing.T) {
	if typeRank("city") >= typeRank("district") {
		t.Error("city must outrank district")
	}
	if typeRank("district") >= typeRank("county") {
		t.Error("district must outrank county")
	}
	if typeRank("county") >= typeRank("street") {
		t.Error("county must outrank street")
	}
	if typeRank("something-here-invented-later") <= typeRank("street") {
		t.Error("an unknown type must sort last, not ahead of a known one")
	}
}

func TestHaversine_KnownDistance(t *testing.T) {
	// Toronto -> Windsor ON is ~332 km great-circle (the ~370 km figure people
	// quote is the drive along the 401, which is not what this measures).
	d := haversineKm(43.6426, -79.3771, 42.3149, -83.0364)
	if d < 325 || d > 340 {
		t.Errorf("Toronto->Windsor = %.1f km, expected ~332", d)
	}
	if haversineKm(43.6426, -79.3771, 43.6426, -79.3771) != 0 {
		t.Error("distance to self must be zero")
	}
}

// /autosuggest REQUIRES an anchor. A request built without one is rejected by
// HERE outright, so an organization with no warehouse must still get a valid
// URL rather than a 400.
func TestHereAutosuggestURL_AlwaysAnchored(t *testing.T) {
	u := hereAutosuggestURL("Toronto", 20, geocodeScope{Country: "CAN"})
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if qs.Get("at") == "" {
		t.Error("no anchor — HERE rejects an autosuggest request without at= or in=circle")
	}
	if qs.Get("in") != "countryCode:CAN" {
		t.Errorf("country filter = %q", qs.Get("in"))
	}
	if qs.Get("q") != "Toronto" {
		t.Errorf("query text mangled: %q", qs.Get("q"))
	}
}

// The warehouse, when set, must be the anchor — not the country centroid.
func TestHereAutosuggestURL_PrefersWarehouseOverCentroid(t *testing.T) {
	u := hereAutosuggestURL("Windsor", 20, toronto)
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if got := qs.Get("at"); got != "43.642600,-79.377100" {
		t.Errorf("anchor = %q, want the Toronto warehouse", got)
	}
	if strings.Contains(u, "USA") {
		t.Errorf("URL mentions USA: %s", u)
	}
}
