package handlers

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"

	"ropacal-backend/internal/database"
	"ropacal-backend/internal/models"

	"github.com/jmoiron/sqlx"
)

// DriverShiftDetailResponse represents detailed shift information for a specific driver
type DriverShiftDetailResponse struct {
	DriverID          string                       `json:"driver_id"`
	DriverName        string                       `json:"driver_name"`
	ShiftID           string                       `json:"shift_id"`
	RouteID           *string                      `json:"route_id"`
	Status            string                       `json:"status"`
	StartTime         *int64                       `json:"start_time"`
	EndTime           *int64                       `json:"end_time"`
	TotalBins         int                          `json:"total_bins"`
	CompletedBins     int                          `json:"completed_bins"`
	TotalPauseSeconds int                          `json:"total_pause_seconds"`
	CreatedAt         int64                        `json:"created_at"`
	UpdatedAt         int64                        `json:"updated_at"`
	Bins              []models.ShiftBinWithDetails `json:"bins"`
	Tasks             []models.RouteTask           `json:"tasks"`
}

// GetDriverShiftDetails returns detailed shift information for a specific driver (manager view)
func GetDriverShiftDetails(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		driverID := r.URL.Query().Get("driver_id")
		if driverID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "driver_id is required",
			})
			return
		}

		log.Printf("📋 GetDriverShiftDetails: Fetching shift details for driver: %s", driverID)

		// First, get the active shift for this driver
		shiftQuery := `
		SELECT
			s.id AS shift_id,
			s.driver_id,
			u.name AS driver_name,
			s.route_id,
			s.status,
			s.start_time,
			s.end_time,
			s.total_bins,
			s.completed_bins,
			s.total_pause_seconds,
			s.created_at,
			s.updated_at
		FROM shifts s
		INNER JOIN users u ON s.driver_id = u.id
		WHERE s.driver_id = $1
			AND s.status IN ('ready', 'active', 'paused')
		ORDER BY s.created_at DESC
		LIMIT 1
	`

		var detail DriverShiftDetailResponse
		err := db.QueryRow(shiftQuery, driverID).Scan(
			&detail.ShiftID,
			&detail.DriverID,
			&detail.DriverName,
			&detail.RouteID,
			&detail.Status,
			&detail.StartTime,
			&detail.EndTime,
			&detail.TotalBins,
			&detail.CompletedBins,
			&detail.TotalPauseSeconds,
			&detail.CreatedAt,
			&detail.UpdatedAt,
		)

		if err == sql.ErrNoRows {
			log.Printf("⚠️  No active shift found for driver: %s", driverID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "No active shift found for this driver",
			})
			return
		}

		if err != nil {
			log.Printf("❌ Database error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch shift details",
			})
			return
		}

		// Get tasks using the shared function (migrated from route_tasks to route_tasks)
		bins, err := getShiftTasksWithDetails(db, detail.ShiftID)
		if err != nil {
			log.Printf("❌ Error fetching tasks: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"error":   "Failed to fetch shift tasks",
			})
			return
		}

		detail.Bins = bins

		// Also get tasks from route_tasks table (new task-based system)
		tasks, err := database.GetShiftTasks(db, detail.ShiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v (using bins only)", err)
			tasks = []models.RouteTask{} // Empty tasks array on error
		}

		detail.Tasks = tasks

		log.Printf("✅ Found shift with %d bins, %d tasks for driver: %s", len(bins), len(tasks), detail.DriverName)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    detail,
		})
	}
}
