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
		validWindows := map[string]bool{
			"morning": true, "afternoon": true,
			"daily_move_report": true, "daily_bin_check_report": true,
			"daily_battery_report": true,
		}
		if !validWindows[window] {
			http.Error(w, "window must be 'morning', 'afternoon', 'daily_move_report', 'daily_bin_check_report', or 'daily_battery_report'", http.StatusBadRequest)
			return
		}

		forceStr := r.URL.Query().Get("force")
		forceRun := forceStr == "true" || forceStr == "1"

		result, err := scheduler.RunDigest(window, forceRun)
		if err != nil {
			log.Printf("❌ [Digest] Manual trigger failed: %v", err)
			http.Error(w, "digest failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}
