package models

import "time"

type Check struct {
	ID             int     `json:"id" db:"id"`
	BinID          string  `json:"bin_id" db:"bin_id"`
	CheckedFrom    string  `json:"checked_from" db:"checked_from"`
	FillPercentage *int    `json:"fill_percentage" db:"fill_percentage"` // Nullable for incident-only check-ins
	CheckedOn      int64   `json:"checked_on" db:"checked_on"`           // Unix timestamp
	PhotoUrl       *string `json:"photo_url" db:"photo_url"`             // Cloudinary URL
	CheckedBy      *string `json:"checked_by" db:"checked_by"`           // User ID who performed the check
	ShiftID        *string `json:"shift_id" db:"shift_id"`               // Shift during which check was performed
	MoveRequestID  *string `json:"move_request_id" db:"move_request_id"` // Links to move request if this check was for pickup/dropoff
}

// CheckResponse is what we send to the client
type CheckResponse struct {
	ID                     int     `json:"id"`
	BinID                  string  `json:"binId"`
	CheckedFrom            string  `json:"checkedFrom"`
	FillPercentage         *int    `json:"fillPercentage"`         // Current fill % after check
	PreviousFillPercentage *int    `json:"previousFillPercentage"` // Previous fill % before this check (calculated from prior check)
	CheckedOnIso           string  `json:"checkedOnIso"`
	CheckedOn              string  `json:"checkedOn"`      // formatted date
	PhotoUrl               *string `json:"photoUrl"`       // Cloudinary URL
	CheckedBy              *string `json:"checkedBy"`      // User ID
	CheckedByName          *string `json:"checkedByName"`  // Driver's name (joined from users table)
	ShiftID                *string `json:"shiftId"`        // Shift ID during which check was performed
	ShiftStatus            *string `json:"shiftStatus"`    // Shift status (active, ended, etc.) - joined from shifts table
	MoveRequestID          *string `json:"moveRequestId"`  // Links to move request if this check was for pickup/dropoff
	BinLocation            *string `json:"binLocation"`    // Bin's actual address (joined from bins table)
}

// ToCheckResponse converts a Check to CheckResponse
// Note: CheckedByName, PreviousFillPercentage, ShiftStatus, and BinLocation must be populated separately by handler
func (c *Check) ToCheckResponse() CheckResponse {
	t := time.Unix(c.CheckedOn, 0)
	return CheckResponse{
		ID:                     c.ID,
		BinID:                  c.BinID,
		CheckedFrom:            c.CheckedFrom,
		FillPercentage:         c.FillPercentage,
		PreviousFillPercentage: nil, // Must be calculated by handler from previous check
		CheckedOnIso:           t.Format(time.RFC3339),
		CheckedOn:              t.Format("Jan 02, 2006"),
		PhotoUrl:               c.PhotoUrl,
		CheckedBy:              c.CheckedBy,
		CheckedByName:          nil, // Must be populated by handler with JOIN query
		ShiftID:                c.ShiftID,
		ShiftStatus:            nil, // Must be populated by handler with JOIN query
		MoveRequestID:          c.MoveRequestID,
		BinLocation:            nil, // Must be populated by handler with JOIN query
	}
}
