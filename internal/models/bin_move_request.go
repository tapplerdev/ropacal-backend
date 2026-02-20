package models

import (
	"strings"
	"time"
)

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
	MoveType                  string  `json:"move_type" db:"move_type"`                                             // 'store', 'relocation', or 'redeployment'
	DisposalAction            *string `json:"disposal_action,omitempty" db:"disposal_action"`                       // DEPRECATED: kept for backward compatibility
	Reason                    *string `json:"reason,omitempty" db:"reason"`
	Notes                     *string `json:"notes,omitempty" db:"notes"`
	SourcePotentialLocationID *string `json:"source_potential_location_id,omitempty" db:"source_potential_location_id"` // For warehouse redeployments to potential locations

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
	MoveType                  string  `json:"move_type"`
	DisposalAction            *string `json:"disposal_action,omitempty"`
	Reason                    *string `json:"reason,omitempty"`
	Notes                     *string `json:"notes,omitempty"`
	SourcePotentialLocationID *string `json:"source_potential_location_id,omitempty"`

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
	MoveType                  string  `json:"move_type" binding:"required"` // 'store', 'relocation', or 'redeployment'
	DisposalAction            *string `json:"disposal_action,omitempty"`    // DEPRECATED: kept for backward compatibility
	Reason                    *string `json:"reason,omitempty"`
	Notes                     *string `json:"notes,omitempty"`
	SourcePotentialLocationID *string `json:"source_potential_location_id,omitempty"` // For warehouse redeployments to potential locations

	// Change tracking
	ReasonCategory *string `json:"reason_category,omitempty"`   // 'landlord_complaint', 'theft', 'vandalism', 'missing', 'relocation_request', 'other'
	CreateNoGoZone *bool   `json:"create_no_go_zone,omitempty"` // Opt-in for relocation_request

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
		NewAddress:                bmr.NewAddress,
		MoveType:                  bmr.MoveType,
		DisposalAction:            bmr.DisposalAction,
		Reason:                    bmr.Reason,
		Notes:                     bmr.Notes,
		SourcePotentialLocationID: bmr.SourcePotentialLocationID,
		AssignmentType:            bmr.AssignmentType,
		AssignedShiftID:   bmr.AssignedShiftID,
		AssignedUserID:    bmr.AssignedUserID,
		CreatedAtIso:      time.Unix(bmr.CreatedAt, 0).Format(time.RFC3339),
		UpdatedAtIso:      time.Unix(bmr.UpdatedAt, 0).Format(time.RFC3339),
	}

	if bmr.CompletedAt != nil {
		iso := time.Unix(*bmr.CompletedAt, 0).Format(time.RFC3339)
		resp.CompletedAtIso = &iso
	}

	// Parse new_address into separate fields for frontend compatibility
	// Format: "Street, City Zip" (e.g., "1030 E El Camino Real, Sunnyvale, Sunnyvale 94087")
	if bmr.NewAddress != nil && *bmr.NewAddress != "" {
		parts := parseAddress(*bmr.NewAddress)
		if parts["street"] != "" {
			resp.NewStreet = &parts["street"]
		}
		if parts["city"] != "" {
			resp.NewCity = &parts["city"]
		}
		if parts["zip"] != "" {
			resp.NewZip = &parts["zip"]
		}
	}

	return resp
}

// parseAddress splits a combined address string into street, city, zip
// Expects format: "Street, City Zip" or "Street, City, City Zip"
func parseAddress(address string) map[string]string {
	result := map[string]string{
		"street": "",
		"city":   "",
		"zip":    "",
	}

	// Split by comma
	parts := []string{}
	for _, part := range []rune(address) {
		if part == ',' {
			parts = append(parts, "")
		} else if len(parts) == 0 {
			parts = append(parts, string(part))
		} else {
			parts[len(parts)-1] += string(part)
		}
	}

	// Trim whitespace from all parts
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	if len(parts) >= 2 {
		// Street is first part
		result["street"] = parts[0]

		// Last part should contain "City Zip"
		lastPart := parts[len(parts)-1]
		lastPartTokens := strings.Fields(lastPart)

		if len(lastPartTokens) >= 2 {
			// Last token is likely the ZIP
			result["zip"] = lastPartTokens[len(lastPartTokens)-1]
			// Everything else is the city
			result["city"] = strings.Join(lastPartTokens[:len(lastPartTokens)-1], " ")
		} else {
			// Just city, no zip
			result["city"] = lastPart
		}
	}

	return result
}
