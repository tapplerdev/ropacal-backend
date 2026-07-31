package geo

import "testing"

// GTA coordinates used to resolve a picked area to its polygon.
var gtaPoints = map[string][2]float64{
	"Toronto":       {43.6532, -79.3832},
	"Mississauga":   {43.5890, -79.6441},
	"Brampton":      {43.7315, -79.7624},
	"Vaughan":       {43.8361, -79.4983},
	"Markham":       {43.8668, -79.2663},
	"Oakville":      {43.4501, -79.6829},
	"Burlington":    {43.3255, -79.7990},
	"Pickering":     {43.8384, -79.0868},
	"Ajax":          {43.8509, -79.0204},
	"Richmond Hill": {43.8828, -79.4403},
}

func store(t *testing.T) *BoundaryStore {
	t.Helper()
	s, err := LoadBoundaries()
	if err != nil {
		t.Fatalf("LoadBoundaries: %v", err)
	}
	return s
}

// The whole point: before this, every GTA area fell back to a bounding box.
func TestOntario_GTAMunicipalitiesResolve(t *testing.T) {
	s := store(t)
	for name, ll := range gtaPoints {
		b := s.Lookup(name, "city", ll[0], ll[1])
		if b == nil {
			t.Errorf("%s did not resolve to a boundary", name)
			continue
		}
		if !b.Contains(ll[0], ll[1]) {
			t.Errorf("%s resolved, but its own centre is outside the polygon", name)
		}
	}
}

// Ontario's polygons must not have displaced California's.
func TestOntario_CaliforniaStillResolves(t *testing.T) {
	s := store(t)
	cases := []struct {
		name     string
		lat, lng float64
	}{
		{"Hayward", 37.6688, -122.0808},
		{"Fremont", 37.5485, -121.9886},
		{"Oakland", 37.8044, -122.2712},
		{"San Jose", 37.3382, -121.8863},
	}
	for _, c := range cases {
		b := s.Lookup(c.name, "city", c.lat, c.lng)
		if b == nil {
			t.Errorf("%s (CA) no longer resolves after adding Ontario", c.name)
			continue
		}
		if !b.Contains(c.lat, c.lng) {
			t.Errorf("%s (CA) resolved to a polygon not containing it", c.name)
		}
	}
}

// THE COLLISION CASE. Windsor is a real city in BOTH California and Ontario.
// Under the old single-value map one of them was permanently unreachable —
// whichever asset loaded second lost. Coordinates must decide.
func TestOntario_SameNameDifferentContinent(t *testing.T) {
	s := store(t)

	onLat, onLng := 42.3149, -83.0364 // Windsor, Ontario
	ca := s.Lookup("Windsor", "city", 38.5471, -122.8164)
	on := s.Lookup("Windsor", "city", onLat, onLng)

	if ca == nil && on == nil {
		t.Skip("no Windsor in either asset; nothing to disambiguate")
	}
	if on == nil {
		t.Error("Windsor, Ontario did not resolve — the Ontario entry is unreachable")
	} else if !on.Contains(onLat, onLng) {
		t.Error("Windsor/Ontario coords resolved to a polygon that does not contain them")
	}
	if ca != nil && on != nil && ca == on {
		t.Error("both coordinate sets resolved to the SAME boundary — disambiguation is not working")
	}
}

// A wrong-continent coordinate must resolve to nothing rather than to the
// same-named city elsewhere. Silently returning the California polygon for a
// Toronto pick would draw a boundary on the other side of the planet.
func TestOntario_WrongCoordinatesRejected(t *testing.T) {
	s := store(t)
	if b := s.Lookup("Mississauga", "city", 37.6688, -122.0808); b != nil {
		t.Errorf("Mississauga resolved from Bay Area coordinates -> %s", b.Name)
	}
	if b := s.Lookup("Hayward", "city", 43.6532, -79.3832); b != nil {
		t.Errorf("Hayward resolved from Toronto coordinates -> %s", b.Name)
	}
}

// Toronto's polygon must actually be Toronto-shaped: contain the downtown core
// and exclude a neighbouring municipality's centre.
func TestOntario_PolygonIsAccurateNotJustPresent(t *testing.T) {
	s := store(t)
	tor := s.Lookup("Toronto", "city", 43.6532, -79.3832)
	if tor == nil {
		t.Fatal("Toronto missing")
	}
	inside := [][2]float64{
		{43.6532, -79.3832}, // Yonge & Dundas
		{43.6426, -79.3871}, // CN Tower
		{43.7615, -79.4111}, // North York
	}
	for _, p := range inside {
		if !tor.Contains(p[0], p[1]) {
			t.Errorf("(%.4f, %.4f) should be inside Toronto", p[0], p[1])
		}
	}
	outside := [][2]float64{
		{43.5890, -79.6441}, // Mississauga centre
		{43.8668, -79.2663}, // Markham centre
		{43.4501, -79.6829}, // Oakville centre
	}
	for _, p := range outside {
		if tor.Contains(p[0], p[1]) {
			t.Errorf("(%.4f, %.4f) should NOT be inside Toronto", p[0], p[1])
		}
	}
}

// Counties and postal codes have no authoritative polygon in any asset; adding
// a region must not change that.
func TestOntario_RejectedTypesStayRejected(t *testing.T) {
	s := store(t)
	for _, typ := range []string{"county", "postalCode", "state", "country"} {
		if b := s.Lookup("Toronto", typ, 43.6532, -79.3832); b != nil {
			t.Errorf("type %q resolved to %s — should never resolve", typ, b.Name)
		}
	}
}
