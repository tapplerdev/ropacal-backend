package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/bindomain"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"

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
			       last_moved, last_checked, last_checked_at, status, fill_percentage,
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

func CreateBin(db *sqlx.DB, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateBinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.CurrentStreet == "" || req.City == "" || req.Zip == "" || req.Status == "" {
			http.Error(w, "Missing required fields (current_street, city, zip, status)", http.StatusBadRequest)
			return
		}

		// Validate status at the boundary (clean 400, not a DB CHECK 500).
		if _, err := bindomain.ParseStatus(req.Status); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Auto-assign bin_number if not provided
		var binNumber int
		if req.BinNumber != nil && *req.BinNumber > 0 {
			// Use provided bin number (for manual override or migration)
			binNumber = *req.BinNumber
			log.Printf("[CREATE-BIN] Using provided bin_number: %d", binNumber)
		} else {
			// Auto-assign based on highest existing bin_number (including retired bins)
			// This ensures continuity: if bins are 54, 55, 56, 57, next will be 58
			var maxBinNumber sql.NullInt64
			err := db.Get(&maxBinNumber, "SELECT MAX(bin_number) FROM bins")
			if err != nil {
				log.Printf("❌ [CREATE-BIN] Failed to get max bin_number: %v", err)
				http.Error(w, "Failed to generate bin number", http.StatusInternalServerError)
				return
			}

			if maxBinNumber.Valid {
				binNumber = int(maxBinNumber.Int64) + 1
			} else {
				// No bins exist yet, start at 1
				binNumber = 1
			}
			log.Printf("[CREATE-BIN] Auto-assigned bin_number: %d (max existing: %v)", binNumber, maxBinNumber)
		}

		// Generate UUID for new bin
		id := uuid.New().String()
		now := time.Now().Unix()

		// Get user ID from context (required - auth middleware ensures this exists)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			log.Printf("❌ [CREATE-BIN] User not found in context")
			http.Error(w, "Unauthorized: user not found in context", http.StatusUnauthorized)
			return
		}

		log.Printf("[CREATE-BIN] Creating bin for user: %s (%s)", userClaims.Email, userClaims.UserID)

		// Default fill_percentage to 0 if not provided
		fillPercentage := 0
		if req.FillPercentage != nil {
			fillPercentage = *req.FillPercentage
		}

		// Geocode the address when coordinates aren't supplied, so a bin is never
		// created without a routable location (the dashboard's coordinate inputs are
		// optional). Best-effort: if geocoding fails we still create the bin, and it
		// can be backfilled later via the batch-geocode endpoint.
		if req.Latitude == nil || req.Longitude == nil {
			geocoder := services.NewHEREGeocodingService(HereAPIKey)
			if lat, lng, gErr := geocoder.GeocodeAddress(req.CurrentStreet, req.City, req.Zip); gErr == nil {
				req.Latitude = &lat
				req.Longitude = &lng
				log.Printf("[CREATE-BIN] Geocoded \"%s, %s %s\" -> (%.6f, %.6f)", req.CurrentStreet, req.City, req.Zip, lat, lng)
			} else {
				log.Printf("⚠️  [CREATE-BIN] Geocoding failed for \"%s, %s %s\": %v (creating without coordinates)", req.CurrentStreet, req.City, req.Zip, gErr)
			}
		}

		// Insert bin
		_, err := db.Exec(`
			INSERT INTO bins (
				id, bin_number, current_street, city, zip, status,
				fill_percentage, checked, move_requested, latitude, longitude,
				created_by_user_id, source_potential_location_id, created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		`,
			id, binNumber, req.CurrentStreet, req.City, req.Zip, req.Status,
			fillPercentage, 0, 0, req.Latitude, req.Longitude,
			userClaims.UserID, req.SourcePotentialLocationID, now, now,
		)

		if err != nil {
			// Check if bin_number already exists
			if strings.Contains(err.Error(), "duplicate key") {
				log.Printf("❌ [CREATE-BIN] Bin number %d already exists", binNumber)
				http.Error(w, "Bin number already exists", http.StatusConflict)
				return
			}
			log.Printf("❌ [CREATE-BIN] Database insert failed: %v", err)
			http.Error(w, "Failed to create bin", http.StatusInternalServerError)
			return
		}

		// If bin was created from a potential location, mark it as converted with snapshot
		if req.SourcePotentialLocationID != nil {
			log.Printf("[CREATE-BIN] 📍 Created from potential location %s - capturing conversion snapshot", *req.SourcePotentialLocationID)

			// Build conversion metadata
			metadata := map[string]interface{}{
				"is_new_bin":  true,
				"created_via": "manager_portal",
				"bin_id":      id,
				"created_by":  userClaims.UserID,
				"created_at":  now,
			}
			metadataJSON, _ := json.Marshal(metadata)

			// Build full address for snapshot
			fullAddress := fmt.Sprintf("%s, %s, %s", req.CurrentStreet, req.City, req.Zip)

			_, err = db.Exec(`
				UPDATE potential_locations
				SET converted_to_bin_id = $1,
					converted_bin_number_snapshot = $2,
					converted_address_snapshot = $3,
					converted_at = $4,
					converted_by_user_id = $5,
					conversion_status = 'converted',
					bin_current_status = 'active',
					conversion_metadata = $6,
					updated_at = $4
				WHERE id = $7
			`, id, binNumber, fullAddress, now, userClaims.UserID, string(metadataJSON), *req.SourcePotentialLocationID)

			if err != nil {
				log.Printf("[CREATE-BIN] ⚠️  Error capturing conversion snapshot: %v", err)
				// Don't fail the whole request - bin was created successfully
			} else {
				log.Printf("[CREATE-BIN] ✅ Conversion snapshot captured: Bin #%d at %s", binNumber, fullAddress)
			}
		}

		// Fetch created bin
		var created models.Bin
		err = db.Get(&created, "SELECT * FROM bins WHERE id = $1", id)
		if err != nil {
			log.Printf("❌ [CREATE-BIN] Failed to fetch created bin: %v", err)
			http.Error(w, "Failed to fetch created bin", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [CREATE-BIN] Created bin #%d (ID: %s) at %s, %s", binNumber, id, req.CurrentStreet, req.City)

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bin_created",
			"data": created.ToBinResponse(),
		})
		log.Printf("📤 [CREATE-BIN] WebSocket event broadcasted to managers")

		// Publish to Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_created", created.ToBinResponse()); pubErr != nil {
				log.Printf("⚠️  [CREATE-BIN] Failed to publish bin_created to Centrifugo: %v", pubErr)
			} else {
				log.Printf("📡 [CREATE-BIN] Centrifugo: Published bin_created to company:events")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(created.ToBinResponse())
	}
}

func UpdateBin(db *sqlx.DB, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		log.Printf("🔧 [UPDATE-BIN] Request received for bin ID: %s", id)

		if id == "" {
			log.Printf("❌ [UPDATE-BIN] Missing bin ID")
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		// Extract user from JWT context (for checked_by tracking)
		var userID *string
		if claims, ok := middleware.GetUserFromContext(r); ok {
			userID = &claims.UserID
			log.Printf("🔐 [UPDATE-BIN] User ID from JWT: %s", claims.UserID)
		}

		var req models.UpdateBinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [UPDATE-BIN] Invalid request body: %v", err)
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate status at the boundary when provided (clean 400, not a DB CHECK 500).
		if req.Status != "" {
			if _, err := bindomain.ParseStatus(req.Status); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		log.Printf("📦 [UPDATE-BIN] Request data: street=%s, city=%s, zip=%s, status=%s, checked=%v, fill=% v, lat=%v, lng=%v",
			req.CurrentStreet, req.City, req.Zip, req.Status, req.Checked, req.FillPercentage, req.Latitude, req.Longitude)

		// Get existing bin
		var existing models.Bin
		err := db.Get(&existing, "SELECT * FROM bins WHERE id = $1", id)
		if err == sql.ErrNoRows {
			log.Printf("❌ [UPDATE-BIN] Bin not found: %s", id)
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ [UPDATE-BIN] Database error fetching bin: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [UPDATE-BIN] Found existing bin #%d: %s, %s %s", existing.BinNumber, existing.CurrentStreet, existing.City, existing.Zip)

		// Partial-update semantics (#14/#35): a partial PATCH must NOT blank omitted fields.
		// Backfill string/number fields from the existing row when the request omits them
		// (empty string / 0 = omitted — never a legitimate value for these), and resolve the
		// pointer bools (nil = omitted → keep existing). The rest of the handler then reads
		// req.* / the resolved bools uniformly, and addrChanged stays false on an omitted
		// address (so coords aren't nulled — the confirmed #35 corruption).
		if strings.TrimSpace(req.CurrentStreet) == "" {
			req.CurrentStreet = existing.CurrentStreet
		}
		if strings.TrimSpace(req.City) == "" {
			req.City = existing.City
		}
		if strings.TrimSpace(req.Zip) == "" {
			req.Zip = existing.Zip
		}
		if req.Status == "" {
			req.Status = existing.Status
		}
		if req.BinNumber == 0 {
			req.BinNumber = existing.BinNumber
		}
		checked := existing.Checked
		if req.Checked != nil {
			checked = *req.Checked
		}
		moveRequested := existing.MoveRequested
		if req.MoveRequested != nil {
			moveRequested = *req.MoveRequested
		}

		wasChecked := existing.Checked
		becomingChecked := checked && !wasChecked

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

		log.Printf("📍 [UPDATE-BIN] Address changed: %v (was: %s, %s %s → now: %s, %s %s)",
			addrChanged, existing.CurrentStreet, existing.City, existing.Zip,
			req.CurrentStreet, req.City, req.Zip)

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [UPDATE-BIN] Failed to begin transaction: %v", err)
			http.Error(w, "Failed to begin transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		log.Printf("🔄 [UPDATE-BIN] Transaction started")

		// Convert booleans to integers (database uses INTEGER for boolean fields)
		checkedInt := 0
		if checked {
			checkedInt = 1
		}
		moveRequestedInt := 0
		if moveRequested {
			moveRequestedInt = 1
		}

		// Dereference fill_percentage pointer to get actual value
		fillPct := 0
		if req.FillPercentage != nil {
			fillPct = *req.FillPercentage
		}

		// Storing a bin (in_storage / retired) takes it out of the field: it holds
		// no waste and its check history is no longer meaningful. Mirror the store
		// finalization in shift_complete.go — force fill to 0% and clear the
		// last-checked timestamps (surfaced as "N/A") regardless of request values.
		isStoring := req.Status == "in_storage" || req.Status == "retired"
		if isStoring {
			fillPct = 0
			checkedInt = 0
		}

		log.Printf("🔧 [UPDATE-BIN] Converted values: checked=%d, fill_percentage=%d, move_requested=%d",
			checkedInt, fillPct, moveRequestedInt)

		// Build update query
		query := `
			UPDATE bins
			SET bin_number = $1, current_street = $2, city = $3, zip = $4, status = $5,
			    checked = $6, fill_percentage = $7, move_requested = $8`

		args := []interface{}{
			req.BinNumber, req.CurrentStreet, req.City, req.Zip, req.Status,
			checkedInt, fillPct, moveRequestedInt,
		}

		paramCount := 8
		if isStoring {
			// Stored/retired bins carry no meaningful check history — surface
			// last-checked as N/A. Overrides any becomingChecked transition.
			query += `, last_checked = NULL, last_checked_at = NULL`
		} else if becomingChecked {
			paramCount++
			query += `, last_checked = $` + fmt.Sprintf("%d", paramCount)
			args = append(args, now.Unix())
		}

		// Handle coordinates logic:
		// 1. If coordinates provided in request → use them
		// 2. Else if address changed → clear coordinates (set to NULL)
		// 3. Otherwise → keep existing coordinates (no change)
		if req.Latitude != nil && req.Longitude != nil {
			// Coordinates provided - use them
			paramCount++
			query += `, latitude = $` + fmt.Sprintf("%d", paramCount)
			args = append(args, *req.Latitude)

			paramCount++
			query += `, longitude = $` + fmt.Sprintf("%d", paramCount)
			args = append(args, *req.Longitude)
		} else if addrChanged {
			// Address changed but no coordinates provided - clear them
			query += `, latitude = NULL, longitude = NULL`
		}
		// Otherwise: keep existing coordinates (no change to query)

		// Handle source_potential_location_id
		// If provided in request, update it; otherwise keep existing value
		if req.SourcePotentialLocationID != nil {
			paramCount++
			query += `, source_potential_location_id = $` + fmt.Sprintf("%d", paramCount)
			args = append(args, *req.SourcePotentialLocationID)
		}

		paramCount++
		query += `, updated_at = $` + fmt.Sprintf("%d", paramCount) + ` WHERE id = $` + fmt.Sprintf("%d", paramCount+1)
		args = append(args, time.Now().Unix(), id)

		log.Printf("📝 [UPDATE-BIN] Executing query with %d parameters", len(args))
		log.Printf("📝 [UPDATE-BIN] Query: %s", query)
		log.Printf("📝 [UPDATE-BIN] Args: %v", args)

		_, err = tx.Exec(query, args...)
		if err != nil {
			log.Printf("❌ [UPDATE-BIN] Failed to execute update query: %v", err)
			log.Printf("❌ [UPDATE-BIN] Query was: %s", query)
			log.Printf("❌ [UPDATE-BIN] Args were: %v", args)
			http.Error(w, "Failed to update bin", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [UPDATE-BIN] Bin updated successfully")

		// If becoming checked, insert check record
		if becomingChecked {
			log.Printf("✓ [UPDATE-BIN] Bin is becoming checked, creating check record")

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

			log.Printf("📝 [UPDATE-BIN] Creating check: from=%s, fill=%d%%, time=%d, user=%v",
				checkedFrom, fillForCheck, now.Unix(), userID)

			// Include checked_by (authenticated user) and photo_url if provided.
			// The address snapshot reads the bins row post-update in this same
			// tx: if the edit also changed the address, the check belongs to
			// the new location (the change log keeps the old one).
			_, err = tx.Exec(`
				INSERT INTO checks (bin_id, checked_from, fill_percentage, checked_on, checked_by, photo_url,
									bin_address_snapshot, bin_latitude_snapshot, bin_longitude_snapshot)
				VALUES ($1, $2, $3, $4, $5, $6,
						(SELECT CONCAT_WS(', ', NULLIF(current_street,''), NULLIF(city,''), NULLIF(zip,'')) FROM bins WHERE id = $1),
						(SELECT latitude FROM bins WHERE id = $1),
						(SELECT longitude FROM bins WHERE id = $1))
			`, id, checkedFrom, fillForCheck, now.Unix(), userID, req.PhotoUrl)
			if err != nil {
				log.Printf("❌ [UPDATE-BIN] Failed to create check record: %v", err)
				http.Error(w, "Failed to create check record", http.StatusInternalServerError)
				return
			}

			log.Printf("✅ [UPDATE-BIN] Check record created")
		}

		// ── Change log + optional no-go zone creation ───────────────────────────
		// NOTE: All follow-on writes below run inside the SAME transaction (tx)
		// opened above. The transaction is committed once at the very end so a
		// failure in any follow-on write rolls back the entire update (no partial
		// state).
		nowUnix := time.Now().Unix()

		// ── Potential Location Conversion (if bin relocated to potential location) ──
		if req.SourcePotentialLocationID != nil {
			// Check if this is a NEW potential location assignment
			isNewAssignment := existing.SourcePotentialLocationID == nil ||
				*existing.SourcePotentialLocationID != *req.SourcePotentialLocationID

			if isNewAssignment {
				log.Printf("[UPDATE-BIN] 📍 Bin #%d relocated to potential location %s - capturing snapshot",
					existing.BinNumber, *req.SourcePotentialLocationID)

				// Build conversion metadata with previous bin state
				metadata := map[string]interface{}{
					"is_new_bin":             false,
					"created_via":            "bin_relocation",
					"bin_id":                 id,
					"bin_previous_address":   fmt.Sprintf("%s, %s, %s", existing.CurrentStreet, existing.City, existing.Zip),
					"bin_previous_latitude":  existing.Latitude,
					"bin_previous_longitude": existing.Longitude,
					"relocation_date":        nowUnix,
					"relocation_by":          userID,
				}
				metadataJSON, _ := json.Marshal(metadata)

				// Build full address for snapshot (use new address from request)
				fullAddress := fmt.Sprintf("%s, %s, %s", req.CurrentStreet, req.City, req.Zip)

				_, err = tx.Exec(`
					UPDATE potential_locations
					SET converted_to_bin_id = $1,
						converted_bin_number_snapshot = $2,
						converted_address_snapshot = $3,
						converted_at = $4,
						converted_by_user_id = $5,
						conversion_status = 'converted',
						bin_current_status = 'active',
						conversion_metadata = $6,
						updated_at = $4
					WHERE id = $7
				`, id, existing.BinNumber, fullAddress, nowUnix, userID, string(metadataJSON), *req.SourcePotentialLocationID)

				if err != nil {
					log.Printf("[UPDATE-BIN] ❌ Error capturing relocation snapshot: %v", err)
					http.Error(w, "Failed to capture relocation snapshot", http.StatusInternalServerError)
					return
				}
				log.Printf("[UPDATE-BIN] ✅ Relocation snapshot captured: Bin #%d moved from %s to %s",
					existing.BinNumber, metadata["bin_previous_address"], fullAddress)
			}
		}

		// Detect what changed and build old/new JSONB snapshots
		changeType := ""
		oldValues := map[string]interface{}{}
		newValues := map[string]interface{}{}

		log.Printf("🔍 [CHANGELOG] addrChanged=%v statusChanged=%v reqFill=%v existingFill=%v userID=%v",
			addrChanged, req.Status != existing.Status, req.FillPercentage, existing.FillPercentage, userID)

		if addrChanged {
			changeType = "address_change"
			oldValues["street"] = existing.CurrentStreet
			oldValues["city"] = existing.City
			oldValues["zip"] = existing.Zip
			newValues["street"] = req.CurrentStreet
			newValues["city"] = req.City
			newValues["zip"] = req.Zip
		} else if req.Status != existing.Status {
			changeType = "status_change"
			oldValues["status"] = existing.Status
			newValues["status"] = req.Status
		} else if req.FillPercentage != nil && (existing.FillPercentage == nil || *req.FillPercentage != *existing.FillPercentage) {
			changeType = "fill_override"
			if existing.FillPercentage != nil {
				oldValues["fill_percentage"] = *existing.FillPercentage
			} else {
				oldValues["fill_percentage"] = nil
			}
			newValues["fill_percentage"] = *req.FillPercentage
		} else if req.BinNumber != existing.BinNumber {
			changeType = "bin_number_change"
			oldValues["bin_number"] = existing.BinNumber
			newValues["bin_number"] = req.BinNumber
		} else if req.Latitude != nil && req.Longitude != nil {
			if existing.Latitude == nil || existing.Longitude == nil ||
				*req.Latitude != *existing.Latitude || *req.Longitude != *existing.Longitude {
				changeType = "coordinates_change"
				oldValues["latitude"] = existing.Latitude
				oldValues["longitude"] = existing.Longitude
				newValues["latitude"] = *req.Latitude
				newValues["longitude"] = *req.Longitude
			}
		}

		// Update potential location status if bin was modified
		if changeType != "" && existing.SourcePotentialLocationID != nil {
			log.Printf("[UPDATE-BIN] 📍 Bin #%d (from potential location) was modified: %s", existing.BinNumber, changeType)

			// Determine new status based on change type
			var newStatus string
			switch changeType {
			case "bin_number_change":
				newStatus = "renumbered"
				log.Printf("[UPDATE-BIN] Bin renumbered: #%d → #%d", existing.BinNumber, req.BinNumber)
			case "address_change", "coordinates_change":
				newStatus = "moved"
				log.Printf("[UPDATE-BIN] Bin address/coordinates changed")
			default:
				// For other changes (status, fill, etc.), keep as 'active'
				newStatus = "active"
			}

			// Update potential location status
			if newStatus != "active" {
				_, err = tx.Exec(`
					UPDATE potential_locations
					SET bin_current_status = $1,
						updated_at = $2
					WHERE id = $3
				`, newStatus, nowUnix, *existing.SourcePotentialLocationID)

				if err != nil {
					log.Printf("[UPDATE-BIN] ❌ Error updating potential location status: %v", err)
					http.Error(w, "Failed to update potential location status", http.StatusInternalServerError)
					return
				}
				log.Printf("[UPDATE-BIN] ✅ Potential location status updated to '%s'", newStatus)
			}
		}

		if changeType != "" && userID != nil {
			oldJSON, _ := json.Marshal(oldValues)
			newJSON, _ := json.Marshal(newValues)
			changeLogID := uuid.New().String()

			noGoZoneCreated := false
			var noGoZoneID *string

			// Determine if we should create a no-go zone at the OLD location
			noGoTriggers := map[string]bool{
				"landlord_complaint": true,
				"theft":              true,
				"vandalism":          true,
				"missing":            true,
			}
			shouldCreateZone := false
			if req.ReasonCategory != nil {
				if noGoTriggers[*req.ReasonCategory] {
					shouldCreateZone = true
				} else if (*req.ReasonCategory == "relocation_request" || *req.ReasonCategory == "pulled_from_service") &&
					req.CreateNoGoZone != nil && *req.CreateNoGoZone {
					shouldCreateZone = true
				}
			}

			// A relocation/address edit flags the OLD location because the bin moved
			// away from it. A status change that vacates the location in place —
			// storing/retiring, OR marking the bin missing — doesn't change coords, so
			// status_change is the trigger and the zone is flagged at the bin's current
			// (soon-to-be-vacated) location. Marking missing mirrors the driver "missing"
			// incident path (shift_complete.go), which always flags a zone.
			statusVacatesLocation := changeType == "status_change" &&
				(isStoring || req.Status == "missing")
			zoneEligibleChange := changeType == "address_change" || changeType == "coordinates_change" ||
				statusVacatesLocation

			if shouldCreateZone && zoneEligibleChange &&
				existing.Latitude != nil && existing.Longitude != nil {
				zoneName := fmt.Sprintf("%s, %s", existing.CurrentStreet, existing.City)
				adminBinChangeSource := "admin_bin_change"
				var incidentDesc string
				switch {
				case req.Status == "missing" && changeType == "status_change":
					incidentDesc = fmt.Sprintf("Bin #%d marked missing by manager. Location flagged — %s", existing.BinNumber, formatIncidentTypeLabel(*req.ReasonCategory))
				case isStoring:
					incidentDesc = fmt.Sprintf("Bin #%d pulled from service by manager. Location flagged — %s", existing.BinNumber, formatIncidentTypeLabel(*req.ReasonCategory))
				default:
					incidentDesc = fmt.Sprintf("Bin #%d address updated by manager. Previous location flagged — %s", existing.BinNumber, formatIncidentTypeLabel(*req.ReasonCategory))
				}
				binIDCopy := id
				// Run on the same transaction so the zone + incident are atomic
				// with the bin update and change-log write below.
				_, createdZoneID, zoneErr := createZoneAndIncidentExt(
					tx,
					*existing.Latitude, *existing.Longitude,
					zoneName,
					*req.ReasonCategory,
					&binIDCopy,
					*userID,
					&incidentDesc,
					nil,      // photoURL
					nil,      // shiftID
					nil,      // checkID
					nil, nil, // reporter GPS
					true, // isFieldObservation
					nowUnix,
					&adminBinChangeSource,
					nil, // moveRequestID
				)
				if zoneErr != nil {
					log.Printf("❌ [UPDATE-BIN] Failed to create no-go zone: %v", zoneErr)
					http.Error(w, "Failed to create no-go zone", http.StatusInternalServerError)
					return
				}
				noGoZoneCreated = true
				// Link the change log to the EXACT zone just created. (Previously this
				// re-derived the id via a nearest-active-zone-at-coords query, which
				// mis-linked when two active zones shared these coordinates — e.g. a bin
				// toggled missing twice.)
				noGoZoneID = &createdZoneID
				log.Printf("✅ [UPDATE-BIN] No-go zone created at old location (%s)", zoneName)
			}

			_, logErr := tx.Exec(`
				INSERT INTO bin_change_log
					(id, bin_id, changed_by_user_id, created_at, change_type,
					 old_values, new_values, reason_category, reason_notes,
					 no_go_zone_created, no_go_zone_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			`,
				changeLogID, id, *userID, nowUnix, changeType,
				string(oldJSON), string(newJSON),
				req.ReasonCategory, req.ReasonNotes,
				noGoZoneCreated, noGoZoneID,
			)
			if logErr != nil {
				log.Printf("❌ [UPDATE-BIN] Failed to write change log: %v", logErr)
				http.Error(w, "Failed to write change log", http.StatusInternalServerError)
				return
			}
			log.Printf("✅ [UPDATE-BIN] Change log entry created (%s)", changeType)
		}

		// ── Cascading Update: Update active shift tasks if address changed ───────
		// DB reads/writes here run inside tx. Driver notifications are deferred
		// until AFTER the commit (see pendingShiftNotifications) so they only
		// fire once the route_task changes are durable, and so NotifyDriverOfRouteUpdate
		// (which reads route_tasks on the bare pool) sees committed data.
		shiftTasksMap := make(map[string][]string)
		if addrChanged && centrifugoClient != nil {
			log.Printf("🔄 [UPDATE-BIN] Address changed - checking for active shift dependencies")

			// Refresh the bin's incomplete tasks on active/scheduled shifts to the
			// new address/coords (domain-owned; atomic with the bin update in tx).
			synced, syncErr := itinerary.SyncBinTasks(tx, id, req.CurrentStreet, req.Latitude, req.Longitude, time.Now().Unix())
			if syncErr != nil {
				log.Printf("❌ [UPDATE-BIN] Failed to sync route tasks: %v", syncErr)
				http.Error(w, "Failed to update route tasks", http.StatusInternalServerError)
				return
			}
			if len(synced) > 0 {
				total := 0
				for sid, tids := range synced {
					shiftTasksMap[sid] = append(shiftTasksMap[sid], tids...)
					total += len(tids)
				}
				log.Printf("🎯 [UPDATE-BIN] Updated %d active shift task(s) to the new address", total)
			} else {
				log.Printf("ℹ️  [UPDATE-BIN] No active shift dependencies found for this bin")
			}
		}

		// ── Cancel pending move requests the manager chose to supersede ─────────────
		// A manual bin edit (e.g. setting the bin to In Warehouse) can make a pending move
		// redundant/contradictory. The dashboard surfaces the move and passes its id here on
		// confirmation, so we cancel it ATOMICALLY with the edit (audited; driver notified
		// post-commit). Unlike CancelBinMoveRequest we do NOT revert the bin — the manual
		// edit is authoritative. Only NON-terminal moves belonging to THIS bin are honored.
		actorID := ""
		if userID != nil {
			actorID = *userID
		}
		type cancelledMove struct {
			id            string
			prevStatus    string
			assignedShift *string
		}
		var cancelledMoves []cancelledMove
		for _, mvID := range req.CancelMoveRequestIDs {
			if mvID == "" {
				continue
			}
			var mv struct {
				Status        string  `db:"status"`
				AssignedShift *string `db:"assigned_shift_id"`
			}
			selErr := tx.Get(&mv, `SELECT status, assigned_shift_id FROM bin_move_requests
				WHERE id = $1 AND bin_id = $2 AND status NOT IN ('completed','cancelled')`, mvID, id)
			if selErr == sql.ErrNoRows {
				log.Printf("⚠️  [UPDATE-BIN] Skip cancel of move %s: not an active move for this bin", mvID)
				continue
			} else if selErr != nil {
				log.Printf("❌ [UPDATE-BIN] Failed to load pending move %s: %v", mvID, selErr)
				http.Error(w, "Failed to check pending move request", http.StatusInternalServerError)
				return
			}
			if err = moverequest.Cancel(tx, mvID, nowUnix); err != nil {
				log.Printf("❌ [UPDATE-BIN] Failed to cancel move %s: %v", mvID, err)
				http.Error(w, "Failed to cancel pending move request", http.StatusInternalServerError)
				return
			}
			if mv.AssignedShift != nil {
				var taskIDs []string
				if e := tx.Select(&taskIDs, `SELECT id FROM route_tasks WHERE move_request_id = $1 AND shift_id = $2 AND is_completed = 0 AND is_deleted = false`, mvID, *mv.AssignedShift); e != nil {
					http.Error(w, "Failed to load move tasks", http.StatusInternalServerError)
					return
				}
				if e := itinerary.RemoveByIDs(tx, taskIDs, actorID, "superseded_by_manual_bin_edit", nowUnix); e != nil {
					http.Error(w, "Failed to remove move tasks", http.StatusInternalServerError)
					return
				}
				if e := itinerary.RecomputeShiftCounts(tx, *mv.AssignedShift, nowUnix); e != nil {
					http.Error(w, "Failed to recompute shift counts", http.StatusInternalServerError)
					return
				}
			}
			cancelledMoves = append(cancelledMoves, cancelledMove{id: mvID, prevStatus: mv.Status, assignedShift: mv.AssignedShift})
			log.Printf("✅ [UPDATE-BIN] Cancelled pending move %s (superseded by manual edit)", mvID)
		}

		// A deactivated bin (missing/retired/in_storage) must drop off every route
		// template immediately — only active bins belong on a collection route. In-tx,
		// so the status change and the template prune commit together.
		if req.Status == "missing" || req.Status == "retired" || req.Status == "in_storage" {
			if pruned, pruneErr := bindomain.PruneFromRouteTemplates(tx, id, now.Unix()); pruneErr != nil {
				log.Printf("❌ [UPDATE-BIN] Failed to prune bin %s from route templates: %v", id, pruneErr)
				http.Error(w, "Failed to update route templates", http.StatusInternalServerError)
				return
			} else if pruned > 0 {
				log.Printf("🧹 [UPDATE-BIN] Pruned bin %s from %d route template(s)", id, pruned)
			}
		}

		// Commit transaction — all writes above (bin update, check record,
		// potential-location snapshot/status, no-go zone + incident, change log,
		// route_task cascade, template prune, superseded move cancellations) are now persisted atomically.
		log.Printf("💾 [UPDATE-BIN] Committing transaction")
		if err := tx.Commit(); err != nil {
			log.Printf("❌ [UPDATE-BIN] Failed to commit transaction: %v", err)
			http.Error(w, "Failed to commit transaction", http.StatusInternalServerError)
			return
		}
		log.Printf("✅ [UPDATE-BIN] Transaction committed successfully")

		// Superseded-move side effects (post-commit, best-effort; the edit already committed).
		if len(cancelledMoves) > 0 {
			actorName := "Unknown Manager"
			if actorID != "" {
				_ = db.Get(&actorName, `SELECT name FROM users WHERE id = $1`, actorID)
			}
			cancelReason := "Superseded by manual bin edit"
			for _, cm := range cancelledMoves {
				if logErr := moverequest.LogCancelled(db, cm.id, actorID, actorName, "manager", cm.prevStatus, &cancelReason); logErr != nil {
					log.Printf("⚠️  [UPDATE-BIN] Failed to log move %s cancellation: %v", cm.id, logErr)
				}
				if cm.assignedShift != nil {
					wsHub.BroadcastToUser(*cm.assignedShift, map[string]interface{}{
						"type":    "move_request_cancelled",
						"bin_id":  id,
						"message": "Move request cancelled — the bin was updated manually by a manager",
					})
					if centrifugoClient != nil {
						if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), *cm.assignedShift, map[string]interface{}{
							"type":            "move_request_cancelled",
							"bin_id":          id,
							"move_request_id": cm.id,
						}); pubErr != nil {
							log.Printf("⚠️  [UPDATE-BIN] Failed to publish move cancel to Centrifugo: %v", pubErr)
						}
					}
				}
			}
		}

		// Notify each affected shift (post-commit, best-effort side effect)
		for shiftID, taskIDs := range shiftTasksMap {
			notifyErr := NotifyDriverOfRouteUpdate(
				db,
				centrifugoClient,
				shiftID,
				"address_changed",
				map[string]interface{}{
					"bin_id":         id,
					"bin_number":     req.BinNumber,
					"old_address":    existing.CurrentStreet,
					"new_address":    req.CurrentStreet,
					"affected_tasks": taskIDs,
				},
			)

			if notifyErr != nil {
				log.Printf("⚠️  [UPDATE-BIN] Failed to notify driver for shift %s: %v", shiftID, notifyErr)
			} else {
				log.Printf("✅ [UPDATE-BIN] Notified driver for shift %s about address change", shiftID)
			}
		}

		// Fetch updated bin
		log.Printf("🔍 [UPDATE-BIN] Fetching updated bin data")
		var updated models.Bin
		err = db.Get(&updated, "SELECT * FROM bins WHERE id = $1", id)
		if err != nil {
			log.Printf("❌ [UPDATE-BIN] Failed to fetch updated bin: %v", err)
			http.Error(w, "Failed to fetch updated bin", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [UPDATE-BIN] Updated bin #%d fetched: %s, %s %s (lat=%v, lng=%v)",
			updated.BinNumber, updated.CurrentStreet, updated.City, updated.Zip,
			updated.Latitude, updated.Longitude)

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bin_updated",
			"data": updated.ToBinResponse(),
		})
		log.Printf("📤 [UPDATE-BIN] WebSocket event broadcasted to managers")

		// Publish to Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_updated", updated.ToBinResponse()); pubErr != nil {
				log.Printf("⚠️  [UPDATE-BIN] Failed to publish bin_updated to Centrifugo: %v", pubErr)
			} else {
				log.Printf("📡 [UPDATE-BIN] Centrifugo: Published bin_updated to company:events")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated.ToBinResponse())

		log.Printf("✅ [UPDATE-BIN] Successfully completed update for bin #%d", updated.BinNumber)
	}
}

func DeleteBin(db *sqlx.DB, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		log.Printf("[DELETE-BIN] Deleting bin ID: %s", id)

		// Get bin first to check if it was converted from a potential location
		var bin models.Bin
		err := db.Get(&bin, "SELECT * FROM bins WHERE id = $1", id)
		if err == sql.ErrNoRows {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("❌ [DELETE-BIN] Error fetching bin: %v", err)
			http.Error(w, "Failed to fetch bin", http.StatusInternalServerError)
			return
		}

		log.Printf("[DELETE-BIN] Found bin #%d, potential_location_id: %v", bin.BinNumber, bin.SourcePotentialLocationID)

		// If bin was converted from a potential location, update its status to 'deleted'
		if bin.SourcePotentialLocationID != nil {
			log.Printf("[DELETE-BIN] 📍 Updating potential location %s to mark bin as deleted", *bin.SourcePotentialLocationID)

			_, err = db.Exec(`
				UPDATE potential_locations
				SET bin_current_status = 'deleted',
					updated_at = $1
				WHERE id = $2
			`, time.Now().Unix(), *bin.SourcePotentialLocationID)

			if err != nil {
				log.Printf("[DELETE-BIN] ⚠️  Error updating potential location status: %v", err)
				// Don't fail the deletion - continue
			} else {
				log.Printf("[DELETE-BIN] ✅ Potential location marked with bin_current_status='deleted'")
			}
		}

		// Delete the bin
		result, err := db.Exec("DELETE FROM bins WHERE id = $1", id)
		if err != nil {
			log.Printf("❌ [DELETE-BIN] Error deleting bin: %v", err)
			http.Error(w, "Failed to delete", http.StatusInternalServerError)
			return
		}

		rows, err := result.RowsAffected()
		if err != nil || rows == 0 {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}

		log.Printf("✅ [DELETE-BIN] Bin #%d deleted successfully", bin.BinNumber)

		// Broadcast to all managers
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bin_deleted",
			"data": map[string]interface{}{
				"bin_id": id,
			},
		})
		log.Printf("📤 [DELETE-BIN] WebSocket event broadcasted to managers")

		// Publish to Centrifugo
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bin_deleted", map[string]interface{}{
				"bin_id": id,
			}); pubErr != nil {
				log.Printf("⚠️  [DELETE-BIN] Failed to publish bin_deleted to Centrifugo: %v", pubErr)
			} else {
				log.Printf("📡 [DELETE-BIN] Centrifugo: Published bin_deleted to company:events")
			}
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// LoadRealBins clears test data and loads real production bins (admin only)
func LoadRealBins(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("🗑️  REQUEST: POST /api/admin/bins/load-real")

		// Step 1: Delete all non-Dallas bins
		deleteResult, err := db.Exec("DELETE FROM bins WHERE city != 'Dallas'")
		if err != nil {
			fmt.Printf("❌ Error deleting test bins: %v\n", err)
			http.Error(w, "Failed to delete test bins", http.StatusInternalServerError)
			return
		}

		deletedRows, _ := deleteResult.RowsAffected()
		fmt.Printf("✅ Deleted %d test bins\n", deletedRows)

		// Step 2: Insert real bins data
		// Using the migration SQL directly embedded in code
		migrationSQL := `
INSERT INTO bins (id, bin_number, current_street, city, zip, last_moved, last_checked, status, fill_percentage, checked, move_requested, latitude, longitude, created_at, updated_at) VALUES
('c96c3c41-fdbd-4777-86eb-326edba84309', 1, '143 E El Camino Real', 'Mountain View', '94040', NULL, 1723403460, 'Missing', 40, 0, 0, 37.37858, -122.071589, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('14a67be5-9b31-4acf-bf48-4aacb39d3130', 2, '1101 W El Camino Real', 'Sunnyvale', '94087', NULL, 1729984568, 'Active', 25, 0, 0, 37.37386, -122.05294, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('8f4f7f05-c61f-4e20-9bc4-6db3f4defd59', 3, '615 Coleman Ave', 'San Jose', '95110', NULL, 1732068105, 'Active', 100, 0, 0, 37.340408, -121.908161, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('2c7d4b00-c070-4515-b91d-da85ec6b53b7', 4, '2400 Charleston Rd', 'Mountain View', '94043', NULL, 1732414957, 'Active', 40, 0, 0, 37.42182, -122.09657, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('826043b0-07a1-455c-ad5e-f7f8b6198262', 5, '1060 E El Camino Real', 'Sunnyvale', '94087', 1729983361, 1732420652, 'Active', 30, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('8e35d3f5-ff41-454b-84e8-224677e740c9', 6, '2161 Monterey Rd', 'San Jose', '95125', NULL, 1723475220, 'Missing', 90, 0, 0, 37.30441, -121.86563, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('d26930d5-394f-4d2b-9b33-759881f791b7', 7, '5055 Almaden Expy', 'San Jose', '95118', NULL, 1732424057, 'Active', 10, 0, 0, 37.25727, -121.8765, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('ebf3cc8a-0c2c-409b-92fb-96fda4b4c2e3', 8, '1933 W El Camino Real', 'Mountain View', '94040', NULL, 1732418838, 'Active', 40, 0, 0, 37.393042, -122.097551, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('153afdd6-fb6c-4ce1-80cc-8ba59302d4db', 9, '5524 Monterey Rd', 'San Jose', '95138', NULL, 1730042536, 'Missing', 100, 0, 0, 37.25637, -121.79907, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('48b39523-183e-49ed-9df5-c0d7352cc29f', 10, '3635 El Camino Real', 'Santa Clara', '95051', NULL, 1732421011, 'Active', 5, 0, 0, 37.352291, -121.988535, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('48f7c1c0-9fab-4b74-bf03-fc8a86156afc', 11, '199 E Middlefield Rd Ste 200', 'Mountain View', '94043', NULL, 1732414062, 'Active', 30, 0, 0, 37.397222, -122.062096, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('0f0ef655-77e2-4738-8976-d04841b41c8c', 12, '1660 Winchester Blvd', 'Campbell', '95008', NULL, NULL, 'Missing', 0, 0, 0, 37.293058, -121.949259, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('f1649781-bcde-4412-a313-a49801de73ed', 13, '1305 S Winchester Blvd', 'San Jose', '95117', NULL, NULL, 'Missing', 0, 0, 0, 37.300899, -121.951779, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('ad92c60c-4b10-4c51-ae47-155b0def8baa', 14, '4644 Meridian Ave', 'San Jose', '95124', NULL, 1732423438, 'Active', 100, 0, 0, 37.256866, -121.897573, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('46f118f7-ac2f-436f-a658-f17ccb0b775c', 15, '1130 Branham Ln', 'San Jose', '95118', NULL, 1722433440, 'Missing', 15, 0, 0, 37.26202, -121.878139, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('2548deaf-8e80-49f6-a2ac-92aaffb45032', 16, '1721 E Bayshore Rd', 'Palo Alto', '94303', NULL, 1732409894, 'Active', 100, 0, 0, 37.460225, -122.137732, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('4c19a8b2-2ffa-47e4-9965-3cc0e4583cdb', 17, '2720 El Camino Real', 'Santa Clara', '95051', NULL, 1732421559, 'Active', 20, 0, 0, 37.352196, -121.975935, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('e30d6984-1ac5-4cce-bc8d-4840ab849197', 18, '1691 The Alameda San Jose, CA  95126 United States', 'San Jose', '95126', 1729448553, 1728108195, 'Missing', 30, 0, 0, 37.337079, -121.919615, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('35ee2834-8b1c-406f-a720-974ecd833dc1', 19, '1041 El Monte Ave', 'Mountain View', '94040', NULL, 1732418844, 'Active', 100, 0, 0, 37.390051, -122.094742, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('fa541645-13fa-479c-93d2-5b8d7cc958ec', 20, '2510 W El Camino Real Suite 2', 'Mountain View', '94040', NULL, 1732416763, 'Active', 25, 0, 0, 37.40003, -122.110255, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('bfa30cb2-14ee-4aa0-871c-7fa78fddbe6a', 21, 'Mountain View Shopping Center 121 E El Camino Real Mountain View, CA  94040 United States', 'Mountain View', '94040', 1729120112, 1732052767, 'Missing', 70, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('1959f69c-0d7e-4f09-a184-17fd47aa3b63', 22, '1757 W San Carlos St San Jose, CA  95128 United States', 'San Jose', '95128', 1728114850, 1732422663, 'Active', 40, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('baf638d9-a0b9-40bc-a232-4b17ccc6e828', 23, '1425 Lafayette St Santa Clara, CA  95050 United States', 'Santa Clara', '95050', 1726533799, 1732059576, 'Active', 40, 0, 0, 37.35466, -121.94572, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('e3f52b60-8164-4adb-9c1c-f908719d3d3a', 24, '3904 Middlefield Rd', 'Palo Alto', '94303', NULL, 1728508791, 'Missing', 20, 0, 0, 37.419241, -122.110524, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('fcbd018f-bfd2-4615-a5fa-06fa96fc0fbf', 25, '2811 Middlefield Rd', 'Palo Alto', '94306', NULL, 1732411205, 'Active', 80, 0, 0, 37.43288, -122.127406, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('c207b5fe-880b-414b-b1fe-02283b5aee36', 26, '887 E El Camino Real Sunnyvale, CA  94087 United States', 'Sunnyvale', '94087', 1725868335, 1732420107, 'Active', 25, 0, 0, 37.354035, -122.014985, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('c552735a-354d-45fe-8637-484f2daa1341', 27, '525 El Camino Real', 'Menlo Park', '94025', NULL, NULL, 'Active', 0, 0, 0, 37.452067, -122.178904, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('6da99eed-a351-4396-80a2-447b1d703eb5', 28, '200 Woodside Plaza', 'Redwood City', '94061', NULL, 1723388760, 'Missing', 100, 0, 0, 37.456535, -122.229593, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('75401257-1c80-4d37-ba9e-6ee484b41487', 29, '5269 Prospect Rd San Jose, CA  95129 United States', 'San Jose', '95129', 1726528434, 1726503609, 'Missing', 0, 0, 1, 37.292949, -121.994242, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('f4906f01-453b-4419-ba4f-6857c6a5ae49', 30, '2495 Lafayette St', 'Santa Clara', '95050', NULL, 1732063444, 'Active', 30, 0, 0, 37.36573, -121.94979, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('74da2fad-2e1e-44ce-896d-2437edb83b1d', 31, '590 Showers Dr', 'Mountain View', '94040', NULL, 1732416751, 'Active', 25, 0, 0, 37.402102, -122.110739, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('1a989b38-f538-4bd9-9444-dcdef185f8c6', 32, '20 Woodside Plaza', 'Redwood City', '94061', NULL, 1723995720, 'Missing', 100, 0, 0, 37.457956, -122.22879, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('efac394f-307a-4b1b-a0ab-21cd8a54d0ae', 33, '1349 Coleman Ave Santa Clara, CA  95050 United States', 'Santa Clara', '95050', 1729456542, 1730656400, 'Active', 20, 0, 0, 37.356773, -121.935764, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('b14813e2-48a1-4075-87ba-654fb362ada3', 34, '2485 El Camino Real', 'Redwood City', '94063', NULL, 1722351600, 'Missing', 50, 0, 0, 37.475639, -122.217094, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('5438c040-1b57-4087-8651-5e1a85fec954', 35, '1920 Camden Ave San Jose, CA  95124 United States', 'San Jose', '95124', 1730768552, 1732422258, 'Active', 2, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('3f9d27a3-bf21-4c7a-987a-5048969afaa4', 36, '2407 El Camino Real Redwood City, CA  94063 United States', 'Redwood City', '94063', 1725860739, 1732410071, 'Active', 100, 0, 0, 37.485323, -122.229117, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('6e7c94b9-e894-4a4c-b20b-d72b19226c5e', 37, '1884 S Norfolk St', 'San Mateo', '94403', NULL, 1732406522, 'Active', 100, 0, 0, 37.554401, -122.29191, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('0a3d7c94-17fd-4951-b11c-698630255f4d', 38, '516 El Camino Real', 'Belmont', '94002', NULL, 1732408347, 'Active', 80, 0, 0, 37.52528, -122.282498, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('da4d6093-eff1-41b4-bd2c-95e97a653122', 39, '1119 Industrial Rd Ste F', 'San Carlos', '94070', NULL, 1727367714, 'Missing', 30, 0, 1, 37.50393, -122.246335, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('5d6c53f6-a03d-4ac2-846c-1fe7382f5e12', 40, '2220 Bridgepointe Pkwy', 'San Mateo', '94404', NULL, 1732407068, 'Active', 100, 0, 0, 37.558595, -122.283297, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('2b92ba74-cd05-426f-8c2c-56094fd60512', 41, '640 Concar Dr', 'San Mateo', '94402', NULL, 1732405547, 'Active', 40, 0, 0, 37.553598, -122.304754, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('64ff1a7c-e994-4544-8188-d3e3ac87aa92', 42, '3904 Middlefield Rd', 'Palo Alto', '94303', 1728582476, 1732415730, 'Active', 40, 0, 0, NULL, NULL, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('35b5527b-25f8-4373-8255-fe1ab0eac396', 43, '2021 The Alameda', 'San Jose', '95126', NULL, 1729183170, 'Active', 5, 0, 0, 37.342805, -121.927995, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT),
('f7aac47e-7479-458d-a717-b792963f9a4f', 44, '4960 Almaden Expy San Jose, CA  95118 United States', 'San Jose', '95118', 1727318409, 1731213193, 'Active', 50, 0, 0, 37.2605, -121.874759, EXTRACT(EPOCH FROM NOW())::BIGINT, EXTRACT(EPOCH FROM NOW())::BIGINT);
`

		_, err = db.Exec(migrationSQL)
		if err != nil {
			fmt.Printf("❌ Error inserting bins: %v\n", err)
			http.Error(w, "Failed to load bins", http.StatusInternalServerError)
			return
		}

		fmt.Println("✅ Inserted 44 real bins")

		// Query summary
		var summary struct {
			TotalBins         int `db:"total_bins"`
			ActiveBins        int `db:"active_bins"`
			MissingBins       int `db:"missing_bins"`
			BinsWithoutCoords int `db:"bins_without_coords"`
		}

		err = db.Get(&summary, `
			SELECT
				COUNT(*) AS total_bins,
				COUNT(CASE WHEN status = 'Active' THEN 1 END) AS active_bins,
				COUNT(CASE WHEN status = 'Missing' THEN 1 END) AS missing_bins,
				COUNT(CASE WHEN latitude IS NULL OR longitude IS NULL THEN 1 END) AS bins_without_coords
			FROM bins
			WHERE city != 'Dallas'
		`)

		if err != nil {
			fmt.Printf("⚠️  Error querying summary: %v\n", err)
			// Still return success since insert worked
		} else {
			fmt.Printf("📊 Summary: %d total, %d active, %d missing, %d without coords\n",
				summary.TotalBins, summary.ActiveBins, summary.MissingBins, summary.BinsWithoutCoords)
		}

		// Return success response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":             true,
			"message":             "Successfully loaded real bins",
			"deleted_test_bins":   deletedRows,
			"loaded_bins":         44,
			"total_bins":          summary.TotalBins,
			"active_bins":         summary.ActiveBins,
			"missing_bins":        summary.MissingBins,
			"bins_without_coords": summary.BinsWithoutCoords,
		})
	}
}

// FixBinStatus lowercases all bin status values for Flutter compatibility
func FixBinStatus(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("🔧 REQUEST: POST /api/admin/bins/fix-status")

		// Update all bin statuses to lowercase
		result, err := db.Exec("UPDATE bins SET status = LOWER(status)")
		if err != nil {
			fmt.Printf("❌ Error updating status: %v\n", err)
			http.Error(w, "Failed to update bin statuses", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		fmt.Printf("✅ Updated %d bin statuses to lowercase\n", rowsAffected)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":       true,
			"message":       "Fixed bin status casing",
			"rows_affected": rowsAffected,
		})
	}
}

// GetBinChangeLog returns the change log for a specific bin
// GET /api/bins/:id/change-log
func GetBinChangeLog(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "id")
		if binID == "" {
			http.Error(w, "Missing bin ID", http.StatusBadRequest)
			return
		}

		type ChangeLogEntry struct {
			ID              string  `db:"id" json:"id"`
			BinID           string  `db:"bin_id" json:"bin_id"`
			ChangedByUserID string  `db:"changed_by_user_id" json:"changed_by_user_id"`
			ChangedByName   *string `db:"changed_by_name" json:"changed_by_name,omitempty"`
			ChangeType      string  `db:"change_type" json:"change_type"`
			OldValues       string  `db:"old_values" json:"old_values"`
			NewValues       string  `db:"new_values" json:"new_values"`
			ReasonCategory  *string `db:"reason_category" json:"reason_category,omitempty"`
			ReasonNotes     *string `db:"reason_notes" json:"reason_notes,omitempty"`
			NoGoZoneCreated bool    `db:"no_go_zone_created" json:"no_go_zone_created"`
			NoGoZoneID      *string `db:"no_go_zone_id" json:"no_go_zone_id,omitempty"`
			CreatedAt       int64   `db:"created_at" json:"created_at"`
			CreatedAtIso    string  `json:"created_at_iso"`
		}

		rows, err := db.Queryx(`
			SELECT
				bcl.id,
				bcl.bin_id,
				bcl.changed_by_user_id,
				u.name AS changed_by_name,
				bcl.change_type,
				bcl.old_values,
				bcl.new_values,
				bcl.reason_category,
				bcl.reason_notes,
				bcl.no_go_zone_created,
				bcl.no_go_zone_id,
				bcl.created_at
			FROM bin_change_log bcl
			LEFT JOIN users u ON u.id = bcl.changed_by_user_id
			WHERE bcl.bin_id = $1
			ORDER BY bcl.created_at DESC
		`, binID)
		if err != nil {
			log.Printf("Error querying bin change log: %v", err)
			http.Error(w, "Failed to fetch change log", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		entries := []ChangeLogEntry{}
		for rows.Next() {
			var entry ChangeLogEntry
			if err := rows.StructScan(&entry); err != nil {
				log.Printf("Error scanning change log row: %v", err)
				continue
			}
			entry.CreatedAtIso = strings.Replace(
				fmt.Sprintf("%s", time.Unix(entry.CreatedAt, 0).UTC().Format(time.RFC3339)),
				" ", "T", 1,
			)
			entries = append(entries, entry)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	}
}
