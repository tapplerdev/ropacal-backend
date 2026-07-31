package handlers

import (
	"testing"

	"ropacal-backend/internal/geo"
)

// rankCitiesByDriveTime reaches OSRM, so these cover the behaviour that must
// hold when it CANNOT — which is the path that actually runs during an outage.

func TestRankCitiesByDriveTime_DegradesInsteadOfBreaking(t *testing.T) {
	// Point at a warehouse OSRM cannot possibly route from (mid-Pacific) so the
	// call fails or returns nothing, exercising the fallback.
	in := []geo.NearbyCity{
		{City: geo.City{Name: "Alpha", Lat: 37.6, Lng: -122.1, Population: 50000}},
		{City: geo.City{Name: "Beta", Lat: 37.7, Lng: -122.2, Population: 90000}},
		{City: geo.City{Name: "Gamma", Lat: 37.8, Lng: -122.3, Population: 10000}},
	}
	before := make([]string, len(in))
	for i, c := range in {
		before[i] = c.Name
	}

	rankCitiesByDriveTime(0.0001, 0.0001, in)

	// Whatever happened, the slice must still hold the same three cities: an
	// expansion hint degrading to a worse ORDER is fine, losing candidates is not.
	if len(in) != 3 {
		t.Fatalf("ranking changed the candidate count: %d", len(in))
	}
	seen := map[string]bool{}
	for _, c := range in {
		seen[c.Name] = true
	}
	for _, name := range before {
		if !seen[name] {
			t.Errorf("city %q disappeared during ranking", name)
		}
	}
}

func TestRankCitiesByDriveTime_TrivialInputsAreNoOps(t *testing.T) {
	// Fewer than two cities means nothing to order — must not call OSRM at all.
	rankCitiesByDriveTime(37.6368, -122.1269, nil)
	one := []geo.NearbyCity{{City: geo.City{Name: "Solo", Lat: 37.6, Lng: -122.1}}}
	rankCitiesByDriveTime(37.6368, -122.1269, one)
	if len(one) != 1 || one[0].Name != "Solo" {
		t.Fatal("single-city slice was modified")
	}
}

// Population must no longer decide anything. Measured against this fleet on
// 2026-07-31: Spearman rho = -0.122 (p=0.285) between city population and
// realized bin fill rate — the best bin in the network is in Newark (45k), while
// San Jose at 22x the population fills at roughly half the rate. A test rather
// than a comment, so reintroducing a population sort or floor fails loudly.
func TestExpansion_NoPopulationFloorOrRank(t *testing.T) {
	// geo.CitiesWithin must not filter on population — a 45k city has to survive.
	got, err := geo.CitiesWithin(37.6368013, -122.1269379, 75) // Ropacal warehouse
	if err != nil {
		t.Fatalf("CitiesWithin: %v", err)
	}
	var newark *geo.NearbyCity
	for i := range got {
		if got[i].Name == "Newark" && got[i].Country == "US" {
			newark = &got[i]
			break
		}
	}
	if newark == nil {
		t.Fatal("Newark (45k, home of the best-performing bin) is missing from the candidate universe")
	}
	if newark.Population >= 100000 {
		t.Fatalf("expected Newark to be well under 100k, got %d — check the data", newark.Population)
	}

	// And at least one city smaller than Newark must also survive, proving no
	// floor crept in just above it.
	smaller := 0
	for _, c := range got {
		if c.Population < newark.Population {
			smaller++
		}
	}
	if smaller == 0 {
		t.Error("no city smaller than Newark survived — a population floor appears to be in effect")
	}
}
