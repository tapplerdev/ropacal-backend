package models

import "time"

type Check struct {
	ID             int    `json:"id" db:"id"`
	BinID          string `json:"bin_id" db:"bin_id"`
	CheckedFrom    string `json:"checked_from" db:"checked_from"`
	FillPercentage int    `json:"fill_percentage" db:"fill_percentage"`
	CheckedOn      int64  `json:"checked_on" db:"checked_on"` // Unix timestamp
}

// CheckResponse is what we send to the client
type CheckResponse struct {
	ID             int    `json:"id"`
	BinID          string `json:"binId"`
	CheckedFrom    string `json:"checkedFrom"`
	FillPercentage int    `json:"fillPercentage"`
	CheckedOnIso   string `json:"checkedOnIso"`
	CheckedOn      string `json:"checkedOn"` // formatted date
}

// ToCheckResponse converts a Check to CheckResponse
func (c *Check) ToCheckResponse() CheckResponse {
	t := time.Unix(c.CheckedOn, 0)
	return CheckResponse{
		ID:             c.ID,
		BinID:          c.BinID,
		CheckedFrom:    c.CheckedFrom,
		FillPercentage: c.FillPercentage,
		CheckedOnIso:   t.Format(time.RFC3339),
		CheckedOn:      t.Format("Jan 02, 2006"),
	}
}
