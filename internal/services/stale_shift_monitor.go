package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"ropacal-backend/internal/geo"
	"ropacal-backend/internal/helpers"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"

	"github.com/jmoiron/sqlx"
)

const (
	// StaleThreshold is how long without GPS before auto-ending a shift
	StaleThreshold = 2 * time.Hour

	// CheckInterval is how often the monitor runs
	StaleCheckInterval = 30 * time.Minute
)

// StaleShiftMonitor periodically checks for active shifts with no GPS updates
// and auto-ends them with end_reason "driver_disconnected".
type StaleShiftMonitor struct {
	db               *sqlx.DB
	redisClient      *redis.Client
	fcmService       *FCMService
	centrifugoClient *centrifugo.Client
	ticker           *time.Ticker
	stopChan         chan bool
}

type activeShiftRow struct {
	ID                string  `db:"id"`
	DriverID          string  `db:"driver_id"`
	StartTime         *int64  `db:"start_time"`
	TotalBins         int     `db:"total_bins"`
	CompletedBins     int     `db:"completed_bins"`
	TotalPauseSeconds int     `db:"total_pause_seconds"`
	RouteID           *string `db:"route_id"`
	CreatedAt         int64   `db:"created_at"`
	DriverName        string  `db:"driver_name"`
	DriverEmail       string  `db:"driver_email"`
}

// NewStaleShiftMonitor creates a new monitor.
func NewStaleShiftMonitor(db *sqlx.DB, redisClient *redis.Client, fcmService *FCMService, centrifugoClient *centrifugo.Client) *StaleShiftMonitor {
	return &StaleShiftMonitor{
		db:               db,
		redisClient:      redisClient,
		fcmService:       fcmService,
		centrifugoClient: centrifugoClient,
		ticker:           time.NewTicker(StaleCheckInterval),
		stopChan:         make(chan bool),
	}
}

// Start begins the background monitoring goroutine.
func (m *StaleShiftMonitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	log.Printf("📋 [StaleShiftMonitor] Starting stale shift monitor (threshold=%s, interval=%s)", StaleThreshold, StaleCheckInterval)

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Check immediately on startup
		m.checkStaleShifts()

		for {
			select {
			case <-ctx.Done():
				log.Println("🛑 [StaleShiftMonitor] Stopping...")
				return
			case <-m.ticker.C:
				m.checkStaleShifts()
			}
		}
	}()
}

// Stop halts the monitor.
func (m *StaleShiftMonitor) Stop() {
	m.ticker.Stop()
	m.stopChan <- true
}

func (m *StaleShiftMonitor) checkStaleShifts() {
	// Only check active shifts (not paused — driver intentionally paused)
	// First: auto-end any shifts where ready_to_end_at expired (driver was at warehouse, GPS stopped)
	var readyToEndShifts []struct {
		ID                string  `db:"id"`
		DriverID          string  `db:"driver_id"`
		DriverName        string  `db:"driver_name"`
		ReadyToEndAt      int64   `db:"ready_to_end_at"`
		StartTime         *int64  `db:"start_time"`
		TotalBins         int     `db:"total_bins"`
		CompletedBins     int     `db:"completed_bins"`
		TotalPauseSeconds int     `db:"total_pause_seconds"`
		RouteID           *string `db:"route_id"`
		CreatedAt         int64   `db:"created_at"`
		DriverEmail       string  `db:"driver_email"`
	}
	m.db.Select(&readyToEndShifts, `
		SELECT s.id, s.driver_id, s.ready_to_end_at, s.start_time, s.total_bins, s.completed_bins,
		       s.total_pause_seconds, s.route_id, s.created_at,
		       u.name as driver_name, u.email as driver_email
		FROM shifts s
		JOIN users u ON s.driver_id = u.id
		WHERE s.status = 'active' AND s.ready_to_end_at IS NOT NULL
		  AND s.ready_to_end_at < $1
	`, time.Now().Unix()-300) // 5 minutes ago

	for _, s := range readyToEndShifts {
		log.Printf("⏰ [StaleShiftMonitor] Auto-ending shift %s for %s — ready_to_end expired %s ago",
			s.ID[:12], s.DriverName, time.Since(time.Unix(s.ReadyToEndAt, 0)).Round(time.Second))
		m.autoEndShift(activeShiftRow{
			ID: s.ID, DriverID: s.DriverID, StartTime: s.StartTime,
			TotalBins: s.TotalBins, CompletedBins: s.CompletedBins,
			TotalPauseSeconds: s.TotalPauseSeconds, RouteID: s.RouteID,
			CreatedAt: s.CreatedAt, DriverName: s.DriverName, DriverEmail: s.DriverEmail,
		})
	}

	// Auto-cancel ready shifts past their availability window:
	// - If scheduled_start is set: cancel 24hr after scheduled_start
	// - Else if scheduled_date is set: cancel if scheduled_date < yesterday
	var expiredReadyShifts []activeShiftRow
	m.db.Select(&expiredReadyShifts, `
		SELECT s.id, s.driver_id, s.start_time, s.total_bins, s.completed_bins,
		       s.total_pause_seconds, s.route_id, s.created_at,
		       u.name as driver_name, u.email as driver_email
		FROM shifts s
		JOIN users u ON s.driver_id = u.id
		WHERE s.status = 'ready'
		  AND (
		    (s.scheduled_start IS NOT NULL AND s.scheduled_start < NOW() - INTERVAL '24 hours')
		    OR (s.scheduled_start IS NULL AND s.scheduled_date IS NOT NULL AND s.scheduled_date < (NOW() AT TIME ZONE 'America/Los_Angeles')::date - INTERVAL '1 day')
		  )
	`)
	for _, s := range expiredReadyShifts {
		log.Printf("⏰ [StaleShiftMonitor] Auto-cancelling expired ready shift %s for %s — scheduled for past date",
			s.ID[:12], s.DriverName)
		m.autoEndShift(s)
	}
	if len(expiredReadyShifts) > 0 {
		log.Printf("📋 [StaleShiftMonitor] Cancelled %d expired ready shifts", len(expiredReadyShifts))
	}

	// Then: check for stale GPS on remaining active shifts
	var shifts []activeShiftRow
	err := m.db.Select(&shifts, `
		SELECT s.id, s.driver_id, s.start_time, s.total_bins, s.completed_bins,
		       s.total_pause_seconds, s.route_id, s.created_at,
		       u.name as driver_name, u.email as driver_email
		FROM shifts s
		JOIN users u ON s.driver_id = u.id
		WHERE s.status = 'active'
	`)
	if err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to query active shifts: %v", err)
		return
	}

	if len(shifts) == 0 {
		return
	}

	staleCount := 0
	healthyCount := 0
	ctx := context.Background()

	for _, shift := range shifts {
		lastGPS, source := m.getLastGPSTime(ctx, shift.DriverID)

		if lastGPS.IsZero() {
			// No GPS data at all — check how long the shift has been active
			if shift.StartTime != nil {
				shiftAge := time.Since(time.Unix(*shift.StartTime, 0))
				if shiftAge > StaleThreshold {
					log.Printf("⚠️  [StaleShiftMonitor] No GPS data ever for driver %s (%s), shift active for %s",
						shift.DriverName, shift.DriverEmail, shiftAge.Round(time.Minute))
					m.autoEndShift(shift)
					staleCount++
					continue
				}
			}
			healthyCount++
			continue
		}

		staleDuration := time.Since(lastGPS)
		if staleDuration > StaleThreshold {
			log.Printf("⚠️  [StaleShiftMonitor] Auto-ending shift %s for driver %s — no GPS for %s (source: %s)",
				shift.ID[:12], shift.DriverName, staleDuration.Round(time.Minute), source)
			m.autoEndShift(shift)
			staleCount++
		} else {
			healthyCount++

			// Write GPS snapshot for stationary detection
			var currentLoc struct {
				Latitude  float64 `db:"latitude"`
				Longitude float64 `db:"longitude"`
			}
			if err := m.db.Get(&currentLoc, `SELECT latitude, longitude FROM driver_current_location WHERE driver_id = $1`, shift.DriverID); err == nil {
				m.db.Exec(`INSERT INTO driver_location_snapshots (driver_id, shift_id, latitude, longitude, recorded_at) VALUES ($1, $2, $3, $4, $5)`,
					shift.DriverID, shift.ID, currentLoc.Latitude, currentLoc.Longitude, time.Now().Unix())
			}
		}
	}

	// Clean up old snapshots (keep last 3 hours)
	m.db.Exec(`DELETE FROM driver_location_snapshots WHERE recorded_at < $1`, time.Now().Unix()-10800)

	// Check for inactive shifts (stationary + no task activity)
	m.checkInactiveShifts(shifts)

	if staleCount > 0 || len(shifts) > 0 {
		log.Printf("📋 [StaleShiftMonitor] Checked %d active shifts: %d stale (auto-ended), %d healthy",
			len(shifts), staleCount, healthyCount)
	}
}

// checkInactiveShifts detects drivers who are stationary with no task activity for 2+ hours.
func (m *StaleShiftMonitor) checkInactiveShifts(shifts []activeShiftRow) {
	log.Printf("🔍 [InactiveCheck] Checking %d shifts for inactivity", len(shifts))
	for _, shift := range shifts {
		// Skip shifts less than 2 hours old
		if shift.StartTime != nil && time.Since(time.Unix(*shift.StartTime, 0)) < 2*time.Hour {
			log.Printf("   ⏭️  %s (%s): shift too new, skipping", shift.ID[:12], shift.DriverName)
			continue
		}

		// Get snapshots from last 2 hours
		var snapshots []struct {
			Latitude  float64 `db:"latitude"`
			Longitude float64 `db:"longitude"`
		}
		err := m.db.Select(&snapshots, `
			SELECT latitude, longitude FROM driver_location_snapshots
			WHERE driver_id = $1 AND recorded_at > $2
			ORDER BY recorded_at DESC
		`, shift.DriverID, time.Now().Unix()-7200)
		if err != nil {
			log.Printf("   ❌ %s (%s): failed to query snapshots: %v", shift.ID[:12], shift.DriverName, err)
			continue
		}

		if len(snapshots) < 3 {
			log.Printf("   ⏭️  %s (%s): only %d snapshots (need 3+), skipping", shift.ID[:12], shift.DriverName, len(snapshots))
			continue // Not enough data yet
		}

		// Calculate max distance between any two snapshots
		maxDist := 0.0
		for i := range snapshots {
			for j := i + 1; j < len(snapshots); j++ {
				d := geo.HaversineMeters(snapshots[i].Latitude, snapshots[i].Longitude,
					snapshots[j].Latitude, snapshots[j].Longitude)
				if d > maxDist {
					maxDist = d
				}
			}
		}

		// Get last task completion time
		var lastTaskResult struct {
			LastTask *int64 `db:"last_task"`
		}
		taskErr := m.db.Get(&lastTaskResult, `
			SELECT MAX(completed_at) as last_task FROM route_tasks
			WHERE shift_id = $1 AND is_completed = 1
		`, shift.ID)
		if taskErr != nil {
			log.Printf("   ⚠️  %s (%s): failed to query last task time: %v", shift.ID[:12], shift.DriverName, taskErr)
		}

		lastActivity := int64(0)
		if shift.StartTime != nil {
			lastActivity = *shift.StartTime
		}
		if lastTaskResult.LastTask != nil && *lastTaskResult.LastTask > lastActivity {
			lastActivity = *lastTaskResult.LastTask
		}
		hoursSinceTask := time.Since(time.Unix(lastActivity, 0)).Hours()

		// Check remaining tasks (exclude warehouse_stop)
		var remainingResult struct {
			Count int `db:"count"`
		}
		remErr := m.db.Get(&remainingResult, `
			SELECT COUNT(*) as count FROM route_tasks
			WHERE shift_id = $1 AND is_completed = 0 AND is_deleted = false AND task_type != 'warehouse_stop'
		`, shift.ID)
		remaining := remainingResult.Count
		if remErr != nil {
			log.Printf("   ⚠️  %s (%s): failed to query remaining tasks: %v", shift.ID[:12], shift.DriverName, remErr)
			remaining = 0
		}

		log.Printf("   📊 %s (%s): snapshots=%d, maxDist=%.0fm, hoursSinceTask=%.1f, remaining=%d",
			shift.ID[:12], shift.DriverName, len(snapshots), maxDist, hoursSinceTask, remaining)

		// Trigger: stationary (<300m) + no task activity (2h)
		// Covers both: driver stopped working with tasks left, AND driver finished all tasks but didn't end shift
		if maxDist < 300 && hoursSinceTask >= 2 {
			log.Printf("⚠️  [StaleShiftMonitor] Auto-ending INACTIVE shift %s for %s — stationary %.0fm, no task in %.1fh, %d tasks remaining",
				shift.ID[:12], shift.DriverName, maxDist, hoursSinceTask, remaining)
			m.autoEndShift(shift, "auto_ended_inactive")
		}
	}
}

// getLastGPSTime returns the most recent GPS timestamp for a driver.
// Checks Redis first (real-time), falls back to PostgreSQL.
func (m *StaleShiftMonitor) getLastGPSTime(ctx context.Context, driverID string) (time.Time, string) {
	// Try Redis first (most recent GPS, updated every 1-3 seconds)
	locationJSON, err := m.redisClient.GetDriverLocation(ctx, driverID)
	if err == nil && locationJSON != "" {
		var loc struct {
			Timestamp int64 `json:"timestamp"`
		}
		if json.Unmarshal([]byte(locationJSON), &loc) == nil && loc.Timestamp > 0 {
			// Timestamp is in milliseconds
			return time.Unix(0, loc.Timestamp*int64(time.Millisecond)), "redis"
		}
	}

	// Fallback to PostgreSQL driver_current_location table
	var updatedAt int64
	err = m.db.Get(&updatedAt, `SELECT updated_at FROM driver_current_location WHERE driver_id = $1`, driverID)
	if err == nil && updatedAt > 0 {
		return time.Unix(updatedAt, 0), "postgres"
	}

	return time.Time{}, "none"
}

// autoEndShift ends a shift with the given end_reason and archives to shift_history.
func (m *StaleShiftMonitor) autoEndShift(shift activeShiftRow, endReasons ...string) {
	endReason := "driver_disconnected"
	if len(endReasons) > 0 {
		endReason = endReasons[0]
	}
	now := time.Now().Unix()

	// Calculate durations
	totalPause := int64(shift.TotalPauseSeconds)
	completionRate := 0.0
	if shift.TotalBins > 0 {
		completionRate = (float64(shift.CompletedBins) / float64(shift.TotalBins)) * 100
	}

	// Count incidents
	var incidentStats struct {
		TotalIncidents    int `db:"total_incidents"`
		FieldObservations int `db:"field_observations"`
	}
	err := m.db.Get(&incidentStats, `
		SELECT COUNT(*) as total_incidents,
		       COUNT(*) FILTER (WHERE is_field_observation = true) as field_observations
		FROM zone_incidents WHERE shift_id = $1
	`, shift.ID)
	if err != nil {
		incidentStats.TotalIncidents = 0
		incidentStats.FieldObservations = 0
	}

	// Get optimization metadata
	var optMetaRaw *json.RawMessage
	var optMetaBytes []byte
	err = m.db.Get(&optMetaBytes, `SELECT optimization_metadata FROM shifts WHERE id = $1`, shift.ID)
	if err == nil && len(optMetaBytes) > 0 {
		raw := json.RawMessage(optMetaBytes)
		optMetaRaw = &raw
	}

	// Archive + end + release as ONE transaction. A torn sequence here (a process
	// kill between the archive and the status update) would otherwise leave a
	// half-ended shift with no rollback. Notifications fire only after commit.
	tx, err := m.db.Beginx()
	if err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to begin tx for shift %s: %v", shift.ID[:12], err)
		return
	}
	defer tx.Rollback() // no-op once Commit succeeds; rolls back on any early return

	// Archive to shift_history
	_, err = tx.Exec(`
		INSERT INTO shift_history (
			id, driver_id, route_id, start_time, end_time, created_at, ended_at,
			total_pause_seconds, total_bins, completed_bins, completion_rate,
			incidents_reported, field_observations,
			end_reason, ended_by_user_id, end_reason_metadata, optimization_metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (id) DO NOTHING
	`,
		shift.ID,
		shift.DriverID,
		shift.RouteID,
		shift.StartTime,
		now, // end_time
		shift.CreatedAt,
		now, // ended_at
		totalPause,
		shift.TotalBins,
		shift.CompletedBins,
		completionRate,
		incidentStats.TotalIncidents,
		incidentStats.FieldObservations,
		endReason, // end_reason
		nil,       // ended_by_user_id (system action)
		nil,       // end_reason_metadata
		optMetaRaw,
	)
	if err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to archive shift %s: %v", shift.ID[:12], err)
		return
	}

	// Update shift status
	_, err = tx.Exec(`
		UPDATE shifts SET status = 'ended', end_time = $1, pause_start_time = NULL, updated_at = $2
		WHERE id = $3
	`, now, now, shift.ID)
	if err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to end shift %s: %v", shift.ID[:12], err)
		return
	}

	// Release incomplete moves to the driver's BACKLOG via the shared helper — the
	// same rule as the manual End/Cancel paths (auto-end is still an end). This
	// previously dumped moves to the unassigned pool ('pending'), inconsistent with
	// the backlog model, and also missed 'assigned' (not-yet-started) moves.
	released, relErr := moverequest.ReleaseFromShift(tx, []string{shift.ID}, now)
	if relErr != nil {
		log.Printf("⚠️  [StaleShiftMonitor] Failed to release move requests: %v", relErr)
	}
	for _, mr := range released {
		if logErr := helpers.LogMoveRequestUnassigned(
			tx, mr.ID, "system", "system",
			mr.AssignmentType, mr.AssignedUserID, mr.AssignedUserName, mr.AssignedShiftID,
		); logErr != nil {
			log.Printf("⚠️  [StaleShiftMonitor] Failed to log unassignment for %s: %v", mr.ID, logErr)
		}
	}

	if err = tx.Commit(); err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to commit auto-end for shift %s: %v", shift.ID[:12], err)
		return
	}
	if len(released) > 0 {
		log.Printf("📝 [StaleShiftMonitor] Returned %d move request(s) to driver backlog", len(released))
	}

	log.Printf("✅ [StaleShiftMonitor] Shift %s auto-ended — driver: %s, completed: %d/%d (%.0f%%)",
		shift.ID[:12], shift.DriverName, shift.CompletedBins, shift.TotalBins, completionRate)

	// Notify managers via Centrifugo
	if m.centrifugoClient != nil {
		m.centrifugoClient.PublishCompanyEvent(context.Background(), "shift_auto_ended", map[string]interface{}{
			"shift_id":        shift.ID,
			"driver_id":       shift.DriverID,
			"driver_name":     shift.DriverName,
			"end_reason":      "driver_disconnected",
			"completed_bins":  shift.CompletedBins,
			"total_bins":      shift.TotalBins,
			"completion_rate": fmt.Sprintf("%.0f%%", completionRate),
		})
	}

	// Send FCM push to driver
	if m.fcmService != nil {
		var tokens []string
		m.db.Select(&tokens, `SELECT token FROM fcm_tokens WHERE user_id = $1`, shift.DriverID)
		if len(tokens) > 0 {
			m.fcmService.SendMulticast(
				tokens,
				"Shift Auto-Ended",
				fmt.Sprintf("Your shift was automatically ended due to inactivity. Completed: %d/%d tasks.", shift.CompletedBins, shift.TotalBins),
				map[string]string{
					"type":     "shift_auto_ended",
					"shift_id": shift.ID,
				},
			)
		}
	}
}
