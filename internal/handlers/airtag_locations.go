package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/services"

	"github.com/jmoiron/sqlx"
)

// GetAirtagLocations reads AirTag locations from the database (written by the FindMy bridge).
func GetAirtagLocations(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		if !airtagTrackingOr404(w, db) {
			return
		}
		entries, err := services.GetAirtagLocationsFromDB(db)
		if err != nil {
			log.Printf("❌ [AirtagLocations] DB query failed: %v", err)
			http.Error(w, "Failed to fetch AirTag locations", http.StatusInternalServerError)
			return
		}

		unmatched, err := services.GetUnmatchedAirtagLocationsFromDB(db)
		if err != nil {
			log.Printf("⚠️ [AirtagLocations] Unmatched query failed: %v", err)
			unmatched = []services.AirtagEntry{}
		}

		// Get last_sync_at from config table
		var lastSync *string
		var syncVal string
		err = db.QueryRow(`SELECT value->>'last_sync_at' FROM config WHERE key = 'airtag_last_sync'`).Scan(&syncVal)
		if err == nil && syncVal != "" {
			lastSync = &syncVal
		}

		resp := services.AirtagAPIResponse{
			Data:           entries,
			Count:          len(entries),
			Unmatched:      unmatched,
			UnmatchedCount: len(unmatched),
			LastSync:       lastSync,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// airtagTrackingOr404 answers 404 for an organization without AirTag tracking
// (organizations.airtag_tracking) and reports whether the handler may go on.
// A failed read is a 500, never a silent "off".
func airtagTrackingOr404(w http.ResponseWriter, db *orgdb.DB) bool {
	on, err := services.AirtagTrackingEnabled(db)
	if err != nil {
		log.Printf("❌ [Airtag] %v", err)
		http.Error(w, "Failed to check AirTag tracking", http.StatusInternalServerError)
		return false
	}
	if !on {
		http.Error(w, "AirTag tracking is not enabled for this organization", http.StatusNotFound)
		return false
	}
	return true
}
