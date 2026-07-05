package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"ropacal-backend/internal/database"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	shiftdomain "ropacal-backend/internal/shift"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
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

// GetShiftTasksWithHistory retrieves ALL tasks including deleted ones for audit/history view
func GetShiftTasksWithHistory(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("📥 REQUEST: GET /api/manager/shifts/:shiftId/tasks/history")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		shiftID := chi.URLParam(r, "shiftId")
		log.Printf("   Manager: %s, Shift ID: %s", userClaims.Email, shiftID)

		tasks, err := database.GetShiftTasksWithDeleted(db, shiftID)
		if err != nil {
			log.Printf("❌ Error: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch task history")
			return
		}

		log.Printf("📤 RESPONSE: 200 - Found %d tasks (including deleted)", len(tasks))
		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    tasks,
		})
	}
}

// CreateShiftWithTasks creates a new shift with tasks (Manager only)
func CreateShiftWithTasks(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client, fcmService *services.FCMService) http.HandlerFunc {
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
		log.Printf("   Shift Type: %s", req.ShiftType)
		log.Printf("   Truck Capacity: %d bins", req.TruckBinCapacity)
		log.Printf("   Tasks: %d", len(req.Tasks))

		// Validate required fields
		if req.DriverID == "" {
			utils.RespondError(w, http.StatusBadRequest, "driver_id is required")
			return
		}
		if len(req.Tasks) == 0 {
			utils.RespondError(w, http.StatusBadRequest, "tasks array cannot be empty")
			return
		}

		// Default shift_type
		if req.ShiftType == "" {
			req.ShiftType = "standard"
		}

		// Validate shift_type + per-task time_window_type at the boundary (clean 400,
		// not a 500 at the shifts / route_tasks CHECK constraints).
		if _, err := shiftdomain.ParseType(req.ShiftType); err != nil {
			utils.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		for i, t := range req.Tasks {
			// task_type was an unchecked passthrough to the DB CHECK (bad value →
			// 500 at insert); validate here for a clean 400. The itinerary domain
			// owns the taxonomy.
			tt, _ := t["task_type"].(string)
			if _, err := itinerary.ParseTaskType(tt); err != nil {
				utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("task %d: %s", i, err.Error()))
				return
			}
			if twt, ok := t["time_window_type"].(string); ok && twt != "" {
				if _, err := itinerary.ParseTimeWindowType(twt); err != nil {
					utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("task %d: %s", i, err.Error()))
					return
				}
			}
		}

		// Check if driver already has an active/ready shift for the same date
		scheduledDate := req.ScheduledDate
		if scheduledDate == nil {
			pacific, _ := time.LoadLocation("America/Los_Angeles")
			today := time.Now().In(pacific).Format("2006-01-02")
			scheduledDate = &today
		}

		var existingShift struct {
			ID          string `db:"id" json:"id"`
			Status      string `db:"status" json:"status"`
			CreatedAt   int64  `db:"created_at" json:"created_at"`
			TotalBins   int    `db:"total_bins" json:"total_bins"`
			ActiveTasks int    `db:"active_tasks" json:"active_tasks"`
		}
		existingErr := db.Get(&existingShift, `
			SELECT s.id, s.status, s.created_at, s.total_bins,
			       (SELECT COUNT(*) FROM route_tasks rt WHERE rt.shift_id = s.id AND rt.is_deleted = false) as active_tasks
			FROM shifts s
			WHERE s.driver_id = $1 AND s.status IN ('active', 'ready')
			  AND (s.scheduled_date = $2 OR s.scheduled_date IS NULL)
			LIMIT 1
		`, req.DriverID, *scheduledDate)
		if existingErr == nil {
			log.Printf("⚠️  Driver %s already has a %s shift: %s", req.DriverID, existingShift.Status, existingShift.ID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":          "Driver already has an active shift",
				"existing_shift": existingShift,
			})
			return
		}

		// Validate based on shift type
		isCustom := req.ShiftType == "custom"
		if !isCustom {
			// Standard shifts require warehouse and truck capacity
			if req.TruckBinCapacity <= 0 {
				utils.RespondError(w, http.StatusBadRequest, "truck_bin_capacity must be greater than 0")
				return
			}
		} else {
			// Custom shifts always use Mapbox optimization
			req.LockRouteOrder = false
		}

		// Auto-populate warehouse coordinates from config if not provided
		if req.WarehouseLatitude == nil || req.WarehouseLongitude == nil || (*req.WarehouseLatitude == 0 && *req.WarehouseLongitude == 0) {
			var whConfig struct {
				Latitude  float64 `db:"latitude"`
				Longitude float64 `db:"longitude"`
				Address   string  `db:"address"`
			}
			cfgErr := db.Get(&whConfig, `SELECT
				COALESCE((SELECT (value::jsonb->>'latitude')::float8 FROM config WHERE key = 'warehouse_location'), 0) as latitude,
				COALESCE((SELECT (value::jsonb->>'longitude')::float8 FROM config WHERE key = 'warehouse_location'), 0) as longitude,
				COALESCE((SELECT value::jsonb->>'address' FROM config WHERE key = 'warehouse_location'), '') as address
			`)
			if cfgErr == nil && whConfig.Latitude != 0 {
				req.WarehouseLatitude = &whConfig.Latitude
				req.WarehouseLongitude = &whConfig.Longitude
				req.WarehouseAddress = &whConfig.Address
				log.Printf("📍 Auto-populated warehouse from config: (%.6f, %.6f) %s", whConfig.Latitude, whConfig.Longitude, whConfig.Address)
			}
		}

		shiftID, taskCount, err := database.CreateShiftWithTasks(
			db,
			req.DriverID,
			req.TruckBinCapacity,
			req.WarehouseLatitude,
			req.WarehouseLongitude,
			req.WarehouseAddress,
			req.Tasks,
			req.LockRouteOrder,
			req.WarehouseDeployments,
			req.ShiftType,
			req.ShiftLabel,
			req.StartLatitude,
			req.StartLongitude,
			req.StartAddress,
			req.EndLatitude,
			req.EndLongitude,
			req.EndAddress,
			req.ScheduledStart,
			req.ScheduledEnd,
			scheduledDate,
			req.RouteID,
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

			// Get manager info for history logging
			managerID := userClaims.UserID
			var managerName string
			err := db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerID)
			if err != nil {
				log.Printf("⚠️  Warning: Failed to fetch manager name: %v", err)
				managerName = "Unknown Manager"
			}

			// Get driver name for history logging
			var driverName string
			err = db.Get(&driverName, `SELECT name FROM users WHERE id = $1`, req.DriverID)
			if err != nil {
				log.Printf("⚠️  Warning: Failed to fetch driver name: %v", err)
				driverName = "Unknown Driver"
			}

			for moveReqID := range moveRequestUpdates {
				log.Printf("   🚚 Updating move request %s", moveReqID)
				log.Printf("      - Status: pending → assigned (shift is 'ready', not started yet)")
				log.Printf("      - Assigned to shift: %s", shiftID)
				log.Printf("      - Assigned to driver: %s (%s)", req.DriverID, driverName)

				// FIX: Set status to 'assigned' since newly created shifts have status 'ready'
				// Only active shifts should set move requests to 'in_progress'
				updateQuery := `UPDATE bin_move_requests
								SET status = 'assigned',
									assigned_shift_id = $1,
									assigned_user_id = $2,
									assignment_type = 'shift',
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

					// Log assignment history
					log.Printf("      📝 Logging assignment to history...")
					assignmentType := "shift"
					err = moverequest.LogAssigned(db, moveReqID, managerID, managerName, "manager",
						assignmentType, &req.DriverID, &driverName, &shiftID)
					if err != nil {
						log.Printf("      ⚠️  WARNING: Failed to log assignment history: %v", err)
					} else {
						log.Printf("      ✅ Assignment history logged successfully")
					}
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

		// Also notify managers and admins
		hub.BroadcastToRole("manager", shiftNotification)
		hub.BroadcastToRole("admin", shiftNotification)
		log.Printf("📡 WebSocket: Broadcasted new shift to managers and admins")

		// Publish shift_created to Centrifugo
		if centrifugoClient != nil {
			shiftCreatedData := map[string]interface{}{
				"shift_id":   shiftID,
				"driver_id":  req.DriverID,
				"status":     "ready",
				"task_count": taskCount,
			}

			// 1. Publish to company:events (for manager dashboards)
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "shift_created", shiftCreatedData); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_created to company:events: %v", pubErr)
			} else {
				log.Printf("📡 Centrifugo: Published shift_created to company:events")
			}

			// 2. Publish to driver:events:{driverId} (for the assigned driver)
			if pubErr := centrifugoClient.PublishDriverEvent(r.Context(), req.DriverID, "shift_created", shiftCreatedData); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_created to driver:events:%s: %v", req.DriverID, pubErr)
			} else {
				log.Printf("📡 Centrifugo: Published shift_created to driver:events:%s", req.DriverID)
			}

			// Create per-user notifications for admins
			adminIDs, _ := services.GetAdminUserIDs(db)
			services.CreateNotificationForUsers(db, adminIDs, "shift_created",
				fmt.Sprintf("New Shift Created"),
				fmt.Sprintf("Shift with %d tasks assigned", taskCount),
				shiftCreatedData)
		}

		// Send FCM push notification to driver (preference-aware)
		driverTitle, driverBody := services.ShiftNotificationText("shift_created", nil)
		_, driverNotifIDs := services.CreateNotificationForUsers(db, []string{req.DriverID}, "shift_created", driverTitle, driverBody, map[string]string{"shift_id": shiftID})
		if len(driverNotifIDs) > 0 && fcmService != nil {
			var driverFCMToken models.FCMToken
			tokenErr := db.Get(&driverFCMToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, req.DriverID)
			if tokenErr != nil {
				log.Printf("⚠️  No FCM token found for driver %s: %v", req.DriverID, tokenErr)
			} else {
				fcmErr := fcmService.SendShiftUpdateNotification(
					driverFCMToken.Token,
					shiftID,
					"shift_created",
					nil,
				)
				if fcmErr != nil {
					log.Printf("⚠️  Failed to send FCM notification: %v", fcmErr)
				} else {
					log.Printf("📱 Sent FCM shift_created to driver %s", req.DriverID)
				}
			}
		}

		utils.RespondJSON(w, http.StatusCreated, map[string]interface{}{
			"success": true,
			"data": models.CreateShiftWithTasksResponse{
				ShiftID:   shiftID,
				TaskCount: taskCount,
			},
		})
	}
}
