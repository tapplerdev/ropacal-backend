package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
	"ropacal-backend/internal/database"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"
)

// GetShiftTasks retrieves all tasks for a shift
func GetShiftTasks(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/shifts/:shiftId/tasks")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "shiftId")
		log.Printf("   User: %s, Shift ID: %s", userClaims.Email, shiftID)

		tasks, err := database.GetShiftTasks(db, shiftID)
		if err != nil {
			log.Printf("❌ Error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch tasks")
			return
		}

		log.Printf("📤 RESPONSE: 200 - Found %d tasks", len(tasks))
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    tasks,
		})
	}
}

// GetShiftTasksDetailed retrieves tasks with JOINed data
func GetShiftTasksDetailed(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/shifts/:shiftId/tasks/detailed")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "shiftId")
		log.Printf("   User: %s, Shift ID: %s", userClaims.Email, shiftID)

		tasks, err := database.GetShiftTasksDetailed(db, shiftID)
		if err != nil {
			log.Printf("❌ Error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch detailed tasks")
			return
		}

		log.Printf("📤 RESPONSE: 200 - Found %d detailed tasks", len(tasks))
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    tasks,
		})
	}
}

// CreateShiftWithTasks creates a new shift with tasks (Manager only)
func CreateShiftWithTasks(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: POST /api/manager/shifts/create-with-tasks")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		log.Printf("   Manager: %s", userClaims.Email)

		var req models.CreateShiftWithTasksRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Invalid request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		log.Printf("   Driver ID: %s", req.DriverID)
		log.Printf("   Truck Capacity: %d bins", req.TruckBinCapacity)
		log.Printf("   Warehouse: %.6f, %.6f", req.WarehouseLatitude, req.WarehouseLongitude)
		log.Printf("   Tasks: %d", len(req.Tasks))

		// Validate required fields
		if req.DriverID == "" {
			utils.RespondError(w, http.StatusBadRequest, "driver_id is required")
			return
		}
		if req.TruckBinCapacity <= 0 {
			utils.RespondError(w, http.StatusBadRequest, "truck_bin_capacity must be greater than 0")
			return
		}
		if len(req.Tasks) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "tasks array cannot be empty")
			return
		}

		shiftID, taskCount, err := database.CreateShiftWithTasks(
			db,
			req.DriverID,
			req.TruckBinCapacity,
			req.WarehouseLatitude,
			req.WarehouseLongitude,
			req.WarehouseAddress,
			req.Tasks,
		)
		if err != nil {
			log.Printf("❌ Error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to create shift with tasks")
			return
		}

		log.Printf("✅ Shift %s created with %d tasks", shiftID, taskCount)

		// Update move requests that were included in this shift
		log.Printf("🔍 Checking for move requests in tasks...")
		moveRequestUpdates := make(map[string]bool) // Track unique move request IDs
		for _, task := range req.Tasks {
			if moveReqID, ok := task["move_request_id"].(string); ok && moveReqID != "" {
				moveRequestUpdates[moveReqID] = true
			}
		}

		if len(moveRequestUpdates) > 0 {
			log.Printf("📝 Found %d move request(s) to update", len(moveRequestUpdates))
			now := time.Now().Unix()

			for moveReqID := range moveRequestUpdates {
				log.Printf("   🚚 Updating move request %s", moveReqID)
				log.Printf("      - Status: pending → in_progress")
				log.Printf("      - Assigned to shift: %s", shiftID)
				log.Printf("      - Assigned to driver: %s", req.DriverID)

				updateQuery := `UPDATE bin_move_requests
								SET status = 'in_progress',
									assigned_shift_id = $1,
									assigned_driver_id = $2,
									updated_at = $3
								WHERE id = $4
								AND status = 'pending'`

				result, err := db.Exec(updateQuery, shiftID, req.DriverID, now, moveReqID)
				if err != nil {
					log.Printf("      ❌ Error updating move request %s: %v", moveReqID, err)
					continue
				}

				rowsAffected, _ := result.RowsAffected()
				if rowsAffected == 0 {
					log.Printf("      ⚠️  Move request %s not updated (already assigned or not found)", moveReqID)
				} else {
					log.Printf("      ✅ Move request %s updated successfully", moveReqID)
				}
			}
		} else {
			log.Printf("   ℹ️  No move requests in this shift")
		}

		log.Printf("📤 RESPONSE: 201 - Created shift %s with %d tasks", shiftID, taskCount)

		// Broadcast shift creation to driver via WebSocket
		shiftNotification := map[string]interface{}{
			"type": "shift_created",
			"data": map[string]interface{}{
				"shift_id":   shiftID,
				"status":     "ready",
				"task_count": taskCount,
				"created_at": time.Now().Unix(),
			},
		}

		// Send to specific driver
		hub.BroadcastToUser(req.DriverID, shiftNotification)
		log.Printf("📡 WebSocket: Broadcasted new shift to driver %s", req.DriverID)

		// Also notify managers
		hub.BroadcastToRole("manager", shiftNotification)
		log.Printf("📡 WebSocket: Broadcasted new shift to managers")

		utils.RespondJSON(w, http.StatusCreated, map[string]interface{}{
			"success": true,
			"data": models.CreateShiftWithTasksResponse{
				ShiftID:   shiftID,
				TaskCount: taskCount,
			},
		})
	}
}

// CompleteTask marks a task as completed
func CompleteTask(db *sqlx.DB, hub *websocket.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: PUT /api/shifts/tasks/:taskId/complete")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		taskID := chi.URLParam(r, "taskId")
		log.Printf("   User: %s, Task ID: %s", userClaims.Email, taskID)

		var req models.CompleteTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ Invalid request body: %v", err)
			utils.RespondError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		// Get task to find shift_id
		task, err := database.GetTaskByID(db, taskID)
		if err != nil {
			log.Printf("❌ Task not found: %v", err)
			utils.RespondError(w, http.StatusNotFound, "Task not found")
			return
		}

		// Complete task
		err = database.CompleteTask(db, taskID, req.UpdatedFillPercentage, req.PhotoURL, req.NewBinID)
		if err != nil {
			log.Printf("❌ Error completing task: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to complete task")
			return
		}

		// Update shift completed_bins count
		_, err = db.Exec(
			"UPDATE shifts SET completed_bins = completed_bins + 1, updated_at = $1 WHERE id = $2",
			time.Now().Unix(),
			task.ShiftID,
		)
		if err != nil {
			log.Printf("❌ Error updating shift: %v", err)
		}

		// Get updated shift for WebSocket broadcast
		var shift models.Shift
		shiftQuery := `SELECT * FROM shifts WHERE id = $1`
		err = db.Get(&shift, shiftQuery, task.ShiftID)
		if err != nil {
			log.Printf("⚠️  Warning: Could not fetch shift for WebSocket: %v", err)
		} else {
			// Get route bins for full shift data
			bins, err := getRouteBinsWithDetails(db, shift.ID)
			if err != nil {
				log.Printf("⚠️  Warning: Could not fetch bins for WebSocket: %v", err)
			} else {
				// Broadcast shift update via WebSocket
				shiftUpdate := map[string]interface{}{
					"id":                  shift.ID,
					"driver_id":           shift.DriverID,
					"route_id":            shift.RouteID,
					"status":              shift.Status,
					"start_time":          shift.StartTime,
					"end_time":            shift.EndTime,
					"total_pause_seconds": shift.TotalPauseSeconds,
					"pause_start_time":    shift.PauseStartTime,
					"total_bins":          shift.TotalBins,
					"completed_bins":      shift.CompletedBins,
					"bins":                bins,
					"created_at":          shift.CreatedAt,
					"updated_at":          shift.UpdatedAt,
				}

				updateMsg := map[string]interface{}{
					"type": "shift_update",
					"data": shiftUpdate,
				}

				// Broadcast to driver
				hub.BroadcastToUser(shift.DriverID, updateMsg)
				log.Printf("📡 WebSocket: Broadcasted shift update to driver %s", shift.DriverID)

				// Broadcast to managers
				hub.BroadcastToRole("manager", updateMsg)
				log.Printf("📡 WebSocket: Broadcasted shift update to managers")
			}
		}

		log.Printf("📤 RESPONSE: 200 - Task %s completed", taskID)
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"task_id":        taskID,
				"completed_bins": shift.CompletedBins,
				"total_bins":     shift.TotalBins,
			},
		})
	}
}
