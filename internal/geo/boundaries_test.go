package geo

import "testing"

func loadOrFail(t *testing.T) *BoundaryStore {
	t.Helper()
	store, err := LoadBoundaries()
	if err != nil {
		t.Fatalf("LoadBoundaries: %v", err)
	}
	if store.Count() < 400 {
		t.Fatalf("expected ~483 CA cities, got %d", store.Count())
	}
	return store
}

func TestContains_RealPoints(t *testing.T) {
	store := loadOrFail(t)

	cases := []struct {
		city     string
		lat, lng float64
		want     bool
		desc     string
	}{
		{"Berkeley", 37.8715, -122.2730, true, "UC Berkeley campus"},
		{"Berkeley", 37.7955, -122.3937, false, "SF Ferry Building"},
		{"Berkeley", 37.8044, -122.2712, false, "downtown Oakland (adjacent)"},
		{"San Jose", 37.3379, -121.8863, true, "San Jose City Hall (multipolygon)"},
		{"San Jose", 37.4419, -122.1430, false, "Palo Alto"},
		{"Oakland", 37.8044, -122.2712, true, "downtown Oakland"},
		{"Hayward", 37.6688, -122.0808, true, "downtown Hayward"},
	}
	for _, c := range cases {
		b := store.Lookup(c.city, "city", c.lat, c.lng)
		if b == nil {
			// A miss is only acceptable when we expected the point OUTSIDE and
			// the geographic sanity check rejected the lookup; but here we pass
			// a matching city center context, so a nil means the city is absent.
			if c.want {
				t.Errorf("%s: city %q not found in store", c.desc, c.city)
			}
			continue
		}
		if got := b.Contains(c.lat, c.lng); got != c.want {
			t.Errorf("%s: %s.Contains(%.4f,%.4f) = %v, want %v", c.desc, c.city, c.lat, c.lng, got, c.want)
		}
	}
}

func TestLookup_TypeGate(t *testing.T) {
	store := loadOrFail(t)

	// A DISTRICT-type "Brentwood" at LA coords resolves to the LA NEIGHBORHOOD
	// polygon (LA Times layer) — NOT the Contra Costa city.
	if b := store.Lookup("Brentwood", "district", 34.0522, -118.4740); b == nil || b.Type != "district" {
		t.Errorf("LA district Brentwood did not resolve to a district polygon: %+v", b)
	}
	// A CITY-type "Brentwood" picked in LA must still be rejected — the only
	// Brentwood CITY is Contra Costa, 400 mi away (geographic sanity check).
	if b := store.Lookup("Brentwood", "city", 34.0522, -118.4740); b != nil {
		t.Errorf("far-away same-name city resolved (%v bbox) — sanity check failed", b.BBox)
	}
	// The real Contra Costa Brentwood CITY, at its own coords, resolves.
	if b := store.Lookup("Brentwood", "city", 37.9319, -121.6957); b == nil || b.Type != "city" {
		t.Errorf("Contra Costa Brentwood city not resolved at its own coordinates")
	}
	// A district picked far from the LA neighborhood is rejected (sanity check).
	if b := store.Lookup("Brentwood", "district", 37.9319, -121.6957); b != nil {
		t.Errorf("LA district resolved for a Contra Costa pick — sanity check failed")
	}
	// County / postal types never resolve.
	if b := store.Lookup("Alameda", "county", 37.6, -122.0); b != nil {
		t.Errorf("county type resolved to %q", b.Name)
	}
	// Unknown names.
	if b := store.Lookup("Nonexistent Placeville", "city", 37, -122); b != nil {
		t.Errorf("unknown city resolved to %q", b.Name)
	}
	if b := store.Lookup("Nonexistent Hood", "district", 34, -118); b != nil {
		t.Errorf("unknown district resolved to %q", b.Name)
	}
}

func TestDistrict_ContainsAndDistance(t *testing.T) {
	store := loadOrFail(t)
	bw := store.Lookup("Brentwood", "district", 34.0663, -118.4703)
	if bw == nil {
		t.Fatal("Brentwood LA district not found")
	}
	// A point on San Vicente in the heart of Brentwood is inside.
	if !bw.Contains(34.0522, -118.4740) {
		t.Errorf("central Brentwood point reported outside the district")
	}
	// Downtown Santa Monica (south-west, well outside Brentwood) is outside and
	// a positive distance past the edge.
	if bw.Contains(34.0195, -118.4912) {
		t.Errorf("Santa Monica point reported inside Brentwood")
	}
	if d := bw.DistanceMeters(34.0195, -118.4912); d <= 0 {
		t.Errorf("Santa Monica distance-to-Brentwood = %.0f, want > 0", d)
	}
}

func TestDistanceMeters(t *testing.T) {
	store := loadOrFail(t)
	b := store.Lookup("Berkeley", "city", 37.8715, -122.2730)
	if b == nil {
		t.Fatal("Berkeley not found")
	}
	// Inside → 0.
	if d := b.DistanceMeters(37.8715, -122.2730); d != 0 {
		t.Errorf("inside distance = %.0f, want 0", d)
	}
	// Downtown Oakland (proven outside Berkeley in TestContains) is a few km
	// south of the border — a positive, finite distance.
	dOak := b.DistanceMeters(37.8044, -122.2712)
	if dOak <= 200 || dOak > 20000 {
		t.Errorf("Oakland distance = %.0f, want a sane positive value", dOak)
	}
	// The SF Ferry Building (across the bay) is farther still.
	if dSF := b.DistanceMeters(37.7955, -122.3937); dSF <= dOak {
		t.Errorf("SF distance %.0f should exceed Oakland %.0f", dSF, dOak)
	}
}

func TestLookup_NameNormalization(t *testing.T) {
	store := loadOrFail(t)
	for _, name := range []string{"san jose", "SAN JOSE", "  San   Jose  "} {
		if b := store.Lookup(name, "city", 37.3379, -121.8863); b == nil {
			t.Errorf("normalized lookup failed for %q", name)
		}
	}
}
