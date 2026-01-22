package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/websocket"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// GetPotentialLocations returns all potential locations (active or converted based on status query param)
// GET /api/potential-locations?status=active|converted
func GetPotentialLocations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")

		var whereClause string
		if status == "converted" {
			whereClause = "WHERE pl.converted_at IS NOT NULL"
		} else {
			// Default: show only active (non-converted) locations
			whereClause = "WHERE pl.converted_at IS NULL"
		}

		query := fmt.Sprintf(`
			SELECT
				pl.*,
				b.bin_number
			FROM potential_locations pl
			LEFT JOIN bins b ON b.id = pl.converted_to_bin_id
			%s
			ORDER BY pl.created_at DESC
		`, whereClause)

		rows, err := db.Query(query)
		if err != nil {
			log.Printf("❌ [GET-POTENTIAL-LOCATIONS] Database query failed: %v", err)
			http.Error(w, "Failed to fetch potential locations", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var locations []models.PotentialLocationResponse
		for rows.Next() {
			var loc models.PotentialLocation
			var binNumber *int

			err := rows.Scan(
				&loc.ID,
				&loc.Address,
				&loc.Street,
				&loc.City,
				&loc.Zip,
				&loc.Latitude,
				&loc.Longitude,
				&loc.RequestedByUserID,
				&loc.RequestedByName,
				&loc.Notes,
				&loc.CreatedAt,
				&loc.UpdatedAt,
				&loc.ConvertedToBinID,
				&loc.ConvertedAt,
				&loc.ConvertedByUserID,
				&binNumber,
			)
			if err != nil {
				log.Printf("❌ [GET-POTENTIAL-LOCATIONS] Row scan failed: %v", err)
				continue
			}

			resp := loc.ToPotentialLocationResponse()
			resp.BinNumber = binNumber
			locations = append(locations, resp)
		}

		if locations == nil {
			locations = []models.PotentialLocationResponse{}
		}

		log.Printf("✅ [GET-POTENTIAL-LOCATIONS] Returned %d locations (status: %s)", len(locations), status)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(locations)
	}
}

// CreatePotentialLocation creates one or more potential location requests
// POST /api/potential-locations (requires authentication)
// Accepts either a single object or an array of objects
// Always returns an array of created locations
func CreatePotentialLocation(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get user from context (set by auth middleware)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized: user not found in context", http.StatusUnauthorized)
			return
		}

		userID := userClaims.UserID
		userName := userClaims.Email

		// Get full user name from database
		var fullName string
		err := db.Get(&fullName, "SELECT name FROM users WHERE id = $1", userID)
		if err != nil {
			log.Printf("❌ [CREATE-POTENTIAL-LOCATION] Failed to get user name: %v", err)
			// Fallback to email if name lookup fails
			fullName = userName
		} else {
			userName = fullName
		}

		// Read raw JSON to detect if it's array or object
		var rawMessage json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&rawMessage); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Parse as array or single object
		var requests []models.CreatePotentialLocationRequest

		// Try to unmarshal as array first
		if err := json.Unmarshal(rawMessage, &requests); err != nil {
			// If that fails, try as single object
			var singleReq models.CreatePotentialLocationRequest
			if err := json.Unmarshal(rawMessage, &singleReq); err != nil {
				http.Error(w, "Invalid request body format", http.StatusBadRequest)
				return
			}
			requests = []models.CreatePotentialLocationRequest{singleReq}
		}

		// Validate we have at least one request
		if len(requests) == 0 {
			http.Error(w, "At least one location is required", http.StatusBadRequest)
			return
		}

		// Validate all requests
		for i, req := range requests {
			if req.Street == "" || req.City == "" || req.Zip == "" {
				http.Error(w, fmt.Sprintf("Missing required fields at index %d (street, city, zip)", i), http.StatusBadRequest)
				return
			}
		}

		// Begin transaction
		tx, err := db.Begin()
		if err != nil {
			log.Printf("❌ [CREATE-POTENTIAL-LOCATION] Transaction begin failed: %v", err)
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		now := time.Now().Unix()
		var createdIDs []string
		var createdLocations []models.PotentialLocationResponse

		// Insert all locations
		for _, req := range requests {
			// Build full address
			address := fmt.Sprintf("%s, %s, %s", req.Street, req.City, req.Zip)

			// Generate UUID
			id := uuid.New().String()
			createdIDs = append(createdIDs, id)

			// Insert potential location
			_, err = tx.Exec(`
				INSERT INTO potential_locations (
					id, address, street, city, zip, latitude, longitude,
					requested_by_user_id, requested_by_name, notes,
					created_at, updated_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			`,
				id, address, req.Street, req.City, req.Zip, req.Latitude, req.Longitude,
				userID, userName, req.Notes,
				now, now,
			)

			if err != nil {
				log.Printf("❌ [CREATE-POTENTIAL-LOCATION] Database insert failed: %v", err)
				http.Error(w, "Failed to create potential location", http.StatusInternalServerError)
				return
			}

			log.Printf("✅ [CREATE-POTENTIAL-LOCATION] Created location (ID: %s) at %s by %s", id, address, userName)
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ [CREATE-POTENTIAL-LOCATION] Transaction commit failed: %v", err)
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch all created locations
		for _, id := range createdIDs {
			var created models.PotentialLocation
			err = db.Get(&created, "SELECT * FROM potential_locations WHERE id = $1", id)
			if err != nil {
				log.Printf("❌ [CREATE-POTENTIAL-LOCATION] Failed to fetch created location: %v", err)
				http.Error(w, "Failed to fetch created location", http.StatusInternalServerError)
				return
			}

			resp := created.ToPotentialLocationResponse()
			createdLocations = append(createdLocations, resp)

			// Broadcast each location to all managers
			wsHub.BroadcastToRole("admin", map[string]interface{}{
				"type": "potential_location_created",
				"data": resp,
			})
		}

		log.Printf("✅ [CREATE-POTENTIAL-LOCATION] Created %d location(s), broadcasted to managers", len(createdLocations))

		// Always return array
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(createdLocations)
	}
}

// DeletePotentialLocation removes a potential location (hard delete)
// DELETE /api/potential-locations/:id (requires admin role)
func DeletePotentialLocation(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing location ID", http.StatusBadRequest)
			return
		}

		// Check if location exists
		var exists bool
		err := db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM potential_locations WHERE id = $1)", id)
		if err != nil {
			log.Printf("❌ [DELETE-POTENTIAL-LOCATION] Database check failed: %v", err)
			http.Error(w, "Failed to check location existence", http.StatusInternalServerError)
			return
		}

		if !exists {
			http.Error(w, "Potential location not found", http.StatusNotFound)
			return
		}

		// Delete location
		_, err = db.Exec("DELETE FROM potential_locations WHERE id = $1", id)
		if err != nil {
			log.Printf("❌ [DELETE-POTENTIAL-LOCATION] Database delete failed: %v", err)
			http.Error(w, "Failed to delete potential location", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [DELETE-POTENTIAL-LOCATION] Deleted location (ID: %s)", id)

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "potential_location_deleted",
			"data": map[string]interface{}{
				"location_id": id,
			},
		})
		log.Printf("📤 [DELETE-POTENTIAL-LOCATION] WebSocket event broadcasted to managers")

		w.WriteHeader(http.StatusNoContent)
	}
}

// ConvertPotentialLocationToBin converts a potential location to an active bin
// POST /api/potential-locations/:id/convert (requires admin role)
func ConvertPotentialLocationToBin(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing location ID", http.StatusBadRequest)
			return
		}

		// Parse optional request body
		var req models.ConvertToBinRequest
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&req)
		}

		// Get user from context (manager who is converting)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized: user not found in context", http.StatusUnauthorized)
			return
		}

		userID := userClaims.UserID

		// Begin transaction
		tx, err := db.Begin()
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Transaction begin failed: %v", err)
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Fetch potential location
		var location models.PotentialLocation
		err = tx.QueryRow(`
			SELECT id, street, city, zip, latitude, longitude, requested_by_user_id
			FROM potential_locations
			WHERE id = $1 AND converted_at IS NULL
		`, id).Scan(
			&location.ID,
			&location.Street,
			&location.City,
			&location.Zip,
			&location.Latitude,
			&location.Longitude,
			&location.RequestedByUserID,
		)

		if err == sql.ErrNoRows {
			http.Error(w, "Potential location not found or already converted", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to fetch location: %v", err)
			http.Error(w, "Failed to fetch potential location", http.StatusInternalServerError)
			return
		}

		// Auto-assign bin number
		var maxBinNumber sql.NullInt64
		err = tx.QueryRow("SELECT MAX(bin_number) FROM bins").Scan(&maxBinNumber)
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to get max bin_number: %v", err)
			http.Error(w, "Failed to generate bin number", http.StatusInternalServerError)
			return
		}

		binNumber := 1
		if maxBinNumber.Valid {
			binNumber = int(maxBinNumber.Int64) + 1
		}

		// Create bin
		binID := uuid.New().String()
		now := time.Now().Unix()

		_, err = tx.Exec(`
			INSERT INTO bins (
				id, bin_number, current_street, city, zip, status,
				fill_percentage, checked, move_requested, latitude, longitude,
				created_by_user_id, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`,
			binID, binNumber, location.Street, location.City, location.Zip, "active",
			req.FillPercentage, 0, 0, location.Latitude, location.Longitude,
			&userID, now, now,
		)

		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to create bin: %v", err)
			http.Error(w, "Failed to create bin", http.StatusInternalServerError)
			return
		}

		// Update potential location to mark as converted (soft delete)
		_, err = tx.Exec(`
			UPDATE potential_locations
			SET converted_to_bin_id = $1,
			    converted_at = $2,
			    converted_by_user_id = $3,
			    updated_at = $2
			WHERE id = $4
		`, binID, now, userID, id)

		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to update location: %v", err)
			http.Error(w, "Failed to update potential location", http.StatusInternalServerError)
			return
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Transaction commit failed: %v", err)
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		// Fetch created bin
		var createdBin models.Bin
		err = db.Get(&createdBin, "SELECT * FROM bins WHERE id = $1", binID)
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to fetch created bin: %v", err)
			http.Error(w, "Failed to fetch created bin", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Converted location (ID: %s) to Bin #%d (ID: %s)", id, binNumber, binID)

		// Broadcast to all managers (both location removed and bin created)
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "potential_location_converted",
			"data": map[string]interface{}{
				"location_id": id,
				"bin":         createdBin.ToBinResponse(),
			},
		})
		log.Printf("📤 [CONVERT-POTENTIAL-LOCATION] WebSocket event broadcasted to managers")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(createdBin.ToBinResponse())
	}
}
