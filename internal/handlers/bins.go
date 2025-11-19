package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func GetBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Auto-uncheck bins older than 3 days
		threeDaysAgo := time.Now().Add(-3 * 24 * time.Hour).Unix()
		_, err := db.Exec(`
			UPDATE bins
			SET checked = 0
			WHERE checked = 1 AND last_checked IS NOT NULL AND last_checked < $1
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

func CreateBin(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateBinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.BinNumber <= 0 || req.CurrentStreet == "" || req.City == "" || req.Zip == "" || req.Status == "" {
			http.Error(w, "Missing required fields", http.StatusBadRequest)
			return
		}

		// Generate UUID for new bin
		id := uuid.New().String()
		now := time.Now().Unix()

		// Insert bin
		_, err := db.Exec(`
			INSERT INTO bins (
				id, bin_number, current_street, city, zip, status,
				fill_percentage, checked, move_requested, latitude, longitude,
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`,
			id, req.BinNumber, req.CurrentStreet, req.City, req.Zip, req.Status,
			req.FillPercentage, 0, 0, req.Latitude, req.Longitude, now, now,
		)

		if err != nil {
			// Check if bin_number already exists
			if strings.Contains(err.Error(), "duplicate key") {
				http.Error(w, "Bin number already exists", http.StatusConflict)
				return
			}
			http.Error(w, "Failed to create bin", http.StatusInternalServerError)
			return
		}

		// Fetch created bin
		var created models.Bin
		err = db.Get(&created, "SELECT * FROM bins WHERE id = $1", id)
		if err != nil {
			http.Error(w, "Failed to fetch created bin", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created.ToBinResponse())
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
		err := db.Get(&existing, "SELECT * FROM bins WHERE id = $1", id)
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
			SET current_street = $1, city = $2, zip = $3, status = $4,
			    checked = $5, fill_percentage = $6, move_requested = $7`

		args := []interface{}{
			req.CurrentStreet, req.City, req.Zip, req.Status,
			req.Checked, req.FillPercentage, req.MoveRequested,
		}

		paramCount := 7
		if becomingChecked {
			paramCount++
			query += `, last_checked = $` + fmt.Sprintf("%d", paramCount)
			args = append(args, now.Unix())
		}

		if addrChanged {
			query += `, latitude = NULL, longitude = NULL`
		}

		paramCount++
		query += `, updated_at = $` + fmt.Sprintf("%d", paramCount) + ` WHERE id = $` + fmt.Sprintf("%d", paramCount+1)
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
				VALUES ($1, $2, $3, $4)
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
		err = db.Get(&updated, "SELECT * FROM bins WHERE id = $1", id)
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

		result, err := db.Exec("DELETE FROM bins WHERE id = $1", id)
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
