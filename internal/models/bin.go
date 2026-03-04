package models

import "time"

type Bin struct {
	ID              string   `json:"id" db:"id"`
	BinNumber       int      `json:"bin_number" db:"bin_number"`
	CurrentStreet   string   `json:"current_street" db:"current_street"`
	City            string   `json:"city" db:"city"`
	Zip             string   `json:"zip" db:"zip"`
	LastMoved       *int64   `json:"last_moved,omitempty" db:"last_moved"`           // Unix timestamp
	LastChecked     *int64   `json:"last_checked,omitempty" db:"last_checked"`       // Unix timestamp
	LastCheckedAt   *int64   `json:"last_checked_at,omitempty" db:"last_checked_at"` // Unix timestamp (for priority calc)
	Status          string   `json:"status" db:"status"`                             // 'active', 'missing', 'retired', 'in_storage', 'pending_move'
	FillPercentage  *int     `json:"fill_percentage,omitempty" db:"fill_percentage"`
	Checked         bool     `json:"checked" db:"checked"`
	MoveRequested   bool     `json:"move_requested" db:"move_requested"`
	Latitude                  *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude                 *float64 `json:"longitude,omitempty" db:"longitude"`
	CreatedByUserID           *string  `json:"created_by_user_id,omitempty" db:"created_by_user_id"`                       // User who created the bin
	PlacementPhotoURL         *string  `json:"placement_photo_url,omitempty" db:"placement_photo_url"`                     // Photo taken during driver placement
	SourcePotentialLocationID *string  `json:"source_potential_location_id,omitempty" db:"source_potential_location_id"` // Potential location this bin was created from
	RetiredAt                 *int64   `json:"retired_at,omitempty" db:"retired_at"`                                       // Unix timestamp when retired
	RetiredByUserID           *string  `json:"retired_by_user_id,omitempty" db:"retired_by_user_id"`                       // User who retired the bin
	CreatedAt                 int64    `json:"created_at" db:"created_at"`                                                 // Unix timestamp
	UpdatedAt                 int64    `json:"updated_at" db:"updated_at"`                                                 // Unix timestamp
}

// BinResponse is what we send to the client with ISO timestamps
type BinResponse struct {
	ID               string   `json:"id"`
	BinNumber        int      `json:"bin_number"`
	CurrentStreet    string   `json:"current_street"`
	City             string   `json:"city"`
	Zip              string   `json:"zip"`
	LastMovedIso     *string  `json:"lastMovedIso,omitempty"`
	LastCheckedIso   *string  `json:"lastCheckedIso,omitempty"`
	LastCheckedAtIso *string  `json:"lastCheckedAtIso,omitempty"`
	Status           string   `json:"status"`
	FillPercentage   *int     `json:"fill_percentage,omitempty"`
	Checked          bool     `json:"checked"`
	MoveRequested    bool     `json:"move_requested"`
	Latitude                  *float64 `json:"latitude,omitempty"`
	Longitude                 *float64 `json:"longitude,omitempty"`
	CreatedByUserID           *string  `json:"created_by_user_id,omitempty"`
	PlacementPhotoURL         *string  `json:"placement_photo_url,omitempty"`
	SourcePotentialLocationID *string  `json:"source_potential_location_id,omitempty"`
	RetiredAtIso              *string  `json:"retiredAtIso,omitempty"`
	RetiredByUserID           *string  `json:"retired_by_user_id,omitempty"`
	PriorityScore             *float64 `json:"priority_score,omitempty"` // Calculated priority (used for sorting)
}

// UpdateBinRequest is the request body for PATCH /api/bins/:id
type UpdateBinRequest struct {
	BinNumber      int      `json:"bin_number"`
	CurrentStreet  string   `json:"current_street"`
	City           string   `json:"city"`
	Zip            string   `json:"zip"`
	Status         string   `json:"status"`
	Checked        bool     `json:"checked"`
	FillPercentage *int     `json:"fill_percentage,omitempty"`
	MoveRequested  bool     `json:"move_requested"`
	Latitude       *float64 `json:"latitude,omitempty"`  // Optional - if provided, won't be cleared
	Longitude      *float64 `json:"longitude,omitempty"` // Optional - if provided, won't be cleared
	CheckedFrom    *string  `json:"checkedFrom,omitempty"`
	CheckedOnIso   *string  `json:"checkedOnIso,omitempty"`
	PhotoUrl       *string  `json:"photoUrl,omitempty"` // Optional photo URL from Cloudinary

	// Admin change tracking
	ReasonCategory *string `json:"reason_category,omitempty"` // Required when meaningful change is made
	SourcePotentialLocationID *string `json:"source_potential_location_id,omitempty"` // Optional - for tracking potential location conversions
	ReasonNotes    *string `json:"reason_notes,omitempty"`
	CreateNoGoZone *bool   `json:"create_no_go_zone,omitempty"` // Opt-in for relocation_request
}

// CreateBinRequest is the request body for POST /api/bins
type CreateBinRequest struct {
	BinNumber                 *int     `json:"bin_number,omitempty"`                    // Optional - auto-assigned if not provided
	CurrentStreet             string   `json:"current_street"`
	City                      string   `json:"city"`
	Zip                       string   `json:"zip"`
	Status                    string   `json:"status"`
	FillPercentage            *int     `json:"fill_percentage,omitempty"`
	Latitude                  *float64 `json:"latitude,omitempty"`
	Longitude                 *float64 `json:"longitude,omitempty"`
	SourcePotentialLocationID *string  `json:"source_potential_location_id,omitempty"` // Optional - for tracking potential location conversions
}

// ToBinResponse converts a Bin to BinResponse
func (b *Bin) ToBinResponse() BinResponse {
	resp := BinResponse{
		ID:                b.ID,
		BinNumber:         b.BinNumber,
		CurrentStreet:     b.CurrentStreet,
		City:              b.City,
		Zip:               b.Zip,
		Status:            b.Status,
		FillPercentage:    b.FillPercentage,
		Checked:           b.Checked,
		MoveRequested:     b.MoveRequested,
		Latitude:          b.Latitude,
		Longitude:         b.Longitude,
		CreatedByUserID:   b.CreatedByUserID,
		PlacementPhotoURL: b.PlacementPhotoURL,
	}

	if b.LastMoved != nil {
		t := time.Unix(*b.LastMoved, 0)
		iso := t.Format(time.RFC3339)
		resp.LastMovedIso = &iso
	}

	if b.LastChecked != nil {
		t := time.Unix(*b.LastChecked, 0)
		iso := t.Format(time.RFC3339)
		resp.LastCheckedIso = &iso
	}

	if b.LastCheckedAt != nil {
		t := time.Unix(*b.LastCheckedAt, 0)
		iso := t.Format(time.RFC3339)
		resp.LastCheckedAtIso = &iso
	}

	if b.RetiredAt != nil {
		t := time.Unix(*b.RetiredAt, 0)
		iso := t.Format(time.RFC3339)
		resp.RetiredAtIso = &iso
	}

	resp.RetiredByUserID = b.RetiredByUserID

	return resp
}
