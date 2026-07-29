package models

import "time"

// AppErrorLog represents an error/diagnostic log from the mobile app
type AppErrorLog struct {
	ID               string   `json:"id" db:"id"`
	OrganizationID   string   `json:"organization_id,omitempty" db:"organization_id"` // Owning tenant (multi-tenancy; column added by migrations/add_multi_tenancy_rls.sql)
	DriverID         *string  `json:"driver_id,omitempty" db:"driver_id"`
	ShiftID          *string  `json:"shift_id,omitempty" db:"shift_id"`
	TaskID           *string  `json:"task_id,omitempty" db:"task_id"`
	CreatedAt        int64    `json:"created_at" db:"created_at"`       // Server timestamp (Unix seconds)
	LogTimestamp     int64    `json:"log_timestamp" db:"log_timestamp"` // Client timestamp (Unix milliseconds)
	Context          string   `json:"context" db:"context"`             // "navigation", "gps", "sync", "map_load", etc.
	ErrorType        string   `json:"error_type" db:"error_type"`       // "invalid_waypoints", "gps_unavailable", etc.
	ErrorMessage     string   `json:"error_message" db:"error_message"`
	Severity         string   `json:"severity" db:"severity"` // "critical", "error", "warning", "info"
	Platform         string   `json:"platform" db:"platform"` // "ios", "android"
	AppVersion       *string  `json:"app_version,omitempty" db:"app_version"`
	OSVersion        *string  `json:"os_version,omitempty" db:"os_version"`
	DeviceInfo       *string  `json:"device_info,omitempty" db:"device_info"`
	LastGPSLatitude  *float64 `json:"last_gps_latitude,omitempty" db:"last_gps_latitude"`
	LastGPSLongitude *float64 `json:"last_gps_longitude,omitempty" db:"last_gps_longitude"`
	StackTrace       *string  `json:"stack_trace,omitempty" db:"stack_trace"`
	Metadata         *string  `json:"metadata,omitempty" db:"metadata"` // JSONB as string
	IsResolved       bool     `json:"is_resolved" db:"is_resolved"`
	ResolvedAt       *int64   `json:"resolved_at,omitempty" db:"resolved_at"`
	ResolvedByUserID *string  `json:"resolved_by_user_id,omitempty" db:"resolved_by_user_id"`
	Notes            *string  `json:"notes,omitempty" db:"notes"`
}

// AppErrorLogResponse is what we send to the client with ISO timestamps and additional info
type AppErrorLogResponse struct {
	ID                 string   `json:"id"`
	DriverID           *string  `json:"driver_id,omitempty"`
	DriverName         *string  `json:"driver_name,omitempty"` // Joined from users table
	ShiftID            *string  `json:"shift_id,omitempty"`
	TaskID             *string  `json:"task_id,omitempty"`
	CreatedAtISO       string   `json:"created_at_iso"`
	LogTimestampISO    string   `json:"log_timestamp_iso"`
	Context            string   `json:"context"`
	ErrorType          string   `json:"error_type"`
	ErrorMessage       string   `json:"error_message"`
	Severity           string   `json:"severity"`
	Platform           string   `json:"platform"`
	AppVersion         *string  `json:"app_version,omitempty"`
	OSVersion          *string  `json:"os_version,omitempty"`
	DeviceInfo         *string  `json:"device_info,omitempty"`
	LastGPSLatitude    *float64 `json:"last_gps_latitude,omitempty"`
	LastGPSLongitude   *float64 `json:"last_gps_longitude,omitempty"`
	StackTrace         *string  `json:"stack_trace,omitempty"`
	Metadata           *string  `json:"metadata,omitempty"`
	IsResolved         bool     `json:"is_resolved"`
	ResolvedAtISO      *string  `json:"resolved_at_iso,omitempty"`
	ResolvedByUserID   *string  `json:"resolved_by_user_id,omitempty"`
	ResolvedByUserName *string  `json:"resolved_by_user_name,omitempty"` // Joined from users table
	Notes              *string  `json:"notes,omitempty"`
}

// CreateAppErrorLogRequest is the request body for POST /api/logs/app-error
type CreateAppErrorLogRequest struct {
	DriverID         *string                `json:"driver_id,omitempty"`
	ShiftID          *string                `json:"shift_id,omitempty"`
	TaskID           *string                `json:"task_id,omitempty"`
	LogTimestamp     int64                  `json:"log_timestamp"` // Client timestamp (ms since epoch)
	Context          string                 `json:"context"`       // Required: "navigation", "gps", etc.
	ErrorType        string                 `json:"error_type"`    // Required: categorized error type
	ErrorMessage     string                 `json:"error_message"` // Required: error description
	Severity         string                 `json:"severity"`      // Required: "critical", "error", "warning", "info"
	Platform         string                 `json:"platform"`      // Required: "ios", "android"
	AppVersion       *string                `json:"app_version,omitempty"`
	OSVersion        *string                `json:"os_version,omitempty"`
	DeviceInfo       *string                `json:"device_info,omitempty"`
	LastGPSLatitude  *float64               `json:"last_gps_latitude,omitempty"`
	LastGPSLongitude *float64               `json:"last_gps_longitude,omitempty"`
	StackTrace       *string                `json:"stack_trace,omitempty"`
	Metadata         map[string]interface{} `json:"metadata,omitempty"` // Flexible field for additional context
}

// ToAppErrorLogResponse converts an AppErrorLog to AppErrorLogResponse
func (a *AppErrorLog) ToAppErrorLogResponse(driverName, resolvedByName *string) AppErrorLogResponse {
	resp := AppErrorLogResponse{
		ID:                 a.ID,
		DriverID:           a.DriverID,
		DriverName:         driverName,
		ShiftID:            a.ShiftID,
		TaskID:             a.TaskID,
		CreatedAtISO:       time.Unix(a.CreatedAt, 0).Format(time.RFC3339),
		LogTimestampISO:    time.Unix(a.LogTimestamp/1000, (a.LogTimestamp%1000)*1000000).Format(time.RFC3339),
		Context:            a.Context,
		ErrorType:          a.ErrorType,
		ErrorMessage:       a.ErrorMessage,
		Severity:           a.Severity,
		Platform:           a.Platform,
		AppVersion:         a.AppVersion,
		OSVersion:          a.OSVersion,
		DeviceInfo:         a.DeviceInfo,
		LastGPSLatitude:    a.LastGPSLatitude,
		LastGPSLongitude:   a.LastGPSLongitude,
		StackTrace:         a.StackTrace,
		Metadata:           a.Metadata,
		IsResolved:         a.IsResolved,
		ResolvedByUserID:   a.ResolvedByUserID,
		ResolvedByUserName: resolvedByName,
		Notes:              a.Notes,
	}

	if a.ResolvedAt != nil {
		iso := time.Unix(*a.ResolvedAt, 0).Format(time.RFC3339)
		resp.ResolvedAtISO = &iso
	}

	return resp
}
