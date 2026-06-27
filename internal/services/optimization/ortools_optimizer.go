package optimization

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ORToolsOptimizer implements the Optimizer interface using a Python OR-Tools microservice
type ORToolsOptimizer struct {
	serviceURL string
	osrmURL    string
	client     *http.Client
}

// NewORToolsOptimizer creates a new OR-Tools optimizer
func NewORToolsOptimizer() *ORToolsOptimizer {
	serviceURL := os.Getenv("ORTOOLS_SERVICE_URL")
	if serviceURL == "" {
		serviceURL = "http://localhost:8000"
	}
	osrmURL := os.Getenv("OSRM_SERVER_URL")
	if osrmURL == "" {
		osrmURL = "http://router.project-osrm.org"
	}
	return &ORToolsOptimizer{
		serviceURL: serviceURL,
		osrmURL:    osrmURL,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (o *ORToolsOptimizer) Name() string {
	return "OR-Tools"
}

// ── OSRM Table API types ──

type osrmTableResponse struct {
	Code      string      `json:"code"`
	Durations [][]float64 `json:"durations"`
	Distances [][]float64 `json:"distances"`
}

// ── OR-Tools service request/response types ──

type ortoolsLocation struct {
	ID   string  `json:"id"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Name string  `json:"name"`
}

type ortoolsVehicle struct {
	ID              string `json:"id"`
	StartLocationID string `json:"start_location_id"`
	EndLocationID   string `json:"end_location_id"`
	Capacity        int    `json:"capacity"`
	InitialLoad     int    `json:"initial_load"`
}

type ortoolsTask struct {
	ID                string `json:"id"`
	Type              string `json:"type"` // collection, placement, move_request, service
	LocationID        string `json:"location_id,omitempty"`
	WarehouseID       string `json:"warehouse_id,omitempty"`
	PickupLocationID  string `json:"pickup_location_id,omitempty"`
	DropoffLocationID string `json:"dropoff_location_id,omitempty"`
	ServiceDuration   int    `json:"service_duration"`
	PickupDuration    int    `json:"pickup_duration,omitempty"`
	DropoffDuration   int    `json:"dropoff_duration,omitempty"`
	TwEarly           *int   `json:"tw_early,omitempty"`
	TwLate            *int   `json:"tw_late,omitempty"`
}

type ortoolsRequest struct {
	Locations         []ortoolsLocation `json:"locations"`
	Vehicle           ortoolsVehicle    `json:"vehicle"`
	Tasks             []ortoolsTask     `json:"tasks"`
	DistanceMatrix    [][]int           `json:"distance_matrix"`
	DurationMatrix    [][]int           `json:"duration_matrix"`
	MaxRuntimeSeconds int               `json:"max_runtime_seconds"`
}

type ortoolsStop struct {
	TaskID             string  `json:"task_id"`
	LocationID         string  `json:"location_id"`
	Type               string  `json:"type"`
	Lat                float64 `json:"lat"`
	Lon                float64 `json:"lon"`
	ArrivalTime        int     `json:"arrival_time"`
	CumulativeDistance int     `json:"cumulative_distance"`
	CumulativeLoad     int     `json:"cumulative_load"`
	ServiceDuration    int     `json:"service_duration"`
}

type ortoolsRoute struct {
	VehicleID     string        `json:"vehicle_id"`
	Stops         []ortoolsStop `json:"stops"`
	TotalDistance  int           `json:"total_distance"`
	TotalDuration int           `json:"total_duration"`
}

type ortoolsResponse struct {
	Routes         []ortoolsRoute `json:"routes"`
	Dropped        []string       `json:"dropped"`
	Feasible       bool           `json:"feasible"`
	SolverRuntimeMs int           `json:"solver_runtime_ms"`
}

// OptimizeRoute optimizes the route using OR-Tools via the Python microservice
func (o *ORToolsOptimizer) OptimizeRoute(req *RouteRequest) (*RouteResponse, error) {
	log.Printf("🔧 [OR-Tools] Building optimization request...")

	// Guard: a vehicle is required to build tasks and the vehicle payload below.
	if req == nil || len(req.Vehicles) == 0 {
		return nil, fmt.Errorf("OR-Tools optimization requires at least one vehicle")
	}

	// Step 1: Collect all unique locations
	locations, locIDToIdx := o.collectLocations(req)
	log.Printf("📍 [OR-Tools] Collected %d unique locations", len(locations))

	// Step 2: Fetch OSRM distance/duration matrix
	distMatrix, durMatrix, err := o.fetchOSRMMatrix(locations)
	if err != nil {
		return nil, fmt.Errorf("OSRM matrix fetch failed: %w", err)
	}
	log.Printf("📊 [OR-Tools] OSRM matrix: %dx%d", len(distMatrix), len(distMatrix[0]))

	// Step 3: Build tasks
	tasks := o.buildTasks(req)
	log.Printf("📋 [OR-Tools] Built %d tasks", len(tasks))

	// Step 4: Build vehicle
	vehicle := o.buildVehicle(req, locIDToIdx)

	// Step 5: Build OR-Tools request
	ortoolsLocs := make([]ortoolsLocation, len(locations))
	for i, loc := range locations {
		ortoolsLocs[i] = ortoolsLocation{
			ID:   loc.ID,
			Lat:  loc.Latitude,
			Lon:  loc.Longitude,
			Name: loc.Name,
		}
	}

	// Scale solver time based on task count
	taskCount := len(tasks)
	maxRuntime := 3 // default for small problems
	if taskCount > 50 {
		maxRuntime = 15
	} else if taskCount > 30 {
		maxRuntime = 10
	} else if taskCount > 15 {
		maxRuntime = 5
	}
	log.Printf("⏱️  [OR-Tools] Solver time: %ds for %d tasks", maxRuntime, taskCount)

	ortoolsReq := ortoolsRequest{
		Locations:         ortoolsLocs,
		Vehicle:           vehicle,
		Tasks:             tasks,
		DistanceMatrix:    distMatrix,
		DurationMatrix:    durMatrix,
		MaxRuntimeSeconds: maxRuntime,
	}

	// Step 6: Call OR-Tools service
	ortoolsResp, err := o.callService(ortoolsReq)
	if err != nil {
		return nil, fmt.Errorf("OR-Tools service call failed: %w", err)
	}

	// Step 7: Convert response to RouteResponse
	response := o.buildRouteResponse(ortoolsResp, req)

	log.Printf("✅ [OR-Tools] Optimization complete: %d routes, %d dropped, %.1f km",
		len(response.Routes), len(response.DroppedTasks), response.TotalDistance/1000)

	return response, nil
}

// collectLocations gathers all unique locations from the request
func (o *ORToolsOptimizer) collectLocations(req *RouteRequest) ([]Location, map[string]int) {
	seen := make(map[string]bool)
	var locations []Location
	locIDToIdx := make(map[string]int)

	addLoc := func(loc Location) {
		if !seen[loc.ID] {
			seen[loc.ID] = true
			locIDToIdx[loc.ID] = len(locations)
			locations = append(locations, loc)
		}
	}

	// Vehicle start and end
	if len(req.Vehicles) > 0 {
		addLoc(req.Vehicles[0].StartLocation)
		addLoc(req.Vehicles[0].EndLocation)
	}

	// Collections
	for _, c := range req.Collections {
		addLoc(c.Location)
	}

	// Placements
	for _, p := range req.Placements {
		addLoc(p.WarehouseLocation)
		addLoc(p.PlacementLocation)
	}

	// Move requests
	for _, m := range req.MoveRequests {
		addLoc(m.PickupLocation)
		addLoc(m.DropoffLocation)
	}

	// Service tasks
	for _, st := range req.ServiceTasks {
		addLoc(st.Location)
	}

	return locations, locIDToIdx
}

// fetchOSRMMatrix calls the OSRM Table API to get NxN distance+duration matrices
func (o *ORToolsOptimizer) fetchOSRMMatrix(locations []Location) ([][]int, [][]int, error) {
	// Build coordinate string: lon,lat;lon,lat;...
	coords := make([]string, len(locations))
	for i, loc := range locations {
		coords[i] = fmt.Sprintf("%.6f,%.6f", loc.Longitude, loc.Latitude)
	}
	coordStr := strings.Join(coords, ";")

	url := fmt.Sprintf("%s/table/v1/driving/%s?annotations=duration,distance", o.osrmURL, coordStr)
	log.Printf("🗺️  [OR-Tools] Calling OSRM Table API (%d locations)...", len(locations))

	resp, err := o.client.Get(url)
	if err != nil {
		return nil, nil, fmt.Errorf("OSRM request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read OSRM response: %w", err)
	}

	var osrmResp osrmTableResponse
	if err := json.Unmarshal(body, &osrmResp); err != nil {
		return nil, nil, fmt.Errorf("failed to parse OSRM response: %w", err)
	}

	if osrmResp.Code != "Ok" {
		return nil, nil, fmt.Errorf("OSRM error: %s", osrmResp.Code)
	}

	// Convert float64 matrices to int
	n := len(locations)
	distMatrix := make([][]int, n)
	durMatrix := make([][]int, n)
	for i := 0; i < n; i++ {
		distMatrix[i] = make([]int, n)
		durMatrix[i] = make([]int, n)
		for j := 0; j < n; j++ {
			distMatrix[i][j] = int(osrmResp.Distances[i][j])
			durMatrix[i][j] = int(osrmResp.Durations[i][j])
		}
	}

	return distMatrix, durMatrix, nil
}

// buildTasks converts RouteRequest tasks to OR-Tools task format
func (o *ORToolsOptimizer) buildTasks(req *RouteRequest) []ortoolsTask {
	var tasks []ortoolsTask

	// Collections
	for _, c := range req.Collections {
		duration := c.Duration
		if duration == 0 {
			duration = 300 // default 5 min
		}
		tasks = append(tasks, ortoolsTask{
			ID:              fmt.Sprintf("collection-%s", c.ID),
			Type:            "collection",
			LocationID:      c.Location.ID,
			ServiceDuration: duration,
		})
	}

	// Placements
	for _, p := range req.Placements {
		tasks = append(tasks, ortoolsTask{
			ID:              fmt.Sprintf("placement-%s", p.ID),
			Type:            "placement",
			LocationID:      p.PlacementLocation.ID,
			WarehouseID:     p.WarehouseLocation.ID,
			PickupDuration:  p.PickupDuration,
			DropoffDuration: p.DropoffDuration,
		})
	}

	// Move requests
	for _, m := range req.MoveRequests {
		tasks = append(tasks, ortoolsTask{
			ID:                fmt.Sprintf("move-%s", m.ID),
			Type:              "move_request",
			PickupLocationID:  m.PickupLocation.ID,
			DropoffLocationID: m.DropoffLocation.ID,
			PickupDuration:    m.PickupDuration,
			DropoffDuration:   m.DropoffDuration,
		})
	}

	// Service tasks
	for _, st := range req.ServiceTasks {
		duration := st.Duration
		if duration == 0 {
			duration = 300
		}
		task := ortoolsTask{
			ID:              fmt.Sprintf("service-%s", st.ID),
			Type:            "service",
			LocationID:      st.Location.ID,
			ServiceDuration: duration,
		}
		if st.EarliestArrival != nil && req.Vehicles[0].EarliestStart != nil {
			twEarly := int(st.EarliestArrival.Sub(*req.Vehicles[0].EarliestStart).Seconds())
			task.TwEarly = &twEarly
		}
		if st.LatestArrival != nil && req.Vehicles[0].EarliestStart != nil {
			twLate := int(st.LatestArrival.Sub(*req.Vehicles[0].EarliestStart).Seconds())
			task.TwLate = &twLate
		}
		tasks = append(tasks, task)
	}

	return tasks
}

// buildVehicle converts the RouteRequest vehicle to OR-Tools format
func (o *ORToolsOptimizer) buildVehicle(req *RouteRequest, locIDToIdx map[string]int) ortoolsVehicle {
	v := req.Vehicles[0]

	capacity := 4 // default
	if bins, ok := v.Capacities["bins"]; ok {
		capacity = bins
	}

	return ortoolsVehicle{
		ID:              v.ID,
		StartLocationID: v.StartLocation.ID,
		EndLocationID:   v.EndLocation.ID,
		Capacity:        capacity,
		InitialLoad:     v.StartupBins,
	}
}

// callService sends the request to the OR-Tools Python microservice
func (o *ORToolsOptimizer) callService(req ortoolsRequest) (*ortoolsResponse, error) {
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	log.Printf("📤 [OR-Tools] Sending to %s/optimize (%d bytes)...", o.serviceURL, len(reqJSON))

	httpReq, err := http.NewRequest("POST", o.serviceURL+"/optimize", bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("service request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("service returned %d: %s", resp.StatusCode, string(body))
	}

	var ortoolsResp ortoolsResponse
	if err := json.Unmarshal(body, &ortoolsResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	firstRouteStops := 0
	if len(ortoolsResp.Routes) > 0 {
		firstRouteStops = len(ortoolsResp.Routes[0].Stops)
	}
	log.Printf("📥 [OR-Tools] Response: feasible=%v, %d stops, %d dropped, %dms",
		ortoolsResp.Feasible, firstRouteStops, len(ortoolsResp.Dropped), ortoolsResp.SolverRuntimeMs)

	return &ortoolsResp, nil
}

// buildRouteResponse converts OR-Tools response to the standard RouteResponse
func (o *ORToolsOptimizer) buildRouteResponse(ortoolsResp *ortoolsResponse, req *RouteRequest) *RouteResponse {
	response := &RouteResponse{
		Routes:        make([]OptimizedRoute, 0),
		DroppedTasks:  ortoolsResp.Dropped,
		OptimizerUsed: "OR-Tools",
	}

	// Compute a base time for ETAs
	baseTime := time.Now()
	if len(req.Vehicles) > 0 && req.Vehicles[0].EarliestStart != nil {
		baseTime = *req.Vehicles[0].EarliestStart
	}

	for _, route := range ortoolsResp.Routes {
		optimizedRoute := OptimizedRoute{
			VehicleID:     route.VehicleID,
			VehicleName:   route.VehicleID,
			Stops:         make([]OptimizedStop, 0),
			TotalDistance:  float64(route.TotalDistance),
			TotalDuration: route.TotalDuration,
		}

		for _, stop := range route.Stops {
			stopType := o.mapStopType(stop.Type)

			// Map duplicate warehouse location IDs back to the real warehouse
			locationID := stop.LocationID
			if strings.Contains(locationID, "__dup_") {
				// Strip __dup_N suffix to get original warehouse ID
				parts := strings.Split(locationID, "__dup_")
				locationID = parts[0]
			}

			optimizedStop := OptimizedStop{
				Type:         stopType,
				LocationID:   locationID,
				LocationName: locationID,
				Latitude:     stop.Lat,
				Longitude:    stop.Lon,
				ETA:          baseTime.Add(time.Duration(stop.ArrivalTime) * time.Second),
				Odometer:     float64(stop.CumulativeDistance),
				Duration:     stop.ServiceDuration,
			}

			// Look up full location details from request
			if loc := findLocationInRequest(locationID, req); loc != nil {
				optimizedStop.Latitude = loc.Latitude
				optimizedStop.Longitude = loc.Longitude
				optimizedStop.Address = loc.Address
				optimizedStop.LocationName = loc.Name
			}

			// Map task references from task_id
			taskID := stop.TaskID
			if strings.HasPrefix(taskID, "collection-") {
				optimizedStop.CollectionID = taskID
			} else if strings.HasPrefix(taskID, "placement-") {
				// Strip "-warehouse" suffix from warehouse pickup stops
				// OR-Tools returns "placement-XXX-warehouse" for warehouse pickups
				// but shifts.go expects "placement-XXX" to match against req.Placements
				cleanID := strings.TrimSuffix(taskID, "-warehouse")
				optimizedStop.PlacementID = cleanID
			} else if strings.HasPrefix(taskID, "move-") {
				// Strip "-pickup" or "-dropoff" suffix
				// OR-Tools returns "move-XXX-pickup" and "move-XXX-dropoff"
				// but shifts.go expects "move-XXX" to match against req.MoveRequests
				cleanID := strings.TrimSuffix(strings.TrimSuffix(taskID, "-pickup"), "-dropoff")
				optimizedStop.MoveRequestID = cleanID
			} else if strings.HasPrefix(taskID, "service-") {
				optimizedStop.CollectionID = taskID
			}

			// Look up bin details for collections
			for _, c := range req.Collections {
				if taskID == fmt.Sprintf("collection-%s", c.ID) {
					optimizedStop.BinNumber = c.BinNumber
					optimizedStop.FillPercentage = c.FillPercentage
					break
				}
			}

			optimizedRoute.Stops = append(optimizedRoute.Stops, optimizedStop)
		}

		response.Routes = append(response.Routes, optimizedRoute)
		response.TotalDistance += optimizedRoute.TotalDistance
		response.TotalDuration += optimizedRoute.TotalDuration
	}

	return response
}

// mapStopType converts OR-Tools stop type strings to StopType constants
func (o *ORToolsOptimizer) mapStopType(t string) StopType {
	switch t {
	case "start":
		return StopTypeStart
	case "end":
		return StopTypeEnd
	case "collection":
		return StopTypeCollection
	case "placement":
		return StopTypePlacement
	case "pickup":
		return StopTypePickup
	case "dropoff":
		return StopTypeDropoff
	case "warehouse":
		// Map warehouse stops to Pickup type — shifts.go handles warehouse
		// pickups as StopTypePickup with a PlacementID set, not StopTypeWarehouse
		return StopTypePickup
	case "service":
		return StopTypeCollection // service tasks treated as collections
	default:
		return StopType(t)
	}
}
