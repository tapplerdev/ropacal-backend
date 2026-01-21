package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jmoiron/sqlx"
)

// GetTopPerformingBins returns top bins by various metrics
func GetTopPerformingBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		metric := r.URL.Query().Get("metric") // reliability, fill_rate, uptime, check_count
		limitStr := r.URL.Query().Get("limit")

		if metric == "" {
			metric = "reliability"
		}

		limit := 10
		if limitStr != "" {
			if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 && parsed <= 100 {
				limit = parsed
			}
		}

		type BinPerformance struct {
			ID                string   `json:"id" db:"id"`
			BinNumber         int      `json:"bin_number" db:"bin_number"`
			CurrentStreet     string   `json:"current_street" db:"current_street"`
			City              string   `json:"city" db:"city"`
			Zip               string   `json:"zip" db:"zip"`
			DaysActive        int      `json:"days_active" db:"days_active"`
			TotalChecks       int      `json:"total_checks" db:"total_checks"`
			AvgFillPercentage *float64 `json:"avg_fill_percentage" db:"avg_fill_percentage"`
			IncidentCount     int      `json:"incident_count" db:"incident_count"`
			LastChecked       *int64   `json:"last_checked" db:"last_checked"`
			PerformanceScore  float64  `json:"performance_score" db:"performance_score"`
		}

		var query string
		switch metric {
		case "reliability":
			query = `
				SELECT
					b.id,
					b.bin_number,
					b.current_street,
					b.city,
					b.zip,
					(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 AS days_active,
					(SELECT COUNT(*) FROM checks WHERE bin_id = b.id) AS total_checks,
					(SELECT AVG(fill_percentage) FROM checks WHERE bin_id = b.id) AS avg_fill_percentage,
					(SELECT COUNT(*) FROM zone_incidents WHERE bin_id = b.id) AS incident_count,
					b.last_checked,
					CASE
						WHEN (SELECT COUNT(*) FROM zone_incidents WHERE bin_id = b.id) = 0
						THEN (
							(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 *
							GREATEST(1, (SELECT COUNT(*) FROM checks WHERE bin_id = b.id)::float / NULLIF((EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 604800, 0))
						)
						ELSE (
							(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 *
							GREATEST(1, (SELECT COUNT(*) FROM checks WHERE bin_id = b.id)::float / NULLIF((EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 604800, 0)) /
							(1 + (SELECT COUNT(*) FROM zone_incidents WHERE bin_id = b.id))
						)
					END AS performance_score
				FROM bins b
				WHERE b.status = 'active'
				ORDER BY performance_score DESC
				LIMIT $1
			`

		case "fill_rate":
			query = `
				SELECT
					b.id,
					b.bin_number,
					b.current_street,
					b.city,
					b.zip,
					(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 AS days_active,
					(SELECT COUNT(*) FROM checks WHERE bin_id = b.id) AS total_checks,
					(SELECT AVG(fill_percentage) FROM checks WHERE bin_id = b.id) AS avg_fill_percentage,
					(SELECT COUNT(*) FROM zone_incidents WHERE bin_id = b.id) AS incident_count,
					b.last_checked,
					COALESCE((SELECT AVG(fill_percentage) FROM checks WHERE bin_id = b.id), 0) AS performance_score
				FROM bins b
				WHERE b.status = 'active'
					AND EXISTS (SELECT 1 FROM checks WHERE bin_id = b.id)
				ORDER BY performance_score DESC
				LIMIT $1
			`

		case "uptime":
			query = `
				SELECT
					b.id,
					b.bin_number,
					b.current_street,
					b.city,
					b.zip,
					(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 AS days_active,
					(SELECT COUNT(*) FROM checks WHERE bin_id = b.id) AS total_checks,
					(SELECT AVG(fill_percentage) FROM checks WHERE bin_id = b.id) AS avg_fill_percentage,
					(SELECT COUNT(*) FROM zone_incidents WHERE bin_id = b.id) AS incident_count,
					b.last_checked,
					(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 AS performance_score
				FROM bins b
				WHERE b.status = 'active'
				ORDER BY performance_score DESC
				LIMIT $1
			`

		case "check_count":
			query = `
				SELECT
					b.id,
					b.bin_number,
					b.current_street,
					b.city,
					b.zip,
					(EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400 AS days_active,
					(SELECT COUNT(*) FROM checks WHERE bin_id = b.id) AS total_checks,
					(SELECT AVG(fill_percentage) FROM checks WHERE bin_id = b.id) AS avg_fill_percentage,
					(SELECT COUNT(*) FROM zone_incidents WHERE bin_id = b.id) AS incident_count,
					b.last_checked,
					(SELECT COUNT(*) FROM checks WHERE bin_id = b.id) AS performance_score
				FROM bins b
				WHERE b.status = 'active'
				ORDER BY performance_score DESC
				LIMIT $1
			`

		default:
			http.Error(w, "Invalid metric. Use: reliability, fill_rate, uptime, check_count", http.StatusBadRequest)
			return
		}

		var results []BinPerformance
		err := db.Select(&results, query, limit)
		if err != nil {
			http.Error(w, "Failed to fetch top performers", http.StatusInternalServerError)
			return
		}

		response := map[string]interface{}{
			"metric": metric,
			"limit":  limit,
			"bins":   results,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// GetAreaPerformance returns area/ZIP code performance metrics
func GetAreaPerformance(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		groupBy := r.URL.Query().Get("group_by") // zip, city
		metric := r.URL.Query().Get("metric")    // success_rate, fill_rate, check_frequency
		limitStr := r.URL.Query().Get("limit")

		if groupBy == "" {
			groupBy = "zip"
		}
		if metric == "" {
			metric = "success_rate"
		}

		limit := 20
		if limitStr != "" {
			if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 && parsed <= 100 {
				limit = parsed
			}
		}

		type AreaPerformance struct {
			GroupValue        string   `json:"group_value" db:"group_value"` // ZIP or city
			City              *string  `json:"city,omitempty" db:"city"`
			TotalBins         int      `json:"total_bins" db:"total_bins"`
			ActiveBins        int      `json:"active_bins" db:"active_bins"`
			CleanBins         int      `json:"clean_bins" db:"clean_bins"`
			ProblematicBins   int      `json:"problematic_bins" db:"problematic_bins"`
			AvgFillPercentage *float64 `json:"avg_fill_percentage" db:"avg_fill_percentage"`
			TotalChecks       int      `json:"total_checks" db:"total_checks"`
			TotalIncidents    int      `json:"total_incidents" db:"total_incidents"`
			SuccessRate       float64  `json:"success_rate" db:"success_rate"`
			AvgDaysActive     *float64 `json:"avg_days_active" db:"avg_days_active"`
			AreaScore         float64  `json:"area_score" db:"area_score"`
		}

		var groupColumn string
		var selectCity string
		if groupBy == "city" {
			groupColumn = "b.city"
			selectCity = "NULL AS city"
		} else {
			groupColumn = "b.zip"
			selectCity = "b.city"
		}

		var orderBy string
		switch metric {
		case "success_rate":
			orderBy = "success_rate DESC, total_bins DESC"
		case "fill_rate":
			orderBy = "avg_fill_percentage DESC"
		case "check_frequency":
			orderBy = "total_checks DESC"
		case "incident_rate":
			orderBy = "total_incidents ASC"
		default:
			orderBy = "success_rate DESC"
		}

		query := fmt.Sprintf(`
			SELECT
				%s AS group_value,
				%s,
				COUNT(DISTINCT b.id) AS total_bins,
				COUNT(DISTINCT CASE WHEN b.status = 'active' THEN b.id END) AS active_bins,
				COUNT(DISTINCT CASE
					WHEN NOT EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id)
					THEN b.id
				END) AS clean_bins,
				COUNT(DISTINCT CASE
					WHEN EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id)
					THEN b.id
				END) AS problematic_bins,
				(SELECT AVG(c.fill_percentage)
				 FROM checks c
				 JOIN bins b2 ON c.bin_id = b2.id
				 WHERE %s = %s) AS avg_fill_percentage,
				(SELECT COUNT(*)
				 FROM checks c
				 JOIN bins b2 ON c.bin_id = b2.id
				 WHERE %s = %s) AS total_checks,
				(SELECT COUNT(*)
				 FROM zone_incidents zi
				 JOIN bins b2 ON zi.bin_id = b2.id
				 WHERE %s = %s) AS total_incidents,
				ROUND(
					(COUNT(DISTINCT CASE
						WHEN NOT EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id)
						THEN b.id
					END)::float / NULLIF(COUNT(DISTINCT b.id)::float, 0)) * 100,
					2
				) AS success_rate,
				AVG((EXTRACT(EPOCH FROM NOW())::BIGINT - b.created_at) / 86400) AS avg_days_active,
				ROUND(
					(COUNT(DISTINCT CASE
						WHEN NOT EXISTS (SELECT 1 FROM zone_incidents zi WHERE zi.bin_id = b.id)
						THEN b.id
					END)::float / NULLIF(COUNT(DISTINCT b.id)::float, 0)) * 100 *
					GREATEST(1, (SELECT COUNT(*) FROM checks c JOIN bins b3 ON c.bin_id = b3.id WHERE %s = %s)::float / NULLIF(COUNT(DISTINCT b.id)::float * 4, 0)),
					2
				) AS area_score
			FROM bins b
			WHERE b.status = 'active'
			GROUP BY %s, %s
			ORDER BY %s
			LIMIT $1
		`, groupColumn, selectCity,
			groupColumn, groupColumn,
			groupColumn, groupColumn,
			groupColumn, groupColumn,
			groupColumn, groupColumn,
			groupColumn, selectCity, orderBy)

		var results []AreaPerformance
		err := db.Select(&results, query, limit)
		if err != nil {
			http.Error(w, "Failed to fetch area performance", http.StatusInternalServerError)
			return
		}

		response := map[string]interface{}{
			"group_by": groupBy,
			"metric":   metric,
			"limit":    limit,
			"areas":    results,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}
