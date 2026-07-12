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

	// A district must never resolve to a same-named city. "Brentwood" the LA
	// district (picked at LA coords) must NOT return the Contra Costa city.
	if b := store.Lookup("Brentwood", "district", 34.0522, -118.4740); b != nil {
		t.Errorf("district type resolved to city %q — guard failed", b.Name)
	}
	// Same name, but typed as a city AND picked in LA — the geographic sanity
	// check must still reject the far-away Contra Costa Brentwood.
	if b := store.Lookup("Brentwood", "city", 34.0522, -118.4740); b != nil {
		t.Errorf("far-away same-name city resolved (%v bbox) — sanity check failed", b.BBox)
	}
	// The real Contra Costa Brentwood, picked at its own coords, resolves.
	if b := store.Lookup("Brentwood", "city", 37.9319, -121.6957); b == nil {
		t.Errorf("Contra Costa Brentwood not resolved at its own coordinates")
	}
	// County / postal types are never cities.
	if b := store.Lookup("Alameda", "county", 37.6, -122.0); b != nil {
		t.Errorf("county type resolved to city %q", b.Name)
	}
	// Unknown name.
	if b := store.Lookup("Nonexistent Placeville", "city", 37, -122); b != nil {
		t.Errorf("unknown city resolved to %q", b.Name)
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
