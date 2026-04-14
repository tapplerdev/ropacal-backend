package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"ropacal-backend/internal/services"
)

// SyncAirtagLocations triggers an immediate location sync on the FindMy bridge.
// The dashboard calls this to force a fresh fetch from Apple's FindMy network.
func SyncAirtagLocations() http.HandlerFunc {
	bridgeURL := os.Getenv("FINDMY_BRIDGE_URL")
	apiKey := os.Getenv("INTERNAL_API_KEY")

	return func(w http.ResponseWriter, r *http.Request) {
		if bridgeURL == "" {
			log.Println("❌ [AirtagSync] FINDMY_BRIDGE_URL not configured")
			http.Error(w, "FindMy bridge not configured", http.StatusServiceUnavailable)
			return
		}

		resp, err := services.TriggerAirtagSync(bridgeURL, apiKey)
		if err != nil {
			log.Printf("❌ [AirtagSync] Failed to trigger sync: %v", err)
			http.Error(w, "Failed to trigger AirTag sync", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}
