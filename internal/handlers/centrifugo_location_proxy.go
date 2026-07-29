package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/geo"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/services/roads"

	"github.com/jmoiron/sqlx"
)

// LocationPublishProxyRequest represents location data from driver
type LocationPublishProxyRequest struct {
	ClientID  string                 `json:"client"`
	Transport string                 `json:"transport"`
	Protocol  string                 `json:"protocol"`
	Encoding  string                 `json:"encoding"`
	User      string                 `json:"user"`    // Driver ID
	Channel   string                 `json:"channel"` // driver:location:{driverId}
	Data      map[string]interface{} `json:"data"`    // GPS data
}

// LocationData represents the GPS data structure
type LocationData struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Accuracy  float64 `json:"accuracy"`
	Heading   float64 `json:"heading"`
	Speed     float64 `json:"speed"`
	ShiftID   *string `json:"shift_id"`
	Timestamp int64   `json:"timestamp"`
}

// CentrifugoLocationPublishProxy handles location publish requests from drivers
// This is called by Centrifugo BEFORE broadcasting the message
// We process the GPS data (save to Redis, snap to roads) and return modified data
func CentrifugoLocationPublishProxy(db *sqlx.DB, redisClient *redis.Client, osrmClient *roads.OSRMClient, centrifugoClient *centrifugo.Client, fcmService *services.FCMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LocationPublishProxyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [LocationProxy] Invalid request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid request",
				},
			})
			return
		}

		// log.Printf("📍 [LocationProxy] Received location from user=%s channel=%s",
		// req.User, req.Channel)

		// 1. Validate channel format: driver:location:{driverId}
		parts := strings.Split(req.Channel, ":")
		if len(parts) != 3 || parts[0] != "driver" || parts[1] != "location" {
			log.Printf("❌ [LocationProxy] Invalid channel format: %s", req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid channel format",
				},
			})
			return
		}

		driverID := parts[2]

		// 2. Authorize: Only the driver can publish to their own location
		// channel. req.User is the identity Centrifugo resolved from the
		// client's authenticated connection token — trustworthy only because
		// the CentrifugoProxyAuth middleware verified the caller IS Centrifugo.
		if req.User != driverID {
			// Silently deny - this is expected behavior when non-drivers try to publish
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "permission denied",
				},
			})
			return
		}

		// 3. Parse location data
		locationData, err := parseLocationData(req.Data)
		if err != nil {
			log.Printf("❌ [LocationProxy] Invalid location data: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: fmt.Sprintf("invalid location data: %v", err),
				},
			})
			return
		}

		// log.Printf("📍 [LocationProxy] Driver %s: lat=%.6f, lng=%.6f, accuracy=%.1fm",
		// driverID, locationData.Latitude, locationData.Longitude, locationData.Accuracy)

		// 3.5 If a shift_id is supplied, verify the shift belongs to this driver
		// BEFORE any side effect (Redis write, proximity goroutine, broadcast).
		// checkWarehouseProximity can end a shift and archive it to
		// shift_history, so a forged (user, shift_id) pair must never reach it.
		if locationData.ShiftID != nil {
			var shiftDriverID sql.NullString
			err := db.Get(&shiftDriverID, `SELECT driver_id FROM shifts WHERE id = $1`, *locationData.ShiftID)
			if err == sql.ErrNoRows {
				log.Printf("🚫 [LocationProxy] Publish rejected — shift %s not found (driver=%s)",
					*locationData.ShiftID, driverID)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(CentrifugoPublishResponse{
					Error: &CentrifugoError{
						Code:    403,
						Message: "permission denied",
					},
				})
				return
			}
			if err != nil {
				log.Printf("❌ [LocationProxy] Shift ownership check failed for shift %s (driver=%s): %v",
					*locationData.ShiftID, driverID, err)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(CentrifugoPublishResponse{
					Error: &CentrifugoError{
						Code:    403,
						Message: "authorization failed",
					},
				})
				return
			}
			if !shiftDriverID.Valid || shiftDriverID.String != driverID {
				log.Printf("🚫 [LocationProxy] Publish rejected — shift %s does not belong to driver %s",
					*locationData.ShiftID, driverID)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(CentrifugoPublishResponse{
					Error: &CentrifugoError{
						Code:    403,
						Message: "permission denied",
					},
				})
				return
			}
		}

		// 4. Save ORIGINAL GPS to Redis (synchronous).
		// Redis is the source of truth for live position (preflight, StartShift,
		// and the dashboard all read it). The write is sub-millisecond, so we do
		// it inline rather than in a fire-and-forget goroutine — that way a failed
		// write surfaces in logs instead of silently dropping the location while
		// the handler still returns success.
		if redisClient != nil {
			ctx := context.Background()
			locationJSON, _ := json.Marshal(locationData)
			if err := redisClient.SaveDriverLocation(ctx, driverID, string(locationJSON)); err != nil {
				log.Printf("⚠️  [LocationProxy] Failed to save to Redis: %v", err)
			}
		}

		// 4.5 Check warehouse proximity for auto-end (non-blocking)
		if locationData.ShiftID != nil && centrifugoClient != nil {
			go checkWarehouseProximity(db, centrifugoClient, fcmService, driverID, *locationData.ShiftID, locationData.Latitude, locationData.Longitude)
		}

		// 5. Broadcast the RAW fix — no synchronous OSRM snap.
		// This proxy sits on Centrifugo's critical path: Centrifugo blocks the
		// broadcast until we respond, so a per-fix SnapToRoad round-trip (worst
		// case a public-OSRM call, then a proxy timeout → the driver's HTTP
		// fallback → a SECOND OSRM call) injected multi-second delivery jitter
		// and reordering — a measured contributor to the marker's stutter. The
		// manager client already snaps the marker to its guide geometry, so
		// snapping here was redundant latency. (osrmClient stays in the
		// signature for other consumers / future async use.)
		_ = osrmClient

		// 6. Return the fix to Centrifugo for broadcast, unmodified.
		modifiedData := map[string]interface{}{
			"latitude":  locationData.Latitude,
			"longitude": locationData.Longitude,
			"accuracy":  locationData.Accuracy,
			"heading":   locationData.Heading,
			"speed":     locationData.Speed,
			"timestamp": locationData.Timestamp,
		}

		if locationData.ShiftID != nil {
			modifiedData["shift_id"] = *locationData.ShiftID
		}

		// log.Printf("✅ [LocationProxy] Returning modified data to Centrifugo for broadcast")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoPublishResponse{
			Result: &CentrifugoPublishResult{
				Data:        modifiedData,
				SkipHistory: false, // Keep in channel history for recovery
			},
		})
	}
}

// parseLocationData extracts and validates location data from the request
func parseLocationData(data map[string]interface{}) (*LocationData, error) {
	location := &LocationData{}

	// Required fields
	lat, ok := data["latitude"].(float64)
	if !ok {
		return nil, fmt.Errorf("latitude is required and must be a number")
	}
	location.Latitude = lat

	lng, ok := data["longitude"].(float64)
	if !ok {
		return nil, fmt.Errorf("longitude is required and must be a number")
	}
	location.Longitude = lng

	// Optional fields with defaults
	if accuracy, ok := data["accuracy"].(float64); ok {
		location.Accuracy = accuracy
	} else {
		location.Accuracy = 100.0 // Default to 100m if not provided
	}

	if heading, ok := data["heading"].(float64); ok {
		location.Heading = heading
	}

	if speed, ok := data["speed"].(float64); ok {
		location.Speed = speed
	}

	if timestamp, ok := data["timestamp"].(float64); ok {
		location.Timestamp = int64(timestamp)
	}

	if shiftID, ok := data["shift_id"].(string); ok {
		location.ShiftID = &shiftID
	}

	// Validate coordinates
	if location.Latitude < -90 || location.Latitude > 90 {
		return nil, fmt.Errorf("invalid latitude: %f", location.Latitude)
	}

	if location.Longitude < -180 || location.Longitude > 180 {
		return nil, fmt.Errorf("invalid longitude: %f", location.Longitude)
	}

	return location, nil
}

const warehouseProximityMeters = 300.0
const autoEndTimeoutMinutes = 5

// checkWarehouseProximity checks if a driver is near the warehouse with all tasks done.
// If so, prompts the driver to end the shift. If ignored for 5 minutes, auto-ends.
func checkWarehouseProximity(db *sqlx.DB, centrifugoClient *centrifugo.Client, fcmService *services.FCMService, driverID string, shiftID string, driverLat, driverLon float64) {
	// Runs in a fire-and-forget goroutine — recover from any panic (e.g. a nil
	// deref) so it can't take down the whole server.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("❌ [PROXIMITY] Recovered from panic in checkWarehouseProximity (driver=%s, shift=%s): %v", driverID, shiftID, rec)
		}
	}()

	// 1. Get shift status and ready_to_end_at
	var shift struct {
		Status             string   `db:"status"`
		ReadyToEndAt       *int64   `db:"ready_to_end_at"`
		WarehouseLatitude  *float64 `db:"warehouse_latitude"`
		WarehouseLongitude *float64 `db:"warehouse_longitude"`
		CompletedBins      int      `db:"completed_bins"`
		TotalBins          int      `db:"total_bins"`
		StartTime          *int64   `db:"start_time"`
		TotalPauseSeconds  int      `db:"total_pause_seconds"`
		RouteID            *string  `db:"route_id"`
		CreatedAt          int64    `db:"created_at"`
		DriverID           string   `db:"driver_id"`
	}
	err := db.Get(&shift, `SELECT status, ready_to_end_at, warehouse_latitude, warehouse_longitude, completed_bins, total_bins, start_time, total_pause_seconds, route_id, created_at, driver_id FROM shifts WHERE id = $1`, shiftID)
	if err != nil || shift.Status != "active" {
		return
	}

	// 2. If ready_to_end_at is already set, check for 5-minute timeout
	if shift.ReadyToEndAt != nil {
		readyAt := time.Unix(*shift.ReadyToEndAt, 0)
		if time.Since(readyAt) >= time.Duration(autoEndTimeoutMinutes)*time.Minute {
			log.Printf("⏰ [PROXIMITY] Auto-ending shift %s — driver ignored prompt for %d minutes", shiftID[:12], autoEndTimeoutMinutes)
			proximityAutoEndShift(db, centrifugoClient, fcmService, shiftID, shift.DriverID)
		}
		return // Already prompted, waiting for response or timeout
	}

	// 3. Check if warehouse coordinates exist
	if shift.WarehouseLatitude == nil || shift.WarehouseLongitude == nil {
		return
	}

	// 4. Check all non-warehouse tasks are done
	var incompleteCount int
	err = db.Get(&incompleteCount, `
		SELECT COUNT(*) FROM route_tasks
		WHERE shift_id = $1 AND is_deleted = false
		AND task_type != 'warehouse_stop' AND is_completed = 0
	`, shiftID)
	if err != nil || incompleteCount > 0 {
		return // Still has incomplete tasks
	}

	// 5. Calculate distance to warehouse
	distance := geo.HaversineMeters(driverLat, driverLon, *shift.WarehouseLatitude, *shift.WarehouseLongitude)
	if distance > warehouseProximityMeters {
		return // Not close enough
	}

	// 6. All conditions met — driver near warehouse with all tasks done
	log.Printf("📍 [PROXIMITY] Driver near warehouse (%.0fm), all tasks done — prompting to end shift %s", distance, shiftID[:12])

	// Set ready_to_end_at + auto-complete the warehouse stops in ONE tx —
	// previously two pool writes, so a failure between them could prompt the
	// driver to end (and auto-end 5 min later) while an incomplete
	// warehouse_stop lingered in the itinerary. Best-effort as before: a
	// failure just logs; the next location publish retries.
	now := time.Now().Unix()
	if tx, txErr := db.Beginx(); txErr != nil {
		log.Printf("⚠️  [PROXIMITY] Failed to begin tx for shift %s: %v", shiftID, txErr)
	} else {
		committed := false
		defer func() {
			if !committed {
				_ = tx.Rollback()
			}
		}()
		if _, err := tx.Exec(`UPDATE shifts SET ready_to_end_at = $1 WHERE id = $2`, now, shiftID); err != nil {
			log.Printf("⚠️  [PROXIMITY] Failed to set ready_to_end_at for shift %s: %v", shiftID, err)
			return
		}
		if _, err := itinerary.CompleteWarehouseStops(tx, shiftID, now); err != nil {
			log.Printf("⚠️  [PROXIMITY] Failed to auto-complete warehouse_stop tasks for shift %s: %v", shiftID, err)
			return
		}
		if err := tx.Commit(); err != nil {
			log.Printf("⚠️  [PROXIMITY] Failed to commit proximity tx for shift %s: %v", shiftID, err)
			return
		}
		committed = true
	}

	// Send Centrifugo event to driver
	ctx := context.Background()
	centrifugoClient.PublishDriverEvent(ctx, driverID, "shift_ready_to_end", map[string]interface{}{
		"shift_id":        shiftID,
		"distance_meters": int(distance),
		"completed_bins":  shift.CompletedBins,
		"total_bins":      shift.TotalBins,
	})

	// Send FCM push notification
	if fcmService != nil {
		var tokens []string
		db.Select(&tokens, `SELECT token FROM fcm_tokens WHERE user_id = $1`, driverID)
		if len(tokens) > 0 {
			fcmService.SendMulticast(
				tokens,
				"All Tasks Complete!",
				"You've arrived at the warehouse. Tap to end your shift.",
				map[string]string{
					"type":     "shift_ready_to_end",
					"shift_id": shiftID,
				},
			)
		}
	}
}

// proximityAutoEndShift auto-ends a shift after the driver ignored the proximity prompt.
func proximityAutoEndShift(db *sqlx.DB, centrifugoClient *centrifugo.Client, fcmService *services.FCMService, shiftID string, driverID string) {
	now := time.Now().Unix()

	// Get full shift for archiving
	var shift struct {
		StartTime         *int64  `db:"start_time"`
		TotalBins         int     `db:"total_bins"`
		CompletedBins     int     `db:"completed_bins"`
		TotalPauseSeconds int     `db:"total_pause_seconds"`
		RouteID           *string `db:"route_id"`
		CreatedAt         int64   `db:"created_at"`
	}
	err := db.Get(&shift, `SELECT start_time, total_bins, completed_bins, total_pause_seconds, route_id, created_at FROM shifts WHERE id = $1`, shiftID)
	if err != nil {
		log.Printf("❌ [PROXIMITY] Failed to get shift %s for auto-end: %v", shiftID[:12], err)
		return
	}

	completionRate := 0.0
	if shift.TotalBins > 0 {
		completionRate = (float64(shift.CompletedBins) / float64(shift.TotalBins)) * 100
	}

	// Archive to shift_history
	if _, err := db.Exec(`
		INSERT INTO shift_history (
			id, driver_id, route_id, start_time, end_time, created_at, ended_at,
			total_pause_seconds, total_bins, completed_bins, completion_rate,
			incidents_reported, field_observations,
			end_reason, ended_by_user_id, end_reason_metadata, optimization_metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (id) DO NOTHING
	`, shiftID, driverID, shift.RouteID, shift.StartTime, now, shift.CreatedAt, now,
		shift.TotalPauseSeconds, shift.TotalBins, shift.CompletedBins, completionRate,
		0, 0, "completed", nil, nil, nil); err != nil {
		log.Printf("⚠️  [PROXIMITY] Failed to archive shift %s to shift_history: %v", shiftID[:12], err)
	}

	// Update shift status
	if _, err := db.Exec(`UPDATE shifts SET status = 'ended', end_time = $1, pause_start_time = NULL, ready_to_end_at = NULL, updated_at = $2 WHERE id = $3`, now, now, shiftID); err != nil {
		log.Printf("⚠️  [PROXIMITY] Failed to update shift %s status to ended: %v", shiftID[:12], err)
	}

	// Return incomplete move requests to pending
	if _, err := db.Exec(`UPDATE bin_move_requests SET status = 'pending', assigned_shift_id = NULL, updated_at = $1 WHERE assigned_shift_id = $2 AND status = 'in_progress'`, now, shiftID); err != nil {
		log.Printf("⚠️  [PROXIMITY] Failed to return incomplete move requests to pending for shift %s: %v", shiftID[:12], err)
	}

	log.Printf("✅ [PROXIMITY] Shift %s auto-ended — driver near warehouse, all tasks done", shiftID[:12])

	// Notify driver
	ctx := context.Background()
	centrifugoClient.PublishDriverEvent(ctx, driverID, "shift_auto_ended", map[string]interface{}{
		"shift_id": shiftID,
		"reason":   "proximity_timeout",
	})

	if fcmService != nil {
		var tokens []string
		db.Select(&tokens, `SELECT token FROM fcm_tokens WHERE user_id = $1`, driverID)
		if len(tokens) > 0 {
			fcmService.SendMulticast(tokens, "Shift Ended", "Your shift has ended. Great work!", map[string]string{
				"type": "shift_auto_ended", "shift_id": shiftID,
			})
		}
	}
}
