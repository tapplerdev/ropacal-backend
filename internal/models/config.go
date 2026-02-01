package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"
)

// Config represents an application configuration setting
type Config struct {
	ID        int             `json:"id"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
	UpdatedBy *string         `json:"updated_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// WarehouseLocation represents warehouse coordinates and address
type WarehouseLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Address   string  `json:"address"`
}

// Value implements the driver.Valuer interface for WarehouseLocation
func (w WarehouseLocation) Value() (driver.Value, error) {
	return json.Marshal(w)
}

// Scan implements the sql.Scanner interface for WarehouseLocation
func (w *WarehouseLocation) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, w)
}
