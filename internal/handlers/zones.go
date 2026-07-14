package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// NoGoZoneResponse represents a no-go zone with ISO timestamps for frontend
type NoGoZoneResponse struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	CenterLatitude   float64 `json:"center_latitude"`
	CenterLongitude  float64 `json:"center_longitude"`
	RadiusMeters     int     `json:"radius_meters"`
	ConflictScore    int     `json:"conflict_score"`
	Status           string  `json:"status"`
	CreatedByUserID  *string `json:"created_by_user_id,omitempty"`
	CreatedAtISO     string  `json:"created_at_iso"`
	UpdatedAtISO     string  `json:"updated_at_iso"`
	ResolvedByUserID *string `json:"resolved_by_user_id,omitempty"`
	ResolvedAtISO    *string `json:"resolved_at_iso,omitempty"`
	ResolutionNotes  *string `json:"resolution_notes,omitempty"`
	// Zone merge fields
	MergedIntoZoneID *string `json:"merged_into_zone_id,omitempty"` // If this zone was merged into another
	ResolutionType   *string `json:"resolution_type,omitempty"`     // 'merged' or 'manual_resolution'
	MergedZoneCount  int     `json:"merged_zone_count,omitempty"`   // Count of zones that were merged into this one
}

// ZoneIncidentResponse represents an incident with ISO timestamps
type ZoneIncidentResponse struct {
	ID                 string   `json:"id"`
	ZoneID             string   `json:"zone_id"`
	BinID              *string  `json:"bin_id,omitempty"` // nil for address-only manager reports
	BinNumber          *int     `json:"bin_number,omitempty"`
	IncidentType       string   `json:"incident_type"`
	ReportedByUserID   *string  `json:"reported_by_user_id,omitempty"`
	ReportedAtISO      string   `json:"reported_at_iso"`
	Description        *string  `json:"description,omitempty"`
	PhotoURL           *string  `json:"photo_url,omitempty"`
	CheckID            *int     `json:"check_id,omitempty"`
	MoveID             *int     `json:"move_id,omitempty"`
	ShiftID            *string  `json:"shift_id,omitempty"`
	ReporterLatitude   *float64 `json:"reporter_latitude,omitempty"`
	ReporterLongitude  *float64 `json:"reporter_longitude,omitempty"`
	IsFieldObservation bool     `json:"is_field_observation"`
	VerifiedByUserID   *string  `json:"verified_by_user_id,omitempty"`
	VerifiedAtISO      *string  `json:"verified_at_iso,omitempty"`
	Status             string   `json:"status"`
	Source             *string  `json:"source,omitempty"`          // 'driver_shift', 'manager_report', 'admin_bin_change', 'move_request'
	MoveRequestID      *string  `json:"move_request_id,omitempty"` // Links back to the move request that triggered this incident
}

// GetNoGoZones returns all no-go zones (optionally filtered by status)
func GetNoGoZones(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/no-go-zones")

		status := r.URL.Query().Get("status")
		includeMerged := r.URL.Query().Get("include_merged") == "true"

		var zones []struct {
			ID               string  `db:"id"`
			Name             string  `db:"name"`
			CenterLatitude   float64 `db:"center_latitude"`
			CenterLongitude  float64 `db:"center_longitude"`
			RadiusMeters     int     `db:"radius_meters"`
			ConflictScore    int     `db:"conflict_score"`
			Status           string  `db:"status"`
			CreatedByUserID  *string `db:"created_by_user_id"`
			CreatedAt        int64   `db:"created_at"`
			UpdatedAt        int64   `db:"updated_at"`
			ResolvedByUserID *string `db:"resolved_by_user_id"`
			ResolvedAt       *int64  `db:"resolved_at"`
			ResolutionNotes  *string `db:"resolution_notes"`
			MergedIntoZoneID *string `db:"merged_into_zone_id"`
			ResolutionType   *string `db:"resolution_type"`
		}

		// Build query with merge filter
		query := "SELECT * FROM no_go_zones"
		whereClause := []string{}
		args := []interface{}{}
		argIndex := 1

		// By default, exclude merged zones unless explicitly requested
		if !includeMerged {
			whereClause = append(whereClause, "(merged_into_zone_id IS NULL OR status != 'resolved')")
		}

		// Apply status filter if provided
		if status != "" {
			whereClause = append(whereClause, fmt.Sprintf("status = $%d", argIndex))
			args = append(args, status)
			argIndex++
		}

		if len(whereClause) > 0 {
			query += " WHERE " + strings.Join(whereClause, " AND ")
		}

		query += " ORDER BY updated_at DESC"

		if err := db.Select(&zones, query, args...); err != nil {
			log.Printf("❌ Error fetching zones: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch zones")
			return
		}

		// Convert to response format with ISO timestamps
		response := make([]NoGoZoneResponse, len(zones))
		for i, zone := range zones {
			// Count zones that were merged into this zone
			mergedCount := 0
			if err := db.Get(&mergedCount, "SELECT COUNT(*) FROM no_go_zones WHERE merged_into_zone_id = $1", zone.ID); err != nil {
				log.Printf("⚠️ Error counting merged zones for %s: %v", zone.ID, err)
			}

			response[i] = NoGoZoneResponse{
				ID:               zone.ID,
				Name:             zone.Name,
				CenterLatitude:   zone.CenterLatitude,
				CenterLongitude:  zone.CenterLongitude,
				RadiusMeters:     zone.RadiusMeters,
				ConflictScore:    zone.ConflictScore,
				Status:           zone.Status,
				CreatedByUserID:  zone.CreatedByUserID,
				CreatedAtISO:     time.Unix(zone.CreatedAt, 0).Format(time.RFC3339),
				UpdatedAtISO:     time.Unix(zone.UpdatedAt, 0).Format(time.RFC3339),
				ResolvedByUserID: zone.ResolvedByUserID,
				ResolutionNotes:  zone.ResolutionNotes,
				MergedIntoZoneID: zone.MergedIntoZoneID,
				ResolutionType:   zone.ResolutionType,
				MergedZoneCount:  mergedCount,
			}

			if zone.ResolvedAt != nil {
				resolvedISO := time.Unix(*zone.ResolvedAt, 0).Format(time.RFC3339)
				response[i].ResolvedAtISO = &resolvedISO
			}
		}

		log.Printf("✅ Found %d zones (status: '%s', include_merged: %v)", len(response), status, includeMerged)
		utils.RespondJSON(w, http.StatusOK, response)
	}
}

// GetNoGoZone returns a single zone by ID
func GetNoGoZone(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := r.PathValue("id")
		log.Printf("📥 REQUEST: GET /api/no-go-zones/%s", zoneID)

		var zone struct {
			ID               string  `db:"id"`
			Name             string  `db:"name"`
			CenterLatitude   float64 `db:"center_latitude"`
			CenterLongitude  float64 `db:"center_longitude"`
			RadiusMeters     int     `db:"radius_meters"`
			ConflictScore    int     `db:"conflict_score"`
			Status           string  `db:"status"`
			CreatedByUserID  *string `db:"created_by_user_id"`
			CreatedAt        int64   `db:"created_at"`
			UpdatedAt        int64   `db:"updated_at"`
			ResolvedByUserID *string `db:"resolved_by_user_id"`
			ResolvedAt       *int64  `db:"resolved_at"`
			ResolutionNotes  *string `db:"resolution_notes"`
			MergedIntoZoneID *string `db:"merged_into_zone_id"`
			ResolutionType   *string `db:"resolution_type"`
		}

		if err := db.Get(&zone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID); err != nil {
			log.Printf("❌ Zone not found: %v", err)
			utils.RespondError(w, http.StatusNotFound, "Zone not found")
			return
		}

		// Count zones that were merged into this zone
		mergedCount := 0
		if err := db.Get(&mergedCount, "SELECT COUNT(*) FROM no_go_zones WHERE merged_into_zone_id = $1", zone.ID); err != nil {
			log.Printf("⚠️ Error counting merged zones for %s: %v", zone.ID, err)
		}

		response := NoGoZoneResponse{
			ID:               zone.ID,
			Name:             zone.Name,
			CenterLatitude:   zone.CenterLatitude,
			CenterLongitude:  zone.CenterLongitude,
			RadiusMeters:     zone.RadiusMeters,
			ConflictScore:    zone.ConflictScore,
			Status:           zone.Status,
			CreatedByUserID:  zone.CreatedByUserID,
			CreatedAtISO:     time.Unix(zone.CreatedAt, 0).Format(time.RFC3339),
			UpdatedAtISO:     time.Unix(zone.UpdatedAt, 0).Format(time.RFC3339),
			ResolvedByUserID: zone.ResolvedByUserID,
			ResolutionNotes:  zone.ResolutionNotes,
			MergedIntoZoneID: zone.MergedIntoZoneID,
			ResolutionType:   zone.ResolutionType,
			MergedZoneCount:  mergedCount,
		}

		if zone.ResolvedAt != nil {
			resolvedISO := time.Unix(*zone.ResolvedAt, 0).Format(time.RFC3339)
			response.ResolvedAtISO = &resolvedISO
		}

		log.Printf("✅ Zone found: %s", zone.Name)
		utils.RespondJSON(w, http.StatusOK, response)
	}
}

// GetZoneIncidents returns all incidents for a specific zone
// Supports ?include_merged=true to include incidents from zones that were merged into this one
func GetZoneIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := r.PathValue("id")
		includeMerged := r.URL.Query().Get("include_merged") == "true"
		log.Printf("📥 REQUEST: GET /api/no-go-zones/%s/incidents (include_merged: %v)", zoneID, includeMerged)

		var incidents []struct {
			ID                 string   `db:"id"`
			ZoneID             string   `db:"zone_id"`
			BinID              *string  `db:"bin_id"`
			BinNumber          *int     `db:"bin_number"`
			IncidentType       string   `db:"incident_type"`
			ReportedByUserID   *string  `db:"reported_by_user_id"`
			ReportedByName     *string  `db:"reported_by_name"`
			ReportedAt         int64    `db:"reported_at"`
			Description        *string  `db:"description"`
			PhotoURL           *string  `db:"photo_url"`
			CheckID            *int     `db:"check_id"`
			MoveID             *int     `db:"move_id"`
			ShiftID            *string  `db:"shift_id"`
			ReporterLatitude   *float64 `db:"reporter_latitude"`
			ReporterLongitude  *float64 `db:"reporter_longitude"`
			IsFieldObservation bool     `db:"is_field_observation"`
			VerifiedByUserID   *string  `db:"verified_by_user_id"`
			VerifiedByName     *string  `db:"verified_by_name"`
			VerifiedAt         *int64   `db:"verified_at"`
			Status             string   `db:"status"`
			Source             *string  `db:"source"`
			MoveRequestID      *string  `db:"move_request_id"`
		}

		query := `
			SELECT zi.id, zi.zone_id, zi.bin_id, zi.incident_type,
			       zi.reported_by_user_id, zi.reported_at,
			       zi.description, zi.photo_url,
			       zi.check_id, zi.move_id, zi.shift_id,
			       zi.reporter_latitude, zi.reporter_longitude,
			       zi.is_field_observation,
			       zi.verified_by_user_id, zi.verified_at,
			       zi.status, zi.source, zi.move_request_id,
			       b.bin_number,
			       u1.name AS reported_by_name,
			       u2.name AS verified_by_name
			FROM zone_incidents zi
			LEFT JOIN bins b ON zi.bin_id = b.id
			LEFT JOIN users u1 ON zi.reported_by_user_id = u1.id
			LEFT JOIN users u2 ON zi.verified_by_user_id = u2.id
			WHERE zi.zone_id = $1
			ORDER BY zi.reported_at DESC
		`

		if err := db.Select(&incidents, query, zoneID); err != nil {
			log.Printf("❌ Error fetching incidents: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch incidents")
			return
		}

		// Convert to response format with ISO timestamps
		response := make([]models.ZoneIncidentResponse, len(incidents))
		for i, incident := range incidents {
			response[i] = models.ZoneIncidentResponse{
				ID:                 incident.ID,
				ZoneID:             incident.ZoneID,
				BinID:              incident.BinID,
				BinNumber:          incident.BinNumber,
				IncidentType:       incident.IncidentType,
				ReportedByUserID:   incident.ReportedByUserID,
				ReportedByName:     incident.ReportedByName,
				ReportedAtIso:      time.Unix(incident.ReportedAt, 0).Format(time.RFC3339),
				Description:        incident.Description,
				PhotoURL:           incident.PhotoURL,
				CheckID:            incident.CheckID,
				MoveID:             incident.MoveID,
				ShiftID:            incident.ShiftID,
				ReporterLatitude:   incident.ReporterLatitude,
				ReporterLongitude:  incident.ReporterLongitude,
				IsFieldObservation: incident.IsFieldObservation,
				VerifiedByUserID:   incident.VerifiedByUserID,
				VerifiedByName:     incident.VerifiedByName,
				Status:             incident.Status,
				Source:             incident.Source,
				MoveRequestID:      incident.MoveRequestID,
			}

			if incident.VerifiedAt != nil {
				verifiedISO := time.Unix(*incident.VerifiedAt, 0).Format(time.RFC3339)
				response[i].VerifiedAtIso = &verifiedISO
			}
		}

		log.Printf("✅ Found %d incidents for zone %s", len(response), zoneID)
		utils.RespondJSON(w, http.StatusOK, response)
	}
}

// GetShiftIncidents returns all incidents reported during a specific shift
func GetShiftIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := r.PathValue("id")
		log.Printf("📥 REQUEST: GET /api/shifts/%s/incidents", shiftID)

		var incidents []struct {
			ID                 string   `db:"id"`
			ZoneID             string   `db:"zone_id"`
			BinID              *string  `db:"bin_id"`
			BinNumber          *int     `db:"bin_number"`
			IncidentType       string   `db:"incident_type"`
			ReportedByUserID   *string  `db:"reported_by_user_id"`
			ReportedAt         int64    `db:"reported_at"`
			Description        *string  `db:"description"`
			PhotoURL           *string  `db:"photo_url"`
			CheckID            *int     `db:"check_id"`
			MoveID             *int     `db:"move_id"`
			ShiftID            *string  `db:"shift_id"`
			ReporterLatitude   *float64 `db:"reporter_latitude"`
			ReporterLongitude  *float64 `db:"reporter_longitude"`
			IsFieldObservation bool     `db:"is_field_observation"`
			VerifiedByUserID   *string  `db:"verified_by_user_id"`
			VerifiedAt         *int64   `db:"verified_at"`
			Status             string   `db:"status"`
		}

		query := `
			SELECT zi.id, zi.zone_id, zi.bin_id, zi.incident_type,
			       zi.reported_by_user_id, zi.reported_at,
			       zi.description, zi.photo_url,
			       zi.check_id, zi.move_id, zi.shift_id,
			       zi.reporter_latitude, zi.reporter_longitude,
			       zi.is_field_observation,
			       zi.verified_by_user_id, zi.verified_at,
			       zi.status,
			       b.bin_number
			FROM zone_incidents zi
			LEFT JOIN bins b ON zi.bin_id = b.id
			WHERE zi.shift_id = $1
			ORDER BY zi.reported_at DESC
		`

		if err := db.Select(&incidents, query, shiftID); err != nil {
			log.Printf("❌ Error fetching shift incidents: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch incidents")
			return
		}

		// Convert to response format
		response := make([]ZoneIncidentResponse, len(incidents))
		for i, incident := range incidents {
			response[i] = ZoneIncidentResponse{
				ID:                 incident.ID,
				ZoneID:             incident.ZoneID,
				BinID:              incident.BinID,
				BinNumber:          incident.BinNumber,
				IncidentType:       incident.IncidentType,
				ReportedByUserID:   incident.ReportedByUserID,
				ReportedAtISO:      time.Unix(incident.ReportedAt, 0).Format(time.RFC3339),
				Description:        incident.Description,
				PhotoURL:           incident.PhotoURL,
				CheckID:            incident.CheckID,
				MoveID:             incident.MoveID,
				ShiftID:            incident.ShiftID,
				ReporterLatitude:   incident.ReporterLatitude,
				ReporterLongitude:  incident.ReporterLongitude,
				IsFieldObservation: incident.IsFieldObservation,
				VerifiedByUserID:   incident.VerifiedByUserID,
				Status:             incident.Status,
			}

			if incident.VerifiedAt != nil {
				verifiedISO := time.Unix(*incident.VerifiedAt, 0).Format(time.RFC3339)
				response[i].VerifiedAtISO = &verifiedISO
			}
		}

		log.Printf("✅ Found %d incidents for shift %s", len(response), shiftID)
		utils.RespondJSON(w, http.StatusOK, response)
	}
}

// GetFieldObservations returns field observations for manager review
func GetFieldObservations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/field-observations")

		statusFilter := r.URL.Query().Get("status") // all, pending, verified

		var incidents []struct {
			ID                 string   `db:"id"`
			ZoneID             string   `db:"zone_id"`
			BinID              *string  `db:"bin_id"`
			BinNumber          *int     `db:"bin_number"`
			IncidentType       string   `db:"incident_type"`
			ReportedByUserID   *string  `db:"reported_by_user_id"`
			ReportedByName     *string  `db:"reported_by_name"`
			ReportedAt         int64    `db:"reported_at"`
			Description        *string  `db:"description"`
			PhotoURL           *string  `db:"photo_url"`
			CheckID            *int     `db:"check_id"`
			MoveID             *int     `db:"move_id"`
			ShiftID            *string  `db:"shift_id"`
			ReporterLatitude   *float64 `db:"reporter_latitude"`
			ReporterLongitude  *float64 `db:"reporter_longitude"`
			IsFieldObservation bool     `db:"is_field_observation"`
			VerifiedByUserID   *string  `db:"verified_by_user_id"`
			VerifiedByName     *string  `db:"verified_by_name"`
			VerifiedAt         *int64   `db:"verified_at"`
			Status             string   `db:"status"`
		}

		query := `
			SELECT zi.id, zi.zone_id, zi.bin_id, zi.incident_type,
			       zi.reported_by_user_id, zi.reported_at,
			       zi.description, zi.photo_url,
			       zi.check_id, zi.move_id, zi.shift_id,
			       zi.reporter_latitude, zi.reporter_longitude,
			       zi.is_field_observation,
			       zi.verified_by_user_id, zi.verified_at,
			       zi.status,
			       b.bin_number,
			       u1.name as reported_by_name,
			       u2.name as verified_by_name
			FROM zone_incidents zi
			LEFT JOIN bins b ON zi.bin_id = b.id
			LEFT JOIN users u1 ON zi.reported_by_user_id = u1.id
			LEFT JOIN users u2 ON zi.verified_by_user_id = u2.id
			WHERE zi.is_field_observation = true
		`

		// Apply status filter
		if statusFilter == "pending" {
			query += " AND zi.verified_at IS NULL"
		} else if statusFilter == "verified" {
			query += " AND zi.verified_at IS NOT NULL"
		}

		query += " ORDER BY zi.reported_at DESC"

		if err := db.Select(&incidents, query); err != nil {
			log.Printf("❌ Error fetching field observations: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch field observations")
			return
		}

		// Convert to response format
		type FieldObservationResponse struct {
			ZoneIncidentResponse
			ReportedByName *string `json:"reported_by_name,omitempty"`
			VerifiedByName *string `json:"verified_by_name,omitempty"`
		}

		response := make([]FieldObservationResponse, len(incidents))
		for i, incident := range incidents {
			response[i] = FieldObservationResponse{
				ZoneIncidentResponse: ZoneIncidentResponse{
					ID:                 incident.ID,
					ZoneID:             incident.ZoneID,
					BinID:              incident.BinID,
					BinNumber:          incident.BinNumber,
					IncidentType:       incident.IncidentType,
					ReportedByUserID:   incident.ReportedByUserID,
					ReportedAtISO:      time.Unix(incident.ReportedAt, 0).Format(time.RFC3339),
					Description:        incident.Description,
					PhotoURL:           incident.PhotoURL,
					CheckID:            incident.CheckID,
					MoveID:             incident.MoveID,
					ShiftID:            incident.ShiftID,
					ReporterLatitude:   incident.ReporterLatitude,
					ReporterLongitude:  incident.ReporterLongitude,
					IsFieldObservation: incident.IsFieldObservation,
					VerifiedByUserID:   incident.VerifiedByUserID,
					Status:             incident.Status,
				},
				ReportedByName: incident.ReportedByName,
				VerifiedByName: incident.VerifiedByName,
			}

			if incident.VerifiedAt != nil {
				verifiedISO := time.Unix(*incident.VerifiedAt, 0).Format(time.RFC3339)
				response[i].VerifiedAtISO = &verifiedISO
			}
		}

		log.Printf("✅ Found %d field observations (filter: '%s')", len(response), statusFilter)
		utils.RespondJSON(w, http.StatusOK, response)
	}
}

// VerifyFieldObservation marks a field observation as verified by a manager
func VerifyFieldObservation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		incidentID := r.PathValue("id")
		log.Printf("📥 REQUEST: PATCH /api/field-observations/%s/verify", incidentID)

		// Get user from context (manager only)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		now := time.Now().Unix()

		// Update the incident
		result, err := db.Exec(`
			UPDATE zone_incidents 
			SET verified_by_user_id = $1, verified_at = $2, status = 'investigating'
			WHERE id = $3 AND is_field_observation = true
		`, userClaims.UserID, now, incidentID)

		if err != nil {
			log.Printf("❌ Error verifying field observation: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to verify observation")
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			utils.RespondError(w, http.StatusNotFound, "Field observation not found")
			return
		}

		log.Printf("✅ Field observation %s verified by manager %s", incidentID, userClaims.UserID)

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":     true,
			"incident_id": incidentID,
			"verified_at": time.Unix(now, 0).Format(time.RFC3339),
			"verified_by": userClaims.UserID,
		})
	}
}

// GetBinIncidents returns all zone incidents associated with a specific bin
func GetBinIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "id")
		if binID == "" {
			utils.RespondError(w, http.StatusBadRequest, "missing bin ID")
			return
		}

		var incidents []models.ZoneIncidentResponse
		err := db.Select(&incidents, `
			SELECT
				zi.id, zi.zone_id, zi.bin_id, zi.incident_type,
				zi.reported_by_user_id, zi.description, zi.photo_url,
				zi.check_id, zi.shift_id, zi.move_id,
				zi.reporter_latitude, zi.reporter_longitude,
				zi.is_field_observation, zi.status,
				zi.verified_by_user_id, zi.verified_at,
				to_timestamp(zi.reported_at) AS reported_at_iso,
				u.name AS reported_by_name,
				v.name AS verified_by_name,
				b.bin_number
			FROM zone_incidents zi
			LEFT JOIN users u ON u.id = zi.reported_by_user_id
			LEFT JOIN users v ON v.id = zi.verified_by_user_id
			LEFT JOIN bins b ON b.id = zi.bin_id
			WHERE zi.bin_id = $1
			ORDER BY zi.reported_at DESC
		`, binID)
		if err != nil {
			log.Printf("❌ [GetBinIncidents] DB error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "failed to fetch bin incidents")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(incidents); err != nil {
			log.Printf("❌ [GetBinIncidents] encode error: %v", err)
		}
	}
}

// formatIncidentTypeLabel converts raw incident type to human-readable label
func formatIncidentTypeLabel(t string) string {
	switch t {
	case "vandalism", "vandalized":
		return "Vandalism"
	case "theft":
		return "Theft"
	case "landlord_complaint":
		return "Landlord Complaint"
	case "relocation_request":
		return "Relocation Request"
	case "missing":
		return "Missing Bin"
	case "damaged":
		return "Damaged"
	case "inaccessible":
		return "Inaccessible"
	case "pulled_from_service":
		return "Pulled From Service"
	default:
		return strings.ReplaceAll(t, "_", " ")
	}
}

// createZoneAndIncident creates a new zone + incident for each report.
// Each incident gets its own zone (1:1). No clustering or merging.
//
// This is a thin wrapper around createZoneAndIncidentExt that runs the inserts
// on the bare *sqlx.DB pool. Callers that need the writes to participate in an
// existing transaction should call createZoneAndIncidentExt with their tx.
func createZoneAndIncident(
	db *sqlx.DB,
	centrifugoClient *centrifugo.Client,
	lat, lng float64,
	zoneName string,
	incidentType string,
	binID *string,
	reportedByUserID string,
	description *string,
	photoURL *string,
	shiftID *string,
	checkID *int,
	reporterLat *float64,
	reporterLng *float64,
	isFieldObservation bool,
	now int64,
	source *string,
	moveRequestID *string,
) (string, error) {
	incidentID, _, err := createZoneAndIncidentExt(
		db,
		lat, lng,
		zoneName,
		incidentType,
		binID,
		reportedByUserID,
		description,
		photoURL,
		shiftID,
		checkID,
		reporterLat,
		reporterLng,
		isFieldObservation,
		now,
		source,
		moveRequestID,
	)
	return incidentID, err
}

// createZoneAndIncidentExt is the sqlx.Ext-based implementation of
// createZoneAndIncident. It accepts any handle satisfying sqlx.Ext (both the
// *sqlx.DB pool and a *sqlx.Tx), so callers can run the two inserts inside an
// existing transaction. The centrifugoClient is intentionally not a parameter
// here since this function performs no broadcasting.
func createZoneAndIncidentExt(
	ext sqlx.Ext,
	lat, lng float64,
	zoneName string,
	incidentType string,
	binID *string,
	reportedByUserID string,
	description *string,
	photoURL *string,
	shiftID *string,
	checkID *int,
	reporterLat *float64,
	reporterLng *float64,
	isFieldObservation bool,
	now int64,
	source *string,
	moveRequestID *string,
) (incidentID string, zoneID string, err error) {
	// 1. Always create a new zone for this incident
	zoneID = uuid.New().String()
	if _, err := ext.Exec(`
		INSERT INTO no_go_zones (id, name, center_latitude, center_longitude, radius_meters, conflict_score, status, created_by_user_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, 0, 'active', $5, $6, $6)
	`, zoneID, zoneName, lat, lng, reportedByUserID, now); err != nil {
		return "", "", fmt.Errorf("failed to create zone: %w", err)
	}
	log.Printf("✅ [createZoneAndIncident] Created zone %s (%s)", zoneID, zoneName)

	// 2. Insert incident record
	incidentID = uuid.New().String()
	if _, err := ext.Exec(`
		INSERT INTO zone_incidents (
			id, zone_id, bin_id, incident_type,
			reported_by_user_id, reported_at,
			description, photo_url,
			check_id, shift_id,
			reporter_latitude, reporter_longitude,
			is_field_observation, status,
			source, move_request_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
	`,
		incidentID, zoneID, binID, incidentType,
		reportedByUserID, now,
		description, photoURL,
		checkID, shiftID,
		reporterLat, reporterLng,
		isFieldObservation, "open",
		source, moveRequestID,
	); err != nil {
		return "", "", fmt.Errorf("failed to insert incident: %w", err)
	}
	log.Printf("✅ [createZoneAndIncident] Incident %s created (zone: %s, type: %s)", incidentID, zoneID, incidentType)

	return incidentID, zoneID, nil
}

// CreateManagerIncidentReport allows a manager/admin to file an incident report
// for a specific bin OR for a geocoded address (when no bin exists yet).
//
// POST /api/manager/incident-report
//
// Request body:
//
//	{
//	  "incident_type":  "landlord_complaint",   // required
//	  "description":    "Caller reported...",   // required
//	  "bin_id":         "uuid",                 // optional — provide bin OR address
//	  "latitude":       40.7128,                // required if no bin_id
//	  "longitude":      -74.0060,               // required if no bin_id
//	  "address":        "123 Main St, NYC",     // required if no bin_id (used as zone name)
//	  "photo_url":      "https://..."           // optional
//	}
func CreateManagerIncidentReport(db *sqlx.DB, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/manager/incident-report")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var req struct {
			IncidentType string   `json:"incident_type"`
			Description  *string  `json:"description"`
			BinID        *string  `json:"bin_id"`
			Latitude     *float64 `json:"latitude"`
			Longitude    *float64 `json:"longitude"`
			Address      *string  `json:"address"`
			PhotoURL     *string  `json:"photo_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Validate incident type
		validTypes := map[string]bool{
			"vandalism": true, "vandalized": true, "landlord_complaint": true,
			"theft": true, "relocation_request": true, "missing": true,
			"damaged": true, "inaccessible": true,
		}
		if !validTypes[req.IncidentType] {
			utils.RespondError(w, http.StatusBadRequest, "invalid incident_type")
			return
		}
		if req.Description == nil || *req.Description == "" {
			utils.RespondError(w, http.StatusBadRequest, "description is required")
			return
		}

		// Resolve coordinates and zone name
		var lat, lng float64
		var zoneName string

		if req.BinID != nil && *req.BinID != "" {
			// Mode 1: bin-linked report — look up the bin's coordinates
			var bin models.Bin
			if err := db.Get(&bin, "SELECT * FROM bins WHERE id = $1", *req.BinID); err != nil {
				utils.RespondError(w, http.StatusNotFound, "bin not found")
				return
			}
			if bin.Latitude == nil || bin.Longitude == nil {
				utils.RespondError(w, http.StatusUnprocessableEntity, "bin has no coordinates")
				return
			}
			lat = *bin.Latitude
			lng = *bin.Longitude
			zoneName = fmt.Sprintf("%s - %s", bin.CurrentStreet, bin.City)
			log.Printf("   📦 Bin-linked report: bin=%s (%s)", *req.BinID, zoneName)
		} else {
			// Mode 2: address-only report — use provided lat/lng and address
			if req.Latitude == nil || req.Longitude == nil || req.Address == nil {
				utils.RespondError(w, http.StatusBadRequest,
					"either bin_id or (latitude, longitude, address) must be provided")
				return
			}
			lat = *req.Latitude
			lng = *req.Longitude
			zoneName = *req.Address
			log.Printf("   🗺️  Address-only report: %s (%.6f, %.6f)", zoneName, lat, lng)
		}

		now := time.Now().Unix()

		managerReportSource := "manager_report"
		incidentID, err := createZoneAndIncident(
			db,
			centrifugoClient,
			lat, lng,
			zoneName,
			req.IncidentType,
			req.BinID, // nil for address-only
			userClaims.UserID,
			req.Description,
			req.PhotoURL,
			nil,      // shiftID — managers don't have shifts
			nil,      // checkID — not from a bin check
			nil, nil, // reporter GPS (manager is in office)
			true, // isFieldObservation — manager-logged
			now,
			&managerReportSource, // source
			nil,                  // moveRequestID — not from a move request
		)
		if err != nil {
			log.Printf("❌ [CreateManagerIncidentReport] %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "failed to create incident report")
			return
		}

		log.Printf("✅ [CreateManagerIncidentReport] Incident %s created by manager %s", incidentID, userClaims.UserID)

		// Fetch the zone that was created/updated so we can broadcast it in real-time
		var zone models.NoGoZone
		if err := db.Get(&zone,
			`SELECT z.* FROM no_go_zones z
			 JOIN zone_incidents zi ON zi.zone_id = z.id
			 WHERE zi.id = $1`, incidentID,
		); err == nil {
			if centrifugoClient != nil {
				zoneResp := zone.ToResponse()
				if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "zone_created", zoneResp); pubErr != nil {
					log.Printf("⚠️ [CreateManagerIncidentReport] Centrifugo publish failed: %v", pubErr)
				}
			}
		} else {
			log.Printf("⚠️ [CreateManagerIncidentReport] Could not fetch zone for broadcast: %v", err)
		}

		utils.RespondJSON(w, http.StatusCreated, map[string]interface{}{
			"success":       true,
			"incident_id":   incidentID,
			"reported_by":   userClaims.UserID,
			"incident_type": req.IncidentType,
			"created_at":    time.Unix(now, 0).Format(time.RFC3339),
		})
	}
}

// UpdateNoGoZone allows managers to edit no-go zone details
// PATCH /api/manager/no-go-zones/{id}
//
// Request body:
//
//	{
//	  "name":              "1185 Campbell Ave - San Jose",  // optional
//	  "center_latitude":   37.3375,                         // optional (from geocoding)
//	  "center_longitude":  -121.9283,                       // optional (from geocoding)
//	  "radius_meters":     300,                             // optional
//	  "status":            "resolved",                      // optional: active, monitoring, resolved
//	  "resolution_notes":  "Issue resolved by landlord"     // optional
//	}
func UpdateNoGoZone(db *sqlx.DB, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := chi.URLParam(r, "id")
		log.Printf("📥 REQUEST: PATCH /api/manager/no-go-zones/%s", zoneID)

		// Get user from context (manager only)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		var req struct {
			Name            *string  `json:"name"`
			CenterLatitude  *float64 `json:"center_latitude"`
			CenterLongitude *float64 `json:"center_longitude"`
			RadiusMeters    *int     `json:"radius_meters"`
			Status          *string  `json:"status"`
			ResolutionNotes *string  `json:"resolution_notes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [UpdateNoGoZone] Invalid request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Validate status if provided
		if req.Status != nil {
			validStatuses := map[string]bool{"active": true, "monitoring": true, "resolved": true}
			if !validStatuses[*req.Status] {
				utils.RespondError(w, http.StatusBadRequest, "status must be 'active', 'monitoring', or 'resolved'")
				return
			}
		}

		// Validate radius if provided
		if req.RadiusMeters != nil && (*req.RadiusMeters < 100 || *req.RadiusMeters > 2000) {
			utils.RespondError(w, http.StatusBadRequest, "radius_meters must be between 100 and 2000")
			return
		}

		// Fetch existing zone
		var zone models.NoGoZone
		if err := db.Get(&zone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID); err != nil {
			if err == sql.ErrNoRows {
				log.Printf("❌ [UpdateNoGoZone] Zone not found: %s", zoneID)
				utils.RespondError(w, http.StatusNotFound, "zone not found")
				return
			}
			log.Printf("❌ [UpdateNoGoZone] Database error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "failed to fetch zone")
			return
		}

		now := time.Now().Unix()

		// Build dynamic UPDATE query
		updates := []string{}
		args := []interface{}{}
		argIndex := 1

		if req.Name != nil {
			updates = append(updates, fmt.Sprintf("name = $%d", argIndex))
			args = append(args, *req.Name)
			argIndex++
		}

		if req.CenterLatitude != nil {
			updates = append(updates, fmt.Sprintf("center_latitude = $%d", argIndex))
			args = append(args, *req.CenterLatitude)
			argIndex++
		}

		if req.CenterLongitude != nil {
			updates = append(updates, fmt.Sprintf("center_longitude = $%d", argIndex))
			args = append(args, *req.CenterLongitude)
			argIndex++
		}

		if req.RadiusMeters != nil {
			updates = append(updates, fmt.Sprintf("radius_meters = $%d", argIndex))
			args = append(args, *req.RadiusMeters)
			argIndex++
		}

		if req.Status != nil {
			updates = append(updates, fmt.Sprintf("status = $%d", argIndex))
			args = append(args, *req.Status)
			argIndex++

			// If status changed to 'resolved', set resolved_by and resolved_at
			if *req.Status == "resolved" && zone.Status != "resolved" {
				updates = append(updates, fmt.Sprintf("resolved_by_user_id = $%d", argIndex))
				args = append(args, userClaims.UserID)
				argIndex++

				updates = append(updates, fmt.Sprintf("resolved_at = $%d", argIndex))
				args = append(args, now)
				argIndex++
			}
		}

		if req.ResolutionNotes != nil {
			updates = append(updates, fmt.Sprintf("resolution_notes = $%d", argIndex))
			args = append(args, *req.ResolutionNotes)
			argIndex++
		}

		// Always update updated_at
		updates = append(updates, fmt.Sprintf("updated_at = $%d", argIndex))
		args = append(args, now)
		argIndex++

		// Add zone ID as the last argument
		args = append(args, zoneID)

		if len(updates) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "no fields to update")
			return
		}

		// Execute update
		query := fmt.Sprintf("UPDATE no_go_zones SET %s WHERE id = $%d", strings.Join(updates, ", "), argIndex)
		if _, err := db.Exec(query, args...); err != nil {
			log.Printf("❌ [UpdateNoGoZone] Update failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "failed to update zone")
			return
		}

		// Fetch updated zone
		var updatedZone models.NoGoZone
		if err := db.Get(&updatedZone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID); err != nil {
			log.Printf("❌ [UpdateNoGoZone] Failed to fetch updated zone: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "failed to fetch updated zone")
			return
		}

		log.Printf("✅ [UpdateNoGoZone] Zone %s updated successfully", zoneID)

		// Broadcast update via Centrifugo
		if centrifugoClient != nil {
			zoneResp := updatedZone.ToResponse()
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "zone_updated", zoneResp); pubErr != nil {
				log.Printf("⚠️ [UpdateNoGoZone] Centrifugo publish failed: %v", pubErr)
			}
		}

		// Return updated zone
		response := updatedZone.ToResponse()
		utils.RespondJSON(w, http.StatusOK, response)
	}
}

// GetNearbyIncidents returns active incidents within a radius of a given point.
// Used by the frontend to show proximity warnings when placing bins or move destinations.
//
// Query params: lat, lng, radius (meters, default 800)
func GetNearbyIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		latStr := r.URL.Query().Get("lat")
		lngStr := r.URL.Query().Get("lng")
		radiusStr := r.URL.Query().Get("radius")

		lat, err := strconv.ParseFloat(latStr, 64)
		if err != nil || lat == 0 {
			utils.RespondError(w, http.StatusBadRequest, "invalid lat parameter")
			return
		}
		lng, err := strconv.ParseFloat(lngStr, 64)
		if err != nil || lng == 0 {
			utils.RespondError(w, http.StatusBadRequest, "invalid lng parameter")
			return
		}

		radius := 800.0 // default 800m
		if radiusStr != "" {
			if parsed, err := strconv.ParseFloat(radiusStr, 64); err == nil && parsed > 0 {
				radius = parsed
			}
		}

		type IncidentRow struct {
			ID              string  `db:"id" json:"id"`
			ZoneID          string  `db:"zone_id" json:"zone_id"`
			IncidentType    string  `db:"incident_type" json:"incident_type"`
			Description     *string `db:"description" json:"description"`
			ReportedAt      int64   `db:"reported_at" json:"reported_at"`
			BinNumber       *int    `db:"bin_number" json:"bin_number"`
			CenterLatitude  float64 `db:"center_latitude" json:"-"`
			CenterLongitude float64 `db:"center_longitude" json:"-"`
			Address         *string `db:"zone_name" json:"address"`
		}

		var rows []IncidentRow
		err = db.Select(&rows, `
			SELECT zi.id, zi.zone_id, zi.incident_type, zi.description, zi.reported_at,
			       b.bin_number, z.center_latitude, z.center_longitude, z.name AS zone_name
			FROM zone_incidents zi
			JOIN no_go_zones z ON zi.zone_id = z.id
			LEFT JOIN bins b ON zi.bin_id = b.id
			WHERE z.status = 'active'
			ORDER BY zi.reported_at DESC
		`)
		if err != nil {
			log.Printf("❌ [GetNearbyIncidents] Query failed: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "failed to fetch incidents")
			return
		}

		type NearbyIncident struct {
			ID             string  `json:"id"`
			ZoneID         string  `json:"zone_id"`
			IncidentType   string  `json:"incident_type"`
			Description    *string `json:"description"`
			DistanceMeters float64 `json:"distance_meters"`
			ReportedAt     int64   `json:"reported_at"`
			BinNumber      *int    `json:"bin_number"`
			Address        *string `json:"address"`
		}

		var nearby []NearbyIncident
		for _, row := range rows {
			dist := calculateZoneDistance(lat, lng, row.CenterLatitude, row.CenterLongitude)
			if dist <= radius {
				nearby = append(nearby, NearbyIncident{
					ID:             row.ID,
					ZoneID:         row.ZoneID,
					IncidentType:   row.IncidentType,
					Description:    row.Description,
					DistanceMeters: math.Round(dist),
					ReportedAt:     row.ReportedAt,
					BinNumber:      row.BinNumber,
					Address:        row.Address,
				})
			}
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"incidents": nearby,
			"count":     len(nearby),
		})
	}
}
