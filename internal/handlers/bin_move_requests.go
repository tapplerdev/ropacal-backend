package handlers

import (
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
func ScheduleBinMove(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService) http.HandlerFunc {
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
		if req.MoveType != "store" && req.MoveType != "pickup_only" && req.MoveType != "relocation" {
			http.Error(w, "Invalid move_type: must be 'store', 'pickup_only' (deprecated), or 'relocation'", http.StatusBadRequest)
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
		moveRequest := models.BinMoveRequest{
			ID:                id,
			BinID:             req.BinID,
			ScheduledDate:     req.ScheduledDate,
			Urgency:           urgency, // Auto-calculated urgency
			RequestedBy:       userID,
			Status:            status,
			OriginalLatitude:  *bin.Latitude,
			OriginalLongitude: *bin.Longitude,
			OriginalAddress:   originalAddress,
			NewLatitude:       req.NewLatitude,
			NewLongitude:      req.NewLongitude,
			NewAddress:        newAddress, // Built from separate fields or provided address
			MoveType:          req.MoveType,
			DisposalAction:    req.DisposalAction,
			Reason:            req.Reason,
			Notes:             req.Notes,
			AssignmentType:    assignmentType, // Set based on whether shift is assigned
			AssignedShiftID:   req.ShiftID,    // Assign to shift if provided
			CreatedAt:         now,
			UpdatedAt:         now,
		}

		// Insert into database
		_, err = db.Exec(`
			INSERT INTO bin_move_requests (
				id, bin_id, scheduled_date, urgency, requested_by, status,
				original_latitude, original_longitude, original_address,
				new_latitude, new_longitude, new_address,
				move_type, disposal_action, reason, notes,
				assignment_type, assigned_shift_id,
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		`,
			moveRequest.ID, moveRequest.BinID, moveRequest.ScheduledDate,
			moveRequest.Urgency, moveRequest.RequestedBy, moveRequest.Status,
			moveRequest.OriginalLatitude, moveRequest.OriginalLongitude, moveRequest.OriginalAddress,
			moveRequest.NewLatitude, moveRequest.NewLongitude, moveRequest.NewAddress,
			moveRequest.MoveType, moveRequest.DisposalAction, moveRequest.Reason, moveRequest.Notes,
			moveRequest.AssignmentType, moveRequest.AssignedShiftID,
			moveRequest.CreatedAt, moveRequest.UpdatedAt,
		)
		if err != nil {
			log.Printf("Error creating bin move request: %v", err)
			http.Error(w, "Failed to create move request", http.StatusInternalServerError)
			return
		}

		// Log history: move request created
		var userName string
		err = db.Get(&userName, `SELECT name FROM users WHERE id = $1`, userID)
		if err != nil {
			log.Printf("Warning: Failed to fetch user name for history: %v", err)
			userName = "Unknown User"
		}
		err = helpers.LogMoveRequestCreated(db, id, userID, userName)
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

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(response)
	}
}

// AssignMoveToShift explicitly assigns a pending move request to a shift
// POST /api/manager/bins/move-requests/:id/assign-to-shift
func AssignMoveToShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService) http.HandlerFunc {
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

		log.Printf("🚚 [ASSIGN TO SHIFT] Found bin - Number: %s", bin.BinNumber)

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
		err = assignMoveToShift(db, wsHub, fcmService, moveRequest, bin, req.ShiftID, req.InsertAfterBinID, req.InsertPosition, managerID, managerName)
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
func assignMoveToShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, moveRequest models.BinMoveRequest, bin models.Bin, shiftID *string, insertAfterBinID *string, insertPosition *string, managerID string, managerName string) error {
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
		SELECT rb.id, rb.shift_id, rb.bin_id, rb.sequence_order, rb.is_completed,
		       b.bin_number, b.current_street, b.city, b.zip, COALESCE(b.fill_percentage, 0) as fill_percentage,
		       b.latitude, b.longitude
		FROM shift_bins rb
		JOIN bins b ON rb.bin_id = b.id
		WHERE rb.shift_id = $1
		ORDER BY rb.sequence_order ASC
	`, activeShift.ID)
	if err != nil {
		return fmt.Errorf("failed to fetch shift bins: %w", err)
	}

	// Determine where to insert the bin based on shift status and parameters
	var insertSequenceOrder int

	now := time.Now().Unix()
	isActiveShift := activeShift.Status == "active"
	isFutureShift := activeShift.Status == "not_started"

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

	// Shift all bins after insert position up by binsAdded
	_, err = tx.Exec(`
		UPDATE shift_bins
		SET sequence_order = sequence_order + $1
		WHERE shift_id = $2 AND sequence_order >= $3
	`, binsAdded, activeShift.ID, insertSequenceOrder)
	if err != nil {
		return fmt.Errorf("failed to shift sequence order: %w", err)
	}

	// Insert pickup waypoint for move request
	pickupSeq := insertSequenceOrder
	log.Printf("   🔍 DEBUG: About to insert PICKUP - insertSequenceOrder=%d, pickupSeq=%d", insertSequenceOrder, pickupSeq)
	log.Printf("   🔍 DEBUG: INSERT params: shift_id=%s, bin_id=%d, sequence=%d, stop_type=pickup, move_request_id=%s",
		activeShift.ID, moveRequest.BinID, pickupSeq, moveRequest.ID)

	_, err = tx.Exec(`
		INSERT INTO shift_bins (shift_id, bin_id, sequence_order, is_completed, created_at, stop_type, move_request_id)
		VALUES ($1, $2, $3, 0, $4, 'pickup', $5)
	`, activeShift.ID, moveRequest.BinID, pickupSeq, now, moveRequest.ID)
	if err != nil {
		return fmt.Errorf("failed to insert pickup waypoint: %w", err)
	}
	log.Printf("   ✅ Inserted pickup waypoint at sequence %d", pickupSeq)

	// For relocation moves, also insert dropoff waypoint immediately after pickup
	if moveRequest.MoveType == "relocation" {
		dropoffSeq := insertSequenceOrder + 1
		log.Printf("   🔍 DEBUG: About to insert DROPOFF - insertSequenceOrder=%d, dropoffSeq=%d", insertSequenceOrder, dropoffSeq)
		log.Printf("   🔍 DEBUG: INSERT params: shift_id=%s, bin_id=%d, sequence=%d, stop_type=dropoff, move_request_id=%s",
			activeShift.ID, moveRequest.BinID, dropoffSeq, moveRequest.ID)

		_, err = tx.Exec(`
			INSERT INTO shift_bins (shift_id, bin_id, sequence_order, is_completed, created_at, stop_type, move_request_id)
			VALUES ($1, $2, $3, 0, $4, 'dropoff', $5)
		`, activeShift.ID, moveRequest.BinID, dropoffSeq, now, moveRequest.ID)
		if err != nil {
			return fmt.Errorf("failed to insert dropoff waypoint: %w", err)
		}
		log.Printf("   ✅ Inserted dropoff waypoint at sequence %d", dropoffSeq)

		// Verify both waypoints have different sequence_order values
		var actualPickupSeq, actualDropoffSeq int
		err = tx.Get(&actualPickupSeq, `
			SELECT sequence_order FROM shift_bins
			WHERE shift_id = $1 AND move_request_id = $2 AND stop_type = 'pickup'
		`, activeShift.ID, moveRequest.ID)
		if err != nil {
			log.Printf("   ⚠️  Warning: Could not verify pickup sequence_order: %v", err)
		}

		err = tx.Get(&actualDropoffSeq, `
			SELECT sequence_order FROM shift_bins
			WHERE shift_id = $1 AND move_request_id = $2 AND stop_type = 'dropoff'
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
		helpers.LogMoveRequestAssigned(db, moveRequest.ID, managerID, managerName,
			newAssignmentType, &activeShift.DriverID, &driverName, &activeShift.ID)
	} else {
		// Reassignment
		helpers.LogMoveRequestReassigned(db, moveRequest.ID, managerID, managerName,
			previousAssignmentType, &newAssignmentType,
			previousAssignedUserID, &activeShift.DriverID,
			previousAssignedUserName, &driverName,
			previousAssignedShiftID, &activeShift.ID)
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

	// 4. Re-optimize remaining route (bins after the inserted move) - only for active shifts
	if isActiveShift {
		var remainingBins []models.ShiftBinWithDetails
		err = tx.Select(&remainingBins, `
			SELECT rb.id, rb.shift_id, rb.bin_id, rb.sequence_order,
			       b.bin_number, b.current_street, b.city, b.zip, COALESCE(b.fill_percentage, 0) as fill_percentage,
			       b.latitude, b.longitude
			FROM shift_bins rb
			JOIN bins b ON rb.bin_id = b.id
			WHERE rb.shift_id = $1 AND rb.sequence_order > $2 AND rb.is_completed = 0
			  AND rb.move_request_id IS NULL
			ORDER BY rb.sequence_order ASC
		`, activeShift.ID, insertSequenceOrder + binsAdded - 1)
		if err != nil {
			return fmt.Errorf("failed to fetch remaining bins: %w", err)
		}

		if len(remainingBins) > 0 {
			// Convert to BinWithPriority for optimizer
			binsToOptimize := make([]services.BinWithPriority, len(remainingBins))
			for i, sb := range remainingBins {
				binsToOptimize[i] = services.BinWithPriority{
					ID:             sb.BinID,
					Latitude:       sb.Latitude,
					Longitude:      sb.Longitude,
					FillPercentage: sb.FillPercentage,
					CurrentStreet:  sb.CurrentStreet,
				}
			}

			// Use dropoff location as start for re-optimization (where driver will be after completing move)
			// For relocation moves, use the new location; for store moves, use original location
			var startLat, startLng float64
			if moveRequest.MoveType == "relocation" && moveRequest.NewLatitude != nil && moveRequest.NewLongitude != nil {
				startLat = *moveRequest.NewLatitude
				startLng = *moveRequest.NewLongitude
			} else {
				// Store move - driver returns to pickup location after storing bin
				startLat = moveRequest.OriginalLatitude
				startLng = moveRequest.OriginalLongitude
			}

			insertedMoveLocation := services.Location{
				Latitude:  startLat,
				Longitude: startLng,
			}

			// Re-optimize remaining bins
			optimizer := services.NewRouteOptimizer()
			optimizedBins := optimizer.OptimizeRoute(binsToOptimize, insertedMoveLocation)

			log.Printf("   Re-optimizing %d remaining bins after inserted move", len(optimizedBins))

			// Update sequence order for optimized bins
			for i, optimizedBin := range optimizedBins {
				newSequence := insertSequenceOrder + binsAdded + i // After both move waypoints
				_, err = tx.Exec(`
					UPDATE shift_bins
					SET sequence_order = $1
					WHERE shift_id = $2 AND bin_id = $3
				`, newSequence, activeShift.ID, optimizedBin.ID)
				if err != nil {
					return fmt.Errorf("failed to update sequence order: %w", err)
				}
			}

			log.Printf("   ✅ Route re-optimized successfully")
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	// 5. Get updated shift and bins for broadcast
	var updatedShift models.Shift
	db.Get(&updatedShift, `SELECT * FROM shifts WHERE id = $1`, activeShift.ID)

	updatedBins, err := getRouteBinsWithDetails(db, activeShift.ID)
	if err != nil {
		log.Printf("⚠️  Failed to fetch updated bins: %v", err)
		updatedBins = []models.ShiftBinWithDetails{}
	}

	// 6. Send WebSocket update to driver
	log.Printf("📡 Broadcasting urgent move update to driver %s", activeShift.DriverID)
	wsHub.BroadcastToUser(activeShift.DriverID, map[string]interface{}{
		"type": "urgent_move_inserted",
		"data": map[string]interface{}{
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
		},
	})

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
		wsHub.BroadcastToUser(activeShift.DriverID, map[string]interface{}{
			"type": "move_request_assigned",
			"data": map[string]interface{}{
				"move_request": moveRequestWithBin,
				"updated_route": map[string]interface{}{
					"shift_id": activeShift.ID,
					"bins":     updatedBins,
				},
			},
		})
		log.Printf("✅ move_request_assigned notification sent")
	} else {
		log.Printf("⚠️  Failed to fetch updated move request for WebSocket: %v", err)
	}

	// 7. Send push notification to driver
	if fcmService != nil {
		var fcmToken models.FCMToken
		tokenErr := db.Get(&fcmToken, `
			SELECT * FROM fcm_tokens
			WHERE user_id = $1
			ORDER BY created_at DESC
			LIMIT 1
		`, activeShift.DriverID)

		if tokenErr == nil {
			err := fcmService.SendShiftUpdateNotification(
				fcmToken.Token,
				activeShift.ID,
				fmt.Sprintf("urgent_move_bin_%d", bin.BinNumber),
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
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch move request
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT * FROM bin_move_requests WHERE id = $1
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

		// Build response
		response := moveRequest.ToBinMoveRequestResponse()

	// Override urgency with smart calculation (resolved for completed/cancelled)
	response.Urgency = calculateUrgency(moveRequest.Status, moveRequest.ScheduledDate)

		// Fetch associated bin details
		var bin models.Bin
		err = db.Get(&bin, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
			FROM bins WHERE id = $1
		`, moveRequest.BinID)
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

		// Fetch assigned driver name if assigned to a shift
		if moveRequest.AssignedShiftID != nil {
			var driverName string
			err = db.Get(&driverName, `
				SELECT u.full_name FROM shifts s
				JOIN users u ON s.driver_id = u.id
				WHERE s.id = $1
			`, *moveRequest.AssignedShiftID)
			if err == nil {
				response.AssignedDriverName = &driverName
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// GetBinMoveRequests returns all bin move requests with optional filtering
// GET /api/manager/bins/move-requests?status=pending&urgency=urgent
func GetBinMoveRequests(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse query params
		status := r.URL.Query().Get("status")
		urgency := r.URL.Query().Get("urgency")

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
func UpdateBinMoveRequest(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
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
		}

		err = db.Get(&moveRequest, `
			SELECT
				mr.*,
				s.status as shift_status,
				u.name as shift_driver_name,
				(SELECT COUNT(*) FROM shift_bins WHERE shift_id = mr.assigned_shift_id) as total_waypoints
			FROM bin_move_requests mr
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
				// Remove from shift_bins, reset to pending
				if moveRequest.AssignedShiftID != nil {
					_, err = tx.Exec(`DELETE FROM shift_bins WHERE shift_id = $1 AND bin_id = $2`,
						*moveRequest.AssignedShiftID, moveRequest.BinID)
					if err != nil {
						log.Printf("Error removing from shift_bins: %v", err)
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
				// For now, just log - full implementation would update waypoint_order in shift_bins

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
				_, err = tx.Exec(`DELETE FROM shift_bins WHERE shift_id = $1 AND bin_id = $2`,
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
				// Remove from shift_bins if previously assigned to a shift
				if moveRequest.AssignedShiftID != nil {
					log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
					log.Printf("⭕ [UNASSIGNMENT] Starting shift removal")
					log.Printf("   Move Request ID: %s", id)
					log.Printf("   Old Shift ID: %s", *moveRequest.AssignedShiftID)
					log.Printf("   Bin ID: %s", moveRequest.BinID)

					_, err = tx.Exec(`DELETE FROM shift_bins WHERE shift_id = $1 AND bin_id = $2`,
						*moveRequest.AssignedShiftID, moveRequest.BinID)
					if err == nil {
						log.Printf("   ✅ Removed bin from shift_bins")
						_, err = tx.Exec(`UPDATE shifts SET total_bins = total_bins - 1, updated_at = $1 WHERE id = $2`,
							now, *moveRequest.AssignedShiftID)
						if err == nil {
							log.Printf("   ✅ Updated shift total_bins count")
						} else {
							log.Printf("   ⚠️  Failed to update shift count: %v", err)
						}
					} else {
						log.Printf("   ❌ Failed to remove from shift_bins: %v", err)
					}

					log.Printf("[UNASSIGNMENT] Removed from shift_bins for shift: %s", *moveRequest.AssignedShiftID)
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

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			http.Error(w, "Failed to commit changes", http.StatusInternalServerError)
			return
		}

		log.Printf("[UPDATE MOVE] ✅ Successfully updated move request: %s", id)

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

			// Compare address (NewStreet, NewCity, NewZip)
			if req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil {
				oldAddress := ""
				if moveRequest.NewAddress != nil {
					oldAddress = *moveRequest.NewAddress
				}
				newAddress := fmt.Sprintf("%s, %s, %s", *req.NewStreet, *req.NewCity, *req.NewZip)
				if oldAddress != newAddress {
					oldAddrPtr := &oldAddress
					if oldAddress == "" {
						oldAddrPtr = nil
					}
					changes = append(changes, ChangeDetail{
						Field: "address",
						Label: "New Address",
						Old:   oldAddrPtr,
						New:   &newAddress,
					})
				}
			}

			// Compare coordinates (if both lat/lng provided)
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
					oldCoords := ""
					if oldLat != nil && oldLng != nil {
						oldCoords = fmt.Sprintf("%.6f, %.6f", *oldLat, *oldLng)
					}
					newCoords := fmt.Sprintf("%.6f, %.6f", *newLat, *newLng)

					oldCoordsPtr := &oldCoords
					if oldCoords == "" {
						oldCoordsPtr = nil
					}

					changes = append(changes, ChangeDetail{
						Field: "coordinates",
						Label: "Coordinates",
						Old:   oldCoordsPtr,
						New:   &newCoords,
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

			notes := "Updated move details"
			err = helpers.LogMoveRequestUpdated(db, id, managerUserID, managerName, &notes, metadataJSON)
			if err != nil {
				log.Printf("Warning: Failed to log move request update: %v", err)
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

		if (assignmentChanged || len(affectedDriverIDs) > 0) && wsHub != nil {
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("📡 [WEBSOCKET] Sending notifications...")

			// Broadcast to all managers
			managerPayload := &websocket.Message{
				Data: map[string]interface{}{
					"type":            "move_request_updated",
					"move_request_id": id,
					"status":          updatedMove.Status,
					"bin_id":          updatedMove.BinID,
				},
			}
			log.Printf("   Broadcasting to managers (admin role):")
			log.Printf("   Payload: %+v", managerPayload)
			wsHub.BroadcastToRole("admin", managerPayload)
			log.Printf("   ✅ Manager notification sent")

			// Notify affected drivers
			if len(affectedDriverIDs) > 0 {
				log.Printf("   Broadcasting to %d affected driver(s):", len(affectedDriverIDs))

				// Fetch bin number for the notification
				var binNumber int
				err := db.Get(&binNumber, `SELECT bin_number FROM bins WHERE id = $1`, updatedMove.BinID)
				if err != nil {
					log.Printf("Warning: Could not fetch bin number: %v", err)
					binNumber = 0 // Fallback
				}

				// Determine action type (removed/added)
				// Recalculate isUnassigning for this scope
				isUnassigning := moveRequest.AssignedShiftID != nil && updatedMove.AssignedShiftID == nil
				actionType := "updated"
				if isUnassigning {
					actionType = "removed"
				} else if moveRequest.AssignedShiftID == nil && updatedMove.AssignedShiftID != nil {
					actionType = "added"
				}

				for i, driverID := range affectedDriverIDs {
					driverPayload := &websocket.Message{
						Data: map[string]interface{}{
							"type":            "route_updated",
							"message":         fmt.Sprintf("%s has %s Bin #%d from your route", managerName, actionType, binNumber),
							"move_request_id": id,
							"manager_name":    managerName,
							"action_type":     actionType,
							"bin_number":      binNumber,
						},
					}
					log.Printf("   [%d/%d] Driver ID: %s", i+1, len(affectedDriverIDs), driverID)
					log.Printf("   Payload: %+v", driverPayload)
					wsHub.BroadcastToUser(driverID, driverPayload)
					log.Printf("   ✅ Notification sent to driver %s", driverID)
				}
			} else {
				log.Printf("   ⚠️  No affected drivers to notify")
			}
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		} else {
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("⚠️  [WEBSOCKET] Skipping notifications")
			if !assignmentChanged && len(affectedDriverIDs) == 0 {
				log.Printf("   Reason: No assignment changes and no affected drivers")
			} else if wsHub == nil {
				log.Printf("   Reason: WebSocket hub not available")
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
func CancelBinMoveRequest(db *sqlx.DB, wsHub *websocket.Hub) http.HandlerFunc {
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

		// If move was assigned to a shift, remove it from shift_bins
		if moveRequest.AssignedShiftID != nil {
			_, err = db.Exec(`
				DELETE FROM shift_bins
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

		log.Printf("👤 [ASSIGN TO USER] Found move request - Status: %s, BinID: %s, CurrentType: %s", moveRequest.Status, moveRequest.BinID, moveRequest.AssignmentType)

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

		// If previously assigned to a shift, remove from shift_bins
		if moveRequest.AssignedShiftID != nil {
			log.Printf("👤 [ASSIGN TO USER] Removing bin from shift %s", *moveRequest.AssignedShiftID)
			_, err = tx.Exec(`
				DELETE FROM shift_bins
				WHERE shift_id = $1 AND bin_id = $2
			`, *moveRequest.AssignedShiftID, moveRequest.BinID)
			if err != nil {
				log.Printf("❌ [ASSIGN TO USER] Failed to remove from shift_bins: %v", err)
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

		} else if moveRequest.MoveType == "relocation" {
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

		log.Printf("🔄 [CLEAR ASSIGNMENT] Current state - Status: %s, Type: %s, ShiftID: %v, UserID: %v",
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

		// If assigned to a shift, remove from shift_bins
		if moveRequest.AssignedShiftID != nil {
			log.Printf("🔄 [CLEAR ASSIGNMENT] Removing bin from shift %s", *moveRequest.AssignedShiftID)
			_, err = tx.Exec(`
				DELETE FROM shift_bins
				WHERE shift_id = $1 AND bin_id = $2
			`, *moveRequest.AssignedShiftID, moveRequest.BinID)
			if err != nil {
				log.Printf("❌ [CLEAR ASSIGNMENT] Failed to remove from shift_bins: %v", err)
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
