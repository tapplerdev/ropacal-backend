package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// haversineDistanceKm calculates the distance between two GPS coordinates in kilometers
func haversineDistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371.0 // Earth's radius in kilometers

	// Convert to radians
	lat1Rad := lat1 * math.Pi / 180
	lat2Rad := lat2 * math.Pi / 180
	deltaLat := (lat2 - lat1) * math.Pi / 180
	deltaLon := (lon2 - lon1) * math.Pi / 180

	// Haversine formula
	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLon/2)*math.Sin(deltaLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}

// GetCurrentShift returns the current active shift for the driver
func GetCurrentShift(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/driver/shift/current")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Check what shifts exist for this driver (for debugging)
		var allShifts []models.Shift
		debugQuery := `SELECT id, status, created_at FROM shifts WHERE driver_id = $1 ORDER BY created_at DESC LIMIT 3`
		db.Select(&allShifts, debugQuery, userClaims.UserID)
		log.Printf("   🔍 DEBUG: Found %d total shifts for this driver:", len(allShifts))
		for i, s := range allShifts {
			log.Printf("      %d. Shift ID: %s, Status: %s, Created: %v", i+1, s.ID, s.Status, s.CreatedAt)
		}

		var shift models.Shift
		query := `SELECT * FROM shifts
				  WHERE driver_id = $1
				  AND status IN ('active', 'paused', 'ready')
				  ORDER BY
			    CASE status
			      WHEN 'active' THEN 1
			      WHEN 'paused' THEN 2
			      WHEN 'ready' THEN 3
			    END ASC,
			    created_at DESC
				  LIMIT 1`

		err := db.Get(&shift, query, userClaims.UserID)
		if err == sql.ErrNoRows {
			log.Printf("📤 RESPONSE: 200 - No active shift found")
			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"data":    nil,
			})
			return
		}
		if err != nil {
			log.Printf("❌ Error getting current shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Database error")
			return
		}

		// Get route bins with details
		bins, err := getRouteBinsWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch route bins")
			return
		}

		log.Printf("📤 RESPONSE: 200 OK")
		log.Printf("   Shift ID: %s", shift.ID)
		log.Printf("   Status: %s", shift.Status)
		log.Printf("   Route: %v", shift.RouteID)
		log.Printf("   Bins: %d/%d (%d bin details)", shift.CompletedBins, shift.TotalBins, len(bins))

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
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
				"bins":                bins,
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
			},
		})
	}
}

// StartShift starts an assigned shift
func StartShift(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/driver/shift/start")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Check if driver has any existing active or paused shift
		var existingShift models.Shift
		existingQuery := `SELECT * FROM shifts
					  WHERE driver_id = $1
					  AND (status = 'active' OR status = 'paused')
					  LIMIT 1`

		existingErr := db.Get(&existingShift, existingQuery, userClaims.UserID)
		if existingErr == nil {
			// Found an existing active/paused shift - auto-end it
			log.Printf("⚠️  Found existing %s shift (%s), auto-ending it before starting new shift", existingShift.Status, existingShift.ID)

			endNow := time.Now().Unix()
			totalPause := int64(existingShift.TotalPauseSeconds)
			if existingShift.PauseStartTime != nil {
				totalPause += endNow - *existingShift.PauseStartTime
			}

			// Calculate completion rate for history
			completionRate := 0.0
			if existingShift.TotalBins > 0 {
				completionRate = (float64(existingShift.CompletedBins) / float64(existingShift.TotalBins)) * 100
			}

			// Determine end reason - auto-ended because driver started new shift
			endReason := "manual_end"
			if existingShift.CompletedBins >= existingShift.TotalBins {
				endReason = "completed"
			}

			// Insert into shift_history
			historyQuery := `INSERT INTO shift_history (
			id, driver_id, route_id, start_time, end_time, created_at, ended_at,
			total_pause_seconds, total_bins, completed_bins, completion_rate,
			end_reason, ended_by_user_id, end_reason_metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

			_, histErr := db.Exec(
				historyQuery,
				existingShift.ID,
				existingShift.DriverID,
				existingShift.RouteID,
				existingShift.StartTime,
				endNow,
				existingShift.CreatedAt,
				endNow,
				totalPause,
				existingShift.TotalBins,
				existingShift.CompletedBins,
				completionRate,
				endReason,
				nil, // Driver action
				nil, // No metadata
			)
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

		// If shift needs optimization (sequence_order = 0), do it now using driver's current location
		// Check if any bin has sequence_order = 0 (unoptimized)
		var needsFullOptimization bool
		err = db.Get(&needsFullOptimization,
			`SELECT EXISTS(SELECT 1 FROM shift_bins WHERE shift_id = $1 AND sequence_order = 0)`,
			shift.ID,
		)
		if err != nil {
			log.Printf("❌ Error checking if shift needs optimization: %v", err)
			needsFullOptimization = false // Default to false on error
		}

		// Get driver's CURRENT location (needed for both optimization types)
		var driverLocation struct {
			Latitude  float64 `db:"latitude"`
			Longitude float64 `db:"longitude"`
		}

		locationErr := db.Get(&driverLocation,
			`SELECT latitude, longitude FROM driver_current_location
				 WHERE driver_id = $1 AND is_connected = true`,
			userClaims.UserID,
		)

		if locationErr != nil {
			log.Printf("❌ Driver location not available: %v", locationErr)
			utils.RespondError(w, http.StatusBadRequest, "Please enable GPS to start shift")
			return
		}

		log.Printf("✅ Got driver location: (%.6f, %.6f)", driverLocation.Latitude, driverLocation.Longitude)

		if needsFullOptimization {
			// Case 1: Custom bin selection - Full TSP optimization from driver's location
			log.Printf("🔄 Custom route - performing full TSP optimization from driver location")

			// Fetch all bins assigned to this shift
			var binDetails []struct {
				ID             string  `db:"id"`
				CurrentStreet  string  `db:"current_street"`
				Latitude       float64 `db:"latitude"`
				Longitude      float64 `db:"longitude"`
				FillPercentage int     `db:"fill_percentage"`
			}

			binQuery := `
				SELECT b.id, b.current_street, b.latitude, b.longitude, COALESCE(b.fill_percentage, 0) as fill_percentage
				FROM bins b
				JOIN shift_bins sb ON b.id = sb.bin_id
				WHERE sb.shift_id = $1
			`
			err = db.Select(&binDetails, binQuery, shift.ID)
			if err != nil {
				log.Printf("❌ Error fetching bins: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch bins")
				return
			}

			log.Printf("📦 Fetched %d bins for optimization", len(binDetails))

			// Convert to optimizer format
			binsToOptimize := make([]services.BinWithPriority, len(binDetails))
			for i, bin := range binDetails {
				binsToOptimize[i] = services.BinWithPriority{
					ID:             bin.ID,
					Latitude:       bin.Latitude,
					Longitude:      bin.Longitude,
					FillPercentage: bin.FillPercentage,
					CurrentStreet:  bin.CurrentStreet,
				}
			}

			// Optimize route using TSP algorithm from driver's current location
			optimizer := services.NewRouteOptimizer()
			startLocation := services.Location{
				Latitude:  driverLocation.Latitude,
				Longitude: driverLocation.Longitude,
			}
			optimizedBins := optimizer.OptimizeRoute(binsToOptimize, startLocation)

			log.Printf("🎯 Route optimized! Order: %v", func() []string {
				streets := make([]string, len(optimizedBins))
				for i, b := range optimizedBins {
					streets[i] = b.CurrentStreet
				}
				return streets
			}())

			// Update shift_bins with optimized sequence_order
			for i, bin := range optimizedBins {
				updateQuery := `UPDATE shift_bins
								SET sequence_order = $1
								WHERE shift_id = $2 AND bin_id = $3`

				_, err = db.Exec(updateQuery, i+1, shift.ID, bin.ID)
				if err != nil {
					log.Printf("❌ Error updating bin sequence: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to optimize route")
					return
				}
			}

			log.Printf("✅ Full TSP optimization complete with %d bins", len(optimizedBins))
		} else {
			// Case 2: Pre-defined route - Rotate sequence to start from closest bin
			log.Printf("🔄 Pre-defined route - rotating sequence to start from closest bin")

			// Fetch bins with their current sequence order
			var binDetails []struct {
				ID            string  `db:"id"`
				CurrentStreet string  `db:"current_street"`
				Latitude      float64 `db:"latitude"`
				Longitude     float64 `db:"longitude"`
				SequenceOrder int     `db:"sequence_order"`
			}

			binQuery := `
			SELECT b.id, b.current_street, b.latitude, b.longitude, sb.sequence_order
			FROM bins b
			JOIN shift_bins sb ON b.id = sb.bin_id
			WHERE sb.shift_id = $1
			ORDER BY sb.sequence_order
		`
			err = db.Select(&binDetails, binQuery, shift.ID)
			if err != nil {
				log.Printf("❌ Error fetching bins: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch bins")
				return
			}

			log.Printf("📦 Fetched %d bins from pre-defined route", len(binDetails))

			// Find closest bin to driver's current location using Haversine distance
			closestIdx := 0
			minDistance := math.MaxFloat64

			for i, bin := range binDetails {
				distance := haversineDistance(
					driverLocation.Latitude, driverLocation.Longitude,
					bin.Latitude, bin.Longitude,
				)
				if distance < minDistance {
					minDistance = distance
					closestIdx = i
				}
			}

			log.Printf("🎯 Closest bin to driver: %s (%.2f km away)", binDetails[closestIdx].CurrentStreet, minDistance)

			// Rotate the sequence to start from closest bin
			// Example: [A, B, C, D, E] with closest = C becomes [C, D, E, A, B]
			rotatedBins := make([]struct {
				ID            string  `db:"id"`
				CurrentStreet string  `db:"current_street"`
				Latitude      float64 `db:"latitude"`
				Longitude     float64 `db:"longitude"`
				SequenceOrder int     `db:"sequence_order"`
			}, len(binDetails))

			for i := 0; i < len(binDetails); i++ {
				srcIdx := (closestIdx + i) % len(binDetails)
				rotatedBins[i] = binDetails[srcIdx]
			}

			log.Printf("🔄 Rotated order: %v", func() []string {
				streets := make([]string, len(rotatedBins))
				for i, b := range rotatedBins {
					streets[i] = b.CurrentStreet
				}
				return streets
			}())

			// Update shift_bins with rotated sequence_order
			for i, bin := range rotatedBins {
				updateQuery := `UPDATE shift_bins
							SET sequence_order = $1
							WHERE shift_id = $2 AND bin_id = $3`

				_, err = db.Exec(updateQuery, i+1, shift.ID, bin.ID)
				if err != nil {
					log.Printf("❌ Error updating bin sequence: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to rotate route")
					return
				}
			}

			log.Printf("✅ Route rotation complete with %d bins", len(rotatedBins))

		}

		// Update shift to active
		now := time.Now().Unix()
		updateQuery := `UPDATE shifts
						SET status = 'active',
							start_time = $1,
							updated_at = $2
						WHERE id = $3`

		_, err = db.Exec(updateQuery, now, now, shift.ID)
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
				hub.BroadcastToRole("admin", map[string]interface{}{
					"type": "move_request_status_updated",
					"data": map[string]interface{}{
						"shift_id":    shift.ID,
						"new_status":  "in_progress",
						"count":       rowsAffected,
						"updated_at":  now,
					},
				})
				hub.BroadcastToRole("manager", map[string]interface{}{
					"type": "move_request_status_updated",
					"data": map[string]interface{}{
						"shift_id":    shift.ID,
						"new_status":  "in_progress",
						"count":       rowsAffected,
						"updated_at":  now,
					},
				})
				log.Printf("📡 Broadcast move_request_status_updated to managers: %d move requests → in_progress", rowsAffected)
			}
		}

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Get route bins with details for WebSocket broadcast
		bins, err := getRouteBinsWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route bins for WebSocket: %v", err)
			bins = []models.ShiftBinWithDetails{} // Empty array on error
		}

		// Broadcast WebSocket update to driver (include bins!)
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": map[string]interface{}{
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
				"bins":                bins, // ← Include route bins!
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
			},
		})

		// Broadcast shift state change to all managers
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📡 BROADCASTING driver_shift_change TO MANAGERS")
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
		log.Printf("   ✅ BroadcastToRole('admin' + 'manager') called")
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		log.Printf("✅ Shift started: %s (Driver: %s)", shift.ID, userClaims.Email)
		log.Printf("📤 RESPONSE: 200 OK")
		log.Printf("   Shift ID: %s", shift.ID)
		log.Printf("   Status: %s", shift.Status)
		log.Printf("   Start Time: %v", shift.StartTime)
		log.Printf("   Route: %v", shift.RouteID)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    shift,
		})
	}
}

// PauseShift pauses an active shift
func PauseShift(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/driver/shift/pause")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		now := time.Now().Unix()
		query := `UPDATE shifts
				  SET status = 'paused',
					  pause_start_time = $1,
					  updated_at = $2
				  WHERE driver_id = $1
				  AND status = 'active'`

		result, err := db.Exec(query, now, now, userClaims.UserID)
		if err != nil {
			log.Printf("❌ Error pausing shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to pause shift")
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			utils.RespondError(w, http.StatusBadRequest, "No active shift to pause")
			return
		}

		// Get updated shift
		var shift models.Shift
		db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'paused'`, userClaims.UserID)

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
		})

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

		log.Printf("⏸️  Shift paused: %s", shift.ID)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"status":           shift.Status,
				"pause_start_time": shift.PauseStartTime,
			},
		})
	}
}

// ResumeShift resumes a paused shift
func ResumeShift(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Get current shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'paused'`, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No paused shift to resume")
			return
		}

		// Calculate pause duration
		pauseDuration := 0
		if shift.PauseStartTime != nil {
			pauseDuration = int(time.Now().Unix() - *shift.PauseStartTime)
		}
		totalPause := shift.TotalPauseSeconds + pauseDuration

		// Update shift
		now := time.Now().Unix()
		query := `UPDATE shifts
				  SET status = 'active',
					  total_pause_seconds = $1,
					  pause_start_time = NULL,
					  updated_at = $2
				  WHERE id = $3`

		_, err = db.Exec(query, totalPause, now, shift.ID)
		if err != nil {
			log.Printf("❌ Error resuming shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to resume shift")
			return
		}

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
		})

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

		log.Printf("▶️  Shift resumed: %s (total pause: %ds)", shift.ID, totalPause)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"status":              shift.Status,
				"total_pause_seconds": shift.TotalPauseSeconds,
			},
		})
	}
}

// EndShift ends the current shift
func EndShift(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
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
		historyQuery := `INSERT INTO shift_history (
			id, driver_id, route_id, start_time, end_time, created_at, ended_at,
			total_pause_seconds, total_bins, completed_bins, completion_rate,
			incidents_reported, field_observations,
			end_reason, ended_by_user_id, end_reason_metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`

		_, err = db.Exec(
			historyQuery,
			shift.ID,
			shift.DriverID,
			shift.RouteID,
			shift.StartTime,
			endTime, // end_time
			shift.CreatedAt,
			now,        // ended_at (when history record created)
			totalPause, // total_pause_seconds
			shift.TotalBins,
			shift.CompletedBins,
			completionRate,
			incidentStats.TotalIncidents,    // NEW: incidents_reported
			incidentStats.FieldObservations, // NEW: field_observations
			endReason,
			nil, // ended_by_user_id (NULL - driver action)
			nil, // end_reason_metadata (NULL for basic driver ends)
		)
		if err != nil {
			log.Printf("❌ Error inserting shift history: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save shift history")
			return
		}

		log.Printf("✅ Shift history saved: %s (reason: %s, completion: %.1f%%)", shift.ID, endReason, completionRate)

		// Update shift
		updateQuery := `UPDATE shifts
						SET status = 'ended',
							end_time = $1,
							total_pause_seconds = $2,
							pause_start_time = NULL,
							updated_at = $3
						WHERE id = $4`

		_, err = db.Exec(updateQuery, endTime, totalPause, now, shift.ID)
		if err != nil {
			log.Printf("❌ Error ending shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to end shift")
			return
		}

		// Update incomplete move requests back to pending and clear assignment
		updateMovesQuery := `UPDATE bin_move_requests
							 SET status = 'pending',
							     assigned_shift_id = NULL,
							     updated_at = $1
							 WHERE assigned_shift_id = $2
							 AND status = 'in_progress'`
		result, err := db.Exec(updateMovesQuery, now, shift.ID)
		if err != nil {
			log.Printf("⚠️ Error updating incomplete move requests: %v", err)
			// Don't fail the request - continue
		} else {
			rowsAffected, _ := result.RowsAffected()
			if rowsAffected > 0 {
				log.Printf("✅ Updated %d incomplete move request(s) back to pending", rowsAffected)
			}
		}

		// Remove incomplete move requests from shift_bins
		deleteShiftBinsQuery := `DELETE FROM shift_bins
								 WHERE shift_id = $1
								 AND is_completed = 0
								 AND bin_id IN (
									SELECT bin_id FROM bin_move_requests
									WHERE assigned_shift_id IS NULL
									AND status = 'pending'
								 )`
		_, err = db.Exec(deleteShiftBinsQuery, shift.ID)
		if err != nil {
			log.Printf("⚠️ Error removing incomplete move bins from shift: %v", err)
			// Don't fail the request - continue
		}

		// Get updated shift with bins for WebSocket broadcast
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
		})

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

// CompleteBin marks a bin as completed
func CompleteBin(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[DIAGNOSTIC] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("[DIAGNOSTIC] 📥 REQUEST: POST /api/driver/shift/complete-bin")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("[DIAGNOSTIC]    User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Parse request body
		var req struct {
			ShiftBinID            int     `json:"shift_bin_id"`                      // ID of shift_bins record (identifies specific waypoint)
			BinID                 string  `json:"bin_id"`                            // DEPRECATED: Use shift_bin_id instead
			UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty"` // Now optional
			PhotoUrl              *string `json:"photo_url,omitempty"`
			MoveRequestID         *string `json:"move_request_id,omitempty"` // Links check to move request

			// Incident reporting fields (all optional)
			HasIncident         bool    `json:"has_incident"`
			IncidentType        *string `json:"incident_type,omitempty"`
			IncidentPhotoUrl    *string `json:"incident_photo_url,omitempty"`
			IncidentDescription *string `json:"incident_description,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("[DIAGNOSTIC] ❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("[DIAGNOSTIC]    Shift Bin ID: %d (waypoint-specific ID)", req.ShiftBinID)
		log.Printf("[DIAGNOSTIC]    Bin ID: %s (deprecated)", req.BinID)
		if req.UpdatedFillPercentage != nil {
			log.Printf("[DIAGNOSTIC]    Updated Fill Percentage: %d%%", *req.UpdatedFillPercentage)
		} else {
			log.Printf("[DIAGNOSTIC]    Updated Fill Percentage: null (not assessed)")
		}
		if req.PhotoUrl != nil {
			log.Printf("[DIAGNOSTIC]    Photo URL: %s", *req.PhotoUrl)
		} else {
			log.Printf("[DIAGNOSTIC]    Photo URL: null (no photo)")
		}
		if req.HasIncident {
			log.Printf("[DIAGNOSTIC]    🚨 INCIDENT REPORTED: %s", *req.IncidentType)
		}

		// Validate: at least photo OR fill percentage required (unless incident is being reported)
		if !req.HasIncident && req.PhotoUrl == nil && req.UpdatedFillPercentage == nil {
			utils.RespondError(w, http.StatusBadRequest, "At least photo or fill percentage is required")
			return
		}

		// Validate fill percentage if provided
		if req.UpdatedFillPercentage != nil && (*req.UpdatedFillPercentage < 0 || *req.UpdatedFillPercentage > 100) {
			utils.RespondError(w, http.StatusBadRequest, "Fill percentage must be between 0 and 100")
			return
		}

		// Validate incident fields if incident is being reported
		if req.HasIncident {
			if req.IncidentType == nil {
				utils.RespondError(w, http.StatusBadRequest, "incident_type is required when reporting incident")
				return
			}
			// Validate incident type
			validTypes := map[string]bool{"vandalism": true, "landlord_complaint": true, "theft": true, "relocation_request": true, "missing": true, "damaged": true, "inaccessible": true}
			if !validTypes[*req.IncidentType] {
				utils.RespondError(w, http.StatusBadRequest, "Invalid incident_type")
				return
			}
			// At least photo OR description required for incidents
			if req.IncidentPhotoUrl == nil && req.IncidentDescription == nil {
				utils.RespondError(w, http.StatusBadRequest, "Either incident photo or description is required")
				return
			}
		}

		// Get current active shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1`, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift")
			return
		}

		// Mark bin as completed in shift_bins table
		now := time.Now().Unix()

		var result sql.Result

		// Use shift_bin_id if provided (new elegant approach), otherwise fall back to bin_id (deprecated)
		if req.ShiftBinID > 0 {
			// NEW: Use shift_bin_id to mark specific waypoint (proper for move requests with multiple waypoints)
			log.Printf("[DIAGNOSTIC] 🆕 Using shift_bin_id=%d (correct approach for move requests)", req.ShiftBinID)
			routeBinQuery := `UPDATE shift_bins
							  SET is_completed = 1,
								  completed_at = $1,
								  updated_fill_percentage = $2
							  WHERE id = $3
							  AND shift_id = $4
							  AND is_completed = 0`
			result, err = db.Exec(routeBinQuery, now, req.UpdatedFillPercentage, req.ShiftBinID, shift.ID)
		} else {
			// DEPRECATED: Use bin_id (only works for regular bins, causes issues with move requests)
			log.Printf("[DIAGNOSTIC] ⚠️  Using bin_id=%s (deprecated - causes issues with move requests!)", req.BinID)
			routeBinQuery := `UPDATE shift_bins
							  SET is_completed = 1,
								  completed_at = $1,
								  updated_fill_percentage = $2
							  WHERE shift_id = $3
							  AND bin_id = $4
							  AND is_completed = 0`
			result, err = db.Exec(routeBinQuery, now, req.UpdatedFillPercentage, shift.ID, req.BinID)
		}
		if err != nil {
			log.Printf("❌ Error marking bin as completed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete bin")
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			log.Printf("[DIAGNOSTIC] ⚠️  Bin not found in route or already completed")
			utils.RespondError(w, http.StatusBadRequest, "Bin not found in route or already completed")
			return
		}

		log.Printf("[DIAGNOSTIC] ✅ Bin marked as completed in route_bins table")

		// Check if this bin is part of a move request
		var moveRequest models.BinMoveRequest
		moveErr := db.Get(&moveRequest, `
			SELECT * FROM bin_move_requests
			WHERE bin_id = $1
			AND assigned_shift_id = $2
			AND status IN ('assigned', 'in_progress')
		`, req.BinID, shift.ID)

		if moveErr == nil {
			// This is a MOVE REQUEST bin!
			log.Printf("[DIAGNOSTIC] 🚚 Detected move request: %s (type: %s)", moveRequest.ID, moveRequest.MoveType)
			// Get the stop_type of the current waypoint to determine if this is pickup or dropoff
			var stopType string
			stopTypeErr := db.Get(&stopType, `
			SELECT stop_type FROM shift_bins
			WHERE id = $1
		`, req.ShiftBinID)

			if stopTypeErr != nil {
				log.Printf("[DIAGNOSTIC] Error getting stop_type: %v", stopTypeErr)
				// Fallback: assume it's a dropoff if we can't determine
				stopType = "dropoff"
			}

			log.Printf("[DIAGNOSTIC] Stop type: %s", stopType)

			// Only finalize move request (update bin location, mark complete) when DROPOFF is completed
			// For pickup, we just mark the waypoint complete (already done above) and continue
			if stopType == "dropoff" {
				log.Printf("[DIAGNOSTIC] This is the DROPOFF - finalizing move request")
				err = handleMoveRequestCompletion(db, hub, moveRequest, req, now)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error handling move request: %v", err)
					// Don't fail - just log
				}
			} else {
				log.Printf("[DIAGNOSTIC] This is the PICKUP - move request remains in_progress")
			}
		} else {
			// Regular bin check - update fill percentage and last_checked_at
			if req.UpdatedFillPercentage != nil {
				log.Printf("[DIAGNOSTIC] 📝 Updating bin fill percentage and last_checked_at in bins table...")
				binUpdateQuery := `UPDATE bins
								   SET fill_percentage = $1,
								       last_checked_at = $2,
								       updated_at = $2
								   WHERE id = $3`

				_, err = db.Exec(binUpdateQuery, *req.UpdatedFillPercentage, now, req.BinID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error updating bin fill percentage: %v", err)
					// Don't fail the request - the bin is already marked complete in route
				} else {
					log.Printf("[DIAGNOSTIC] ✅ Bin fill percentage updated to %d%% and last_checked_at set to %d", *req.UpdatedFillPercentage, now)
				}
			} else {
				// Even without fill percentage, update last_checked_at
				log.Printf("[DIAGNOSTIC] 📝 Updating last_checked_at (no fill percentage due to incident)...")
				_, err = db.Exec(`UPDATE bins SET last_checked_at = $1, updated_at = $1 WHERE id = $2`, now, req.BinID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error updating last_checked_at: %v", err)
				} else {
					log.Printf("[DIAGNOSTIC] ✅ last_checked_at set to %d", now)
				}
			}
		}

		// Insert check record into checks table and get the ID back
		log.Printf("[DIAGNOSTIC] 📝 Inserting check record into checks table...")
		var checkID *int
		checkQuery := `INSERT INTO checks (bin_id, checked_from, fill_percentage, checked_on, checked_by, photo_url, move_request_id)
					   VALUES ($1, $2, $3, $4, $5, $6, $7)
					   RETURNING id`

		var returnedID int
		err = db.QueryRow(checkQuery, req.BinID, "shift", req.UpdatedFillPercentage, now, userClaims.UserID, req.PhotoUrl, req.MoveRequestID).Scan(&returnedID)
		if err != nil {
			log.Printf("[DIAGNOSTIC] ❌ Error inserting check record: %v", err)
			// Don't fail the request - the bin is already marked complete
			log.Printf("[DIAGNOSTIC] ⚠️  Continuing despite check insert error...")
			checkID = nil
		} else {
			checkID = &returnedID
			if req.PhotoUrl != nil {
				log.Printf("[DIAGNOSTIC] ✅ Check record inserted with photo_url (ID: %d)", returnedID)
			} else {
				log.Printf("[DIAGNOSTIC] ✅ Check record inserted without photo (ID: %d)", returnedID)
			}

			// Auto-resolve any pending check recommendations for this bin
			autoResolveCheckRecommendation(db, req.BinID, userClaims.UserID, now)
		}

		// Create incident if reported
		var createdIncidentID *string
		if req.HasIncident && checkID != nil {
			log.Printf("[DIAGNOSTIC] 🚨 Creating incident report for %s...", *req.IncidentType)

			// Get bin details for zone creation
			var bin models.Bin
			err = db.Get(&bin, "SELECT * FROM bins WHERE id = $1", req.BinID)
			if err != nil {
				log.Printf("[DIAGNOSTIC] ❌ Error fetching bin details: %v", err)
			} else {
				log.Printf("[DIAGNOSTIC]    Bin found: %s, %s", bin.CurrentStreet, bin.City)
				log.Printf("[DIAGNOSTIC]    Latitude: %v, Longitude: %v", bin.Latitude, bin.Longitude)
			}

			if err == nil && bin.Latitude != nil && bin.Longitude != nil {
				// Call the zone incident creation logic
				incidentID := uuid.New().String()
				log.Printf("[DIAGNOSTIC]    Incident ID: %s", incidentID)

				// Check for existing zone within 100m
				var zoneID string
				var existingZone *models.NoGoZone
				var zones []models.NoGoZone
				err = db.Select(&zones, "SELECT * FROM no_go_zones WHERE status = 'active'")
				if err != nil {
					log.Printf("[DIAGNOSTIC] ⚠️  Error fetching zones: %v", err)
				} else {
					log.Printf("[DIAGNOSTIC]    Checking %d active zones for proximity...", len(zones))
					for _, zone := range zones {
						distance := calculateZoneDistance(*bin.Latitude, *bin.Longitude, zone.CenterLatitude, zone.CenterLongitude)
						if distance < 100 {
							existingZone = &zone
							log.Printf("[DIAGNOSTIC]    Found existing zone within 100m (distance: %.2fm)", distance)
							break
						}
					}
				}

				// Create or update zone
				if existingZone != nil {
					zoneID = existingZone.ID
					newScore := existingZone.ConflictScore + getIncidentScore(*req.IncidentType)
					_, err = db.Exec(`UPDATE no_go_zones SET conflict_score = $1, updated_at = $2 WHERE id = $3`, newScore, now, zoneID)
					if err != nil {
						log.Printf("[DIAGNOSTIC] ❌ Error updating zone: %v", err)
					} else {
						log.Printf("[DIAGNOSTIC] ✅ Updated existing zone (new score: %d)", newScore)
					}
				} else {
					zoneID = uuid.New().String()
					zoneName := fmt.Sprintf("%s - %s", bin.CurrentStreet, bin.City)
					radiusMeters := getZoneRadius(*req.IncidentType)
					log.Printf("[DIAGNOSTIC]    Creating new zone: %s (radius: %dm)", zoneName, radiusMeters)
					_, err = db.Exec(`
						INSERT INTO no_go_zones (id, name, center_latitude, center_longitude, radius_meters, conflict_score, status, created_by_user_id, created_at, updated_at)
						VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
					`, zoneID, zoneName, *bin.Latitude, *bin.Longitude, radiusMeters, getIncidentScore(*req.IncidentType), "active", nil, now, now)
					if err != nil {
						log.Printf("[DIAGNOSTIC] ❌ Error creating zone: %v", err)
					} else {
						log.Printf("[DIAGNOSTIC] ✅ Created new no-go zone (ID: %s)", zoneID)
					}
				}

				// Check for zone merges after creating/updating zone
				if err == nil {
					log.Printf("[DIAGNOSTIC] 🔍 Checking for zone merges...")
					if mergeErr := detectAndMergeZones(db, zoneID, now); mergeErr != nil {
						log.Printf("[DIAGNOSTIC] ⚠️  Zone merge check failed: %v", mergeErr)
						// Don't fail the request if merge fails - it's not critical
					}
				}

				// Create incident record
				log.Printf("[DIAGNOSTIC]    Inserting incident record...")
				_, err = db.Exec(`
					INSERT INTO zone_incidents (id, zone_id, bin_id, incident_type, reported_by_user_id, reported_at, description, photo_url, check_id, shift_id, is_field_observation, status)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
				`, incidentID, zoneID, req.BinID, *req.IncidentType, userClaims.UserID, now, req.IncidentDescription, req.IncidentPhotoUrl, checkID, shift.ID, false, "open")

				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ ERROR inserting incident: %v", err)
				} else {
					createdIncidentID = &incidentID
					log.Printf("[DIAGNOSTIC] ✅ Incident created (ID: %s) and linked to check ID %d", incidentID, *checkID)
				}
			} else if err != nil {
				log.Printf("[DIAGNOSTIC] ⚠️  Could not create incident: failed to fetch bin")
			} else {
				log.Printf("[DIAGNOSTIC] ⚠️  Could not create incident: bin has no coordinates (lat: %v, lng: %v)", bin.Latitude, bin.Longitude)
			}
		}

		// Update shift completed_bins count
		shiftQuery := `UPDATE shifts
					   SET completed_bins = completed_bins + 1,
						   updated_at = $1
					   WHERE id = $2`

		_, err = db.Exec(shiftQuery, now, shift.ID)
		if err != nil {
			log.Printf("❌ Error updating shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift")
			return
		}

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Get updated bins list
		bins, err := getRouteBinsWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			bins = []models.ShiftBinWithDetails{}
		}

		// Calculate LOGICAL bin counts (treating pickup+dropoff as 1)
		logicalTotal, logicalCompleted := calculateLogicalBinCounts(bins)
		log.Printf("[DIAGNOSTIC] 🔢 Logical counts: %d completed / %d total (Physical: %d/%d)",
			logicalCompleted, logicalTotal, shift.CompletedBins, shift.TotalBins)

		// Broadcast WebSocket update with bins
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": map[string]interface{}{
				"id":             shift.ID,
				"status":         shift.Status,
				"completed_bins": logicalCompleted,
				"total_bins":     logicalTotal,
				"bins":           bins,
			},
		})

		log.Printf("[DIAGNOSTIC] ✅ Bin completed: %d/%d (logical)", logicalCompleted, logicalTotal)

		completionPercentage := 0.0
		if logicalTotal > 0 {
			completionPercentage = float64(logicalCompleted) / float64(logicalTotal) * 100
		}

		response := models.CompleteBinResponse{
			CompletedBins:        logicalCompleted,
			TotalBins:            logicalTotal,
			CompletionPercentage: completionPercentage,
			CheckID:              checkID,
			IncidentID:           createdIncidentID,
		}

		log.Printf("[DIAGNOSTIC] 📤 RESPONSE: 200 OK")
		log.Printf("[DIAGNOSTIC]    Completed: %d/%d (%.1f%%) [LOGICAL COUNTS]", logicalCompleted, logicalTotal, completionPercentage)
		log.Printf("[DIAGNOSTIC]    Photo uploaded: %v", req.PhotoUrl != nil)
		if checkID != nil {
			log.Printf("[DIAGNOSTIC]    Check ID: %d (available for incident linking)", *checkID)
		}
		if createdIncidentID != nil {
			log.Printf("[DIAGNOSTIC]    Incident ID: %s (incident successfully created)", *createdIncidentID)
		}
		log.Printf("[DIAGNOSTIC] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}
}

// GetDriverShiftHistory returns all completed shifts for the authenticated driver
func GetDriverShiftHistory(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/driver/shift-history")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Query all shifts where start_time is NOT NULL (shift was actually started)
		// Order by most recent first, limit to 100 for performance
		query := `
			SELECT id, driver_id, route_id, status, start_time, end_time,
			       total_pause_seconds, total_bins, completed_bins,
			       created_at, updated_at
			FROM shifts
			WHERE driver_id = $1 AND start_time IS NOT NULL
			ORDER BY start_time DESC
			LIMIT 100`

		var shifts []models.Shift
		err := db.Select(&shifts, query, userClaims.UserID)
		if err != nil {
			log.Printf("❌ Error fetching shift history: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift history")
			return
		}

		log.Printf("✅ Found %d shifts in history", len(shifts))
		log.Printf("📤 RESPONSE: 200 OK")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    shifts,
		})
	}
}

// GetShiftDetails returns detailed information about a specific shift including all bins
func GetShiftDetails(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/driver/shift-details")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := r.URL.Query().Get("shift_id")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "shift_id query parameter is required")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)
		log.Printf("   Shift ID: %s", shiftID)

		// Get shift details
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1 AND driver_id = $2`, shiftID, userClaims.UserID)
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}

		// Get all bins with details for this shift
		bins, err := getRouteBinsWithDetails(db, shiftID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			bins = []models.ShiftBinWithDetails{} // Return empty array on error
		}

		log.Printf("✅ Shift found with %d bins", len(bins))
		log.Printf("📤 RESPONSE: 200 OK")

		// Return shift with bins array
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"id":                  shift.ID,
				"driver_id":           shift.DriverID,
				"route_id":            shift.RouteID,
				"status":              shift.Status,
				"start_time":          shift.StartTime,
				"end_time":            shift.EndTime,
				"total_pause_seconds": shift.TotalPauseSeconds,
				"total_bins":          shift.TotalBins,
				"completed_bins":      shift.CompletedBins,
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
				"bins":                bins,
			},
		})
	}
}

// GetShiftMoveRequests returns all move requests assigned to a shift
func GetShiftMoveRequests(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/driver/shift-move-requests")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := r.URL.Query().Get("shift_id")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "shift_id query parameter is required")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)
		log.Printf("   Shift ID: %s", shiftID)

		// Verify shift exists and belongs to the driver
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1 AND driver_id = $2`, shiftID, userClaims.UserID)
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}

		// Query move requests for this shift with bin details
		query := `
			SELECT
				mr.id,
				mr.bin_id,
				mr.scheduled_date,
				mr.urgency,
				mr.requested_by,
				mr.status,
				mr.original_latitude,
				mr.original_longitude,
				mr.original_address,
				mr.new_latitude,
				mr.new_longitude,
				mr.new_address,
				mr.move_type,
				mr.disposal_action,
				mr.reason,
				mr.notes,
				mr.assignment_type,
				mr.assigned_shift_id,
				mr.assigned_user_id,
				mr.completed_at,
				mr.created_at,
				mr.updated_at,
				b.bin_number,
				b.current_street,
				b.city,
				b.zip
			FROM bin_move_requests mr
			JOIN bins b ON mr.bin_id = b.id
			WHERE mr.assigned_shift_id = $1
			ORDER BY mr.scheduled_date ASC`

		type MoveRequestWithBinDetails struct {
			models.BinMoveRequest
			BinNumber     int    `db:"bin_number"`
			CurrentStreet string `db:"current_street"`
			City          string `db:"city"`
			Zip           string `db:"zip"`
		}

		var moveRequests []MoveRequestWithBinDetails
		err = db.Select(&moveRequests, query, shiftID)
		if err != nil {
			log.Printf("❌ Error fetching move requests: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch move requests")
			return
		}

		log.Printf("✅ Found %d move requests for shift", len(moveRequests))
		log.Printf("📤 RESPONSE: 200 OK")

		// Convert to response format
		responses := make([]models.BinMoveRequestResponse, 0, len(moveRequests))
		for _, mr := range moveRequests {
			resp := mr.BinMoveRequest.ToBinMoveRequestResponse()
			resp.BinNumber = mr.BinNumber
			resp.CurrentStreet = mr.CurrentStreet
			resp.City = mr.City
			resp.Zip = mr.Zip
			responses = append(responses, resp)
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    responses,
		})
	}
}

// calculateLogicalBinCounts groups move request pickup+dropoff as one logical bin
// Returns (logicalTotal, logicalCompleted)
func calculateLogicalBinCounts(bins []models.ShiftBinWithDetails) (int, int) {
	processedMoveRequests := make(map[string]bool)
	logicalTotal := 0
	logicalCompleted := 0

	for _, bin := range bins {
		// If it's part of a move request
		if bin.MoveRequestID != nil && *bin.MoveRequestID != "" {
			moveReqID := *bin.MoveRequestID

			// Only count once per move request (not per waypoint)
			if !processedMoveRequests[moveReqID] {
				logicalTotal++
				processedMoveRequests[moveReqID] = true

				// Check if BOTH waypoints are completed
				pickupCompleted := false
				dropoffCompleted := false

				for _, b := range bins {
					if b.MoveRequestID != nil && *b.MoveRequestID == moveReqID {
						if b.StopType == "pickup" && b.IsCompleted == 1 {
							pickupCompleted = true
						}
						if b.StopType == "dropoff" && b.IsCompleted == 1 {
							dropoffCompleted = true
						}
					}
				}

				if pickupCompleted && dropoffCompleted {
					logicalCompleted++
				}
			}
		} else {
			// Regular collection bin
			logicalTotal++
			if bin.IsCompleted == 1 {
				logicalCompleted++
			}
		}
	}

	return logicalTotal, logicalCompleted
}

// getRouteBinsWithDetails fetches route bins with full bin details
func getRouteBinsWithDetails(db *sqlx.DB, shiftID string) ([]models.ShiftBinWithDetails, error) {
	query := `
		SELECT
			rb.id,
			rb.shift_id,
			rb.bin_id,
			rb.sequence_order,
			rb.is_completed,
			rb.completed_at,
			rb.updated_fill_percentage,
			rb.created_at,
			b.bin_number,
			CASE
				WHEN rb.stop_type = 'dropoff' AND mr.new_address IS NOT NULL THEN mr.new_address
				ELSE b.current_street
			END as current_street,
			b.city,
			b.zip,
			COALESCE(b.fill_percentage, 0) as fill_percentage,
			CASE
				WHEN rb.stop_type = 'dropoff' AND mr.new_latitude IS NOT NULL THEN mr.new_latitude
				ELSE b.latitude
			END as latitude,
			CASE
				WHEN rb.stop_type = 'dropoff' AND mr.new_longitude IS NOT NULL THEN mr.new_longitude
				ELSE b.longitude
			END as longitude,
			rb.stop_type,
			rb.move_request_id,
			mr.original_address,
			mr.new_address,
			mr.move_type
		FROM shift_bins rb
		JOIN bins b ON rb.bin_id = b.id
		LEFT JOIN bin_move_requests mr ON rb.move_request_id = mr.id
		WHERE rb.shift_id = $1
		ORDER BY rb.sequence_order ASC`

	var bins []models.ShiftBinWithDetails
	err := db.Select(&bins, query, shiftID)
	if err != nil {
		return nil, err
	}

	return bins, nil
}

// AssignRoute assigns a route to a driver (manager only)
func AssignRoute(db *sqlx.DB, hub *websocket.Hub, fcmService *services.FCMService) http.HandlerFunc {
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

		// If we found pre-defined route bins, use their sequence
		if len(routeBins) > 0 {
			log.Printf("✅ Using pre-defined route sequence with %d bins", len(routeBins))
			for _, rb := range routeBins {
				routeBinQuery := `INSERT INTO shift_bins (shift_id, bin_id, sequence_order, created_at)
								  VALUES ($1, $2, $3, $4)`

				_, err = tx.Exec(routeBinQuery, shiftID, rb.BinID, rb.SequenceOrder, now)
				if err != nil {
					log.Printf("❌ Error inserting shift_bin: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to assign bins to shift")
					return
				}
			}
		} else {
			// Custom selection or route without pre-defined bins - insert with sequence_order = 0
			log.Printf("ℹ️  Custom bin selection - will optimize from driver's start location")
			for _, binID := range req.BinIDs {
				routeBinQuery := `INSERT INTO shift_bins (shift_id, bin_id, sequence_order, created_at)
								  VALUES ($1, $2, 0, $3)`

				_, err = tx.Exec(routeBinQuery, shiftID, binID, now)
				if err != nil {
					log.Printf("❌ Error inserting shift_bin: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to assign bins to shift")
					return
				}
			}
		}

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
		bins, err := getRouteBinsWithDetails(db, shiftID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch route bins")
			return
		}

		// Send push notification
		var fcmToken models.FCMToken
		tokenErr := db.Get(&fcmToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, req.DriverID)
		notificationSent := false

		if tokenErr == nil {
			err := fcmService.SendRouteAssignedNotification(fcmToken.Token, req.RouteID, totalBins)
			if err != nil {
				log.Printf("⚠️  Failed to send FCM notification: %v", err)
			} else {
				notificationSent = true
			}
		}

		// Broadcast WebSocket update to driver with FULL shift data
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📡 ATTEMPTING WEBSOCKET BROADCAST")
		log.Printf("   Target driver_id: %s", req.DriverID)
		log.Printf("   Is driver connected: %v", hub.IsUserConnected(req.DriverID))
		log.Printf("   Total connected clients: %d", hub.GetClientCount())
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		hub.BroadcastToUser(req.DriverID, map[string]interface{}{
			"type": "route_assigned",
			"data": map[string]interface{}{
				"id":                  shift.ID,
				"driver_id":           shift.DriverID,
				"route_id":            shift.RouteID,
				"status":              shift.Status, // CRITICAL: Include status for ShiftState.fromJson()
				"start_time":          shift.StartTime,
				"end_time":            shift.EndTime,
				"total_pause_seconds": shift.TotalPauseSeconds,
				"pause_start_time":    shift.PauseStartTime,
				"total_bins":          shift.TotalBins,
				"completed_bins":      shift.CompletedBins,
				"bins":                bins,
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
				"message":             "New route assigned!",
			},
		})

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

		log.Printf("✅ Route assigned: %s to driver %s (%d bins)", req.RouteID, req.DriverID, totalBins)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"shift_id":          shiftID,
				"driver_id":         req.DriverID,
				"route_id":          req.RouteID,
				"status":            shift.Status,
				"total_bins":        totalBins,
				"bins":              bins,
				"notification_sent": notificationSent,
			},
		})
	}
}

// RegisterFCMToken registers a Firebase Cloud Messaging token
func RegisterFCMToken(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Parse request body
		var req struct {
			Token      string `json:"token"`
			DeviceType string `json:"device_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate device type
		if req.DeviceType != "ios" && req.DeviceType != "android" {
			utils.RespondError(w, http.StatusBadRequest, "Invalid device_type (must be 'ios' or 'android')")
			return
		}

		// Insert or update token
		now := time.Now().Unix()
		query := `INSERT INTO fcm_tokens (user_id, token, device_type, created_at, updated_at)
				  VALUES ($1, $2, $3, $4, $5)
				  ON CONFLICT(token) DO UPDATE SET
					  user_id = excluded.user_id,
					  device_type = excluded.device_type,
					  updated_at = excluded.updated_at`

		_, err := db.Exec(query, userClaims.UserID, req.Token, req.DeviceType, now, now)
		if err != nil {
			log.Printf("❌ Error registering FCM token: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to register FCM token")
			return
		}

		log.Printf("📱 FCM token registered: %s (%s)", userClaims.Email, req.DeviceType)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "FCM token registered successfully",
		})
	}
}

// ClearAllShifts deletes all shifts from the database (for testing purposes)
func ClearAllShifts(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("🗑️  REQUEST: DELETE /api/admin/shifts/clear")

		// Get all affected driver IDs before deleting
		var affectedDrivers []string
		query := `SELECT DISTINCT driver_id FROM shifts WHERE status != 'inactive'`
		err := db.Select(&affectedDrivers, query)
		if err != nil && err != sql.ErrNoRows {
			log.Printf("⚠️  Error getting affected drivers: %v", err)
			affectedDrivers = []string{} // Continue even if this fails
		}

		log.Printf("📊 Found %d drivers with active/ready shifts", len(affectedDrivers))

		// Execute delete query
		result, err := db.Exec("DELETE FROM shifts")
		if err != nil {
			log.Printf("❌ Error clearing shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to clear shifts")
			return
		}

		rowsAffected, _ := result.RowsAffected()
		log.Printf("✅ Cleared %d shifts from database", rowsAffected)

		// Broadcast shift_deleted event to all affected drivers
		for _, driverID := range affectedDrivers {
			hub.BroadcastToUser(driverID, map[string]interface{}{
				"type": "shift_deleted",
				"data": map[string]interface{}{
					"shift_id": "all",
					"message":  "All shifts have been cleared by manager",
					"reason":   "manager_clear_all",
				},
			})
			log.Printf("📤 Sent shift_deleted event to driver: %s", driverID)

			// Also broadcast to managers that this driver's shift ended
			broadcastPayload := map[string]interface{}{
				"type": "driver_shift_change",
				"data": map[string]interface{}{
					"driver_id": driverID,
					"status":    "ended",
					"shift_id":  "all",
				},
			}
			hub.BroadcastToRole("admin", broadcastPayload)
			hub.BroadcastToRole("manager", broadcastPayload)
		}

		log.Printf("✅ WebSocket events sent to %d drivers", len(affectedDrivers))
		log.Printf("📡 Broadcast driver_shift_change to managers for %d drivers (shifts cleared)", len(affectedDrivers))

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":       true,
			"message":       "All shifts cleared successfully",
			"rows_affected": rowsAffected,
		})
	}
}

// UpdateLocation handles driver location updates (POST /api/driver/location)
// Called every 10 seconds when driver is on active shift
func UpdateLocation(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var req struct {
			Latitude  float64  `json:"latitude"`
			Longitude float64  `json:"longitude"`
			Heading   *float64 `json:"heading"`
			Speed     *float64 `json:"speed"`
			Accuracy  *float64 `json:"accuracy"`
			ShiftID   *string  `json:"shift_id"`
			Timestamp int64    `json:"timestamp"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Validate required fields
		if req.Latitude == 0 && req.Longitude == 0 {
			utils.RespondError(w, http.StatusBadRequest, "Invalid coordinates")
			return
		}

		// Insert location into database
		query := `
			INSERT INTO driver_locations (
				driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, created_at
		`

		var locationID int
		var createdAt int64

		err := db.QueryRow(
			query,
			userClaims.UserID,
			req.Latitude,
			req.Longitude,
			req.Heading,
			req.Speed,
			req.Accuracy,
			req.ShiftID,
			req.Timestamp,
		).Scan(&locationID, &createdAt)

		if err != nil {
			log.Printf("❌ Error saving location: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save location")
			return
		}

		// Broadcast location update to all connected managers via WebSocket
		locationUpdate := map[string]interface{}{
			"type": "driver_location_update",
			"data": map[string]interface{}{
				"id":         locationID,
				"driver_id":  userClaims.UserID,
				"latitude":   req.Latitude,
				"longitude":  req.Longitude,
				"heading":    req.Heading,
				"speed":      req.Speed,
				"accuracy":   req.Accuracy,
				"shift_id":   req.ShiftID,
				"timestamp":  req.Timestamp,
				"created_at": createdAt,
			},
		}

		// Broadcast to all managers (users with role "admin")
		hub.BroadcastToRole("admin", locationUpdate)

		// Return success response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "Location updated successfully",
			"id":      locationID,
		})
	}
}

// GetAllDrivers returns all drivers regardless of shift status
// Drivers with active shifts will show their current shift info
// Drivers without active shifts will show status as 'inactive'
// GET /api/manager/drivers
func GetAllDrivers(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("📋 GetAllDrivers: Fetching all drivers...")

		query := `
			SELECT
				u.id AS driver_id,
				u.name AS driver_name,
				u.email,
				s.id AS shift_id,
				s.route_id,
				s.status AS shift_status,
				s.start_time,
				s.total_bins,
				s.completed_bins,
				s.updated_at,
				dl.latitude,
				dl.longitude
			FROM users u
			LEFT JOIN shifts s ON u.id = s.driver_id AND s.status IN ('ready', 'active', 'paused')
			LEFT JOIN (
				-- Get the most recent location for each driver
				SELECT DISTINCT ON (driver_id)
					driver_id, latitude, longitude
				FROM driver_locations
				ORDER BY driver_id, timestamp DESC
			) dl ON u.id = dl.driver_id
			WHERE u.role = 'driver'
			ORDER BY
				CASE
					WHEN s.status IS NOT NULL THEN 0  -- Active drivers first
					ELSE 1                             -- Idle drivers last
				END,
				s.updated_at DESC NULLS LAST,
				u.name ASC
		`

		rows, err := db.Query(query)
		if err != nil {
			log.Printf("❌ Database error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch drivers",
			})
			return
		}
		defer rows.Close()

		var allDrivers []AllDriverResponse

		for rows.Next() {
			var driver AllDriverResponse
			var shiftID, routeID, shiftStatus sql.NullString
			var startTime, updatedAt sql.NullInt64
			var totalBins, completedBins sql.NullInt32
			var latitude, longitude sql.NullFloat64

			err := rows.Scan(
				&driver.DriverID,
				&driver.DriverName,
				&driver.Email,
				&shiftID,
				&routeID,
				&shiftStatus,
				&startTime,
				&totalBins,
				&completedBins,
				&updatedAt,
				&latitude,
				&longitude,
			)
			if err != nil {
				log.Printf("❌ Row scan error: %v", err)
				continue
			}

			// Set shift-related fields if driver has an active shift
			if shiftID.Valid {
				driver.ShiftID = &shiftID.String
				driver.Status = shiftStatus.String

				if routeID.Valid {
					driver.RouteID = &routeID.String
				}
				if startTime.Valid {
					t := startTime.Int64
					driver.StartTime = &t
				}
				if totalBins.Valid {
					driver.TotalBins = int(totalBins.Int32)
				}
				if completedBins.Valid {
					driver.CompletedBins = int(completedBins.Int32)
				}
				if updatedAt.Valid {
					t := updatedAt.Int64
					driver.UpdatedAt = &t
				}
			} else {
				// No active shift - driver is inactive
				driver.Status = "inactive"
				driver.TotalBins = 0
				driver.CompletedBins = 0
			}

			// Add location if available
			if latitude.Valid && longitude.Valid {
				driver.CurrentLocation = &DriverLocation{
					Latitude:  latitude.Float64,
					Longitude: longitude.Float64,
				}
			}

			allDrivers = append(allDrivers, driver)
		}

		if err = rows.Err(); err != nil {
			log.Printf("❌ Rows iteration error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to process drivers",
			})
			return
		}

		log.Printf("✅ Found %d driver(s)", len(allDrivers))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    allDrivers,
		})
	}
}

// Helper Functions for Incident Reporting and No-Go Zones

// getZoneRadius returns the radius in meters based on incident type
func getZoneRadius(incidentType string) int {
	switch incidentType {
	case "theft", "vandalized":
		return 500 // High severity - 500m radius
	case "damaged", "missing":
		return 300 // Medium severity - 300m radius
	case "landlord_complaint":
		return 200 // Localized issue - 200m radius
	case "inaccessible", "relocation_request":
		return 150 // Very localized - 150m radius
	default:
		return 250 // Default radius
	}
}

// getIncidentScore returns the conflict score to add based on incident type
func getIncidentScore(incidentType string) int {
	switch incidentType {
	case "theft":
		return 20 // Most severe
	case "vandalized":
		return 15
	case "damaged":
		return 10
	case "landlord_complaint":
		return 8
	case "missing":
		return 12
	case "inaccessible":
		return 5
	case "relocation_request":
		return 3
	default:
		return 5
	}
}

// calculateZoneDistance calculates the distance in meters between a point and a zone center
func calculateZoneDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusMeters = 6371000 // Earth's radius in meters

	// Convert degrees to radians
	lat1Rad := lat1 * (3.141592653589793 / 180)
	lat2Rad := lat2 * (3.141592653589793 / 180)
	deltaLatRad := (lat2 - lat1) * (3.141592653589793 / 180)
	deltaLonRad := (lon2 - lon1) * (3.141592653589793 / 180)

	// Haversine formula
	a := math.Sin(deltaLatRad/2)*math.Sin(deltaLatRad/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*
			math.Sin(deltaLonRad/2)*math.Sin(deltaLonRad/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadiusMeters * c
}

// calculateZoneOverlap calculates the overlap percentage between two circular zones
// Returns the percentage of overlap (0-100) based on the smaller zone
func calculateZoneOverlap(lat1, lon1 float64, radius1 int, lat2, lon2 float64, radius2 int) float64 {
	distance := calculateZoneDistance(lat1, lon1, lat2, lon2)
	r1 := float64(radius1)
	r2 := float64(radius2)

	// If zones don't overlap at all
	if distance >= r1+r2 {
		return 0.0
	}

	// If one zone completely contains the other
	if distance+math.Min(r1, r2) <= math.Max(r1, r2) {
		return 100.0
	}

	// Calculate intersection area using circle-circle intersection formula
	d := distance
	part1 := r1 * r1 * math.Acos((d*d+r1*r1-r2*r2)/(2*d*r1))
	part2 := r2 * r2 * math.Acos((d*d+r2*r2-r1*r1)/(2*d*r2))
	part3 := 0.5 * math.Sqrt((r1+r2-d)*(r1-r2+d)*(-r1+r2+d)*(r1+r2+d))

	intersectionArea := part1 + part2 - part3

	// Calculate percentage based on smaller zone
	smallerZoneArea := math.Pi * math.Min(r1, r2) * math.Min(r1, r2)
	overlapPercent := (intersectionArea / smallerZoneArea) * 100

	return overlapPercent
}

// detectAndMergeZones checks if the given zone should be merged with any existing zones
// Merges zones if they overlap by more than 50%
func detectAndMergeZones(db *sqlx.DB, zoneID string, now int64) error {
	// Get the current zone details
	var currentZone models.NoGoZone
	err := db.Get(&currentZone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID)
	if err != nil {
		return fmt.Errorf("failed to fetch current zone: %w", err)
	}

	// Get all other active zones
	var otherZones []models.NoGoZone
	err = db.Select(&otherZones, "SELECT * FROM no_go_zones WHERE status = 'active' AND id != $1 AND merged_into_zone_id IS NULL", zoneID)
	if err != nil {
		return fmt.Errorf("failed to fetch other zones: %w", err)
	}

	log.Printf("[ZONE MERGE] Checking zone %s for potential merges with %d other zones", zoneID[:8], len(otherZones))

	for _, otherZone := range otherZones {
		// Calculate overlap percentage
		overlapPercent := calculateZoneOverlap(
			currentZone.CenterLatitude, currentZone.CenterLongitude, currentZone.RadiusMeters,
			otherZone.CenterLatitude, otherZone.CenterLongitude, otherZone.RadiusMeters,
		)

		log.Printf("[ZONE MERGE] Zone %s vs %s: %.1f%% overlap", currentZone.ID[:8], otherZone.ID[:8], overlapPercent)

		// If overlap is greater than 50%, merge the zones
		if overlapPercent > 50.0 {
			log.Printf("[ZONE MERGE] 🔄 Merging zones (%.1f%% overlap)", overlapPercent)

			// Determine which zone to keep (higher conflict score wins, or larger radius)
			var primaryZone, secondaryZone models.NoGoZone
			if currentZone.ConflictScore > otherZone.ConflictScore {
				primaryZone = currentZone
				secondaryZone = otherZone
			} else if currentZone.ConflictScore < otherZone.ConflictScore {
				primaryZone = otherZone
				secondaryZone = currentZone
			} else {
				// Equal scores, use larger radius
				if currentZone.RadiusMeters >= otherZone.RadiusMeters {
					primaryZone = currentZone
					secondaryZone = otherZone
				} else {
					primaryZone = otherZone
					secondaryZone = currentZone
				}
			}

			// Execute the merge
			err = executeMerge(db, primaryZone, secondaryZone, now)
			if err != nil {
				log.Printf("[ZONE MERGE] ❌ Failed to merge zones: %v", err)
				continue
			}

			log.Printf("[ZONE MERGE] ✅ Successfully merged zone %s into %s", secondaryZone.ID[:8], primaryZone.ID[:8])
		}
	}

	return nil
}

// executeMerge merges secondaryZone into primaryZone
func executeMerge(db *sqlx.DB, primaryZone, secondaryZone models.NoGoZone, now int64) error {
	log.Printf("[ZONE MERGE] Executing merge: %s <- %s", primaryZone.ID[:8], secondaryZone.ID[:8])

	// Calculate combined conflict score
	combinedScore := primaryZone.ConflictScore + secondaryZone.ConflictScore

	// Use the larger radius
	newRadius := primaryZone.RadiusMeters
	if secondaryZone.RadiusMeters > newRadius {
		newRadius = secondaryZone.RadiusMeters
	}

	// Update primary zone with combined score and larger radius
	_, err := db.Exec(`
		UPDATE no_go_zones
		SET conflict_score = $1, radius_meters = $2, updated_at = $3
		WHERE id = $4
	`, combinedScore, newRadius, now, primaryZone.ID)
	if err != nil {
		return fmt.Errorf("failed to update primary zone: %w", err)
	}
	log.Printf("[ZONE MERGE]    ✓ Updated primary zone (score: %d -> %d, radius: %dm -> %dm)",
		primaryZone.ConflictScore, combinedScore, primaryZone.RadiusMeters, newRadius)

	// Transfer all incidents from secondary zone to primary zone
	result, err := db.Exec(`
		UPDATE zone_incidents
		SET zone_id = $1
		WHERE zone_id = $2
	`, primaryZone.ID, secondaryZone.ID)
	if err != nil {
		return fmt.Errorf("failed to transfer incidents: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	log.Printf("[ZONE MERGE]    ✓ Transferred %d incidents to primary zone", rowsAffected)

	// Mark secondary zone as merged
	_, err = db.Exec(`
		UPDATE no_go_zones
		SET merged_into_zone_id = $1, resolution_type = 'merged', status = 'resolved', resolved_at = $2, updated_at = $2
		WHERE id = $3
	`, primaryZone.ID, now, secondaryZone.ID)
	if err != nil {
		return fmt.Errorf("failed to mark secondary zone as merged: %w", err)
	}
	log.Printf("[ZONE MERGE]    ✓ Marked secondary zone as merged")

	return nil
}

// handleMoveRequestCompletion handles move request completion logic
func handleMoveRequestCompletion(db *sqlx.DB, hub *websocket.Hub, moveRequest models.BinMoveRequest, req struct {
	ShiftBinID            int     `json:"shift_bin_id"`
	BinID                 string  `json:"bin_id"`
	UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty"`
	PhotoUrl              *string `json:"photo_url,omitempty"`
	MoveRequestID         *string `json:"move_request_id,omitempty"` // Links check to move request
	HasIncident           bool    `json:"has_incident"`
	IncidentType          *string `json:"incident_type,omitempty"`
	IncidentPhotoUrl      *string `json:"incident_photo_url,omitempty"`
	IncidentDescription   *string `json:"incident_description,omitempty"`
}, now int64) error {
	log.Printf("[MOVE] 🚚 Handling move request completion")
	log.Printf("[MOVE]    Type: %s", moveRequest.MoveType)

	// Mark move request as completed
	_, err := db.Exec(`
		UPDATE bin_move_requests
		SET status = 'completed', completed_at = $1, updated_at = $1
		WHERE id = $2
	`, now, moveRequest.ID)
	if err != nil {
		return fmt.Errorf("failed to complete move request: %w", err)
	}
	log.Printf("[MOVE] ✅ Move request marked as completed")

	// Broadcast move request completion to dashboard
	hub.BroadcastToRole("admin", map[string]interface{}{
		"type": "move_request_status_updated",
		"data": map[string]interface{}{
			"move_request_id": moveRequest.ID,
			"bin_id":          moveRequest.BinID,
			"new_status":      "completed",
			"completed_at":    now,
		},
	})
	hub.BroadcastToRole("manager", map[string]interface{}{
		"type": "move_request_status_updated",
		"data": map[string]interface{}{
			"move_request_id": moveRequest.ID,
			"bin_id":          moveRequest.BinID,
			"new_status":      "completed",
			"completed_at":    now,
		},
	})
	log.Printf("📡 Broadcast move_request_status_updated to managers: Move request %s → completed", moveRequest.ID)

	if moveRequest.MoveType == "pickup_only" {
		// Pickup for retirement or storage
		newStatus := "active" // Fallback
		if moveRequest.DisposalAction != nil {
			if *moveRequest.DisposalAction == "retire" {
				newStatus = "retired"
				log.Printf("[MOVE]    → Bin will be RETIRED")
			} else if *moveRequest.DisposalAction == "store" {
				newStatus = "in_storage"
				log.Printf("[MOVE]    → Bin will be IN STORAGE")
			}
		}

		_, err = db.Exec(`
			UPDATE bins
			SET status = $1, updated_at = $2
			WHERE id = $3
		`, newStatus, now, moveRequest.BinID)
		if err != nil {
			return fmt.Errorf("failed to update bin status: %w", err)
		}
		log.Printf("[MOVE] ✅ Bin status updated to %s", newStatus)

	} else if moveRequest.MoveType == "relocation" {
		// Update bin location to new coordinates
		log.Printf("[MOVE]    → Relocating bin to new address")
		_, err = db.Exec(`
			UPDATE bins
			SET latitude = $1,
			    longitude = $2,
			    current_street = $3,
			    status = 'active',
			    updated_at = $4
			WHERE id = $5
		`, moveRequest.NewLatitude,
			moveRequest.NewLongitude,
			moveRequest.NewAddress,
			now,
			moveRequest.BinID)
		if err != nil {
			return fmt.Errorf("failed to relocate bin: %w", err)
		}

		// Record the move in moves table
		// Parse address into separate fields
		var fromStreet, fromCity, fromZip, toStreet, toCity, toZip *string

		// Parse original address
		if moveRequest.OriginalAddress != "" {
			parts := strings.Split(moveRequest.OriginalAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				fromStreet = &street
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					fromCity = &city
					fromZip = &zip
				}
			}
		}

		// Parse new address
		if moveRequest.NewAddress != nil {
			parts := strings.Split(*moveRequest.NewAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				toStreet = &street
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					toCity = &city
					toZip = &zip
				}
			}
		}

		_, err = db.Exec(`
			INSERT INTO moves (
				bin_id, moved_from, moved_to, moved_on,
				move_type, from_street, from_city, from_zip,
				to_street, to_city, to_zip,
				move_request_id, shift_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, moveRequest.BinID,
			moveRequest.OriginalAddress,
			*moveRequest.NewAddress,
			now,
			"shift", // move_type
			fromStreet, fromCity, fromZip,
			toStreet, toCity, toZip,
			moveRequest.ID,
			moveRequest.AssignedShiftID)
		if err != nil {
			log.Printf("[MOVE] ⚠️  Failed to record move: %v", err)
			// Don't fail - move is already completed
		}

		log.Printf("[MOVE] ✅ Bin relocated to %s", *moveRequest.NewAddress)
	}

	return nil
}

// CancelShift cancels a specific shift
// PUT /api/manager/shifts/:id/cancel
func CancelShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "id")
		log.Printf("❌ REQUEST: PUT /api/manager/shifts/%s/cancel", shiftID)

		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "shift_id is required")
			return
		}

		now := time.Now().Unix()

		// Get shift details for websocket/FCM notifications
		var shift models.Shift
		err := db.Get(&shift, "SELECT * FROM shifts WHERE id = $1", shiftID)
		if err != nil {
			if err == sql.ErrNoRows {
				utils.RespondError(w, http.StatusNotFound, "Shift not found")
				return
			}
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift")
			return
		}

		// Only allow cancelling active, paused, or ready shifts
		if shift.Status != "active" && shift.Status != "paused" && shift.Status != "ready" {
			utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Cannot cancel shift with status: %s", shift.Status))
			return
		}

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		// 1. Update shift status to cancelled
		_, err = tx.Exec(`
			UPDATE shifts
			SET status = 'cancelled', updated_at = $1
			WHERE id = $2
		`, now, shiftID)
		if err != nil {
			log.Printf("❌ Error updating shift status: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to cancel shift")
			return
		}

		// 2. Return all in_progress move requests to pending
		result, err := tx.Exec(`
			UPDATE bin_move_requests
			SET status = 'pending',
			    assigned_shift_id = NULL,
			    updated_at = $1
			WHERE assigned_shift_id = $2
			AND status = 'in_progress'
		`, now, shiftID)
		if err != nil {
			log.Printf("⚠️  Error returning move requests to pending: %v", err)
			// Don't fail - continue
		} else {
			rowsAffected, _ := result.RowsAffected()
			if rowsAffected > 0 {
				log.Printf("✅ Returned %d move request(s) to pending status", rowsAffected)
			}
		}

		// 3. Delete shift_bins entries (cleanup)
		_, err = tx.Exec(`DELETE FROM shift_bins WHERE shift_id = $1`, shiftID)
		if err != nil {
			log.Printf("⚠️  Error deleting shift_bins: %v", err)
			// Don't fail - continue
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit cancellation")
			return
		}

		log.Printf("✅ Shift %s cancelled successfully", shiftID)

		// 4. Send WebSocket notification to driver's mobile app
		wsHub.BroadcastToUser(shift.DriverID, map[string]interface{}{
			"type": "shift_cancelled",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"cancelled_at": now,
				"message":      "Your shift has been cancelled by management",
			},
		})
		log.Printf("📡 Sent shift_cancelled websocket to driver %s", shift.DriverID)

		// 5. Send FCM push notification to driver
		if fcmService != nil {
			// Get driver's FCM token
			var fcmToken sql.NullString
			err = db.Get(&fcmToken, "SELECT fcm_token FROM users WHERE id = $1", shift.DriverID)
			if err == nil && fcmToken.Valid && fcmToken.String != "" {
				fcmErr := fcmService.SendShiftUpdateNotification(
					fcmToken.String,
					shiftID,
					"shift_cancelled",
				)
				if fcmErr != nil {
					log.Printf("⚠️  Failed to send FCM notification: %v", fcmErr)
				} else {
					log.Printf("📱 Sent FCM notification to driver")
				}
			}
		}

		// 6. Broadcast to dashboard (managers/admins)
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "shift_cancelled",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"driver_id":    shift.DriverID,
				"cancelled_at": now,
			},
		})
		wsHub.BroadcastToRole("manager", map[string]interface{}{
			"type": "shift_cancelled",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"driver_id":    shift.DriverID,
				"cancelled_at": now,
			},
		})

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "Shift cancelled successfully",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"driver_id":    shift.DriverID,
				"cancelled_at": now,
			},
		})
	}
}

// CancelAllActiveShifts cancels all active or paused shifts
// POST /api/manager/shifts/cancel-all-active
func CancelAllActiveShifts(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("❌ REQUEST: POST /api/manager/shifts/cancel-all-active")

		now := time.Now().Unix()

		// Get all active/paused shifts
		var shifts []models.Shift
		err := db.Select(&shifts, `
			SELECT * FROM shifts
			WHERE status IN ('active', 'paused', 'ready')
		`)
		if err != nil {
			log.Printf("❌ Error fetching active shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch active shifts")
			return
		}

		if len(shifts) == 0 {
			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"message": "No active shifts to cancel",
				"data": map[string]interface{}{
					"cancelled_count": 0,
				},
			})
			return
		}

		log.Printf("📋 Found %d active/paused shift(s) to cancel", len(shifts))

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		// Collect shift IDs
		shiftIDs := make([]string, len(shifts))
		for i, shift := range shifts {
			shiftIDs[i] = shift.ID
		}

		// 1. Update all shifts to cancelled
		query, args, err := sqlx.In(`
			UPDATE shifts
			SET status = 'cancelled', updated_at = ?
			WHERE id IN (?)
		`, now, shiftIDs)
		if err != nil {
			log.Printf("❌ Error building update query: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to build query")
			return
		}
		query = tx.Rebind(query)
		_, err = tx.Exec(query, args...)
		if err != nil {
			log.Printf("❌ Error updating shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to cancel shifts")
			return
		}

		// 2. Return all in_progress move requests to pending
		moveQuery, moveArgs, err := sqlx.In(`
			UPDATE bin_move_requests
			SET status = 'pending',
			    assigned_shift_id = NULL,
			    updated_at = ?
			WHERE assigned_shift_id IN (?)
			AND status = 'in_progress'
		`, now, shiftIDs)
		if err != nil {
			log.Printf("⚠️  Error building move request query: %v", err)
		} else {
			moveQuery = tx.Rebind(moveQuery)
			result, err := tx.Exec(moveQuery, moveArgs...)
			if err != nil {
				log.Printf("⚠️  Error returning move requests to pending: %v", err)
			} else {
				rowsAffected, _ := result.RowsAffected()
				if rowsAffected > 0 {
					log.Printf("✅ Returned %d move request(s) to pending status", rowsAffected)
				}
			}
		}

		// 3. Delete shift_bins entries
		deleteQuery, deleteArgs, err := sqlx.In(`DELETE FROM shift_bins WHERE shift_id IN (?)`, shiftIDs)
		if err != nil {
			log.Printf("⚠️  Error building delete query: %v", err)
		} else {
			deleteQuery = tx.Rebind(deleteQuery)
			_, err = tx.Exec(deleteQuery, deleteArgs...)
			if err != nil {
				log.Printf("⚠️  Error deleting shift_bins: %v", err)
			}
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit cancellations")
			return
		}

		log.Printf("✅ Cancelled %d shift(s) successfully", len(shifts))

		// 4. Send notifications to each affected driver
		for _, shift := range shifts {
			// WebSocket notification
			wsHub.BroadcastToUser(shift.DriverID, map[string]interface{}{
				"type": "shift_cancelled",
				"data": map[string]interface{}{
					"shift_id":     shift.ID,
					"cancelled_at": now,
					"message":      "Your shift has been cancelled by management",
				},
			})

			// FCM push notification
			if fcmService != nil {
				var fcmToken sql.NullString
				err = db.Get(&fcmToken, "SELECT fcm_token FROM users WHERE id = $1", shift.DriverID)
				if err == nil && fcmToken.Valid && fcmToken.String != "" {
					fcmErr := fcmService.SendShiftUpdateNotification(
						fcmToken.String,
						shift.ID,
						"shift_cancelled",
					)
					if fcmErr != nil {
						log.Printf("⚠️  Failed to send FCM to driver %s: %v", shift.DriverID, fcmErr)
					}
				}
			}
		}

		log.Printf("📡 Sent notifications to %d driver(s)", len(shifts))

		// 5. Broadcast to dashboard
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bulk_shifts_cancelled",
			"data": map[string]interface{}{
				"cancelled_count": len(shifts),
				"shift_ids":       shiftIDs,
				"cancelled_at":    now,
			},
		})
		wsHub.BroadcastToRole("manager", map[string]interface{}{
			"type": "bulk_shifts_cancelled",
			"data": map[string]interface{}{
				"cancelled_count": len(shifts),
				"shift_ids":       shiftIDs,
				"cancelled_at":    now,
			},
		})

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("Successfully cancelled %d shift(s)", len(shifts)),
			"data": map[string]interface{}{
				"cancelled_count": len(shifts),
				"shift_ids":       shiftIDs,
				"cancelled_at":    now,
			},
		})
	}
}
