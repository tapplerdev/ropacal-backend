package handlers

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/pkg/utils"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

type shiftHistoryExec interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// archiveShiftParams carries the per-call values for a shift_history insert.
// optMeta is passed in (rather than derived) so each call site preserves its
// exact source — marshaled struct metadata vs. raw bytes re-read from the DB.
type archiveShiftParams struct {
	ID                  string
	DriverID            string
	RouteID             interface{}
	StartTime           interface{}
	EndTime             interface{}
	CreatedAt           interface{}
	EndedAt             interface{}
	TotalPauseSeconds   interface{}
	TotalBins           int
	CompletedBins       int
	IncidentsReported   int
	FieldObservations   int
	EndReason           string
	EndedByUserID       interface{}
	EndReasonMetadata   interface{}
	OptMeta             interface{}
	OnConflictDoNothing bool
}

// completionRate computes the percentage of bins completed for a shift,
// guarding against division by zero.
func completionRate(completedBins, totalBins int) float64 {
	if totalBins > 0 {
		return (float64(completedBins) / float64(totalBins)) * 100
	}
	return 0.0
}

// archiveShift performs the canonical 17-column shift_history insert. It is the
// single source of truth for archiving a shift; all end/cancel paths route
// through it so the column set never drifts again.
func archiveShift(exec shiftHistoryExec, p archiveShiftParams) (sql.Result, error) {
	query := `INSERT INTO shift_history (
		id, driver_id, route_id, start_time, end_time, created_at, ended_at,
		total_pause_seconds, total_bins, completed_bins, completion_rate,
		incidents_reported, field_observations,
		end_reason, ended_by_user_id, end_reason_metadata, optimization_metadata
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`
	if p.OnConflictDoNothing {
		query += " ON CONFLICT (id) DO NOTHING"
	}
	return exec.Exec(
		query,
		p.ID,
		p.DriverID,
		p.RouteID,
		p.StartTime,
		p.EndTime,
		p.CreatedAt,
		p.EndedAt,
		p.TotalPauseSeconds,
		p.TotalBins,
		p.CompletedBins,
		completionRate(p.CompletedBins, p.TotalBins),
		p.IncidentsReported,
		p.FieldObservations,
		p.EndReason,
		p.EndedByUserID,
		p.EndReasonMetadata,
		p.OptMeta,
	)
}

// GetCurrentShift returns the current active shift for the driver
func GetCurrentShift(store ShiftStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/driver/shift/current")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   User: %s (%s)", userClaims.Email, userClaims.UserID)

		ctx := r.Context()

		// Prioritize active/paused shifts (any date), then ready shifts for today or earlier.
		// Future-scheduled ready shifts only show on or after their scheduled_date.
		pacific, _ := time.LoadLocation("America/Los_Angeles")
		todayStr := time.Now().In(pacific).Format("2006-01-02")

		shift, err := store.CurrentByDriver(ctx, userClaims.UserID, todayStr)
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

		// Fetch the legacy bin-detail view. It isn't part of the response, but the
		// original handler 500s if it fails — preserve that behavior exactly.
		if _, err := store.TasksWithDetails(ctx, shift.ID); err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch route bins")
			return
		}

		// Get tasks from route_tasks table (new task-based system)
		tasks, err := store.Tasks(ctx, shift.ID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v (using bins only)", err)
			tasks = []models.RouteTask{} // Empty tasks array on error
		}

		// Derive the progress counts from route_tasks (the source of truth) rather
		// than the stored shifts.total_bins/completed_bins columns, which drift
		// (hand-adjusted ±1 across handlers). Same semantics, can't go stale.
		counts := itinerary.CountStops(tasks)

		log.Printf("📤 RESPONSE: 200 OK — shift %s status %s, %d tasks (%d/%d bins)", shift.ID, shift.Status, len(tasks), counts.CompletedBins, counts.TotalBins)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": currentShiftResponse{
				CompletedBins:     counts.CompletedBins,
				CreatedAt:         shift.CreatedAt,
				DriverID:          shift.DriverID,
				EndTime:           shift.EndTime,
				ID:                shift.ID,
				PauseStartTime:    shift.PauseStartTime,
				RouteID:           shift.RouteID,
				StartTime:         shift.StartTime,
				Status:            shift.Status,
				Tasks:             tasks,
				TotalBins:         counts.TotalBins,
				TotalPauseSeconds: shift.TotalPauseSeconds,
				UpdatedAt:         shift.UpdatedAt,
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
				"tasks":               bins, // ← Changed from "bins" to "tasks" for mobile app compatibility
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
			ActiveTaskCount         int      `json:"active_task_count"`                   // Count of non-deleted tasks
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

			// Calculate active (non-deleted) task count from route_tasks
			var activeTaskCount int
			taskCountErr := db.Get(&activeTaskCount, `
				SELECT COUNT(*) FROM route_tasks
				WHERE shift_id = $1 AND is_deleted = FALSE
			`, shift.ID)
			if taskCountErr != nil {
				log.Printf("⚠️  Error counting active tasks for shift %s: %v", shift.ID, taskCountErr)
				activeTaskCount = shift.TotalBins // Fallback to database value
			}

			// Create response with computed fields
			shiftResp := ShiftResponse{
				ShiftWithDriver: shift,
				ActiveTaskCount: activeTaskCount,
			}

			// Add computed fields if optimization metadata exists
			if shift.OptimizationMetadata != nil {
				distanceMiles := shift.OptimizationMetadata.TotalDistanceMiles
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
				COALESCE(COUNT(CASE WHEN rt.task_type = 'warehouse_stop' AND rt.is_completed = 1 THEN 1 END), 0) AS warehouse_stops,
				sh.optimization_metadata
			FROM shift_history sh
			LEFT JOIN users u ON sh.driver_id = u.id
			LEFT JOIN route_tasks rt ON sh.id = rt.shift_id
			%s
			GROUP BY sh.id, u.name, u.email, sh.optimization_metadata
			ORDER BY sh.ended_at DESC
			LIMIT $%d OFFSET $%d
		`, where, argIndex, argIndex+1)
		args = append(args, limit, offset)

		type ShiftHistoryRow struct {
			ID                    string                       `db:"id" json:"id"`
			DriverID              string                       `db:"driver_id" json:"driver_id"`
			DriverName            string                       `db:"driver_name" json:"driver_name"`
			DriverEmail           string                       `db:"driver_email" json:"driver_email"`
			RouteID               *string                      `db:"route_id" json:"route_id"`
			StartTime             *int64                       `db:"start_time" json:"start_time"`
			EndTime               *int64                       `db:"end_time" json:"end_time"`
			CreatedAt             int64                        `db:"created_at" json:"created_at"`
			EndedAt               int64                        `db:"ended_at" json:"ended_at"`
			TotalPauseSeconds     int                          `db:"total_pause_seconds" json:"total_pause_seconds"`
			TotalBins             int                          `db:"total_bins" json:"total_bins"`
			CompletedBins         int                          `db:"completed_bins" json:"completed_bins"`
			CompletionRate        float64                      `db:"completion_rate" json:"completion_rate"`
			IncidentsReported     int                          `db:"incidents_reported" json:"incidents_reported"`
			FieldObservations     int                          `db:"field_observations" json:"field_observations"`
			EndReason             string                       `db:"end_reason" json:"end_reason"`
			CollectionsCompleted  int64                        `db:"collections_completed" json:"collections_completed"`
			CollectionsSkipped    int64                        `db:"collections_skipped" json:"collections_skipped"`
			PlacementsCompleted   int64                        `db:"placements_completed" json:"placements_completed"`
			PlacementsSkipped     int64                        `db:"placements_skipped" json:"placements_skipped"`
			MoveRequestsCompleted int64                        `db:"move_requests_completed" json:"move_requests_completed"`
			TotalSkipped          int64                        `db:"total_skipped" json:"total_skipped"`
			WarehouseStops        int64                        `db:"warehouse_stops" json:"warehouse_stops"`
			OptimizationMetadata  *models.OptimizationMetadata `db:"optimization_metadata" json:"optimization_metadata,omitempty"`
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
			ID            string   `db:"id"                    json:"id"`
			SequenceOrder int      `db:"sequence_order"        json:"sequence_order"`
			TaskType      string   `db:"task_type"             json:"task_type"`
			IsCompleted   int      `db:"is_completed"          json:"is_completed"`
			Skipped       bool     `db:"skipped"               json:"skipped"`
			CompletedAt   *int64   `db:"completed_at"          json:"completed_at"`
			Address       *string  `db:"address"               json:"address"`
			Latitude      *float64 `db:"latitude"              json:"latitude"`
			Longitude     *float64 `db:"longitude"             json:"longitude"`
			TaskData      *string  `db:"task_data"             json:"task_data"`
			// Collection / bin fields
			BinID               *string  `db:"bin_id"                json:"bin_id"`
			BinNumber           *int     `db:"bin_number"            json:"bin_number"`
			UpdatedFillPct      *int     `db:"updated_fill_percentage" json:"updated_fill_percentage"`
			BinStreet           *string  `db:"bin_street"            json:"bin_street"`
			BinCity             *string  `db:"bin_city"              json:"bin_city"`
			PhotoURL            *string  `db:"photo_url"             json:"photo_url"`
			AfterPhotoURL       *string  `db:"after_photo_url"       json:"after_photo_url"`
			PhotoLatitude       *float64 `db:"photo_latitude"        json:"photo_latitude"`
			PhotoLongitude      *float64 `db:"photo_longitude"       json:"photo_longitude"`
			AfterPhotoLatitude  *float64 `db:"after_photo_latitude"  json:"after_photo_latitude"`
			AfterPhotoLongitude *float64 `db:"after_photo_longitude" json:"after_photo_longitude"`
			// Placement fields
			PotentialLocationID       *string `db:"potential_location_id" json:"potential_location_id"`
			NewBinNumber              *int    `db:"new_bin_number"        json:"new_bin_number"`
			PlacementAddress          *string `db:"placement_address"     json:"placement_address"`
			PlacementCreatedBinID     *string `db:"placement_created_bin_id" json:"placement_created_bin_id"`
			PlacementCreatedBinNumber *int    `db:"placement_created_bin_number" json:"placement_created_bin_number"`
			// Move request fields
			MoveRequestID      *string `db:"move_request_id"       json:"move_request_id"`
			MoveType           *string `db:"move_type"             json:"move_type"`
			DestinationAddress *string `db:"destination_address"   json:"destination_address"`
			// Warehouse fields
			WarehouseAction *string `db:"warehouse_action"      json:"warehouse_action"`
			BinsToLoad      *int    `db:"bins_to_load"          json:"bins_to_load"`
			// Service fields
			TaskLabel       *string `db:"task_label"            json:"task_label"`
			TaskDescription *string `db:"task_description"      json:"task_description"`
			CompletionNotes *string `db:"completion_notes"      json:"completion_notes"`
			PhotoRequired   bool    `db:"photo_required"        json:"photo_required"`
			// Driver-reported incident at this stop (if any). Completion validation
			// allows a photo-less/fill-less completion when an incident is filed —
			// the driver's evidence lives on the incident record, so the history
			// card must surface it or the stop looks like a missing photo.
			IncidentType        *string `db:"incident_type"        json:"incident_type"`
			IncidentPhotoURL    *string `db:"incident_photo_url"   json:"incident_photo_url"`
			IncidentDescription *string `db:"incident_description" json:"incident_description"`
			IncidentZoneID      *string `db:"incident_zone_id"     json:"incident_zone_id"`
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
				COALESCE(bc.photo_url, rt.photo_url) AS photo_url,
				rt.after_photo_url,
				rt.photo_latitude,
				rt.photo_longitude,
				rt.after_photo_latitude,
				rt.after_photo_longitude,
				rt.potential_location_id,
				rt.new_bin_number,
				pl.address          AS placement_address,
				pl.converted_to_bin_id AS placement_created_bin_id,
				cb.bin_number       AS placement_created_bin_number,
				rt.move_request_id,
				bmr.move_type,
				bmr.new_address       AS destination_address,
				rt.warehouse_action,
				rt.bins_to_load,
				rt.task_label,
				rt.task_description,
				rt.completion_notes,
				rt.photo_required,
				zi.incident_type,
				zi.photo_url    AS incident_photo_url,
				zi.description  AS incident_description,
				zi.zone_id      AS incident_zone_id
			FROM route_tasks rt
			LEFT JOIN bins b   ON b.id  = rt.bin_id
			LEFT JOIN LATERAL (
				SELECT photo_url
				FROM checks
				WHERE bin_id = rt.bin_id
				AND shift_id = rt.shift_id
				AND checked_on = rt.completed_at
				ORDER BY checked_on DESC
				LIMIT 1
			) bc ON true
			LEFT JOIN LATERAL (
				SELECT incident_type, photo_url, description, zone_id
				FROM zone_incidents
				WHERE shift_id = rt.shift_id AND bin_id = rt.bin_id
				ORDER BY reported_at DESC
				LIMIT 1
			) zi ON true
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

// GetDriverShiftHistoryByID returns all completed shifts for a specific driver (manager access)
// GET /api/manager/drivers/{driverId}/shifts
func GetDriverShiftHistoryByID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		driverID := chi.URLParam(r, "driverId")
		log.Printf("📥 REQUEST: GET /api/manager/drivers/%s/shifts", driverID)

		if driverID == "" {
			utils.RespondError(w, http.StatusBadRequest, "driver_id is required")
			return
		}

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
		err := db.Select(&shifts, query, driverID)
		if err != nil {
			log.Printf("❌ Error fetching shift history for driver %s: %v", driverID, err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift history")
			return
		}

		log.Printf("✅ Found %d shifts for driver %s", len(shifts), driverID)
		log.Printf("📤 RESPONSE: 200 OK")

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    shifts,
		})
	}
}

// GetShiftDetails returns detailed information about a specific shift including all bins
func GetShiftDetails(store ShiftStore) http.HandlerFunc {
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

		ctx := r.Context()

		shift, err := store.ByIDForDriver(ctx, shiftID, userClaims.UserID)
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}

		// Legacy bin-detail view — not in the response, and (unlike GetCurrentShift)
		// a failure is non-fatal here. Fetch only for parity with prior behavior.
		if _, err := store.TasksWithDetails(ctx, shiftID); err != nil {
			log.Printf("❌ Error fetching route bins: %v", err)
		}

		tasks, err := store.Tasks(ctx, shiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v (using bins only)", err)
			tasks = []models.RouteTask{} // Empty tasks array on error
		}

		// Derived counts (see GetCurrentShift) — drift-proof, same semantics.
		counts := itinerary.CountStops(tasks)

		log.Printf("📤 RESPONSE: 200 OK — shift %s, %d tasks (%d/%d bins)", shift.ID, len(tasks), counts.CompletedBins, counts.TotalBins)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": shiftDetailsResponse{
				CompletedBins:     counts.CompletedBins,
				CreatedAt:         shift.CreatedAt,
				DriverID:          shift.DriverID,
				EndTime:           shift.EndTime,
				ID:                shift.ID,
				RouteID:           shift.RouteID,
				StartTime:         shift.StartTime,
				Status:            shift.Status,
				Tasks:             tasks,
				TotalBins:         counts.TotalBins,
				TotalPauseSeconds: shift.TotalPauseSeconds,
				UpdatedAt:         shift.UpdatedAt,
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

// CheckShiftDriverProximity checks if the shift's driver is currently nearby their current task
// GET /api/manager/shifts/:id/driver-proximity

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
		} else if bin.StopType != "warehouse_stop" && bin.StopType != "service" {
			// Regular bin task (collection, placement, etc.) — exclude warehouse/service
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
		WHERE rt.shift_id = $1 AND rt.is_deleted = false
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
