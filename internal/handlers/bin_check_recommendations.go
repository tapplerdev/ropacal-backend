package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// FlagStaleBins creates recommendations for bins that haven't been checked in 7+ days
// This is typically run as a daily cron job or can be triggered manually by managers
func FlagStaleBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		sevenDaysAgo := now - (7 * 24 * 60 * 60)

		log.Println("[FLAG-STALE-BINS] Starting scan for bins not checked in 7+ days...")

		// Find all active bins that haven't been checked in 7+ days
		// and don't already have a pending recommendation
		query := `
			SELECT
				b.id,
				b.bin_number,
				b.last_checked_at,
				CASE
					WHEN b.last_checked_at IS NULL THEN EXTRACT(EPOCH FROM (NOW() - to_timestamp(b.created_at)))::INT / 86400
					ELSE EXTRACT(EPOCH FROM (NOW() - to_timestamp(b.last_checked_at)))::INT / 86400
				END as days_since_check
			FROM bins b
			WHERE b.status = 'active'
			AND (b.last_checked_at IS NULL OR b.last_checked_at < $1)
			AND NOT EXISTS (
				SELECT 1 FROM bin_check_recommendations bcr
				WHERE bcr.bin_id = b.id
				AND bcr.status = 'pending'
			)
		`

		var staleBins []struct {
			ID             string `db:"id"`
			BinNumber      int    `db:"bin_number"`
			LastCheckedAt  *int64 `db:"last_checked_at"`
			DaysSinceCheck int    `db:"days_since_check"`
		}

		if err := db.Select(&staleBins, query, sevenDaysAgo); err != nil {
			log.Printf("❌ [FLAG-STALE-BINS] Database query failed: %v", err)
			http.Error(w, "Failed to query stale bins", http.StatusInternalServerError)
			return
		}

		if len(staleBins) == 0 {
			log.Println("✅ [FLAG-STALE-BINS] No stale bins found - all bins recently checked!")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"message":         "No stale bins found",
				"flagged_count":   0,
				"checked_at":      now,
			})
			return
		}

		log.Printf("📋 [FLAG-STALE-BINS] Found %d stale bins, creating recommendations...", len(staleBins))

		// Create recommendations for each stale bin
		flaggedCount := 0
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [FLAG-STALE-BINS] Failed to start transaction: %v", err)
			http.Error(w, "Failed to create recommendations", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		for _, bin := range staleBins {
			recID := uuid.New().String()

			recommendation := models.BinCheckRecommendation{
				ID:             recID,
				BinID:          bin.ID,
				Reason:         "time_based",
				FlaggedAt:      now,
				DaysSinceCheck: bin.DaysSinceCheck,
				Status:         "pending",
				CreatedAt:      now,
				UpdatedAt:      now,
			}

			_, err := tx.NamedExec(`
				INSERT INTO bin_check_recommendations
				(id, bin_id, reason, flagged_at, days_since_check, status, created_at, updated_at)
				VALUES
				(:id, :bin_id, :reason, :flagged_at, :days_since_check, :status, :created_at, :updated_at)
			`, recommendation)

			if err != nil {
				log.Printf("❌ [FLAG-STALE-BINS] Failed to create recommendation for bin %s: %v", bin.ID, err)
				continue
			}

			flaggedCount++
			log.Printf("   🚩 Flagged bin #%d (ID: %s) - %d days since last check", bin.BinNumber, bin.ID, bin.DaysSinceCheck)
		}

		if err := tx.Commit(); err != nil {
			log.Printf("❌ [FLAG-STALE-BINS] Failed to commit transaction: %v", err)
			http.Error(w, "Failed to save recommendations", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [FLAG-STALE-BINS] Successfully flagged %d bins for checking", flaggedCount)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":         "Stale bins flagged successfully",
			"flagged_count":   flaggedCount,
			"checked_at":      now,
		})
	}
}

// GetBinCheckRecommendations retrieves pending check recommendations
// Optional query params: status (pending/resolved/dismissed), bin_id
func GetBinCheckRecommendations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "pending" // Default to pending recommendations
		}

		binID := r.URL.Query().Get("bin_id")

		// Build query
		query := `
			SELECT
				bcr.*,
				b.id as "bin.id",
				b.bin_number as "bin.bin_number",
				b.current_street as "bin.current_street",
				b.city as "bin.city",
				b.zip as "bin.zip",
				b.status as "bin.status",
				b.fill_percentage as "bin.fill_percentage",
				b.latitude as "bin.latitude",
				b.longitude as "bin.longitude",
				b.last_checked_at as "bin.last_checked_at"
			FROM bin_check_recommendations bcr
			JOIN bins b ON b.id = bcr.bin_id
			WHERE bcr.status = $1
		`

		args := []interface{}{status}

		if binID != "" {
			query += " AND bcr.bin_id = $2"
			args = append(args, binID)
		}

		query += " ORDER BY bcr.days_since_check DESC, bcr.flagged_at DESC"

		rows, err := db.Queryx(query, args...)
		if err != nil {
			log.Printf("❌ [GET-CHECK-RECOMMENDATIONS] Query failed: %v", err)
			http.Error(w, "Failed to retrieve recommendations", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var recommendations []models.BinCheckRecommendationWithBin

		for rows.Next() {
			var rec models.BinCheckRecommendationWithBin
			rec.Bin = &models.Bin{}

			err := rows.Scan(
				&rec.ID,
				&rec.BinID,
				&rec.Reason,
				&rec.FlaggedAt,
				&rec.DaysSinceCheck,
				&rec.Status,
				&rec.ResolvedAt,
				&rec.ResolvedByUserID,
				&rec.Notes,
				&rec.CreatedAt,
				&rec.UpdatedAt,
				&rec.Bin.ID,
				&rec.Bin.BinNumber,
				&rec.Bin.CurrentStreet,
				&rec.Bin.City,
				&rec.Bin.Zip,
				&rec.Bin.Status,
				&rec.Bin.FillPercentage,
				&rec.Bin.Latitude,
				&rec.Bin.Longitude,
				&rec.Bin.LastCheckedAt,
			)

			if err != nil {
				log.Printf("❌ [GET-CHECK-RECOMMENDATIONS] Row scan failed: %v", err)
				continue
			}

			recommendations = append(recommendations, rec)
		}

		log.Printf("✅ [GET-CHECK-RECOMMENDATIONS] Found %d recommendations (status: %s)", len(recommendations), status)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(recommendations)
	}
}

// DismissBinCheckRecommendation allows managers to dismiss a recommendation
// (e.g., if they manually verified the bin is fine)
func DismissBinCheckRecommendation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recommendationID := chi.URLParam(r, "id")
		userID, _ := r.Context().Value("user_id").(string)

		var req struct {
			Notes *string `json:"notes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Update recommendation status to dismissed
		result, err := db.Exec(`
			UPDATE bin_check_recommendations
			SET status = 'dismissed',
			    resolved_at = $1,
			    resolved_by_user_id = $2,
			    notes = $3,
			    updated_at = $1
			WHERE id = $4
			AND status = 'pending'
		`, now, userID, req.Notes, recommendationID)

		if err != nil {
			log.Printf("❌ [DISMISS-RECOMMENDATION] Update failed: %v", err)
			http.Error(w, "Failed to dismiss recommendation", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			http.Error(w, "Recommendation not found or already resolved", http.StatusNotFound)
			return
		}

		log.Printf("✅ [DISMISS-RECOMMENDATION] Recommendation %s dismissed by user %s", recommendationID, userID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Recommendation dismissed successfully",
		})
	}
}

// autoResolveCheckRecommendation is a helper function called when a bin is checked
// It marks any pending recommendations for that bin as resolved
func autoResolveCheckRecommendation(db *sqlx.DB, binID string, userID string, now int64) {
	result, err := db.Exec(`
		UPDATE bin_check_recommendations
		SET status = 'resolved',
		    resolved_at = $1,
		    resolved_by_user_id = $2,
		    updated_at = $1
		WHERE bin_id = $3
		AND status = 'pending'
	`, now, userID, binID)

	if err != nil {
		log.Printf("⚠️  [AUTO-RESOLVE] Failed to auto-resolve recommendation for bin %s: %v", binID, err)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected > 0 {
		log.Printf("✅ [AUTO-RESOLVE] Auto-resolved %d check recommendation(s) for bin %s", rowsAffected, binID)
	}
}
