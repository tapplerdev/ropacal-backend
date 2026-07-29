package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/orgdb"

	"github.com/google/uuid"
)

// ErrTaskDescriptor marks a create-with-tasks task descriptor the caller got
// wrong (bad shape or reference). Handlers map it to a 400 with the wrapped
// detail instead of a 500 — validate-at-boundary, never a silent skip.
var ErrTaskDescriptor = errors.New("invalid task descriptor")

// GetShiftTasks retrieves all active (non-deleted) tasks for a shift ordered by sequence
func GetShiftTasks(db *orgdb.DB, shiftID string) ([]models.RouteTask, error) {
	var tasks []models.RouteTask
	query := `
		SELECT
			rt.*,
			COALESCE(c.photo_url, rt.photo_url) AS photo_url
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
func GetShiftTasksWithDeleted(db *orgdb.DB, shiftID string) ([]models.RouteTask, error) {
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
			rt.added_by,
			rt.addition_reason,
			rt.task_data,
			rt.created_at,
			rt.updated_at,
			COALESCE(c.photo_url, rt.photo_url) AS photo_url,
			rt.after_photo_url,
			rt.task_label,
			rt.task_description,
			rt.photo_required,
			rt.completion_notes
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
func GetShiftTasksDetailed(db *orgdb.DB, shiftID string) ([]map[string]interface{}, error) {
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
			rt.placement_source,
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
			COALESCE(c.photo_url, rt.photo_url) AS photo_url,
			rt.after_photo_url,
			rt.task_label,
			rt.task_description,
			rt.photo_required,
			rt.completion_notes,
			inc.incident_type,
			inc.incident_photo_url
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
		-- Latest incident reported on THIS shift for this bin (inaccessible/damaged/etc.).
		-- An incident completion has no fill/photo, so the shift view shows the incident
		-- instead of a misleading "0% Fill" on a stop the driver couldn't service.
		LEFT JOIN LATERAL (
			SELECT zi.incident_type, zi.photo_url AS incident_photo_url
			FROM zone_incidents zi
			WHERE zi.shift_id = rt.shift_id AND zi.bin_id = rt.bin_id
			ORDER BY zi.reported_at DESC
			LIMIT 1
		) inc ON true
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

	// MapScan hands uuid/numeric/jsonb columns back as []byte, which
	// encoding/json then emits as BASE64 — consumers were receiving
	// id="ZTBiMmUy..." and latitude="MzcuNjE1..." instead of plain values.
	// (Both known consumers grew defensive decoders around this over time —
	// RemoveTasksFromShift's base64-decode-with-fallback and the app's
	// route_task extension fallback loop — while driver endpoints without a
	// decoder 500'd on the base64 ids: invalid uuid at the WHERE clause.)
	// Normalize at the SOURCE: bytes → string, coordinates → numbers. The
	// downstream decoders keep working: a plain UUID contains '-' (not in the
	// base64 alphabet), so their decode fails and falls back to as-is.
	numericKeys := map[string]bool{
		"latitude": true, "longitude": true,
		"destination_latitude": true, "destination_longitude": true,
	}
	for _, task := range tasks {
		for k, v := range task {
			b, ok := v.([]byte)
			if !ok {
				continue
			}
			s := string(b)
			if numericKeys[k] {
				if f, perr := strconv.ParseFloat(s, 64); perr == nil {
					task[k] = f
					continue
				}
			}
			task[k] = s
		}
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

// CreateShiftWithTasks creates a shift and its tasks in a transaction.
// Warehouse deployments are minted as real redeployment move requests in the
// same transaction; their ids are returned so the handler can log history
// post-commit. managerID stamps requested_by on those minted moves.
func CreateShiftWithTasks(
	db *orgdb.DB,
	driverID string,
	managerID string,
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
	// Schedule fields (vehicle time constraints)
	scheduledStart *string,
	scheduledEnd *string,
	// Date this shift is scheduled for
	scheduledDate *string,
	// Route template ID (if created from a template)
	routeID *string,
) (string, int, []string, []models.SkippedBin, error) {
	tx, err := db.Beginx()
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Default shift_type to "standard"
	if shiftType == "" {
		shiftType = "standard"
	}

	// Create shift
	shiftID := uuid.New().String()
	now := time.Now().Unix()

	// Default scheduled_date to today if not provided (use Pacific time, not UTC)
	if scheduledDate == nil {
		pacific, _ := time.LoadLocation("America/Los_Angeles")
		today := time.Now().In(pacific).Format("2006-01-02")
		scheduledDate = &today
	}

	// Derive the shift-level route_id when the client omitted it but the
	// tasks carry a consistent template id (the mobile app sends route_id
	// per task only) — otherwise app-created template shifts are invisible
	// to template performance stats, which aggregate shift_history.route_id.
	if routeID == nil {
		derived := ""
		consistent := true
		for _, t := range tasks {
			if v, ok := t["route_id"].(string); ok && v != "" {
				if derived == "" {
					derived = v
				} else if derived != v {
					consistent = false
					break
				}
			}
		}
		if consistent && derived != "" {
			routeID = &derived
		}
	}

	shiftQuery := `
		INSERT INTO shifts (
			id, driver_id, status, total_bins, completed_bins,
			truck_bin_capacity, warehouse_latitude, warehouse_longitude, warehouse_address,
			lock_route_order, shift_type, shift_label,
			start_latitude, start_longitude, start_address,
			end_latitude, end_longitude, end_address,
			scheduled_start, scheduled_end, scheduled_date,
			route_id, created_at, updated_at
		) VALUES ($1, $2, 'ready', $3, 0, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
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
		scheduledStart, scheduledEnd, scheduledDate,
		routeID, now, now,
	)
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("failed to create shift: %w", err)
	}

	// Create tasks — the INSERT itself lives in the itinerary domain
	// (itinerary.InsertCreatedTask, the shift-birth 30-column contract);
	// this loop keeps the legacy client-first enrichment/merge semantics.
	skippedInactive := 0
	var skippedBins []models.SkippedBin
	for i, taskData := range tasks {
		taskID := uuid.New().String()

		// Extract task fields with nil safety
		taskType, _ := taskData["task_type"].(string)
		lat, _ := taskData["latitude"].(float64)
		lon, _ := taskData["longitude"].(float64)

		log.Printf("   🔍 Task #%d DEBUG: Received task_type='%s', lat=%.6f, lon=%.6f", i+1, taskType, lat, lon)

		// Phase 2: a placement descriptor carrying a move is a REDEPLOYMENT
		// ({task_type:'placement', move_request_id} — the shape dashboard
		// transfers send). It cannot ride the generic path below: its bin is
		// in_storage (which the inactive-bin guard would silently skip — the
		// shell-shift bug) and the row + move assignment belong to the move
		// domain. Mint via itinerary.AddMove and repoint the move in this tx.
		if taskType == "placement" {
			if moveReqID, _ := taskData["move_request_id"].(string); moveReqID != "" {
				mrData, mrErr := itinerary.ResolveMoveEnrichment(tx, moveReqID)
				if mrErr != nil {
					if errors.Is(mrErr, sql.ErrNoRows) {
						return "", 0, nil, nil, fmt.Errorf("%w: task #%d references unknown move request %s", ErrTaskDescriptor, i+1, moveReqID)
					}
					// Infra failure, not a caller mistake — surface as a 500 with cause.
					return "", 0, nil, nil, fmt.Errorf("resolve move %s (task #%d): %w", moveReqID, i+1, mrErr)
				}
				if mrData.MoveType != "redeployment" {
					return "", 0, nil, nil, fmt.Errorf("%w: task #%d: move %s is a %s — send its pickup/dropoff legs, not a placement", ErrTaskDescriptor, i+1, moveReqID, mrData.MoveType)
				}
				if mrData.NewLatitude == nil || mrData.NewLongitude == nil {
					return "", 0, nil, nil, fmt.Errorf("%w: task #%d: redeployment %s has no destination coordinates", ErrTaskDescriptor, i+1, moveReqID)
				}
				if err := moverequest.AssignToShift(tx, moveReqID, shiftID, string(moverequest.StatusAssigned), now); err != nil {
					if errors.Is(err, moverequest.ErrInvalidTransition) {
						return "", 0, nil, nil, fmt.Errorf("%w: task #%d: move %s cannot be assigned (already completed or cancelled)", ErrTaskDescriptor, i+1, moveReqID)
					}
					return "", 0, nil, nil, fmt.Errorf("assign move %s (task #%d): %w", moveReqID, i+1, err)
				}
				binNum, _ := itinerary.ResolveBinNumber(tx, mrData.BinID)
				destAddr := ""
				if mrData.NewAddress != nil {
					destAddr = *mrData.NewAddress
				}
				if _, err := itinerary.AddMove(tx, shiftID, itinerary.MovePlacement{
					InsertSeq:      i + 1,
					MoveRequestID:  moveReqID,
					BinID:          mrData.BinID,
					BinNumber:      binNum,
					MoveType:       "redeployment",
					DropoffLat:     *mrData.NewLatitude,
					DropoffLng:     *mrData.NewLongitude,
					DropoffAddress: destAddr,
					Now:            now,
				}); err != nil {
					return "", 0, nil, nil, fmt.Errorf("failed to add redeployment placement (task #%d): %w", i+1, err)
				}
				log.Printf("   ✅ Task #%d: Redeployment placement minted for move %s (bin #%d)", i+1, moveReqID, binNum)
				continue
			}
			// A placement must come from SOMEWHERE — a potential location (new
			// bin), a move (redeployment), or explicit coordinates. Nothing at
			// all used to silently insert a null-island row.
			if plID, _ := taskData["potential_location_id"].(string); plID == "" && lat == 0 && lon == 0 {
				return "", 0, nil, nil, fmt.Errorf("%w: task #%d: placement requires potential_location_id, move_request_id, or coordinates", ErrTaskDescriptor, i+1)
			}
		}

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
						binCoords, err := itinerary.ResolveBinCoords(tx, binID)
						if err == nil && binCoords.Latitude != 0 && binCoords.Longitude != 0 {
							lat = binCoords.Latitude
							lon = binCoords.Longitude
							// Persist into taskData: latitude/longitude are re-extracted
							// from taskData just before insert, which used to discard
							// this fix and insert the pickup at 0,0 (null island) —
							// feeding the optimizer matrix and app nav bad coordinates.
							taskData["latitude"] = lat
							taskData["longitude"] = lon
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
			// Persist into taskData (see the pickup fix above): the pre-insert
			// re-extract used to discard this override and insert warehouse
			// stops at 0,0 when the client sent no coordinates.
			taskData["latitude"] = lat
			taskData["longitude"] = lon
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
				// Safety net: skip retired/missing/warehoused bins — and RECORD the
				// skip so the response can surface it (was a log-only silent drop).
				binStatus, err := itinerary.ResolveBinStatus(tx, binID)
				if err == nil {
					if binStatus == "retired" || binStatus == "missing" || binStatus == "in_storage" {
						log.Printf("   ⚠️  Task #%d: Skipping %s bin %s (status=%s)", i+1, taskType, binID, binStatus)
						binNum, _ := itinerary.ResolveBinNumber(tx, binID)
						skippedBins = append(skippedBins, models.SkippedBin{
							BinID: binID, BinNumber: binNum, Status: binStatus,
						})
						skippedInactive++
						continue
					}
				}

				// Auto-populate bin details from bins table if missing
				_, hasBinNumber := taskData["bin_number"]
				_, hasFillPercentage := taskData["fill_percentage"]
				_, hasLatitude := taskData["latitude"]
				_, hasLongitude := taskData["longitude"]
				_, hasAddress := taskData["address"]

				if !hasBinNumber || !hasFillPercentage || !hasLatitude || !hasLongitude || !hasAddress {
					binData, err := itinerary.ResolveBinEnrichment(tx, binID)
					if err == nil {
						if !hasBinNumber {
							taskData["bin_number"] = binData.BinNumber
							log.Printf("   ✅ Task #%d: Auto-populated bin_number=%d from bins table", i+1, binData.BinNumber)
						}
						if !hasFillPercentage {
							taskData["fill_percentage"] = binData.FillPercentage
							log.Printf("   ✅ Task #%d: Auto-populated fill_percentage=%d from bins table", i+1, binData.FillPercentage)
						}
						if !hasLatitude {
							taskData["latitude"] = binData.Latitude
						}
						if !hasLongitude {
							taskData["longitude"] = binData.Longitude
						}
						if !hasAddress {
							taskData["address"] = binData.ComposedAddress()
						}
						if !hasLatitude || !hasLongitude || !hasAddress {
							log.Printf("   ✅ Task #%d: Auto-populated location (%.6f, %.6f) %s", i+1, binData.Latitude, binData.Longitude, binData.CurrentStreet)
						}
					} else {
						log.Printf("   ⚠️  Task #%d: Failed to lookup bin data for %s: %v", i+1, binID, err)
					}
				}
			}
		}

		// Auto-populate potential location coordinates if missing
		if plIDInterface, ok := taskData["potential_location_id"]; ok && plIDInterface != nil {
			plID, _ := plIDInterface.(string)
			if plID != "" {
				lat, hasLat := taskData["latitude"].(float64)
				lon, hasLon := taskData["longitude"].(float64)
				_, hasAddr := taskData["address"]

				if !hasLat || !hasLon || !hasAddr || lat == 0 || lon == 0 {
					plData, err := itinerary.ResolvePotentialLocationEnrichment(tx, plID)
					if err == nil && plData.Latitude != 0 {
						taskData["latitude"] = plData.Latitude
						taskData["longitude"] = plData.Longitude
						if !hasAddr {
							taskData["address"] = plData.ComposedAddress()
						}
						log.Printf("   ✅ Task #%d: Auto-populated from potential location (%.6f, %.6f) %s", i+1, plData.Latitude, plData.Longitude, plData.Street)
					} else if err != nil {
						log.Printf("   ⚠️  Task #%d: Failed to lookup potential location %s: %v", i+1, plID, err)
					}
				}
			}
		}

		// Auto-populate move request addresses if missing
		if moveReqIDInterface, ok := taskData["move_request_id"]; ok && moveReqIDInterface != nil {
			moveReqID, _ := moveReqIDInterface.(string)
			if moveReqID != "" {
				_, hasLat := taskData["latitude"]
				_, hasLng := taskData["longitude"]
				_, hasAddr := taskData["address"]
				_, hasDestAddr := taskData["destination_address"]
				_, hasDestLat := taskData["destination_latitude"]
				_, hasDestLng := taskData["destination_longitude"]

				if !hasLat || !hasLng || !hasAddr || !hasDestAddr || !hasDestLat || !hasDestLng {
					mrData, err := itinerary.ResolveMoveEnrichment(tx, moveReqID)
					if err == nil {

						// For "store" type: destination is ALWAYS the current warehouse
						// (move request may have stale warehouse address from when it was created)
						if mrData.MoveType == "store" {
							whCfg, cfgErr := itinerary.ResolveWarehouseConfig(tx)
							if cfgErr == nil && whCfg.Lat != 0 {
								mrData.NewLatitude = &whCfg.Lat
								mrData.NewLongitude = &whCfg.Lng
								mrData.NewAddress = &whCfg.Addr
								log.Printf("   📍 Task #%d: Store move — destination set to current warehouse (%s)", i+1, whCfg.Addr)
							}
						}

						// Get bin_number
						binNum, _ := itinerary.ResolveBinNumber(tx, mrData.BinID)
						if binNum > 0 {
							taskData["bin_number"] = binNum
							taskData["bin_id"] = mrData.BinID
						}

						if taskType == "pickup" {
							if !hasLat && mrData.OriginalLatitude != nil {
								taskData["latitude"] = *mrData.OriginalLatitude
							}
							if !hasLng && mrData.OriginalLongitude != nil {
								taskData["longitude"] = *mrData.OriginalLongitude
							}
							if !hasAddr && mrData.OriginalAddress != nil {
								taskData["address"] = *mrData.OriginalAddress
							}
							if !hasDestLat && mrData.NewLatitude != nil {
								taskData["destination_latitude"] = *mrData.NewLatitude
							}
							if !hasDestLng && mrData.NewLongitude != nil {
								taskData["destination_longitude"] = *mrData.NewLongitude
							}
							if !hasDestAddr && mrData.NewAddress != nil {
								taskData["destination_address"] = *mrData.NewAddress
							}
						} else if taskType == "dropoff" {
							if !hasLat && mrData.NewLatitude != nil {
								taskData["latitude"] = *mrData.NewLatitude
							}
							if !hasLng && mrData.NewLongitude != nil {
								taskData["longitude"] = *mrData.NewLongitude
							}
							if !hasAddr && mrData.NewAddress != nil {
								taskData["address"] = *mrData.NewAddress
							}
							if !hasDestLat && mrData.NewLatitude != nil {
								taskData["destination_latitude"] = *mrData.NewLatitude
							}
							if !hasDestLng && mrData.NewLongitude != nil {
								taskData["destination_longitude"] = *mrData.NewLongitude
							}
							if !hasDestAddr && mrData.NewAddress != nil {
								taskData["destination_address"] = *mrData.NewAddress
							}
						}
						log.Printf("   ✅ Task #%d: Auto-populated move request addresses from bin_move_requests", i+1)
					} else {
						log.Printf("   ⚠️  Task #%d: Failed to lookup move request %s: %v", i+1, moveReqID, err)
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

		// Re-extract lat/lon after auto-populate (may have been updated from bins/move_requests)
		lat, _ = taskData["latitude"].(float64)
		lon, _ = taskData["longitude"].(float64)

		err = itinerary.InsertCreatedTask(tx, shiftID, itinerary.CreatedTask{
			ID: taskID, Seq: i + 1, TaskType: taskType, Lat: lat, Lng: lon,
			Address:                getString("address"),
			BinID:                  getString("bin_id"),
			BinNumber:              getInt("bin_number"),
			FillPercentage:         getInt("fill_percentage"),
			PotentialLocationID:    getString("potential_location_id"),
			NewBinNumber:           getString("new_bin_number"),
			PlacementSource:        placementSource,
			MoveRequestID:          getString("move_request_id"),
			DestLat:                getFloat("destination_latitude"),
			DestLng:                getFloat("destination_longitude"),
			DestAddress:            getString("destination_address"),
			MoveType:               getString("move_type"),
			WarehouseAction:        getString("warehouse_action"),
			BinsToLoad:             getInt("bins_to_load"),
			RouteID:                getString("route_id"),
			TaskData:               taskDataJSON,
			CreatedAt:              now,
			TaskLabel:              getString("task_label"),
			TaskDescription:        getString("task_description"),
			PhotoRequired:          photoRequired,
			EarliestArrival:        earliestArrival,
			LatestArrival:          latestArrival,
			TimeWindowType:         getString("time_window_type"),
			ServiceDurationSeconds: getInt("service_duration_seconds"),
		})
		if err != nil {
			return "", 0, nil, nil, fmt.Errorf("failed to create task %d: %w", i+1, err)
		}
	}

	// Warehouse deployments: each becomes a REAL redeployment move request
	// (minted here, in the same tx) plus ONE placement task at the destination
	// (Phase 2: bin_id + placement_source='redeployment' + move_request_id) —
	// the optimizer's placement model sources the bin from the Load-N warehouse
	// run, and completing the placement finalizes the move. (Replaced both the
	// pre-#42 untracked placement_source='warehouse' rows AND the interim
	// pickup/dropoff pair shape.)
	//
	// The bin's status is deliberately NOT flipped to pending_move (unlike
	// ScheduleBinMove): it stays in_storage until the placement completes,
	// preserving warehouse-inventory visibility — the old deployment behavior.
	nextSeq := len(tasks) + 1
	mintedMoveIDs := make([]string, 0, len(warehouseDeployments))
	if len(warehouseDeployments) > 0 {
		if warehouseLat == nil || warehouseLon == nil {
			return "", 0, nil, nil, fmt.Errorf("warehouse location is required for warehouse deployments")
		}
		whAddr := ""
		if warehouseAddr != nil {
			whAddr = *warehouseAddr
		}
		log.Printf("🏭 Minting %d redeployment move(s) for warehouse deployment(s)...", len(warehouseDeployments))
		assignmentType := "shift"
		for _, d := range warehouseDeployments {
			moveID := uuid.New().String()
			notes := "Created via shift builder (warehouse deployment)"
			if err = moverequest.Insert(tx, &models.BinMoveRequest{
				ID:                moveID,
				BinID:             d.BinID, // existing in_storage bin
				ScheduledDate:     now,
				Urgency:           "scheduled",
				RequestedBy:       managerID,
				Status:            "assigned",
				OriginalLatitude:  *warehouseLat,
				OriginalLongitude: *warehouseLon,
				OriginalAddress:   whAddr,
				NewLatitude:       &d.DestinationLatitude,
				NewLongitude:      &d.DestinationLongitude,
				NewAddress:        &d.DestinationAddress,
				MoveType:          "redeployment",
				Notes:             &notes,
				AssignmentType:    &assignmentType,
				AssignedShiftID:   &shiftID,
				CreatedAt:         now,
				UpdatedAt:         now,
			}, nil); err != nil {
				// ErrOpenMoveExists surfaces here if the bin gained an open move
				// between the handler's pre-check and this insert (or the same bin
				// appears twice in one request) — the tx rolls back whole.
				return "", 0, nil, nil, fmt.Errorf("failed to create deployment move for bin #%d: %w", d.BinNumber, err)
			}

			// Phase 2: a redeployment rides the PLACEMENT rails — ONE placement task
			// at the destination (bin_id + placement_source='redeployment' +
			// move_request_id), NOT a pickup/dropoff pair. The warehouse fetch is not
			// a driver-visible leg: the optimizer's placement model sources the bin
			// from the one-tap "Load N bins" run automatically, and the driver
			// completes a placement-style "Place Bin #N" stop. The move request above
			// remains the manager-facing record (audit, one-open-move, cancel-revert)
			// and is finalized when this placement task completes.
			if err = itinerary.InsertCreatedTask(tx, shiftID, itinerary.CreatedTask{
				ID: uuid.New().String(), Seq: nextSeq, TaskType: "placement",
				Lat: d.DestinationLatitude, Lng: d.DestinationLongitude,
				Address:         d.DestinationAddress,
				BinID:           d.BinID,
				BinNumber:       d.BinNumber,
				NewBinNumber:    d.BinNumber, // "Place Bin #N" display parity with placements
				PlacementSource: "redeployment",
				MoveRequestID:   moveID,
				DestLat:         d.DestinationLatitude,
				DestLng:         d.DestinationLongitude,
				DestAddress:     d.DestinationAddress,
				MoveType:        "redeployment",
				TaskData:        []byte("{}"),
				CreatedAt:       now,
			}); err != nil {
				return "", 0, nil, nil, fmt.Errorf("failed to create deployment placement task: %w", err)
			}
			nextSeq++
			mintedMoveIDs = append(mintedMoveIDs, moveID)
			log.Printf("   ✅ Deployment move %s minted (bin #%d: warehouse → %s, placement task seq %d)", moveID, d.BinNumber, d.DestinationAddress, nextSeq-1)
		}
	}

	// ❌ REMOVED: Auto-append warehouse stop logic
	// Mapbox Optimization v2 will automatically add warehouse stops (end stops) as needed
	// Let the optimizer handle warehouse returns for optimal routing
	//
	// task_count = rows actually inserted (skips excluded; each deployment now
	// contributes ONE placement task — Phase 2 redeployment-as-placement).
	actualTasks := len(tasks) - skippedInactive + len(warehouseDeployments)
	if skippedInactive > 0 {
		log.Printf("⚠️  Skipped %d inactive bins (retired/missing) from shift", skippedInactive)
	}

	// Recompute the shift's counts from route_tasks in-tx — the same single
	// semantic (logical bins: a relocation pair counts once) every later
	// mutation rewrites them to. Replaces the hand-count + skip-decrement
	// (whose Exec error was silently swallowed) + the dead deployment
	// increment, so counts are consistent from birth instead of from the
	// first mutation.
	if err = itinerary.RecomputeShiftCounts(tx, shiftID, now); err != nil {
		return "", 0, nil, nil, fmt.Errorf("failed to recompute shift counts: %w", err)
	}
	log.Printf("✅ Created shift with %d tasks - optimizer will add warehouse stops during optimization", actualTasks)

	err = tx.Commit()
	if err != nil {
		return "", 0, nil, nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	log.Printf("✅ Created shift %s with %d tasks (%d inactive skipped)", shiftID, actualTasks, skippedInactive)
	return shiftID, actualTasks, mintedMoveIDs, skippedBins, nil
}

// GetNextIncompleteTask gets the next task to complete in a shift
func GetNextIncompleteTask(db *orgdb.DB, shiftID string) (*models.RouteTask, error) {
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
