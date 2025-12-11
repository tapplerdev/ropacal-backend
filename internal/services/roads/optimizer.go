package roads

import (
	"fmt"
	"log"
	"math"
	"sync"
)

// LocationOptimizer handles position delta filtering and accuracy-based optimization
// Reduces API calls by filtering out unnecessary snap requests
type LocationOptimizer struct {
	lastPositions map[string]*LastPosition // Key: driver_id
	mutex         sync.RWMutex
	stats         OptimizerStats
}

// LastPosition stores the last significant position for a driver
type LastPosition struct {
	Latitude  float64
	Longitude float64
	Timestamp int64
}

// OptimizerStats tracks optimization performance
type OptimizerStats struct {
	TotalRequests       int64
	SkippedByAccuracy   int64
	SkippedByDelta      int64
	ProcessedRequests   int64
	TotalSavings        float64
	mutex               sync.RWMutex
}

// Configuration constants
const (
	// MinPositionDelta is the minimum distance (meters) for a significant position change
	// Points closer than this to the last position are skipped
	// REDUCED from 5m to 1m for smoother real-time tracking (Uber-style)
	MinPositionDelta = 1.0 // 1 meter (very frequent updates for smooth animations)

	// MaxTimeSinceLastBroadcast is the maximum time (seconds) before forcing a broadcast
	// Even if driver hasn't moved MinPositionDelta, send update after this time
	// Prevents stale positions when driver is moving slowly or stopped
	MaxTimeSinceLastBroadcast = 2.0 // 2 seconds

	// AccuracyThreshold is the GPS accuracy threshold (meters)
	// Only snap points with accuracy worse than this
	AccuracyThreshold = 15.0 // 15 meters

	// MaxAccuracy is the maximum acceptable GPS accuracy
	// Points with worse accuracy are rejected entirely
	MaxAccuracy = 100.0 // 100 meters
)

// NewLocationOptimizer creates a new location optimizer
func NewLocationOptimizer() *LocationOptimizer {
	return &LocationOptimizer{
		lastPositions: make(map[string]*LastPosition),
	}
}

// ShouldSnapByAccuracy determines if a point should be snapped based on GPS accuracy
// Returns true if accuracy is poor and snapping would help
func (o *LocationOptimizer) ShouldSnapByAccuracy(accuracy float64) bool {
	o.recordRequest()

	// Reject points with extremely poor accuracy
	if accuracy > MaxAccuracy {
		o.recordSkippedByAccuracy()
		return false
	}

	// Good accuracy - no need to snap
	if accuracy < AccuracyThreshold {
		o.recordSkippedByAccuracy()
		return false
	}

	// Poor accuracy - snapping will help
	return true
}

// ShouldProcessByDelta determines if a new position is significantly different from the last
// This implements position delta storage + time-based fallback for smooth real-time tracking
func (o *LocationOptimizer) ShouldProcessByDelta(driverID string, lat, lng float64, timestamp int64) bool {
	o.mutex.RLock()
	last, exists := o.lastPositions[driverID]
	o.mutex.RUnlock()

	// First position for this driver - always process
	if !exists {
		o.updateLastPosition(driverID, lat, lng, timestamp)
		return true
	}

	// Calculate distance from last significant position
	distance := haversineDistance(last.Latitude, last.Longitude, lat, lng)

	// Calculate time since last broadcast (milliseconds → seconds)
	timeSinceLastBroadcast := float64(timestamp-last.Timestamp) / 1000.0

	// OPTION 1: Significant distance moved (>= 1m)
	if distance >= MinPositionDelta {
		o.updateLastPosition(driverID, lat, lng, timestamp)
		return true
	}

	// OPTION 2: Time-based fallback (> 2 seconds since last broadcast)
	// Ensures managers get updates even when driver is slow/stopped
	// Prevents marker from going stale
	if timeSinceLastBroadcast > MaxTimeSinceLastBroadcast {
		log.Printf("⏱️  Time-based broadcast (%.1fs since last update, distance: %.1fm)",
			timeSinceLastBroadcast, distance)
		o.updateLastPosition(driverID, lat, lng, timestamp)
		return true
	}

	// Neither distance nor time threshold met - skip
	o.recordSkippedByDelta()
	return false
}

// updateLastPosition stores the latest significant position for a driver
func (o *LocationOptimizer) updateLastPosition(driverID string, lat, lng float64, timestamp int64) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	o.lastPositions[driverID] = &LastPosition{
		Latitude:  lat,
		Longitude: lng,
		Timestamp: timestamp,
	}
}

// ClearDriver removes stored position for a driver (call when shift ends)
func (o *LocationOptimizer) ClearDriver(driverID string) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	delete(o.lastPositions, driverID)
}

// haversineDistance calculates the distance between two GPS coordinates in meters
func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000.0 // Earth's radius in meters

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

// Statistics recording methods

func (o *LocationOptimizer) recordRequest() {
	o.stats.mutex.Lock()
	defer o.stats.mutex.Unlock()

	o.stats.TotalRequests++
}

func (o *LocationOptimizer) recordSkippedByAccuracy() {
	o.stats.mutex.Lock()
	defer o.stats.mutex.Unlock()

	o.stats.SkippedByAccuracy++
	// Each skip saves one API call
	o.stats.TotalSavings += 0.005
}

func (o *LocationOptimizer) recordSkippedByDelta() {
	o.stats.mutex.Lock()
	defer o.stats.mutex.Unlock()

	o.stats.SkippedByDelta++
	o.stats.TotalSavings += 0.005
}

func (o *LocationOptimizer) recordProcessed() {
	o.stats.mutex.Lock()
	defer o.stats.mutex.Unlock()

	o.stats.ProcessedRequests++
}

// GetStats returns optimizer statistics
func (o *LocationOptimizer) GetStats() map[string]interface{} {
	o.stats.mutex.RLock()
	defer o.stats.mutex.RUnlock()

	totalSkipped := o.stats.SkippedByAccuracy + o.stats.SkippedByDelta
	skipRate := 0.0
	if o.stats.TotalRequests > 0 {
		skipRate = float64(totalSkipped) / float64(o.stats.TotalRequests) * 100
	}

	return map[string]interface{}{
		"total_requests":        o.stats.TotalRequests,
		"skipped_by_accuracy":   o.stats.SkippedByAccuracy,
		"skipped_by_delta":      o.stats.SkippedByDelta,
		"processed_requests":    o.stats.ProcessedRequests,
		"skip_rate":             fmt.Sprintf("%.2f%%", skipRate),
		"total_savings":         fmt.Sprintf("$%.2f", o.stats.TotalSavings),
		"min_position_delta_m":  MinPositionDelta,
		"accuracy_threshold_m":  AccuracyThreshold,
	}
}
