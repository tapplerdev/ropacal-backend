package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/websocket"
	"ropacal-backend/pkg/utils"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"
)

func ClearAllShifts(db *sqlx.DB, hub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("🗑️  REQUEST: DELETE /api/admin/shifts/clear")

		// Get all affected driver IDs before deleting
		var affectedDrivers []string
		query := `SELECT DISTINCT driver_id FROM shifts WHERE status != 'inactive'`
		err := db.Select(&affectedDrivers, query)
		if err != nil && err != sql.ErrNoRows {
			log.Printf("⚠️  Error getting affected drivers: %v", err)
			affectedDrivers = []string{} // Continue even if this fails
		}

		log.Printf("📊 Found %d drivers with active/ready shifts", len(affectedDrivers))

		// Execute delete query
		result, err := db.Exec("DELETE FROM shifts")
		if err != nil {
			log.Printf("❌ Error clearing shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to clear shifts")
			return
		}

		rowsAffected, _ := result.RowsAffected()
		log.Printf("✅ Cleared %d shifts from database", rowsAffected)

		// Broadcast shift_deleted event to all affected drivers
		for _, driverID := range affectedDrivers {
			hub.BroadcastToUser(driverID, map[string]interface{}{
				"type": "shift_deleted",
				"data": map[string]interface{}{
					"shift_id": "all",
					"message":  "All shifts have been cleared by manager",
					"reason":   "manager_clear_all",
				},
			})
			log.Printf("📤 Sent shift_deleted event to driver: %s", driverID)

			// Also broadcast to managers that this driver's shift ended
			broadcastPayload := map[string]interface{}{
				"type": "driver_shift_change",
				"data": map[string]interface{}{
					"driver_id": driverID,
					"status":    "ended",
					"shift_id":  "all",
				},
			}
			hub.BroadcastToRole("admin", broadcastPayload)
			hub.BroadcastToRole("manager", broadcastPayload)

			// Also publish via Centrifugo
			if centrifugoClient != nil {
				// shift_deleted to company channel (drivers subscribe to company:events)
				if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "shift_deleted", map[string]interface{}{
					"driver_id": driverID,
					"shift_id":  "all",
					"message":   "All shifts have been cleared by manager",
					"reason":    "manager_clear_all",
				}); pubErr != nil {
					log.Printf("⚠️  Failed to publish shift_deleted to Centrifugo: %v", pubErr)
				}

				// driver_shift_change to company channel
				if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
					"driver_id": driverID,
					"status":    "ended",
					"shift_id":  "all",
				}); pubErr != nil {
					log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
				}
			}
		}

		log.Printf("✅ WebSocket events sent to %d drivers", len(affectedDrivers))
		log.Printf("📡 Broadcast driver_shift_change to managers for %d drivers (shifts cleared)", len(affectedDrivers))

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success":       true,
			"message":       "All shifts cleared successfully",
			"rows_affected": rowsAffected,
		})
	}
}

// UpdateLocation handles driver location updates (POST /api/driver/location)
// Called every 10 seconds when driver is on active shift

func CancelShift(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shiftID := chi.URLParam(r, "id")
		log.Printf("❌ REQUEST: PUT /api/manager/shifts/%s/cancel", shiftID)

		if shiftID == "" {
			utils.RespondError(w, http.StatusBadRequest, "shift_id is required")
			return
		}

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		now := time.Now().Unix()

		// Get shift details for websocket/FCM notifications
		var shift models.Shift
		err := db.Get(&shift, "SELECT * FROM shifts WHERE id = $1", shiftID)
		if err != nil {
			if err == sql.ErrNoRows {
				utils.RespondError(w, http.StatusNotFound, "Shift not found")
				return
			}
			log.Printf("❌ Error fetching shift: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch shift")
			return
		}

		// Only allow cancelling active, paused, or ready shifts
		if shift.Status != "active" && shift.Status != "paused" && shift.Status != "ready" {
			utils.RespondError(w, http.StatusBadRequest, fmt.Sprintf("Cannot cancel shift with status: %s", shift.Status))
			return
		}

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		// 1. Update shift status to cancelled, record end_time
		_, err = tx.Exec(`
			UPDATE shifts
			SET status = 'cancelled', end_time = $1, updated_at = $1
			WHERE id = $2
		`, now, shiftID)
		if err != nil {
			log.Printf("❌ Error updating shift status: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to cancel shift")
			return
		}

		// 2. Release the shift's incomplete moves to the driver's backlog (shared
		// helper — same release rule as EndShift), then log the cancellation in
		// each move's history.
		released, relErr := moverequest.ReleaseFromShift(tx, []string{shiftID}, now)
		if relErr != nil {
			log.Printf("⚠️  Error releasing move requests on cancel: %v", relErr)
		}
		for _, mr := range released {
			metadata := fmt.Sprintf(`{"shift_id":"%s","end_reason":"manager_cancelled","cancelled_by":"%s"}`, shiftID, userClaims.UserID)
			if logErr := moverequest.LogUnassigned(
				tx, mr.ID, userClaims.UserID, userClaims.Email,
				mr.AssignmentType, mr.AssignedUserID, mr.AssignedUserName, mr.AssignedShiftID,
			); logErr != nil {
				log.Printf("⚠️  Failed to log move request unassignment history for %s: %v", mr.ID, logErr)
			}
			notesQuery := `UPDATE move_request_history SET notes = $1, metadata = $2 WHERE move_request_id = $3 AND action_type = 'unassigned' AND created_at = (SELECT MAX(created_at) FROM move_request_history WHERE move_request_id = $3 AND action_type = 'unassigned')`
			if _, noteErr := tx.Exec(notesQuery, "Shift cancelled by manager", metadata, mr.ID); noteErr != nil {
				log.Printf("⚠️  Failed to update history notes for %s: %v", mr.ID, noteErr)
			}
		}

		// 3. route_tasks are preserved for shift history audit trail

		// 4. Insert into shift_history so this cancellation appears in history tab
		var cancelOptMeta interface{}
		if shift.OptimizationMetadata != nil {
			if b, e := json.Marshal(shift.OptimizationMetadata); e == nil {
				cancelOptMeta = b
			}
		}
		_, err = archiveShift(tx, archiveShiftParams{
			ID:                  shift.ID,
			DriverID:            shift.DriverID,
			RouteID:             shift.RouteID,
			StartTime:           shift.StartTime,
			EndTime:             now,
			CreatedAt:           shift.CreatedAt,
			EndedAt:             now,
			TotalPauseSeconds:   shift.TotalPauseSeconds,
			TotalBins:           shift.TotalBins,
			CompletedBins:       shift.CompletedBins,
			IncidentsReported:   0,
			FieldObservations:   0,
			EndReason:           "manager_cancelled",
			EndedByUserID:       userClaims.UserID,
			EndReasonMetadata:   nil,
			OptMeta:             cancelOptMeta,
			OnConflictDoNothing: true,
		})
		if err != nil {
			log.Printf("⚠️  Error inserting shift history on cancel: %v", err)
			// Don't fail the cancellation — history is best-effort
		} else {
			log.Printf("✅ Shift %s recorded in history (manager_cancelled by %s)", shiftID, userClaims.UserID)
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit cancellation")
			return
		}

		log.Printf("✅ Shift %s cancelled successfully", shiftID)

		// 4. Send WebSocket notification to driver's mobile app (OLD - backward compatibility)
		wsHub.BroadcastToUser(shift.DriverID, map[string]interface{}{
			"type": "shift_cancelled",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"cancelled_at": now,
				"message":      "Your shift has been cancelled by management",
			},
		})
		log.Printf("📡 Sent shift_cancelled websocket (old) to driver %s", shift.DriverID)

		// 4b. Send Centrifugo notification via shift:updates channel (NEW)
		if centrifugoClient != nil {
			cancellationData := map[string]interface{}{
				"type":         "shift_cancelled",
				"shift_id":     shiftID,
				"cancelled_at": now,
				"cancelled_by": userClaims.Email,
				"message":      "Your shift has been cancelled by management",
			}
			err := centrifugoClient.PublishShiftUpdate(r.Context(), shiftID, cancellationData)
			if err != nil {
				log.Printf("⚠️  Failed to send Centrifugo shift cancellation: %v", err)
			} else {
				log.Printf("📡 Sent shift_cancelled via Centrifugo to shift:updates:%s", shiftID)
			}
		}

		// 5. Send FCM push notification to driver (preference-aware)
		cancelExtra := map[string]string{"cancelled_by": userClaims.Email}
		cancelTitle, cancelBody := services.ShiftNotificationText("shift_cancelled", cancelExtra)
		_, cancelNotifIDs := services.CreateNotificationForUsers(db, []string{shift.DriverID}, "shift_cancelled", cancelTitle, cancelBody, map[string]string{"shift_id": shiftID})
		if len(cancelNotifIDs) > 0 && fcmService != nil {
			var driverFCMToken models.FCMToken
			tokenErr := db.Get(&driverFCMToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, shift.DriverID)
			if tokenErr != nil {
				log.Printf("⚠️  No FCM token found for driver %s: %v", shift.DriverID, tokenErr)
			} else {
				fcmErr := fcmService.SendShiftUpdateNotification(
					driverFCMToken.Token,
					shiftID,
					"shift_cancelled",
					cancelExtra,
				)
				if fcmErr != nil {
					log.Printf("⚠️  Failed to send FCM notification: %v", fcmErr)
				} else {
					log.Printf("📱 Sent FCM shift_cancelled to driver %s", shift.DriverID)
				}
			}
		}

		// 6. Broadcast to dashboard (managers/admins)
		cancelDashboardData := map[string]interface{}{
			"shift_id":     shiftID,
			"driver_id":    shift.DriverID,
			"cancelled_at": now,
		}
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "shift_cancelled",
			"data": cancelDashboardData,
		})
		wsHub.BroadcastToRole("manager", map[string]interface{}{
			"type": "shift_cancelled",
			"data": cancelDashboardData,
		})

		// Publish shift_cancelled + driver_shift_change to Centrifugo company:events
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "shift_cancelled", cancelDashboardData); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_cancelled to company:events: %v", pubErr)
			}
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
				"driver_id": shift.DriverID,
				"status":    "cancelled",
				"shift_id":  shiftID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish driver_shift_change to company:events: %v", pubErr)
			}
			// Notify driver via driver:events channel
			if pubErr := centrifugoClient.PublishDriverEvent(r.Context(), shift.DriverID, "shift_cancelled", cancelDashboardData); pubErr != nil {
				log.Printf("⚠️  Failed to publish shift_cancelled to driver:events:%s: %v", shift.DriverID, pubErr)
			}

			// Create per-user notifications for admins
			adminIDs, _ := services.GetAdminUserIDs(db)
			services.CreateNotificationForUsers(db, adminIDs, "shift_cancelled",
				"Shift Cancelled",
				fmt.Sprintf("Shift %s has been cancelled", shiftID),
				cancelDashboardData)
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "Shift cancelled successfully",
			"data": map[string]interface{}{
				"shift_id":     shiftID,
				"driver_id":    shift.DriverID,
				"cancelled_at": now,
			},
		})
	}
}

// CancelAllActiveShifts cancels all active or paused shifts
// POST /api/manager/shifts/cancel-all-active
func CancelAllActiveShifts(db *sqlx.DB, wsHub *websocket.Hub, fcmService *services.FCMService, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("❌ REQUEST: POST /api/manager/shifts/cancel-all-active")

		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			utils.RespondError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}

		now := time.Now().Unix()

		// Get all active/paused shifts
		var shifts []models.Shift
		err := db.Select(&shifts, `
			SELECT * FROM shifts
			WHERE status IN ('active', 'paused', 'ready')
		`)
		if err != nil {
			log.Printf("❌ Error fetching active shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to fetch active shifts")
			return
		}

		if len(shifts) == 0 {
			utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"message": "No active shifts to cancel",
				"data": map[string]interface{}{
					"cancelled_count": 0,
				},
			})
			return
		}

		log.Printf("📋 Found %d active/paused shift(s) to cancel", len(shifts))

		// Start transaction
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("❌ Error starting transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to start transaction")
			return
		}
		defer tx.Rollback()

		// Collect shift IDs
		shiftIDs := make([]string, len(shifts))
		for i, shift := range shifts {
			shiftIDs[i] = shift.ID
		}

		// 1. Update all shifts to cancelled, record end_time
		query, args, err := sqlx.In(`
			UPDATE shifts
			SET status = 'cancelled', end_time = ?, updated_at = ?
			WHERE id IN (?)
		`, now, now, shiftIDs)
		if err != nil {
			log.Printf("❌ Error building update query: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to build query")
			return
		}
		query = tx.Rebind(query)
		_, err = tx.Exec(query, args...)
		if err != nil {
			log.Printf("❌ Error updating shifts: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to cancel shifts")
			return
		}

		// 2. Release each shift's incomplete moves to that shift driver's backlog
		// (shared helper — same rule as End/Cancel, and handles the bulk shiftIDs).
		if _, relErr := moverequest.ReleaseFromShift(tx, shiftIDs, now); relErr != nil {
			log.Printf("⚠️  Error releasing move requests on cancel-all: %v", relErr)
		}

		// 3. route_tasks are preserved for shift history audit trail

		// 4. Insert each shift into shift_history
		for _, s := range shifts {
			var bulkOptMeta interface{}
			if s.OptimizationMetadata != nil {
				if b, e := json.Marshal(s.OptimizationMetadata); e == nil {
					bulkOptMeta = b
				}
			}
			_, histErr := archiveShift(tx, archiveShiftParams{
				ID:                  s.ID,
				DriverID:            s.DriverID,
				RouteID:             s.RouteID,
				StartTime:           s.StartTime,
				EndTime:             now,
				CreatedAt:           s.CreatedAt,
				EndedAt:             now,
				TotalPauseSeconds:   s.TotalPauseSeconds,
				TotalBins:           s.TotalBins,
				CompletedBins:       s.CompletedBins,
				IncidentsReported:   0,
				FieldObservations:   0,
				EndReason:           "manager_cancelled",
				EndedByUserID:       userClaims.UserID,
				EndReasonMetadata:   nil,
				OptMeta:             bulkOptMeta,
				OnConflictDoNothing: true,
			})
			if histErr != nil {
				log.Printf("⚠️  Error inserting history for shift %s: %v", s.ID, histErr)
			}
		}

		// Commit transaction
		if err := tx.Commit(); err != nil {
			log.Printf("❌ Error committing transaction: %v", err)
			utils.RespondError(w, http.StatusInternalServerError, "Failed to commit cancellations")
			return
		}

		log.Printf("✅ Cancelled %d shift(s) successfully", len(shifts))

		// 4. Send notifications to each affected driver
		for _, shift := range shifts {
			cancelData := map[string]interface{}{
				"shift_id":     shift.ID,
				"cancelled_at": now,
				"message":      "Your shift has been cancelled by management",
			}

			// WebSocket notification
			wsHub.BroadcastToUser(shift.DriverID, map[string]interface{}{
				"type": "shift_cancelled",
				"data": cancelData,
			})

			// Centrifugo: notify driver + shift channel + company
			if centrifugoClient != nil {
				if pubErr := centrifugoClient.PublishDriverEvent(r.Context(), shift.DriverID, "shift_cancelled", cancelData); pubErr != nil {
					log.Printf("⚠️  Failed to publish shift_cancelled to driver:events:%s: %v", shift.DriverID, pubErr)
				}
				if pubErr := centrifugoClient.PublishShiftUpdate(r.Context(), shift.ID, map[string]interface{}{
					"type": "shift_cancelled",
					"data": cancelData,
				}); pubErr != nil {
					log.Printf("⚠️  Failed to publish shift_cancelled to shift:updates:%s: %v", shift.ID, pubErr)
				}
				if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "driver_shift_change", map[string]interface{}{
					"driver_id": shift.DriverID,
					"status":    "cancelled",
					"shift_id":  shift.ID,
				}); pubErr != nil {
					log.Printf("⚠️  Failed to publish driver_shift_change to Centrifugo: %v", pubErr)
				}
			}

			// FCM push notification (preference-aware)
			delTitle, delBody := services.ShiftNotificationText("shift_deleted", nil)
			_, delNotifIDs := services.CreateNotificationForUsers(db, []string{shift.DriverID}, "shift_deleted", delTitle, delBody, map[string]string{"shift_id": shift.ID})
			if len(delNotifIDs) > 0 && fcmService != nil {
				var bulkFCMToken models.FCMToken
				tokenErr := db.Get(&bulkFCMToken, `SELECT * FROM fcm_tokens WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 1`, shift.DriverID)
				if tokenErr != nil {
					log.Printf("⚠️  No FCM token found for driver %s: %v", shift.DriverID, tokenErr)
				} else {
					fcmErr := fcmService.SendShiftUpdateNotification(
						bulkFCMToken.Token,
						shift.ID,
						"shift_deleted",
						nil,
					)
					if fcmErr != nil {
						log.Printf("⚠️  Failed to send FCM to driver %s: %v", shift.DriverID, fcmErr)
					} else {
						log.Printf("📱 Sent FCM shift_deleted to driver %s", shift.DriverID)
					}
				}
			}
		}

		log.Printf("📡 Sent notifications to %d driver(s)", len(shifts))

		// 5. Broadcast to dashboard
		bulkCancelData := map[string]interface{}{
			"cancelled_count": len(shifts),
			"shift_ids":       shiftIDs,
			"cancelled_at":    now,
		}
		wsHub.BroadcastToRole("admin", map[string]interface{}{
			"type": "bulk_shifts_cancelled",
			"data": bulkCancelData,
		})
		wsHub.BroadcastToRole("manager", map[string]interface{}{
			"type": "bulk_shifts_cancelled",
			"data": bulkCancelData,
		})

		// Publish bulk_shifts_cancelled to Centrifugo company:events
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "bulk_shifts_cancelled", bulkCancelData); pubErr != nil {
				log.Printf("⚠️  Failed to publish bulk_shifts_cancelled to Centrifugo: %v", pubErr)
			}
		}

		utils.RespondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("Successfully cancelled %d shift(s)", len(shifts)),
			"data": map[string]interface{}{
				"cancelled_count": len(shifts),
				"shift_ids":       shiftIDs,
				"cancelled_at":    now,
			},
		})
	}
}

// ============================================================================
// SEGMENTED ROUTE OPTIMIZATION
// Manager places warehouse stops, we optimize tasks within each segment
// ============================================================================

// optimizeRouteInSegments performs route optimization between warehouse stops
// Warehouse stops act as segment boundaries and stay in place
