package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/models"
)

// GetShiftTasks retrieves all active (non-deleted) tasks for a shift ordered by sequence
func GetShiftTasks(db *sqlx.DB, shiftID string) ([]models.RouteTask, error) {
	var tasks []models.RouteTask
	query := `
		SELECT
			rt.*,
			c.photo_url
		FROM route_tasks rt
		LEFT JOIN LATERAL (
			SELECT photo_url
			FROM checks
			WHERE bin_id = rt.bin_id
				AND shift_id = rt.shift_id
				AND checked_on = rt.completed_at
			ORDER BY checked_on DESC
			LIMIT 1
		) c ON true
		WHERE rt.shift_id = $1 AND rt.is_deleted = FALSE
		ORDER BY rt.sequence_order ASC
	`

	err := db.Select(&tasks, query, shiftID)
	if err != nil {
		return nil, fmt.Errorf("failed to get shift tasks: %w", err)
	}

	return tasks, nil
}

// GetShiftTasksWithDeleted retrieves ALL tasks for a shift including deleted ones (for audit/history)
func GetShiftTasksWithDeleted(db *sqlx.DB, shiftID string) ([]models.RouteTask, error) {
	var tasks []models.RouteTask
	query := `
		SELECT
			rt.id,
			rt.shift_id,
			rt.sequence_order,
			rt.task_type,
			rt.latitude,
			rt.longitude,
			rt.address,
			rt.bin_id,
			COALESCE(rt.bin_number, b.bin_number) as bin_number,
			rt.fill_percentage,
			rt.potential_location_id,
			rt.new_bin_number,
			rt.placement_source,
			rt.move_request_id,
			rt.destination_latitude,
			rt.destination_longitude,
			rt.destination_address,
			rt.move_type,
			rt.warehouse_action,
			rt.bins_to_load,
			rt.route_id,
			rt.is_completed,
			rt.completed_at,
			rt.skipped,
			rt.updated_fill_percentage,
			rt.is_deleted,
			rt.deleted_at,
			rt.deleted_by,
			rt.deletion_reason,
			rt.task_data,
			rt.created_at,
			rt.updated_at,
			c.photo_url
		FROM route_tasks rt
		LEFT JOIN bins b ON rt.bin_id = b.id
		LEFT JOIN LATERAL (
			SELECT photo_url
			FROM checks
			WHERE bin_id = rt.bin_id
				AND shift_id = rt.shift_id
				AND checked_on = rt.completed_at
			ORDER BY checked_on DESC
			LIMIT 1
		) c ON true
		WHERE rt.shift_id = $1
		ORDER BY rt.is_deleted ASC, rt.sequence_order ASC
	`

	err := db.Select(&tasks, query, shiftID)
	if err != nil {
		return nil, fmt.Errorf("failed to get shift tasks with deleted: %w", err)
	}

	return tasks, nil
}

// GetShiftTasksDetailed retrieves tasks with JOINed data from related tables
func GetShiftTasksDetailed(db *sqlx.DB, shiftID string) ([]map[string]interface{}, error) {
	// Query route_tasks directly and LEFT JOIN with bins table to get bin_number for collection tasks
	// Also LEFT JOIN with checks table to get photo_url for completed tasks
	query := `
		SELECT
			rt.id,
			rt.shift_id,
			rt.sequence_order,
			rt.task_type,
			rt.latitude,
			rt.longitude,
			rt.address,
			rt.bin_id,
			COALESCE(rt.bin_number, b.bin_number::int) as bin_number,
			rt.fill_percentage,
			rt.potential_location_id,
			rt.new_bin_number,
			rt.move_request_id,
			rt.destination_latitude,
			rt.destination_longitude,
			rt.destination_address,
			rt.move_type,
			rt.warehouse_action,
			rt.bins_to_load,
			rt.route_id,
			rt.task_data,
			rt.is_completed,
			rt.completed_at,
			rt.updated_fill_percentage,
			rt.skipped,
			rt.created_at,
			rt.updated_at,
			c.photo_url
		FROM route_tasks rt
		LEFT JOIN bins b ON rt.bin_id = b.id
		LEFT JOIN LATERAL (
			SELECT photo_url
			FROM checks
			WHERE bin_id = rt.bin_id
				AND shift_id = rt.shift_id
				AND checked_on = rt.completed_at
			ORDER BY checked_on DESC
			LIMIT 1
		) c ON true
		WHERE rt.shift_id = $1 AND rt.is_deleted = FALSE
		ORDER BY rt.sequence_order ASC
	`

	rows, err := db.Queryx(query, shiftID)
	if err != nil {
		return nil, fmt.Errorf("failed to get detailed tasks: %w", err)
	}
	defer rows.Close()

	var tasks []map[string]interface{}
	for rows.Next() {
		task := make(map[string]interface{})
		err := rows.MapScan(task)
		if err != nil {
			return nil, fmt.Errorf("failed to scan task row: %w", err)
		}
		tasks = append(tasks, task)
	}

	// Log first 3 tasks to see coordinate data types
	if len(tasks) > 0 {
		log.Println("[DIAGNOSTIC] 🔍 RAW TASK DATA FROM DATABASE (first 3 tasks):")
		for i := 0; i < len(tasks) && i < 3; i++ {
			task := tasks[i]
			log.Printf("[DIAGNOSTIC]    Task #%d: task_type=%v, lat=%v (type=%T), lng=%v (type=%T), addr=%v",
				i, task["task_type"], task["latitude"], task["latitude"], task["longitude"], task["longitude"], task["address"])
		}
	}

	return tasks, nil
}

// CreateShiftWithTasks creates a shift and its tasks in a transaction
func CreateShiftWithTasks(
	db *sqlx.DB,
	driverID string,
	truckBinCapacity int,
	warehouseLat, warehouseLon *float64,
	warehouseAddr *string,
	tasks []map[string]interface{},
	lockRouteOrder bool,
	warehouseDeployments []models.WarehouseDeploymentTask,
	// Custom shift fields
	shiftType string,
	shiftLabel *string,
	startLat, startLon *float64,
	startAddr *string,
	endLat, endLon *float64,
	endAddr *string,
) (string, int, error) {
	tx, err := db.Beginx()
	if err != nil {
		return "", 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Default shift_type to "standard"
	if shiftType == "" {
		shiftType = "standard"
	}

	// Create shift
	shiftID := uuid.New().String()
	now := time.Now().Unix()

	shiftQuery := `
		INSERT INTO shifts (
			id, driver_id, status, total_bins, completed_bins,
			truck_bin_capacity, warehouse_latitude, warehouse_longitude, warehouse_address,
			lock_route_order, shift_type, shift_label,
			start_latitude, start_longitude, start_address,
			end_latitude, end_longitude, end_address,
			created_at, updated_at
		) VALUES ($1, $2, 'ready', $3, 0, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	`

	// Count only bin-related tasks (exclude warehouse_stop) for total_bins
	totalBins := 0
	for _, taskData := range tasks {
		taskType, _ := taskData["task_type"].(string)
		if taskType != "warehouse_stop" {
			totalBins++
		}
	}
	log.Printf("   📊 Shift total_bins: %d (excluding warehouse stops from %d total tasks)", totalBins, len(tasks))

	_, err = tx.Exec(
		shiftQuery,
		shiftID, driverID, totalBins, truckBinCapacity,
		warehouseLat, warehouseLon, warehouseAddr,
		lockRouteOrder, shiftType, shiftLabel,
		startLat, startLon, startAddr,
		endLat, endLon, endAddr,
		now, now,
	)
	if err != nil {
		return "", 0, fmt.Errorf("failed to create shift: %w", err)
	}

	// Create tasks
	taskQuery := `
		INSERT INTO route_tasks (
			id, shift_id, sequence_order, task_type, latitude, longitude, address,
			bin_id, bin_number, fill_percentage,
			potential_location_id, new_bin_number, placement_source,
			move_request_id, destination_latitude, destination_longitude, destination_address, move_type,
			warehouse_action, bins_to_load,
			route_id, task_data, created_at,
			task_label, task_description, photo_required,
			earliest_arrival, latest_arrival, time_window_type,
			service_duration_seconds
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10,
			$11, $12, $13,
			$14, $15, $16, $17, $18,
			$19, $20,
			$21, $22, $23,
			$24, $25, $26,
			$27, $28, $29,
			$30
		)
	`

	for i, taskData := range tasks {
		taskID := uuid.New().String()

		// Extract task fields with nil safety
		taskType, _ := taskData["task_type"].(string)
		lat, _ := taskData["latitude"].(float64)
		lon, _ := taskData["longitude"].(float64)

		log.Printf("   🔍 Task #%d DEBUG: Received task_type='%s', lat=%.6f, lon=%.6f", i+1, taskType, lat, lon)

		// For pickup tasks with 0,0 coordinates, look up bin coordinates
		moveType, _ := taskData["move_type"].(string)
		log.Printf("   🔍 Task #%d DEBUG: move_type='%s'", i+1, moveType)
		log.Printf("   🔍 Task #%d DEBUG: Checking if taskType=='pickup': %v", i+1, taskType == "pickup")

		if taskType == "pickup" {
			log.Printf("   ✅ Task #%d: Matched 'pickup' condition", i+1)
			if lat == 0 && lon == 0 {
				// Get bin_id from task data
				if binIDInterface, ok := taskData["bin_id"]; ok && binIDInterface != nil {
					binID, _ := binIDInterface.(string)
					if binID != "" {
						// Look up bin coordinates from bins table
						var binCoords struct {
							Latitude  float64 `db:"latitude"`
							Longitude float64 `db:"longitude"`
						}
						err := tx.Get(&binCoords, "SELECT latitude, longitude FROM bins WHERE id = $1", binID)
						if err == nil && binCoords.Latitude != 0 && binCoords.Longitude != 0 {
							lat = binCoords.Latitude
							lon = binCoords.Longitude
							log.Printf("   ✅ Task #%d: Populated pickup coordinates from bin %s: %.6f, %.6f", i+1, binID, lat, lon)
						} else if err != nil {
							log.Printf("   ⚠️  Task #%d: Failed to lookup bin coordinates for %s: %v", i+1, binID, err)
						}
					}
				}
			}
		} else {
			log.Printf("   ℹ️  Task #%d: Not a pickup task or has coordinates - using provided values: lat=%.6f, lon=%.6f", i+1, lat, lon)
		}

		// For warehouse_stop tasks, ALWAYS use shift's warehouse coordinates
		// This ensures consistency - shift warehouse is single source of truth
		if taskType == "warehouse_stop" && warehouseLat != nil && warehouseLon != nil {
			log.Printf("   🏭 Task #%d: Warehouse stop detected - overriding with shift warehouse coordinates", i+1)
			lat = *warehouseLat
			lon = *warehouseLon
			log.Printf("   ✅ Task #%d: Using shift warehouse: %.6f, %.6f", i+1, lat, lon)

			// Also override address if not provided or empty
			if addr, ok := taskData["address"]; !ok || addr == nil || addr == "" {
				if warehouseAddr != nil {
					taskData["address"] = *warehouseAddr
					log.Printf("   ✅ Task #%d: Set warehouse address: %s", i+1, *warehouseAddr)
				}
			}
		}

		// Convert task_data to JSON if present, default to empty object
		var taskDataJSON interface{}
		if td, ok := taskData["task_data"]; ok && td != nil {
			taskDataJSON, _ = json.Marshal(td)
		} else {
			// Default to empty JSON object instead of NULL to prevent scan errors
			taskDataJSON = []byte("{}")
		}

		// Helper function to get values with nil safety
		getString := func(key string) interface{} {
			if val, ok := taskData[key]; ok {
				return val
			}
			return nil
		}

		getInt := func(key string) interface{} {
			if val, ok := taskData[key]; ok {
				// Handle int, float64, and string from JSON
				switch v := val.(type) {
				case float64:
					return int(v)
				case int:
					return v
				case string:
					// Handle string numbers (e.g., "33" from dashboard)
					if intVal, err := strconv.Atoi(v); err == nil {
						return intVal
					}
				}
			}
			return nil
		}

		getFloat := func(key string) interface{} {
			if val, ok := taskData[key]; ok {
				return val
			}
			return nil
		}

		// Auto-populate bin_number and fill_percentage from bins table if missing
		// This ensures data integrity even if the client (dashboard/mobile) doesn't send these fields
		if binIDInterface, ok := taskData["bin_id"]; ok && binIDInterface != nil {
			binID, _ := binIDInterface.(string)
			if binID != "" {
				// Check if bin_number or fill_percentage is missing
				_, hasBinNumber := taskData["bin_number"]
				_, hasFillPercentage := taskData["fill_percentage"]

				if !hasBinNumber || !hasFillPercentage {
					var binData struct {
						BinNumber      int `db:"bin_number"`
						FillPercentage int `db:"fill_percentage"`
					}
					err := tx.Get(&binData, "SELECT bin_number, fill_percentage FROM bins WHERE id = $1", binID)
					if err == nil {
						if !hasBinNumber {
							taskData["bin_number"] = binData.BinNumber
							log.Printf("   ✅ Task #%d: Auto-populated bin_number=%d from bins table", i+1, binData.BinNumber)
						}
						if !hasFillPercentage {
							taskData["fill_percentage"] = binData.FillPercentage
							log.Printf("   ✅ Task #%d: Auto-populated fill_percentage=%d from bins table", i+1, binData.FillPercentage)
						}
					} else if err != nil {
						log.Printf("   ⚠️  Task #%d: Failed to lookup bin data for %s: %v", i+1, binID, err)
					}
				}
			}
		}

		// Determine placement_source: default to "potential_location" for placement tasks
		placementSource := getString("placement_source")
		if taskType == "placement" && placementSource == nil {
			s := "potential_location"
			placementSource = s
		}

		// Parse time window fields for service tasks
		var earliestArrival, latestArrival interface{}
		if ea, ok := taskData["earliest_arrival"].(string); ok && ea != "" {
			if t, err := time.Parse(time.RFC3339, ea); err == nil {
				earliestArrival = t
			}
		}
		if la, ok := taskData["latest_arrival"].(string); ok && la != "" {
			if t, err := time.Parse(time.RFC3339, la); err == nil {
				latestArrival = t
			}
		}

		// Photo required defaults to true for service tasks
		photoRequired := false
		if pr, ok := taskData["photo_required"].(bool); ok {
			photoRequired = pr
		} else if taskType == "service" {
			photoRequired = true
		}

		_, err = tx.Exec(
			taskQuery,
			taskID, shiftID, i+1, taskType, lat, lon,
			getString("address"),
			getString("bin_id"), getInt("bin_number"), getInt("fill_percentage"),
			getString("potential_location_id"), getString("new_bin_number"), placementSource,
			getString("move_request_id"), getFloat("destination_latitude"),
			getFloat("destination_longitude"), getString("destination_address"), getString("move_type"),
			getString("warehouse_action"), getInt("bins_to_load"),
			getString("route_id"), taskDataJSON, now,
			getString("task_label"), getString("task_description"), photoRequired,
			earliestArrival, latestArrival, getString("time_window_type"),
			getInt("service_duration_seconds"),
		)
		if err != nil {
			return "", 0, fmt.Errorf("failed to create task %d: %w", i+1, err)
		}
	}

	// Insert warehouse deployment placement tasks (source="warehouse")
	// ❌ REMOVED: warehouse_stop "load" task creation
	// Mapbox will automatically route to warehouse for placements - no explicit load stop needed
	nextSeq := len(tasks) + 1
	if len(warehouseDeployments) > 0 {
		log.Printf("🏭 Inserting %d warehouse deployment placement task(s)...", len(warehouseDeployments))
		log.Printf("   ℹ️  Mapbox will automatically handle warehouse pickup routing")

		// Create placement tasks: one per deployment bin, with placement_source="warehouse"
		// Mapbox treats these as shipments (pickup at warehouse, dropoff at destination)
		for _, d := range warehouseDeployments {
			placeSrc := "warehouse"
			addr := d.DestinationAddress
			depTaskID := uuid.New().String()
			_, err = tx.Exec(
				taskQuery,
				depTaskID, shiftID, nextSeq, "placement",
				d.DestinationLatitude, d.DestinationLongitude, addr,
				d.BinID, d.BinNumber, nil, // bin_id (existing bin), bin_number, fill_percentage
				nil, nil, placeSrc,        // potential_location_id, new_bin_number, placement_source
				nil, nil, nil, nil, nil,   // move_request fields
				nil, nil,                  // warehouse_action, bins_to_load
				nil, []byte("{}"), now,
				nil, nil, false,           // task_label, task_description, photo_required
				nil, nil, nil,             // earliest_arrival, latest_arrival, time_window_type
				nil,                       // service_duration_seconds
			)
			if err != nil {
				return "", 0, fmt.Errorf("failed to create warehouse deployment placement task: %w", err)
			}
			log.Printf("   ✅ Deployment placement inserted (seq %d, bin #%d → %s)", nextSeq, d.BinNumber, d.DestinationAddress)
			nextSeq++
		}

		totalBins += len(warehouseDeployments) // Only count placements, not warehouse_stop
	}

	// ❌ REMOVED: Auto-append warehouse stop logic
	// Mapbox Optimization v2 will automatically add warehouse stops (end stops) as needed
	// Let the optimizer handle warehouse returns for optimal routing
	log.Printf("✅ Created shift with %d tasks - Mapbox will add warehouse stops during optimization", len(tasks))

	err = tx.Commit()
	if err != nil {
		return "", 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("✅ Created shift %s with %d tasks", shiftID, len(tasks))
	return shiftID, len(tasks), nil
}

// CompleteTask marks a task as completed
// Photo is stored in the checks table (not route_tasks) — see shifts.go CompleteTask handler
func CompleteTask(
	db *sqlx.DB,
	taskID string,
	updatedFillPercentage *int,
	completionNotes *string,
) error {
	now := time.Now().Unix()

	query := `
		UPDATE route_tasks
		SET is_completed = 1,
		    completed_at = $1,
		    updated_fill_percentage = $2,
		    updated_at = $3,
		    completion_notes = $4
		WHERE id = $5
	`

	result, err := db.Exec(query, now, updatedFillPercentage, now, completionNotes, taskID)
	if err != nil {
		return fmt.Errorf("failed to complete task: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("task not found: %s", taskID)
	}

	log.Printf("✅ Task %s marked as completed", taskID)
	return nil
}

// GetNextIncompleteTask gets the next task to complete in a shift
func GetNextIncompleteTask(db *sqlx.DB, shiftID string) (*models.RouteTask, error) {
	var task models.RouteTask
	query := `
		SELECT * FROM route_tasks
		WHERE shift_id = $1 AND is_completed = 0 AND is_deleted = FALSE
		ORDER BY sequence_order ASC
		LIMIT 1
	`

	err := db.Get(&task, query, shiftID)
	if err == sql.ErrNoRows {
		return nil, nil // No incomplete tasks
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get next task: %w", err)
	}

	return &task, nil
}

// GetTaskByID retrieves a single task by its ID
func GetTaskByID(db *sqlx.DB, taskID string) (*models.RouteTask, error) {
	var task models.RouteTask
	query := `SELECT * FROM route_tasks WHERE id = $1`

	err := db.Get(&task, query, taskID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get task: %w", err)
	}

	return &task, nil
}

// DeleteShiftTasks deletes all tasks for a shift
func DeleteShiftTasks(db *sqlx.DB, shiftID string) error {
	query := `DELETE FROM route_tasks WHERE shift_id = $1`

	result, err := db.Exec(query, shiftID)
	if err != nil {
		return fmt.Errorf("failed to delete shift tasks: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	log.Printf("🗑️  Deleted %d tasks for shift %s", rowsAffected, shiftID)
	return nil
}
