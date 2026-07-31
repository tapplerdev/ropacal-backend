package geo

import (
	"strings"
	"testing"
)

// The two warehouses this feature exists to serve.
const (
	hqLat, hqLng           = 37.6368013, -122.1269379 // Ropacal, Hayward CA
	torontoLat, torontoLng = 43.6532, -79.3832        // a GTA tenant
)

func TestCityAssetLoads(t *testing.T) {
	n, err := CityCount()
	if err != nil {
		t.Fatalf("embedded cities.json failed to parse: %v", err)
	}
	// Guards against a truncated or half-written asset, which would otherwise
	// surface as quietly empty recommendations rather than an error.
	if n < 40000 {
		t.Fatalf("only %d cities embedded — asset looks truncated (expect ~64k)", n)
	}
}

// The bug this whole change exists to fix: a Toronto tenant being told to
// expand into Fremont and Hayward.
func TestCitiesWithin_TorontoGetsGTANotBayArea(t *testing.T) {
	got, err := CitiesWithin(torontoLat, torontoLng, 75)
	if err != nil {
		t.Fatalf("CitiesWithin: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no cities within 75 mi of Toronto")
	}

	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	for _, want := range []string{"Mississauga", "Brampton", "Markham", "Vaughan", "Oakville"} {
		if !names[want] {
			t.Errorf("expected %q within 75 mi of Toronto, missing", want)
		}
	}
	for _, unwanted := range []string{"Fremont", "Hayward", "Oakland", "Berkeley"} {
		if names[unwanted] {
			t.Errorf("Bay Area city %q returned for a Toronto origin", unwanted)
		}
	}
	// Sorted by population, largest first.
	for i := 1; i < len(got); i++ {
		if got[i-1].Population < got[i].Population {
			t.Fatalf("not sorted by population at %d: %d < %d",
				i, got[i-1].Population, got[i].Population)
		}
	}
}

// The existing Bay Area behaviour must survive the switch from the hardcoded
// list to derived data — same cities, just no longer typed by hand.
func TestCitiesWithin_BayAreaStillFindsTheOldHardcodedSet(t *testing.T) {
	got, err := CitiesWithin(hqLat, hqLng, 75)
	if err != nil {
		t.Fatalf("CitiesWithin: %v", err)
	}
	names := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
	}
	// The eight that used to be hardcoded in chat_locations.go.
	for _, want := range []string{
		"Fremont", "Hayward", "Oakland", "Berkeley",
		"Union City", "Newark", "Milpitas", "San Leandro",
	} {
		if !names[want] {
			t.Errorf("previously-hardcoded city %q not found within 75 mi of the warehouse", want)
		}
	}
	if names["Toronto"] || names["Mississauga"] {
		t.Error("a GTA city was returned for a Bay Area origin")
	}
}

func TestCitiesWithin_RadiusIsHonoured(t *testing.T) {
	for _, r := range []float64{5, 25, 75} {
		got, err := CitiesWithin(hqLat, hqLng, r)
		if err != nil {
			t.Fatalf("radius %.0f: %v", r, err)
		}
		for _, c := range got {
			if c.DistanceMiles > r {
				t.Errorf("radius %.0f returned %s at %.1f mi", r, c.Name, c.DistanceMiles)
			}
		}
	}
	small, _ := CitiesWithin(hqLat, hqLng, 5)
	big, _ := CitiesWithin(hqLat, hqLng, 75)
	if len(small) >= len(big) {
		t.Errorf("5 mi returned %d cities, 75 mi returned %d — radius has no effect", len(small), len(big))
	}
}

// Provisioning seeds new organizations with a 0,0 warehouse. A radius around
// that point lands in the Gulf of Guinea and would return West African towns as
// "expansion candidates" for a Californian hauler. It must fail loudly, because
// an empty list is indistinguishable from "nothing nearby".
func TestCitiesWithin_RefusesUnsetWarehouse(t *testing.T) {
	_, err := CitiesWithin(0, 0, 75)
	if err == nil {
		t.Fatal("0,0 origin was accepted — an unset warehouse must be an error, not a silent empty result")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "warehouse") {
		t.Errorf("error should name the cause, got %q", err)
	}
}

func TestCitiesWithin_RejectsNonsenseInput(t *testing.T) {
	if _, err := CitiesWithin(91, 0, 10); err == nil {
		t.Error("latitude 91 accepted")
	}
	if _, err := CitiesWithin(hqLat, hqLng, 0); err == nil {
		t.Error("zero radius accepted")
	}
	if _, err := CitiesWithin(hqLat, hqLng, -5); err == nil {
		t.Error("negative radius accepted")
	}
}

func TestExcludeCities_IgnoresPunctuationAndCase(t *testing.T) {
	in := []NearbyCity{
		{City: City{Name: "Hayward"}},
		{City: City{Name: "San Leandro"}},
		{City: City{Name: "Fremont"}},
	}
	// Covered names come from human- and geocoder-written bin address fields,
	// so casing and punctuation vary.
	got := ExcludeCities(in, []string{"  hayward ", "SAN-LEANDRO"})
	if len(got) != 1 || got[0].Name != "Fremont" {
		names := make([]string, len(got))
		for i, c := range got {
			names[i] = c.Name
		}
		t.Fatalf("expected only Fremont to survive, got %v", names)
	}
}

func TestExcludeCities_EmptyCoveredIsPassthrough(t *testing.T) {
	in := []NearbyCity{{City: City{Name: "Hayward"}}}
	if got := ExcludeCities(in, nil); len(got) != 1 {
		t.Fatalf("nil covered list dropped candidates: %d", len(got))
	}
}
