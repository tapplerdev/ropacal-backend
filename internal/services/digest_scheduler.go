package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ropacal-backend/internal/services/centrifugo"

	"github.com/jmoiron/sqlx"
)

// DigestScheduler sends daily report notifications to admins.
// - Daily Move Report: consolidates overdue/urgent/soon moves + warehouse bins into ONE notification with snapshot
// - Daily Bin Check Report: bins not checked in 7+ days, grouped by severity
type DigestScheduler struct {
	db               *sqlx.DB
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	bridgeURL        string
	ticker           *time.Ticker
	stopChan         chan bool
}

// DigestResult contains the outcome of a report run.
type DigestResult struct {
	AlreadySent    bool   `json:"already_sent"`
	Window         string `json:"window"`
	OverdueCount   int    `json:"overdue_count"`
	UrgentCount    int    `json:"urgent_count"`
	SoonCount      int    `json:"soon_count"`
	WarehouseCount int    `json:"warehouse_count"`
	CriticalBins   int    `json:"critical_bins,omitempty"`
	OverdueBins    int    `json:"overdue_bins,omitempty"`
	TokensSent     int    `json:"tokens_sent"`
}

// NewDigestScheduler creates a new digest scheduler that checks every minute.
func NewDigestScheduler(db *sqlx.DB, fcmService *FCMService, centrifugoClient *centrifugo.Client) *DigestScheduler {
	return &DigestScheduler{
		db:               db,
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		bridgeURL:        os.Getenv("FINDMY_BRIDGE_URL"),
		ticker:           time.NewTicker(1 * time.Minute),
		stopChan:         make(chan bool),
	}
}

// Start begins the background scheduler goroutine.
func (s *DigestScheduler) Start() {
	log.Println("📬 [DailyReport] Starting daily report scheduler (minute-level check)")

	go func() {
		s.checkAndSend()

		for {
			select {
			case <-s.stopChan:
				log.Println("🛑 [DailyReport] Stopping...")
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

// checkAndSend checks the current time against configured report times.
func (s *DigestScheduler) checkAndSend() {
	settings := loadNotificationSettings(s.db)

	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		log.Printf("❌ [DailyReport] Invalid timezone %q, falling back to America/New_York: %v", settings.Timezone, err)
		loc, _ = time.LoadLocation("America/New_York")
	}

	now := time.Now().In(loc)
	hour := now.Hour()
	minute := now.Minute()

	log.Printf("🕐 [DailyReport] Time check: current=%d:%02d (%s), move=%d:%02d (on=%v), bincheck=%d:%02d (on=%v), battery=%d:%02d (on=%v)",
		hour, minute, settings.Timezone,
		settings.DailyMoveReportHour, settings.DailyMoveReportMinute, settings.DailyMoveReportEnabled,
		settings.DailyBinCheckHour, settings.DailyBinCheckMinute, settings.DailyBinCheckEnabled,
		settings.DailyBatteryReportHour, settings.DailyBatteryReportMinute, settings.DailyBatteryReportEnabled)

	// Check daily move report
	if hour == settings.DailyMoveReportHour && minute == settings.DailyMoveReportMinute && settings.DailyMoveReportEnabled {
		result, err := s.RunDailyMoveReport()
		if err != nil {
			log.Printf("❌ [DailyReport] Failed to send move report: %v", err)
		} else if result.AlreadySent {
			log.Printf("📬 [DailyReport] Move report already sent today, skipping")
		} else {
			log.Printf("✅ [DailyReport] Move report sent — overdue:%d urgent:%d soon:%d warehouse:%d tokens:%d",
				result.OverdueCount, result.UrgentCount, result.SoonCount, result.WarehouseCount, result.TokensSent)
		}
	}

	// Check daily bin check report
	if hour == settings.DailyBinCheckHour && minute == settings.DailyBinCheckMinute && settings.DailyBinCheckEnabled {
		result, err := s.RunDailyBinCheckReport()
		if err != nil {
			log.Printf("❌ [DailyReport] Failed to send bin check report: %v", err)
		} else if result.AlreadySent {
			log.Printf("📬 [DailyReport] Bin check report already sent today, skipping")
		} else {
			log.Printf("✅ [DailyReport] Bin check report sent — critical:%d overdue:%d tokens:%d",
				result.CriticalBins, result.OverdueBins, result.TokensSent)
		}
	}

	// Check daily battery report
	if hour == settings.DailyBatteryReportHour && minute == settings.DailyBatteryReportMinute && settings.DailyBatteryReportEnabled {
		result, err := s.RunDailyBatteryReport()
		if err != nil {
			log.Printf("❌ [DailyReport] Failed to send battery report: %v", err)
		} else if result.AlreadySent {
			log.Printf("📬 [DailyReport] Battery report already sent today, skipping")
		} else {
			log.Printf("✅ [DailyReport] Battery report sent — critical:%d low:%d tokens:%d",
				result.CriticalBins, result.OverdueBins, result.TokensSent)
		}
	}

	// Backward compat: also check old morning/afternoon digest times
	if hour == settings.MorningDigestHour && minute == settings.MorningDigestMinute && settings.MorningDigestEnabled &&
		!(hour == settings.DailyMoveReportHour && minute == settings.DailyMoveReportMinute && settings.DailyMoveReportEnabled) {
		log.Printf("🔄 [DailyReport] Legacy morning digest time hit — running as daily move report")
		s.RunDailyMoveReport()
	}
}

// RunDailyMoveReport sends ONE consolidated notification with overdue, upcoming, and warehouse data + snapshot.
func (s *DigestScheduler) RunDailyMoveReport(force ...bool) (*DigestResult, error) {
	ctx := context.Background()

	settings := loadNotificationSettings(s.db)
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc, _ = time.LoadLocation("America/New_York")
	}
	today := time.Now().In(loc).Format("2006-01-02")
	configKey := "last_daily_move_report"

	// Idempotency check
	skipDedup := len(force) > 0 && force[0]
	if !skipDedup {
		var lastSent string
		dedupErr := s.db.QueryRow(`SELECT value->>'date' FROM config WHERE key = $1`, configKey).Scan(&lastSent)
		if dedupErr == nil && lastSent == today {
			return &DigestResult{AlreadySent: true, Window: "daily_move_report"}, nil
		}
	}

	// Query move requests with bin details for snapshot
	type moveRow struct {
		ID              string  `db:"id"`
		BinID           string  `db:"bin_id"`
		BinNumber       int     `db:"bin_number"`
		Status          string  `db:"status"`
		ScheduledDate   int64   `db:"scheduled_date"`
		OriginalAddress string  `db:"original_address"`
		NewAddress      *string `db:"new_address"`
	}
	var moves []moveRow
	err = s.db.Select(&moves, `
		SELECT bmr.id, bmr.bin_id, b.bin_number, bmr.status, bmr.scheduled_date,
			   COALESCE(b.current_street || ', ' || b.city, 'Unknown') as original_address,
			   bmr.new_address
		FROM bin_move_requests bmr
		JOIN bins b ON bmr.bin_id = b.id
		WHERE bmr.status IN ('pending', 'in_progress')
		ORDER BY bmr.scheduled_date ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query move requests: %w", err)
	}

	now := time.Now().Unix()
	overdueCount, urgentCount, soonCount := 0, 0, 0

	type snapshotItem struct {
		BinNumber       int     `json:"bin_number"`
		Address         string  `json:"address"`
		Status          string  `json:"status"`
		DaysOverdue     int     `json:"days_overdue,omitempty"`
		HoursUntil      int     `json:"hours_until,omitempty"`
		MoveRequestID   string  `json:"move_request_id"`
	}

	var overdueItems, upcomingItems []snapshotItem

	for _, m := range moves {
		hoursUntil := float64(m.ScheduledDate-now) / 3600.0
		addr := m.OriginalAddress
		if m.NewAddress != nil && *m.NewAddress != "" {
			addr = *m.NewAddress
		}

		if hoursUntil < 0 {
			overdueCount++
			if len(overdueItems) < 15 {
				overdueItems = append(overdueItems, snapshotItem{
					BinNumber:     m.BinNumber,
					Address:       addr,
					Status:        m.Status,
					DaysOverdue:   int(-hoursUntil / 24),
					MoveRequestID: m.ID,
				})
			}
		} else if hoursUntil < 24 {
			urgentCount++
			if len(upcomingItems) < 15 {
				upcomingItems = append(upcomingItems, snapshotItem{
					BinNumber:     m.BinNumber,
					Address:       addr,
					Status:        m.Status,
					HoursUntil:    int(hoursUntil),
					MoveRequestID: m.ID,
				})
			}
		} else if hoursUntil < 72 {
			soonCount++
			if len(upcomingItems) < 15 {
				upcomingItems = append(upcomingItems, snapshotItem{
					BinNumber:     m.BinNumber,
					Address:       addr,
					Status:        m.Status,
					HoursUntil:    int(hoursUntil),
					MoveRequestID: m.ID,
				})
			}
		}
	}

	// Query warehouse bins
	type warehouseBin struct {
		BinNumber     int    `db:"bin_number"`
		Address       string `db:"address"`
		DaysInStorage int    `db:"days_in_storage"`
	}
	var warehouseBins []warehouseBin
	err = s.db.Select(&warehouseBins, `
		SELECT bin_number,
			   COALESCE(current_street || ', ' || city, 'Warehouse') as address,
			   GREATEST(($1 - updated_at) / 86400, 0) as days_in_storage
		FROM bins WHERE status = 'in_storage'
		ORDER BY updated_at ASC
		LIMIT 15
	`, now)
	if err != nil {
		return nil, fmt.Errorf("query warehouse bins: %w", err)
	}
	warehouseCount := 0
	s.db.QueryRow(`SELECT COUNT(*) FROM bins WHERE status = 'in_storage'`).Scan(&warehouseCount)

	// Nothing to report?
	if overdueCount == 0 && urgentCount == 0 && soonCount == 0 && warehouseCount == 0 {
		if !skipDedup {
			s.updateConfigTimestamp(configKey, today)
		}
		return &DigestResult{Window: "daily_move_report"}, nil
	}

	// Build title/body
	totalMoves := overdueCount + urgentCount + soonCount
	title := fmt.Sprintf("Daily Move Report: %d Active Request%s", totalMoves, plural(totalMoves))
	parts := []string{}
	if overdueCount > 0 {
		parts = append(parts, fmt.Sprintf("%d move%s overdue and need attention", overdueCount, plural(overdueCount)))
	}
	if urgentCount > 0 {
		parts = append(parts, fmt.Sprintf("%d move%s due within 24 hours", urgentCount, plural(urgentCount)))
	}
	if soonCount > 0 {
		parts = append(parts, fmt.Sprintf("%d move%s coming up in the next few days", soonCount, plural(soonCount)))
	}
	if warehouseCount > 0 {
		parts = append(parts, fmt.Sprintf("%d bin%s sitting in warehouse awaiting redeployment", warehouseCount, plural(warehouseCount)))
	}
	body := joinParts(parts)
	if totalMoves == 0 && warehouseCount == 0 {
		body = "All clear — no pending move requests today."
	}

	// Build warehouse snapshot items
	type warehouseSnapshotItem struct {
		BinNumber     int    `json:"bin_number"`
		Address       string `json:"address"`
		DaysInStorage int    `json:"days_in_storage"`
	}
	var warehouseSnapshot []warehouseSnapshotItem
	for _, wb := range warehouseBins {
		warehouseSnapshot = append(warehouseSnapshot, warehouseSnapshotItem{
			BinNumber:     wb.BinNumber,
			Address:       wb.Address,
			DaysInStorage: wb.DaysInStorage,
		})
	}

	// Rich payload for DB storage (JSONB — no size limit)
	richData := map[string]interface{}{
		"type":             "daily_move_report",
		"overdue_count":    overdueCount,
		"urgent_count":     urgentCount,
		"soon_count":       soonCount,
		"warehouse_count":  warehouseCount,
		"overdue_items":    overdueItems,
		"upcoming_items":   upcomingItems,
		"warehouse_items":  warehouseSnapshot,
		"report_date":      today,
		"deep_link":        "/manager/move-requests",
		"subtitle":         "Daily Report",
	}

	// Flat payload for FCM push (must be map[string]string, <4KB)
	fcmData := map[string]string{
		"type":            "daily_move_report",
		"overdue_count":   strconv.Itoa(overdueCount),
		"urgent_count":    strconv.Itoa(urgentCount),
		"soon_count":      strconv.Itoa(soonCount),
		"warehouse_count": strconv.Itoa(warehouseCount),
		"deep_link":       "/manager/move-requests",
		"subtitle":        "Daily Report",
	}

	// Get admin tokens
	var tokens []string
	s.db.Select(&tokens, `
		SELECT ft.token FROM fcm_tokens ft
		JOIN users u ON ft.user_id = u.id
		WHERE u.role = 'admin'
	`)

	// Send FCM
	if s.fcmService != nil && len(tokens) > 0 {
		if err := s.fcmService.SendMulticast(tokens, title, body, fcmData); err != nil {
			log.Printf("⚠️  [DailyReport] Failed to send move report FCM: %v", err)
		}
	} else if s.fcmService == nil {
		log.Println("⚠️  [DailyReport] FCM service is nil — push notifications will NOT be sent")
	}

	// Store in DB (one notification per admin with rich snapshot)
	adminIDs, _ := GetAdminUserIDs(s.db)
	CreateNotificationForUsers(s.db, adminIDs, "daily_move_report", title, body, richData)

	// Publish to Centrifugo for real-time dashboard
	if s.centrifugoClient != nil {
		_ = s.centrifugoClient.PublishCompanyEvent(ctx, "daily_move_report", map[string]interface{}{
			"overdue_count":   overdueCount,
			"urgent_count":    urgentCount,
			"soon_count":      soonCount,
			"warehouse_count": warehouseCount,
			"overdue_items":   overdueItems,
			"upcoming_items":  upcomingItems,
			"warehouse_items": warehouseSnapshot,
		})
	}

	// Mark as sent
	if !skipDedup {
		s.updateConfigTimestamp(configKey, today)
	}

	return &DigestResult{
		Window:         "daily_move_report",
		OverdueCount:   overdueCount,
		UrgentCount:    urgentCount,
		SoonCount:      soonCount,
		WarehouseCount: warehouseCount,
		TokensSent:     len(tokens),
	}, nil
}

// RunDailyBinCheckReport sends ONE notification summarizing bins that need checking (7+ days).
func (s *DigestScheduler) RunDailyBinCheckReport(force ...bool) (*DigestResult, error) {
	ctx := context.Background()

	settings := loadNotificationSettings(s.db)
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc, _ = time.LoadLocation("America/New_York")
	}
	today := time.Now().In(loc).Format("2006-01-02")
	configKey := "last_daily_bin_check_report"

	// Idempotency check
	skipDedup := len(force) > 0 && force[0]
	if !skipDedup {
		var lastSent string
		dedupErr := s.db.QueryRow(`SELECT value->>'date' FROM config WHERE key = $1`, configKey).Scan(&lastSent)
		if dedupErr == nil && lastSent == today {
			return &DigestResult{AlreadySent: true, Window: "daily_bin_check_report"}, nil
		}
	}

	now := time.Now().Unix()

	// Query active bins (skip missing, retired, in_storage)
	type binRow struct {
		ID            string  `db:"id"`
		BinNumber     int     `db:"bin_number"`
		Address       string  `db:"address"`
		LastCheckedAt *int64  `db:"last_checked_at"`
		CreatedAt     int64   `db:"created_at"`
	}
	var bins []binRow
	err = s.db.Select(&bins, `
		SELECT id, bin_number,
			   COALESCE(current_street || ', ' || city, 'Unknown') as address,
			   last_checked_at, created_at
		FROM bins
		WHERE status IN ('active', 'pending_move')
		ORDER BY COALESCE(last_checked_at, created_at) ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query bins: %w", err)
	}

	type binCheckItem struct {
		BinNumber      int    `json:"bin_number"`
		Address        string `json:"address"`
		DaysSinceCheck int    `json:"days_since_check"`
		BinID          string `json:"bin_id"`
	}

	var criticalItems, overdueItems []binCheckItem
	criticalCount, overdueCount := 0, 0

	for _, b := range bins {
		var daysSince int
		if b.LastCheckedAt != nil {
			daysSince = int((now - *b.LastCheckedAt) / 86400)
		} else {
			daysSince = int((now - b.CreatedAt) / 86400)
		}

		if daysSince >= 14 {
			criticalCount++
			if len(criticalItems) < 15 {
				criticalItems = append(criticalItems, binCheckItem{
					BinNumber:      b.BinNumber,
					Address:        b.Address,
					DaysSinceCheck: daysSince,
					BinID:          b.ID,
				})
			}
		} else if daysSince >= 7 {
			overdueCount++
			if len(overdueItems) < 15 {
				overdueItems = append(overdueItems, binCheckItem{
					BinNumber:      b.BinNumber,
					Address:        b.Address,
					DaysSinceCheck: daysSince,
					BinID:          b.ID,
				})
			}
		}
	}

	totalBins := criticalCount + overdueCount
	if totalBins == 0 {
		if !skipDedup {
			s.updateConfigTimestamp(configKey, today)
		}
		return &DigestResult{Window: "daily_bin_check_report"}, nil
	}

	// Build title/body
	title := fmt.Sprintf("Bin Check Report: %d Bin%s Need Checking", totalBins, plural(totalBins))
	parts := []string{}
	if criticalCount > 0 {
		parts = append(parts, fmt.Sprintf("%d bin%s haven't been checked in over 2 weeks", criticalCount, plural(criticalCount)))
	}
	if overdueCount > 0 {
		parts = append(parts, fmt.Sprintf("%d bin%s are overdue for a check (7–13 days)", overdueCount, plural(overdueCount)))
	}
	body := joinParts(parts)

	// Rich payload for DB
	richData := map[string]interface{}{
		"type":           "daily_bin_check_report",
		"critical_count": criticalCount,
		"overdue_count":  overdueCount,
		"critical_items": criticalItems,
		"overdue_items":  overdueItems,
		"report_date":    today,
		"deep_link":      "/manager/bins",
		"subtitle":       "Daily Check Report",
	}

	// Flat payload for FCM
	fcmData := map[string]string{
		"type":           "daily_bin_check_report",
		"critical_count": strconv.Itoa(criticalCount),
		"overdue_count":  strconv.Itoa(overdueCount),
		"deep_link":      "/manager/bins",
		"subtitle":       "Daily Check Report",
	}

	// Get admin tokens
	var tokens []string
	s.db.Select(&tokens, `
		SELECT ft.token FROM fcm_tokens ft
		JOIN users u ON ft.user_id = u.id
		WHERE u.role = 'admin'
	`)

	// Send FCM
	if s.fcmService != nil && len(tokens) > 0 {
		if err := s.fcmService.SendMulticast(tokens, title, body, fcmData); err != nil {
			log.Printf("⚠️  [DailyReport] Failed to send bin check report FCM: %v", err)
		}
	}

	// Store in DB
	adminIDs, _ := GetAdminUserIDs(s.db)
	CreateNotificationForUsers(s.db, adminIDs, "daily_bin_check_report", title, body, richData)

	// Centrifugo
	if s.centrifugoClient != nil {
		_ = s.centrifugoClient.PublishCompanyEvent(ctx, "daily_bin_check_report", map[string]interface{}{
			"critical_count": criticalCount,
			"overdue_count":  overdueCount,
			"critical_items": criticalItems,
			"overdue_items":  overdueItems,
		})
	}

	if !skipDedup {
		s.updateConfigTimestamp(configKey, today)
	}

	return &DigestResult{
		Window:       "daily_bin_check_report",
		CriticalBins: criticalCount,
		OverdueBins:  overdueCount,
		TokensSent:   len(tokens),
	}, nil
}

// RunDigest is kept for backward compatibility with the HTTP trigger endpoint.
// It delegates to RunDailyMoveReport.
func (s *DigestScheduler) RunDigest(window string, force ...bool) (*DigestResult, error) {
	switch window {
	case "daily_move_report", "morning", "afternoon":
		return s.RunDailyMoveReport(force...)
	case "daily_bin_check_report":
		return s.RunDailyBinCheckReport(force...)
	case "daily_battery_report":
		return s.RunDailyBatteryReport(force...)
	default:
		return s.RunDailyMoveReport(force...)
	}
}

// RunDailyBatteryReport sends ONE notification summarizing AirTags with low or critical battery.
func (s *DigestScheduler) RunDailyBatteryReport(force ...bool) (*DigestResult, error) {
	ctx := context.Background()

	settings := loadNotificationSettings(s.db)
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc, _ = time.LoadLocation("America/New_York")
	}
	today := time.Now().In(loc).Format("2006-01-02")
	configKey := "last_daily_battery_report"

	// Idempotency check
	skipDedup := len(force) > 0 && force[0]
	if !skipDedup {
		var lastSent string
		dedupErr := s.db.QueryRow(`SELECT value->>'date' FROM config WHERE key = $1`, configKey).Scan(&lastSent)
		if dedupErr == nil && lastSent == today {
			return &DigestResult{AlreadySent: true, Window: "daily_battery_report"}, nil
		}
	}

	// Fetch AirTag locations from bridge
	if s.bridgeURL == "" {
		log.Println("⚠️  [DailyReport] FINDMY_BRIDGE_URL not set — skipping battery report")
		return &DigestResult{Window: "daily_battery_report"}, nil
	}

	airtags, err := s.fetchBridgeAirtagLocations()
	if err != nil {
		return nil, fmt.Errorf("fetch airtag locations: %w", err)
	}

	// Filter low (2) and critical (3) battery
	type batteryItem struct {
		BinNumber     int    `json:"bin_number"`
		Name          string `json:"name"`
		Address       string `json:"address"`
		BatteryStatus int    `json:"battery_status"`
		LastSeen      string `json:"last_seen"`
	}

	var criticalItems, lowItems []batteryItem
	criticalCount, lowCount := 0, 0

	for _, at := range airtags {
		if at.BatteryStatus == 3 {
			criticalCount++
			if len(criticalItems) < 15 {
				addr := formatAddress(at.Address, at.City)
				criticalItems = append(criticalItems, batteryItem{
					BinNumber:     at.BinNumber,
					Name:          at.Name,
					Address:       addr,
					BatteryStatus: at.BatteryStatus,
					LastSeen:      at.LastSeen,
				})
			}
		} else if at.BatteryStatus == 2 {
			lowCount++
			if len(lowItems) < 15 {
				addr := formatAddress(at.Address, at.City)
				lowItems = append(lowItems, batteryItem{
					BinNumber:     at.BinNumber,
					Name:          at.Name,
					Address:       addr,
					BatteryStatus: at.BatteryStatus,
					LastSeen:      at.LastSeen,
				})
			}
		}
	}

	totalBad := criticalCount + lowCount
	if totalBad == 0 {
		if !skipDedup {
			s.updateConfigTimestamp(configKey, today)
		}
		return &DigestResult{Window: "daily_battery_report"}, nil
	}

	// Build title/body
	title := fmt.Sprintf("Battery Alert: %d Tag%s Need Attention", totalBad, plural(totalBad))
	parts := []string{}
	if criticalCount > 0 {
		parts = append(parts, fmt.Sprintf("%d tag%s with critical battery", criticalCount, plural(criticalCount)))
	}
	if lowCount > 0 {
		parts = append(parts, fmt.Sprintf("%d tag%s with low battery", lowCount, plural(lowCount)))
	}
	parts = append(parts, "Replace batteries soon.")
	body := joinParts(parts)

	// Rich payload for DB (JSONB)
	richData := map[string]interface{}{
		"type":           "daily_battery_report",
		"critical_count": criticalCount,
		"low_count":      lowCount,
		"critical_items": criticalItems,
		"low_items":      lowItems,
		"report_date":    today,
		"deep_link":      "/operations/airtag-tracker",
		"subtitle":       "Daily Battery Report",
	}

	// Flat payload for FCM
	fcmData := map[string]string{
		"type":           "daily_battery_report",
		"critical_count": strconv.Itoa(criticalCount),
		"low_count":      strconv.Itoa(lowCount),
		"deep_link":      "/operations/airtag-tracker",
		"subtitle":       "Daily Battery Report",
	}

	// Get admin tokens
	var tokens []string
	s.db.Select(&tokens, `
		SELECT ft.token FROM fcm_tokens ft
		JOIN users u ON ft.user_id = u.id
		WHERE u.role = 'admin'
	`)

	// Send FCM
	if s.fcmService != nil && len(tokens) > 0 {
		if err := s.fcmService.SendMulticast(tokens, title, body, fcmData); err != nil {
			log.Printf("⚠️  [DailyReport] Failed to send battery report FCM: %v", err)
		}
	}

	// Store in DB
	adminIDs, _ := GetAdminUserIDs(s.db)
	CreateNotificationForUsers(s.db, adminIDs, "daily_battery_report", title, body, richData)

	// Centrifugo
	if s.centrifugoClient != nil {
		_ = s.centrifugoClient.PublishCompanyEvent(ctx, "daily_battery_report", map[string]interface{}{
			"critical_count": criticalCount,
			"low_count":      lowCount,
			"critical_items": criticalItems,
			"low_items":      lowItems,
		})
	}

	if !skipDedup {
		s.updateConfigTimestamp(configKey, today)
	}

	return &DigestResult{
		Window:       "daily_battery_report",
		CriticalBins: criticalCount,
		OverdueBins:  lowCount,
		TokensSent:   len(tokens),
	}, nil
}

// fetchBridgeAirtagLocations fetches AirTag locations from the FindMy bridge service.
func (s *DigestScheduler) fetchBridgeAirtagLocations() ([]airtagEntry, error) {
	url := s.bridgeURL + "/api/airtag-locations"

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("bridge returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var apiResp airtagAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return apiResp.Data, nil
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
		log.Printf("⚠️  [DailyReport] Failed to update config %s: %v", key, err)
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
	result := strings.Join(parts, ". ")
	// Capitalize first letter and ensure trailing period
	if len(result) > 0 {
		result = strings.ToUpper(result[:1]) + result[1:]
		if !strings.HasSuffix(result, ".") {
			result += "."
		}
	}
	return result
}

// marshalSnapshot is a helper to safely marshal snapshot data for logging.
func marshalSnapshot(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}
