package models

import "encoding/json"

// ShiftBin represents a bin assigned to an active shift (from route_tasks table)
// Note: This was formerly called RouteBin, but renamed for clarity
type ShiftBin struct {
	ID            int     `db:"id" json:"id"`
	ShiftID       string  `db:"shift_id" json:"shift_id"`
	BinID         string  `db:"bin_id" json:"bin_id"`
	SequenceOrder int     `db:"sequence_order" json:"sequence_order"`
	IsCompleted   int     `db:"is_completed" json:"is_completed"` // SQLite uses INTEGER for boolean
	CompletedAt   *int64  `db:"completed_at" json:"completed_at"`
	CreatedAt     int64   `db:"created_at" json:"created_at"`
	StopType      string  `db:"stop_type" json:"stop_type"`
	MoveRequestID *string `db:"move_request_id" json:"move_request_id"`
}

// ShiftBinWithDetails extends ShiftBin with bin details for API responses
// MIGRATED: Now uses RouteTask field names (address, task_type, destination_address)
type ShiftBinWithDetails struct {
	ID                    string   `db:"id" json:"id"`
	ShiftID               string   `db:"shift_id" json:"shift_id"`
	BinID                 string   `db:"bin_id" json:"bin_id"`
	SequenceOrder         int      `db:"sequence_order" json:"sequence_order"`
	IsCompleted           int      `db:"is_completed" json:"is_completed"`
	CompletedAt           *int64   `db:"completed_at" json:"completed_at"`
	UpdatedFillPercentage *int     `db:"updated_fill_percentage" json:"updated_fill_percentage"`
	CreatedAt             int64    `db:"created_at" json:"created_at"`
	BinNumber             int      `db:"bin_number" json:"bin_number"`
	Address               string   `db:"address" json:"address"` // RENAMED from CurrentStreet
	FillPercentage        int      `db:"fill_percentage" json:"fill_percentage"`
	Latitude              float64  `db:"latitude" json:"latitude"`
	Longitude             float64  `db:"longitude" json:"longitude"`
	TaskType              string   `db:"task_type" json:"task_type"` // RENAMED from StopType
	MoveRequestID         *string  `db:"move_request_id" json:"move_request_id"`
	DestinationAddress    *string  `db:"destination_address" json:"destination_address"` // RENAMED from NewAddress
	MoveType              *string  `db:"move_type" json:"move_type"`

	// Placement task fields
	PotentialLocationID *string `db:"potential_location_id" json:"potential_location_id"`
	NewBinNumber        *int    `db:"new_bin_number" json:"new_bin_number"`

	// Warehouse stop fields
	WarehouseAction *string `db:"warehouse_action" json:"warehouse_action"`
	BinsToLoad      *int    `db:"bins_to_load" json:"bins_to_load"`

	// Skip tracking fields
	Skipped  bool             `db:"skipped" json:"skipped"`
	TaskData *json.RawMessage `db:"task_data" json:"task_data,omitempty"`
}
