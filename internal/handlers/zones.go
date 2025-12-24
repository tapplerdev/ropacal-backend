package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// GetNoGoZones returns all no-go zones with optional filtering
func GetNoGoZones(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status") // active, monitoring, resolved

		query := `SELECT * FROM no_go_zones WHERE 1=1`
		args := []interface{}{}

		if status != "" {
			query += ` AND status = $1`
			args = append(args, status)
		}

		query += ` ORDER BY conflict_score DESC, created_at DESC`

		var zones []models.NoGoZone
		err := db.Select(&zones, query, args...)
		if err != nil {
			http.Error(w, "Failed to fetch zones", http.StatusInternalServerError)
			return
		}

		responses := make([]models.NoGoZoneResponse, len(zones))
		for i, zone := range zones {
			responses[i] = zone.ToResponse()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// GetNoGoZone returns a single zone with optional inclusions
func GetNoGoZone(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := chi.URLParam(r, "id")
		if zoneID == "" {
			http.Error(w, "Zone ID required", http.StatusBadRequest)
			return
		}

		var zone models.NoGoZone
		err := db.Get(&zone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID)
		if err == sql.ErrNoRows {
			http.Error(w, "Zone not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch zone", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(zone.ToResponse())
	}
}

// CreateNoGoZone creates a new no-go zone
func CreateNoGoZone(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name            string  `json:"name"`
			CenterLatitude  float64 `json:"center_latitude"`
			CenterLongitude float64 `json:"center_longitude"`
			RadiusMeters    int     `json:"radius_meters"`
			Reason          string  `json:"reason"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate
		if req.Name == "" || req.RadiusMeters <= 0 {
			http.Error(w, "Name and radius required", http.StatusBadRequest)
			return
		}

		// Get user from context (manager)
		var userID *string
		if claims, ok := r.Context().Value("userClaims").(map[string]interface{}); ok {
			if uid, ok := claims["user_id"].(string); ok {
				userID = &uid
			}
		}

		now := time.Now().Unix()
		zoneID := uuid.New().String()

		_, err := db.Exec(`
			INSERT INTO no_go_zones (
				id, name, center_latitude, center_longitude,
				radius_meters, conflict_score, status,
				created_by_user_id, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, zoneID, req.Name, req.CenterLatitude, req.CenterLongitude,
			req.RadiusMeters, 0, "active", userID, now, now)

		if err != nil {
			http.Error(w, "Failed to create zone", http.StatusInternalServerError)
			return
		}

		// Fetch created zone
		var zone models.NoGoZone
		err = db.Get(&zone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID)
		if err != nil {
			http.Error(w, "Failed to fetch created zone", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(zone.ToResponse())
	}
}

// UpdateNoGoZone updates zone status or details
func UpdateNoGoZone(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := chi.URLParam(r, "id")
		if zoneID == "" {
			http.Error(w, "Zone ID required", http.StatusBadRequest)
			return
		}

		var req struct {
			Status          *string `json:"status"`
			ConflictScore   *int    `json:"conflict_score"`
			ResolutionNotes *string `json:"resolution_notes"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Get user from context
		var userID *string
		if claims, ok := r.Context().Value("userClaims").(map[string]interface{}); ok {
			if uid, ok := claims["user_id"].(string); ok {
				userID = &uid
			}
		}

		now := time.Now().Unix()

		// Build dynamic update query
		query := "UPDATE no_go_zones SET updated_at = $1"
		args := []interface{}{now}
		paramCount := 1

		if req.Status != nil {
			paramCount++
			query += fmt.Sprintf(", status = $%d", paramCount)
			args = append(args, *req.Status)

			// If resolving, set resolved_by and resolved_at
			if *req.Status == "resolved" {
				paramCount++
				query += fmt.Sprintf(", resolved_by_user_id = $%d", paramCount)
				args = append(args, userID)

				paramCount++
				query += fmt.Sprintf(", resolved_at = $%d", paramCount)
				args = append(args, now)
			}
		}

		if req.ConflictScore != nil {
			paramCount++
			query += fmt.Sprintf(", conflict_score = $%d", paramCount)
			args = append(args, *req.ConflictScore)
		}

		if req.ResolutionNotes != nil {
			paramCount++
			query += fmt.Sprintf(", resolution_notes = $%d", paramCount)
			args = append(args, *req.ResolutionNotes)
		}

		paramCount++
		query += fmt.Sprintf(" WHERE id = $%d", paramCount)
		args = append(args, zoneID)

		_, err := db.Exec(query, args...)
		if err != nil {
			http.Error(w, "Failed to update zone", http.StatusInternalServerError)
			return
		}

		// Fetch updated zone
		var zone models.NoGoZone
		err = db.Get(&zone, "SELECT * FROM no_go_zones WHERE id = $1", zoneID)
		if err != nil {
			http.Error(w, "Failed to fetch updated zone", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(zone.ToResponse())
	}
}

// DeleteNoGoZone soft-deletes a zone (sets status to resolved)
func DeleteNoGoZone(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := chi.URLParam(r, "id")
		if zoneID == "" {
			http.Error(w, "Zone ID required", http.StatusBadRequest)
			return
		}

		// Soft delete - set status to resolved
		_, err := db.Exec(`
			UPDATE no_go_zones
			SET status = 'resolved', updated_at = $1
			WHERE id = $2
		`, time.Now().Unix(), zoneID)

		if err != nil {
			http.Error(w, "Failed to delete zone", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// GetZoneIncidents returns all incidents for a zone
func GetZoneIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		zoneID := chi.URLParam(r, "id")
		if zoneID == "" {
			http.Error(w, "Zone ID required", http.StatusBadRequest)
			return
		}

		type IncidentWithDetails struct {
			models.ZoneIncident
			BinNumber      *int    `db:"bin_number"`
			ReportedByName *string `db:"reported_by_name"`
		}

		var incidents []IncidentWithDetails
		err := db.Select(&incidents, `
			SELECT
				i.*,
				b.bin_number,
				u.name as reported_by_name
			FROM zone_incidents i
			LEFT JOIN bins b ON i.bin_id = b.id
			LEFT JOIN users u ON i.reported_by_user_id = u.id
			WHERE i.zone_id = $1
			ORDER BY i.reported_at DESC
		`, zoneID)

		if err != nil {
			http.Error(w, "Failed to fetch incidents", http.StatusInternalServerError)
			return
		}

		responses := make([]models.ZoneIncidentResponse, len(incidents))
		for i, incident := range incidents {
			resp := incident.ZoneIncident.ToResponse()
			resp.BinNumber = incident.BinNumber
			resp.ReportedByName = incident.ReportedByName
			responses[i] = resp
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// CreateZoneIncident reports a new incident and creates/updates zone
func CreateZoneIncident(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			BinID             string   `json:"bin_id"`
			IncidentType      string   `json:"incident_type"`
			Description       *string  `json:"description"`
			PhotoURL          *string  `json:"photo_url"`
			CheckID           *int     `json:"check_id"`
			ReporterLatitude  *float64 `json:"reporter_latitude"`
			ReporterLongitude *float64 `json:"reporter_longitude"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate
		if req.BinID == "" || req.IncidentType == "" {
			http.Error(w, "bin_id and incident_type required", http.StatusBadRequest)
			return
		}

		// Determine if this is a field observation
		isFieldObservation := req.CheckID == nil

		// ALL incidents require photo evidence (proof)
		if req.PhotoURL == nil {
			http.Error(w, "Photo evidence is required for all incident reports", http.StatusBadRequest)
			return
		}

		// Field observations additionally require GPS coordinates (proof driver was there)
		if isFieldObservation && (req.ReporterLatitude == nil || req.ReporterLongitude == nil) {
			http.Error(w, "Field observations require reporter GPS coordinates", http.StatusBadRequest)
			return
		}

		// Get user from context
		var userID *string
		if claims, ok := r.Context().Value("userClaims").(map[string]interface{}); ok {
			if uid, ok := claims["user_id"].(string); ok {
				userID = &uid
			}
		}

		// Get active shift if user is a driver with an active shift
		var activeShiftID *string
		if userID != nil {
			var shift models.Shift
			err := db.Get(&shift, `
				SELECT id FROM shifts
				WHERE driver_id = $1
				AND status = 'active'
				LIMIT 1
			`, userID)
			if err == nil {
				activeShiftID = &shift.ID
			}
		}

		// Get bin details
		var bin models.Bin
		err := db.Get(&bin, "SELECT * FROM bins WHERE id = $1", req.BinID)
		if err != nil {
			http.Error(w, "Bin not found", http.StatusNotFound)
			return
		}

		// Check if bin already has coordinates
		if bin.Latitude == nil || bin.Longitude == nil {
			http.Error(w, "Bin must have coordinates to create zone", http.StatusBadRequest)
			return
		}

		// Determine radius based on incident type
		radiusMeters := 500 // default
		switch req.IncidentType {
		case "landlord_complaint":
			radiusMeters = 150
		case "vandalism":
			radiusMeters = 500
		case "theft":
			radiusMeters = 600
		}

		// Check if zone already exists within 100m of this location
		var existingZone *models.NoGoZone
		var zones []models.NoGoZone
		err = db.Select(&zones, "SELECT * FROM no_go_zones WHERE status = 'active'")
		if err == nil {
			for _, zone := range zones {
				distance := calculateZoneDistance(*bin.Latitude, *bin.Longitude, zone.CenterLatitude, zone.CenterLongitude)
				if distance < 100 { // Within 100m, use existing zone
					existingZone = &zone
					break
				}
			}
		}

		var zoneID string
		now := time.Now().Unix()

		if existingZone != nil {
			// Use existing zone, increment conflict score
			zoneID = existingZone.ID
			newScore := existingZone.ConflictScore + getIncidentScore(req.IncidentType)
			_, err = db.Exec(`
				UPDATE no_go_zones
				SET conflict_score = $1, updated_at = $2
				WHERE id = $3
			`, newScore, now, zoneID)
			if err != nil {
				http.Error(w, "Failed to update zone", http.StatusInternalServerError)
				return
			}
		} else {
			// Create new zone
			zoneID = uuid.New().String()
			zoneName := fmt.Sprintf("%s - %s", bin.CurrentStreet, bin.City)

			_, err = db.Exec(`
				INSERT INTO no_go_zones (
					id, name, center_latitude, center_longitude,
					radius_meters, conflict_score, status,
					created_by_user_id, created_at, updated_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`, zoneID, zoneName, *bin.Latitude, *bin.Longitude,
				radiusMeters, getIncidentScore(req.IncidentType), "active",
				nil, now, now) // nil for created_by = auto-created

			if err != nil {
				http.Error(w, "Failed to create zone", http.StatusInternalServerError)
				return
			}
		}

		// Create incident record
		incidentID := uuid.New().String()
		_, err = db.Exec(`
			INSERT INTO zone_incidents (
				id, zone_id, bin_id, incident_type,
				reported_by_user_id, reported_at, description,
				photo_url, check_id, shift_id, reporter_latitude, reporter_longitude,
				is_field_observation, status
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`, incidentID, zoneID, req.BinID, req.IncidentType,
			userID, now, req.Description, req.PhotoURL, req.CheckID,
			activeShiftID, req.ReporterLatitude, req.ReporterLongitude, isFieldObservation, "open")

		if err != nil {
			http.Error(w, "Failed to create incident", http.StatusInternalServerError)
			return
		}

		// Fetch created incident
		var incident models.ZoneIncident
		err = db.Get(&incident, "SELECT * FROM zone_incidents WHERE id = $1", incidentID)
		if err != nil {
			http.Error(w, "Failed to fetch created incident", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(incident.ToResponse())
	}
}

// GetFieldObservations returns unverified field observations for manager review
func GetFieldObservations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status") // all, pending, verified

		query := `
			SELECT
				zi.*,
				b.bin_number,
				u1.name AS reported_by_name,
				u2.name AS verified_by_name
			FROM zone_incidents zi
			LEFT JOIN bins b ON zi.bin_id = b.id
			LEFT JOIN users u1 ON zi.reported_by_user_id = u1.id
			LEFT JOIN users u2 ON zi.verified_by_user_id = u2.id
			WHERE zi.is_field_observation = true
		`

		// Filter by verification status
		switch status {
		case "pending":
			query += " AND zi.verified_by_user_id IS NULL"
		case "verified":
			query += " AND zi.verified_by_user_id IS NOT NULL"
		// "all" or empty = no additional filter
		}

		query += " ORDER BY zi.reported_at DESC"

		type IncidentWithDetails struct {
			models.ZoneIncident
			BinNumber      *int    `db:"bin_number"`
			ReportedByName *string `db:"reported_by_name"`
			VerifiedByName *string `db:"verified_by_name"`
		}

		var incidents []IncidentWithDetails
		err := db.Select(&incidents, query)
		if err != nil {
			http.Error(w, "Failed to fetch field observations", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.ZoneIncidentResponse, len(incidents))
		for i, inc := range incidents {
			resp := inc.ZoneIncident.ToResponse()
			resp.BinNumber = inc.BinNumber
			resp.ReportedByName = inc.ReportedByName
			resp.VerifiedByName = inc.VerifiedByName
			responses[i] = resp
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// VerifyFieldObservation allows manager to verify a field observation
func VerifyFieldObservation(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		incidentID := chi.URLParam(r, "id")
		if incidentID == "" {
			http.Error(w, "Incident ID required", http.StatusBadRequest)
			return
		}

		// Get user from context (should be manager)
		var userID string
		if claims, ok := r.Context().Value("userClaims").(map[string]interface{}); ok {
			if uid, ok := claims["user_id"].(string); ok {
				userID = uid
			}
		}

		if userID == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		now := time.Now().Unix()

		// Update incident as verified
		_, err := db.Exec(`
			UPDATE zone_incidents
			SET verified_by_user_id = $1, verified_at = $2
			WHERE id = $3 AND is_field_observation = true
		`, userID, now, incidentID)

		if err != nil {
			http.Error(w, "Failed to verify incident", http.StatusInternalServerError)
			return
		}

		// Fetch updated incident
		var incident models.ZoneIncident
		err = db.Get(&incident, "SELECT * FROM zone_incidents WHERE id = $1", incidentID)
		if err != nil {
			http.Error(w, "Failed to fetch verified incident", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(incident.ToResponse())
	}
}

// GetShiftIncidents returns all incidents reported during a specific shift
func GetShiftIncidents(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "id")
		if shiftID == "" {
			http.Error(w, "Shift ID required", http.StatusBadRequest)
			return
		}

		type IncidentWithDetails struct {
			models.ZoneIncident
			BinNumber      *int    `db:"bin_number"`
			ReportedByName *string `db:"reported_by_name"`
			VerifiedByName *string `db:"verified_by_name"`
		}

		var incidents []IncidentWithDetails
		err := db.Select(&incidents, `
			SELECT
				zi.*,
				b.bin_number,
				u1.name AS reported_by_name,
				u2.name AS verified_by_name
			FROM zone_incidents zi
			LEFT JOIN bins b ON zi.bin_id = b.id
			LEFT JOIN users u1 ON zi.reported_by_user_id = u1.id
			LEFT JOIN users u2 ON zi.verified_by_user_id = u2.id
			WHERE zi.shift_id = $1
			ORDER BY zi.reported_at DESC
		`, shiftID)

		if err != nil {
			http.Error(w, "Failed to fetch shift incidents", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.ZoneIncidentResponse, len(incidents))
		for i, inc := range incidents {
			resp := inc.ZoneIncident.ToResponse()
			resp.BinNumber = inc.BinNumber
			resp.ReportedByName = inc.ReportedByName
			resp.VerifiedByName = inc.VerifiedByName
			responses[i] = resp
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// Helper: Calculate distance between two lat/lng points in meters (for zones)
func calculateZoneDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000 // meters

	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)

	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadius * c
}

// Helper: Get conflict score increment based on incident type
func getIncidentScore(incidentType string) int {
	switch incidentType {
	case "vandalism":
		return 10
	case "landlord_complaint":
		return 15
	case "theft":
		return 20
	case "relocation_request":
		return 5
	default:
		return 5
	}
}
