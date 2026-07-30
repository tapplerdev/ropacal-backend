package services

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/services/centrifugo"

	"github.com/jmoiron/sqlx"
)

// MoveRequestMonitor periodically checks for overdue and due-soon move requests
// and sends individual real-time alerts to admins. Follows the same pattern as AirtagMonitor.
type MoveRequestMonitor struct {
	root             *sqlx.DB  // unscoped pool — org enumeration only
	db               *orgdb.DB // org-bound handle for the CURRENT per-org pass (see checkMoveRequests)
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	ticker           *time.Ticker
	stopChan         chan bool
}

// NewMoveRequestMonitor creates a new monitor that checks every 15 minutes (configurable via settings).
func NewMoveRequestMonitor(db *sqlx.DB, fcmService *FCMService, centrifugoClient *centrifugo.Client) *MoveRequestMonitor {
	return &MoveRequestMonitor{
		root:             db,
		db:               orgdb.Passthrough(db),
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		ticker:           time.NewTicker(15 * time.Minute),
		stopChan:         make(chan bool),
	}
}

// Start begins the background monitoring goroutine.
func (m *MoveRequestMonitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	log.Println("📋 [MoveRequestMonitor] Starting move request monitor (checks for overdue/due-soon)")

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Check immediately on startup
		m.checkMoveRequests()

		for {
			select {
			case <-ctx.Done():
				log.Println("🛑 [MoveRequestMonitor] Stopping...")
				return
			case <-m.ticker.C:
				m.checkMoveRequests()
			}
		}
	}()
}

// Stop halts the monitor.
func (m *MoveRequestMonitor) Stop() {
	m.ticker.Stop()
	m.stopChan <- true
}

// checkMoveRequests runs one overdue/due-soon sweep per active organization.
// Settings, the actionable-move working set, the 24h dedup, and the admin
// recipient list all resolve through the org-bound handle, so each tenant's
// alerts reach only that tenant's admins.
func (m *MoveRequestMonitor) checkMoveRequests() {
	orgdb.ForEachActiveOrg(m.root, "MoveRequestMonitor", func(d *orgdb.DB) error {
		o := *m // shallow copy: shared clients/ticker, per-org db
		o.db = d
		o.checkMoveRequestsOrg()
		return nil
	})
}

func (m *MoveRequestMonitor) checkMoveRequestsOrg() {
	ctx := context.Background()

	// Load settings
	settings := loadNotificationSettings(m.db)

	if !settings.OverdueMoveAlertsEnabled && !settings.DueSoonAlertsEnabled {
		return
	}

	dueSoonThreshold := float64(settings.DueSoonHoursBefore)

	// The domain owns "what's overdue / due soon" (query + classification); the
	// watcher owns the alert policy (which buckets are enabled) and the fan-out.
	overdueAlerts, dueSoonAlerts, err := moverequest.NewService(moverequest.NewSQLStore(m.db)).FindActionable(dueSoonThreshold)
	if err != nil {
		log.Printf("❌ [MoveRequestMonitor] Failed to load move requests: %v", err)
		return
	}
	if !settings.OverdueMoveAlertsEnabled {
		overdueAlerts = nil
	}
	if !settings.DueSoonAlertsEnabled {
		dueSoonAlerts = nil
	}

	now := time.Now().Unix()

	// Send overdue alerts (with dedup — don't re-alert for the same move within 24h)
	for _, move := range overdueAlerts {
		if m.wasAlertedRecently(move.ID, "move_request_overdue") {
			continue
		}

		hoursOverdue := float64(now-move.ScheduledDate) / 3600.0
		title := fmt.Sprintf("Move Request Overdue: Bin #%d", move.BinNumber)
		body := fmt.Sprintf("This move is %.0f hours past its scheduled date.", hoursOverdue)
		data := map[string]string{
			"type":            "move_request_overdue",
			"move_request_id": move.ID,
			"bin_id":          move.BinID,
			"bin_number":      strconv.Itoa(move.BinNumber),
			"hours_overdue":   fmt.Sprintf("%.0f", hoursOverdue),
			"deep_link":       "/manager/move-requests",
			"subtitle":        "Overdue Alert",
		}

		m.sendAlert(ctx, "move_request_overdue", title, body, data)
		log.Printf("🚨 [MoveRequestMonitor] Overdue alert sent: Bin #%d (%.0fh overdue)", move.BinNumber, hoursOverdue)
	}

	// Send due-soon alerts (with dedup)
	for _, move := range dueSoonAlerts {
		if m.wasAlertedRecently(move.ID, "move_request_due_soon") {
			continue
		}

		hoursUntil := float64(move.ScheduledDate-now) / 3600.0
		title := fmt.Sprintf("Move Due Soon: Bin #%d", move.BinNumber)
		body := fmt.Sprintf("This move is due in %.0f hours.", hoursUntil)
		data := map[string]string{
			"type":            "move_request_due_soon",
			"move_request_id": move.ID,
			"bin_id":          move.BinID,
			"bin_number":      strconv.Itoa(move.BinNumber),
			"hours_until":     fmt.Sprintf("%.0f", hoursUntil),
			"deep_link":       "/manager/move-requests",
			"subtitle":        "Upcoming Alert",
		}

		m.sendAlert(ctx, "move_request_due_soon", title, body, data)
		log.Printf("⏰ [MoveRequestMonitor] Due-soon alert sent: Bin #%d (%.0fh remaining)", move.BinNumber, hoursUntil)
	}
}

// wasAlertedRecently checks if an alert of this type was already sent for this move request in the last 24h.
func (m *MoveRequestMonitor) wasAlertedRecently(moveRequestID, notifType string) bool {
	cutoff := time.Now().Unix() - 24*3600 // 24 hours ago
	var count int
	err := m.db.QueryRow(`
		SELECT COUNT(*) FROM notification_log
		WHERE type = $1
		  AND data->>'move_request_id' = $2
		  AND created_at > $3
	`, notifType, moveRequestID, cutoff).Scan(&count)
	if err != nil {
		log.Printf("⚠️  [MoveRequestMonitor] Dedup query failed: %v — sending anyway", err)
		return false
	}
	return count > 0
}

// sendAlert sends an alert to all admins via FCM + CreateNotificationForUsers + Centrifugo.
func (m *MoveRequestMonitor) sendAlert(ctx context.Context, notifType, title, body string, data map[string]string) {
	// Get admin tokens for FCM
	var tokens []string
	err := m.db.Select(&tokens, `
		SELECT ft.token FROM fcm_tokens ft
		JOIN users u ON ft.user_id = u.id
		WHERE u.role = 'admin'
		  AND u.email NOT LIKE '%@binly-platform.internal'
	`)
	if err != nil {
		log.Printf("⚠️  [MoveRequestMonitor] Failed to query admin tokens: %v", err)
	}

	// Send FCM push
	if m.fcmService != nil && len(tokens) > 0 {
		if sendErr := m.fcmService.SendMulticast(tokens, title, body, data); sendErr != nil {
			log.Printf("⚠️  [MoveRequestMonitor] Failed to send FCM: %v", sendErr)
		}
	} else if m.fcmService == nil {
		log.Println("⚠️  [MoveRequestMonitor] FCM service is nil — push not sent")
	}

	// Publish to Centrifugo for real-time dashboard updates
	if m.centrifugoClient != nil {
		payload := map[string]interface{}{
			"type":  notifType,
			"title": title,
			"body":  body,
			"data":  data,
		}
		if pubErr := m.centrifugoClient.PublishCompanyEvent(ctx, m.db.OrgID(), notifType, payload); pubErr != nil {
			log.Printf("⚠️  [MoveRequestMonitor] Failed to publish to Centrifugo: %v", pubErr)
		}
	}

	// Create per-user notifications for admins
	adminIDs, _ := GetAdminUserIDs(m.db)
	CreateNotificationForUsers(m.db, adminIDs, notifType, title, body, data)
}
