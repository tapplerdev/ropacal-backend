package handlers

import (
	"log"
	"net/http"
	"time"

	"ropacal-backend/pkg/utils"

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
}

// ZoneIncidentResponse represents an incident with ISO timestamps
type ZoneIncidentResponse struct {
	ID                  string   `json:"id"`
	ZoneID              string   `json:"zone_id"`
	BinID               string   `json:"bin_id"`
	BinNumber           *int     `json:"bin_number,omitempty"`
	IncidentType        string   `json:"incident_type"`
	ReportedByUserID    *string  `json:"reported_by_user_id,omitempty"`
	ReportedAtISO       string   `json:"reported_at_iso"`
	Description         *string  `json:"description,omitempty"`
	PhotoURL            *string  `json:"photo_url,omitempty"`
	CheckID             *int     `json:"check_id,omitempty"`
	MoveID              *int     `json:"move_id,omitempty"`
	ShiftID             *string  `json:"shift_id,omitempty"`
	ReporterLatitude    *float64 `json:"reporter_latitude,omitempty"`
	ReporterLongitude   *float64 `json:"reporter_longitude,omitempty"`
	IsFieldObservation  bool     `json:"is_field_observation"`
	VerifiedByUserID    *string  `json:"verified_by_user_id,omitempty"`
	VerifiedAtISO       *string  `json:"verified_at_iso,omitempty"`
	Status              string   `json:"status"`
}

// GetNoGoZones returns all no-go zones (optionally filtered by status)
func GetNoGoZones(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/no-go-zones")

		status := r.URL.Query().Get("status")

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
		}

		query := "SELECT * FROM no_go_zones"
		if status != "" {
			query += " WHERE status = $1 ORDER BY updated_at DESC"
			if err := db.Select(&zones, query, status); err != nil {
				log.Printf("❌ Error fetching zones: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch zones")
				return
			}
		} else {
			query += " ORDER BY updated_at DESC"
			if err := db.Select(&zones, query); err != nil {
				log.Printf("❌ Error fetching zones: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch zones")
				return
			}
		}

		// Convert to response format with ISO timestamps
		response := make([]NoGoZoneResponse, len(zones))
		for i, zone := range zones {
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
			}

			if zone.ResolvedAt != nil {
				resolvedISO := time.Unix(*zone.ResolvedAt, 0).Format(time.RFC3339)
				response[i].ResolvedAtISO = &resolvedISO
			}
		}

		log.Printf("✅ Found %d zones (status filter: '%s')", len(response), status)
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
		}

		if err := db.Get(&zone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID); err != nil {
			log.Printf("❌ Zone not found: %v", err)
			utils.RespondError(w, http.StatusNotFound, "Zone not found")
			return
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
func GetZoneIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := r.PathValue("id")
		log.Printf("📥 REQUEST: GET /api/no-go-zones/%s/incidents", zoneID)

		var incidents []struct {
			ID                 string   `db:"id"`
			ZoneID             string   `db:"zone_id"`
			BinID              string   `db:"bin_id"`
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
			WHERE zi.zone_id = $1
			ORDER BY zi.reported_at DESC
		`

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

		log.Printf("✅ Found %d incidents for zone %s", len(response), zoneID)
		utils.RespondJSON(w, http.StatusOK, response)
	}
}
