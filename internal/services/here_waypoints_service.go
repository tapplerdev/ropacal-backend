package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// HEREWaypoint represents a waypoint for HERE Maps optimization
type HEREWaypoint struct {
	ID        string
	Name      string
	Latitude  float64
	Longitude float64
}

// HEREOptimizationResult contains the optimization results from HERE Maps
type HEREOptimizationResult struct {
	OptimizedOrder       []string // Ordered list of waypoint IDs
	TotalDistanceKm      float64
	TotalDurationSeconds int
}

// HEREWaypointsService handles HERE Maps Waypoints Sequence API v8
type HEREWaypointsService struct {
	apiKey string
}

// NewHEREWaypointsService creates a new HERE waypoints service
func NewHEREWaypointsService(apiKey string) *HEREWaypointsService {
	return &HEREWaypointsService{
		apiKey: apiKey,
	}
}

// OptimizeWaypoints calls HERE Maps Waypoints Sequence API v8 to optimize the route
func (s *HEREWaypointsService) OptimizeWaypoints(
	startLat, startLng float64,
	endLat, endLng float64,
	waypoints []HEREWaypoint,
	departureTime string, // ISO 8601 format
) (*HEREOptimizationResult, error) {
	log.Printf("🗺️  [HERE Maps] Optimizing route with %d waypoints", len(waypoints))
	log.Printf("   Start: (%.6f, %.6f)", startLat, startLng)
	log.Printf("   End: (%.6f, %.6f)", endLat, endLng)
	log.Printf("   Departure: %s", departureTime)

	// Build HERE Waypoints Sequence API v8 URL
	baseURL := "https://wps.hereapi.com/v8/findsequence2"
	params := url.Values{}
	params.Add("apiKey", s.apiKey)

	// Set mode with traffic enabled
	params.Add("mode", "fastest;car;traffic:enabled")
	params.Add("improveFor", "time")

	// Add departure time for real-time traffic
	params.Add("departure", departureTime)

	// Add start point (driver's current location)
	params.Add("start", fmt.Sprintf("start-driver;%.6f,%.6f", startLat, startLng))

	// Add all waypoints as destinations
	for i, wp := range waypoints {
		destinationID := fmt.Sprintf("dest-%d-%s", i, wp.ID[:8]) // Use first 8 chars of ID
		params.Add(fmt.Sprintf("destination%d", i+1), fmt.Sprintf("%s;%.6f,%.6f", destinationID, wp.Latitude, wp.Longitude))
	}

	// Add end point (warehouse)
	params.Add("end", fmt.Sprintf("end-warehouse;%.6f,%.6f", endLat, endLng))

	// Make HTTP request
	fullURL := baseURL + "?" + params.Encode()
	log.Printf("   📡 Calling HERE Maps API...")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("   ❌ HERE Maps API error (%d): %s", resp.StatusCode, string(body))
		return nil, fmt.Errorf("HERE Maps API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var hereResp struct {
		Results []struct {
			Waypoints []struct {
				ID               string `json:"id"`
				Lat              string `json:"lat"`
				Lng              string `json:"lng"`
				Sequence         int    `json:"sequence"`
				EstimatedArrival string `json:"estimatedArrival,omitempty"`
			} `json:"waypoints"`
			Distance         string `json:"distance"` // Total distance in meters (string)
			Time             string `json:"time"`     // Total time in seconds (string)
			Interconnections []struct {
				FromWaypoint string  `json:"fromWaypoint"`
				ToWaypoint   string  `json:"toWaypoint"`
				Distance     float64 `json:"distance"` // meters
				Time         float64 `json:"time"`     // seconds
			} `json:"interconnections"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &hereResp); err != nil {
		log.Printf("   ❌ Failed to parse HERE response: %v", err)
		log.Printf("   Response body: %s", string(body))
		return nil, fmt.Errorf("failed to parse HERE response: %w", err)
	}

	if len(hereResp.Results) == 0 {
		return nil, fmt.Errorf("HERE Maps returned no results")
	}

	result := hereResp.Results[0]

	// Build a map of destination ID to original waypoint ID
	destinationToWaypointID := make(map[string]string)
	for i, wp := range waypoints {
		destinationID := fmt.Sprintf("dest-%d-%s", i, wp.ID[:8])
		destinationToWaypointID[destinationID] = wp.ID
	}

	// Extract optimized order (excluding start and end)
	optimizedOrder := make([]string, 0, len(waypoints))
	for _, wp := range result.Waypoints {
		// Skip start and end waypoints
		if wp.ID == "start-driver" || wp.ID == "end-warehouse" {
			continue
		}

		// Map destination ID back to original waypoint ID
		originalID, ok := destinationToWaypointID[wp.ID]
		if ok {
			optimizedOrder = append(optimizedOrder, originalID)
		} else {
			log.Printf("   ⚠️  Warning: Could not map waypoint ID %s", wp.ID)
		}
	}

	// Parse distance (meters to km)
	var totalDistanceMeters float64
	fmt.Sscanf(result.Distance, "%f", &totalDistanceMeters)
	totalDistanceKm := totalDistanceMeters / 1000.0

	// Parse time (seconds)
	var totalDurationSeconds float64
	fmt.Sscanf(result.Time, "%f", &totalDurationSeconds)

	log.Printf("   ✅ HERE Maps optimization successful!")
	log.Printf("      Total Distance: %.2f km", totalDistanceKm)
	log.Printf("      Total Duration: %.0f seconds (%.1f minutes)", totalDurationSeconds, totalDurationSeconds/60.0)
	log.Printf("      Optimized Order: %v", optimizedOrder)

	return &HEREOptimizationResult{
		OptimizedOrder:       optimizedOrder,
		TotalDistanceKm:      totalDistanceKm,
		TotalDurationSeconds: int(totalDurationSeconds),
	}, nil
}
