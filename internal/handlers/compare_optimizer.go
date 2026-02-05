package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"

	"ropacal-backend/internal/services/optimization"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

// TaskRow represents a route task row from the database
type TaskRow struct {
	ID                   string   `db:"id"`
	TaskType             string   `db:"task_type"`
	SequenceOrder        int      `db:"sequence_order"`
	BinID                *string  `db:"bin_id"`
	BinNumber            *int     `db:"bin_number"`
	Latitude             *float64 `db:"latitude"`
	Longitude            *float64 `db:"longitude"`
	Address              *string  `db:"address"`
	FillPercentage       *int     `db:"fill_percentage"`
	NewBinNumber         *int     `db:"new_bin_number"`
	MoveRequestID        *string  `db:"move_request_id"`
	DestinationLatitude  *float64 `db:"destination_latitude"`
	DestinationLongitude *float64 `db:"destination_longitude"`
	DestinationAddress   *string  `db:"destination_address"`
	PotentialLocationID  *string  `db:"potential_location_id"`
	WarehouseAction      *string  `db:"warehouse_action"`
	BinsToLoad           *int     `db:"bins_to_load"`
}

// CompareOptimizerForShift compares Mapbox v2 vs HERE Maps optimization
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

		// 2. Fetch all tasks for this shift from route_tasks table
		var tasks []TaskRow
		err = db.Select(&tasks, `
			SELECT
				id, task_type, sequence_order,
				bin_id, bin_number, latitude, longitude, address, fill_percentage,
				new_bin_number, move_request_id,
				destination_latitude, destination_longitude, destination_address,
				potential_location_id, warehouse_action, bins_to_load
			FROM route_tasks
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
						Duration:       300,
						FillPercentage: getIntValue(task.FillPercentage),
					}
					req.Collections = append(req.Collections, collection)
				}

			case "placement":
				if task.PotentialLocationID != nil && task.DestinationLatitude != nil && task.DestinationLongitude != nil {
					placement := optimization.Placement{
						ID:                *task.PotentialLocationID,
						NewBinNumber:      getIntValue(task.NewBinNumber),
						WarehouseLocation: warehouseLocation,
						PlacementLocation: optimization.Location{
							ID:        *task.PotentialLocationID,
							Name:      fmt.Sprintf("Placement #%d", getIntValue(task.NewBinNumber)),
							Latitude:  *task.DestinationLatitude,
							Longitude: *task.DestinationLongitude,
							Address:   getStringValue(task.DestinationAddress),
						},
						PickupDuration:  60,
						DropoffDuration: 120,
					}
					req.Placements = append(req.Placements, placement)
				}

			case "pickup":
				if task.MoveRequestID != nil && task.Latitude != nil && task.Longitude != nil &&
					task.DestinationLatitude != nil && task.DestinationLongitude != nil {
					moveRequest := optimization.MoveRequest{
						ID:        *task.MoveRequestID,
						BinID:     getStringValue(task.BinID),
						BinNumber: getIntValue(task.BinNumber),
						PickupLocation: optimization.Location{
							ID:        fmt.Sprintf("%s-pickup", *task.MoveRequestID),
							Name:      fmt.Sprintf("Pickup #%d", getIntValue(task.BinNumber)),
							Latitude:  *task.Latitude,
							Longitude: *task.Longitude,
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

			case "warehouse_stop":
				warehouseStop := optimization.WarehouseStop{
					ID:         task.ID,
					Location:   warehouseLocation,
					Duration:   300,
					Action:     getStringValue(task.WarehouseAction),
					BinsToLoad: getIntValue(task.BinsToLoad),
				}
				req.WarehouseStops = append(req.WarehouseStops, warehouseStop)
			}
		}

		log.Printf("📊 Route Request: %d collections, %d placements, %d moves, %d warehouse",
			len(req.Collections), len(req.Placements), len(req.MoveRequests), len(req.WarehouseStops))

		// 4. Run Mapbox v2 optimization
		log.Printf("🚀 Running Mapbox v2...")
		mapboxOptimizer := optimization.NewMapboxOptimizer()
		mapboxResp, mapboxErr := mapboxOptimizer.OptimizeRoute(req)

		// 5. Run HERE Maps optimization
		log.Printf("🚀 Running HERE Maps...")
		hereResp, hereErr := callHereOptimization(shift.WarehouseLatitude, shift.WarehouseLongitude, tasks)

		// 6. Build comparison result
		result := map[string]interface{}{
			"shift_id": shiftID,
			"summary": map[string]interface{}{
				"total_tasks":     len(tasks),
				"collections":     len(req.Collections),
				"placements":      len(req.Placements),
				"move_requests":   len(req.MoveRequests),
				"warehouse_stops": len(req.WarehouseStops),
			},
		}

		if mapboxErr == nil {
			result["mapbox"] = map[string]interface{}{
				"success":                true,
				"total_distance_meters":  mapboxResp.TotalDistance,
				"total_duration_seconds": mapboxResp.TotalDuration,
				"total_duration_minutes": mapboxResp.TotalDuration / 60,
				"number_of_stops":        len(mapboxResp.Routes[0].Stops),
				"dropped_tasks":          mapboxResp.DroppedTasks,
			}
		} else {
			result["mapbox"] = map[string]interface{}{
				"success": false,
				"error":   mapboxErr.Error(),
			}
		}

		if hereErr == nil {
			result["heremaps"] = hereResp
		} else {
			result["heremaps"] = map[string]interface{}{
				"success": false,
				"error":   hereErr.Error(),
			}
		}

		// Add comparison if both succeeded
		if mapboxErr == nil && hereErr == nil {
			mapboxDuration := mapboxResp.TotalDuration
			hereDuration := int(hereResp["total_duration_seconds"].(float64))

			result["comparison"] = map[string]interface{}{
				"faster_optimizer":        getFasterOptimizer(mapboxDuration, hereDuration),
				"time_difference_seconds": abs(mapboxDuration - hereDuration),
				"time_difference_minutes": abs(mapboxDuration-hereDuration) / 60,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

// callHereOptimization calls HERE Maps Waypoints Sequence API
func callHereOptimization(warehouseLat, warehouseLon float64, tasks []TaskRow) (map[string]interface{}, error) {
	apiURL := "https://wps.hereapi.com/v8/findsequence2"
	params := url.Values{}
	params.Add("apiKey", HereAPIKey)
	params.Add("mode", "fastest;car;traffic:disabled")
	params.Add("improveFor", "time")
	params.Add("start", fmt.Sprintf("warehouse;%.6f,%.6f", warehouseLat, warehouseLon))
	params.Add("end", fmt.Sprintf("warehouse;%.6f,%.6f", warehouseLat, warehouseLon))

	destNum := 1
	for _, task := range tasks {
		if task.Latitude != nil && task.Longitude != nil && task.TaskType != "warehouse_stop" {
			params.Add(fmt.Sprintf("destination%d", destNum),
				fmt.Sprintf("stop%d;%.6f,%.6f", destNum, *task.Latitude, *task.Longitude))
			destNum++
		}
	}

	fullURL := fmt.Sprintf("%s?%s", apiURL, params.Encode())
	resp, err := http.Get(fullURL)
	if err != nil {
		return nil, fmt.Errorf("HERE API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HERE API returned %d: %s", resp.StatusCode, string(body))
	}

	var hereResp struct {
		Results []struct {
			Distance string `json:"distance"`
			Time     string `json:"time"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&hereResp); err != nil {
		return nil, fmt.Errorf("failed to parse HERE response: %w", err)
	}

	if len(hereResp.Results) == 0 {
		return nil, fmt.Errorf("HERE returned no results")
	}

	// Parse distance and time from strings
	var distance, duration float64
	fmt.Sscanf(hereResp.Results[0].Distance, "%f", &distance)
	fmt.Sscanf(hereResp.Results[0].Time, "%f", &duration)

	return map[string]interface{}{
		"success":                true,
		"total_distance_meters":  distance,
		"total_duration_seconds": duration,
		"total_duration_minutes": duration / 60,
	}, nil
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

func getFasterOptimizer(mapboxDuration, hereDuration int) string {
	if mapboxDuration < hereDuration {
		return "mapbox"
	}
	return "heremaps"
}
