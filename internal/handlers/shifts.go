package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ropacal-backend/internal/database"
	"ropacal-backend/internal/helpers"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/optimization"
	"ropacal-backend/internal/services/redis"
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

		// Get route bins with details (for backward compatibility)
		bins, err := getShiftTasksWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch route bins")
			return
		}

		// Also get tasks from route_tasks table (new task-based system)
		tasks, err := database.GetShiftTasks(db, shift.ID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v (using bins only)", err)
			tasks = []models.RouteTask{} // Empty tasks array on error
		}

		log.Printf("📤 RESPONSE: 200 OK")
		log.Printf("   Shift ID: %s", shift.ID)
		log.Printf("   Status: %s", shift.Status)
		log.Printf("   Route: %v", shift.RouteID)
		log.Printf("   Bins: %d/%d (%d bin details)", shift.CompletedBins, shift.TotalBins, len(bins))
		log.Printf("   Tasks: %d (new task-based system)", len(tasks))

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
				"bins":                bins, // Legacy field
				"tasks":               tasks, // New task-based field
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
			},
		})
	}
}

// GetShiftByID retrieves a specific shift by its ID (manager/admin only)
func GetShiftByID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "shiftId")
		log.Printf("📥 REQUEST: GET /api/manager/shifts/%s", shiftID)

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		var shift models.Shift
		query := `SELECT * FROM shifts WHERE id = $1`

		err := db.Get(&shift, query, shiftID)
		if err == sql.ErrNoRows {
			log.Printf("📤 RESPONSE: 404 - Shift not found")
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}
		if err != nil {
			log.Printf("❌ Error getting shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Database error")
			return
		}

		// Get route tasks with details
		bins, err := getShiftTasksWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route tasks: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch route tasks")
			return
		}

		log.Printf("📤 RESPONSE: 200 OK")
		log.Printf("   Shift ID: %s", shift.ID)
		log.Printf("   Status: %s", shift.Status)
		log.Printf("   Tasks: %d/%d (%d task details)", shift.CompletedBins, shift.TotalBins, len(bins))

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
				"truck_bin_capacity":  shift.TruckBinCapacity,
				"bins":                bins,
				"created_at":          shift.CreatedAt,
				"updated_at":          shift.UpdatedAt,
			},
		})
	}
}

// GetAllShifts returns a list of all shifts with optional filtering
// GET /api/manager/shifts
// Query params: status, driver_id, start_date, end_date, limit, offset
func GetAllShifts(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/manager/shifts")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Parse query parameters
		status := r.URL.Query().Get("status")
		driverID := r.URL.Query().Get("driver_id")
		startDate := r.URL.Query().Get("start_date") // Unix timestamp
		endDate := r.URL.Query().Get("end_date")     // Unix timestamp
		limitStr := r.URL.Query().Get("limit")
		offsetStr := r.URL.Query().Get("offset")

		// Default pagination
		limit := 100
		offset := 0

		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 1000 {
				limit = l
			}
		}
		if offsetStr != "" {
			if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
				offset = o
			}
		}

		// Build query dynamically based on filters
		query := `
			SELECT s.*, u.name as driver_name, u.email as driver_email
			FROM shifts s
			LEFT JOIN users u ON s.driver_id = u.id
			WHERE 1=1`
		args := []interface{}{}
		argIndex := 1

		if status != "" {
			query += fmt.Sprintf(" AND s.status = $%d", argIndex)
			args = append(args, status)
			argIndex++
		}

		if driverID != "" {
			query += fmt.Sprintf(" AND s.driver_id = $%d", argIndex)
			args = append(args, driverID)
			argIndex++
		}

		if startDate != "" {
			startTimestamp, err := strconv.ParseInt(startDate, 10, 64)
			if err == nil {
				query += fmt.Sprintf(" AND s.created_at >= $%d", argIndex)
				args = append(args, startTimestamp)
				argIndex++
			}
		}

		if endDate != "" {
			endTimestamp, err := strconv.ParseInt(endDate, 10, 64)
			if err == nil {
				query += fmt.Sprintf(" AND s.created_at <= $%d", argIndex)
				args = append(args, endTimestamp)
				argIndex++
			}
		}

		// Add ordering and pagination
		query += " ORDER BY s.created_at DESC"
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIndex, argIndex+1)
		args = append(args, limit, offset)

		// Execute query
		rows, err := db.Queryx(query, args...)
		if err != nil {
			log.Printf("❌ Error fetching shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shifts")
			return
		}
		defer rows.Close()

		type ShiftWithDriver struct {
			models.Shift
			DriverName  string `db:"driver_name" json:"driver_name"`
			DriverEmail string `db:"driver_email" json:"driver_email"`
		}

		type ShiftResponse struct {
			ShiftWithDriver
			EstimatedCompletionTime *int64   `json:"estimated_completion_time,omitempty"` // Unix timestamp
			TotalDistanceMiles      *float64 `json:"total_distance_miles,omitempty"`
		}

		var shifts []ShiftResponse
		for rows.Next() {
			var shift ShiftWithDriver
			if err := rows.StructScan(&shift); err != nil {
				log.Printf("❌ Error scanning shift: %v", err)
				continue
			}

			// Create response with computed fields
			shiftResp := ShiftResponse{
				ShiftWithDriver: shift,
			}

			// Add computed fields if optimization metadata exists
			if shift.OptimizationMetadata != nil {
				// Convert distance from km to miles
				distanceMiles := shift.OptimizationMetadata.TotalDistanceKm * 0.621371
				shiftResp.TotalDistanceMiles = &distanceMiles

				// Calculate estimated completion time (start_time + duration)
				if shift.StartTime != nil {
					estimatedCompletion := *shift.StartTime + int64(shift.OptimizationMetadata.TotalDurationSeconds)
					shiftResp.EstimatedCompletionTime = &estimatedCompletion
				}
			}

			shifts = append(shifts, shiftResp)
		}

		// Get total count for pagination
		countQuery := `SELECT COUNT(*) FROM shifts s WHERE 1=1`
		if status != "" {
			countQuery += " AND s.status = '" + status + "'"
		}
		if driverID != "" {
			countQuery += " AND s.driver_id = '" + driverID + "'"
		}

		var totalCount int
		db.Get(&totalCount, countQuery)

		log.Printf("📤 RESPONSE: 200 OK")
		log.Printf("   Found %d shifts (total: %d)", len(shifts), totalCount)
		log.Printf("   Filters: status=%s, driver_id=%s, limit=%d, offset=%d",
			status, driverID, limit, offset)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"shifts":      shifts,
				"total_count": totalCount,
				"limit":       limit,
				"offset":      offset,
			},
		})
	}
}

// GetManagerShiftHistory returns paginated completed-shift history with per-shift task stats.
// GET /api/manager/shifts/history
// Query params: driver_id, start_date (unix), end_date (unix), limit, offset
func GetManagerShiftHistory(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/manager/shifts/history")

		_, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Pagination
		limit := 50
		offset := 0
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 500 {
			limit = l
		}
		if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
			offset = o
		}

		driverID := r.URL.Query().Get("driver_id")
		startDate := r.URL.Query().Get("start_date")
		endDate := r.URL.Query().Get("end_date")

		// Build WHERE clause
		where := "WHERE 1=1"
		args := []interface{}{}
		argIndex := 1

		if driverID != "" {
			where += fmt.Sprintf(" AND sh.driver_id = $%d", argIndex)
			args = append(args, driverID)
			argIndex++
		}
		if startDate != "" {
			if ts, err := strconv.ParseInt(startDate, 10, 64); err == nil {
				where += fmt.Sprintf(" AND sh.ended_at >= $%d", argIndex)
				args = append(args, ts)
				argIndex++
			}
		}
		if endDate != "" {
			if ts, err := strconv.ParseInt(endDate, 10, 64); err == nil {
				where += fmt.Sprintf(" AND sh.ended_at <= $%d", argIndex)
				args = append(args, ts)
				argIndex++
			}
		}

		// Main query — join route_tasks for per-type counts
		query := fmt.Sprintf(`
			SELECT
				sh.id,
				sh.driver_id,
				COALESCE(u.name, '') AS driver_name,
				COALESCE(u.email, '') AS driver_email,
				sh.route_id,
				sh.start_time,
				sh.end_time,
				sh.created_at,
				sh.ended_at,
				sh.total_pause_seconds,
				sh.total_bins,
				sh.completed_bins,
				sh.completion_rate,
				sh.incidents_reported,
				sh.field_observations,
				sh.end_reason,
				COALESCE(COUNT(CASE WHEN rt.task_type = 'collection' AND rt.is_completed = 1 THEN 1 END), 0) AS collections_completed,
				COALESCE(COUNT(CASE WHEN rt.task_type = 'collection' AND rt.skipped = TRUE THEN 1 END), 0) AS collections_skipped,
				COALESCE(COUNT(CASE WHEN rt.task_type = 'placement' AND rt.is_completed = 1 THEN 1 END), 0) AS placements_completed,
				COALESCE(COUNT(CASE WHEN rt.task_type = 'placement' AND rt.skipped = TRUE THEN 1 END), 0) AS placements_skipped,
				COALESCE(COUNT(DISTINCT CASE WHEN rt.task_type = 'pickup' AND rt.is_completed = 1 AND rt.move_request_id IS NOT NULL THEN rt.move_request_id END), 0) AS move_requests_completed,
				COALESCE(COUNT(CASE WHEN rt.skipped = TRUE THEN 1 END), 0) AS total_skipped,
				COALESCE(COUNT(CASE WHEN rt.task_type = 'warehouse_stop' AND rt.is_completed = 1 THEN 1 END), 0) AS warehouse_stops
			FROM shift_history sh
			LEFT JOIN users u ON sh.driver_id = u.id
			LEFT JOIN route_tasks rt ON sh.id = rt.shift_id
			%s
			GROUP BY sh.id, u.name, u.email
			ORDER BY sh.ended_at DESC
			LIMIT $%d OFFSET $%d
		`, where, argIndex, argIndex+1)
		args = append(args, limit, offset)

		type ShiftHistoryRow struct {
			ID                    string   `db:"id" json:"id"`
			DriverID              string   `db:"driver_id" json:"driver_id"`
			DriverName            string   `db:"driver_name" json:"driver_name"`
			DriverEmail           string   `db:"driver_email" json:"driver_email"`
			RouteID               *string  `db:"route_id" json:"route_id"`
			StartTime             *int64   `db:"start_time" json:"start_time"`
			EndTime               *int64   `db:"end_time" json:"end_time"`
			CreatedAt             int64    `db:"created_at" json:"created_at"`
			EndedAt               int64    `db:"ended_at" json:"ended_at"`
			TotalPauseSeconds     int      `db:"total_pause_seconds" json:"total_pause_seconds"`
			TotalBins             int      `db:"total_bins" json:"total_bins"`
			CompletedBins         int      `db:"completed_bins" json:"completed_bins"`
			CompletionRate        float64  `db:"completion_rate" json:"completion_rate"`
			IncidentsReported     int      `db:"incidents_reported" json:"incidents_reported"`
			FieldObservations     int      `db:"field_observations" json:"field_observations"`
			EndReason             string   `db:"end_reason" json:"end_reason"`
			CollectionsCompleted  int64    `db:"collections_completed" json:"collections_completed"`
			CollectionsSkipped    int64    `db:"collections_skipped" json:"collections_skipped"`
			PlacementsCompleted   int64    `db:"placements_completed" json:"placements_completed"`
			PlacementsSkipped     int64    `db:"placements_skipped" json:"placements_skipped"`
			MoveRequestsCompleted int64    `db:"move_requests_completed" json:"move_requests_completed"`
			TotalSkipped          int64    `db:"total_skipped" json:"total_skipped"`
			WarehouseStops        int64    `db:"warehouse_stops" json:"warehouse_stops"`
		}

		rows, err := db.Queryx(query, args...)
		if err != nil {
			log.Printf("❌ Error fetching shift history: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift history")
			return
		}
		defer rows.Close()

		var history []ShiftHistoryRow
		for rows.Next() {
			var row ShiftHistoryRow
			if err := rows.StructScan(&row); err != nil {
				log.Printf("❌ Error scanning shift history row: %v", err)
				continue
			}
			history = append(history, row)
		}
		if history == nil {
			history = []ShiftHistoryRow{}
		}

		// Total count (without GROUP BY)
		countWhere := where
		countArgs := args[:len(args)-2] // strip limit/offset
		var totalCount int
		countQuery := fmt.Sprintf(`SELECT COUNT(DISTINCT sh.id) FROM shift_history sh LEFT JOIN users u ON sh.driver_id = u.id %s`, countWhere)
		if err := db.Get(&totalCount, countQuery, countArgs...); err != nil {
			log.Printf("⚠️  Could not count shift history: %v", err)
		}

		log.Printf("📤 RESPONSE: 200 OK — %d shift history records (total: %d)", len(history), totalCount)
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"shifts":      history,
				"total_count": totalCount,
				"limit":       limit,
				"offset":      offset,
			},
		})
	}
}

// GetShiftHistoryTasks returns the granular per-task breakdown for a single completed shift.
// GET /api/manager/shifts/history/{shiftId}/tasks
func GetShiftHistoryTasks(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "shiftId")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "shiftId is required")
			return
		}

		_, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		type TaskRow struct {
			ID                  string   `db:"id"                    json:"id"`
			SequenceOrder       int      `db:"sequence_order"        json:"sequence_order"`
			TaskType            string   `db:"task_type"             json:"task_type"`
			IsCompleted         int      `db:"is_completed"          json:"is_completed"`
			Skipped             bool     `db:"skipped"               json:"skipped"`
			CompletedAt         *int64   `db:"completed_at"          json:"completed_at"`
			Address             *string  `db:"address"               json:"address"`
			Latitude            *float64 `db:"latitude"              json:"latitude"`
			Longitude           *float64 `db:"longitude"             json:"longitude"`
			TaskData            *string  `db:"task_data"             json:"task_data"`
			// Collection / bin fields
			BinID               *string  `db:"bin_id"                json:"bin_id"`
			BinNumber           *int     `db:"bin_number"            json:"bin_number"`
			UpdatedFillPct      *int     `db:"updated_fill_percentage" json:"updated_fill_percentage"`
			BinStreet           *string  `db:"bin_street"            json:"bin_street"`
			BinCity             *string  `db:"bin_city"              json:"bin_city"`
			// Placement fields
			PotentialLocationID *string  `db:"potential_location_id" json:"potential_location_id"`
			NewBinNumber        *int     `db:"new_bin_number"        json:"new_bin_number"`
			PlacementAddress    *string  `db:"placement_address"     json:"placement_address"`
			PlacementCreatedBinID *string `db:"placement_created_bin_id" json:"placement_created_bin_id"`
			PlacementCreatedBinNumber *int `db:"placement_created_bin_number" json:"placement_created_bin_number"`
			// Move request fields
			MoveRequestID       *string  `db:"move_request_id"       json:"move_request_id"`
			MoveType            *string  `db:"move_type"             json:"move_type"`
			DestinationAddress  *string  `db:"destination_address"   json:"destination_address"`
			// Warehouse fields
			WarehouseAction     *string  `db:"warehouse_action"      json:"warehouse_action"`
			BinsToLoad          *int     `db:"bins_to_load"          json:"bins_to_load"`
		}

		query := `
			SELECT
				rt.id,
				rt.sequence_order,
				rt.task_type,
				rt.is_completed,
				rt.skipped,
				rt.completed_at,
				rt.address,
				rt.latitude,
				rt.longitude,
				rt.task_data::text AS task_data,
				rt.bin_id,
				rt.bin_number,
				rt.updated_fill_percentage,
				b.current_street    AS bin_street,
				b.city              AS bin_city,
				rt.potential_location_id,
				rt.new_bin_number,
				pl.address          AS placement_address,
				pl.converted_to_bin_id AS placement_created_bin_id,
				cb.bin_number       AS placement_created_bin_number,
				rt.move_request_id,
				bmr.move_type,
				bmr.new_address       AS destination_address,
				rt.warehouse_action,
				rt.bins_to_load
			FROM route_tasks rt
			LEFT JOIN bins b   ON b.id  = rt.bin_id
			LEFT JOIN potential_locations pl ON pl.id = rt.potential_location_id
			LEFT JOIN bins cb  ON cb.id = pl.converted_to_bin_id
			LEFT JOIN bin_move_requests bmr ON bmr.id = rt.move_request_id
			WHERE rt.shift_id = $1
			ORDER BY rt.sequence_order ASC
		`

		rows, err := db.Queryx(query, shiftID)
		if err != nil {
			log.Printf("❌ GetShiftHistoryTasks: query error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch tasks")
			return
		}
		defer rows.Close()

		var tasks []TaskRow
		for rows.Next() {
			var t TaskRow
			if err := rows.StructScan(&t); err != nil {
				log.Printf("❌ GetShiftHistoryTasks: scan error: %v", err)
				continue
			}
			tasks = append(tasks, t)
		}
		if tasks == nil {
			tasks = []TaskRow{}
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    tasks,
		})
	}
}

// PreflightCheck validates GPS readiness before starting a shift
// Returns: ready status, location cached, Centrifugo connection health
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

		// Check 1: Verify location is in Redis
		ctx := context.Background()
		locationJSON, locationErr := redisClient.GetDriverLocation(ctx, userClaims.UserID)

		if locationErr != nil {
			log.Printf("❌ Location not cached in Redis: %v", locationErr)
			checks["location_cached"] = false
			message = "Location syncing - Please wait..."

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

		// Check 2: Parse and validate GPS accuracy
		var driverLocation models.DriverLocation
		if err := json.Unmarshal([]byte(locationJSON), &driverLocation); err != nil {
			log.Printf("❌ Failed to parse location JSON: %v", err)
			message = "Invalid location data"

			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success":     true,
				"ready":       ready,
				"checks":      checks,
				"message":     message,
				"retry_after": retryAfter,
			})
			return
		}

		// Check accuracy availability
		accuracy := 0.0
		if driverLocation.Accuracy != nil {
			accuracy = *driverLocation.Accuracy
		}

		log.Printf("✅ Location cached: (%.6f, %.6f), accuracy: %.1fm",
			driverLocation.Latitude, driverLocation.Longitude, accuracy)

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

		log.Printf("✅ Preflight checks passed:")
		log.Printf("   GPS Quality: %s (%.1fm)", checks["gps_quality"], accuracy)
		log.Printf("   Location Cached: %v", checks["location_cached"])
		log.Printf("   Can Optimize: %v", checks["can_optimize"])
		log.Printf("   Estimated Start Time: < 5 seconds")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":               true,
			"ready":                 ready,
			"checks":                checks,
			"message":               message,
			"estimated_start_time":  "< 5 seconds",
			"location": map[string]float64{
				"latitude":  driverLocation.Latitude,
				"longitude": driverLocation.Longitude,
				"accuracy":  accuracy,
			},
		})
	}
}

// StartShift starts an assigned shift
func StartShift(db *sqlx.DB, hub *websocket.Hub, redisClient *redis.Client) http.HandlerFunc {
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


	// Smart Route Optimization Logic
	// If lock_route_order is true, skip optimization (use manager's exact task order)
	// If lock_route_order is false, run full route optimization with dynamic warehouse insertion
	if shift.LockRouteOrder {
		log.Printf("🔒 Route order is locked - skipping optimization and using manager's exact task sequence")
	} else {
		log.Printf("🚀 Route order unlocked - performing smart optimization with dynamic warehouse insertion")

		// Get driver's CURRENT location from Redis (published via Centrifugo)
		var driverLocation struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		}

		if redisClient == nil {
			log.Printf("❌ Redis client not available - cannot retrieve location")
			utils.RespondError(w, http.StatusInternalServerError, "Location service unavailable")
			return
		}

		ctx := context.Background()
		locationJSON, locationErr := redisClient.GetDriverLocation(ctx, userClaims.UserID)

		if locationErr != nil {
			log.Printf("❌ Driver location not available in Redis: %v", locationErr)
			log.Printf("   This means the driver hasn't published their GPS location via the mobile app yet.")
			log.Printf("   The mobile app publishes location to Centrifugo approximately every 1 second when GPS is enabled.")
			utils.RespondError(w, http.StatusBadRequest, "Please enable GPS to start shift")
			return
		}

		// DEBUG: Log raw Redis data to diagnose unexpected coordinates
		log.Printf("🔍 [DEBUG] Raw Redis data for driver %s: %s", userClaims.UserID, locationJSON)

		// Parse location JSON from Redis
		if err := json.Unmarshal([]byte(locationJSON), &driverLocation); err != nil {
			log.Printf("❌ Failed to parse location JSON from Redis: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to parse location data")
			return
		}

		log.Printf("✅ Got driver location from Redis: (%.6f, %.6f)", driverLocation.Latitude, driverLocation.Longitude)

		// Validate warehouse coordinates
		if shift.WarehouseLatitude == nil || shift.WarehouseLongitude == nil {
			log.Printf("❌ Warehouse coordinates not set for shift")
			utils.RespondError(w, http.StatusInternalServerError, "Warehouse location not configured")
			return
		}

		// Run Mapbox v2 route optimization (capacity-aware, automatic warehouse trips)
		capacity := 4 // Default capacity
		if shift.TruckBinCapacity != nil {
			capacity = *shift.TruckBinCapacity
		}

		warehouseAddr := ""
		if shift.WarehouseAddress != nil {
			warehouseAddr = *shift.WarehouseAddress
		}

		err = optimizeRouteWithMapbox(
			db,
			shift.ID,
			capacity,
			driverLocation.Latitude,
			driverLocation.Longitude,
			*shift.WarehouseLatitude,
			*shift.WarehouseLongitude,
			warehouseAddr,
		)

		if err != nil {
			log.Printf("❌ Mapbox v2 route optimization failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Route optimization failed")
			return
		}

		log.Printf("✅ Mapbox v2 route optimization complete")
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
	bins, err := getShiftTasksWithDetails(db, shift.ID)
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
		log.Printf("🔄 Ending shift: %s (status: ended)", shift.ID)
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
		log.Printf("✅ Shift ended successfully")

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

		// Remove incomplete move request tasks from route_tasks
		deleteTasksQuery := `DELETE FROM route_tasks
								 WHERE shift_id = $1
								 AND is_completed = 0
								 AND bin_id IN (
									SELECT bin_id FROM bin_move_requests
									WHERE assigned_shift_id IS NULL
									AND status = 'pending'
								 )`
		_, err = db.Exec(deleteTasksQuery, shift.ID)
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

// CompleteShiftBin marks a task as completed within an active shift (collection, pickup, dropoff, warehouse, placement)
func CompleteTask(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[DIAGNOSTIC] ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("[DIAGNOSTIC] 📥 REQUEST: POST /api/driver/shift/complete-task")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("[DIAGNOSTIC]    User: %s (%s)", userClaims.Email, userClaims.UserID)

		// Parse request body
		var req struct {
			TaskID                string     `json:"task_id"`                      // ID of shift_bins record (identifies specific waypoint)
			BinID                 string  `json:"bin_id"`                            // DEPRECATED: Use task_id instead
			UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty"` // Now optional
			PhotoUrl              *string `json:"photo_url,omitempty"`
			MoveRequestID         *string `json:"move_request_id,omitempty"` // Links check to move request
			NewBinNumber          int     `json:"new_bin_number"`                // REQUIRED: Driver-provided bin number for placements

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

		log.Printf("[DIAGNOSTIC]    Task ID: %s (route task UUID)", req.TaskID)
		log.Printf("[DIAGNOSTIC]    Bin ID: %s (deprecated)", req.BinID)
		log.Printf("[DIAGNOSTIC] 🔍 FILL PERCENTAGE DEBUG:")
		if req.UpdatedFillPercentage != nil {
			log.Printf("[DIAGNOSTIC]    ✅ Received non-null fill_percentage from mobile app: %d%%", *req.UpdatedFillPercentage)
			log.Printf("[DIAGNOSTIC]    📊 This value WILL be written to database")
		} else {
			log.Printf("[DIAGNOSTIC]    ⚠️  Received NULL fill_percentage from mobile app")
			log.Printf("[DIAGNOSTIC]    📊 Database will store NULL (incident report or no assessment)")
		}
		if req.PhotoUrl != nil {
			log.Printf("[DIAGNOSTIC]    Photo URL: %s", *req.PhotoUrl)
		} else {
			log.Printf("[DIAGNOSTIC]    Photo URL: null (no photo)")
		}
		if req.HasIncident {
			log.Printf("[DIAGNOSTIC]    🚨 INCIDENT REPORTED: %s", *req.IncidentType)
		}

		// Note: Validation for photo/fill percentage is deferred until we know the stop type
		// Warehouse stops don't require photo or fill percentage

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

		// Mark task as completed (route_tasks ONLY - unified task system)
		now := time.Now().Unix()

		log.Printf("[DIAGNOSTIC] 🔍 Finding task in route_tasks table...")
		log.Printf("[DIAGNOSTIC]    Shift ID: %s", shift.ID)
		log.Printf("[DIAGNOSTIC]    Bin ID: %s", req.BinID)

		// Find the next incomplete task in this shift
		var taskID string
		var taskType string

		if req.BinID != "" {
			// Collection/Pickup/Dropoff task - has bin_id
			log.Printf("[DIAGNOSTIC] 🔍 Looking for task with bin_id...")
			err = db.QueryRow(`
				SELECT id, task_type
				FROM route_tasks
				WHERE shift_id = $1
				  AND bin_id = $2
				  AND is_completed = 0
				ORDER BY sequence_order ASC
				LIMIT 1
			`, shift.ID, req.BinID).Scan(&taskID, &taskType)
		} else {
			// Warehouse or Placement task - no bin_id
			log.Printf("[DIAGNOSTIC] 🔍 Looking for warehouse/placement task...")
			err = db.QueryRow(`
				SELECT id, task_type
				FROM route_tasks
				WHERE shift_id = $1
				  AND is_completed = 0
				  AND (task_type = 'warehouse_stop' OR task_type = 'placement')
				ORDER BY sequence_order ASC
				LIMIT 1
			`, shift.ID).Scan(&taskID, &taskType)
		}

		if err == sql.ErrNoRows {
			log.Printf("[DIAGNOSTIC] ⚠️  Task not found in route or already completed")
			utils.RespondError(w, http.StatusBadRequest, "Task not found in route or already completed")
			return
		}
		if err != nil {
			log.Printf("❌ Error querying route_tasks: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to find task")
			return
		}

		log.Printf("[DIAGNOSTIC] ✅ Found task: ID=%s, Type=%s", taskID, taskType)

		// Validate: at least photo OR fill percentage required (unless incident or warehouse)
		if !req.HasIncident && taskType != "warehouse_stop" && req.PhotoUrl == nil && req.UpdatedFillPercentage == nil {
			log.Printf("[DIAGNOSTIC] ⚠️  Validation failed: task_type=%s, photo=%v, fill=%v",
				taskType, req.PhotoUrl != nil, req.UpdatedFillPercentage != nil)
			utils.RespondError(w, http.StatusBadRequest, "At least photo or fill percentage is required")
			return
		}
		// Update the task as completed
		updateQuery := `UPDATE route_tasks
						SET is_completed = 1,
							completed_at = $1,
							updated_fill_percentage = $2,
							updated_at = $3
						WHERE id = $4`
		result, err := db.Exec(updateQuery, now, req.UpdatedFillPercentage, now, taskID)
		if err != nil {
			log.Printf("❌ Error marking task as completed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete task")
			return
		}

		log.Printf("[DIAGNOSTIC] ✅ Task marked as completed in route_tasks table")

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			log.Printf("[DIAGNOSTIC] ⚠️  Update affected 0 rows")
			utils.RespondError(w, http.StatusBadRequest, "Failed to update task")
			return
		}

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
			// Use task_type from route_tasks (already fetched above)
			log.Printf("[DIAGNOSTIC] Task type: %s", taskType)

			// Only finalize move request (update bin location, mark complete) when DROPOFF is completed
			// For pickup, we just mark the task complete (already done above) and continue
			if taskType == "dropoff" {
				log.Printf("[DIAGNOSTIC] This is the DROPOFF - finalizing move request")
				err = handleMoveRequestCompletion(db, hub, moveRequest, req, now)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error handling move request: %v", err)
					// Don't fail - just log
				}
			} else {
				log.Printf("[DIAGNOSTIC] This is the PICKUP - move request remains in_progress")
			}
		} else if taskType == "placement" {
			// PLACEMENT task - either create new bin (potential_location) or redeploy warehouse bin
			log.Printf("[DIAGNOSTIC] 📍 Detected PLACEMENT task - determining source...")

			// Get placement details from route_tasks table
			var potentialLocationID *string
			var placementSource *string
			var taskBinID *string
			err = db.QueryRow(`
				SELECT potential_location_id, placement_source, bin_id
				FROM route_tasks
				WHERE id = $1
			`, taskID).Scan(&potentialLocationID, &placementSource, &taskBinID)
			if err != nil {
				log.Printf("[DIAGNOSTIC] ❌ Error fetching placement details: %v", err)
				db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to retrieve placement details")
				return
			}

			src := "potential_location"
			if placementSource != nil {
				src = *placementSource
			}
			log.Printf("[DIAGNOSTIC]    placement_source: %s", src)

			if src == "warehouse" {
				// ── WAREHOUSE REDEPLOYMENT ─────────────────────────────────────────────
				// The bin already exists (in_storage). Update its location + status → active.
				log.Printf("[DIAGNOSTIC] 🏭 Warehouse redeployment path")

				if taskBinID == nil || *taskBinID == "" {
					log.Printf("[DIAGNOSTIC] ❌ No bin_id on warehouse placement task")
					db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
					utils.RespondError(w, http.StatusBadRequest, "bin_id is required for warehouse deployment tasks")
					return
				}

				// Use task coordinates + address as the deployment destination
				var destLat, destLon float64
				var destAddr *string
				db.QueryRow(`SELECT latitude, longitude, address FROM route_tasks WHERE id = $1`, taskID).Scan(&destLat, &destLon, &destAddr)
				addrVal := ""
				if destAddr != nil {
					addrVal = *destAddr
				}

				_, err = db.Exec(`
					UPDATE bins
					SET status = 'active',
					    current_street = $1,
					    latitude = $2,
					    longitude = $3,
					    last_checked_at = $4,
					    updated_at = $4
					WHERE id = $5
				`, addrVal, destLat, destLon, now, *taskBinID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error redeploying warehouse bin: %v", err)
					db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to redeploy bin")
					return
				}
				log.Printf("[DIAGNOSTIC] ✅ Bin %s redeployed to active at (%f, %f)", *taskBinID, destLat, destLon)

				// Broadcast bin_redeployed event to managers
				if hub != nil {
					hub.BroadcastToRole("manager", websocket.Message{
						UserID: "",
						Data: map[string]interface{}{
							"type":     "bin_redeployed",
							"bin_id":   *taskBinID,
							"address":  addrVal,
							"status":   "active",
							"shift_id": shift.ID,
						},
					})
					log.Printf("[DIAGNOSTIC] 📡 Broadcast bin_redeployed event to managers")
				}
			} else {
				// ── POTENTIAL LOCATION PLACEMENT ──────────────────────────────────────
				// Create a brand new bin from a potential location
				if potentialLocationID == nil {
					log.Printf("[DIAGNOSTIC] ❌ Missing potential_location_id for potential_location placement")
					db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to retrieve placement location")
					return
				}

				log.Printf("[DIAGNOSTIC]    Potential Location ID: %s", *potentialLocationID)

				// Fetch potential location details
				var potentialLocation models.PotentialLocation
				err = db.Get(&potentialLocation, "SELECT * FROM potential_locations WHERE id = $1", *potentialLocationID)
				if err != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error fetching potential location: %v", err)
				} else {
					log.Printf("[DIAGNOSTIC]    Location: %s, %s %s", potentialLocation.Street, potentialLocation.City, potentialLocation.Zip)

					// Create new bin with pre-assigned bin_number
					newBinID := uuid.New().String()
					binInsertQuery := `
						INSERT INTO bins (
							id, bin_number, current_street, city, zip,
							latitude, longitude, status, fill_percentage,
							last_checked_at, created_by_user_id, created_at, updated_at
						) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
					`

					// Use driver-provided bin number (required)
					actualBinNumber := req.NewBinNumber
					log.Printf("[DIAGNOSTIC] Using driver-provided bin number: %d", actualBinNumber)
					if actualBinNumber == 0 {
						log.Printf("[DIAGNOSTIC] ❌ Driver did not provide a bin number")
						db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
						utils.RespondError(w, http.StatusBadRequest, "Bin number is required for placement tasks")
						return
					}

					// Check if this bin number is already taken
					var binAlreadyExists bool
					db.QueryRow(`SELECT EXISTS(SELECT 1 FROM bins WHERE bin_number = $1)`, actualBinNumber).Scan(&binAlreadyExists)
					if binAlreadyExists {
						log.Printf("[DIAGNOSTIC] ❌ Bin #%d already exists — rejecting duplicate", actualBinNumber)
						db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
						utils.RespondError(w, http.StatusConflict, fmt.Sprintf("Bin #%d already exists, please use a different number", actualBinNumber))
						return
					}

					_, err = db.Exec(
						binInsertQuery,
						newBinID,
						actualBinNumber,
						potentialLocation.Street,
						potentialLocation.City,
						potentialLocation.Zip,
						potentialLocation.Latitude,
						potentialLocation.Longitude,
						"active",
						0, // New bins start at 0% fill
						now,
						userClaims.UserID,
						now,
						now,
					)

					if err != nil {
						log.Printf("[DIAGNOSTIC] ❌ Error creating bin: %v", err)
						db.Exec(`UPDATE route_tasks SET is_completed = 0, completed_at = NULL, updated_at = $1 WHERE id = $2`, now, taskID)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to create bin, please try again")
						return
					} else {
						log.Printf("[DIAGNOSTIC] ✅ Created new Bin #%d (ID: %s)", actualBinNumber, newBinID)

						// Update potential_location record (mark as converted via shift)
						_, err = db.Exec(`
							UPDATE potential_locations
							SET converted_to_bin_id = $1,
								converted_at = $2,
								converted_by_user_id = $3,
								converted_via_shift_id = $4,
								updated_at = $5
							WHERE id = $6
						`, newBinID, now, userClaims.UserID, shift.ID, now, *potentialLocationID)

						if err != nil {
							log.Printf("[DIAGNOSTIC] ❌ Error updating potential_location: %v", err)
						} else {
							log.Printf("[DIAGNOSTIC] ✅ Potential location marked as converted (via shift %s)", shift.ID)

							// Broadcast WebSocket event to managers
							if hub != nil {
								binCreatedMsg := websocket.Message{
									UserID: "",
									Data: map[string]interface{}{
										"type":       "bin_created",
										"bin_id":     newBinID,
										"bin_number": actualBinNumber,
										"street":     potentialLocation.Street,
										"city":       potentialLocation.City,
										"zip":        potentialLocation.Zip,
										"status":     "active",
										"created_by": "driver_placement",
										"shift_id":   shift.ID,
									},
								}
								hub.BroadcastToRole("manager", binCreatedMsg)
								log.Printf("[DIAGNOSTIC] 📡 Broadcast bin_created event to managers")
							}

							// Also publish potential_location_converted via Centrifugo so the
							// manager dashboard removes this location from the selector in real-time
							if centrifugoClient != nil {
								plData := map[string]interface{}{
									"location_id": *potentialLocationID,
									"bin_id":      newBinID,
									"bin_number":  actualBinNumber,
									"street":      potentialLocation.Street,
									"city":        potentialLocation.City,
									"zip":         potentialLocation.Zip,
									"shift_id":    shift.ID,
								}
								if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "potential_location_converted", plData); pubErr != nil {
									log.Printf("[DIAGNOSTIC] Failed to publish potential_location_converted via Centrifugo: %v", pubErr)
								} else {
									log.Printf("[DIAGNOSTIC] Published potential_location_converted via Centrifugo (location_id: %s)", *potentialLocationID)
								}
							}
						}
					}
				}
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

		// Insert check record into checks table (only for bin-related stops, not warehouse)
		var checkID *int
		if req.BinID != "" && taskType != "warehouse_stop" {
			log.Printf("[DIAGNOSTIC] 📝 Inserting check record into checks table...")
			log.Printf("[DIAGNOSTIC] 💾 CHECKS TABLE INSERT - fill_percentage value:")
			if req.UpdatedFillPercentage != nil {
				log.Printf("[DIAGNOSTIC]    Inserting fill_percentage: %d%%", *req.UpdatedFillPercentage)
			} else {
				log.Printf("[DIAGNOSTIC]    Inserting fill_percentage: NULL")
			}

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
		} else {
			log.Printf("[DIAGNOSTIC] ⏭️  Skipping check record insert (warehouse stop or no bin_id)")
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

		// relocation_request is an operational note, not a location safety signal — skip zone creation
		if err == nil && bin.Latitude != nil && bin.Longitude != nil && *req.IncidentType != "relocation_request" {
			binIDCopy := req.BinID
			shiftIDCopy := shift.ID
			zoneName := fmt.Sprintf("%s - %s", bin.CurrentStreet, bin.City)
			driverShiftSource := "driver_shift"
			incidentID, incidentErr := createZoneAndIncident(
				db,
				centrifugoClient,
				*bin.Latitude, *bin.Longitude,
				zoneName,
				*req.IncidentType,
				&binIDCopy,
				userClaims.UserID,
				req.IncidentDescription,
				req.IncidentPhotoUrl,
				&shiftIDCopy,
				checkID,
				nil, nil,
				false,
				now,
				&driverShiftSource, // source
				nil,                // moveRequestID
			)
			if incidentErr != nil {
					log.Printf("[DIAGNOSTIC] ❌ Error creating zone/incident: %v", incidentErr)
				} else {
					createdIncidentID = &incidentID
					log.Printf("[DIAGNOSTIC] ✅ Incident created (ID: %s) and linked to check ID %d", incidentID, *checkID)
				}
			} else if err != nil {
				log.Printf("[DIAGNOSTIC] ⚠️  Could not create incident: failed to fetch bin")
			} else {
				log.Printf("[DIAGNOSTIC] ⚠️  Could not create incident: bin has no coordinates (lat: %v, lng: %v)", bin.Latitude, bin.Longitude)
			}

		// If the incident type is "missing", flip the bin status and broadcast in real-time
		if *req.IncidentType == "missing" {
			if _, updateErr := db.Exec(
				`UPDATE bins SET status = 'missing', updated_at = $1 WHERE id = $2`,
				now, req.BinID,
			); updateErr != nil {
				log.Printf("[DIAGNOSTIC] ⚠️  Failed to mark bin %s as missing: %v", req.BinID, updateErr)
			} else {
				log.Printf("[DIAGNOSTIC] 🔍 Bin %s marked as missing", req.BinID)
				if centrifugoClient != nil {
					var updatedBin models.Bin
					if fetchErr := db.Get(&updatedBin, "SELECT * FROM bins WHERE id = $1", req.BinID); fetchErr == nil {
						if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_updated", updatedBin); pubErr != nil {
							log.Printf("[DIAGNOSTIC] ⚠️  Centrifugo bin_updated publish failed: %v", pubErr)
						}
					}
				}
			}
		}
		}

		// Update shift completed_bins count (only for bin tasks, not warehouse stops)
		if taskType != "warehouse_stop" {
			log.Printf("🔄 Updating shift completed_bins count (shift_id=%s, task_type=%s)", shift.ID, taskType)
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
			log.Printf("✅ Shift completed_bins incremented")
		} else {
			log.Printf("⏭️  Skipping completed_bins increment for warehouse_stop task")
		}

		// Get updated shift
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)

		// Get updated bins list
		log.Printf("🔄 Fetching shift tasks via getShiftTasksWithDetails...")
		bins, err := getShiftTasksWithDetails(db, shift.ID)
		if err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			bins = []models.ShiftBinWithDetails{}
		}
		log.Printf("✅ Fetched %d tasks", len(bins))

		// Get updated tasks list (new task-based system)
		tasks, err := database.GetShiftTasks(db, shift.ID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v (using bins only)", err)
			tasks = []models.RouteTask{}
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
				"tasks":          tasks,
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

// ReoptimizeActiveShift re-optimizes an active shift's remaining tasks using current traffic
// skipGates: if true, skips time-based gates (for manager-initiated changes)
func ReoptimizeActiveShift(db *sqlx.DB, shiftID string, centrifugoClient *centrifugo.Client, skipGates bool) error {
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🔄 [REOPTIMIZE] Starting re-optimization for shift %s", shiftID)
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Step 1: Get shift details
	var shift models.Shift
	err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
	if err != nil {
		return fmt.Errorf("failed to get shift: %w", err)
	}

	if shift.Status != "active" {
		return fmt.Errorf("shift is not active (status: %s)", shift.Status)
	}

	// Step 2: Get remaining (uncompleted) route_tasks
	var tasks []models.RouteTask
	err = db.Select(&tasks, `
		SELECT * FROM route_tasks
		WHERE shift_id = $1 AND is_completed = 0
		ORDER BY sequence_order ASC
	`, shiftID)
	if err != nil {
		return fmt.Errorf("failed to get route tasks: %w", err)
	}

	remainingTasks := len(tasks)
	log.Printf("📊 [REOPTIMIZE] Found %d remaining tasks", remainingTasks)

	// Gate 1: Minimum tasks check
	minTasks := 5
	if skipGates {
		minTasks = 3 // More lenient for manager edits
	}

	if remainingTasks < minTasks {
		return fmt.Errorf("insufficient remaining tasks (%d < %d)", remainingTasks, minTasks)
	}

	// Gate 2: Time-based checks (only if skipGates is false)
	if !skipGates {
		shiftDuration := time.Since(time.Unix(shift.CreatedAt, 0)).Minutes()
		if shiftDuration < 30 {
			return fmt.Errorf("shift too new (%.0f min < 30 min)", shiftDuration)
		}
		log.Printf("✅ [REOPTIMIZE] Shift active for %.0f minutes", shiftDuration)
	} else {
		log.Printf("⏭️  [REOPTIMIZE] Skipping time gates (manager-initiated)")
	}

	// Step 3: Get warehouse location
	warehouseLocation := optimization.Location{
		ID:        "warehouse",
		Name:      "Warehouse",
		Latitude:  *shift.WarehouseLatitude,
		Longitude: *shift.WarehouseLongitude,
		Address:   *shift.WarehouseAddress,
	}

	// Step 4: Get driver's current location (use warehouse as fallback)
	driverStartLocation := warehouseLocation

	// Try to get driver's last GPS location
	var lastLocation struct {
		Latitude  *float64 `db:"latitude"`
		Longitude *float64 `db:"longitude"`
	}
	err = db.Get(&lastLocation, `
		SELECT latitude, longitude
		FROM driver_locations
		WHERE driver_id = $1
		ORDER BY timestamp DESC
		LIMIT 1
	`, shift.DriverID)

	if err == nil && lastLocation.Latitude != nil && lastLocation.Longitude != nil {
		driverStartLocation = optimization.Location{
			ID:        "driver-current",
			Name:      "Driver Current Location",
			Latitude:  *lastLocation.Latitude,
			Longitude: *lastLocation.Longitude,
			Address:   "Current GPS Position",
		}
		log.Printf("📍 [REOPTIMIZE] Using driver's current location: (%.6f, %.6f)",
			*lastLocation.Latitude, *lastLocation.Longitude)
	} else {
		log.Printf("⚠️  [REOPTIMIZE] No driver location found, using warehouse")
	}

	// Step 5: Build optimization request
	req := &optimization.RouteRequest{
		Vehicles:       make([]optimization.Vehicle, 1),
		Collections:    make([]optimization.Collection, 0),
		Placements:     make([]optimization.Placement, 0),
		MoveRequests:   make([]optimization.MoveRequest, 0),
		WarehouseStops: make([]optimization.WarehouseStop, 0),
	}

	capacity := 4
	if shift.TruckBinCapacity != nil {
		capacity = *shift.TruckBinCapacity
	}

	req.Vehicles[0] = optimization.Vehicle{
		ID:             fmt.Sprintf("truck-%s", shift.DriverID[:8]),
		Name:           fmt.Sprintf("Truck-%s", shift.DriverID[:8]),
		StartLocation:  driverStartLocation,
		EndLocation:    warehouseLocation,
		RoutingProfile: "mapbox/driving-traffic",
		Capacities: map[string]int{
			"bins": capacity,
		},
	}

	// Helper functions for nil-safe value extraction
	getIntValue := func(ptr *int) int {
		if ptr != nil {
			return *ptr
		}
		return 0
	}

	getStringValue := func(ptr *string) string {
		if ptr != nil {
			return *ptr
		}
		return ""
	}

	// Convert tasks to optimization format
	for _, task := range tasks {
		switch task.TaskType {
		case "collection":
			if task.BinID != nil && task.Latitude != 0 && task.Longitude != 0 {
				collection := optimization.Collection{
					ID:        task.ID,
					BinID:     *task.BinID,
					BinNumber: getIntValue(task.BinNumber),
					Location: optimization.Location{
						ID:        *task.BinID,
						Name:      fmt.Sprintf("Bin #%d", getIntValue(task.BinNumber)),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					Duration:       300,
					FillPercentage: getIntValue(task.FillPercentage),
				}
				req.Collections = append(req.Collections, collection)
			}

		case "placement":
			if task.PotentialLocationID != nil && task.Latitude != 0 && task.Longitude != 0 {
				placement := optimization.Placement{
					ID:                *task.PotentialLocationID,
					NewBinNumber:      getIntValue(task.NewBinNumber),
					WarehouseLocation: warehouseLocation,
					PlacementLocation: optimization.Location{
						ID:        *task.PotentialLocationID,
						Name:      fmt.Sprintf("Placement #%d", getIntValue(task.NewBinNumber)),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					PickupDuration:  60,
					DropoffDuration: 120,
				}
				req.Placements = append(req.Placements, placement)
			}

		case "pickup":
			if task.MoveRequestID != nil && task.Latitude != 0 && task.Longitude != 0 &&
				task.DestinationLatitude != nil && task.DestinationLongitude != nil {
				moveRequest := optimization.MoveRequest{
					ID:        *task.MoveRequestID,
					BinID:     getStringValue(task.BinID),
					BinNumber: getIntValue(task.BinNumber),
					PickupLocation: optimization.Location{
						ID:        fmt.Sprintf("%s-pickup", *task.MoveRequestID),
						Name:      fmt.Sprintf("Pickup #%d", getIntValue(task.BinNumber)),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					DropoffLocation: optimization.Location{
						ID:        fmt.Sprintf("%s-dropoff", *task.MoveRequestID),
						Name:      fmt.Sprintf("Dropoff #%d", getIntValue(task.BinNumber)),
						Latitude:  *task.DestinationLatitude,
						Longitude: *task.DestinationLongitude,
						Address:   getStringValue(task.DestinationAddress),
					},
					PickupDuration:  120,
					DropoffDuration: 120,
				}
				req.MoveRequests = append(req.MoveRequests, moveRequest)
			}
		}
	}

	log.Printf("📊 [REOPTIMIZE] Request: %d collections, %d placements, %d moves",
		len(req.Collections), len(req.Placements), len(req.MoveRequests))

	// Step 6: Call MapBox optimizer
	log.Printf("🚀 [REOPTIMIZE] Calling MapBox Optimization v2 API...")
	mapboxOptimizer := optimization.NewMapboxOptimizer()
	response, err := mapboxOptimizer.OptimizeRoute(req)
	if err != nil {
		return fmt.Errorf("MapBox optimization failed: %w", err)
	}

	if len(response.Routes) == 0 {
		return fmt.Errorf("MapBox returned no routes (dropped tasks: %v)", response.DroppedTasks)
	}

	route := response.Routes[0]
	log.Printf("✅ [REOPTIMIZE] Optimization complete: %d stops", len(route.Stops))

	// Step 7: Calculate old vs new completion time (time savings check)
	if !skipGates && len(tasks) > 0 {
		// This is a simplified check - in production you'd want more sophisticated comparison
		log.Printf("ℹ️  [REOPTIMIZE] Time savings check skipped (would need historical ETA data)")
	}

	// Step 8: Update route_tasks with new sequence_order
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback()

	taskIDToOriginal := make(map[string]models.RouteTask)
	for _, task := range tasks {
		taskIDToOriginal[task.ID] = task
	}

	sequenceOrder := 1
	for _, stop := range route.Stops {
		if stop.Type == optimization.StopTypeStart || stop.Type == optimization.StopTypeEnd {
			continue
		}

		// Find the corresponding task
		var taskID string
		if stop.CollectionID != "" {
			// It's a collection
			for _, origTask := range tasks {
				if origTask.TaskType == "collection" && origTask.ID == stop.CollectionID {
					taskID = origTask.ID
					break
				}
			}
		} else if stop.PlacementID != "" {
			// It's a placement
			for _, origTask := range tasks {
				if origTask.TaskType == "placement" && origTask.PotentialLocationID != nil {
					if stop.PlacementID == "placement-"+*origTask.PotentialLocationID {
						taskID = origTask.ID
						break
					}
				}
			}
		} else if stop.MoveRequestID != "" {
			// It's a pickup or dropoff
			for _, origTask := range tasks {
				if origTask.MoveRequestID != nil && stop.MoveRequestID == "move-"+*origTask.MoveRequestID {
					if (stop.Type == optimization.StopTypePickup && origTask.TaskType == "pickup") ||
						(stop.Type == optimization.StopTypeDropoff && origTask.TaskType == "dropoff") {
						taskID = origTask.ID
						break
					}
				}
			}
		}

		if taskID != "" {
			_, err = tx.Exec(`
				UPDATE route_tasks
				SET sequence_order = $1, updated_at = $2
				WHERE id = $3
			`, sequenceOrder, time.Now().Unix(), taskID)
			if err != nil {
				return fmt.Errorf("failed to update sequence_order: %w", err)
			}
			log.Printf("   Updated task %s → sequence_order = %d", taskID[:8], sequenceOrder)
			sequenceOrder++
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// Step 9: Notify driver via Centrifugo
	if centrifugoClient != nil {
		err = NotifyDriverOfRouteUpdate(
			db,
			centrifugoClient,
			shiftID,
			"route_reoptimized",
			map[string]interface{}{
				"tasks_reordered": remainingTasks,
				"reason":          "Automatic re-optimization with current traffic",
			},
		)
		if err != nil {
			log.Printf("⚠️  [REOPTIMIZE] Failed to notify driver: %v", err)
		} else {
			log.Printf("✅ [REOPTIMIZE] Notified driver of route changes")
		}
	}

	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("✅ [REOPTIMIZE] Re-optimization complete")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	return nil
}

// SkipTask marks a task as skipped with a required reason
// If skipping a pickup task, automatically skips the paired dropoff
func SkipTask(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
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

		// Increment shift completed_bins counter (only for bin tasks, not warehouse stops)
		binTasksSkipped := tasksSkipped
		if task.TaskType == "warehouse_stop" {
			binTasksSkipped = 0 // Warehouse stops don't count toward completed_bins
			log.Printf("⏭️  Main task is warehouse_stop - not counting toward completed_bins")
		}

		log.Printf("🔄 Updating shift completed_bins (skipping %d tasks, %d count toward completed_bins)", tasksSkipped, binTasksSkipped)
		_, err = tx.Exec(`
			UPDATE shifts
			SET completed_bins = completed_bins + $1,
				updated_at = $2
			WHERE id = $3
		`, binTasksSkipped, now, shift.ID)
		if err != nil {
			log.Printf("❌ Error updating shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift")
			return
		}
		log.Printf("✅ Shift updated for skip")

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

		// Get updated tasks list (new task-based system)
		tasks, err := database.GetShiftTasks(db, shift.ID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v (using bins only)", err)
			tasks = []models.RouteTask{}
		}

		// Calculate LOGICAL bin counts (treating pickup+dropoff as 1)
		logicalTotal, logicalCompleted := calculateLogicalBinCounts(bins)

		// Broadcast WebSocket update with bins
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": map[string]interface{}{
				"id":             shift.ID,
				"status":         shift.Status,
				"completed_bins": logicalCompleted,
				"total_bins":     logicalTotal,
				"bins":           bins,
				"tasks":          tasks,
			},
		})

		log.Printf("📡 WebSocket: Broadcasted shift update to driver %s", userClaims.UserID)

		// Re-optimize the shift after skipping task (skipGates=false for driver-initiated)
		if err := ReoptimizeActiveShift(db, shift.ID, nil, false); err != nil {
			log.Printf("⚠️  Failed to re-optimize shift after task skip: %v", err)
			// Don't fail the request if re-optimization fails
		} else {
			log.Printf("✅ Successfully re-optimized shift %s after task skip", shift.ID)
		}

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
func RemoveTasksFromShift(db *sqlx.DB, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/manager/shifts/:shift_id/tasks/remove")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "shift_id")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "Shift ID is required")
			return
		}

		var req struct {
			TaskIDs []string `json:"task_ids"`
			Reason  string   `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		if len(req.TaskIDs) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "At least one task ID is required")
			return
		}

		log.Printf("   Shift ID: %s", shiftID)
		log.Printf("   Task IDs: %v", req.TaskIDs)
		log.Printf("   Reason: %s", req.Reason)
		log.Printf("   Manager: %s (%s)", userClaims.Email, userClaims.UserID)

		// Get shift details
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err == sql.ErrNoRows {
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift")
			return
		}

		// Only allow removing tasks from ready or active shifts
		if shift.Status != "active" && shift.Status != "ready" {
			utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Can only remove tasks from ready or active shifts (current status: %s)", shift.Status))
			return
		}

		log.Printf("✅ Found shift (status: %s) for driver %s", shift.Status, shift.DriverID)

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		now := time.Now().Unix()
		removedCount := 0
		var removedTasks []models.RouteTask

		// Process each task
		for _, taskID := range req.TaskIDs {
			// Decode base64 task ID to UUID
			decodedBytes, decodeErr := base64.StdEncoding.DecodeString(taskID)
			actualTaskID := taskID // Default to original if decode fails
			if decodeErr == nil {
				actualTaskID = string(decodedBytes)
				log.Printf("🔓 Decoded task ID: %s -> %s", taskID, actualTaskID)
			} else {
				log.Printf("⚠️  Task ID %s is not base64, using as-is", taskID)
			}

			// Get task details
			var task models.RouteTask
			err = tx.Get(&task, `SELECT * FROM route_tasks WHERE id = $1 AND shift_id = $2`, actualTaskID, shiftID)
			if err == sql.ErrNoRows {
				log.Printf("⚠️  Task %s not found in shift %s, skipping", taskID, shiftID)
				continue
			}
			if err != nil {
				log.Printf("❌ Error fetching task %s: %v", taskID, err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch task")
				return
			}

			// Don't remove already completed tasks
			if task.IsCompleted == 1 {
				log.Printf("⚠️  Task %s already completed, skipping", taskID)
				continue
			}

			log.Printf("🗑️  Removing task: ID=%s, Type=%s, Seq=%d", task.ID, task.TaskType, task.SequenceOrder)

			// Mark task as deleted (soft delete for audit trail)
			_, err = tx.Exec(`
				UPDATE route_tasks
				SET is_deleted = true,
					deleted_at = $1,
					deleted_by = $2,
					deletion_reason = $3,
					updated_at = $1
				WHERE id = $4
			`, now, userClaims.UserID, req.Reason, actualTaskID)
			if err != nil {
				log.Printf("❌ Error marking task as deleted: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to remove task")
				return
			}

			// Unassign underlying resources
			switch task.TaskType {
			case models.TaskTypePickup, models.TaskTypeDropoff:
				// Unassign move request
				if task.MoveRequestID != nil {
					log.Printf("   Unassigning move request %s", *task.MoveRequestID)
					_, err = tx.Exec(`
						UPDATE bin_move_requests
						SET assigned_shift_id = NULL,
							assigned_user_id = NULL,
							updated_at = $1
						WHERE id = $2
					`, now, *task.MoveRequestID)
					if err != nil {
						log.Printf("❌ Error unassigning move request: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign move request")
						return
					}
				}

			case models.TaskTypePlacement:
				// Unassign potential location
				if task.PotentialLocationID != nil {
					log.Printf("   Unassigning potential location %s", *task.PotentialLocationID)
					_, err = tx.Exec(`
						UPDATE potential_locations
						SET assigned_shift_id = NULL,
							updated_at = $1
						WHERE id = $2
					`, now, *task.PotentialLocationID)
					if err != nil {
						log.Printf("❌ Error unassigning potential location: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign potential location")
						return
					}
				}

			case models.TaskTypeCollection:
				// For collection tasks, just mark as removed
				// The bin itself stays in the system
				binID := "unknown"
				if task.BinID != nil {
					binID = *task.BinID
				}
				log.Printf("   Collection task removed (bin %s remains in system)", binID)
			}

			removedTasks = append(removedTasks, task)
			removedCount++
		}

		if removedCount == 0 {
			utils.RespondError(w, http.StatusBadRequest, "No valid tasks to remove")
			return
		}

		log.Printf("✅ Marked %d tasks as removed", removedCount)

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit changes")
			return
		}

		log.Printf("✅ Transaction committed")

		// Refresh shift data
		err = db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err != nil {
			log.Printf("⚠️  Error refreshing shift: %v", err)
		}

		// Get updated task list
		tasks, err := database.GetShiftTasks(db, shiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v", err)
			tasks = []models.RouteTask{}
		}

		// Publish Centrifugo event to driver's shift channel
		if centrifugoClient != nil {
			eventData := map[string]interface{}{
				"type":          "task_removed",
				"shift_id":      shiftID,
				"removed_count": removedCount,
				"reason":        req.Reason,
				"manager_name":  userClaims.Email,
			}

			channel := fmt.Sprintf("shift:updates:%s", shiftID)
			if pubErr := centrifugoClient.PublishToChannel(r.Context(), channel, eventData); pubErr != nil {
				log.Printf("⚠️  Failed to publish task removal to Centrifugo: %v", pubErr)
			} else {
				log.Printf("📡 Published task_removed event to %s", channel)
			}
		}

		// Re-optimize the shift after removing tasks (skip gates since manager-initiated)
		if err := ReoptimizeActiveShift(db, shiftID, centrifugoClient, true); err != nil {
			log.Printf("⚠️  Failed to re-optimize shift after task removal: %v", err)
			// Don't fail the request if re-optimization fails
		} else {
			log.Printf("✅ Successfully re-optimized shift %s after removing %d tasks", shiftID, removedCount)
		}

		// Return success response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":       true,
			"removed_count": removedCount,
			"message":       fmt.Sprintf("%d task(s) removed from shift successfully", removedCount),
			"shift": map[string]interface{}{
				"id":     shift.ID,
				"status": shift.Status,
				"tasks":  tasks,
			},
		})
	}
}

// UpdateShift comprehensively updates a shift (time, driver, add/remove tasks)
// PATCH /api/manager/shifts/:id
func UpdateShift(db *sqlx.DB, centrifugoClient *centrifugo.Client, fcmService *services.FCMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: PATCH /api/manager/shifts/:id")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "id")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "Shift ID is required")
			return
		}

		// Request structure
		var req struct {
			// Shift details (optional - only update if provided)
			StartTime *int64  `json:"start_time,omitempty"`
			EndTime   *int64  `json:"end_time,omitempty"`
			DriverID  *string `json:"driver_id,omitempty"`
			RouteID   *string `json:"route_id,omitempty"`

			// Task modifications
			AddTasks      []AddTaskRequest `json:"add_tasks,omitempty"`
			RemoveTaskIDs []string         `json:"remove_task_ids,omitempty"`

			// Flags
			Reoptimize bool   `json:"reoptimize"` // Default: false (will be set to true if tasks change)
			Reason     string `json:"reason"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("   Shift ID: %s", shiftID)
		log.Printf("   Manager: %s (%s)", userClaims.Email, userClaims.UserID)
		log.Printf("   Changes requested:")
		if req.StartTime != nil {
			log.Printf("      - Start time: %d", *req.StartTime)
		}
		if req.EndTime != nil {
			log.Printf("      - End time: %d", *req.EndTime)
		}
		if req.DriverID != nil {
			log.Printf("      - Driver ID: %s", *req.DriverID)
		}
		if req.RouteID != nil {
			log.Printf("      - Route ID: %s", *req.RouteID)
		}
		log.Printf("      - Tasks to add: %d", len(req.AddTasks))
		log.Printf("      - Tasks to remove: %d", len(req.RemoveTaskIDs))
		log.Printf("      - Reason: %s", req.Reason)

		// Debug: Log each task being added
		for i, task := range req.AddTasks {
			log.Printf("      [Task %d] Type: %s", i+1, task.TaskType)
			if task.BinID != nil {
				log.Printf("      [Task %d]   Bin ID: %s", i+1, *task.BinID)
			}
			if task.PotentialLocationID != nil {
				log.Printf("      [Task %d]   Potential Location ID: %s", i+1, *task.PotentialLocationID)
			}
		}

		// Get current shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err == sql.ErrNoRows {
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift")
			return
		}

		// Only allow editing ready or active shifts
		if shift.Status != "ready" && shift.Status != "active" {
			utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Can only edit ready or active shifts (current status: %s)", shift.Status))
			return
		}

		log.Printf("✅ Found shift (status: %s) for driver %s", shift.Status, shift.DriverID)

		// Track changes for event payload
		changes := make(map[string]interface{})
		changes["tasks_added"] = 0
		changes["tasks_removed"] = 0
		changes["driver_changed"] = false
		changes["time_changed"] = false
		changes["route_reoptimized"] = false

		oldDriverID := shift.DriverID

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		now := time.Now().Unix()

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 1: Update shift details (time, driver, route)
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		updateFields := []string{"updated_at = $1"}
		updateArgs := []interface{}{now}
		argIndex := 2

		if req.StartTime != nil {
			updateFields = append(updateFields, fmt.Sprintf("start_time = $%d", argIndex))
			updateArgs = append(updateArgs, *req.StartTime)
			argIndex++
			changes["time_changed"] = true
		}

		if req.EndTime != nil {
			updateFields = append(updateFields, fmt.Sprintf("end_time = $%d", argIndex))
			updateArgs = append(updateArgs, *req.EndTime)
			argIndex++
			changes["time_changed"] = true
		}

		if req.DriverID != nil && *req.DriverID != shift.DriverID {
			updateFields = append(updateFields, fmt.Sprintf("driver_id = $%d", argIndex))
			updateArgs = append(updateArgs, *req.DriverID)
			argIndex++
			changes["driver_changed"] = true
			log.Printf("🔄 Driver reassignment: %s → %s", oldDriverID, *req.DriverID)
		}

		if req.RouteID != nil {
			updateFields = append(updateFields, fmt.Sprintf("route_id = $%d", argIndex))
			updateArgs = append(updateArgs, *req.RouteID)
			argIndex++
		}

		if len(updateFields) > 1 { // More than just updated_at
			updateArgs = append(updateArgs, shiftID)
			query := fmt.Sprintf("UPDATE shifts SET %s WHERE id = $%d", strings.Join(updateFields, ", "), argIndex)
			_, err = tx.Exec(query, updateArgs...)
			if err != nil {
				log.Printf("❌ Error updating shift: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift")
				return
			}
			log.Printf("✅ Updated shift details")
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 2: Remove tasks (if any)
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		removedCount := 0
		if len(req.RemoveTaskIDs) > 0 {
			log.Printf("🗑️  Removing %d tasks...", len(req.RemoveTaskIDs))

			for _, taskID := range req.RemoveTaskIDs {
				// Decode base64 task ID to UUID
				decodedBytes, decodeErr := base64.StdEncoding.DecodeString(taskID)
				actualTaskID := taskID
				if decodeErr == nil {
					actualTaskID = string(decodedBytes)
				}

				// Get task details
				var task models.RouteTask
				err = tx.Get(&task, `SELECT * FROM route_tasks WHERE id = $1 AND shift_id = $2`, actualTaskID, shiftID)
				if err == sql.ErrNoRows {
					log.Printf("⚠️  Task %s not found, skipping", taskID)
					continue
				}
				if err != nil {
					log.Printf("❌ Error fetching task: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch task")
					return
				}

				// Don't remove completed tasks
				if task.IsCompleted == 1 {
					log.Printf("⚠️  Task %s already completed, skipping", taskID)
					continue
				}

				log.Printf("   Removing task: %s (type=%s)", task.ID, task.TaskType)

				// Mark as deleted
				_, err = tx.Exec(`
					UPDATE route_tasks
					SET is_deleted = true,
						deleted_at = $1,
						deleted_by = $2,
						deletion_reason = $3,
						updated_at = $1
					WHERE id = $4
				`, now, userClaims.UserID, req.Reason, actualTaskID)
				if err != nil {
					log.Printf("❌ Error marking task as deleted: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to remove task")
					return
				}

				// Unassign resources based on task type
				switch task.TaskType {
				case models.TaskTypePickup, models.TaskTypeDropoff:
					if task.MoveRequestID != nil {
						_, err = tx.Exec(`
							UPDATE bin_move_requests
							SET assigned_shift_id = NULL,
								assigned_user_id = NULL,
								updated_at = $1
							WHERE id = $2
						`, now, *task.MoveRequestID)
						if err != nil {
							log.Printf("❌ Error unassigning move request: %v", err)
							utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign move request")
							return
						}
					}
				case models.TaskTypePlacement:
					if task.PotentialLocationID != nil {
						_, err = tx.Exec(`
							UPDATE potential_locations
							SET assigned_shift_id = NULL,
								updated_at = $1
							WHERE id = $2
						`, now, *task.PotentialLocationID)
						if err != nil {
							log.Printf("❌ Error unassigning potential location: %v", err)
							utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign potential location")
							return
						}
					}
				}

				removedCount++
			}

			changes["tasks_removed"] = removedCount
			log.Printf("✅ Removed %d tasks", removedCount)
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 3: Add new tasks (if any)
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		addedCount := 0
		if len(req.AddTasks) > 0 {
			log.Printf("➕ Adding %d tasks...", len(req.AddTasks))

			for _, addReq := range req.AddTasks {
				newTaskID := uuid.New().String()
				log.Printf("   Creating task: type=%s, id=%s", addReq.TaskType, newTaskID)

				// Get next sequence order
				var maxSeq sql.NullInt64
				err = tx.Get(&maxSeq, `
					SELECT MAX(sequence_order)
					FROM route_tasks
					WHERE shift_id = $1 AND is_deleted = false
				`, shiftID)
				if err != nil && err != sql.ErrNoRows {
					log.Printf("❌ Error getting max sequence: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to get sequence order")
					return
				}

				nextSeq := 1
				if maxSeq.Valid {
					nextSeq = int(maxSeq.Int64) + 1
				}

				// Build task based on type
				var task models.RouteTask
				task.ID = newTaskID
				task.ShiftID = shiftID
				task.TaskType = models.TaskType(addReq.TaskType)
				task.SequenceOrder = nextSeq
				task.IsCompleted = 0
				task.CreatedAt = now
				task.UpdatedAt = &now

				switch addReq.TaskType {
				case "collection":
					if addReq.BinID == nil {
						utils.RespondError(w, http.StatusBadRequest, "bin_id required for collection task")
						return
					}

					log.Printf("🔍 [SHIFT UPDATE] Looking up bin_id: %s", *addReq.BinID)

					// Fetch bin details
					var bin struct {
						ID            string   `db:"id"`
						BinNumber     int      `db:"bin_number"`
						Latitude      float64  `db:"latitude"`
						Longitude     float64  `db:"longitude"`
						CurrentStreet string   `db:"current_street"`
						City          string   `db:"city"`
						ZipCode       string   `db:"zip_code"`
						FillPercentage int     `db:"fill_percentage"`
					}
					err = tx.Get(&bin, `SELECT id, bin_number, latitude, longitude, current_street, city, zip_code, fill_percentage FROM bins WHERE id = $1`, *addReq.BinID)
					if err != nil {
						log.Printf("❌ [SHIFT UPDATE] Error fetching bin_id=%s: %v", *addReq.BinID, err)
						if err == sql.ErrNoRows {
							log.Printf("❌ [SHIFT UPDATE] Bin not found in database: %s", *addReq.BinID)
						}
						utils.RespondError(w, http.StatusBadRequest, "Bin not found")
						return
					}

					log.Printf("✅ [SHIFT UPDATE] Found bin #%d at %s", bin.BinNumber, bin.CurrentStreet)

					task.BinID = addReq.BinID
					task.BinNumber = &bin.BinNumber
					task.Latitude = bin.Latitude
					task.Longitude = bin.Longitude
					address := fmt.Sprintf("%s, %s %s", bin.CurrentStreet, bin.City, bin.ZipCode)
					task.Address = &address
					task.FillPercentage = &bin.FillPercentage

				case "placement":
					if addReq.PotentialLocationID == nil {
						utils.RespondError(w, http.StatusBadRequest, "potential_location_id required for placement task")
						return
					}
					// Fetch potential location details
					var potLoc struct {
						ID        string  `db:"id"`
						Latitude  float64 `db:"latitude"`
						Longitude float64 `db:"longitude"`
						Address   string  `db:"address"`
					}
					err = tx.Get(&potLoc, `SELECT id, latitude, longitude, address FROM potential_locations WHERE id = $1`, *addReq.PotentialLocationID)
					if err != nil {
						log.Printf("❌ Error fetching potential location: %v", err)
						utils.RespondError(w, http.StatusBadRequest, "Potential location not found")
						return
					}

					task.PotentialLocationID = addReq.PotentialLocationID
					task.Latitude = potLoc.Latitude
					task.Longitude = potLoc.Longitude
					task.Address = &potLoc.Address

					// Mark as assigned
					_, err = tx.Exec(`UPDATE potential_locations SET assigned_shift_id = $1, updated_at = $2 WHERE id = $3`,
						shiftID, now, *addReq.PotentialLocationID)
					if err != nil {
						log.Printf("❌ Error assigning potential location: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to assign potential location")
						return
					}

				case "pickup", "dropoff":
					if addReq.MoveRequestID == nil {
						utils.RespondError(w, http.StatusBadRequest, "move_request_id required for pickup/dropoff task")
						return
					}
					// Fetch move request details
					var moveReq struct {
						ID         string  `db:"id"`
						BinID      string  `db:"bin_id"`
						Latitude   *float64 `db:"latitude"`
						Longitude  *float64 `db:"longitude"`
						Address    *string  `db:"address"`
						DestLatitude  *float64 `db:"destination_latitude"`
						DestLongitude *float64 `db:"destination_longitude"`
						DestAddress   *string  `db:"destination_address"`
					}
					err = tx.Get(&moveReq, `SELECT id, bin_id, latitude, longitude, address, destination_latitude, destination_longitude, destination_address FROM bin_move_requests WHERE id = $1`, *addReq.MoveRequestID)
					if err != nil {
						log.Printf("❌ Error fetching move request: %v", err)
						utils.RespondError(w, http.StatusBadRequest, "Move request not found")
						return
					}

					task.MoveRequestID = addReq.MoveRequestID
					task.BinID = &moveReq.BinID

					if addReq.TaskType == "pickup" {
						if moveReq.Latitude != nil && moveReq.Longitude != nil {
							task.Latitude = *moveReq.Latitude
							task.Longitude = *moveReq.Longitude
						}
						if moveReq.Address != nil {
							task.Address = moveReq.Address
						}
					} else { // dropoff
						if moveReq.DestLatitude != nil && moveReq.DestLongitude != nil {
							task.Latitude = *moveReq.DestLatitude
							task.Longitude = *moveReq.DestLongitude
						}
						if moveReq.DestAddress != nil {
							task.DestinationAddress = moveReq.DestAddress
						}
					}

					// Mark as assigned
					_, err = tx.Exec(`UPDATE bin_move_requests SET assigned_shift_id = $1, assigned_user_id = $2, updated_at = $3 WHERE id = $4`,
						shiftID, shift.DriverID, now, *addReq.MoveRequestID)
					if err != nil {
						log.Printf("❌ Error assigning move request: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to assign move request")
						return
					}

				default:
					utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid task type: %s", addReq.TaskType))
					return
				}

				// Insert task
				_, err = tx.Exec(`
					INSERT INTO route_tasks (
						id, shift_id, task_type, bin_id, potential_location_id, move_request_id,
						bin_number, latitude, longitude, address, destination_address,
						fill_percentage, sequence_order, is_completed, created_at, updated_at
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
				`, task.ID, task.ShiftID, task.TaskType, task.BinID, task.PotentialLocationID, task.MoveRequestID,
					task.BinNumber, task.Latitude, task.Longitude, task.Address, task.DestinationAddress,
					task.FillPercentage, task.SequenceOrder, task.IsCompleted, task.CreatedAt, task.UpdatedAt)
				if err != nil {
					log.Printf("❌ Error inserting task: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to add task")
					return
				}

				addedCount++
			}

			changes["tasks_added"] = addedCount
			log.Printf("✅ Added %d tasks", addedCount)
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit changes")
			return
		}

		log.Printf("✅ Transaction committed")

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 4: Re-optimize if tasks changed or driver changed
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		shouldReoptimize := req.Reoptimize || addedCount > 0 || removedCount > 0 || changes["driver_changed"].(bool)

		if shouldReoptimize && shift.Status == "active" {
			log.Printf("🔄 Re-optimizing route (tasks changed or driver reassigned)...")

			// Update shift reference if driver changed
			if req.DriverID != nil {
				shift.DriverID = *req.DriverID
			}

			err = ReoptimizeActiveShift(db, shiftID, centrifugoClient, true)
			if err != nil {
				log.Printf("⚠️  Failed to re-optimize: %v", err)
				// Don't fail the request
			} else {
				changes["route_reoptimized"] = true
				log.Printf("✅ Route re-optimized")
			}
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 5: Publish Centrifugo events
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		if centrifugoClient != nil {
			// If driver changed, notify old driver
			if changes["driver_changed"].(bool) {
				oldDriverChannel := fmt.Sprintf("shift:updates:%s", shiftID)
				reassignedEvent := map[string]interface{}{
					"type":   "shift_reassigned",
					"reason": "Shift has been reassigned to another driver",
				}
				if pubErr := centrifugoClient.PublishToChannel(r.Context(), oldDriverChannel, reassignedEvent); pubErr != nil {
					log.Printf("⚠️  Failed to notify old driver: %v", pubErr)
				} else {
					log.Printf("📡 Published shift_reassigned event to old driver")
				}
			}

			// Publish shift_edited event to current/new driver
			newDriverChannel := fmt.Sprintf("shift:updates:%s", shiftID)
			editedEvent := map[string]interface{}{
				"type":    "shift_edited",
				"shift_id": shiftID,
				"changes":  changes,
				"reason":   req.Reason,
				"manager_name": userClaims.Email,
			}
			if pubErr := centrifugoClient.PublishToChannel(r.Context(), newDriverChannel, editedEvent); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_edited event: %v", pubErr)
			} else {
				log.Printf("📡 Published shift_edited event")
			}
		}

		// Send FCM notifications
		if fcmService != nil && changes["driver_changed"].(bool) {
			// Notify old driver
			var oldDriverToken models.FCMToken
			if tokenErr := db.Get(&oldDriverToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, oldDriverID); tokenErr == nil {
				go fcmService.SendShiftUpdateNotification(oldDriverToken.Token, shiftID, "reassigned")
				log.Printf("📱 Sent FCM notification to old driver: %s", oldDriverID)
			}

			// Notify new driver
			if req.DriverID != nil {
				var newDriverToken models.FCMToken
				if tokenErr := db.Get(&newDriverToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, *req.DriverID); tokenErr == nil {
					go fcmService.SendShiftUpdateNotification(newDriverToken.Token, shiftID, "assigned")
					log.Printf("📱 Sent FCM notification to new driver: %s", *req.DriverID)
				}
			}
		}

		// Get updated shift
		err = db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err != nil {
			log.Printf("⚠️  Error refreshing shift: %v", err)
		}

		// Get updated tasks
		tasks, err := database.GetShiftTasks(db, shiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v", err)
			tasks = []models.RouteTask{}
		}

		// Return response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "Shift updated successfully",
			"changes": changes,
			"shift": map[string]interface{}{
				"id":     shift.ID,
				"status": shift.Status,
				"driver_id": shift.DriverID,
				"tasks":  tasks,
			},
		})

		log.Printf("✅ Shift updated successfully")
		log.Printf("   Changes: %+v", changes)
	}
}

// AddTaskRequest represents a request to add a task to a shift
type AddTaskRequest struct {
	TaskType            string  `json:"task_type"` // collection, placement, pickup, dropoff, warehouse_stop
	BinID               *string `json:"bin_id,omitempty"`
	PotentialLocationID *string `json:"potential_location_id,omitempty"`
	MoveRequestID       *string `json:"move_request_id,omitempty"`
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
		bins, err := getShiftTasksWithDetails(db, shiftID)
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

// getShiftTasksWithDetails fetches shift tasks with full details
// ONLY uses route_tasks table (new unified task system)
func getShiftTasksWithDetails(db *sqlx.DB, shiftID string) ([]models.ShiftBinWithDetails, error) {
	// Query route_tasks table with enhanced fields for all task types
	query := `
		SELECT
			rt.id as id,
			rt.shift_id,
			COALESCE(rt.bin_id, '') as bin_id,
			rt.sequence_order,
			rt.is_completed,
			rt.completed_at,
			rt.updated_fill_percentage,
			rt.created_at,
			COALESCE(b.bin_number, 0) as bin_number,
			COALESCE(rt.address, '') as current_street,
			COALESCE(b.city, '') as city,
			COALESCE(b.zip, '') as zip,
			COALESCE(b.fill_percentage, 0) as fill_percentage,
			rt.latitude,
			rt.longitude,
			rt.task_type as stop_type,
			rt.move_request_id,
			rt.address as original_address,
			rt.destination_address as new_address,
			rt.move_type,
			rt.potential_location_id,
			rt.new_bin_number,
			rt.warehouse_action,
			rt.bins_to_load,
			rt.skipped,
			rt.task_data
		FROM route_tasks rt
		LEFT JOIN bins b ON rt.bin_id = b.id
		WHERE rt.shift_id = $1
		ORDER BY rt.sequence_order ASC`

	var tasks []models.ShiftBinWithDetails
	err := db.Select(&tasks, query, shiftID)
	if err != nil {
		return nil, err
	}

	log.Printf("📦 Loaded %d tasks from route_tasks table (all types)", len(tasks))
	return tasks, nil
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

		// DEPRECATED: This endpoint no longer creates tasks.
		// Use POST /api/manager/shifts/with-tasks (CreateShiftWithTasks) instead.
		// shift_bins table has been removed in favor of route_tasks.
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
func UpdateLocation(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		// Only drivers can post location
		if userClaims.Role != "driver" {
			utils.RespondError(w, http.StatusForbidden, "Only drivers can post location")
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

		// Validate coordinates
		if req.Latitude < -90 || req.Latitude > 90 || req.Longitude < -180 || req.Longitude > 180 {
			log.Printf("❌ Invalid coordinates: lat=%.6f, lng=%.6f", req.Latitude, req.Longitude)
			utils.RespondError(w, http.StatusBadRequest, "Invalid coordinates")
			return
		}

		log.Printf("📍 Location update from driver %s: (%.6f, %.6f)", userClaims.UserID, req.Latitude, req.Longitude)

		// Default accuracy to 100m if not provided
		accuracyValue := 100.0
		if req.Accuracy != nil {
			accuracyValue = *req.Accuracy
		}

		// Step 1: Snap to roads using OSRM (if hub has roadsClient and accuracy > 15m)
		snappedLat := req.Latitude
		snappedLng := req.Longitude

		if hub != nil && hub.GetRoadsClient() != nil {
			roadsClient := hub.GetRoadsClient()
			newLat, newLng, err := roadsClient.SnapToRoad(req.Latitude, req.Longitude, accuracyValue)
			if err == nil && (newLat != req.Latitude || newLng != req.Longitude) {
				snappedLat = newLat
				snappedLng = newLng
				log.Printf("🗺️  Snapped to road: (%.6f, %.6f) → (%.6f, %.6f)", req.Latitude, req.Longitude, snappedLat, snappedLng)
			}
		}

		// Step 2: Update driver_current_location (UPSERT for shift start validation)
		currentLocationQuery := `
			INSERT INTO driver_current_location (
				driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp, is_connected, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, EXTRACT(EPOCH FROM NOW())::BIGINT)
			ON CONFLICT (driver_id)
			DO UPDATE SET
				latitude = EXCLUDED.latitude,
				longitude = EXCLUDED.longitude,
				heading = EXCLUDED.heading,
				speed = EXCLUDED.speed,
				accuracy = EXCLUDED.accuracy,
				shift_id = EXCLUDED.shift_id,
				timestamp = EXCLUDED.timestamp,
				is_connected = TRUE,
				updated_at = EXTRACT(EPOCH FROM NOW())::BIGINT
			RETURNING updated_at
		`

		var updatedAt int64
		err := db.QueryRow(
			currentLocationQuery,
			userClaims.UserID,
			req.Latitude,  // Store ORIGINAL GPS for audit
			req.Longitude, // Store ORIGINAL GPS for audit
			req.Heading,
			req.Speed,
			req.Accuracy,
			req.ShiftID,
			req.Timestamp,
		).Scan(&updatedAt)

		if err != nil {
			log.Printf("❌ Error updating driver_current_location: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to save location")
			return
		}

		log.Printf("✅ Updated driver_current_location (original GPS for audit)")

		// Step 3: Insert into driver_locations (historical log - optional)
		// Note: This table may not exist in your schema, commenting out for now
		// historicalQuery := `
		// 	INSERT INTO driver_locations (
		// 		driver_id, latitude, longitude, heading, speed, accuracy, shift_id, timestamp
		// 	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		// `
		// _, _ = db.Exec(historicalQuery, userClaims.UserID, req.Latitude, req.Longitude, req.Heading, req.Speed, req.Accuracy, req.ShiftID, req.Timestamp)

		// Step 4: Publish SNAPPED location to Centrifugo for managers
		if centrifugoClient != nil {
			// Convert pointer fields to values (use 0 if nil)
			heading := 0.0
			if req.Heading != nil {
				heading = *req.Heading
			}
			speed := 0.0
			if req.Speed != nil {
				speed = *req.Speed
			}
			accuracy := accuracyValue

			locationData := centrifugo.DriverLocation{
				Latitude:  snappedLat, // SNAPPED coordinates for display
				Longitude: snappedLng, // SNAPPED coordinates for display
				Heading:   heading,
				Speed:     speed,
				Accuracy:  accuracy,
				Timestamp: req.Timestamp,
			}

			err := centrifugoClient.PublishDriverLocation(r.Context(), userClaims.UserID, locationData)
			if err != nil {
				log.Printf("⚠️  Failed to publish to Centrifugo: %v", err)
				// Don't fail the request - location is already saved to DB
			} else {
				log.Printf("📤 Published snapped location to Centrifugo: driver:location:%s", userClaims.UserID)
			}
		}

		// Step 5: Broadcast SNAPPED location to managers via legacy WebSocket
		locationUpdate := map[string]interface{}{
			"type": "driver_location_update",
			"data": map[string]interface{}{
				"driver_id":  userClaims.UserID,
				"latitude":   snappedLat, // SNAPPED for display
				"longitude":  snappedLng, // SNAPPED for display
				"heading":    req.Heading,
				"speed":      req.Speed,
				"accuracy":   req.Accuracy,
				"shift_id":   req.ShiftID,
				"timestamp":  req.Timestamp,
				"updated_at": updatedAt,
			},
		}

		hub.BroadcastToRole("admin", locationUpdate)
		// log.Printf("📤 Broadcasted snapped location to managers via WebSocket")

		// Return success response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":    true,
			"updated_at": updatedAt,
			"snapped": map[string]interface{}{
				"latitude":  snappedLat,
				"longitude": snappedLng,
			},
		})
	}
}

// getFloat64Value safely extracts value from pointer or returns 0
func getFloat64Value(val *float64) float64 {
	if val == nil {
		return 0
	}
	return *val
}

// GetAllDrivers returns all drivers regardless of shift status
// Drivers with active shifts will show their current shift info
// Drivers without active shifts will show status as 'inactive'
// Location data comes from Redis (real-time current position)
// GET /api/manager/drivers
func GetAllDrivers(db *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("📋 GetAllDrivers: Fetching all drivers...")

		ctx := context.Background()

		// 1. Get all driver locations from Redis
		var locations map[string]string
		if redisClient != nil {
			var err error
			locations, err = redisClient.GetAllDriverLocations(ctx)
			if err != nil {
				log.Printf("⚠️ Redis error (non-fatal): %v", err)
				locations = make(map[string]string)
			} else {
				log.Printf("📍 Found %d drivers with location data in Redis", len(locations))
			}
		} else {
			locations = make(map[string]string)
		}

		// 2. Query database for all drivers
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
				s.updated_at
			FROM users u
			LEFT JOIN shifts s ON u.id = s.driver_id AND s.status IN ('ready', 'active', 'paused')
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
			)
			if err != nil {
				log.Printf("❌ Row scan error: %v", err)
				continue
			}

			// 3. Get location from Redis if available
			if locationJSON, ok := locations[driver.DriverID]; ok {
				var location LocationData
				if err := json.Unmarshal([]byte(locationJSON), &location); err == nil {
					driver.CurrentLocation = &DriverLocation{
						Latitude:  location.Latitude,
						Longitude: location.Longitude,
					}

					// Calculate location age
					locationAge := time.Now().Unix() - (location.Timestamp / 1000)
					t := location.Timestamp / 1000
					driver.UpdatedAt = &t

					log.Printf("   📍 %s: has location (age=%ds)", driver.DriverName, locationAge)
				}
			}

			// 4. Set shift-related fields if driver has an active shift
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
			} else {
				// No active shift - driver is inactive
				driver.Status = "inactive"
				driver.TotalBins = 0
				driver.CompletedBins = 0
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
func detectAndMergeZones(db *sqlx.DB, centrifugoClient *centrifugo.Client, zoneID string, now int64) error {
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

			// Broadcast merge events via Centrifugo
			if centrifugoClient != nil {
				ctx := context.Background()

				// zone_merged: tells frontend to move consumed zone to resolved and update surviving
				mergedPayload := map[string]string{
					"consumed_zone_id":  secondaryZone.ID,
					"surviving_zone_id": primaryZone.ID,
				}
				if pubErr := centrifugoClient.PublishCompanyEvent(ctx, "zone_merged", mergedPayload); pubErr != nil {
					log.Printf("[ZONE MERGE] ⚠️  Centrifugo zone_merged publish failed: %v", pubErr)
				}

				// zone_updated: fetch the updated primary zone and broadcast full response
				var updatedPrimary models.NoGoZone
				if fetchErr := db.Get(&updatedPrimary, "SELECT * FROM no_go_zones WHERE id = $1", primaryZone.ID); fetchErr == nil {
					if pubErr := centrifugoClient.PublishCompanyEvent(ctx, "zone_updated", updatedPrimary.ToResponse()); pubErr != nil {
						log.Printf("[ZONE MERGE] ⚠️  Centrifugo zone_updated publish failed: %v", pubErr)
					}
				} else {
					log.Printf("[ZONE MERGE] ⚠️  Could not fetch updated primary zone for broadcast: %v", fetchErr)
				}
			}
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
	TaskID                string     `json:"task_id"`
	BinID                 string  `json:"bin_id"`
	UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty"`
	PhotoUrl              *string `json:"photo_url,omitempty"`
	MoveRequestID         *string `json:"move_request_id,omitempty"` // Links check to move request
	NewBinNumber          int     `json:"new_bin_number"`            // Required for placements (not used for moves)
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

	// Log history: move request completed by driver
	if moveRequest.AssignedUserID != nil {
		var driverName string
		err = db.Get(&driverName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID)
		if err != nil {
			log.Printf("Warning: Failed to fetch driver name for history: %v", err)
			driverName = "Unknown Driver"
		}
		err = helpers.LogMoveRequestCompleted(db, moveRequest.ID, *moveRequest.AssignedUserID, driverName)
		if err != nil {
			log.Printf("Warning: Failed to log move request completion: %v", err)
		}
	} else {
		log.Printf("Warning: Move request completed without assigned user ID")
	}

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

	} else if moveRequest.MoveType == "relocation" || moveRequest.MoveType == "redeployment" {
		// Update bin location to new coordinates
		log.Printf("[MOVE]    → Relocating bin to new address (type: %s)", moveRequest.MoveType)
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

		// If this was a warehouse redeployment to a potential location, mark location as converted
		if moveRequest.MoveType == "redeployment" && moveRequest.SourcePotentialLocationID != nil {
			log.Printf("[MOVE]    → Warehouse redeployment to potential location - marking as converted")

			// Get shift ID from assigned_shift_id
			var shiftID *string
			if moveRequest.AssignedShiftID != nil {
				shiftID = moveRequest.AssignedShiftID
			}

			_, err = db.Exec(`
				UPDATE potential_locations
				SET converted_to_bin_id = $1,
				    converted_at = $2,
				    converted_via_shift_id = $3,
				    updated_at = $2
				WHERE id = $4
			`, moveRequest.BinID, now, shiftID, *moveRequest.SourcePotentialLocationID)

			if err != nil {
				log.Printf("[MOVE] ⚠️  Error updating potential location: %v", err)
			} else {
				log.Printf("[MOVE] ✅ Potential location marked as converted")

				// Broadcast potential_location_converted event to dashboard
				hub.BroadcastToRole("manager", map[string]interface{}{
					"type": "potential_location_converted",
					"data": map[string]interface{}{
						"potential_location_id": *moveRequest.SourcePotentialLocationID,
						"bin_id":                moveRequest.BinID,
						"shift_id":              shiftID,
						"converted_at":          now,
					},
				})
				log.Printf("📡 Broadcast potential_location_converted to managers")
			}
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
func CancelShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "id")
		log.Printf("❌ REQUEST: PUT /api/manager/shifts/%s/cancel", shiftID)

		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "shift_id is required")
			return
		}

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
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

		// 1. Update shift status to cancelled, record end_time
		_, err = tx.Exec(`
			UPDATE shifts
			SET status = 'cancelled', end_time = $1, updated_at = $1
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

		// 3. route_tasks are preserved for shift history audit trail

		// 4. Insert into shift_history so this cancellation appears in history tab
		var completionRate float64
		if shift.TotalBins > 0 {
			completionRate = float64(shift.CompletedBins) / float64(shift.TotalBins) * 100
		}
		_, err = tx.Exec(`
			INSERT INTO shift_history (
				id, driver_id, route_id, start_time, end_time, created_at, ended_at,
				total_pause_seconds, total_bins, completed_bins, completion_rate,
				incidents_reported, field_observations,
				end_reason, ended_by_user_id, end_reason_metadata
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (id) DO NOTHING
		`,
			shift.ID, shift.DriverID, shift.RouteID,
			shift.StartTime, now, shift.CreatedAt, now,
			shift.TotalPauseSeconds, shift.TotalBins, shift.CompletedBins, completionRate,
			0, 0,
			"manager_cancelled", userClaims.UserID, nil,
		)
		if err != nil {
			log.Printf("⚠️  Error inserting shift history on cancel: %v", err)
			// Don't fail the cancellation — history is best-effort
		} else {
			log.Printf("✅ Shift %s recorded in history (manager_cancelled by %s)", shiftID, userClaims.UserID)
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit cancellation")
			return
		}

		log.Printf("✅ Shift %s cancelled successfully", shiftID)

		// 4. Send WebSocket notification to driver's mobile app (OLD - backward compatibility)
		wsHub.BroadcastToUser(shift.DriverID, map[string]interface{}{
			"type": "shift_cancelled",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"cancelled_at": now,
				"message":      "Your shift has been cancelled by management",
			},
		})
		log.Printf("📡 Sent shift_cancelled websocket (old) to driver %s", shift.DriverID)

		// 4b. Send Centrifugo notification via shift:updates channel (NEW)
		if centrifugoClient != nil {
			cancellationData := map[string]interface{}{
				"type":         "shift_cancelled",
				"shift_id":     shiftID,
				"cancelled_at": now,
				"message":      "Your shift has been cancelled by management",
			}
			err := centrifugoClient.PublishShiftUpdate(r.Context(), shiftID, cancellationData)
			if err != nil {
				log.Printf("⚠️  Failed to send Centrifugo shift cancellation: %v", err)
			} else {
				log.Printf("📡 Sent shift_cancelled via Centrifugo to shift:updates:%s", shiftID)
			}
		}

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

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

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

		// 1. Update all shifts to cancelled, record end_time
		query, args, err := sqlx.In(`
			UPDATE shifts
			SET status = 'cancelled', end_time = ?, updated_at = ?
			WHERE id IN (?)
		`, now, now, shiftIDs)
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

		// 3. route_tasks are preserved for shift history audit trail

		// 4. Insert each shift into shift_history
		for _, s := range shifts {
			var cr float64
			if s.TotalBins > 0 {
				cr = float64(s.CompletedBins) / float64(s.TotalBins) * 100
			}
			_, histErr := tx.Exec(`
				INSERT INTO shift_history (
					id, driver_id, route_id, start_time, end_time, created_at, ended_at,
					total_pause_seconds, total_bins, completed_bins, completion_rate,
					incidents_reported, field_observations,
					end_reason, ended_by_user_id, end_reason_metadata
				) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
				ON CONFLICT (id) DO NOTHING
			`,
				s.ID, s.DriverID, s.RouteID,
				s.StartTime, now, s.CreatedAt, now,
				s.TotalPauseSeconds, s.TotalBins, s.CompletedBins, cr,
				0, 0,
				"manager_cancelled", userClaims.UserID, nil,
			)
			if histErr != nil {
				log.Printf("⚠️  Error inserting history for shift %s: %v", s.ID, histErr)
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

// ============================================================================
// SEGMENTED ROUTE OPTIMIZATION
// Manager places warehouse stops, we optimize tasks within each segment
// ============================================================================

// optimizeRouteInSegments performs route optimization between warehouse stops
// Warehouse stops act as segment boundaries and stay in place
func optimizeRouteInSegments(
	db *sqlx.DB,
	shiftID string,
	driverLat, driverLon float64,
	warehouseLat, warehouseLon float64,
	hereAPIKey string,
) error {
	log.Printf("🚀 [SEGMENT OPTIMIZER] Starting segmented route optimization")

	// Step 1: Fetch ALL tasks in order
	var allTasks []models.RouteTask
	query := `SELECT * FROM route_tasks WHERE shift_id = $1 ORDER BY sequence_order ASC`
	err := db.Select(&allTasks, query, shiftID)
	if err != nil {
		return fmt.Errorf("failed to fetch tasks: %w", err)
	}

	log.Printf("📋 [SEGMENT OPTIMIZER] Fetched %d tasks", len(allTasks))

	if len(allTasks) == 0 {
		log.Printf("✅ [SEGMENT OPTIMIZER] No tasks to optimize")
		return nil
	}

	// Step 2: Split tasks into segments by warehouse stops
	segments := [][]models.RouteTask{}
	currentSegment := []models.RouteTask{}

	for _, task := range allTasks {
		if task.TaskType == "warehouse_stop" {
			// Warehouse stop ends current segment
			if len(currentSegment) > 0 {
				segments = append(segments, currentSegment)
				currentSegment = []models.RouteTask{}
			}
			// Warehouse stop is its own "segment" (single task)
			segments = append(segments, []models.RouteTask{task})
		} else {
			currentSegment = append(currentSegment, task)
		}
	}

	// Add final segment if exists
	if len(currentSegment) > 0 {
		segments = append(segments, currentSegment)
	}

	log.Printf("📦 [SEGMENT OPTIMIZER] Split into %d segments", len(segments))

	// Step 3: Optimize each non-warehouse segment with HERE Maps
	hereService := services.NewHEREWaypointsService(hereAPIKey)
	optimizedTasks := []models.RouteTask{}
	startLat, startLon := driverLat, driverLon // Track position for next segment

	// Track total optimization metadata across all segments
	var totalDistanceKm float64
	var totalDurationSeconds int

	for segmentIdx, segment := range segments {
		// Warehouse stops don't get optimized, just keep them
		if len(segment) == 1 && segment[0].TaskType == "warehouse_stop" {
			log.Printf("   Segment #%d: Warehouse stop (keeping as-is)", segmentIdx+1)
			optimizedTasks = append(optimizedTasks, segment[0])
			startLat, startLon = segment[0].Latitude, segment[0].Longitude
			continue
		}

		// Single task segment - no optimization needed
		if len(segment) == 1 {
			log.Printf("   Segment #%d: Single task (no optimization needed)", segmentIdx+1)
			optimizedTasks = append(optimizedTasks, segment[0])
			startLat, startLon = segment[0].Latitude, segment[0].Longitude
			continue
		}

		// Multiple tasks - optimize with HERE
		log.Printf("   Segment #%d: Optimizing %d tasks...", segmentIdx+1, len(segment))
		log.Printf("   📍 Segment start: (%.6f, %.6f)", startLat, startLon)

		// Log original task order
		log.Printf("   📋 Original task order:")
		for i, task := range segment {
			log.Printf("      %d. %s (%.6f, %.6f) - %s",
				i+1, task.TaskType, task.Latitude, task.Longitude,
				truncateAddress(task.Address))
		}

		waypoints := make([]services.HEREWaypoint, len(segment))
		for i, task := range segment {
			name := ""
			if task.Address != nil {
				name = *task.Address
			}
			waypoints[i] = services.HEREWaypoint{
				ID:        task.ID,
				Name:      name,
				Latitude:  task.Latitude,
				Longitude: task.Longitude,
				TaskType:  string(task.TaskType), // Pass task type for service time calculation
			}
		}

		// Determine segment end point (next warehouse or final warehouse)
		endLat, endLon := warehouseLat, warehouseLon
		if segmentIdx < len(segments)-1 && segments[segmentIdx+1][0].TaskType == "warehouse_stop" {
			endLat = segments[segmentIdx+1][0].Latitude
			endLon = segments[segmentIdx+1][0].Longitude
		}
		log.Printf("   🏁 Segment end: (%.6f, %.6f)", endLat, endLon)

		// Calculate BEFORE optimization distance (straight-line Haversine estimate)
		beforeDistanceKm := 0.0
		beforeDistanceKm += haversineDistance(startLat, startLon, segment[0].Latitude, segment[0].Longitude)
		for i := 0; i < len(segment)-1; i++ {
			beforeDistanceKm += haversineDistance(
				segment[i].Latitude, segment[i].Longitude,
				segment[i+1].Latitude, segment[i+1].Longitude,
			)
		}
		beforeDistanceKm += haversineDistance(
			segment[len(segment)-1].Latitude, segment[len(segment)-1].Longitude,
			endLat, endLon,
		)

		log.Printf("   📏 BEFORE optimization (original order):")
		log.Printf("      Straight-line distance: %.2f km", beforeDistanceKm)

		departureTime := time.Now().Format(time.RFC3339)
		log.Printf("   🚀 Calling HERE Maps API with %d waypoints...", len(waypoints))

		result, err := hereService.OptimizeWaypoints(
			startLat, startLon,
			endLat, endLon,
			waypoints,
			departureTime,
		)

		if err != nil {
			log.Printf("   ❌ HERE optimization failed for segment #%d: %v", segmentIdx+1, err)
			log.Printf("   Using original order for this segment")
			optimizedTasks = append(optimizedTasks, segment...)
			if len(segment) > 0 {
				lastTask := segment[len(segment)-1]
				startLat, startLon = lastTask.Latitude, lastTask.Longitude
			}
			continue
		}

		log.Printf("   📏 AFTER optimization (HERE Maps optimized):")
		log.Printf("      Driving distance: %.2f km", result.TotalDistanceKm)
		log.Printf("      Driving time: %d sec (%.1f min)", result.TotalDurationSeconds, float64(result.TotalDurationSeconds)/60.0)

		// Accumulate total distance and duration for metadata
		totalDistanceKm += result.TotalDistanceKm
		totalDurationSeconds += result.TotalDurationSeconds

		log.Printf("   📊 OPTIMIZATION RESULT:")
		savingsKm := beforeDistanceKm - result.TotalDistanceKm
		savingsPercent := (savingsKm / beforeDistanceKm) * 100.0
		if savingsKm > 0 {
			log.Printf("      ✅ SAVED: %.2f km (%.1f%% improvement)", savingsKm, savingsPercent)
		} else if savingsKm < 0 {
			log.Printf("      ⚠️  Route LONGER by %.2f km (original was better)", -savingsKm)
		} else {
			log.Printf("      ⏸️  No distance change")
		}

		// Log the optimized order from HERE Maps
		log.Printf("   🔄 Optimized task order from HERE Maps:")
		taskMap := make(map[string]models.RouteTask)
		for _, task := range segment {
			taskMap[task.ID] = task
		}

		for i, taskID := range result.OptimizedOrder {
			task := taskMap[taskID]
			log.Printf("      %d. %s (%.6f, %.6f) - %s",
				i+1, task.TaskType, task.Latitude, task.Longitude,
				truncateAddress(task.Address))
		}

		// Check if order actually changed
		orderChanged := false
		for i, taskID := range result.OptimizedOrder {
			if i < len(segment) && taskID != segment[i].ID {
				orderChanged = true
				break
			}
		}
		if orderChanged {
			log.Printf("   ⚡ Order CHANGED by HERE Maps optimization")
		} else {
			log.Printf("   ⏸️  Order UNCHANGED (already optimal or HERE kept original order)")
		}

		for _, taskID := range result.OptimizedOrder {
			optimizedTasks = append(optimizedTasks, taskMap[taskID])
		}

		// Update start position for next segment
		if len(result.OptimizedOrder) > 0 {
			lastTask := taskMap[result.OptimizedOrder[len(result.OptimizedOrder)-1]]
			startLat, startLon = lastTask.Latitude, lastTask.Longitude
		}
	}

	// Step 4: Update sequence_order for all tasks
	log.Printf("\n📝 [SEGMENT OPTIMIZER] Updating task sequence...")
	now := time.Now().Unix()
	updateQuery := `UPDATE route_tasks SET sequence_order = $1, updated_at = $2 WHERE id = $3`

	for i, task := range optimizedTasks {
		_, err := db.Exec(updateQuery, i+1, now, task.ID)
		if err != nil {
			log.Printf("❌ Failed to update task %s: %v", task.ID, err)
			return fmt.Errorf("failed to update task sequence: %w", err)
		}
	}

	// Step 5: Save optimization metadata to shifts table
	log.Printf("\n💾 [SEGMENT OPTIMIZER] Saving optimization metadata...")
	log.Printf("   Total Distance: %.2f km", totalDistanceKm)
	log.Printf("   Total Duration: %d seconds (%.1f minutes)", totalDurationSeconds, float64(totalDurationSeconds)/60.0)

	// Format duration as "Xh Ym"
	hours := totalDurationSeconds / 3600
	minutes := (totalDurationSeconds % 3600) / 60
	var durationFormatted string
	if hours > 0 {
		durationFormatted = fmt.Sprintf("%dh %dm", hours, minutes)
	} else {
		durationFormatted = fmt.Sprintf("%dm", minutes)
	}

	optimizationMetadata := models.OptimizationMetadata{
		TotalDistanceKm:        totalDistanceKm,
		TotalDurationSeconds:   totalDurationSeconds,
		TotalDurationFormatted: durationFormatted,
		OptimizedAt:            time.Now().Format(time.RFC3339),
		EstimatedCompletion:    time.Now().Add(time.Duration(totalDurationSeconds) * time.Second).Format(time.RFC3339),
	}

	metadataJSON, err := json.Marshal(optimizationMetadata)
	if err != nil {
		log.Printf("⚠️  Error marshaling optimization metadata: %v", err)
		// Continue anyway - this is not critical
	} else {
		updateMetadataQuery := `UPDATE shifts SET optimization_metadata = $1, updated_at = $2 WHERE id = $3`
		_, err = db.Exec(updateMetadataQuery, metadataJSON, time.Now().Unix(), shiftID)
		if err != nil {
			log.Printf("⚠️  Error saving optimization metadata: %v", err)
			// Continue anyway - this is not critical
		} else {
			log.Printf("✅ Saved optimization metadata: %.2f km, %s, ETA: %s",
				optimizationMetadata.TotalDistanceKm,
				optimizationMetadata.TotalDurationFormatted,
				optimizationMetadata.EstimatedCompletion)
		}
	}

	log.Printf("✅ [SEGMENT OPTIMIZER] Route optimization complete - %d total tasks", len(optimizedTasks))
	return nil
}

// Helper functions for route optimization
func stringPtr(s string) *string { return &s }
func intPtr(i int) *int          { return &i }
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
// optimizeRouteWithMapbox optimizes a shift's route using Mapbox Optimization v2 API
// This replaces the old segmented HERE Maps optimization with intelligent capacity-aware routing
func optimizeRouteWithMapbox(
	db *sqlx.DB,
	shiftID string,
	capacity int,
	driverLat, driverLon float64,
	warehouseLat, warehouseLon float64,
	warehouseAddr string,
) error {
	log.Printf("🚀 [MAPBOX OPTIMIZER] Starting Mapbox v2 route optimization")

	// Step 1: Fetch shift details
	var shift struct {
		ID            string  `db:"id"`
		DriverID      string  `db:"driver_id"`
		WarehouseAddr *string `db:"warehouse_address"`
	}
	err := db.Get(&shift, `SELECT id, driver_id, warehouse_address FROM shifts WHERE id = $1`, shiftID)
	if err != nil {
		return fmt.Errorf("failed to fetch shift: %w", err)
	}

	// Step 2: Fetch all tasks for the shift
	var tasks []models.RouteTask
	query := `SELECT * FROM route_tasks WHERE shift_id = $1 ORDER BY sequence_order ASC`
	err = db.Select(&tasks, query, shiftID)
	if err != nil {
		return fmt.Errorf("failed to fetch tasks: %w", err)
	}

	log.Printf("📋 [MAPBOX OPTIMIZER] Fetched %d tasks", len(tasks))

	if len(tasks) == 0 {
		log.Printf("✅ [MAPBOX OPTIMIZER] No tasks to optimize")
		return nil
	}

	// Step 3: Convert to optimization.RouteRequest
	req := &optimization.RouteRequest{
		Vehicles:       make([]optimization.Vehicle, 1),
		Collections:    make([]optimization.Collection, 0),
		Placements:     make([]optimization.Placement, 0),
		MoveRequests:   make([]optimization.MoveRequest, 0),
		WarehouseStops: make([]optimization.WarehouseStop, 0),
	}

	// Define driver's current location (where route starts)
	driverStartLocation := optimization.Location{
		ID:        "driver-current",
		Name:      "Driver Current Location",
		Latitude:  driverLat,
		Longitude: driverLon,
		Address:   "Current GPS Position",
	}

	// Define warehouse location (where route ends)
	warehouseLocation := optimization.Location{
		ID:        "warehouse",
		Name:      "Warehouse",
		Latitude:  warehouseLat,
		Longitude: warehouseLon,
		Address:   warehouseAddr,
	}

	log.Printf("📍 [MAPBOX OPTIMIZER] Start: Driver location (%.6f, %.6f)", driverLat, driverLon)
	log.Printf("🏭 [MAPBOX OPTIMIZER] End: Warehouse location (%.6f, %.6f)", warehouseLat, warehouseLon)

	// Define vehicle with capacity
	req.Vehicles[0] = optimization.Vehicle{
		ID:            shift.DriverID,
		Name:          fmt.Sprintf("Truck-%s", shift.DriverID[:8]),
		StartLocation: driverStartLocation,
		EndLocation:   warehouseLocation,
		Capacities: map[string]int{
			"bins": capacity,
		},
	}

	// Helper functions for nil-safe value extraction
	getIntValue := func(ptr *int) int {
		if ptr != nil {
			return *ptr
		}
		return 0
	}

	getStringValue := func(ptr *string) string {
		if ptr != nil {
			return *ptr
		}
		return ""
	}

	// Convert tasks to optimization format
	for _, task := range tasks {
		switch task.TaskType {
		case "collection":
			if task.BinID != nil && task.Latitude != 0 && task.Longitude != 0 {
				collection := optimization.Collection{
					ID:             task.ID,
					BinID:          *task.BinID,
					BinNumber:      getIntValue(task.BinNumber),
					Location: optimization.Location{
						ID:        *task.BinID,
						Name:      fmt.Sprintf("Bin #%d", getIntValue(task.BinNumber)),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					Duration:       300, // 5 minutes
					FillPercentage: getIntValue(task.FillPercentage),
				}
				req.Collections = append(req.Collections, collection)
			} else {
				log.Printf("⚠️  Skipping collection task %s: missing required fields", task.ID)
			}

		case "placement":
			if task.PotentialLocationID != nil && task.Latitude != 0 && task.Longitude != 0 {
				placement := optimization.Placement{
					ID:                *task.PotentialLocationID,
					NewBinNumber:      getIntValue(task.NewBinNumber),
					WarehouseLocation: warehouseLocation,
					PlacementLocation: optimization.Location{
						ID:        *task.PotentialLocationID,
						Name:      fmt.Sprintf("Placement #%d", getIntValue(task.NewBinNumber)),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					PickupDuration:  60,  // 1 minute pickup
					DropoffDuration: 120, // 2 minutes dropoff
				}
				req.Placements = append(req.Placements, placement)
			} else {
				log.Printf("⚠️  Skipping placement task %s: missing required fields", task.ID)
			}

		case "pickup":
			if task.MoveRequestID != nil && task.Latitude != 0 && task.Longitude != 0 &&
				task.DestinationLatitude != nil && task.DestinationLongitude != nil {
				moveRequest := optimization.MoveRequest{
					ID:        *task.MoveRequestID,
					BinID:     getStringValue(task.BinID),
					BinNumber: getIntValue(task.BinNumber),
					PickupLocation: optimization.Location{
						ID:        fmt.Sprintf("%s-pickup", *task.MoveRequestID),
						Name:      fmt.Sprintf("Pickup #%d", getIntValue(task.BinNumber)),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					DropoffLocation: optimization.Location{
						ID:        fmt.Sprintf("%s-dropoff", *task.MoveRequestID),
						Name:      fmt.Sprintf("Dropoff #%d", getIntValue(task.BinNumber)),
						Latitude:  *task.DestinationLatitude,
						Longitude: *task.DestinationLongitude,
						Address:   getStringValue(task.DestinationAddress),
					},
					PickupDuration:  120, // 2 minutes pickup
					DropoffDuration: 120, // 2 minutes dropoff
				}
				req.MoveRequests = append(req.MoveRequests, moveRequest)
			} else {
				log.Printf("⚠️  Skipping move request task %s: missing required fields", task.ID)
			}

		case "warehouse_stop":
			// Skip warehouse stops - Mapbox handles them automatically
			log.Printf("⏭️  Skipping warehouse_stop task %s (handled implicitly by Mapbox)", task.ID)
		}
	}

	log.Printf("📊 [MAPBOX OPTIMIZER] Request: %d collections, %d placements, %d moves",
		len(req.Collections), len(req.Placements), len(req.MoveRequests))

	// Step 4: Call Mapbox optimizer
	log.Printf("🚀 [MAPBOX OPTIMIZER] Calling Mapbox Optimization v2 API...")
	mapboxOptimizer := optimization.NewMapboxOptimizer()
	response, err := mapboxOptimizer.OptimizeRoute(req)
	if err != nil {
		log.Printf("❌ [MAPBOX OPTIMIZER] Optimization FAILED: %v", err)
		return fmt.Errorf("Mapbox optimization failed: %w", err)
	}

	log.Printf("✅ [MAPBOX OPTIMIZER] Received response from Mapbox")
	log.Printf("📊 [MAPBOX OPTIMIZER] Response contains %d routes", len(response.Routes))

	if len(response.Routes) == 0 {
		log.Printf("❌ [MAPBOX OPTIMIZER] No routes in response! Dropped tasks: %v", response.DroppedTasks)
		return fmt.Errorf("Mapbox returned no routes")
	}

	route := response.Routes[0]
	log.Printf("✅ [MAPBOX OPTIMIZER] Optimization complete: %d stops, %.2f meters, %d seconds",
		len(route.Stops), route.TotalDistance, route.TotalDuration)

	// Step 5: Delete old route_tasks and create new ones from Mapbox response
	_, err = db.Exec(`DELETE FROM route_tasks WHERE shift_id = $1`, shiftID)
	if err != nil {
		return fmt.Errorf("failed to delete old tasks: %w", err)
	}

	log.Printf("🗑️  [MAPBOX OPTIMIZER] Deleted old route tasks")

	// Step 6: Create new route_tasks from optimized stops
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📍 Creating route tasks from %d optimized stops", len(route.Stops))
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	for i, stop := range route.Stops {
		log.Printf("")
		log.Printf("🔍 Processing Stop #%d/%d:", i+1, len(route.Stops))
		log.Printf("   Type: %s", stop.Type)
		log.Printf("   LocationID: %s", stop.LocationID)
		log.Printf("   CollectionID: '%s'", stop.CollectionID)
		log.Printf("   PlacementID: '%s'", stop.PlacementID)
		log.Printf("   MoveRequestID: '%s'", stop.MoveRequestID)
		log.Printf("   Coordinates: (%.6f, %.6f)", stop.Latitude, stop.Longitude)

		var task models.RouteTask
		task.ID = uuid.New().String()
		task.ShiftID = shiftID
		task.SequenceOrder = i + 1
		task.Latitude = stop.Latitude
		task.Longitude = stop.Longitude
		task.Address = &stop.Address

		// Determine task type and map to original task
		// Note: Mapbox returns "service" for collections, "pickup"/"dropoff" for shipments
		switch stop.Type {
		case optimization.StopTypeStart:
			// Skip start - driver is already at warehouse
			log.Printf("   ⏭️  SKIPPED: Start stop (driver already at warehouse)")
			continue

		case optimization.StopTypeEnd:
			// Map end to warehouse_stop so mobile knows to return to warehouse
			task.TaskType = "warehouse_stop"
			log.Printf("   ✅ Mapped end stop to warehouse_stop (return to warehouse)")

		case "service":
			// Mapbox returns "service" type for collections (not "collection")
			// Match by CollectionID which we extracted from the services array
			if stop.CollectionID != "" {
				for _, collection := range req.Collections {
					if stop.CollectionID == fmt.Sprintf("collection-%s", collection.ID) {
						task.TaskType = "collection"
						task.BinID = &collection.BinID
						task.BinNumber = &collection.BinNumber
						task.FillPercentage = &collection.FillPercentage
						log.Printf("   ✅ Matched service to collection (Bin #%d)", collection.BinNumber)
						break
					}
				}
			}

		case optimization.StopTypePickup:
			// Pickup for placement or move request
			// Match by PlacementID or MoveRequestID (extracted from pickups array)
			if stop.PlacementID != "" {
				// This is a placement pickup at warehouse
				for _, placement := range req.Placements {
					if stop.PlacementID == fmt.Sprintf("placement-%s", placement.ID) {
						task.TaskType = "warehouse_stop"
						task.PotentialLocationID = &placement.ID
						log.Printf("   ✅ Matched pickup to placement warehouse stop")
						break
					}
				}
			} else if stop.MoveRequestID != "" {
				// This is a move request pickup
				for _, moveReq := range req.MoveRequests {
					if stop.MoveRequestID == fmt.Sprintf("move-%s", moveReq.ID) {
						task.TaskType = "pickup"
						task.MoveRequestID = &moveReq.ID
						task.BinID = &moveReq.BinID
						task.BinNumber = &moveReq.BinNumber
						task.DestinationLatitude = &moveReq.DropoffLocation.Latitude
						task.DestinationLongitude = &moveReq.DropoffLocation.Longitude
						task.DestinationAddress = &moveReq.DropoffLocation.Address
						log.Printf("   ✅ Matched pickup to move request (Bin #%d)", moveReq.BinNumber)
						break
					}
				}
			}

		case optimization.StopTypeDropoff:
			// Dropoff for placement or move request
			// Match by PlacementID or MoveRequestID (extracted from dropoffs array)
			if stop.PlacementID != "" {
				// This is a placement dropoff
				for _, placement := range req.Placements {
					if stop.PlacementID == fmt.Sprintf("placement-%s", placement.ID) {
						task.TaskType = "placement"
						task.PotentialLocationID = &placement.ID
						task.NewBinNumber = &placement.NewBinNumber
						log.Printf("   ✅ Matched dropoff to placement (new bin #%d)", placement.NewBinNumber)
						break
					}
				}
			} else if stop.MoveRequestID != "" {
				// This is a move request dropoff
				for _, moveReq := range req.MoveRequests {
					if stop.MoveRequestID == fmt.Sprintf("move-%s", moveReq.ID) {
						task.TaskType = "dropoff"
						task.MoveRequestID = &moveReq.ID
						task.BinID = &moveReq.BinID
						task.BinNumber = &moveReq.BinNumber
						// Populate destination fields with dropoff location for consistent data structure
						task.DestinationLatitude = &stop.Latitude
						task.DestinationLongitude = &stop.Longitude
						task.DestinationAddress = &stop.Address
						log.Printf("   ✅ Matched dropoff to move request (Bin #%d)", moveReq.BinNumber)
						break
					}
				}
			}
		}

		// Validate task has a type before inserting
		if task.TaskType == "" {
			log.Printf("   ⚠️  WARNING: No task_type determined! Stop will cause database error.")
			log.Printf("   ⚠️  This stop did not match any case in the switch statement.")
		} else {
			log.Printf("   ✅ Mapped to task_type: '%s'", task.TaskType)
		}

		// Insert task
		log.Printf("   💾 Inserting into database...")
		insertQuery := `
			INSERT INTO route_tasks (
				id, shift_id, task_type, sequence_order,
				bin_id, bin_number, fill_percentage,
				potential_location_id, new_bin_number, placement_source,
				move_request_id, destination_latitude, destination_longitude, destination_address,
				latitude, longitude, address
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		`
		_, err = db.Exec(insertQuery,
			task.ID, task.ShiftID, task.TaskType, task.SequenceOrder,
			task.BinID, task.BinNumber, task.FillPercentage,
			task.PotentialLocationID, task.NewBinNumber, task.PlacementSource,
			task.MoveRequestID, task.DestinationLatitude, task.DestinationLongitude, task.DestinationAddress,
			task.Latitude, task.Longitude, task.Address,
		)
		if err != nil {
			return fmt.Errorf("failed to insert optimized task: %w", err)
		}
	}

	log.Printf("✅ [MAPBOX OPTIMIZER] Created %d optimized route tasks", len(route.Stops)-2) // -2 for start/end

	// Step 7: Save optimization metadata to shifts table
	optimizationMetadata := map[string]interface{}{
		"optimizer":       "mapbox-v2",
		"distance_meters": route.TotalDistance,
		"duration_seconds": route.TotalDuration,
		"optimized_at":    time.Now().Format(time.RFC3339),
	}

	metadataJSON, err := json.Marshal(optimizationMetadata)
	if err != nil {
		log.Printf("⚠️  Failed to marshal optimization metadata: %v", err)
	} else {
		_, err = db.Exec(`
			UPDATE shifts
			SET optimization_metadata = $1
			WHERE id = $2
		`, metadataJSON, shiftID)
		if err != nil {
			log.Printf("⚠️  Failed to save optimization metadata: %v", err)
		}
	}

	log.Printf("✅ [MAPBOX OPTIMIZER] Route optimization complete!")
	return nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// truncateAddress truncates an address string to max 50 characters for logging
func truncateAddress(addr *string) string {
	if addr == nil {
		return "No address"
	}
	if len(*addr) <= 50 {
		return *addr
	}
	return (*addr)[:47] + "..."
}

// NotifyDriverOfRouteUpdate sends a Centrifugo notification to a driver when their shift tasks change
// This is used when managers edit bins, move requests, or potential locations that affect active shifts
func NotifyDriverOfRouteUpdate(
	db *sqlx.DB,
	centrifugoClient *centrifugo.Client,
	shiftID string,
	changeType string,
	changeDetails map[string]interface{},
) error {
	if centrifugoClient == nil {
		log.Printf("⚠️  [NOTIFY-DRIVER] Centrifugo client is nil, skipping notification for shift %s", shiftID)
		return nil
	}

	log.Printf("📢 [NOTIFY-DRIVER] Notifying driver about route update for shift %s (change: %s)", shiftID, changeType)

	// Get updated tasks for the shift (only incomplete tasks)
	var tasks []models.RouteTask
	err := db.Select(&tasks, `
		SELECT * FROM route_tasks
		WHERE shift_id = $1 AND is_completed = 0
		ORDER BY sequence_order ASC
	`, shiftID)
	if err != nil {
		log.Printf("❌ [NOTIFY-DRIVER] Failed to fetch route tasks for shift %s: %v", shiftID, err)
		return fmt.Errorf("failed to fetch route tasks: %w", err)
	}

	log.Printf("📋 [NOTIFY-DRIVER] Found %d incomplete tasks for shift %s", len(tasks), shiftID)

	// Build notification payload
	payload := map[string]interface{}{
		"type":         "route_updated",
		"change_type":  changeType,
		"details":      changeDetails,
		"updated_tasks": tasks,
		"timestamp":    time.Now().Unix(),
	}

	// Publish to shift-specific channel
	channel := fmt.Sprintf("shift:updates:%s", shiftID)
	ctx := context.Background()
	err = centrifugoClient.PublishToChannel(ctx, channel, payload)
	if err != nil {
		log.Printf("❌ [NOTIFY-DRIVER] Failed to publish to Centrifugo channel %s: %v", channel, err)
		return fmt.Errorf("failed to publish notification: %w", err)
	}

	log.Printf("✅ [NOTIFY-DRIVER] Successfully published route update notification to channel %s", channel)
	return nil
}
