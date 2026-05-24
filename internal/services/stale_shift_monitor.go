package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"ropacal-backend/internal/helpers"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"

	"github.com/jmoiron/sqlx"
)

const (
	// StaleThreshold is how long without GPS before auto-ending a shift
	// Set to 5 minutes for testing, change to 2 hours for production
	StaleThreshold = 5 * time.Minute // TODO: change to 2 * time.Hour for production

	// CheckInterval is how often the monitor runs
	// Set to 1 minute for testing, change to 30 minutes for production
	StaleCheckInterval = 1 * time.Minute // TODO: change to 30 * time.Minute for production
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
func (m *StaleShiftMonitor) Start() {
	log.Printf("📋 [StaleShiftMonitor] Starting stale shift monitor (threshold=%s, interval=%s)", StaleThreshold, StaleCheckInterval)

	go func() {
		// Check immediately on startup
		m.checkStaleShifts()

		for {
			select {
			case <-m.stopChan:
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
		}
	}

	if staleCount > 0 || len(shifts) > 0 {
		log.Printf("📋 [StaleShiftMonitor] Checked %d active shifts: %d stale (auto-ended), %d healthy",
			len(shifts), staleCount, healthyCount)
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

// autoEndShift ends a stale shift with end_reason "driver_disconnected".
func (m *StaleShiftMonitor) autoEndShift(shift activeShiftRow) {
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

	// Archive to shift_history
	_, err = m.db.Exec(`
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
		now,             // end_time
		shift.CreatedAt,
		now,             // ended_at
		totalPause,
		shift.TotalBins,
		shift.CompletedBins,
		completionRate,
		incidentStats.TotalIncidents,
		incidentStats.FieldObservations,
		"driver_disconnected", // end_reason
		nil,                   // ended_by_user_id (system action)
		nil,                   // end_reason_metadata
		optMetaRaw,
	)
	if err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to archive shift %s: %v", shift.ID[:12], err)
		return
	}

	// Update shift status
	_, err = m.db.Exec(`
		UPDATE shifts SET status = 'ended', end_time = $1, pause_start_time = NULL, updated_at = $2
		WHERE id = $3
	`, now, now, shift.ID)
	if err != nil {
		log.Printf("❌ [StaleShiftMonitor] Failed to end shift %s: %v", shift.ID[:12], err)
		return
	}

	// Return incomplete move requests to pending
	type MoveRequestInfo struct {
		ID               string  `db:"id"`
		AssignmentType   *string `db:"assignment_type"`
		AssignedUserID   *string `db:"assigned_user_id"`
		AssignedUserName *string `db:"assigned_user_name"`
		AssignedShiftID  *string `db:"assigned_shift_id"`
	}
	var affectedMoveRequests []MoveRequestInfo
	m.db.Select(&affectedMoveRequests, `
		SELECT mr.id, mr.assignment_type, mr.assigned_user_id, mr.assigned_shift_id,
		       u.name as assigned_user_name
		FROM bin_move_requests mr
		LEFT JOIN users u ON mr.assigned_user_id = u.id
		WHERE mr.assigned_shift_id = $1 AND mr.status = 'in_progress'
	`, shift.ID)

	result, err := m.db.Exec(`
		UPDATE bin_move_requests
		SET status = 'pending', assigned_shift_id = NULL, updated_at = $1
		WHERE assigned_shift_id = $2 AND status = 'in_progress'
	`, now, shift.ID)
	if err != nil {
		log.Printf("⚠️  [StaleShiftMonitor] Failed to return move requests: %v", err)
	} else {
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			log.Printf("📝 [StaleShiftMonitor] Returned %d move request(s) to pending", rowsAffected)
			for _, mr := range affectedMoveRequests {
				helpers.LogMoveRequestUnassigned(
					m.db, mr.ID, "system", "system",
					mr.AssignmentType, mr.AssignedUserID, mr.AssignedUserName, mr.AssignedShiftID,
				)
			}
		}
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
