package models

import "time"

// PotentialLocation represents a driver-requested location for a future bin
type PotentialLocation struct {
	ID                string   `json:"id" db:"id"`
	Address           string   `json:"address" db:"address"`
	Street            string   `json:"street" db:"street"`
	City              string   `json:"city" db:"city"`
	Zip               string   `json:"zip" db:"zip"`
	Latitude          *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude         *float64 `json:"longitude,omitempty" db:"longitude"`
	RequestedByUserID string   `json:"requested_by_user_id" db:"requested_by_user_id"`
	RequestedByName   string   `json:"requested_by_name" db:"requested_by_name"`
	Notes             *string  `json:"notes,omitempty" db:"notes"`
	CreatedAt         int64    `json:"created_at" db:"created_at"`
	UpdatedAt         int64    `json:"updated_at" db:"updated_at"`
	ConvertedToBinID  *string  `json:"converted_to_bin_id,omitempty" db:"converted_to_bin_id"`
	ConvertedAt       *int64   `json:"converted_at,omitempty" db:"converted_at"`
	ConvertedByUserID *string  `json:"converted_by_user_id,omitempty" db:"converted_by_user_id"`
}

// PotentialLocationResponse is what we send to the client with ISO timestamps
type PotentialLocationResponse struct {
	ID                string   `json:"id"`
	Address           string   `json:"address"`
	Street            string   `json:"street"`
	City              string   `json:"city"`
	Zip               string   `json:"zip"`
	Latitude          *float64 `json:"latitude,omitempty"`
	Longitude         *float64 `json:"longitude,omitempty"`
	RequestedByUserID string   `json:"requested_by_user_id"`
	RequestedByName   string   `json:"requested_by_name"`
	Notes             *string  `json:"notes,omitempty"`
	CreatedAtIso      string   `json:"created_at_iso"`
	ConvertedToBinID  *string  `json:"converted_to_bin_id,omitempty"`
	ConvertedAtIso    *string  `json:"converted_at_iso,omitempty"`
	ConvertedByUserID *string  `json:"converted_by_user_id,omitempty"`
	BinNumber         *int     `json:"bin_number,omitempty"` // From JOIN with bins table
}

// CreatePotentialLocationRequest is the request body for POST /api/potential-locations
type CreatePotentialLocationRequest struct {
	Street    string   `json:"street"`
	City      string   `json:"city"`
	Zip       string   `json:"zip"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Notes     *string  `json:"notes,omitempty"`
}

// ConvertToBinRequest is the request body for POST /api/potential-locations/:id/convert
type ConvertToBinRequest struct {
	FillPercentage *int `json:"fill_percentage,omitempty"`
}

// ToPotentialLocationResponse converts a PotentialLocation to PotentialLocationResponse
func (pl *PotentialLocation) ToPotentialLocationResponse() PotentialLocationResponse {
	resp := PotentialLocationResponse{
		ID:                pl.ID,
		Address:           pl.Address,
		Street:            pl.Street,
		City:              pl.City,
		Zip:               pl.Zip,
		Latitude:          pl.Latitude,
		Longitude:         pl.Longitude,
		RequestedByUserID: pl.RequestedByUserID,
		RequestedByName:   pl.RequestedByName,
		Notes:             pl.Notes,
		CreatedAtIso:      time.Unix(pl.CreatedAt, 0).Format(time.RFC3339),
		ConvertedToBinID:  pl.ConvertedToBinID,
		ConvertedByUserID: pl.ConvertedByUserID,
	}

	if pl.ConvertedAt != nil {
		t := time.Unix(*pl.ConvertedAt, 0)
		iso := t.Format(time.RFC3339)
		resp.ConvertedAtIso = &iso
	}

	return resp
}
