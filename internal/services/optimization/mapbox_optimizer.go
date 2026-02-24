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

		// Add routing profile (always use traffic-aware routing)
		routingProfile := v.RoutingProfile
		if routingProfile == "" {
			routingProfile = "mapbox/driving-traffic" // Default to traffic-aware
		}
		vehicle["routing_profile"] = routingProfile

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
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("🚀 [MAPBOX] STARTING ROUTE OPTIMIZATION REQUEST")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Validate access token
	if m.accessToken == "" {
		log.Println("❌ [MAPBOX] CRITICAL: Access token is empty!")
		log.Println("   Check MAPBOX_ACCESS_TOKEN environment variable")
		return "", fmt.Errorf("MAPBOX_ACCESS_TOKEN not configured")
	}
	log.Printf("✅ [MAPBOX] Access token present (length: %d)", len(m.accessToken))
	log.Printf("   Token preview: %s...%s", m.accessToken[:8], m.accessToken[len(m.accessToken)-8:])

	url := fmt.Sprintf("https://api.mapbox.com/optimized-trips/v2?access_token=%s", m.accessToken)

	log.Printf("🌐 [MAPBOX] Target URL: https://api.mapbox.com/optimized-trips/v2?access_token=***")

	// Log problem details
	if locations, ok := problem["locations"].([]map[string]interface{}); ok {
		log.Printf("📍 [MAPBOX] Problem contains %d locations", len(locations))
	}
	if vehicles, ok := problem["vehicles"].([]map[string]interface{}); ok {
		log.Printf("🚚 [MAPBOX] Problem contains %d vehicles", len(vehicles))
	}
	if services, ok := problem["services"].([]map[string]interface{}); ok {
		log.Printf("📦 [MAPBOX] Problem contains %d services (collections)", len(services))
	}
	if shipments, ok := problem["shipments"].([]map[string]interface{}); ok {
		log.Printf("📦 [MAPBOX] Problem contains %d shipments (placements/moves)", len(shipments))
	}

	problemJSON, err := json.Marshal(problem)
	if err != nil {
		log.Printf("❌ [MAPBOX] Failed to marshal problem JSON: %v", err)
		return "", fmt.Errorf("failed to marshal problem: %w", err)
	}

	log.Printf("📏 [MAPBOX] Request size: %d bytes (%.2f KB)", len(problemJSON), float64(len(problemJSON))/1024.0)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(problemJSON))
	if err != nil {
		log.Printf("❌ [MAPBOX] Failed to create HTTP request: %v", err)
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	log.Printf("📤 [MAPBOX] Sending POST request to Mapbox Optimization API...")
	requestStart := time.Now()
	resp, err := m.client.Do(req)
	requestDuration := time.Since(requestStart)

	if err != nil {
		log.Printf("❌ [MAPBOX] HTTP request FAILED after %v", requestDuration)
		log.Printf("   Error type: %T", err)
		log.Printf("   Error details: %v", err)

		// Categorize error
		if os.IsTimeout(err) {
			log.Println("   ⏱️  Error category: TIMEOUT")
			log.Println("   Possible causes:")
			log.Println("      - Mapbox API is slow/overloaded")
			log.Println("      - Network connectivity issues")
			log.Println("      - Request too large/complex")
		} else {
			log.Println("   🌐 Error category: NETWORK/CONNECTION")
			log.Println("   Possible causes:")
			log.Println("      - DNS resolution failed")
			log.Println("      - Cannot reach Mapbox servers")
			log.Println("      - Firewall blocking outbound HTTPS")
		}

		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("📡 [MAPBOX] Received response in %v", requestDuration)
	log.Printf("   Status code: %d", resp.StatusCode)
	log.Printf("   Status text: %s", resp.Status)
	log.Printf("   Response headers: %v", resp.Header)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("❌ [MAPBOX] Failed to read response body: %v", err)
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	log.Printf("📥 [MAPBOX] Response body size: %d bytes", len(body))
	log.Printf("📥 [MAPBOX] Response body: %s", string(body))

	if resp.StatusCode != 202 {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("❌ [MAPBOX] REQUEST REJECTED - Status %d (expected 202 Accepted)", resp.StatusCode)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Categorize error by status code
		switch resp.StatusCode {
		case 400:
			log.Println("   ⚠️  Error: BAD REQUEST (400)")
			log.Println("   This means the problem JSON is malformed or invalid")
			log.Println("   Common causes:")
			log.Println("      - Missing required fields (vehicles, locations)")
			log.Println("      - Invalid coordinate format")
			log.Println("      - Invalid time windows")
			log.Println("      - Too many locations (>1000)")
		case 401:
			log.Println("   🔒 Error: UNAUTHORIZED (401)")
			log.Println("   This means the Mapbox access token is invalid or expired")
			log.Println("   Check:")
			log.Printf("      - Token: %s...%s", m.accessToken[:8], m.accessToken[len(m.accessToken)-8:])
			log.Println("      - MAPBOX_ACCESS_TOKEN environment variable")
			log.Println("      - Token hasn't been revoked in Mapbox dashboard")
		case 403:
			log.Println("   🚫 Error: FORBIDDEN (403)")
			log.Println("   This means the account doesn't have access to Optimization API")
			log.Println("   Check:")
			log.Println("      - Mapbox subscription includes Optimization API")
			log.Println("      - Account hasn't exceeded usage limits")
		case 422:
			log.Println("   ⚠️  Error: UNPROCESSABLE ENTITY (422)")
			log.Println("   This means Mapbox can't solve the problem")
			log.Println("   Common causes:")
			log.Println("      - Impossible constraints (time windows, capacities)")
			log.Println("      - Unreachable locations")
			log.Println("      - Problem too complex to solve")
		case 429:
			log.Println("   ⏱️  Error: RATE LIMIT EXCEEDED (429)")
			log.Println("   Too many requests to Mapbox API")
			log.Println("   Wait and retry with backoff")
		case 500, 502, 503, 504:
			log.Printf("   🔥 Error: SERVER ERROR (%d)", resp.StatusCode)
			log.Println("   Mapbox API is experiencing issues")
			log.Println("   Retry later")
		default:
			log.Printf("   ❓ Error: UNKNOWN (%d)", resp.StatusCode)
		}

		log.Printf("   Error response: %s", string(body))
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		log.Printf("❌ [MAPBOX] Failed to parse response JSON: %v", err)
		log.Printf("❌ [MAPBOX] Raw response was: %s", string(body))
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("✅ [MAPBOX] REQUEST ACCEPTED - Job submitted successfully")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("   Job ID: %s", response.ID)
	log.Printf("   Status: %s", response.Status)
	log.Printf("   Request duration: %v", requestDuration)
	log.Println("   Next step: Polling for solution...")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	return response.ID, nil
}

// pollForSolution polls for the solution until it's ready or timeout
func (m *MapboxOptimizer) pollForSolution(jobID string, timeout time.Duration) (map[string]interface{}, error) {
	url := fmt.Sprintf("https://api.mapbox.com/optimized-trips/v2/%s?access_token=%s", jobID, m.accessToken)
	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second
	pollCount := 0

	log.Printf("⏳ [MAPBOX] Starting to poll for job %s (timeout: %v)", jobID, timeout)

	for time.Now().Before(deadline) {
		pollCount++
		log.Printf("🔄 [MAPBOX] Poll attempt #%d for job %s", pollCount, jobID)

		resp, err := m.client.Get(url)
		if err != nil {
			log.Printf("❌ [MAPBOX] Poll request failed: %v", err)
			return nil, fmt.Errorf("failed to check status: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("❌ [MAPBOX] Failed to read poll response body: %v", err)
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		log.Printf("📡 [MAPBOX] Poll response status: %d", resp.StatusCode)

		// 200 OK = solution ready
		if resp.StatusCode == 200 {
			log.Printf("✅ [MAPBOX] Solution ready! (after %d polls)", pollCount)
			log.Printf("📥 [MAPBOX] Solution body size: %d bytes", len(body))

			var solution map[string]interface{}
			if err := json.Unmarshal(body, &solution); err != nil {
				log.Printf("❌ [MAPBOX] Failed to parse solution JSON: %v", err)
				log.Printf("❌ [MAPBOX] Raw solution: %s", string(body))
				return nil, fmt.Errorf("failed to parse solution: %w", err)
			}

			log.Printf("✅ [MAPBOX] Solution parsed successfully")
			return solution, nil
		}

		// 202 Accepted = still processing
		if resp.StatusCode == 202 {
			log.Printf("⏳ [MAPBOX] Solution not ready yet (status: 202 Accepted), waiting %v...", pollInterval)
			log.Printf("📄 [MAPBOX] Status response: %s", string(body))
			time.Sleep(pollInterval)
			continue
		}

		// Other status = error
		log.Printf("❌ [MAPBOX] Unexpected status %d", resp.StatusCode)
		log.Printf("❌ [MAPBOX] Error response: %s", string(body))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	log.Printf("❌ [MAPBOX] Timeout! Waited %v (%d polls total)", timeout, pollCount)
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
	// Check start location
	if locationID == req.Vehicles[0].StartLocation.ID {
		return &req.Vehicles[0].StartLocation
	}

	// Check end location (warehouse)
	if locationID == req.Vehicles[0].EndLocation.ID {
		return &req.Vehicles[0].EndLocation
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
