package models

import "time"

type Bin struct {
	ID             string   `json:"id" db:"id"`
	BinNumber      int      `json:"bin_number" db:"bin_number"`
	CurrentStreet  string   `json:"current_street" db:"current_street"`
	City           string   `json:"city" db:"city"`
	Zip            string   `json:"zip" db:"zip"`
	LastMoved      *int64   `json:"last_moved,omitempty" db:"last_moved"`       // Unix timestamp
	LastChecked    *int64   `json:"last_checked,omitempty" db:"last_checked"`   // Unix timestamp
	Status         string   `json:"status" db:"status"`
	FillPercentage *int     `json:"fill_percentage,omitempty" db:"fill_percentage"`
	Checked        bool     `json:"checked" db:"checked"`
	MoveRequested  bool     `json:"move_requested" db:"move_requested"`
	Latitude       *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude      *float64 `json:"longitude,omitempty" db:"longitude"`
	CreatedAt      int64    `json:"created_at" db:"created_at"`                 // Unix timestamp
	UpdatedAt      int64    `json:"updated_at" db:"updated_at"`                 // Unix timestamp
}

// BinResponse is what we send to the client with ISO timestamps
type BinResponse struct {
	ID              string   `json:"id"`
	BinNumber       int      `json:"bin_number"`
	CurrentStreet   string   `json:"current_street"`
	City            string   `json:"city"`
	Zip             string   `json:"zip"`
	LastMovedIso    *string  `json:"lastMovedIso,omitempty"`
	LastCheckedIso  *string  `json:"lastCheckedIso,omitempty"`
	Status          string   `json:"status"`
	FillPercentage  *int     `json:"fill_percentage,omitempty"`
	Checked         bool     `json:"checked"`
	MoveRequested   bool     `json:"move_requested"`
	Latitude        *float64 `json:"latitude,omitempty"`
	Longitude       *float64 `json:"longitude,omitempty"`
}

// UpdateBinRequest is the request body for PATCH /api/bins/:id
type UpdateBinRequest struct {
	CurrentStreet  string  `json:"current_street"`
	City           string  `json:"city"`
	Zip            string  `json:"zip"`
	Status         string  `json:"status"`
	Checked        bool    `json:"checked"`
	FillPercentage *int    `json:"fill_percentage,omitempty"`
	MoveRequested  bool    `json:"move_requested"`
	CheckedFrom    *string `json:"checkedFrom,omitempty"`
	CheckedOnIso   *string `json:"checkedOnIso,omitempty"`
	PhotoUrl       *string `json:"photoUrl,omitempty"` // Optional photo URL from Cloudinary
}

// CreateBinRequest is the request body for POST /api/bins
type CreateBinRequest struct {
	BinNumber      int      `json:"bin_number"`
	CurrentStreet  string   `json:"current_street"`
	City           string   `json:"city"`
	Zip            string   `json:"zip"`
	Status         string   `json:"status"`
	FillPercentage *int     `json:"fill_percentage,omitempty"`
	Latitude       *float64 `json:"latitude,omitempty"`
	Longitude      *float64 `json:"longitude,omitempty"`
}

// ToBinResponse converts a Bin to BinResponse
func (b *Bin) ToBinResponse() BinResponse {
	resp := BinResponse{
		ID:             b.ID,
		BinNumber:      b.BinNumber,
		CurrentStreet:  b.CurrentStreet,
		City:           b.City,
		Zip:            b.Zip,
		Status:         b.Status,
		FillPercentage: b.FillPercentage,
		Checked:        b.Checked,
		MoveRequested:  b.MoveRequested,
		Latitude:       b.Latitude,
		Longitude:      b.Longitude,
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

	return resp
}
