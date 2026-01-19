package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/websocket"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

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
		if req.BinID == "" || req.Urgency == "" || req.MoveType == "" {
			http.Error(w, "Missing required fields: bin_id, urgency, move_type", http.StatusBadRequest)
			return
		}

		// Validate urgency
		if req.Urgency != "urgent" && req.Urgency != "scheduled" {
			http.Error(w, "Invalid urgency: must be 'urgent' or 'scheduled'", http.StatusBadRequest)
			return
		}

		// Validate move_type
		if req.MoveType != "pickup_only" && req.MoveType != "relocation" {
			http.Error(w, "Invalid move_type: must be 'pickup_only' or 'relocation'", http.StatusBadRequest)
			return
		}

		// Validate pickup_only moves require disposal_action
		if req.MoveType == "pickup_only" && req.DisposalAction == nil {
			http.Error(w, "pickup_only moves require disposal_action ('retire' or 'store')", http.StatusBadRequest)
			return
		}

		// Validate relocation moves require new location
		if req.MoveType == "relocation" && (req.NewLatitude == nil || req.NewLongitude == nil || req.NewAddress == nil) {
			http.Error(w, "relocation moves require new_latitude, new_longitude, and new_address", http.StatusBadRequest)
			return
		}

		// Get requesting user ID from context (set by Auth middleware)
		userID, ok := r.Context().Value("user_id").(string)
		if !ok || userID == "" {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}

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

		// Generate ID and timestamps
		id := uuid.New().String()
		now := time.Now().Unix()

		// Create bin move request
		moveRequest := models.BinMoveRequest{
			ID:                id,
			BinID:             req.BinID,
			ScheduledDate:     req.ScheduledDate,
			Urgency:           req.Urgency,
			RequestedBy:       userID,
			Status:            "pending",
			OriginalLatitude:  *bin.Latitude,
			OriginalLongitude: *bin.Longitude,
			OriginalAddress:   originalAddress,
			NewLatitude:       req.NewLatitude,
			NewLongitude:      req.NewLongitude,
			NewAddress:        req.NewAddress,
			MoveType:          req.MoveType,
			DisposalAction:    req.DisposalAction,
			Reason:            req.Reason,
			Notes:             req.Notes,
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
				created_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		`,
			moveRequest.ID, moveRequest.BinID, moveRequest.ScheduledDate,
			moveRequest.Urgency, moveRequest.RequestedBy, moveRequest.Status,
			moveRequest.OriginalLatitude, moveRequest.OriginalLongitude, moveRequest.OriginalAddress,
			moveRequest.NewLatitude, moveRequest.NewLongitude, moveRequest.NewAddress,
			moveRequest.MoveType, moveRequest.DisposalAction, moveRequest.Reason, moveRequest.Notes,
			moveRequest.CreatedAt, moveRequest.UpdatedAt,
		)
		if err != nil {
			log.Printf("Error creating bin move request: %v", err)
			http.Error(w, "Failed to create move request", http.StatusInternalServerError)
			return
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
		if moveRequestID == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		var req struct {
			ShiftID *string `json:"shift_id"` // Optional - auto-find active shift if nil
		}
		json.NewDecoder(r.Body).Decode(&req)

		// Fetch move request
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, `
			SELECT * FROM bin_move_requests WHERE id = $1
		`, moveRequestID)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// Check if already assigned
		if moveRequest.Status != "pending" {
			http.Error(w, fmt.Sprintf("Move request already %s", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// Fetch bin details
		var bin models.Bin
		err = db.Get(&bin, "SELECT * FROM bins WHERE id = $1", moveRequest.BinID)
		if err != nil {
			http.Error(w, "Bin not found", http.StatusNotFound)
			return
		}

		// Call the assignment logic
		err = assignMoveToShift(db, wsHub, fcmService, moveRequest, bin, req.ShiftID)
		if err != nil {
			log.Printf("Error assigning move to shift: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Move request assigned to shift successfully",
		})
	}
}

// assignMoveToShift inserts move as next waypoint in shift and re-optimizes route
func assignMoveToShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, moveRequest models.BinMoveRequest, bin models.Bin, shiftID *string) error {
	log.Printf("🚚 ASSIGN MOVE: Assigning move request for bin #%d to shift", bin.BinNumber)

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
		       b.bin_number, b.current_street, b.city, b.zip, b.fill_percentage,
		       b.latitude, b.longitude
		FROM shift_bins rb
		JOIN bins b ON rb.bin_id = b.id
		WHERE rb.shift_id = $1
		ORDER BY rb.sequence_order ASC
	`, activeShift.ID)
	if err != nil {
		return fmt.Errorf("failed to fetch shift bins: %w", err)
	}

	// Find first uncompleted bin index
	currentIndex := -1
	for i, sb := range shiftBins {
		if sb.IsCompleted == 0 {
			currentIndex = i
			break
		}
	}

	if currentIndex == -1 {
		log.Printf("⚠️  All bins completed - cannot insert urgent move")
		return nil // All bins done, nothing to do
	}

	log.Printf("   Current position: bin #%d at index %d", shiftBins[currentIndex].BinNumber, currentIndex)
	log.Printf("   Inserting urgent move as next waypoint (index %d)", currentIndex+1)

	// 3. Insert move request bin as next waypoint
	now := time.Now().Unix()
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Shift all bins after current position up by 1
	_, err = tx.Exec(`
		UPDATE shift_bins
		SET sequence_order = sequence_order + 1
		WHERE shift_id = $1 AND sequence_order > $2
	`, activeShift.ID, currentIndex+1)
	if err != nil {
		return fmt.Errorf("failed to shift sequence order: %w", err)
	}

	// Insert urgent move bin at next position
	_, err = tx.Exec(`
		INSERT INTO shift_bins (shift_id, bin_id, sequence_order, is_completed, created_at)
		VALUES ($1, $2, $3, 0, $4)
	`, activeShift.ID, moveRequest.BinID, currentIndex+2, now)
	if err != nil {
		return fmt.Errorf("failed to insert urgent move bin: %w", err)
	}

	// Update move request to assign it to this shift
	_, err = tx.Exec(`
		UPDATE bin_move_requests
		SET assigned_shift_id = $1, status = 'in_progress', updated_at = $2
		WHERE id = $3
	`, activeShift.ID, now, moveRequest.ID)
	if err != nil {
		return fmt.Errorf("failed to update move request: %w", err)
	}

	// Update shift total_bins count
	_, err = tx.Exec(`
		UPDATE shifts
		SET total_bins = total_bins + 1, updated_at = $1
		WHERE id = $2
	`, now, activeShift.ID)
	if err != nil {
		return fmt.Errorf("failed to update shift: %w", err)
	}

	// 4. Re-optimize remaining route (bins after the urgent move)
	var remainingBins []models.ShiftBinWithDetails
	err = tx.Select(&remainingBins, `
		SELECT rb.id, rb.shift_id, rb.bin_id, rb.sequence_order,
		       b.bin_number, b.current_street, b.city, b.zip, b.fill_percentage,
		       b.latitude, b.longitude
		FROM shift_bins rb
		JOIN bins b ON rb.bin_id = b.id
		WHERE rb.shift_id = $1 AND rb.sequence_order > $2 AND rb.is_completed = 0
		ORDER BY rb.sequence_order ASC
	`, activeShift.ID, currentIndex+2)
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

		// Use urgent move bin as start location for re-optimization
		urgentMoveLocation := services.Location{
			Latitude:  moveRequest.OriginalLatitude,
			Longitude: moveRequest.OriginalLongitude,
		}

		// Re-optimize remaining bins
		optimizer := services.NewRouteOptimizer()
		optimizedBins := optimizer.OptimizeRoute(binsToOptimize, urgentMoveLocation)

		log.Printf("   Re-optimizing %d remaining bins after urgent move", len(optimizedBins))

		// Update sequence order for optimized bins
		for i, optimizedBin := range optimizedBins {
			newSequence := currentIndex + 3 + i // After current + urgent move
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
				"id":                  updatedShift.ID,
				"status":              updatedShift.Status,
				"total_bins":          updatedShift.TotalBins,
				"completed_bins":      updatedShift.CompletedBins,
				"bins":                updatedBins,
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
			       bmr.assigned_shift_id, bmr.completed_at, bmr.created_at, bmr.updated_at
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

		// Fetch associated bins
		responses := make([]models.BinMoveRequestResponse, len(moveRequests))
		for i, mr := range moveRequests {
			responses[i] = mr.ToBinMoveRequestResponse()

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
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
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
