package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/services/redis"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// ActiveShiftDependency represents a dependency on an active shift
type ActiveShiftDependency struct {
	ShiftID      string   `json:"shift_id"`
	ShiftDate    string   `json:"shift_date_iso"`
	DriverID     string   `json:"driver_id"`
	DriverName   string   `json:"driver_name"`
	Status       string   `json:"status"`
	AffectedTasks []AffectedTask `json:"affected_tasks"`

	// Proximity information (for warning about driver being nearby)
	CurrentTaskID         *string  `json:"current_task_id,omitempty"`
	CurrentTaskAddress    *string  `json:"current_task_address,omitempty"`
	CurrentTaskBinNumber  *int     `json:"current_task_bin_number,omitempty"`
	DriverDistanceMiles   *float64 `json:"driver_distance_miles,omitempty"`
	LocationAgeSeconds    *int64   `json:"location_age_seconds,omitempty"`
	IsDriverNearby        bool     `json:"is_driver_nearby"`
}

// AffectedTask represents a task that would be affected by the change
type AffectedTask struct {
	TaskID       string `json:"task_id"`
	TaskType     string `json:"task_type"`
	SequenceOrder int   `json:"sequence_order"`
	Address      string `json:"address"`
	BinID        *string `json:"bin_id,omitempty"`
	MoveRequestID *string `json:"move_request_id,omitempty"`
}

// CheckBinDependencies checks if a bin is referenced in any active shifts
// GET /api/bins/:id/active-shift-dependencies
func CheckBinDependencies(db *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "id")

		var dependencies []ActiveShiftDependency

		// Query for active shifts with tasks referencing this bin
		query := `
			SELECT
				s.id as shift_id,
				s.shift_date_iso,
				s.driver_id,
				u.name as driver_name,
				s.status,
				rt.id as task_id,
				rt.task_type,
				rt.sequence_order,
				rt.address
			FROM route_tasks rt
			JOIN shifts s ON rt.shift_id = s.id
			JOIN users u ON s.driver_id = u.id
			WHERE rt.bin_id = $1
			  AND rt.is_completed = 0
			  AND s.status IN ('active', 'scheduled')
			ORDER BY s.shift_date_iso ASC, rt.sequence_order ASC
		`

		rows, err := db.Query(query, binID)
		if err != nil {
			http.Error(w, "Failed to check dependencies", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		// Group tasks by shift
		shiftMap := make(map[string]*ActiveShiftDependency)

		for rows.Next() {
			var (
				shiftID       string
				shiftDateISO  string
				driverID      string
				driverName    string
				status        string
				taskID        string
				taskType      string
				sequenceOrder int
				address       string
			)

			err := rows.Scan(&shiftID, &shiftDateISO, &driverID, &driverName, &status,
				&taskID, &taskType, &sequenceOrder, &address)
			if err != nil {
				continue
			}

			// Get or create shift dependency
			if _, exists := shiftMap[shiftID]; !exists {
				shiftMap[shiftID] = &ActiveShiftDependency{
					ShiftID:    shiftID,
					ShiftDate:  shiftDateISO,
					DriverID:   driverID,
					DriverName: driverName,
					Status:     status,
					AffectedTasks: []AffectedTask{},
				}
			}

			// Add task to shift
			shiftMap[shiftID].AffectedTasks = append(shiftMap[shiftID].AffectedTasks, AffectedTask{
				TaskID:       taskID,
				TaskType:     taskType,
				SequenceOrder: sequenceOrder,
				Address:      address,
				BinID:        &binID,
			})
		}

		// Calculate proximity information for each shift
		if redisClient != nil {
			ctx := context.Background()
			for shiftID, dep := range shiftMap {
				// Get driver's current location from Redis
				locationJSON, err := redisClient.GetDriverLocation(ctx, dep.DriverID)
				if err != nil {
					// Driver location not available - skip proximity calculation
					log.Printf("⚠️  [CheckBinDependencies] No location for driver %s: %v", dep.DriverID, err)
					continue
				}

				// Parse location
				var driverLoc struct {
					Latitude  float64 `json:"latitude"`
					Longitude float64 `json:"longitude"`
					Timestamp int64   `json:"timestamp"`
				}
				if err := json.Unmarshal([]byte(locationJSON), &driverLoc); err != nil {
					log.Printf("⚠️  [CheckBinDependencies] Failed to parse location for driver %s: %v", dep.DriverID, err)
					continue
				}

				// Get current task (first uncompleted task for this shift)
				var currentTask struct {
					ID        string   `db:"id"`
					Address   *string  `db:"address"`
					BinNumber *int     `db:"bin_number"`
					Latitude  float64  `db:"latitude"`
					Longitude float64  `db:"longitude"`
				}
				err = db.Get(&currentTask, `
					SELECT id, address, bin_number, latitude, longitude
					FROM route_tasks
					WHERE shift_id = $1 AND is_completed = 0
					ORDER BY sequence_order ASC
					LIMIT 1
				`, shiftID)

				if err != nil {
					// No current task found - skip proximity calculation
					log.Printf("⚠️  [CheckBinDependencies] No current task for shift %s: %v", shiftID, err)
					continue
				}

				// Calculate distance using haversine formula
				distanceKm := haversineDistance(
					driverLoc.Latitude, driverLoc.Longitude,
					currentTask.Latitude, currentTask.Longitude,
				)
				distanceMiles := distanceKm * 0.621371 // Convert to miles

				// Calculate location age
				now := time.Now().Unix()
				locationAge := now - (driverLoc.Timestamp / 1000) // Convert ms to seconds

				// Determine if driver is nearby (within 5 miles and location is fresh)
				isNearby := distanceMiles <= 5.0 && locationAge < 300 // 5 minutes

				// Populate proximity fields
				dep.CurrentTaskID = &currentTask.ID
				dep.CurrentTaskAddress = currentTask.Address
				dep.CurrentTaskBinNumber = currentTask.BinNumber
				dep.DriverDistanceMiles = &distanceMiles
				dep.LocationAgeSeconds = &locationAge
				dep.IsDriverNearby = isNearby

				log.Printf("📍 [CheckBinDependencies] Driver %s: %.2f miles from current task (nearby=%v, age=%ds)",
					dep.DriverName, distanceMiles, isNearby, locationAge)
			}
		}

		// Convert map to slice
		for _, dep := range shiftMap {
			dependencies = append(dependencies, *dep)
		}

		// Return empty array if no dependencies
		if dependencies == nil {
			dependencies = []ActiveShiftDependency{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dependencies)
	}
}

// CheckMoveRequestDependencies checks if a move request is referenced in any active shifts
// GET /api/manager/bins/move-requests/:id/active-shift-dependencies
func CheckMoveRequestDependencies(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		moveRequestID := chi.URLParam(r, "id")

		var dependencies []ActiveShiftDependency

		// Query for active shifts with tasks referencing this move request
		query := `
			SELECT
				s.id as shift_id,
				s.shift_date_iso,
				s.driver_id,
				u.name as driver_name,
				s.status,
				rt.id as task_id,
				rt.task_type,
				rt.sequence_order,
				rt.address
			FROM route_tasks rt
			JOIN shifts s ON rt.shift_id = s.id
			JOIN users u ON s.driver_id = u.id
			WHERE rt.move_request_id = $1
			  AND rt.is_completed = 0
			  AND s.status IN ('active', 'scheduled')
			ORDER BY s.shift_date_iso ASC, rt.sequence_order ASC
		`

		rows, err := db.Query(query, moveRequestID)
		if err != nil {
			http.Error(w, "Failed to check dependencies", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		// Group tasks by shift
		shiftMap := make(map[string]*ActiveShiftDependency)

		for rows.Next() {
			var (
				shiftID       string
				shiftDateISO  string
				driverID      string
				driverName    string
				status        string
				taskID        string
				taskType      string
				sequenceOrder int
				address       string
			)

			err := rows.Scan(&shiftID, &shiftDateISO, &driverID, &driverName, &status,
				&taskID, &taskType, &sequenceOrder, &address)
			if err != nil {
				continue
			}

			// Get or create shift dependency
			if _, exists := shiftMap[shiftID]; !exists {
				shiftMap[shiftID] = &ActiveShiftDependency{
					ShiftID:    shiftID,
					ShiftDate:  shiftDateISO,
					DriverID:   driverID,
					DriverName: driverName,
					Status:     status,
					AffectedTasks: []AffectedTask{},
				}
			}

			// Add task to shift
			shiftMap[shiftID].AffectedTasks = append(shiftMap[shiftID].AffectedTasks, AffectedTask{
				TaskID:        taskID,
				TaskType:      taskType,
				SequenceOrder: sequenceOrder,
				Address:       address,
				MoveRequestID: &moveRequestID,
			})
		}

		// Convert map to slice
		for _, dep := range shiftMap {
			dependencies = append(dependencies, *dep)
		}

		// Return empty array if no dependencies
		if dependencies == nil {
			dependencies = []ActiveShiftDependency{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dependencies)
	}
}

// CheckPotentialLocationDependencies checks if a potential location is referenced in any active shifts
// GET /api/potential-locations/:id/active-shift-dependencies
func CheckPotentialLocationDependencies(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		potentialLocationID := chi.URLParam(r, "id")

		var dependencies []ActiveShiftDependency

		// Query for active shifts with placement tasks referencing this potential location
		query := `
			SELECT
				s.id as shift_id,
				s.shift_date_iso,
				s.driver_id,
				u.name as driver_name,
				s.status,
				rt.id as task_id,
				rt.task_type,
				rt.sequence_order,
				rt.address
			FROM route_tasks rt
			JOIN shifts s ON rt.shift_id = s.id
			JOIN users u ON s.driver_id = u.id
			WHERE rt.potential_location_id = $1
			  AND rt.task_type = 'placement'
			  AND rt.is_completed = 0
			  AND s.status IN ('active', 'scheduled')
			ORDER BY s.shift_date_iso ASC, rt.sequence_order ASC
		`

		rows, err := db.Query(query, potentialLocationID)
		if err != nil {
			http.Error(w, "Failed to check dependencies", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		// Group tasks by shift
		shiftMap := make(map[string]*ActiveShiftDependency)

		for rows.Next() {
			var (
				shiftID       string
				shiftDateISO  string
				driverID      string
				driverName    string
				status        string
				taskID        string
				taskType      string
				sequenceOrder int
				address       string
			)

			err := rows.Scan(&shiftID, &shiftDateISO, &driverID, &driverName, &status,
				&taskID, &taskType, &sequenceOrder, &address)
			if err != nil {
				continue
			}

			// Get or create shift dependency
			if _, exists := shiftMap[shiftID]; !exists {
				shiftMap[shiftID] = &ActiveShiftDependency{
					ShiftID:    shiftID,
					ShiftDate:  shiftDateISO,
					DriverID:   driverID,
					DriverName: driverName,
					Status:     status,
					AffectedTasks: []AffectedTask{},
				}
			}

			// Add task to shift
			shiftMap[shiftID].AffectedTasks = append(shiftMap[shiftID].AffectedTasks, AffectedTask{
				TaskID:       taskID,
				TaskType:     taskType,
				SequenceOrder: sequenceOrder,
				Address:      address,
			})
		}

		// Convert map to slice
		for _, dep := range shiftMap {
			dependencies = append(dependencies, *dep)
		}

		// Return empty array if no dependencies
		if dependencies == nil {
			dependencies = []ActiveShiftDependency{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dependencies)
	}
}
