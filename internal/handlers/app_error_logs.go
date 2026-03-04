package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
)

// LogAppError handles diagnostic/error logs from the mobile app
// POST /api/logs/app-error
func LogAppError(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateAppErrorLogRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.Context == "" || req.ErrorType == "" || req.ErrorMessage == "" ||
			req.Severity == "" || req.Platform == "" {
			http.Error(w, "Missing required fields: context, error_type, error_message, severity, platform", http.StatusBadRequest)
			return
		}

		// Validate severity
		validSeverities := map[string]bool{
			"critical": true,
			"error":    true,
			"warning":  true,
			"info":     true,
		}
		if !validSeverities[req.Severity] {
			http.Error(w, "Invalid severity. Must be: critical, error, warning, or info", http.StatusBadRequest)
			return
		}

		// Validate platform
		if req.Platform != "ios" && req.Platform != "android" {
			http.Error(w, "Invalid platform. Must be: ios or android", http.StatusBadRequest)
			return
		}

		// Convert metadata to JSON string if provided
		var metadataJSON *string
		if req.Metadata != nil && len(req.Metadata) > 0 {
			metadataBytes, err := json.Marshal(req.Metadata)
			if err != nil {
				http.Error(w, "Invalid metadata format", http.StatusBadRequest)
				return
			}
			metadataStr := string(metadataBytes)
			metadataJSON = &metadataStr
		}

		// Create error log entry
		errorLog := models.AppErrorLog{
			ID:               uuid.New().String(),
			DriverID:         req.DriverID,
			ShiftID:          req.ShiftID,
			TaskID:           req.TaskID,
			CreatedAt:        time.Now().Unix(),
			LogTimestamp:     req.LogTimestamp,
			Context:          req.Context,
			ErrorType:        req.ErrorType,
			ErrorMessage:     req.ErrorMessage,
			Severity:         req.Severity,
			Platform:         req.Platform,
			AppVersion:       req.AppVersion,
			OSVersion:        req.OSVersion,
			DeviceInfo:       req.DeviceInfo,
			LastGPSLatitude:  req.LastGPSLatitude,
			LastGPSLongitude: req.LastGPSLongitude,
			StackTrace:       req.StackTrace,
			Metadata:         metadataJSON,
			IsResolved:       false,
		}

		// Insert into database
		query := `
			INSERT INTO app_error_logs (
				id, driver_id, shift_id, task_id, created_at, log_timestamp,
				context, error_type, error_message, severity, platform,
				app_version, os_version, device_info,
				last_gps_latitude, last_gps_longitude,
				stack_trace, metadata, is_resolved
			)
			VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, $8, $9, $10, $11,
				$12, $13, $14,
				$15, $16,
				$17, $18, $19
			)
		`

		_, err := db.Exec(query,
			errorLog.ID, errorLog.DriverID, errorLog.ShiftID, errorLog.TaskID,
			errorLog.CreatedAt, errorLog.LogTimestamp,
			errorLog.Context, errorLog.ErrorType, errorLog.ErrorMessage,
			errorLog.Severity, errorLog.Platform,
			errorLog.AppVersion, errorLog.OSVersion, errorLog.DeviceInfo,
			errorLog.LastGPSLatitude, errorLog.LastGPSLongitude,
			errorLog.StackTrace, errorLog.Metadata, errorLog.IsResolved,
		)

		if err != nil {
			log.Printf("Failed to insert app error log: %v", err)
			http.Error(w, "Failed to save error log", http.StatusInternalServerError)
			return
		}

		// Log to console with color coding for immediate visibility
		prefix := "📱"
		switch errorLog.Severity {
		case "critical":
			prefix = "🔴"
		case "error":
			prefix = "🟠"
		case "warning":
			prefix = "🟡"
		case "info":
			prefix = "🔵"
		}

		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("%s MOBILE APP ERROR [%s]", prefix, errorLog.Severity)
		log.Printf("   Platform:    %s", errorLog.Platform)
		log.Printf("   Context:     %s", errorLog.Context)
		log.Printf("   Error Type:  %s", errorLog.ErrorType)
		log.Printf("   Message:     %s", errorLog.ErrorMessage)
		if errorLog.DriverID != nil {
			log.Printf("   Driver ID:   %s", *errorLog.DriverID)
		}
		if errorLog.ShiftID != nil {
			log.Printf("   Shift ID:    %s", *errorLog.ShiftID)
		}
		if errorLog.LastGPSLatitude != nil && errorLog.LastGPSLongitude != nil {
			log.Printf("   Last GPS:    %f, %f", *errorLog.LastGPSLatitude, *errorLog.LastGPSLongitude)
		}
		if errorLog.AppVersion != nil {
			log.Printf("   App Version: %s", *errorLog.AppVersion)
		}
		log.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Return success response
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "logged",
			"id":     errorLog.ID,
		})
	}
}

// GetAppErrorLogs retrieves error logs with filtering and pagination
// GET /manager/logs/app-errors
func GetAppErrorLogs(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse query parameters
		driverID := r.URL.Query().Get("driver_id")
		shiftID := r.URL.Query().Get("shift_id")
		context := r.URL.Query().Get("context")
		severity := r.URL.Query().Get("severity")
		platform := r.URL.Query().Get("platform")
		resolvedParam := r.URL.Query().Get("is_resolved")
		limit := r.URL.Query().Get("limit")

		// Build query dynamically
		query := `
			SELECT
				el.*,
				u.name as driver_name,
				ru.name as resolved_by_user_name
			FROM app_error_logs el
			LEFT JOIN users u ON el.driver_id = u.id
			LEFT JOIN users ru ON el.resolved_by_user_id = ru.id
			WHERE 1=1
		`
		args := []interface{}{}
		argCount := 0

		if driverID != "" {
			argCount++
			query += fmt.Sprintf(" AND el.driver_id = $%d", argCount)
			args = append(args, driverID)
		}

		if shiftID != "" {
			argCount++
			query += fmt.Sprintf(" AND el.shift_id = $%d", argCount)
			args = append(args, shiftID)
		}

		if context != "" {
			argCount++
			query += fmt.Sprintf(" AND el.context = $%d", argCount)
			args = append(args, context)
		}

		if severity != "" {
			argCount++
			query += fmt.Sprintf(" AND el.severity = $%d", argCount)
			args = append(args, severity)
		}

		if platform != "" {
			argCount++
			query += fmt.Sprintf(" AND el.platform = $%d", argCount)
			args = append(args, platform)
		}

		if resolvedParam == "true" {
			query += " AND el.is_resolved = true"
		} else if resolvedParam == "false" {
			query += " AND el.is_resolved = false"
		}

		query += " ORDER BY el.created_at DESC"

		if limit != "" {
			argCount++
			query += fmt.Sprintf(" LIMIT $%d", argCount)
			args = append(args, limit)
		} else {
			query += " LIMIT 100" // Default limit
		}

		// Execute query
		rows, err := db.Queryx(query, args...)
		if err != nil {
			log.Printf("Failed to fetch app error logs: %v", err)
			http.Error(w, "Failed to fetch error logs", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		errorLogs := []models.AppErrorLogResponse{}
		for rows.Next() {
			var errorLog models.AppErrorLog
			var driverName, resolvedByName *string

			err := rows.Scan(
				&errorLog.ID, &errorLog.DriverID, &errorLog.ShiftID, &errorLog.TaskID,
				&errorLog.CreatedAt, &errorLog.LogTimestamp,
				&errorLog.Context, &errorLog.ErrorType, &errorLog.ErrorMessage,
				&errorLog.Severity, &errorLog.Platform,
				&errorLog.AppVersion, &errorLog.OSVersion, &errorLog.DeviceInfo,
				&errorLog.LastGPSLatitude, &errorLog.LastGPSLongitude,
				&errorLog.StackTrace, &errorLog.Metadata,
				&errorLog.IsResolved, &errorLog.ResolvedAt, &errorLog.ResolvedByUserID, &errorLog.Notes,
				&driverName, &resolvedByName,
			)
			if err != nil {
				log.Printf("Failed to scan app error log: %v", err)
				continue
			}

			errorLogs = append(errorLogs, errorLog.ToAppErrorLogResponse(driverName, resolvedByName))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(errorLogs)
	}
}

// ResolveAppErrorLog marks an error log as resolved
// PATCH /manager/logs/app-errors/:id/resolve
func ResolveAppErrorLog(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		errorLogID := chi.URLParam(r, "id")
		if errorLogID == "" {
			http.Error(w, "Error log ID required", http.StatusBadRequest)
			return
		}

		// Get user ID from context (set by auth middleware)
		userClaims, ok := r.Context().Value("user").(*middleware.UserClaims)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Parse request body for notes
		var reqBody struct {
			Notes *string `json:"notes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			// Notes are optional, so ignore decode errors for empty body
		}

		// Update error log
		nowUnix := time.Now().Unix()
		query := `
			UPDATE app_error_logs
			SET is_resolved = true,
				resolved_at = $1,
				resolved_by_user_id = $2,
				notes = COALESCE($3, notes)
			WHERE id = $4
		`

		result, err := db.Exec(query, nowUnix, userClaims.UserID, reqBody.Notes, errorLogID)
		if err != nil {
			log.Printf("Failed to resolve app error log: %v", err)
			http.Error(w, "Failed to resolve error log", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			http.Error(w, "Error log not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "resolved",
			"id":     errorLogID,
		})
	}
}

// GetAppErrorStats retrieves error statistics for dashboard
// GET /manager/logs/app-error-stats
func GetAppErrorStats(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get stats for last 24 hours, 7 days, and 30 days
		query := `
			SELECT
				COUNT(*) FILTER (WHERE created_at >= EXTRACT(EPOCH FROM NOW() - INTERVAL '24 hours')::BIGINT) as last_24h,
				COUNT(*) FILTER (WHERE created_at >= EXTRACT(EPOCH FROM NOW() - INTERVAL '7 days')::BIGINT) as last_7d,
				COUNT(*) FILTER (WHERE created_at >= EXTRACT(EPOCH FROM NOW() - INTERVAL '30 days')::BIGINT) as last_30d,
				COUNT(*) FILTER (WHERE severity = 'critical' AND is_resolved = false) as critical_unresolved,
				COUNT(*) FILTER (WHERE severity = 'error' AND is_resolved = false) as error_unresolved,
				COUNT(*) FILTER (WHERE context = 'navigation' AND is_resolved = false) as navigation_errors,
				COUNT(*) FILTER (WHERE is_resolved = false) as total_unresolved
			FROM app_error_logs
		`

		var stats struct {
			Last24h            int `json:"last_24h" db:"last_24h"`
			Last7d             int `json:"last_7d" db:"last_7d"`
			Last30d            int `json:"last_30d" db:"last_30d"`
			CriticalUnresolved int `json:"critical_unresolved" db:"critical_unresolved"`
			ErrorUnresolved    int `json:"error_unresolved" db:"error_unresolved"`
			NavigationErrors   int `json:"navigation_errors" db:"navigation_errors"`
			TotalUnresolved    int `json:"total_unresolved" db:"total_unresolved"`
		}

		err := db.Get(&stats, query)
		if err != nil {
			log.Printf("Failed to fetch app error stats: %v", err)
			http.Error(w, "Failed to fetch error stats", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
	}
}
