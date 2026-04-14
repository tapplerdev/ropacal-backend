package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"ropacal-backend/internal/services"

	"github.com/jmoiron/sqlx"
)

// GetAirtagLocations reads AirTag locations from the database (written by the FindMy bridge).
func GetAirtagLocations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := services.GetAirtagLocationsFromDB(db)
		if err != nil {
			log.Printf("❌ [AirtagLocations] DB query failed: %v", err)
			http.Error(w, "Failed to fetch AirTag locations", http.StatusInternalServerError)
			return
		}

		// Get last_sync_at from config table
		var lastSync *string
		var syncVal string
		err = db.QueryRow(`SELECT value->>'last_sync_at' FROM config WHERE key = 'airtag_last_sync'`).Scan(&syncVal)
		if err == nil && syncVal != "" {
			lastSync = &syncVal
		}

		resp := services.AirtagAPIResponse{
			Data:     entries,
			Count:    len(entries),
			LastSync: lastSync,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}
