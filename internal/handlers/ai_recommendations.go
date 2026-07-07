package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/middleware"
	pkg "ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

type AIRecommendation struct {
	ID                string  `json:"id" db:"id"`
	Type              string  `json:"type" db:"type"`
	EntityType        string  `json:"entity_type" db:"entity_type"`
	EntityID          *string `json:"entity_id,omitempty" db:"entity_id"`
	Title             string  `json:"title" db:"title"`
	Description       string  `json:"description" db:"description"`
	Severity          string  `json:"severity" db:"severity"`
	RecommendedAction *string `json:"recommended_action,omitempty" db:"recommended_action"`
	Status            string  `json:"status" db:"status"`
	Source            string  `json:"source" db:"source"`
	Reasoning         *string `json:"reasoning,omitempty" db:"reasoning"`
	CreatedAt         int64   `json:"created_at" db:"created_at"`
	ExpiresAt         *int64  `json:"expires_at,omitempty" db:"expires_at"`
	ActionedAt        *int64  `json:"actioned_at,omitempty" db:"actioned_at"`
	ActionedByUserID  *string `json:"actioned_by_user_id,omitempty" db:"actioned_by_user_id"`
	SnoozedUntil      *int64  `json:"snoozed_until,omitempty" db:"snoozed_until"`
}

// GetAIRecommendations returns filtered list of AI recommendations
func GetAIRecommendations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		recType := r.URL.Query().Get("type")
		limit := r.URL.Query().Get("limit")

		if limit == "" {
			limit = "50"
		}

		query := `SELECT * FROM ai_recommendations WHERE 1=1`
		args := []any{}
		argIdx := 1

		if status != "" && status != "all" {
			query += fmt.Sprintf(` AND status = $%d`, argIdx)
			args = append(args, status)
			argIdx++
		}
		if recType != "" {
			query += fmt.Sprintf(` AND type = $%d`, argIdx)
			args = append(args, recType)
			argIdx++
		}

		// Exclude expired recommendations
		now := time.Now().Unix()
		query += fmt.Sprintf(` AND (expires_at IS NULL OR expires_at > $%d)`, argIdx)
		args = append(args, now)
		argIdx++

		// Exclude snoozed recommendations that haven't reached their snooze time
		query += fmt.Sprintf(` AND (snoozed_until IS NULL OR snoozed_until <= $%d)`, argIdx)
		args = append(args, now)
		argIdx++

		query += ` ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, created_at DESC`
		query += fmt.Sprintf(` LIMIT $%d`, argIdx)
		args = append(args, limit)

		var recs []AIRecommendation
		err := db.Select(&recs, query, args...)
		if err != nil {
			log.Printf("❌ [AI Recommendations] Query failed: %v", err)
			pkg.RespondError(w, http.StatusInternalServerError, "Failed to fetch recommendations")
			return
		}

		// Get counts by status
		type statusCount struct {
			Status string `db:"status"`
			Count  int    `db:"count"`
		}
		var counts []statusCount
		db.Select(&counts, `SELECT status, COUNT(*) as count FROM ai_recommendations WHERE (expires_at IS NULL OR expires_at > $1) GROUP BY status`, now)

		countMap := map[string]int{}
		total := 0
		for _, c := range counts {
			countMap[c.Status] = c.Count
			total += c.Count
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"recommendations": recs,
			"counts": map[string]int{
				"total":     total,
				"pending":   countMap["pending"],
				"accepted":  countMap["accepted"],
				"dismissed": countMap["dismissed"],
				"snoozed":   countMap["snoozed"],
			},
		})
	}
}

// AcceptRecommendation marks a recommendation as accepted
func AcceptRecommendation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			pkg.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		id := chi.URLParam(r, "id")
		now := time.Now().Unix()

		result, err := db.Exec(`UPDATE ai_recommendations SET status = 'accepted', actioned_at = $1, actioned_by_user_id = $2 WHERE id = $3 AND status = 'pending'`,
			now, userClaims.UserID, id)
		if err != nil {
			pkg.RespondError(w, http.StatusInternalServerError, "Failed to accept recommendation")
			return
		}

		rows, _ := result.RowsAffected()
		if rows == 0 {
			pkg.RespondError(w, http.StatusNotFound, "Recommendation not found or already actioned")
			return
		}

		log.Printf("✅ [AI Recommendations] Accepted: %s by %s", id, userClaims.Email)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Recommendation accepted"})
	}
}

// DismissRecommendation marks a recommendation as dismissed
func DismissRecommendation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			pkg.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		id := chi.URLParam(r, "id")
		now := time.Now().Unix()

		result, err := db.Exec(`UPDATE ai_recommendations SET status = 'dismissed', actioned_at = $1, actioned_by_user_id = $2 WHERE id = $3 AND status = 'pending'`,
			now, userClaims.UserID, id)
		if err != nil {
			pkg.RespondError(w, http.StatusInternalServerError, "Failed to dismiss recommendation")
			return
		}

		rows, _ := result.RowsAffected()
		if rows == 0 {
			pkg.RespondError(w, http.StatusNotFound, "Recommendation not found or already actioned")
			return
		}

		log.Printf("🚫 [AI Recommendations] Dismissed: %s by %s", id, userClaims.Email)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Recommendation dismissed"})
	}
}

// SnoozeRecommendation snoozes a recommendation until a specified time
func SnoozeRecommendation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			pkg.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		id := chi.URLParam(r, "id")

		var req struct {
			SnoozeUntil int64 `json:"snooze_until"` // Unix timestamp
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SnoozeUntil == 0 {
			// Default: snooze for 24 hours
			req.SnoozeUntil = time.Now().Add(24 * time.Hour).Unix()
		}

		result, err := db.Exec(`UPDATE ai_recommendations SET status = 'snoozed', snoozed_until = $1, actioned_by_user_id = $2 WHERE id = $3 AND status = 'pending'`,
			req.SnoozeUntil, userClaims.UserID, id)
		if err != nil {
			pkg.RespondError(w, http.StatusInternalServerError, "Failed to snooze recommendation")
			return
		}

		rows, _ := result.RowsAffected()
		if rows == 0 {
			pkg.RespondError(w, http.StatusNotFound, "Recommendation not found or already actioned")
			return
		}

		log.Printf("💤 [AI Recommendations] Snoozed: %s until %d by %s", id, req.SnoozeUntil, userClaims.Email)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "Recommendation snoozed"})
	}
}

// GetPendingCount returns count of pending recommendations (for sidebar badge)
func GetPendingRecommendationCount(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().Unix()
		var count int
		err := db.Get(&count, `SELECT COUNT(*) FROM ai_recommendations WHERE status = 'pending' AND (expires_at IS NULL OR expires_at > $1) AND (snoozed_until IS NULL OR snoozed_until <= $1)`, now)
		if err != nil {
			count = 0
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"count": count})
	}
}
