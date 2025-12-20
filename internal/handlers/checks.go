package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func GetChecks(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "id")
		if binID == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Query with JOIN to get driver names
		type CheckWithName struct {
			models.Check
			CheckedByName *string `db:"checked_by_name"`
		}

		var checksWithNames []CheckWithName
		err := db.Select(&checksWithNames, `
			SELECT
				c.id,
				c.bin_id,
				c.checked_from,
				c.fill_percentage,
				c.checked_on,
				c.photo_url,
				c.checked_by,
				u.name AS checked_by_name
			FROM checks c
			LEFT JOIN users u ON c.checked_by = u.id
			WHERE c.bin_id = $1
			ORDER BY c.checked_on DESC
		`, binID)
		if err != nil {
			http.Error(w, "Failed to fetch checks", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.CheckResponse, len(checksWithNames))
		for i, checkWithName := range checksWithNames {
			response := checkWithName.Check.ToCheckResponse()
			response.CheckedByName = checkWithName.CheckedByName
			responses[i] = response
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// GetAllChecks returns all checks with optional filtering
// Query parameters:
//   - driver_id: filter by driver who performed the check
//   - start_date: filter checks after this date (RFC3339 format)
//   - end_date: filter checks before this date (RFC3339 format)
//   - has_photo: filter checks with photos (true/false)
//   - limit: max number of results (default 100, max 500)
//   - offset: pagination offset (default 0)
func GetAllChecks(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse query parameters
		driverID := r.URL.Query().Get("driver_id")
		startDate := r.URL.Query().Get("start_date")
		endDate := r.URL.Query().Get("end_date")
		hasPhoto := r.URL.Query().Get("has_photo")
		limitStr := r.URL.Query().Get("limit")
		offsetStr := r.URL.Query().Get("offset")

		// Default pagination
		limit := 100
		offset := 0

		if limitStr != "" {
			if parsedLimit, err := strconv.Atoi(limitStr); err == nil {
				if parsedLimit > 0 && parsedLimit <= 500 {
					limit = parsedLimit
				}
			}
		}

		if offsetStr != "" {
			if parsedOffset, err := strconv.Atoi(offsetStr); err == nil {
				if parsedOffset >= 0 {
					offset = parsedOffset
				}
			}
		}

		// Build dynamic query
		query := `
			SELECT
				c.id,
				c.bin_id,
				c.checked_from,
				c.fill_percentage,
				c.checked_on,
				c.photo_url,
				c.checked_by,
				u.name AS checked_by_name
			FROM checks c
			LEFT JOIN users u ON c.checked_by = u.id
			WHERE 1=1`

		args := []interface{}{}
		argCount := 0

		// Add driver filter
		if driverID != "" {
			argCount++
			query += fmt.Sprintf(" AND c.checked_by = $%d", argCount)
			args = append(args, driverID)
		}

		// Add start date filter
		if startDate != "" {
			if parsedTime, err := time.Parse(time.RFC3339, startDate); err == nil {
				argCount++
				query += fmt.Sprintf(" AND c.checked_on >= $%d", argCount)
				args = append(args, parsedTime.Unix())
			}
		}

		// Add end date filter
		if endDate != "" {
			if parsedTime, err := time.Parse(time.RFC3339, endDate); err == nil {
				argCount++
				query += fmt.Sprintf(" AND c.checked_on <= $%d", argCount)
				args = append(args, parsedTime.Unix())
			}
		}

		// Add has_photo filter
		if hasPhoto == "true" {
			query += " AND c.photo_url IS NOT NULL"
		} else if hasPhoto == "false" {
			query += " AND c.photo_url IS NULL"
		}

		// Add ordering, limit, offset
		query += " ORDER BY c.checked_on DESC"
		argCount++
		query += fmt.Sprintf(" LIMIT $%d", argCount)
		args = append(args, limit)
		argCount++
		query += fmt.Sprintf(" OFFSET $%d", argCount)
		args = append(args, offset)

		// Execute query
		type CheckWithName struct {
			models.Check
			CheckedByName *string `db:"checked_by_name"`
		}

		var checksWithNames []CheckWithName
		err := db.Select(&checksWithNames, query, args...)
		if err != nil {
			http.Error(w, "Failed to fetch checks", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.CheckResponse, len(checksWithNames))
		for i, checkWithName := range checksWithNames {
			response := checkWithName.Check.ToCheckResponse()
			response.CheckedByName = checkWithName.CheckedByName
			responses[i] = response
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}
