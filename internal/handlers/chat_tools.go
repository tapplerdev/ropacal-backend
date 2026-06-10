package handlers

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// toolSearchBins searches bins by city, status, fill level, days since check
func (h *ChatHandler) toolSearchBins(params map[string]any) (string, error) {
	query := `SELECT bin_number, current_street, city, zip, status, fill_percentage, latitude, longitude, last_checked_at FROM bins WHERE 1=1`
	args := []any{}
	argIdx := 1

	if city, ok := params["city"].(string); ok && city != "" {
		query += fmt.Sprintf(" AND LOWER(city) = LOWER($%d)", argIdx)
		args = append(args, city)
		argIdx++
	}
	if status, ok := params["status"].(string); ok && status != "" {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, status)
		argIdx++
	}
	if minFill, ok := params["min_fill_percentage"].(float64); ok {
		query += fmt.Sprintf(" AND fill_percentage >= $%d", argIdx)
		args = append(args, int(minFill))
		argIdx++
	}
	if maxFill, ok := params["max_fill_percentage"].(float64); ok {
		query += fmt.Sprintf(" AND fill_percentage <= $%d", argIdx)
		args = append(args, int(maxFill))
		argIdx++
	}
	if daysSince, ok := params["days_since_check_min"].(float64); ok {
		cutoff := time.Now().Unix() - int64(daysSince)*86400
		query += fmt.Sprintf(" AND (last_checked_at IS NULL OR last_checked_at < $%d)", argIdx)
		args = append(args, cutoff)
		argIdx++
	}

	limit := 20
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
		if limit > 50 {
			limit = 50
		}
	}
	query += fmt.Sprintf(" ORDER BY bin_number LIMIT $%d", argIdx)
	args = append(args, limit)

	type binRow struct {
		BinNumber      int      `db:"bin_number" json:"bin_number"`
		CurrentStreet  string   `db:"current_street" json:"current_street"`
		City           string   `db:"city" json:"city"`
		Zip            string   `db:"zip" json:"zip"`
		Status         string   `db:"status" json:"status"`
		FillPercentage *int     `db:"fill_percentage" json:"fill_percentage"`
		Latitude       *float64 `db:"latitude" json:"latitude,omitempty"`
		Longitude      *float64 `db:"longitude" json:"longitude,omitempty"`
		LastCheckedAt  *int64   `db:"last_checked_at" json:"last_checked_at,omitempty"`
	}

	var rows []binRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	// Enrich with days since check
	type enrichedBin struct {
		binRow
		DaysSinceCheck *int `json:"days_since_check,omitempty"`
	}
	now := time.Now().Unix()
	results := make([]enrichedBin, len(rows))
	for i, r := range rows {
		results[i] = enrichedBin{binRow: r}
		if r.LastCheckedAt != nil {
			days := int((now - *r.LastCheckedAt) / 86400)
			results[i].DaysSinceCheck = &days
		}
	}

	b, _ := json.Marshal(map[string]any{"count": len(results), "bins": results})
	return string(b), nil
}

// toolGetAreaPerformance gets area-level metrics
func (h *ChatHandler) toolGetAreaPerformance(params map[string]any) (string, error) {
	groupBy := "city"
	if g, ok := params["group_by"].(string); ok && (g == "city" || g == "zip") {
		groupBy = g
	}

	metric := "success_rate"
	if m, ok := params["metric"].(string); ok && m != "" {
		metric = m
	}

	limit := 20
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
		if limit > 50 {
			limit = 50
		}
	}

	var groupColumn, selectCity string
	if groupBy == "city" {
		groupColumn = "b.city"
		selectCity = "NULL AS city"
	} else {
		groupColumn = "b.zip"
		selectCity = "b.city"
	}

	var orderBy string
	switch metric {
	case "fill_rate":
		orderBy = "avg_fill_percentage DESC"
	case "check_frequency":
		orderBy = "total_checks DESC"
	case "incident_rate":
		orderBy = "total_incidents ASC"
	default:
		orderBy = "success_rate DESC, total_bins DESC"
	}

	query := fmt.Sprintf(`
		SELECT
			%s AS group_value, %s,
			COUNT(DISTINCT b.id) AS total_bins,
			COUNT(DISTINCT CASE WHEN b.status = 'active' THEN b.id END) AS active_bins,
			COUNT(DISTINCT CASE WHEN NOT EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id) THEN b.id END) AS clean_bins,
			COUNT(DISTINCT CASE WHEN EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id) THEN b.id END) AS problematic_bins,
			(SELECT AVG(c.fill_percentage) FROM checks c JOIN bins b2 ON c.bin_id = b2.id WHERE %s = %s) AS avg_fill_percentage,
			(SELECT COUNT(*) FROM checks c JOIN bins b2 ON c.bin_id = b2.id WHERE %s = %s) AS total_checks,
			(SELECT COUNT(*) FROM zone_incidents zi JOIN bins b2 ON zi.bin_id = b2.id WHERE %s = %s) AS total_incidents,
			ROUND(
				(COUNT(DISTINCT CASE WHEN NOT EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id) THEN b.id END)::float
				/ NULLIF(COUNT(DISTINCT b.id)::float, 0)) * 100, 2
			) AS success_rate
		FROM bins b
		WHERE b.status = 'active'
		GROUP BY %s, %s
		ORDER BY %s
		LIMIT $1
	`, groupColumn, selectCity,
		groupColumn, groupColumn,
		groupColumn, groupColumn,
		groupColumn, groupColumn,
		groupColumn, selectCity, orderBy)

	type areaRow struct {
		GroupValue        string   `db:"group_value" json:"area"`
		City              *string  `db:"city" json:"city,omitempty"`
		TotalBins         int      `db:"total_bins" json:"total_bins"`
		ActiveBins        int      `db:"active_bins" json:"active_bins"`
		CleanBins         int      `db:"clean_bins" json:"clean_bins"`
		ProblematicBins   int      `db:"problematic_bins" json:"problematic_bins"`
		AvgFillPercentage *float64 `db:"avg_fill_percentage" json:"avg_fill_percentage"`
		TotalChecks       int      `db:"total_checks" json:"total_checks"`
		TotalIncidents    int      `db:"total_incidents" json:"total_incidents"`
		SuccessRate       float64  `db:"success_rate" json:"success_rate"`
	}

	var rows []areaRow
	if err := h.db.Select(&rows, query, limit); err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	b, _ := json.Marshal(map[string]any{"group_by": groupBy, "count": len(rows), "areas": rows})
	return string(b), nil
}

// toolGetBinCheckHistory gets check history for a specific bin
func (h *ChatHandler) toolGetBinCheckHistory(params map[string]any) (string, error) {
	limit := 10
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
		if limit > 50 {
			limit = 50
		}
	}

	var binID string

	// Look up by bin_number first
	if binNum, ok := params["bin_number"].(float64); ok {
		err := h.db.Get(&binID, "SELECT id FROM bins WHERE bin_number = $1", int(binNum))
		if err != nil {
			return "", fmt.Errorf("bin #%d not found", int(binNum))
		}
	} else if id, ok := params["bin_id"].(string); ok && id != "" {
		binID = id
	} else {
		return "", fmt.Errorf("either bin_number or bin_id is required")
	}

	type checkRow struct {
		FillPercentage int    `db:"fill_percentage" json:"fill_percentage"`
		CheckedOn      int64  `db:"checked_on" json:"checked_on"`
		CheckedOnDate  string `json:"checked_on_date"`
		PhotoURL       *string `db:"photo_url" json:"photo_url,omitempty"`
	}

	var rows []checkRow
	err := h.db.Select(&rows, `
		SELECT fill_percentage, checked_on, photo_url
		FROM checks
		WHERE bin_id = $1 AND fill_percentage IS NOT NULL
		ORDER BY checked_on DESC
		LIMIT $2
	`, binID, limit)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	// Add human-readable dates
	loc, _ := time.LoadLocation("America/Los_Angeles")
	for i := range rows {
		rows[i].CheckedOnDate = time.Unix(rows[i].CheckedOn, 0).In(loc).Format("Jan 2, 2006 3:04 PM")
	}

	// Get bin info
	type binInfo struct {
		BinNumber     int    `db:"bin_number" json:"bin_number"`
		CurrentStreet string `db:"current_street" json:"current_street"`
		City          string `db:"city" json:"city"`
		Status        string `db:"status" json:"status"`
	}
	var info binInfo
	h.db.Get(&info, "SELECT bin_number, current_street, city, status FROM bins WHERE id = $1", binID)

	b, _ := json.Marshal(map[string]any{"bin": info, "check_count": len(rows), "checks": rows})
	return string(b), nil
}

// toolGetNoGoZones gets no-go zones with optional proximity filter
func (h *ChatHandler) toolGetNoGoZones(params map[string]any) (string, error) {
	status := "active"
	if s, ok := params["status"].(string); ok && s != "" {
		status = s
	}

	type zoneRow struct {
		ID              string  `db:"id" json:"id"`
		Name            string  `db:"name" json:"name"`
		CenterLatitude  float64 `db:"center_latitude" json:"center_latitude"`
		CenterLongitude float64 `db:"center_longitude" json:"center_longitude"`
		RadiusMeters    int     `db:"radius_meters" json:"radius_meters"`
		ConflictScore   int     `db:"conflict_score" json:"conflict_score"`
		Status          string  `db:"status" json:"status"`
		IncidentCount   int     `db:"incident_count" json:"incident_count"`
	}

	var rows []zoneRow
	var err error

	if status == "all" {
		err = h.db.Select(&rows, `
			SELECT z.id, z.name, z.center_latitude, z.center_longitude, z.radius_meters, z.conflict_score, z.status,
				(SELECT COUNT(*) FROM zone_incidents zi WHERE zi.zone_id = z.id) AS incident_count
			FROM no_go_zones z
			WHERE z.merged_into_zone_id IS NULL
			ORDER BY z.created_at DESC
			LIMIT 50
		`)
	} else {
		err = h.db.Select(&rows, `
			SELECT z.id, z.name, z.center_latitude, z.center_longitude, z.radius_meters, z.conflict_score, z.status,
				(SELECT COUNT(*) FROM zone_incidents zi WHERE zi.zone_id = z.id) AS incident_count
			FROM no_go_zones z
			WHERE z.status = $1 AND z.merged_into_zone_id IS NULL
			ORDER BY z.created_at DESC
			LIMIT 50
		`, status)
	}
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	// Apply proximity filter if coordinates provided
	nearLat, hasLat := params["near_lat"].(float64)
	nearLng, hasLng := params["near_lng"].(float64)
	if hasLat && hasLng {
		radius := 2000.0
		if r, ok := params["near_radius_meters"].(float64); ok && r > 0 {
			radius = r
		}

		var filtered []zoneRow
		for _, z := range rows {
			dist := haversineMetersChat(nearLat, nearLng, z.CenterLatitude, z.CenterLongitude)
			if dist <= radius {
				filtered = append(filtered, z)
			}
		}
		rows = filtered
	}

	b, _ := json.Marshal(map[string]any{"count": len(rows), "zones": rows})
	return string(b), nil
}

// toolGetShiftHistory gets completed shift history
func (h *ChatHandler) toolGetShiftHistory(params map[string]any) (string, error) {
	daysBack := 30
	if d, ok := params["days_back"].(float64); ok && d > 0 {
		daysBack = int(d)
		if daysBack > 365 {
			daysBack = 365
		}
	}

	limit := 20
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
		if limit > 50 {
			limit = 50
		}
	}

	cutoff := time.Now().Unix() - int64(daysBack)*86400

	type shiftRow struct {
		ShiftDate      string   `db:"shift_date" json:"shift_date"`
		DriverName     string   `db:"driver_name" json:"driver_name"`
		Status         string   `db:"status" json:"status"`
		TotalTasks     int      `db:"total_tasks" json:"total_tasks"`
		CompletedTasks int      `db:"completed_tasks" json:"completed_tasks"`
		DurationMins   *float64 `db:"duration_mins" json:"duration_mins,omitempty"`
		DistanceMiles  *float64 `db:"distance_miles" json:"distance_miles,omitempty"`
		EndReason      *string  `db:"end_reason" json:"end_reason,omitempty"`
	}

	query := `
		SELECT
			TO_CHAR(TO_TIMESTAMP(sh.started_at) AT TIME ZONE 'America/Los_Angeles', 'YYYY-MM-DD') AS shift_date,
			u.name AS driver_name,
			sh.status,
			sh.total_tasks,
			sh.completed_tasks,
			CASE WHEN sh.ended_at IS NOT NULL AND sh.started_at IS NOT NULL
				THEN ROUND((sh.ended_at - sh.started_at)::numeric / 60, 1)
			END AS duration_mins,
			sh.total_distance_miles AS distance_miles,
			sh.end_reason
		FROM shift_history sh
		JOIN users u ON sh.driver_id = u.id
		WHERE sh.started_at >= $1
	`
	args := []any{cutoff}
	argIdx := 2

	if driverName, ok := params["driver_name"].(string); ok && driverName != "" {
		query += fmt.Sprintf(" AND LOWER(u.name) LIKE LOWER($%d)", argIdx)
		args = append(args, "%"+driverName+"%")
		argIdx++
	}

	query += fmt.Sprintf(" ORDER BY sh.started_at DESC LIMIT $%d", argIdx)
	args = append(args, limit)

	var rows []shiftRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	b, _ := json.Marshal(map[string]any{"count": len(rows), "shifts": rows})
	return string(b), nil
}

// toolGetPotentialLocations gets driver-suggested bin locations
func (h *ChatHandler) toolGetPotentialLocations(params map[string]any) (string, error) {
	status := "active"
	if s, ok := params["status"].(string); ok && s != "" {
		status = s
	}

	type locRow struct {
		ID              string   `db:"id" json:"id"`
		Street          string   `db:"street" json:"street"`
		City            string   `db:"city" json:"city"`
		Zip             string   `db:"zip" json:"zip"`
		Latitude        *float64 `db:"latitude" json:"latitude,omitempty"`
		Longitude       *float64 `db:"longitude" json:"longitude,omitempty"`
		RequestedByName string   `db:"requested_by_name" json:"requested_by"`
		Notes           *string  `db:"notes" json:"notes,omitempty"`
		CreatedAt       int64    `db:"created_at" json:"created_at"`
		CreatedDate     string   `json:"created_date"`
	}

	query := `SELECT id, street, city, zip, latitude, longitude, requested_by_name, notes, created_at FROM potential_locations WHERE 1=1`
	args := []any{}
	argIdx := 1

	if status == "active" {
		query += " AND converted_at IS NULL"
	} else if status == "converted" {
		query += " AND converted_at IS NOT NULL"
	}

	if city, ok := params["city"].(string); ok && city != "" {
		query += fmt.Sprintf(" AND LOWER(city) = LOWER($%d)", argIdx)
		args = append(args, city)
		argIdx++
	}

	query += " ORDER BY created_at DESC LIMIT 30"

	var rows []locRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	loc, _ := time.LoadLocation("America/Los_Angeles")
	for i := range rows {
		rows[i].CreatedDate = time.Unix(rows[i].CreatedAt, 0).In(loc).Format("Jan 2, 2006")
	}

	b, _ := json.Marshal(map[string]any{"count": len(rows), "locations": rows})
	return string(b), nil
}

// haversineMetersChat calculates distance in meters between two coordinates
func haversineMetersChat(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadius * c
}

