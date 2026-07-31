package handlers

import (
	"fmt"
	"net/url"
	"strings"

	"ropacal-backend/internal/orgdb"
)

// Country- and proximity-scoping for HERE geocoding.
//
// Every geocode call used to hardcode the United States twice — appending
// ",USA" to the query text AND filtering in=countryCode:USA. A Canadian
// organization typing "Brampton" into the area picker got "Brampton Twp, MI,
// United States", because Brampton, Ontario was excluded by construction.
//
// WHY NOT JUST SWAP THE COUNTRY. The USA pin was not arbitrary: it existed to
// make headless resolution DETERMINISTIC. Without it, a bare "Brentwood" is
// ambiguous across states and a background run could silently resolve to the
// wrong one. Replacing the pin with the organization's country alone would
// reintroduce exactly that for US tenants.
//
// WHERE THE PROXIMITY BIAS WENT. This file originally solved that by sending
// `at={warehouse}` on every request, letting HERE rank by nearness. Measurement
// killed that idea twice over:
//
//   - It SUPPRESSES distant matches. "Windsor" for a Toronto org returned one
//     POI near Toronto and no Windsor, Ontario at all.
//   - It does not even deliver the ranking it was there for. "London" still
//     returned the Thames Centre district above the City of London, because
//     both are ~190 km out and HERE's own relevance decided the rest.
//
// So the request now carries the country filter ONLY, and proximity is applied
// afterwards by rankGeocodeResults, over the full unsuppressed candidate list.
// The determinism US tenants relied on is preserved — a Hayward org searching
// "Brentwood" still gets Brentwood CA — but it is now something this codebase
// implements and unit-tests, rather than something it hopes a vendor heuristic
// keeps doing.
//
//   - in=countryCode:{org country}  — hard filter, no cross-border results
//   - anchor()                      — only for endpoints that REQUIRE one

// hereCountryCode maps ISO 3166-1 alpha-2 (organizations.country) to the
// alpha-3 codes HERE expects. Unknown codes return "" so the caller omits the
// filter rather than sending a malformed one — a wider search beats no results.
func hereCountryCode(iso2 string) string {
	switch strings.ToUpper(strings.TrimSpace(iso2)) {
	case "US":
		return "USA"
	case "CA":
		return "CAN"
	case "GB":
		return "GBR"
	case "AU":
		return "AUS"
	case "MX":
		return "MEX"
	default:
		return ""
	}
}

// geocodeScope is the country filter and proximity bias for one organization.
type geocodeScope struct {
	Country  string  // HERE alpha-3, or "" for unfiltered
	Lat, Lng float64 // warehouse; zero when unset
}

// scopeForOrg reads the organization's country and warehouse once.
func scopeForOrg(db *orgdb.DB) geocodeScope {
	s := geocodeScope{Country: hereCountryCode(organizationCountry(db))}
	if lat, lng, ok := warehouseCoords(db); ok {
		s.Lat, s.Lng = lat, lng
	}
	return s
}

// apply appends the hard country filter. Deliberately NOT the proximity bias —
// see the note above; that is rankGeocodeResults' job now.
func (s geocodeScope) apply(qs *url.Values) {
	if s.Country != "" {
		qs.Set("in", "countryCode:"+s.Country)
	}
}

// anchor is the `at=` value for endpoints that REQUIRE one (autosuggest rejects
// a request without it). Falls back to the country centroid, because
// provisioning seeds new organizations at 0,0 and anchoring a Canadian search
// in the Gulf of Guinea is worse than a coarse but correct-continent anchor.
func (s geocodeScope) anchor() string {
	if s.Lat != 0 || s.Lng != 0 {
		return fmt.Sprintf("%.6f,%.6f", s.Lat, s.Lng)
	}
	switch s.Country {
	case "CAN":
		return "56.130400,-106.346800"
	case "GBR":
		return "54.702400,-3.276600"
	case "AUS":
		return "-25.274400,133.775100"
	case "MEX":
		return "23.634500,-102.552800"
	default:
		return "39.828200,-98.579500" // geographic centre of the contiguous US
	}
}

// hereGeocodeURL builds a scoped /geocode request — used where a single
// authoritative resolution is wanted (a known city name from a tool call), not
// for interactive search. See hereAutosuggestURL for the picker.
//
// The raw query is passed through WITHOUT a country suffix. Appending ",USA"
// was the other half of the old bug: it polluted the search text itself, so
// even lifting the filter would not have fixed it.
func hereGeocodeURL(q string, limit int, s geocodeScope) string {
	qs := url.Values{}
	qs.Set("q", q)
	qs.Set("limit", fmt.Sprintf("%d", limit))
	qs.Set("apiKey", HereAPIKey)
	s.apply(&qs)
	return "https://geocode.search.hereapi.com/v1/geocode?" + qs.Encode()
}

// hereAutosuggestURL builds a scoped /autosuggest request for INTERACTIVE
// search — the picker, where the user is still typing.
//
// This is the endpoint HERE ships for typeahead, and it earns its place on
// PREFIXES: measured across ten partial strings in both countries ("Bramp",
// "Mississ", "San Jo", "Sacrame"…), it put the intended city first 10/10.
// /geocode resolves complete place names and has no such guarantee.
//
// What it is NOT good at is surfacing the right cities, because it is
// POI-weighted by design. Anchored at Hayward, "Richmond" came back as one
// distant city (VA), one district, and six local streets — Richmond, CA, 34 km
// away and 116,000 people, was absent from the response entirely. That is why
// the anchor is a bare requirement here rather than a ranking strategy, and why
// candidateLimit asks for far more rows than the picker shows.
func hereAutosuggestURL(q string, limit int, s geocodeScope) string {
	qs := url.Values{}
	qs.Set("q", q)
	qs.Set("limit", fmt.Sprintf("%d", limit))
	qs.Set("apiKey", HereAPIKey)
	s.apply(&qs)
	qs.Set("at", s.anchor()) // required: autosuggest rejects an unanchored request
	return "https://autosuggest.search.hereapi.com/v1/autosuggest?" + qs.Encode()
}
