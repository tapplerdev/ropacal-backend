package models

import "encoding/json"

// TaskType represents the type of task in a route
type TaskType string

const (
	TaskTypeCollection    TaskType = "collection"
	TaskTypePlacement     TaskType = "placement"
	TaskTypePickup        TaskType = "pickup"
	TaskTypeDropoff       TaskType = "dropoff"
	TaskTypeWarehouseStop TaskType = "warehouse_stop"
)

// RouteTask represents a single task in a driver's shift route
// Supports all task types: collections, placements, move requests, and warehouse stops
type RouteTask struct {
	// Core fields
	ID            string   `json:"id" db:"id"`
	ShiftID       string   `json:"shift_id" db:"shift_id"`
	SequenceOrder int      `json:"sequence_order" db:"sequence_order"`
	TaskType      TaskType `json:"task_type" db:"task_type"`
	Latitude      float64  `json:"latitude" db:"latitude"`
	Longitude     float64  `json:"longitude" db:"longitude"`
	Address       *string  `json:"address,omitempty" db:"address"`

	// Collection task fields
	BinID          *string `json:"bin_id,omitempty" db:"bin_id"`
	BinNumber      *int    `json:"bin_number,omitempty" db:"bin_number"`
	FillPercentage *int    `json:"fill_percentage,omitempty" db:"fill_percentage"`

	// Placement task fields
	PotentialLocationID *string `json:"potential_location_id,omitempty" db:"potential_location_id"`
	NewBinNumber        *string `json:"new_bin_number,omitempty" db:"new_bin_number"`

	// Move request task fields
	MoveRequestID        *string  `json:"move_request_id,omitempty" db:"move_request_id"`
	DestinationLatitude  *float64 `json:"destination_latitude,omitempty" db:"destination_latitude"`
	DestinationLongitude *float64 `json:"destination_longitude,omitempty" db:"destination_longitude"`
	DestinationAddress   *string  `json:"destination_address,omitempty" db:"destination_address"`
	MoveType             *string  `json:"move_type,omitempty" db:"move_type"`

	// Warehouse stop fields
	WarehouseAction *string `json:"warehouse_action,omitempty" db:"warehouse_action"` // "load", "unload", "both"
	BinsToLoad      *int    `json:"bins_to_load,omitempty" db:"bins_to_load"`

	// Route tracking
	RouteID *string `json:"route_id,omitempty" db:"route_id"`

	// Completion tracking
	IsCompleted           int    `json:"is_completed" db:"is_completed"`
	CompletedAt           *int64 `json:"completed_at,omitempty" db:"completed_at"`
	Skipped               bool   `json:"skipped" db:"skipped"`
	UpdatedFillPercentage *int   `json:"updated_fill_percentage,omitempty" db:"updated_fill_percentage"`

	// Metadata
	TaskData  json.RawMessage `json:"task_data,omitempty" db:"task_data"`
	CreatedAt int64           `json:"created_at" db:"created_at"`
	UpdatedAt *int64          `json:"updated_at,omitempty" db:"updated_at"`
}

// CreateShiftWithTasksRequest represents the request to create a shift with tasks
type CreateShiftWithTasksRequest struct {
	DriverID           string                   `json:"driver_id"`
	TruckBinCapacity   int                      `json:"truck_bin_capacity"`
	WarehouseLatitude  float64                  `json:"warehouse_latitude"`
	WarehouseLongitude float64                  `json:"warehouse_longitude"`
	WarehouseAddress   string                   `json:"warehouse_address"`
	Tasks              []map[string]interface{} `json:"tasks"` // Raw task data
}

// CompleteTaskRequest represents the request to complete a task
type CompleteTaskRequest struct {
	UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty"`
	PhotoURL              *string `json:"photo_url,omitempty"`
	NewBinID              *string `json:"new_bin_id,omitempty"` // For placement tasks
	HasIncident           bool    `json:"has_incident"`
	IncidentType          *string `json:"incident_type,omitempty"`
	IncidentPhotoURL      *string `json:"incident_photo_url,omitempty"`
	IncidentDescription   *string `json:"incident_description,omitempty"`
}

// CreateShiftWithTasksResponse represents the response after creating a shift
type CreateShiftWithTasksResponse struct {
	ShiftID   string `json:"shift_id"`
	TaskCount int    `json:"task_count"`
}
