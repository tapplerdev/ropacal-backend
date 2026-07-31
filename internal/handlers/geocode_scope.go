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
// Toronto organization searching "Waterloo" gets Waterloo ON. The behaviour US
// tenants relied on is preserved and now generalises.
//
// TWO GAPS THE ANCHOR LEAVES, both measured against production and both patched
// where they occur rather than by abandoning the anchor:
//
//   - It does not order same-name results sensibly. "London" returned the
//     Thames Centre district above the City of London (also Cambridge, Guelph,
//     Peterborough — 4 of 10 Ontario cities). Fixed by rankGeocodeResults.
//   - It can suppress a distant city entirely. "Windsor" returned one POI near
//     Toronto and nothing else. Fixed by the unbiased() retry.
//
// Replacing the anchor was tried and measured worse: without it, HERE stops
// returning the nearby city at all — a Hayward org searching "Hayward" gets
// streets and a district, and "Richmond" loses Richmond CA in favour of
// Richmond VA. The anchor earns its place; it just needed help at the edges.

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

// unbiased is this scope with the proximity anchor removed, keeping the country
// filter. Used for ONE case: the bias occasionally suppresses a distant city so
// completely that nothing survives filtering. "Windsor" for a Toronto org
// returned a single POI near Toronto and no Windsor, Ontario at all; unanchored,
// the same query returns Windsor ON and Windsor QC.
//
// This is a fallback, not the default, because the anchor is doing real work
// the rest of the time — it is what makes "Hayward" resolve to Hayward.
func (s geocodeScope) unbiased() geocodeScope {
	s.Lat, s.Lng = 0, 0
	return s
}

// hereGeocodeURL builds a scoped geocode request.
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
