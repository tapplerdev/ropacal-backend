package services

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jmoiron/sqlx"
)

// notificationSettings holds all configurable notification preferences.
type notificationSettings struct {
	DriftAlertsEnabled        bool `json:"drift_alerts_enabled"`
	DriftCheckIntervalMinutes int  `json:"drift_check_interval_minutes"`
	DriftThresholdMeters      int  `json:"drift_threshold_meters"`
	MorningDigestEnabled      bool `json:"morning_digest_enabled"`
	MorningDigestHour         int  `json:"morning_digest_hour"`
	AfternoonDigestEnabled    bool `json:"afternoon_digest_enabled"`
	AfternoonDigestHour       int  `json:"afternoon_digest_hour"`
	ShiftNotificationsEnabled bool `json:"shift_notifications_enabled"`
	MoveRequestNotifEnabled   bool `json:"move_request_notifications_enabled"`
}

func defaultNotificationSettings() notificationSettings {
	return notificationSettings{
		DriftAlertsEnabled:        true,
		DriftCheckIntervalMinutes: 5,
		DriftThresholdMeters:      500,
		MorningDigestEnabled:      true,
		MorningDigestHour:         8,
		AfternoonDigestEnabled:    true,
		AfternoonDigestHour:       14,
		ShiftNotificationsEnabled: true,
		MoveRequestNotifEnabled:   true,
	}
}

func loadNotificationSettings(db *sqlx.DB) notificationSettings {
	var configValue []byte
	err := db.QueryRow(`SELECT value FROM config WHERE key = 'notification_settings'`).Scan(&configValue)
	if err == sql.ErrNoRows || err != nil {
		return defaultNotificationSettings()
	}

	var settings notificationSettings
	if err := json.Unmarshal(configValue, &settings); err != nil {
		return defaultNotificationSettings()
	}
	return settings
}

func logNotification(db *sqlx.DB, notifType, title, body string, data interface{}, recipientsCount int) {
	dataJSON, _ := json.Marshal(data)
	id := fmt.Sprintf("notif_%d", time.Now().UnixNano())

	_, err := db.Exec(`
		INSERT INTO notification_log (id, type, title, body, data, recipients_count, created_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
	`, id, notifType, title, body, string(dataJSON), recipientsCount, time.Now().Unix())

	if err != nil {
		log.Printf("⚠️ Failed to log notification: %v", err)
	}
}
