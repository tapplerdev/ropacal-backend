package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/orgdb"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func GetBinMoveRequest(store moverequest.Store, root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		store := moverequest.NewSQLStore(db)
		id := chi.URLParam(r, "id")
		log.Printf("🔍 [GET_MOVE_REQUEST] Fetching move request ID: %s", id)

		if id == "" {
			log.Printf("❌ [GET_MOVE_REQUEST] Missing move request ID")
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch the move request through the domain Store. The bin + driver reads
		// below stay on db: they cross into the bin/shift domains, which the
		// move-request Store does not own.
		log.Printf("   [STEP 1] Querying bin_move_requests table...")
		moveRequest, err := store.ByID(id)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
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
		response.Urgency = moverequest.Urgency(moveRequest.Status, moveRequest.ScheduledDate, time.Now().Unix())

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

		// Resolve the responsible driver via the domain — one rule for both manual
		// (assigned_user_id) and shift (the shift's driver); empty for pool moves.
		if _, driverName, dErr := store.ResponsibleDriver(moveRequest); dErr == nil && driverName != "" {
			response.AssignedDriverName = &driverName
		} else if dErr != nil {
			log.Printf("⚠️  [STEP 5] Could not resolve responsible driver: %v", dErr)
		}

		log.Printf("🎉 [GET_MOVE_REQUEST] Successfully prepared response for move request %s", id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// GetBinMoveRequests returns all bin move requests with optional filtering
// GET /api/manager/bins/move-requests?status=pending&urgency=urgent
func GetBinMoveRequests(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
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
			responses[i].Urgency = moverequest.Urgency(mr.Status, mr.ScheduledDate, time.Now().Unix())

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
func GetBinMoveRequestsByBinID(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
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
			responses[i].Urgency = moverequest.Urgency(mr.Status, mr.ScheduledDate, time.Now().Unix())

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

func GetMoveRequestHistory(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Get history using helper
		history, err := moverequest.GetHistory(db, id)
		if err != nil {
			log.Printf("Error fetching move request history: %v", err)
			http.Error(w, "Failed to fetch history", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(history)
	}
}

// GetDriverPendingMoves returns pending/assigned/overdue move requests for a specific driver.
// Used by shift creation to show move request awareness banners.
func GetDriverPendingMoves(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		driverID := chi.URLParam(r, "id")
		if driverID == "" {
			http.Error(w, "Missing driver ID", http.StatusBadRequest)
			return
		}

		type PendingMove struct {
			ID            string  `json:"id" db:"id"`
			BinID         string  `json:"bin_id" db:"bin_id"`
			BinNumber     int     `json:"bin_number" db:"bin_number"`
			Status        string  `json:"status" db:"status"`
			MoveType      string  `json:"move_type" db:"move_type"`
			ScheduledDate int64   `json:"scheduled_date" db:"scheduled_date"`
			Reason        *string `json:"reason" db:"reason"`
			CurrentStreet string  `json:"current_street" db:"current_street"`
			City          string  `json:"city" db:"city"`
			Urgency       string  `json:"urgency"`
		}

		var moves []PendingMove
		err := db.Select(&moves, `
			SELECT mr.id, mr.bin_id, b.bin_number, mr.status, mr.move_type,
				-- scheduled_date is already BIGINT epoch seconds (NOT NULL), not a
				-- timestamp. EXTRACT(EPOCH FROM bigint) has no such function, so this
				-- 500'd on every call: "function pg_catalog.extract(unknown, bigint)
				-- does not exist". Select it directly.
				mr.scheduled_date,
				mr.reason, b.current_street, b.city
			FROM bin_move_requests mr
			JOIN bins b ON b.id = mr.bin_id
			WHERE mr.assigned_user_id = $1
				AND mr.status IN ('pending', 'assigned', 'in_progress')
			ORDER BY mr.scheduled_date ASC
		`, driverID)
		if err != nil {
			log.Printf("❌ [PendingMoves] DB error: %v", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		// Compute urgency based on scheduled date
		now := time.Now().Unix()
		for i := range moves {
			if moves[i].ScheduledDate == 0 {
				moves[i].Urgency = "scheduled"
			} else if moves[i].ScheduledDate < now-7*86400 {
				moves[i].Urgency = "critical" // overdue >7 days
			} else if moves[i].ScheduledDate < now {
				moves[i].Urgency = "overdue"
			} else if moves[i].ScheduledDate < now+86400 {
				moves[i].Urgency = "due_today"
			} else {
				moves[i].Urgency = "scheduled"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(moves)
	}
}
