package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/optimization"
	"ropacal-backend/internal/services/redis"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func ReoptimizeActiveShift(db *sqlx.DB, redisClient *redis.Client, shiftID string, centrifugoClient *centrifugo.Client, skipGates bool) error {
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

	// #19: lock_route_order means the manager pinned this route's order. A mid-shift
	// re-optimize must NOT reshuffle it (the bug: lock was honored at shift START but
	// ignored here). Take the order-preserving path — dense-renumber to close gaps from
	// any added/removed tasks (never re-sort), notify the driver so the app refreshes,
	// and return WITHOUT invoking the optimizer. (DESIGN: "Resequence path, never ApplyOrder".)
	if shift.LockRouteOrder {
		log.Printf("🔒 [REOPTIMIZE] lock_route_order set for shift %s — preserving manager's order (resequence only, no reshuffle)", shiftID)
		if rerr := itinerary.Resequence(db, shiftID); rerr != nil {
			return fmt.Errorf("resequence locked shift: %w", rerr)
		}
		if centrifugoClient != nil {
			if nerr := NotifyDriverOfRouteUpdate(db, centrifugoClient, shiftID, "route_reoptimized", map[string]interface{}{
				"reason": "Route order locked by manager — preserved (no re-optimization)",
			}); nerr != nil {
				log.Printf("⚠️  [REOPTIMIZE] Failed to notify driver (locked path): %v", nerr)
			}
		}
		return nil
	}

	// Step 2: Get remaining (uncompleted) route_tasks (exclude soft-deleted tasks)
	var tasks []models.RouteTask
	err = db.Select(&tasks, `
		SELECT * FROM route_tasks
		WHERE shift_id = $1 AND is_completed = 0 AND is_deleted = false
		ORDER BY sequence_order ASC
	`, shiftID)
	if err != nil {
		return fmt.Errorf("failed to get route tasks: %w", err)
	}

	remainingTasks := len(tasks)
	log.Printf("📊 [REOPTIMIZE] Found %d remaining tasks", remainingTasks)

	// Count non-warehouse tasks for gate check
	nonWarehouseTasks := 0
	for _, task := range tasks {
		if task.TaskType != "warehouse_stop" {
			nonWarehouseTasks++
		}
	}
	log.Printf("📊 [REOPTIMIZE] %d non-warehouse tasks", nonWarehouseTasks)

	// Gate checks (skip entirely for manager-initiated changes)
	if !skipGates {
		// Gate 1: Minimum tasks check
		if nonWarehouseTasks < 5 {
			return fmt.Errorf("insufficient remaining tasks (%d < 5)", nonWarehouseTasks)
		}

		// Gate 2: Time-based check
		shiftDuration := time.Since(time.Unix(shift.CreatedAt, 0)).Minutes()
		if shiftDuration < 30 {
			return fmt.Errorf("shift too new (%.0f min < 30 min)", shiftDuration)
		}
		log.Printf("✅ [REOPTIMIZE] Shift active for %.0f minutes", shiftDuration)
	} else {
		log.Printf("⏭️  [REOPTIMIZE] Skipping time gates (manager-initiated)")
	}

	// Step 3: Get warehouse location (may be nil for custom shifts)
	warehouseLocation := optimization.Location{
		ID:   "warehouse",
		Name: "Warehouse",
	}
	if shift.WarehouseLatitude != nil && shift.WarehouseLongitude != nil {
		warehouseLocation.Latitude = *shift.WarehouseLatitude
		warehouseLocation.Longitude = *shift.WarehouseLongitude
		if shift.WarehouseAddress != nil {
			warehouseLocation.Address = *shift.WarehouseAddress
		}
	}

	// Filter out warehouse_stop tasks from our working slice. (The DB-side hard-delete of these
	// rows now runs INSIDE the reopt tx at Step 8a below — so a failure between here and commit,
	// including an optimizer error, can no longer strip the shift's warehouse stops with no rollback.)
	filteredTasks := make([]models.RouteTask, 0)
	for _, task := range tasks {
		if task.TaskType != "warehouse_stop" {
			filteredTasks = append(filteredTasks, task)
		}
	}
	tasks = filteredTasks
	remainingTasks = len(tasks)
	log.Printf("📊 [REOPTIMIZE] %d tasks to reoptimize (warehouse stops excluded)", remainingTasks)

	// Step 4: Resolve the driver's current GPS (Redis primary, durable DB fallback,
	// null-island/freshness-guarded — the same resolver StartShift/preflight use, so
	// a Redis miss no longer hard-blocks mid-shift reoptimization).
	loc, locErr := resolveDriverStartLocation(context.Background(), db, redisClient, shift.DriverID)
	if locErr != nil {
		log.Printf("❌ [REOPTIMIZE] No usable driver location: %v", locErr)
		return fmt.Errorf("driver location for reoptimization: %w", locErr)
	}
	driverStartLocation := optimization.Location{
		ID:        "driver-current",
		Name:      "Driver Current Location",
		Latitude:  loc.Latitude,
		Longitude: loc.Longitude,
		Address:   "Current GPS Position",
	}
	log.Printf("✅ [REOPTIMIZE] Driver start location via %s: (%.6f, %.6f)", loc.Source, loc.Latitude, loc.Longitude)

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

	// Determine end location: custom end or warehouse
	vehicleEndLocation := warehouseLocation
	if shift.EndLatitude != nil && shift.EndLongitude != nil {
		endAddr := ""
		if shift.EndAddress != nil {
			endAddr = *shift.EndAddress
		}
		vehicleEndLocation = optimization.Location{
			ID:        "custom-end",
			Name:      "Custom End Location",
			Latitude:  *shift.EndLatitude,
			Longitude: *shift.EndLongitude,
			Address:   endAddr,
		}
		log.Printf("🏁 [REOPTIMIZE] Using custom end location: (%.6f, %.6f)", *shift.EndLatitude, *shift.EndLongitude)
	}

	vehicle := optimization.Vehicle{
		ID:             fmt.Sprintf("truck-%s", shift.DriverID[:8]),
		Name:           fmt.Sprintf("Truck-%s", shift.DriverID[:8]),
		StartLocation:  driverStartLocation,
		EndLocation:    vehicleEndLocation,
		RoutingProfile: "mapbox/driving-traffic",
		Capacities: map[string]int{
			"bins": capacity,
		},
		StartupBins: 0, // Will be set below based on bins currently on truck
	}

	// Apply shift schedule as vehicle time constraints
	if shift.ScheduledStart != nil && *shift.ScheduledStart != "" {
		if t, err := time.Parse(time.RFC3339, *shift.ScheduledStart); err == nil {
			vehicle.EarliestStart = &t
			log.Printf("⏰ [REOPTIMIZE] Vehicle earliest_start: %s", t.Format(time.RFC3339))
		}
	}
	if shift.ScheduledEnd != nil && *shift.ScheduledEnd != "" {
		if t, err := time.Parse(time.RFC3339, *shift.ScheduledEnd); err == nil {
			vehicle.LatestEnd = &t
			log.Printf("⏰ [REOPTIMIZE] Vehicle latest_end: %s", t.Format(time.RFC3339))
		}
	}

	req.Vehicles[0] = vehicle

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

	// Step 5.5: Calculate bins currently on truck (two-warehouse trick)
	// Count completed warehouse stops (bins loaded) and completed placements (bins used)
	var allTasks []models.RouteTask
	err = db.Select(&allTasks, `
		SELECT * FROM route_tasks
		WHERE shift_id = $1 AND is_deleted = false
		ORDER BY sequence_order ASC
	`, shiftID)
	if err != nil {
		log.Printf("⚠️  [REOPTIMIZE] Failed to fetch all tasks for bin calculation: %v", err)
	}

	warehouseStopsCompleted := 0
	placementsCompleted := 0
	for _, t := range allTasks {
		if t.IsCompleted == 1 {
			if t.TaskType == "warehouse_stop" {
				warehouseStopsCompleted++
			} else if t.TaskType == "placement" {
				placementsCompleted++
			}
		}
	}

	// Calculate bins on truck
	// Bins come from: preloaded at shift start + warehouse stops during shift
	// Bins used by: completed placements
	binsLoaded := shift.PreloadedBins + (warehouseStopsCompleted * capacity)
	binsUsed := placementsCompleted
	binsOnTruck := binsLoaded - binsUsed

	if binsOnTruck < 0 {
		binsOnTruck = 0 // Safety check
	}

	// Count remaining (uncompleted) placements
	remainingPlacements := 0
	for _, task := range tasks {
		if task.TaskType == "placement" {
			remainingPlacements++
		}
	}

	// KEY FIX: If remaining placements fit within truck capacity, treat ALL as preloaded.
	// This prevents OR-Tools from creating a separate warehouse stop per placement.
	// The end-of-route warehouse stop (added by optimizer) serves as the reload point.
	if remainingPlacements <= capacity && binsOnTruck < remainingPlacements {
		log.Printf("📦 [REOPTIMIZE] Adjusting: %d remaining placements fit in truck (capacity=%d). Driver has %d, needs %d more.",
			remainingPlacements, capacity, binsOnTruck, remainingPlacements-binsOnTruck)
		log.Printf("📦 [REOPTIMIZE] Treating all as preloaded — driver will reload at warehouse stop.")
		binsOnTruck = remainingPlacements
	}

	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🔢 [REOPTIMIZE] Bin Inventory Calculation:")
	log.Printf("   Warehouse stops completed: %d", warehouseStopsCompleted)
	log.Printf("   Bins loaded (original): %d (capacity=%d)", binsLoaded, capacity)
	log.Printf("   Placements completed: %d", placementsCompleted)
	log.Printf("   Bins on truck (adjusted): %d", binsOnTruck)
	log.Printf("   Remaining placements: %d", remainingPlacements)
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Set startup bins to current truck inventory
	// Google will use Vehicle.StartupBins for native startLoadDemands
	// Mapbox will ignore it and use the two-warehouse trick
	req.Vehicles[0].StartupBins = binsOnTruck
	req.BinsPreloaded = (binsOnTruck > 0)
	log.Printf("📦 [REOPTIMIZE] BinsPreloaded=%v, StartupBins=%d/%d", req.BinsPreloaded, binsOnTruck, capacity)

	// Convert tasks to optimization format
	placementIndex := 0
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
				if placementIndex < binsOnTruck {
					// Bin already on truck — model as service task (just a dropoff)
					log.Printf("   📦 Placement %d: bin on truck — modeled as dropoff", placementIndex+1)
					svcTask := optimization.ServiceTask{
						ID: *task.PotentialLocationID,
						Location: optimization.Location{
							ID:        *task.PotentialLocationID,
							Name:      fmt.Sprintf("Placement #%d", getIntValue(task.NewBinNumber)),
							Latitude:  task.Latitude,
							Longitude: task.Longitude,
							Address:   getStringValue(task.Address),
						},
						Duration: 120,
						Label:    fmt.Sprintf("Place Bin #%d", getIntValue(task.NewBinNumber)),
					}
					req.ServiceTasks = append(req.ServiceTasks, svcTask)
				} else {
					// Need warehouse reload — model as shipment
					log.Printf("   🏭 Placement %d: needs warehouse — modeled as shipment", placementIndex+1)
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
				placementIndex++
			}

		case "pickup":
			if task.MoveRequestID != nil && task.Latitude != 0 && task.Longitude != 0 &&
				task.DestinationLatitude != nil && task.DestinationLongitude != nil {
				log.Printf("🔍 [OPTIMIZER] Move request %s: pickup=(%.6f,%.6f) '%s' → dropoff=(%.6f,%.6f) '%s'",
					*task.MoveRequestID, task.Latitude, task.Longitude, getStringValue(task.Address),
					*task.DestinationLatitude, *task.DestinationLongitude, getStringValue(task.DestinationAddress))
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

		case "service":
			if task.Latitude != 0 && task.Longitude != 0 {
				svcTask := optimization.ServiceTask{
					ID: task.ID,
					Location: optimization.Location{
						ID:        fmt.Sprintf("service-%s", task.ID),
						Name:      getStringValue(task.TaskLabel),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					Duration: func() int {
						if task.ServiceDurationSeconds != nil {
							return *task.ServiceDurationSeconds
						}
						return 300
					}(),
					Label:           getStringValue(task.TaskLabel),
					EarliestArrival: task.EarliestArrival,
					LatestArrival:   task.LatestArrival,
					TimeWindowType:  getStringValue(task.TimeWindowType),
				}
				req.ServiceTasks = append(req.ServiceTasks, svcTask)
			}
		}
	}

	log.Printf("📊 [REOPTIMIZE] Request: %d collections, %d placements, %d moves, %d service tasks",
		len(req.Collections), len(req.Placements), len(req.MoveRequests), len(req.ServiceTasks))
	log.Printf("🔍 [REOPTIMIZE] Vehicle: start=%s (%.6f,%.6f) end=%s (%.6f,%.6f) capacity=%d startupBins=%d",
		req.Vehicles[0].StartLocation.ID, req.Vehicles[0].StartLocation.Latitude, req.Vehicles[0].StartLocation.Longitude,
		req.Vehicles[0].EndLocation.ID, req.Vehicles[0].EndLocation.Latitude, req.Vehicles[0].EndLocation.Longitude,
		req.Vehicles[0].Capacities["bins"], req.Vehicles[0].StartupBins)
	for ci, c := range req.Collections {
		log.Printf("🔍 [REOPTIMIZE] Collection[%d]: ID=%s Bin#%d at (%.6f,%.6f)", ci, c.ID, c.BinNumber, c.Location.Latitude, c.Location.Longitude)
	}

	// Step 6: Call optimizer (configured via OPTIMIZER_TYPE env var)
	log.Printf("🚀 [REOPTIMIZE] Calling route optimizer...")
	optimizer := optimization.NewOptimizer()
	log.Printf("📍 [REOPTIMIZE] Using optimizer: %s", optimizer.Name())
	response, err := optimizer.OptimizeRoute(req)
	if err != nil {
		return fmt.Errorf("optimization failed: %w", err)
	}

	if len(response.Routes) == 0 {
		return fmt.Errorf("optimizer returned no routes (dropped tasks: %v)", response.DroppedTasks)
	}

	route := response.Routes[0]
	log.Printf("✅ [REOPTIMIZE] Optimization complete: %d stops, %d dropped tasks", len(route.Stops), len(response.DroppedTasks))
	if len(response.DroppedTasks) > 0 {
		log.Printf("⚠️  [REOPTIMIZE] DROPPED TASKS: %v", response.DroppedTasks)
	}
	for si, s := range route.Stops {
		log.Printf("   🔍 [REOPTIMIZE] Raw stop %d: Type=%s LocationID=%s CollectionID=%s PlacementID=%s MoveRequestID=%s Lat=%.6f Lng=%.6f",
			si, s.Type, s.LocationID, s.CollectionID, s.PlacementID, s.MoveRequestID, s.Latitude, s.Longitude)
	}

	// Step 7: Calculate old vs new completion time (time savings check)
	if !skipGates && len(tasks) > 0 {
		// This is a simplified check - in production you'd want more sophisticated comparison
		log.Printf("ℹ️  [REOPTIMIZE] Time savings check skipped (would need historical ETA data)")
	}

	// Step 8: Update route_tasks with new sequence_order and create new warehouse_stop tasks
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer tx.Rollback()

	// Step 8a: Hard-delete incomplete warehouse_stop tasks INSIDE the tx (was a pre-tx db.Exec).
	// They're system-generated routing artifacts regenerated by the loop below, so soft-deleting
	// them only spammed the task-history audit view with phantom rows; completed ones (driver
	// actually visited) are kept. Moving it in-tx makes delete+regenerate atomic — an early return
	// below can no longer leave the shift with warehouse stops stripped and no rollback. Error is
	// logged-and-continued (best-effort, exactly as before — not fatal to the reopt).
	if delResult, delErr := tx.Exec(`
		DELETE FROM route_tasks
		WHERE shift_id = $1 AND task_type = 'warehouse_stop' AND is_completed = 0 AND is_deleted = false
	`, shiftID); delErr != nil {
		log.Printf("⚠️  [REOPTIMIZE] Failed to delete warehouse stops: %v", delErr)
	} else if deletedCount, _ := delResult.RowsAffected(); deletedCount > 0 {
		log.Printf("🗑️  [REOPTIMIZE] Deleted %d incomplete warehouse_stop tasks (will regenerate)", deletedCount)
	}

	taskIDToOriginal := make(map[string]models.RouteTask)
	for _, task := range tasks {
		taskIDToOriginal[task.ID] = task
	}

	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📍 [REOPTIMIZE] Processing %d optimized stops", len(route.Stops))
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Build the optimizer's order (interleaved) for itinerary.ApplyOrder — the order-deciding
	// sequence_order writer. Each stop is either an existing task to UPDATE (matched below) or a
	// regenerated warehouse_stop to INSERT, appended in optimizer order.
	orderedStops := make([]itinerary.OrderedStop, 0, len(route.Stops))

	for i, stop := range route.Stops {
		log.Printf("   Stop #%d/%d: Type=%s, Location=%s, CollectionID=%s, PlacementID=%s, MoveRequestID=%s, Odometer=%.0f",
			i+1, len(route.Stops), stop.Type, stop.LocationID, stop.CollectionID, stop.PlacementID, stop.MoveRequestID, stop.Odometer)

		// Skip start stops
		if stop.Type == optimization.StopTypeStart {
			log.Printf("      ⏭️  Skipped start stop")
			continue
		}

		// Handle end stops (return to warehouse) — queued for ApplyOrder to INSERT.
		if stop.Type == optimization.StopTypeEnd {
			addr := stop.Address
			orderedStops = append(orderedStops, itinerary.OrderedStop{Insert: true, Task: models.RouteTask{
				ID:        uuid.New().String(),
				ShiftID:   shiftID,
				TaskType:  "warehouse_stop",
				Latitude:  stop.Latitude,
				Longitude: stop.Longitude,
				Address:   &addr,
			}})
			log.Printf("      ➕ Queued warehouse_stop (return to warehouse)")
			continue
		}

		// Handle warehouse pickup stops
		if stop.Type == optimization.StopTypePickup && (stop.LocationID == "warehouse" || stop.LocationID == "driver-current-warehouse") {
			// Skip fake warehouse pickup stop (driver already has these bins on truck)
			if stop.LocationID == "driver-current-warehouse" {
				log.Printf("      ⏭️  Skipped fake warehouse pickup (bins already on truck)")
				continue
			}

			// Real warehouse pickup — queued for ApplyOrder to INSERT.
			addr := stop.Address
			orderedStops = append(orderedStops, itinerary.OrderedStop{Insert: true, Task: models.RouteTask{
				ID:        uuid.New().String(),
				ShiftID:   shiftID,
				TaskType:  "warehouse_stop",
				Latitude:  stop.Latitude,
				Longitude: stop.Longitude,
				Address:   &addr,
			}})
			log.Printf("      ➕ Queued warehouse_stop (pickup bins)")
			continue
		}

		// Find the corresponding existing task (collection, placement, move)
		var taskID string
		log.Printf("      🔍 Matching: CollectionID=%q PlacementID=%q MoveRequestID=%q StopType=%s",
			stop.CollectionID, stop.PlacementID, stop.MoveRequestID, stop.Type)
		if stop.CollectionID != "" {
			// Strip prefix: OR-Tools returns "collection-{id}" or "service-{id}"
			cleanID := strings.TrimPrefix(stop.CollectionID, "collection-")
			cleanID = strings.TrimPrefix(cleanID, "service-")

			// Try matching collection by task ID
			for _, origTask := range tasks {
				if origTask.TaskType == "collection" && origTask.ID == cleanID {
					taskID = origTask.ID
					log.Printf("      ✅ Matched collection: %s", taskID[:8])
					break
				}
			}
			// Try matching placement modeled as service (preloaded bins)
			if taskID == "" {
				for _, origTask := range tasks {
					if origTask.TaskType == "placement" && origTask.PotentialLocationID != nil && *origTask.PotentialLocationID == cleanID {
						taskID = origTask.ID
						log.Printf("      ✅ Matched service→placement: %s", taskID[:8])
						break
					}
				}
			}
			if taskID == "" {
				log.Printf("      ❌ No match for CollectionID=%s (cleanID=%s)", stop.CollectionID, cleanID)
			}
		} else if stop.PlacementID != "" {
			cleanID := strings.TrimPrefix(stop.PlacementID, "placement-")
			for _, origTask := range tasks {
				if origTask.TaskType == "placement" && origTask.PotentialLocationID != nil && *origTask.PotentialLocationID == cleanID {
					taskID = origTask.ID
					log.Printf("      ✅ Matched placement: %s", taskID[:8])
					break
				}
			}
			if taskID == "" {
				log.Printf("      ❌ No match for PlacementID=%s (cleanID=%s)", stop.PlacementID, cleanID)
			}
		} else if stop.MoveRequestID != "" {
			cleanID := strings.TrimPrefix(stop.MoveRequestID, "move-")
			for _, origTask := range tasks {
				if origTask.MoveRequestID != nil && *origTask.MoveRequestID == cleanID {
					if (stop.Type == optimization.StopTypePickup && origTask.TaskType == "pickup") ||
						(stop.Type == optimization.StopTypeDropoff && origTask.TaskType == "dropoff") {
						taskID = origTask.ID
						log.Printf("      ✅ Matched move %s: %s", origTask.TaskType, taskID[:8])
						break
					}
				}
			}
			if taskID == "" {
				log.Printf("      ❌ No match for MoveRequestID=%s (cleanID=%s)", stop.MoveRequestID, cleanID)
			}
		}

		if taskID != "" {
			orderedStops = append(orderedStops, itinerary.OrderedStop{Task: models.RouteTask{ID: taskID}})
			taskType := "unknown"
			if origTask, exists := taskIDToOriginal[taskID]; exists {
				taskType = string(origTask.TaskType)
			}
			log.Printf("      ➕ Queued %s task %s (pos %d)", taskType, taskID[:8], len(orderedStops))
		} else {
			log.Printf("      ⚠️  No matching task found for stop")
		}
	}

	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📊 [REOPTIMIZE] Applying optimizer order: %d stops", len(orderedStops))
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// itinerary.ApplyOrder is the order-deciding sequence_order writer: it stamps the matched
	// existing tasks (seq-only UPDATE), INSERTs the regenerated warehouse stops, then Resequences
	// to the canonical dense 1..N (isFirst=false → warehouse-last, which washes out the interim
	// bin/warehouse interleaving on the reopt path).
	if err := itinerary.ApplyOrder(tx, shiftID, orderedStops, false); err != nil {
		return fmt.Errorf("failed to apply optimized order: %w", err)
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

	totalDistanceMiles := totalDistanceKm * 0.621371
	optimizationMetadata := models.OptimizationMetadata{
		TotalDistanceMiles:     totalDistanceMiles,
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
			log.Printf("✅ Saved optimization metadata: %.2f miles, %s, ETA: %s",
				optimizationMetadata.TotalDistanceMiles,
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
// isFirstOptimization: true = shift starting (UPDATE tasks), false = mid-shift reoptimization (DELETE+CREATE tasks)
func optimizeRouteWithMapbox(
	db *sqlx.DB,
	shiftID string,
	capacity int,
	driverLat, driverLon float64,
	warehouseLat, warehouseLon float64,
	warehouseAddr string,
	binsPreloaded bool,
	isFirstOptimization bool,
	customStartLat, customStartLon *float64, // Custom start location (overrides driver GPS)
	customStartAddr *string,
	customEndLat, customEndLon *float64, // Custom end location (overrides warehouse as end)
	customEndAddr *string,
) error {
	log.Printf("🚀 [MAPBOX OPTIMIZER] Starting Mapbox v2 route optimization (first_optimization=%v)", isFirstOptimization)
	log.Printf("   🚚 Bins preloaded: %v", binsPreloaded)

	// Step 1: Fetch shift details
	var shift struct {
		ID             string  `db:"id"`
		DriverID       string  `db:"driver_id"`
		WarehouseAddr  *string `db:"warehouse_address"`
		ScheduledStart *string `db:"scheduled_start"`
		ScheduledEnd   *string `db:"scheduled_end"`
	}
	err := db.Get(&shift, `SELECT id, driver_id, warehouse_address, scheduled_start, scheduled_end FROM shifts WHERE id = $1`, shiftID)
	if err != nil {
		return fmt.Errorf("failed to fetch shift: %w", err)
	}

	// Step 2: Fetch all tasks for the shift (only active, non-deleted tasks)
	var tasks []models.RouteTask
	query := `SELECT * FROM route_tasks WHERE shift_id = $1 AND is_deleted = FALSE ORDER BY sequence_order ASC`
	err = db.Select(&tasks, query, shiftID)
	if err != nil {
		return fmt.Errorf("failed to fetch tasks: %w", err)
	}

	log.Printf("📋 [MAPBOX OPTIMIZER] Fetched %d tasks", len(tasks))

	// Log detailed task breakdown by type
	taskCounts := make(map[string]int)
	for _, task := range tasks {
		taskCounts[string(task.TaskType)]++
	}
	log.Printf("📊 [MAPBOX OPTIMIZER] Task breakdown:")
	for taskType, count := range taskCounts {
		log.Printf("   - %s: %d", taskType, count)
	}

	// 🔍 DEBUG: Log ORIGINAL task sequence from database (BEFORE Mapbox optimization)
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("🗂️  [DEBUG] ORIGINAL TASK SEQUENCE (from database BEFORE optimization):")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	for i, task := range tasks {
		addr := "N/A"
		if task.Address != nil {
			addr = *task.Address
		}
		log.Printf("   #%d: [%s] %s - %s (Seq: %d)",
			i+1, task.TaskType, task.ID[:8], addr, task.SequenceOrder)
	}
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Log placement task details specifically
	for i, task := range tasks {
		if task.TaskType == "placement" {
			log.Printf("🎯 [PLACEMENT TASK #%d] ID=%s, PotentialLocationID=%v, Lat=%.6f, Lon=%.6f, Address=%v",
				i+1, task.ID,
				func() string {
					if task.PotentialLocationID != nil {
						return *task.PotentialLocationID
					} else {
						return "nil"
					}
				}(),
				task.Latitude, task.Longitude,
				func() string {
					if task.Address != nil {
						return *task.Address
					} else {
						return "nil"
					}
				}(),
			)
		}
	}

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

	// Define driver's current GPS location
	driverGPSLocation := optimization.Location{
		ID:        "driver-current",
		Name:      "Driver Current Location",
		Latitude:  driverLat,
		Longitude: driverLon,
		Address:   "Current GPS Position",
	}

	// Define warehouse location
	warehouseLocation := optimization.Location{
		ID:        "warehouse",
		Name:      "Warehouse",
		Latitude:  warehouseLat,
		Longitude: warehouseLon,
		Address:   warehouseAddr,
	}

	// Smart Start Location Logic:
	// Use custom start location if set, otherwise use driver's current GPS
	vehicleStartLocation := driverGPSLocation
	if customStartLat != nil && customStartLon != nil {
		startAddr := ""
		if customStartAddr != nil {
			startAddr = *customStartAddr
		}
		vehicleStartLocation = optimization.Location{
			ID:        "custom-start",
			Name:      "Custom Start Location",
			Latitude:  *customStartLat,
			Longitude: *customStartLon,
			Address:   startAddr,
		}
		log.Printf("🚚 [MAPBOX OPTIMIZER] Starting from CUSTOM location (%.6f, %.6f) %s", *customStartLat, *customStartLon, startAddr)
	} else {
		log.Printf("🚚 [MAPBOX OPTIMIZER] Starting from DRIVER GPS (%.6f, %.6f)", driverLat, driverLon)
	}
	log.Printf("   🏭 Will route to warehouse to pickup bins: (%.6f, %.6f)", warehouseLat, warehouseLon)
	log.Printf("   ℹ️  No fake warehouse trick - better routing to nearby stops")
	// Determine end location: custom end location or warehouse
	vehicleEndLocation := warehouseLocation
	if customEndLat != nil && customEndLon != nil {
		endAddr := ""
		if customEndAddr != nil {
			endAddr = *customEndAddr
		}
		vehicleEndLocation = optimization.Location{
			ID:        "custom-end",
			Name:      "Custom End Location",
			Latitude:  *customEndLat,
			Longitude: *customEndLon,
			Address:   endAddr,
		}
		log.Printf("🏁 [MAPBOX OPTIMIZER] End: Custom location (%.6f, %.6f) %s", *customEndLat, *customEndLon, endAddr)
	} else {
		log.Printf("🏁 [MAPBOX OPTIMIZER] End: Warehouse location (%.6f, %.6f)", warehouseLat, warehouseLon)
	}

	// Define vehicle with capacity
	// startupBins is ALWAYS 0 (no bins preloaded)
	startupBins := 0

	vehicle := optimization.Vehicle{
		ID:            shift.DriverID,
		Name:          fmt.Sprintf("Truck-%s", shift.DriverID[:8]),
		StartLocation: vehicleStartLocation,
		EndLocation:   vehicleEndLocation,
		Capacities: map[string]int{
			"bins": capacity,
		},
		StartupBins: startupBins, // Always 0
	}

	// Apply shift schedule as vehicle time constraints
	if shift.ScheduledStart != nil && *shift.ScheduledStart != "" {
		if t, err := time.Parse(time.RFC3339, *shift.ScheduledStart); err == nil {
			vehicle.EarliestStart = &t
			log.Printf("⏰ [MAPBOX OPTIMIZER] Vehicle earliest_start: %s", t.Format(time.RFC3339))
		}
	}
	if shift.ScheduledEnd != nil && *shift.ScheduledEnd != "" {
		if t, err := time.Parse(time.RFC3339, *shift.ScheduledEnd); err == nil {
			vehicle.LatestEnd = &t
			log.Printf("⏰ [MAPBOX OPTIMIZER] Vehicle latest_end: %s", t.Format(time.RFC3339))
		}
	}

	req.Vehicles[0] = vehicle

	req.BinsPreloaded = binsPreloaded

	// Build lookup: move_request_id → actual move_type from bin_move_requests
	moveRequestTypes := make(map[string]string)
	var mrRows []struct {
		ID       string `db:"id"`
		MoveType string `db:"move_type"`
	}
	db.Select(&mrRows, `SELECT id, move_type FROM bin_move_requests WHERE id IN (SELECT DISTINCT move_request_id FROM route_tasks WHERE shift_id = $1 AND move_request_id IS NOT NULL AND is_deleted = false)`, shiftID)
	for _, mr := range mrRows {
		moveRequestTypes[mr.ID] = mr.MoveType
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

	// Count placements + redeployments for initial_load when bins are preloaded
	if binsPreloaded {
		preloadCount := 0
		for _, task := range tasks {
			if task.TaskType == "placement" {
				preloadCount++
			} else if task.TaskType == "pickup" && task.MoveRequestID != nil && moveRequestTypes[*task.MoveRequestID] == "redeployment" {
				preloadCount++
			}
		}
		req.Vehicles[0].StartupBins = preloadCount
		log.Printf("📦 [OPTIMIZER] BinsPreloaded=true, StartupBins=%d/%d (driver loaded bins at warehouse)", preloadCount, capacity)
	} else {
		log.Printf("📦 [OPTIMIZER] BinsPreloaded=false, StartupBins=0/%d (will route to warehouse for pickups)", capacity)
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
					Duration:       300, // 5 minutes
					FillPercentage: getIntValue(task.FillPercentage),
				}
				req.Collections = append(req.Collections, collection)
			} else {
				log.Printf("⚠️  Skipping collection task %s: missing required fields", task.ID)
			}

		case "placement":
			if task.PotentialLocationID != nil && task.Latitude != 0 && task.Longitude != 0 {
				if binsPreloaded {
					// Bins already on truck — model as a service task (just a dropoff, no warehouse pickup)
					log.Printf("📦 [PLACEMENT] Bin already on truck — modeled as dropoff service")
					svcTask := optimization.ServiceTask{
						ID: *task.PotentialLocationID,
						Location: optimization.Location{
							ID:        *task.PotentialLocationID,
							Name:      fmt.Sprintf("Placement #%d", getIntValue(task.NewBinNumber)),
							Latitude:  task.Latitude,
							Longitude: task.Longitude,
							Address:   getStringValue(task.Address),
						},
						Duration: 120,
						Label:    fmt.Sprintf("Place Bin #%d", getIntValue(task.NewBinNumber)),
					}
					req.ServiceTasks = append(req.ServiceTasks, svcTask)
				} else {
					// Bins not loaded — model as shipment (warehouse pickup → site dropoff)
					log.Printf("🏭 [PLACEMENT] Needs warehouse pickup — modeled as shipment")
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
			}

		case "pickup":
			if task.MoveRequestID != nil && task.Latitude != 0 && task.Longitude != 0 &&
				task.DestinationLatitude != nil && task.DestinationLongitude != nil {
				log.Printf("🔍 [OPTIMIZER] Move request %s: pickup=(%.6f,%.6f) '%s' → dropoff=(%.6f,%.6f) '%s'",
					*task.MoveRequestID, task.Latitude, task.Longitude, getStringValue(task.Address),
					*task.DestinationLatitude, *task.DestinationLongitude, getStringValue(task.DestinationAddress))

				// Redeployment with bins preloaded: skip warehouse pickup, just dropoff
				isRedeployment := task.MoveRequestID != nil && moveRequestTypes[*task.MoveRequestID] == "redeployment"
				if binsPreloaded && isRedeployment {
					log.Printf("📦 [REDEPLOYMENT] Bins preloaded — modeled as service task (dropoff only)")
					svcTask := optimization.ServiceTask{
						ID: *task.MoveRequestID,
						Location: optimization.Location{
							ID:        fmt.Sprintf("%s-dropoff", *task.MoveRequestID),
							Name:      fmt.Sprintf("Deploy Bin #%d", getIntValue(task.BinNumber)),
							Latitude:  *task.DestinationLatitude,
							Longitude: *task.DestinationLongitude,
							Address:   getStringValue(task.DestinationAddress),
						},
						Duration: 120,
						Label:    fmt.Sprintf("Deploy Bin #%d", getIntValue(task.BinNumber)),
					}
					req.ServiceTasks = append(req.ServiceTasks, svcTask)
				} else {
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
				}
			} else {
				log.Printf("⚠️  Skipping move request task %s: missing required fields", task.ID)
			}

		case "service":
			// Service tasks are location visits with optional time windows
			if task.Latitude != 0 && task.Longitude != 0 {
				svcTask := optimization.ServiceTask{
					ID: task.ID,
					Location: optimization.Location{
						ID:        fmt.Sprintf("service-%s", task.ID),
						Name:      getStringValue(task.TaskLabel),
						Latitude:  task.Latitude,
						Longitude: task.Longitude,
						Address:   getStringValue(task.Address),
					},
					Duration: func() int {
						if task.ServiceDurationSeconds != nil {
							return *task.ServiceDurationSeconds
						}
						return 300 // Default 5 minutes
					}(),
					Label:           getStringValue(task.TaskLabel),
					EarliestArrival: task.EarliestArrival,
					LatestArrival:   task.LatestArrival,
					TimeWindowType:  getStringValue(task.TimeWindowType),
				}
				req.ServiceTasks = append(req.ServiceTasks, svcTask)

				if task.EarliestArrival != nil && task.LatestArrival != nil {
					log.Printf("⏰ [SERVICE] Task %s has time window: %s - %s (type: %s)",
						task.ID, task.EarliestArrival.Format("15:04"), task.LatestArrival.Format("15:04"),
						getStringValue(task.TimeWindowType))
				}
			} else {
				log.Printf("⚠️  Skipping service task %s: missing coordinates", task.ID)
			}

		case "warehouse_stop":
			// Skip warehouse stops - Mapbox handles them automatically
			log.Printf("⏭️  Skipping warehouse_stop task %s (handled implicitly by Mapbox)", task.ID)
		}
	}

	log.Printf("📊 [MAPBOX OPTIMIZER] Request: %d collections, %d placements, %d moves, %d service tasks",
		len(req.Collections), len(req.Placements), len(req.MoveRequests), len(req.ServiceTasks))

	// Step 4: Call optimizer (configured via OPTIMIZER_TYPE env var)
	log.Printf("🚀 [OPTIMIZER] Calling route optimizer...")
	optimizer := optimization.NewOptimizer()
	log.Printf("📍 [OPTIMIZER] Using optimizer: %s", optimizer.Name())
	response, err := optimizer.OptimizeRoute(req)
	if err != nil {
		log.Printf("❌ [OPTIMIZER] Optimization FAILED: %v", err)
		return fmt.Errorf("optimization failed: %w", err)
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

	// 🔍 DEBUG: Log MAPBOX RESPONSE - what order did Mapbox return?
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📦 [DEBUG] MAPBOX RESPONSE - Optimized Stop Sequence:")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	for i, stop := range route.Stops {
		stopDesc := fmt.Sprintf("Type=%s", stop.Type)
		if stop.CollectionID != "" {
			stopDesc += fmt.Sprintf(", CollectionID=%s", stop.CollectionID)
		}
		if stop.PlacementID != "" {
			stopDesc += fmt.Sprintf(", PlacementID=%s", stop.PlacementID)
		}
		if stop.MoveRequestID != "" {
			stopDesc += fmt.Sprintf(", MoveRequestID=%s", stop.MoveRequestID)
		}
		log.Printf("   Stop #%d: %s - %.6f, %.6f - %s",
			i+1, stopDesc, stop.Latitude, stop.Longitude, stop.Address)
	}
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Step 5: Handle existing tasks based on optimization type
	now := time.Now().Unix()

	// Persist the optimized order atomically. The optimizer HTTP call already ran
	// above (outside any transaction); from here every route_tasks write goes
	// through one tx, so a mid-persist failure rolls back instead of leaving a
	// half-renumbered / partially-rewritten route. (Phase 4 slice 1.)
	tx, txErr := db.Beginx()
	if txErr != nil {
		return fmt.Errorf("begin optimize persist: %w", txErr)
	}
	defer tx.Rollback()

	if isFirstOptimization {
		// FIRST OPTIMIZATION (shift start): update sequence_order, preserve task IDs.
		// Hard-delete existing warehouse_stop tasks — system artifacts the optimizer
		// regenerates; soft-deleting them just spams the task-history audit view.
		_, err = tx.Exec(`
			DELETE FROM route_tasks
			WHERE shift_id = $1 AND task_type = 'warehouse_stop' AND is_completed = 0 AND is_deleted = false
		`, shiftID)
		if err != nil {
			log.Printf("⚠️  Failed to delete old warehouse stops: %v", err)
		} else {
			log.Printf("🗑️  Deleted existing warehouse_stop tasks (will be regenerated by optimizer)")
		}
		// Auto-complete redeployment pickups when bins are preloaded (driver already has them)
		if binsPreloaded {
			res, pickupErr := tx.Exec(`
				UPDATE route_tasks SET is_completed = 1, completed_at = $1, updated_at = $1
				WHERE shift_id = $2 AND task_type = 'pickup' AND is_deleted = false AND is_completed = 0
				  AND move_request_id IN (SELECT id FROM bin_move_requests WHERE move_type = 'redeployment')
			`, now, shiftID)
			if pickupErr == nil {
				if rows, _ := res.RowsAffected(); rows > 0 {
					log.Printf("✅ Auto-completed %d redeployment pickup(s) (bins preloaded)", rows)
				}
			}
		}
		log.Printf("✏️  [MAPBOX OPTIMIZER] First optimization - will UPDATE existing tasks (no deletion)")
	} else {
		// SUBSEQUENT OPTIMIZATION (mid-shift): Soft delete old tasks for audit trail
		_, err = tx.Exec(`
			UPDATE route_tasks
			SET is_deleted = true,
				deleted_at = $1,
				deleted_by = $2,
				deletion_reason = $3,
				updated_at = $1
			WHERE shift_id = $4 AND is_deleted = false
		`, now, "system", "shift_reoptimized", shiftID)
		if err != nil {
			return fmt.Errorf("failed to soft delete old tasks: %w", err)
		}
		log.Printf("🗑️  [MAPBOX OPTIMIZER] Soft deleted old route tasks (mid-shift reoptimization)")
	}

	// Step 6: Process optimized stops (UPDATE on first optimization, CREATE on subsequent)
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	if isFirstOptimization {
		log.Printf("📍 Updating %d existing route tasks with optimized order", len(route.Stops))
	} else {
		log.Printf("📍 Creating %d new route tasks from optimized stops", len(route.Stops))
	}
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Fetch existing tasks for matching (only if first optimization)
	var existingTasks []models.RouteTask
	existingTasksMap := make(map[string]*models.RouteTask) // key = bin_id or potential_location_id or move_request_id

	if isFirstOptimization {
		err = tx.Select(&existingTasks, `
			SELECT * FROM route_tasks
			WHERE shift_id = $1 AND is_deleted = false
			ORDER BY sequence_order ASC
		`, shiftID)
		if err != nil {
			return fmt.Errorf("failed to fetch existing tasks: %w", err)
		}

		// Build lookup map by identifiers
		for i := range existingTasks {
			task := &existingTasks[i]
			if task.BinID != nil {
				existingTasksMap[*task.BinID] = task
			}
			if task.PotentialLocationID != nil {
				// Use composite key for placements so warehouse pickup and
				// placement dropoff don't overwrite each other in the map
				existingTasksMap[*task.PotentialLocationID+":"+string(task.TaskType)] = task
			}
			if task.MoveRequestID != nil {
				// Use composite key for move requests so pickup and dropoff
				// don't overwrite each other in the map
				existingTasksMap[*task.MoveRequestID+":"+string(task.TaskType)] = task
			}
			// Service tasks: match by task ID
			if task.TaskType == "service" {
				existingTasksMap[task.ID] = task
			}
		}
		log.Printf("📋 Loaded %d existing tasks for matching", len(existingTasks))
	}

	// Build the optimizer's order (interleaved) for itinerary.ApplyOrder. Each non-skipped stop
	// becomes either an existing task to UPDATE-in-place (first-opt match) or a new row to INSERT.
	orderedStops := make([]itinerary.OrderedStop, 0, len(route.Stops))
	sequenceOrder := 1 // logging only; ApplyOrder assigns the final sequence_order
	for i, stop := range route.Stops {
		log.Printf("")
		log.Printf("🔍 Processing Stop #%d/%d:", i+1, len(route.Stops))
		log.Printf("   Type: %s", stop.Type)
		log.Printf("   LocationID: %s", stop.LocationID)
		log.Printf("   CollectionID: '%s'", stop.CollectionID)
		log.Printf("   PlacementID: '%s'", stop.PlacementID)
		log.Printf("   MoveRequestID: '%s'", stop.MoveRequestID)
		log.Printf("   Coordinates: (%.6f, %.6f)", stop.Latitude, stop.Longitude)

		// Skip start stops
		if stop.Type == optimization.StopTypeStart {
			log.Printf("   ⏭️  SKIPPED: Start stop (driver already at warehouse)")
			continue
		}

		var task models.RouteTask
		task.ShiftID = shiftID
		task.SequenceOrder = sequenceOrder
		task.Latitude = stop.Latitude
		task.Longitude = stop.Longitude
		task.Address = &stop.Address

		// Determine task type and map to original task
		// Note: Mapbox returns "service" for collections, "pickup"/"dropoff" for shipments
		switch stop.Type {

		case optimization.StopTypeEnd:
			// Map end to warehouse_stop so mobile knows to return to warehouse
			task.TaskType = "warehouse_stop"
			log.Printf("   ✅ Mapped end stop to warehouse_stop (return to warehouse)")

		case "service", optimization.StopTypeCollection:
			// Mapbox returns "service" type, OR-Tools returns "collection" type
			// Match by CollectionID which we extracted from the services array
			if stop.CollectionID != "" {
				matched := false

				// First: check if this is a placement modeled as service (binsPreloaded).
				// The CollectionID will be "service-{potentialLocationId}".
				// Map it back to the original placement task, NOT a new service task.
				svcID := strings.TrimPrefix(stop.CollectionID, "service-")
				for _, origTask := range existingTasks {
					if origTask.TaskType == "placement" && origTask.PotentialLocationID != nil && *origTask.PotentialLocationID == svcID {
						task.TaskType = "placement"
						task.PotentialLocationID = origTask.PotentialLocationID
						task.NewBinNumber = origTask.NewBinNumber
						log.Printf("   ✅ Matched service→placement (preloaded bins): %s", svcID[:8])
						matched = true
						break
					}
				}

				// Check if this is a redeployment modeled as service (binsPreloaded).
				// The CollectionID will be "service-{moveRequestId}".
				// Map it back to the original dropoff task.
				if !matched {
					for _, origTask := range existingTasks {
						if origTask.TaskType == "dropoff" && origTask.MoveRequestID != nil && *origTask.MoveRequestID == svcID {
							task.TaskType = "dropoff"
							task.MoveRequestID = origTask.MoveRequestID
							task.BinID = origTask.BinID
							task.BinNumber = origTask.BinNumber
							task.DestinationLatitude = origTask.DestinationLatitude
							task.DestinationLongitude = origTask.DestinationLongitude
							task.DestinationAddress = origTask.DestinationAddress
							log.Printf("   ✅ Matched service→dropoff (redeployment preloaded): %s", svcID[:8])
							matched = true
							break
						}
					}
				}

				// Then: check if it's a real service task (custom shift stop)
				if !matched {
					for _, svcTask := range req.ServiceTasks {
						if stop.CollectionID == fmt.Sprintf("service-%s", svcTask.ID) {
							task.TaskType = "service"
							label := svcTask.Label
							task.TaskLabel = &label
							log.Printf("   ✅ Matched service to service task: %s", svcTask.Label)
							matched = true
							break
						}
					}
				}

				// Finally: try matching to a collection
				if !matched {
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

		case optimization.StopTypeDropoff, optimization.StopTypePlacement:
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

		// Find existing task if first optimization
		var existingTask *models.RouteTask
		if isFirstOptimization {
			// Try to match by bin_id, potential_location_id, move_request_id, or service task ID
			if task.BinID != nil {
				existingTask = existingTasksMap[*task.BinID]
			} else if task.PotentialLocationID != nil {
				// Use composite key to match placement vs warehouse_stop correctly
				existingTask = existingTasksMap[*task.PotentialLocationID+":"+string(task.TaskType)]
			} else if task.MoveRequestID != nil {
				// Use composite key to match pickup vs dropoff correctly
				existingTask = existingTasksMap[*task.MoveRequestID+":"+string(task.TaskType)]
			} else if task.TaskType == "service" && stop.CollectionID != "" {
				// Service tasks: extract original task ID from "service-{uuid}"
				svcID := strings.TrimPrefix(stop.CollectionID, "service-")
				existingTask = existingTasksMap[svcID]
			}
		}

		if isFirstOptimization && existingTask != nil {
			// Existing row: queue an in-place UPDATE (preserve ID, refresh coords) for ApplyOrder.
			task.ID = existingTask.ID
			orderedStops = append(orderedStops, itinerary.OrderedStop{Task: task})
			log.Printf("   ✏️  Queued existing task %s for in-place update", existingTask.ID[:8])
		} else {
			// No existing match (warehouse stop; or — dead path — a reopt insert): queue a new row.
			task.ID = uuid.New().String()
			orderedStops = append(orderedStops, itinerary.OrderedStop{Insert: true, Task: task})
			log.Printf("   💾 Queued new task %s for insert (type=%s)", task.ID[:8], task.TaskType)
		}

		// Increment sequence for next non-skipped stop
		sequenceOrder++
	}

	if isFirstOptimization {
		log.Printf("✅ [MAPBOX OPTIMIZER] Updated existing tasks with optimized sequence order")
	} else {
		log.Printf("✅ [MAPBOX OPTIMIZER] Created %d new optimized route tasks", len(route.Stops)-2) // -2 for start/end
	}

	// Persist the queued order atomically via the itinerary domain. ApplyOrder UPDATEs existing
	// rows in place (preserving IDs + refreshing coords on first-opt), INSERTs new warehouse stops,
	// then renumbers to a dense 1..N. isFirst=true uses the sequence_order/created_at renumber
	// (NOT warehouse-last) so a leading warehouse pickup and binsPreloaded auto-completed pickups
	// keep their positions — the legacy first-opt behavior.
	if err := itinerary.ApplyOrder(tx, shiftID, orderedStops, isFirstOptimization); err != nil {
		return fmt.Errorf("failed to apply optimized order: %w", err)
	}

	// 🔍 DEBUG: Verify what was ACTUALLY saved to database
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("💾 [DEBUG] FINAL DATABASE STATE - Tasks saved with their sequence_order:")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	var finalTasks []models.RouteTask
	finalTasksQuery := `
		SELECT id, shift_id, task_type, sequence_order,
		       bin_id, bin_number, latitude, longitude, address
		FROM route_tasks
		WHERE shift_id = $1 AND is_deleted = false
		ORDER BY sequence_order ASC
	`
	err = tx.Select(&finalTasks, finalTasksQuery, shiftID)
	if err != nil {
		log.Printf("⚠️  Warning: Could not fetch final tasks for debug logging: %v", err)
	} else {
		for i, task := range finalTasks {
			addr := "N/A"
			if task.Address != nil {
				addr = *task.Address
			}
			binInfo := ""
			if task.BinID != nil && task.BinNumber != nil {
				binInfo = fmt.Sprintf(" [Bin #%d]", *task.BinNumber)
			}
			log.Printf("   #%d: [%s]%s %s - Seq: %d - %.6f, %.6f",
				i+1, task.TaskType, binInfo, addr, task.SequenceOrder, task.Latitude, task.Longitude)
		}
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📊 [DEBUG] Total tasks in database after optimization: %d", len(finalTasks))
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	}

	// Step 7: Save optimization metadata to shifts table
	totalMiles := float64(route.TotalDistance) / 1609.34
	totalDuration := route.TotalDuration
	durationHours := totalDuration / 3600
	durationMins := (totalDuration % 3600) / 60
	var durationFmt string
	if durationHours > 0 {
		durationFmt = fmt.Sprintf("%dh %dm", durationHours, durationMins)
	} else {
		durationFmt = fmt.Sprintf("%dm", durationMins)
	}

	optimizationMetadata := models.OptimizationMetadata{
		TotalDistanceMiles:     totalMiles,
		TotalDurationSeconds:   totalDuration,
		TotalDurationFormatted: durationFmt,
		OptimizedAt:            time.Now().Format(time.RFC3339),
		EstimatedCompletion:    time.Now().Add(time.Duration(totalDuration) * time.Second).Format(time.RFC3339),
	}

	metadataJSON, err := json.Marshal(optimizationMetadata)
	if err != nil {
		log.Printf("⚠️  Failed to marshal optimization metadata: %v", err)
	} else {
		_, err = tx.Exec(`
			UPDATE shifts
			SET optimization_metadata = $1
			WHERE id = $2
		`, metadataJSON, shiftID)
		if err != nil {
			log.Printf("⚠️  Failed to save optimization metadata: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit optimize persist: %w", err)
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
		"type":          "route_updated",
		"change_type":   changeType,
		"details":       changeDetails,
		"updated_tasks": tasks,
		"timestamp":     time.Now().Unix(),
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
