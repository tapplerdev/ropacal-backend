package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/models"
)

// GetShiftTasks retrieves all tasks for a shift ordered by sequence
func GetShiftTasks(db *sqlx.DB, shiftID string) ([]models.RouteTask, error) {
	var tasks []models.RouteTask
	query := `SELECT * FROM route_tasks
	          WHERE shift_id = $1
	          ORDER BY sequence_order ASC`

	err := db.Select(&tasks, query, shiftID)
	if err != nil {
		return nil, fmt.Errorf("failed to get shift tasks: %w", err)
	}

	return tasks, nil
}

// GetShiftTasksDetailed retrieves tasks with JOINed data from related tables
func GetShiftTasksDetailed(db *sqlx.DB, shiftID string) ([]map[string]interface{}, error) {
	query := `SELECT * FROM shift_tasks_detailed WHERE shift_id = $1 ORDER BY sequence_order ASC`

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

	return tasks, nil
}

// CreateShiftWithTasks creates a shift and its tasks in a transaction
func CreateShiftWithTasks(
	db *sqlx.DB,
	driverID string,
	truckBinCapacity int,
	warehouseLat, warehouseLon float64,
	warehouseAddr string,
	tasks []map[string]interface{},
) (string, int, error) {
	tx, err := db.Beginx()
	if err != nil {
		return "", 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Create shift
	shiftID := uuid.New().String()
	now := time.Now().Unix()

	shiftQuery := `
		INSERT INTO shifts (
			id, driver_id, status, total_bins, completed_bins,
			truck_bin_capacity, warehouse_latitude, warehouse_longitude, warehouse_address,
			created_at, updated_at
		) VALUES ($1, $2, 'ready', $3, 0, $4, $5, $6, $7, $8, $9)
	`

	totalBins := len(tasks)
	_, err = tx.Exec(
		shiftQuery,
		shiftID, driverID, totalBins, truckBinCapacity,
		warehouseLat, warehouseLon, warehouseAddr,
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
			potential_location_id, new_bin_number,
			move_request_id, destination_latitude, destination_longitude, destination_address, move_type,
			warehouse_action, bins_to_load,
			route_id, task_data, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10,
			$11, $12,
			$13, $14, $15, $16, $17,
			$18, $19,
			$20, $21, $22
		)
	`

	for i, taskData := range tasks {
		taskID := uuid.New().String()

		// Extract task fields with nil safety
		taskType, _ := taskData["task_type"].(string)
		lat, _ := taskData["latitude"].(float64)
		lon, _ := taskData["longitude"].(float64)

		// Convert task_data to JSON if present
		var taskDataJSON []byte
		if td, ok := taskData["task_data"]; ok && td != nil {
			taskDataJSON, _ = json.Marshal(td)
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
				// Handle both int and float64 from JSON
				switch v := val.(type) {
				case float64:
					return int(v)
				case int:
					return v
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

		_, err = tx.Exec(
			taskQuery,
			taskID, shiftID, i+1, taskType, lat, lon,
			getString("address"),
			getString("bin_id"), getInt("bin_number"), getInt("fill_percentage"),
			getString("potential_location_id"), getString("new_bin_number"),
			getString("move_request_id"), getFloat("destination_latitude"),
			getFloat("destination_longitude"), getString("destination_address"), getString("move_type"),
			getString("warehouse_action"), getInt("bins_to_load"),
			getString("route_id"), taskDataJSON, now,
		)
		if err != nil {
			return "", 0, fmt.Errorf("failed to create task %d: %w", i+1, err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return "", 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("✅ Created shift %s with %d tasks", shiftID, len(tasks))
	return shiftID, len(tasks), nil
}

// CompleteTask marks a task as completed
func CompleteTask(
	db *sqlx.DB,
	taskID string,
	updatedFillPercentage *int,
	photoURL *string,
	newBinID *string,
) error {
	now := time.Now().Unix()

	query := `
		UPDATE route_tasks
		SET is_completed = 1,
		    completed_at = $1,
		    updated_fill_percentage = $2,
		    updated_at = $3
		WHERE id = $4
	`

	result, err := db.Exec(query, now, updatedFillPercentage, now, taskID)
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
		WHERE shift_id = $1 AND is_completed = 0
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
