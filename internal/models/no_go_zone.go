package models

import "time"

type NoGoZone struct {
	ID                 string   `json:"id" db:"id"`
	Name               string   `json:"name" db:"name"`
	CenterLatitude     float64  `json:"center_latitude" db:"center_latitude"`
	CenterLongitude    float64  `json:"center_longitude" db:"center_longitude"`
	RadiusMeters       int      `json:"radius_meters" db:"radius_meters"`
	ConflictScore      int      `json:"conflict_score" db:"conflict_score"`
	Status             string   `json:"status" db:"status"` // active, monitoring, resolved
	CreatedByUserID    *string  `json:"created_by_user_id" db:"created_by_user_id"`
	CreatedAt          int64    `json:"created_at" db:"created_at"`
	UpdatedAt          int64    `json:"updated_at" db:"updated_at"`
	ResolvedByUserID   *string  `json:"resolved_by_user_id" db:"resolved_by_user_id"`
	ResolvedAt         *int64   `json:"resolved_at" db:"resolved_at"`
	ResolutionNotes    *string  `json:"resolution_notes" db:"resolution_notes"`
}

type ZoneIncident struct {
	ID                  string   `json:"id" db:"id"`
	ZoneID              string   `json:"zone_id" db:"zone_id"`
	BinID               string   `json:"bin_id" db:"bin_id"`
	IncidentType        string   `json:"incident_type" db:"incident_type"` // vandalism, landlord_complaint, theft, relocation_request
	ReportedByUserID    *string  `json:"reported_by_user_id" db:"reported_by_user_id"`
	ReportedAt          int64    `json:"reported_at" db:"reported_at"`
	Description         *string  `json:"description" db:"description"`
	PhotoURL            *string  `json:"photo_url" db:"photo_url"`
	CheckID             *int     `json:"check_id" db:"check_id"`
	MoveID              *int     `json:"move_id" db:"move_id"`
	ReporterLatitude    *float64 `json:"reporter_latitude" db:"reporter_latitude"`
	ReporterLongitude   *float64 `json:"reporter_longitude" db:"reporter_longitude"`
	IsFieldObservation  bool     `json:"is_field_observation" db:"is_field_observation"`
	VerifiedByUserID    *string  `json:"verified_by_user_id" db:"verified_by_user_id"`
	VerifiedAt          *int64   `json:"verified_at" db:"verified_at"`
	Status              string   `json:"status" db:"status"` // open, resolved, investigating
}

type ZoneRiskOverride struct {
	ID               string  `json:"id" db:"id"`
	ZoneID           string  `json:"zone_id" db:"zone_id"`
	BinID            string  `json:"bin_id" db:"bin_id"`
	ManagerID        string  `json:"manager_id" db:"manager_id"`
	OverrideReason   string  `json:"override_reason" db:"override_reason"`
	OverrideAt       int64   `json:"override_at" db:"override_at"`
	ExpiresAt        *int64  `json:"expires_at" db:"expires_at"` // NULL = indefinite
	Status           string  `json:"status" db:"status"`         // active, expired, revoked
	IncidentCount    int     `json:"incident_count" db:"incident_count"`
	LastIncidentID   *string `json:"last_incident_id" db:"last_incident_id"`
}

// Response DTOs with ISO timestamps
type NoGoZoneResponse struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	CenterLatitude     float64  `json:"center_latitude"`
	CenterLongitude    float64  `json:"center_longitude"`
	RadiusMeters       int      `json:"radius_meters"`
	ConflictScore      int      `json:"conflict_score"`
	Status             string   `json:"status"`
	CreatedByUserID    *string  `json:"created_by_user_id,omitempty"`
	CreatedAtIso       string   `json:"created_at_iso"`
	UpdatedAtIso       string   `json:"updated_at_iso"`
	ResolvedByUserID   *string  `json:"resolved_by_user_id,omitempty"`
	ResolvedAtIso      *string  `json:"resolved_at_iso,omitempty"`
	ResolutionNotes    *string  `json:"resolution_notes,omitempty"`
}

type ZoneIncidentResponse struct {
	ID                  string   `json:"id"`
	ZoneID              string   `json:"zone_id"`
	BinID               string   `json:"bin_id"`
	BinNumber           *int     `json:"bin_number,omitempty"`           // Joined from bins table
	IncidentType        string   `json:"incident_type"`
	ReportedByUserID    *string  `json:"reported_by_user_id,omitempty"`
	ReportedByName      *string  `json:"reported_by_name,omitempty"`     // Joined from users table
	ReportedAtIso       string   `json:"reported_at_iso"`
	Description         *string  `json:"description,omitempty"`
	PhotoURL            *string  `json:"photo_url,omitempty"`
	CheckID             *int     `json:"check_id,omitempty"`
	MoveID              *int     `json:"move_id,omitempty"`
	ReporterLatitude    *float64 `json:"reporter_latitude,omitempty"`
	ReporterLongitude   *float64 `json:"reporter_longitude,omitempty"`
	IsFieldObservation  bool     `json:"is_field_observation"`
	VerifiedByUserID    *string  `json:"verified_by_user_id,omitempty"`
	VerifiedByName      *string  `json:"verified_by_name,omitempty"`     // Joined from users table
	VerifiedAtIso       *string  `json:"verified_at_iso,omitempty"`
	Status              string   `json:"status"`
}

type ZoneRiskOverrideResponse struct {
	ID               string  `json:"id"`
	ZoneID           string  `json:"zone_id"`
	ZoneName         *string `json:"zone_name,omitempty"`         // Joined from zones table
	BinID            string  `json:"bin_id"`
	BinNumber        *int    `json:"bin_number,omitempty"`        // Joined from bins table
	ManagerID        string  `json:"manager_id"`
	ManagerName      *string `json:"manager_name,omitempty"`      // Joined from users table
	OverrideReason   string  `json:"override_reason"`
	OverrideAtIso    string  `json:"override_at_iso"`
	ExpiresAtIso     *string `json:"expires_at_iso,omitempty"`
	Status           string  `json:"status"`
	IncidentCount    int     `json:"incident_count"`
	LastIncidentID   *string `json:"last_incident_id,omitempty"`
	DaysRemaining    *int    `json:"days_remaining,omitempty"`     // Computed field
}

// Convert models to response DTOs
func (z *NoGoZone) ToResponse() NoGoZoneResponse {
	resp := NoGoZoneResponse{
		ID:               z.ID,
		Name:             z.Name,
		CenterLatitude:   z.CenterLatitude,
		CenterLongitude:  z.CenterLongitude,
		RadiusMeters:     z.RadiusMeters,
		ConflictScore:    z.ConflictScore,
		Status:           z.Status,
		CreatedByUserID:  z.CreatedByUserID,
		CreatedAtIso:     time.Unix(z.CreatedAt, 0).Format(time.RFC3339),
		UpdatedAtIso:     time.Unix(z.UpdatedAt, 0).Format(time.RFC3339),
		ResolvedByUserID: z.ResolvedByUserID,
		ResolutionNotes:  z.ResolutionNotes,
	}

	if z.ResolvedAt != nil {
		iso := time.Unix(*z.ResolvedAt, 0).Format(time.RFC3339)
		resp.ResolvedAtIso = &iso
	}

	return resp
}

func (i *ZoneIncident) ToResponse() ZoneIncidentResponse {
	resp := ZoneIncidentResponse{
		ID:                 i.ID,
		ZoneID:             i.ZoneID,
		BinID:              i.BinID,
		IncidentType:       i.IncidentType,
		ReportedByUserID:   i.ReportedByUserID,
		ReportedAtIso:      time.Unix(i.ReportedAt, 0).Format(time.RFC3339),
		Description:        i.Description,
		PhotoURL:           i.PhotoURL,
		CheckID:            i.CheckID,
		MoveID:             i.MoveID,
		ReporterLatitude:   i.ReporterLatitude,
		ReporterLongitude:  i.ReporterLongitude,
		IsFieldObservation: i.IsFieldObservation,
		VerifiedByUserID:   i.VerifiedByUserID,
		Status:             i.Status,
	}

	if i.VerifiedAt != nil {
		iso := time.Unix(*i.VerifiedAt, 0).Format(time.RFC3339)
		resp.VerifiedAtIso = &iso
	}

	return resp
}

func (o *ZoneRiskOverride) ToResponse() ZoneRiskOverrideResponse {
	resp := ZoneRiskOverrideResponse{
		ID:              o.ID,
		ZoneID:          o.ZoneID,
		BinID:           o.BinID,
		ManagerID:       o.ManagerID,
		OverrideReason:  o.OverrideReason,
		OverrideAtIso:   time.Unix(o.OverrideAt, 0).Format(time.RFC3339),
		Status:          o.Status,
		IncidentCount:   o.IncidentCount,
		LastIncidentID:  o.LastIncidentID,
	}

	if o.ExpiresAt != nil {
		iso := time.Unix(*o.ExpiresAt, 0).Format(time.RFC3339)
		resp.ExpiresAtIso = &iso

		// Calculate days remaining
		now := time.Now().Unix()
		if *o.ExpiresAt > now {
			remaining := int((*o.ExpiresAt - now) / 86400)
			resp.DaysRemaining = &remaining
		} else {
			zero := 0
			resp.DaysRemaining = &zero
		}
	}

	return resp
}
