package roads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// RoadsClient handles Google Roads API requests with full optimization
type RoadsClient struct {
	apiKey     string
	httpClient *http.Client
	cache      *RouteCache
	optimizer  *LocationOptimizer
}

// LatLng represents a geographic coordinate
type LatLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// SnappedPoint represents a point snapped to the road network
type SnappedPoint struct {
	Location      LatLng `json:"location"`
	OriginalIndex *int   `json:"originalIndex,omitempty"`
	PlaceID       string `json:"placeId,omitempty"`
}

// SnapToRoadsResponse represents the Google Roads API response
type SnapToRoadsResponse struct {
	SnappedPoints []SnappedPoint `json:"snappedPoints"`
}

// NewRoadsClient creates a new Google Roads API client with all optimizations
func NewRoadsClient() *RoadsClient {
	apiKey := os.Getenv("GOOGLE_MAPS_API_KEY")
	if apiKey == "" {
		log.Printf("⚠️  GOOGLE_MAPS_API_KEY not set - snap-to-roads disabled")
	}

	return &RoadsClient{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cache:     NewRouteCache(),
		optimizer: NewLocationOptimizer(),
	}
}

// SnapToRoad snaps a single GPS coordinate to the nearest road
// Returns original coordinates if API fails or optimization filters it out
func (c *RoadsClient) SnapToRoad(lat, lng, accuracy float64) (float64, float64, error) {
	if c.apiKey == "" {
		// API key not configured - return original coordinates
		return lat, lng, nil
	}

	// OPTIMIZATION 1: Accuracy-based filtering
	// Only snap if GPS accuracy is poor (> 15 meters)
	if !c.optimizer.ShouldSnapByAccuracy(accuracy) {
		log.Printf("✅ GPS accuracy good (%.1fm) - skipping snap-to-roads", accuracy)
		return lat, lng, nil
	}

	// Snap single point
	points := []LatLng{{Latitude: lat, Longitude: lng}}
	snapped, err := c.snapPoints(points, false)

	if err != nil || len(snapped) == 0 {
		log.Printf("⚠️  Snap-to-roads failed: %v - using original coordinates", err)
		return lat, lng, err
	}

	log.Printf("🛣️  Snapped GPS: (%.6f, %.6f) → (%.6f, %.6f)",
		lat, lng, snapped[0].Latitude, snapped[0].Longitude)

	return snapped[0].Latitude, snapped[0].Longitude, nil
}

// SnapBatch snaps multiple GPS points with batching optimization
// Returns snapped points if successful, original points if failed
func (c *RoadsClient) SnapBatch(points []LatLng) ([]LatLng, error) {
	if c.apiKey == "" || len(points) == 0 {
		return points, nil
	}

	// OPTIMIZATION 2: Check cache for route signature
	signature := c.cache.GenerateRouteSignature(points)
	if cached, found := c.cache.Get(signature); found {
		log.Printf("📦 Cache HIT for route signature: %s", signature)
		return cached, nil
	}

	// OPTIMIZATION 3: Batch processing (up to 100 points per request)
	batchSize := 100
	var allSnapped []LatLng

	for i := 0; i < len(points); i += batchSize {
		end := i + batchSize
		if end > len(points) {
			end = len(points)
		}

		batch := points[i:end]
		snapped, err := c.snapPoints(batch, true)

		if err != nil {
			log.Printf("⚠️  Batch snap failed for points %d-%d: %v", i, end, err)
			// Return original points on failure
			return points, err
		}

		allSnapped = append(allSnapped, snapped...)
	}

	// OPTIMIZATION 4: Cache the snapped route
	c.cache.Set(signature, allSnapped)
	log.Printf("💾 Cached route signature: %s (%d points)", signature, len(allSnapped))

	return allSnapped, nil
}

// snapPoints makes the actual API call to Google Roads API
func (c *RoadsClient) snapPoints(points []LatLng, interpolate bool) ([]LatLng, error) {
	if len(points) == 0 {
		return []LatLng{}, nil
	}

	// Build path parameter
	var pathBuilder bytes.Buffer
	for i, p := range points {
		if i > 0 {
			pathBuilder.WriteString("|")
		}
		pathBuilder.WriteString(fmt.Sprintf("%.6f,%.6f", p.Latitude, p.Longitude))
	}

	// Build URL
	url := fmt.Sprintf(
		"https://roads.googleapis.com/v1/snapToRoads?path=%s&interpolate=%t&key=%s",
		pathBuilder.String(),
		interpolate,
		c.apiKey,
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
	var apiResp SnapToRoadsResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Extract snapped coordinates
	snapped := make([]LatLng, len(apiResp.SnappedPoints))
	for i, sp := range apiResp.SnappedPoints {
		snapped[i] = sp.Location
	}

	log.Printf("📍 Snapped %d points via Google Roads API", len(snapped))
	return snapped, nil
}

// GetCacheStats returns cache statistics for monitoring
func (c *RoadsClient) GetCacheStats() map[string]interface{} {
	return c.cache.GetStats()
}

// GetOptimizerStats returns optimizer statistics for monitoring
func (c *RoadsClient) GetOptimizerStats() map[string]interface{} {
	return c.optimizer.GetStats()
}
