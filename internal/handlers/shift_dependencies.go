package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/services/redis"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// ActiveShiftDependency represents a dependency on an active shift
type ActiveShiftDependency struct {
	ShiftID       string         `json:"shift_id"`
	ShiftDate     string         `json:"shift_date_iso"`
	DriverID      string         `json:"driver_id"`
	DriverName    string         `json:"driver_name"`
	Status        string         `json:"status"`
	AffectedTasks []AffectedTask `json:"affected_tasks"`

	// Proximity information (for warning about driver being nearby)
	CurrentTaskID        *string  `json:"current_task_id,omitempty"`
	CurrentTaskAddress   *string  `json:"current_task_address,omitempty"`
	CurrentTaskBinNumber *int     `json:"current_task_bin_number,omitempty"`
	DriverDistanceMiles  *float64 `json:"driver_distance_miles,omitempty"`
	LocationAgeSeconds   *int64   `json:"location_age_seconds,omitempty"`
	IsDriverNearby       bool     `json:"is_driver_nearby"`
}

// AffectedTask represents a task that would be affected by the change
type AffectedTask struct {
	TaskID        string  `json:"task_id"`
	TaskType      string  `json:"task_type"`
	SequenceOrder int     `json:"sequence_order"`
	Address       string  `json:"address"`
	BinID         *string `json:"bin_id,omitempty"`
	MoveRequestID *string `json:"move_request_id,omitempty"`
}

// queryActiveShiftDependencies runs the shared active-shift dependency query and
// groups the resulting tasks by shift. filterClause is the parameterized SQL
// predicate selecting the relevant route_tasks (e.g. "rt.bin_id = $1"); arg is
// the single bind value for that clause. enrichTask, when non-nil, is invoked
// for each scanned task so callers can attach entity-specific fields (e.g. the
// originating bin or move-request ID). The returned map is keyed by shift ID.
func queryActiveShiftDependencies(
	w http.ResponseWriter,
	db *orgdb.DB,
	filterClause string,
	arg interface{},
	enrichTask func(task *AffectedTask),
) (map[string]*ActiveShiftDependency, bool) {
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
		WHERE ` + filterClause + `
		  AND rt.is_completed = 0
		  AND s.status IN ('active', 'scheduled')
		ORDER BY s.shift_date_iso ASC, rt.sequence_order ASC
	`

	rows, err := db.Query(query, arg)
	if err != nil {
		http.Error(w, "Failed to check dependencies", http.StatusInternalServerError)
		return nil, false
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

		if err := rows.Scan(&shiftID, &shiftDateISO, &driverID, &driverName, &status,
			&taskID, &taskType, &sequenceOrder, &address); err != nil {
			continue
		}

		// Get or create shift dependency
		if _, exists := shiftMap[shiftID]; !exists {
			shiftMap[shiftID] = &ActiveShiftDependency{
				ShiftID:       shiftID,
				ShiftDate:     shiftDateISO,
				DriverID:      driverID,
				DriverName:    driverName,
				Status:        status,
				AffectedTasks: []AffectedTask{},
			}
		}

		// Add task to shift
		task := AffectedTask{
			TaskID:        taskID,
			TaskType:      taskType,
			SequenceOrder: sequenceOrder,
			Address:       address,
		}
		if enrichTask != nil {
			enrichTask(&task)
		}
		shiftMap[shiftID].AffectedTasks = append(shiftMap[shiftID].AffectedTasks, task)
	}

	return shiftMap, true
}

// dependenciesFromShiftMap flattens the shift map into a non-nil slice so the
// JSON response is always an array rather than null.
func dependenciesFromShiftMap(shiftMap map[string]*ActiveShiftDependency) []ActiveShiftDependency {
	dependencies := []ActiveShiftDependency{}
	for _, dep := range shiftMap {
		dependencies = append(dependencies, *dep)
	}
	return dependencies
}

// CheckBinDependencies checks if a bin is referenced in any active shifts
// GET /api/bins/:id/active-shift-dependencies
func CheckBinDependencies(root *sqlx.DB, redisClient *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		binID := chi.URLParam(r, "id")

		shiftMap, ok := queryActiveShiftDependencies(w, db, "rt.bin_id = $1", binID, func(task *AffectedTask) {
			task.BinID = &binID
		})
		if !ok {
			return
		}

		// Calculate proximity information for each shift
		if redisClient != nil {
			ctx := context.Background()
			for shiftID, dep := range shiftMap {
				// Get driver's current location from Redis
				locationJSON, err := redisClient.GetDriverLocation(ctx, db.OrgID(), dep.DriverID)
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
					ID        string  `db:"id"`
					Address   *string `db:"address"`
					BinNumber *int    `db:"bin_number"`
					Latitude  float64 `db:"latitude"`
					Longitude float64 `db:"longitude"`
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

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dependenciesFromShiftMap(shiftMap))
	}
}

// CheckMoveRequestDependencies checks if a move request is referenced in any active shifts
// GET /api/manager/bins/move-requests/:id/active-shift-dependencies
func CheckMoveRequestDependencies(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		moveRequestID := chi.URLParam(r, "id")

		shiftMap, ok := queryActiveShiftDependencies(w, db, "rt.move_request_id = $1", moveRequestID, func(task *AffectedTask) {
			task.MoveRequestID = &moveRequestID
		})
		if !ok {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dependenciesFromShiftMap(shiftMap))
	}
}

// CheckPotentialLocationDependencies checks if a potential location is referenced in any active shifts
// GET /api/potential-locations/:id/active-shift-dependencies
func CheckPotentialLocationDependencies(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		potentialLocationID := chi.URLParam(r, "id")

		// Only placement tasks reference a potential location.
		shiftMap, ok := queryActiveShiftDependencies(w, db,
			"rt.potential_location_id = $1 AND rt.task_type = 'placement'",
			potentialLocationID, nil)
		if !ok {
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(dependenciesFromShiftMap(shiftMap))
	}
}
