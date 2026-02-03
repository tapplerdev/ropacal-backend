package roads

// RoadSnapper defines the interface for road snapping services
// This allows easy switching between Google Roads API and OSRM
type RoadSnapper interface {
	// SnapToRoad snaps a single GPS coordinate to the nearest road
	// Returns snapped (lat, lng) and error if any
	SnapToRoad(lat, lng, accuracy float64) (float64, float64, error)

	// SnapBatch snaps multiple GPS points efficiently with batching
	// Returns snapped points or original points if failed
	SnapBatch(points []LatLng) ([]LatLng, error)

	// GetCacheStats returns cache performance statistics
	GetCacheStats() map[string]interface{}

	// GetOptimizerStats returns optimization statistics
	GetOptimizerStats() map[string]interface{}
}

// Ensure both implementations satisfy the interface
var (
	_ RoadSnapper = (*RoadsClient)(nil)
	_ RoadSnapper = (*OSRMClient)(nil)
)
