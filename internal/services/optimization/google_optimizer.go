package optimization

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// GoogleOptimizer implements the Optimizer interface using Google Route Optimization API (Optimize Tours)
type GoogleOptimizer struct {
	client *http.Client
}

// NewGoogleOptimizer creates a new Google optimizer with OAuth2 authentication
func NewGoogleOptimizer() *GoogleOptimizer {
	// Load service account JSON file path from environment
	credPath := os.Getenv("GOOGLE_SERVICE_ACCOUNT_JSON")
	if credPath == "" {
		credPath = "service-account-key.json" // Default path in backend directory
	}

	log.Printf("🔐 [GOOGLE] Loading service account from: %s", credPath)

	// Read the service account JSON file
	data, err := os.ReadFile(credPath)
	if err != nil {
		log.Printf("❌ [GOOGLE] Failed to load service account key: %v", err)
		log.Printf("   Make sure the file exists at: %s", credPath)
		return nil
	}

	// Create JWT config with Route Optimization API scope
	conf, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		log.Printf("❌ [GOOGLE] Failed to create JWT config: %v", err)
		return nil
	}

	log.Printf("✅ [GOOGLE] Service account loaded successfully")
	log.Printf("   Email: %s", conf.Email)

	// Create authenticated HTTP client
	ctx := context.Background()
	client := conf.Client(ctx)
	client.Timeout = 60 * time.Second

	return &GoogleOptimizer{
		client: client,
	}
}

func (g *GoogleOptimizer) Name() string {
	return "Google Route Optimization (Optimize Tours)"
}

// OptimizeRoute optimizes the route using Google Route Optimization API
func (g *GoogleOptimizer) OptimizeRoute(req *RouteRequest) (*RouteResponse, error) {
	if g.client == nil {
		return nil, fmt.Errorf("Google optimizer not initialized - check service account configuration")
	}

	log.Printf("📦 Converting request to Google Optimize Tours format...")
	problem := g.buildGoogleRequest(req)

	// Debug: Log the full problem being sent to Google
	problemJSON, _ := json.MarshalIndent(problem, "", "  ")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📤 GOOGLE REQUEST (Optimize Tours JSON):")
	log.Printf("%s", string(problemJSON))
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	log.Printf("📤 Submitting routing problem to Google...")
	solution, err := g.submitOptimizationRequest(problem)
	if err != nil {
		return nil, fmt.Errorf("failed to submit problem: %w", err)
	}

	log.Printf("✅ Solution received, parsing...")

	// Debug: Log the full solution from Google
	solutionJSON, marshalErr := json.MarshalIndent(solution, "", "  ")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📥 GOOGLE RESPONSE (Solution JSON):")
	if marshalErr != nil {
		log.Printf("❌ Failed to marshal solution: %v", marshalErr)
		log.Printf("Raw solution map: %+v", solution)
	} else {
		log.Printf("%s", string(solutionJSON))
	}
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	response := g.parseGoogleResponse(solution, req)
	response.OptimizerUsed = "google"

	log.Printf("✅ [GOOGLE OPTIMIZER] Received response from Google")
	log.Printf("📊 [GOOGLE OPTIMIZER] Response contains %d routes", len(response.Routes))

	if len(response.Routes) == 0 {
		log.Printf("❌ [GOOGLE OPTIMIZER] No routes in response! Dropped tasks: %v", response.DroppedTasks)
		return nil, fmt.Errorf("Google returned no routes")
	}

	log.Printf("✅ Optimization complete: %d routes, %.0fm total", len(response.Routes), response.TotalDistance)

	return response, nil
}

// buildGoogleRequest converts our internal types to Google Optimize Tours API format
func (g *GoogleOptimizer) buildGoogleRequest(req *RouteRequest) map[string]interface{} {
	// Build model (the optimization problem)
	model := make(map[string]interface{})

	// Build shipments (placements + move requests)
	shipments := make([]map[string]interface{}, 0)

	// Add placements as shipments
	for _, p := range req.Placements {
		shipment := map[string]interface{}{
			"label": fmt.Sprintf("placement-%s", p.ID),
			"pickups": []map[string]interface{}{
				{
					"arrivalWaypoint": map[string]interface{}{
						"location": map[string]interface{}{
							"latLng": map[string]interface{}{
								"latitude":  p.WarehouseLocation.Latitude,
								"longitude": p.WarehouseLocation.Longitude,
							},
						},
					},
					"duration": fmt.Sprintf("%ds", p.PickupDuration),
					"tags":     []string{"warehouse-pickup"},
				},
			},
			"deliveries": []map[string]interface{}{
				{
					"arrivalWaypoint": map[string]interface{}{
						"location": map[string]interface{}{
							"latLng": map[string]interface{}{
								"latitude":  p.PlacementLocation.Latitude,
								"longitude": p.PlacementLocation.Longitude,
							},
						},
					},
					"duration": fmt.Sprintf("%ds", p.DropoffDuration),
					"tags":     []string{"placement-delivery"},
				},
			},
			"loadDemands": map[string]interface{}{
				"bins": map[string]interface{}{
					"amount": "1",
				},
			},
		}
		shipments = append(shipments, shipment)
	}

	// Add move requests as shipments
	for _, mr := range req.MoveRequests {
		shipment := map[string]interface{}{
			"label": fmt.Sprintf("move-%s", mr.ID),
			"pickups": []map[string]interface{}{
				{
					"arrivalWaypoint": map[string]interface{}{
						"location": map[string]interface{}{
							"latLng": map[string]interface{}{
								"latitude":  mr.PickupLocation.Latitude,
								"longitude": mr.PickupLocation.Longitude,
							},
						},
					},
					"duration": fmt.Sprintf("%ds", mr.PickupDuration),
					"tags":     []string{"bin-pickup"},
				},
			},
			"deliveries": []map[string]interface{}{
				{
					"arrivalWaypoint": map[string]interface{}{
						"location": map[string]interface{}{
							"latLng": map[string]interface{}{
								"latitude":  mr.DropoffLocation.Latitude,
								"longitude": mr.DropoffLocation.Longitude,
							},
						},
					},
					"duration": fmt.Sprintf("%ds", mr.DropoffDuration),
					"tags":     []string{"bin-delivery"},
				},
			},
			"loadDemands": map[string]interface{}{
				"bins": map[string]interface{}{
					"amount": "1",
				},
			},
		}
		shipments = append(shipments, shipment)
	}

	model["shipments"] = shipments

	// Build vehicles
	vehicles := make([]map[string]interface{}, len(req.Vehicles))
	for i, v := range req.Vehicles {
		vehicle := map[string]interface{}{
			"label": v.ID,
			"startWaypoint": map[string]interface{}{
				"location": map[string]interface{}{
					"latLng": map[string]interface{}{
						"latitude":  v.StartLocation.Latitude,
						"longitude": v.StartLocation.Longitude,
					},
				},
			},
			"endWaypoint": map[string]interface{}{
				"location": map[string]interface{}{
					"latLng": map[string]interface{}{
						"latitude":  v.EndLocation.Latitude,
						"longitude": v.EndLocation.Longitude,
					},
				},
			},
			"travelMode": "DRIVING", // Google's equivalent to Mapbox driving profile
		}

		// Add load limits (capacity)
		if len(v.Capacities) > 0 {
			loadLimits := make(map[string]interface{})
			for key, value := range v.Capacities {
				// Build load limit for this capacity type
				loadLimit := map[string]interface{}{
					"maxLoad": fmt.Sprintf("%d", value),
				}

				// Add startLoadInterval if truck has bins already loaded (startup load)
				// NOTE: startLoadInterval goes INSIDE the LoadLimit object, not top-level!
				if key == "bins" && v.StartupBins > 0 {
					loadLimit["startLoadInterval"] = map[string]interface{}{
						"min": fmt.Sprintf("%d", v.StartupBins),
						"max": fmt.Sprintf("%d", v.StartupBins),
					}
					log.Printf("✨ [GOOGLE] Startup load: truck starts with %d/%d bins", v.StartupBins, value)
				} else if key == "bins" {
					log.Printf("📦 [GOOGLE] No startup load: truck starts empty (0 bins)")
				}

				loadLimits[key] = loadLimit
			}
			vehicle["loadLimits"] = loadLimits
		}

		// Add time windows if defined
		if v.EarliestStart != nil || v.LatestEnd != nil {
			timeWindow := make(map[string]interface{})
			if v.EarliestStart != nil {
				timeWindow["startTime"] = v.EarliestStart.Format(time.RFC3339)
			}
			if v.LatestEnd != nil {
				timeWindow["endTime"] = v.LatestEnd.Format(time.RFC3339)
			}
			vehicle["startTimeWindows"] = []map[string]interface{}{timeWindow}
		}

		vehicles[i] = vehicle
	}

	model["vehicles"] = vehicles

	// Add global settings
	model["globalStartTime"] = time.Now().Format(time.RFC3339)
	model["globalEndTime"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)

	// Build the request body
	request := map[string]interface{}{
		"model": model,
		"searchMode": "RETURN_FAST", // Fast mode for production
		"solvingMode": "DEFAULT_SOLVE",
	}

	return request
}

// submitOptimizationRequest submits the request to Google (synchronous - no polling!)
func (g *GoogleOptimizer) submitOptimizationRequest(problem map[string]interface{}) (map[string]interface{}, error) {
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Println("🚀 [GOOGLE] STARTING ROUTE OPTIMIZATION REQUEST")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// OAuth2 authentication is handled automatically by the HTTP client
	log.Println("✅ [GOOGLE] Using OAuth2 service account authentication")

	// Use wildcard project (works with service account)
	url := "https://routeoptimization.googleapis.com/v1/projects/-:optimizeTours"

	log.Printf("🌐 [GOOGLE] Target URL: %s", url)

	// Log problem details
	if model, ok := problem["model"].(map[string]interface{}); ok {
		if shipments, ok := model["shipments"].([]map[string]interface{}); ok {
			log.Printf("📦 [GOOGLE] Problem contains %d shipments", len(shipments))
		}
		if vehicles, ok := model["vehicles"].([]map[string]interface{}); ok {
			log.Printf("🚚 [GOOGLE] Problem contains %d vehicles", len(vehicles))
		}
	}

	problemJSON, err := json.Marshal(problem)
	if err != nil {
		log.Printf("❌ [GOOGLE] Failed to marshal problem JSON: %v", err)
		return nil, fmt.Errorf("failed to marshal problem: %w", err)
	}

	log.Printf("📏 [GOOGLE] Request size: %d bytes (%.2f KB)", len(problemJSON), float64(len(problemJSON))/1024.0)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(problemJSON))
	if err != nil {
		log.Printf("❌ [GOOGLE] Failed to create HTTP request: %v", err)
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	// OAuth2 client will automatically add Authorization: Bearer <token> header

	log.Printf("📤 [GOOGLE] Sending POST request to Google Route Optimization API...")
	requestStart := time.Now()
	resp, err := g.client.Do(req)
	requestDuration := time.Since(requestStart)

	if err != nil {
		log.Printf("❌ [GOOGLE] HTTP request FAILED after %v", requestDuration)
		log.Printf("   Error type: %T", err)
		log.Printf("   Error details: %v", err)
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("📡 [GOOGLE] Received response in %v", requestDuration)
	log.Printf("   Status code: %d", resp.StatusCode)
	log.Printf("   Status text: %s", resp.Status)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("❌ [GOOGLE] Failed to read response body: %v", err)
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	log.Printf("📥 [GOOGLE] Response body size: %d bytes", len(body))

	if resp.StatusCode != 200 {
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("❌ [GOOGLE] REQUEST FAILED - Status %d (expected 200 OK)", resp.StatusCode)
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Log error response
		log.Printf("   Error response: %s", string(body))

		// Categorize error by status code
		switch resp.StatusCode {
		case 400:
			log.Println("   ⚠️  Error: BAD REQUEST (400)")
			log.Println("   Request is malformed or contains invalid data")
		case 401:
			log.Println("   🔒 Error: UNAUTHORIZED (401)")
			log.Println("   Service account authentication failed - check credentials")
		case 403:
			log.Println("   🚫 Error: FORBIDDEN (403)")
			log.Println("   Service account doesn't have access to Route Optimization API")
			log.Println("   Make sure the API is enabled in Google Cloud Console")
		case 429:
			log.Println("   ⏱️  Error: RATE LIMIT EXCEEDED (429)")
			log.Println("   Too many requests")
		case 500, 502, 503, 504:
			log.Printf("   🔥 Error: SERVER ERROR (%d)", resp.StatusCode)
			log.Println("   Google API is experiencing issues")
		}

		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var solution map[string]interface{}
	if err := json.Unmarshal(body, &solution); err != nil {
		log.Printf("❌ [GOOGLE] Failed to parse response JSON: %v", err)
		log.Printf("❌ [GOOGLE] Raw response was: %s", string(body))
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("✅ [GOOGLE] REQUEST SUCCESSFUL")
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("   Request duration: %v", requestDuration)
	log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	return solution, nil
}

// parseGoogleResponse converts Google solution back to our internal format
func (g *GoogleOptimizer) parseGoogleResponse(solution map[string]interface{}, originalReq *RouteRequest) *RouteResponse {
	response := &RouteResponse{
		Routes:        make([]OptimizedRoute, 0),
		DroppedTasks:  make([]string, 0),
		TotalDistance: 0,
		TotalDuration: 0,
	}

	// Parse skipped shipments (dropped tasks)
	if skipped, ok := solution["skippedShipments"].([]interface{}); ok {
		for _, s := range skipped {
			if shipment, ok := s.(map[string]interface{}); ok {
				if label, ok := shipment["label"].(string); ok {
					response.DroppedTasks = append(response.DroppedTasks, label)
				}
			}
		}
	}

	// Parse routes
	if routes, ok := solution["routes"].([]interface{}); ok {
		for routeIdx, r := range routes {
			route, ok := r.(map[string]interface{})
			if !ok {
				continue
			}

			vehicleLabel := fmt.Sprintf("vehicle-%d", routeIdx)
			if label, ok := route["vehicleLabel"].(string); ok {
				vehicleLabel = label
			}

			visits, _ := route["visits"].([]interface{})
			transitions, _ := route["transitions"].([]interface{})

			optimizedRoute := OptimizedRoute{
				VehicleID:   vehicleLabel,
				VehicleName: vehicleLabel,
				Stops:       make([]OptimizedStop, 0),
			}

			// Process visits (each visit is a stop)
			currentTime := time.Now()
			currentOdometer := 0.0

			for visitIdx, v := range visits {
				visit, ok := v.(map[string]interface{})
				if !ok {
					continue
				}

				// Get shipment index to identify the task
				shipmentIndex := -1
				if idx, ok := visit["shipmentIndex"].(float64); ok {
					shipmentIndex = int(idx)
				}

				isPickup := visit["isPickup"].(bool)

				// Find the corresponding shipment
				var shipmentLabel string
				var waypoint map[string]interface{}
				var duration string
				var tags []string

				if shipmentIndex >= 0 && shipmentIndex < len(originalReq.Placements)+len(originalReq.MoveRequests) {
					// Get shipment from original request
					if shipmentIndex < len(originalReq.Placements) {
						placement := originalReq.Placements[shipmentIndex]
						shipmentLabel = fmt.Sprintf("placement-%s", placement.ID)
						if isPickup {
							waypoint = map[string]interface{}{
								"location": map[string]interface{}{
									"latLng": map[string]interface{}{
										"latitude":  placement.WarehouseLocation.Latitude,
										"longitude": placement.WarehouseLocation.Longitude,
									},
								},
							}
							duration = fmt.Sprintf("%ds", placement.PickupDuration)
							tags = []string{"warehouse-pickup"}
						} else {
							waypoint = map[string]interface{}{
								"location": map[string]interface{}{
									"latLng": map[string]interface{}{
										"latitude":  placement.PlacementLocation.Latitude,
										"longitude": placement.PlacementLocation.Longitude,
									},
								},
							}
							duration = fmt.Sprintf("%ds", placement.DropoffDuration)
							tags = []string{"placement-delivery"}
						}
					} else {
						// Move request
						mrIdx := shipmentIndex - len(originalReq.Placements)
						moveReq := originalReq.MoveRequests[mrIdx]
						shipmentLabel = fmt.Sprintf("move-%s", moveReq.ID)
						if isPickup {
							waypoint = map[string]interface{}{
								"location": map[string]interface{}{
									"latLng": map[string]interface{}{
										"latitude":  moveReq.PickupLocation.Latitude,
										"longitude": moveReq.PickupLocation.Longitude,
									},
								},
							}
							duration = fmt.Sprintf("%ds", moveReq.PickupDuration)
						} else {
							waypoint = map[string]interface{}{
								"location": map[string]interface{}{
									"latLng": map[string]interface{}{
										"latitude":  moveReq.DropoffLocation.Latitude,
										"longitude": moveReq.DropoffLocation.Longitude,
									},
								},
							}
							duration = fmt.Sprintf("%ds", moveReq.DropoffDuration)
						}
					}
				}

				// Parse duration
				durationSeconds := 0
				if duration != "" {
					fmt.Sscanf(duration, "%ds", &durationSeconds)
				}

				// Get coordinates
				lat, lon := 0.0, 0.0
				if waypoint != nil {
					if loc, ok := waypoint["location"].(map[string]interface{}); ok {
						if latLng, ok := loc["latLng"].(map[string]interface{}); ok {
							if latitude, ok := latLng["latitude"].(float64); ok {
								lat = latitude
							}
							if longitude, ok := latLng["longitude"].(float64); ok {
								lon = longitude
							}
						}
					}
				}

				// Calculate ETA from transition
				if visitIdx < len(transitions) {
					if transition, ok := transitions[visitIdx].(map[string]interface{}); ok {
						if travelDuration, ok := transition["travelDuration"].(string); ok {
							var travelSeconds int
							fmt.Sscanf(travelDuration, "%ds", &travelSeconds)
							currentTime = currentTime.Add(time.Duration(travelSeconds) * time.Second)
						}
						if travelDistance, ok := transition["travelDistanceMeters"].(float64); ok {
							currentOdometer += travelDistance
						}
					}
				}

				// Determine stop type
				stopType := StopTypePickup
				if isPickup {
					// Check if it's a warehouse pickup
					if len(tags) > 0 && tags[0] == "warehouse-pickup" {
						stopType = StopTypePickup // Warehouse pickup for placement
					} else {
						stopType = StopTypePickup // Bin pickup for move request
					}
				} else {
					// Check if it's a placement delivery
					if len(tags) > 0 && tags[0] == "placement-delivery" {
						stopType = StopTypePlacement
					} else {
						stopType = StopTypeDropoff
					}
				}

				optimizedStop := OptimizedStop{
					Type:         stopType,
					LocationID:   shipmentLabel,
					LocationName: shipmentLabel,
					Latitude:     lat,
					Longitude:    lon,
					ETA:          currentTime,
					Odometer:     currentOdometer,
					Duration:     durationSeconds,
					Wait:         0,
				}

				// Set task references
				if strings.HasPrefix(shipmentLabel, "placement-") {
					optimizedStop.PlacementID = shipmentLabel
				} else if strings.HasPrefix(shipmentLabel, "move-") {
					optimizedStop.MoveRequestID = shipmentLabel
				}

				optimizedRoute.Stops = append(optimizedRoute.Stops, optimizedStop)
				currentTime = currentTime.Add(time.Duration(durationSeconds) * time.Second)
			}

			// Add end stop (return to warehouse)
			if len(transitions) > len(visits) {
				if transition, ok := transitions[len(visits)].(map[string]interface{}); ok {
					if travelDuration, ok := transition["travelDuration"].(string); ok {
						var travelSeconds int
						fmt.Sscanf(travelDuration, "%ds", &travelSeconds)
						currentTime = currentTime.Add(time.Duration(travelSeconds) * time.Second)
					}
					if travelDistance, ok := transition["travelDistanceMeters"].(float64); ok {
						currentOdometer += travelDistance
					}
				}
			}

			endStop := OptimizedStop{
				Type:         StopTypeEnd,
				LocationID:   "warehouse",
				LocationName: "Warehouse",
				Latitude:     originalReq.Vehicles[0].EndLocation.Latitude,
				Longitude:    originalReq.Vehicles[0].EndLocation.Longitude,
				ETA:          currentTime,
				Odometer:     currentOdometer,
				Duration:     0,
				Wait:         0,
			}
			optimizedRoute.Stops = append(optimizedRoute.Stops, endStop)

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
