package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/pkg/utils"

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
			BinID              *string  `db:"bin_id"` // nil for address-only manager reports
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

		var query string
		if includeMerged {
			// Include incidents from zones that were merged into this zone
			query = `
				SELECT zi.*, b.bin_number
				FROM zone_incidents zi
				LEFT JOIN bins b ON zi.bin_id = b.id
				WHERE zi.zone_id = $1 OR zi.zone_id IN (
					SELECT id FROM no_go_zones WHERE merged_into_zone_id = $1
				)
				ORDER BY zi.reported_at DESC
			`
		} else {
			// Only incidents directly associated with this zone
			query = `
				SELECT zi.*, b.bin_number
				FROM zone_incidents zi
				LEFT JOIN bins b ON zi.bin_id = b.id
				WHERE zi.zone_id = $1
				ORDER BY zi.reported_at DESC
			`
		}

		if err := db.Select(&incidents, query, zoneID); err != nil {
			log.Printf("❌ Error fetching incidents: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch incidents")
			return
		}

		// Convert to response format with ISO timestamps
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

		log.Printf("✅ Found %d incidents for zone %s (include_merged: %v)", len(response), zoneID, includeMerged)
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
			SELECT zi.*, b.bin_number
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
			SELECT zi.*, 
			       b.bin_number,
			       u1.full_name as reported_by_name,
			       u2.full_name as verified_by_name
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

// createZoneAndIncident is a shared helper that finds or creates a no-go zone
// at the given coordinates, inserts a zone_incident, and runs merge detection.
//
// Used by both the driver CompleteTask flow and the manager incident report flow.
//
// Parameters:
//   - binID: nil for address-only manager reports (no bin involved)
//   - shiftID / checkID: nil for manager reports
//   - isFieldObservation: true for manager-logged reports
//
// Returns the created incidentID or an error.
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
) (string, error) {
	// 1. Find existing active zone within 100m
	var existingZone *models.NoGoZone
	var zones []models.NoGoZone
	if err := db.Select(&zones, "SELECT * FROM no_go_zones WHERE status = 'active'"); err != nil {
		log.Printf("⚠️  [createZoneAndIncident] Error fetching zones: %v", err)
		// Non-fatal — continue to create new zone
	} else {
		var minDist float64 = -1
		for _, z := range zones {
			dist := calculateZoneDistance(lat, lng, z.CenterLatitude, z.CenterLongitude)
			if dist < 100 && (minDist < 0 || dist < minDist) {
				zCopy := z
				existingZone = &zCopy
				minDist = dist
			}
		}
		if existingZone != nil {
			log.Printf("📍 [createZoneAndIncident] Found nearest existing zone within 100m (%.2fm)", minDist)
		}
	}

	// 2. Create or update zone
	var zoneID string
	if existingZone != nil {
		zoneID = existingZone.ID
		newScore := existingZone.ConflictScore + getIncidentScore(incidentType)
		if _, err := db.Exec(
			`UPDATE no_go_zones SET conflict_score = $1, updated_at = $2 WHERE id = $3`,
			newScore, now, zoneID,
		); err != nil {
			return "", fmt.Errorf("failed to update zone score: %w", err)
		}
		log.Printf("✅ [createZoneAndIncident] Updated zone %s (new score: %d)", zoneID, newScore)
	} else {
		zoneID = uuid.New().String()
		radius := getZoneRadius(incidentType)
		score := getIncidentScore(incidentType)
		if _, err := db.Exec(`
			INSERT INTO no_go_zones (id, name, center_latitude, center_longitude, radius_meters, conflict_score, status, created_by_user_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $8)
		`, zoneID, zoneName, lat, lng, radius, score, reportedByUserID, now); err != nil {
			return "", fmt.Errorf("failed to create zone: %w", err)
		}
		log.Printf("✅ [createZoneAndIncident] Created new zone %s (%s, radius %dm, score %d)", zoneID, zoneName, radius, score)
	}

	// 3. Run merge detection (non-fatal if it fails)
	if mergeErr := detectAndMergeZones(db, centrifugoClient, zoneID, now); mergeErr != nil {
		log.Printf("⚠️  [createZoneAndIncident] Zone merge check failed: %v", mergeErr)
	}

	// 4. Insert incident record
	incidentID := uuid.New().String()
	if _, err := db.Exec(`
		INSERT INTO zone_incidents (
			id, zone_id, bin_id, incident_type,
			reported_by_user_id, reported_at,
			description, photo_url,
			check_id, shift_id,
			reporter_latitude, reporter_longitude,
			is_field_observation, status
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`,
		incidentID, zoneID, binID, incidentType,
		reportedByUserID, now,
		description, photoURL,
		checkID, shiftID,
		reporterLat, reporterLng,
		isFieldObservation, "open",
	); err != nil {
		return "", fmt.Errorf("failed to insert incident: %w", err)
	}
	log.Printf("✅ [createZoneAndIncident] Incident %s created (zone: %s, type: %s)", incidentID, zoneID, incidentType)

	return incidentID, nil
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

		incidentID, err := createZoneAndIncident(
			db,
			centrifugoClient,
			lat, lng,
			zoneName,
			req.IncidentType,
			req.BinID,               // nil for address-only
			userClaims.UserID,
			req.Description,
			req.PhotoURL,
			nil,                     // shiftID — managers don't have shifts
			nil,                     // checkID — not from a bin check
			nil, nil,                // reporter GPS (manager is in office)
			true,                    // isFieldObservation — manager-logged
			now,
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
