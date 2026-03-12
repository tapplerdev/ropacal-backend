package services

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// notificationSettings holds all configurable notification preferences.
type notificationSettings struct {
	DriftAlertsEnabled        bool   `json:"drift_alerts_enabled"`
	DriftCheckIntervalMinutes int    `json:"drift_check_interval_minutes"`
	DriftThresholdMeters      int    `json:"drift_threshold_meters"`
	MorningDigestEnabled      bool   `json:"morning_digest_enabled"`
	MorningDigestHour         int    `json:"morning_digest_hour"`
	MorningDigestMinute       int    `json:"morning_digest_minute"`
	AfternoonDigestEnabled    bool   `json:"afternoon_digest_enabled"`
	AfternoonDigestHour       int    `json:"afternoon_digest_hour"`
	AfternoonDigestMinute     int    `json:"afternoon_digest_minute"`
	ShiftNotificationsEnabled bool   `json:"shift_notifications_enabled"`
	MoveRequestNotifEnabled   bool   `json:"move_request_notifications_enabled"`
	Timezone                  string `json:"timezone"`
	OverdueMoveAlertsEnabled    bool `json:"overdue_move_alerts_enabled"`
	OverdueMoveCheckIntervalMin int  `json:"overdue_move_check_interval_minutes"`
	DueSoonAlertsEnabled        bool `json:"due_soon_alerts_enabled"`
	DueSoonHoursBefore          int  `json:"due_soon_hours_before"`
	// Daily reports (replace morning/afternoon digest)
	DailyMoveReportEnabled bool `json:"daily_move_report_enabled"`
	DailyMoveReportHour    int  `json:"daily_move_report_hour"`
	DailyMoveReportMinute  int  `json:"daily_move_report_minute"`
	DailyBinCheckEnabled   bool `json:"daily_bin_check_enabled"`
	DailyBinCheckHour      int  `json:"daily_bin_check_hour"`
	DailyBinCheckMinute    int  `json:"daily_bin_check_minute"`
}

func defaultNotificationSettings() notificationSettings {
	return notificationSettings{
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
	}
}

func loadNotificationSettings(db *sqlx.DB) notificationSettings {
	var configValue []byte
	err := db.QueryRow(`SELECT value FROM config WHERE key = 'notification_settings'`).Scan(&configValue)
	if err == sql.ErrNoRows {
		log.Println("ℹ️  [Settings] No notification_settings in config table — using defaults (first run?)")
		return defaultNotificationSettings()
	}
	if err != nil {
		log.Printf("❌ [Settings] ERROR: Failed to load notification_settings from config table: %v", err)
		log.Printf("❌ [Settings] This likely means the config table does not exist! Check your migrations.")
		return defaultNotificationSettings()
	}

	var settings notificationSettings
	if err := json.Unmarshal(configValue, &settings); err != nil {
		log.Printf("❌ [Settings] ERROR: Failed to parse notification_settings JSON: %v", err)
		log.Printf("❌ [Settings] Raw value: %s", string(configValue))
		return defaultNotificationSettings()
	}

	// Apply defaults for new fields that may not exist in stored JSON
	if settings.Timezone == "" {
		settings.Timezone = "America/New_York"
	}
	if settings.OverdueMoveCheckIntervalMin == 0 {
		settings.OverdueMoveCheckIntervalMin = 15
	}
	if settings.DueSoonHoursBefore == 0 {
		settings.DueSoonHoursBefore = 24
	}

	// Migrate old morning/afternoon digest to new daily move report fields
	if settings.DailyMoveReportHour == 0 && settings.DailyMoveReportMinute == 0 && !settings.DailyMoveReportEnabled {
		if settings.MorningDigestEnabled {
			settings.DailyMoveReportEnabled = true
			settings.DailyMoveReportHour = settings.MorningDigestHour
			settings.DailyMoveReportMinute = settings.MorningDigestMinute
		} else {
			// First run or old settings — use defaults
			settings.DailyMoveReportEnabled = true
			settings.DailyMoveReportHour = 8
		}
	}
	if settings.DailyBinCheckHour == 0 && settings.DailyBinCheckMinute == 0 && !settings.DailyBinCheckEnabled {
		settings.DailyBinCheckEnabled = true
		settings.DailyBinCheckHour = 9
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

// preferenceCategory maps a notification type string to the user_notification_preferences column.
func preferenceCategory(notifType string) string {
	switch {
	case strings.HasPrefix(notifType, "bin_drift"):
		return "drift_alerts"
	case strings.HasPrefix(notifType, "digest_"):
		return "digests"
	case notifType == "daily_move_report":
		return "digests"
	case notifType == "daily_bin_check_report":
		return "bin_check_reports"
	case strings.Contains(notifType, "shift") || notifType == "route_assigned":
		return "shift_events"
	case notifType == "move_request_overdue":
		return "overdue_move_alerts"
	case notifType == "move_request_due_soon":
		return "due_soon_alerts"
	case strings.Contains(notifType, "move_request"):
		return "move_requests"
	case strings.Contains(notifType, "potential_location"):
		return "move_requests"
	default:
		return ""
	}
}

// GetAdminUserIDs returns all admin user IDs.
func GetAdminUserIDs(db *sqlx.DB) ([]string, error) {
	var ids []string
	err := db.Select(&ids, `SELECT id FROM users WHERE role = 'admin'`)
	return ids, err
}

// CreateNotificationForUsers is the central function that:
// 1. Inserts into notification_log (global audit)
// 2. For each recipient, checks preferences, inserts into user_notifications
// Note: Real-time Centrifugo events are published by the callers (digest_scheduler,
// move_request_monitor, etc.) with the semantic event type, NOT here.
func CreateNotificationForUsers(
	db *sqlx.DB,
	recipientUserIDs []string,
	notifType, title, body string,
	data interface{},
) (logID string, userNotifIDs []string) {
	dataJSON, _ := json.Marshal(data)
	logID = fmt.Sprintf("notif_%d", time.Now().UnixNano())
	now := time.Now().Unix()

	// 1. Insert global log
	_, err := db.Exec(`
		INSERT INTO notification_log (id, type, title, body, data, recipients_count, created_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
	`, logID, notifType, title, body, string(dataJSON), len(recipientUserIDs), now)
	if err != nil {
		log.Printf("⚠️ Failed to log notification: %v", err)
		return logID, nil
	}

	// 2. Determine preference column
	prefCol := preferenceCategory(notifType)

	// 3. For each recipient, check preference and insert
	for _, userID := range recipientUserIDs {
		if prefCol != "" {
			var enabled bool
			err := db.QueryRow(
				fmt.Sprintf(`SELECT COALESCE((SELECT %s FROM user_notification_preferences WHERE user_id = $1), true)`, prefCol),
				userID,
			).Scan(&enabled)
			if err == nil && !enabled {
				continue
			}
		}

		notifID := fmt.Sprintf("un_%d", time.Now().UnixNano())
		_, insertErr := db.Exec(`
			INSERT INTO user_notifications (id, user_id, notification_log_id, type, title, body, data, delivery_status, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, 'pending', $8)
		`, notifID, userID, logID, notifType, title, body, string(dataJSON), now)
		if insertErr != nil {
			log.Printf("⚠️ Failed to insert user notification for %s: %v", userID, insertErr)
			continue
		}
		userNotifIDs = append(userNotifIDs, notifID)
	}

	return logID, userNotifIDs
}

// UpdateDeliveryStatus updates the delivery status of a user notification.
func UpdateDeliveryStatus(db *sqlx.DB, notifID, status string) {
	_, err := db.Exec(`UPDATE user_notifications SET delivery_status = $1 WHERE id = $2`, status, notifID)
	if err != nil {
		log.Printf("⚠️ Failed to update delivery status for %s: %v", notifID, err)
	}
}
