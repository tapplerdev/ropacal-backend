package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"ropacal-backend/internal/bindomain"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/orgdb"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// BinWithPriority extends Bin with calculated priority score and metadata
type BinWithPriority struct {
	models.Bin
	PriorityScore          float64 `json:"priority_score"`
	DaysSinceCheck         *int    `json:"days_since_check,omitempty"`
	NextMoveRequestDate    *int64  `json:"next_move_request_date,omitempty"`
	MoveRequestUrgency     *string `json:"move_request_urgency,omitempty"`
	HasPendingMove         bool    `json:"has_pending_move"`
	HasCheckRecommendation bool    `json:"has_check_recommendation"`
}

// calculateBinPriority computes a weighted priority score for a bin
// Higher score = higher priority
//
// Scoring is purely based on days since last check:
//   - 0-3 days:  0 (CHECKED)
//   - 4-6 days:  200 (CHECK SOON)
//   - 7-13 days: 500 (OVERDUE)
//   - 14+ days:  1000 (CRITICAL)
//
// For never-checked bins, created_at is used as the baseline.
func calculateBinPriority(bin models.Bin, moveRequest *models.BinMoveRequest, hasCheckRec bool, now int64) (float64, *int) {
	var daysSinceCheck *int

	if bin.LastCheckedAt != nil {
		daysSince := int((now - *bin.LastCheckedAt) / 86400)
		daysSinceCheck = &daysSince
	} else {
		// Never checked — use creation date as baseline
		daysSince := int((now - bin.CreatedAt) / 86400)
		daysSinceCheck = &daysSince
	}

	score := 0.0
	if daysSinceCheck != nil {
		d := *daysSinceCheck
		switch {
		case d >= 14:
			score = 1000.0
		case d >= 7:
			score = 500.0
		case d >= 4:
			score = 200.0
		}
	}

	return score, daysSinceCheck
}

// GetBinsWithPriority returns bins with priority scores and filtering
// Query params:
//   - sort: priority (default), bin_number, fill_percentage, days_since_check
//   - filter: next_move_request, longest_unchecked, high_fill, has_check_recommendation, all (default)
//   - status: active (default), all, retired, pending_move, in_storage
//   - limit: max results (default: 100)
func GetBinsWithPriority(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		sortBy := r.URL.Query().Get("sort")
		if sortBy == "" {
			sortBy = "priority"
		}

		filter := r.URL.Query().Get("filter")
		if filter == "" {
			filter = "all"
		}

		status := r.URL.Query().Get("status")
		if status == "" {
			status = "active"
		}

		limitStr := r.URL.Query().Get("limit")
		limit := 100
		if limitStr != "" {
			if l, err := strconv.Atoi(limitStr); err == nil {
				limit = l
			}
		}

		now := time.Now().Unix()

		log.Printf("[GET-BINS-PRIORITY] Fetching bins (sort=%s, filter=%s, status=%s, limit=%d)", sortBy, filter, status, limit)

		// Build base query
		query := `SELECT * FROM bins WHERE 1=1`
		args := []interface{}{}

		// Status filter
		if status != "all" {
			query += ` AND status = $1`
			args = append(args, status)
		}

		var bins []models.Bin
		if len(args) > 0 {
			if err := db.Select(&bins, query, args...); err != nil {
				log.Printf("❌ [GET-BINS-PRIORITY] Database query failed: %v", err)
				http.Error(w, "Failed to fetch bins", http.StatusInternalServerError)
				return
			}
		} else {
			if err := db.Select(&bins, query); err != nil {
				log.Printf("❌ [GET-BINS-PRIORITY] Database query failed: %v", err)
				http.Error(w, "Failed to fetch bins", http.StatusInternalServerError)
				return
			}
		}

		// Fetch all pending move requests
		var moveRequests []models.BinMoveRequest
		err := db.Select(&moveRequests, `
			SELECT * FROM bin_move_requests
			WHERE status IN ('pending', 'in_progress')
		`)
		if err != nil {
			log.Printf("⚠️  [GET-BINS-PRIORITY] Failed to fetch move requests: %v", err)
		}

		// Create map for quick lookup
		moveRequestMap := make(map[string]*models.BinMoveRequest)
		for i := range moveRequests {
			moveRequestMap[moveRequests[i].BinID] = &moveRequests[i]
		}

		// Fetch bins with pending check recommendations
		var checkRecBinIDs []string
		err = db.Select(&checkRecBinIDs, `
			SELECT DISTINCT bin_id FROM bin_check_recommendations
			WHERE status = 'pending'
		`)
		if err != nil {
			log.Printf("⚠️  [GET-BINS-PRIORITY] Failed to fetch check recommendations: %v", err)
		}

		checkRecMap := make(map[string]bool)
		for _, binID := range checkRecBinIDs {
			checkRecMap[binID] = true
		}

		// Calculate priorities and build response
		var binsWithPriority []BinWithPriority
		for _, bin := range bins {
			moveReq := moveRequestMap[bin.ID]
			hasCheckRec := checkRecMap[bin.ID]

			priority, daysSinceCheck := calculateBinPriority(bin, moveReq, hasCheckRec, now)

			binWithPriority := BinWithPriority{
				Bin:                    bin,
				PriorityScore:          priority,
				DaysSinceCheck:         daysSinceCheck,
				HasPendingMove:         moveReq != nil,
				HasCheckRecommendation: hasCheckRec,
			}

			if moveReq != nil {
				binWithPriority.NextMoveRequestDate = &moveReq.ScheduledDate
				binWithPriority.MoveRequestUrgency = &moveReq.Urgency
			}

			// Apply filter
			include := false
			switch filter {
			case "all":
				include = true
			case "next_move_request":
				include = moveReq != nil
			case "longest_unchecked":
				include = daysSinceCheck != nil && *daysSinceCheck >= 7
			case "high_fill":
				include = bin.FillPercentage != nil && *bin.FillPercentage >= 60
			case "has_check_recommendation":
				include = hasCheckRec
			default:
				include = true
			}

			if include {
				binsWithPriority = append(binsWithPriority, binWithPriority)
			}
		}

		// Sort based on sortBy parameter
		switch sortBy {
		case "priority":
			// Sort by priority score descending
			sort.Slice(binsWithPriority, func(i, j int) bool {
				return binsWithPriority[i].PriorityScore > binsWithPriority[j].PriorityScore
			})
		case "bin_number":
			// Sort by bin number ascending
			sort.Slice(binsWithPriority, func(i, j int) bool {
				return binsWithPriority[i].BinNumber < binsWithPriority[j].BinNumber
			})
		case "fill_percentage":
			// Sort by fill percentage descending (nulls last)
			sort.Slice(binsWithPriority, func(i, j int) bool {
				iFill := 0
				jFill := 0
				if binsWithPriority[i].FillPercentage != nil {
					iFill = *binsWithPriority[i].FillPercentage
				}
				if binsWithPriority[j].FillPercentage != nil {
					jFill = *binsWithPriority[j].FillPercentage
				}
				return iFill > jFill
			})
		case "days_since_check":
			// Sort by days since check descending (nulls first = never checked)
			sort.Slice(binsWithPriority, func(i, j int) bool {
				iDays := 999999 // Never checked = highest
				jDays := 999999
				if binsWithPriority[i].DaysSinceCheck != nil {
					iDays = *binsWithPriority[i].DaysSinceCheck
				}
				if binsWithPriority[j].DaysSinceCheck != nil {
					jDays = *binsWithPriority[j].DaysSinceCheck
				}
				return iDays > jDays
			})
		}

		// Apply limit
		if limit > 0 && len(binsWithPriority) > limit {
			binsWithPriority = binsWithPriority[:limit]
		}

		log.Printf("✅ [GET-BINS-PRIORITY] Returning %d bins", len(binsWithPriority))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(binsWithPriority)
	}
}

// RetireBin marks a bin as retired
// POST /api/manager/bins/{id}/retire
// Body: { "reason": "optional reason", "disposal_action": "retire|store" }
func RetireBin(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		binID := chi.URLParam(r, "id")
		if binID == "" {
			http.Error(w, "Bin ID is required", http.StatusBadRequest)
			return
		}

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		userID := userClaims.UserID

		var req struct {
			Reason         *string `json:"reason"`
			DisposalAction string  `json:"disposal_action"` // "retire" or "store"
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.DisposalAction != "retire" && req.DisposalAction != "store" {
			http.Error(w, "disposal_action must be 'retire' or 'store'", http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Determine new status
		newStatus := "retired"
		if req.DisposalAction == "store" {
			newStatus = "in_storage"
		}

		// Deactivating the bin (retire/store) and pruning it from every route template
		// must be atomic — a stored/retired bin can never remain on a collection route.
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [RETIRE-BIN] Failed to begin transaction: %v", err)
			http.Error(w, "Failed to retire bin", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		result, err := tx.Exec(`
			UPDATE bins
			SET status = $1,
			    retired_at = $2,
			    retired_by_user_id = $3,
			    updated_at = $2
			WHERE id = $4
			AND status = 'active'
		`, newStatus, now, userID, binID)
		if err != nil {
			log.Printf("❌ [RETIRE-BIN] Database update failed: %v", err)
			http.Error(w, "Failed to retire bin", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			http.Error(w, "Bin not found or already retired", http.StatusNotFound)
			return
		}

		// A retired/stored bin leaves the field — drop it from all route templates.
		if _, err = bindomain.PruneFromRouteTemplates(tx, binID, now); err != nil {
			log.Printf("❌ [RETIRE-BIN] Failed to prune bin %s from route templates: %v", binID, err)
			http.Error(w, "Failed to retire bin", http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("❌ [RETIRE-BIN] Failed to commit transaction: %v", err)
			http.Error(w, "Failed to retire bin", http.StatusInternalServerError)
			return
		}

		log.Printf("✅ [RETIRE-BIN] Bin %s retired by user %s (action: %s)", binID, userID, req.DisposalAction)

		// Write to bin_change_log for audit trail (best-effort, post-commit)
		oldJSON := `{"status":"active"}`
		newJSON := fmt.Sprintf(`{"status":"%s"}`, newStatus)
		var reasonNotes *string
		if req.Reason != nil {
			reasonNotes = req.Reason
		}
		db.Exec(`INSERT INTO bin_change_log (id, bin_id, changed_by_user_id, created_at, change_type, old_values, new_values, reason_category, reason_notes, no_go_zone_created, no_go_zone_id)
			VALUES ($1, $2, $3, $4, 'status_change', $5, $6, NULL, $7, false, NULL)`,
			uuid.New().String(), binID, userID, now, oldJSON, newJSON, reasonNotes)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Bin retired successfully",
			"status":  newStatus,
		})
	}
}

// ReactivateBin unretires a bin and sets its location
// POST /api/manager/bins/{id}/reactivate
func ReactivateBin(root *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := orgdb.From(r)
		binID := chi.URLParam(r, "id")
		if binID == "" {
			http.Error(w, "Bin ID is required", http.StatusBadRequest)
			return
		}

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			Destination string   `json:"destination"` // "warehouse", "previous", "custom"
			Latitude    *float64 `json:"latitude"`
			Longitude   *float64 `json:"longitude"`
			Street      *string  `json:"street"`
			City        *string  `json:"city"`
			Zip         *string  `json:"zip"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Destination != "warehouse" && req.Destination != "previous" && req.Destination != "custom" {
			http.Error(w, "destination must be 'warehouse', 'previous', or 'custom'", http.StatusBadRequest)
			return
		}

		if req.Destination == "custom" && (req.Latitude == nil || req.Longitude == nil || req.Street == nil) {
			http.Error(w, "custom destination requires latitude, longitude, and street", http.StatusBadRequest)
			return
		}

		// Get current bin state
		var bin struct {
			ID            string  `db:"id"`
			BinNumber     int     `db:"bin_number"`
			Status        string  `db:"status"`
			CurrentStreet string  `db:"current_street"`
			City          string  `db:"city"`
			Zip           string  `db:"zip"`
			Latitude      float64 `db:"latitude"`
			Longitude     float64 `db:"longitude"`
		}
		err := db.Get(&bin, `SELECT id, bin_number, status, current_street, city, zip, COALESCE(latitude,0) as latitude, COALESCE(longitude,0) as longitude FROM bins WHERE id = $1`, binID)
		if err != nil {
			http.Error(w, "Bin not found", http.StatusNotFound)
			return
		}
		if bin.Status != "retired" {
			http.Error(w, fmt.Sprintf("Bin is not retired (current status: %s)", bin.Status), http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()
		newStatus := "active"
		newStreet := bin.CurrentStreet
		newCity := bin.City
		newZip := bin.Zip
		newLat := bin.Latitude
		newLng := bin.Longitude

		switch req.Destination {
		case "warehouse":
			newStatus = "in_storage"
			// Get warehouse coords from config
			var warehouseJSON []byte
			if whErr := db.QueryRow(`SELECT value FROM config WHERE key = 'warehouse_location'`).Scan(&warehouseJSON); whErr == nil {
				var wh struct {
					Latitude  float64 `json:"latitude"`
					Longitude float64 `json:"longitude"`
					Address   string  `json:"address"`
				}
				if json.Unmarshal(warehouseJSON, &wh) == nil {
					newLat = wh.Latitude
					newLng = wh.Longitude
					newStreet = wh.Address
					newCity = "Hayward"
					newZip = ""
				}
			}
		case "previous":
			newStatus = "active"
			// Keep existing coords
		case "custom":
			newStatus = "active"
			newLat = *req.Latitude
			newLng = *req.Longitude
			newStreet = *req.Street
			if req.City != nil {
				newCity = *req.City
			}
			if req.Zip != nil {
				newZip = *req.Zip
			}
		}

		// Update bin
		_, err = db.Exec(`
			UPDATE bins SET
				status = $1, latitude = $2, longitude = $3,
				current_street = $4, city = $5, zip = $6,
				fill_percentage = 0, last_checked_at = NULL,
				retired_at = NULL, retired_by_user_id = NULL,
				updated_at = $7
			WHERE id = $8
		`, newStatus, newLat, newLng, newStreet, newCity, newZip, now, binID)
		if err != nil {
			log.Printf("❌ [REACTIVATE-BIN] Failed to update bin: %v", err)
			http.Error(w, "Failed to reactivate bin", http.StatusInternalServerError)
			return
		}

		// Write to bin_change_log
		oldJSON := fmt.Sprintf(`{"status":"retired","street":"%s","city":"%s"}`, bin.CurrentStreet, bin.City)
		newJSON := fmt.Sprintf(`{"status":"%s","street":"%s","city":"%s","destination":"%s"}`, newStatus, newStreet, newCity, req.Destination)
		db.Exec(`INSERT INTO bin_change_log (id, bin_id, changed_by_user_id, created_at, change_type, old_values, new_values, reason_category, reason_notes, no_go_zone_created, no_go_zone_id)
			VALUES ($1, $2, $3, $4, 'status_change', $5, $6, NULL, NULL, false, NULL)`,
			uuid.New().String(), binID, userClaims.UserID, now, oldJSON, newJSON)

		log.Printf("✅ [REACTIVATE-BIN] Bin #%d reactivated by %s → %s at %s", bin.BinNumber, userClaims.Email, newStatus, newStreet)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Bin reactivated successfully",
			"status":  newStatus,
			"street":  newStreet,
			"city":    newCity,
		})
	}
}
