package services

import (
	"log"
	"math"
)

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

// OptimizeRoute optimizes bin order using weighted nearest neighbor TSP
// Prioritizes high fill percentage bins and minimizes total distance
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

	// Weighted nearest neighbor algorithm
	// Combines distance minimization with fill percentage priority
	for len(remaining) > 0 {
		bestIdx := 0
		bestScore := math.MaxFloat64

		for i, bin := range remaining {
			// Calculate straight-line distance (Haversine)
			distance := haversineDistance(
				current.Latitude,
				current.Longitude,
				bin.Latitude,
				bin.Longitude,
			)

			// Weight factor: prioritize high fill percentage
			// Higher fill % = lower score = higher priority
			// fillWeight reduces the score based on how full the bin is
			fillWeight := float64(100-bin.FillPercentage) * 0.01 // 0.0 to 1.0

			// Final score = distance * fillWeight
			// Example: 80% full bin 5km away: 5 * 0.2 = 1.0
			// Example: 70% full bin 3km away: 3 * 0.3 = 0.9 (chosen first)
			score := distance * (1.0 + fillWeight)

			if score < bestScore {
				bestScore = score
				bestIdx = i
			}
		}

		// Add best bin to optimized route
		bestBin := remaining[bestIdx]
		optimized = append(optimized, bestBin)

		log.Printf("   Step %d: Selected bin at %s (%.1f%% full, score: %.2f)",
			len(optimized), bestBin.CurrentStreet, float64(bestBin.FillPercentage), bestScore)

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
