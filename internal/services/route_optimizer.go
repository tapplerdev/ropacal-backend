package services

import (
	"encoding/json"
	"log"
	"math"

	"ropacal-backend/internal/geo"
)

// RETIRED: Warehouse constants - now fetched from database config table
// Keeping for reference and fallback purposes
// const (
// 	WAREHOUSE_LAT     = 37.34692
// 	WAREHOUSE_LNG     = -121.92984
// 	WAREHOUSE_ADDRESS = "1185 Campbell Ave, San Jose, CA 95126"
// )

// GetWarehouseLocation fetches the warehouse location from the config table.
// ok=false when the row is missing, unreadable, unparseable, or null-island —
// callers MUST fail loudly and must not optimize.
//
// There is deliberately NO hardcoded fallback anymore. Under row-level
// security a missing config row means THIS TENANT has no warehouse
// configured; silently anchoring their routes to somebody else's depot
// coordinates is the worst failure mode this system has (routes still
// generate — they are just geometrically wrong, and nothing errors).
// A 4xx/5xx on a misconfigured tenant is strictly better.
func GetWarehouseLocation(db Querier) (OptimizerLocation, bool) {
	var configValue []byte
	err := db.Get(&configValue, `
		SELECT value
		FROM config
		WHERE key = 'warehouse_location'
	`)

	if err != nil {
		log.Printf("❌ Warehouse location unavailable from config (NO fallback — configure the warehouse): %v", err)
		return OptimizerLocation{}, false
	}

	var warehouse struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Address   string  `json:"address"`
	}

	if err := json.Unmarshal(configValue, &warehouse); err != nil {
		log.Printf("❌ Warehouse location config is unparseable (NO fallback): %v", err)
		return OptimizerLocation{}, false
	}
	if warehouse.Latitude == 0 && warehouse.Longitude == 0 {
		log.Printf("❌ Warehouse location config is null-island 0,0 — treating as unset (NO fallback)")
		return OptimizerLocation{}, false
	}

	return OptimizerLocation{
		Latitude:  warehouse.Latitude,
		Longitude: warehouse.Longitude,
	}, true
}

// OptimizerLocation represents a geographic point for route optimization
type OptimizerLocation struct {
	Latitude  float64
	Longitude float64
}

// BinWithPriority represents a bin with its location and fill percentage
type BinWithPriority struct {
	ID             string
	Latitude       float64
	Longitude      float64
	FillPercentage int
	CurrentStreet  string
}

// RouteOptimizer handles route optimization using TSP algorithms
type RouteOptimizer struct{}

// NewRouteOptimizer creates a new route optimizer
func NewRouteOptimizer() *RouteOptimizer {
	return &RouteOptimizer{}
}

// OptimizeRoute optimizes bin order using nearest neighbor TSP
// Minimizes total distance by always selecting the closest remaining bin
func (ro *RouteOptimizer) OptimizeRoute(
	bins []BinWithPriority,
	startLocation OptimizerLocation,
) []BinWithPriority {
	if len(bins) == 0 {
		return bins
	}

	if len(bins) == 1 {
		return bins
	}

	log.Printf("🎯 Starting route optimization from (%.6f, %.6f)",
		startLocation.Latitude, startLocation.Longitude)
	log.Printf("   Total bins to optimize: %d", len(bins))

	optimized := make([]BinWithPriority, 0, len(bins))
	remaining := make([]BinWithPriority, len(bins))
	copy(remaining, bins)

	current := startLocation

	// Nearest neighbor algorithm - pure distance-based TSP
	// Always selects the closest remaining bin from current location
	for len(remaining) > 0 {
		bestIdx := 0
		bestDistance := math.MaxFloat64

		for i, bin := range remaining {
			// Calculate straight-line distance (Haversine)
			distance := geo.HaversineKm(
				current.Latitude,
				current.Longitude,
				bin.Latitude,
				bin.Longitude,
			)

			// Select the nearest bin (shortest distance)
			if distance < bestDistance {
				bestDistance = distance
				bestIdx = i
			}
		}

		// Add best bin to optimized route
		bestBin := remaining[bestIdx]
		optimized = append(optimized, bestBin)

		log.Printf("   Step %d: Selected bin at %s (%.1f%% full, distance: %.2f km)",
			len(optimized), bestBin.CurrentStreet, float64(bestBin.FillPercentage), bestDistance)

		// Update current location to the bin we just added
		current = OptimizerLocation{
			Latitude:  bestBin.Latitude,
			Longitude: bestBin.Longitude,
		}

		// Remove selected bin from remaining
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}

	// Calculate total distance of optimized route
	totalDistance := 0.0
	routePoint := startLocation
	for _, bin := range optimized {
		distance := geo.HaversineKm(
			routePoint.Latitude,
			routePoint.Longitude,
			bin.Latitude,
			bin.Longitude,
		)
		totalDistance += distance
		routePoint = OptimizerLocation{
			Latitude:  bin.Latitude,
			Longitude: bin.Longitude,
		}
	}

	log.Printf("✅ Route optimization complete!")
	log.Printf("   Total distance: %.2f km", totalDistance)
	log.Printf("   Optimized order:")
	for i, bin := range optimized {
		log.Printf("      %d. %s (%d%% full)", i+1, bin.CurrentStreet, bin.FillPercentage)
	}

	return optimized
}
