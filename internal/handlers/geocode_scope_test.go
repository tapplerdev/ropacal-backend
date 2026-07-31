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
	if got := qs.Get("at"); got != "43.642600,-79.377100" {
		t.Errorf("proximity bias = %q, want the warehouse coordinates", got)
	}
}

// Proximity is what preserves determinism for US tenants after the hardcoded
// country pin was removed. A Bay Area organization searching "Brentwood" must
// still be biased toward its own warehouse.
func TestHereGeocodeURL_ProximityBiasIsSet(t *testing.T) {
	u := hereGeocodeURL("Brentwood", 5, geocodeScope{Country: "USA", Lat: 37.6368, Lng: -122.1269})
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if qs.Get("at") == "" {
		t.Error("no proximity bias — a bare 'Brentwood' becomes ambiguous across states again")
	}
	if qs.Get("in") != "countryCode:USA" {
		t.Errorf("country filter = %q", qs.Get("in"))
	}
}

// Provisioning seeds new organizations with a 0,0 warehouse. Biasing a search
// toward the Gulf of Guinea is worse than not biasing at all.
func TestHereGeocodeURL_UnsetWarehouseOmitsProximity(t *testing.T) {
	u := hereGeocodeURL("Toronto", 5, geocodeScope{Country: "CAN"})
	qs, _ := url.ParseQuery(strings.SplitN(u, "?", 2)[1])
	if qs.Get("at") != "" {
		t.Errorf("proximity bias %q was set from an unset (0,0) warehouse", qs.Get("at"))
	}
	if qs.Get("in") != "countryCode:CAN" {
		t.Error("country filter should still apply without a warehouse")
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
