package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"

	"github.com/jmoiron/sqlx"
)

// CompleteWarehouseRun completes a whole warehouse reload run in one call — the
// batch twin of CompleteTask for warehouse loads. The driver taps "Complete"
// once at the warehouse and every warehouse_stop in the run (e.g. all 6 loads
// of a "Load 6 bins" card) is marked done in a single transaction, instead of
// six identical "Warehouse Check-In" dialogs at the same physical spot.
//
// Warehouse stops carry no evidence (no photo/fill/check record) and no move
// leg-order dependency, so this path is simple: complete the rows, recompute
// counts, broadcast the shift update. The rows persist individually (each tags
// the placement it feeds) — only the driver's gesture collapses, not the data.
//
// POST /api/driver/shift/complete-warehouse-run   body: { "task_ids": [...] }
func CompleteWarehouseRun(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var req struct {
			TaskIDs []string `json:"task_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if len(req.TaskIDs) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "task_ids is required")
			return
		}

		var shift models.Shift
		if err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1`, userClaims.UserID); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift")
			return
		}

		now := time.Now().Unix()
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [WAREHOUSE RUN] begin tx: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete warehouse run")
			return
		}
		defer tx.Rollback()

		// Scoped to warehouse_stop rows of THIS shift only — a caller can't
		// complete arbitrary tasks through this endpoint.
		n, err := itinerary.CompleteWarehouseRun(tx, shift.ID, req.TaskIDs, now)
		if err != nil {
			log.Printf("❌ [WAREHOUSE RUN] complete: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete warehouse run")
			return
		}
		if n == 0 {
			utils.RespondError(w, http.StatusBadRequest,
				"No matching warehouse stops to complete (already done, or not in this shift)")
			return
		}

		// Counts recompute on the SAME tx (warehouse stops are excluded from the
		// bins semantic, so this is a no-op for progress — but keeps the stored
		// stop counts consistent, matching every other completion path).
		if rerr := itinerary.RecomputeShiftCounts(tx, shift.ID, now); rerr != nil {
			log.Printf("❌ [WAREHOUSE RUN] recompute counts: %v", rerr)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift counts")
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("❌ [WAREHOUSE RUN] commit: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete warehouse run")
			return
		}
		log.Printf("✅ [WAREHOUSE RUN] Completed %d warehouse stop(s) for shift %s", n, shift.ID[:12])

		// Post-commit, best-effort side effects — mirror CompleteTask so the
		// manager map + driver panel refresh identically.
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)
		bins, berr := getShiftTasksWithDetails(db, shift.ID)
		if berr != nil {
			bins = []models.ShiftBinWithDetails{}
		}
		logicalTotal, logicalCompleted := calculateLogicalBinCounts(bins)

		updateData := map[string]interface{}{
			"id":             shift.ID,
			"status":         shift.Status,
			"completed_bins": logicalCompleted,
			"total_bins":     logicalTotal,
			"tasks":          bins,
		}
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": updateData,
		})
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": updateData,
			}); pubErr != nil {
				log.Printf("⚠️  [WAREHOUSE RUN] Centrifugo publish failed: %v", pubErr)
			}
		}

		pct := 0.0
		if logicalTotal > 0 {
			pct = float64(logicalCompleted) / float64(logicalTotal) * 100
		}
		utils.RespondJSON(w, http.StatusOK, models.CompleteBinResponse{
			CompletedBins:        logicalCompleted,
			TotalBins:            logicalTotal,
			CompletionPercentage: pct,
		})
	}
}

// SkipWarehouseRun skips a whole warehouse reload run in one call — the skip
// twin of CompleteWarehouseRun. A reload run is ONE physical stop rendered from
// N warehouse_stop rows (one per bin); the app's Skip button used the per-task
// skip, which burned one row at a time and re-surfaced an identical warehouse
// card — reading as a frozen task. One gesture now skips the whole run, with
// the driver's reason recorded on every row (same task_data shape as SkipTask).
//
// POST /api/driver/shift/skip-warehouse-run   body: { "task_ids": [...], "reason": "..." }
func SkipWarehouseRun(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		var req struct {
			TaskIDs []string `json:"task_ids"`
			Reason  string   `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
		if len(req.TaskIDs) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "task_ids is required")
			return
		}

		var shift models.Shift
		if err := db.Get(&shift, `SELECT * FROM shifts WHERE driver_id = $1 AND status = 'active' ORDER BY created_at DESC LIMIT 1`, userClaims.UserID); err != nil {
			utils.RespondError(w, http.StatusBadRequest, "No active shift")
			return
		}

		skipData, err := json.Marshal(map[string]string{"skip_reason": req.Reason})
		if err != nil {
			utils.RespondError(w, http.StatusBadRequest, "Invalid skip reason")
			return
		}

		now := time.Now().Unix()
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ [WAREHOUSE RUN SKIP] begin tx: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to skip warehouse run")
			return
		}
		defer tx.Rollback()

		n, err := itinerary.SkipWarehouseRun(tx, shift.ID, req.TaskIDs, skipData, now)
		if err != nil {
			log.Printf("❌ [WAREHOUSE RUN SKIP] skip: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to skip warehouse run")
			return
		}
		if n == 0 {
			utils.RespondError(w, http.StatusBadRequest,
				"No matching warehouse stops to skip (already processed, or not in this shift)")
			return
		}

		// Skips count as processed, so the stored counts move — same tx.
		if rerr := itinerary.RecomputeShiftCounts(tx, shift.ID, now); rerr != nil {
			log.Printf("❌ [WAREHOUSE RUN SKIP] recompute counts: %v", rerr)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift counts")
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("❌ [WAREHOUSE RUN SKIP] commit: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to skip warehouse run")
			return
		}
		log.Printf("⏭️  [WAREHOUSE RUN SKIP] Skipped %d warehouse stop(s) for shift %s (reason: %s)", n, shift.ID[:12], req.Reason)

		// Post-commit, best-effort side effects — identical to the complete twin.
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shift.ID)
		bins, berr := getShiftTasksWithDetails(db, shift.ID)
		if berr != nil {
			bins = []models.ShiftBinWithDetails{}
		}
		logicalTotal, logicalCompleted := calculateLogicalBinCounts(bins)

		updateData := map[string]interface{}{
			"id":             shift.ID,
			"status":         shift.Status,
			"completed_bins": logicalCompleted,
			"total_bins":     logicalTotal,
			"tasks":          bins,
		}
		hub.BroadcastToUser(userClaims.UserID, map[string]interface{}{
			"type": "shift_update",
			"data": updateData,
		})
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
				"type": "shift_update",
				"data": updateData,
			}); pubErr != nil {
				log.Printf("⚠️  [WAREHOUSE RUN SKIP] Centrifugo publish failed: %v", pubErr)
			}
		}

		pct := 0.0
		if logicalTotal > 0 {
			pct = float64(logicalCompleted) / float64(logicalTotal) * 100
		}
		utils.RespondJSON(w, http.StatusOK, models.CompleteBinResponse{
			CompletedBins:        logicalCompleted,
			TotalBins:            logicalTotal,
			CompletionPercentage: pct,
		})
	}
}
