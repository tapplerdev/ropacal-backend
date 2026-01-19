package models

import "time"

type BinMoveRequest struct {
	ID               string   `json:"id" db:"id"`
	BinID            string   `json:"bin_id" db:"bin_id"`
	ScheduledDate    int64    `json:"scheduled_date" db:"scheduled_date"` // Unix timestamp
	Urgency          string   `json:"urgency" db:"urgency"` // 'urgent' or 'scheduled'
	RequestedBy      string   `json:"requested_by" db:"requested_by"` // User ID
	Status           string   `json:"status" db:"status"` // 'pending', 'in_progress', 'completed', 'cancelled'

	// Original location
	OriginalLatitude  float64 `json:"original_latitude" db:"original_latitude"`
	OriginalLongitude float64 `json:"original_longitude" db:"original_longitude"`
	OriginalAddress   string  `json:"original_address" db:"original_address"`

	// New location (nullable for pickup-only)
	NewLatitude  *float64 `json:"new_latitude,omitempty" db:"new_latitude"`
	NewLongitude *float64 `json:"new_longitude,omitempty" db:"new_longitude"`
	NewAddress   *string  `json:"new_address,omitempty" db:"new_address"`

	// Move metadata
	MoveType        string  `json:"move_type" db:"move_type"` // 'pickup_only' or 'relocation'
	DisposalAction  *string `json:"disposal_action,omitempty" db:"disposal_action"` // 'retire' or 'store'
	Reason          *string `json:"reason,omitempty" db:"reason"`
	Notes           *string `json:"notes,omitempty" db:"notes"`

	// Assigned shift
	AssignedShiftID *string `json:"assigned_shift_id,omitempty" db:"assigned_shift_id"`
	CompletedAt     *int64  `json:"completed_at,omitempty" db:"completed_at"`

	// Timestamps
	CreatedAt int64 `json:"created_at" db:"created_at"`
	UpdatedAt int64 `json:"updated_at" db:"updated_at"`
}

// BinMoveRequestResponse includes ISO formatted timestamps for client
type BinMoveRequestResponse struct {
	ID               string   `json:"id"`
	BinID            string   `json:"bin_id"`
	ScheduledDateIso string   `json:"scheduled_date_iso"`
	Urgency          string   `json:"urgency"`
	RequestedBy      string   `json:"requested_by"`
	Status           string   `json:"status"`

	// Original location
	OriginalLatitude  float64 `json:"original_latitude"`
	OriginalLongitude float64 `json:"original_longitude"`
	OriginalAddress   string  `json:"original_address"`

	// New location
	NewLatitude  *float64 `json:"new_latitude,omitempty"`
	NewLongitude *float64 `json:"new_longitude,omitempty"`
	NewAddress   *string  `json:"new_address,omitempty"`

	// Move metadata
	MoveType        string  `json:"move_type"`
	DisposalAction  *string `json:"disposal_action,omitempty"`
	Reason          *string `json:"reason,omitempty"`
	Notes           *string `json:"notes,omitempty"`

	// Assigned shift
	AssignedShiftID *string `json:"assigned_shift_id,omitempty"`
	CompletedAtIso  *string `json:"completed_at_iso,omitempty"`

	// Timestamps
	CreatedAtIso string `json:"created_at_iso"`
	UpdatedAtIso string `json:"updated_at_iso"`

	// Populated bin details (optional, for dashboard display)
	Bin *BinResponse `json:"bin,omitempty"`
}

// CreateBinMoveRequest is the request body for POST /api/manager/bins/schedule-move
type CreateBinMoveRequest struct {
	BinID         string   `json:"bin_id" binding:"required"`
	ScheduledDate int64    `json:"scheduled_date" binding:"required"` // Unix timestamp
	Urgency       string   `json:"urgency" binding:"required"` // 'urgent' or 'scheduled'

	// New location (optional for pickup-only)
	NewLatitude  *float64 `json:"new_latitude,omitempty"`
	NewLongitude *float64 `json:"new_longitude,omitempty"`
	NewAddress   *string  `json:"new_address,omitempty"`

	// Move metadata
	MoveType        string  `json:"move_type" binding:"required"` // 'pickup_only' or 'relocation'
	DisposalAction  *string `json:"disposal_action,omitempty"` // Required if move_type is 'pickup_only'
	Reason          *string `json:"reason,omitempty"`
	Notes           *string `json:"notes,omitempty"`
}

// ToBinMoveRequestResponse converts BinMoveRequest to BinMoveRequestResponse
func (bmr *BinMoveRequest) ToBinMoveRequestResponse() BinMoveRequestResponse {
	resp := BinMoveRequestResponse{
		ID:                bmr.ID,
		BinID:             bmr.BinID,
		ScheduledDateIso:  time.Unix(bmr.ScheduledDate, 0).Format(time.RFC3339),
		Urgency:           bmr.Urgency,
		RequestedBy:       bmr.RequestedBy,
		Status:            bmr.Status,
		OriginalLatitude:  bmr.OriginalLatitude,
		OriginalLongitude: bmr.OriginalLongitude,
		OriginalAddress:   bmr.OriginalAddress,
		NewLatitude:       bmr.NewLatitude,
		NewLongitude:      bmr.NewLongitude,
		NewAddress:        bmr.NewAddress,
		MoveType:          bmr.MoveType,
		DisposalAction:    bmr.DisposalAction,
		Reason:            bmr.Reason,
		Notes:             bmr.Notes,
		AssignedShiftID:   bmr.AssignedShiftID,
		CreatedAtIso:      time.Unix(bmr.CreatedAt, 0).Format(time.RFC3339),
		UpdatedAtIso:      time.Unix(bmr.UpdatedAt, 0).Format(time.RFC3339),
	}

	if bmr.CompletedAt != nil {
		iso := time.Unix(*bmr.CompletedAt, 0).Format(time.RFC3339)
		resp.CompletedAtIso = &iso
	}

	return resp
}
