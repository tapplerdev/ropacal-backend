package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"ropacal-backend/internal/services/centrifugo"

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

// preferenceCategory maps a notification type string to the user_notification_preferences column.
func preferenceCategory(notifType string) string {
	switch {
	case strings.HasPrefix(notifType, "bin_drift"):
		return "drift_alerts"
	case strings.HasPrefix(notifType, "digest_"):
		return "digests"
	case strings.Contains(notifType, "shift"):
		return "shift_events"
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
// 3. Publishes a Centrifugo event for real-time delivery
func CreateNotificationForUsers(
	db *sqlx.DB,
	centrifugoClient *centrifugo.Client,
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

		// Publish real-time event
		if centrifugoClient != nil {
			_ = centrifugoClient.PublishCompanyEvent(context.Background(), "user_notification_created", map[string]interface{}{
				"id":         notifID,
				"user_id":    userID,
				"type":       notifType,
				"title":      title,
				"body":       body,
				"data":       data,
				"created_at": now,
			})
		}
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
