package models

import (
	"database/sql"
	"time"
)

// ShiftStatus represents the current status of a shift
type ShiftStatus string

const (
	ShiftStatusInactive  ShiftStatus = "inactive"  // Deprecated - use ended/cancelled
	ShiftStatusReady     ShiftStatus = "ready"     // Route assigned, not started
	ShiftStatusActive    ShiftStatus = "active"    // Shift in progress
	ShiftStatusPaused    ShiftStatus = "paused"    // On break
	ShiftStatusEnded     ShiftStatus = "ended"     // Completed or manually ended
	ShiftStatusCancelled ShiftStatus = "cancelled" // Cancelled by manager
)

// Shift represents a driver's work shift
type Shift struct {
	ID                string      `json:"id" db:"id"`
	DriverID          string      `json:"driver_id" db:"driver_id"`
	RouteID           *string     `json:"route_id" db:"route_id"`
	Status            ShiftStatus `json:"status" db:"status"`
	StartTime         *int64      `json:"start_time" db:"start_time"`
	EndTime           *int64      `json:"end_time" db:"end_time"`
	TotalPauseSeconds int         `json:"total_pause_seconds" db:"total_pause_seconds"`
	PauseStartTime    *int64      `json:"pause_start_time" db:"pause_start_time"`
	TotalBins         int         `json:"total_bins" db:"total_bins"`
	CompletedBins     int         `json:"completed_bins" db:"completed_bins"`
	CreatedAt         int64       `json:"created_at" db:"created_at"`
	UpdatedAt         int64       `json:"updated_at" db:"updated_at"`
}

// FCMToken represents a Firebase Cloud Messaging token for a user
type FCMToken struct {
	ID         int    `json:"id" db:"id"`
	UserID     string `json:"user_id" db:"user_id"`
	Token      string `json:"token" db:"token"`
	DeviceType string `json:"device_type" db:"device_type"` // "ios" or "android"
	CreatedAt  int64  `json:"created_at" db:"created_at"`
	UpdatedAt  int64  `json:"updated_at" db:"updated_at"`
}

// GetActiveShiftDuration calculates the active duration excluding pauses
func (s *Shift) GetActiveShiftDuration() time.Duration {
	if s.StartTime == nil {
		return 0
	}

	now := time.Now().Unix()
	totalSeconds := now - *s.StartTime

	// Subtract pause time
	pauseSeconds := int64(s.TotalPauseSeconds)

	// If currently paused, add current pause duration
	if s.PauseStartTime != nil {
		pauseSeconds += now - *s.PauseStartTime
	}

	activeSeconds := totalSeconds - pauseSeconds
	if activeSeconds < 0 {
		activeSeconds = 0
	}

	return time.Duration(activeSeconds) * time.Second
}

// IsComplete returns true if all bins are completed
func (s *Shift) IsComplete() bool {
	return s.CompletedBins >= s.TotalBins
}

// GetCompletionPercentage returns completion as 0.0-1.0
func (s *Shift) GetCompletionPercentage() float64 {
	if s.TotalBins == 0 {
		return 0.0
	}
	return float64(s.CompletedBins) / float64(s.TotalBins)
}

// ShiftEndResponse contains details when shift ends
type ShiftEndResponse struct {
	Status               ShiftStatus `json:"status"`
	EndTime              int64       `json:"end_time"`
	TotalDurationSeconds int64       `json:"total_duration_seconds"`
	ActiveDurationSeconds int64       `json:"active_duration_seconds"`
	TotalPauseSeconds    int         `json:"total_pause_seconds"`
	CompletedBins        int         `json:"completed_bins"`
	TotalBins            int         `json:"total_bins"`
}

// CompleteBinResponse contains bin completion progress
type CompleteBinResponse struct {
	CompletedBins        int     `json:"completed_bins"`
	TotalBins            int     `json:"total_bins"`
	CompletionPercentage float64 `json:"completion_percentage"`
	CheckID              *int    `json:"check_id,omitempty"`     // ID of created check record (for linking incidents)
	IncidentID           *string `json:"incident_id,omitempty"`  // ID of created incident (if incident was reported)
}

// ToNullInt64 converts a pointer to int64 to sql.NullInt64
func ToNullInt64(i *int64) sql.NullInt64 {
	if i == nil {
		return sql.NullInt64{Valid: false}
	}
	return sql.NullInt64{Int64: *i, Valid: true}
}

// FromNullInt64 converts sql.NullInt64 to pointer to int64
func FromNullInt64(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

// ToNullString converts a pointer to string to sql.NullString
func ToNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: *s, Valid: true}
}

// FromNullString converts sql.NullString to pointer to string
func FromNullString(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}
