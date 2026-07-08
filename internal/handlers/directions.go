package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// OSRMRouteResponse represents the OSRM Route API response
type OSRMRouteResponse struct {
	Code   string      `json:"code"`
	Routes []OSRMRoute `json:"routes"`
}

// OSRMRoute represents a single route from OSRM
type OSRMRoute struct {
	Geometry OSRMRouteGeometry `json:"geometry"`
	Distance float64           `json:"distance"` // meters
	Duration float64           `json:"duration"` // seconds
}

// OSRMRouteGeometry represents the route geometry in GeoJSON
type OSRMRouteGeometry struct {
	Type        string      `json:"type"`
	Coordinates [][]float64 `json:"coordinates"` // [lng, lat] pairs
}

// DirectionsResponse is the response sent to the client
type DirectionsResponse struct {
	Success     bool              `json:"success"`
	Coordinates []DirectionsPoint `json:"coordinates"`
	Distance    float64           `json:"distance_meters"`
	Duration    float64           `json:"duration_seconds"`
}

// DirectionsPoint represents a single lat/lng point
type DirectionsPoint struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// GetDirections returns a road-following polyline between two points using OSRM Route API
// GET /api/directions?origin_lat=X&origin_lng=Y&dest_lat=X&dest_lng=Y
func GetDirections() http.HandlerFunc {
	client := &http.Client{Timeout: 10 * time.Second}

	return func(w http.ResponseWriter, r *http.Request) {
		// Parse query parameters
		originLat, err := strconv.ParseFloat(r.URL.Query().Get("origin_lat"), 64)
		if err != nil {
			http.Error(w, "Invalid origin_lat", http.StatusBadRequest)
			return
		}
		originLng, err := strconv.ParseFloat(r.URL.Query().Get("origin_lng"), 64)
		if err != nil {
			http.Error(w, "Invalid origin_lng", http.StatusBadRequest)
			return
		}
		destLat, err := strconv.ParseFloat(r.URL.Query().Get("dest_lat"), 64)
		if err != nil {
			http.Error(w, "Invalid dest_lat", http.StatusBadRequest)
			return
		}
		destLng, err := strconv.ParseFloat(r.URL.Query().Get("dest_lng"), 64)
		if err != nil {
			http.Error(w, "Invalid dest_lng", http.StatusBadRequest)
			return
		}

		// Build OSRM Route API URL. Honor OSRM_SERVER_URL like every other
		// OSRM consumer (roads client, OR-Tools matrix, route optimize) — this
		// endpoint builds the manager map's guide polylines, so being pinned to
		// the public demo server meant its rate-limit failures degraded guides
		// to straight chords through buildings. Falls back to the demo server
		// only when the env var is unset.
		// Note: OSRM uses lng,lat order (GeoJSON standard).
		osrmBase := os.Getenv("OSRM_SERVER_URL")
		if osrmBase == "" {
			osrmBase = "http://router.project-osrm.org"
		}
		osrmBase = strings.TrimRight(osrmBase, "/")
		osrmURL := fmt.Sprintf(
			"%s/route/v1/driving/%.6f,%.6f;%.6f,%.6f?overview=full&geometries=geojson",
			osrmBase, originLng, originLat, destLng, destLat,
		)

		log.Printf("🛣️  [Directions] OSRM request: (%.4f,%.4f) → (%.4f,%.4f)", originLat, originLng, destLat, destLng)

		// Make OSRM request
		resp, err := client.Get(osrmURL)
		if err != nil {
			log.Printf("❌ [Directions] OSRM request failed: %v", err)
			// Fallback: return straight line
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(DirectionsResponse{
				Success: false,
				Coordinates: []DirectionsPoint{
					{Latitude: originLat, Longitude: originLng},
					{Latitude: destLat, Longitude: destLng},
				},
				Distance: 0,
				Duration: 0,
			})
			return
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("❌ [Directions] Failed to read OSRM response: %v", err)
			http.Error(w, "Failed to read routing response", http.StatusInternalServerError)
			return
		}

		// Parse OSRM response
		var osrmResp OSRMRouteResponse
		if err := json.Unmarshal(body, &osrmResp); err != nil {
			log.Printf("❌ [Directions] Failed to parse OSRM response: %v", err)
			http.Error(w, "Failed to parse routing response", http.StatusInternalServerError)
			return
		}

		if osrmResp.Code != "Ok" || len(osrmResp.Routes) == 0 {
			log.Printf("⚠️  [Directions] OSRM returned no routes (code: %s)", osrmResp.Code)
			// Fallback: return straight line
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(DirectionsResponse{
				Success: false,
				Coordinates: []DirectionsPoint{
					{Latitude: originLat, Longitude: originLng},
					{Latitude: destLat, Longitude: destLng},
				},
				Distance: 0,
				Duration: 0,
			})
			return
		}

		// Convert OSRM coordinates [lng, lat] → {latitude, longitude}
		route := osrmResp.Routes[0]
		points := make([]DirectionsPoint, len(route.Geometry.Coordinates))
		for i, coord := range route.Geometry.Coordinates {
			points[i] = DirectionsPoint{
				Latitude:  coord[1], // OSRM returns [lng, lat]
				Longitude: coord[0],
			}
		}

		log.Printf("✅ [Directions] Got route: %d points, %.0fm, %.0fs",
			len(points), route.Distance, route.Duration)

		// Return response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DirectionsResponse{
			Success:     true,
			Coordinates: points,
			Distance:    route.Distance,
			Duration:    route.Duration,
		})
	}
}
