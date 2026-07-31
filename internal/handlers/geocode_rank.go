package handlers

import (
	"math"
	"sort"
)

// Server-side ranking for the area picker.
//
// WHY WE RANK AT ALL, rather than trusting HERE's order.
//
// Measured against the deployed endpoint, Toronto organization:
//
//	"London"  ->  1. London, Thames Centre, ON  [district]
//	              2. London, ON                 [city]      <- pop. 420,000
//
// Same shape for Cambridge, Guelph, and Peterborough: a hamlet inside a
// neighbouring township outranks the city itself. This is NOT a proximity
// artifact — Thames Centre sits beside London, both ~190 km from Toronto, so
// removing the `at=` bias does not change it. It is HERE's own relevance
// scoring, and there is no request parameter that means "prefer cities".
//
// The ordering this picker needs is domain knowledge HERE does not have: it
// selects OPERATING AREAS NEAR A DEPOT, so a city beats a same-named district,
// and closer beats farther. Encoding that here is not second-guessing the
// vendor's relevance — it is supplying context the vendor was never given.
//
// It also makes a promise the old code only claimed. The comment in
// geocode_scope.go asserted that `at=` gave deterministic resolution, but that
// determinism was delegated to an undocumented vendor heuristic: switching
// endpoints silently flipped "Springfield" from Springfield CA to Springfield
// MO for a Bay Area org. A guarantee you do not implement is not a guarantee.
// This function is the implementation, and it is unit-testable offline.

// typeRank orders result kinds by how likely they are to be what someone means
// when they type a name into an area picker.
//
// Deliberately a total function over one flat switch. If this ever grows an
// "except when..." branch, the rule has stopped being explainable and the right
// move is to rethink it rather than extend it.
func typeRank(typ string) int {
	switch typ {
	case "city", "town", "village", "locality":
		return 0
	case "district", "subdistrict", "quarter":
		return 1
	case "county", "administrativeArea":
		return 2
	case "street", "houseNumber", "address", "addressBlock", "intersection":
		return 3
	default:
		return 4
	}
}

// haversineKm is great-circle distance. Precision beyond a few hundred metres
// is irrelevant here — this only ever breaks ties within one type rank.
func haversineKm(lat1, lng1, lat2, lng2 float64) float64 {
	const earthKm = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthKm * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// rankGeocodeResults orders results in place: type first, then distance from the
// organization's warehouse.
//
// With no warehouse set, HERE's original order is preserved within each type
// rank rather than sorted against 0,0 — ranking Ontario by proximity to the
// Gulf of Guinea would be worse than not ranking at all.
func rankGeocodeResults(out []geocodeSearchResult, s geocodeScope) {
	hasOrigin := s.Lat != 0 || s.Lng != 0
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := typeRank(out[i].Type), typeRank(out[j].Type)
		if ri != rj {
			return ri < rj
		}
		if !hasOrigin {
			return false // stable: keep HERE's order inside the rank
		}
		return haversineKm(s.Lat, s.Lng, out[i].Lat, out[i].Lng) <
			haversineKm(s.Lat, s.Lng, out[j].Lat, out[j].Lng)
	})
}
