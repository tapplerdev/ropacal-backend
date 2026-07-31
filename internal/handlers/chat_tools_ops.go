package handlers

import (
	"encoding/json"
	"fmt"
	"time"
)

// "What happened this week?" — the two tools that were missing.
//
// The chat could reason about bins, areas, checks, shifts, zones and potential
// locations, but was BLIND to the two objects an operator asks about most:
// move requests and incidents.
//
// Move requests had no tool at all, so "any move requests this week?" could only
// be answered with an honest "I can't see that". Incidents were worse, because
// get_no_go_zones exposes a LIFETIME incident_count per zone with no dates —
// a tool of the wrong shape invites a confident answer to a question it did not
// actually answer ("zone 3 has 12 incidents" is true, and reads like a reply to
// "today"). Both now read their tables directly, with a real time window.
//
// Pure reads on the caller's org-bound handle, so tenancy comes for free.

// reportTimezone is the zone dates are rendered in for chat answers.
//
// Every existing tool hardcodes America/Los_Angeles. For the Toronto
// organization that is three hours off, so an incident reported at 1am local
// renders as the PREVIOUS day — and "were there any incidents today" is exactly
// the question where a wrong day boundary gives a confidently wrong answer.
//
// Country-level approximation, deliberately. It is right for the two
// organizations that exist and strictly better than always-Pacific, but it is
// still wrong for a Vancouver Canadian org or a New York US one. The real fix is
// a per-organization timezone column; this is not that, and should be replaced
// rather than extended country by country.
func (h *ChatHandler) reportTimezone() *time.Location {
	name := "America/Los_Angeles"
	if organizationCountry(h.db) == "CA" {
		name = "America/Toronto"
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// opsWindowDays clamps a requested lookback. 0/absent means "no date filter" for
// move requests (an open request from six weeks ago is still open and still
// relevant), but incidents default to a week — "what happened" is a recent
// question, and an unbounded incident dump buries the answer.
func opsWindowDays(params map[string]any, def int) int {
	if d, ok := params["days"].(float64); ok && d > 0 {
		if d > 365 {
			return 365
		}
		return int(d)
	}
	return def
}

// toolGetMoveRequests answers "what moves are outstanding / happened recently".
func (h *ChatHandler) toolGetMoveRequests(params map[string]any) (string, error) {
	type moveRow struct {
		ID            string  `db:"id" json:"id"`
		BinNumber     *int    `db:"bin_number" json:"bin_number,omitempty"`
		Status        string  `db:"status" json:"status"`
		Urgency       string  `db:"urgency" json:"urgency"`
		MoveType      string  `db:"move_type" json:"move_type"`
		ScheduledDate int64   `db:"scheduled_date" json:"-"`
		ScheduledOn   string  `json:"scheduled_for"`
		FromAddress   string  `db:"original_address" json:"from_address"`
		ToAddress     *string `db:"new_address" json:"to_address,omitempty"`
		Reason        *string `db:"reason" json:"reason,omitempty"`
		AssignedTo    *string `db:"assigned_to" json:"assigned_to,omitempty"`
		CreatedAt     int64   `db:"created_at" json:"-"`
		CreatedOn     string  `json:"requested_on"`
		CompletedAt   *int64  `db:"completed_at" json:"-"`
		CompletedOn   *string `json:"completed_on,omitempty"`
	}

	query := `
		SELECT m.id, b.bin_number, m.status, m.urgency, m.move_type,
		       m.scheduled_date, m.original_address, m.new_address, m.reason,
		       u.name AS assigned_to, m.created_at, m.completed_at
		FROM bin_move_requests m
		LEFT JOIN bins b ON b.id = m.bin_id
		LEFT JOIN users u ON u.id = m.assigned_user_id
		WHERE 1=1`
	args := []any{}
	argIdx := 1

	// "open" is the operator's word, not a column value — it means everything
	// still needing action, which is the common question by a wide margin.
	switch s, _ := params["status"].(string); s {
	case "", "open":
		query += " AND m.status NOT IN ('completed','cancelled')"
	case "all":
	default:
		query += fmt.Sprintf(" AND m.status = $%d", argIdx)
		args = append(args, s)
		argIdx++
	}

	if u, ok := params["urgency"].(string); ok && u != "" {
		query += fmt.Sprintf(" AND m.urgency = $%d", argIdx)
		args = append(args, u)
		argIdx++
	}
	if mt, ok := params["move_type"].(string); ok && mt != "" {
		query += fmt.Sprintf(" AND m.move_type = $%d", argIdx)
		args = append(args, mt)
		argIdx++
	}
	// Window on created_at, not scheduled_date: "requests this week" means when
	// they were RAISED. A move raised today for next month is this week's news.
	if days := opsWindowDays(params, 0); days > 0 {
		query += fmt.Sprintf(" AND m.created_at >= $%d", argIdx)
		args = append(args, time.Now().AddDate(0, 0, -days).Unix())
		argIdx++
	}
	query += " ORDER BY m.created_at DESC LIMIT 100"

	var rows []moveRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		return "", fmt.Errorf("move request query failed: %w", err)
	}

	loc := h.reportTimezone()
	for i := range rows {
		rows[i].ScheduledOn = time.Unix(rows[i].ScheduledDate, 0).In(loc).Format("Jan 2, 2006")
		rows[i].CreatedOn = time.Unix(rows[i].CreatedAt, 0).In(loc).Format("Jan 2, 2006")
		if rows[i].CompletedAt != nil {
			s := time.Unix(*rows[i].CompletedAt, 0).In(loc).Format("Jan 2, 2006")
			rows[i].CompletedOn = &s
		}
	}

	out, err := json.Marshal(map[string]any{"count": len(rows), "move_requests": rows})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// toolGetIncidents answers "were there any incidents", with dates.
func (h *ChatHandler) toolGetIncidents(params map[string]any) (string, error) {
	type incidentRow struct {
		ID           string  `db:"id" json:"id"`
		IncidentType string  `db:"incident_type" json:"type"`
		Status       string  `db:"status" json:"status"`
		ReportedAt   int64   `db:"reported_at" json:"-"`
		ReportedOn   string  `json:"reported_on"`
		Description  *string `db:"description" json:"description,omitempty"`
		ZoneName     *string `db:"zone_name" json:"zone,omitempty"`
		BinNumber    *int    `db:"bin_number" json:"bin_number,omitempty"`
		ReportedBy   *string `db:"reported_by" json:"reported_by,omitempty"`
		FieldObs     bool    `db:"is_field_observation" json:"is_field_observation"`
	}

	query := `
		SELECT i.id, i.incident_type, i.status, i.reported_at, i.description,
		       z.name AS zone_name, b.bin_number, u.name AS reported_by,
		       i.is_field_observation
		FROM zone_incidents i
		LEFT JOIN no_go_zones z ON z.id = i.zone_id
		LEFT JOIN bins b ON b.id = i.bin_id
		LEFT JOIN users u ON u.id = i.reported_by_user_id
		WHERE 1=1`
	args := []any{}
	argIdx := 1

	if t, ok := params["incident_type"].(string); ok && t != "" {
		query += fmt.Sprintf(" AND i.incident_type = $%d", argIdx)
		args = append(args, t)
		argIdx++
	}
	if s, ok := params["status"].(string); ok && s != "" && s != "all" {
		query += fmt.Sprintf(" AND i.status = $%d", argIdx)
		args = append(args, s)
		argIdx++
	}
	// Defaults to 7 days. "Any incidents?" is a recent question, and an
	// unbounded dump buries the answer it was asked for.
	days := opsWindowDays(params, 7)
	query += fmt.Sprintf(" AND i.reported_at >= $%d", argIdx)
	args = append(args, time.Now().AddDate(0, 0, -days).Unix())
	argIdx++

	query += " ORDER BY i.reported_at DESC LIMIT 100"

	var rows []incidentRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		return "", fmt.Errorf("incident query failed: %w", err)
	}

	loc := h.reportTimezone()
	for i := range rows {
		rows[i].ReportedOn = time.Unix(rows[i].ReportedAt, 0).In(loc).Format("Jan 2, 2006 3:04 PM")
	}

	// The window is echoed back so the model can state what it actually looked
	// at. Without it, "no incidents" is ambiguous between "none happened" and
	// "none in the range I happened to check".
	out, err := json.Marshal(map[string]any{
		"count":          len(rows),
		"window_days":    days,
		"searched_since": time.Now().AddDate(0, 0, -days).In(loc).Format("Jan 2, 2006"),
		"incidents":      rows,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
