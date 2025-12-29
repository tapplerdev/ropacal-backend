package models

// Route represents a route blueprint/template
type Route struct {
	ID                     string   `json:"id" db:"id"`
	Name                   string   `json:"name" db:"name"`
	Description            *string  `json:"description,omitempty" db:"description"`
	GeographicArea         string   `json:"geographic_area" db:"geographic_area"`
	SchedulePattern        *string  `json:"schedule_pattern,omitempty" db:"schedule_pattern"`
	BinCount               int      `json:"bin_count" db:"bin_count"`
	EstimatedDurationHours *float64 `json:"estimated_duration_hours,omitempty" db:"estimated_duration_hours"`
	CreatedByUserID        *string  `json:"created_by_user_id,omitempty" db:"created_by_user_id"`
	CreatedAt              int64    `json:"created_at" db:"created_at"` // Unix timestamp
	UpdatedAt              int64    `json:"updated_at" db:"updated_at"` // Unix timestamp
}

// RouteBin represents a bin in a route blueprint (from route_bins table)
// This is for route templates/blueprints, NOT active shifts
type RouteBin struct {
	ID            int    `json:"id" db:"id"`
	RouteID       string `json:"route_id" db:"route_id"`
	BinID         string `json:"bin_id" db:"bin_id"`
	SequenceOrder int    `json:"sequence_order" db:"sequence_order"`
	CreatedAt     int64  `json:"created_at" db:"created_at"` // Unix timestamp
}

// Note: ShiftBin is defined in route_bin.go (for active shifts)

// RouteWithBins represents a route with its associated bins
type RouteWithBins struct {
	Route
	Bins []BinInRoute `json:"bins"`
}

// BinInRoute represents a bin with its sequence order in a route
type BinInRoute struct {
	BinResponse
	SequenceOrder int `json:"sequence_order"`
}

// CreateRouteRequest is the request body for POST /api/routes
type CreateRouteRequest struct {
	Name                   string   `json:"name"`
	Description            string   `json:"description"`
	GeographicArea         string   `json:"geographic_area"`
	SchedulePattern        string   `json:"schedule_pattern"`
	BinIDs                 []string `json:"bin_ids"`
	EstimatedDurationHours *float64 `json:"estimated_duration_hours,omitempty"`
}

// UpdateRouteRequest is the request body for PATCH /api/routes/:id
type UpdateRouteRequest struct {
	Name                   *string  `json:"name,omitempty"`
	Description            *string  `json:"description,omitempty"`
	GeographicArea         *string  `json:"geographic_area,omitempty"`
	SchedulePattern        *string  `json:"schedule_pattern,omitempty"`
	BinIDs                 []string `json:"bin_ids,omitempty"`
	EstimatedDurationHours *float64 `json:"estimated_duration_hours,omitempty"`
}

// DuplicateRouteRequest is the request body for POST /api/routes/:id/duplicate
type DuplicateRouteRequest struct {
	Name string `json:"name"`
}
