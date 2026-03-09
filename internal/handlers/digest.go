package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"ropacal-backend/internal/services"
)

// TriggerDigest returns an HTTP handler that manually triggers the daily digest.
// Idempotent — if the digest was already sent for the current window, it returns early.
func TriggerDigest(scheduler *services.DigestScheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Determine which window to run (default: auto-detect based on time)
		window := r.URL.Query().Get("window")
		if window == "" {
			window = "morning" // default
		}
		if window != "morning" && window != "afternoon" {
			http.Error(w, "window must be 'morning' or 'afternoon'", http.StatusBadRequest)
			return
		}

		result, err := scheduler.RunDigest(window)
		if err != nil {
			log.Printf("❌ [Digest] Manual trigger failed: %v", err)
			http.Error(w, "digest failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
