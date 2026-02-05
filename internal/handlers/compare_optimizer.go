package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"ropacal-backend/internal/services/optimization"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// CompareOptimizerForShift compares the current route with Mapbox v2 optimization
func CompareOptimizerForShift(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "id")
		log.Printf("🔍 Comparing optimization for shift: %s", shiftID)

		// 1. Fetch shift details
		var shift struct {
			ID                 string  `db:"id"`
			DriverID           string  `db:"driver_id"`
			TruckBinCapacity   int     `db:"truck_bin_capacity"`
			WarehouseLatitude  float64 `db:"warehouse_latitude"`
			WarehouseLongitude float64 `db:"warehouse_longitude"`
			WarehouseAddress   string  `db:"warehouse_address"`
		}

		err := db.Get(&shift, `
			SELECT id, driver_id, truck_bin_capacity, warehouse_latitude,
			       warehouse_longitude, warehouse_address
			FROM shifts
			WHERE id = $1
		`, shiftID)
		if err == sql.ErrNoRows {
			http.Error(w, "Shift not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			http.Error(w, "Failed to fetch shift", http.StatusInternalServerError)
			return
		}

		log.Printf("📦 Shift found: Driver=%s, Capacity=%d bins", shift.DriverID, shift.TruckBinCapacity)

		// 2. Fetch all tasks for this shift
		type TaskRow struct {
			ID                  string   `db:"id"`
			ShiftID             string   `db:"shift_id"`
			TaskType            string   `db:"task_type"`
			SequenceOrder       int      `db:"sequence_order"`
			BinID               *string  `db:"bin_id"`
			BinNumber           *int     `db:"bin_number"`
			Latitude            *float64 `db:"latitude"`
			Longitude           *float64 `db:"longitude"`
			Address             *string  `db:"address"`
			FillPercentage      *int     `db:"fill_percentage"`
			NewBinNumber        *int     `db:"new_bin_number"`
			MoveRequestID       *string  `db:"move_request_id"`
			PickupLatitude      *float64 `db:"pickup_latitude"`
			PickupLongitude     *float64 `db:"pickup_longitude"`
			PickupAddress       *string  `db:"pickup_address"`
			DropoffLatitude     *float64 `db:"dropoff_latitude"`
			DropoffLongitude    *float64 `db:"dropoff_longitude"`
			DropoffAddress      *string  `db:"dropoff_address"`
			PotentialLocationID *string  `db:"potential_location_id"`
			WarehouseAction     *string  `db:"warehouse_action"`
			BinsToLoad          *int     `db:"bins_to_load"`
		}

		var tasks []TaskRow
		err = db.Select(&tasks, `
			SELECT
				id, shift_id, task_type, sequence_order,
				bin_id, bin_number, latitude, longitude, address, fill_percentage,
				new_bin_number, move_request_id,
				pickup_latitude, pickup_longitude, pickup_address,
				dropoff_latitude, dropoff_longitude, dropoff_address,
				potential_location_id, warehouse_action, bins_to_load
			FROM shift_bins
			WHERE shift_id = $1
			ORDER BY sequence_order ASC
		`, shiftID)
		if err != nil {
			log.Printf("❌ Error fetching tasks: %v", err)
			http.Error(w, "Failed to fetch tasks", http.StatusInternalServerError)
			return
		}

		log.Printf("📋 Found %d tasks for shift", len(tasks))

		// 3. Convert to optimization.RouteRequest
		req := &optimization.RouteRequest{
			Vehicles:       make([]optimization.Vehicle, 1),
			Collections:    make([]optimization.Collection, 0),
			Placements:     make([]optimization.Placement, 0),
			MoveRequests:   make([]optimization.MoveRequest, 0),
			WarehouseStops: make([]optimization.WarehouseStop, 0),
		}

		// Define warehouse location
		warehouseLocation := optimization.Location{
			ID:        "warehouse",
			Name:      "Warehouse",
			Latitude:  shift.WarehouseLatitude,
			Longitude: shift.WarehouseLongitude,
			Address:   shift.WarehouseAddress,
		}

		// Define vehicle
		req.Vehicles[0] = optimization.Vehicle{
			ID:            shift.DriverID,
			Name:          fmt.Sprintf("Truck-%s", shift.DriverID[:8]),
			StartLocation: warehouseLocation,
			EndLocation:   warehouseLocation,
			Capacities: map[string]int{
				"bins": shift.TruckBinCapacity,
			},
		}

		// Convert tasks
		for _, task := range tasks {
			switch task.TaskType {
			case "collection":
				if task.BinID != nil && task.Latitude != nil && task.Longitude != nil {
					collection := optimization.Collection{
						ID:             task.ID,
						BinID:          *task.BinID,
						BinNumber:      getIntValue(task.BinNumber),
						Location: optimization.Location{
							ID:        *task.BinID,
							Name:      fmt.Sprintf("Bin #%d", getIntValue(task.BinNumber)),
							Latitude:  *task.Latitude,
							Longitude: *task.Longitude,
							Address:   getStringValue(task.Address),
						},
						Duration:       300, // 5 minutes default
						FillPercentage: getIntValue(task.FillPercentage),
					}
					req.Collections = append(req.Collections, collection)
					log.Printf("   ➕ Collection: Bin #%d at (%.6f, %.6f)", collection.BinNumber, collection.Location.Latitude, collection.Location.Longitude)
				}

			case "placement":
				if task.PotentialLocationID != nil && task.DropoffLatitude != nil && task.DropoffLongitude != nil {
					placement := optimization.Placement{
						ID:                *task.PotentialLocationID,
						NewBinNumber:      getIntValue(task.NewBinNumber),
						WarehouseLocation: warehouseLocation,
						PlacementLocation: optimization.Location{
							ID:        *task.PotentialLocationID,
							Name:      fmt.Sprintf("Placement Site #%d", getIntValue(task.NewBinNumber)),
							Latitude:  *task.DropoffLatitude,
							Longitude: *task.DropoffLongitude,
							Address:   getStringValue(task.DropoffAddress),
						},
						PickupDuration:  60,  // 1 minute to load bin
						DropoffDuration: 120, // 2 minutes to place bin
					}
					req.Placements = append(req.Placements, placement)
					log.Printf("   ➕ Placement: Bin #%d at (%.6f, %.6f)", placement.NewBinNumber, placement.PlacementLocation.Latitude, placement.PlacementLocation.Longitude)
				}

			case "pickup", "dropoff":
				// For move requests, we need to ensure we only add once (not for both pickup and dropoff)
				if task.MoveRequestID != nil && task.TaskType == "pickup" {
					if task.PickupLatitude != nil && task.PickupLongitude != nil &&
						task.DropoffLatitude != nil && task.DropoffLongitude != nil {
						moveRequest := optimization.MoveRequest{
							ID:        *task.MoveRequestID,
							BinID:     getStringValue(task.BinID),
							BinNumber: getIntValue(task.BinNumber),
							PickupLocation: optimization.Location{
								ID:        fmt.Sprintf("%s-pickup", *task.MoveRequestID),
								Name:      fmt.Sprintf("Pickup Bin #%d", getIntValue(task.BinNumber)),
								Latitude:  *task.PickupLatitude,
								Longitude: *task.PickupLongitude,
								Address:   getStringValue(task.PickupAddress),
							},
							DropoffLocation: optimization.Location{
								ID:        fmt.Sprintf("%s-dropoff", *task.MoveRequestID),
								Name:      fmt.Sprintf("Dropoff Bin #%d", getIntValue(task.BinNumber)),
								Latitude:  *task.DropoffLatitude,
								Longitude: *task.DropoffLongitude,
								Address:   getStringValue(task.DropoffAddress),
							},
							PickupDuration:  120, // 2 minutes to pick up
							DropoffDuration: 120, // 2 minutes to drop off
						}
						req.MoveRequests = append(req.MoveRequests, moveRequest)
						log.Printf("   ➕ Move Request: Bin #%d from (%.6f, %.6f) to (%.6f, %.6f)",
							moveRequest.BinNumber,
							moveRequest.PickupLocation.Latitude, moveRequest.PickupLocation.Longitude,
							moveRequest.DropoffLocation.Latitude, moveRequest.DropoffLocation.Longitude)
					}
				}

			case "warehouse_stop":
				warehouseStop := optimization.WarehouseStop{
					ID:       task.ID,
					Location: warehouseLocation,
					Duration: 300, // 5 minutes default
					Action:   getStringValue(task.WarehouseAction),
					BinsToLoad: getIntValue(task.BinsToLoad),
				}
				req.WarehouseStops = append(req.WarehouseStops, warehouseStop)
				log.Printf("   ➕ Warehouse Stop: %s (%d bins)", warehouseStop.Action, warehouseStop.BinsToLoad)
			}
		}

		log.Printf("📊 Route Request Summary:")
		log.Printf("   - Collections: %d", len(req.Collections))
		log.Printf("   - Placements: %d", len(req.Placements))
		log.Printf("   - Move Requests: %d", len(req.MoveRequests))
		log.Printf("   - Warehouse Stops: %d", len(req.WarehouseStops))

		// 4. Run through Mapbox v2 optimizer
		log.Printf("🚀 Running Mapbox v2 optimization...")
		optimizer := optimization.NewMapboxOptimizer()

		response, err := optimizer.OptimizeRoute(req)
		if err != nil {
			log.Printf("❌ Optimization failed: %v", err)
			http.Error(w, fmt.Sprintf("Optimization failed: %v", err), http.StatusInternalServerError)
			return
		}

		log.Printf("✅ Optimization complete!")
		log.Printf("   - Total Routes: %d", len(response.Routes))
		log.Printf("   - Total Distance: %.0fm", response.TotalDistance)
		log.Printf("   - Total Duration: %ds", response.TotalDuration)
		log.Printf("   - Dropped Tasks: %d", len(response.DroppedTasks))

		// 5. Return the result
		result := map[string]interface{}{
			"shift_id": shiftID,
			"original_tasks": map[string]interface{}{
				"total":           len(tasks),
				"collections":     len(req.Collections),
				"placements":      len(req.Placements),
				"move_requests":   len(req.MoveRequests),
				"warehouse_stops": len(req.WarehouseStops),
			},
			"mapbox_v2_result": response,
			"optimizer_used":   optimizer.Name(),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// Helper functions
func getStringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func getIntValue(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}
