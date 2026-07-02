package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func AssignMoveToShift(store moverequest.Store, db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
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

		// Fetch the move via the domain Store.
		moveRequest, err := store.ByID(moveRequestID)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
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
		err = assignMoveToShift(db, wsHub, fcmService, centrifugoClient, *moveRequest, bin, req.ShiftID, req.InsertAfterBinID, req.InsertPosition, managerID, managerName)
		if err != nil {
			log.Printf("❌ [ASSIGN TO SHIFT] Error assigning move to shift: %v", err)
			if errors.Is(err, itinerary.ErrMissingDestination) {
				// store/pickup_only need a warehouse dropoff; if the warehouse location isn't
				// configured we can't route the move — a clean 400, not a 500.
				http.Error(w, "Cannot assign this move: its destination is unresolved (for store/pickup-only moves, configure the warehouse location first)", http.StatusBadRequest)
				return
			}
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
		SELECT rt.id, rt.shift_id, COALESCE(rt.bin_id, '') as bin_id, rt.sequence_order, rt.is_completed,
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
	isFutureShift := activeShift.Status == "ready" // FIX: "ready" is the correct status for future shifts

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

	// 3. Insert the move's stops at the determined position (itinerary.AddMove).
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Resolve the move's dropoff destination. relocation/redeployment go to the move's new
	// location; store/pickup_only go to the CURRENT warehouse (every move is two-leg — #37).
	var dropoffLat, dropoffLng float64
	var dropoffAddr string
	if moveRequest.NewLatitude != nil && moveRequest.NewLongitude != nil {
		dropoffLat = *moveRequest.NewLatitude
		dropoffLng = *moveRequest.NewLongitude
	}
	if moveRequest.NewAddress != nil {
		dropoffAddr = *moveRequest.NewAddress
	}
	if moveRequest.MoveType == "store" || moveRequest.MoveType == "pickup_only" {
		if wlat, wlng, waddr, ok := resolveCurrentWarehouse(db); ok {
			dropoffLat, dropoffLng, dropoffAddr = wlat, wlng, waddr
		}
	}

	// Assemble the move's stops (pickup [+ dropoff]) at the insert position — the
	// itinerary domain owns route_tasks writes (was inline INSERTs here).
	addReason := "assigned to shift"
	_, err = itinerary.AddMove(tx, activeShift.ID, itinerary.MovePlacement{
		InsertSeq:      insertSequenceOrder,
		MoveRequestID:  moveRequest.ID,
		BinID:          moveRequest.BinID,
		BinNumber:      bin.BinNumber,
		FillPercentage: bin.FillPercentage,
		MoveType:       string(moveRequest.MoveType),
		PickupLat:      bin.Latitude,
		PickupLng:      bin.Longitude,
		PickupAddress:  bin.CurrentStreet,
		DropoffLat:     dropoffLat,
		DropoffLng:     dropoffLng,
		DropoffAddress: dropoffAddr,
		AddedBy:        &managerID,
		AdditionReason: &addReason,
		Now:            now,
	})
	if err != nil {
		return err
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

	if err = moverequest.AssignToShift(tx, moveRequest.ID, activeShift.ID, moveRequestStatus, now); err != nil {
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
		err = moverequest.LogAssigned(db, moveRequest.ID, managerID, managerName, "manager",
			newAssignmentType, &activeShift.DriverID, &driverName, &activeShift.ID)
		if err != nil {
			log.Printf("   ⚠️  WARNING: Failed to log assignment history: %v", err)
		} else {
			log.Printf("   ✅ Assignment history logged successfully")
		}
	} else {
		// Reassignment
		log.Printf("   📝 Logging REASSIGNMENT to history (from previous assignment to driver: %s, shift: %s)", driverName, activeShift.ID)
		err = moverequest.LogReassigned(db, moveRequest.ID, managerID, managerName, "manager",
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

	// Recompute the shift's counts from route_tasks (single source of truth) — replaces the +binsAdded.
	if err = itinerary.RecomputeShiftCounts(tx, activeShift.ID, now); err != nil {
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

func AssignMoveToUser(store moverequest.Store, db *sqlx.DB) http.HandlerFunc {
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

		// Fetch via the domain Store. (The previous partial SELECT omitted
		// assigned_shift_id, so the route_tasks cleanup below was dead code and a
		// shift→user reassignment orphaned the old shift's task; SELECT * populates
		// it, so that cleanup now runs correctly.)
		moveRequest, err := store.ByID(id)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
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
			log.Printf("👤 [ASSIGN TO USER] Removing this move's tasks from shift %s", *moveRequest.AssignedShiftID)
			// Move-scoped audited soft-delete (was a bin-scoped HARD delete that also wiped
			// unrelated same-bin tasks + completed tasks — the #24 data-loss bug).
			actor, _ := middleware.GetUserFromContext(r)
			var taskIDs []string
			if serr := tx.Select(&taskIDs, `
				SELECT id FROM route_tasks
				WHERE move_request_id = $1 AND shift_id = $2 AND is_completed = 0 AND is_deleted = false
			`, id, *moveRequest.AssignedShiftID); serr != nil {
				log.Printf("❌ [ASSIGN TO USER] Failed to select route_tasks: %v", serr)
				http.Error(w, "Failed to remove from shift", http.StatusInternalServerError)
				return
			}
			if rerr := itinerary.RemoveByIDs(tx, taskIDs, actor.UserID, "move_reassigned_to_user", now); rerr != nil {
				log.Printf("❌ [ASSIGN TO USER] Failed to remove from route_tasks: %v", rerr)
				http.Error(w, "Failed to remove from shift", http.StatusInternalServerError)
				return
			}

			// Recompute the old shift's counts from route_tasks — replaces the -1.
			if err = itinerary.RecomputeShiftCounts(tx, *moveRequest.AssignedShiftID, now); err != nil {
				log.Printf("❌ [ASSIGN TO USER] Failed to update shift count: %v", err)
			}
		}

		// Set the move on the driver's backlog (domain owns the transition).
		if err = moverequest.AssignToDriver(tx, id, req.UserID, now); err != nil {
			log.Printf("❌ [ASSIGN TO USER] Error updating move request: %v", err)
			http.Error(w, "Failed to assign move request", http.StatusInternalServerError)
			return
		}

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

			err = moverequest.LogAssigned(db, id, managerID, managerName, "manager", "manual", &req.UserID, &userName, nil)
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

func ClearMoveAssignment(store moverequest.Store, db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		log.Printf("🔄 [CLEAR ASSIGNMENT] Starting for move request: %s", id)
		if id == "" {
			log.Printf("❌ [CLEAR ASSIGNMENT] Missing move request ID")
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch the move via the domain Store. The shift cleanup (route_tasks /
		// total_bins) below stays on db — that's shift-domain work this handler
		// orchestrates around the move-request transition.
		moveRequest, err := store.ByID(id)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
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
			log.Printf("🔄 [CLEAR ASSIGNMENT] Removing this move's tasks from shift %s", *moveRequest.AssignedShiftID)
			// Move-scoped audited soft-delete (was a bin-scoped HARD delete that also wiped
			// unrelated same-bin tasks + completed tasks — the #24 data-loss bug).
			actor, _ := middleware.GetUserFromContext(r)
			var taskIDs []string
			if serr := tx.Select(&taskIDs, `
				SELECT id FROM route_tasks
				WHERE move_request_id = $1 AND shift_id = $2 AND is_completed = 0 AND is_deleted = false
			`, id, *moveRequest.AssignedShiftID); serr != nil {
				log.Printf("❌ [CLEAR ASSIGNMENT] Failed to select route_tasks: %v", serr)
				http.Error(w, "Failed to remove from shift", http.StatusInternalServerError)
				return
			}
			if rerr := itinerary.RemoveByIDs(tx, taskIDs, actor.UserID, "move_assignment_cleared", now); rerr != nil {
				log.Printf("❌ [CLEAR ASSIGNMENT] Failed to remove from route_tasks: %v", rerr)
				http.Error(w, "Failed to remove from shift", http.StatusInternalServerError)
				return
			}

			// Recompute the old shift's counts from route_tasks — replaces the -1.
			if err = itinerary.RecomputeShiftCounts(tx, *moveRequest.AssignedShiftID, now); err != nil {
				log.Printf("❌ [CLEAR ASSIGNMENT] Failed to update shift count: %v", err)
			}
		}

		// Clear all assignments and reset to the pending pool (domain owns this
		// transition; the counterpart to ReleaseFromShift's backlog rule).
		if err = moverequest.ClearAssignment(tx, id, now); err != nil {
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

			err = moverequest.LogUnassigned(db, id, managerID, managerName, "manager",
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
