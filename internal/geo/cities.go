package geo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// citiesJSON is the compiled-in city gazetteer: every populated place over
// 5,000 people worldwide, from the GeoNames cities5000 dump — name, centroid,
// country, admin1 and population. Regenerate with scripts/build_cities.py.
//
// ATTRIBUTION (CC BY 4.0, required wherever this data is surfaced):
//
//	Includes data from GeoNames (https://www.geonames.org/), CC BY 4.0.
//
// WHY THIS IS EMBEDDED RATHER THAN QUERIED
// The expansion recommender used to work from eight hardcoded Bay Area cities,
// so a Toronto tenant was told to expand into Fremont and Hayward. The fix has
// to cover any country a tenant might operate in, which rules out the US-only
// TIGER data already embedded here. Of the alternatives checked, GeoNames is the
// only one with worldwide coverage, real populations AND a license permitting
// commercial embedding: HERE exposes no population field at all, and OSM's
// population tags are simply absent for Oakville, Ajax and Newmarket among
// others in the GTA — a ranking built on those would silently drop real cities.
//
//go:embed data/cities.json
var citiesJSON []byte

// City is one populated place. Coordinates are the GeoNames centroid, not a
// boundary — for true municipal outlines see Boundary in boundaries.go.
type City struct {
	Name       string  `json:"name"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	Country    string  `json:"country"` // ISO-3166 alpha-2, e.g. "US", "CA"
	Admin1     string  `json:"admin1"`  // state/province code, GeoNames admin1
	Population int     `json:"pop"`
}

// NearbyCity is a City plus how far it sits from the query point.
type NearbyCity struct {
	City
	DistanceMiles float64
}

var (
	citiesOnce sync.Once
	cities     []City
	citiesErr  error
)

func loadCities() {
	citiesOnce.Do(func() {
		if err := json.Unmarshal(citiesJSON, &cities); err != nil {
			citiesErr = fmt.Errorf("geo: parsing embedded cities.json: %w", err)
		}
	})
}

// CityCount reports how many cities are compiled in. Used by tests and the
// boot log so a truncated or missing asset is visible immediately rather than
// as an empty recommendation list weeks later.
func CityCount() (int, error) {
	loadCities()
	return len(cities), citiesErr
}

// CitiesWithin returns every known city within radiusMiles of (lat, lng),
// nearest-agnostic and sorted by POPULATION descending, largest first.
//
// The origin is expected to be a tenant's warehouse. Callers must not pass 0,0:
// provisioning seeds new organizations with a 0,0 warehouse, and a radius around
// that lands in the Gulf of Guinea, which would return a handful of West African
// towns as "expansion candidates". That case returns an error rather than an
// empty list, so the caller cannot mistake it for "no cities nearby".
//
// Cost is a linear scan with a cheap bounding-box reject before each haversine.
// At ~64k cities that is well under a millisecond, so there is no index.
func CitiesWithin(lat, lng, radiusMiles float64) ([]NearbyCity, error) {
	loadCities()
	if citiesErr != nil {
		return nil, citiesErr
	}
	if lat == 0 && lng == 0 {
		return nil, fmt.Errorf("geo: refusing to search around 0,0 — the warehouse location is unset")
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return nil, fmt.Errorf("geo: origin out of range (%.4f, %.4f)", lat, lng)
	}
	if radiusMiles <= 0 {
		return nil, fmt.Errorf("geo: radius must be positive, got %.1f", radiusMiles)
	}

	// Latitude degrees are ~69 miles everywhere; longitude shrinks toward the
	// poles. This box only PRE-FILTERS — the haversine below is authoritative —
	// so a generous margin costs nothing but avoids clipping near the poles.
	dLat := radiusMiles/69.0 + 0.01
	cosLat := cosDeg(lat)
	if cosLat < 0.01 {
		cosLat = 0.01
	}
	dLng := radiusMiles/(69.0*cosLat) + 0.01

	out := make([]NearbyCity, 0, 64)
	for _, c := range cities {
		if c.Lat < lat-dLat || c.Lat > lat+dLat {
			continue
		}
		// Deliberately no antimeridian wrap: a tenant whose service area spans
		// ±180° does not exist, and a silent wrap bug would be worse than the
		// miss.
		if c.Lng < lng-dLng || c.Lng > lng+dLng {
			continue
		}
		if d := HaversineMiles(lat, lng, c.Lat, c.Lng); d <= radiusMiles {
			out = append(out, NearbyCity{City: c, DistanceMiles: d})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Population != out[j].Population {
			return out[i].Population > out[j].Population
		}
		return out[i].Name < out[j].Name // stable for equal populations
	})
	return out, nil
}

// ExcludeCities drops any candidate whose name matches one already covered.
//
// Matching is case- and space-insensitive on the name alone. That is
// deliberately loose: the covered set comes from bin address fields typed by
// humans and geocoders, so "St. Catharines" and "st catharines" must collide.
// The cost of a false match is one missing suggestion; the cost of a miss is
// recommending a city the tenant already serves, which reads as a broken
// product.
func ExcludeCities(candidates []NearbyCity, covered []string) []NearbyCity {
	if len(covered) == 0 {
		return candidates
	}
	skip := make(map[string]struct{}, len(covered))
	for _, c := range covered {
		if k := normalizeCityKey(c); k != "" {
			skip[k] = struct{}{}
		}
	}
	out := make([]NearbyCity, 0, len(candidates))
	for _, c := range candidates {
		if _, found := skip[normalizeCityKey(c.Name)]; found {
			continue
		}
		out = append(out, c)
	}
	return out
}

func normalizeCityKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(".", "", ",", "", "-", " ", "'", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
