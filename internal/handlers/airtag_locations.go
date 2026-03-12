package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"ropacal-backend/internal/services"
)

// GetAirtagLocations proxies AirTag location data from the FindMy bridge service.
// The dashboard calls this instead of the bridge directly, keeping the bridge private.
func GetAirtagLocations() http.HandlerFunc {
	bridgeURL := os.Getenv("FINDMY_BRIDGE_URL")

	return func(w http.ResponseWriter, r *http.Request) {
		if bridgeURL == "" {
			log.Println("❌ [AirtagLocations] FINDMY_BRIDGE_URL not configured")
			http.Error(w, "FindMy bridge not configured", http.StatusServiceUnavailable)
			return
		}

		resp, err := services.FetchAirtagLocations(bridgeURL)
		if err != nil {
			log.Printf("❌ [AirtagLocations] Failed to fetch from bridge: %v", err)
			http.Error(w, "Failed to fetch AirTag locations", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}
