package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ropacal-backend/internal/middleware"
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

		// Determine status based on whether shift is assigned
		status := "pending"
		if req.ShiftID != nil {
			status = "assigned" // Immediately assigned to shift
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
			AssignmentType:    "shift",   // Default to shift-based assignment
			AssignedShiftID:   req.ShiftID, // Assign to shift if provided
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
		if moveRequestID == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		var req struct {
			ShiftID          *string `json:"shift_id"`            // Optional - auto-find active shift if nil
			InsertAfterBinID *string `json:"insert_after_bin_id"` // For active shifts - insert after specific bin
			InsertPosition   *string `json:"insert_position"`     // For future shifts - 'start' or 'end'
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

		// Check if can be assigned (only pending, assigned, or in_progress moves can be reassigned)
		if moveRequest.Status != "pending" && moveRequest.Status != "assigned" && moveRequest.Status != "in_progress" {
			http.Error(w, fmt.Sprintf("Cannot reassign move request with status: %s", moveRequest.Status), http.StatusBadRequest)
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
		err = assignMoveToShift(db, wsHub, fcmService, moveRequest, bin, req.ShiftID, req.InsertAfterBinID, req.InsertPosition)
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

// assignMoveToShift inserts move at specified position in shift and re-optimizes route
func assignMoveToShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, moveRequest models.BinMoveRequest, bin models.Bin, shiftID *string, insertAfterBinID *string, insertPosition *string) error {
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
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Shift all bins after insert position up by 1
	_, err = tx.Exec(`
		UPDATE shift_bins
		SET sequence_order = sequence_order + 1
		WHERE shift_id = $1 AND sequence_order >= $2
	`, activeShift.ID, insertSequenceOrder)
	if err != nil {
		return fmt.Errorf("failed to shift sequence order: %w", err)
	}

	// Insert move bin at determined position
	_, err = tx.Exec(`
		INSERT INTO shift_bins (shift_id, bin_id, sequence_order, is_completed, created_at)
		VALUES ($1, $2, $3, 0, $4)
	`, activeShift.ID, moveRequest.BinID, insertSequenceOrder, now)
	if err != nil {
		return fmt.Errorf("failed to insert urgent move bin: %w", err)
	}

	// Update move request to assign it to this shift
	_, err = tx.Exec(`
		UPDATE bin_move_requests
		SET assigned_shift_id = $1, status = 'assigned', updated_at = $2
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

	// 4. Re-optimize remaining route (bins after the inserted move) - only for active shifts
	if isActiveShift {
		var remainingBins []models.ShiftBinWithDetails
		err = tx.Select(&remainingBins, `
			SELECT rb.id, rb.shift_id, rb.bin_id, rb.sequence_order,
			       b.bin_number, b.current_street, b.city, b.zip, b.fill_percentage,
			       b.latitude, b.longitude
			FROM shift_bins rb
			JOIN bins b ON rb.bin_id = b.id
			WHERE rb.shift_id = $1 AND rb.sequence_order > $2 AND rb.is_completed = 0
			ORDER BY rb.sequence_order ASC
		`, activeShift.ID, insertSequenceOrder)
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

			// Use inserted move bin as start location for re-optimization
			insertedMoveLocation := services.Location{
				Latitude:  moveRequest.OriginalLatitude,
				Longitude: moveRequest.OriginalLongitude,
			}

			// Re-optimize remaining bins
			optimizer := services.NewRouteOptimizer()
			optimizedBins := optimizer.OptimizeRoute(binsToOptimize, insertedMoveLocation)

			log.Printf("   Re-optimizing %d remaining bins after inserted move", len(optimizedBins))

			// Update sequence order for optimized bins
			for i, optimizedBin := range optimizedBins {
				newSequence := insertSequenceOrder + 1 + i // After inserted move
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

		// Fetch associated bins, requester names, and driver names
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
					SELECT u.full_name FROM shifts s
					JOIN users u ON s.driver_id = u.id
					WHERE s.id = $1
				`, *mr.AssignedShiftID)
				if err == nil {
					responses[i].AssignedDriverName = &driverName
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
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

// UpdateBinMoveRequest updates move request details (date, notes, location, etc.)
// PUT /api/manager/bins/move-requests/:id
func UpdateBinMoveRequest(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Parse request body
		var req struct {
			ScheduledDate *int64   `json:"scheduled_date,omitempty"`
			Reason        *string  `json:"reason,omitempty"`
			Notes         *string  `json:"notes,omitempty"`
			NewStreet     *string  `json:"new_street,omitempty"`
			NewCity       *string  `json:"new_city,omitempty"`
			NewZip        *string  `json:"new_zip,omitempty"`
			NewLatitude   *float64 `json:"new_latitude,omitempty"`
			NewLongitude  *float64 `json:"new_longitude,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Fetch existing move request
		var moveRequest models.BinMoveRequest
		err := db.Get(&moveRequest, "SELECT * FROM bin_move_requests WHERE id = $1", id)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// Only allow updating pending or assigned moves
		if moveRequest.Status == "completed" || moveRequest.Status == "cancelled" {
			http.Error(w, "Cannot update completed or cancelled move request", http.StatusBadRequest)
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

		// Add ID parameter at the end
		args = append(args, id)

		// Execute update
		query := fmt.Sprintf("UPDATE bin_move_requests SET %s WHERE id = $%d",
			strings.Join(updates, ", "), argCount)

		_, err = db.Exec(query, args...)
		if err != nil {
			log.Printf("Error updating move request: %v", err)
			http.Error(w, "Failed to update move request", http.StatusInternalServerError)
			return
		}

		// Fetch updated move request
		err = db.Get(&moveRequest, "SELECT * FROM bin_move_requests WHERE id = $1", id)
		if err != nil {
			log.Printf("Error fetching updated move request: %v", err)
			http.Error(w, "Failed to fetch updated move request", http.StatusInternalServerError)
			return
		}

		// Return updated move request
		response := moveRequest.ToBinMoveRequestResponse()

		// Fetch bin details
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

// AssignMoveToUser assigns a move request to a specific user for manual completion
// PUT /api/manager/bins/move-requests/:id/assign-to-user
func AssignMoveToUser(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		var req struct {
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.UserID == "" {
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
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// Only allow assigning pending moves
		if moveRequest.Status != "pending" {
			http.Error(w, fmt.Sprintf("Cannot assign move request with status: %s", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// Verify user exists
		var userExists bool
		err = db.Get(&userExists, "SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)", req.UserID)
		if err != nil || !userExists {
			http.Error(w, "User not found", http.StatusNotFound)
			return
		}

		now := time.Now().Unix()

		// Update move request
		_, err = db.Exec(`
			UPDATE bin_move_requests
			SET assignment_type = 'manual',
			    assigned_user_id = $1,
			    status = 'assigned',
			    updated_at = $2
			WHERE id = $3
		`, req.UserID, now, id)
		if err != nil {
			log.Printf("Error assigning move to user: %v", err)
			http.Error(w, "Failed to assign move request", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ Move request %s assigned to user %s for manual completion", id, req.UserID)

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
		if moveRequest.AssignmentType != "manual" {
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
