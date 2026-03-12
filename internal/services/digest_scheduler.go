package services

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"ropacal-backend/internal/services/centrifugo"

	"github.com/jmoiron/sqlx"
)

// DigestScheduler sends daily summary notifications to admins at 8 AM and 2 PM.
// It checks overdue/urgent move requests and warehouse bins, then sends FCM
// multicast pushes. Uses the config table for idempotency.
type DigestScheduler struct {
	db               *sqlx.DB
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	ticker           *time.Ticker
	stopChan         chan bool
}

// DigestResult contains the outcome of a digest run.
type DigestResult struct {
	AlreadySent    bool   `json:"already_sent"`
	Window         string `json:"window"`
	OverdueCount   int    `json:"overdue_count"`
	UrgentCount    int    `json:"urgent_count"`
	SoonCount      int    `json:"soon_count"`
	WarehouseCount int    `json:"warehouse_count"`
	TokensSent     int    `json:"tokens_sent"`
}

// NewDigestScheduler creates a new digest scheduler that checks every minute.
func NewDigestScheduler(db *sqlx.DB, fcmService *FCMService, centrifugoClient *centrifugo.Client) *DigestScheduler {
	return &DigestScheduler{
		db:               db,
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		ticker:           time.NewTicker(1 * time.Minute),
		stopChan:         make(chan bool),
	}
}

// Start begins the background scheduler goroutine.
func (s *DigestScheduler) Start() {
	log.Println("📬 [Digest] Starting daily digest scheduler (minute-level check)")

	go func() {
		// Check immediately on startup
		s.checkAndSend()

		for {
			select {
			case <-s.stopChan:
				log.Println("🛑 [Digest] Stopping...")
				return
			case <-s.ticker.C:
				s.checkAndSend()
			}
		}
	}()
}

// Stop halts the scheduler.
func (s *DigestScheduler) Stop() {
	s.ticker.Stop()
	s.stopChan <- true
}

// checkAndSend checks the current hour and sends if it's a digest window.
func (s *DigestScheduler) checkAndSend() {
	settings := loadNotificationSettings(s.db)

	// Load timezone from settings (default: America/New_York)
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		log.Printf("❌ [Digest] Invalid timezone %q, falling back to America/New_York: %v", settings.Timezone, err)
		loc, _ = time.LoadLocation("America/New_York")
	}

	now := time.Now().In(loc)
	hour := now.Hour()
	minute := now.Minute()

	log.Printf("🕐 [Digest] Time check: current=%d:%02d (%s), morning=%d:%02d (enabled=%v), afternoon=%d:%02d (enabled=%v)",
		hour, minute, settings.Timezone,
		settings.MorningDigestHour, settings.MorningDigestMinute, settings.MorningDigestEnabled,
		settings.AfternoonDigestHour, settings.AfternoonDigestMinute, settings.AfternoonDigestEnabled)

	var window string
	if hour == settings.MorningDigestHour && minute == settings.MorningDigestMinute && settings.MorningDigestEnabled {
		window = "morning"
	} else if hour == settings.AfternoonDigestHour && minute == settings.AfternoonDigestMinute && settings.AfternoonDigestEnabled {
		window = "afternoon"
	} else {
		log.Printf("🕐 [Digest] No match — skipping")
		return
	}

	result, err := s.RunDigest(window)
	if err != nil {
		log.Printf("❌ [Digest] Failed to send %s digest: %v", window, err)
		return
	}

	if result.AlreadySent {
		log.Printf("📬 [Digest] %s digest already sent today, skipping", window)
	} else {
		log.Printf("✅ [Digest] %s digest sent — overdue:%d urgent:%d soon:%d warehouse:%d tokens:%d",
			window, result.OverdueCount, result.UrgentCount, result.SoonCount, result.WarehouseCount, result.TokensSent)
	}
}

// RunDigest executes the digest logic. Can be called from the scheduler or the HTTP endpoint.
// The window parameter should be "morning" or "afternoon".
// If force is true, the idempotency check is skipped (useful for testing).
func (s *DigestScheduler) RunDigest(window string, force ...bool) (*DigestResult, error) {
	ctx := context.Background()

	// Use timezone-aware date for idempotency
	settings := loadNotificationSettings(s.db)
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc, _ = time.LoadLocation("America/New_York")
	}
	today := time.Now().In(loc).Format("2006-01-02")
	configKey := fmt.Sprintf("last_%s_digest", window)

	// Step 0: Idempotency check — has this window already been sent today?
	skipDedup := len(force) > 0 && force[0]
	if !skipDedup {
		var lastSent string
		dedupErr := s.db.QueryRow(`SELECT value->>'date' FROM config WHERE key = $1`, configKey).Scan(&lastSent)
		if dedupErr == nil && lastSent == today {
			return &DigestResult{AlreadySent: true, Window: window}, nil
		}
	}

	// Step 1: Query active move requests and compute urgency
	type moveRow struct {
		Status        string `db:"status"`
		ScheduledDate int64  `db:"scheduled_date"`
	}
	var moves []moveRow
	err = s.db.Select(&moves, `
		SELECT status, scheduled_date FROM bin_move_requests
		WHERE status IN ('pending', 'in_progress')
	`)
	if err != nil {
		return nil, fmt.Errorf("query move requests: %w", err)
	}

	now := time.Now().Unix()
	overdueCount, urgentCount, soonCount := 0, 0, 0
	for _, m := range moves {
		hoursUntil := float64(m.ScheduledDate-now) / 3600.0
		if hoursUntil < 0 {
			overdueCount++
		} else if hoursUntil < 24 {
			urgentCount++
		} else if hoursUntil < 72 {
			soonCount++
		}
	}

	// Step 2: Query warehouse bins
	var warehouseCount int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM bins WHERE status = 'in_storage'`).Scan(&warehouseCount)
	if err != nil {
		return nil, fmt.Errorf("query warehouse bins: %w", err)
	}

	// Step 3: Nothing to report?
	if overdueCount == 0 && urgentCount == 0 && soonCount == 0 && warehouseCount == 0 {
		if !skipDedup {
			s.updateConfigTimestamp(configKey, today)
		}
		return &DigestResult{Window: window}, nil
	}

	// Step 4: Get admin FCM tokens
	var tokens []string
	err = s.db.Select(&tokens, `
		SELECT ft.token FROM fcm_tokens ft
		JOIN users u ON ft.user_id = u.id
		WHERE u.role = 'admin'
	`)
	if err != nil {
		return nil, fmt.Errorf("query admin tokens: %w", err)
	}

	if len(tokens) == 0 {
		log.Println("⚠️  [Digest] No admin FCM tokens found, skipping push")
		if !skipDedup {
			s.updateConfigTimestamp(configKey, today)
		}
		return &DigestResult{
			Window:         window,
			OverdueCount:   overdueCount,
			UrgentCount:    urgentCount,
			SoonCount:      soonCount,
			WarehouseCount: warehouseCount,
		}, nil
	}

	// Step 5: Send FCM notifications (only for non-zero counts)
	if s.fcmService == nil {
		log.Println("⚠️  [Digest] FCM service is nil — push notifications will NOT be sent. Notifications still saved to DB.")
	}
	if s.fcmService != nil {
		if overdueCount > 0 {
			title := fmt.Sprintf("%d Overdue Move Request%s", overdueCount, plural(overdueCount))
			body := "These moves are past their scheduled date."
			data := map[string]string{
				"type":           "digest_overdue_moves",
				"overdue_count":  strconv.Itoa(overdueCount),
				"deep_link":      "/manager/move-requests",
				"subtitle":       fmt.Sprintf("%s Digest", strings.ToUpper(window[:1])+window[1:]),
			}
			if err := s.fcmService.SendMulticast(tokens, title, body, data); err != nil {
				log.Printf("⚠️  [Digest] Failed to send overdue notification: %v", err)
			}
			adminIDs, _ := GetAdminUserIDs(s.db)
			CreateNotificationForUsers(s.db, s.centrifugoClient, adminIDs, "digest_overdue_moves", title, body, data)
		}

		if urgentCount > 0 || soonCount > 0 {
			total := urgentCount + soonCount
			title := fmt.Sprintf("%d Move Request%s Due Soon", total, plural(total))
			parts := []string{}
			if urgentCount > 0 {
				parts = append(parts, fmt.Sprintf("%d urgent (< 24h)", urgentCount))
			}
			if soonCount > 0 {
				parts = append(parts, fmt.Sprintf("%d due within 3 days", soonCount))
			}
			body := fmt.Sprintf("%s.", joinParts(parts))
			data := map[string]string{
				"type":          "digest_upcoming_moves",
				"urgent_count":  strconv.Itoa(urgentCount),
				"soon_count":    strconv.Itoa(soonCount),
				"deep_link":     "/manager/move-requests",
				"subtitle":      fmt.Sprintf("%s Digest", strings.ToUpper(window[:1])+window[1:]),
			}
			if err := s.fcmService.SendMulticast(tokens, title, body, data); err != nil {
				log.Printf("⚠️  [Digest] Failed to send upcoming notification: %v", err)
			}
			adminIDs, _ := GetAdminUserIDs(s.db)
			CreateNotificationForUsers(s.db, s.centrifugoClient, adminIDs, "digest_upcoming_moves", title, body, data)
		}

		if warehouseCount > 0 {
			title := fmt.Sprintf("%d Bin%s in Warehouse", warehouseCount, plural(warehouseCount))
			body := "Awaiting redeployment."
			data := map[string]string{
				"type":             "digest_warehouse_bins",
				"warehouse_count":  strconv.Itoa(warehouseCount),
				"deep_link":        "/manager/move-requests",
				"subtitle":         fmt.Sprintf("%s Digest", strings.ToUpper(window[:1])+window[1:]),
			}
			if err := s.fcmService.SendMulticast(tokens, title, body, data); err != nil {
				log.Printf("⚠️  [Digest] Failed to send warehouse notification: %v", err)
			}
			adminIDs, _ := GetAdminUserIDs(s.db)
			CreateNotificationForUsers(s.db, s.centrifugoClient, adminIDs, "digest_warehouse_bins", title, body, data)
		}
	}

	// Step 6: Also publish to Centrifugo for any connected dashboard/app
	if s.centrifugoClient != nil {
		digestData := map[string]interface{}{
			"window":          window,
			"overdue_count":   overdueCount,
			"urgent_count":    urgentCount,
			"soon_count":      soonCount,
			"warehouse_count": warehouseCount,
		}

		if overdueCount > 0 {
			_ = s.centrifugoClient.PublishCompanyEvent(ctx, "digest_overdue_moves", digestData)
		}
		if urgentCount > 0 || soonCount > 0 {
			_ = s.centrifugoClient.PublishCompanyEvent(ctx, "digest_upcoming_moves", digestData)
		}
		if warehouseCount > 0 {
			_ = s.centrifugoClient.PublishCompanyEvent(ctx, "digest_warehouse_bins", digestData)
		}
	}

	// Step 7: Mark as sent (skip if force/test so the real scheduled run still fires)
	if !skipDedup {
		s.updateConfigTimestamp(configKey, today)
	}

	return &DigestResult{
		Window:         window,
		OverdueCount:   overdueCount,
		UrgentCount:    urgentCount,
		SoonCount:      soonCount,
		WarehouseCount: warehouseCount,
		TokensSent:     len(tokens),
	}, nil
}

// updateConfigTimestamp upserts the config key with today's date.
func (s *DigestScheduler) updateConfigTimestamp(key, date string) {
	value := fmt.Sprintf(`{"date": "%s", "timestamp": %d}`, date, time.Now().Unix())
	_, err := s.db.Exec(`
		INSERT INTO config (key, value, updated_by, updated_at)
		VALUES ($1, $2::jsonb, 'system', CURRENT_TIMESTAMP)
		ON CONFLICT (key)
		DO UPDATE SET value = $2::jsonb, updated_by = 'system', updated_at = CURRENT_TIMESTAMP
	`, key, value)
	if err != nil {
		log.Printf("⚠️  [Digest] Failed to update config %s: %v", key, err)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func joinParts(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return parts[0] + ", " + parts[1]
}
