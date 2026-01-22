package models

import "time"

type BinMoveRequest struct {
	ID            string `json:"id" db:"id"`
	BinID         string `json:"bin_id" db:"bin_id"`
	ScheduledDate int64  `json:"scheduled_date" db:"scheduled_date"` // Unix timestamp
	Urgency       string `json:"urgency" db:"urgency"`               // 'urgent' or 'scheduled'
	RequestedBy   string `json:"requested_by" db:"requested_by"`     // User ID
	Status        string `json:"status" db:"status"`                 // 'pending', 'in_progress', 'completed', 'cancelled'

	// Original location
	OriginalLatitude  float64 `json:"original_latitude" db:"original_latitude"`
	OriginalLongitude float64 `json:"original_longitude" db:"original_longitude"`
	OriginalAddress   string  `json:"original_address" db:"original_address"`

	// New location (nullable for pickup-only)
	NewLatitude  *float64 `json:"new_latitude,omitempty" db:"new_latitude"`
	NewLongitude *float64 `json:"new_longitude,omitempty" db:"new_longitude"`
	NewAddress   *string  `json:"new_address,omitempty" db:"new_address"`

	// Move metadata
	MoveType       string  `json:"move_type" db:"move_type"`                       // 'store' or 'relocation'
	DisposalAction *string `json:"disposal_action,omitempty" db:"disposal_action"` // DEPRECATED: kept for backward compatibility
	Reason         *string `json:"reason,omitempty" db:"reason"`
	Notes          *string `json:"notes,omitempty" db:"notes"`

	// Assignment (shift-based or manual)
	AssignmentType  *string `json:"assignment_type,omitempty" db:"assignment_type"` // 'shift' or 'manual', NULL for unassigned
	AssignedShiftID *string `json:"assigned_shift_id,omitempty" db:"assigned_shift_id"`
	AssignedUserID  *string `json:"assigned_user_id,omitempty" db:"assigned_user_id"` // For manual moves
	CompletedAt     *int64  `json:"completed_at,omitempty" db:"completed_at"`

	// Timestamps
	CreatedAt int64 `json:"created_at" db:"created_at"`
	UpdatedAt int64 `json:"updated_at" db:"updated_at"`
}

// BinMoveRequestResponse includes ISO formatted timestamps for client
type BinMoveRequestResponse struct {
	ID               string `json:"id"`
	BinID            string `json:"bin_id"`
	ScheduledDate    int64  `json:"scheduled_date"` // Unix timestamp (for frontend date math)
	ScheduledDateIso string  `json:"scheduled_date_iso"`
	Urgency          string  `json:"urgency"`
	RequestedBy      string  `json:"requested_by"`
	RequestedByName  *string `json:"requested_by_name,omitempty"` // Requester's name (populated from users table)
	Status           string  `json:"status"`

	// Flattened bin fields (for easy table display)
	BinNumber     int    `json:"bin_number"`
	CurrentStreet string `json:"current_street"`
	City          string `json:"city"`
	Zip           string `json:"zip"`

	// Original location
	OriginalStreet    *string  `json:"original_street,omitempty"`
	OriginalCity      *string  `json:"original_city,omitempty"`
	OriginalZip       *string  `json:"original_zip,omitempty"`
	OriginalLatitude  float64  `json:"original_latitude"`
	OriginalLongitude float64  `json:"original_longitude"`
	OriginalAddress   string   `json:"original_address"`

	// New location
	NewStreet    *string  `json:"new_street,omitempty"`
	NewCity      *string  `json:"new_city,omitempty"`
	NewZip       *string  `json:"new_zip,omitempty"`
	NewLatitude  *float64 `json:"new_latitude,omitempty"`
	NewLongitude *float64 `json:"new_longitude,omitempty"`
	NewAddress   *string  `json:"new_address,omitempty"`

	// Move metadata
	MoveType       string  `json:"move_type"`
	DisposalAction *string `json:"disposal_action,omitempty"`
	Reason         *string `json:"reason,omitempty"`
	Notes          *string `json:"notes,omitempty"`

	// Assignment (shift-based or manual)
	AssignmentType     *string `json:"assignment_type,omitempty"`     // 'shift' or 'manual', NULL for unassigned
	AssignedShiftID    *string `json:"assigned_shift_id,omitempty"`
	AssignedDriverName *string `json:"assigned_driver_name,omitempty"` // Driver's full name (populated when assigned to shift)
	AssignedUserID     *string `json:"assigned_user_id,omitempty"`
	AssignedUserName   *string `json:"assigned_user_name,omitempty"` // User's full name (populated when assigned manually)
	DriverName         *string `json:"driver_name,omitempty"`         // Unified field: returns driver or user name (whichever is set)
	CompletedAtIso     *string `json:"completed_at_iso,omitempty"`

	// Timestamps
	CreatedAtIso string `json:"created_at_iso"`
	UpdatedAtIso string `json:"updated_at_iso"`

	// Populated bin details (optional, for dashboard display)
	Bin *BinResponse `json:"bin,omitempty"`
}

// CreateBinMoveRequest is the request body for POST /api/manager/bins/schedule-move
type CreateBinMoveRequest struct {
	BinID         string `json:"bin_id" binding:"required"`
	ScheduledDate int64  `json:"scheduled_date" binding:"required"` // Unix timestamp
	// Urgency is now auto-calculated on backend, not required from frontend

	// New location (optional for pickup-only)
	// Accept both single address string (backward compatibility) and separate fields
	NewLatitude  *float64 `json:"new_latitude,omitempty"`
	NewLongitude *float64 `json:"new_longitude,omitempty"`
	NewAddress   *string  `json:"new_address,omitempty"` // Single combined address (backward compatibility)
	NewStreet    *string  `json:"new_street,omitempty"`  // Separate address fields (new format)
	NewCity      *string  `json:"new_city,omitempty"`
	NewZip       *string  `json:"new_zip,omitempty"`

	// Move metadata
	MoveType       string  `json:"move_type" binding:"required"` // 'store' or 'relocation'
	DisposalAction *string `json:"disposal_action,omitempty"`    // DEPRECATED: kept for backward compatibility
	Reason         *string `json:"reason,omitempty"`
	Notes          *string `json:"notes,omitempty"`

	// Assignment (optional - if provided, assigns to shift immediately)
	ShiftID *string `json:"shift_id,omitempty"`
}

// ToBinMoveRequestResponse converts BinMoveRequest to BinMoveRequestResponse
func (bmr *BinMoveRequest) ToBinMoveRequestResponse() BinMoveRequestResponse {
	resp := BinMoveRequestResponse{
		ID:                bmr.ID,
		BinID:             bmr.BinID,
		ScheduledDate:     bmr.ScheduledDate, // Include Unix timestamp
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
		AssignmentType:    bmr.AssignmentType,
		AssignedShiftID:   bmr.AssignedShiftID,
		AssignedUserID:    bmr.AssignedUserID,
		CreatedAtIso:      time.Unix(bmr.CreatedAt, 0).Format(time.RFC3339),
		UpdatedAtIso:      time.Unix(bmr.UpdatedAt, 0).Format(time.RFC3339),
	}

	if bmr.CompletedAt != nil {
		iso := time.Unix(*bmr.CompletedAt, 0).Format(time.RFC3339)
		resp.CompletedAtIso = &iso
	}

	return resp
}
