package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"
)

// GetWarehouseLocation returns the current warehouse location from config
func GetWarehouseLocation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var configValue []byte
		err := db.QueryRow(`
			SELECT value
			FROM config
			WHERE key = 'warehouse_location'
		`).Scan(&configValue)

		if err == sql.ErrNoRows {
			log.Printf("⚠️  Warehouse location not configured in database")
			http.Error(w, `{"error":"Warehouse location not configured"}`, http.StatusNotFound)
			return
		}

		if err != nil {
			log.Printf("❌ Failed to fetch warehouse location: %v", err)
			http.Error(w, `{"error":"Failed to fetch warehouse location"}`, http.StatusInternalServerError)
			return
		}

		var warehouse models.WarehouseLocation
		if err := json.Unmarshal(configValue, &warehouse); err != nil {
			log.Printf("❌ Failed to parse warehouse location: %v", err)
			http.Error(w, `{"error":"Failed to parse warehouse location"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(warehouse)
	}
}

// UpdateWarehouseLocation updates the warehouse location in config
func UpdateWarehouseLocation(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input models.WarehouseLocation
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, `{"error":"Invalid request body"}`, http.StatusBadRequest)
			return
		}

		// Validate coordinates
		if input.Latitude < -90 || input.Latitude > 90 {
			http.Error(w, `{"error":"Latitude must be between -90 and 90"}`, http.StatusBadRequest)
			return
		}
		if input.Longitude < -180 || input.Longitude > 180 {
			http.Error(w, `{"error":"Longitude must be between -180 and 180"}`, http.StatusBadRequest)
			return
		}
		// Reject null-island (0,0): a real warehouse is never at 0°,0°, and downstream code
		// (store/pickup-only move dropoffs) treats 0,0 as "unresolved" — so accepting it would
		// silently break those moves. Guard here at the boundary.
		if input.Latitude == 0 && input.Longitude == 0 {
			http.Error(w, `{"error":"Warehouse coordinates cannot be 0,0 (null island)"}`, http.StatusBadRequest)
			return
		}
		if input.Address == "" {
			http.Error(w, `{"error":"Address is required"}`, http.StatusBadRequest)
			return
		}

		// Marshal to JSON
		warehouseJSON, err := json.Marshal(input)
		if err != nil {
			log.Printf("❌ Failed to marshal warehouse data: %v", err)
			http.Error(w, `{"error":"Failed to marshal warehouse data"}`, http.StatusInternalServerError)
			return
		}

		// Update or insert warehouse location (no user tracking for now)
		_, err = db.Exec(`
			INSERT INTO config (key, value, updated_by, updated_at)
			VALUES ('warehouse_location', $1, 'system', CURRENT_TIMESTAMP)
			ON CONFLICT (key)
			DO UPDATE SET
				value = EXCLUDED.value,
				updated_by = 'system',
				updated_at = CURRENT_TIMESTAMP
		`, warehouseJSON)

		if err != nil {
			log.Printf("❌ Failed to update warehouse location: %v", err)
			http.Error(w, `{"error":"Failed to update warehouse location"}`, http.StatusInternalServerError)
			return
		}

		log.Printf("✅ Warehouse location updated: %.6f, %.6f - %s",
			input.Latitude, input.Longitude, input.Address)

		// Broadcast warehouse location update to all managers/admins via WebSocket
		log.Printf("📡 Broadcasting warehouse_location_updated to managers/admins")
		hub.BroadcastToRole("admin", map[string]interface{}{
			"type": "warehouse_location_updated",
			"data": input,
		})
		hub.BroadcastToRole("manager", map[string]interface{}{
			"type": "warehouse_location_updated",
			"data": input,
		})
		log.Printf("✅ Warehouse update broadcast sent")

		// Also publish via Centrifugo for mobile app notification pipeline
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "warehouse_location_updated", input); pubErr != nil {
				log.Printf("⚠️  Failed to publish warehouse_location_updated to Centrifugo: %v", pubErr)
			} else {
				log.Printf("📡 Published warehouse_location_updated via Centrifugo")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":  "Warehouse location updated successfully",
			"location": input,
		})
	}
}
