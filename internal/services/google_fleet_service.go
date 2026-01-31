package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// GoogleFleetService handles Google Route Optimization API (Fleet Routing)
type GoogleFleetService struct {
	apiKey     string
	projectID  string
}

// NewGoogleFleetService creates a new Google Fleet Routing service
func NewGoogleFleetService(apiKey, projectID string) *GoogleFleetService {
	return &GoogleFleetService{
		apiKey:    apiKey,
		projectID: projectID,
	}
}

// LatLng represents a geographic coordinate
type LatLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Location represents a waypoint location
type Location struct {
	LatLng LatLng `json:"latLng"`
}

// Waypoint represents an arrival/departure waypoint
type Waypoint struct {
	Location Location `json:"location"`
}

// TimeWindow represents a time constraint
type TimeWindow struct {
	StartTime string `json:"startTime"` // RFC3339 format
	EndTime   string `json:"endTime"`   // RFC3339 format
}

// VisitRequest represents a pickup or delivery visit
type VisitRequest struct {
	ArrivalWaypoint Waypoint     `json:"arrivalWaypoint"`
	Duration        string       `json:"duration,omitempty"`        // e.g., "900s" for 15 minutes
	TimeWindows     []TimeWindow `json:"timeWindows,omitempty"`
	Label           string       `json:"label,omitempty"`
}

// LoadDemand represents capacity requirements
type LoadDemand struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
}

// Shipment represents a bin collection stop (delivery-only)
type Shipment struct {
	Deliveries   []VisitRequest `json:"deliveries"`
	Label        string         `json:"label"`
	LoadDemands  map[string]LoadDemand `json:"loadDemands,omitempty"`
}

// Vehicle represents a truck in the fleet
type Vehicle struct {
	StartWaypoint     Waypoint     `json:"startWaypoint"`
	EndWaypoint       Waypoint     `json:"endWaypoint"`
	Label             string       `json:"label,omitempty"`
	CostPerHour       float64      `json:"costPerHour,omitempty"`
	CostPerKilometer  float64      `json:"costPerKilometer,omitempty"`
	StartTimeWindows  []TimeWindow `json:"startTimeWindows,omitempty"`
	EndTimeWindows    []TimeWindow `json:"endTimeWindows,omitempty"`
	LoadLimits        map[string]LoadLimit `json:"loadLimits,omitempty"`
}

// LoadLimit represents vehicle capacity constraints
type LoadLimit struct {
	MaxLoad string `json:"maxLoad"`
}

// ShipmentModel represents the optimization problem
type ShipmentModel struct {
	Shipments       []Shipment `json:"shipments"`
	Vehicles        []Vehicle  `json:"vehicles"`
	GlobalStartTime string     `json:"globalStartTime"` // RFC3339
	GlobalEndTime   string     `json:"globalEndTime"`   // RFC3339
}

// OptimizeToursRequest is the main API request
type OptimizeToursRequest struct {
	Parent                      string        `json:"-"` // Set separately, not in JSON
	Model                       ShipmentModel `json:"model"`
	Timeout                     string        `json:"timeout,omitempty"` // e.g., "30s"
	ConsiderRoadTraffic         bool          `json:"considerRoadTraffic,omitempty"`
	PopulatePolylines           bool          `json:"populatePolylines,omitempty"`
	PopulateTransitionPolylines bool          `json:"populateTransitionPolylines,omitempty"`
}

// Visit represents a stop in the optimized route
type Visit struct {
	ShipmentIndex int    `json:"shipmentIndex"`
	IsPickup      bool   `json:"isPickup"`
	ShipmentLabel string `json:"shipmentLabel"`
	StartTime     string `json:"startTime"`
}

// Transition represents travel between stops
type Transition struct {
	TravelDuration string  `json:"travelDuration"`
	TravelDistanceMeters float64 `json:"travelDistanceMeters"`
}

// ShipmentRoute represents the optimized route for one vehicle
type ShipmentRoute struct {
	VehicleIndex        int          `json:"vehicleIndex"`
	VehicleLabel        string       `json:"vehicleLabel"`
	Visits              []Visit      `json:"visits"`
	Transitions         []Transition `json:"transitions"`
	RoutePolyline       interface{}  `json:"routePolyline,omitempty"`
	Metrics             RouteMetrics `json:"metrics"`
}

// RouteMetrics contains route statistics
type RouteMetrics struct {
	PerformedShipmentCount int     `json:"performedShipmentCount"`
	TravelDuration         string  `json:"travelDuration"`
	TravelDistanceMeters   float64 `json:"travelDistanceMeters"`
	VisitDuration          string  `json:"visitDuration,omitempty"`
}

// Metrics contains overall optimization statistics
type Metrics struct {
	UsedVehicleCount       int     `json:"usedVehicleCount"`
	TotalCost              float64 `json:"totalCost"`
	Costs                  map[string]float64 `json:"costs"`
}

// OptimizeToursResponse is the API response
type OptimizeToursResponse struct {
	Routes  []ShipmentRoute `json:"routes"`
	Metrics Metrics         `json:"metrics"`
	RequestLabel string     `json:"requestLabel,omitempty"`
}

// OptimizeTours calls Google Route Optimization API
func (gfs *GoogleFleetService) OptimizeTours(request OptimizeToursRequest) (*OptimizeToursResponse, error) {
	// Build endpoint URL
	endpoint := fmt.Sprintf("https://routeoptimization.googleapis.com/v1/projects/%s:optimizeTours", gfs.projectID)

	// Marshal request to JSON
	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	log.Printf("🚛 Google Fleet Routing Request:")
	log.Printf("   Endpoint: %s", endpoint)
	log.Printf("   Shipments: %d", len(request.Model.Shipments))
	log.Printf("   Vehicles: %d", len(request.Model.Vehicles))

	// Create HTTP request
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-goog-api-key", gfs.apiKey)

	// Make request
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		log.Printf("❌ Google Fleet API Error (Status %d): %s", resp.StatusCode, string(body))
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var result OptimizeToursResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	log.Printf("✅ Google Fleet Routing Response:")
	log.Printf("   Routes: %d", len(result.Routes))
	log.Printf("   Used Vehicles: %d", result.Metrics.UsedVehicleCount)
	log.Printf("   Total Cost: $%.2f", result.Metrics.TotalCost)

	return &result, nil
}
