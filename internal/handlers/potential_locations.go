package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services/centrifugo"
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
		log.Printf("🔍 [GET-POTENTIAL-LOCATIONS] Request for status: '%s'", status)

		var whereClause string
		if status == "converted" {
			whereClause = "WHERE pl.converted_at IS NOT NULL"
		} else {
			// Default: show only active (non-converted) locations
			whereClause = "WHERE pl.converted_at IS NULL"
		}
		log.Printf("🔍 [GET-POTENTIAL-LOCATIONS] Using WHERE clause: %s", whereClause)

		query := fmt.Sprintf(`
			SELECT
				pl.id,
				pl.address,
				pl.street,
				pl.city,
				pl.zip,
				pl.latitude,
				pl.longitude,
				pl.requested_by_user_id,
				pl.requested_by_name,
				pl.notes,
				pl.created_at,
				pl.updated_at,
				pl.converted_to_bin_id,
				pl.converted_at,
				pl.converted_by_user_id,
				pl.converted_via_shift_id,
				b.bin_number,
				driver.name AS driver_name,
				manager.name AS manager_name
			FROM potential_locations pl
			LEFT JOIN bins b ON b.id = pl.converted_to_bin_id
			LEFT JOIN shifts s ON s.id = pl.converted_via_shift_id
			LEFT JOIN users driver ON driver.id = s.driver_id
			LEFT JOIN users manager ON manager.id = pl.converted_by_user_id
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
			var driverName *string
			var managerName *string

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
				&loc.ConvertedViaShiftID,
				&binNumber,
				&driverName,
				&managerName,
			)
			if err != nil {
				log.Printf("❌ [GET-POTENTIAL-LOCATIONS] Row scan failed: %v", err)
				continue
			}

			// Debug: log if this is supposed to be "active" but has converted_at
			if status != "converted" && loc.ConvertedAt != nil {
				log.Printf("⚠️ [GET-POTENTIAL-LOCATIONS] WARNING: Location %s has converted_at=%d but is being returned as ACTIVE!", loc.ID, *loc.ConvertedAt)
			}

			resp := loc.ToPotentialLocationResponse()
			resp.BinNumber = binNumber
			resp.ConvertedByDriverName = driverName
			resp.ConvertedByManagerName = managerName
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
func CreatePotentialLocation(db *sqlx.DB, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
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
		tx, err := db.Beginx()
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

			// Broadcast each location to all managers via Centrifugo
			if centrifugoClient != nil {
				if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "potential_location_created", resp); pubErr != nil {
					log.Printf("\u26a0\ufe0f [CREATE-POTENTIAL-LOCATION] Centrifugo publish failed: %v", pubErr)
					wsHub.BroadcastToRole("admin", map[string]interface{}{"type": "potential_location_created", "data": resp})
				}
			} else {
				wsHub.BroadcastToRole("admin", map[string]interface{}{"type": "potential_location_created", "data": resp})
			}
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
func DeletePotentialLocation(db *sqlx.DB, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
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

		// Get authenticated user
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		managerUserID := userClaims.UserID

		now := time.Now().Unix()

		// Delete location
		_, err = db.Exec("DELETE FROM potential_locations WHERE id = $1", id)
		if err != nil {
			log.Printf("❌ [DELETE-POTENTIAL-LOCATION] Database delete failed: %v", err)
			http.Error(w, "Failed to delete potential location", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [DELETE-POTENTIAL-LOCATION] Deleted location (ID: %s)", id)

		// ── Cascading Update: Notify drivers if location was in active shifts ───────
		if centrifugoClient != nil {
			log.Printf("🔄 [DELETE-POTENTIAL-LOCATION] Checking for active shift dependencies")

			// Find active shifts with placement tasks for this potential location
			var affectedShifts []struct {
				ShiftID string `db:"shift_id"`
				TaskID  string `db:"task_id"`
			}

			err = db.Select(&affectedShifts, `
				SELECT
					rt.shift_id,
					rt.id as task_id
				FROM route_tasks rt
				JOIN shifts s ON rt.shift_id = s.id
				WHERE rt.potential_location_id = $1
				  AND rt.task_type = 'placement'
				  AND rt.is_completed = 0
				  AND s.status IN ('active', 'scheduled')
			`, id)

			if err != nil && err != sql.ErrNoRows {
				log.Printf("⚠️  [DELETE-POTENTIAL-LOCATION] Failed to check active shift dependencies: %v", err)
			} else if len(affectedShifts) > 0 {
				log.Printf("🎯 [DELETE-POTENTIAL-LOCATION] Found %d active shift task(s) affected by deletion", len(affectedShifts))

				// Group tasks by shift for notification
				shiftTasksMap := make(map[string][]string)
				for _, affected := range affectedShifts {
					shiftTasksMap[affected.ShiftID] = append(shiftTasksMap[affected.ShiftID], affected.TaskID)

					// Soft delete the placement task (potential location no longer exists)
					_, deleteErr := db.Exec(`
						UPDATE route_tasks
						SET is_deleted = true,
							deleted_at = $1,
							deleted_by = $2,
							deletion_reason = $3,
							updated_at = $1
						WHERE id = $4 AND is_deleted = false
					`, now, managerUserID, "potential_location_deleted", affected.TaskID)

					if deleteErr != nil {
						log.Printf("⚠️  [DELETE-POTENTIAL-LOCATION] Failed to soft delete placement task %s: %v", affected.TaskID, deleteErr)
					} else {
						log.Printf("✅ [DELETE-POTENTIAL-LOCATION] Soft deleted placement task %s", affected.TaskID)
					}
				}

				// Notify each affected shift
				for shiftID, taskIDs := range shiftTasksMap {
					notifyErr := NotifyDriverOfRouteUpdate(
						db,
						centrifugoClient,
						shiftID,
						"potential_location_deleted",
						map[string]interface{}{
							"potential_location_id": id,
							"removed_tasks":         taskIDs,
							"reason":                "Potential location was deleted by manager",
						},
					)

					if notifyErr != nil {
						log.Printf("⚠️  [DELETE-POTENTIAL-LOCATION] Failed to notify driver for shift %s: %v", shiftID, notifyErr)
					} else {
						log.Printf("✅ [DELETE-POTENTIAL-LOCATION] Notified driver for shift %s about placement task removal", shiftID)
					}

					// Re-optimize the shift (skip gates since this is manager-initiated)
					if reoptErr := ReoptimizeActiveShift(db, shiftID, centrifugoClient, true); reoptErr != nil {
						log.Printf("⚠️  [DELETE-POTENTIAL-LOCATION] Failed to re-optimize shift %s: %v", shiftID, reoptErr)
					} else {
						log.Printf("✅ [DELETE-POTENTIAL-LOCATION] Successfully re-optimized shift %s", shiftID)
					}
				}
			} else {
				log.Printf("ℹ️  [DELETE-POTENTIAL-LOCATION] No active shift dependencies found for this potential location")
			}
		}

		// Broadcast to all managers via Centrifugo
		deleteData := map[string]interface{}{"location_id": id}
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "potential_location_deleted", deleteData); pubErr != nil {
				log.Printf("\u26a0\ufe0f [DELETE-POTENTIAL-LOCATION] Centrifugo publish failed: %v", pubErr)
				wsHub.BroadcastToRole("admin", map[string]interface{}{"type": "potential_location_deleted", "data": deleteData})
			}
		} else {
			wsHub.BroadcastToRole("admin", map[string]interface{}{"type": "potential_location_deleted", "data": deleteData})
		}
		log.Printf("\U0001f4e4 [DELETE-POTENTIAL-LOCATION] Event broadcasted to managers")

		w.WriteHeader(http.StatusNoContent)
	}
}

// ConvertPotentialLocationToBin converts a potential location to an active bin
// POST /api/potential-locations/:id/convert (requires admin role)
func ConvertPotentialLocationToBin(db *sqlx.DB, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] ========== START ==========")
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Request received for location ID: %s", id)

		if id == "" {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Missing location ID")
			http.Error(w, "Missing location ID", http.StatusBadRequest)
			return
		}

		// Parse optional request body
		var req models.ConvertToBinRequest
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&req)
		}
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Request body parsed: bin_number=%v, fill_percentage=%v", req.BinNumber, req.FillPercentage)

		// Get user from context (manager who is converting)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Unauthorized: user not found in context")
			http.Error(w, "Unauthorized: user not found in context", http.StatusUnauthorized)
			return
		}

		userID := userClaims.UserID
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] User ID: %s", userID)

		// First, check if location exists at all (without transaction)
		var existsCheck bool
		err := db.Get(&existsCheck, "SELECT EXISTS(SELECT 1 FROM potential_locations WHERE id = $1)", id)
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Error checking if location exists: %v", err)
		} else {
			log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Location exists in DB: %v", existsCheck)
		}

		// Check if already converted (without transaction)
		var convertedAt *int64
		var convertedToBinID *string
		err = db.QueryRow("SELECT converted_at, converted_to_bin_id FROM potential_locations WHERE id = $1", id).Scan(&convertedAt, &convertedToBinID)
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Error checking conversion status: %v", err)
		} else {
			if convertedAt == nil {
				log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] converted_at is NULL - location NOT converted yet")
			} else {
				if convertedToBinID == nil {
					log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] converted_at = %d, but converted_to_bin_id is NULL - BIN WAS DELETED! Re-conversion should be allowed.", *convertedAt)
				} else {
					log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] converted_at = %d, converted_to_bin_id = %s - location already converted to existing bin", *convertedAt, *convertedToBinID)
				}
			}
		}

		// Begin transaction
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Starting transaction...")
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Transaction begin failed: %v", err)
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Transaction started successfully")

		// Fetch potential location
		// Allow re-conversion if bin was deleted (converted_to_bin_id IS NULL)
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Executing query: SELECT id, street, city, zip, latitude, longitude, requested_by_user_id FROM potential_locations WHERE id = '%s' AND (converted_at IS NULL OR converted_to_bin_id IS NULL)", id)
		var location models.PotentialLocation
		err = tx.QueryRow(`
			SELECT id, street, city, zip, latitude, longitude, requested_by_user_id
			FROM potential_locations
			WHERE id = $1 AND (converted_at IS NULL OR converted_to_bin_id IS NULL)
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
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Query returned NO ROWS - location not found or already converted to an existing bin")
			http.Error(w, "Potential location not found or already converted to an existing bin", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Query failed with error: %v (error type: %T)", err, err)
			http.Error(w, "Failed to fetch potential location", http.StatusInternalServerError)
			return
		}
		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Location fetched: %s, %s %s", location.Street, location.City, location.Zip)

		// Validate bin_number is required
		if req.BinNumber == nil || *req.BinNumber <= 0 {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Bin number is required")
			http.Error(w, "Bin number is required", http.StatusBadRequest)
			return
		}

		binNumber := *req.BinNumber
		log.Printf("📝 [CONVERT-POTENTIAL-LOCATION] Using bin number: %d", binNumber)

		// Create bin
		binID := uuid.New().String()
		now := time.Now().Unix()
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Generated bin ID: %s, timestamp: %d", binID, now)

		// Default fill_percentage to 0 if not provided
		fillPercentage := 0
		if req.FillPercentage != nil {
			fillPercentage = *req.FillPercentage
		}

		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Creating bin with: bin_number=%d, street=%s, city=%s, zip=%s, fill_percentage=%d", binNumber, location.Street, location.City, location.Zip, fillPercentage)
		_, err = tx.Exec(`
			INSERT INTO bins (
				id, bin_number, current_street, city, zip, status,
				fill_percentage, checked, move_requested, latitude, longitude,
				created_by_user_id, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		`,
			binID, binNumber, location.Street, location.City, location.Zip, "active",
			fillPercentage, 0, 0, location.Latitude, location.Longitude,
			&userID, now, now,
		)

		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to create bin: %v", err)
			http.Error(w, "Failed to create bin", http.StatusInternalServerError)
			return
		}
		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Bin created successfully")

		// Update potential location to mark as converted (soft delete)
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Updating potential location to mark as converted...")
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
		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Potential location marked as converted")

		// Commit transaction
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Committing transaction...")
		if err = tx.Commit(); err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Transaction commit failed: %v", err)
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Transaction committed successfully")

		// Fetch created bin
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Fetching created bin from database...")
		var createdBin models.Bin
		err = db.Get(&createdBin, "SELECT * FROM bins WHERE id = $1", binID)
		if err != nil {
			log.Printf("❌ [CONVERT-POTENTIAL-LOCATION] Failed to fetch created bin: %v", err)
			http.Error(w, "Failed to fetch created bin", http.StatusInternalServerError)
			return
		}
		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Fetched created bin successfully")

		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Converted location (ID: %s) to Bin #%d (ID: %s)", id, binNumber, binID)

		// ── Cascading Update: Notify drivers if location was in active shifts ───────
		if centrifugoClient != nil {
			log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] Checking for active shift dependencies")

			// Find active shifts with placement tasks for this potential location
			var affectedShifts []struct {
				ShiftID string `db:"shift_id"`
				TaskID  string `db:"task_id"`
			}

			err = db.Select(&affectedShifts, `
				SELECT
					rt.shift_id,
					rt.id as task_id
				FROM route_tasks rt
				JOIN shifts s ON rt.shift_id = s.id
				WHERE rt.potential_location_id = $1
				  AND rt.task_type = 'placement'
				  AND rt.is_completed = 0
				  AND s.status IN ('active', 'scheduled')
			`, id)

			if err != nil && err != sql.ErrNoRows {
				log.Printf("⚠️  [CONVERT-POTENTIAL-LOCATION] Failed to check active shift dependencies: %v", err)
			} else if len(affectedShifts) > 0 {
				log.Printf("🎯 [CONVERT-POTENTIAL-LOCATION] Found %d active shift task(s) affected by conversion", len(affectedShifts))

				// Group tasks by shift for notification
				shiftTasksMap := make(map[string][]string)
				for _, affected := range affectedShifts {
					shiftTasksMap[affected.ShiftID] = append(shiftTasksMap[affected.ShiftID], affected.TaskID)

					// Soft delete the placement task (potential location converted to bin)
					_, deleteErr := db.Exec(`
						UPDATE route_tasks
						SET is_deleted = true,
							deleted_at = $1,
							deleted_by = $2,
							deletion_reason = $3,
							updated_at = $1
						WHERE id = $4 AND is_deleted = false
					`, now, userID, "potential_location_converted_to_bin", affected.TaskID)

					if deleteErr != nil {
						log.Printf("⚠️  [CONVERT-POTENTIAL-LOCATION] Failed to soft delete placement task %s: %v", affected.TaskID, deleteErr)
					} else {
						log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Soft deleted placement task %s", affected.TaskID)
					}
				}

				// Notify each affected shift
				for shiftID, taskIDs := range shiftTasksMap {
					notifyErr := NotifyDriverOfRouteUpdate(
						db,
						centrifugoClient,
						shiftID,
						"potential_location_converted",
						map[string]interface{}{
							"potential_location_id": id,
							"new_bin_id":            binID,
							"new_bin_number":        binNumber,
							"removed_tasks":         taskIDs,
							"reason":                "Potential location was converted to a bin by manager",
						},
					)

					if notifyErr != nil {
						log.Printf("⚠️  [CONVERT-POTENTIAL-LOCATION] Failed to notify driver for shift %s: %v", shiftID, notifyErr)
					} else {
						log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Notified driver for shift %s about placement task removal", shiftID)
					}

					// Re-optimize the shift (skip gates since this is manager-initiated)
					if reoptErr := ReoptimizeActiveShift(db, shiftID, centrifugoClient, true); reoptErr != nil {
						log.Printf("⚠️  [CONVERT-POTENTIAL-LOCATION] Failed to re-optimize shift %s: %v", shiftID, reoptErr)
					} else {
						log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Successfully re-optimized shift %s", shiftID)
					}
				}
			} else {
				log.Printf("ℹ️  [CONVERT-POTENTIAL-LOCATION] No active shift dependencies found for this potential location")
			}
		}

		// Broadcast to all managers via Centrifugo (company-wide channel)
		eventData := map[string]interface{}{
			"type": "potential_location_converted",
			"data": map[string]interface{}{
				"location_id": id,
				"bin":         createdBin.ToBinResponse(),
			},
		}
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "potential_location_converted", map[string]interface{}{
				"location_id": id,
				"bin":         createdBin.ToBinResponse(),
			}); pubErr != nil {
				log.Printf("⚠️ [CONVERT-POTENTIAL-LOCATION] Centrifugo publish failed: %v — falling back to WebSocket", pubErr)
				wsHub.BroadcastToRole("admin", eventData)
			} else {
				log.Printf("📡 [CONVERT-POTENTIAL-LOCATION] Published potential_location_converted via Centrifugo")
			}
		} else {
			// Centrifugo not available — fall back to legacy WebSocket hub
			wsHub.BroadcastToRole("admin", eventData)
			log.Printf("📤 [CONVERT-POTENTIAL-LOCATION] WebSocket event broadcasted to managers (fallback)")
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(createdBin.ToBinResponse())
		log.Printf("🔄 [CONVERT-POTENTIAL-LOCATION] ========== SUCCESS ==========")
		log.Printf("✅ [CONVERT-POTENTIAL-LOCATION] Response sent: HTTP 201 with bin data")
	}
}

// GetNearbyPotentialLocations finds active potential locations within a radius of a bin's coordinates.
// GET /api/bins/{binId}/nearby-potential-locations?max_distance=500
// Requires: admin auth
func GetNearbyPotentialLocations(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "binId")
		if binID == "" {
			http.Error(w, "Missing bin ID", http.StatusBadRequest)
			return
		}

		// Parse max_distance query param (default: return all locations sorted by distance)
		// If max_distance is provided, filter to only locations within that radius
		var maxDistance *float64
		if raw := r.URL.Query().Get("max_distance"); raw != "" {
			if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed > 0 {
				maxDistance = &parsed
			}
		}

		if maxDistance != nil {
			log.Printf("🔍 [NEARBY-POTENTIAL-LOCATIONS] bin=%s max_distance=%.0fm", binID, *maxDistance)
		} else {
			log.Printf("🔍 [NEARBY-POTENTIAL-LOCATIONS] bin=%s returning ALL locations sorted by distance", binID)
		}

		// Fetch bin coordinates
		var binLat, binLng *float64
		var binNumber int
		var binStreet, binCity, binZip string
		err := db.QueryRow(`
			SELECT bin_number, current_street, city, zip, latitude, longitude
			FROM bins WHERE id = $1
		`, binID).Scan(&binNumber, &binStreet, &binCity, &binZip, &binLat, &binLng)
		if err == sql.ErrNoRows {
			http.Error(w, "Bin not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ [NEARBY-POTENTIAL-LOCATIONS] DB error fetching bin: %v", err)
			http.Error(w, "Failed to fetch bin", http.StatusInternalServerError)
			return
		}
		if binLat == nil || binLng == nil {
			http.Error(w, "Bin has no coordinates", http.StatusBadRequest)
			return
		}

		// Fetch all active potential locations that have coordinates
		rows, err := db.Query(`
			SELECT id, street, city, zip, latitude, longitude,
			       requested_by_name, notes, created_at
			FROM potential_locations
			WHERE converted_at IS NULL
			  AND latitude IS NOT NULL
			  AND longitude IS NOT NULL
		`)
		if err != nil {
			log.Printf("❌ [NEARBY-POTENTIAL-LOCATIONS] DB error fetching locations: %v", err)
			http.Error(w, "Failed to fetch potential locations", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type NearbyResult struct {
			ID              string  `json:"id"`
			Street          string  `json:"street"`
			City            string  `json:"city"`
			Zip             string  `json:"zip"`
			Latitude        float64 `json:"latitude"`
			Longitude       float64 `json:"longitude"`
			RequestedByName string  `json:"requested_by_name"`
			Notes           *string `json:"notes,omitempty"`
			CreatedAt       int64   `json:"created_at"`
			DistanceMeters  float64 `json:"distance_meters"`
		}

		var results []NearbyResult
		for rows.Next() {
			var loc NearbyResult
			if err := rows.Scan(
				&loc.ID, &loc.Street, &loc.City, &loc.Zip,
				&loc.Latitude, &loc.Longitude,
				&loc.RequestedByName, &loc.Notes, &loc.CreatedAt,
			); err != nil {
				log.Printf("⚠️ [NEARBY-POTENTIAL-LOCATIONS] Row scan error: %v", err)
				continue
			}
			dist := haversineMeters(*binLat, *binLng, loc.Latitude, loc.Longitude)
			loc.DistanceMeters = math.Round(dist*10) / 10

			// Only filter by distance if max_distance is provided
			if maxDistance == nil || dist <= *maxDistance {
				results = append(results, loc)
			}
		}

		// Sort ascending by distance
		sort.Slice(results, func(i, j int) bool {
			return results[i].DistanceMeters < results[j].DistanceMeters
		})

		if results == nil {
			results = []NearbyResult{}
		}

		if maxDistance != nil {
			log.Printf("✅ [NEARBY-POTENTIAL-LOCATIONS] Found %d location(s) within %.0fm of bin #%d", len(results), *maxDistance, binNumber)
		} else {
			log.Printf("✅ [NEARBY-POTENTIAL-LOCATIONS] Found %d location(s) sorted by distance from bin #%d", len(results), binNumber)
		}

		response := map[string]interface{}{
			"bin": map[string]interface{}{
				"id":             binID,
				"bin_number":     binNumber,
				"current_street": binStreet,
				"city":           binCity,
				"zip":            binZip,
				"latitude":       *binLat,
				"longitude":      *binLng,
			},
			"count":   len(results),
			"results": results,
		}

		// Only include search_radius_meters if a max_distance was specified
		if maxDistance != nil {
			response["search_radius_meters"] = *maxDistance
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// haversineMeters returns the great-circle distance in metres between two lat/lng points.
func haversineMeters(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadius = 6371000.0
	φ1 := lat1 * math.Pi / 180
	φ2 := lat2 * math.Pi / 180
	Δφ := (lat2 - lat1) * math.Pi / 180
	Δλ := (lng2 - lng1) * math.Pi / 180
	a := math.Sin(Δφ/2)*math.Sin(Δφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	return 2 * earthRadius * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
