package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"ropacal-backend/internal/itinerary"
	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/models"
	"ropacal-backend/internal/moverequest"
	"ropacal-backend/internal/services/centrifugo"
	"ropacal-backend/internal/services/redis"
	"ropacal-backend/internal/websocket"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func UpdateBinMoveRequest(store moverequest.Store, db *sqlx.DB, redisClient *redis.Client, wsHub *websocket.Hub, centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "Missing move request ID", http.StatusBadRequest)
			return
		}

		// Parse request body
		var req struct {
			// Basic fields
			ScheduledDate *int64   `json:"scheduled_date,omitempty"`
			MoveType      *string  `json:"move_type,omitempty"` // "store" or "relocation"
			Reason        *string  `json:"reason,omitempty"`
			Notes         *string  `json:"notes,omitempty"`
			NewStreet     *string  `json:"new_street,omitempty"`
			NewCity       *string  `json:"new_city,omitempty"`
			NewZip        *string  `json:"new_zip,omitempty"`
			NewLatitude   *float64 `json:"new_latitude,omitempty"`
			NewLongitude  *float64 `json:"new_longitude,omitempty"`

			// Assignment fields
			AssignedShiftID *string `json:"assigned_shift_id,omitempty"`
			AssignedUserID  *string `json:"assigned_user_id,omitempty"`
			AssignmentType  *string `json:"assignment_type,omitempty"` // "shift", "manual", or "" for unassigned

			// Edge case handling
			ClientUpdatedAt          *int64  `json:"client_updated_at,omitempty"`     // For optimistic locking
			ConfirmActiveShiftChange bool    `json:"confirm_active_shift_change"`     // User confirmed warning
			InProgressAction         *string `json:"in_progress_action,omitempty"`    // "remove_from_route", "insert_after_current", "reoptimize_route"
			InsertAfterWaypoint      *int    `json:"insert_after_waypoint,omitempty"` // For manual insertion
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Validate move_type at the boundary (typed → clean 400, not a 500 at the
		// DB CHECK and not a silent passthrough). One source of truth: moverequest.ParseMoveType.
		if req.MoveType != nil {
			if _, err := moverequest.ParseMoveType(*req.MoveType); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		// Get authenticated user (manager making the update)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "User not authenticated", http.StatusUnauthorized)
			return
		}
		managerUserID := userClaims.UserID

		// Fetch manager's name for notifications
		var managerName string
		err := db.Get(&managerName, `SELECT name FROM users WHERE id = $1`, managerUserID)
		if err != nil {
			log.Printf("Warning: Could not fetch manager name: %v", err)
			managerName = "A manager" // Fallback
		}

		// Fetch existing move request (with shift/bin context) via the domain Store.
		moveRequest, err := store.EditByID(id)
		if err != nil {
			if errors.Is(err, moverequest.ErrNotFound) {
				http.Error(w, "Move request not found", http.StatusNotFound)
				return
			}
			log.Printf("Error fetching move request: %v", err)
			http.Error(w, "Failed to fetch move request", http.StatusInternalServerError)
			return
		}

		// BLOCK: Completed or cancelled moves cannot be edited
		if moveRequest.Status == "completed" || moveRequest.Status == "cancelled" {
			http.Error(w, fmt.Sprintf("❌ Cannot edit %s move request. This move has been finalized and cannot be modified.", moveRequest.Status), http.StatusBadRequest)
			return
		}

		// OPTIMISTIC LOCKING: Check if move was modified by another user
		if req.ClientUpdatedAt != nil && moveRequest.UpdatedAt != *req.ClientUpdatedAt {
			http.Error(w,
				fmt.Sprintf("⚠️ Conflict: This move request was modified by another user while you were editing it. "+
					"The driver may have completed this bin, or another manager may have reassigned it. "+
					"Please refresh and try again with the latest data."),
				http.StatusConflict)
			return
		}

		// CHECK: Is this move on an active shift?
		isOnActiveShift := moveRequest.AssignedShiftID != nil &&
			moveRequest.ShiftStatus != nil &&
			*moveRequest.ShiftStatus == "active"

		// CHECK: Is driver currently at this location?
		isInProgress := moveRequest.Status == "in_progress"

		// VALIDATION: In-progress moves require explicit action
		if isInProgress && (req.AssignedShiftID != nil || req.AssignedUserID != nil) {
			if req.InProgressAction == nil || *req.InProgressAction == "" {
				driverInfo := "Unknown driver"
				if moveRequest.ShiftDriverName != nil {
					driverInfo = *moveRequest.ShiftDriverName
				}
				http.Error(w,
					fmt.Sprintf("⚠️ Driver %s is currently at this location. "+
						"You must specify what should happen to this move by providing 'in_progress_action': "+
						"'remove_from_route', 'insert_after_current', or 'reoptimize_route'.",
						driverInfo),
					http.StatusBadRequest)
				return
			}
		}

		// VALIDATION: Active shift changes require confirmation
		if isOnActiveShift && !isInProgress && !req.ConfirmActiveShiftChange {
			// User is trying to change assignment for a move on an active route
			driverName := "the driver"
			if moveRequest.ShiftDriverName != nil {
				driverName = *moveRequest.ShiftDriverName
			}

			http.Error(w,
				fmt.Sprintf("⚠️ This move is on %s's active route. Changing it will affect their navigation. "+
					"Please confirm by setting 'confirm_active_shift_change' to true.", driverName),
				http.StatusBadRequest)
			return
		}

		now := time.Now().Unix()

		// Plain field changes go through the moverequest domain (EditFields, called
		// once the tx is open below). Urgency is recomputed inside the domain when
		// scheduled_date changes; new_address is composed here at the boundary.
		// Assignment changes are NOT here — they're handled by the assignment block
		// below via updates[] and the guarded domain transitions.
		fieldEdits := moverequest.FieldEdits{
			ScheduledDate: req.ScheduledDate,
			MoveType:      req.MoveType,
			Reason:        req.Reason,
			Notes:         req.Notes,
			NewLatitude:   req.NewLatitude,
			NewLongitude:  req.NewLongitude,
		}
		if req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil {
			addr := fmt.Sprintf("%s, %s %s", *req.NewStreet, *req.NewCity, *req.NewZip)
			fieldEdits.NewAddress = &addr
		}

		// ═══════════════════════════════════════════════════════════════════
		// ASSIGNMENT HANDLING (Shift/User reassignment)
		// ═══════════════════════════════════════════════════════════════════

		// START TRANSACTION for assignment changes
		tx, err := db.Beginx()
		if err != nil {
			log.Printf("Error starting transaction: %v", err)
			http.Error(w, "Failed to start transaction", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		// Apply the plain field changes through the domain (recomputes urgency,
		// guarded so a terminal move can't be edited). No-op if nothing changed.
		if err = moverequest.EditFields(tx, id, fieldEdits, now); err != nil {
			log.Printf("Error applying field edits: %v", err)
			http.Error(w, "Failed to update move request", http.StatusInternalServerError)
			return
		}

		assignmentChanged := false
		affectedDriverIDs := []string{}

		// Resolve the assignment intent (exactly ONE coherent change) and route it
		// through the guarded domain transitions — replacing the old fragment-append
		// SETs, the combined UPDATE, and the post-write status re-read/fixup. Each
		// transition sets the assignment columns AND status atomically + coherently
		// (AssignToShift nulls the user; ClearAssignment is a real unassign). An
		// in-progress move only changes assignment via the explicit remove_from_route.
		var assignChange moverequest.AssignmentChange
		if isInProgress {
			if req.InProgressAction != nil && *req.InProgressAction == "remove_from_route" {
				assignChange = moverequest.AssignmentChange{Kind: moverequest.AssignUnassignKind}
			}
		} else {
			assignChange = moverequest.PlanAssignment(
				moveRequest.AssignedShiftID, moveRequest.AssignedUserID,
				req.AssignedShiftID, req.AssignedUserID, req.AssignmentType,
			)
		}

		if assignChange.Kind != moverequest.AssignNoChange {
			assignmentChanged = true

			// Leaving a shift → detach this move's route_tasks, decrement the shift's
			// bin count, and remember the old driver to notify post-commit.
			if moveRequest.AssignedShiftID != nil {
				if _, derr := tx.Exec(`DELETE FROM route_tasks WHERE shift_id = $1 AND bin_id = $2`,
					*moveRequest.AssignedShiftID, moveRequest.BinID); derr != nil {
					log.Printf("Error detaching route_tasks on reassignment: %v", derr)
					http.Error(w, "Failed to update assignment", http.StatusInternalServerError)
					return
				}
				if _, derr := tx.Exec(`UPDATE shifts SET total_bins = total_bins - 1, updated_at = $1 WHERE id = $2`,
					now, *moveRequest.AssignedShiftID); derr != nil {
					log.Printf("Warning: failed to decrement old shift total_bins: %v", derr)
				}
				var oldDriverID string
				if derr := db.Get(&oldDriverID, `SELECT driver_id FROM shifts WHERE id = $1`, *moveRequest.AssignedShiftID); derr == nil {
					affectedDriverIDs = append(affectedDriverIDs, oldDriverID)
				}
			}

			switch assignChange.Kind {
			case moverequest.AssignToShiftKind:
				// Active target shift → in_progress, else assigned.
				var shiftStatus string
				if derr := tx.Get(&shiftStatus, `SELECT status FROM shifts WHERE id = $1`, assignChange.ShiftID); derr != nil {
					log.Printf("Error reading target shift status: %v", derr)
					http.Error(w, "Failed to assign to shift", http.StatusInternalServerError)
					return
				}
				st := moverequest.StatusForAssignment(shiftStatus == "active")
				if aerr := moverequest.AssignToShift(tx, id, assignChange.ShiftID, string(st), now); aerr != nil {
					log.Printf("Error assigning move to shift: %v", aerr)
					http.Error(w, "Failed to assign to shift", http.StatusInternalServerError)
					return
				}
			case moverequest.AssignToDriverKind:
				affectedDriverIDs = append(affectedDriverIDs, assignChange.UserID)
				if aerr := moverequest.AssignToDriver(tx, id, assignChange.UserID, now); aerr != nil {
					log.Printf("Error assigning move to driver: %v", aerr)
					http.Error(w, "Failed to assign to driver", http.StatusInternalServerError)
					return
				}
			case moverequest.AssignUnassignKind:
				if aerr := moverequest.ClearAssignment(tx, id, now); aerr != nil {
					log.Printf("Error unassigning move: %v", aerr)
					http.Error(w, "Failed to unassign move", http.StatusInternalServerError)
					return
				}
			}
		}

		// ── Cascading Update: Update route_tasks if move request address or type changed ───────
		if centrifugoClient != nil && moveRequest.AssignedShiftID != nil {
			log.Printf("🔄 [UPDATE-MOVE] Checking for cascading updates to route_tasks")

			// Check if address changed (for relocation moves with pickup/dropoff tasks)
			addressChanged := false
			if req.NewStreet != nil || req.NewCity != nil || req.NewZip != nil ||
				req.NewLatitude != nil || req.NewLongitude != nil {
				addressChanged = true
			}

			// Check if move_type changed (could remove dropoff task)
			moveTypeChanged := req.MoveType != nil && *req.MoveType != moveRequest.MoveType

			if addressChanged || moveTypeChanged {
				// Find affected route tasks for this move request
				var affectedTasks []struct {
					ShiftID    string  `db:"shift_id"`
					TaskID     string  `db:"task_id"`
					TaskType   string  `db:"task_type"`
					OldAddress *string `db:"old_address"`
				}

				err = tx.Select(&affectedTasks, `
					SELECT
						rt.shift_id,
						rt.id as task_id,
						rt.task_type,
						rt.address as old_address
					FROM route_tasks rt
					JOIN shifts s ON rt.shift_id = s.id
					WHERE rt.move_request_id = $1
					  AND rt.is_completed = 0
					  AND s.status IN ('active', 'scheduled')
				`, id)

				if err != nil && err != sql.ErrNoRows {
					log.Printf("⚠️  [UPDATE-MOVE] Failed to check route tasks: %v", err)
				} else if len(affectedTasks) > 0 {
					log.Printf("🎯 [UPDATE-MOVE] Found %d route task(s) affected by change", len(affectedTasks))

					// If move_type changed from relocation → store, soft delete dropoff tasks
					if moveTypeChanged && req.MoveType != nil && *req.MoveType == "store" {
						var dropoffIDs []string
						for _, task := range affectedTasks {
							if task.TaskType == "dropoff" {
								dropoffIDs = append(dropoffIDs, task.TaskID)
							}
						}
						if deleteErr := itinerary.RemoveByIDs(tx, dropoffIDs, managerUserID, "move_type_changed_to_store", now); deleteErr != nil {
							log.Printf("⚠️  [UPDATE-MOVE] Failed to soft delete dropoff tasks: %v", deleteErr)
						} else if len(dropoffIDs) > 0 {
							log.Printf("✅ [UPDATE-MOVE] Soft deleted %d dropoff task(s) (move_type → store)", len(dropoffIDs))
						}

						// Notify driver that dropoff task was removed
						if moveRequest.AssignedShiftID != nil {
							notifyErr := NotifyDriverOfRouteUpdate(
								db,
								centrifugoClient,
								*moveRequest.AssignedShiftID,
								"move_type_changed",
								map[string]interface{}{
									"move_request_id": id,
									"bin_number":      moveRequest.BinNumber,
									"old_move_type":   moveRequest.MoveType,
									"new_move_type":   *req.MoveType,
									"dropoff_removed": true,
								},
							)

							if notifyErr != nil {
								log.Printf("⚠️  [UPDATE-MOVE] Failed to notify driver: %v", notifyErr)
							} else {
								log.Printf("✅ [UPDATE-MOVE] Notified driver about move type change")
							}
						}

						// If move_type changed from store → relocation, add dropoff task
					} else if moveTypeChanged && req.MoveType != nil && *req.MoveType == "relocation" && moveRequest.MoveType == "store" {
						// Find the pickup task to get shift_id and sequence info
						var pickupTask *struct {
							TaskID        string
							ShiftID       string
							SequenceOrder int
						}

						for _, task := range affectedTasks {
							if task.TaskType == "pickup" {
								pickupTask = &struct {
									TaskID        string
									ShiftID       string
									SequenceOrder int
								}{
									TaskID:        task.TaskID,
									ShiftID:       task.ShiftID,
									SequenceOrder: 0, // Will query for actual sequence
								}
								break
							}
						}

						if pickupTask != nil && req.NewStreet != nil && req.NewLatitude != nil && req.NewLongitude != nil {
							// Get actual sequence order of pickup task
							err := tx.QueryRow(`SELECT sequence_order FROM route_tasks WHERE id = $1`, pickupTask.TaskID).Scan(&pickupTask.SequenceOrder)
							if err != nil {
								log.Printf("⚠️  [UPDATE-MOVE] Failed to get pickup task sequence: %v", err)
							} else {
								// Create new dropoff task right after pickup
								newDropoffID := uuid.New().String()
								_, insertErr := tx.Exec(`
									INSERT INTO route_tasks (
										id, shift_id, move_request_id, task_type,
										sequence_order, address, destination_address,
										destination_latitude, destination_longitude,
										is_completed, created_at, updated_at
									) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 0, $10, $10)
								`, newDropoffID, pickupTask.ShiftID, id, "dropoff",
									pickupTask.SequenceOrder+1, *req.NewStreet, *req.NewStreet,
									*req.NewLatitude, *req.NewLongitude, time.Now().Unix())

								if insertErr != nil {
									log.Printf("⚠️  [UPDATE-MOVE] Failed to create dropoff task: %v", insertErr)
								} else {
									log.Printf("✅ [UPDATE-MOVE] Created dropoff task %s (move_type → relocation)", newDropoffID)

									// Notify driver that dropoff task was added
									if moveRequest.AssignedShiftID != nil {
										notifyErr := NotifyDriverOfRouteUpdate(
											db,
											centrifugoClient,
											*moveRequest.AssignedShiftID,
											"move_type_changed",
											map[string]interface{}{
												"move_request_id": id,
												"bin_number":      moveRequest.BinNumber,
												"old_move_type":   moveRequest.MoveType,
												"new_move_type":   *req.MoveType,
												"dropoff_added":   true,
												"new_address":     *req.NewStreet,
											},
										)

										if notifyErr != nil {
											log.Printf("⚠️  [UPDATE-MOVE] Failed to notify driver: %v", notifyErr)
										} else {
											log.Printf("✅ [UPDATE-MOVE] Notified driver about move type change")
										}
									}
								}
							}
						}

					} else if addressChanged {
						// Update address/coordinates for affected tasks
						for _, task := range affectedTasks {
							// Update based on task type
							if task.TaskType == "dropoff" {
								// Update dropoff destination
								if req.NewStreet != nil && req.NewLatitude != nil && req.NewLongitude != nil {
									_, updateErr := tx.Exec(`
										UPDATE route_tasks
										SET destination_address = $1,
											destination_latitude = $2,
											destination_longitude = $3,
											updated_at = $4
										WHERE id = $5
									`, *req.NewStreet, *req.NewLatitude, *req.NewLongitude, time.Now().Unix(), task.TaskID)

									if updateErr != nil {
										log.Printf("⚠️  [UPDATE-MOVE] Failed to update dropoff task %s: %v", task.TaskID, updateErr)
									} else {
										oldAddr := "unknown"
										if task.OldAddress != nil {
											oldAddr = *task.OldAddress
										}
										log.Printf("✅ [UPDATE-MOVE] Updated dropoff task %s: %s → %s", task.TaskID, oldAddr, *req.NewStreet)
									}
								}
							}
						}

						// Notify driver about address change
						if moveRequest.AssignedShiftID != nil {
							notifyErr := NotifyDriverOfRouteUpdate(
								db,
								centrifugoClient,
								*moveRequest.AssignedShiftID,
								"move_request_address_changed",
								map[string]interface{}{
									"move_request_id": id,
									"bin_number":      moveRequest.BinNumber,
									"old_address":     moveRequest.NewAddress,
									"new_address":     *req.NewStreet,
								},
							)

							if notifyErr != nil {
								log.Printf("⚠️  [UPDATE-MOVE] Failed to notify driver: %v", notifyErr)
							} else {
								log.Printf("✅ [UPDATE-MOVE] Notified driver about address change")
							}
						}
					}
				} else {
					log.Printf("ℹ️  [UPDATE-MOVE] No active route tasks found for this move request")
				}
			}
		}

		// Commit transaction
		if err = tx.Commit(); err != nil {
			log.Printf("Error committing transaction: %v", err)
			http.Error(w, "Failed to commit changes", http.StatusInternalServerError)
			return
		}

		log.Printf("[UPDATE MOVE] ✅ Successfully updated move request: %s", id)

		// Re-optimize shift if move request was on an active shift and address/move_type changed
		if moveRequest.AssignedShiftID != nil {
			addressChanged := req.NewStreet != nil || req.NewCity != nil || req.NewZip != nil ||
				req.NewLatitude != nil || req.NewLongitude != nil
			moveTypeChanged := req.MoveType != nil && *req.MoveType != moveRequest.MoveType

			if addressChanged || moveTypeChanged {
				// Check if shift is active
				var shiftStatus string
				err := db.Get(&shiftStatus, `SELECT status FROM shifts WHERE id = $1`, *moveRequest.AssignedShiftID)
				if err == nil && shiftStatus == "active" {
					log.Printf("🔄 [UPDATE-MOVE] Triggering re-optimization for shift %s (manager-initiated change)", *moveRequest.AssignedShiftID)
					if reoptErr := ReoptimizeActiveShift(db, redisClient, *moveRequest.AssignedShiftID, centrifugoClient, true); reoptErr != nil {
						log.Printf("⚠️  [UPDATE-MOVE] Failed to re-optimize shift: %v", reoptErr)
						// Don't fail the entire request if re-optimization fails
					} else {
						log.Printf("✅ [UPDATE-MOVE] Successfully re-optimized shift %s", *moveRequest.AssignedShiftID)
					}
				}
			}
		}

		// Fetch updated move request to get new state for history logging
		var updatedMove struct {
			models.BinMoveRequest
			AssignedUserName   *string `db:"assigned_user_name"`
			AssignedDriverName *string `db:"assigned_driver_name"`
		}
		err = db.Get(&updatedMove, `
			SELECT
				mr.*,
				assigned_user.name AS assigned_user_name,
				shift_driver.name AS assigned_driver_name
			FROM bin_move_requests mr
			LEFT JOIN users assigned_user ON mr.assigned_user_id = assigned_user.id
			LEFT JOIN shifts s ON mr.assigned_shift_id = s.id
			LEFT JOIN users shift_driver ON s.driver_id = shift_driver.id
			WHERE mr.id = $1
		`, id)
		if err != nil {
			log.Printf("Error fetching updated move request: %v", err)
			http.Error(w, "Failed to fetch updated move request", http.StatusInternalServerError)
			return
		}

		// Log history: determine specific type of change
		if assignmentChanged {
			// Determine what kind of assignment change occurred
			oldHadAssignment := moveRequest.AssignedShiftID != nil || moveRequest.AssignedUserID != nil
			newHasAssignment := updatedMove.AssignedShiftID != nil || updatedMove.AssignedUserID != nil

			if !oldHadAssignment && newHasAssignment {
				// ASSIGNED: Was unassigned, now assigned
				assignmentType := ""
				if updatedMove.AssignmentType != nil {
					assignmentType = *updatedMove.AssignmentType
				}

				// Determine assigned user name (could be from manual assignment or shift driver)
				var assignedUserName *string
				if updatedMove.AssignedUserName != nil {
					assignedUserName = updatedMove.AssignedUserName
				} else if updatedMove.AssignedDriverName != nil {
					assignedUserName = updatedMove.AssignedDriverName
				}

				err = moverequest.LogAssigned(
					db, id, managerUserID, managerName,
					assignmentType,
					updatedMove.AssignedUserID,
					assignedUserName,
					updatedMove.AssignedShiftID,
				)
				if err != nil {
					log.Printf("Warning: Failed to log move request assignment: %v", err)
				}

			} else if oldHadAssignment && !newHasAssignment {
				// UNASSIGNED: Was assigned, now unassigned
				// Determine old assigned user name
				var oldAssignedUserName *string
				if moveRequest.AssignedUserID != nil {
					// Fetch the old assigned user's name
					var userName string
					nameErr := db.Get(&userName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID)
					if nameErr == nil {
						oldAssignedUserName = &userName
					}
				} else if moveRequest.AssignedShiftID != nil {
					// Fetch the old shift driver's name
					var driverName string
					nameErr := db.Get(&driverName, `SELECT u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`, *moveRequest.AssignedShiftID)
					if nameErr == nil {
						oldAssignedUserName = &driverName
					}
				}

				err = moverequest.LogUnassigned(
					db, id, managerUserID, managerName,
					moveRequest.AssignmentType,
					moveRequest.AssignedUserID,
					oldAssignedUserName,
					moveRequest.AssignedShiftID,
				)
				if err != nil {
					log.Printf("Warning: Failed to log move request unassignment: %v", err)
				}

			} else if oldHadAssignment && newHasAssignment {
				// REASSIGNED: Assignment changed from one to another
				// Determine old assigned user name
				var oldAssignedUserName *string
				if moveRequest.AssignedUserID != nil {
					var userName string
					nameErr := db.Get(&userName, `SELECT name FROM users WHERE id = $1`, *moveRequest.AssignedUserID)
					if nameErr == nil {
						oldAssignedUserName = &userName
					}
				} else if moveRequest.AssignedShiftID != nil {
					var driverName string
					nameErr := db.Get(&driverName, `SELECT u.name FROM shifts s JOIN users u ON s.driver_id = u.id WHERE s.id = $1`, *moveRequest.AssignedShiftID)
					if nameErr == nil {
						oldAssignedUserName = &driverName
					}
				}

				// Determine new assigned user name
				var newAssignedUserName *string
				if updatedMove.AssignedUserName != nil {
					newAssignedUserName = updatedMove.AssignedUserName
				} else if updatedMove.AssignedDriverName != nil {
					newAssignedUserName = updatedMove.AssignedDriverName
				}

				err = moverequest.LogReassigned(
					db, id, managerUserID, managerName,
					moveRequest.AssignmentType,
					updatedMove.AssignmentType,
					moveRequest.AssignedUserID,
					updatedMove.AssignedUserID,
					oldAssignedUserName,
					newAssignedUserName,
					moveRequest.AssignedShiftID,
					updatedMove.AssignedShiftID,
				)
				if err != nil {
					log.Printf("Warning: Failed to log move request reassignment: %v", err)
				}
			}
		} else if req.ScheduledDate != nil || req.MoveType != nil || req.Reason != nil || req.Notes != nil ||
			(req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil) ||
			req.NewLatitude != nil || req.NewLongitude != nil {
			// Only log "updated" if move detail fields (not just assignment) were actually provided

			// Build metadata JSON with old/new value comparisons
			type ChangeDetail struct {
				Field        string  `json:"field"`
				Label        string  `json:"label"`
				Old          *string `json:"old,omitempty"`
				New          *string `json:"new,omitempty"`
				OldFormatted *string `json:"old_formatted,omitempty"`
				NewFormatted *string `json:"new_formatted,omitempty"`
				OldTimestamp *int64  `json:"old_timestamp,omitempty"`
				NewTimestamp *int64  `json:"new_timestamp,omitempty"`
			}

			type MetadataStruct struct {
				Changes []ChangeDetail `json:"changes"`
			}

			var changes []ChangeDetail

			// Compare scheduled_date
			if req.ScheduledDate != nil && *req.ScheduledDate != moveRequest.ScheduledDate {
				oldDate := time.Unix(moveRequest.ScheduledDate, 0).Format("Jan 2, 2006")
				newDate := time.Unix(*req.ScheduledDate, 0).Format("Jan 2, 2006")
				changes = append(changes, ChangeDetail{
					Field:        "scheduled_date",
					Label:        "Scheduled Date",
					OldFormatted: &oldDate,
					NewFormatted: &newDate,
					OldTimestamp: &moveRequest.ScheduledDate,
					NewTimestamp: req.ScheduledDate,
				})
			}

			// Compare move_type
			if req.MoveType != nil && *req.MoveType != moveRequest.MoveType {
				old := moveRequest.MoveType
				new := *req.MoveType
				changes = append(changes, ChangeDetail{
					Field: "move_type",
					Label: "Move Type",
					Old:   &old,
					New:   &new,
				})
			}

			// Compare reason
			if req.Reason != nil {
				oldReason := ""
				if moveRequest.Reason != nil {
					oldReason = *moveRequest.Reason
				}
				newReason := *req.Reason
				if oldReason != newReason {
					oldReasonPtr := &oldReason
					if oldReason == "" {
						oldReasonPtr = nil
					}
					changes = append(changes, ChangeDetail{
						Field: "reason",
						Label: "Reason",
						Old:   oldReasonPtr,
						New:   &newReason,
					})
				}
			}

			// Compare notes
			if req.Notes != nil {
				oldNotes := ""
				if moveRequest.Notes != nil {
					oldNotes = *moveRequest.Notes
				}
				newNotes := *req.Notes
				if oldNotes != newNotes {
					oldNotesPtr := &oldNotes
					if oldNotes == "" {
						oldNotesPtr = nil
					}
					changes = append(changes, ChangeDetail{
						Field: "notes",
						Label: "Notes",
						Old:   oldNotesPtr,
						New:   &newNotes,
					})
				}
			}

			// NOTE: We don't compare address strings because the format can vary
			// (e.g., "Street, City 12345" vs "Street, City, 12345"). This causes
			// false positives. The coordinates comparison below is more reliable.

			// Compare coordinates (if both lat/lng provided)
			// We use coordinates for reliable comparison, but display formatted addresses
			if req.NewLatitude != nil && req.NewLongitude != nil {
				oldLat := moveRequest.NewLatitude
				oldLng := moveRequest.NewLongitude
				newLat := req.NewLatitude
				newLng := req.NewLongitude

				latChanged := (oldLat == nil && newLat != nil) || (oldLat != nil && newLat == nil) ||
					(oldLat != nil && newLat != nil && *oldLat != *newLat)
				lngChanged := (oldLng == nil && newLng != nil) || (oldLng != nil && newLng == nil) ||
					(oldLng != nil && newLng != nil && *oldLng != *newLng)

				if latChanged || lngChanged {
					// Get old address (stored as single formatted string in database)
					var oldAddressPtr *string
					if moveRequest.NewAddress != nil && *moveRequest.NewAddress != "" {
						oldAddressPtr = moveRequest.NewAddress
					}

					// Format new address from request (separate fields)
					var newAddressPtr *string
					if req.NewStreet != nil && req.NewCity != nil && req.NewZip != nil {
						newAddr := fmt.Sprintf("%s, %s %s", *req.NewStreet, *req.NewCity, *req.NewZip)
						newAddressPtr = &newAddr
					}

					changes = append(changes, ChangeDetail{
						Field: "new_location",
						Label: "New Location",
						Old:   oldAddressPtr,
						New:   newAddressPtr,
					})
				}
			}

			// Build metadata JSON
			var metadataJSON *string
			if len(changes) > 0 {
				metadata := MetadataStruct{Changes: changes}
				metadataBytes, err := json.Marshal(metadata)
				if err == nil {
					metadataStr := string(metadataBytes)
					metadataJSON = &metadataStr
				} else {
					log.Printf("Warning: Failed to marshal metadata JSON: %v", err)
				}
			}

			// Only log "updated" history entry if there are actual changes
			if len(changes) > 0 {
				notes := "Updated move details"
				err = moverequest.LogUpdated(db, id, managerUserID, managerName, &notes, metadataJSON)
				if err != nil {
					log.Printf("Warning: Failed to log move request update: %v", err)
				}
			}
		}

		// Check if this move request is on an active shift (even if assignment didn't change)
		// This ensures drivers are notified when move details change (scheduled_date, move_type, etc.)
		if updatedMove.AssignedShiftID != nil && wsHub != nil && !assignmentChanged {
			var shift struct {
				DriverID string `db:"driver_id"`
				Status   string `db:"status"`
			}
			err = db.Get(&shift, `SELECT driver_id, status FROM shifts WHERE id = $1`, *updatedMove.AssignedShiftID)
			if err == nil && shift.Status == "active" {
				// Add driver to affected list if not already present
				driverAlreadyInList := false
				for _, dID := range affectedDriverIDs {
					if dID == shift.DriverID {
						driverAlreadyInList = true
						break
					}
				}
				if !driverAlreadyInList {
					affectedDriverIDs = append(affectedDriverIDs, shift.DriverID)
					log.Printf("[UPDATE MOVE] Added driver %s to notification list (active shift field update)", shift.DriverID)
				}
			}
		}

		// Send WebSocket notification if assignment changed OR if there are affected drivers
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		log.Printf("📡 [WEBSOCKET NOTIFICATION CHECK]")
		log.Printf("   assignmentChanged: %v", assignmentChanged)
		log.Printf("   affectedDriverIDs: %v (count: %d)", affectedDriverIDs, len(affectedDriverIDs))
		log.Printf("   wsHub available: %v", wsHub != nil)
		log.Printf("   Should send notifications: %v", (assignmentChanged || len(affectedDriverIDs) > 0) && wsHub != nil)
		log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		// Always publish move_request_updated to Centrifugo so all manager views refresh
		if centrifugoClient != nil {
			if pubErr := centrifugoClient.PublishCompanyEvent(r.Context(), "move_request_updated", map[string]interface{}{
				"move_request_id": id,
				"status":          updatedMove.Status,
				"bin_id":          updatedMove.BinID,
			}); pubErr != nil {
				log.Printf("⚠️  Failed to publish move_request_updated to Centrifugo: %v", pubErr)
			}
		}

		// Notify affected drivers via Centrifugo
		if (assignmentChanged || len(affectedDriverIDs) > 0) && centrifugoClient != nil {
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("📡 [CENTRIFUGO] Sending route update notifications...")

			// Notify affected drivers
			if len(affectedDriverIDs) > 0 {
				log.Printf("   Notifying %d affected driver(s):", len(affectedDriverIDs))

				// Fetch bin number for the notification
				var binNumber int
				err := db.Get(&binNumber, `SELECT bin_number FROM bins WHERE id = $1`, updatedMove.BinID)
				if err != nil {
					log.Printf("⚠️  Warning: Could not fetch bin number: %v", err)
					binNumber = 0 // Fallback
				}

				// Determine action type (removed/added/updated)
				isUnassigning := moveRequest.AssignedShiftID != nil && updatedMove.AssignedShiftID == nil
				actionType := "updated"
				if isUnassigning {
					actionType = "removed"
				} else if moveRequest.AssignedShiftID == nil && updatedMove.AssignedShiftID != nil {
					actionType = "added"
				}

				// Get shift IDs for affected drivers
				// Map: driverID -> shiftID
				driverShifts := make(map[string]string)
				for _, driverID := range affectedDriverIDs {
					var shiftID string
					err := db.Get(&shiftID, `
						SELECT id FROM shifts
						WHERE driver_id = $1 AND status IN ('active', 'ready')
						ORDER BY scheduled_start DESC
						LIMIT 1
					`, driverID)
					if err == nil {
						driverShifts[driverID] = shiftID
					} else {
						log.Printf("⚠️  Warning: Could not find active shift for driver %s: %v", driverID, err)
					}
				}

				// Send notification to each affected shift
				for i, driverID := range affectedDriverIDs {
					shiftID, hasShift := driverShifts[driverID]
					if !hasShift {
						log.Printf("   [%d/%d] Skipping driver %s (no active shift)", i+1, len(affectedDriverIDs), driverID)
						continue
					}

					log.Printf("   [%d/%d] Notifying shift %s (driver: %s)", i+1, len(affectedDriverIDs), shiftID, driverID)

					// Call NotifyDriverOfRouteUpdate with full notification details
					notifyErr := NotifyDriverOfRouteUpdate(
						db,
						centrifugoClient,
						shiftID,
						"route_updated",
						map[string]interface{}{
							"move_request_id": id,
							"manager_name":    managerName,
							"action_type":     actionType,
							"bin_number":      binNumber,
							"message":         fmt.Sprintf("%s has %s Bin #%d from your route", managerName, actionType, binNumber),
						},
					)
					if notifyErr != nil {
						log.Printf("⚠️  Failed to send notification to shift %s: %v", shiftID, notifyErr)
					} else {
						log.Printf("   ✅ Notification sent to shift %s", shiftID)
					}
				}
			} else {
				log.Printf("   ⚠️  No affected drivers to notify")
			}
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		} else {
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
			log.Printf("⚠️  [CENTRIFUGO] Skipping notifications")
			if !assignmentChanged && len(affectedDriverIDs) == 0 {
				log.Printf("   Reason: No assignment changes and no affected drivers")
			} else if centrifugoClient == nil {
				log.Printf("   Reason: Centrifugo client not available")
			}
			log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		}

		// Return updated move request
		response := updatedMove.BinMoveRequest.ToBinMoveRequestResponse()

		// Add assigned user/driver names to response
		if updatedMove.AssignedUserName != nil {
			response.AssignedUserName = updatedMove.AssignedUserName
		}
		if updatedMove.AssignedDriverName != nil {
			response.AssignedDriverName = updatedMove.AssignedDriverName
		}

		// Set unified driver_name field
		if updatedMove.AssignedDriverName != nil {
			response.DriverName = updatedMove.AssignedDriverName
		} else if updatedMove.AssignedUserName != nil {
			response.DriverName = updatedMove.AssignedUserName
		}

		// Fetch bin details
		var bin models.Bin
		err = db.Get(&bin, `
			SELECT id, bin_number, current_street, city, zip, latitude, longitude, status
			FROM bins WHERE id = $1
		`, updatedMove.BinID)
		if err == nil {
			binResp := bin.ToBinResponse()
			response.Bin = &binResp
			// Flatten bin fields for easy table display
			response.BinNumber = bin.BinNumber
			response.CurrentStreet = bin.CurrentStreet
			response.City = bin.City
			response.Zip = bin.Zip
		}

		// Parse new address into separate fields if available
		if updatedMove.NewAddress != nil {
			// Split "street, city zip" format
			parts := strings.Split(*updatedMove.NewAddress, ", ")
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

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// CancelBinMoveRequest cancels a pending move request
// PUT /api/manager/bins/move-requests/:id/cancel
