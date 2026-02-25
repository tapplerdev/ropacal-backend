package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/services/roads"

	"github.com/jmoiron/sqlx"
)

// LocationPublishProxyRequest represents location data from driver
type LocationPublishProxyRequest struct {
	ClientID  string                 `json:"client"`
	Transport string                 `json:"transport"`
	Protocol  string                 `json:"protocol"`
	Encoding  string                 `json:"encoding"`
	User      string                 `json:"user"`      // Driver ID
	Channel   string                 `json:"channel"`  // driver:location:{driverId}
	Data      map[string]interface{} `json:"data"`     // GPS data
}

// LocationData represents the GPS data structure
type LocationData struct {
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Accuracy  float64  `json:"accuracy"`
	Heading   float64  `json:"heading"`
	Speed     float64  `json:"speed"`
	ShiftID   *string  `json:"shift_id"`
	Timestamp int64    `json:"timestamp"`
}

// CentrifugoLocationPublishProxy handles location publish requests from drivers
// This is called by Centrifugo BEFORE broadcasting the message
// We process the GPS data (save to Redis, snap to roads) and return modified data
func CentrifugoLocationPublishProxy(db *sqlx.DB, redisClient *redis.Client, osrmClient *roads.OSRMClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LocationPublishProxyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [LocationProxy] Invalid request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid request",
				},
			})
			return
		}

		log.Printf("📍 [LocationProxy] Received location from user=%s channel=%s",
			req.User, req.Channel)

		// 1. Validate channel format: driver:location:{driverId}
		parts := strings.Split(req.Channel, ":")
		if len(parts) != 3 || parts[0] != "driver" || parts[1] != "location" {
			log.Printf("❌ [LocationProxy] Invalid channel format: %s", req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid channel format",
				},
			})
			return
		}

		driverID := parts[2]

		// 2. Authorize: Only the driver can publish to their own location channel
		if req.User != driverID {
			// Silently deny - this is expected behavior when non-drivers try to publish
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "permission denied",
				},
			})
			return
		}

		// 3. Parse location data
		locationData, err := parseLocationData(req.Data)
		if err != nil {
			log.Printf("❌ [LocationProxy] Invalid location data: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: fmt.Sprintf("invalid location data: %v", err),
				},
			})
			return
		}

		log.Printf("📍 [LocationProxy] Driver %s: lat=%.6f, lng=%.6f, accuracy=%.1fm",
			driverID, locationData.Latitude, locationData.Longitude, locationData.Accuracy)

		// 4. Save ORIGINAL GPS to Redis (non-blocking)
		if redisClient != nil {
			ctx := context.Background()
			go func() {
				locationJSON, _ := json.Marshal(locationData)
				if err := redisClient.SaveDriverLocation(ctx, driverID, string(locationJSON)); err != nil {
					log.Printf("⚠️  [LocationProxy] Failed to save to Redis: %v", err)
				} else {
					log.Printf("✅ [LocationProxy] Saved to Redis: driver:%s", driverID)
				}
			}()
		}

		// 5. OSRM road snapping (if accuracy > 15m)
		snappedLat := locationData.Latitude
		snappedLng := locationData.Longitude

		if osrmClient != nil && locationData.Accuracy > 15 {
			newLat, newLng, err := osrmClient.SnapToRoad(
				locationData.Latitude,
				locationData.Longitude,
				locationData.Accuracy,
			)

			if err != nil {
				log.Printf("⚠️  [LocationProxy] OSRM snap failed: %v (using original coords)", err)
			} else if newLat != locationData.Latitude || newLng != locationData.Longitude {
				snappedLat = newLat
				snappedLng = newLng
				log.Printf("🗺️  [LocationProxy] Snapped: (%.6f, %.6f) → (%.6f, %.6f)",
					locationData.Latitude, locationData.Longitude, snappedLat, snappedLng)
			} else {
				log.Printf("✅ [LocationProxy] GPS accuracy good (%.1fm) - no snapping needed",
					locationData.Accuracy)
			}
		} else if locationData.Accuracy <= 15 {
			log.Printf("✅ [LocationProxy] GPS accuracy excellent (%.1fm) - skipping OSRM",
				locationData.Accuracy)
		}

		// 6. Return MODIFIED data to Centrifugo (it will broadcast this instead of original)
		modifiedData := map[string]interface{}{
			"latitude":  snappedLat,
			"longitude": snappedLng,
			"accuracy":  locationData.Accuracy,
			"heading":   locationData.Heading,
			"speed":     locationData.Speed,
			"timestamp": locationData.Timestamp,
		}

		if locationData.ShiftID != nil {
			modifiedData["shift_id"] = *locationData.ShiftID
		}

		log.Printf("✅ [LocationProxy] Returning modified data to Centrifugo for broadcast")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoPublishResponse{
			Result: &CentrifugoPublishResult{
				Data: modifiedData,
				SkipHistory: false, // Keep in channel history for recovery
			},
		})
	}
}

// parseLocationData extracts and validates location data from the request
func parseLocationData(data map[string]interface{}) (*LocationData, error) {
	location := &LocationData{}

	// Required fields
	lat, ok := data["latitude"].(float64)
	if !ok {
		return nil, fmt.Errorf("latitude is required and must be a number")
	}
	location.Latitude = lat

	lng, ok := data["longitude"].(float64)
	if !ok {
		return nil, fmt.Errorf("longitude is required and must be a number")
	}
	location.Longitude = lng

	// Optional fields with defaults
	if accuracy, ok := data["accuracy"].(float64); ok {
		location.Accuracy = accuracy
	} else {
		location.Accuracy = 100.0 // Default to 100m if not provided
	}

	if heading, ok := data["heading"].(float64); ok {
		location.Heading = heading
	}

	if speed, ok := data["speed"].(float64); ok {
		location.Speed = speed
	}

	if timestamp, ok := data["timestamp"].(float64); ok {
		location.Timestamp = int64(timestamp)
	}

	if shiftID, ok := data["shift_id"].(string); ok {
		location.ShiftID = &shiftID
	}

	// Validate coordinates
	if location.Latitude < -90 || location.Latitude > 90 {
		return nil, fmt.Errorf("invalid latitude: %f", location.Latitude)
	}

	if location.Longitude < -180 || location.Longitude > 180 {
		return nil, fmt.Errorf("invalid longitude: %f", location.Longitude)
	}

	return location, nil
}
