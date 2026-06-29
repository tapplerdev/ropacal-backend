package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// shiftHistoryExec is satisfied by both *sqlx.DB and *sqlx.Tx, allowing
// archiveShift to run against a plain connection or inside a transaction.
func PreflightCheck(db *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/driver/shift/preflight")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Initialize response
		checks := map[string]interface{}{
			"gps_quality":          "unknown",
			"location_cached":      false,
			"can_optimize":         false,
			"centrifugo_connected": true, // Assume true if they can call this
		}
		ready := false
		message := ""
		retryAfter := 2 // seconds

		// Check 1: Resolve location via the SAME resolver StartShift uses (Redis
		// primary, durable driver_current_location fallback) — so the start-gate and
		// the start action can never disagree on whether the driver is "located".
		loc, locErr := resolveDriverStartLocation(r.Context(), db, redisClient, userClaims.UserID)
		if locErr != nil {
			switch {
			case errors.Is(locErr, errDriverLocationStale):
				message = "Location is stale - move to an open area"
			case errors.Is(locErr, errNoDriverLocation):
				message = "Location syncing - Please wait..."
			default:
				log.Printf("❌ Preflight location lookup error: %v", locErr)
				message = "Location service unavailable - retrying..."
			}
			checks["location_cached"] = false

			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success":     true,
				"ready":       ready,
				"checks":      checks,
				"message":     message,
				"retry_after": retryAfter,
			})
			return
		}

		checks["location_cached"] = true
		checks["location_source"] = loc.Source

		// Check 2: GPS accuracy
		accuracy := loc.Accuracy
		log.Printf("✅ Location resolved via %s: (%.6f, %.6f), accuracy: %.1fm",
			loc.Source, loc.Latitude, loc.Longitude, accuracy)

		// Evaluate GPS quality based on accuracy
		if accuracy <= 10 {
			checks["gps_quality"] = "excellent"
		} else if accuracy <= 50 {
			checks["gps_quality"] = "good"
		} else if accuracy <= 100 {
			checks["gps_quality"] = "fair"
		} else {
			checks["gps_quality"] = "poor"
			message = "GPS signal weak - Move to open area"

			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success":     true,
				"ready":       ready,
				"checks":      checks,
				"message":     message,
				"retry_after": retryAfter,
			})
			return
		}

		// Check 3: Verify shift is ready to start
		var shift models.Shift
		shiftQuery := `SELECT * FROM shifts
		              WHERE driver_id = $1
		              AND status = 'ready'
		              ORDER BY created_at DESC
		              LIMIT 1`

		shiftErr := db.Get(&shift, shiftQuery, userClaims.UserID)
		if shiftErr != nil {
			log.Printf("❌ No ready shift found: %v", shiftErr)
			message = "No shift assigned"

			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success":     true,
				"ready":       ready,
				"checks":      checks,
				"message":     message,
				"retry_after": retryAfter,
			})
			return
		}

		checks["can_optimize"] = true
		ready = true
		message = "Ready to start shift"

		// Check if shift has tasks requiring warehouse bins (placements or redeployments)
		needsWarehouseBins := false
		placementCount := 0
		redeploymentCount := 0

		var tasks []struct {
			TaskType string  `db:"task_type"`
			MoveType *string `db:"move_type"`
		}

		tasksQuery := `
			SELECT rt.task_type, mr.move_type
			FROM route_tasks rt
			LEFT JOIN bin_move_requests mr ON rt.move_request_id = mr.id
			WHERE rt.shift_id = $1 AND rt.is_deleted = false
		`

		if err := db.Select(&tasks, tasksQuery, shift.ID); err == nil {
			for _, task := range tasks {
				if task.TaskType == "placement" {
					needsWarehouseBins = true
					placementCount++
				} else if task.TaskType == "pickup" && task.MoveType != nil && *task.MoveType == "redeployment" {
					needsWarehouseBins = true
					redeploymentCount++
				}
			}
		}

		if needsWarehouseBins {
			log.Printf("🏭 Shift has %d placements + %d redeployments - will need bins prompt", placementCount, redeploymentCount)
		}

		log.Printf("✅ Preflight checks passed:")
		log.Printf("   GPS Quality: %s (%.1fm)", checks["gps_quality"], accuracy)
		log.Printf("   Location Cached: %v", checks["location_cached"])
		log.Printf("   Can Optimize: %v", checks["can_optimize"])
		log.Printf("   Estimated Start Time: < 5 seconds")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":                     true,
			"ready":                       ready,
			"checks":                      checks,
			"message":                     message,
			"estimated_start_time":        "< 5 seconds",
			"needs_warehouse_bins_prompt": needsWarehouseBins,
			"placement_count":             placementCount,
			"redeployment_count":          redeploymentCount,
			"location": map[string]float64{
				"latitude":  loc.Latitude,
				"longitude": loc.Longitude,
				"accuracy":  accuracy,
			},
		})
	}
}

// driverStartLocation is a resolved GPS fix used as a route's start point.
func StartShift(db *sqlx.DB, hub *websocket.Hub, redisClient *redis.Client, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/driver/shift/start")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		var req struct {
			BinsPreloaded bool `json:"bins_preloaded"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("   ℹ️  No request body or parse error (ignored): %v", err)
		}
		log.Printf("   🚚 Bins preloaded: %v", req.BinsPreloaded)

		// Check if driver has any existing active or paused shift
		var existingShift models.Shift
		existingQuery := `SELECT * FROM shifts
					  WHERE driver_id = $1
					  AND (status = 'active' OR status = 'paused')
					  LIMIT 1`

		existingErr := db.Get(&existingShift, existingQuery, userClaims.UserID)
		if existingErr == nil {
			// IDEMPOTENCY FIX: If shift is already active, just return it (don't end it)
			if existingShift.Status == "active" {
				log.Printf("✅ Shift already active (%s), returning existing shift (idempotent)", existingShift.ID)
				log.Printf("📤 RESPONSE: 200 OK - Returning existing active shift")
				utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
					"success": true,
					"data":    existingShift,
				})
				return
			}

			// Found an existing PAUSED shift - auto-end it
			log.Printf("⚠️  Found existing paused shift (%s), auto-ending it before starting new shift", existingShift.ID)

			endNow := time.Now().Unix()
			totalPause := int64(existingShift.TotalPauseSeconds)
			if existingShift.PauseStartTime != nil {
				totalPause += endNow - *existingShift.PauseStartTime
			}

			// Determine end reason - auto-ended because driver started new shift
			endReason := "manual_end"
			if existingShift.CompletedBins >= existingShift.TotalBins {
				endReason = "completed"
			}

			// Get optimization metadata JSON from the shift
			var optMeta interface{}
			if existingShift.OptimizationMetadata != nil {
				if b, e := json.Marshal(existingShift.OptimizationMetadata); e == nil {
					optMeta = b
				}
			}

			_, histErr := archiveShift(db, archiveShiftParams{
				ID:                existingShift.ID,
				DriverID:          existingShift.DriverID,
				RouteID:           existingShift.RouteID,
				StartTime:         existingShift.StartTime,
				EndTime:           endNow,
				CreatedAt:         existingShift.CreatedAt,
				EndedAt:           endNow,
				TotalPauseSeconds: totalPause,
				TotalBins:         existingShift.TotalBins,
				CompletedBins:     existingShift.CompletedBins,
				IncidentsReported: 0,
				FieldObservations: 0,
				EndReason:         endReason,
				EndedByUserID:     nil, // Driver action
				EndReasonMetadata: nil, // No metadata
				OptMeta:           optMeta,
			})
			if histErr != nil {
				log.Printf("❌ Error saving auto-ended shift to history: %v", histErr)
				// Continue anyway
			}

			endQuery := `UPDATE shifts
					 SET status = 'ended',
						 end_time = $1,
						 total_pause_seconds = $2,
						 pause_start_time = NULL,
						 updated_at = $3
					 WHERE id = $4`

			_, err := db.Exec(endQuery, endNow, totalPause, endNow, existingShift.ID)
			if err != nil {
				log.Printf("❌ Error auto-ending existing shift: %v", err)
				// Don't fail - continue with starting new shift
			} else {
				log.Printf("✅ Auto-ended existing shift %s (saved to history)", existingShift.ID)
			}
		}

		// Check if driver has a ready shift
		var shift models.Shift
		query := `SELECT * FROM shifts
				  WHERE driver_id = $1
				  AND status = 'ready'
				  LIMIT 1`

		err := db.Get(&shift, query, userClaims.UserID)
		if err == sql.ErrNoRows {
			log.Printf("📤 RESPONSE: 400 - No route assigned")
			utils.RespondError(w, http.StatusBadRequest, "No route assigned. Contact your manager.")
			return
		}
		if err != nil {
			log.Printf("❌ Error getting shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Database error")
			return
		}

		// Smart Route Optimization Logic
		// If lock_route_order is true, skip optimization (use manager's exact task order)
		// If lock_route_order is false, run full route optimization with dynamic warehouse insertion
		if shift.LockRouteOrder {
			log.Printf("🔒 Route order is locked - skipping optimization and using manager's exact task sequence")
		} else {
			log.Printf("🚀 Route order unlocked - performing smart optimization with dynamic warehouse insertion")

			// Resolve the driver's GPS start point (Redis primary, durable DB fallback,
			// freshness- and null-island-guarded — see resolveDriverStartLocation).
			var driverLocation struct {
				Latitude  float64
				Longitude float64
			}
			loc, locErr := resolveDriverStartLocation(r.Context(), db, redisClient, userClaims.UserID)
			if locErr != nil {
				if errors.Is(locErr, errNoDriverLocation) || errors.Is(locErr, errDriverLocationStale) {
					log.Printf("❌ No usable GPS to start shift: %v", locErr)
					utils.RespondError(w, http.StatusBadRequest, "Please enable GPS to start shift")
				} else {
					log.Printf("❌ Location service error starting shift: %v", locErr)
					utils.RespondError(w, http.StatusInternalServerError, "Location service unavailable")
				}
				return
			}
			driverLocation.Latitude = loc.Latitude
			driverLocation.Longitude = loc.Longitude
			log.Printf("✅ Driver start location via %s: (%.6f, %.6f)", loc.Source, loc.Latitude, loc.Longitude)

			// Validate warehouse coordinates (required for standard shifts, optional for custom)
			if shift.ShiftType != "custom" && (shift.WarehouseLatitude == nil || shift.WarehouseLongitude == nil) {
				log.Printf("❌ Warehouse coordinates not set for shift")
				utils.RespondError(w, http.StatusInternalServerError, "Warehouse location not configured")
				return
			}

			// Run Mapbox v2 route optimization (capacity-aware, automatic warehouse trips)
			capacity := 4 // Default capacity
			if shift.TruckBinCapacity != nil {
				capacity = *shift.TruckBinCapacity
			}

			// For custom shifts with no warehouse, use 0 capacity (no bin constraints)
			warehouseLat := 0.0
			warehouseLon := 0.0
			warehouseAddr := ""
			if shift.WarehouseLatitude != nil {
				warehouseLat = *shift.WarehouseLatitude
			}
			if shift.WarehouseLongitude != nil {
				warehouseLon = *shift.WarehouseLongitude
			}
			if shift.WarehouseAddress != nil {
				warehouseAddr = *shift.WarehouseAddress
			}

			err = optimizeRouteWithMapbox(
				db,
				shift.ID,
				capacity,
				driverLocation.Latitude,
				driverLocation.Longitude,
				warehouseLat,
				warehouseLon,
				warehouseAddr,
				req.BinsPreloaded,
				true,                // isFirstOptimization = true (shift starting)
				shift.StartLatitude, // Custom start location (nil for standard shifts)
				shift.StartLongitude,
				shift.StartAddress,
				shift.EndLatitude, // Custom end location (nil for standard shifts)
				shift.EndLongitude,
				shift.EndAddress,
			)

			if err != nil {
				log.Printf("❌ Mapbox v2 route optimization failed: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Route optimization failed")
				return
			}

			log.Printf("✅ Mapbox v2 route optimization complete")
		}

		// Calculate preloaded bins count
		preloadedBins := 0
		if req.BinsPreloaded {
			var count int
			err = db.Get(&count, `SELECT COUNT(*) FROM route_tasks WHERE shift_id = $1 AND task_type = 'placement' AND is_deleted = false`, shift.ID)
			if err == nil {
				preloadedBins = count
			}
			log.Printf("📦 Preloaded bins saved: %d", preloadedBins)
		}

		// Update shift to active
		now := time.Now().Unix()
		updateQuery := `UPDATE shifts
					SET status = 'active',
						start_time = $1,
						updated_at = $2,
						preloaded_bins = $3
					WHERE id = $4`

		_, err = db.Exec(updateQuery, now, now, preloadedBins, shift.ID)
		if err != nil {
			log.Printf("❌ Error starting shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start shift")
			return
		}

		// Update all assigned move requests for this shift to in_progress
		updateMovesQuery := `UPDATE bin_move_requests
						 SET status = 'in_progress', updated_at = $1
						 WHERE assigned_shift_id = $2
						 AND status = 'assigned'`
		result, err := db.Exec(updateMovesQuery, now, shift.ID)
		if err != nil {
			log.Printf("⚠️ Error updating move requests to in_progress: %v", err)
			// Don't fail the request - continue
		} else {
			rowsAffected, _ := result.RowsAffected()
			if rowsAffected > 0 {
				log.Printf("✅ Updated %d move request(s) to in_progress", rowsAffected)

				// Broadcast move request status update to dashboard
				moveReqData := map[string]interface{}{
					"shift_id":   shift.ID,
					"new_status": "in_progress",
					"count":      rowsAffected,
					"updated_at": now,
				}
				hub.BroadcastToRole("admin", map[string]interface{}{
					"type": "move_request_status_updated",
					"data": moveReqData,
				})
				hub.BroadcastToRole("manager", map[string]interface{}{
					"type": "move_request_status_updated",
					"data": moveReqData,
				})
				log.Printf("📡 Broadcast move_request_status_updated to managers: %d move requests → in_progress", rowsAffected)

				// Publish to Centrifugo
				if centrifugoClient != nil {
					if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "move_request_status_updated", moveReqData); pubErr != nil {
						log.Printf("⚠️  Failed to publish move_request_status_updated to Centrifugo: %v", pubErr)
					}
				}
			}
		}

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		log.Printf("✅ Shift started: %s (Driver: %s)", shift.ID, userClaims.Email)
		log.Printf("📤 RESPONSE: 200 OK - Returning immediately to mobile")
		log.Printf("   Shift ID: %s", shift.ID)
		log.Printf("   Status: %s", shift.Status)
		log.Printf("   Start Time: %v", shift.StartTime)
		log.Printf("   Route: %v", shift.RouteID)

		// Return HTTP response IMMEDIATELY (don't wait for broadcasts)
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    shift,
		})

		// Do WebSocket broadcasts in background (async - don't block HTTP response)
		go func() {
			log.Printf("📡 [ASYNC] Starting background broadcasts for shift %s", shift.ID)

			// Get route bins with details for WebSocket broadcast
			bins, err := getShiftTasksWithDetails(db, shift.ID)
			if err != nil {
				log.Printf("❌ [ASYNC] Error fetching route bins for WebSocket: %v", err)
				bins = []models.ShiftBinWithDetails{} // Empty array on error
			}

			// Broadcast WebSocket update to driver (include tasks!)
			shiftUpdateData := map[string]interface{}{
				"id":                  shift.ID,
				"driver_id":           shift.DriverID,
				"route_id":            shift.RouteID,
				"status":              shift.Status,
				"start_time":          shift.StartTime,
				"end_time":            shift.EndTime,
				"total_pause_seconds": shift.TotalPauseSeconds,
				"pause_start_time":    shift.PauseStartTime,
				"total_bins":          shift.TotalBins,
				"completed_bins":      shift.CompletedBins,
				"tasks":               bins,
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
			}
			hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
				"type": "shift_update",
				"data": shiftUpdateData,
			})

			// Also publish via Centrifugo shift channel
			if centrifugoClient != nil {
				if pubErr := centrifugoClient.PublishShiftUpdate(context.Background(), shift.ID, map[string]interface{}{
					"type": "shift_update",
					"data": shiftUpdateData,
				}); pubErr != nil {
					log.Printf("⚠️  Failed to publish shift_update to Centrifugo: %v", pubErr)
				}
			}

			// Broadcast shift state change to all managers
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("📡 [ASYNC] BROADCASTING driver_shift_change TO MANAGERS")
			log.Printf("   Driver ID: %s", shift.DriverID)
			log.Printf("   Driver Email: %s", userClaims.Email)
			log.Printf("   Status: %s", shift.Status)
			log.Printf("   Shift ID: %s", shift.ID)

			broadcastData := map[string]interface{}{
				"type": "driver_shift_change",
				"data": map[string]interface{}{
					"driver_id": shift.DriverID,
					"status":    shift.Status,
					"shift_id":  shift.ID,
				},
			}
			log.Printf("   Broadcast payload: %+v", broadcastData)

			hub.BroadcastToRole("admin", broadcastData)
			hub.BroadcastToRole("manager", broadcastData)
			log.Printf("   ✅ [ASYNC] BroadcastToRole('admin' + 'manager') called")

			// Also publish via Centrifugo for mobile app notification pipeline
			if centrifugoClient != nil {
				if pubErr := centrifugoClient.PublishCompanyEvent(context.Background(), "driver_shift_change", map[string]interface{}{
					"driver_id": shift.DriverID,
					"status":    shift.Status,
					"shift_id":  shift.ID,
				}); pubErr != nil {
					log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
				} else {
					log.Printf("📡 Published driver_shift_change via Centrifugo (start)")
				}
			}

			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("✅ [ASYNC] Background broadcasts complete for shift %s", shift.ID)
		}()
	}
}

// PauseShift pauses an active shift
func PauseShift(store ShiftStore, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/driver/shift/pause")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		ctx := r.Context()
		now := time.Now().Unix()

		shift, err := store.PauseByDriver(ctx, userClaims.UserID, now)
		if err == sql.ErrNoRows {
			utils.RespondError(w, http.StatusBadRequest, "No active shift to pause")
			return
		}
		if err != nil {
			log.Printf("❌ Error pausing shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to pause shift")
			return
		}

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
		})

		// Publish shift_update via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": shift,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_update to Centrifugo: %v", pubErr)
			}
		}

		// Broadcast shift state change to all managers
		broadcastPayload := map[string]interface{}{
			"type": "driver_shift_change",
			"data": map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    shift.Status,
				"shift_id":  shift.ID,
			},
		}
		hub.BroadcastToRole("admin", broadcastPayload)
		hub.BroadcastToRole("manager", broadcastPayload)
		log.Printf("📡 Broadcast driver_shift_change to managers: Driver paused shift")

		// Also publish driver_shift_change via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    shift.Status,
				"shift_id":  shift.ID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
			}
		}

		log.Printf("⏸️  Shift paused: %s", shift.ID)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": pauseResponse{
				PauseStartTime: shift.PauseStartTime,
				Status:         shift.Status,
			},
		})
	}
}

// ResumeShift resumes a paused shift
func ResumeShift(store ShiftStore, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		ctx := r.Context()

		// Get the paused shift
		paused, err := store.PausedByDriver(ctx, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No paused shift to resume")
			return
		}

		// Calculate accumulated pause duration
		pauseDuration := 0
		if paused.PauseStartTime != nil {
			pauseDuration = int(time.Now().Unix() - *paused.PauseStartTime)
		}
		totalPause := paused.TotalPauseSeconds + pauseDuration

		// Flip back to active with the updated pause total
		now := time.Now().Unix()
		shift, err := store.ResumeByID(ctx, paused.ID, int64(totalPause), now)
		if err != nil {
			log.Printf("❌ Error resuming shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to resume shift")
			return
		}

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
		})

		// Publish shift_update via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": shift,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_update to Centrifugo: %v", pubErr)
			}
		}

		// Broadcast shift state change to all managers
		broadcastPayload := map[string]interface{}{
			"type": "driver_shift_change",
			"data": map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    shift.Status,
				"shift_id":  shift.ID,
			},
		}
		hub.BroadcastToRole("admin", broadcastPayload)
		hub.BroadcastToRole("manager", broadcastPayload)
		log.Printf("📡 Broadcast driver_shift_change to managers: Driver resumed shift")

		// Also publish driver_shift_change via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    shift.Status,
				"shift_id":  shift.ID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
			}
		}

		log.Printf("▶️  Shift resumed: %s (total pause: %ds)", shift.ID, totalPause)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": resumeResponse{
				Status:            shift.Status,
				TotalPauseSeconds: shift.TotalPauseSeconds,
			},
		})
	}
}

// EndShift ends the current shift
func EndShift(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Get current shift
		var shift models.Shift
		query := `SELECT * FROM shifts
				  WHERE driver_id = $1
				  AND (status = 'active' OR status = 'paused')
				  LIMIT 1`

		err := db.Get(&shift, query, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift to end")
			return
		}

		// Calculate durations
		now := time.Now().Unix()
		endTime := now

		totalDuration := int64(0)
		if shift.StartTime != nil {
			totalDuration = endTime - *shift.StartTime
		}

		// Add current pause if still paused
		totalPause := int64(shift.TotalPauseSeconds)
		if shift.PauseStartTime != nil {
			totalPause += now - *shift.PauseStartTime
		}

		activeDuration := totalDuration - totalPause

		// Calculate completion rate
		completionRate := 0.0
		if shift.TotalBins > 0 {
			completionRate = (float64(shift.CompletedBins) / float64(shift.TotalBins)) * 100
		}

		// Count incidents reported during this shift
		var incidentStats struct {
			TotalIncidents    int `db:"total_incidents"`
			FieldObservations int `db:"field_observations"`
		}
		err = db.Get(&incidentStats, `
			SELECT
				COUNT(*) as total_incidents,
				COUNT(*) FILTER (WHERE is_field_observation = true) as field_observations
			FROM zone_incidents
			WHERE shift_id = $1
		`, shift.ID)
		if err != nil {
			log.Printf("⚠️  Warning: Failed to count incidents for shift: %v", err)
			// Continue anyway - this is not critical
			incidentStats.TotalIncidents = 0
			incidentStats.FieldObservations = 0
		}

		// Determine end reason
		endReason := "manual_end" // Default: driver ended shift manually
		if shift.CompletedBins >= shift.TotalBins {
			endReason = "completed" // All bins completed
		}

		// Insert into shift_history BEFORE updating shift status
		var optMeta interface{}
		if shift.OptimizationMetadata != nil {
			if b, e := json.Marshal(shift.OptimizationMetadata); e == nil {
				optMeta = b
			}
		}

		// Archive + end + release + task cleanup run as ONE transaction, so a kill
		// mid-sequence can't leave a half-ended shift. Broadcasts fire post-commit.
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction to end shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to end shift")
			return
		}
		defer tx.Rollback() // no-op once Commit succeeds

		_, err = archiveShift(tx, archiveShiftParams{
			ID:                shift.ID,
			DriverID:          shift.DriverID,
			RouteID:           shift.RouteID,
			StartTime:         shift.StartTime,
			EndTime:           endTime, // end_time
			CreatedAt:         shift.CreatedAt,
			EndedAt:           now,        // ended_at (when history record created)
			TotalPauseSeconds: totalPause, // total_pause_seconds
			TotalBins:         shift.TotalBins,
			CompletedBins:     shift.CompletedBins,
			IncidentsReported: incidentStats.TotalIncidents,    // incidents_reported
			FieldObservations: incidentStats.FieldObservations, // field_observations
			EndReason:         endReason,
			EndedByUserID:     nil, // ended_by_user_id (NULL - driver action)
			EndReasonMetadata: nil, // end_reason_metadata (NULL for basic driver ends)
			OptMeta:           optMeta,
		})
		if err != nil {
			log.Printf("❌ Error inserting shift history: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save shift history")
			return
		}

		log.Printf("✅ Shift history saved: %s (reason: %s, completion: %.1f%%)", shift.ID, endReason, completionRate)

		// Update shift
		log.Printf("🔄 Ending shift: %s (status: ended)", shift.ID)
		updateQuery := `UPDATE shifts
						SET status = 'ended',
							end_time = $1,
							total_pause_seconds = $2,
							pause_start_time = NULL,
							updated_at = $3
						WHERE id = $4`

		_, err = tx.Exec(updateQuery, endTime, totalPause, now, shift.ID)
		if err != nil {
			log.Printf("❌ Error ending shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to end shift")
			return
		}
		log.Printf("✅ Shift ended successfully")

		// Return incomplete move requests to the pending pool (shared helper) and
		// log the unassignment history for each. (Was an inline SELECT + UPDATE
		// copy-pasted across End/Cancel/CancelAll; the release now lives in
		// releaseShiftMoveRequests so it can't drift between them.)
		released, relErr := moverequest.ReleaseFromShift(tx, []string{shift.ID}, now)
		if relErr != nil {
			log.Printf("⚠️ Error returning incomplete move requests to backlog: %v", relErr)
		}
		for _, mr := range released {
			metadata := fmt.Sprintf(`{"shift_id":"%s","end_reason":"manual_end"}`, shift.ID)
			if logErr := moverequest.LogUnassigned(
				tx, mr.ID, userClaims.UserID, userClaims.Email,
				mr.AssignmentType, mr.AssignedUserID, mr.AssignedUserName, mr.AssignedShiftID,
			); logErr != nil {
				log.Printf("⚠️ Failed to log move request unassignment history for %s: %v", mr.ID, logErr)
			}
			// Also annotate the notes field with end-specific context.
			notesQuery := `UPDATE move_request_history SET notes = $1, metadata = $2 WHERE move_request_id = $3 AND action_type = 'unassigned' AND created_at = (SELECT MAX(created_at) FROM move_request_history WHERE move_request_id = $3 AND action_type = 'unassigned')`
			if _, noteErr := tx.Exec(notesQuery, "Shift ended before completing move request", metadata, mr.ID); noteErr != nil {
				log.Printf("⚠️ Failed to update history notes for %s: %v", mr.ID, noteErr)
			}
		}

		// Soft-delete this shift's incomplete tasks for moves that were just released
		// (now off any shift). The select-then-remove keeps the audited soft-delete in
		// itinerary.RemoveByIDs. #16 fix: the old subquery keyed on status='pending',
		// but the backlog model releases moves to status='assigned' (ReleaseFromShift
		// above), so those tasks never got cleaned — include both.
		var staleTaskIDs []string
		if selErr := tx.Select(&staleTaskIDs, `
			SELECT id FROM route_tasks
			WHERE shift_id = $1 AND is_completed = 0 AND is_deleted = false
			  AND bin_id IN (
				SELECT bin_id FROM bin_move_requests
				WHERE assigned_shift_id IS NULL AND status IN ('pending', 'assigned')
			  )`, shift.ID); selErr != nil {
			log.Printf("⚠️ Error selecting incomplete move tasks to clean: %v", selErr)
		}
		if err = itinerary.RemoveByIDs(tx, staleTaskIDs, userClaims.UserID, "shift_ended_before_completion", now); err != nil {
			// A hard error here aborts the whole end via the transaction below.
			log.Printf("⚠️ Error soft deleting incomplete move tasks from shift: %v", err)
		}

		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing shift end: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to end shift")
			return
		}

		// Get updated shift with bins for WebSocket broadcast
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
		})

		// Publish shift_update via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": shift,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_update to Centrifugo: %v", pubErr)
			}
		}

		// Broadcast shift state change to all managers
		broadcastPayload := map[string]interface{}{
			"type": "driver_shift_change",
			"data": map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    shift.Status,
				"shift_id":  shift.ID,
			},
		}
		hub.BroadcastToRole("admin", broadcastPayload)
		hub.BroadcastToRole("manager", broadcastPayload)
		log.Printf("📡 Broadcast driver_shift_change to managers: Driver ended shift")

		// Also publish driver_shift_change via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    shift.Status,
				"shift_id":  shift.ID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
			}
		}

		log.Printf("🏁 Shift ended: %s (%dm active)", shift.ID, activeDuration/60)

		response := models.ShiftEndResponse{
			Status:                "ended",
			EndTime:               endTime,
			TotalDurationSeconds:  totalDuration,
			ActiveDurationSeconds: activeDuration,
			TotalPauseSeconds:     int(totalPause),
			CompletedBins:         shift.CompletedBins,
			TotalBins:             shift.TotalBins,
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}
}

// CompleteShiftBin marks a task as completed within an active shift (collection, pickup, dropoff, warehouse, placement)
func SkipTask(db *sqlx.DB, redisClient *redis.Client, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/driver/shift/skip-task")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var req struct {
			TaskID string `json:"task_id"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("   Task ID: %s", req.TaskID)
		log.Printf("   Reason: %s", req.Reason)

		// Validate reason is not empty
		if strings.TrimSpace(req.Reason) == "" {
			utils.RespondError(w, http.StatusBadRequest, "Skip reason is required")
			return
		}

		// Get current active shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1`, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift")
			return
		}

		// Get task details to check type and move_request_id
		var task models.RouteTask
		err = db.Get(&task, `SELECT * FROM route_tasks WHERE id = $1 AND shift_id = $2`, req.TaskID, shift.ID)
		if err == sql.ErrNoRows {
			utils.RespondError(w, http.StatusBadRequest, "Task not found in route or already completed")
			return
		}
		if err != nil {
			log.Printf("❌ Error querying route_tasks: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to find task")
			return
		}

		log.Printf("✅ Found task: ID=%s, Type=%s", task.ID, task.TaskType)

		// Check if already completed or skipped
		if task.IsCompleted == 1 {
			utils.RespondError(w, http.StatusBadRequest, "Task already completed or skipped")
			return
		}

		now := time.Now().Unix()

		// Create task_data JSON with skip reason
		skipData := map[string]interface{}{
			"skip_reason": req.Reason,
		}
		skipDataJSON, err := json.Marshal(skipData)
		if err != nil {
			log.Printf("❌ Error marshaling skip data: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to process skip")
			return
		}

		// Start transaction for atomic updates
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to skip task")
			return
		}
		defer tx.Rollback()

		tasksSkipped := 1 // At minimum, we skip the current task

		// Mark the task as skipped
		_, err = tx.Exec(`
			UPDATE route_tasks
			SET skipped = true,
				is_completed = 1,
				completed_at = $1,
				task_data = $2,
				updated_at = $3
			WHERE id = $4
		`, now, skipDataJSON, now, task.ID)
		if err != nil {
			log.Printf("❌ Error marking task as skipped: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to skip task")
			return
		}

		log.Printf("✅ Task marked as skipped: %s", task.ID)

		// If skipping a pickup, also skip the paired dropoff
		if task.TaskType == models.TaskTypePickup && task.MoveRequestID != nil {
			log.Printf("🔗 Pickup task has move_request_id: %s, also skipping dropoff...", *task.MoveRequestID)

			var dropoffID string
			err = tx.QueryRow(`
				SELECT id FROM route_tasks
				WHERE shift_id = $1
				  AND move_request_id = $2
				  AND task_type = 'dropoff'
				  AND is_completed = 0
			`, shift.ID, *task.MoveRequestID).Scan(&dropoffID)

			if err == nil {
				// Found paired dropoff, skip it too
				_, err = tx.Exec(`
					UPDATE route_tasks
					SET skipped = true,
						is_completed = 1,
						completed_at = $1,
						task_data = $2,
						updated_at = $3
					WHERE id = $4
				`, now, skipDataJSON, now, dropoffID)
				if err != nil {
					log.Printf("❌ Error marking dropoff as skipped: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to skip paired dropoff")
					return
				}
				tasksSkipped++
				log.Printf("✅ Paired dropoff also marked as skipped: %s", dropoffID)
			} else if err != sql.ErrNoRows {
				log.Printf("❌ Error querying dropoff: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to find paired dropoff")
				return
			}
		}

		// FIX: Do NOT increment completed_bins for skipped tasks!
		// Skipped tasks should not count toward completion percentage.
		// Only tasks that are actually completed (not skipped) should increment completed_bins.
		// The mobile app now filters remainingTasks by is_completed=0, and counts
		// actual completion by is_completed=1 (not including skipped tasks with is_completed=1 + skipped=true).
		//
		// Previous behavior: Skipped tasks counted toward completed_bins
		// New behavior: Only truly completed tasks count toward completed_bins
		// This prevents premature shift auto-end when drivers skip remaining tasks.
		log.Printf("⏭️  Skipped %d task(s) - NOT incrementing completed_bins (skipped tasks don't count as completed)", tasksSkipped)

		// Only update the shift's updated_at timestamp
		_, err = tx.Exec(`
			UPDATE shifts
			SET updated_at = $1
			WHERE id = $2
		`, now, shift.ID)
		if err != nil {
			log.Printf("❌ Error updating shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift")
			return
		}
		log.Printf("✅ Shift updated for skip (completed_bins unchanged)")

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit skip")
			return
		}

		log.Printf("✅ Transaction committed - %d task(s) skipped", tasksSkipped)

		// Refresh shift data
		err = db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)
		if err != nil {
			log.Printf("⚠️  Error refreshing shift: %v", err)
		}

		// Get updated bin/task list
		bins, err := getShiftTasksWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("⚠️  Error fetching route bins: %v", err)
			bins = []models.ShiftBinWithDetails{}
		}

		// Calculate LOGICAL bin counts (treating pickup+dropoff as 1)
		logicalTotal, logicalCompleted := calculateLogicalBinCounts(bins)

		// Broadcast WebSocket update with bins
		skipTaskUpdateData := map[string]interface{}{
			"id":             shift.ID,
			"status":         shift.Status,
			"completed_bins": logicalCompleted,
			"total_bins":     logicalTotal,
			"tasks":          bins,
		}
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": skipTaskUpdateData,
		})

		// Publish shift_update via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": skipTaskUpdateData,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_update to Centrifugo: %v", pubErr)
			}
		}

		log.Printf("📡 WebSocket: Broadcasted shift update to driver %s", userClaims.UserID)

		// DISABLED: Re-optimize the shift after skipping task (skipGates=false for driver-initiated)
		// Reason: Two-warehouse trick causes suboptimal routes (35min penalty)
		// Accepting current Mapbox Optimization v2 API limitations
		// if err := ReoptimizeActiveShift(db, redisClient, shift.ID, nil, false); err != nil {
		// 	log.Printf("⚠️  Failed to re-optimize shift after task skip: %v", err)
		// 	// Don't fail the request if re-optimization fails
		// } else {
		// 	log.Printf("✅ Successfully re-optimized shift %s after task skip", shift.ID)
		// }
		log.Printf("ℹ️  Re-optimization disabled - driver continues with current route order")

		// Return success
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":       true,
			"tasks_skipped": tasksSkipped,
			"message":       fmt.Sprintf("%d task(s) skipped successfully", tasksSkipped),
		})
	}
}

// RemoveTasksFromShift removes one or more tasks from an active shift (manager-initiated)
// This unassigns tasks without deleting the underlying resources
func AssignRoute(db *sqlx.DB, hub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Check if user is manager
		if userClaims.Role != "manager" && userClaims.Role != "admin" {
			utils.RespondError(w, http.StatusForbidden, "Manager access required")
			return
		}

		// Parse request body
		var req struct {
			DriverID string   `json:"driver_id"`
			RouteID  string   `json:"route_id"`
			BinIDs   []string `json:"bin_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate request
		if len(req.BinIDs) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "At least one bin_id is required")
			return
		}

		log.Printf("📋 Assigning route %s to driver %s with %d bins", req.RouteID, req.DriverID, len(req.BinIDs))
		log.Printf("🔄 Route will be optimized when driver starts shift (based on actual location)")

		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to assign route")
			return
		}
		defer tx.Rollback()

		// Validate all bins exist
		query := `SELECT COUNT(*) FROM bins WHERE id IN (?)`
		query, args, err := sqlx.In(query, req.BinIDs)
		if err != nil {
			log.Printf("❌ Error building query: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to validate bins")
			return
		}
		query = tx.Rebind(query)

		var count int
		err = tx.Get(&count, query, args...)
		if err != nil {
			log.Printf("❌ Error validating bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to validate bins")
			return
		}
		if count != len(req.BinIDs) {
			utils.RespondError(w, http.StatusBadRequest, "One or more bin_ids are invalid")
			return
		}

		// Create new shift (route optimization will happen when driver starts)
		shiftID := uuid.New().String()
		totalBins := len(req.BinIDs)

		shiftQuery := `INSERT INTO shifts (id, driver_id, route_id, status, total_bins, created_at, updated_at)
					   VALUES ($1, $2, $3, 'ready', $4, $5, $6)`

		_, err = tx.Exec(shiftQuery, shiftID, req.DriverID, req.RouteID, totalBins, now, now)
		if err != nil {
			log.Printf("❌ Error creating shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create shift")
			return
		}

		// Insert bins - preserve route sequence if from pre-defined route, otherwise mark as unoptimized
		// Check if this is from a pre-defined route (has bins in route_bins table)
		var routeBins []struct {
			BinID         string `db:"bin_id"`
			SequenceOrder int    `db:"sequence_order"`
		}

		if req.RouteID != "" && req.RouteID != "custom" {
			// Try to get pre-defined route bins with sequence
			routeBinsQuery := `SELECT bin_id, sequence_order FROM route_bins
							   WHERE route_id = $1
							   ORDER BY sequence_order`
			err = tx.Select(&routeBins, routeBinsQuery, req.RouteID)
			if err != nil && err != sql.ErrNoRows {
				log.Printf("❌ Error fetching route_bins: %v", err)
				// Continue anyway - will treat as custom
				routeBins = nil
			}
		}

		// DEPRECATED: This endpoint no longer creates tasks.
		// Use POST /api/manager/shifts/with-tasks (CreateShiftWithTasks) instead.
		// route_tasks table has been removed in favor of route_tasks.
		log.Printf("⚠️  DEPRECATED: AssignRoute endpoint called. This endpoint is legacy and does not create tasks.")
		log.Printf("⚠️  Please update clients to use POST /api/manager/shifts/with-tasks instead.")

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to assign route")
			return
		}

		// Get created shift
		var shift models.Shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)

		// Get route bins with details
		bins, err := getShiftTasksWithDetails(db, shiftID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch route bins")
			return
		}

		// Send push notification (preference-aware)
		raTitle, raBody := services.ShiftNotificationText("route_assigned", nil)
		_, raNotifIDs := services.CreateNotificationForUsers(db, []string{req.DriverID}, "route_assigned", raTitle, raBody, map[string]string{"route_id": req.RouteID})
		notificationSent := false

		if len(raNotifIDs) > 0 {
			var fcmToken models.FCMToken
			tokenErr := db.Get(&fcmToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, req.DriverID)
			if tokenErr == nil {
				err := fcmService.SendRouteAssignedNotification(fcmToken.Token, req.RouteID, totalBins)
				if err != nil {
					log.Printf("⚠️  Failed to send FCM notification: %v", err)
				} else {
					notificationSent = true
				}
			}
		}

		// Broadcast WebSocket update to driver with FULL shift data
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📡 ATTEMPTING WEBSOCKET BROADCAST")
		log.Printf("   Target driver_id: %s", req.DriverID)
		log.Printf("   Is driver connected: %v", hub.IsUserConnected(req.DriverID))
		log.Printf("   Total connected clients: %d", hub.GetClientCount())
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		routeAssignedData := map[string]interface{}{
			"id":                  shift.ID,
			"driver_id":           shift.DriverID,
			"route_id":            shift.RouteID,
			"status":              shift.Status,
			"start_time":          shift.StartTime,
			"end_time":            shift.EndTime,
			"total_pause_seconds": shift.TotalPauseSeconds,
			"pause_start_time":    shift.PauseStartTime,
			"total_bins":          shift.TotalBins,
			"completed_bins":      shift.CompletedBins,
			"tasks":               bins,
			"created_at":          shift.CreatedAt,
			"updated_at":          shift.UpdatedAt,
			"message":             "New route assigned!",
		}
		hub.BroadcastToUser(req.DriverID, map[string]interface{}{
			"type": "route_assigned",
			"data": routeAssignedData,
		})

		// Publish route_assigned via Centrifugo shift channel
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "route_assigned",
				"data": routeAssignedData,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish route_assigned to Centrifugo: %v", pubErr)
			}
		}

		// Broadcast shift state change to all managers (new driver assigned)
		broadcastPayload := map[string]interface{}{
			"type": "driver_shift_change",
			"data": map[string]interface{}{
				"driver_id": req.DriverID,
				"status":    shift.Status,
				"shift_id":  shiftID,
			},
		}
		hub.BroadcastToRole("admin", broadcastPayload)
		hub.BroadcastToRole("manager", broadcastPayload)
		log.Printf("📡 Broadcast driver_shift_change to managers: Route assigned to driver")

		// Also publish via Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
				"driver_id": req.DriverID,
				"status":    shift.Status,
				"shift_id":  shiftID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
			}
		}

		log.Printf("✅ Route assigned: %s to driver %s (%d bins)", req.RouteID, req.DriverID, totalBins)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"shift_id":          shiftID,
				"driver_id":         req.DriverID,
				"route_id":          req.RouteID,
				"status":            shift.Status,
				"total_bins":        totalBins,
				"tasks":             bins, // ← Changed from "bins" to "tasks" for mobile app compatibility
				"notification_sent": notificationSent,
			},
		})
	}
}

// RegisterFCMToken registers a Firebase Cloud Messaging token
