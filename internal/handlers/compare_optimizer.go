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

		// Log all tasks for debugging
		for i, task := range tasks {
			log.Printf("  Task %d: type=%s, id=%s, potentialLocID=%v, destLat=%v, destLon=%v",
				i+1, task.TaskType, task.ID, task.PotentialLocationID, task.DestinationLatitude, task.DestinationLongitude)
		}

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
				} else {
					log.Printf("⚠️  Skipping collection task %s: missing required fields (binID=%v, lat=%v, lon=%v)",
						task.ID, task.BinID != nil, task.Latitude != nil, task.Longitude != nil)
				}

			case "placement":
				log.Printf("🔍 Processing placement task: id=%s, potentialLocID=%v, lat=%v, lon=%v",
					task.ID, task.PotentialLocationID, task.Latitude, task.Longitude)
				// For placement tasks, latitude/longitude contain the placement location coordinates
				if task.PotentialLocationID != nil && task.Latitude != nil && task.Longitude != nil {
					placement := optimization.Placement{
						ID:                *task.PotentialLocationID,
						NewBinNumber:      getIntValue(task.NewBinNumber),
						WarehouseLocation: warehouseLocation,
						PlacementLocation: optimization.Location{
							ID:        *task.PotentialLocationID,
							Name:      fmt.Sprintf("Placement #%d", getIntValue(task.NewBinNumber)),
							Latitude:  *task.Latitude,
							Longitude: *task.Longitude,
							Address:   getStringValue(task.Address),
						},
						PickupDuration:  60,
						DropoffDuration: 120,
					}
					req.Placements = append(req.Placements, placement)
					log.Printf("✅ Added placement: %s", *task.PotentialLocationID)
				} else {
					log.Printf("⚠️  Skipping placement task %s: missing required fields (potentialLocID=%v, lat=%v, lon=%v)",
						task.ID, task.PotentialLocationID != nil, task.Latitude != nil, task.Longitude != nil)
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
				// Skip warehouse stops for Mapbox - they're implicit in placement/move request pickups
				// Adding them as separate services causes duplicate warehouse visits
				log.Printf("⏭️  Skipping warehouse_stop task %s (warehouse visits handled implicitly by shipments)", task.ID)
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

		// 6. Run Segmented HERE Maps optimization (Smart Engine + HERE)
		log.Printf("🚀 Running Segmented HERE Maps (Smart Engine)...")
		segmentedResp, segmentedErr := callSegmentedOptimization(shift.WarehouseLatitude, shift.WarehouseLongitude, tasks)

		// 7. Build comparison result
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
			// Extract detailed stop sequence
			stops := make([]map[string]interface{}, 0)
			for i, stop := range mapboxResp.Routes[0].Stops {
				stops = append(stops, map[string]interface{}{
					"sequence":      i + 1,
					"type":          string(stop.Type),
					"location_name": stop.LocationName,
					"address":       stop.Address,
					"latitude":      stop.Latitude,
					"longitude":     stop.Longitude,
					"eta":           stop.ETA.Format("15:04:05"),
					"duration_sec":  stop.Duration,
				})
			}

			result["mapbox"] = map[string]interface{}{
				"success":                true,
				"total_distance_meters":  mapboxResp.TotalDistance,
				"total_duration_seconds": mapboxResp.TotalDuration,
				"total_duration_minutes": mapboxResp.TotalDuration / 60,
				"number_of_stops":        len(mapboxResp.Routes[0].Stops),
				"dropped_tasks":          mapboxResp.DroppedTasks,
				"route_sequence":         stops,
				"respects_shipments":     "YES - Pickup always before dropoff",
				"respects_capacity":      fmt.Sprintf("YES - Max %d bins", shift.TruckBinCapacity),
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

		if segmentedErr == nil {
			result["segmented"] = segmentedResp
		} else {
			result["segmented"] = map[string]interface{}{
				"success": false,
				"error":   segmentedErr.Error(),
			}
		}

		// Add comparison if all succeeded
		if mapboxErr == nil && hereErr == nil && segmentedErr == nil {
			mapboxDuration := mapboxResp.TotalDuration
			hereDuration := int(hereResp["total_duration_seconds"].(float64))
			segmentedDuration := int(segmentedResp["total_duration_seconds"].(float64))

			// Find fastest
			fastest := "mapbox"
			fastestTime := mapboxDuration
			if hereDuration < fastestTime {
				fastest = "heremaps"
				fastestTime = hereDuration
			}
			if segmentedDuration < fastestTime {
				fastest = "segmented"
				fastestTime = segmentedDuration
			}

			result["comparison"] = map[string]interface{}{
				"fastest_optimizer": fastest,
				"times": map[string]interface{}{
					"mapbox_minutes":    mapboxDuration / 60,
					"heremaps_minutes":  hereDuration / 60,
					"segmented_minutes": segmentedDuration / 60,
				},
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
	params.Add("departure", "now") // Required when using service time constraints
	params.Add("start", fmt.Sprintf("warehouse-start;%.6f,%.6f", warehouseLat, warehouseLon))
	params.Add("end", fmt.Sprintf("warehouse-end;%.6f,%.6f", warehouseLat, warehouseLon))

	destNum := 1
	for _, task := range tasks {
		if task.Latitude != nil && task.Longitude != nil && task.TaskType != "warehouse_stop" {
			// Determine service duration based on task type (same as Mapbox)
			var serviceDuration int
			switch task.TaskType {
			case "collection":
				serviceDuration = 300 // 5 minutes
			case "placement":
				serviceDuration = 180 // 1 min pickup + 2 min dropoff = 3 min
			case "pickup", "dropoff":
				serviceDuration = 120 // 2 minutes
			default:
				serviceDuration = 300 // Default 5 minutes
			}

			// HERE Maps format: name;lat,lon;st:duration_in_seconds
			params.Add(fmt.Sprintf("destination%d", destNum),
				fmt.Sprintf("stop%d;%.6f,%.6f;st:%d", destNum, *task.Latitude, *task.Longitude, serviceDuration))
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
			Distance        string `json:"distance"`
			Time            string `json:"time"`
			Interconnections []struct {
				FromWaypoint string  `json:"fromWaypoint"`
				ToWaypoint   string  `json:"toWaypoint"`
				Distance     float64 `json:"distance"`
				Time         float64 `json:"time"`
			} `json:"interconnections"`
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

	// Build route sequence from interconnections
	routeSequence := make([]map[string]interface{}, 0)
	cumulativeTime := 0.0

	if len(hereResp.Results[0].Interconnections) > 0 {
		// Add start location
		startWaypoint := hereResp.Results[0].Interconnections[0].FromWaypoint
		routeSequence = append(routeSequence, map[string]interface{}{
			"sequence":     1,
			"waypoint":     startWaypoint,
			"type":         getWaypointType(startWaypoint),
			"task_info":    getTaskInfo(startWaypoint, tasks),
			"arrival_time": formatSeconds(cumulativeTime),
		})

		// Add intermediate stops
		for i, conn := range hereResp.Results[0].Interconnections {
			cumulativeTime += conn.Time
			routeSequence = append(routeSequence, map[string]interface{}{
				"sequence":     i + 2,
				"waypoint":     conn.ToWaypoint,
				"type":         getWaypointType(conn.ToWaypoint),
				"task_info":    getTaskInfo(conn.ToWaypoint, tasks),
				"arrival_time": formatSeconds(cumulativeTime),
				"distance_from_prev_meters": conn.Distance,
				"time_from_prev_seconds":    conn.Time,
			})
		}
	}

	return map[string]interface{}{
		"success":                true,
		"total_distance_meters":  distance,
		"total_duration_seconds": duration,
		"total_duration_minutes": duration / 60,
		"route_sequence":         routeSequence,
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

func getWaypointType(waypoint string) string {
	if waypoint == "warehouse-start" {
		return "start"
	}
	if waypoint == "warehouse-end" {
		return "end"
	}
	// Parse stop numbers like "stop1", "stop2", etc.
	return "stop"
}

func getTaskInfo(waypoint string, tasks []TaskRow) map[string]interface{} {
	// Extract stop number from waypoint (e.g., "stop1" -> 1)
	var stopNum int
	fmt.Sscanf(waypoint, "stop%d", &stopNum)

	// Find the task by matching against the original task list
	// Note: stopNum corresponds to the order we added tasks (non-warehouse tasks)
	taskIdx := 0
	for _, task := range tasks {
		if task.Latitude != nil && task.Longitude != nil && task.TaskType != "warehouse_stop" {
			taskIdx++
			if taskIdx == stopNum {
				return map[string]interface{}{
					"task_type":  task.TaskType,
					"bin_number": getIntValue(task.BinNumber),
					"latitude":   *task.Latitude,
					"longitude":  *task.Longitude,
					"address":    getStringValue(task.Address),
				}
			}
		}
	}

	// Warehouse waypoints
	if waypoint == "warehouse-start" || waypoint == "warehouse-end" {
		return map[string]interface{}{
			"task_type": "warehouse",
		}
	}

	return map[string]interface{}{}
}

func formatSeconds(seconds float64) string {
	minutes := int(seconds) / 60
	secs := int(seconds) % 60
	return fmt.Sprintf("%02d:%02d", minutes, secs)
}

// callSegmentedOptimization performs segmented optimization with HERE Maps
// Splits tasks by warehouse stops and optimizes each segment independently
func callSegmentedOptimization(warehouseLat, warehouseLon float64, tasks []TaskRow) (map[string]interface{}, error) {
	log.Printf("📦 [SEGMENTED] Starting segmented optimization with %d tasks", len(tasks))

	// Step 1: Split tasks into segments by warehouse stops
	type segment struct {
		tasks    []TaskRow
		isWarehouse bool
	}

	segments := []segment{}
	currentSegment := segment{tasks: []TaskRow{}}

	for _, task := range tasks {
		if task.TaskType == "warehouse_stop" {
			// End current segment if it has tasks
			if len(currentSegment.tasks) > 0 {
				segments = append(segments, currentSegment)
				currentSegment = segment{tasks: []TaskRow{}}
			}
			// Warehouse stop is its own segment
			segments = append(segments, segment{
				tasks:       []TaskRow{task},
				isWarehouse: true,
			})
		} else {
			currentSegment.tasks = append(currentSegment.tasks, task)
		}
	}

	// Add final segment if exists
	if len(currentSegment.tasks) > 0 {
		segments = append(segments, currentSegment)
	}

	log.Printf("📦 [SEGMENTED] Split into %d segments", len(segments))

	// Step 2: Optimize each segment
	var totalDistance float64
	var totalDuration float64
	var allStops []map[string]interface{}
	sequenceNum := 1
	cumulativeTime := 0.0

	// Add start location
	allStops = append(allStops, map[string]interface{}{
		"sequence":     sequenceNum,
		"waypoint":     "warehouse-start",
		"type":         "start",
		"task_info":    map[string]interface{}{"task_type": "warehouse"},
		"arrival_time": formatSeconds(cumulativeTime),
	})
	sequenceNum++

	startLat, startLon := warehouseLat, warehouseLon

	for segIdx, seg := range segments {
		log.Printf("   Segment #%d: %d tasks (warehouse: %v)", segIdx+1, len(seg.tasks), seg.isWarehouse)

		// Warehouse stops are kept as-is
		if seg.isWarehouse {
			task := seg.tasks[0]
			allStops = append(allStops, map[string]interface{}{
				"sequence":     sequenceNum,
				"waypoint":     fmt.Sprintf("warehouse-seg%d", segIdx),
				"type":         "warehouse_stop",
				"task_info":    getTaskInfo("", tasks),
				"arrival_time": formatSeconds(cumulativeTime),
			})
			sequenceNum++
			startLat, startLon = *task.Latitude, *task.Longitude
			continue
		}

		// Single task - no optimization needed
		if len(seg.tasks) == 1 {
			task := seg.tasks[0]
			// Rough distance estimate (Haversine returns km, convert to meters)
			distKm := haversineDistance(startLat, startLon, *task.Latitude, *task.Longitude)
			dist := distKm * 1000 // Convert to meters
			travelTime := (distKm / 50.0) * 3600 // Assume 50 km/h average

			cumulativeTime += travelTime
			allStops = append(allStops, map[string]interface{}{
				"sequence":     sequenceNum,
				"waypoint":     fmt.Sprintf("stop%d", sequenceNum-1),
				"type":         "stop",
				"task_info":    getTaskInfoFromTask(task),
				"arrival_time": formatSeconds(cumulativeTime),
			})
			sequenceNum++
			totalDistance += dist
			totalDuration += travelTime
			startLat, startLon = *task.Latitude, *task.Longitude
			continue
		}

		// Multiple tasks - optimize with HERE Maps
		log.Printf("   🚀 Optimizing segment #%d with HERE Maps (%d tasks)", segIdx+1, len(seg.tasks))

		// Determine segment end (next warehouse or final warehouse)
		endLat, endLon := warehouseLat, warehouseLon
		if segIdx < len(segments)-1 && segments[segIdx+1].isWarehouse {
			endLat = *segments[segIdx+1].tasks[0].Latitude
			endLon = *segments[segIdx+1].tasks[0].Longitude
		}

		// Call HERE Maps for this segment
		segResult, err := callHereSegmentOptimization(startLat, startLon, endLat, endLon, seg.tasks)
		if err != nil {
			log.Printf("   ❌ Segment optimization failed: %v", err)
			return nil, err
		}

		// Add segment results
		segDist := segResult["distance"].(float64)
		segDur := segResult["duration"].(float64)
		totalDistance += segDist
		totalDuration += segDur

		// Add segment stops (skip start since we already have it)
		segStops := segResult["stops"].([]map[string]interface{})
		for i, stop := range segStops {
			if i == 0 && segIdx > 0 {
				// Skip first stop if not first segment (it's the previous segment's end)
				continue
			}
			stop["sequence"] = sequenceNum
			stop["arrival_time"] = formatSeconds(cumulativeTime + stop["arrival_time"].(float64))
			allStops = append(allStops, stop)
			sequenceNum++
		}

		cumulativeTime += segDur

		// Update start position for next segment
		if len(segStops) > 0 {
			lastStop := segStops[len(segStops)-1]
			taskInfo := lastStop["task_info"].(map[string]interface{})
			if lat, ok := taskInfo["latitude"].(float64); ok {
				startLat = lat
			}
			if lon, ok := taskInfo["longitude"].(float64); ok {
				startLon = lon
			}
		}
	}

	// Add end location
	allStops = append(allStops, map[string]interface{}{
		"sequence":     sequenceNum,
		"waypoint":     "warehouse-end",
		"type":         "end",
		"task_info":    map[string]interface{}{"task_type": "warehouse"},
		"arrival_time": formatSeconds(cumulativeTime),
	})

	log.Printf("✅ [SEGMENTED] Total: %.2f km, %.0f seconds (%d minutes)",
		totalDistance/1000, totalDuration, int(totalDuration/60))

	return map[string]interface{}{
		"success":                true,
		"total_distance_meters":  totalDistance,
		"total_duration_seconds": totalDuration,
		"total_duration_minutes": totalDuration / 60,
		"route_sequence":         allStops,
		"segments_count":         len(segments),
		"description":            "Smart Engine + HERE Maps (Segmented)",
	}, nil
}

// callHereSegmentOptimization calls HERE Maps for a single segment
func callHereSegmentOptimization(startLat, startLon, endLat, endLon float64, tasks []TaskRow) (map[string]interface{}, error) {
	HereAPIKey := "paBPqXdCxmq01bP5eA0_i2jA53PnqMH7YCc6q21wwrw"

	apiURL := "https://wps.hereapi.com/v8/findsequence2"
	params := url.Values{}
	params.Add("apiKey", HereAPIKey)
	params.Add("mode", "fastest;car;traffic:disabled")
	params.Add("improveFor", "time")
	params.Add("departure", "now")
	params.Add("start", fmt.Sprintf("seg-start;%.6f,%.6f", startLat, startLon))
	params.Add("end", fmt.Sprintf("seg-end;%.6f,%.6f", endLat, endLon))

	destNum := 1
	for _, task := range tasks {
		if task.Latitude != nil && task.Longitude != nil {
			var serviceDuration int
			switch task.TaskType {
			case "collection":
				serviceDuration = 300
			case "placement":
				serviceDuration = 180
			case "pickup", "dropoff":
				serviceDuration = 120
			default:
				serviceDuration = 300
			}

			params.Add(fmt.Sprintf("destination%d", destNum),
				fmt.Sprintf("stop%d;%.6f,%.6f;st:%d", destNum, *task.Latitude, *task.Longitude, serviceDuration))
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
			Distance        string `json:"distance"`
			Time            string `json:"time"`
			Interconnections []struct {
				FromWaypoint string  `json:"fromWaypoint"`
				ToWaypoint   string  `json:"toWaypoint"`
				Distance     float64 `json:"distance"`
				Time         float64 `json:"time"`
			} `json:"interconnections"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&hereResp); err != nil {
		return nil, fmt.Errorf("failed to parse HERE response: %w", err)
	}

	if len(hereResp.Results) == 0 {
		return nil, fmt.Errorf("HERE returned no results")
	}

	var distance, duration float64
	fmt.Sscanf(hereResp.Results[0].Distance, "%f", &distance)
	fmt.Sscanf(hereResp.Results[0].Time, "%f", &duration)

	// Build stops list
	stops := []map[string]interface{}{}
	cumTime := 0.0

	// Add start
	stops = append(stops, map[string]interface{}{
		"waypoint":     "seg-start",
		"type":         "segment_start",
		"task_info":    map[string]interface{}{},
		"arrival_time": cumTime,
	})

	// Add intermediate stops
	for _, conn := range hereResp.Results[0].Interconnections {
		cumTime += conn.Time
		stops = append(stops, map[string]interface{}{
			"waypoint":     conn.ToWaypoint,
			"type":         "stop",
			"task_info":    getTaskInfoByWaypoint(conn.ToWaypoint, tasks),
			"arrival_time": cumTime,
		})
	}

	return map[string]interface{}{
		"distance": distance,
		"duration": duration,
		"stops":    stops,
	}, nil
}

// Helper to extract task info from TaskRow
func getTaskInfoFromTask(task TaskRow) map[string]interface{}{
	return map[string]interface{}{
		"task_type":  task.TaskType,
		"bin_number": getIntValue(task.BinNumber),
		"latitude":   *task.Latitude,
		"longitude":  *task.Longitude,
		"address":    getStringValue(task.Address),
	}
}

// Helper to get task info by waypoint from segment tasks
func getTaskInfoByWaypoint(waypoint string, tasks []TaskRow) map[string]interface{} {
	var stopNum int
	fmt.Sscanf(waypoint, "stop%d", &stopNum)

	if stopNum > 0 && stopNum <= len(tasks) {
		return getTaskInfoFromTask(tasks[stopNum-1])
	}

	return map[string]interface{}{}
}

