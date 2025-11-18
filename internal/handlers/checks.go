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

		var checks []models.Check
		err := db.Select(&checks, `
			SELECT id, bin_id, checked_from, fill_percentage, checked_on
			FROM checks
			WHERE bin_id = ?
			ORDER BY checked_on DESC
		`, binID)
		if err != nil {
			http.Error(w, "Failed to fetch checks", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.CheckResponse, len(checks))
		for i, check := range checks {
			responses[i] = check.ToCheckResponse()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}
