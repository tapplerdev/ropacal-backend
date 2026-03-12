package handlers

import (
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
func GetNotificationSettings(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
func UpdateNotificationSettings(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var settings NotificationSettings
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
func GetNotificationLog(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
func LogNotification(db *sqlx.DB, notifType, title, body string, data interface{}, recipientsCount int) {
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
