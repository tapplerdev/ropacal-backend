package handlers

import (
	"ropacal-backend/internal/orgdb"

	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
)

// NotificationSettings holds all configurable notification preferences
type NotificationSettings struct {
	DriftAlertsEnabled          bool   `json:"drift_alerts_enabled"`
	DriftCheckIntervalMinutes   int    `json:"drift_check_interval_minutes"`
	DriftThresholdMeters        int    `json:"drift_threshold_meters"`
	MorningDigestEnabled        bool   `json:"morning_digest_enabled"`
	MorningDigestHour           int    `json:"morning_digest_hour"`
	MorningDigestMinute         int    `json:"morning_digest_minute"`
	AfternoonDigestEnabled      bool   `json:"afternoon_digest_enabled"`
	AfternoonDigestHour         int    `json:"afternoon_digest_hour"`
	AfternoonDigestMinute       int    `json:"afternoon_digest_minute"`
	ShiftNotificationsEnabled   bool   `json:"shift_notifications_enabled"`
	MoveRequestNotifEnabled     bool   `json:"move_request_notifications_enabled"`
	Timezone                    string `json:"timezone"`
	OverdueMoveAlertsEnabled    bool   `json:"overdue_move_alerts_enabled"`
	OverdueMoveCheckIntervalMin int    `json:"overdue_move_check_interval_minutes"`
	DueSoonAlertsEnabled        bool   `json:"due_soon_alerts_enabled"`
	DueSoonHoursBefore          int    `json:"due_soon_hours_before"`
	DailyMoveReportEnabled      bool   `json:"daily_move_report_enabled"`
	DailyMoveReportHour         int    `json:"daily_move_report_hour"`
	DailyMoveReportMinute       int    `json:"daily_move_report_minute"`
	DailyBinCheckEnabled        bool   `json:"daily_bin_check_enabled"`
	DailyBinCheckHour           int    `json:"daily_bin_check_hour"`
	DailyBinCheckMinute         int    `json:"daily_bin_check_minute"`
	DailyBatteryReportEnabled   bool   `json:"daily_battery_report_enabled"`
	DailyBatteryReportHour      int    `json:"daily_battery_report_hour"`
	DailyBatteryReportMinute    int    `json:"daily_battery_report_minute"`
}

// DefaultNotificationSettings returns the default settings
func DefaultNotificationSettings() NotificationSettings {
	return NotificationSettings{
		DriftAlertsEnabled:          true,
		DriftCheckIntervalMinutes:   5,
		DriftThresholdMeters:        500,
		MorningDigestEnabled:        true,
		MorningDigestHour:           8,
		MorningDigestMinute:         0,
		AfternoonDigestEnabled:      true,
		AfternoonDigestHour:         14,
		AfternoonDigestMinute:       0,
		ShiftNotificationsEnabled:   true,
		MoveRequestNotifEnabled:     true,
		Timezone:                    "America/New_York",
		OverdueMoveAlertsEnabled:    true,
		OverdueMoveCheckIntervalMin: 15,
		DueSoonAlertsEnabled:        true,
		DueSoonHoursBefore:          24,
		DailyMoveReportEnabled:      true,
		DailyMoveReportHour:         8,
		DailyMoveReportMinute:       0,
		DailyBinCheckEnabled:        true,
		DailyBinCheckHour:           9,
		DailyBinCheckMinute:         0,
		DailyBatteryReportEnabled:   true,
		DailyBatteryReportHour:      10,
		DailyBatteryReportMinute:    0,
	}
}

// NotificationLogEntry represents a single sent notification
type NotificationLogEntry struct {
	ID              string          `json:"id" db:"id"`
	Type            string          `json:"type" db:"type"`
	Title           string          `json:"title" db:"title"`
	Body            string          `json:"body" db:"body"`
	Data            json.RawMessage `json:"data" db:"data"`
	RecipientsCount int             `json:"recipients_count" db:"recipients_count"`
	CreatedAt       int64           `json:"created_at" db:"created_at"`
}

// GetNotificationSettings returns current notification preferences
func GetNotificationSettings(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		var configValue []byte
		err := db.QueryRow(`SELECT value FROM config WHERE key = 'notification_settings'`).Scan(&configValue)

		if err == sql.ErrNoRows || err != nil {
			// Return defaults if not configured yet
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(DefaultNotificationSettings())
			return
		}

		var settings NotificationSettings
		if err := json.Unmarshal(configValue, &settings); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(DefaultNotificationSettings())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(settings)
	}
}

// UpdateNotificationSettings saves notification preferences
func UpdateNotificationSettings(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		// Partial update (#14): start from the currently-stored settings (or defaults) so a
		// partial PATCH overrides ONLY the fields it sends — json.Decode into a pre-populated
		// struct leaves omitted fields at their existing values instead of blanking them to
		// Go zero-values (which the validators below would also reject, e.g. interval 0 < 1).
		settings := DefaultNotificationSettings()
		var existingRaw []byte
		if err := db.QueryRow(`SELECT value FROM config WHERE key = 'notification_settings'`).Scan(&existingRaw); err == nil {
			_ = json.Unmarshal(existingRaw, &settings) // keep defaults if the stored blob is unparseable
		}
		if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}

		// Validate
		if settings.DriftCheckIntervalMinutes < 1 {
			http.Error(w, `{"error":"Drift check interval must be at least 1 minute"}`, http.StatusBadRequest)
			return
		}
		if settings.DriftThresholdMeters < 50 {
			http.Error(w, `{"error":"Drift threshold must be at least 50 meters"}`, http.StatusBadRequest)
			return
		}
		if settings.MorningDigestHour < 0 || settings.MorningDigestHour > 23 {
			http.Error(w, `{"error":"Morning digest hour must be 0-23"}`, http.StatusBadRequest)
			return
		}
		if settings.AfternoonDigestHour < 0 || settings.AfternoonDigestHour > 23 {
			http.Error(w, `{"error":"Afternoon digest hour must be 0-23"}`, http.StatusBadRequest)
			return
		}
		if settings.MorningDigestMinute < 0 || settings.MorningDigestMinute > 59 {
			http.Error(w, `{"error":"Morning digest minute must be 0-59"}`, http.StatusBadRequest)
			return
		}
		if settings.AfternoonDigestMinute < 0 || settings.AfternoonDigestMinute > 59 {
			http.Error(w, `{"error":"Afternoon digest minute must be 0-59"}`, http.StatusBadRequest)
			return
		}
		if settings.Timezone != "" {
			if _, tzErr := time.LoadLocation(settings.Timezone); tzErr != nil {
				http.Error(w, `{"error":"Invalid timezone"}`, http.StatusBadRequest)
				return
			}
		} else {
			settings.Timezone = "America/New_York"
		}
		if settings.OverdueMoveCheckIntervalMin < 5 {
			http.Error(w, `{"error":"Overdue move check interval must be at least 5 minutes"}`, http.StatusBadRequest)
			return
		}
		if settings.DueSoonHoursBefore < 1 || settings.DueSoonHoursBefore > 168 {
			http.Error(w, `{"error":"Due soon hours must be between 1 and 168 (7 days)"}`, http.StatusBadRequest)
			return
		}
		if settings.DailyMoveReportHour < 0 || settings.DailyMoveReportHour > 23 {
			http.Error(w, `{"error":"Daily move report hour must be 0-23"}`, http.StatusBadRequest)
			return
		}
		if settings.DailyMoveReportMinute < 0 || settings.DailyMoveReportMinute > 59 {
			http.Error(w, `{"error":"Daily move report minute must be 0-59"}`, http.StatusBadRequest)
			return
		}
		if settings.DailyBinCheckHour < 0 || settings.DailyBinCheckHour > 23 {
			http.Error(w, `{"error":"Daily bin check hour must be 0-23"}`, http.StatusBadRequest)
			return
		}
		if settings.DailyBinCheckMinute < 0 || settings.DailyBinCheckMinute > 59 {
			http.Error(w, `{"error":"Daily bin check minute must be 0-59"}`, http.StatusBadRequest)
			return
		}
		if settings.DailyBatteryReportHour < 0 || settings.DailyBatteryReportHour > 23 {
			http.Error(w, `{"error":"Daily battery report hour must be 0-23"}`, http.StatusBadRequest)
			return
		}
		if settings.DailyBatteryReportMinute < 0 || settings.DailyBatteryReportMinute > 59 {
			http.Error(w, `{"error":"Daily battery report minute must be 0-59"}`, http.StatusBadRequest)
			return
		}

		valueJSON, err := json.Marshal(settings)
		if err != nil {
			http.Error(w, `{"error":"Failed to marshal settings"}`, http.StatusInternalServerError)
			return
		}

		_, err = db.Exec(`
			INSERT INTO config (key, value, updated_by, updated_at)
			VALUES ('notification_settings', $1::jsonb, 'admin', CURRENT_TIMESTAMP)
			ON CONFLICT (key) DO UPDATE SET
				value = $1::jsonb,
				updated_by = 'admin',
				updated_at = CURRENT_TIMESTAMP
		`, string(valueJSON))

		if err != nil {
			log.Printf("❌ Failed to save notification settings: %v", err)
			http.Error(w, `{"error":"Failed to save settings"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"settings": settings,
		})
	}
}

// GetNotificationLog returns paginated notification history
func GetNotificationLog(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit < 1 || limit > 100 {
			limit = 20
		}
		offset := (page - 1) * limit
		typeFilter := r.URL.Query().Get("type")

		var total int
		var notifications []NotificationLogEntry

		if typeFilter != "" {
			err := db.Get(&total, `SELECT COUNT(*) FROM notification_log WHERE type = $1`, typeFilter)
			if err != nil {
				log.Printf("❌ Failed to count notification log: %v", err)
				http.Error(w, `{"error":"Failed to query notification log"}`, http.StatusInternalServerError)
				return
			}
			err = db.Select(&notifications, `
				SELECT id, type, title, body, data, recipients_count, created_at
				FROM notification_log WHERE type = $1
				ORDER BY created_at DESC LIMIT $2 OFFSET $3
			`, typeFilter, limit, offset)
			if err != nil {
				log.Printf("❌ Failed to query notification log: %v", err)
				http.Error(w, `{"error":"Failed to query notification log"}`, http.StatusInternalServerError)
				return
			}
		} else {
			err := db.Get(&total, `SELECT COUNT(*) FROM notification_log`)
			if err != nil {
				log.Printf("❌ Failed to count notification log: %v", err)
				http.Error(w, `{"error":"Failed to query notification log"}`, http.StatusInternalServerError)
				return
			}
			err = db.Select(&notifications, `
				SELECT id, type, title, body, data, recipients_count, created_at
				FROM notification_log
				ORDER BY created_at DESC LIMIT $1 OFFSET $2
			`, limit, offset)
			if err != nil {
				log.Printf("❌ Failed to query notification log: %v", err)
				http.Error(w, `{"error":"Failed to query notification log"}`, http.StatusInternalServerError)
				return
			}
		}

		if notifications == nil {
			notifications = []NotificationLogEntry{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"notifications": notifications,
			"total":         total,
			"page":          page,
			"limit":         limit,
		})
	}
}

// LogNotification inserts a notification into the log table
func LogNotification(db *orgdb.DB, notifType, title, body string, data interface{}, recipientsCount int) {
	dataJSON, _ := json.Marshal(data)
	id := fmt.Sprintf("notif_%d", time.Now().UnixNano())

	_, err := db.Exec(`
		INSERT INTO notification_log (id, type, title, body, data, recipients_count, created_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
	`, id, notifType, title, body, string(dataJSON), recipientsCount, time.Now().Unix())

	if err != nil {
		log.Printf("❌ Failed to log notification: %v", err)
	}
}
