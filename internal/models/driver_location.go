package models

// DriverLocation represents a GPS location update from a driver
type DriverLocation struct {
	ID        int      `json:"id" db:"id"`
	DriverID  string   `json:"driver_id" db:"driver_id"`
	Latitude  float64  `json:"latitude" db:"latitude"`
	Longitude float64  `json:"longitude" db:"longitude"`
	Heading   *float64 `json:"heading,omitempty" db:"heading"`       // Direction of travel (0-360 degrees)
	Speed     *float64 `json:"speed,omitempty" db:"speed"`           // Speed in m/s
	Accuracy  *float64 `json:"accuracy,omitempty" db:"accuracy"`     // GPS accuracy in meters
	ShiftID   *string  `json:"shift_id,omitempty" db:"shift_id"`     // Associated shift
	Timestamp int64    `json:"timestamp" db:"timestamp"`             // Client-side timestamp
	CreatedAt int64    `json:"created_at" db:"created_at"`           // Server-side timestamp
}

// DriverStatus represents a driver's current state for manager dashboard
type DriverStatus struct {
	DriverID     string          `json:"driver_id"`
	Name         string          `json:"name"`
	Status       ShiftStatus     `json:"status"` // "active", "paused", "ready", etc.
	ShiftID      *string         `json:"shift_id,omitempty"`
	CurrentBin   int             `json:"current_bin,omitempty"`
	TotalBins    int             `json:"total_bins,omitempty"`
	LastLocation *DriverLocation `json:"last_location,omitempty"`
}
