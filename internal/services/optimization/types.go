package optimization

import "time"

// StopType represents the type of stop in a route
type StopType string

const (
	StopTypeStart        StopType = "start"
	StopTypeEnd          StopType = "end"
	StopTypeCollection   StopType = "collection"
	StopTypePlacement    StopType = "placement"
	StopTypePickup       StopType = "pickup"
	StopTypeDropoff      StopType = "dropoff"
	StopTypeWarehouse    StopType = "warehouse"
)

// Location represents a physical point to visit
type Location struct {
	ID          string
	Name        string
	Latitude    float64
	Longitude   float64
	Address     string
}

// Vehicle represents a truck/vehicle in the fleet
type Vehicle struct {
	ID              string
	Name            string
	StartLocation   Location
	EndLocation     Location
	Capacities      map[string]int // e.g., {"bins": 4, "weight_kg": 1000}
	Capabilities    []string       // e.g., ["refrigeration", "ladder"]
	RoutingProfile  string         // e.g., "mapbox/driving-traffic" (default if empty)
	EarliestStart   *time.Time
	LatestEnd       *time.Time
}

// Collection represents a bin collection task (no capacity impact)
type Collection struct {
	ID            string
	BinID         string
	BinNumber     int
	Location      Location
	Duration      int // seconds
	FillPercentage int
}

// Placement represents a new bin placement task (shipment)
type Placement struct {
	ID              string
	NewBinNumber    int
	WarehouseLocation Location
	PlacementLocation Location
	PickupDuration   int // seconds
	DropoffDuration  int // seconds
}

// MoveRequest represents picking up a bin and moving it (shipment)
type MoveRequest struct {
	ID               string
	BinID            string
	BinNumber        int
	PickupLocation   Location
	DropoffLocation  Location
	PickupDuration   int // seconds
	DropoffDuration  int // seconds
}

// WarehouseStop represents a stop at the warehouse (service)
type WarehouseStop struct {
	ID          string
	Location    Location
	Duration    int // seconds
	Action      string // "load", "unload", "both"
	BinsToLoad  int
}

// RouteRequest is the input to the optimizer
type RouteRequest struct {
	Vehicles       []Vehicle
	Collections    []Collection
	Placements     []Placement
	MoveRequests   []MoveRequest
	WarehouseStops []WarehouseStop
}

// OptimizedStop represents a single stop in the optimized route
type OptimizedStop struct {
	Type            StopType
	LocationID      string
	LocationName    string
	Latitude        float64
	Longitude       float64
	Address         string
	ETA             time.Time
	Odometer        float64 // meters
	Duration        int     // seconds (service time)
	Wait            int     // seconds (waiting time before service)

	// Task references
	CollectionID    string // For collection stops
	PlacementID     string // For placement stops
	MoveRequestID   string // For pickup/dropoff stops
	WarehouseStopID string // For warehouse stops

	// Task details
	BinNumber       int
	FillPercentage  int
}

// OptimizedRoute represents a complete route for one vehicle
type OptimizedRoute struct {
	VehicleID     string
	VehicleName   string
	Stops         []OptimizedStop
	TotalDistance float64 // meters
	TotalDuration int     // seconds
}

// RouteResponse is the output from the optimizer
type RouteResponse struct {
	Routes          []OptimizedRoute
	DroppedTasks    []string // IDs of tasks that couldn't be scheduled
	TotalDistance   float64
	TotalDuration   int
	OptimizerUsed   string // "here" or "mapbox"
}
