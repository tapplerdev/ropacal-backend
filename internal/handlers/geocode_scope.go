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
// So scoping is two parts:
//
//   - in=countryCode:{org country}  — a hard filter, no cross-border results
//   - at={warehouse lat,lng}        — proximity RANKING, so the nearest match wins
//
// That keeps determinism without hardcoding a country: a Hayward organization
// searching "Brentwood" still gets Brentwood CA because it is nearest, and a
// Toronto organization searching "Windsor" gets Windsor ON rather than Windsor
// CA. The behaviour US tenants relied on is preserved and now generalises.

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

// apply appends the scoping parameters to a HERE geocode query string.
func (s geocodeScope) apply(qs *url.Values) {
	if s.Country != "" {
		qs.Set("in", "countryCode:"+s.Country)
	}
	// Proximity ranking. Omitted when the warehouse is unset — provisioning
	// seeds new organizations at 0,0, and biasing toward the Gulf of Guinea is
	// worse than no bias at all.
	if s.Lat != 0 || s.Lng != 0 {
		qs.Set("at", fmt.Sprintf("%.6f,%.6f", s.Lat, s.Lng))
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
// search.
//
// /geocode is the wrong endpoint for a picker. Measured: with the Toronto
// warehouse as the proximity anchor, "Windsor" on /geocode returned exactly ONE
// result — a POI near Toronto typed `place` — and Windsor, Ontario (pop.
// 230,000, 350 km away) never appeared at all. The `at=` bias makes /geocode
// prefer a strong nearby match and suppress distant ones, which is precisely
// backwards for "where should we expand". Dropping the bias fixes Windsor and
// breaks US determinism, so neither setting of that one knob is correct.
//
// /autosuggest surfaces both the near and the distant candidates. Its own
// ranking is weaker — it put Springfield, MO above Springfield, CA for a Bay
// Area anchor — so the ORDER is redone server-side by rankGeocodeResults, where
// it can be reasoned about instead of inferred from a vendor's heuristics.
func hereAutosuggestURL(q string, limit int, s geocodeScope) string {
	qs := url.Values{}
	qs.Set("q", q)
	qs.Set("limit", fmt.Sprintf("%d", limit))
	qs.Set("apiKey", HereAPIKey)
	s.apply(&qs)
	// /autosuggest REQUIRES `at` or `in=circle`. Fall back to the country
	// centroid when the warehouse is unset so the request is still valid.
	if qs.Get("at") == "" {
		qs.Set("at", countryCentroid(s.Country))
	}
	return "https://autosuggest.search.hereapi.com/v1/autosuggest?" + qs.Encode()
}

// countryCentroid is a rough anchor for /autosuggest when an organization has
// no warehouse yet. Only ever affects tie-breaking, never filtering.
func countryCentroid(here3 string) string {
	switch here3 {
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
