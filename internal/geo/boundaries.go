package geo

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// caPlacesJSON is the compiled-in boundary asset: incorporated California
// cities from US Census TIGER/Line 2025 (PLACE layer, FIPS 06), simplified to
// ~33 m. Public domain. Regenerate with scripts/build_ca_places.py when a new
// TIGER vintage ships. CDPs (unincorporated Census-designated places) are
// deliberately excluded — this covers LEGAL city limits only.
//
//go:embed data/ca_places.json
var caPlacesJSON []byte

// laDistrictsJSON is the compiled-in DISTRICT asset: 114 City-of-Los-Angeles
// neighborhoods from the LA Times "Mapping L.A." set (LA GeoHub, CC-BY 4.0,
// attribute "Los Angeles Times"), simplified to ~33 m. These are the canonical
// colloquial neighborhood polygons (Brentwood, Bel-Air…) — the district layer
// TIGER does not provide. Regenerate with scripts/build_la_districts.py.
//
//go:embed data/la_districts.json
var laDistrictsJSON []byte

// ring is a closed sequence of [lng, lat] vertices.
type ring [][2]float64

// simplePolygon is one outer ring with zero or more holes (donut cutouts —
// real city limits have them around enclosed unincorporated islands).
type simplePolygon struct {
	outer ring
	holes []ring
}

// Boundary is one area's true outline plus the raw GeoJSON geometry kept
// verbatim for serving to map clients (no lossy re-encode of the parsed rings).
type Boundary struct {
	Name     string     `json:"name"`
	NameNorm string     `json:"-"`
	NameLSAD string     `json:"namelsad"`
	Type     string     `json:"type"` // "city" (TIGER) or "district" (LA neighborhood)
	BBox     [4]float64 `json:"bbox"` // west, south, east, north

	polys       []simplePolygon
	rawGeometry json.RawMessage
}

// GeometryJSON returns the city's GeoJSON geometry (Polygon/MultiPolygon).
func (b *Boundary) GeometryJSON() json.RawMessage { return b.rawGeometry }

// Contains reports whether (lat, lng) falls within the city's legal limits.
// A point counts as inside if it lies in any polygon's outer ring and in none
// of that polygon's holes.
func (b *Boundary) Contains(lat, lng float64) bool {
	// Cheap bbox reject first — the vast majority of candidates are far away.
	if lng < b.BBox[0] || lng > b.BBox[2] || lat < b.BBox[1] || lat > b.BBox[3] {
		return false
	}
	for _, p := range b.polys {
		if pointInRing(lng, lat, p.outer) {
			inHole := false
			for _, h := range p.holes {
				if pointInRing(lng, lat, h) {
					inHole = true
					break
				}
			}
			if !inHole {
				return true
			}
		}
	}
	return false
}

// DistanceMeters returns 0 when (lat,lng) is inside the city, else the distance
// to the nearest point on its boundary — the true "how far past the edge",
// accurate for polygon cities where the bounding box would read 0 for a point
// in an interior gap.
func (b *Boundary) DistanceMeters(lat, lng float64) float64 {
	if b.Contains(lat, lng) {
		return 0
	}
	best := math.MaxFloat64
	cosLat := math.Cos(lat * math.Pi / 180)
	for _, p := range b.polys {
		if d := ringDistMeters(lat, lng, cosLat, p.outer); d < best {
			best = d
		}
		for _, h := range p.holes {
			if d := ringDistMeters(lat, lng, cosLat, h); d < best {
				best = d
			}
		}
	}
	return best
}

// ringDistMeters is the min distance (meters) from (lat,lng) to any edge of a
// ring, via a local equirectangular projection — exact enough at city scale.
func ringDistMeters(lat, lng, cosLat float64, r ring) float64 {
	const mPerDeg = 111320.0
	best := math.MaxFloat64
	for i := 0; i+1 < len(r); i++ {
		// Project both endpoints to planar meters with the target at the origin.
		ax := (r[i][0] - lng) * mPerDeg * cosLat
		ay := (r[i][1] - lat) * mPerDeg
		bx := (r[i+1][0] - lng) * mPerDeg * cosLat
		by := (r[i+1][1] - lat) * mPerDeg
		if d := distToSegment(ax, ay, bx, by); d < best {
			best = d
		}
	}
	return best
}

// distToSegment is the distance from the origin to segment (ax,ay)-(bx,by).
func distToSegment(ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(ax, ay)
	}
	t := -(ax*dx + ay*dy) / l2
	t = math.Max(0, math.Min(1, t))
	px, py := ax+t*dx, ay+t*dy
	return math.Hypot(px, py)
}

// pointInRing runs the standard even-odd ray-casting test. x=lng, y=lat.
func pointInRing(x, y float64, r ring) bool {
	inside := false
	n := len(r)
	if n < 3 {
		return false
	}
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := r[i][0], r[i][1]
		xj, yj := r[j][0], r[j][1]
		if (yi > y) != (yj > y) {
			xIntersect := (xj-xi)*(y-yi)/(yj-yi) + xi
			if x < xIntersect {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

// BoundaryStore is an in-memory index of boundaries keyed by normalized name,
// split into cities (TIGER) and districts (LA neighborhoods). Loaded once at
// startup and read concurrently thereafter (never mutated after Load, so no
// locking needed).
type BoundaryStore struct {
	cityByNorm     map[string]*Boundary
	districtByNorm map[string]*Boundary
}

// districtTypes are HERE area types that resolve to a NEIGHBORHOOD polygon
// (LA Times set) rather than a city.
var districtTypes = map[string]bool{"district": true, "subdistrict": true}

// rejectTypes are area types we never have an authoritative polygon for —
// picking one must not fabricate a city/district shape.
var rejectTypes = map[string]bool{
	"county": true, "administrativeArea": true, "postalCode": true,
	"state": true, "country": true,
}

// loadRecords parses one embedded asset (cities or districts) into a
// name-keyed map. First writer wins on a name collision (names are ~unique
// within each layer; the lat/lng sanity check in Lookup catches the rest).
func loadRecords(data []byte, typ, label string) (map[string]*Boundary, error) {
	var records []struct {
		Name     string          `json:"name"`
		NameNorm string          `json:"name_norm"`
		NameLSAD string          `json:"namelsad"`
		Parent   string          `json:"parent"`
		BBox     [4]float64      `json:"bbox"`
		Geometry json.RawMessage `json:"geometry"`
	}
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	out := make(map[string]*Boundary, len(records))
	for _, rec := range records {
		polys, err := parseGeometry(rec.Geometry)
		if err != nil {
			return nil, fmt.Errorf("parse geometry for %q: %w", rec.Name, err)
		}
		lsad := rec.NameLSAD
		if lsad == "" && rec.Parent != "" {
			lsad = rec.Name + ", " + rec.Parent
		}
		b := &Boundary{
			Name: rec.Name, NameNorm: rec.NameNorm, NameLSAD: lsad,
			Type: typ, BBox: rec.BBox, polys: polys, rawGeometry: rec.Geometry,
		}
		if _, exists := out[b.NameNorm]; !exists {
			out[b.NameNorm] = b
		}
	}
	return out, nil
}

// LoadBoundaries parses the embedded city + district assets into a lookup store.
func LoadBoundaries() (*BoundaryStore, error) {
	cities, err := loadRecords(caPlacesJSON, "city", "ca_places.json")
	if err != nil {
		return nil, err
	}
	districts, err := loadRecords(laDistrictsJSON, "district", "la_districts.json")
	if err != nil {
		return nil, err
	}
	return &BoundaryStore{cityByNorm: cities, districtByNorm: districts}, nil
}

// Count returns the total number of loaded boundaries (cities + districts).
func (s *BoundaryStore) Count() int { return len(s.cityByNorm) + len(s.districtByNorm) }

// Lookup resolves a picked area to its boundary polygon, or nil when there is
// none: a district-type name resolves against the neighborhood layer, a
// city-type name against the city layer, and county/postal/etc. never resolve.
// A same-name match far from where the user picked is rejected by the lat/lng
// sanity check (pass 0,0 to skip it).
func (s *BoundaryStore) Lookup(name, typ string, lat, lng float64) *Boundary {
	if rejectTypes[typ] {
		return nil
	}
	var b *Boundary
	if districtTypes[typ] {
		b = s.districtByNorm[normName(name)]
	} else {
		b = s.cityByNorm[normName(name)]
	}
	if b == nil {
		return nil
	}
	// Reject a same-name area elsewhere: the picked center must sit inside the
	// matched boundary's bbox (with a ~5.5 km margin for centers HERE places
	// just outside the line).
	if lat != 0 || lng != 0 {
		const margin = 0.05
		if lng < b.BBox[0]-margin || lng > b.BBox[2]+margin ||
			lat < b.BBox[1]-margin || lat > b.BBox[3]+margin {
			return nil
		}
	}
	return b
}

// normName lowercases and collapses whitespace to match the asset's name_norm.
func normName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// parseGeometry converts a GeoJSON Polygon/MultiPolygon into simplePolygons.
func parseGeometry(raw json.RawMessage) ([]simplePolygon, error) {
	var g struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	switch g.Type {
	case "Polygon":
		var rings [][][2]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil {
			return nil, err
		}
		return []simplePolygon{ringsToPolygon(rings)}, nil
	case "MultiPolygon":
		var multi [][][][2]float64
		if err := json.Unmarshal(g.Coordinates, &multi); err != nil {
			return nil, err
		}
		polys := make([]simplePolygon, 0, len(multi))
		for _, rings := range multi {
			polys = append(polys, ringsToPolygon(rings))
		}
		return polys, nil
	default:
		return nil, fmt.Errorf("unsupported geometry type %q", g.Type)
	}
}

// ringsToPolygon takes GeoJSON polygon rings (outer first, holes after) into a
// simplePolygon.
func ringsToPolygon(rings [][][2]float64) simplePolygon {
	var p simplePolygon
	for i, r := range rings {
		if i == 0 {
			p.outer = ring(r)
		} else {
			p.holes = append(p.holes, ring(r))
		}
	}
	return p
}
