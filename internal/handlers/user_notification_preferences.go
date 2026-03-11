package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// UserNotificationPreferences represents per-user notification opt-in/out settings.
type UserNotificationPreferences struct {
	UserID       string `json:"user_id" db:"user_id"`
	DriftAlerts  bool   `json:"drift_alerts" db:"drift_alerts"`
	Digests      bool   `json:"digests" db:"digests"`
	ShiftEvents  bool   `json:"shift_events" db:"shift_events"`
	MoveRequests bool   `json:"move_requests" db:"move_requests"`
}

// GetNotificationPreferences returns the current user's notification preferences.
func GetNotificationPreferences(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var prefs UserNotificationPreferences
		err := db.Get(&prefs, `SELECT user_id, drift_alerts, digests, shift_events, move_requests FROM user_notification_preferences WHERE user_id = $1`, user.UserID)
		if err == sql.ErrNoRows {
			prefs = UserNotificationPreferences{
				UserID:       user.UserID,
				DriftAlerts:  true,
				Digests:      true,
				ShiftEvents:  true,
				MoveRequests: true,
			}
		} else if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch preferences")
			return
		}

		utils.RespondJSON(w, http.StatusOK, prefs)
	}
}

// UpdateNotificationPreferences updates the current user's notification preferences.
func UpdateNotificationPreferences(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var input struct {
			DriftAlerts  bool `json:"drift_alerts"`
			Digests      bool `json:"digests"`
			ShiftEvents  bool `json:"shift_events"`
			MoveRequests bool `json:"move_requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		now := time.Now().Unix()
		_, err := db.Exec(`
			INSERT INTO user_notification_preferences (user_id, drift_alerts, digests, shift_events, move_requests, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (user_id) DO UPDATE SET
				drift_alerts = $2, digests = $3, shift_events = $4, move_requests = $5, updated_at = $6
		`, user.UserID, input.DriftAlerts, input.Digests, input.ShiftEvents, input.MoveRequests, now)
		if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save preferences")
			return
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"status": "ok",
			"preferences": UserNotificationPreferences{
				UserID:       user.UserID,
				DriftAlerts:  input.DriftAlerts,
				Digests:      input.Digests,
				ShiftEvents:  input.ShiftEvents,
				MoveRequests: input.MoveRequests,
			},
		})
	}
}
