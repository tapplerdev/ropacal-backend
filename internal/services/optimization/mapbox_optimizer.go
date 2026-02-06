package optimization

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// MapboxOptimizer implements the Optimizer interface using Mapbox Optimization v2 API
type MapboxOptimizer struct {
	accessToken string
	client      *http.Client
}

// NewMapboxOptimizer creates a new Mapbox optimizer
func NewMapboxOptimizer() *MapboxOptimizer {
	return &MapboxOptimizer{
		accessToken: os.Getenv("MAPBOX_ACCESS_TOKEN"),
		client:      &http.Client{Timeout: 30 * time.Second},
	}
}

func (m *MapboxOptimizer) Name() string {
	return "Mapbox Optimization v2"
}

// OptimizeRoute optimizes the route using Mapbox Optimization v2 API
func (m *MapboxOptimizer) OptimizeRoute(req *RouteRequest) (*RouteResponse, error) {
	if m.accessToken == "" {
		return nil, fmt.Errorf("MAPBOX_ACCESS_TOKEN not configured")
	}

	log.Printf("📦 Converting request to Mapbox format...")
	problem := m.buildMapboxProblem(req)

	// Debug: Log the full problem being sent to Mapbox
	problemJSON, _ := json.MarshalIndent(problem, "", "  ")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📤 MAPBOX REQUEST (Problem JSON):")
	log.Printf("%s", string(problemJSON))
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	log.Printf("📤 Submitting routing problem to Mapbox...")
	jobID, err := m.submitProblem(problem)
	if err != nil {
		return nil, fmt.Errorf("failed to submit problem: %w", err)
	}

	log.Printf("✅ Problem submitted, job ID: %s", jobID)
	log.Printf("⏳ Polling for solution (max 60 seconds)...")

	solution, err := m.pollForSolution(jobID, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to get solution: %w", err)
	}

	log.Printf("✅ Solution received, parsing...")

	// Debug: Log the full solution from Mapbox
	solutionJSON, _ := json.MarshalIndent(solution, "", "  ")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📥 MAPBOX RESPONSE (Solution JSON):")
	log.Printf("%s", string(solutionJSON))
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	response := m.parseMapboxSolution(solution, req)
	response.OptimizerUsed = "mapbox"

	log.Printf("✅ Optimization complete: %d routes, %.0fm total", len(response.Routes), response.TotalDistance)

	return response, nil
}

// buildMapboxProblem converts our internal types to Mapbox API format
func (m *MapboxOptimizer) buildMapboxProblem(req *RouteRequest) map[string]interface{} {
	// Build locations map
	locations := make([]map[string]interface{}, 0)
	locationSet := make(map[string]bool)

	// Helper to add unique location
	addLocation := func(loc Location) {
		if locationSet[loc.ID] {
			return
		}
		locationSet[loc.ID] = true
		locations = append(locations, map[string]interface{}{
			"name":        loc.ID,
			"coordinates": []float64{loc.Longitude, loc.Latitude},
		})
	}

	// Add warehouse locations
	for _, vehicle := range req.Vehicles {
		addLocation(vehicle.StartLocation)
		addLocation(vehicle.EndLocation)
	}
	for _, placement := range req.Placements {
		addLocation(placement.WarehouseLocation)
		addLocation(placement.PlacementLocation)
	}
	for _, moveReq := range req.MoveRequests {
		addLocation(moveReq.PickupLocation)
		addLocation(moveReq.DropoffLocation)
	}
	for _, collection := range req.Collections {
		addLocation(collection.Location)
	}
	for _, warehouse := range req.WarehouseStops {
		addLocation(warehouse.Location)
	}

	// Build vehicles
	vehicles := make([]map[string]interface{}, len(req.Vehicles))
	for i, v := range req.Vehicles {
		vehicle := map[string]interface{}{
			"name":           v.ID,
			"start_location": v.StartLocation.ID,
			"end_location":   v.EndLocation.ID,
		}

		// Add capacities if defined
		if len(v.Capacities) > 0 {
			vehicle["capacities"] = v.Capacities
		}

		// Add capabilities if defined
		if len(v.Capabilities) > 0 {
			vehicle["capabilities"] = v.Capabilities
		}

		// Add time windows if defined
		if v.EarliestStart != nil {
			vehicle["earliest_start"] = v.EarliestStart.Format(time.RFC3339)
		}
		if v.LatestEnd != nil {
			vehicle["latest_end"] = v.LatestEnd.Format(time.RFC3339)
		}

		vehicles[i] = vehicle
	}

	// Build services (collections + warehouse stops)
	services := make([]map[string]interface{}, 0)

	// Add collections as services (no capacity impact)
	for _, c := range req.Collections {
		service := map[string]interface{}{
			"name":     fmt.Sprintf("collection-%s", c.ID),
			"location": c.Location.ID,
		}
		if c.Duration > 0 {
			service["duration"] = c.Duration
		}
		services = append(services, service)
	}

	// Add warehouse stops as services
	for _, w := range req.WarehouseStops {
		service := map[string]interface{}{
			"name":     fmt.Sprintf("warehouse-%s", w.ID),
			"location": w.Location.ID,
		}
		if w.Duration > 0 {
			service["duration"] = w.Duration
		}
		services = append(services, service)
	}

	// Build shipments (placements + move requests)
	shipments := make([]map[string]interface{}, 0)

	// Add placements as shipments
	for _, p := range req.Placements {
		shipment := map[string]interface{}{
			"name": fmt.Sprintf("placement-%s", p.ID),
			"from": p.WarehouseLocation.ID,
			"to":   p.PlacementLocation.ID,
			"size": map[string]int{"bins": 1}, // Placements consume 1 bin capacity
		}
		if p.PickupDuration > 0 {
			shipment["pickup_duration"] = p.PickupDuration
		}
		if p.DropoffDuration > 0 {
			shipment["dropoff_duration"] = p.DropoffDuration
		}
		shipments = append(shipments, shipment)
	}

	// Add move requests as shipments
	for _, mr := range req.MoveRequests {
		shipment := map[string]interface{}{
			"name": fmt.Sprintf("move-%s", mr.ID),
			"from": mr.PickupLocation.ID,
			"to":   mr.DropoffLocation.ID,
			"size": map[string]int{"bins": 1}, // Move requests consume 1 bin capacity
		}
		if mr.PickupDuration > 0 {
			shipment["pickup_duration"] = mr.PickupDuration
		}
		if mr.DropoffDuration > 0 {
			shipment["dropoff_duration"] = mr.DropoffDuration
		}
		shipments = append(shipments, shipment)
	}

	// Build the problem document
	problem := map[string]interface{}{
		"version":   1,
		"locations": locations,
		"vehicles":  vehicles,
	}

	// Only include services if we have any
	if len(services) > 0 {
		problem["services"] = services
	}

	// Only include shipments if we have any
	if len(shipments) > 0 {
		problem["shipments"] = shipments
	}

	// Add optimization objective: minimize total travel duration for the fleet
	// For single-vehicle routes, this minimizes the total route time
	problem["options"] = map[string]interface{}{
		"objectives": []string{"min-total-travel-duration"},
	}

	return problem
}

// submitProblem submits the routing problem to Mapbox
func (m *MapboxOptimizer) submitProblem(problem map[string]interface{}) (string, error) {
	url := fmt.Sprintf("https://api.mapbox.com/optimized-trips/v2?access_token=%s", m.accessToken)

	problemJSON, err := json.Marshal(problem)
	if err != nil {
		return "", fmt.Errorf("failed to marshal problem: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(problemJSON))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != 202 {
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	return response.ID, nil
}

// pollForSolution polls for the solution until it's ready or timeout
func (m *MapboxOptimizer) pollForSolution(jobID string, timeout time.Duration) (map[string]interface{}, error) {
	url := fmt.Sprintf("https://api.mapbox.com/optimized-trips/v2/%s?access_token=%s", jobID, m.accessToken)
	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second

	for time.Now().Before(deadline) {
		resp, err := m.client.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to check status: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		// 200 OK = solution ready
		if resp.StatusCode == 200 {
			var solution map[string]interface{}
			if err := json.Unmarshal(body, &solution); err != nil {
				return nil, fmt.Errorf("failed to parse solution: %w", err)
			}
			return solution, nil
		}

		// 202 Accepted = still processing
		if resp.StatusCode == 202 {
			log.Printf("⏳ Solution not ready yet, waiting %v...", pollInterval)
			time.Sleep(pollInterval)
			continue
		}

		// Other status = error
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil, fmt.Errorf("timeout waiting for solution (waited %v)", timeout)
}

// parseMapboxSolution converts Mapbox solution back to our internal format
func (m *MapboxOptimizer) parseMapboxSolution(solution map[string]interface{}, originalReq *RouteRequest) *RouteResponse {
	response := &RouteResponse{
		Routes:        make([]OptimizedRoute, 0),
		DroppedTasks:  make([]string, 0),
		TotalDistance: 0,
		TotalDuration: 0,
	}

	// Parse dropped tasks
	if dropped, ok := solution["dropped"].(map[string]interface{}); ok {
		if services, ok := dropped["services"].([]interface{}); ok {
			for _, s := range services {
				if svc, ok := s.(string); ok {
					response.DroppedTasks = append(response.DroppedTasks, svc)
				}
			}
		}
		if shipments, ok := dropped["shipments"].([]interface{}); ok {
			for _, s := range shipments {
				if shp, ok := s.(string); ok {
					response.DroppedTasks = append(response.DroppedTasks, shp)
				}
			}
		}
	}

	// Parse routes
	if routes, ok := solution["routes"].([]interface{}); ok {
		for _, r := range routes {
			route, ok := r.(map[string]interface{})
			if !ok {
				continue
			}

			vehicleID, _ := route["vehicle"].(string)
			stops, _ := route["stops"].([]interface{})

			optimizedRoute := OptimizedRoute{
				VehicleID:   vehicleID,
				VehicleName: vehicleID,
				Stops:       make([]OptimizedStop, 0),
			}

			for _, s := range stops {
				stop, ok := s.(map[string]interface{})
				if !ok {
					continue
				}

				stopType, _ := stop["type"].(string)
				location, _ := stop["location"].(string)
				etaStr, _ := stop["eta"].(string)
				odometer, _ := stop["odometer"].(float64)
				duration, _ := stop["duration"].(float64)
				wait, _ := stop["wait"].(float64)

				eta, _ := time.Parse(time.RFC3339, etaStr)

				optimizedStop := OptimizedStop{
					Type:         StopType(stopType),
					LocationID:   location,
					LocationName: location,
					ETA:          eta,
					Odometer:     odometer,
					Duration:     int(duration),
					Wait:         int(wait),
				}

				// Extract coordinates from the original request
				if originalLoc := findLocationInRequest(location, originalReq); originalLoc != nil {
					optimizedStop.Latitude = originalLoc.Latitude
					optimizedStop.Longitude = originalLoc.Longitude
					optimizedStop.Address = originalLoc.Address
					optimizedStop.LocationName = originalLoc.Name
				}

				// Extract task references from services/pickups/dropoffs
				if services, ok := stop["services"].([]interface{}); ok && len(services) > 0 {
					if svc, ok := services[0].(string); ok {
						optimizedStop.CollectionID = svc
					}
				}

				// Extract pickup references (for shipments)
				if pickups, ok := stop["pickups"].([]interface{}); ok && len(pickups) > 0 {
					if pickup, ok := pickups[0].(string); ok {
						// Pickup ID could be "placement-XXX" or "move-XXX"
						if len(pickup) > 10 && pickup[:10] == "placement-" {
							optimizedStop.PlacementID = pickup
						} else if len(pickup) > 5 && pickup[:5] == "move-" {
							optimizedStop.MoveRequestID = pickup
						}
					}
				}

				// Extract dropoff references (for shipments)
				if dropoffs, ok := stop["dropoffs"].([]interface{}); ok && len(dropoffs) > 0 {
					if dropoff, ok := dropoffs[0].(string); ok {
						// Dropoff ID could be "placement-XXX" or "move-XXX"
						if len(dropoff) > 10 && dropoff[:10] == "placement-" {
							optimizedStop.PlacementID = dropoff
						} else if len(dropoff) > 5 && dropoff[:5] == "move-" {
							optimizedStop.MoveRequestID = dropoff
						}
					}
				}

				optimizedRoute.Stops = append(optimizedRoute.Stops, optimizedStop)
			}

			// Calculate route totals
			if len(optimizedRoute.Stops) > 0 {
				lastStop := optimizedRoute.Stops[len(optimizedRoute.Stops)-1]
				optimizedRoute.TotalDistance = lastStop.Odometer
				optimizedRoute.TotalDuration = int(lastStop.ETA.Sub(optimizedRoute.Stops[0].ETA).Seconds())
			}

			response.Routes = append(response.Routes, optimizedRoute)
			response.TotalDistance += optimizedRoute.TotalDistance
			response.TotalDuration += optimizedRoute.TotalDuration
		}
	}

	return response
}

// findLocationInRequest finds location details from the original request
func findLocationInRequest(locationID string, req *RouteRequest) *Location {
	// Check warehouse
	if locationID == req.Vehicles[0].StartLocation.ID {
		return &req.Vehicles[0].StartLocation
	}

	// Check collections
	for _, c := range req.Collections {
		if c.Location.ID == locationID {
			return &c.Location
		}
	}

	// Check placements
	for _, p := range req.Placements {
		if p.WarehouseLocation.ID == locationID {
			return &p.WarehouseLocation
		}
		if p.PlacementLocation.ID == locationID {
			return &p.PlacementLocation
		}
	}

	// Check move requests
	for _, m := range req.MoveRequests {
		if m.PickupLocation.ID == locationID {
			return &m.PickupLocation
		}
		if m.DropoffLocation.ID == locationID {
			return &m.DropoffLocation
		}
	}

	// Check warehouse stops
	for _, w := range req.WarehouseStops {
		if w.Location.ID == locationID {
			return &w.Location
		}
	}

	return nil
}
