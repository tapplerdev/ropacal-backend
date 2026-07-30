package roads

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// GPS_RADIUS_MULTIPLIER defines how much to multiply GPS accuracy for OSRM radius
	// Based on Lyft's research: 5x-10x provides best matching results
	// Lower values = faster but more restrictive matching
	// Higher values = slower but better matches for noisy GPS
	GPS_RADIUS_MULTIPLIER = 5.0
)

// OSRMClient handles OSRM Map Matching API requests
type OSRMClient struct {
	baseURL    string
	httpClient *http.Client
	cache      *RouteCache
	Optimizer  *LocationOptimizer
}

// OSRMMatchResponse represents the OSRM Match API response
type OSRMMatchResponse struct {
	Code        string           `json:"code"`
	Message     string           `json:"message,omitempty"`
	Matchings   []OSRMMatching   `json:"matchings"`
	Tracepoints []OSRMTracepoint `json:"tracepoints"`
}

// OSRMMatching represents a matched route segment
type OSRMMatching struct {
	Confidence float64      `json:"confidence"` // 0-1, how confident the match is
	Geometry   OSRMGeometry `json:"geometry"`
	Legs       []OSRMLeg    `json:"legs"`
	Distance   float64      `json:"distance"` // meters
	Duration   float64      `json:"duration"` // seconds
}

// OSRMGeometry represents the matched route geometry
type OSRMGeometry struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"` // [lng, lat] pairs
}

// OSRMLeg represents a route segment between waypoints
type OSRMLeg struct {
	Distance float64 `json:"distance"` // meters
	Duration float64 `json:"duration"` // seconds
	Summary  string  `json:"summary"`
}

// OSRMTracepoint represents a GPS point matched to the road network
type OSRMTracepoint struct {
	Location          []float64 `json:"location"`           // [lng, lat]
	Name              string    `json:"name"`               // Road name
	Distance          float64   `json:"distance"`           // Distance from original GPS point (meters)
	MatchingsIndex    int       `json:"matchings_index"`    // Which matching this belongs to
	WaypointIndex     int       `json:"waypoint_index"`     // Position in the route
	AlternativesCount int       `json:"alternatives_count"` // Number of alternative matches
}

// NewOSRMClient creates a new OSRM Map Matching client
func NewOSRMClient() *OSRMClient {
	// Try environment variable, fallback to public demo server
	baseURL := os.Getenv("OSRM_SERVER_URL")
	if baseURL == "" {
		baseURL = "http://router.project-osrm.org" // Public demo server
		log.Printf("⚠️  OSRM_SERVER_URL not set - using public demo server (not for production!)")
	}

	return &OSRMClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cache:     NewRouteCache(),
		Optimizer: NewLocationOptimizer(),
	}
}

// SnapToRoad snaps a single GPS coordinate to the nearest road using OSRM
// Returns original coordinates if API fails or optimization filters it out
func (c *OSRMClient) SnapToRoad(lat, lng, accuracy float64) (float64, float64, error) {
	// OPTIMIZATION 1: Accuracy-based filtering
	// Only snap if GPS accuracy is poor (> 15 meters)
	if !c.Optimizer.ShouldSnapByAccuracy(accuracy) {
		log.Printf("✅ GPS accuracy good (%.1fm) - skipping snap-to-roads", accuracy)
		return lat, lng, nil
	}

	// Snap single point using OSRM Match API
	points := []LatLng{{Latitude: lat, Longitude: lng}}
	// Multiply GPS accuracy by 5x (Lyft best practice for better matching)
	radiuses := []float64{accuracy * GPS_RADIUS_MULTIPLIER}
	timestamps := []int64{time.Now().Unix()}

	snapped, err := c.matchPoints(points, radiuses, timestamps)
	if err != nil || len(snapped) == 0 {
		log.Printf("⚠️  OSRM snap-to-roads failed: %v - using original coordinates", err)
		return lat, lng, err
	}

	log.Printf("🛣️  [OSRM] Snapped GPS: (%.6f, %.6f) → (%.6f, %.6f)",
		lat, lng, snapped[0].Latitude, snapped[0].Longitude)

	return snapped[0].Latitude, snapped[0].Longitude, nil
}

// SnapBatch snaps multiple GPS points with batching optimization
// Returns snapped points if successful, original points if failed
func (c *OSRMClient) SnapBatch(points []LatLng) ([]LatLng, error) {
	if len(points) == 0 {
		return points, nil
	}

	// OPTIMIZATION 2: Check cache for route signature
	signature := c.cache.GenerateRouteSignature(points)
	if cached, found := c.cache.Get(signature); found {
		log.Printf("📦 [OSRM] Cache HIT for route signature: %s", signature)
		return cached, nil
	}

	// Default radiuses (assume good GPS accuracy of ~10m, multiply by 5x)
	radiuses := make([]float64, len(points))
	for i := range radiuses {
		radiuses[i] = 10.0 * GPS_RADIUS_MULTIPLIER // 50m radius (10m * 5x)
	}

	// Timestamps (assume 5 seconds apart)
	timestamps := make([]int64, len(points))
	baseTime := time.Now().Unix()
	for i := range timestamps {
		timestamps[i] = baseTime + int64(i*5)
	}

	// Match points using OSRM
	snapped, err := c.matchPoints(points, radiuses, timestamps)
	if err != nil {
		log.Printf("⚠️  [OSRM] Batch snap failed: %v", err)
		return points, err
	}

	// OPTIMIZATION 3: Cache the snapped route
	c.cache.Set(signature, snapped)
	log.Printf("💾 [OSRM] Cached route signature: %s (%d points)", signature, len(snapped))

	return snapped, nil
}

// matchPoints makes the actual API call to OSRM Match service
func (c *OSRMClient) matchPoints(points []LatLng, radiuses []float64, timestamps []int64) ([]LatLng, error) {
	if len(points) == 0 {
		return []LatLng{}, nil
	}

	// Build coordinates parameter (lng,lat;lng,lat;...)
	coords := make([]string, len(points))
	for i, p := range points {
		coords[i] = fmt.Sprintf("%.6f,%.6f", p.Longitude, p.Latitude)
	}
	coordsParam := strings.Join(coords, ";")

	// Build radiuses parameter
	radiusesParam := make([]string, len(radiuses))
	for i, r := range radiuses {
		radiusesParam[i] = fmt.Sprintf("%.0f", r)
	}
	radiusesStr := strings.Join(radiusesParam, ";")

	// Build timestamps parameter
	timestampsParam := make([]string, len(timestamps))
	for i, t := range timestamps {
		timestampsParam[i] = fmt.Sprintf("%d", t)
	}
	timestampsStr := strings.Join(timestampsParam, ";")

	// Build URL
	// tidy=true enables OSRM's built-in noise reduction for GPS traces
	url := fmt.Sprintf(
		"%s/match/v1/driving/%s?radiuses=%s&timestamps=%s&tidy=true&overview=full&geometries=geojson&annotations=true",
		c.baseURL,
		coordsParam,
		radiusesStr,
		timestampsStr,
	)

	// Make API request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var apiResp OSRMMatchResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Check for errors
	if apiResp.Code != "Ok" {
		return nil, fmt.Errorf("OSRM error: %s - %s", apiResp.Code, apiResp.Message)
	}

	// Extract snapped coordinates from tracepoints
	snapped := make([]LatLng, 0, len(apiResp.Tracepoints))
	for _, tp := range apiResp.Tracepoints {
		if len(tp.Location) == 2 {
			snapped = append(snapped, LatLng{
				Latitude:  tp.Location[1], // OSRM returns [lng, lat]
				Longitude: tp.Location[0],
			})
		}
	}

	if len(apiResp.Matchings) > 0 {
		confidence := apiResp.Matchings[0].Confidence
		log.Printf("📍 [OSRM] Matched %d points (confidence: %.2f)", len(snapped), confidence)
	} else {
		log.Printf("📍 [OSRM] Matched %d points", len(snapped))
	}

	return snapped, nil
}

// GetCacheStats returns cache statistics for monitoring
func (c *OSRMClient) GetCacheStats() map[string]interface{} {
	return c.cache.GetStats()
}

// GetOptimizerStats returns optimizer statistics for monitoring
func (c *OSRMClient) GetOptimizerStats() map[string]interface{} {
	return c.Optimizer.GetStats()
}
