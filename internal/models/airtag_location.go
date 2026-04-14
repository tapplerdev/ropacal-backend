package models

// AirtagLocation represents an AirTag's most recent position, written by the FindMy bridge.
type AirtagLocation struct {
	ID            string  `json:"id" db:"id"`
	BinNumber     *int    `json:"bin_number" db:"bin_number"`
	Name          string  `json:"name" db:"name"`
	Latitude      float64 `json:"latitude" db:"latitude"`
	Longitude     float64 `json:"longitude" db:"longitude"`
	Address       string  `json:"address" db:"address"`
	City          string  `json:"city" db:"city"`
	LastSeen      string  `json:"last_seen" db:"last_seen"`
	BatteryStatus int     `json:"battery_status" db:"battery_status"`
	IsMatched     bool    `json:"-" db:"is_matched"`
	UpdatedAt     int64   `json:"-" db:"updated_at"`
}
