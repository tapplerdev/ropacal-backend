package geo

import (
	_ "embed"
	"encoding/json"
	"fmt"
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

// ring is a closed sequence of [lng, lat] vertices.
type ring [][2]float64

// simplePolygon is one outer ring with zero or more holes (donut cutouts —
// real city limits have them around enclosed unincorporated islands).
type simplePolygon struct {
	outer ring
	holes []ring
}

// Boundary is one city's true legal outline plus the raw GeoJSON geometry kept
// verbatim for serving to map clients (no lossy re-encode of the parsed rings).
type Boundary struct {
	Name     string     `json:"name"`
	NameNorm string     `json:"-"`
	NameLSAD string     `json:"namelsad"`
	Type     string     `json:"type"` // always "city" for TIGER places
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

// BoundaryStore is an in-memory index of city boundaries keyed by normalized
// name. Loaded once at startup and read concurrently thereafter (never mutated
// after Load, so no locking needed).
type BoundaryStore struct {
	byNorm map[string]*Boundary
}

// nonCityTypes are HERE area types that must never resolve to a TIGER city
// polygon. Without this guard, picking the LA DISTRICT "Brentwood" would match
// the Contra Costa CITY "Brentwood" and draw the wrong shape 400 miles away.
var nonCityTypes = map[string]bool{
	"district": true, "subdistrict": true, "county": true,
	"administrativeArea": true, "postalCode": true, "state": true, "country": true,
}

// LoadBoundaries parses the embedded asset into a lookup store.
func LoadBoundaries() (*BoundaryStore, error) {
	var records []struct {
		Name     string          `json:"name"`
		NameNorm string          `json:"name_norm"`
		NameLSAD string          `json:"namelsad"`
		BBox     [4]float64      `json:"bbox"`
		Geometry json.RawMessage `json:"geometry"`
	}
	if err := json.Unmarshal(caPlacesJSON, &records); err != nil {
		return nil, fmt.Errorf("parse ca_places.json: %w", err)
	}
	store := &BoundaryStore{byNorm: make(map[string]*Boundary, len(records))}
	for _, rec := range records {
		polys, err := parseGeometry(rec.Geometry)
		if err != nil {
			return nil, fmt.Errorf("parse geometry for %q: %w", rec.Name, err)
		}
		b := &Boundary{
			Name: rec.Name, NameNorm: rec.NameNorm, NameLSAD: rec.NameLSAD,
			Type: "city", BBox: rec.BBox, polys: polys, rawGeometry: rec.Geometry,
		}
		// First writer wins on a name collision (place names are ~unique per
		// state in TIGER); the lat/lng sanity check in Lookup catches the rest.
		if _, exists := store.byNorm[b.NameNorm]; !exists {
			store.byNorm[b.NameNorm] = b
		}
	}
	return store, nil
}

// Count returns the number of loaded city boundaries.
func (s *BoundaryStore) Count() int { return len(s.byNorm) }

// Lookup resolves a picked area to its city boundary, or nil when there is no
// authoritative polygon (a district, a county, an unknown name, or a name that
// collides with a city far from where the user actually picked).
//
// typ is the HERE area type; lat/lng is the picked center (pass 0,0 to skip the
// geographic sanity check).
func (s *BoundaryStore) Lookup(name, typ string, lat, lng float64) *Boundary {
	if nonCityTypes[typ] {
		return nil // TIGER covers cities only — never fake a district/county
	}
	b := s.byNorm[normName(name)]
	if b == nil {
		return nil
	}
	// Reject a same-name city in a different part of the state: the picked
	// center must sit inside the matched city's bbox (with a ~5.5 km margin for
	// centers that HERE places just outside the legal line).
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
