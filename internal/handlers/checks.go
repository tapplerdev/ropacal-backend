package handlers

import (
	"encoding/json"
	"net/http"

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
