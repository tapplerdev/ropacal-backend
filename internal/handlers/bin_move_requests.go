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
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"

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

// ScheduleBinMove creates a new bin move request (urgent or future scheduled)
// POST /api/manager/bins/schedule-move
func ScheduleBinMove(store moverequest.Store, db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
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
		urgency := moverequest.ScheduledUrgency(req.ScheduledDate, time.Now().Unix())

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

		// Fetch warehouse location for store/redeployment moves
		var warehouseForMove *models.WarehouseLocation
		if req.MoveType == "store" || req.MoveType == "redeployment" {
			var warehouseJSON []byte
			err := db.QueryRow(`SELECT value FROM config WHERE key = 'warehouse_location'`).Scan(&warehouseJSON)

			if err == sql.ErrNoRows {
				log.Printf("❌ Warehouse location not configured in database")
				http.Error(w, "Warehouse location must be configured before creating store/redeployment moves.", http.StatusPreconditionFailed)
				return
			}
			if err != nil {
				log.Printf("❌ Failed to fetch warehouse location: %v", err)
				http.Error(w, "Failed to fetch warehouse location", http.StatusInternalServerError)
				return
			}

			var warehouse models.WarehouseLocation
			if err := json.Unmarshal(warehouseJSON, &warehouse); err != nil {
				log.Printf("❌ Failed to parse warehouse location: %v", err)
				http.Error(w, "Failed to parse warehouse location", http.StatusInternalServerError)
				return
			}
			warehouseForMove = &warehouse

			if req.MoveType == "store" {
				// Store: auto-set destination to warehouse
				log.Printf("🏭 [STORE MOVE] Auto-filling warehouse destination")
				req.NewLatitude = &warehouse.Latitude
				req.NewLongitude = &warehouse.Longitude
				newAddress = &warehouse.Address
				log.Printf("✅ [STORE MOVE] Warehouse destination set: %s (%.6f, %.6f)",
					warehouse.Address, warehouse.Latitude, warehouse.Longitude)
			} else if req.MoveType == "redeployment" {
				log.Printf("🏭 [REDEPLOYMENT] Will use warehouse as pickup location (bin is in_storage)")
			}
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

		// Build original address (for redeployment, use warehouse since bin is in_storage there)
		var originalAddress string
		if req.MoveType == "redeployment" && warehouseForMove != nil {
			originalAddress = warehouseForMove.Address
		} else {
			originalAddress = fmt.Sprintf("%s, %s %s", bin.CurrentStreet, bin.City, bin.Zip)
		}

		// Generate ID and timestamp for insert
		id := uuid.New().String()
		now := time.Now().Unix()

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
			ID:            id,
			BinID:         req.BinID,
			ScheduledDate: req.ScheduledDate,
			Urgency:       urgency, // Auto-calculated urgency
			RequestedBy:   userID,
			Status:        status,
			OriginalLatitude: func() float64 {
				if req.MoveType == "redeployment" && warehouseForMove != nil {
					return warehouseForMove.Latitude
				}
				return *bin.Latitude
			}(),
			OriginalLongitude: func() float64 {
				if req.MoveType == "redeployment" && warehouseForMove != nil {
					return warehouseForMove.Longitude
				}
				return *bin.Longitude
			}(),
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

		// Insert via the domain Store (the lifecycle entry point).
		if err = store.Create(&moveRequest, req.ReasonCategory); err != nil {
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
		err = moverequest.LogCreated(db, id, userID, userName, req.MoveType, newAddress)
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
