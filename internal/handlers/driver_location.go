package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/roads"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// LocationUpdateRequest represents the location data sent by the driver
type LocationUpdateRequest struct {
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Heading   *float64 `json:"heading"`
	Speed     *float64 `json:"speed"`
	Accuracy  *float64 `json:"accuracy"`
	ShiftID   *string  `json:"shift_id"`
	Timestamp int64    `json:"timestamp"`
}

// PostDriverLocation handles location updates from drivers
// Flow: Save to DB → OSRM snap → Publish to Centrifugo
func PostDriverLocation(db *sqlx.DB, centrifugoClient *centrifugo.Client, roadsClient *roads.OSRMClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get authenticated user
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Only drivers can post location
		if userClaims.Role != "driver" {
			utils.RespondError(w, http.StatusForbidden, "Only drivers can post location")
			return
		}

		// Parse request body
		var req LocationUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Invalid location request: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate coordinates
		if req.Latitude < -90 || req.Latitude > 90 || req.Longitude < -180 || req.Longitude > 180 {
			log.Printf("❌ Invalid coordinates: lat=%.6f, lng=%.6f", req.Latitude, req.Longitude)
			utils.RespondError(w, http.StatusBadRequest, "Invalid coordinates")
			return
		}

		log.Printf("📍 Location update from driver %s: (%.6f, %.6f)", userClaims.UserID, req.Latitude, req.Longitude)

		// Default accuracy to 100m if not provided
		accuracyValue := 100.0
		if req.Accuracy != nil {
			accuracyValue = *req.Accuracy
		}

		// Step 1: Save ORIGINAL GPS to database (for audit/legal)
		query := `
			INSERT INTO driver_current_location (
				driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp, is_connected, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, EXTRACT(EPOCH FROM NOW())::BIGINT)
			ON CONFLICT (driver_id)
			DO UPDATE SET
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				heading = EXCLUDED.heading,
				speed = EXCLUDED.speed,
				accuracy = EXCLUDED.accuracy,
				shift_id = EXCLUDED.shift_id,
				timestamp = EXCLUDED.timestamp,
				is_connected = TRUE,
				updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
			RETURNING updated_at
		`

		var updatedAt int64
		err := db.QueryRow(
			query,
			userClaims.UserID,
			req.Latitude,  // Original GPS
			req.Longitude, // Original GPS
			req.Heading,
			req.Speed,
			req.Accuracy,
			req.ShiftID,
			req.Timestamp,
		).Scan(&updatedAt)

		if err != nil {
			log.Printf("❌ Error saving location to database: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save location")
			return
		}

		log.Printf("✅ Location saved to database (original GPS for audit)")

		// Step 2: Snap to roads using OSRM (if accuracy > 15m)
		snappedLat := req.Latitude
		snappedLng := req.Longitude

		if roadsClient != nil {
			newLat, newLng, err := roadsClient.SnapToRoad(req.Latitude, req.Longitude, accuracyValue)
			if err == nil && (newLat != req.Latitude || newLng != req.Longitude) {
				snappedLat = newLat
				snappedLng = newLng
				log.Printf("🗺️  Snapped to road: (%.6f, %.6f) → (%.6f, %.6f)", req.Latitude, req.Longitude, snappedLat, snappedLng)
			}
		}

		// Step 3: Publish SNAPPED location to Centrifugo for managers
		if centrifugoClient != nil {
			// Convert pointer fields to values (use 0 if nil)
			heading := 0.0
			if req.Heading != nil {
				heading = *req.Heading
			}
			speed := 0.0
			if req.Speed != nil {
				speed = *req.Speed
			}
			accuracy := accuracyValue

			locationData := centrifugo.DriverLocation{
				Latitude:  snappedLat, // SNAPPED coordinates for display
				Longitude: snappedLng, // SNAPPED coordinates for display
				Heading:   heading,
				Speed:     speed,
				Accuracy:  accuracy,
				Timestamp: req.Timestamp,
			}

			err := centrifugoClient.PublishDriverLocation(r.Context(), userClaims.UserID, locationData)
			if err != nil {
				log.Printf("⚠️  Failed to publish to Centrifugo: %v", err)
				// Don't fail the request - location is already saved to DB
			} else {
				log.Printf("📤 Published snapped location to Centrifugo: driver:location:%s", userClaims.UserID)
			}
		}

		// Return success
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":    true,
			"updated_at": updatedAt,
			"snapped": map[string]interface{}{
				"latitude":  snappedLat,
				"longitude": snappedLng,
			},
		})
	}
}
