package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/helpers"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/websocket"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// stringPtrEqual compares two string pointers for equality
// Returns true if both are nil or both point to the same string value
func stringPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// calculateUrgency determines the urgency level based on status and scheduled date
// Returns "resolved" for completed/cancelled moves, otherwise calculates time-based urgency
func calculateUrgency(status string, scheduledDate int64) string {
	// If move is completed or cancelled, urgency is "resolved"
	if status == "completed" || status == "cancelled" {
		return "resolved"
	}

	// Otherwise calculate urgency based on time until scheduled date
	now := time.Now().Unix()
	hoursUntil := float64(scheduledDate-now) / 3600.0

	if hoursUntil < 0 {
		return "overdue"
	} else if hoursUntil < 24 {
		return "urgent"
	} else if hoursUntil < 72 {
		return "soon"
	} else {
		return "scheduled"
	}
}

// ScheduleBinMove creates a new bin move request (urgent or future scheduled)
// POST /api/manager/bins/schedule-move
func ScheduleBinMove(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req models.CreateBinMoveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.BinID == "" || req.MoveType == "" {
			http.Error(w, "Missing required fields: bin_id, move_type", http.StatusBadRequest)
			return
		}

		// Auto-calculate urgency from scheduled_date
		now := time.Now().Unix()
		hoursUntil := float64(req.ScheduledDate-now) / 3600.0
		var urgency string
		if hoursUntil < 24 {
			urgency = "urgent"
		} else {
			urgency = "scheduled"
		}

		// Validate move_type (accept both 'store' and 'pickup_only' for backward compatibility)
		if req.MoveType != "store" && req.MoveType != "pickup_only" && req.MoveType != "relocation" && req.MoveType != "redeployment" {
			http.Error(w, "Invalid move_type: must be 'store', 'relocation', or 'redeployment'", http.StatusBadRequest)
			return
		}

		// Validate pickup_only moves require disposal_action
		if req.MoveType == "pickup_only" && req.DisposalAction == nil {
			http.Error(w, "pickup_only moves require disposal_action ('retire' or 'store')", http.StatusBadRequest)
			return
		}

		// Build new address from separate fields or use provided address
		var newAddress *string
		if req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil {
			// Build from separate fields (new format from frontend)
			combined := fmt.Sprintf("%s, %s %s", *req.NewStreet, *req.NewCity, *req.NewZip)
			newAddress = &combined
		} else if req.NewAddress != nil {
			// Use provided address (backward compatibility)
			newAddress = req.NewAddress
		}

		// Validate relocation moves require new location
		if req.MoveType == "relocation" && (req.NewLatitude == nil || req.NewLongitude == nil || newAddress == nil) {
			http.Error(w, "relocation moves require new_latitude, new_longitude, and address (either new_address or new_street+new_city+new_zip)", http.StatusBadRequest)
			return
		}

		// Validate redeployment moves require new location and bin must be in_storage
		if req.MoveType == "redeployment" {
			if req.NewLatitude == nil || req.NewLongitude == nil || newAddress == nil {
				http.Error(w, "redeployment moves require new_latitude, new_longitude, and address (either new_address or new_street+new_city+new_zip)", http.StatusBadRequest)
				return
			}
		}

		// Auto-fill warehouse destination for "store" moves
		if req.MoveType == "store" {
			log.Printf("🏭 [STORE MOVE] Auto-filling warehouse destination for move request")

			// Fetch warehouse location from config (no fallback - must be configured)
			var warehouseJSON []byte
			err := db.QueryRow(`
				SELECT value
				FROM config
				WHERE key = 'warehouse_location'
			`).Scan(&warehouseJSON)

			if err == sql.ErrNoRows {
				log.Printf("❌ [STORE MOVE] Warehouse location not configured in database")
				http.Error(w, "Warehouse location must be configured before creating store moves. Please contact your administrator.", http.StatusPreconditionFailed)
				return
			}

			if err != nil {
				log.Printf("❌ [STORE MOVE] Failed to fetch warehouse location: %v", err)
				http.Error(w, "Failed to fetch warehouse location", http.StatusInternalServerError)
				return
			}

			var warehouse models.WarehouseLocation
			if err := json.Unmarshal(warehouseJSON, &warehouse); err != nil {
				log.Printf("❌ [STORE MOVE] Failed to parse warehouse location: %v", err)
				http.Error(w, "Failed to parse warehouse location", http.StatusInternalServerError)
				return
			}

			// Auto-set destination to warehouse
			req.NewLatitude = &warehouse.Latitude
			req.NewLongitude = &warehouse.Longitude
			newAddress = &warehouse.Address

			log.Printf("✅ [STORE MOVE] Warehouse destination set: %s (%.6f, %.6f)",
				warehouse.Address, warehouse.Latitude, warehouse.Longitude)
		}

		// Get requesting user ID from context (set by Auth middleware)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		userID := userClaims.UserID

		// Fetch bin to get current location
		var bin models.Bin
		err := db.Get(&bin, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
			FROM bins
			WHERE id = $1
		`, req.BinID)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Bin not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching bin: %v", err)
			http.Error(w, "Failed to fetch bin", http.StatusInternalServerError)
			return
		}

		// Validate bin has location
		if bin.Latitude == nil || bin.Longitude == nil {
			http.Error(w, "Bin must have latitude and longitude coordinates", http.StatusBadRequest)
			return
		}

		// Validate redeployment moves require bin to be in_storage
		if req.MoveType == "redeployment" && bin.Status != "in_storage" {
			http.Error(w, "redeployment moves require bin status to be 'in_storage' (current status: "+bin.Status+")", http.StatusBadRequest)
			return
		}

		// Build original address
		originalAddress := fmt.Sprintf("%s, %s %s", bin.CurrentStreet, bin.City, bin.Zip)

		// Generate ID (now already declared above for urgency calculation)
		id := uuid.New().String()

		// Determine status and assignment type based on whether shift is assigned
		status := "pending"
		var assignmentType *string // nil for unassigned moves
		if req.ShiftID != nil {
			status = "assigned" // Immediately assigned to shift
			shiftType := "shift"
			assignmentType = &shiftType
		}

		// Create bin move request
		// Note: For redeployment moves, original_latitude/longitude will be warehouse location (where in_storage bin currently is)
		moveRequest := models.BinMoveRequest{
			ID:                        id,
			BinID:                     req.BinID,
			ScheduledDate:             req.ScheduledDate,
			Urgency:                   urgency, // Auto-calculated urgency
			RequestedBy:               userID,
			Status:                    status,
			OriginalLatitude:          *bin.Latitude,  // For redeployment: warehouse pickup location
			OriginalLongitude:         *bin.Longitude, // For redeployment: warehouse pickup location
			OriginalAddress:           originalAddress,
			NewLatitude:               req.NewLatitude,
			NewLongitude:              req.NewLongitude,
			NewAddress:                newAddress, // Built from separate fields or provided address
			MoveType:                  req.MoveType,
			DisposalAction:            req.DisposalAction,
			Reason:                    req.Reason,
			Notes:                     req.Notes,
			SourcePotentialLocationID: req.SourcePotentialLocationID, // For warehouse redeployments to potential locations
			AssignmentType:            assignmentType,                // Set based on whether shift is assigned
			AssignedShiftID:           req.ShiftID,                   // Assign to shift if provided
			CreatedAt:                 now,
			UpdatedAt:                 now,
		}

		// Insert into database
		_, err = db.Exec(`
			INSERT INTO bin_move_requests (
				id, bin_id, scheduled_date, urgency, requested_by, status,
				original_latitude, original_longitude, original_address,
				new_latitude, new_longitude, new_address,
				move_type, disposal_action, reason, notes,
				source_potential_location_id,
				assignment_type, assigned_shift_id,
				created_at, updated_at,
				reason_category, no_go_zone_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		`,
			moveRequest.ID, moveRequest.BinID, moveRequest.ScheduledDate,
			moveRequest.Urgency, moveRequest.RequestedBy, moveRequest.Status,
			moveRequest.OriginalLatitude, moveRequest.OriginalLongitude, moveRequest.OriginalAddress,
			moveRequest.NewLatitude, moveRequest.NewLongitude, moveRequest.NewAddress,
			moveRequest.MoveType, moveRequest.DisposalAction, moveRequest.Reason, moveRequest.Notes,
			moveRequest.SourcePotentialLocationID,
			moveRequest.AssignmentType, moveRequest.AssignedShiftID,
			moveRequest.CreatedAt, moveRequest.UpdatedAt,
			req.ReasonCategory, nil, // reason_category, no_go_zone_id (zone created after insert)
		)
		if err != nil {
			log.Printf("Error creating bin move request: %v", err)
			http.Error(w, "Failed to create move request", http.StatusInternalServerError)
			return
		}

		// --- No-go zone creation logic ---
		// Determine if we should auto-create a no-go zone based on reason_category
		shouldCreateZone := false
		if req.ReasonCategory != nil {
			switch *req.ReasonCategory {
			case "landlord_complaint", "theft", "vandalism", "missing":
				shouldCreateZone = true
			case "relocation_request":
				// Only create if explicitly opted in
				if req.CreateNoGoZone != nil && *req.CreateNoGoZone {
					shouldCreateZone = true
				}
			}
		}

		if shouldCreateZone {
			moveRequestSource := "move_request"
			idCopy := id
			// Map reason category to incident type
			incidentType := "relocation_request"
			if req.ReasonCategory != nil {
				switch *req.ReasonCategory {
				case "landlord_complaint":
					incidentType = "landlord_complaint"
				case "theft":
					incidentType = "theft"
				case "vandalism":
					incidentType = "vandalism"
				case "missing":
					incidentType = "missing"
				}
			}
			// Use bin address for zone name (matches manual zone creation UX)
			zoneName := fmt.Sprintf("%s - %s", bin.CurrentStreet, bin.City)
			binIDCopy := req.BinID
			// Use manager notes as description, or generate a fallback
			incidentNotes := req.Notes
			if incidentNotes == nil || *incidentNotes == "" {
				fallback := fmt.Sprintf("Bin #%d move request created — %s", bin.BinNumber, formatIncidentTypeLabel(incidentType))
				incidentNotes = &fallback
			}
			_, zoneErr := createZoneAndIncident(
				db, centrifugoClient,
				*bin.Latitude, *bin.Longitude,
				zoneName, incidentType,
				&binIDCopy, userID,
				incidentNotes, nil,
				nil, nil,
				nil, nil,
				false, now,
				&moveRequestSource,
				&idCopy,
			)
			if zoneErr != nil {
				log.Printf("⚠️  Warning: Failed to create no-go zone for move request %s: %v", id, zoneErr)
			} else {
				// Retrieve the zone ID from the nearest zone at those coordinates
				var zoneID string
				nearErr := db.Get(&zoneID, `
					SELECT id FROM no_go_zones
					ORDER BY (
						(center_latitude - $1) * (center_latitude - $1) +
						(center_longitude - $2) * (center_longitude - $2)
					) ASC
					LIMIT 1
				`, *bin.Latitude, *bin.Longitude)
				if nearErr != nil {
					log.Printf("⚠️  Warning: Could not retrieve zone ID for move request %s: %v", id, nearErr)
				} else {
					_, updErr := db.Exec(`
						UPDATE bin_move_requests SET no_go_zone_id = $1 WHERE id = $2
					`, zoneID, id)
					if updErr != nil {
						log.Printf("⚠️  Warning: Failed to link no_go_zone_id to move request %s: %v", id, updErr)
					}
				}
			}
		}
		// --- End no-go zone creation logic ---

		// Log history: move request created (with enhanced metadata)
		var userName string
		err = db.Get(&userName, `SELECT name FROM users WHERE id = $1`, userID)
		if err != nil {
			log.Printf("Warning: Failed to fetch user name for history: %v", err)
			userName = "Unknown User"
		}
		err = helpers.LogMoveRequestCreated(db, id, userID, userName, req.MoveType, newAddress)
		if err != nil {
			log.Printf("Warning: Failed to log move request creation: %v", err)
			// Don't fail the request, just log the warning
		}

		// Update bin status to pending_move
		_, err = db.Exec(`
			UPDATE bins
			SET status = 'pending_move', updated_at = $1
			WHERE id = $2
		`, now, req.BinID)
		if err != nil {
			log.Printf("Warning: Failed to update bin status: %v", err)
			// Don't fail the request, just log the warning
		}

		log.Printf("✅ Move request created successfully (status: pending)")
		log.Printf("   To assign to a shift, use POST /api/manager/bins/move-requests/%s/assign-to-shift", id)

		// Return the created move request
		response := moveRequest.ToBinMoveRequestResponse()
		response.Bin = &models.BinResponse{
			ID:            bin.ID,
			BinNumber:     bin.BinNumber,
			CurrentStreet: bin.CurrentStreet,
			City:          bin.City,
			Zip:           bin.Zip,
			Latitude:      bin.Latitude,
			Longitude:     bin.Longitude,
			Status:        bin.Status,
		}
		// Flatten bin fields for easy table display
		response.BinNumber = bin.BinNumber
		response.CurrentStreet = bin.CurrentStreet
		response.City = bin.City
		response.Zip = bin.Zip

		// Parse new address into separate fields if available
		if moveRequest.NewAddress != nil {
			// Split "street, city zip" format
			parts := strings.Split(*moveRequest.NewAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					response.NewStreet = &street
					response.NewCity = &city
					response.NewZip = &zip
				}
			}
		}

		// Publish move_request_created to company:events so all manager dashboards update
		moveCreatedData := map[string]interface{}{
			"move_request_id": id,
			"bin_id":          req.BinID,
			"status":          "pending",
		}
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "move_request_created", moveCreatedData); pubErr != nil {
				log.Printf("⚠️  Failed to publish move_request_created to Centrifugo: %v", pubErr)
			}

			// Create per-user notifications for admins
			adminIDs, _ := services.GetAdminUserIDs(db)
			services.CreateNotificationForUsers(db, adminIDs, "move_request_created",
				"New Move Request",
				fmt.Sprintf("Move request created for bin %s", req.BinID),
				moveCreatedData)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(response)
	}
}

// AssignMoveToShift explicitly assigns a pending move request to a shift
// POST /api/manager/bins/move-requests/:id/assign-to-shift
func AssignMoveToShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		moveRequestID := chi.URLParam(r, "id")
		log.Printf("🚚 [ASSIGN TO SHIFT] Starting assignment for move request: %s", moveRequestID)
		if moveRequestID == "" {
			log.Printf("❌ [ASSIGN TO SHIFT] Missing move request ID")
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		var req struct {
			ShiftID          *string `json:"shift_id"`            // Optional - auto-find active shift if nil
			InsertAfterBinID *string `json:"insert_after_bin_id"` // For active shifts - insert after specific bin
			InsertPosition   *string `json:"insert_position"`     // For future shifts - 'start' or 'end'
		}
		json.NewDecoder(r.Body).Decode(&req)

		log.Printf("🚚 [ASSIGN TO SHIFT] Request body - ShiftID: %v, InsertAfterBinID: %v, InsertPosition: %v",
			req.ShiftID, req.InsertAfterBinID, req.InsertPosition)

		// Fetch move request
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT * FROM bin_move_requests WHERE id = $1
		`, moveRequestID)
		if err != nil {
			if err == sql.ErrNoRows {
				log.Printf("❌ [ASSIGN TO SHIFT] Move request not found: %s", moveRequestID)
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("❌ [ASSIGN TO SHIFT] Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		log.Printf("🚚 [ASSIGN TO SHIFT] Found move request - Status: %s, BinID: %s", moveRequest.Status, moveRequest.BinID)

		// Check if can be assigned (only pending, assigned, or in_progress moves can be reassigned)
		if moveRequest.Status != "pending" && moveRequest.Status != "assigned" && moveRequest.Status != "in_progress" {
			log.Printf("❌ [ASSIGN TO SHIFT] Cannot reassign move request with status: %s", moveRequest.Status)
			http.Error(w, fmt.Sprintf("Cannot reassign move request with status: %s", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// Fetch bin details
		var bin models.Bin
		err = db.Get(&bin, "SELECT * FROM bins WHERE id = $1", moveRequest.BinID)
		if err != nil {
			log.Printf("❌ [ASSIGN TO SHIFT] Bin not found: %s", moveRequest.BinID)
			http.Error(w, "Bin not found", http.StatusNotFound)
			return
		}

		log.Printf("🚚 [ASSIGN TO SHIFT] Found bin - Number: %d", bin.BinNumber)

		// Get manager ID and name from context
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			log.Printf("❌ [ASSIGN TO SHIFT] User not authenticated")
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		managerID := userClaims.UserID

		var managerName string
		err = db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerID)
		if err != nil {
			log.Printf("Warning: Failed to fetch manager name: %v", err)
			managerName = "Unknown Manager"
		}

		// Call the assignment logic
		err = assignMoveToShift(db, wsHub, fcmService, centrifugoClient, moveRequest, bin, req.ShiftID, req.InsertAfterBinID, req.InsertPosition, managerID, managerName)
		if err != nil {
			log.Printf("❌ [ASSIGN TO SHIFT] Error assigning move to shift: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [ASSIGN TO SHIFT] Successfully assigned move request %s to shift", moveRequestID)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Move request assigned to shift successfully",
		})
	}
}

// assignMoveToShift inserts move at specified position in shift and re-optimizes route
func assignMoveToShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client, moveRequest models.BinMoveRequest, bin models.Bin, shiftID *string, insertAfterBinID *string, insertPosition *string, managerID string, managerName string) error {
	log.Printf("🚚 ASSIGN MOVE: Assigning move request for bin #%d to shift", bin.BinNumber)

	// Store previous assignment info for history logging
	previousAssignedShiftID := moveRequest.AssignedShiftID
	var previousAssignedUserID *string
	var previousAssignedUserName *string
	var previousAssignmentType *string

	if moveRequest.AssignedUserID != nil {
		previousAssignedUserID = moveRequest.AssignedUserID
		var prevUserName string
		if err := db.Get(&prevUserName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID); err == nil {
			previousAssignedUserName = &prevUserName
		}
	}
	if moveRequest.AssignmentType != nil {
		previousAssignmentType = moveRequest.AssignmentType
	}

	// 1. Find shift (use provided ID or auto-find active shift)
	var activeShift models.Shift
	var err error

	if shiftID != nil && *shiftID != "" {
		// Use specific shift ID
		log.Printf("   Using specified shift ID: %s", *shiftID)
		err = db.Get(&activeShift, "SELECT * FROM shifts WHERE id = $1", *shiftID)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("shift not found: %s", *shiftID)
			}
			return fmt.Errorf("failed to fetch shift: %w", err)
		}
	} else {
		// Auto-find active/paused shift
		log.Printf("   Auto-finding active shift...")
		err = db.Get(&activeShift, `
			SELECT * FROM shifts
			WHERE status IN ('active', 'paused')
			ORDER BY
				CASE status
					WHEN 'active' THEN 1
					WHEN 'paused' THEN 2
				END ASC,
				created_at DESC
			LIMIT 1
		`)
		if err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("no active shift found - please specify shift_id")
			}
			return fmt.Errorf("failed to find active shift: %w", err)
		}
	}

	log.Printf("   Found active shift: %s (driver: %s, status: %s)", activeShift.ID, activeShift.DriverID, activeShift.Status)

	// 2. Determine current position in route (find first uncompleted bin)
	var shiftBins []models.ShiftBinWithDetails
	err = db.Select(&shiftBins, `
		SELECT rt.id, rt.shift_id, rt.bin_id, rt.sequence_order, rt.is_completed,
		       COALESCE(b.bin_number, 0) as bin_number, COALESCE(b.current_street, rt.address, '') as current_street,
		       COALESCE(b.city, '') as city, COALESCE(b.zip, '') as zip, COALESCE(b.fill_percentage, 0) as fill_percentage,
		       COALESCE(b.latitude, rt.latitude) as latitude, COALESCE(b.longitude, rt.longitude) as longitude
		FROM route_tasks rt
		LEFT JOIN bins b ON rt.bin_id = b.id
		WHERE rt.shift_id = $1
		ORDER BY rt.sequence_order ASC
	`, activeShift.ID)
	if err != nil {
		return fmt.Errorf("failed to fetch shift bins: %w", err)
	}

	// Determine where to insert the bin based on shift status and parameters
	var insertSequenceOrder int

	now := time.Now().Unix()
	isActiveShift := activeShift.Status == "active"
	isFutureShift := activeShift.Status == "ready"  // FIX: "ready" is the correct status for future shifts

	log.Printf("   🔍 SHIFT STATUS DEBUG: status=%s, isActiveShift=%t, isFutureShift=%t", activeShift.Status, isActiveShift, isFutureShift)

	// CASE 1: Active shift with specific insertAfterBinID
	if isActiveShift && insertAfterBinID != nil && *insertAfterBinID != "" {
		log.Printf("   Inserting after specific bin ID: %s", *insertAfterBinID)
		// Find the specified bin in the route
		targetIndex := -1
		for i, sb := range shiftBins {
			if sb.BinID == *insertAfterBinID {
				targetIndex = i
				break
			}
		}

		if targetIndex == -1 {
			return fmt.Errorf("specified bin not found in shift route: %s", *insertAfterBinID)
		}

		insertSequenceOrder = shiftBins[targetIndex].SequenceOrder + 1
		log.Printf("   Inserting after bin #%d at sequence %d", shiftBins[targetIndex].BinNumber, insertSequenceOrder)
	} else if isFutureShift && insertPosition != nil {
		// CASE 2: Future shift with insertPosition ('start' or 'end')
		if *insertPosition == "start" {
			log.Printf("   Inserting at START of future shift")
			insertSequenceOrder = 1
		} else { // 'end'
			log.Printf("   Inserting at END of future shift")
			if len(shiftBins) > 0 {
				lastBin := shiftBins[len(shiftBins)-1]
				insertSequenceOrder = lastBin.SequenceOrder + 1
			} else {
				insertSequenceOrder = 1
			}
		}
	} else {
		// CASE 3: Default behavior - insert as "next waypoint" for active shifts
		currentIndex := -1
		for i, sb := range shiftBins {
			if sb.IsCompleted == 0 {
				currentIndex = i
				break
			}
		}

		if currentIndex == -1 {
			log.Printf("⚠️  All bins completed - inserting at end")
			if len(shiftBins) > 0 {
				lastBin := shiftBins[len(shiftBins)-1]
				insertSequenceOrder = lastBin.SequenceOrder + 1
			} else {
				insertSequenceOrder = 1
			}
		} else {
			log.Printf("   Current position: bin #%d at index %d", shiftBins[currentIndex].BinNumber, currentIndex)
			log.Printf("   Inserting as next waypoint (index %d)", currentIndex+1)
			insertSequenceOrder = shiftBins[currentIndex].SequenceOrder + 1
		}
	}

	// 3. Insert move request bin at determined position
	// Determine how many waypoints to add (pickup only, or pickup + dropoff)
	binsAdded := 1
	if moveRequest.MoveType == "relocation" {
		binsAdded = 2
		log.Printf("   Relocation move - will add both pickup and dropoff waypoints")
	} else {
		log.Printf("   Store move - will add pickup waypoint only")
	}

	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Shift all tasks after insert position up by binsAdded
	_, err = tx.Exec(`
		UPDATE route_tasks
		SET sequence_order = sequence_order + $1
		WHERE shift_id = $2 AND sequence_order >= $3
	`, binsAdded, activeShift.ID, insertSequenceOrder)
	if err != nil {
		return fmt.Errorf("failed to shift sequence order: %w", err)
	}

	// Insert pickup waypoint for move request
	pickupSeq := insertSequenceOrder
	pickupID := uuid.New().String()
	log.Printf("   🔍 DEBUG: About to insert PICKUP - insertSequenceOrder=%d, pickupSeq=%d", insertSequenceOrder, pickupSeq)
	log.Printf("   🔍 DEBUG: INSERT params: shift_id=%s, bin_id=%s, sequence=%d, task_type=pickup, move_request_id=%s",
		activeShift.ID, moveRequest.BinID, pickupSeq, moveRequest.ID)

	// For pickup, use bin's current location
	pickupAddress := bin.CurrentStreet
	moveTypeStr := string(moveRequest.MoveType)
	_, err = tx.Exec(`
		INSERT INTO route_tasks (
			id, shift_id, bin_id, bin_number, sequence_order, task_type,
			latitude, longitude, address, fill_percentage,
			move_request_id, move_type,
			is_completed, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 0, $13)
	`, pickupID, activeShift.ID, moveRequest.BinID, bin.BinNumber, pickupSeq, "pickup",
		bin.Latitude, bin.Longitude, pickupAddress, bin.FillPercentage,
		moveRequest.ID, moveTypeStr, now)
	if err != nil {
		return fmt.Errorf("failed to insert pickup waypoint: %w", err)
	}
	log.Printf("   ✅ Inserted pickup waypoint at sequence %d", pickupSeq)

	// For relocation moves, also insert dropoff waypoint immediately after pickup
	if moveRequest.MoveType == "relocation" {
		dropoffSeq := insertSequenceOrder + 1
		dropoffID := uuid.New().String()
		log.Printf("   🔍 DEBUG: About to insert DROPOFF - insertSequenceOrder=%d, dropoffSeq=%d", insertSequenceOrder, dropoffSeq)
		log.Printf("   🔍 DEBUG: INSERT params: shift_id=%s, bin_id=%s, sequence=%d, task_type=dropoff, move_request_id=%s",
			activeShift.ID, moveRequest.BinID, dropoffSeq, moveRequest.ID)

		// For dropoff, use destination location
		// Destination coordinates should be in NewLatitude/NewLongitude fields
		var dropoffLat, dropoffLng float64
		var dropoffAddr string
		if moveRequest.NewLatitude != nil && moveRequest.NewLongitude != nil {
			dropoffLat = *moveRequest.NewLatitude
			dropoffLng = *moveRequest.NewLongitude
		}
		if moveRequest.NewAddress != nil {
			dropoffAddr = *moveRequest.NewAddress
		}

		_, err = tx.Exec(`
			INSERT INTO route_tasks (
				id, shift_id, bin_id, bin_number, sequence_order, task_type,
				latitude, longitude, address,
				destination_latitude, destination_longitude, destination_address,
				move_request_id, move_type,
				is_completed, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 0, $15)
		`, dropoffID, activeShift.ID, moveRequest.BinID, bin.BinNumber, dropoffSeq, "dropoff",
			dropoffLat, dropoffLng, dropoffAddr,
			dropoffLat, dropoffLng, dropoffAddr,
			moveRequest.ID, moveTypeStr, now)
		if err != nil {
			return fmt.Errorf("failed to insert dropoff waypoint: %w", err)
		}
		log.Printf("   ✅ Inserted dropoff waypoint at sequence %d", dropoffSeq)

		// Verify both waypoints have different sequence_order values
		var actualPickupSeq, actualDropoffSeq int
		err = tx.Get(&actualPickupSeq, `
			SELECT sequence_order FROM route_tasks
			WHERE shift_id = $1 AND move_request_id = $2 AND task_type = 'pickup'
		`, activeShift.ID, moveRequest.ID)
		if err != nil {
			log.Printf("   ⚠️  Warning: Could not verify pickup sequence_order: %v", err)
		}

		err = tx.Get(&actualDropoffSeq, `
			SELECT sequence_order FROM route_tasks
			WHERE shift_id = $1 AND move_request_id = $2 AND task_type = 'dropoff'
		`, activeShift.ID, moveRequest.ID)
		if err != nil {
			log.Printf("   ⚠️  Warning: Could not verify dropoff sequence_order: %v", err)
		}

		log.Printf("   🔍 VERIFICATION: Expected pickup=%d, actual=%d | Expected dropoff=%d, actual=%d",
			pickupSeq, actualPickupSeq, dropoffSeq, actualDropoffSeq)

		if actualPickupSeq == actualDropoffSeq {
			log.Printf("   ❌ ERROR: Pickup and dropoff have SAME sequence_order: %d", actualPickupSeq)
			return fmt.Errorf("duplicate sequence_order detected: both pickup and dropoff at %d", actualPickupSeq)
		}
		if actualPickupSeq >= actualDropoffSeq {
			log.Printf("   ❌ ERROR: Pickup sequence (%d) >= Dropoff sequence (%d)", actualPickupSeq, actualDropoffSeq)
			return fmt.Errorf("invalid sequence order: pickup at %d, dropoff at %d", actualPickupSeq, actualDropoffSeq)
		}
		log.Printf("   ✅ VALIDATION PASSED: Pickup (%d) < Dropoff (%d)", actualPickupSeq, actualDropoffSeq)
	}

	// Update move request to assign it to this shift (clear any previous user assignment)
	// If shift is already active, set status to 'in_progress', otherwise 'assigned'
	moveRequestStatus := "assigned"
	if isActiveShift {
		moveRequestStatus = "in_progress"
		log.Printf("   📊 Move request status: 'in_progress' (shift is ACTIVE)")
	} else {
		log.Printf("   📊 Move request status: 'assigned' (shift is NOT active, status=%s)", activeShift.Status)
	}

	_, err = tx.Exec(`
		UPDATE bin_move_requests
		SET assignment_type = 'shift', assigned_shift_id = $1, assigned_user_id = NULL, status = $2, updated_at = $3
		WHERE id = $4
	`, activeShift.ID, moveRequestStatus, now, moveRequest.ID)
	if err != nil {
		return fmt.Errorf("failed to update move request: %w", err)
	}

	// Get driver info from shift
	var driverName string
	err = db.Get(&driverName, `SELECT name FROM users WHERE id = $1`, activeShift.DriverID)
	if err != nil {
		log.Printf("Warning: Failed to fetch driver name for history: %v", err)
		driverName = "Unknown Driver"
	}

	// Log history: check if reassignment or new assignment
	newAssignmentType := "shift"
	if previousAssignedShiftID == nil && previousAssignedUserID == nil {
		// New assignment
		log.Printf("   📝 Logging NEW assignment to history (driver: %s, shift: %s)", driverName, activeShift.ID)
		err = helpers.LogMoveRequestAssigned(db, moveRequest.ID, managerID, managerName,
			newAssignmentType, &activeShift.DriverID, &driverName, &activeShift.ID)
		if err != nil {
			log.Printf("   ⚠️  WARNING: Failed to log assignment history: %v", err)
		} else {
			log.Printf("   ✅ Assignment history logged successfully")
		}
	} else {
		// Reassignment
		log.Printf("   📝 Logging REASSIGNMENT to history (from previous assignment to driver: %s, shift: %s)", driverName, activeShift.ID)
		err = helpers.LogMoveRequestReassigned(db, moveRequest.ID, managerID, managerName,
			previousAssignmentType, &newAssignmentType,
			previousAssignedUserID, &activeShift.DriverID,
			previousAssignedUserName, &driverName,
			previousAssignedShiftID, &activeShift.ID)
		if err != nil {
			log.Printf("   ⚠️  WARNING: Failed to log reassignment history: %v", err)
		} else {
			log.Printf("   ✅ Reassignment history logged successfully")
		}
	}

	// Update shift total_bins count
	_, err = tx.Exec(`
		UPDATE shifts
		SET total_bins = total_bins + $1, updated_at = $2
		WHERE id = $3
	`, binsAdded, now, activeShift.ID)
	if err != nil {
		return fmt.Errorf("failed to update shift: %w", err)
	}

	// NOTE: Manual re-optimization removed - Mapbox handles optimization better
	// When move requests are added to active shifts, the route will be re-optimized:
	// 1. Automatically when driver starts shift (optimizeRouteWithMapbox)
	// 2. When driver skips tasks (ReoptimizeActiveShift)
	// 3. When manager edits shift (UpdateShift → ReoptimizeActiveShift)
	// 4. Manually via dashboard re-optimize button
	// The old greedy algorithm here was inferior to Mapbox Optimization v2 API

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 5. Get updated shift and bins for broadcast
	var updatedShift models.Shift
	db.Get(&updatedShift, `SELECT * FROM shifts WHERE id = $1`, activeShift.ID)

	updatedBins, err := getShiftTasksWithDetails(db, activeShift.ID)
	if err != nil {
		log.Printf("⚠️  Failed to fetch updated bins: %v", err)
		updatedBins = []models.ShiftBinWithDetails{}
	}

	// 6. Send WebSocket update to driver
	log.Printf("📡 Broadcasting urgent move update to driver %s", activeShift.DriverID)
	urgentMoveData := map[string]interface{}{
		"shift": map[string]interface{}{
			"id":             updatedShift.ID,
			"status":         updatedShift.Status,
			"total_bins":     updatedShift.TotalBins,
			"completed_bins": updatedShift.CompletedBins,
			"bins":           updatedBins,
		},
		"urgent_bin": map[string]interface{}{
			"bin_number":     bin.BinNumber,
			"current_street": bin.CurrentStreet,
			"city":           bin.City,
			"zip":            bin.Zip,
		},
		"message": fmt.Sprintf("Urgent: Bin #%d added as your next stop", bin.BinNumber),
	}
	wsHub.BroadcastToUser(activeShift.DriverID, map[string]interface{}{
		"type": "urgent_move_inserted",
		"data": urgentMoveData,
	})

	// Publish via Centrifugo to driver:events and shift:updates
	if centrifugoClient != nil {
		if pubErr := centrifugoClient.PublishDriverEvent(context.Background(), activeShift.DriverID, "urgent_move_inserted", urgentMoveData); pubErr != nil {
			log.Printf("⚠️  Failed to publish urgent_move_inserted to driver:events: %v", pubErr)
		}
		if pubErr := centrifugoClient.PublishShiftUpdate(context.Background(), activeShift.ID, map[string]interface{}{
			"type": "urgent_move_inserted",
			"data": urgentMoveData,
		}); pubErr != nil {
			log.Printf("⚠️  Failed to publish urgent_move_inserted to shift:updates: %v", pubErr)
		}
	}

	// 6b. Send move_request_assigned WebSocket notification (for mobile app)
	log.Printf("📡 Broadcasting move_request_assigned to driver %s", activeShift.DriverID)

	// Fetch the updated move request with bin_number from database
	var moveRequestWithBin struct {
		models.BinMoveRequest
		BinNumber int `db:"bin_number" json:"bin_number"`
	}
	err = db.Get(&moveRequestWithBin, `
		SELECT mr.*, b.bin_number
		FROM bin_move_requests mr
		JOIN bins b ON mr.bin_id = b.id
		WHERE mr.id = $1
	`, moveRequest.ID)
	if err == nil {
		moveRequestAssignedData := map[string]interface{}{
			"move_request": moveRequestWithBin,
			"updated_route": map[string]interface{}{
				"shift_id": activeShift.ID,
				"bins":     updatedBins,
			},
		}
		wsHub.BroadcastToUser(activeShift.DriverID, map[string]interface{}{
			"type": "move_request_assigned",
			"data": moveRequestAssignedData,
		})
		log.Printf("✅ move_request_assigned notification sent")

		// Publish via Centrifugo shift channel
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(context.Background(), activeShift.ID, map[string]interface{}{
				"type": "move_request_assigned",
				"data": moveRequestAssignedData,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish move_request_assigned to Centrifugo: %v", pubErr)
			}
		}
	} else {
		log.Printf("⚠️  Failed to fetch updated move request for WebSocket: %v", err)
	}

	// 7. Send push notification to driver (preference-aware)
	moveExtra := map[string]string{"bin_number": fmt.Sprintf("%d", bin.BinNumber)}
	moveTitle, moveBody := services.ShiftNotificationText("move_request_assigned", moveExtra)
	_, moveNotifIDs := services.CreateNotificationForUsers(db, []string{activeShift.DriverID}, "move_request_assigned", moveTitle, moveBody, map[string]string{"shift_id": activeShift.ID, "bin_number": fmt.Sprintf("%d", bin.BinNumber)})
	if len(moveNotifIDs) > 0 && fcmService != nil {
		var fcmToken models.FCMToken
		tokenErr := db.Get(&fcmToken, `
			SELECT * FROM fcm_tokens
			WHERE user_id = $1
			ORDER BY updated_at DESC
			LIMIT 1
		`, activeShift.DriverID)

		if tokenErr == nil {
			err := fcmService.SendShiftUpdateNotification(
				fcmToken.Token,
				activeShift.ID,
				"move_request_assigned",
				moveExtra,
			)
			if err != nil {
				log.Printf("⚠️  Failed to send FCM notification: %v", err)
			} else {
				log.Printf("✅ Push notification sent successfully")
			}
		}
	}

	log.Printf("✅ Urgent move handled successfully")
	return nil
}

// GetBinMoveRequest returns a single move request by ID
// GET /api/manager/bins/move-requests/:id
func GetBinMoveRequest(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		log.Printf("🔍 [GET_MOVE_REQUEST] Fetching move request ID: %s", id)

		if id == "" {
			log.Printf("❌ [GET_MOVE_REQUEST] Missing move request ID")
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch move request
		log.Printf("   [STEP 1] Querying bin_move_requests table...")
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT * FROM bin_move_requests WHERE id = $1
		`, id)
		if err != nil {
			if err == sql.ErrNoRows {
				log.Printf("❌ [GET_MOVE_REQUEST] Move request not found in database: %s", id)
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("❌ [GET_MOVE_REQUEST] Database error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}
		log.Printf("✅ [STEP 1] Move request found - BinID: %s, Status: %s, MoveType: %s",
			moveRequest.BinID, moveRequest.Status, moveRequest.MoveType)

		// Build response
		log.Printf("   [STEP 2] Building response from move request...")
		response := moveRequest.ToBinMoveRequestResponse()
		log.Printf("✅ [STEP 2] Response built successfully")

		// Override urgency with smart calculation (resolved for completed/cancelled)
		response.Urgency = calculateUrgency(moveRequest.Status, moveRequest.ScheduledDate)

		// Fetch associated bin details
		log.Printf("   [STEP 3] Fetching bin details for BinID: %s", moveRequest.BinID)
		var bin models.Bin
		err = db.Get(&bin, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
			FROM bins WHERE id = $1
		`, moveRequest.BinID)
		if err == nil {
			log.Printf("✅ [STEP 3] Bin found - Number: %d, Address: %s, %s %s",
				bin.BinNumber, bin.CurrentStreet, bin.City, bin.Zip)
			binResp := bin.ToBinResponse()
			response.Bin = &binResp
			// Flatten bin fields for easy table display
			response.BinNumber = bin.BinNumber
			response.CurrentStreet = bin.CurrentStreet
			response.City = bin.City
			response.Zip = bin.Zip
		} else {
			log.Printf("⚠️  [STEP 3] Could not fetch bin: %v", err)
		}

		// Parse new address into separate fields if available
		log.Printf("   [STEP 4] Parsing new address...")
		if moveRequest.NewAddress != nil {
			log.Printf("   New address: %s", *moveRequest.NewAddress)
			// Split "street, city zip" format
			parts := strings.Split(*moveRequest.NewAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					response.NewStreet = &street
					response.NewCity = &city
					response.NewZip = &zip
					log.Printf("✅ [STEP 4] Address parsed - Street: %s, City: %s, Zip: %s", street, city, zip)
				}
			}
		} else {
			log.Printf("   No new address to parse")
		}

		// Fetch assigned driver name if assigned to a shift
		log.Printf("   [STEP 5] Checking for assigned driver...")
		if moveRequest.AssignedShiftID != nil {
			log.Printf("   AssignedShiftID: %s, querying driver name...", *moveRequest.AssignedShiftID)
			var driverName string
			err = db.Get(&driverName, `
				SELECT u.name FROM shifts s
				JOIN users u ON s.driver_id = u.id
				WHERE s.id = $1
			`, *moveRequest.AssignedShiftID)
			if err == nil {
				log.Printf("✅ [STEP 5] Driver name found: %s", driverName)
				response.AssignedDriverName = &driverName
			} else {
				log.Printf("⚠️  [STEP 5] Could not fetch driver name: %v", err)
			}
		} else {
			log.Printf("   No assigned shift")
		}

		log.Printf("🎉 [GET_MOVE_REQUEST] Successfully prepared response for move request %s", id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// GetBinMoveRequests returns all bin move requests with optional filtering
// GET /api/manager/bins/move-requests?status=pending&urgency=urgent
func GetBinMoveRequests(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/manager/bins/move-requests")

		// Parse query params
		status := r.URL.Query().Get("status")
		urgency := r.URL.Query().Get("urgency")
		assigned := r.URL.Query().Get("assigned")

		log.Printf("   Query params: status=%s, urgency=%s, assigned=%s", status, urgency, assigned)

		// Build query
		query := `
			SELECT bmr.id, bmr.bin_id, bmr.scheduled_date, bmr.urgency, bmr.requested_by,
			       bmr.status, bmr.original_latitude, bmr.original_longitude, bmr.original_address,
			       bmr.new_latitude, bmr.new_longitude, bmr.new_address,
			       bmr.move_type, bmr.disposal_action, bmr.reason, bmr.notes,
		       bmr.assignment_type, bmr.assigned_shift_id, bmr.assigned_user_id,
		       bmr.completed_at, bmr.created_at, bmr.updated_at
			FROM bin_move_requests bmr
			WHERE 1=1
		`
		args := []interface{}{}
		argCount := 1

		if status != "" {
			query += fmt.Sprintf(" AND bmr.status = $%d", argCount)
			args = append(args, status)
			argCount++
		}

		if urgency != "" {
			query += fmt.Sprintf(" AND bmr.urgency = $%d", argCount)
			args = append(args, urgency)
			argCount++
		}

		query += " ORDER BY bmr.scheduled_date ASC, bmr.created_at DESC"

		// Fetch move requests
		var moveRequests []models.BinMoveRequest
		err := db.Select(&moveRequests, query, args...)
		if err != nil {
			log.Printf("Error fetching move requests: %v", err)
			http.Error(w, "Failed to fetch move requests", http.StatusInternalServerError)
			return
		}

		// Fetch associated bins, requester names, and driver names
		responses := make([]models.BinMoveRequestResponse, len(moveRequests))
		for i, mr := range moveRequests {
			responses[i] = mr.ToBinMoveRequestResponse()

			// Override urgency with smart calculation (resolved for completed/cancelled)
			responses[i].Urgency = calculateUrgency(mr.Status, mr.ScheduledDate)

			// Fetch bin details
			var bin models.Bin
			err := db.Get(&bin, `
				SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
				FROM bins
				WHERE id = $1
			`, mr.BinID)
			if err == nil {
				binResp := bin.ToBinResponse()
				responses[i].Bin = &binResp
				// Flatten bin fields for easy table display
				responses[i].BinNumber = bin.BinNumber
				responses[i].CurrentStreet = bin.CurrentStreet
				responses[i].City = bin.City
				responses[i].Zip = bin.Zip
			}

			// Fetch requester name
			var requesterName string
			err = db.Get(&requesterName, `
				SELECT name FROM users WHERE id = $1
			`, mr.RequestedBy)
			if err == nil {
				responses[i].RequestedByName = &requesterName
			}

			// Parse original address into separate fields
			parts := strings.Split(mr.OriginalAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					responses[i].OriginalStreet = &street
					responses[i].OriginalCity = &city
					responses[i].OriginalZip = &zip
				}
			}

			// Parse new address into separate fields if available
			if mr.NewAddress != nil {
				// Split "street, city zip" format
				parts := strings.Split(*mr.NewAddress, ", ")
				if len(parts) >= 2 {
					street := parts[0]
					cityZip := strings.TrimSpace(parts[1])
					cityZipParts := strings.Split(cityZip, " ")
					if len(cityZipParts) >= 2 {
						city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
						zip := cityZipParts[len(cityZipParts)-1]
						responses[i].NewStreet = &street
						responses[i].NewCity = &city
						responses[i].NewZip = &zip
					}
				}
			}

			// Fetch assigned driver name if assigned to a shift
			if mr.AssignedShiftID != nil {
				var driverName string
				err := db.Get(&driverName, `
					SELECT u.name FROM shifts s
					JOIN users u ON s.driver_id = u.id
					WHERE s.id = $1
				`, *mr.AssignedShiftID)
				if err == nil {
					responses[i].AssignedDriverName = &driverName
					responses[i].DriverName = &driverName // Set unified field
				}
			}

			// Fetch assigned user name if manually assigned
			if mr.AssignedUserID != nil {
				var userName string
				err := db.Get(&userName, `
					SELECT name FROM users WHERE id = $1
				`, *mr.AssignedUserID)
				if err == nil {
					responses[i].AssignedUserName = &userName
					responses[i].DriverName = &userName // Set unified field
				}
			}
		}

		log.Printf("📤 RESPONSE: 200 - Returning %d move requests", len(responses))
		for i, resp := range responses {
			driverInfo := "Unassigned"
			if resp.DriverName != nil {
				driverInfo = *resp.DriverName
			}
			log.Printf("   %d. Move Request %s - Bin #%d - Status: %s - Assigned to: %s",
				i+1, resp.ID[:8], resp.BinNumber, resp.Status, driverInfo)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// GetBinMoveRequestsByBinID returns all move requests for a specific bin
// GET /api/bins/:id/move-requests
func GetBinMoveRequestsByBinID(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "id")
		if binID == "" {
			http.Error(w, "Missing bin ID", http.StatusBadRequest)
			return
		}

		// Parse optional status filter
		status := r.URL.Query().Get("status")

		// Build query
		query := `
			SELECT bmr.id, bmr.bin_id, bmr.scheduled_date, bmr.urgency, bmr.requested_by,
			       bmr.status, bmr.original_latitude, bmr.original_longitude, bmr.original_address,
			       bmr.new_latitude, bmr.new_longitude, bmr.new_address,
			       bmr.move_type, bmr.disposal_action, bmr.reason, bmr.notes,
			       bmr.assigned_shift_id, bmr.completed_at, bmr.created_at, bmr.updated_at,
			       bmr.assignment_type, bmr.assigned_user_id
			FROM bin_move_requests bmr
			WHERE bmr.bin_id = $1
		`
		args := []interface{}{binID}
		argCount := 2

		if status != "" {
			query += fmt.Sprintf(" AND bmr.status = $%d", argCount)
			args = append(args, status)
			argCount++
		}

		query += " ORDER BY bmr.created_at DESC"

		// Fetch move requests
		var moveRequests []models.BinMoveRequest
		err := db.Select(&moveRequests, query, args...)
		if err != nil {
			log.Printf("Error fetching move requests for bin %s: %v", binID, err)
			http.Error(w, "Failed to fetch move requests", http.StatusInternalServerError)
			return
		}

		// Fetch associated data (requester name, driver name, etc.)
		responses := make([]models.BinMoveRequestResponse, len(moveRequests))
		for i, mr := range moveRequests {
			responses[i] = mr.ToBinMoveRequestResponse()

			// Override urgency with smart calculation (resolved for completed/cancelled)
			responses[i].Urgency = calculateUrgency(mr.Status, mr.ScheduledDate)

			// Fetch bin details
			var bin models.Bin
			err := db.Get(&bin, `
				SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
				FROM bins
				WHERE id = $1
			`, mr.BinID)
			if err == nil {
				binResp := bin.ToBinResponse()
				responses[i].Bin = &binResp
				// Flatten bin fields
				responses[i].BinNumber = bin.BinNumber
				responses[i].CurrentStreet = bin.CurrentStreet
				responses[i].City = bin.City
				responses[i].Zip = bin.Zip
			}

			// Fetch requester name
			var requesterName string
			err = db.Get(&requesterName, `
				SELECT name FROM users WHERE id = $1
			`, mr.RequestedBy)
			if err == nil {
				responses[i].RequestedByName = &requesterName
			}

			// Parse original address into separate fields
			parts := strings.Split(mr.OriginalAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					responses[i].OriginalStreet = &street
					responses[i].OriginalCity = &city
					responses[i].OriginalZip = &zip
				}
			}

			// Parse new address into separate fields if available
			if mr.NewAddress != nil {
				parts := strings.Split(*mr.NewAddress, ", ")
				if len(parts) >= 2 {
					street := parts[0]
					cityZip := strings.TrimSpace(parts[1])
					cityZipParts := strings.Split(cityZip, " ")
					if len(cityZipParts) >= 2 {
						city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
						zip := cityZipParts[len(cityZipParts)-1]
						responses[i].NewStreet = &street
						responses[i].NewCity = &city
						responses[i].NewZip = &zip
					}
				}
			}

			// Fetch assigned driver name if assigned to a shift
			if mr.AssignedShiftID != nil {
				var driverName string
				err := db.Get(&driverName, `
					SELECT u.name FROM shifts s
					JOIN users u ON s.driver_id = u.id
					WHERE s.id = $1
				`, *mr.AssignedShiftID)
				if err == nil {
					responses[i].AssignedDriverName = &driverName
					responses[i].DriverName = &driverName // Set unified field
				}
			}

			// Fetch assigned user name if manually assigned
			if mr.AssignedUserID != nil {
				var userName string
				err := db.Get(&userName, `
					SELECT name FROM users WHERE id = $1
				`, *mr.AssignedUserID)
				if err == nil {
					responses[i].AssignedUserName = &userName
					responses[i].DriverName = &userName // Set unified field
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// UpdateBinMoveRequest updates move request details (date, notes, location, assignment, etc.)
// PUT /api/manager/bins/move-requests/:id
func UpdateBinMoveRequest(db *sqlx.DB, redisClient *redis.Client, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Parse request body
		var req struct {
			// Basic fields
			ScheduledDate *int64   `json:"scheduled_date,omitempty"`
			MoveType      *string  `json:"move_type,omitempty"` // "store" or "relocation"
			Reason        *string  `json:"reason,omitempty"`
			Notes         *string  `json:"notes,omitempty"`
			NewStreet     *string  `json:"new_street,omitempty"`
			NewCity       *string  `json:"new_city,omitempty"`
			NewZip        *string  `json:"new_zip,omitempty"`
			NewLatitude   *float64 `json:"new_latitude,omitempty"`
			NewLongitude  *float64 `json:"new_longitude,omitempty"`

			// Assignment fields
			AssignedShiftID       *string `json:"assigned_shift_id,omitempty"`
			AssignedUserID        *string `json:"assigned_user_id,omitempty"`
			AssignmentType        *string `json:"assignment_type,omitempty"` // "shift", "manual", or "" for unassigned

			// Edge case handling
			ClientUpdatedAt              *int64  `json:"client_updated_at,omitempty"`              // For optimistic locking
			ConfirmActiveShiftChange     bool    `json:"confirm_active_shift_change"`              // User confirmed warning
			InProgressAction             *string `json:"in_progress_action,omitempty"`             // "remove_from_route", "insert_after_current", "reoptimize_route"
			InsertAfterWaypoint          *int    `json:"insert_after_waypoint,omitempty"`          // For manual insertion
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Get authenticated user (manager making the update)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		managerUserID := userClaims.UserID

		// Fetch manager's name for notifications
		var managerName string
		err := db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerUserID)
		if err != nil {
			log.Printf("Warning: Could not fetch manager name: %v", err)
			managerName = "A manager" // Fallback
		}

		// Fetch existing move request with shift details
		var moveRequest struct {
			models.BinMoveRequest
			ShiftStatus     *string `db:"shift_status"`
			ShiftDriverName *string `db:"shift_driver_name"`
			TotalWaypoints  *int    `db:"total_waypoints"`
			BinNumber       int     `db:"bin_number"`
		}

		err = db.Get(&moveRequest, `
			SELECT
				mr.*,
				b.bin_number,
				s.status as shift_status,
				u.name as shift_driver_name,
				(SELECT COUNT(*) FROM route_tasks WHERE shift_id = mr.assigned_shift_id) as total_waypoints
			FROM bin_move_requests mr
			LEFT JOIN bins b ON mr.bin_id = b.id
			LEFT JOIN shifts s ON mr.assigned_shift_id = s.id
			LEFT JOIN users u ON s.driver_id = u.id
			WHERE mr.id = $1
		`, id)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// BLOCK: Completed or cancelled moves cannot be edited
		if moveRequest.Status == "completed" || moveRequest.Status == "cancelled" {
			http.Error(w, fmt.Sprintf("❌ Cannot edit %s move request. This move has been finalized and cannot be modified.", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// OPTIMISTIC LOCKING: Check if move was modified by another user
		if req.ClientUpdatedAt != nil && moveRequest.UpdatedAt != *req.ClientUpdatedAt {
			http.Error(w,
				fmt.Sprintf("⚠️ Conflict: This move request was modified by another user while you were editing it. "+
					"The driver may have completed this bin, or another manager may have reassigned it. "+
					"Please refresh and try again with the latest data."),
				http.StatusConflict)
			return
		}

		// CHECK: Is this move on an active shift?
		isOnActiveShift := moveRequest.AssignedShiftID != nil &&
			moveRequest.ShiftStatus != nil &&
			*moveRequest.ShiftStatus == "active"

		// CHECK: Is driver currently at this location?
		isInProgress := moveRequest.Status == "in_progress"

		// VALIDATION: In-progress moves require explicit action
		if isInProgress && (req.AssignedShiftID != nil || req.AssignedUserID != nil) {
			if req.InProgressAction == nil || *req.InProgressAction == "" {
				driverInfo := "Unknown driver"
				if moveRequest.ShiftDriverName != nil {
					driverInfo = *moveRequest.ShiftDriverName
				}
				http.Error(w,
					fmt.Sprintf("⚠️ Driver %s is currently at this location. "+
						"You must specify what should happen to this move by providing 'in_progress_action': "+
						"'remove_from_route', 'insert_after_current', or 'reoptimize_route'.",
						driverInfo),
					http.StatusBadRequest)
				return
			}
		}

		// VALIDATION: Active shift changes require confirmation
		if isOnActiveShift && !isInProgress && !req.ConfirmActiveShiftChange {
			// User is trying to change assignment for a move on an active route
			driverName := "the driver"
			if moveRequest.ShiftDriverName != nil {
				driverName = *moveRequest.ShiftDriverName
			}

			http.Error(w,
				fmt.Sprintf("⚠️ This move is on %s's active route. Changing it will affect their navigation. "+
					"Please confirm by setting 'confirm_active_shift_change' to true.", driverName),
				http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Build dynamic update query
		updates := []string{"updated_at = $1"}
		args := []interface{}{now}
		argCount := 2

		// Update scheduled date and recalculate urgency if date changed
		if req.ScheduledDate != nil {
			updates = append(updates, fmt.Sprintf("scheduled_date = $%d", argCount))
			args = append(args, *req.ScheduledDate)
			argCount++

			// Recalculate urgency
			hoursUntil := float64(*req.ScheduledDate-now) / 3600.0
			var newUrgency string
			if hoursUntil < 24 {
				newUrgency = "urgent"
			} else {
				newUrgency = "scheduled"
			}
			updates = append(updates, fmt.Sprintf("urgency = $%d", argCount))
			args = append(args, newUrgency)
			argCount++
		}


		if req.MoveType != nil {
			updates = append(updates, fmt.Sprintf("move_type = $%d", argCount))
			args = append(args, *req.MoveType)
			argCount++
		}
		if req.Reason != nil {
			updates = append(updates, fmt.Sprintf("reason = $%d", argCount))
			args = append(args, *req.Reason)
			argCount++
		}

		if req.Notes != nil {
			updates = append(updates, fmt.Sprintf("notes = $%d", argCount))
			args = append(args, *req.Notes)
			argCount++
		}

		// Build new address if separate fields provided
		if req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil {
			newAddress := fmt.Sprintf("%s, %s %s", *req.NewStreet, *req.NewCity, *req.NewZip)
			updates = append(updates, fmt.Sprintf("new_address = $%d", argCount))
			args = append(args, newAddress)
			argCount++
		}

		if req.NewLatitude != nil {
			updates = append(updates, fmt.Sprintf("new_latitude = $%d", argCount))
			args = append(args, *req.NewLatitude)
			argCount++
		}

		if req.NewLongitude != nil {
			updates = append(updates, fmt.Sprintf("new_longitude = $%d", argCount))
			args = append(args, *req.NewLongitude)
			argCount++
		}

		// ═══════════════════════════════════════════════════════════════════
		// ASSIGNMENT HANDLING (Shift/User reassignment)
		// ═══════════════════════════════════════════════════════════════════

		// START TRANSACTION for assignment changes
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("Error starting transaction: %v", err)
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Track if assignment changed (for WebSocket notification)
		assignmentChanged := false
		affectedDriverIDs := []string{}

		// HANDLE IN-PROGRESS ACTION (driver is at location)
		if isInProgress && req.InProgressAction != nil {
			log.Printf("[IN-PROGRESS EDIT] Handling action: %s", *req.InProgressAction)

			switch *req.InProgressAction {
			case "remove_from_route":
				// Remove from route_tasks, reset to pending
				if moveRequest.AssignedShiftID != nil {
					_, err = tx.Exec(`DELETE FROM route_tasks WHERE shift_id = $1 AND bin_id = $2`,
						*moveRequest.AssignedShiftID, moveRequest.BinID)
					if err != nil {
						log.Printf("Error removing from route_tasks: %v", err)
						http.Error(w, "Failed to remove from driver's route", http.StatusInternalServerError)
						return
					}

					_, err = tx.Exec(`UPDATE shifts SET total_bins = total_bins - 1, updated_at = $1 WHERE id = $2`,
						now, *moveRequest.AssignedShiftID)
					if err != nil {
						log.Printf("Error updating shift total_bins: %v", err)
					}

					log.Printf("[IN-PROGRESS EDIT] ✅ Removed bin from driver's route")
					assignmentChanged = true
					if moveRequest.AssignedUserID != nil {
						affectedDriverIDs = append(affectedDriverIDs, *moveRequest.AssignedUserID)
					}
				}

				// Clear assignment, return to pending
				updates = append(updates, fmt.Sprintf("assigned_shift_id = NULL, assigned_user_id = NULL, assignment_type = NULL, status = 'pending'"))

			case "insert_after_current":
				// Keep on route, adjust waypoint order
				log.Printf("[IN-PROGRESS EDIT] Inserting after current waypoint")
				// Implementation: Re-order waypoints (complex, may need route optimization logic)
				// For now, just log - full implementation would update waypoint_order in route_tasks

			case "reoptimize_route":
				// Trigger route re-optimization
				log.Printf("[IN-PROGRESS EDIT] Triggering route re-optimization")
				// Implementation: Call route optimization service
				// For now, just log - full implementation would recalculate optimal waypoint order
			}
		}

		// HANDLE ASSIGNMENT CHANGES (for non-in-progress moves)
		if !isInProgress {
			// Remove from old shift if changing
			if moveRequest.AssignedShiftID != nil && req.AssignedShiftID != nil && *req.AssignedShiftID != *moveRequest.AssignedShiftID {
				_, err = tx.Exec(`DELETE FROM route_tasks WHERE shift_id = $1 AND bin_id = $2`,
					*moveRequest.AssignedShiftID, moveRequest.BinID)
				if err == nil {
					_, err = tx.Exec(`UPDATE shifts SET total_bins = total_bins - 1, updated_at = $1 WHERE id = $2`,
						now, *moveRequest.AssignedShiftID)
				}
				log.Printf("[REASSIGNMENT] Removed from old shift: %s", *moveRequest.AssignedShiftID)
				assignmentChanged = true
			}

			// Add assignment fields to update (treat empty strings as NULL)
			if req.AssignedShiftID != nil {
				if *req.AssignedShiftID == "" {
					updates = append(updates, "assigned_shift_id = NULL")
					// Only mark as changed if it was previously set
					if moveRequest.AssignedShiftID != nil {
						assignmentChanged = true
					}
				} else {
					updates = append(updates, fmt.Sprintf("assigned_shift_id = $%d", argCount))
					args = append(args, *req.AssignedShiftID)
					argCount++
					// Only mark as changed if the value is different
					if !stringPtrEqual(moveRequest.AssignedShiftID, req.AssignedShiftID) {
						assignmentChanged = true
					}
				}
			}

			if req.AssignedUserID != nil {
				if *req.AssignedUserID == "" {
					updates = append(updates, "assigned_user_id = NULL")
					// Only mark as changed if it was previously set
					if moveRequest.AssignedUserID != nil {
						assignmentChanged = true
					}
				} else {
					updates = append(updates, fmt.Sprintf("assigned_user_id = $%d", argCount))
					args = append(args, *req.AssignedUserID)
					argCount++
					affectedDriverIDs = append(affectedDriverIDs, *req.AssignedUserID)
					// Only mark as changed if the value is different
					if !stringPtrEqual(moveRequest.AssignedUserID, req.AssignedUserID) {
						assignmentChanged = true
					}
				}
			}

			// Determine final assignment state (after potential updates)
			finalShiftID := moveRequest.AssignedShiftID
			finalUserID := moveRequest.AssignedUserID
			if req.AssignedShiftID != nil {
				if *req.AssignedShiftID == "" {
					finalShiftID = nil
				} else {
					finalShiftID = req.AssignedShiftID
				}
			}
			if req.AssignedUserID != nil {
				if *req.AssignedUserID == "" {
					finalUserID = nil
				} else {
					finalUserID = req.AssignedUserID
				}
			}

			// If both assignments are being cleared, also clear assignment_type
			isUnassigning := (finalShiftID == nil || (finalShiftID != nil && *finalShiftID == "")) &&
				(finalUserID == nil || (finalUserID != nil && *finalUserID == ""))

			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("🔍 [UNASSIGNMENT DETECTION]")
			log.Printf("   isUnassigning: %v", isUnassigning)
			log.Printf("   finalShiftID: %v", finalShiftID)
			log.Printf("   finalUserID: %v", finalUserID)
			log.Printf("   moveRequest.AssignedShiftID: %v", moveRequest.AssignedShiftID)
			log.Printf("   moveRequest.Status: %s", moveRequest.Status)
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

			if isUnassigning {
				// Remove from route_tasks if previously assigned to a shift
				if moveRequest.AssignedShiftID != nil {
					log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
					log.Printf("⭕ [UNASSIGNMENT] Starting shift removal")
					log.Printf("   Move Request ID: %s", id)
					log.Printf("   Old Shift ID: %s", *moveRequest.AssignedShiftID)
					log.Printf("   Bin ID: %s", moveRequest.BinID)

					_, err = tx.Exec(`DELETE FROM route_tasks WHERE shift_id = $1 AND bin_id = $2`,
						*moveRequest.AssignedShiftID, moveRequest.BinID)
					if err == nil {
						log.Printf("   ✅ Removed bin from route_tasks")
						_, err = tx.Exec(`UPDATE shifts SET total_bins = total_bins - 1, updated_at = $1 WHERE id = $2`,
							now, *moveRequest.AssignedShiftID)
						if err == nil {
							log.Printf("   ✅ Updated shift total_bins count")
						} else {
							log.Printf("   ⚠️  Failed to update shift count: %v", err)
						}
					} else {
						log.Printf("   ❌ Failed to remove from route_tasks: %v", err)
					}

					log.Printf("[UNASSIGNMENT] Removed from route_tasks for shift: %s", *moveRequest.AssignedShiftID)
					assignmentChanged = true

					// Track affected driver for WebSocket notification
					log.Printf("   Fetching driver ID for WebSocket notification...")
					var driverID string
					err = db.Get(&driverID, `SELECT driver_id FROM shifts WHERE id = $1`, *moveRequest.AssignedShiftID)
					if err == nil {
						affectedDriverIDs = append(affectedDriverIDs, driverID)
						log.Printf("   ✅ Driver ID found: %s", driverID)
						log.Printf("   Driver will receive WebSocket notification")
					} else {
						log.Printf("   ❌ Failed to fetch driver ID: %v", err)
					}
					log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
				} else {
					log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
					log.Printf("⚠️  [UNASSIGNMENT] No shift assignment to remove")
					log.Printf("   Move request was not assigned to a shift")
					log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
				}

				// Clear assignment_type and set status to pending when unassigning
				updates = append(updates, "assignment_type = NULL, status = 'pending'")
				log.Printf("[UNASSIGNMENT] Clearing assignment_type and setting status to pending")
			} else if req.AssignmentType != nil {
				// Only update assignment_type if provided and not unassigning
				// Treat empty string as NULL
				if *req.AssignmentType == "" {
					updates = append(updates, "assignment_type = NULL")
				} else {
					updates = append(updates, fmt.Sprintf("assignment_type = $%d", argCount))
					args = append(args, *req.AssignmentType)
					argCount++
				}
			}
		}

		// Add ID parameter at the end
		args = append(args, id)

		// Execute update
		query := fmt.Sprintf("UPDATE bin_move_requests SET %s WHERE id = $%d",
			strings.Join(updates, ", "), argCount)

		_, err = tx.Exec(query, args...)
		if err != nil {
			log.Printf("Error updating move request: %v", err)
			http.Error(w, "Failed to update move request", http.StatusInternalServerError)
			return
		}

		// CRITICAL FIX: After updating, check if move request should have correct status
		// This ensures status matches the assignment state and shift status
		var finalShiftID, finalUserID *string
		var shiftStatus *string
		err = tx.QueryRow(`
			SELECT
				mr.assigned_shift_id,
				mr.assigned_user_id,
				s.status as shift_status
			FROM bin_move_requests mr
			LEFT JOIN shifts s ON mr.assigned_shift_id = s.id
			WHERE mr.id = $1
		`, id).Scan(&finalShiftID, &finalUserID, &shiftStatus)
		if err != nil {
			log.Printf("Error checking final assignment status: %v", err)
			http.Error(w, "Failed to verify assignment status", http.StatusInternalServerError)
			return
		}

		// Set status based on assignment and shift state
		// Only update status if it's not already in_progress or completed
		if (finalShiftID != nil || finalUserID != nil) && moveRequest.Status != "in_progress" && moveRequest.Status != "completed" {
			var newStatus string

			// If assigned to an ACTIVE shift, status should be "in_progress"
			// If assigned to a future/scheduled shift, status should be "assigned"
			if finalShiftID != nil && shiftStatus != nil && *shiftStatus == "active" {
				newStatus = "in_progress"
				log.Printf("[UPDATE MOVE] Move request assigned to ACTIVE shift, setting status to 'in_progress'")
			} else {
				newStatus = "assigned"
				log.Printf("[UPDATE MOVE] Move request has assignment (shift: %v, user: %v, shift_status: %v), setting status to 'assigned'",
					finalShiftID, finalUserID, shiftStatus)
			}

			_, err = tx.Exec(`
				UPDATE bin_move_requests
				SET status = $1
				WHERE id = $2
			`, newStatus, id)
			if err != nil {
				log.Printf("Error setting status to %s: %v", newStatus, err)
				http.Error(w, "Failed to update status", http.StatusInternalServerError)
				return
			}
		}

		// ── Cascading Update: Update route_tasks if move request address or type changed ───────
		if centrifugoClient != nil && moveRequest.AssignedShiftID != nil {
			log.Printf("🔄 [UPDATE-MOVE] Checking for cascading updates to route_tasks")

			// Check if address changed (for relocation moves with pickup/dropoff tasks)
			addressChanged := false
			if req.NewStreet != nil || req.NewCity != nil || req.NewZip != nil ||
				req.NewLatitude != nil || req.NewLongitude != nil {
				addressChanged = true
			}

			// Check if move_type changed (could remove dropoff task)
			moveTypeChanged := req.MoveType != nil && *req.MoveType != moveRequest.MoveType

			if addressChanged || moveTypeChanged {
				// Find affected route tasks for this move request
				var affectedTasks []struct {
					ShiftID    string  `db:"shift_id"`
					TaskID     string  `db:"task_id"`
					TaskType   string  `db:"task_type"`
					OldAddress *string `db:"old_address"`
				}

				err = tx.Select(&affectedTasks, `
					SELECT
						rt.shift_id,
						rt.id as task_id,
						rt.task_type,
						rt.address as old_address
					FROM route_tasks rt
					JOIN shifts s ON rt.shift_id = s.id
					WHERE rt.move_request_id = $1
					  AND rt.is_completed = 0
					  AND s.status IN ('active', 'scheduled')
				`, id)

				if err != nil && err != sql.ErrNoRows {
					log.Printf("⚠️  [UPDATE-MOVE] Failed to check route tasks: %v", err)
				} else if len(affectedTasks) > 0 {
					log.Printf("🎯 [UPDATE-MOVE] Found %d route task(s) affected by change", len(affectedTasks))

					// If move_type changed from relocation → store, soft delete dropoff tasks
					if moveTypeChanged && req.MoveType != nil && *req.MoveType == "store" {
						for _, task := range affectedTasks {
							if task.TaskType == "dropoff" {
								_, deleteErr := tx.Exec(`
									UPDATE route_tasks
									SET is_deleted = true,
										deleted_at = $1,
										deleted_by = $2,
										deletion_reason = $3,
										updated_at = $1
									WHERE id = $4 AND is_deleted = false
								`, now, managerUserID, "move_type_changed_to_store", task.TaskID)

								if deleteErr != nil {
									log.Printf("⚠️  [UPDATE-MOVE] Failed to soft delete dropoff task %s: %v", task.TaskID, deleteErr)
								} else {
									log.Printf("✅ [UPDATE-MOVE] Soft deleted dropoff task %s (move_type → store)", task.TaskID)
								}
							}
						}

						// Notify driver that dropoff task was removed
						if moveRequest.AssignedShiftID != nil {
							notifyErr := NotifyDriverOfRouteUpdate(
								db,
								centrifugoClient,
								*moveRequest.AssignedShiftID,
								"move_type_changed",
								map[string]interface{}{
									"move_request_id": id,
									"bin_number":      moveRequest.BinNumber,
									"old_move_type":   moveRequest.MoveType,
									"new_move_type":   *req.MoveType,
									"dropoff_removed": true,
								},
							)

							if notifyErr != nil {
								log.Printf("⚠️  [UPDATE-MOVE] Failed to notify driver: %v", notifyErr)
							} else {
								log.Printf("✅ [UPDATE-MOVE] Notified driver about move type change")
							}
						}

					// If move_type changed from store → relocation, add dropoff task
					} else if moveTypeChanged && req.MoveType != nil && *req.MoveType == "relocation" && moveRequest.MoveType == "store" {
						// Find the pickup task to get shift_id and sequence info
						var pickupTask *struct {
							TaskID        string
							ShiftID       string
							SequenceOrder int
						}

						for _, task := range affectedTasks {
							if task.TaskType == "pickup" {
								pickupTask = &struct {
									TaskID        string
									ShiftID       string
									SequenceOrder int
								}{
									TaskID:        task.TaskID,
									ShiftID:       task.ShiftID,
									SequenceOrder: 0, // Will query for actual sequence
								}
								break
							}
						}

						if pickupTask != nil && req.NewStreet != nil && req.NewLatitude != nil && req.NewLongitude != nil {
							// Get actual sequence order of pickup task
							err := tx.QueryRow(`SELECT sequence_order FROM route_tasks WHERE id = $1`, pickupTask.TaskID).Scan(&pickupTask.SequenceOrder)
							if err != nil {
								log.Printf("⚠️  [UPDATE-MOVE] Failed to get pickup task sequence: %v", err)
							} else {
								// Create new dropoff task right after pickup
								newDropoffID := uuid.New().String()
								_, insertErr := tx.Exec(`
									INSERT INTO route_tasks (
										id, shift_id, move_request_id, task_type,
										sequence_order, address, destination_address,
										destination_latitude, destination_longitude,
										is_completed, created_at, updated_at
									) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, $10, $10)
								`, newDropoffID, pickupTask.ShiftID, id, "dropoff",
									pickupTask.SequenceOrder+1, *req.NewStreet, *req.NewStreet,
									*req.NewLatitude, *req.NewLongitude, time.Now().Unix())

								if insertErr != nil {
									log.Printf("⚠️  [UPDATE-MOVE] Failed to create dropoff task: %v", insertErr)
								} else {
									log.Printf("✅ [UPDATE-MOVE] Created dropoff task %s (move_type → relocation)", newDropoffID)

									// Notify driver that dropoff task was added
									if moveRequest.AssignedShiftID != nil {
										notifyErr := NotifyDriverOfRouteUpdate(
											db,
											centrifugoClient,
											*moveRequest.AssignedShiftID,
											"move_type_changed",
											map[string]interface{}{
												"move_request_id": id,
												"bin_number":      moveRequest.BinNumber,
												"old_move_type":   moveRequest.MoveType,
												"new_move_type":   *req.MoveType,
												"dropoff_added":   true,
												"new_address":     *req.NewStreet,
											},
										)

										if notifyErr != nil {
											log.Printf("⚠️  [UPDATE-MOVE] Failed to notify driver: %v", notifyErr)
										} else {
											log.Printf("✅ [UPDATE-MOVE] Notified driver about move type change")
										}
									}
								}
							}
						}

					} else if addressChanged {
						// Update address/coordinates for affected tasks
						for _, task := range affectedTasks {
							// Update based on task type
							if task.TaskType == "dropoff" {
								// Update dropoff destination
								if req.NewStreet != nil && req.NewLatitude != nil && req.NewLongitude != nil {
									_, updateErr := tx.Exec(`
										UPDATE route_tasks
										SET destination_address = $1,
											destination_latitude = $2,
											destination_longitude = $3,
											updated_at = $4
										WHERE id = $5
									`, *req.NewStreet, *req.NewLatitude, *req.NewLongitude, time.Now().Unix(), task.TaskID)

									if updateErr != nil {
										log.Printf("⚠️  [UPDATE-MOVE] Failed to update dropoff task %s: %v", task.TaskID, updateErr)
									} else {
										oldAddr := "unknown"
										if task.OldAddress != nil {
											oldAddr = *task.OldAddress
										}
										log.Printf("✅ [UPDATE-MOVE] Updated dropoff task %s: %s → %s", task.TaskID, oldAddr, *req.NewStreet)
									}
								}
							}
						}

						// Notify driver about address change
						if moveRequest.AssignedShiftID != nil {
							notifyErr := NotifyDriverOfRouteUpdate(
								db,
								centrifugoClient,
								*moveRequest.AssignedShiftID,
								"move_request_address_changed",
								map[string]interface{}{
									"move_request_id": id,
									"bin_number":      moveRequest.BinNumber,
									"old_address":     moveRequest.NewAddress,
									"new_address":     *req.NewStreet,
								},
							)

							if notifyErr != nil {
								log.Printf("⚠️  [UPDATE-MOVE] Failed to notify driver: %v", notifyErr)
							} else {
								log.Printf("✅ [UPDATE-MOVE] Notified driver about address change")
							}
						}
					}
				} else {
					log.Printf("ℹ️  [UPDATE-MOVE] No active route tasks found for this move request")
				}
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			http.Error(w, "Failed to commit changes", http.StatusInternalServerError)
			return
		}

		log.Printf("[UPDATE MOVE] ✅ Successfully updated move request: %s", id)

	// Re-optimize shift if move request was on an active shift and address/move_type changed
	if moveRequest.AssignedShiftID != nil {
		addressChanged := req.NewStreet != nil || req.NewCity != nil || req.NewZip != nil ||
			req.NewLatitude != nil || req.NewLongitude != nil
		moveTypeChanged := req.MoveType != nil && *req.MoveType != moveRequest.MoveType

		if addressChanged || moveTypeChanged {
			// Check if shift is active
			var shiftStatus string
			err := db.Get(&shiftStatus, `SELECT status FROM shifts WHERE id = $1`, *moveRequest.AssignedShiftID)
			if err == nil && shiftStatus == "active" {
				log.Printf("🔄 [UPDATE-MOVE] Triggering re-optimization for shift %s (manager-initiated change)", *moveRequest.AssignedShiftID)
				if reoptErr := ReoptimizeActiveShift(db, redisClient, *moveRequest.AssignedShiftID, centrifugoClient, true); reoptErr != nil {
					log.Printf("⚠️  [UPDATE-MOVE] Failed to re-optimize shift: %v", reoptErr)
					// Don't fail the entire request if re-optimization fails
				} else {
					log.Printf("✅ [UPDATE-MOVE] Successfully re-optimized shift %s", *moveRequest.AssignedShiftID)
				}
			}
		}
	}

// Fetch updated move request to get new state for history logging
		var updatedMove struct {
			models.BinMoveRequest
			AssignedUserName   *string `db:"assigned_user_name"`
			AssignedDriverName *string `db:"assigned_driver_name"`
		}
		err = db.Get(&updatedMove, `
			SELECT
				mr.*,
				assigned_user.name AS assigned_user_name,
				shift_driver.name AS assigned_driver_name
			FROM bin_move_requests mr
			LEFT JOIN users assigned_user ON mr.assigned_user_id = assigned_user.id
			LEFT JOIN shifts s ON mr.assigned_shift_id = s.id
			LEFT JOIN users shift_driver ON s.driver_id = shift_driver.id
			WHERE mr.id = $1
		`, id)
		if err != nil {
			log.Printf("Error fetching updated move request: %v", err)
			http.Error(w, "Failed to fetch updated move request", http.StatusInternalServerError)
			return
		}

		// Log history: determine specific type of change
		if assignmentChanged {
			// Determine what kind of assignment change occurred
			oldHadAssignment := moveRequest.AssignedShiftID != nil || moveRequest.AssignedUserID != nil
			newHasAssignment := updatedMove.AssignedShiftID != nil || updatedMove.AssignedUserID != nil

			if !oldHadAssignment && newHasAssignment {
				// ASSIGNED: Was unassigned, now assigned
				assignmentType := ""
				if updatedMove.AssignmentType != nil {
					assignmentType = *updatedMove.AssignmentType
				}

				// Determine assigned user name (could be from manual assignment or shift driver)
				var assignedUserName *string
				if updatedMove.AssignedUserName != nil {
					assignedUserName = updatedMove.AssignedUserName
				} else if updatedMove.AssignedDriverName != nil {
					assignedUserName = updatedMove.AssignedDriverName
				}

				err = helpers.LogMoveRequestAssigned(
					db, id, managerUserID, managerName,
					assignmentType,
					updatedMove.AssignedUserID,
					assignedUserName,
					updatedMove.AssignedShiftID,
				)
				if err != nil {
					log.Printf("Warning: Failed to log move request assignment: %v", err)
				}

			} else if oldHadAssignment && !newHasAssignment {
				// UNASSIGNED: Was assigned, now unassigned
				// Determine old assigned user name
				var oldAssignedUserName *string
				if moveRequest.AssignedUserID != nil {
					// Fetch the old assigned user's name
					var userName string
					nameErr := db.Get(&userName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID)
					if nameErr == nil {
						oldAssignedUserName = &userName
					}
				} else if moveRequest.AssignedShiftID != nil {
					// Fetch the old shift driver's name
					var driverName string
					nameErr := db.Get(&driverName, `SELECT u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`, *moveRequest.AssignedShiftID)
					if nameErr == nil {
						oldAssignedUserName = &driverName
					}
				}

				err = helpers.LogMoveRequestUnassigned(
					db, id, managerUserID, managerName,
					moveRequest.AssignmentType,
					moveRequest.AssignedUserID,
					oldAssignedUserName,
					moveRequest.AssignedShiftID,
				)
				if err != nil {
					log.Printf("Warning: Failed to log move request unassignment: %v", err)
				}

			} else if oldHadAssignment && newHasAssignment {
				// REASSIGNED: Assignment changed from one to another
				// Determine old assigned user name
				var oldAssignedUserName *string
				if moveRequest.AssignedUserID != nil {
					var userName string
					nameErr := db.Get(&userName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID)
					if nameErr == nil {
						oldAssignedUserName = &userName
					}
				} else if moveRequest.AssignedShiftID != nil {
					var driverName string
					nameErr := db.Get(&driverName, `SELECT u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`, *moveRequest.AssignedShiftID)
					if nameErr == nil {
						oldAssignedUserName = &driverName
					}
				}

				// Determine new assigned user name
				var newAssignedUserName *string
				if updatedMove.AssignedUserName != nil {
					newAssignedUserName = updatedMove.AssignedUserName
				} else if updatedMove.AssignedDriverName != nil {
					newAssignedUserName = updatedMove.AssignedDriverName
				}

				err = helpers.LogMoveRequestReassigned(
					db, id, managerUserID, managerName,
					moveRequest.AssignmentType,
					updatedMove.AssignmentType,
					moveRequest.AssignedUserID,
					updatedMove.AssignedUserID,
					oldAssignedUserName,
					newAssignedUserName,
					moveRequest.AssignedShiftID,
					updatedMove.AssignedShiftID,
				)
				if err != nil {
					log.Printf("Warning: Failed to log move request reassignment: %v", err)
				}
			}
		} else if req.ScheduledDate != nil || req.MoveType != nil || req.Reason != nil || req.Notes != nil ||
			(req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil) ||
			req.NewLatitude != nil || req.NewLongitude != nil {
			// Only log "updated" if move detail fields (not just assignment) were actually provided

			// Build metadata JSON with old/new value comparisons
			type ChangeDetail struct {
				Field         string  `json:"field"`
				Label         string  `json:"label"`
				Old           *string `json:"old,omitempty"`
				New           *string `json:"new,omitempty"`
				OldFormatted  *string `json:"old_formatted,omitempty"`
				NewFormatted  *string `json:"new_formatted,omitempty"`
				OldTimestamp  *int64  `json:"old_timestamp,omitempty"`
				NewTimestamp  *int64  `json:"new_timestamp,omitempty"`
			}

			type MetadataStruct struct {
				Changes []ChangeDetail `json:"changes"`
			}

			var changes []ChangeDetail

			// Compare scheduled_date
			if req.ScheduledDate != nil && *req.ScheduledDate != moveRequest.ScheduledDate {
				oldDate := time.Unix(moveRequest.ScheduledDate, 0).Format("Jan 2, 2006")
				newDate := time.Unix(*req.ScheduledDate, 0).Format("Jan 2, 2006")
				changes = append(changes, ChangeDetail{
					Field:        "scheduled_date",
					Label:        "Scheduled Date",
					OldFormatted: &oldDate,
					NewFormatted: &newDate,
					OldTimestamp: &moveRequest.ScheduledDate,
					NewTimestamp: req.ScheduledDate,
				})
			}

			// Compare move_type
			if req.MoveType != nil && *req.MoveType != moveRequest.MoveType {
				old := moveRequest.MoveType
				new := *req.MoveType
				changes = append(changes, ChangeDetail{
					Field: "move_type",
					Label: "Move Type",
					Old:   &old,
					New:   &new,
				})
			}

			// Compare reason
			if req.Reason != nil {
				oldReason := ""
				if moveRequest.Reason != nil {
					oldReason = *moveRequest.Reason
				}
				newReason := *req.Reason
				if oldReason != newReason {
					oldReasonPtr := &oldReason
					if oldReason == "" {
						oldReasonPtr = nil
					}
					changes = append(changes, ChangeDetail{
						Field: "reason",
						Label: "Reason",
						Old:   oldReasonPtr,
						New:   &newReason,
					})
				}
			}

			// Compare notes
			if req.Notes != nil {
				oldNotes := ""
				if moveRequest.Notes != nil {
					oldNotes = *moveRequest.Notes
				}
				newNotes := *req.Notes
				if oldNotes != newNotes {
					oldNotesPtr := &oldNotes
					if oldNotes == "" {
						oldNotesPtr = nil
					}
					changes = append(changes, ChangeDetail{
						Field: "notes",
						Label: "Notes",
						Old:   oldNotesPtr,
						New:   &newNotes,
					})
				}
			}

			// NOTE: We don't compare address strings because the format can vary
			// (e.g., "Street, City 12345" vs "Street, City, 12345"). This causes
			// false positives. The coordinates comparison below is more reliable.

			// Compare coordinates (if both lat/lng provided)
			// We use coordinates for reliable comparison, but display formatted addresses
			if req.NewLatitude != nil && req.NewLongitude != nil {
				oldLat := moveRequest.NewLatitude
				oldLng := moveRequest.NewLongitude
				newLat := req.NewLatitude
				newLng := req.NewLongitude

				latChanged := (oldLat == nil && newLat != nil) || (oldLat != nil && newLat == nil) ||
							  (oldLat != nil && newLat != nil && *oldLat != *newLat)
				lngChanged := (oldLng == nil && newLng != nil) || (oldLng != nil && newLng == nil) ||
							  (oldLng != nil && newLng != nil && *oldLng != *newLng)

				if latChanged || lngChanged {
					// Get old address (stored as single formatted string in database)
					var oldAddressPtr *string
					if moveRequest.NewAddress != nil && *moveRequest.NewAddress != "" {
						oldAddressPtr = moveRequest.NewAddress
					}

					// Format new address from request (separate fields)
					var newAddressPtr *string
					if req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil {
						newAddr := fmt.Sprintf("%s, %s %s", *req.NewStreet, *req.NewCity, *req.NewZip)
						newAddressPtr = &newAddr
					}

					changes = append(changes, ChangeDetail{
						Field: "new_location",
						Label: "New Location",
						Old:   oldAddressPtr,
						New:   newAddressPtr,
					})
				}
			}

			// Build metadata JSON
			var metadataJSON *string
			if len(changes) > 0 {
				metadata := MetadataStruct{Changes: changes}
				metadataBytes, err := json.Marshal(metadata)
				if err == nil {
					metadataStr := string(metadataBytes)
					metadataJSON = &metadataStr
				} else {
					log.Printf("Warning: Failed to marshal metadata JSON: %v", err)
				}
			}

			// Only log "updated" history entry if there are actual changes
			if len(changes) > 0 {
				notes := "Updated move details"
				err = helpers.LogMoveRequestUpdated(db, id, managerUserID, managerName, &notes, metadataJSON)
				if err != nil {
					log.Printf("Warning: Failed to log move request update: %v", err)
				}
			}
		}

		// Check if this move request is on an active shift (even if assignment didn't change)
		// This ensures drivers are notified when move details change (scheduled_date, move_type, etc.)
		if updatedMove.AssignedShiftID != nil && wsHub != nil && !assignmentChanged {
			var shift struct {
				DriverID string `db:"driver_id"`
				Status   string `db:"status"`
			}
			err = db.Get(&shift, `SELECT driver_id, status FROM shifts WHERE id = $1`, *updatedMove.AssignedShiftID)
			if err == nil && shift.Status == "active" {
				// Add driver to affected list if not already present
				driverAlreadyInList := false
				for _, dID := range affectedDriverIDs {
					if dID == shift.DriverID {
						driverAlreadyInList = true
						break
					}
				}
				if !driverAlreadyInList {
					affectedDriverIDs = append(affectedDriverIDs, shift.DriverID)
					log.Printf("[UPDATE MOVE] Added driver %s to notification list (active shift field update)", shift.DriverID)
				}
			}
		}

		// Send WebSocket notification if assignment changed OR if there are affected drivers
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📡 [WEBSOCKET NOTIFICATION CHECK]")
		log.Printf("   assignmentChanged: %v", assignmentChanged)
		log.Printf("   affectedDriverIDs: %v (count: %d)", affectedDriverIDs, len(affectedDriverIDs))
		log.Printf("   wsHub available: %v", wsHub != nil)
		log.Printf("   Should send notifications: %v", (assignmentChanged || len(affectedDriverIDs) > 0) && wsHub != nil)
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Always publish move_request_updated to Centrifugo so all manager views refresh
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "move_request_updated", map[string]interface{}{
				"move_request_id": id,
				"status":          updatedMove.Status,
				"bin_id":          updatedMove.BinID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish move_request_updated to Centrifugo: %v", pubErr)
			}
		}

		// Notify affected drivers via Centrifugo
		if (assignmentChanged || len(affectedDriverIDs) > 0) && centrifugoClient != nil {
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("📡 [CENTRIFUGO] Sending route update notifications...")

			// Notify affected drivers
			if len(affectedDriverIDs) > 0 {
				log.Printf("   Notifying %d affected driver(s):", len(affectedDriverIDs))

				// Fetch bin number for the notification
				var binNumber int
				err := db.Get(&binNumber, `SELECT bin_number FROM bins WHERE id = $1`, updatedMove.BinID)
				if err != nil {
					log.Printf("⚠️  Warning: Could not fetch bin number: %v", err)
					binNumber = 0 // Fallback
				}

				// Determine action type (removed/added/updated)
				isUnassigning := moveRequest.AssignedShiftID != nil && updatedMove.AssignedShiftID == nil
				actionType := "updated"
				if isUnassigning {
					actionType = "removed"
				} else if moveRequest.AssignedShiftID == nil && updatedMove.AssignedShiftID != nil {
					actionType = "added"
				}

				// Get shift IDs for affected drivers
				// Map: driverID -> shiftID
				driverShifts := make(map[string]string)
				for _, driverID := range affectedDriverIDs {
					var shiftID string
					err := db.Get(&shiftID, `
						SELECT id FROM shifts
						WHERE driver_id = $1 AND status IN ('active', 'ready')
						ORDER BY scheduled_start DESC
						LIMIT 1
					`, driverID)
					if err == nil {
						driverShifts[driverID] = shiftID
					} else {
						log.Printf("⚠️  Warning: Could not find active shift for driver %s: %v", driverID, err)
					}
				}

				// Send notification to each affected shift
				for i, driverID := range affectedDriverIDs {
					shiftID, hasShift := driverShifts[driverID]
					if !hasShift {
						log.Printf("   [%d/%d] Skipping driver %s (no active shift)", i+1, len(affectedDriverIDs), driverID)
						continue
					}

					log.Printf("   [%d/%d] Notifying shift %s (driver: %s)", i+1, len(affectedDriverIDs), shiftID, driverID)

					// Call NotifyDriverOfRouteUpdate with full notification details
					notifyErr := NotifyDriverOfRouteUpdate(
						db,
						centrifugoClient,
						shiftID,
						"route_updated",
						map[string]interface{}{
							"move_request_id": id,
							"manager_name":    managerName,
							"action_type":     actionType,
							"bin_number":      binNumber,
							"message":         fmt.Sprintf("%s has %s Bin #%d from your route", managerName, actionType, binNumber),
						},
					)
					if notifyErr != nil {
						log.Printf("⚠️  Failed to send notification to shift %s: %v", shiftID, notifyErr)
					} else {
						log.Printf("   ✅ Notification sent to shift %s", shiftID)
					}
				}
			} else {
				log.Printf("   ⚠️  No affected drivers to notify")
			}
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		} else {
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("⚠️  [CENTRIFUGO] Skipping notifications")
			if !assignmentChanged && len(affectedDriverIDs) == 0 {
				log.Printf("   Reason: No assignment changes and no affected drivers")
			} else if centrifugoClient == nil {
				log.Printf("   Reason: Centrifugo client not available")
			}
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		}

		// Return updated move request
		response := updatedMove.BinMoveRequest.ToBinMoveRequestResponse()

		// Add assigned user/driver names to response
		if updatedMove.AssignedUserName != nil {
			response.AssignedUserName = updatedMove.AssignedUserName
		}
		if updatedMove.AssignedDriverName != nil {
			response.AssignedDriverName = updatedMove.AssignedDriverName
		}

		// Set unified driver_name field
		if updatedMove.AssignedDriverName != nil {
			response.DriverName = updatedMove.AssignedDriverName
		} else if updatedMove.AssignedUserName != nil {
			response.DriverName = updatedMove.AssignedUserName
		}

		// Fetch bin details
		var bin models.Bin
		err = db.Get(&bin, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
			FROM bins WHERE id = $1
		`, updatedMove.BinID)
		if err == nil {
			binResp := bin.ToBinResponse()
			response.Bin = &binResp
			// Flatten bin fields for easy table display
			response.BinNumber = bin.BinNumber
			response.CurrentStreet = bin.CurrentStreet
			response.City = bin.City
			response.Zip = bin.Zip
		}

		// Parse new address into separate fields if available
		if updatedMove.NewAddress != nil {
			// Split "street, city zip" format
			parts := strings.Split(*updatedMove.NewAddress, ", ")
			if len(parts) >= 2 {
				street := parts[0]
				cityZip := strings.TrimSpace(parts[1])
				cityZipParts := strings.Split(cityZip, " ")
				if len(cityZipParts) >= 2 {
					city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
					zip := cityZipParts[len(cityZipParts)-1]
					response.NewStreet = &street
					response.NewCity = &city
					response.NewZip = &zip
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// CancelBinMoveRequest cancels a pending move request
// PUT /api/manager/bins/move-requests/:id/cancel
func CancelBinMoveRequest(db *sqlx.DB, redisClient *redis.Client, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch move request to check status
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT id, bin_id, status, assigned_shift_id
			FROM bin_move_requests
			WHERE id = $1
		`, id)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// Only allow cancelling pending or in_progress moves
		if moveRequest.Status == "completed" {
			http.Error(w, "Cannot cancel completed move request", http.StatusBadRequest)
			return
		}
		if moveRequest.Status == "cancelled" {
			http.Error(w, "Move request already cancelled", http.StatusBadRequest)
			return
		}

		// Get manager ID from context
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		managerID := userClaims.UserID

		now := time.Now().Unix()

		// Update move request status to cancelled
		_, err = db.Exec(`
			UPDATE bin_move_requests
			SET status = 'cancelled', updated_at = $1
			WHERE id = $2
		`, now, id)
		if err != nil {
			log.Printf("Error cancelling move request: %v", err)
			http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
			return
		}

		// Log history: move request cancelled by manager
		var managerName string
		err = db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerID)
		if err != nil {
			log.Printf("Warning: Failed to fetch manager name for history: %v", err)
			managerName = "Unknown Manager"
		}
		reason := "Cancelled by manager"
		err = helpers.LogMoveRequestCancelled(db, id, managerID, managerName, &reason)
		if err != nil {
			log.Printf("Warning: Failed to log move request cancellation: %v", err)
		}

		// Update bin status back to active
		_, err = db.Exec(`
			UPDATE bins
			SET status = 'active', updated_at = $1
			WHERE id = $2
		`, now, moveRequest.BinID)
		if err != nil {
			log.Printf("Warning: Failed to update bin status: %v", err)
		}

		// If move was assigned to a shift, remove it from route_tasks
		if moveRequest.AssignedShiftID != nil {
			_, err = db.Exec(`
				DELETE FROM route_tasks
				WHERE shift_id = $1 AND bin_id = $2
			`, *moveRequest.AssignedShiftID, moveRequest.BinID)
			if err != nil {
				log.Printf("Warning: Failed to remove bin from shift: %v", err)
			}

			// Send WebSocket update to driver
			wsHub.BroadcastToUser(*moveRequest.AssignedShiftID, map[string]interface{}{
				"type":    "move_request_cancelled",
				"bin_id":  moveRequest.BinID,
				"message": "Move request cancelled by manager",
			})

		// Soft delete route_tasks associated with this move request (for audit trail)
		_, err = db.Exec(`
			UPDATE route_tasks
			SET is_deleted = true,
				deleted_at = $1,
				deleted_by = $2,
				deletion_reason = $3,
				updated_at = $1
			WHERE move_request_id = $4 AND shift_id = $5 AND is_completed = 0 AND is_deleted = false
		`, now, managerID, "move_request_cancelled", id, *moveRequest.AssignedShiftID)
		if err != nil {
			log.Printf("⚠️  Failed to soft delete route_tasks for cancelled move request: %v", err)
		} else {
			log.Printf("✅ Soft deleted route_tasks for cancelled move request %s from shift %s", id, *moveRequest.AssignedShiftID)
		}

		// Notify driver that route has been updated
		if err := NotifyDriverOfRouteUpdate(db, centrifugoClient, *moveRequest.AssignedShiftID, "move_request_cancelled", map[string]interface{}{
			"move_request_id": id,
			"reason":          "Move request was cancelled by manager",
		}); err != nil {
			log.Printf("⚠️  Failed to notify driver of route update: %v", err)
		}

		// Re-optimize the shift (skip gates since this is manager-initiated)
		if err := ReoptimizeActiveShift(db, redisClient, *moveRequest.AssignedShiftID, centrifugoClient, true); err != nil {
			log.Printf("⚠️  Failed to re-optimize shift after move request cancellation: %v", err)
			// Don't fail the entire request if re-optimization fails
		} else {
			log.Printf("✅ Successfully re-optimized shift %s after move request cancellation", *moveRequest.AssignedShiftID)
		}
		}

		// Publish move_request_cancelled to Centrifugo so all manager dashboards update
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "move_request_cancelled", map[string]interface{}{
				"move_request_id": id,
				"bin_id":          moveRequest.BinID,
				"status":          "cancelled",
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish move_request_cancelled to Centrifugo: %v", pubErr)
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Move request cancelled successfully",
		})
	}
}

// AssignMoveToUser assigns a move request to a specific user for manual completion
// PUT /api/manager/bins/move-requests/:id/assign-to-user
func AssignMoveToUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		log.Printf("👤 [ASSIGN TO USER] Starting assignment for move request: %s", id)
		if id == "" {
			log.Printf("❌ [ASSIGN TO USER] Missing move request ID")
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		var req struct {
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [ASSIGN TO USER] Invalid request body: %v", err)
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		log.Printf("👤 [ASSIGN TO USER] Request body - UserID: %s", req.UserID)

		if req.UserID == "" {
			log.Printf("❌ [ASSIGN TO USER] user_id is required but empty")
			http.Error(w, "user_id is required", http.StatusBadRequest)
			return
		}

		// Fetch move request to check status
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT id, bin_id, status, assignment_type
			FROM bin_move_requests
			WHERE id = $1
		`, id)
		if err != nil {
			if err == sql.ErrNoRows {
				log.Printf("❌ [ASSIGN TO USER] Move request not found: %s", id)
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("❌ [ASSIGN TO USER] Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		log.Printf("👤 [ASSIGN TO USER] Found move request - Status: %s, BinID: %s, CurrentType: %v", moveRequest.Status, moveRequest.BinID, moveRequest.AssignmentType)

		// Allow reassigning from any status except completed or cancelled
		if moveRequest.Status == "completed" || moveRequest.Status == "cancelled" {
			log.Printf("❌ [ASSIGN TO USER] Cannot assign move request with status: %s", moveRequest.Status)
			http.Error(w, fmt.Sprintf("Cannot reassign %s move request", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// Verify user exists
		var userExists bool
		err = db.Get(&userExists, "SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)", req.UserID)
		if err != nil || !userExists {
			log.Printf("❌ [ASSIGN TO USER] User not found: %s (error: %v, exists: %v)", req.UserID, err, userExists)
			http.Error(w, "User not found", http.StatusNotFound)
			return
		}

		log.Printf("👤 [ASSIGN TO USER] User exists, proceeding with assignment")

		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [ASSIGN TO USER] Failed to begin transaction: %v", err)
			http.Error(w, "Failed to assign move request", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// If previously assigned to a shift, remove from route_tasks
		if moveRequest.AssignedShiftID != nil {
			log.Printf("👤 [ASSIGN TO USER] Removing bin from shift %s", *moveRequest.AssignedShiftID)
			_, err = tx.Exec(`
				DELETE FROM route_tasks
				WHERE shift_id = $1 AND bin_id = $2
			`, *moveRequest.AssignedShiftID, moveRequest.BinID)
			if err != nil {
				log.Printf("❌ [ASSIGN TO USER] Failed to remove from route_tasks: %v", err)
				http.Error(w, "Failed to remove from shift", http.StatusInternalServerError)
				return
			}

			// Update shift total_bins count
			_, err = tx.Exec(`
				UPDATE shifts
				SET total_bins = total_bins - 1, updated_at = $1
				WHERE id = $2
			`, now, *moveRequest.AssignedShiftID)
			if err != nil {
				log.Printf("❌ [ASSIGN TO USER] Failed to update shift count: %v", err)
			}
		}

		// Update move request - clear shift assignment and set user assignment
		result, err := tx.Exec(`
			UPDATE bin_move_requests
			SET assignment_type = 'manual',
			    assigned_user_id = $1,
			    assigned_shift_id = NULL,
			    status = 'assigned',
			    updated_at = $2
			WHERE id = $3
		`, req.UserID, now, id)
		if err != nil {
			log.Printf("❌ [ASSIGN TO USER] Error updating move request: %v", err)
			http.Error(w, "Failed to assign move request", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		log.Printf("👤 [ASSIGN TO USER] Update result - Rows affected: %d", rowsAffected)

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ [ASSIGN TO USER] Failed to commit transaction: %v", err)
			http.Error(w, "Failed to assign move request", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [ASSIGN TO USER] Move request %s assigned to user %s for manual completion", id, req.UserID)

		// Log history: move request manually assigned to user
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			log.Printf("Warning: Could not get manager context for history logging")
		} else {
			managerID := userClaims.UserID
			var managerName string
			err = db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerID)
			if err != nil {
				log.Printf("Warning: Failed to fetch manager name for history: %v", err)
				managerName = "Unknown Manager"
			}

			var userName string
			err = db.Get(&userName, `SELECT name FROM users WHERE id = $1`, req.UserID)
			if err != nil {
				log.Printf("Warning: Failed to fetch assigned user name for history: %v", err)
				userName = "Unknown User"
			}

			err = helpers.LogMoveRequestAssigned(db, id, managerID, managerName, "manual", &req.UserID, &userName, nil)
			if err != nil {
				log.Printf("Warning: Failed to log move request assignment: %v", err)
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Move request assigned successfully",
		})
	}
}

// ManuallyCompleteMoveRequest marks a move request as manually completed
// PUT /api/manager/bins/move-requests/:id/complete-manually
func ManuallyCompleteMoveRequest(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Get user ID from context (person completing the move)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		userID := userClaims.UserID

		// Fetch move request
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `SELECT * FROM bin_move_requests WHERE id = $1`, id)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// Verify this is a manual move
		if moveRequest.AssignmentType == nil || *moveRequest.AssignmentType != "manual" {
			http.Error(w, "This endpoint is only for manual moves. Use shift completion flow for shift-based moves.", http.StatusBadRequest)
			return
		}

		// Only allow completing assigned or in_progress manual moves
		if moveRequest.Status != "assigned" && moveRequest.Status != "in_progress" {
			http.Error(w, fmt.Sprintf("Cannot complete move request with status: %s", moveRequest.Status), http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Mark move request as completed
		_, err = db.Exec(`
			UPDATE bin_move_requests
			SET status = 'completed', completed_at = $1, updated_at = $1
			WHERE id = $2
		`, now, moveRequest.ID)
		if err != nil {
			log.Printf("Error completing move request: %v", err)
			http.Error(w, "Failed to complete move request", http.StatusInternalServerError)
			return
		}

		log.Printf("[MANUAL MOVE] ✅ Move request marked as completed")

		// Log history: move request manually completed by manager
		var managerName string
		err = db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, userID)
		if err != nil {
			log.Printf("Warning: Failed to fetch manager name for history: %v", err)
			managerName = "Unknown Manager"
		}
		err = helpers.LogMoveRequestCompleted(db, moveRequest.ID, userID, managerName)
		if err != nil {
			log.Printf("Warning: Failed to log move request completion: %v", err)
		}

		if moveRequest.MoveType == "pickup_only" {
			// Pickup for retirement or storage
			newStatus := "active" // Fallback
			if moveRequest.DisposalAction != nil {
				if *moveRequest.DisposalAction == "retire" {
					newStatus = "retired"
					log.Printf("[MANUAL MOVE]    → Bin will be RETIRED")
				} else if *moveRequest.DisposalAction == "store" {
					newStatus = "in_storage"
					log.Printf("[MANUAL MOVE]    → Bin will be IN STORAGE")
				}
			}

			_, err = db.Exec(`
				UPDATE bins
				SET status = $1, updated_at = $2
				WHERE id = $3
			`, newStatus, now, moveRequest.BinID)
			if err != nil {
				log.Printf("Error updating bin status: %v", err)
				http.Error(w, "Failed to update bin status", http.StatusInternalServerError)
				return
			}

			log.Printf("[MANUAL MOVE] ✅ Bin status updated to %s", newStatus)

		} else if moveRequest.MoveType == "relocation" || moveRequest.MoveType == "redeployment" {
			// Update bin location to new coordinates
			log.Printf("[MANUAL MOVE]    → Relocating bin to new address")

			// Parse address into separate fields
			var fromStreet, fromCity, fromZip, toStreet, toCity, toZip *string

			// Parse original address
			if moveRequest.OriginalAddress != "" {
				parts := strings.Split(moveRequest.OriginalAddress, ", ")
				if len(parts) >= 2 {
					street := parts[0]
					fromStreet = &street
					cityZip := strings.TrimSpace(parts[1])
					cityZipParts := strings.Split(cityZip, " ")
					if len(cityZipParts) >= 2 {
						city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
						zip := cityZipParts[len(cityZipParts)-1]
						fromCity = &city
						fromZip = &zip
					}
				}
			}

			// Parse new address
			if moveRequest.NewAddress != nil {
				parts := strings.Split(*moveRequest.NewAddress, ", ")
				if len(parts) >= 2 {
					street := parts[0]
					toStreet = &street
					cityZip := strings.TrimSpace(parts[1])
					cityZipParts := strings.Split(cityZip, " ")
					if len(cityZipParts) >= 2 {
						city := strings.Join(cityZipParts[:len(cityZipParts)-1], " ")
						zip := cityZipParts[len(cityZipParts)-1]
						toCity = &city
						toZip = &zip
					}
				}
			}

			_, err = db.Exec(`
				UPDATE bins
				SET latitude = $1,
				    longitude = $2,
				    current_street = $3,
				    status = 'active',
				    updated_at = $4
				WHERE id = $5
			`, moveRequest.NewLatitude,
				moveRequest.NewLongitude,
				moveRequest.NewAddress,
				now,
				moveRequest.BinID)
			if err != nil {
				log.Printf("Error relocating bin: %v", err)
				http.Error(w, "Failed to relocate bin", http.StatusInternalServerError)
				return
			}

			// Record the move in moves table with manual flag
			_, err = db.Exec(`
				INSERT INTO moves (
					bin_id, moved_from, moved_to, moved_on,
					move_type, from_street, from_city, from_zip,
					to_street, to_city, to_zip,
					move_request_id, completed_by_user_id
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			`, moveRequest.BinID,
				moveRequest.OriginalAddress,
				*moveRequest.NewAddress,
				now,
				"manual", // move_type
				fromStreet, fromCity, fromZip,
				toStreet, toCity, toZip,
				moveRequest.ID,
				userID)
			if err != nil {
				log.Printf("[MANUAL MOVE] ⚠️  Failed to record move: %v", err)
				// Don't fail - move is already completed
			} else {
				log.Printf("[MANUAL MOVE] ✅ Move recorded in history")
			}

			log.Printf("[MANUAL MOVE] ✅ Bin relocated to %s", *moveRequest.NewAddress)
		}
			// If this was a relocation/redeployment to a potential location, mark location as converted
			if moveRequest.SourcePotentialLocationID != nil {
				log.Printf("[MANUAL MOVE]    → Manual relocation to potential location - marking as converted")
				
				_, err = db.Exec(`
					UPDATE potential_locations
					SET converted_to_bin_id = $1,
					    converted_at = $2,
					    converted_by_user_id = $3,
					    updated_at = $2
					WHERE id = $4
				`, moveRequest.BinID, now, userID, *moveRequest.SourcePotentialLocationID)
				
				if err != nil {
					log.Printf("[MANUAL MOVE] ⚠️  Error converting potential location: %v", err)
				} else {
					log.Printf("[MANUAL MOVE] ✅ Potential location marked as converted")
				}
			}


		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Move completed successfully",
		})
	}
}

// ClearMoveAssignment removes all assignment from a move request (shift or user)
// PUT /api/manager/bins/move-requests/:id/clear-assignment
func ClearMoveAssignment(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		log.Printf("🔄 [CLEAR ASSIGNMENT] Starting for move request: %s", id)
		if id == "" {
			log.Printf("❌ [CLEAR ASSIGNMENT] Missing move request ID")
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch move request to check current assignment
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT id, bin_id, status, assignment_type, assigned_shift_id, assigned_user_id
			FROM bin_move_requests
			WHERE id = $1
		`, id)
		if err != nil {
			if err == sql.ErrNoRows {
				log.Printf("❌ [CLEAR ASSIGNMENT] Move request not found: %s", id)
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("❌ [CLEAR ASSIGNMENT] Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		log.Printf("🔄 [CLEAR ASSIGNMENT] Current state - Status: %s, Type: %v, ShiftID: %v, UserID: %v",
			moveRequest.Status, moveRequest.AssignmentType, moveRequest.AssignedShiftID, moveRequest.AssignedUserID)

		// Only allow clearing assignments from pending or assigned moves
		if moveRequest.Status != "pending" && moveRequest.Status != "assigned" {
			log.Printf("❌ [CLEAR ASSIGNMENT] Cannot clear assignment from status: %s", moveRequest.Status)
			http.Error(w, fmt.Sprintf("Cannot clear assignment from %s move request", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// Check if there's any assignment to clear
		if moveRequest.AssignedShiftID == nil && moveRequest.AssignedUserID == nil {
			log.Printf("⚠️  [CLEAR ASSIGNMENT] Move request already unassigned")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"message": "Move request is already unassigned",
			})
			return
		}

		now := time.Now().Unix()

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [CLEAR ASSIGNMENT] Failed to begin transaction: %v", err)
			http.Error(w, "Failed to clear assignment", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// If assigned to a shift, remove from route_tasks
		if moveRequest.AssignedShiftID != nil {
			log.Printf("🔄 [CLEAR ASSIGNMENT] Removing bin from shift %s", *moveRequest.AssignedShiftID)
			_, err = tx.Exec(`
				DELETE FROM route_tasks
				WHERE shift_id = $1 AND bin_id = $2
			`, *moveRequest.AssignedShiftID, moveRequest.BinID)
			if err != nil {
				log.Printf("❌ [CLEAR ASSIGNMENT] Failed to remove from route_tasks: %v", err)
				http.Error(w, "Failed to remove from shift", http.StatusInternalServerError)
				return
			}

			// Update shift total_bins count
			_, err = tx.Exec(`
				UPDATE shifts
				SET total_bins = total_bins - 1, updated_at = $1
				WHERE id = $2
			`, now, *moveRequest.AssignedShiftID)
			if err != nil {
				log.Printf("❌ [CLEAR ASSIGNMENT] Failed to update shift count: %v", err)
			}
		}

		// Clear all assignments and reset to pending
		_, err = tx.Exec(`
			UPDATE bin_move_requests
			SET assignment_type = '',
			    assigned_shift_id = NULL,
			    assigned_user_id = NULL,
			    status = 'pending',
			    updated_at = $1
			WHERE id = $2
		`, now, id)
		if err != nil {
			log.Printf("❌ [CLEAR ASSIGNMENT] Error clearing assignment: %v", err)
			http.Error(w, "Failed to clear assignment", http.StatusInternalServerError)
			return
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ [CLEAR ASSIGNMENT] Failed to commit transaction: %v", err)
			http.Error(w, "Failed to clear assignment", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [CLEAR ASSIGNMENT] Assignment cleared successfully for move request %s", id)

		// Log history: move request unassigned
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			log.Printf("Warning: Could not get manager context for history logging")
		} else {
			managerID := userClaims.UserID
			var managerName string
			err = db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerID)
			if err != nil {
				log.Printf("Warning: Failed to fetch manager name for history: %v", err)
				managerName = "Unknown Manager"
			}

			// Get previous assignment info
			var previousUserID *string
			var previousUserName *string
			var previousShiftID *string

			if moveRequest.AssignedUserID != nil {
				previousUserID = moveRequest.AssignedUserID
				var userName string
				err = db.Get(&userName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID)
				if err == nil {
					previousUserName = &userName
				}
			}

			if moveRequest.AssignedShiftID != nil {
				previousShiftID = moveRequest.AssignedShiftID
			}

			err = helpers.LogMoveRequestUnassigned(db, id, managerID, managerName,
				moveRequest.AssignmentType, previousUserID, previousUserName, previousShiftID)
			if err != nil {
				log.Printf("Warning: Failed to log move request unassignment: %v", err)
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Assignment cleared successfully",
		})
	}
}

// GetMoveRequestHistory retrieves the full audit trail for a move request
// GET /api/manager/bins/move-requests/:id/history
func GetMoveRequestHistory(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Get history using helper
		history, err := helpers.GetMoveRequestHistory(db, id)
		if err != nil {
			log.Printf("Error fetching move request history: %v", err)
			http.Error(w, "Failed to fetch history", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(history)
	}
}
