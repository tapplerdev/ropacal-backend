package handlers

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/database"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/pkg/utils"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func RemoveTasksFromShift(db *sqlx.DB, redisClient *redis.Client, centrifugoClient *centrifugo.Client, fcmService *services.FCMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/manager/shifts/:shift_id/tasks/remove")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "shift_id")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "Shift ID is required")
			return
		}

		var req struct {
			TaskIDs []string `json:"task_ids"`
			Reason  string   `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		if len(req.TaskIDs) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "At least one task ID is required")
			return
		}

		log.Printf("   Shift ID: %s", shiftID)
		log.Printf("   Task IDs: %v", req.TaskIDs)
		log.Printf("   Reason: %s", req.Reason)
		log.Printf("   Manager: %s (%s)", userClaims.Email, userClaims.UserID)

		// Get shift details
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err == sql.ErrNoRows {
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift")
			return
		}

		// Only allow removing tasks from ready or active shifts
		if shift.Status != "active" && shift.Status != "ready" {
			utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Can only remove tasks from ready or active shifts (current status: %s)", shift.Status))
			return
		}

		log.Printf("✅ Found shift (status: %s) for driver %s", shift.Status, shift.DriverID)

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		now := time.Now().Unix()
		removedCount := 0
		var removedTasks []models.RouteTask

		// Process each task
		for _, taskID := range req.TaskIDs {
			// Decode base64 task ID to UUID
			decodedBytes, decodeErr := base64.StdEncoding.DecodeString(taskID)
			actualTaskID := taskID // Default to original if decode fails
			if decodeErr == nil {
				actualTaskID = string(decodedBytes)
				log.Printf("🔓 Decoded task ID: %s -> %s", taskID, actualTaskID)
			} else {
				log.Printf("⚠️  Task ID %s is not base64, using as-is", taskID)
			}

			// Get task details
			var task models.RouteTask
			err = tx.Get(&task, `SELECT * FROM route_tasks WHERE id = $1 AND shift_id = $2`, actualTaskID, shiftID)
			if err == sql.ErrNoRows {
				log.Printf("⚠️  Task %s not found in shift %s, skipping", taskID, shiftID)
				continue
			}
			if err != nil {
				log.Printf("❌ Error fetching task %s: %v", taskID, err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch task")
				return
			}

			// Don't remove already completed tasks
			if task.IsCompleted == 1 {
				log.Printf("⚠️  Task %s already completed, skipping", taskID)
				continue
			}

			// Block removing a dropoff if its paired pickup is completed (bin is on the truck)
			if task.TaskType == "dropoff" && task.MoveRequestID != nil {
				var pickupCompleted int
				pickupErr := tx.Get(&pickupCompleted, `
					SELECT COALESCE(MAX(is_completed), 0) FROM route_tasks
					WHERE shift_id = $1 AND move_request_id = $2 AND task_type = 'pickup' AND is_deleted = false
				`, shiftID, *task.MoveRequestID)
				if pickupErr == nil && pickupCompleted == 1 {
					log.Printf("⚠️  Cannot remove dropoff %s — paired pickup is completed (bin on truck)", taskID)
					continue
				}
			}

			log.Printf("🗑️  Removing task: ID=%s, Type=%s, Seq=%d", task.ID, task.TaskType, task.SequenceOrder)

			// Mark task as deleted (audited soft-delete — domain owns the removal).
			if err = itinerary.RemoveByIDs(tx, []string{actualTaskID}, userClaims.UserID, req.Reason, now); err != nil {
				log.Printf("❌ Error marking task as deleted: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to remove task")
				return
			}

			// Unassign underlying resources
			switch task.TaskType {
			case models.TaskTypePickup, models.TaskTypeDropoff, models.TaskTypePlacement:
				// A redeployment rides the placement rails (Phase 2): its single
				// placement task carries move_request_id and releases like a pair
				// leg. A potential-location placement instead unassigns its
				// location below — the nil-guards pick the right arm.
				// Detach the move from the shift and return it to that shift
				// driver's backlog (status='assigned', assignment_type='manual',
				// assigned_user_id = the shift's driver), matching the shift-level
				// release rule in releaseShiftMoveRequests. (Previously this left
				// status='in_progress' with no shift, orphaning the move.) Use the
				// clear-assignment endpoint to drop it all the way to the pool.
				if task.MoveRequestID != nil {
					log.Printf("   Releasing move request %s to driver backlog", *task.MoveRequestID)

					// Capture assignment details before clearing them (for history).
					var mrDetails struct {
						AssignmentType  *string `db:"assignment_type"`
						AssignedUserID  *string `db:"assigned_user_id"`
						AssignedShiftID *string `db:"assigned_shift_id"`
					}
					_ = tx.Get(&mrDetails, `SELECT assignment_type, assigned_user_id, assigned_shift_id FROM bin_move_requests WHERE id = $1`, *task.MoveRequestID)
					var assignedUserName *string
					if mrDetails.AssignedUserID != nil {
						var name string
						if e := tx.Get(&name, `SELECT name FROM users WHERE id = $1`, *mrDetails.AssignedUserID); e == nil {
							assignedUserName = &name
						}
					}

					_, err = tx.Exec(`
						UPDATE bin_move_requests AS mr
						SET assigned_shift_id = NULL,
							assigned_user_id   = s.driver_id,
							assignment_type    = 'manual',
							status             = 'assigned',
							updated_at         = $1
						FROM shifts s
						WHERE mr.id = $2 AND s.id = $3
					`, now, *task.MoveRequestID, shiftID)
					if err != nil {
						log.Printf("❌ Error releasing move request: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to release move request")
						return
					}

					if logErr := moverequest.LogUnassigned(tx, *task.MoveRequestID, userClaims.UserID, userClaims.Email, "manager", mrDetails.AssignmentType, mrDetails.AssignedUserID, assignedUserName, mrDetails.AssignedShiftID); logErr != nil {
						log.Printf("⚠️  Failed to log move request unassignment: %v", logErr)
					}
				}

				// Unassign potential location (potential-location placements only —
				// nil on a redeployment placement, whose move was released above).
				if task.PotentialLocationID != nil {
					log.Printf("   Unassigning potential location %s", *task.PotentialLocationID)
					_, err = tx.Exec(`
						UPDATE potential_locations
						SET assigned_shift_id = NULL,
							updated_at = $1
						WHERE id = $2
					`, now, *task.PotentialLocationID)
					if err != nil {
						log.Printf("❌ Error unassigning potential location: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign potential location")
						return
					}
				}

			case models.TaskTypeCollection:
				// For collection tasks, just mark as removed
				// The bin itself stays in the system
				binID := "unknown"
				if task.BinID != nil {
					binID = *task.BinID
				}
				log.Printf("   Collection task removed (bin %s remains in system)", binID)
			}

			removedTasks = append(removedTasks, task)
			removedCount++
		}

		if removedCount == 0 {
			utils.RespondError(w, http.StatusBadRequest, "No valid tasks to remove")
			return
		}

		log.Printf("✅ Marked %d tasks as removed", removedCount)

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit changes")
			return
		}

		log.Printf("✅ Transaction committed")

		// Refresh shift data
		err = db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err != nil {
			log.Printf("⚠️  Error refreshing shift: %v", err)
		}

		// Get updated task list
		tasks, err := database.GetShiftTasks(db, shiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v", err)
			tasks = []models.RouteTask{}
		}

		// Auto-cancel shift if no active tasks remain
		activeTasks := 0
		for _, t := range tasks {
			if !t.IsDeleted {
				activeTasks++
			}
		}
		if activeTasks == 0 {
			log.Printf("🚫 All tasks removed from shift %s — auto-cancelling", shiftID)
			cancelNow := time.Now().Unix()

			// Archive to shift_history first
			var optMetaBytes []byte
			db.Get(&optMetaBytes, `SELECT optimization_metadata FROM shifts WHERE id = $1`, shiftID)
			var optMeta interface{}
			if len(optMetaBytes) > 0 {
				raw := json.RawMessage(optMetaBytes)
				optMeta = &raw
			}

			_, histErr := archiveShift(db, archiveShiftParams{
				ID:                  shift.ID,
				DriverID:            shift.DriverID,
				RouteID:             shift.RouteID,
				StartTime:           shift.StartTime,
				EndTime:             cancelNow,
				CreatedAt:           shift.CreatedAt,
				EndedAt:             cancelNow,
				TotalPauseSeconds:   shift.TotalPauseSeconds,
				TotalBins:           shift.TotalBins,
				CompletedBins:       shift.CompletedBins,
				IncidentsReported:   0, // incidents
				FieldObservations:   0, // field observations
				EndReason:           "manager_cancelled",
				EndedByUserID:       userClaims.UserID,
				EndReasonMetadata:   nil,
				OptMeta:             optMeta,
				OnConflictDoNothing: true,
			})
			if histErr != nil {
				log.Printf("⚠️  Failed to archive cancelled shift: %v", histErr)
			}

			// Update shift status (no end_reason on shifts table)
			_, cancelErr := db.Exec(`
				UPDATE shifts SET status = 'cancelled', end_time = $1, pause_start_time = NULL, updated_at = $1
				WHERE id = $2
			`, cancelNow, shiftID)
			if cancelErr != nil {
				log.Printf("⚠️  Failed to auto-cancel empty shift: %v", cancelErr)
			} else {
				shift.Status = "cancelled"
				log.Printf("✅ Shift %s auto-cancelled and archived to history (0 active tasks)", shiftID)
			}
		}

		// Publish Centrifugo event to driver's shift channel
		if centrifugoClient != nil {
			eventData := map[string]interface{}{
				"type":          "task_removed",
				"shift_id":      shiftID,
				"removed_count": removedCount,
				"reason":        req.Reason,
				"manager_name":  userClaims.Email,
			}

			channel := fmt.Sprintf("shift:updates:%s", shiftID)
			if pubErr := centrifugoClient.PublishToChannel(r.Context(), channel, eventData); pubErr != nil {
				log.Printf("⚠️  Failed to publish task removal to Centrifugo: %v", pubErr)
			} else {
				log.Printf("📡 Published task_removed event to %s", channel)
			}
		}

		// Send FCM push notification to driver (in case WebSocket isn't connected)
		if fcmService != nil && shift.Status == "active" {
			fcmData := map[string]string{
				"tasks_removed": fmt.Sprintf("%d", removedCount),
			}
			title, body := services.ShiftNotificationText("task_removed", fcmData)
			_, notifIDs := services.CreateNotificationForUsers(db, []string{shift.DriverID}, "task_removed", title, body, map[string]string{"shift_id": shiftID})
			if len(notifIDs) > 0 {
				var driverToken models.FCMToken
				if tokenErr := db.Get(&driverToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, shift.DriverID); tokenErr == nil {
					go fcmService.SendShiftUpdateNotification(driverToken.Token, shiftID, "task_removed", fcmData)
					log.Printf("📱 Sent FCM task_removed notification to driver %s", shift.DriverID)
				}
			}
		}

		// Re-optimize the shift after removing tasks (only if shift is active and has remaining tasks)
		if shift.Status == "active" && activeTasks > 0 {
			if err := ReoptimizeActiveShift(db, redisClient, shiftID, centrifugoClient, true); err != nil {
				log.Printf("⚠️  Failed to re-optimize shift after task removal: %v", err)
			} else {
				log.Printf("✅ Successfully re-optimized shift %s after removing %d tasks", shiftID, removedCount)
			}
		}

		// Return success response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":       true,
			"removed_count": removedCount,
			"message":       fmt.Sprintf("%d task(s) removed from shift successfully", removedCount),
			"shift": map[string]interface{}{
				"id":     shift.ID,
				"status": shift.Status,
				"tasks":  tasks,
			},
		})
	}
}

// UpdateShift comprehensively updates a shift (time, driver, add/remove tasks)
// PATCH /api/manager/shifts/:id
func UpdateShift(db *sqlx.DB, redisClient *redis.Client, centrifugoClient *centrifugo.Client, fcmService *services.FCMService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: PATCH /api/manager/shifts/:id")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "id")
		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "Shift ID is required")
			return
		}

		// Request structure
		var req struct {
			// Shift details (optional - only update if provided)
			StartTime *int64  `json:"start_time,omitempty"`
			EndTime   *int64  `json:"end_time,omitempty"`
			DriverID  *string `json:"driver_id,omitempty"`
			RouteID   *string `json:"route_id,omitempty"`

			// Task modifications
			AddTasks      []AddTaskRequest `json:"add_tasks,omitempty"`
			RemoveTaskIDs []string         `json:"remove_task_ids,omitempty"`

			// Flags
			Reoptimize bool   `json:"reoptimize"` // Default: false (will be set to true if tasks change)
			Reason     string `json:"reason"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Error decoding request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("   Shift ID: %s", shiftID)
		log.Printf("   Manager: %s (%s)", userClaims.Email, userClaims.UserID)
		log.Printf("   Changes requested:")
		if req.StartTime != nil {
			log.Printf("      - Start time: %d", *req.StartTime)
		}
		if req.EndTime != nil {
			log.Printf("      - End time: %d", *req.EndTime)
		}
		if req.DriverID != nil {
			log.Printf("      - Driver ID: %s", *req.DriverID)
		}
		if req.RouteID != nil {
			log.Printf("      - Route ID: %s", *req.RouteID)
		}
		log.Printf("      - Tasks to add: %d", len(req.AddTasks))
		log.Printf("      - Tasks to remove: %d", len(req.RemoveTaskIDs))
		log.Printf("      - Reason: %s", req.Reason)

		// Debug: Log each task being added
		for i, task := range req.AddTasks {
			log.Printf("      [Task %d] Type: %s", i+1, task.TaskType)
			if task.BinID != nil {
				log.Printf("      [Task %d]   Bin ID: %s", i+1, *task.BinID)
			}
			if task.PotentialLocationID != nil {
				log.Printf("      [Task %d]   Potential Location ID: %s", i+1, *task.PotentialLocationID)
			}
		}

		// Get current shift
		var shift models.Shift
		err := db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err == sql.ErrNoRows {
			utils.RespondError(w, http.StatusNotFound, "Shift not found")
			return
		}
		if err != nil {
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift")
			return
		}

		// Only allow editing ready or active shifts
		if shift.Status != "ready" && shift.Status != "active" {
			utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Can only edit ready or active shifts (current status: %s)", shift.Status))
			return
		}

		log.Printf("✅ Found shift (status: %s) for driver %s", shift.Status, shift.DriverID)

		// Track changes for event payload
		changes := make(map[string]interface{})
		changes["tasks_added"] = 0
		changes["tasks_removed"] = 0
		changes["driver_changed"] = false
		changes["time_changed"] = false
		changes["route_reoptimized"] = false

		oldDriverID := shift.DriverID

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		now := time.Now().Unix()

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 1: Update shift details (time, driver, route)
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		updateFields := []string{"updated_at = $1"}
		updateArgs := []interface{}{now}
		argIndex := 2

		if req.StartTime != nil {
			updateFields = append(updateFields, fmt.Sprintf("start_time = $%d", argIndex))
			updateArgs = append(updateArgs, *req.StartTime)
			argIndex++
			changes["time_changed"] = true
		}

		if req.EndTime != nil {
			updateFields = append(updateFields, fmt.Sprintf("end_time = $%d", argIndex))
			updateArgs = append(updateArgs, *req.EndTime)
			argIndex++
			changes["time_changed"] = true
		}

		if req.DriverID != nil && *req.DriverID != shift.DriverID {
			// Block reassignment if no real incomplete tasks remain
			var realIncompleteTasks int
			tx.Get(&realIncompleteTasks, `
				SELECT COUNT(*) FROM route_tasks
				WHERE shift_id = $1 AND is_deleted = false AND is_completed = 0
				  AND COALESCE(skipped, false) = false AND task_type != 'warehouse_stop'
			`, shiftID)
			if realIncompleteTasks == 0 {
				log.Printf("⚠️  No real incomplete tasks to reassign — blocking driver change")
				utils.RespondError(w, http.StatusBadRequest, "No tasks to reassign — all tasks are completed or only a warehouse stop remains")
				return
			}

			updateFields = append(updateFields, fmt.Sprintf("driver_id = $%d", argIndex))
			updateArgs = append(updateArgs, *req.DriverID)
			argIndex++
			changes["driver_changed"] = true
			log.Printf("🔄 Driver reassignment: %s → %s", oldDriverID, *req.DriverID)

			// If active shift is being reassigned, reset to ready + remove completed tasks
			if shift.Status == "active" || shift.Status == "paused" {
				log.Printf("🔄 Active shift reassignment — resetting to 'ready', removing completed tasks")
				updateFields = append(updateFields, fmt.Sprintf("status = $%d", argIndex))
				updateArgs = append(updateArgs, "ready")
				argIndex++
				updateFields = append(updateFields, fmt.Sprintf("start_time = $%d", argIndex))
				updateArgs = append(updateArgs, nil)
				argIndex++
				updateFields = append(updateFields, fmt.Sprintf("pause_start_time = $%d", argIndex))
				updateArgs = append(updateArgs, nil)
				argIndex++
				updateFields = append(updateFields, fmt.Sprintf("ready_to_end_at = $%d", argIndex))
				updateArgs = append(updateArgs, nil)
				argIndex++

				// Soft-delete completed and skipped tasks — new driver only gets remaining
				// work. Select-then-remove keeps the reassign-specific predicate here and
				// routes the audited deletion through the itinerary primitive (which also
				// stamps deleted_by, unlike the original).
				var doneTaskIDs []string
				if selErr := tx.Select(&doneTaskIDs, `
					SELECT id FROM route_tasks
					WHERE shift_id = $1 AND is_deleted = false AND (is_completed = 1 OR skipped = true)
				`, shiftID); selErr != nil {
					log.Printf("⚠️  Failed to select completed tasks on reassign: %v", selErr)
				}
				if delErr := itinerary.RemoveByIDs(tx, doneTaskIDs, userClaims.UserID, "completed_before_reassign", now); delErr != nil {
					log.Printf("⚠️  Failed to remove completed tasks on reassign: %v", delErr)
				} else if len(doneTaskIDs) > 0 {
					log.Printf("🗑️  Removed %d completed/skipped tasks for reassignment", len(doneTaskIDs))
					changes["completed_tasks_removed"] = len(doneTaskIDs)
				}
			}
		}

		if req.RouteID != nil {
			updateFields = append(updateFields, fmt.Sprintf("route_id = $%d", argIndex))
			updateArgs = append(updateArgs, *req.RouteID)
			argIndex++
		}

		if len(updateFields) > 1 { // More than just updated_at
			updateArgs = append(updateArgs, shiftID)
			query := fmt.Sprintf("UPDATE shifts SET %s WHERE id = $%d", strings.Join(updateFields, ", "), argIndex)
			_, err = tx.Exec(query, updateArgs...)
			if err != nil {
				log.Printf("❌ Error updating shift: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to update shift")
				return
			}
			log.Printf("✅ Updated shift details")
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 2: Remove tasks (if any)
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		removedCount := 0
		if len(req.RemoveTaskIDs) > 0 {
			log.Printf("🗑️  Removing %d tasks...", len(req.RemoveTaskIDs))

			// VALIDATION: Ensure pickup/dropoff pairs are removed together
			// Build set of task IDs being removed for quick lookup
			removeSet := make(map[string]bool)
			decodedTaskIDs := make(map[string]string) // taskID -> actualTaskID mapping
			for _, taskID := range req.RemoveTaskIDs {
				// Decode base64 task ID to UUID
				decodedBytes, decodeErr := base64.StdEncoding.DecodeString(taskID)
				actualTaskID := taskID
				if decodeErr == nil {
					actualTaskID = string(decodedBytes)
				}
				removeSet[actualTaskID] = true
				decodedTaskIDs[taskID] = actualTaskID
			}

			// Check each task being removed - if it's a pickup/dropoff, ensure paired task is also being removed
			for taskID, actualTaskID := range decodedTaskIDs {
				var task models.RouteTask
				err = tx.Get(&task, `SELECT * FROM route_tasks WHERE id = $1 AND shift_id = $2`, actualTaskID, shiftID)
				if err == sql.ErrNoRows {
					continue
				}
				if err != nil {
					log.Printf("❌ Error validating task %s: %v", taskID, err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to validate task removal")
					return
				}

				// Only validate pickup/dropoff tasks
				if (task.TaskType == models.TaskTypePickup || task.TaskType == models.TaskTypeDropoff) && task.MoveRequestID != nil {
					// Find paired tasks (other tasks for the same move request in this shift)
					var pairedTasks []models.RouteTask
					err = tx.Select(&pairedTasks, `
						SELECT * FROM route_tasks
						WHERE shift_id = $1
						  AND move_request_id = $2
						  AND id != $3
						  AND is_deleted = FALSE
					`, shiftID, *task.MoveRequestID, actualTaskID)
					if err != nil && err != sql.ErrNoRows {
						log.Printf("❌ Error finding paired tasks: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to validate task removal")
						return
					}

					// Check if any paired task is NOT being removed
					for _, paired := range pairedTasks {
						if !removeSet[paired.ID] {
							log.Printf("⚠️  Cannot remove %s without also removing paired %s (move_request_id=%s)",
								task.TaskType, paired.TaskType, *task.MoveRequestID)
							utils.RespondError(w, http.StatusBadRequest,
								fmt.Sprintf("Must remove both pickup and dropoff tasks together for move request. Please select both tasks and try again."))
							return
						}
					}
				}
			}

			log.Printf("✅ Validation passed - all pickup/dropoff pairs will be removed together")

			// Track which move requests have been logged (to avoid duplicate history entries)
			loggedMoveRequests := make(map[string]bool)

			for _, taskID := range req.RemoveTaskIDs {
				// Decode base64 task ID to UUID
				decodedBytes, decodeErr := base64.StdEncoding.DecodeString(taskID)
				actualTaskID := taskID
				if decodeErr == nil {
					actualTaskID = string(decodedBytes)
				}

				// Get task details
				var task models.RouteTask
				err = tx.Get(&task, `SELECT * FROM route_tasks WHERE id = $1 AND shift_id = $2`, actualTaskID, shiftID)
				if err == sql.ErrNoRows {
					log.Printf("⚠️  Task %s not found, skipping", taskID)
					continue
				}
				if err != nil {
					log.Printf("❌ Error fetching task: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch task")
					return
				}

				// Don't remove completed tasks
				if task.IsCompleted == 1 {
					log.Printf("⚠️  Task %s already completed, skipping", taskID)
					continue
				}

				log.Printf("   Removing task: %s (type=%s)", task.ID, task.TaskType)

				// Mark as deleted (audited soft-delete via the itinerary primitive)
				if err = itinerary.RemoveByIDs(tx, []string{actualTaskID}, userClaims.UserID, req.Reason, now); err != nil {
					log.Printf("❌ Error marking task as deleted: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to remove task")
					return
				}

				// Unassign resources based on task type. A redeployment placement
				// (Phase 2: one placement task carrying move_request_id) unassigns
				// its move like a pair leg; a potential-location placement
				// unassigns its location — the nil-guards pick the right arm.
				switch task.TaskType {
				case models.TaskTypePickup, models.TaskTypeDropoff, models.TaskTypePlacement:
					if task.MoveRequestID != nil && !loggedMoveRequests[*task.MoveRequestID] {
						// Get move request assignment details BEFORE unassigning (for history logging)
						var moveReq struct {
							AssignmentType  *string `db:"assignment_type"`
							AssignedUserID  *string `db:"assigned_user_id"`
							AssignedShiftID *string `db:"assigned_shift_id"`
						}
						err = tx.Get(&moveReq, `SELECT assignment_type, assigned_user_id, assigned_shift_id FROM bin_move_requests WHERE id = $1`, *task.MoveRequestID)
						if err != nil {
							log.Printf("❌ Error fetching move request for history: %v", err)
							utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch move request")
							return
						}

						// Get assigned user name if exists (for history log)
						var assignedUserName *string
						if moveReq.AssignedUserID != nil {
							var userName string
							err = tx.Get(&userName, `SELECT name FROM users WHERE id = $1`, *moveReq.AssignedUserID)
							if err == nil {
								assignedUserName = &userName
							}
						}

						// Unassign the move request
						_, err = tx.Exec(`
							UPDATE bin_move_requests
							SET assigned_shift_id = NULL,
								assigned_user_id = NULL,
								status = 'pending',
								updated_at = $1
							WHERE id = $2
						`, now, *task.MoveRequestID)
						if err != nil {
							log.Printf("❌ Error unassigning move request: %v", err)
							utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign move request")
							return
						}
						log.Printf("✅ Unassigned move request %s (status → pending)", *task.MoveRequestID)

						// Log history entry
						err = moverequest.LogUnassigned(
							tx,
							*task.MoveRequestID,
							userClaims.UserID,
							userClaims.Email,
							"manager",
							moveReq.AssignmentType,
							moveReq.AssignedUserID,
							assignedUserName,
							moveReq.AssignedShiftID,
						)
						if err != nil {
							log.Printf("⚠️  Failed to log move request history: %v", err)
							// Don't fail the entire operation, just log the error
						} else {
							log.Printf("📝 Logged unassignment in move request history")
						}

						// Mark as logged to avoid duplicate entries
						loggedMoveRequests[*task.MoveRequestID] = true
					}
					if task.PotentialLocationID != nil {
						_, err = tx.Exec(`
							UPDATE potential_locations
							SET assigned_shift_id = NULL,
								updated_at = $1
							WHERE id = $2
						`, now, *task.PotentialLocationID)
						if err != nil {
							log.Printf("❌ Error unassigning potential location: %v", err)
							utils.RespondError(w, http.StatusInternalServerError, "Failed to unassign potential location")
							return
						}
					}
				}

				removedCount++
			}

			changes["tasks_removed"] = removedCount
			log.Printf("✅ Removed %d tasks", removedCount)
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 3: Add new tasks (if any)
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		addedCount := 0
		if len(req.AddTasks) > 0 {
			log.Printf("➕ Adding %d tasks...", len(req.AddTasks))

			// Build dedup sets from existing tasks to prevent duplicates on merge
			existingBinIDs := make(map[string]bool)
			existingMoveRequestIDs := make(map[string]bool)
			existingPotentialLocationIDs := make(map[string]bool)
			var existingTasks []struct {
				BinID               *string `db:"bin_id"`
				MoveRequestID       *string `db:"move_request_id"`
				PotentialLocationID *string `db:"potential_location_id"`
			}
			tx.Select(&existingTasks, `SELECT bin_id, move_request_id, potential_location_id FROM route_tasks WHERE shift_id = $1 AND is_deleted = false`, shiftID)
			for _, et := range existingTasks {
				if et.BinID != nil {
					existingBinIDs[*et.BinID] = true
				}
				if et.MoveRequestID != nil {
					existingMoveRequestIDs[*et.MoveRequestID] = true
				}
				if et.PotentialLocationID != nil {
					existingPotentialLocationIDs[*et.PotentialLocationID] = true
				}
			}

			skippedCount := 0
			loggedAssignments := make(map[string]bool) // one history event per move per request
			for _, addReq := range req.AddTasks {
				// Validate task_type at the boundary — a clean 400 instead of a 500
				// at INSERT time (the DB CHECK). The itinerary domain owns the taxonomy.
				if _, err := itinerary.ParseTaskType(addReq.TaskType); err != nil {
					utils.RespondError(w, http.StatusBadRequest, err.Error())
					return
				}
				// Dedup check: skip if this task already exists on the shift
				if addReq.TaskType == "collection" && addReq.BinID != nil && existingBinIDs[*addReq.BinID] {
					log.Printf("   ⏭️  Skipping duplicate collection for bin %s", *addReq.BinID)
					skippedCount++
					continue
				}
				if (addReq.TaskType == "pickup" || addReq.TaskType == "dropoff") && addReq.MoveRequestID != nil && existingMoveRequestIDs[*addReq.MoveRequestID] {
					log.Printf("   ⏭️  Skipping duplicate %s for move request %s", addReq.TaskType, *addReq.MoveRequestID)
					skippedCount++
					continue
				}
				if addReq.TaskType == "placement" && addReq.PotentialLocationID != nil && existingPotentialLocationIDs[*addReq.PotentialLocationID] {
					log.Printf("   ⏭️  Skipping duplicate placement for location %s", *addReq.PotentialLocationID)
					skippedCount++
					continue
				}

				log.Printf("   Creating task: type=%s", addReq.TaskType)

				// Get next sequence order
				var maxSeq sql.NullInt64
				err = tx.Get(&maxSeq, `
					SELECT MAX(sequence_order)
					FROM route_tasks
					WHERE shift_id = $1 AND is_deleted = false
				`, shiftID)
				if err != nil && err != sql.ErrNoRows {
					log.Printf("❌ Error getting max sequence: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to get sequence order")
					return
				}

				nextSeq := 1
				if maxSeq.Valid {
					nextSeq = int(maxSeq.Int64) + 1
				}

				// Resolve per type at the boundary (400s live here), then insert via
				// the itinerary domain's intent methods — the single route_tasks
				// writer (Phase 5). Column contract unchanged from the legacy INSERT.
				var newTaskID string
				switch addReq.TaskType {
				case "collection":
					if addReq.BinID == nil {
						utils.RespondError(w, http.StatusBadRequest, "bin_id required for collection task")
						return
					}

					log.Printf("🔍 [SHIFT UPDATE] Looking up bin_id: %s", *addReq.BinID)

					// Fetch bin details
					var bin struct {
						ID             string  `db:"id"`
						BinNumber      int     `db:"bin_number"`
						Latitude       float64 `db:"latitude"`
						Longitude      float64 `db:"longitude"`
						CurrentStreet  string  `db:"current_street"`
						City           string  `db:"city"`
						ZipCode        string  `db:"zip"`
						FillPercentage int     `db:"fill_percentage"`
					}
					err = tx.Get(&bin, `SELECT id, bin_number, latitude, longitude, current_street, city, zip, fill_percentage FROM bins WHERE id = $1`, *addReq.BinID)
					if err != nil {
						log.Printf("❌ [SHIFT UPDATE] Error fetching bin_id=%s: %v", *addReq.BinID, err)
						if err == sql.ErrNoRows {
							log.Printf("❌ [SHIFT UPDATE] Bin not found in database: %s", *addReq.BinID)
						}
						utils.RespondError(w, http.StatusBadRequest, "Bin not found")
						return
					}

					log.Printf("✅ [SHIFT UPDATE] Found bin #%d at %s", bin.BinNumber, bin.CurrentStreet)

					newTaskID, err = itinerary.AddCollection(tx, shiftID, itinerary.NewCollection{
						Seq:            nextSeq,
						BinID:          *addReq.BinID,
						BinNumber:      bin.BinNumber,
						Lat:            bin.Latitude,
						Lng:            bin.Longitude,
						Address:        fmt.Sprintf("%s, %s %s", bin.CurrentStreet, bin.City, bin.ZipCode),
						FillPercentage: bin.FillPercentage,
						AddedBy:        userClaims.UserID,
						AdditionReason: req.Reason,
						Now:            now,
					})

				case "placement":
					if addReq.PotentialLocationID == nil {
						utils.RespondError(w, http.StatusBadRequest, "potential_location_id required for placement task")
						return
					}
					// Fetch potential location details
					var potLoc struct {
						ID        string  `db:"id"`
						Latitude  float64 `db:"latitude"`
						Longitude float64 `db:"longitude"`
						Address   string  `db:"address"`
					}
					err = tx.Get(&potLoc, `SELECT id, latitude, longitude, address FROM potential_locations WHERE id = $1`, *addReq.PotentialLocationID)
					if err != nil {
						log.Printf("❌ Error fetching potential location: %v", err)
						utils.RespondError(w, http.StatusBadRequest, "Potential location not found")
						return
					}

					// Mark as assigned
					_, err = tx.Exec(`UPDATE potential_locations SET assigned_shift_id = $1, updated_at = $2 WHERE id = $3`,
						shiftID, now, *addReq.PotentialLocationID)
					if err != nil {
						log.Printf("❌ Error assigning potential location: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to assign potential location")
						return
					}

					newTaskID, err = itinerary.AddPlacement(tx, shiftID, itinerary.NewPlacement{
						Seq:                 nextSeq,
						PotentialLocationID: *addReq.PotentialLocationID,
						Lat:                 potLoc.Latitude,
						Lng:                 potLoc.Longitude,
						Address:             potLoc.Address,
						AddedBy:             userClaims.UserID,
						AdditionReason:      req.Reason,
						Now:                 now,
					})

				case "pickup", "dropoff":
					if addReq.MoveRequestID == nil {
						utils.RespondError(w, http.StatusBadRequest, "move_request_id required for pickup/dropoff task")
						return
					}
					// Fetch move request details (+ move_type and the bin's number/fill,
					// stamped on the legs since Slice 2b for create/AddMove parity).
					var moveReq struct {
						ID            string   `db:"id"`
						BinID         string   `db:"bin_id"`
						MoveType      string   `db:"move_type"`
						BinNumber     *int     `db:"bin_number"`
						BinFill       *int     `db:"fill_percentage"`
						Latitude      *float64 `db:"original_latitude"`
						Longitude     *float64 `db:"original_longitude"`
						Address       *string  `db:"original_address"`
						DestLatitude  *float64 `db:"new_latitude"`
						DestLongitude *float64 `db:"new_longitude"`
						DestAddress   *string  `db:"new_address"`
					}
					err = tx.Get(&moveReq, `
						SELECT mr.id, mr.bin_id, mr.move_type,
						       b.bin_number, b.fill_percentage,
						       mr.original_latitude, mr.original_longitude, mr.original_address,
						       mr.new_latitude, mr.new_longitude, mr.new_address
						FROM bin_move_requests mr
						LEFT JOIN bins b ON b.id = mr.bin_id
						WHERE mr.id = $1`, *addReq.MoveRequestID)
					if err != nil {
						log.Printf("❌ Error fetching move request: %v", err)
						utils.RespondError(w, http.StatusBadRequest, "Move request not found")
						return
					}

					// Resolve the move's DESTINATION once for both legs: the move's
					// new_* (relocation/redeployment) or the shift's warehouse
					// (store/pickup_only). The pickup carries it in destination_* so
					// the optimizer can model the move as a pickup→dropoff shipment —
					// it reads moves off the pickup row alone (#34).
					var destLat, destLng *float64
					var destAddr *string
					if moveReq.DestLatitude != nil && moveReq.DestLongitude != nil {
						destLat, destLng, destAddr = moveReq.DestLatitude, moveReq.DestLongitude, moveReq.DestAddress
					} else if wlat, wlng, waddr, ok := resolveCurrentWarehouse(db); ok {
						// Store/pickup_only destination: the CURRENT config warehouse —
						// never the shift's creation-time snapshot, which goes stale if
						// the warehouse moved (matches CreateShiftWithTasks + AddMove).
						destLat, destLng = &wlat, &wlng
						if waddr != "" {
							destAddr = &waddr
						}
					}

					leg := itinerary.NewMoveLeg{
						Seq:                nextSeq,
						MoveRequestID:      *addReq.MoveRequestID,
						BinID:              moveReq.BinID,
						BinNumber:          moveReq.BinNumber,
						MoveType:           moveReq.MoveType,
						DestLat:            destLat,
						DestLng:            destLng,
						DestinationAddress: destAddr,
						AddedBy:            userClaims.UserID,
						AdditionReason:     req.Reason,
						Now:                now,
					}
					if addReq.TaskType == "pickup" {
						leg.Type = itinerary.Pickup
						leg.FillPercentage = moveReq.BinFill
						if moveReq.Latitude != nil && moveReq.Longitude != nil {
							leg.Lat = *moveReq.Latitude
							leg.Lng = *moveReq.Longitude
						}
						if moveReq.Address != nil {
							leg.Address = moveReq.Address
						}
					} else { // dropoff
						leg.Type = itinerary.Dropoff
						// The dropoff sits AT the destination (its own coords duplicate
						// destination_* — the app-nav convention shared with AddMove).
						if destLat == nil || destLng == nil {
							// Store move on a shift without warehouse coords: destination
							// unresolvable — skip the dropoff (preserved legacy behavior).
							log.Printf("⚠️  [SHIFT UPDATE] Warehouse coordinates not set for store move, skipping dropoff task for move request %s", *addReq.MoveRequestID)
							continue // Skip this task entirely
						}
						leg.Lat = *destLat
						leg.Lng = *destLng
						leg.Address = destAddr
					}

					// Assign via the domain's guarded transition — status flips to
					// assigned/in_progress, assignment_type='shift', assigned_user_id
					// NULL (the shift's driver is derived, never denormalized — #31).
					// The raw UPDATE this replaces set no status and stamped the
					// driver into assigned_user_id, silently diverging from every
					// other assignment path — and would even "assign" a cancelled move.
					moveStatus := string(moverequest.StatusAssigned)
					if shift.Status == "active" {
						moveStatus = string(moverequest.StatusInProgress)
					}
					if err = moverequest.AssignToShift(tx, *addReq.MoveRequestID, shiftID, moveStatus, now); err != nil {
						if errors.Is(err, moverequest.ErrInvalidTransition) {
							utils.RespondError(w, http.StatusBadRequest, "move request is already completed or cancelled")
							return
						}
						log.Printf("❌ Error assigning move request: %v", err)
						utils.RespondError(w, http.StatusInternalServerError, "Failed to assign move request")
						return
					}
					// History: one 'assigned' event per move per request (both legs
					// share the move), logged in-tx so it commits with the assignment.
					if !loggedAssignments[*addReq.MoveRequestID] {
						loggedAssignments[*addReq.MoveRequestID] = true
						var driverName string
						if e := tx.Get(&driverName, `SELECT name FROM users WHERE id = $1`, shift.DriverID); e != nil {
							driverName = "Unknown Driver"
						}
						if logErr := moverequest.LogAssigned(tx, *addReq.MoveRequestID, userClaims.UserID, userClaims.Email, "manager",
							"shift", &shift.DriverID, &driverName, &shiftID); logErr != nil {
							log.Printf("⚠️  Failed to log move assignment history: %v", logErr)
						}
					}

					newTaskID, err = itinerary.AddMoveLeg(tx, shiftID, leg)

				default:
					utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Invalid task type: %s", addReq.TaskType))
					return
				}

				if err != nil {
					log.Printf("❌ Error inserting task: %v", err)
					utils.RespondError(w, http.StatusInternalServerError, "Failed to add task")
					return
				}
				log.Printf("   Created task: type=%s, id=%s", addReq.TaskType, newTaskID)

				addedCount++
			}

			changes["tasks_added"] = addedCount
			if skippedCount > 0 {
				log.Printf("⏭️  Skipped %d duplicate tasks", skippedCount)
			}
			log.Printf("✅ Added %d tasks", addedCount)
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 3.5: Recalculate total_bins if tasks were added/removed
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		if addedCount > 0 || removedCount > 0 {
			log.Printf("🔢 Recalculating total_bins after task changes...")

			// Recompute the shift's counts from route_tasks (single source of truth,
			// logical bins) — replaces an ad-hoc raw COUNT that set only total_bins.
			if err = itinerary.RecomputeShiftCounts(tx, shiftID, now); err != nil {
				log.Printf("❌ Error updating total_bins: %v", err)
				utils.RespondError(w, http.StatusInternalServerError, "Failed to update total_bins")
				return
			}
			log.Printf("✅ Recomputed shift counts")
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit changes")
			return
		}

		log.Printf("✅ Transaction committed")

		// Auto-cancel shift if all tasks were removed
		if removedCount > 0 {
			var remainingTasks int
			db.Get(&remainingTasks, `SELECT COUNT(*) FROM route_tasks WHERE shift_id = $1 AND is_deleted = false AND task_type != 'warehouse_stop'`, shiftID)
			if remainingTasks == 0 {
				log.Printf("🚫 All tasks removed from shift %s via PATCH — auto-cancelling", shiftID)
				cancelNow := time.Now().Unix()

				var optMetaBytes []byte
				db.Get(&optMetaBytes, `SELECT optimization_metadata FROM shifts WHERE id = $1`, shiftID)
				var optMeta interface{}
				if len(optMetaBytes) > 0 {
					raw := json.RawMessage(optMetaBytes)
					optMeta = &raw
				}

				_, _ = archiveShift(db, archiveShiftParams{
					ID:                  shift.ID,
					DriverID:            shift.DriverID,
					RouteID:             shift.RouteID,
					StartTime:           shift.StartTime,
					EndTime:             cancelNow,
					CreatedAt:           shift.CreatedAt,
					EndedAt:             cancelNow,
					TotalPauseSeconds:   shift.TotalPauseSeconds,
					TotalBins:           shift.TotalBins,
					CompletedBins:       shift.CompletedBins,
					IncidentsReported:   0,
					FieldObservations:   0,
					EndReason:           "manager_cancelled",
					EndedByUserID:       userClaims.UserID,
					EndReasonMetadata:   nil,
					OptMeta:             optMeta,
					OnConflictDoNothing: true,
				})
				db.Exec(`UPDATE shifts SET status = 'cancelled', end_time = $1, pause_start_time = NULL, updated_at = $1 WHERE id = $2`, cancelNow, shiftID)
				shift.Status = "cancelled"
				log.Printf("✅ Shift %s auto-cancelled and archived (0 tasks remain)", shiftID)
			}
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 4: Re-optimize if tasks changed or driver changed
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// Refresh shift status from DB in case it was changed (e.g., active→ready on reassign)
		db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		shouldReoptimize := req.Reoptimize || addedCount > 0 || removedCount > 0 || changes["driver_changed"].(bool)

		// Clean up warehouse stops on ready shifts — they're meaningless before optimization
		if shouldReoptimize && shift.Status == "ready" {
			// (Now also scoped to is_deleted=false like the two optimizer purge
			// sites — soft-deleted artifacts are audit rows, not live clutter.)
			if deleted, _ := itinerary.PurgeIncompleteWarehouseStops(db, shiftID); deleted > 0 {
				log.Printf("🗑️ Cleaned up %d stale warehouse stops on ready shift", deleted)
			}
		}

		if shouldReoptimize && shift.Status == "active" {
			log.Printf("🔄 Re-optimizing route (tasks changed or driver reassigned)...")

			// Update shift reference if driver changed
			if req.DriverID != nil {
				shift.DriverID = *req.DriverID
			}

			// Re-optimize the shift after task changes
			err = ReoptimizeActiveShift(db, redisClient, shiftID, centrifugoClient, true)
			if err != nil {
				log.Printf("⚠️  Failed to re-optimize: %v", err)
				// Don't fail the request — driver keeps current route order
			} else {
				changes["route_reoptimized"] = true
				log.Printf("✅ Route re-optimized")
			}
		}

		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		// STEP 5: Publish Centrifugo events
		// ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
		if centrifugoClient != nil {
			// If driver changed, notify old driver
			if changes["driver_changed"].(bool) {
				oldDriverChannel := fmt.Sprintf("shift:updates:%s", shiftID)
				reassignedEvent := map[string]interface{}{
					"type":   "shift_reassigned",
					"reason": "Shift has been reassigned to another driver",
				}
				if pubErr := centrifugoClient.PublishToChannel(r.Context(), oldDriverChannel, reassignedEvent); pubErr != nil {
					log.Printf("⚠️  Failed to notify old driver: %v", pubErr)
				} else {
					log.Printf("📡 Published shift_reassigned event to old driver")
				}
			}

			// Publish shift_edited event to current/new driver
			newDriverChannel := fmt.Sprintf("shift:updates:%s", shiftID)
			editedEvent := map[string]interface{}{
				"type":         "shift_edited",
				"shift_id":     shiftID,
				"changes":      changes,
				"reason":       req.Reason,
				"manager_name": userClaims.Email,
			}
			if pubErr := centrifugoClient.PublishToChannel(r.Context(), newDriverChannel, editedEvent); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_edited event: %v", pubErr)
			} else {
				log.Printf("📡 Published shift_edited event")
			}
		}

		// Send FCM notifications (preference-aware)
		if fcmService != nil && changes["driver_changed"].(bool) {
			// Notify old driver
			title, body := services.ShiftNotificationText("shift_reassigned", nil)
			_, oldNotifIDs := services.CreateNotificationForUsers(db, []string{oldDriverID}, "shift_reassigned", title, body, map[string]string{"shift_id": shiftID})
			if len(oldNotifIDs) > 0 {
				var oldDriverToken models.FCMToken
				if tokenErr := db.Get(&oldDriverToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, oldDriverID); tokenErr == nil {
					go fcmService.SendShiftUpdateNotification(oldDriverToken.Token, shiftID, "shift_reassigned", nil)
					log.Printf("📱 Sent FCM notification to old driver: %s", oldDriverID)
				}
			}

			// Notify new driver
			if req.DriverID != nil {
				newTitle, newBody := services.ShiftNotificationText("shift_created", nil)
				_, newNotifIDs := services.CreateNotificationForUsers(db, []string{*req.DriverID}, "shift_created", newTitle, newBody, map[string]string{"shift_id": shiftID})
				if len(newNotifIDs) > 0 {
					var newDriverToken models.FCMToken
					if tokenErr := db.Get(&newDriverToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, *req.DriverID); tokenErr == nil {
						go fcmService.SendShiftUpdateNotification(newDriverToken.Token, shiftID, "shift_created", nil)
						log.Printf("📱 Sent FCM notification to new driver: %s", *req.DriverID)
					}
				}
			}
		}

		// Get updated shift
		err = db.Get(&shift, `SELECT * FROM shifts WHERE id = $1`, shiftID)
		if err != nil {
			log.Printf("⚠️  Error refreshing shift: %v", err)
		}

		// Get updated tasks
		tasks, err := database.GetShiftTasks(db, shiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch tasks: %v", err)
			tasks = []models.RouteTask{}
		}

		// Return response
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "Shift updated successfully",
			"changes": changes,
			"shift": map[string]interface{}{
				"id":        shift.ID,
				"status":    shift.Status,
				"driver_id": shift.DriverID,
				"tasks":     tasks,
			},
		})

		log.Printf("✅ Shift updated successfully")
		log.Printf("   Changes: %+v", changes)
	}
}

// AddTaskRequest represents a request to add a task to a shift
type AddTaskRequest struct {
	TaskType            string  `json:"task_type"` // collection, placement, pickup, dropoff, warehouse_stop
	BinID               *string `json:"bin_id,omitempty"`
	PotentialLocationID *string `json:"potential_location_id,omitempty"`
	MoveRequestID       *string `json:"move_request_id,omitempty"`
}

// GetDriverShiftHistory returns all completed shifts for the authenticated driver
