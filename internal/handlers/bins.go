package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func GetBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Auto-uncheck bins older than 3 days
		threeDaysAgo := time.Now().Add(-3 * 24 * time.Hour).Unix()
		_, err := db.Exec(`
			UPDATE bins
			SET checked = 0
			WHERE checked = 1 AND last_checked IS NOT NULL AND last_checked < ?
		`, threeDaysAgo)
		if err != nil {
			http.Error(w, "Failed to update bins", http.StatusInternalServerError)
			return
		}

		// Get all bins
		var bins []models.Bin
		err = db.Select(&bins, `
			SELECT id, bin_number, current_street, city, zip,
			       last_moved, last_checked, status, fill_percentage,
			       checked, move_requested, latitude, longitude,
			       created_at, updated_at
			FROM bins
			ORDER BY bin_number ASC
		`)
		if err != nil {
			http.Error(w, "Failed to fetch bins", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		responses := make([]models.BinResponse, len(bins))
		for i, bin := range bins {
			responses[i] = bin.ToBinResponse()
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

func UpdateBin(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		var req models.UpdateBinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Get existing bin
		var existing models.Bin
		err := db.Get(&existing, "SELECT * FROM bins WHERE id = ?", id)
		if err == sql.ErrNoRows {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		wasChecked := existing.Checked
		becomingChecked := req.Checked && !wasChecked

		// Determine check time
		now := time.Now()
		if req.CheckedOnIso != nil {
			if parsed, err := time.Parse(time.RFC3339, *req.CheckedOnIso); err == nil {
				now = parsed
			}
		}

		// Clamp fill percentage
		if req.FillPercentage != nil {
			val := *req.FillPercentage
			if val < 0 {
				val = 0
			}
			if val > 100 {
				val = 100
			}
			req.FillPercentage = &val
		}

		// Check if address changed
		addrChanged := strings.TrimSpace(req.CurrentStreet) != existing.CurrentStreet ||
			strings.TrimSpace(req.City) != existing.City ||
			strings.TrimSpace(req.Zip) != existing.Zip

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to begin transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Build update query
		query := `
			UPDATE bins
			SET current_street = ?, city = ?, zip = ?, status = ?,
			    checked = ?, fill_percentage = ?, move_requested = ?`

		args := []interface{}{
			req.CurrentStreet, req.City, req.Zip, req.Status,
			req.Checked, req.FillPercentage, req.MoveRequested,
		}

		if becomingChecked {
			query += `, last_checked = ?`
			args = append(args, now.Unix())
		}

		if addrChanged {
			query += `, latitude = NULL, longitude = NULL`
		}

		query += `, updated_at = ? WHERE id = ?`
		args = append(args, time.Now().Unix(), id)

		_, err = tx.Exec(query, args...)
		if err != nil {
			http.Error(w, "Failed to update bin", http.StatusInternalServerError)
			return
		}

		// If becoming checked, insert check record
		if becomingChecked {
			checkedFrom := ""
			if req.CheckedFrom != nil && strings.TrimSpace(*req.CheckedFrom) != "" {
				checkedFrom = *req.CheckedFrom
			} else {
				checkedFrom = req.CurrentStreet + ", " + req.City + " " + req.Zip
			}

			fillForCheck := 0
			if req.FillPercentage != nil {
				fillForCheck = *req.FillPercentage
			}

			_, err = tx.Exec(`
				INSERT INTO checks (bin_id, checked_from, fill_percentage, checked_on)
				VALUES (?, ?, ?, ?)
			`, id, checkedFrom, fillForCheck, now.Unix())
			if err != nil {
				http.Error(w, "Failed to create check record", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch updated bin
		var updated models.Bin
		err = db.Get(&updated, "SELECT * FROM bins WHERE id = ?", id)
		if err != nil {
			http.Error(w, "Failed to fetch updated bin", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated.ToBinResponse())
	}
}

func DeleteBin(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		result, err := db.Exec("DELETE FROM bins WHERE id = ?", id)
		if err != nil {
			http.Error(w, "Failed to delete", http.StatusInternalServerError)
			return
		}

		rows, err := result.RowsAffected()
		if err != nil || rows == 0 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
