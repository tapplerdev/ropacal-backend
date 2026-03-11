package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// UserNotification represents a per-user notification row.
type UserNotification struct {
	ID                string          `json:"id" db:"id"`
	UserID            string          `json:"user_id" db:"user_id"`
	NotificationLogID *string         `json:"notification_log_id,omitempty" db:"notification_log_id"`
	Type              string          `json:"type" db:"type"`
	Title             string          `json:"title" db:"title"`
	Body              string          `json:"body" db:"body"`
	Data              json.RawMessage `json:"data" db:"data"`
	DeliveryStatus    string          `json:"delivery_status" db:"delivery_status"`
	ReadAt            *int64          `json:"read_at" db:"read_at"`
	CreatedAt         int64           `json:"created_at" db:"created_at"`
}

// GetUserNotifications returns paginated notifications for the current user.
func GetUserNotifications(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit < 1 || limit > 50 {
			limit = 20
		}
		offset := (page - 1) * limit

		var total int
		_ = db.Get(&total, `SELECT COUNT(*) FROM user_notifications WHERE user_id = $1`, user.UserID)

		var notifications []UserNotification
		_ = db.Select(&notifications, `
			SELECT id, user_id, notification_log_id, type, title, body, data, delivery_status, read_at, created_at
			FROM user_notifications
			WHERE user_id = $1
			ORDER BY created_at DESC
			LIMIT $2 OFFSET $3
		`, user.UserID, limit, offset)

		if notifications == nil {
			notifications = []UserNotification{}
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"notifications": notifications,
			"total":         total,
			"page":          page,
			"limit":         limit,
		})
	}
}

// GetUnreadCount returns the unread notification count for the current user.
func GetUnreadCount(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var count int
		_ = db.Get(&count, `SELECT COUNT(*) FROM user_notifications WHERE user_id = $1 AND read_at IS NULL`, user.UserID)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"unread_count": count,
		})
	}
}

// MarkNotificationRead marks a single notification as read.
func MarkNotificationRead(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		notifID := chi.URLParam(r, "id")
		now := time.Now().Unix()

		result, err := db.Exec(
			`UPDATE user_notifications SET read_at = $1 WHERE id = $2 AND user_id = $3 AND read_at IS NULL`,
			now, notifID, user.UserID,
		)
		if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to mark notification as read")
			return
		}

		rows, _ := result.RowsAffected()
		if rows == 0 {
			utils.RespondError(w, http.StatusNotFound, "Notification not found or already read")
			return
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
	}
}

// MarkAllNotificationsRead marks all notifications as read for the current user.
func MarkAllNotificationsRead(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		now := time.Now().Unix()
		result, _ := db.Exec(
			`UPDATE user_notifications SET read_at = $1 WHERE user_id = $2 AND read_at IS NULL`,
			now, user.UserID,
		)

		count, _ := result.RowsAffected()
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"status":       "ok",
			"marked_count": count,
		})
	}
}

// GetNotificationRecipients returns per-user delivery details for a notification log entry (admin only).
func GetNotificationRecipients(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logID := chi.URLParam(r, "id")

		type recipientRow struct {
			UserID         string  `json:"user_id" db:"user_id"`
			UserName       string  `json:"user_name" db:"user_name"`
			UserEmail      string  `json:"user_email" db:"user_email"`
			DeliveryStatus string  `json:"delivery_status" db:"delivery_status"`
			ReadAt         *int64  `json:"read_at" db:"read_at"`
			CreatedAt      int64   `json:"created_at" db:"created_at"`
		}

		var recipients []recipientRow
		err := db.Select(&recipients, `
			SELECT un.user_id, u.name AS user_name, u.email AS user_email,
			       un.delivery_status, un.read_at, un.created_at
			FROM user_notifications un
			JOIN users u ON un.user_id = u.id
			WHERE un.notification_log_id = $1
			ORDER BY un.created_at DESC
		`, logID)
		if err != nil {
			utils.RespondError(w, http.StatusInternalServerError, "Failed to query recipients")
			return
		}

		if recipients == nil {
			recipients = []recipientRow{}
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"recipients": recipients,
			"total":      len(recipients),
		})
	}
}
