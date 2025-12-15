package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

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
				"id":                   shift.ID,
				"driver_id":            shift.DriverID,
				"route_id":             shift.RouteID,
				"status":               shift.Status,
				"start_time":           shift.StartTime,
				"end_time":             shift.EndTime,
				"total_pause_seconds":  shift.TotalPauseSeconds,
				"pause_start_time":     shift.PauseStartTime,
				"total_bins":           shift.TotalBins,
				"completed_bins":       shift.CompletedBins,
				"bins":                 bins,
				"created_at":           shift.CreatedAt,
				"updated_at":           shift.UpdatedAt,
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
			log.Printf("✅ Auto-ended existing shift %s", existingShift.ID)
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

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Broadcast WebSocket update to driver
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": shift,
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
				"status":               shift.Status,
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
		log.Printf("📥 REQUEST: POST /api/driver/shift/complete-bin")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Parse request body
		var req struct {
			BinID string `json:"bin_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("   Bin ID: %s", req.BinID)

		// Get current active shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1`, userClaims.UserID)
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift")
			return
		}

		// Mark bin as completed in route_bins table
		now := time.Now().Unix()
		routeBinQuery := `UPDATE route_bins
						  SET is_completed = 1,
							  completed_at = $1
						  WHERE shift_id = $2
						  AND bin_id = $3
						  AND is_completed = 0`

		result, err := db.Exec(routeBinQuery, now, shift.ID, req.BinID)
		if err != nil {
			log.Printf("❌ Error marking bin as completed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete bin")
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			utils.RespondError(w, http.StatusBadRequest, "Bin not found in route or already completed")
			return
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
			bins = []models.RouteBinWithDetails{}
		}

		// Broadcast WebSocket update with bins
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": map[string]interface{}{
				"id":                  shift.ID,
				"status":              shift.Status,
				"completed_bins":      shift.CompletedBins,
				"total_bins":          shift.TotalBins,
				"bins":                bins,
			},
		})

		log.Printf("✅ Bin completed: %d/%d", shift.CompletedBins, shift.TotalBins)

		completionPercentage := 0.0
		if shift.TotalBins > 0 {
			completionPercentage = float64(shift.CompletedBins) / float64(shift.TotalBins) * 100
		}

		response := models.CompleteBinResponse{
			CompletedBins:        shift.CompletedBins,
			TotalBins:            shift.TotalBins,
			CompletionPercentage: completionPercentage,
		}

		log.Printf("📤 RESPONSE: 200 OK")
		log.Printf("   Completed: %d/%d (%.1f%%)", shift.CompletedBins, shift.TotalBins, completionPercentage)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}
}

// getRouteBinsWithDetails fetches route bins with full bin details
func getRouteBinsWithDetails(db *sqlx.DB, shiftID string) ([]models.RouteBinWithDetails, error) {
	query := `
		SELECT
			rb.id,
			rb.shift_id,
			rb.bin_id,
			rb.sequence_order,
			rb.is_completed,
			rb.completed_at,
			rb.created_at,
			b.bin_number,
			b.current_street,
			b.city,
			b.zip,
			b.fill_percentage,
			b.latitude,
			b.longitude
		FROM route_bins rb
		JOIN bins b ON rb.bin_id = b.id
		WHERE rb.shift_id = $1
		ORDER BY rb.sequence_order ASC`

	var bins []models.RouteBinWithDetails
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

		// Check if driver is online and has recent location
		var driverLocation struct {
			Latitude    float64 `db:"latitude"`
			Longitude   float64 `db:"longitude"`
			IsConnected bool    `db:"is_connected"`
			UpdatedAt   int64   `db:"updated_at"`
		}

		now := time.Now().Unix()
		locationQuery := `
			SELECT latitude, longitude, is_connected, updated_at
			FROM driver_current_location
			WHERE driver_id = $1
		`

		err := db.Get(&driverLocation, locationQuery, req.DriverID)
		if err == sql.ErrNoRows {
			log.Printf("❌ Driver is not online (no location data)")
			utils.RespondError(w, http.StatusBadRequest, "Driver is not online. Ask driver to log in and enable location.")
			return
		}
		if err != nil {
			log.Printf("❌ Error checking driver location: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to check driver status")
			return
		}

		// Check if driver is connected
		if !driverLocation.IsConnected {
			log.Printf("❌ Driver is not connected (is_connected = false)")
			utils.RespondError(w, http.StatusBadRequest, "Driver is not online. Ask driver to log in and enable location.")
			return
		}

		// Check if location is recent (updated within last 30 seconds)
		timeSinceUpdate := now - driverLocation.UpdatedAt
		if timeSinceUpdate > 30 {
			log.Printf("❌ Driver location is stale (last update: %ds ago)", timeSinceUpdate)
			utils.RespondError(w, http.StatusBadRequest, "Driver location is not available. Ask driver to ensure GPS is enabled.")
			return
		}

		log.Printf("✅ Driver is online with recent location (%.6f, %.6f) - updated %ds ago",
			driverLocation.Latitude, driverLocation.Longitude, timeSinceUpdate)

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

		// Fetch full bin details for TSP optimization
		binQuery := `SELECT id, current_street, latitude, longitude, fill_percentage FROM bins WHERE id IN (?)`
		binQuery, binArgs, err := sqlx.In(binQuery, req.BinIDs)
		if err != nil {
			log.Printf("❌ Error building bin query: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch bins")
			return
		}
		binQuery = tx.Rebind(binQuery)

		var binDetails []struct {
			ID             string  `db:"id"`
			CurrentStreet  string  `db:"current_street"`
			Latitude       float64 `db:"latitude"`
			Longitude      float64 `db:"longitude"`
			FillPercentage int     `db:"fill_percentage"`
		}
		err = tx.Select(&binDetails, binQuery, binArgs...)
		if err != nil {
			log.Printf("❌ Error fetching bin details: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch bins")
			return
		}

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

		// Optimize route using TSP algorithm
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

		// Create new shift
		shiftID := uuid.New().String()
		now := time.Now().Unix()
		totalBins := len(req.BinIDs)

		shiftQuery := `INSERT INTO shifts (id, driver_id, route_id, status, total_bins, created_at, updated_at)
					   VALUES ($1, $2, $3, 'ready', $4, $5, $6)`

		_, err = tx.Exec(shiftQuery, shiftID, req.DriverID, req.RouteID, totalBins, now, now)
		if err != nil {
			log.Printf("❌ Error creating shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create shift")
			return
		}

		// Insert route bins with OPTIMIZED sequence order
		for i, bin := range optimizedBins {
			routeBinQuery := `INSERT INTO route_bins (shift_id, bin_id, sequence_order, created_at)
							  VALUES ($1, $2, $3, $4)`

			_, err = tx.Exec(routeBinQuery, shiftID, bin.ID, i+1, now)
			if err != nil {
				log.Printf("❌ Error inserting route_bin: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to assign bins to route")
				return
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

// GetAllDrivers returns all drivers with their current status and last location
// GET /api/manager/drivers
func GetAllDrivers(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/manager/drivers")

		// Query all drivers with their current shift status
		query := `
			SELECT
				u.id as driver_id,
				u.name,
				COALESCE(s.status, 'inactive') as status,
				s.id as shift_id,
				s.completed_bins as current_bin,
				s.total_bins
			FROM users u
			LEFT JOIN shifts s ON u.id = s.driver_id
				AND s.status IN ('active', 'paused', 'ready')
			WHERE u.role = 'driver'
			ORDER BY u.name
		`

		type DriverRow struct {
			DriverID    string  `db:"driver_id"`
			Name        string  `db:"name"`
			Status      string  `db:"status"`
			ShiftID     *string `db:"shift_id"`
			CurrentBin  *int    `db:"current_bin"`
			TotalBins   *int    `db:"total_bins"`
		}

		var drivers []DriverRow
		err := db.Select(&drivers, query)
		if err != nil {
			log.Printf("❌ Error fetching drivers: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Database error")
			return
		}

		// For each driver with active shift, get their last location
		var response []map[string]interface{}
		for _, driver := range drivers {
			driverData := map[string]interface{}{
				"driver_id":   driver.DriverID,
				"name":        driver.Name,
				"status":      driver.Status,
				"shift_id":    driver.ShiftID,
				"current_bin": driver.CurrentBin,
				"total_bins":  driver.TotalBins,
			}

			// Get last location from driver_current_location table
			// This table has exactly 1 row per driver with their latest position
			if driver.ShiftID != nil {
				var location models.DriverLocation
				locationQuery := `
					SELECT driver_id, latitude, longitude, heading, speed,
						   accuracy, shift_id, timestamp, is_connected, updated_at
					FROM driver_current_location
					WHERE driver_id = $1
				`
				err := db.Get(&location, locationQuery, driver.DriverID)
				if err == nil {
					driverData["last_location"] = location
				}
			}

			response = append(response, driverData)
		}

		log.Printf("📤 RESPONSE: 200 - Found %d drivers", len(response))
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    response,
		})
	}
}
