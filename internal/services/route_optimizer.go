package services

import (
	"log"
	"math"
)

// Warehouse constants - all routes end here
const (
	WAREHOUSE_LAT     = 37.34692
	WAREHOUSE_LNG     = -121.92984
	WAREHOUSE_ADDRESS = "1185 Campbell Ave, San Jose, CA 95126"
)

// GetWarehouseLocation returns the default warehouse location
func GetWarehouseLocation() Location {
	return Location{
		Latitude:  WAREHOUSE_LAT,
		Longitude: WAREHOUSE_LNG,
	}
}

// Location represents a geographic point
type Location struct {
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
	startLocation Location,
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
			distance := haversineDistance(
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
		current = Location{
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
		distance := haversineDistance(
			routePoint.Latitude,
			routePoint.Longitude,
			bin.Latitude,
			bin.Longitude,
		)
		totalDistance += distance
		routePoint = Location{
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

// haversineDistance calculates the distance between two GPS coordinates in kilometers
func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371.0 // Earth's radius in kilometers

	// Convert to radians
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180

	// Haversine formula
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}
