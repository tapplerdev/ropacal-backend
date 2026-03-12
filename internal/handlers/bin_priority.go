package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"ropacal-backend/internal/models"

	"github.com/go-chi/chi/v5"
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
func GetBinsWithPriority(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			for i := 0; i < len(binsWithPriority); i++ {
				for j := i + 1; j < len(binsWithPriority); j++ {
					if binsWithPriority[i].PriorityScore < binsWithPriority[j].PriorityScore {
						binsWithPriority[i], binsWithPriority[j] = binsWithPriority[j], binsWithPriority[i]
					}
				}
			}
		case "bin_number":
			// Sort by bin number ascending
			for i := 0; i < len(binsWithPriority); i++ {
				for j := i + 1; j < len(binsWithPriority); j++ {
					if binsWithPriority[i].BinNumber > binsWithPriority[j].BinNumber {
						binsWithPriority[i], binsWithPriority[j] = binsWithPriority[j], binsWithPriority[i]
					}
				}
			}
		case "fill_percentage":
			// Sort by fill percentage descending (nulls last)
			for i := 0; i < len(binsWithPriority); i++ {
				for j := i + 1; j < len(binsWithPriority); j++ {
					iFill := 0
					jFill := 0
					if binsWithPriority[i].FillPercentage != nil {
						iFill = *binsWithPriority[i].FillPercentage
					}
					if binsWithPriority[j].FillPercentage != nil {
						jFill = *binsWithPriority[j].FillPercentage
					}
					if iFill < jFill {
						binsWithPriority[i], binsWithPriority[j] = binsWithPriority[j], binsWithPriority[i]
					}
				}
			}
		case "days_since_check":
			// Sort by days since check descending (nulls first = never checked)
			for i := 0; i < len(binsWithPriority); i++ {
				for j := i + 1; j < len(binsWithPriority); j++ {
					iDays := 0
					jDays := 0
					if binsWithPriority[i].DaysSinceCheck != nil {
						iDays = *binsWithPriority[i].DaysSinceCheck
					} else {
						iDays = 999999 // Never checked = highest
					}
					if binsWithPriority[j].DaysSinceCheck != nil {
						jDays = *binsWithPriority[j].DaysSinceCheck
					} else {
						jDays = 999999
					}
					if iDays < jDays {
						binsWithPriority[i], binsWithPriority[j] = binsWithPriority[j], binsWithPriority[i]
					}
				}
			}
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
func RetireBin(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binID := chi.URLParam(r, "id")
		if binID == "" {
			http.Error(w, "Bin ID is required", http.StatusBadRequest)
			return
		}

		userID, _ := r.Context().Value("user_id").(string)

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

		// Update bin
		result, err := db.Exec(`
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

		log.Printf("✅ [RETIRE-BIN] Bin %s retired by user %s (action: %s)", binID, userID, req.DisposalAction)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": "Bin retired successfully",
			"status":  newStatus,
		})
	}
}
