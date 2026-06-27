// Package geo holds small geospatial helpers shared across the backend.
package geo

import "math"

const (
	earthRadiusMeters = 6371000.0
	metersPerMile     = 1609.34
)

// HaversineMeters returns the great-circle distance in meters between two
// (latitude, longitude) points given in decimal degrees.
func HaversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusMeters * c
}

// HaversineKm returns the great-circle distance in kilometers.
func HaversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	return HaversineMeters(lat1, lon1, lat2, lon2) / 1000.0
}

// HaversineMiles returns the great-circle distance in miles.
func HaversineMiles(lat1, lon1, lat2, lon2 float64) float64 {
	return HaversineMeters(lat1, lon1, lat2, lon2) / metersPerMile
}
