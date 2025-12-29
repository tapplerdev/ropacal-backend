package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// GetRoutes returns all route blueprints
func GetRoutes(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var routes []models.Route
		err := db.Select(&routes, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			ORDER BY created_at DESC
		`)
		if err != nil {
			http.Error(w, "Failed to fetch routes", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(routes)
	}
}

// GetRoute returns a single route with its bins
func GetRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routeID := chi.URLParam(r, "id")

		// Get route
		var route models.Route
		err := db.Get(&route, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, routeID)
		if err == sql.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch route", http.StatusInternalServerError)
			return
		}

		// Get route bins with details
		type BinWithSequence struct {
			models.Bin
			SequenceOrder int `db:"sequence_order"`
		}

		var binsWithSequence []BinWithSequence
		err = db.Select(&binsWithSequence, `
			SELECT b.id, b.bin_number, b.current_street, b.city, b.zip,
			       b.last_moved, b.last_checked, b.status, b.fill_percentage,
			       b.checked, b.move_requested, b.latitude, b.longitude,
			       b.created_at, b.updated_at, rb.sequence_order
			FROM bins b
			INNER JOIN route_bins rb ON b.id = rb.bin_id
			WHERE rb.route_id = $1
			ORDER BY rb.sequence_order ASC
		`, routeID)
		if err != nil {
			http.Error(w, "Failed to fetch route bins", http.StatusInternalServerError)
			return
		}

		// Convert to response format
		bins := make([]models.BinInRoute, len(binsWithSequence))
		for i, bws := range binsWithSequence {
			bins[i] = models.BinInRoute{
				BinResponse:   bws.Bin.ToBinResponse(),
				SequenceOrder: bws.SequenceOrder,
			}
		}

		response := models.RouteWithBins{
			Route: route,
			Bins:  bins,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// CreateRoute creates a new route blueprint
func CreateRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.Name == "" || req.GeographicArea == "" || len(req.BinIDs) == 0 {
			http.Error(w, "Missing required fields: name, geographic_area, and bin_ids", http.StatusBadRequest)
			return
		}

		// Get user ID from context (set by auth middleware)
		userID, _ := r.Context().Value("user_id").(string)

		// Generate UUID and timestamp
		id := uuid.New().String()
		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Prepare optional fields
		var description *string
		if req.Description != "" {
			description = &req.Description
		}

		var schedulePattern *string
		if req.SchedulePattern != "" {
			schedulePattern = &req.SchedulePattern
		}

		var createdBy *string
		if userID != "" {
			createdBy = &userID
		}

		// Insert route
		_, err = tx.Exec(`
			INSERT INTO routes (
				id, name, description, geographic_area, schedule_pattern,
				bin_count, estimated_duration_hours, created_by_user_id,
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			id, req.Name, description, req.GeographicArea, schedulePattern,
			len(req.BinIDs), req.EstimatedDurationHours, createdBy, now, now,
		)
		if err != nil {
			http.Error(w, "Failed to create route", http.StatusInternalServerError)
			return
		}

		// Insert route_bins
		for i, binID := range req.BinIDs {
			_, err = tx.Exec(`
				INSERT INTO route_bins (route_id, bin_id, sequence_order, created_at)
				VALUES ($1, $2, $3, $4)
			`, id, binID, i+1, now)
			if err != nil {
				http.Error(w, "Failed to add bins to route", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch created route
		var created models.Route
		err = db.Get(&created, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, id)
		if err != nil {
			http.Error(w, "Failed to fetch created route", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)
	}
}

// UpdateRoute updates an existing route
func UpdateRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routeID := chi.URLParam(r, "id")

		var req models.UpdateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Build dynamic update query
		updates := []string{}
		args := []interface{}{}
		argCount := 1

		if req.Name != nil {
			updates = append(updates, "name = $"+string(rune('0'+argCount)))
			args = append(args, *req.Name)
			argCount++
		}
		if req.Description != nil {
			updates = append(updates, "description = $"+string(rune('0'+argCount)))
			args = append(args, *req.Description)
			argCount++
		}
		if req.GeographicArea != nil {
			updates = append(updates, "geographic_area = $"+string(rune('0'+argCount)))
			args = append(args, *req.GeographicArea)
			argCount++
		}
		if req.SchedulePattern != nil {
			updates = append(updates, "schedule_pattern = $"+string(rune('0'+argCount)))
			args = append(args, *req.SchedulePattern)
			argCount++
		}
		if req.EstimatedDurationHours != nil {
			updates = append(updates, "estimated_duration_hours = $"+string(rune('0'+argCount)))
			args = append(args, *req.EstimatedDurationHours)
			argCount++
		}

		// Update bin_ids if provided
		if req.BinIDs != nil {
			updates = append(updates, "bin_count = $"+string(rune('0'+argCount)))
			args = append(args, len(req.BinIDs))
			argCount++

			// Delete existing bin associations
			_, err = tx.Exec("DELETE FROM route_bins WHERE route_id = $1", routeID)
			if err != nil {
				http.Error(w, "Failed to update route bins", http.StatusInternalServerError)
				return
			}

			// Insert new bin associations
			for i, binID := range req.BinIDs {
				_, err = tx.Exec(`
					INSERT INTO route_bins (route_id, bin_id, sequence_order, created_at)
					VALUES ($1, $2, $3, $4)
				`, routeID, binID, i+1, now)
				if err != nil {
					http.Error(w, "Failed to add bins to route", http.StatusInternalServerError)
					return
				}
			}
		}

		// Always update updated_at
		updates = append(updates, "updated_at = $"+string(rune('0'+argCount)))
		args = append(args, now)
		argCount++

		// Add route ID as final parameter
		args = append(args, routeID)

		// Execute update if there are changes
		if len(updates) > 1 { // More than just updated_at
			query := "UPDATE routes SET " + joinStrings(updates, ", ") + " WHERE id = $" + string(rune('0'+argCount))
			_, err = tx.Exec(query, args...)
			if err != nil {
				http.Error(w, "Failed to update route", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch updated route
		var updated models.Route
		err = db.Get(&updated, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, routeID)
		if err == sql.ErrNoRows {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch updated route", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated)
	}
}

// DeleteRoute deletes a route blueprint
func DeleteRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routeID := chi.URLParam(r, "id")

		result, err := db.Exec("DELETE FROM routes WHERE id = $1", routeID)
		if err != nil {
			http.Error(w, "Failed to delete route", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			http.Error(w, "Route not found", http.StatusNotFound)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// DuplicateRoute creates a copy of an existing route
func DuplicateRoute(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sourceRouteID := chi.URLParam(r, "id")

		var req models.DuplicateRouteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Name == "" {
			http.Error(w, "Name is required", http.StatusBadRequest)
			return
		}

		// Get source route
		var sourceRoute models.Route
		err := db.Get(&sourceRoute, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, sourceRouteID)
		if err == sql.ErrNoRows {
			http.Error(w, "Source route not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "Failed to fetch source route", http.StatusInternalServerError)
			return
		}

		// Get source route bins
		var sourceBins []models.RouteBin
		err = db.Select(&sourceBins, `
			SELECT id, route_id, bin_id, sequence_order, created_at
			FROM route_bins
			WHERE route_id = $1
			ORDER BY sequence_order ASC
		`, sourceRouteID)
		if err != nil {
			http.Error(w, "Failed to fetch source route bins", http.StatusInternalServerError)
			return
		}

		// Get user ID from context
		userID, _ := r.Context().Value("user_id").(string)

		// Generate new UUID and timestamp
		newID := uuid.New().String()
		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		var createdBy *string
		if userID != "" {
			createdBy = &userID
		}

		// Create new route (duplicate)
		_, err = tx.Exec(`
			INSERT INTO routes (
				id, name, description, geographic_area, schedule_pattern,
				bin_count, estimated_duration_hours, created_by_user_id,
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			newID, req.Name, sourceRoute.Description, sourceRoute.GeographicArea,
			sourceRoute.SchedulePattern, sourceRoute.BinCount,
			sourceRoute.EstimatedDurationHours, createdBy, now, now,
		)
		if err != nil {
			http.Error(w, "Failed to create duplicate route", http.StatusInternalServerError)
			return
		}

		// Copy route bins
		for _, bin := range sourceBins {
			_, err = tx.Exec(`
				INSERT INTO route_bins (route_id, bin_id, sequence_order, created_at)
				VALUES ($1, $2, $3, $4)
			`, newID, bin.BinID, bin.SequenceOrder, now)
			if err != nil {
				http.Error(w, "Failed to copy route bins", http.StatusInternalServerError)
				return
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch created route
		var created models.Route
		err = db.Get(&created, `
			SELECT id, name, description, geographic_area, schedule_pattern,
			       bin_count, estimated_duration_hours, created_by_user_id,
			       created_at, updated_at
			FROM routes
			WHERE id = $1
		`, newID)
		if err != nil {
			http.Error(w, "Failed to fetch duplicated route", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created)
	}
}

// Helper function to join strings
func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	result := strs[0]
	for _, s := range strs[1:] {
		result += sep + s
	}
	return result
}
