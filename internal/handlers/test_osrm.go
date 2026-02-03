package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/services/roads"
	"ropacal-backend/internal/websocket"

	"github.com/jmoiron/sqlx"
)

// TestOSRMRequest represents the request body for testing OSRM integration
type TestOSRMRequest struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Accuracy  float64 `json:"accuracy"`
}

// TestOSRMResponse represents the response for OSRM test endpoint
type TestOSRMResponse struct {
	Original struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"original"`
	Snapped struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"snapped"`
	WasSnapped       bool    `json:"was_snapped"`
	DriverID         string  `json:"driver_id"`
	BroadcastSuccess bool    `json:"broadcast_success"`
	Message          string  `json:"message"`
}

// TestOSRM is a test endpoint to verify OSRM integration
// Requires JWT authentication (driver login)
// POST /api/test/osrm
func TestOSRM(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get user from JWT token (set by Auth middleware)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Parse request body
		var req TestOSRMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate coordinates
		if req.Latitude == 0 || req.Longitude == 0 {
			http.Error(w, "Invalid coordinates", http.StatusBadRequest)
			return
		}

		// Default accuracy if not provided
		if req.Accuracy == 0 {
			req.Accuracy = 100.0
		}

		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("🧪 [OSRM TEST] Request received")
		log.Printf("   Driver: %s (%s)", userClaims.Email, userClaims.UserID)
		log.Printf("   Original: %.6f, %.6f", req.Latitude, req.Longitude)
		log.Printf("   Accuracy: %.1fm", req.Accuracy)
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Initialize response
		response := TestOSRMResponse{
			DriverID: userClaims.UserID,
		}
		response.Original.Latitude = req.Latitude
		response.Original.Longitude = req.Longitude

		// Try to snap coordinates using OSRM
		roadsClient := roads.NewRoadSnapperFromEnv()
		snappedLat, snappedLng, err := roadsClient.SnapToRoad(req.Latitude, req.Longitude, req.Accuracy)

		if err != nil {
			log.Printf("⚠️  [OSRM TEST] Snap failed: %v", err)
			response.Snapped.Latitude = req.Latitude
			response.Snapped.Longitude = req.Longitude
			response.WasSnapped = false
			response.Message = "Snap failed: " + err.Error()
		} else {
			response.Snapped.Latitude = snappedLat
			response.Snapped.Longitude = snappedLng
			response.WasSnapped = (snappedLat != req.Latitude || snappedLng != req.Longitude)

			if response.WasSnapped {
				log.Printf("✅ [OSRM TEST] Snapped: %.6f, %.6f", snappedLat, snappedLng)
			} else {
				log.Printf("ℹ️  [OSRM TEST] No snap needed (coordinates unchanged)")
			}
		}

		// Update database with ORIGINAL coordinates (for audit trail)
		query := `
			INSERT INTO driver_current_location (
				driver_id, latitude, longitude, accuracy, timestamp, is_connected, updated_at
			) VALUES ($1, $2, $3, $4, $5, TRUE, EXTRACT(EPOCH FROM NOW())::BIGINT)
			ON CONFLICT (driver_id)
			DO UPDATE SET
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				accuracy = EXCLUDED.accuracy,
				timestamp = EXCLUDED.timestamp,
				is_connected = TRUE,
				updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
			RETURNING updated_at
		`

		var updatedAt int64
		timestamp := time.Now().Unix()
		err = db.QueryRow(query, userClaims.UserID, req.Latitude, req.Longitude, req.Accuracy, timestamp).Scan(&updatedAt)
		if err != nil {
			log.Printf("❌ [OSRM TEST] Database update failed: %v", err)
			http.Error(w, "Failed to update location", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [OSRM TEST] Database updated (original coords)")

		// Broadcast SNAPPED coordinates to managers
		locationUpdate := map[string]interface{}{
			"type": "driver_location_update",
			"data": map[string]interface{}{
				"driver_id":  userClaims.UserID,
				"latitude":   response.Snapped.Latitude,
				"longitude":  response.Snapped.Longitude,
				"accuracy":   req.Accuracy,
				"timestamp":  timestamp,
				"updated_at": updatedAt,
			},
		}

		hub.BroadcastToRole("admin", locationUpdate)
		response.BroadcastSuccess = true
		response.Message = "Location updated and broadcasted to managers"

		log.Printf("📤 [OSRM TEST] Broadcasted to managers (snapped coords)")
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Return response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}
