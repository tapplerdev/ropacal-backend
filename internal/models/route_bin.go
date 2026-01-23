package models

// ShiftBin represents a bin assigned to an active shift (from shift_bins table)
// Note: This was formerly called RouteBin, but renamed for clarity
type ShiftBin struct {
	ID            int    `db:"id" json:"id"`
	ShiftID       string `db:"shift_id" json:"shift_id"`
	BinID         string `db:"bin_id" json:"bin_id"`
	SequenceOrder int    `db:"sequence_order" json:"sequence_order"`
	IsCompleted   int    `db:"is_completed" json:"is_completed"` // SQLite uses INTEGER for boolean
	CompletedAt   *int64 `db:"completed_at" json:"completed_at"`
	CreatedAt     int64  `db:"created_at" json:"created_at"`
}

// ShiftBinWithDetails extends ShiftBin with bin details for API responses
type ShiftBinWithDetails struct {
	ID                    int     `db:"id" json:"id"`
	ShiftID               string  `db:"shift_id" json:"shift_id"`
	BinID                 string  `db:"bin_id" json:"bin_id"`
	SequenceOrder         int     `db:"sequence_order" json:"sequence_order"`
	IsCompleted           int     `db:"is_completed" json:"is_completed"`
	CompletedAt           *int64  `db:"completed_at" json:"completed_at"`
	UpdatedFillPercentage *int    `db:"updated_fill_percentage" json:"updated_fill_percentage"`
	CreatedAt             int64   `db:"created_at" json:"created_at"`
	BinNumber             int      `db:"bin_number" json:"bin_number"`
	CurrentStreet         string   `db:"current_street" json:"current_street"`
	City                  string   `db:"city" json:"city"`
	Zip                   string   `db:"zip" json:"zip"`
	FillPercentage        int      `db:"fill_percentage" json:"fill_percentage"`
	Latitude              float64  `db:"latitude" json:"latitude"`
	Longitude             float64  `db:"longitude" json:"longitude"`
}
