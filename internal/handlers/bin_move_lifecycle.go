package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/websocket"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func CancelBinMoveRequest(store moverequest.Store, db *sqlx.DB, redisClient *redis.Client, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Fetch via the domain Store.
		moveRequest, err := store.ByID(id)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
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

		// Resolve manager name up front for the post-commit history log (a read).
		var managerName string
		if nErr := db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerID); nErr != nil {
			log.Printf("Warning: Failed to fetch manager name for history: %v", nErr)
			managerName = "Unknown Manager"
		}

		// Atomic core: cancel the move, revert the bin, and soft-delete THIS move's
		// incomplete tasks on its shift — all-or-nothing.
		//
		// Previously this ran on the bare pool and HARD-deleted route_tasks by
		// (shift_id, bin_id) BEFORE the scoped soft-delete. That was wrong twice over:
		// (a) it wiped EVERY task for that bin on the shift — e.g. an unrelated
		// collection of the same bin — not just the move's; and (b) it ran first, so
		// the audited soft-delete that followed matched zero rows and no audit trail
		// survived a cancel. Now there is a single, move-scoped, audited removal via
		// itinerary.RemoveByIDs, inside one transaction.
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("Error starting cancel transaction: %v", err)
			http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Mark cancelled (domain owns the transition).
		if err = moverequest.Cancel(tx, id, now); err != nil {
			log.Printf("Error cancelling move request: %v", err)
			http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
			return
		}

		// Revert bin status. Street moves flipped the bin to pending_move, so they
		// revert to active — but a redeployment's bin never left the warehouse:
		// cancelling one must leave it in_storage, not surface a warehoused bin as
		// active at its stale street coordinates.
		revertStatus := "active"
		if moveRequest.MoveType == "redeployment" {
			revertStatus = "in_storage"
		}
		if _, err = tx.Exec(`UPDATE bins SET status = $1, updated_at = $2 WHERE id = $3`, revertStatus, now, moveRequest.BinID); err != nil {
			log.Printf("Error reverting bin status on cancel: %v", err)
			http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
			return
		}

		// Soft-delete only THIS move's incomplete tasks on its shift (audited).
		if moveRequest.AssignedShiftID != nil {
			var taskIDs []string
			if selErr := tx.Select(&taskIDs, `SELECT id FROM route_tasks WHERE move_request_id = $1 AND shift_id = $2 AND is_completed = 0 AND is_deleted = false`, id, *moveRequest.AssignedShiftID); selErr != nil {
				log.Printf("Error selecting move tasks to remove on cancel: %v", selErr)
				http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
				return
			}
			if err = itinerary.RemoveByIDs(tx, taskIDs, managerID, "move_request_cancelled", now); err != nil {
				log.Printf("Error soft-deleting move tasks on cancel: %v", err)
				http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
				return
			}
			// Removed this move's tasks — recompute the shift's counts from route_tasks.
			if err = itinerary.RecomputeShiftCounts(tx, *moveRequest.AssignedShiftID, now); err != nil {
				log.Printf("Error recomputing shift counts on cancel: %v", err)
				http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
				return
			}
		}

		if err = tx.Commit(); err != nil {
			log.Printf("Error committing cancel transaction: %v", err)
			http.Error(w, "Failed to cancel move request", http.StatusInternalServerError)
			return
		}

		// ---- post-commit side effects (the cancel already succeeded; best-effort) ----

		reason := "Cancelled by manager"
		if logErr := moverequest.LogCancelled(db, id, managerID, managerName, "manager", moveRequest.Status, &reason); logErr != nil {
			log.Printf("Warning: Failed to log move request cancellation: %v", logErr)
		}

		if moveRequest.AssignedShiftID != nil {
			// Notify driver (WebSocket + Centrifugo route update).
			wsHub.BroadcastToUser(*moveRequest.AssignedShiftID, map[string]interface{}{
				"type":    "move_request_cancelled",
				"bin_id":  moveRequest.BinID,
				"message": "Move request cancelled by manager",
			})
			if notifyErr := NotifyDriverOfRouteUpdate(db, centrifugoClient, *moveRequest.AssignedShiftID, "move_request_cancelled", map[string]interface{}{
				"move_request_id": id,
				"reason":          "Move request was cancelled by manager",
			}); notifyErr != nil {
				log.Printf("⚠️  Failed to notify driver of route update: %v", notifyErr)
			}

			// Re-optimize the shift (manager-initiated → skip gates).
			if reoptErr := ReoptimizeActiveShift(db, redisClient, *moveRequest.AssignedShiftID, centrifugoClient, true); reoptErr != nil {
				log.Printf("⚠️  Failed to re-optimize shift after move request cancellation: %v", reoptErr)
			} else {
				log.Printf("✅ Re-optimized shift %s after move request cancellation", *moveRequest.AssignedShiftID)
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

func ManuallyCompleteMoveRequest(store moverequest.Store, db *sqlx.DB) http.HandlerFunc {
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

		// Fetch via the domain Store.
		moveRequest, err := store.ByID(id)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
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

		// Mark completed (domain owns the transition).
		if err = moverequest.Complete(db, moveRequest.ID, now); err != nil {
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
		err = moverequest.LogCompleted(db, moveRequest.ID, userID, managerName, "manager", moveRequest.Status)
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
