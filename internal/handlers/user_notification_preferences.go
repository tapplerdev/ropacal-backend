package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// UserNotificationPreferences represents per-user notification opt-in/out settings.
type UserNotificationPreferences struct {
	UserID            string `json:"user_id" db:"user_id"`
	DriftAlerts       bool   `json:"drift_alerts" db:"drift_alerts"`
	Digests           bool   `json:"digests" db:"digests"`
	ShiftEvents       bool   `json:"shift_events" db:"shift_events"`
	MoveRequests      bool   `json:"move_requests" db:"move_requests"`
	OverdueMoveAlerts bool   `json:"overdue_move_alerts" db:"overdue_move_alerts"`
	DueSoonAlerts     bool   `json:"due_soon_alerts" db:"due_soon_alerts"`
	BinCheckReports   bool   `json:"bin_check_reports" db:"bin_check_reports"`
	BatteryAlerts     bool   `json:"battery_alerts" db:"battery_alerts"`
}

// GetNotificationPreferences returns the current user's notification preferences.
func GetNotificationPreferences(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var prefs UserNotificationPreferences
		err := db.Get(&prefs, `SELECT user_id, drift_alerts, digests, shift_events, move_requests, overdue_move_alerts, due_soon_alerts, bin_check_reports, battery_alerts FROM user_notification_preferences WHERE user_id = $1`, user.UserID)
		if err == sql.ErrNoRows {
			prefs = UserNotificationPreferences{
				UserID:            user.UserID,
				DriftAlerts:       true,
				Digests:           true,
				ShiftEvents:       true,
				MoveRequests:      true,
				OverdueMoveAlerts: true,
				DueSoonAlerts:     true,
				BinCheckReports:   true,
				BatteryAlerts:     true,
			}
		} else if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch preferences")
			return
		}

		utils.RespondJSON(w, http.StatusOK, prefs)
	}
}

// UpdateNotificationPreferences updates the current user's notification preferences.
func UpdateNotificationPreferences(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Pointer fields → nil when the client omits a key, so a partial PATCH updates only
		// what it sends instead of blanking the rest (#14).
		var input struct {
			DriftAlerts       *bool `json:"drift_alerts"`
			Digests           *bool `json:"digests"`
			ShiftEvents       *bool `json:"shift_events"`
			MoveRequests      *bool `json:"move_requests"`
			OverdueMoveAlerts *bool `json:"overdue_move_alerts"`
			DueSoonAlerts     *bool `json:"due_soon_alerts"`
			BinCheckReports   *bool `json:"bin_check_reports"`
			BatteryAlerts     *bool `json:"battery_alerts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		now := time.Now().Unix()
		// Partial update: COALESCE keeps the existing value for any omitted (NULL) field on
		// conflict; a brand-new row defaults omitted fields to true (opt-in), matching
		// GetNotificationPreferences' no-row defaults.
		_, err := db.Exec(`
			INSERT INTO user_notification_preferences (user_id, drift_alerts, digests, shift_events, move_requests, overdue_move_alerts, due_soon_alerts, bin_check_reports, battery_alerts, created_at, updated_at)
			VALUES ($1, COALESCE($2,true), COALESCE($3,true), COALESCE($4,true), COALESCE($5,true), COALESCE($6,true), COALESCE($7,true), COALESCE($8,true), COALESCE($9,true), $10, $10)
			ON CONFLICT (user_id) DO UPDATE SET
				drift_alerts = COALESCE($2, user_notification_preferences.drift_alerts),
				digests = COALESCE($3, user_notification_preferences.digests),
				shift_events = COALESCE($4, user_notification_preferences.shift_events),
				move_requests = COALESCE($5, user_notification_preferences.move_requests),
				overdue_move_alerts = COALESCE($6, user_notification_preferences.overdue_move_alerts),
				due_soon_alerts = COALESCE($7, user_notification_preferences.due_soon_alerts),
				bin_check_reports = COALESCE($8, user_notification_preferences.bin_check_reports),
				battery_alerts = COALESCE($9, user_notification_preferences.battery_alerts),
				updated_at = $10
		`, user.UserID, input.DriftAlerts, input.Digests, input.ShiftEvents, input.MoveRequests,
			input.OverdueMoveAlerts, input.DueSoonAlerts, input.BinCheckReports, input.BatteryAlerts, now)
		if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save preferences")
			return
		}

		// Return the SAVED (merged) row, not the partial input.
		var saved UserNotificationPreferences
		if err := db.Get(&saved, `SELECT user_id, drift_alerts, digests, shift_events, move_requests, overdue_move_alerts, due_soon_alerts, bin_check_reports, battery_alerts FROM user_notification_preferences WHERE user_id = $1`, user.UserID); err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to load saved preferences")
			return
		}
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"status":      "ok",
			"preferences": saved,
		})
	}
}
