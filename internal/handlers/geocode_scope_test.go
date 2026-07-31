package handlers

import (
	"net/url"
	"strings"
	"testing"
)

func TestHereCountryCode(t *testing.T) {
	cases := map[string]string{
		"US": "USA", "us": "USA", " CA ": "CAN", "ca": "CAN",
		"GB": "GBR", "AU": "AUS",
		// Unknown must return "" so the caller OMITS the filter. Sending a
		// malformed country would return nothing at all; a wider search at
		// least returns something.
		"ZZ": "", "": "", "CAN": "",
	}
	for in, want := range cases {
		if got := hereCountryCode(in); got != want {
			t.Errorf("hereCountryCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// The bug, reproduced as a test: the query text must NOT carry a country
// suffix. Appending ",USA" polluted the search string itself, so even lifting
// the in=countryCode filter would not have let "Brampton" find Ontario.
func TestHereGeocodeURL_NoCountrySuffixInQuery(t *testing.T) {
	u := hereGeocodeURL("Brampton", 6, geocodeScope{Country: "CAN", Lat: 43.6426, Lng: -79.3771})
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	qs := parsed.Query()

	if got := qs.Get("q"); got != "Brampton" {
		t.Errorf("query text is %q — it must be the user's text alone, with no country appended", got)
	}
	if strings.Contains(u, "USA") {
		t.Errorf("URL still mentions USA: %s", u)
	}
	if got := qs.Get("in"); got != "countryCode:CAN" {
		t.Errorf("country filter = %q, want countryCode:CAN", got)
	}
}

// /geocode must NOT carry a proximity bias. This test previously asserted the
// exact opposite, and the reversal is the point.
//
// `at=` was there to make a bare "Brentwood" resolve deterministically for a
// Bay Area org. Measured against production it did not do that job: it
// SUPPRESSED distant candidates ("Windsor" for a Toronto org returned no
// Windsor, Ontario at all) while still letting HERE's own relevance put a
// hamlet above a city ("London" -> Thames Centre district). Both halves failed.
//
// Determinism did not go away — it moved somewhere it can be tested offline.
// See rankGeocodeResults and TestRank_SameTypeFallsBackToDistance.
func TestHereGeocodeURL_CarriesNoProximityBias(t *testing.T) {
	u := hereGeocodeURL("Brentwood", 5, geocodeScope{Country: "USA", Lat: 37.6368, Lng: -122.1269})
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if got := qs.Get("at"); got != "" {
		t.Errorf("proximity bias %q is set — it suppresses distant candidates before "+
			"rankGeocodeResults ever sees them", got)
	}
	if qs.Get("in") != "countryCode:USA" {
		t.Errorf("country filter = %q — the hard filter must stay", qs.Get("in"))
	}
}

// The country filter is independent of the warehouse: an organization with no
// warehouse yet must still be confined to its own country.
func TestHereGeocodeURL_UnsetWarehouseStillFiltersCountry(t *testing.T) {
	u := hereGeocodeURL("Toronto", 5, geocodeScope{Country: "CAN"})
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if qs.Get("in") != "countryCode:CAN" {
		t.Error("country filter should apply without a warehouse")
	}
	if qs.Get("at") != "" {
		t.Errorf("unexpected bias %q", qs.Get("at"))
	}
}

// Autosuggest is the one endpoint that REQUIRES an anchor, so an organization
// seeded at 0,0 must still produce a valid request. The centroid is coarse but
// on the right continent, which the Gulf of Guinea is not.
func TestAnchor_FallsBackToCountryCentroid(t *testing.T) {
	if got := (geocodeScope{Country: "CAN"}).anchor(); got != "56.130400,-106.346800" {
		t.Errorf("unset warehouse anchored at %q, want the Canadian centroid", got)
	}
	if got := (geocodeScope{Country: "CAN", Lat: 43.6426, Lng: -79.3771}).anchor(); got != "43.642600,-79.377100" {
		t.Errorf("anchor = %q, want the warehouse when it is set", got)
	}
	if got := (geocodeScope{}).anchor(); got != "39.828200,-98.579500" {
		t.Errorf("unknown country anchored at %q, want the US centroid default", got)
	}
}

// An unknown country must widen the search, not break it.
func TestHereGeocodeURL_UnknownCountryOmitsFilter(t *testing.T) {
	u := hereGeocodeURL("Springfield", 5, geocodeScope{Country: ""})
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if _, present := qs["in"]; present {
		t.Errorf("an empty country produced a filter: %q", qs.Get("in"))
	}
	if qs.Get("q") != "Springfield" {
		t.Errorf("query text mangled: %q", qs.Get("q"))
	}
}

// Text with spaces and punctuation must survive encoding — the old code did a
// manual space->plus replacement, which breaks on other reserved characters.
func TestHereGeocodeURL_EncodesQueryProperly(t *testing.T) {
	u := hereGeocodeURL("St. Catharines, ON", 5, geocodeScope{Country: "CAN"})
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := parsed.Query().Get("q"); got != "St. Catharines, ON" {
		t.Errorf("round-trip mangled the query: %q", got)
	}
}
