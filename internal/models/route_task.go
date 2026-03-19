package models

import (
	"encoding/json"
	"time"
)

// TaskType represents the type of task in a route
type TaskType string

const (
	TaskTypeCollection    TaskType = "collection"
	TaskTypePlacement     TaskType = "placement"
	TaskTypePickup        TaskType = "pickup"
	TaskTypeDropoff       TaskType = "dropoff"
	TaskTypeWarehouseStop TaskType = "warehouse_stop"
	TaskTypeService       TaskType = "service"
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
	NewBinNumber        *int    `json:"new_bin_number,omitempty" db:"new_bin_number"`
	PlacementSource     *string `json:"placement_source,omitempty" db:"placement_source"` // "potential_location" | "warehouse"

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
	IsCompleted           int     `json:"is_completed" db:"is_completed"`
	CompletedAt           *int64  `json:"completed_at,omitempty" db:"completed_at"`
	Skipped               bool    `json:"skipped" db:"skipped"`
	UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty" db:"updated_fill_percentage"`
	PhotoURL              *string `json:"photo_url,omitempty" db:"photo_url"` // Populated via LEFT JOIN on checks table (not a route_tasks column)

	// Addition tracking (for audit trail)
	AddedBy        *string `json:"added_by,omitempty" db:"added_by"`             // User ID who added the task (NULL if created with shift)
	AdditionReason *string `json:"addition_reason,omitempty" db:"addition_reason"` // Reason task was added

	// Deletion tracking (for audit trail)
	IsDeleted      bool    `json:"is_deleted" db:"is_deleted"`
	DeletedAt      *int64  `json:"deleted_at,omitempty" db:"deleted_at"`
	DeletedBy      *string `json:"deleted_by,omitempty" db:"deleted_by"`       // User ID who deleted the task
	DeletionReason *string `json:"deletion_reason,omitempty" db:"deletion_reason"`

	// Time window fields (for Mapbox v2 service_times)
	EarliestArrival        *time.Time `json:"earliest_arrival,omitempty" db:"earliest_arrival"`
	LatestArrival          *time.Time `json:"latest_arrival,omitempty" db:"latest_arrival"`
	TimeWindowType         *string    `json:"time_window_type,omitempty" db:"time_window_type"`
	ServiceDurationSeconds *int       `json:"service_duration_seconds,omitempty" db:"service_duration_seconds"`

	// Service task fields
	TaskLabel       *string `json:"task_label,omitempty" db:"task_label"`
	TaskDescription *string `json:"task_description,omitempty" db:"task_description"`
	PhotoRequired   bool    `json:"photo_required" db:"photo_required"`
	CompletionNotes *string `json:"completion_notes,omitempty" db:"completion_notes"`

	// Metadata
	TaskData  *json.RawMessage `json:"task_data,omitempty" db:"task_data"`
	CreatedAt int64            `json:"created_at" db:"created_at"`
	UpdatedAt *int64           `json:"updated_at,omitempty" db:"updated_at"`
}

// WarehouseDeploymentTask represents a request to deploy an in_storage bin to a new field address
type WarehouseDeploymentTask struct {
	BinID                string  `json:"bin_id"`                // ID of the existing in_storage bin
	BinNumber            int     `json:"bin_number"`            // Bin number (for display/logging)
	DestinationAddress   string  `json:"destination_address"`
	DestinationLatitude  float64 `json:"destination_latitude"`
	DestinationLongitude float64 `json:"destination_longitude"`
}

// CreateShiftWithTasksRequest represents the request to create a shift with tasks
type CreateShiftWithTasksRequest struct {
	DriverID             string                   `json:"driver_id"`
	TruckBinCapacity     int                      `json:"truck_bin_capacity"`
	WarehouseLatitude    *float64                 `json:"warehouse_latitude,omitempty"`
	WarehouseLongitude   *float64                 `json:"warehouse_longitude,omitempty"`
	WarehouseAddress     *string                  `json:"warehouse_address,omitempty"`
	LockRouteOrder       bool                     `json:"lock_route_order"`                        // If true, skip optimization on shift start
	Tasks                []map[string]interface{} `json:"tasks"`                                   // Raw task data
	WarehouseDeployments []WarehouseDeploymentTask `json:"warehouse_deployments,omitempty"`         // Bins to deploy from warehouse

	// Custom shift fields
	ShiftType      string   `json:"shift_type,omitempty"`      // "standard" (default) or "custom"
	ShiftLabel     *string  `json:"shift_label,omitempty"`     // Display name for custom shifts
	StartLatitude  *float64 `json:"start_latitude,omitempty"`  // Custom vehicle start location
	StartLongitude *float64 `json:"start_longitude,omitempty"`
	StartAddress   *string  `json:"start_address,omitempty"`
	EndLatitude    *float64 `json:"end_latitude,omitempty"` // Custom vehicle end location
	EndLongitude   *float64 `json:"end_longitude,omitempty"`
	EndAddress     *string  `json:"end_address,omitempty"`
	ScheduledStart *string  `json:"scheduled_start,omitempty"` // ISO 8601 — vehicle earliest_start
	ScheduledEnd   *string  `json:"scheduled_end,omitempty"`   // ISO 8601 — vehicle latest_end
}

// CompleteTaskRequest represents the request to complete a task
type CompleteTaskRequest struct {
	UpdatedFillPercentage *int    `json:"updated_fill_percentage,omitempty"`
	PhotoURL              *string `json:"photo_url,omitempty"`
	NewBinID              *string `json:"new_bin_id,omitempty"`          // DEPRECATED: For backward compatibility
	NewBinNumber          int     `json:"new_bin_number"`                // REQUIRED: Driver-provided bin number for placements
	HasIncident           bool    `json:"has_incident"`
	IncidentType          *string `json:"incident_type,omitempty"`
	IncidentPhotoURL      *string `json:"incident_photo_url,omitempty"`
	IncidentDescription   *string `json:"incident_description,omitempty"`
	CompletionNotes       *string `json:"completion_notes,omitempty"` // Driver notes for service tasks
}

// CreateShiftWithTasksResponse represents the response after creating a shift
type CreateShiftWithTasksResponse struct {
	ShiftID   string `json:"shift_id"`
	TaskCount int    `json:"task_count"`
}
